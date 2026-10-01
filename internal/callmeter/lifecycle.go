package callmeter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// The hook events whose rows the derived tables read.
const (
	EventSubagentStart = "SubagentStart"
	EventSubagentStop  = "SubagentStop"
	EventSessionStart  = "SessionStart"
	EventSessionEnd    = "SessionEnd"
)

// engineClaude is the sessions.engine every row of this binary carries.
const engineClaude = "claude"

// Event is one events row: a hook event that is neither a tool call nor a
// turn edge. A nil field is NULL. The payload's prompt and message text never
// reach it: only their byte counts do.
type Event struct {
	EventID        string // lowercase hex SHA-256 of the hook run's stdin
	Event          string // the hook_event_name
	TS             int64  // Unix ms UTC
	SessionID      *string
	AgentID        *string
	AgentType      *string
	PromptID       *string
	Effort         *string
	PermissionMode *string
	Source         *string
	Model          *string
	Reason         *string
	Trigger        *string
	ErrorType      *string
	LoadReason     *string
	MemoryType     *string
	FilePath       *string
	ToolName       *string
	CommandName    *string
	TaskID         *string
	PromptBytes    *int64
	Detail         *string // sanitized JSON
	SeatDir        *string
}

func (e Event) columns() []column {
	cs := []column{{name: "event", value: e.Event}, {name: "ts", value: e.TS}}
	cs = add(cs, "session_id", e.SessionID)
	cs = add(cs, "agent_id", e.AgentID)
	cs = add(cs, "agent_type", e.AgentType)
	cs = add(cs, "prompt_id", e.PromptID)
	cs = add(cs, "effort", e.Effort)
	cs = add(cs, "permission_mode", e.PermissionMode)
	cs = add(cs, "source", e.Source)
	cs = add(cs, "model", e.Model)
	cs = add(cs, "reason", e.Reason)
	cs = add(cs, "trigger", e.Trigger)
	cs = add(cs, "error_type", e.ErrorType)
	cs = add(cs, "load_reason", e.LoadReason)
	cs = add(cs, "memory_type", e.MemoryType)
	cs = add(cs, "file_path", e.FilePath)
	cs = add(cs, "tool_name", e.ToolName)
	cs = add(cs, "command_name", e.CommandName)
	cs = add(cs, "task_id", e.TaskID)
	cs = add(cs, "prompt_bytes", e.PromptBytes)
	cs = add(cs, "detail", e.Detail)
	return add(cs, "seat_dir", e.SeatDir)
}

// Turn is one turns row: a Stop or SubagentStop event. A nil field is NULL.
type Turn struct {
	EventID                   string // lowercase hex SHA-256 of the hook run's stdin
	Event                     string
	SessionID                 *string
	AgentID                   *string
	AgentType                 *string
	PromptID                  *string
	TS                        int64 // Unix ms UTC
	Effort                    *string
	PermissionMode            *string
	BackgroundTasks           *string // sanitized JSON
	SessionCrons              *string // sanitized JSON
	LastAssistantMessageBytes *int64
	StopHookActive            *bool
	SeatDir                   *string
}

func (turn Turn) columns() []column {
	cs := []column{{name: "event", value: turn.Event}, {name: "ts", value: turn.TS}}
	cs = add(cs, "session_id", turn.SessionID)
	cs = add(cs, "agent_id", turn.AgentID)
	cs = add(cs, "agent_type", turn.AgentType)
	cs = add(cs, "prompt_id", turn.PromptID)
	cs = add(cs, "effort", turn.Effort)
	cs = add(cs, "permission_mode", turn.PermissionMode)
	cs = add(cs, "background_tasks", turn.BackgroundTasks)
	cs = add(cs, "session_crons", turn.SessionCrons)
	cs = add(cs, "last_assistant_message_bytes", turn.LastAssistantMessageBytes)
	cs = add(cs, "stop_hook_active", turn.StopHookActive)
	return add(cs, "seat_dir", turn.SeatDir)
}

// Session is what one hook run says about its session: TouchSession folds it
// into the sessions row.
type Session struct {
	SessionID       string
	TS              int64 // the run's clock reading, Unix ms UTC
	Cwd             *string
	TranscriptPath  *string // the main transcript
	SeatDir         *string
	ConfigDir       *string
	Host            *string
	TZName          *string
	TZOffsetMinutes *int64
}

// InsertEvent writes e under the clash rule of insertEarliest (a payload
// delivered twice is one row, the earliest delivery's) and reports whether a
// row went in or was replaced.
func (t *Tx) InsertEvent(ctx context.Context, e Event) (bool, error) {
	if e.Event == "" {
		return false, fmt.Errorf("callmeter store %s: insert event %q without a hook_event_name", t.path, e.EventID)
	}
	return t.insertEarliest(ctx, "events", "event_id", e.EventID, e.TS, e.columns())
}

// InsertTurn writes turn under the clash rule of insertEarliest and reports
// whether a row went in or was replaced.
func (t *Tx) InsertTurn(ctx context.Context, turn Turn) (bool, error) {
	if turn.Event == "" {
		return false, fmt.Errorf("callmeter store %s: insert turn %q without a hook_event_name", t.path, turn.EventID)
	}
	return t.insertEarliest(ctx, "turns", "event_id", turn.EventID, turn.TS, turn.columns())
}

// insertEarliest writes one row keyed by its payload's hash, so the same bytes
// are one row whatever their ts: a stored row with a larger ts is replaced
// whole by this delivery, a stored row with the same or a smaller ts stays.
// The row is then the earliest delivery's, whatever order they landed in.
func (t *Tx) insertEarliest(ctx context.Context, table, keyName, key string, ts int64, columns []column) (bool, error) {
	if key == "" {
		return false, fmt.Errorf("callmeter store %s: insert into %s without a %s", t.path, table, keyName)
	}
	if _, err := t.tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = ? AND ts > ?", table, keyName), key, ts); err != nil {
		return false, fmt.Errorf("callmeter store %s: clear the later %s row %s=%q: %w", t.path, table, keyName, key, err)
	}
	names := []string{keyName}
	marks := []string{"?"}
	values := []any{key}
	for _, c := range columns {
		names = append(names, c.name)
		marks = append(marks, "?")
		values = append(values, c.value)
	}
	result, err := t.tx.ExecContext(ctx, fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
		table, strings.Join(names, ", "), strings.Join(marks, ", ")), values...)
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: insert %s %s=%q: %w", t.path, table, keyName, key, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: count the %s row inserted for %s=%q: %w", t.path, table, keyName, key, err)
	}
	return n > 0, nil
}

// agentEdge is one SubagentStart or SubagentStop event of an agent.
type agentEdge struct {
	eventID   string
	stop      bool
	ts        int64
	sessionID sql.NullString
	agentType sql.NullString
	promptID  sql.NullString
}

// agentTurn is one agent_turns row before its seq.
type agentTurn struct {
	sessionID sql.NullString
	agentType sql.NullString
	promptID  sql.NullString
	started   sql.NullInt64
	stopped   sql.NullInt64
	startID   sql.NullString
	stopID    sql.NullString
}

// fillEmpty takes the edge's owner columns where the turn has none.
func (turn *agentTurn) fillEmpty(edge agentEdge) {
	for _, pair := range []struct{ to, from *sql.NullString }{
		{&turn.sessionID, &edge.sessionID},
		{&turn.agentType, &edge.agentType},
		{&turn.promptID, &edge.promptID},
	} {
		if !pair.to.Valid {
			*pair.to = *pair.from
		}
	}
}

// RebuildAgentTurns rewrites the agent_turns rows of agentID from its events,
// ordered by (ts, event_id) and walked once: a start opens a turn (a turn
// still open stays with stopped NULL), a stop closes the open turn, and a stop
// with no open turn is a row with started NULL. The result is a function of
// the events stored, never of the order they arrived in.
func (t *Tx) RebuildAgentTurns(ctx context.Context, agentID string) error {
	if agentID == "" {
		return fmt.Errorf("callmeter store %s: rebuild agent turns without an agent_id", t.path)
	}
	rows, err := t.tx.QueryContext(ctx,
		`SELECT event_id, event, ts, session_id, agent_type, prompt_id FROM events
		WHERE agent_id = ? AND event IN (?, ?) ORDER BY ts, event_id`,
		agentID, EventSubagentStart, EventSubagentStop)
	if err != nil {
		return fmt.Errorf("callmeter store %s: list start and stop events of agent %q: %w", t.path, agentID, err)
	}
	var edges []agentEdge
	for rows.Next() {
		var edge agentEdge
		var event string
		if err := rows.Scan(&edge.eventID, &event, &edge.ts, &edge.sessionID, &edge.agentType, &edge.promptID); err != nil {
			return errors.Join(fmt.Errorf("callmeter store %s: scan an event of agent %q: %w", t.path, agentID, err), rows.Close())
		}
		edge.stop = event == EventSubagentStop
		edges = append(edges, edge)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("callmeter store %s: read the events of agent %q: %w", t.path, agentID, err)
	}

	var turns []agentTurn
	var open *agentTurn
	for _, edge := range edges {
		switch {
		case !edge.stop:
			if open != nil {
				turns = append(turns, *open)
			}
			open = &agentTurn{started: sql.NullInt64{Int64: edge.ts, Valid: true}, startID: nullID(edge.eventID)}
			open.fillEmpty(edge)
		case open != nil:
			open.stopped = sql.NullInt64{Int64: edge.ts, Valid: true}
			open.stopID = nullID(edge.eventID)
			open.fillEmpty(edge)
			turns = append(turns, *open)
			open = nil
		default:
			orphan := agentTurn{stopped: sql.NullInt64{Int64: edge.ts, Valid: true}, stopID: nullID(edge.eventID)}
			orphan.fillEmpty(edge)
			turns = append(turns, orphan)
		}
	}
	if open != nil {
		turns = append(turns, *open)
	}

	if _, err := t.tx.ExecContext(ctx, "DELETE FROM agent_turns WHERE agent_id = ?", agentID); err != nil {
		return fmt.Errorf("callmeter store %s: clear the agent turns of %q: %w", t.path, agentID, err)
	}
	for i, turn := range turns {
		if _, err := t.tx.ExecContext(ctx,
			`INSERT INTO agent_turns (agent_id, seq, session_id, agent_type, prompt_id, started, stopped, start_event_id, stop_event_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			agentID, i+1, turn.sessionID, turn.agentType, turn.promptID, turn.started, turn.stopped, turn.startID, turn.stopID,
		); err != nil {
			return fmt.Errorf("callmeter store %s: insert turn %d of agent %q: %w", t.path, i+1, agentID, err)
		}
	}
	return nil
}

func nullID(id string) sql.NullString { return sql.NullString{String: id, Valid: id != ""} }

// TouchSession folds one hook run into its sessions row: first_ts keeps the
// minimum and last_ts the maximum of every run's clock reading, and engine is
// claude. Each of the run's own columns holds the value of the earliest run
// that carried it, with that run's ts in {column}_ts: a run's value replaces
// the stored one when the stored one is NULL, when the run's ts is below the
// stored {column}_ts, or when the ts are equal and the run's value is smaller.
// The result is the same whatever order the runs land in, and a column the run
// does not carry is left to the others. The derived columns are
// RefreshSession's.
func (t *Tx) TouchSession(ctx context.Context, s Session) error {
	if s.SessionID == "" {
		return fmt.Errorf("callmeter store %s: upsert into sessions without a session_id", t.path)
	}
	var own []column
	own = add(own, "cwd", s.Cwd)
	own = add(own, "transcript_path", s.TranscriptPath)
	own = add(own, "seat_dir", s.SeatDir)
	own = add(own, "config_dir", s.ConfigDir)
	own = add(own, "host", s.Host)
	own = add(own, "tz_name", s.TZName)
	own = add(own, "tz_offset_minutes", s.TZOffsetMinutes)

	names := []string{"session_id", "first_ts", "last_ts", "engine"}
	values := []any{s.SessionID, s.TS, s.TS, engineClaude}
	sets := []string{
		mergeSet("sessions", "first_ts", KeepMin, nil),
		mergeSet("sessions", "last_ts", KeepMax, nil),
		mergeSet("sessions", "engine", Overwrite, nil),
	}
	for _, c := range own {
		names = append(names, c.name, c.name+"_ts")
		values = append(values, c.value, s.TS)
		// The run's value wins on an empty column, an earlier ts, or the smaller
		// value at an equal ts. Every SET expression reads the row as it was
		// before this update, so the column and its ts both judge the stored pair.
		wins := fmt.Sprintf("sessions.%[1]s IS NULL OR excluded.%[1]s_ts < sessions.%[1]s_ts OR "+
			"(excluded.%[1]s_ts = sessions.%[1]s_ts AND excluded.%[1]s < sessions.%[1]s)", c.name)
		sets = append(sets,
			fmt.Sprintf("%[1]s = CASE WHEN %[2]s THEN excluded.%[1]s ELSE sessions.%[1]s END", c.name, wins),
			fmt.Sprintf("%[1]s_ts = CASE WHEN %[2]s THEN excluded.%[1]s_ts ELSE sessions.%[1]s_ts END", c.name, wins))
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	statement := fmt.Sprintf("INSERT INTO sessions (%s) VALUES (%s) ON CONFLICT(session_id) DO UPDATE SET %s",
		strings.Join(names, ", "), marks, strings.Join(sets, ", "))
	if _, err := t.tx.ExecContext(ctx, statement, values...); err != nil {
		return fmt.Errorf("callmeter store %s: upsert sessions session_id=%q: %w", t.path, s.SessionID, err)
	}
	return nil
}

// AgentSpan reads the stored first start and latest stop of agentID on the
// transaction; an agent with no row is two NULLs. Batch begins deferred, so a
// caller reads it after the transaction's first write, which holds the lock.
func (t *Tx) AgentSpan(ctx context.Context, agentID string) (started, stopped sql.NullInt64, err error) {
	err = t.tx.QueryRowContext(ctx, "SELECT started, stopped FROM agents WHERE agent_id = ?", agentID).Scan(&started, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return started, stopped, nil
	}
	if err != nil {
		return started, stopped, fmt.Errorf("callmeter store %s: read the start and stop of agent %q: %w", t.path, agentID, err)
	}
	return started, stopped, nil
}

// RefreshSession recomputes the derived columns of the sessions row of
// sessionID from the rows stored: start_source is the source of its earliest
// SessionStart, end_reason the reason of its latest SessionEnd, and model that
// of its latest main-chat request with a model, else of its latest SessionStart
// carrying one. A session TouchSession never wrote is an error.
func (t *Tx) RefreshSession(ctx context.Context, sessionID string) error {
	result, err := t.tx.ExecContext(ctx,
		`UPDATE sessions SET
			start_source = (SELECT source FROM events WHERE session_id = ?1 AND event = ?2 ORDER BY ts, event_id LIMIT 1),
			end_reason = (SELECT reason FROM events WHERE session_id = ?1 AND event = ?3 ORDER BY ts DESC, event_id DESC LIMIT 1),
			model = COALESCE(
				(SELECT model FROM requests WHERE session_id = ?1 AND agent_id IS NULL AND model IS NOT NULL ORDER BY ts DESC, request_id DESC LIMIT 1),
				(SELECT model FROM events WHERE session_id = ?1 AND event = ?2 AND model IS NOT NULL ORDER BY ts DESC, event_id DESC LIMIT 1))
		WHERE session_id = ?1`,
		sessionID, EventSessionStart, EventSessionEnd)
	if err != nil {
		return fmt.Errorf("callmeter store %s: refresh session %q: %w", t.path, sessionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("callmeter store %s: count the refreshed rows of session %q: %w", t.path, sessionID, err)
	}
	if n == 0 {
		return fmt.Errorf("callmeter store %s: refresh session %q: no sessions row, TouchSession comes first", t.path, sessionID)
	}
	return nil
}
