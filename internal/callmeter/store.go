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

// Fault stages: where a failure to record or parse happened. StageBinary rows
// come only from ingesting the wrapper's missed.log (IngestMissed).
const (
	StagePayload    = "payload"
	StageStore      = "store"
	StageTranscript = "transcript"
	StageParse      = "parse"
	StageBinary     = "binary"
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
	tz_offset_minutes INTEGER
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

// pruneTables lists, in prune order, each table with the predicate over its
// rows that have aged out. A calls row with no ts (only its batch landed) ages
// by its request's ts, so calls goes first, while that request is still there;
// any other row with no timestamp has no age and stays. command_parts goes
// last: its rows age with their call, and an orphan has no cutoff of its own.
var pruneTables = []struct {
	name    string
	expired string
	cutoff  bool // the predicate takes the cutoff as its one argument
}{
	{"calls", "COALESCE(ts, (SELECT r.ts FROM requests r WHERE r.request_id = calls.request_id)) < ?", true},
	{"requests", "ts < ?", true},
	{"turns", "ts < ?", true},
	{"events", "ts < ?", true},
	{"faults", "ts < ?", true},
	{"agents", "COALESCE(stopped, started) < ?", true},
	{"agent_turns", "COALESCE(stopped, started) < ?", true},
	{"sessions", "last_ts < ?", true},
	{"command_parts", "tool_use_id NOT IN (SELECT tool_use_id FROM calls)", false},
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

// Prune removes every row older than before from the store, in one
// transaction, after copying it into archive.db beside the store (see
// archiveTables): calls, requests, turns, events and faults by ts; agents and
// agent_turns by COALESCE(stopped, started); sessions by last_ts; then every
// command_parts row whose call is gone. It returns how many store rows went.
// Any failure, the archive's included, rolls the whole prune back: a row is
// never deleted without its archive copy.
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
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("callmeter store %s: begin prune: %w", s.path, err)
	}
	rollback := func(cause error) (int64, error) {
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
			return 0, errors.Join(cause, fmt.Errorf("callmeter store %s: roll back prune: %w", s.path, err))
		}
		return 0, cause
	}
	if err := prepareArchive(ctx, conn, archivePath); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: %w", s.path, err))
	}
	for _, table := range pruneTables {
		var args []any
		if table.cutoff {
			args = []any{cutoff}
		}
		columns := strings.Join(archiveTables[table.name].columns, ", ")
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			"INSERT OR IGNORE INTO archive.%s (%s) SELECT %s FROM %s WHERE %s",
			table.name, columns, columns, table.name, table.expired,
		), args...); err != nil {
			return rollback(fmt.Errorf("callmeter store %s: archive %s: %w", s.path, table.name, err))
		}
		result, err := conn.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s", table.name, table.expired), args...)
		if err != nil {
			return rollback(fmt.Errorf("callmeter store %s: prune %s: %w", s.path, table.name, err))
		}
		n, err := result.RowsAffected()
		if err != nil {
			return rollback(fmt.Errorf("callmeter store %s: prune count of %s: %w", s.path, table.name, err))
		}
		removed += n
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("callmeter store %s: commit prune: %w", s.path, err))
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
