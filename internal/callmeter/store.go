// Package callmeter owns the callmeter store: one SQLite file recording every
// tool call a Claude chat or sub-agent makes, the model request that grouped
// it and the sub-agent that made it (docs/design.md § The store), plus the
// input sanitizer and the transcript readers the hook entry and the reports
// share. Every row comes from a hook event, or from the transcript of a
// session gone quiet (RecoverQuiet).
package callmeter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// SchemaVersion is the store schema this binary writes and reads, kept in
// PRAGMA user_version.
const SchemaVersion = 1

// BusyTimeout is how long a statement waits on a concurrent writer when the
// caller sets no tighter bound (OpenDBWaiting).
const BusyTimeout = 5 * time.Second

// SourceHook is the source a hook's write sets: the hook wrote the row.
const SourceHook = "hook"

// SourceTranscript is the source of a row the quiet-session recovery wrote
// from a transcript, no hook (RecoverQuiet).
const SourceTranscript = "transcript"

// Fault stages: where a failure to record or parse happened. StageBinary and
// StageTerminated rows come only from ingesting missed.log (IngestMissed):
// binary from a wrapper line, terminated from a line the binary itself wrote
// (a signal, a busy or unavailable store, a panic: parseMissedLine).
// IngestMissed writes such a row only beyond the identical rows already
// stored, so a missed.log claim ingested again adds none.
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
// is one tab, `name TYPE`, and one trailing comma but the table's last line,
// with no SQL comment: deriveTables derives the archive's tables and the
// columns completeSchema adds to an older store from this text. A column added
// after version 1 goes last in its table, so a store that gained it by ALTER
// has the order of one created fresh.
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
CREATE INDEX IF NOT EXISTS calls_request ON calls(request_id);
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
	seat_dir TEXT,
	thinking_tokens INTEGER
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
CREATE TABLE IF NOT EXISTS compactions (
	entry_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_id TEXT,
	ts INTEGER,
	trigger TEXT,
	pre_tokens INTEGER,
	post_tokens INTEGER,
	cumulative_dropped_tokens INTEGER,
	duration_ms INTEGER,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS compactions_session_ts ON compactions(session_id, ts);
CREATE TABLE IF NOT EXISTS session_costs (
	session_id TEXT PRIMARY KEY,
	ts INTEGER,
	started INTEGER,
	cost_usd REAL,
	api_ms INTEGER,
	api_no_retry_ms INTEGER,
	tool_ms INTEGER,
	wall_ms INTEGER,
	model_costs TEXT,
	seat_dir TEXT
);
CREATE TABLE IF NOT EXISTS stop_hooks (
	entry_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_id TEXT,
	prompt_id TEXT,
	ts INTEGER,
	hook_count INTEGER,
	hook_errors INTEGER,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS stop_hooks_session_ts ON stop_hooks(session_id, ts);
CREATE TABLE IF NOT EXISTS stop_hook_runs (
	entry_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	ts INTEGER,
	name TEXT,
	command_bytes INTEGER,
	duration_ms INTEGER,
	PRIMARY KEY (entry_id, seq)
);
CREATE TABLE IF NOT EXISTS turn_durations (
	entry_id TEXT PRIMARY KEY,
	session_id TEXT,
	agent_id TEXT,
	prompt_id TEXT,
	ts INTEGER,
	duration_ms INTEGER,
	message_count INTEGER,
	background_agents INTEGER,
	seat_dir TEXT
);
CREATE INDEX IF NOT EXISTS turn_durations_session_ts ON turn_durations(session_id, ts);
`

// Store is an open callmeter database.
type Store struct {
	db       *sql.DB
	path     string
	complete bool // every table, column and index of schema is present
}

// SchemaComplete reports whether every table, column and index of schema is
// present; false only when an open could not add them (a busy store), and a
// later open tries again.
func (s *Store) SchemaComplete() bool { return s.complete }

// OpenDB opens the store at path with the default wait (OpenDBWaiting).
func OpenDB(ctx context.Context, path string) (*Store, error) {
	return OpenDBWaiting(ctx, path, BusyTimeout)
}

// OpenDBWaiting opens (creating it and its directory) the store at path: WAL,
// a busy timeout of wait, foreign keys off, schema SchemaVersion. A store
// written by a newer schema is refused, never opened and misread.
//
// A store another process is creating at this instant answers SQLITE_BUSY at
// once — its switch to WAL takes no busy wait — so a busy open is retried
// until wait has passed: the first async hooks of a session all race to create
// the store, and the loser must not lose its record. wait is also the wait of
// every later statement on the store, since the driver does not stop a busy
// wait on a ctx deadline.
//
// An open of a current-version store writes nothing, except once, best effort,
// to add the tables, columns and indexes a store created before them lacks
// (completeSchema); Store.SchemaComplete tells whether it found or added them.
func OpenDBWaiting(ctx context.Context, path string, wait time.Duration) (*Store, error) {
	deadline := clock.Real.Now().Add(wait)
	for {
		store, err := openAndPrepare(ctx, path, wait)
		if err == nil || !IsBusy(err) || clock.Real.Now().After(deadline) {
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

// IsBusy reports whether err is SQLite answering SQLITE_BUSY.
func IsBusy(err error) bool {
	var sqliteError *modernsqlite.Error
	return errors.As(err, &sqliteError) && sqliteError.Code()&0xff == sqliteBusy
}

// ErrNewerSchema is wrapped by the refusal to open a store whose schema
// version is newer than SchemaVersion (a seat on a newer callmeter raised it):
// the hook names it `store unavailable: newer schema` (StoreFailureClass).
var ErrNewerSchema = errors.New("refusing to open it")

func openAndPrepare(ctx context.Context, path string, wait time.Duration) (*Store, error) {
	db, err := sqlitedb.OpenStore(ctx, path, wait)
	if err != nil {
		return nil, fmt.Errorf("open callmeter store: %w", err)
	}
	complete, err := prepare(ctx, db, path)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db, path: path, complete: complete}, nil
}

// prepare brings the store to SchemaVersion and reports whether its schema is
// complete: a fresh store is, a current-version store is when completeSchema
// found or added what it lacked.
func prepare(ctx context.Context, db *sql.DB, path string) (bool, error) {
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return false, fmt.Errorf("callmeter store %s: disable foreign keys: %w", path, err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("callmeter store %s: read schema version: %w", path, err)
	}
	if version > SchemaVersion {
		return false, fmt.Errorf(
			"callmeter store %s has schema version %d, newer than this callmeter's version %d: %w",
			path,
			version,
			SchemaVersion,
			ErrNewerSchema,
		)
	}
	if version == SchemaVersion {
		// current: an open writes nothing but, once, what a store created
		// before it lacks (best effort), so concurrent hooks never collide here
		return completeSchema(ctx, db), nil
	}
	return true, createSchema(ctx, db, path)
}

// completeSchema adds to a version-1 store whatever of schema it lacks: a store
// created before a column, table or index existed has none of it, and the
// version stays 1 because every addition is only a column or a table the store
// can do without (a hook then writes the old columns, and the prune leaves
// the store alone: Prune). An open that finds everything writes nothing, one
// read-only query. An open that does not takes the write lock at BEGIN
// IMMEDIATE, works out again what is missing (a concurrent open may have added
// it), adds each missing column by ALTER TABLE, then runs schema, which
// creates the missing tables and indexes since every statement of it is IF NOT
// EXISTS, and commits. It reports true when the store is complete. Any failure
// (busy included) rolls back and reports false, and the open goes on without:
// an open must never lose a hook's record over it, and a later open tries
// again.
func completeSchema(ctx context.Context, db *sql.DB) bool {
	alters, schemaMissing, err := missingFromSchema(ctx, db)
	if err != nil {
		return false
	}
	if len(alters) == 0 && !schemaMissing {
		return true
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false
	}
	rollback := func() bool {
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		return false
	}
	if alters, _, err = missingFromSchema(ctx, conn); err != nil {
		return rollback()
	}
	for _, alter := range alters {
		if _, err := conn.ExecContext(ctx, alter); err != nil {
			return rollback()
		}
	}
	if _, err := conn.ExecContext(ctx, schema); err != nil {
		return rollback()
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback()
	}
	return true
}

// queryer is what *sql.DB and *sql.Conn share for a read.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// missingFromSchema compares the store behind q with schema, writing nothing:
// the ALTER TABLE statements for the columns missing from a table that exists
// (in schema order), and whether any table or index is missing as a whole.
func missingFromSchema(ctx context.Context, q queryer) (alters []string, schemaMissing bool, err error) {
	have := map[string]map[string]bool{}
	rows, err := q.QueryContext(ctx,
		"SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type = 'table'")
	if err != nil {
		return nil, false, fmt.Errorf("read the store's columns: %w", err)
	}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, false, errors.Join(fmt.Errorf("read the store's columns: %w", err), rows.Close())
		}
		if have[table] == nil {
			have[table] = map[string]bool{}
		}
		have[table][column] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, fmt.Errorf("read the store's columns: %w", err)
	}
	haveIndex := map[string]bool{}
	rows, err = q.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'index'")
	if err != nil {
		return nil, false, fmt.Errorf("read the store's indexes: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, false, errors.Join(fmt.Errorf("read the store's indexes: %w", err), rows.Close())
		}
		haveIndex[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, fmt.Errorf("read the store's indexes: %w", err)
	}
	for _, name := range sortedKeys(storeTables) {
		table := storeTables[name]
		if have[name] == nil {
			schemaMissing = true
			continue
		}
		for i, column := range table.columns {
			if !have[name][column] {
				alters = append(alters, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", name, table.definitions[i]))
			}
		}
	}
	for _, name := range schemaIndexes {
		if !haveIndex[name] {
			schemaMissing = true
		}
	}
	return alters, schemaMissing, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
	return s.AddFaultHeld(ctx, f, nil)
}

// AddFaultHeld is AddFault with held called once its transaction holds the
// store's write lock (BatchHeld).
func (s *Store) AddFaultHeld(ctx context.Context, f Fault, held func()) error {
	return s.BatchHeld(ctx, held, func(tx *Tx) error { return tx.AddFault(ctx, f) })
}

// AddFaultIfFree records f in its own transaction only when the store's write
// lock is free at once. written is whether f committed, and stands whatever
// err says: err after a commit is only the failure to restore the store's own
// busy wait on its one connection, or to hand that connection back. Not
// written with no err means another writer holds the lock, and nothing was
// written.
func (s *Store) AddFaultIfFree(ctx context.Context, f Fault) (written bool, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: take its connection: %w", s.path, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: release its connection: %w", s.path, closeErr))
		}
	}()
	var wait int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&wait); err != nil {
		return false, fmt.Errorf("callmeter store %s: read busy_timeout: %w", s.path, err)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return false, fmt.Errorf("callmeter store %s: set busy_timeout: %w", s.path, err)
	}
	defer func() {
		if _, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", wait)); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: restore busy_timeout: %w", s.path, restoreErr))
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if IsBusy(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: begin transaction: %w", s.path, err)
	}
	if err := (&Tx{tx: tx, path: s.path, complete: s.SchemaComplete()}).AddFault(ctx, f); err != nil {
		return false, errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("callmeter store %s: commit: %w", s.path, err)
	}
	return true, nil
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
	verb     string   // REPLACE for aggregates, IGNORE for distinct facts
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
	{name: "calls", verb: "REPLACE", key: []string{"tool_use_id"}, expired: callsExpired},
	{name: "requests", verb: "REPLACE", key: []string{"request_id"}, expired: "ts < ?"},
	{name: "turns", verb: "IGNORE", key: []string{"event_id"}, expired: "ts < ?"},
	{name: "events", verb: "IGNORE", key: []string{"event_id"}, expired: "ts < ?"},
	// A recreated store can reuse fault_id; inArchive checks the entire copy
	// before deletion, while phase 1 leaves the original archived fact intact.
	{name: "faults", verb: "IGNORE", key: []string{"fault_id"}, expired: "ts < ?"},
	{name: "agents", verb: "REPLACE", key: []string{"agent_id"}, expired: "COALESCE(stopped, started) < ?"},
	{name: "agent_turns", verb: "IGNORE", key: []string{"agent_id", "seq"}, expired: "COALESCE(stopped, started) < ?"},
	{name: "sessions", verb: "REPLACE", key: []string{"session_id"}, expired: "last_ts < ?"},
	{name: "compactions", verb: "IGNORE", key: []string{"entry_id"}, expired: "ts < ?"},
	{name: "session_costs", verb: "REPLACE", key: []string{"session_id"}, expired: "ts < ?"},
	{name: "stop_hooks", verb: "IGNORE", key: []string{"entry_id"}, expired: "ts < ?"},
	{name: "stop_hook_runs", verb: "IGNORE", key: []string{"entry_id", "seq"}, expired: "ts < ?"},
	{name: "turn_durations", verb: "IGNORE", key: []string{"entry_id"}, expired: "ts < ?"},
	{
		name:    "command_parts",
		verb:    "IGNORE",
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

// inArchive requires an identical copy on every column the archive keeps.
// IS compares NULLs as well as values; omitted text is never compared.
func (t pruneTable) inArchive() string {
	return t.archiveMatch(archiveTables[t.name].columns)
}

func (t pruneTable) archiveMatch(columns []string) string {
	matches := make([]string, len(columns))
	for i, column := range columns {
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

// archiveTable is one table of the store's schema: its columns, with the
// definition of each, and the DDL of its archive twin.
type archiveTable struct {
	columns     []string // the kept columns, in store order
	definitions []string // the definition of each column, parallel to columns: `name TYPE`
	ddl         string   // CREATE TABLE IF NOT EXISTS archive.{table} (...)
}

// archiveTables is the archive's shape: the store schema's tables without
// their indexes and without the omittedFromArchive columns.
var archiveTables = deriveTables(schema, omittedFromArchive)

// storeTables is the store's own shape: every table of schema with every column,
// what completeSchema checks an open store against.
var storeTables = deriveTables(schema, nil)

// schemaIndexes names the indexes of schema.
var schemaIndexes = deriveIndexes(schema)

// deriveIndexes reads the index names out of the store DDL text.
func deriveIndexes(ddl string) []string {
	const create = "CREATE INDEX IF NOT EXISTS "
	var names []string
	for _, line := range strings.Split(ddl, "\n") {
		if strings.HasPrefix(line, create) {
			names = append(names, strings.Fields(strings.TrimPrefix(line, create))[0])
		}
	}
	return names
}

// deriveTables reads the tables out of the store DDL text, one column per line
// (see schema), and rebuilds each without the columns omit names as
// `table.column`.
func deriveTables(ddl string, omit map[string]bool) map[string]archiveTable {
	const create = "CREATE TABLE IF NOT EXISTS "
	tables := map[string]archiveTable{}
	var name string
	var lines, columns, definitions []string
	for _, line := range strings.Split(ddl, "\n") {
		switch {
		case strings.HasPrefix(line, create):
			name = strings.Fields(strings.TrimPrefix(line, create))[0]
			lines, columns, definitions = nil, nil, nil
		case name != "" && strings.HasPrefix(line, "\t"):
			definition := strings.TrimSuffix(strings.TrimSpace(line), ",")
			column := strings.Fields(definition)[0]
			if omit[name+"."+column] {
				continue
			}
			lines = append(lines, definition)
			if column != "PRIMARY" {
				columns = append(columns, column)
				definitions = append(definitions, definition)
			}
		case name != "" && strings.HasPrefix(line, ")"):
			tables[name] = archiveTable{
				columns:     columns,
				definitions: definitions,
				ddl:         create + "archive." + name + " (\n\t" + strings.Join(lines, ",\n\t") + "\n)",
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
// events, faults, compactions, stop_hooks, stop_hook_runs and turn_durations by
// ts; session_costs by the ts of its snapshot; agents and agent_turns by
// COALESCE(stopped, started); sessions by last_ts; then every command_parts row
// whose call is gone. It returns how many store rows went.
//
// A commit spanning two WAL databases is atomic per file only, so each
// transaction writes one file. Phase 1 uses deferred archive transactions of
// at most pruneChunk rows: aggregates replace an older copy, facts retain the
// first copy of their key. Phase 2 uses short immediate store transactions and
// deletes only rows identical to their committed archive copy. A changed fact
// whose key the archive already holds stays in the store past retention.
// Committed chunks survive a failure; the next prune resumes from those copies.
//
// A store whose schema is incomplete (SchemaComplete: an open could not add
// what it lacked) is left alone and returns 0, nil: the prune's statements name
// columns and tables it lacks. A later open that completes the store prunes it.
//
// A store with no row to archive is left alone and archive.db is never
// attached, so a prune with nothing to do creates no file beside the store.
func (s *Store) Prune(ctx context.Context, before time.Time) (removed int64, err error) {
	if !s.complete {
		return 0, nil
	}
	cutoff := before.UnixMilli()
	pending, err := s.anyToArchive(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if !pending {
		return 0, nil
	}
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
		if _, detachErr := conn.ExecContext(context.WithoutCancel(ctx), "DETACH DATABASE archive"); detachErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: archive detach %s: %w", s.path, archivePath, detachErr))
		}
	}()
	if err := s.archiveExpired(ctx, conn, archivePath, cutoff); err != nil {
		return 0, err
	}
	if pruneAfterArchive != nil {
		if err := pruneAfterArchive(); err != nil {
			return 0, fmt.Errorf("callmeter store %s: prune stopped between phase 1 (archive) and phase 2 (delete): %w", s.path, err)
		}
	}
	return s.deleteArchived(ctx, conn, cutoff)
}

// pruneTransaction uses the phase's transaction mode and rolls back only the
// current chunk on failure, even when the caller's context was cancelled.
func (s *Store) pruneTransaction(ctx context.Context, conn *sql.Conn, begin, phase string, fn func() error) error {
	if _, err := conn.ExecContext(ctx, begin); err != nil {
		return fmt.Errorf("callmeter store %s: begin prune %s: %w", s.path, phase, err)
	}
	rollback := func(cause error) error {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err != nil {
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

// anyToArchive reports whether any pruneTables entry holds a row phase 1 would
// copy. Every expired row is one of them, so false means the prune has nothing
// to archive and nothing to delete.
func (s *Store) anyToArchive(ctx context.Context, cutoff int64) (bool, error) {
	for _, table := range pruneTables {
		predicate := table.archivePredicate()
		var found bool
		if err := s.db.QueryRowContext(ctx, fmt.Sprintf(
			"SELECT EXISTS (SELECT 1 FROM %s WHERE %s)", table.name, predicate,
		), predicateArgs(predicate, cutoff)...).Scan(&found); err != nil {
			return false, fmt.Errorf("callmeter store %s: look for %s rows to prune: %w", s.path, table.name, err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

const pruneChunk = 2000

// archiveExpired commits archive-only chunks without reserving the main store's
// write lock. Missing keys select facts; missing identical copies select aggregates.
func (s *Store) archiveExpired(ctx context.Context, conn *sql.Conn, archivePath string, cutoff int64) error {
	if err := s.pruneTransaction(ctx, conn, "BEGIN", "phase 1 (archive)", func() error {
		return prepareArchive(ctx, conn, archivePath)
	}); err != nil {
		return err
	}
	for _, table := range pruneTables {
		columns := strings.Join(archiveTables[table.name].columns, ", ")
		predicate := table.archivePredicate()
		match := table.archiveMatch(table.key)
		if table.verb == "REPLACE" {
			match = table.inArchive()
		}
		for {
			var copied int64
			err := s.pruneTransaction(ctx, conn, "BEGIN", "phase 1 (archive)", func() error {
				result, err := conn.ExecContext(ctx, fmt.Sprintf(
					"INSERT OR %s INTO archive.%s (%s) SELECT %s FROM %s WHERE (%s) AND NOT EXISTS (SELECT 1 FROM archive.%s a WHERE %s) LIMIT %d",
					table.verb, table.name, columns, columns, table.name, predicate, table.name, match, pruneChunk,
				), predicateArgs(predicate, cutoff)...)
				if err != nil {
					return fmt.Errorf("archive %s: %w", table.name, err)
				}
				copied, err = result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count the archived %s: %w", table.name, err)
				}
				return nil
			})
			if err != nil {
				return err
			}
			if copied == 0 {
				break
			}
		}
	}
	return nil
}

// deleteArchived selects candidates without the write lock, then rechecks their
// expiry and identical copies under it. A large prefix of conflicting facts
// therefore cannot turn a candidate scan into a long write transaction.
func (s *Store) deleteArchived(ctx context.Context, conn *sql.Conn, cutoff int64) (removed int64, err error) {
	for _, table := range pruneTables {
		predicate := fmt.Sprintf("(%s) AND EXISTS (SELECT 1 FROM archive.%s a WHERE %s)", table.expired, table.name, table.inArchive())
		for {
			ids, err := pruneRowIDs(ctx, conn, table.name, predicate, cutoff)
			if err != nil {
				return removed, err
			}
			if len(ids) == 0 {
				break
			}
			var deleted int64
			err = s.pruneTransaction(ctx, conn, "BEGIN IMMEDIATE", "phase 2 (delete)", func() error {
				marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
				args := append(predicateArgs(predicate, cutoff), ids...)
				result, err := conn.ExecContext(ctx, fmt.Sprintf(
					"DELETE FROM %s WHERE %s AND rowid IN (%s)", table.name, predicate, marks,
				), args...)
				if err != nil {
					return fmt.Errorf("delete %s: %w", table.name, err)
				}
				deleted, err = result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count the deleted %s: %w", table.name, err)
				}
				return nil
			})
			if err != nil {
				return removed, err
			}
			removed += deleted
			if deleted == 0 {
				break
			}
			// Give a waiting hook a chance to acquire the lock before the next chunk.
			if err := clock.Real.Sleep(ctx, time.Millisecond); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

func pruneRowIDs(ctx context.Context, conn *sql.Conn, table, predicate string, cutoff int64) ([]any, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf(
		"SELECT rowid FROM %s WHERE %s LIMIT %d", table, predicate, pruneChunk,
	), predicateArgs(predicate, cutoff)...)
	if err != nil {
		return nil, fmt.Errorf("select %s prune chunk: %w", table, err)
	}
	defer rows.Close()
	ids := make([]any, 0, pruneChunk)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read %s prune chunk: %w", table, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s prune chunk: %w", table, err)
	}
	return ids, nil
}

// prepareArchive creates the archive's tables on first use, adds to a table an
// older archive wrote the kept columns it lacks, and stamps its version; an
// archive written by a newer schema is refused, never written.
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
		if err := addArchiveColumns(ctx, conn, table.name, archivePath); err != nil {
			return err
		}
	}
	if version == 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA archive.user_version=%d", SchemaVersion)); err != nil {
			return fmt.Errorf("archive %s: set schema version: %w", archivePath, err)
		}
	}
	return nil
}

// addArchiveColumns adds to archive.{table} each kept column an archive written
// before the column existed lacks, so the prune's copy statements name only
// columns the archive has.
func addArchiveColumns(ctx context.Context, conn *sql.Conn, table, archivePath string) error {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("PRAGMA archive.table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("archive %s: read the columns of %s: %w", table, archivePath, err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return errors.Join(fmt.Errorf("archive %s: read the columns of %s: %w", table, archivePath, err), rows.Close())
		}
		have[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("archive %s: read the columns of %s: %w", table, archivePath, err)
	}
	kept := archiveTables[table]
	for i, column := range kept.columns {
		if have[column] {
			continue
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE archive.%s ADD COLUMN %s", table, kept.definitions[i])); err != nil {
			return fmt.Errorf("archive %s: add column %s in %s: %w", table, column, archivePath, err)
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
