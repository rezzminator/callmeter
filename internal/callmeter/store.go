// Package callmeter owns the callmeter store: one SQLite file recording every
// tool call a Claude chat or sub-agent makes, the model request that grouped
// it and the sub-agent that made it (docs/design.md § The store), plus the
// input sanitizer and the transcript readers the hook entry and the reports
// share. Every row comes from a hook event.
package callmeter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// SchemaVersion is the store schema this binary writes and reads, kept in
// PRAGMA user_version.
const SchemaVersion = 1

// BusyTimeout is how long a statement waits on a concurrent async writer.
const BusyTimeout = 5 * time.Second

// SourceHook is the source every write sets: the hook wrote the row.
const SourceHook = "hook"

// Fault stages: where a failure to record or parse happened. StageBinary and
// StageTerminated rows come only from ingesting missed.log (IngestMissed):
// binary from a wrapper line, terminated from a line the binary itself wrote.
const (
	StagePayload    = "payload"
	StageStore      = "store"
	StageTranscript = "transcript"
	StageParse      = "parse"
	StageBinary     = "binary"
	StageTerminated = "terminated"
)

// ArchiveFile is the archive database's name, kept in the store file's own
// directory.
const ArchiveFile = "archive.db"

// schema is the one store DDL (docs/design.md § The store). Every column line
// is one tab, `name TYPE`, and one trailing comma but the table's last line:
// archiveSchema derives the archive's tables from this text.
const schema = `
CREATE TABLE IF NOT EXISTS calls (
	tool_use_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_id TEXT,
	agent_type TEXT,
	request_id TEXT,
	prompt_id TEXT,
	ts INTEGER,
	tool TEXT,
	input TEXT,
	cwd TEXT,
	duration_ms INTEGER,
	failed INTEGER,
	is_interrupt INTEGER,
	error TEXT,
	bytes_real INTEGER,
	bytes_delivered INTEGER,
	persisted_path TEXT,
	file_path TEXT,
	file_bytes INTEGER,
	file_bytes_before INTEGER,
	read_start INTEGER,
	read_lines INTEGER,
	read_total_lines INTEGER,
	effort TEXT,
	permission_mode TEXT,
	lines_added INTEGER,
	lines_removed INTEGER,
	commit_sha TEXT,
	commit_branch TEXT,
	test_runner TEXT,
	source TEXT,
	config_dir TEXT,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS calls_session_agent_ts ON calls(session_id, agent_id, ts);
CREATE INDEX IF NOT EXISTS calls_ts ON calls(ts);
CREATE INDEX IF NOT EXISTS calls_file_path ON calls(file_path);
CREATE INDEX IF NOT EXISTS calls_prompt ON calls(prompt_id);
CREATE TABLE IF NOT EXISTS requests (
	request_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_id TEXT,
	prompt_id TEXT,
	ts INTEGER,
	model TEXT,
	stop_reason TEXT,
	input_tokens INTEGER,
	cache_read_tokens INTEGER,
	cache_creation_tokens INTEGER,
	cache_creation_5m_tokens INTEGER,
	cache_creation_1h_tokens INTEGER,
	context_tokens INTEGER,
	output_tokens INTEGER,
	calls INTEGER,
	pending INTEGER,
	source TEXT,
	config_dir TEXT,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS requests_session_agent_pending ON requests(session_id, agent_id, pending);
CREATE TABLE IF NOT EXISTS agents (
	agent_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_type TEXT,
	prompt_id TEXT,
	parent_tool_use_id TEXT,
	started INTEGER,
	stopped INTEGER,
	transcript_path TEXT,
	total_tokens INTEGER,
	tool_uses INTEGER,
	model TEXT,
	source TEXT,
	config_dir TEXT,
	seat_dir TEXT
);
CREATE TABLE IF NOT EXISTS agent_turns (
	agent_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	session_id TEXT,
	agent_type TEXT,
	prompt_id TEXT,
	started INTEGER,
	stopped INTEGER,
	start_event_id TEXT,
	stop_event_id TEXT,
	PRIMARY KEY (agent_id, seq)
);
CREATE TABLE IF NOT EXISTS turns (
	event_id TEXT PRIMARY KEY,
	event TEXT,
	session_id TEXT,
	agent_id TEXT,
	agent_type TEXT,
	prompt_id TEXT,
	ts INTEGER,
	effort TEXT,
	permission_mode TEXT,
	background_tasks TEXT,
	session_crons TEXT,
	last_assistant_message_bytes INTEGER,
	stop_hook_active INTEGER,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS turns_session_ts ON turns(session_id, ts);
CREATE TABLE IF NOT EXISTS events (
	event_id TEXT PRIMARY KEY,
	event TEXT,
	ts INTEGER,
	session_id TEXT,
	agent_id TEXT,
	agent_type TEXT,
	prompt_id TEXT,
	effort TEXT,
	permission_mode TEXT,
	source TEXT,
	model TEXT,
	reason TEXT,
	trigger TEXT,
	error_type TEXT,
	load_reason TEXT,
	memory_type TEXT,
	file_path TEXT,
	tool_name TEXT,
	command_name TEXT,
	task_id TEXT,
	prompt_bytes INTEGER,
	detail TEXT,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS events_session_ts ON events(session_id, ts);
CREATE INDEX IF NOT EXISTS events_event_ts ON events(event, ts);
CREATE TABLE IF NOT EXISTS sessions (
	session_id TEXT PRIMARY KEY,
	first_ts INTEGER,
	last_ts INTEGER,
	engine TEXT,
	model TEXT,
	start_source TEXT,
	end_reason TEXT,
	cwd TEXT,
	transcript_path TEXT,
	seat_dir TEXT,
	config_dir TEXT,
	host TEXT,
	tz_name TEXT,
	tz_offset_minutes INTEGER,
	cwd_ts INTEGER,
	transcript_path_ts INTEGER,
	seat_dir_ts INTEGER,
	config_dir_ts INTEGER,
	host_ts INTEGER,
	tz_name_ts INTEGER,
	tz_offset_minutes_ts INTEGER
);
CREATE TABLE IF NOT EXISTS command_parts (
	tool_use_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	lang TEXT,
	program TEXT,
	args TEXT,
	files TEXT,
	parse_status TEXT,
	conditional INTEGER NOT NULL DEFAULT 0,
	parser INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (tool_use_id, seq)
);
CREATE TABLE IF NOT EXISTS faults (
	fault_id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER,
	session_id TEXT,
	tool_use_id TEXT,
	stage TEXT,
	error TEXT
);
`

// Store is an open callmeter database.
type Store struct {
	db   *sql.DB
	path string
}

// OpenDB opens (creating it and its directory) the store at path: WAL,
// BusyTimeout, foreign keys off, schema SchemaVersion. A store written by a
// newer schema is refused, never opened and misread.
//
// A store another process is creating at this instant answers SQLITE_BUSY at
// once — its switch to WAL takes no busy wait — so a busy open is retried
// until BusyTimeout has passed: the first async hooks of a session all race
// to create the store, and the loser must not lose its record.
func OpenDB(ctx context.Context, path string) (*Store, error) {
	deadline := clock.Real.Now().Add(BusyTimeout)
	for {
		store, err := openAndPrepare(ctx, path)
		if err == nil || !isBusySQLite(err) || clock.Real.Now().After(deadline) {
			return store, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(err, fmt.Errorf("callmeter store %s: retry a busy open: %w", path, ctx.Err()))
		case <-clock.Real.After(openRetryDelay):
		}
	}
}

// openRetryDelay spaces the retries of a busy open.
const openRetryDelay = 20 * time.Millisecond

// sqliteBusy is SQLite's primary result code SQLITE_BUSY.
const sqliteBusy = 5

// isBusySQLite reports whether err is SQLite answering SQLITE_BUSY.
func isBusySQLite(err error) bool {
	var sqliteError *modernsqlite.Error
	return errors.As(err, &sqliteError) && sqliteError.Code()&0xff == sqliteBusy
}

func openAndPrepare(ctx context.Context, path string) (*Store, error) {
	db, err := sqlitedb.OpenStore(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open callmeter store: %w", err)
	}
	if err := prepare(ctx, db, path); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db, path: path}, nil
}

func prepare(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", BusyTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("callmeter store %s: set busy_timeout: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("callmeter store %s: disable foreign keys: %w", path, err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("callmeter store %s: read schema version: %w", path, err)
	}
	if version > SchemaVersion {
		return fmt.Errorf(
			"callmeter store %s has schema version %d, newer than this callmeter's version %d: refusing to open it",
			path,
			version,
			SchemaVersion,
		)
	}
	if version == SchemaVersion {
		return nil // current: an open writes nothing, so concurrent hooks never collide here
	}
	return createSchema(ctx, db, path)
}

// createSchema writes the schema and its version in one transaction that takes
// the write lock at BEGIN IMMEDIATE, so a concurrent writer is waited out by
// the busy timeout. A deferred transaction would read first and then fail with
// SQLITE_BUSY at its first write, no busy wait, whenever another async hook
// wrote in between — the store open that lost a hook's whole record. A failed
// create leaves the store at version 0.
func createSchema(ctx context.Context, db *sql.DB, path string) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("callmeter store %s: take a connection for the schema: %w", path, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: release the schema connection: %w", path, closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("callmeter store %s: begin schema transaction: %w", path, err)
	}
	rollback := func(cause error) error {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			return errors.Join(cause, fmt.Errorf("callmeter store %s: roll back schema: %w", path, err))
		}
		return cause
	}
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: create schema: %w", path, err))
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: set schema version: %w", path, err))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: commit schema: %w", path, err))
	}
	return nil
}

// DB is the handle for read-only report queries.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the store.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close callmeter store %s: %w", s.path, err)
	}
	return nil
}

// Fault is one failure to record or parse.
type Fault struct {
	TS        int64 // Unix ms UTC
	SessionID string
	ToolUseID string
	Stage     string // one of the Stage* constants
	Error     string
}

// AddFault records f in its own transaction; an empty SessionID or ToolUseID
// is stored as NULL.
func (s *Store) AddFault(ctx context.Context, f Fault) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.AddFault(ctx, f) })
}

// AddFault records f as part of the transaction; an empty SessionID or
// ToolUseID is stored as NULL.
func (t *Tx) AddFault(ctx context.Context, f Fault) error {
	if _, err := t.tx.ExecContext(ctx,
		"INSERT INTO faults (ts, session_id, tool_use_id, stage, error) VALUES (?, ?, ?, ?, ?)",
		f.TS, nullString(f.SessionID), nullString(f.ToolUseID), f.Stage, f.Error,
	); err != nil {
		return fmt.Errorf("callmeter store %s: add %s fault for call %q: %w", t.path, f.Stage, f.ToolUseID, err)
	}
	return nil
}

// pruneTable is one table of the prune: the predicate over its rows that have
// aged out and the columns that identify one row in the store and the archive.
type pruneTable struct {
	name     string
	key      []string // the row's identity, the same columns in the archive
	expired  string   // the rows that aged out; the cutoff is its one argument when it holds a ?
	archived string   // the rows phase 1 copies; empty means expired
}

// callsExpired is the calls predicate. A calls row with no ts (only its batch
// landed) ages by its request's ts, so calls is pruned before requests, while
// that request is still there.
const callsExpired = "COALESCE(ts, (SELECT r.ts FROM requests r WHERE r.request_id = calls.request_id)) < ?"

// pruneTables lists, in prune order, each table with its predicates (see
// pruneTable); any row with no timestamp has no age and stays. command_parts
// goes last: its rows age with their call. Phase 1 copies a part whose call
// is expiring or already gone, and phase 2, running after the calls are
// deleted, removes the parts whose call is gone: an orphan has no cutoff of its
// own.
var pruneTables = []pruneTable{
	{name: "calls", key: []string{"tool_use_id"}, expired: callsExpired},
	{name: "requests", key: []string{"request_id"}, expired: "ts < ?"},
	{name: "turns", key: []string{"event_id"}, expired: "ts < ?"},
	{name: "events", key: []string{"event_id"}, expired: "ts < ?"},
	// fault_id alone is a number a recreated store hands out again; the archived
	// columns beside it tell a different fault under a reused id from its copy.
	{name: "faults", key: []string{"fault_id", "ts", "session_id", "tool_use_id", "stage"}, expired: "ts < ?"},
	{name: "agents", key: []string{"agent_id"}, expired: "COALESCE(stopped, started) < ?"},
	{name: "agent_turns", key: []string{"agent_id", "seq"}, expired: "COALESCE(stopped, started) < ?"},
	{name: "sessions", key: []string{"session_id"}, expired: "last_ts < ?"},
	{
		name:    "command_parts",
		key:     []string{"tool_use_id", "seq"},
		expired: "tool_use_id NOT IN (SELECT tool_use_id FROM calls)",
		archived: "tool_use_id NOT IN (SELECT tool_use_id FROM calls) OR tool_use_id IN (SELECT tool_use_id FROM calls WHERE " +
			callsExpired + ")",
	},
}

// archivePredicate is the predicate of the rows phase 1 copies.
func (t pruneTable) archivePredicate() string {
	if t.archived != "" {
		return t.archived
	}
	return t.expired
}

// inArchive is the condition that the store row of t has its key in the
// archive; it is meant for an EXISTS over archive.{table} aliased a. IS
// matches a NULL key column, which = never does.
func (t pruneTable) inArchive() string {
	matches := make([]string, len(t.key))
	for i, column := range t.key {
		matches[i] = fmt.Sprintf("a.%[1]s IS %[2]s.%[1]s", column, t.name)
	}
	return strings.Join(matches, " AND ")
}

// predicateArgs is the cutoff as the argument of a predicate that takes it, and
// no argument for one that does not.
func predicateArgs(predicate string, cutoff int64) []any {
	if strings.Contains(predicate, "?") {
		return []any{cutoff}
	}
	return nil
}

// omittedFromArchive names the text columns the archive never keeps: a
// command's sanitized input, an error text and an event's detail stay in the
// store only.
var omittedFromArchive = map[string]bool{
	"calls.input":   true,
	"calls.error":   true,
	"faults.error":  true,
	"events.detail": true,
}

// archiveTable is one table of archive.db, derived from the store's schema.
type archiveTable struct {
	columns []string // the kept columns, in store order
	ddl     string   // CREATE TABLE IF NOT EXISTS archive.{table} (...)
}

// archiveTables is the archive's shape: the store schema's tables without
// their indexes and without the omittedFromArchive columns.
var archiveTables = deriveArchiveTables(schema)

// deriveArchiveTables reads the tables out of the store DDL text, one column
// per line (see schema), and rebuilds each without its omitted columns.
func deriveArchiveTables(ddl string) map[string]archiveTable {
	const create = "CREATE TABLE IF NOT EXISTS "
	tables := map[string]archiveTable{}
	var name string
	var definitions, columns []string
	for _, line := range strings.Split(ddl, "\n") {
		switch {
		case strings.HasPrefix(line, create):
			name = strings.Fields(strings.TrimPrefix(line, create))[0]
			definitions, columns = nil, nil
		case name != "" && strings.HasPrefix(line, "\t"):
			definition := strings.TrimSuffix(strings.TrimSpace(line), ",")
			column := strings.Fields(definition)[0]
			if omittedFromArchive[name+"."+column] {
				continue
			}
			definitions = append(definitions, definition)
			if column != "PRIMARY" {
				columns = append(columns, column)
			}
		case name != "" && strings.HasPrefix(line, ")"):
			tables[name] = archiveTable{
				columns: columns,
				ddl:     create + "archive." + name + " (\n\t" + strings.Join(definitions, ",\n\t") + "\n)",
			}
			name = ""
		}
	}
	return tables
}

// pruneAfterArchive is the seam between the two phases of Prune: nil in
// production, a test sets it to stop the prune after phase 1 has committed (an
// error it returns ends Prune before phase 2) or to act between the phases.
var pruneAfterArchive func() error

// Prune removes every row older than before from the store and keeps its copy
// in archive.db beside the store (see archiveTables): calls, requests, turns,
// events and faults by ts; agents and agent_turns by COALESCE(stopped, started);
// sessions by last_ts; then every command_parts row whose call is gone. It
// returns how many store rows went.
//
// A commit spanning two WAL databases is atomic per file only, so the prune is
// two single-file transactions. Phase 1 writes only archive.db: it copies every
// row the prune will remove, with INSERT OR IGNORE, and commits. Phase 2 writes
// only callmeter.db: it deletes each expired row whose key is in the archive
// and commits. Any failure rolls back its phase and ends the prune, the archive's
// included, so a row is never deleted without its archive copy. A stop between
// the phases leaves rows in both files, which the next prune deletes from the
// store and does not copy again.
func (s *Store) Prune(ctx context.Context, before time.Time) (removed int64, err error) {
	cutoff := before.UnixMilli()
	archivePath := filepath.Join(filepath.Dir(s.path), ArchiveFile)
	// ATTACH belongs to one connection and cannot run inside a transaction.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("callmeter store %s: take a connection to prune: %w", s.path, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: release the prune connection: %w", s.path, closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS archive", archivePath); err != nil {
		return 0, fmt.Errorf("callmeter store %s: archive attach %s: %w", s.path, archivePath, err)
	}
	defer func() {
		if _, detachErr := conn.ExecContext(ctx, "DETACH DATABASE archive"); detachErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: archive detach %s: %w", s.path, archivePath, detachErr))
		}
	}()
	if err := s.pruneTransaction(ctx, conn, "phase 1 (archive)", func() error {
		return archiveExpired(ctx, conn, archivePath, cutoff)
	}); err != nil {
		return 0, err
	}
	if pruneAfterArchive != nil {
		if err := pruneAfterArchive(); err != nil {
			return 0, fmt.Errorf("callmeter store %s: prune stopped between phase 1 (archive) and phase 2 (delete): %w", s.path, err)
		}
	}
	if err := s.pruneTransaction(ctx, conn, "phase 2 (delete)", func() (err error) {
		removed, err = deleteArchived(ctx, conn, cutoff)
		return err
	}); err != nil {
		return 0, err
	}
	return removed, nil
}

// pruneTransaction runs fn on conn in one transaction that takes the write lock
// at BEGIN IMMEDIATE. A failure rolls the transaction back and names the store
// and the phase.
func (s *Store) pruneTransaction(ctx context.Context, conn *sql.Conn, phase string, fn func() error) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("callmeter store %s: begin prune %s: %w", s.path, phase, err)
	}
	rollback := func(cause error) error {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			return errors.Join(cause, fmt.Errorf("callmeter store %s: roll back prune %s: %w", s.path, phase, err))
		}
		return cause
	}
	if err := fn(); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: prune %s: %w", s.path, phase, err))
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: commit prune %s: %w", s.path, phase, err))
	}
	return nil
}

// archiveExpired is phase 1 of Prune, inside its transaction: it creates the
// archive's tables and copies the rows of every pruneTables entry, leaving the
// store alone.
func archiveExpired(ctx context.Context, conn *sql.Conn, archivePath string, cutoff int64) error {
	if err := prepareArchive(ctx, conn, archivePath); err != nil {
		return err
	}
	for _, table := range pruneTables {
		columns := strings.Join(archiveTables[table.name].columns, ", ")
		predicate := table.archivePredicate()
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			"INSERT OR IGNORE INTO archive.%s (%s) SELECT %s FROM %s WHERE %s",
			table.name, columns, columns, table.name, predicate,
		), predicateArgs(predicate, cutoff)...); err != nil {
			return fmt.Errorf("archive %s: %w", table.name, err)
		}
	}
	return nil
}

// deleteArchived is phase 2 of Prune, inside its transaction: it deletes from
// each pruneTables entry, in order, the expired rows whose key is in the
// archive, and returns how many went. An expired row the archive lacks (it
// arrived after phase 1) stays for the next prune.
func deleteArchived(ctx context.Context, conn *sql.Conn, cutoff int64) (removed int64, err error) {
	for _, table := range pruneTables {
		result, err := conn.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE %s AND EXISTS (SELECT 1 FROM archive.%s a WHERE %s)",
			table.name, table.expired, table.name, table.inArchive(),
		), predicateArgs(table.expired, cutoff)...)
		if err != nil {
			return 0, fmt.Errorf("delete %s: %w", table.name, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count the deleted %s: %w", table.name, err)
		}
		removed += n
	}
	return removed, nil
}

// prepareArchive creates the archive's tables on first use and stamps its
// version; an archive written by a newer schema is refused, never written.
func prepareArchive(ctx context.Context, conn *sql.Conn, archivePath string) error {
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA archive.user_version").Scan(&version); err != nil {
		return fmt.Errorf("archive %s: read schema version: %w", archivePath, err)
	}
	if version > SchemaVersion {
		return fmt.Errorf(
			"archive %s has schema version %d, newer than this callmeter's version %d: refusing to write it",
			archivePath,
			version,
			SchemaVersion,
		)
	}
	for _, table := range pruneTables {
		if _, err := conn.ExecContext(ctx, archiveTables[table.name].ddl); err != nil {
			return fmt.Errorf("archive %s: create the table in %s: %w", table.name, archivePath, err)
		}
	}
	if version == 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA archive.user_version=%d", SchemaVersion)); err != nil {
			return fmt.Errorf("archive %s: set schema version: %w", archivePath, err)
		}
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
