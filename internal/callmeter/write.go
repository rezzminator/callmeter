package callmeter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Mode is how an upsert treats a column the caller provided.
type Mode int

const (
	// Overwrite replaces the stored value with every provided column (the hook).
	Overwrite Mode = iota
	// FillEmpty writes a provided column only where the stored value is NULL
	// (the hook events that only fill: a call's cwd after PostToolUse, a
	// call's or sub-agent's start, a sub-agent's totals and its model); a
	// missing row is inserted.
	FillEmpty
	// KeepMin keeps the smaller of the stored and the provided value, and
	// takes the provided one where the stored is NULL (agents.started). For
	// integer columns only.
	KeepMin
	// KeepMax keeps the larger of the two, the same way (agents.stopped, a
	// session's last_ts). For integer columns only.
	KeepMax
)

// ErrorLimit is how many characters of a failed call's error text are kept.
const ErrorLimit = 500

// PendingPrefix starts a provisional request key.
const PendingPrefix = "pending:"

// ProvisionalKey is the request key of a batch whose model message id is not
// yet on disk: pending:{first tool_use_id of the batch}.
func ProvisionalKey(firstToolUseID string) string { return PendingPrefix + firstToolUseID }

// Ptr returns a pointer to v: the way a caller marks a column as provided.
func Ptr[T any](v T) *T { return &v }

// Call is one calls row. A nil field is not provided: the upsert leaves the
// stored column untouched.
type Call struct {
	ToolUseID       string
	SessionID       *string
	AgentID         *string
	AgentType       *string
	RequestID       *string
	PromptID        *string
	TS              *int64 // Unix ms UTC
	Tool            *string
	Input           *string // SanitizeInput's output
	Cwd             *string
	DurationMS      *int64
	Failed          *bool
	IsInterrupt     *bool
	Error           *string // cut to ErrorLimit characters on write
	BytesReal       *int64
	BytesDelivered  *int64
	PersistedPath   *string
	FilePath        *string
	FileBytes       *int64
	FileBytesBefore *int64
	ReadStart       *int64
	ReadLines       *int64
	ReadTotalLines  *int64
	Effort          *string
	PermissionMode  *string
	LinesAdded      *int64 // OutcomeColumns: the lines a file edit added
	LinesRemoved    *int64
	CommitSHA       *string // OutcomeColumns: the commit a git commit made
	CommitBranch    *string
	TestRunner      *string // TestRunnerOf: the runner a Bash command invoked
	Source          *string
	ConfigDir       *string
	SeatDir         *string // the hook's CLAUDE_CONFIG_DIR, symlinks unresolved
}

// Request is one requests row; a nil field is not provided.
type Request struct {
	RequestID             string
	SessionID             *string
	AgentID               *string
	PromptID              *string
	TS                    *int64
	Model                 *string
	StopReason            *string
	InputTokens           *int64
	CacheReadTokens       *int64
	CacheCreationTokens   *int64
	CacheCreation5mTokens *int64 // NULL when the usage carries no cache_creation split
	CacheCreation1hTokens *int64
	ContextTokens         *int64 // input + cache read + cache creation
	OutputTokens          *int64
	Calls                 *int64
	Pending               *bool
	Source                *string
	ConfigDir             *string
	SeatDir               *string
}

// Agent is one agents row; a nil field is not provided.
type Agent struct {
	AgentID         string
	SessionID       *string
	AgentType       *string
	PromptID        *string
	ParentToolUseID *string
	Started         *int64
	Stopped         *int64
	TranscriptPath  *string
	TotalTokens     *int64
	ToolUses        *int64
	Model           *string
	Source          *string
	ConfigDir       *string
	SeatDir         *string
}

// CommandPart is one simple command parsed out of a Bash call.
type CommandPart struct {
	Seq         int
	Lang        string
	Program     string
	Args        []string
	Files       []string
	ParseStatus string
	Conditional bool // inside a branch, a case arm, or right of && / ||
	Parser      int  // the cmdparse.Version that produced the part
}

// PendingRequest is a provisional request and the calls carrying its key.
type PendingRequest struct {
	RequestID string   // the provisional key
	CallIDs   []string // ordered by ts, then tool_use_id
}

// column is one provided column of an upsert. mode, when set, replaces the
// upsert's own mode for this column.
type column struct {
	name  string
	value any
	mode  *Mode
}

func add[T any](columns []column, name string, value *T) []column {
	if value == nil {
		return columns
	}
	return append(columns, column{name: name, value: *value})
}

// withMode is c with its own merge mode.
func (c column) withMode(mode Mode) column {
	c.mode = &mode
	return c
}

func (c Call) columns() []column {
	var cs []column
	cs = add(cs, "session_id", c.SessionID)
	cs = add(cs, "agent_id", c.AgentID)
	cs = add(cs, "agent_type", c.AgentType)
	cs = add(cs, "request_id", c.RequestID)
	cs = add(cs, "prompt_id", c.PromptID)
	cs = add(cs, "ts", c.TS)
	cs = add(cs, "tool", c.Tool)
	cs = add(cs, "input", c.Input)
	cs = add(cs, "cwd", c.Cwd)
	cs = add(cs, "duration_ms", c.DurationMS)
	cs = add(cs, "failed", c.Failed)
	cs = add(cs, "is_interrupt", c.IsInterrupt)
	if c.Error != nil {
		cs = append(cs, column{name: "error", value: cut(*c.Error, ErrorLimit)})
	}
	cs = add(cs, "bytes_real", c.BytesReal)
	cs = add(cs, "bytes_delivered", c.BytesDelivered)
	cs = add(cs, "persisted_path", c.PersistedPath)
	cs = add(cs, "file_path", c.FilePath)
	cs = add(cs, "file_bytes", c.FileBytes)
	cs = add(cs, "file_bytes_before", c.FileBytesBefore)
	cs = add(cs, "read_start", c.ReadStart)
	cs = add(cs, "read_lines", c.ReadLines)
	cs = add(cs, "read_total_lines", c.ReadTotalLines)
	cs = add(cs, "effort", c.Effort)
	cs = add(cs, "permission_mode", c.PermissionMode)
	cs = add(cs, "lines_added", c.LinesAdded)
	cs = add(cs, "lines_removed", c.LinesRemoved)
	cs = add(cs, "commit_sha", c.CommitSHA)
	cs = add(cs, "commit_branch", c.CommitBranch)
	cs = add(cs, "test_runner", c.TestRunner)
	cs = add(cs, "source", c.Source)
	cs = add(cs, "config_dir", c.ConfigDir)
	return add(cs, "seat_dir", c.SeatDir)
}

func (r Request) columns() []column {
	var cs []column
	cs = add(cs, "session_id", r.SessionID)
	cs = add(cs, "agent_id", r.AgentID)
	cs = add(cs, "prompt_id", r.PromptID)
	cs = add(cs, "ts", r.TS)
	cs = add(cs, "model", r.Model)
	cs = add(cs, "stop_reason", r.StopReason)
	cs = add(cs, "input_tokens", r.InputTokens)
	cs = add(cs, "cache_read_tokens", r.CacheReadTokens)
	cs = add(cs, "cache_creation_tokens", r.CacheCreationTokens)
	cs = add(cs, "cache_creation_5m_tokens", r.CacheCreation5mTokens)
	cs = add(cs, "cache_creation_1h_tokens", r.CacheCreation1hTokens)
	cs = add(cs, "context_tokens", r.ContextTokens)
	cs = add(cs, "output_tokens", r.OutputTokens)
	cs = add(cs, "calls", r.Calls)
	cs = add(cs, "pending", r.Pending)
	cs = add(cs, "source", r.Source)
	cs = add(cs, "config_dir", r.ConfigDir)
	return add(cs, "seat_dir", r.SeatDir)
}

func (a Agent) columns() []column {
	var cs []column
	cs = add(cs, "session_id", a.SessionID)
	cs = add(cs, "agent_type", a.AgentType)
	cs = add(cs, "prompt_id", a.PromptID)
	cs = add(cs, "parent_tool_use_id", a.ParentToolUseID)
	cs = add(cs, "started", a.Started)
	cs = add(cs, "stopped", a.Stopped)
	cs = add(cs, "transcript_path", a.TranscriptPath)
	cs = add(cs, "total_tokens", a.TotalTokens)
	cs = add(cs, "tool_uses", a.ToolUses)
	cs = add(cs, "model", a.Model)
	cs = add(cs, "source", a.Source)
	cs = add(cs, "config_dir", a.ConfigDir)
	return add(cs, "seat_dir", a.SeatDir)
}

// cut keeps the first limit characters of s.
func cut(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// Tx is one write transaction: every write of one event goes through one Tx.
type Tx struct {
	tx   *sql.Tx
	path string
}

// Batch runs fn in one transaction, committed when fn returns nil and rolled
// back otherwise.
func (s *Store) Batch(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("callmeter store %s: begin transaction: %w", s.path, err)
	}
	if err := fn(&Tx{tx: tx, path: s.path}); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("callmeter store %s: commit: %w", s.path, err)
	}
	return nil
}

// UpsertCall writes c's provided columns in its own transaction.
func (s *Store) UpsertCall(ctx context.Context, c Call, mode Mode) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.UpsertCall(ctx, c, mode) })
}

// UpsertRequest writes r's provided columns in its own transaction.
func (s *Store) UpsertRequest(ctx context.Context, r Request, mode Mode) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.UpsertRequest(ctx, r, mode) })
}

// UpsertAgent writes a's provided columns in its own transaction.
func (s *Store) UpsertAgent(ctx context.Context, a Agent, mode Mode) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.UpsertAgent(ctx, a, mode) })
}

// ResolveRequest rewrites the provisional request provisionalID to its model
// message id r.RequestID, in one transaction.
func (s *Store) ResolveRequest(ctx context.Context, provisionalID string, r Request) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.ResolveRequest(ctx, provisionalID, r) })
}

// ReplaceCommandParts replaces every command_parts row of toolUseID with
// parts, in one transaction.
func (s *Store) ReplaceCommandParts(ctx context.Context, toolUseID string, parts []CommandPart) error {
	return s.Batch(ctx, func(tx *Tx) error { return tx.ReplaceCommandParts(ctx, toolUseID, parts) })
}

// UpsertCall writes c's provided columns by tool_use_id.
func (t *Tx) UpsertCall(ctx context.Context, c Call, mode Mode) error {
	return t.upsert(ctx, "calls", "tool_use_id", c.ToolUseID, c.columns(), mode)
}

// UpsertRequest writes r's provided columns by request_id.
func (t *Tx) UpsertRequest(ctx context.Context, r Request, mode Mode) error {
	return t.upsert(ctx, "requests", "request_id", r.RequestID, r.columns(), mode)
}

// UpsertAgent writes a's provided columns by agent_id.
func (t *Tx) UpsertAgent(ctx context.Context, a Agent, mode Mode) error {
	return t.upsert(ctx, "agents", "agent_id", a.AgentID, a.columns(), mode)
}

// mergeSet is the `column = expression` of an upsert's DO UPDATE SET: the
// column's own mode when it has one, else the upsert's. The MIN and MAX of two
// values is NULL when either is, so COALESCE falls back to the side that is set.
func mergeSet(table, name string, mode Mode, own *Mode) string {
	if own != nil {
		mode = *own
	}
	switch mode {
	case FillEmpty:
		return fmt.Sprintf("%s = COALESCE(%s.%s, excluded.%s)", name, table, name, name)
	case KeepMin:
		return fmt.Sprintf("%s = COALESCE(MIN(%s.%s, excluded.%s), %s.%s, excluded.%s)", name, table, name, name, table, name, name)
	case KeepMax:
		return fmt.Sprintf("%s = COALESCE(MAX(%s.%s, excluded.%s), %s.%s, excluded.%s)", name, table, name, name, table, name, name)
	default:
		return fmt.Sprintf("%s = excluded.%s", name, name)
	}
}

func (t *Tx) upsert(ctx context.Context, table, keyName, key string, columns []column, mode Mode) error {
	if key == "" {
		return fmt.Errorf("callmeter store %s: upsert into %s without a %s", t.path, table, keyName)
	}
	names := []string{keyName}
	marks := []string{"?"}
	values := []any{key}
	sets := make([]string, 0, len(columns))
	for _, c := range columns {
		names = append(names, c.name)
		marks = append(marks, "?")
		values = append(values, c.value)
		sets = append(sets, mergeSet(table, c.name, mode, c.mode))
	}
	conflict := "DO NOTHING"
	if len(sets) > 0 {
		conflict = "DO UPDATE SET " + strings.Join(sets, ", ")
	}
	statement := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) %s",
		table, strings.Join(names, ", "), strings.Join(marks, ", "), keyName, conflict)
	if _, err := t.tx.ExecContext(ctx, statement, values...); err != nil {
		return fmt.Errorf("callmeter store %s: upsert %s %s=%q: %w", t.path, table, keyName, key, err)
	}
	return nil
}

// ResolveRequest merges the provisional row provisionalID into the row keyed
// r.RequestID (created when absent; parallel calls of one message can land in
// two batches, so the row may already exist): each column keeps the stored
// value and takes the provisional one where NULL and ts is the earlier. It
// then writes r's provided columns over the merge with pending = 0, deletes
// the provisional row, points every call carrying provisionalID at
// r.RequestID and recounts its calls (RecountRequest): a batch delivered twice,
// once provisional and once resolved, counts its calls once.
func (t *Tx) ResolveRequest(ctx context.Context, provisionalID string, r Request) error {
	if provisionalID == "" || r.RequestID == "" {
		return fmt.Errorf(
			"callmeter store %s: resolve request needs both keys, got %q -> %q",
			t.path,
			provisionalID,
			r.RequestID,
		)
	}
	merge := `INSERT INTO requests (request_id, session_id, agent_id, prompt_id, ts, model, stop_reason, input_tokens, cache_read_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, context_tokens, output_tokens, calls, pending, source, config_dir, seat_dir)
		SELECT ?, session_id, agent_id, prompt_id, ts, model, stop_reason, input_tokens, cache_read_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, context_tokens, output_tokens, calls, pending, source, config_dir, seat_dir
		FROM requests WHERE request_id = ?
		ON CONFLICT(request_id) DO UPDATE SET
			session_id = COALESCE(requests.session_id, excluded.session_id),
			agent_id = COALESCE(requests.agent_id, excluded.agent_id),
			prompt_id = COALESCE(requests.prompt_id, excluded.prompt_id),
			ts = CASE WHEN requests.ts IS NULL OR excluded.ts < requests.ts THEN excluded.ts ELSE requests.ts END,
			model = COALESCE(requests.model, excluded.model),
			stop_reason = COALESCE(requests.stop_reason, excluded.stop_reason),
			input_tokens = COALESCE(requests.input_tokens, excluded.input_tokens),
			cache_read_tokens = COALESCE(requests.cache_read_tokens, excluded.cache_read_tokens),
			cache_creation_tokens = COALESCE(requests.cache_creation_tokens, excluded.cache_creation_tokens),
			cache_creation_5m_tokens = COALESCE(requests.cache_creation_5m_tokens, excluded.cache_creation_5m_tokens),
			cache_creation_1h_tokens = COALESCE(requests.cache_creation_1h_tokens, excluded.cache_creation_1h_tokens),
			context_tokens = COALESCE(requests.context_tokens, excluded.context_tokens),
			output_tokens = COALESCE(requests.output_tokens, excluded.output_tokens),
			source = COALESCE(requests.source, excluded.source),
			config_dir = COALESCE(requests.config_dir, excluded.config_dir),
			seat_dir = COALESCE(requests.seat_dir, excluded.seat_dir)`
	if _, err := t.tx.ExecContext(ctx, merge, r.RequestID, provisionalID); err != nil {
		return fmt.Errorf("callmeter store %s: merge request %q into %q: %w", t.path, provisionalID, r.RequestID, err)
	}
	if provisionalID != r.RequestID {
		if _, err := t.tx.ExecContext(ctx, "DELETE FROM requests WHERE request_id = ?", provisionalID); err != nil {
			return fmt.Errorf("callmeter store %s: delete provisional request %q: %w", t.path, provisionalID, err)
		}
	}
	r.Pending = Ptr(false)
	if err := t.UpsertRequest(ctx, r, Overwrite); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(
		ctx,
		"UPDATE calls SET request_id = ? WHERE request_id = ?",
		r.RequestID,
		provisionalID,
	); err != nil {
		return fmt.Errorf("callmeter store %s: point calls of %q at %q: %w", t.path, provisionalID, r.RequestID, err)
	}
	return t.RecountRequest(ctx, r.RequestID)
}

// RecountRequest sets requestID's calls to the number of calls rows carrying
// it, so the count is the same in any landing order and under a duplicate
// delivery. A request with no row is left absent.
func (t *Tx) RecountRequest(ctx context.Context, requestID string) error {
	if _, err := t.tx.ExecContext(
		ctx,
		"UPDATE requests SET calls = (SELECT COUNT(*) FROM calls WHERE request_id = ?) WHERE request_id = ?",
		requestID,
		requestID,
	); err != nil {
		return fmt.Errorf("callmeter store %s: recount calls of request %q: %w", t.path, requestID, err)
	}
	return nil
}

// ReplaceCommandParts deletes toolUseID's command_parts rows and the parse
// faults an earlier parse of it left, then inserts parts: a call's parse
// result is replaced whole.
func (t *Tx) ReplaceCommandParts(ctx context.Context, toolUseID string, parts []CommandPart) error {
	if toolUseID == "" {
		return fmt.Errorf("callmeter store %s: replace command parts without a tool_use_id", t.path)
	}
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM command_parts WHERE tool_use_id = ?", toolUseID); err != nil {
		return fmt.Errorf("callmeter store %s: clear command parts of %q: %w", t.path, toolUseID, err)
	}
	if _, err := t.tx.ExecContext(
		ctx,
		"DELETE FROM faults WHERE tool_use_id = ? AND stage = ?",
		toolUseID,
		StageParse,
	); err != nil {
		return fmt.Errorf("callmeter store %s: clear parse faults of %q: %w", t.path, toolUseID, err)
	}
	for _, part := range parts {
		args, err := jsonList(part.Args)
		if err != nil {
			return fmt.Errorf("callmeter store %s: encode args of %q part %d: %w", t.path, toolUseID, part.Seq, err)
		}
		files, err := jsonList(part.Files)
		if err != nil {
			return fmt.Errorf("callmeter store %s: encode files of %q part %d: %w", t.path, toolUseID, part.Seq, err)
		}
		if _, err := t.tx.ExecContext(
			ctx,
			"INSERT INTO command_parts (tool_use_id, seq, lang, program, args, files, parse_status, conditional, parser) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			toolUseID,
			part.Seq,
			nullString(part.Lang),
			nullString(part.Program),
			args,
			files,
			nullString(part.ParseStatus),
			part.Conditional,
			part.Parser,
		); err != nil {
			return fmt.Errorf("callmeter store %s: insert command part %d of %q: %w", t.path, part.Seq, toolUseID, err)
		}
	}
	return nil
}

func jsonList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	return string(encoded), err
}

// PendingRequests lists the provisional requests of one session and agent
// (agentID "" is the main chat, agent_id IS NULL), each with its call ids.
func (s *Store) PendingRequests(ctx context.Context, sessionID, agentID string) ([]PendingRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id FROM requests
		WHERE pending = 1 AND session_id = ? AND ((? = '' AND agent_id IS NULL) OR agent_id = ?)
		ORDER BY ts, request_id`, sessionID, agentID, agentID)
	if err != nil {
		return nil, fmt.Errorf(
			"callmeter store %s: list pending requests of session %q agent %q: %w",
			s.path,
			sessionID,
			agentID,
			err,
		)
	}
	var pending []PendingRequest
	for rows.Next() {
		var p PendingRequest
		if err := rows.Scan(&p.RequestID); err != nil {
			return nil, errors.Join(
				fmt.Errorf("callmeter store %s: scan pending request: %w", s.path, err),
				rows.Close(),
			)
		}
		pending = append(pending, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read pending requests: %w", s.path, err)
	}
	for i := range pending {
		ids, err := s.callIDs(ctx, pending[i].RequestID)
		if err != nil {
			return nil, err
		}
		pending[i].CallIDs = ids
	}
	return pending, nil
}

// callIDs lists the calls carrying requestID; a provisional key's own first
// tool_use_id is always included, since that call's row may not be written yet.
func (s *Store) callIDs(ctx context.Context, requestID string) ([]string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		"SELECT tool_use_id FROM calls WHERE request_id = ? ORDER BY ts, tool_use_id",
		requestID,
	)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list calls of request %q: %w", s.path, requestID, err)
	}
	var ids []string
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(
				fmt.Errorf("callmeter store %s: scan call of request %q: %w", s.path, requestID, err),
				rows.Close(),
			)
		}
		ids = append(ids, id)
		seen[id] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read calls of request %q: %w", s.path, requestID, err)
	}
	if first, ok := strings.CutPrefix(requestID, PendingPrefix); ok && first != "" && !seen[first] {
		ids = append([]string{first}, ids...)
	}
	return ids, nil
}
