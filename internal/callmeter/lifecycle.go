package callmeter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The hook events whose rows the derived tables read.
const (
	EventSubagentStart = "SubagentStart"
	EventSubagentStop  = "SubagentStop"
	EventSessionStart  = "SessionStart"
	EventSessionEnd    = "SessionEnd"
	EventStop          = "Stop"
	EventStopFailure   = "StopFailure"
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
	Reason         *string // a SessionEnd label, or LabelNotStored and its size
	Trigger        *string
	ErrorType      *string // a StopFailure label, or LabelNotStored and its size
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

// LabelNotStored is what events.error_type or events.reason holds, followed by
// ` (N bytes)`, for a value that is no label of its event (detailLabels): such
// a value may be free text, so only its UTF-8 byte count is stored.
const LabelNotStored = "label not stored"

// EndReasonNever is what sessions.end_reason holds for a session whose latest
// run went quiet with no SessionEnd recorded and no trace of one lost: Claude
// Code did not run its SessionEnd hooks. Quiet-session recovery writes it
// (RecoverQuiet); it is no SessionEnd label (detailLabels), so it never
// collides with a reason a hook stored. Any later hook of the session, one
// dated after its last hook, clears it (TouchSession), since the session runs
// again; a refreshing hook recomputes it as well (RefreshSession).
const EndReasonNever = "never"

// EndReasonLost is what sessions.end_reason holds for a session whose latest
// run's SessionEnd hook ran and was lost before it recorded: a binary or
// terminated fault of a SessionEnd names the session (a missed.log line of the
// wrapper or of a hook binary killed by a signal). Quiet-session recovery
// writes it (RecoverQuiet) only while end_reason is NULL; it is no SessionEnd
// label (detailLabels), and a later hook of the session clears it
// (RefreshSession), as it does EndReasonNever.
const EndReasonLost = "lost"

// labelColumn is the stored form of the string a payload of event carries
// under key: a label of detailLabels as it is, nil as nil, and any other value
// as LabelNotStored and its size.
func labelColumn(event, key string, value *string) *string {
	if value == nil || isLabel(event, key, *value) {
		return value
	}
	return Ptr(fmt.Sprintf("%s (%d bytes)", LabelNotStored, len(*value)))
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
	cs = add(cs, "reason", labelColumn(e.Event, "reason", e.Reason))
	cs = add(cs, "trigger", e.Trigger)
	cs = add(cs, "error_type", labelColumn(e.Event, "error", e.ErrorType))
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

// RecoveredDetail is the detail of a turn end rebuilt from the transcript
// because its hook was lost: a main-chat Stop or StopFailure (Claude Code
// cancels it as a headless process exits), or a sub-agent's SubagentStop
// (RecoverAgentStop). A hook's own row never carries it. A rebuilt Stop's or
// SubagentStop's turns row shares its events row's id.
const RecoveredDetail = `{"from_transcript":true}`

// latestPromptTS is the SQL ts of session's latest main-chat prompt, 0 when it
// has none; session is an SQL expression.
func latestPromptTS(session string) string {
	return `(SELECT COALESCE(MAX(ts), 0) FROM events WHERE session_id = ` + session +
		` AND event = 'UserPromptSubmit' AND COALESCE(agent_id, '') = '')`
}

// turnEnded is the SQL condition that session's main chat has a turn end, a
// Stop turn or a StopFailure event, at or after since; both are SQL
// expressions.
func turnEnded(session, since string) string {
	return `(EXISTS (SELECT 1 FROM events f WHERE f.session_id = ` + session + ` AND f.event = 'StopFailure'
			AND COALESCE(f.agent_id, '') = '' AND f.ts >= ` + since + `)
		OR EXISTS (SELECT 1 FROM turns t WHERE t.session_id = ` + session + ` AND t.event = 'Stop'
			AND COALESCE(t.agent_id, '') = '' AND t.ts >= ` + since + `))`
}

// turnEndMissing is the SQL condition that session's latest main-chat prompt
// has no turn end; session is an SQL expression.
func turnEndMissing(session string) string {
	return `(EXISTS (SELECT 1 FROM events WHERE session_id = ` + session + ` AND event = 'UserPromptSubmit'
			AND COALESCE(agent_id, '') = '')
		AND NOT ` + turnEnded(session, latestPromptTS(session)) + `)`
}

// RecoverTurnEnd inserts e, whose Event is EventStop or EventStopFailure, as
// the main chat's turn end for the session's latest prompt, rebuilt from the
// transcript (RecoveredDetail), and reports whether a row went in. A Stop is
// an events row and a turns row under one id; the turns columns only a hook
// carries stay NULL. It writes nothing when the session already has, since
// that prompt, a main-chat StopFailure or Stop (the hook's or an earlier
// recovery), or for a Stop whose transcript entry predates the prompt: that
// answer is an earlier turn's, and the latest turn's is not on disk. The row's
// id follows from the event, the session and the prompt's ts, so a repeated
// recovery is a no-op by event_id too; its ts is e.TS, never before the
// prompt's. Call it after the transaction's first write, which holds the lock.
func (t *Tx) RecoverTurnEnd(ctx context.Context, e Event) (bool, error) {
	if e.Event != EventStop && e.Event != EventStopFailure {
		return false, fmt.Errorf("callmeter store %s: recover a turn end of event %q, want Stop or StopFailure", t.path, e.Event)
	}
	if e.SessionID == nil || *e.SessionID == "" {
		return false, fmt.Errorf("callmeter store %s: recover a %s without a session_id", t.path, e.Event)
	}
	session := *e.SessionID
	var promptID sql.NullString
	var asked int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT prompt_id, ts FROM events WHERE session_id = ? AND event = 'UserPromptSubmit'
		AND COALESCE(agent_id, '') = '' ORDER BY ts DESC LIMIT 1`, session).Scan(&promptID, &asked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("callmeter store %s: read the latest prompt of session %q: %w", t.path, session, err)
	}
	if e.Event == EventStop && e.TS != 0 && e.TS < asked {
		return false, nil
	}
	var answered int
	if err := t.tx.QueryRowContext(ctx, `SELECT `+turnEnded("?1", "?2"), session, asked).Scan(&answered); err != nil {
		return false, fmt.Errorf("callmeter store %s: read the turn end of session %q: %w", t.path, session, err)
	}
	if answered != 0 {
		return false, nil
	}
	sum := sha256.Sum256([]byte("callmeter recovered " + e.Event + "\x00" + session + "\x00" + strconv.FormatInt(asked, 10)))
	e.EventID = hex.EncodeToString(sum[:])
	e.PromptID = nil
	if promptID.Valid {
		e.PromptID = &promptID.String
	}
	e.TS = max(e.TS, asked)
	e.Detail = Ptr(RecoveredDetail)
	inserted, err := t.InsertEvent(ctx, e)
	if err != nil {
		return false, fmt.Errorf("recover the %s of session %q: %w", e.Event, session, err)
	}
	if e.Event == EventStop {
		turn := Turn{EventID: e.EventID, Event: EventStop, SessionID: e.SessionID, PromptID: e.PromptID, TS: e.TS, SeatDir: e.SeatDir}
		if _, err := t.InsertTurn(ctx, turn); err != nil {
			return false, fmt.Errorf("recover the Stop turn of session %q: %w", session, err)
		}
	}
	return inserted, nil
}

// DropRecoveredTurnEnd deletes the session's rebuilt turn ends (RecoverTurnEnd)
// of the prompt a Stop or StopFailure hook delivered at hookTS answers, a
// rebuilt Stop's turns row with its events row: the hook's own row replaces
// them. Only a rebuilt main-chat row (RecoveredDetail) at or after that prompt
// goes.
func (t *Tx) DropRecoveredTurnEnd(ctx context.Context, sessionID string, hookTS int64) error {
	rebuilt := `SELECT event_id FROM events WHERE event IN ('Stop', 'StopFailure') AND session_id = ?1
		AND COALESCE(agent_id, '') = '' AND detail = ?2 AND ts >= (SELECT COALESCE(MAX(ts), 0) FROM events
			WHERE session_id = ?1 AND event = 'UserPromptSubmit' AND COALESCE(agent_id, '') = '' AND ts <= ?3)`
	for _, statement := range []string{
		`DELETE FROM turns WHERE event_id IN (` + rebuilt + `)`,
		`DELETE FROM events WHERE event_id IN (` + rebuilt + `)`,
	} {
		if _, err := t.tx.ExecContext(ctx, statement, sessionID, RecoveredDetail, hookTS); err != nil {
			return fmt.Errorf("callmeter store %s: drop the rebuilt turn end of session %q: %w", t.path, sessionID, err)
		}
	}
	return nil
}

// AgentStop is a sub-agent turn's end read from its quiet transcript
// (RecoverAgentStop): the agent's latest turn, seq, still open, the ts of the
// transcript entry that ended it, and the agent's transcript totals.
type AgentStop struct {
	AgentID string
	Seq     int64
	TS      int64 // the turn-end entry's own timestamp, Unix ms UTC
	Totals  AgentTotals
	SeatDir *string
}

// fromNull is n as a column value: nil when NULL.
func fromNull(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	return &n.String
}

// RecoverAgentStop writes stop as the SubagentStop its hook would have, rebuilt
// from the agent's transcript (RecoveredDetail) because the hook's event was
// lost, and reports whether a row went in: an events row and a turns row under
// one id, dated at the transcript entry that ended the turn and never before
// the turn's start, the turns columns only a hook carries NULL; the agent's
// turns rebuilt from its events, so the turn closes at it; the agent's stopped
// kept at the latest, its session, type and prompt filled, and, as the hook
// does at the agent's latest stop, its total_tokens and tool_uses from the
// transcript, its model filled. It writes nothing unless turn stop.Seq is still
// the agent's latest and has no stop: a stop stored since (the hook's, or an
// earlier recovery's) wins. The id follows from the agent and the turn's start,
// so a repeated recovery is a no-op by event_id too. The SubagentStop hook,
// landing later, drops it (DropRecoveredAgentStop). Call it after the
// transaction's first write, which holds the lock.
func (t *Tx) RecoverAgentStop(ctx context.Context, stop AgentStop) (bool, error) {
	if stop.AgentID == "" {
		return false, fmt.Errorf("callmeter store %s: recover a SubagentStop without an agent_id", t.path)
	}
	var sessionID, agentType, promptID sql.NullString
	var started int64
	err := t.tx.QueryRowContext(ctx,
		`SELECT session_id, agent_type, prompt_id, COALESCE(started, 0) FROM agent_turns
		WHERE agent_id = ?1 AND seq = ?2 AND stopped IS NULL
		AND seq = (SELECT MAX(x.seq) FROM agent_turns x WHERE x.agent_id = ?1)`, stop.AgentID, stop.Seq).
		Scan(&sessionID, &agentType, &promptID, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: read the open turn %d of agent %q: %w", t.path, stop.Seq, stop.AgentID, err)
	}
	sum := sha256.Sum256([]byte("callmeter recovered " + EventSubagentStop + "\x00" + stop.AgentID + "\x00" + strconv.FormatInt(started, 10)))
	ts := max(stop.TS, started)
	e := Event{EventID: hex.EncodeToString(sum[:]), Event: EventSubagentStop, TS: ts, SessionID: fromNull(sessionID),
		AgentID: Ptr(stop.AgentID), AgentType: fromNull(agentType), PromptID: fromNull(promptID),
		Detail: Ptr(RecoveredDetail), SeatDir: stop.SeatDir}
	inserted, err := t.InsertEvent(ctx, e)
	if err != nil {
		return false, fmt.Errorf("recover the SubagentStop of agent %q: %w", stop.AgentID, err)
	}
	turn := Turn{EventID: e.EventID, Event: EventSubagentStop, SessionID: e.SessionID, AgentID: e.AgentID,
		AgentType: e.AgentType, PromptID: e.PromptID, TS: ts, SeatDir: stop.SeatDir}
	if _, err := t.InsertTurn(ctx, turn); err != nil {
		return false, fmt.Errorf("recover the SubagentStop turn of agent %q: %w", stop.AgentID, err)
	}
	if err := t.RebuildAgentTurns(ctx, stop.AgentID); err != nil {
		return false, err
	}
	_, stopped, err := t.AgentSpan(ctx, stop.AgentID)
	if err != nil {
		return false, err
	}
	latest := !stopped.Valid || ts >= stopped.Int64
	for _, write := range []struct {
		agent Agent
		mode  Mode
	}{
		{Agent{AgentID: stop.AgentID, SessionID: e.SessionID, AgentType: e.AgentType, PromptID: e.PromptID}, FillEmpty},
		{Agent{AgentID: stop.AgentID, Stopped: Ptr(ts)}, KeepMax},
	} {
		if err := t.UpsertAgent(ctx, write.agent, write.mode); err != nil {
			return false, fmt.Errorf("recover the SubagentStop of agent %q: %w", stop.AgentID, err)
		}
	}
	if !latest {
		return inserted, nil // an earlier stop than the one stored keeps the stored totals
	}
	totals := Agent{AgentID: stop.AgentID, TotalTokens: Ptr(stop.Totals.TotalTokens), ToolUses: Ptr(stop.Totals.ToolUses)}
	if err := t.UpsertAgent(ctx, totals, Overwrite); err != nil {
		return false, fmt.Errorf("recover the totals of agent %q: %w", stop.AgentID, err)
	}
	if err := t.UpsertAgent(ctx, Agent{AgentID: stop.AgentID, Model: presentString(stop.Totals.Model)}, FillEmpty); err != nil {
		return false, fmt.Errorf("recover the model of agent %q: %w", stop.AgentID, err)
	}
	return inserted, nil
}

// DropRecoveredAgentStop deletes the agent's rebuilt SubagentStop
// (RecoverAgentStop) of the turn a SubagentStop hook delivered at hookTS
// ends, its turns row with its events row: the hook's own row replaces them,
// and the agent's turns are rebuilt after it. Only a rebuilt row
// (RecoveredDetail) from that turn's start, the agent's latest SubagentStart
// at or before hookTS, to its next start goes.
func (t *Tx) DropRecoveredAgentStop(ctx context.Context, agentID string, hookTS int64) error {
	rebuilt := `SELECT event_id FROM events WHERE event = 'SubagentStop' AND agent_id = ?1 AND detail = ?2
		AND ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE agent_id = ?1 AND event = 'SubagentStart' AND ts <= ?3)
		AND ts < (SELECT COALESCE(MIN(ts), 9223372036854775807) FROM events WHERE agent_id = ?1 AND event = 'SubagentStart' AND ts > ?3)`
	for _, statement := range []string{
		`DELETE FROM turns WHERE event_id IN (` + rebuilt + `)`,
		`DELETE FROM events WHERE event_id IN (` + rebuilt + `)`,
	} {
		if _, err := t.tx.ExecContext(ctx, statement, agentID, RecoveredDetail, hookTS); err != nil {
			return fmt.Errorf("callmeter store %s: drop the rebuilt SubagentStop of agent %q: %w", t.path, agentID, err)
		}
	}
	return nil
}

// InsertTurn writes turn under the clash rule of insertEarliest and reports
// whether a row went in or was replaced.
func (t *Tx) InsertTurn(ctx context.Context, turn Turn) (bool, error) {
	if turn.Event == "" {
		return false, fmt.Errorf("callmeter store %s: insert turn %q without a hook_event_name", t.path, turn.EventID)
	}
	return t.insertEarliest(ctx, "turns", "event_id", turn.EventID, turn.TS, turn.columns())
}

// RedeliveryWindow: the same payload bytes delivered within it of a stored
// occurrence are that occurrence redelivered; further apart they are a new
// occurrence (an agent woken twice in one prompt sends identical
// SubagentStart bytes seconds apart).
const RedeliveryWindow int64 = 1000 // ms

// occurrence is one stored events row of a payload's bytes: its event_id (the
// plain hash or hash:ts) and its ts, the earliest delivery's.
type occurrence struct {
	id string
	ts int64
}

// ClaimOccurrence is the event_id a delivery of the payload hashed hash, at
// ts, is stored under, with the stored rows of those bytes moved to their
// final ids first. The delivery is a redelivery of the stored occurrence
// whose ts is nearest, the earlier on a tie, when that lies within
// RedeliveryWindow, else a new occurrence. The earliest occurrence of the
// bytes holds the plain hash and every other one hash:{its earliest ts}, so
// the ids follow from the deliveries' ts, never from their arrival order: a
// delivery that makes an occurrence the earliest takes the plain hash from the
// one holding it, and an earlier redelivery moves its occurrence to its own
// ts. A move renames the occurrence's events and turns rows and the
// agent_turns edges naming them; the insert that follows replaces the moved
// row whole when this delivery is the earlier. A row an earlier build stored
// under the plain hash is an occurrence like any other. A payload carries no
// timestamp, so the hash alone cannot tell a redelivery from the same bytes
// sent again later. Every turns row shares its run's events row id, so the
// events rows list the occurrences. Call it after the transaction's first
// write, which holds the lock.
func (t *Tx) ClaimOccurrence(ctx context.Context, hash string, ts int64) (string, error) {
	if hash == "" {
		return "", fmt.Errorf("callmeter store %s: resolve an occurrence without a payload hash", t.path)
	}
	// A range on the primary key: ':' is 0x3A and ';' 0x3B, so it holds the plain
	// hash and every hash:ts id, and nothing else.
	rows, err := t.tx.QueryContext(ctx, "SELECT event_id, ts FROM events WHERE event_id >= ?1 AND event_id < ?1 || ';'", hash)
	if err != nil {
		return "", fmt.Errorf("callmeter store %s: list the occurrences of payload %q: %w", t.path, hash, err)
	}
	var stored []occurrence
	for rows.Next() {
		var o occurrence
		if err := rows.Scan(&o.id, &o.ts); err != nil {
			return "", errors.Join(fmt.Errorf("callmeter store %s: scan an occurrence of payload %q: %w", t.path, hash, err), rows.Close())
		}
		stored = append(stored, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", fmt.Errorf("callmeter store %s: read the occurrences of payload %q: %w", t.path, hash, err)
	}

	own, ownDistance := -1, int64(0)
	for i, o := range stored {
		distance := max(o.ts-ts, ts-o.ts)
		if distance > RedeliveryWindow {
			continue
		}
		if own < 0 || distance < ownDistance || (distance == ownDistance && o.ts < stored[own].ts) {
			own, ownDistance = i, distance
		}
	}
	ownTS := ts
	if own >= 0 {
		ownTS = min(ownTS, stored[own].ts)
	}
	earliest := true
	for i, o := range stored {
		if i != own && o.ts < ownTS {
			earliest = false
		}
	}
	id := hash + ":" + strconv.FormatInt(ownTS, 10)
	if earliest {
		id = hash
		for i, o := range stored {
			if i != own && o.id == hash {
				if err := t.moveOccurrence(ctx, hash, hash+":"+strconv.FormatInt(o.ts, 10)); err != nil {
					return "", err
				}
			}
		}
	}
	if own >= 0 && stored[own].id != id {
		if err := t.moveOccurrence(ctx, stored[own].id, id); err != nil {
			return "", err
		}
	}
	return id, nil
}

// moveOccurrence renames the occurrence stored under from to to: its events
// and turns rows, and the agent_turns edges that name it.
func (t *Tx) moveOccurrence(ctx context.Context, from, to string) error {
	for _, statement := range []string{
		"UPDATE events SET event_id = ?2 WHERE event_id = ?1",
		"UPDATE turns SET event_id = ?2 WHERE event_id = ?1",
		"UPDATE agent_turns SET start_event_id = ?2 WHERE start_event_id = ?1",
		"UPDATE agent_turns SET stop_event_id = ?2 WHERE stop_event_id = ?1",
	} {
		if _, err := t.tx.ExecContext(ctx, statement, from, to); err != nil {
			return fmt.Errorf("callmeter store %s: move occurrence %q to %q (%s): %w", t.path, from, to, statement, err)
		}
	}
	return nil
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
// does not carry is left to the others. A run dated after the stored last_ts
// clears an EndReasonNever mark, since the quiet session runs again; a late
// run, dated at or before it, leaves the mark. The derived columns are
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
		fmt.Sprintf("end_reason = CASE WHEN sessions.end_reason = '%s' AND excluded.last_ts > sessions.last_ts "+
			"THEN NULL ELSE sessions.end_reason END", EndReasonNever),
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

// sessionModel is the SQL model of the session named by the expression
// session: that of its latest main-chat request with a model, else of its
// latest SessionStart carrying one.
func sessionModel(session string) string {
	return `COALESCE(
				(SELECT model FROM requests WHERE session_id = ` + session + ` AND agent_id IS NULL AND model IS NOT NULL ORDER BY ts DESC, request_id DESC LIMIT 1),
				(SELECT model FROM events WHERE session_id = ` + session + ` AND event = '` + EventSessionStart + `' AND model IS NOT NULL ORDER BY ts DESC, event_id DESC LIMIT 1))`
}

// RefreshSessionModel recomputes only the session's model (sessionModel),
// keeping the stored one when the rows name none: report-time recovery writes
// main-chat requests no Stop or SessionEnd hook will refresh the session from,
// and it must not touch end_reason, which RefreshSession recomputes from
// SessionEnd events alone.
func (t *Tx) RefreshSessionModel(ctx context.Context, sessionID string) error {
	if _, err := t.tx.ExecContext(ctx, `UPDATE sessions SET model = COALESCE(`+sessionModel("?1")+`, model) WHERE session_id = ?1`, sessionID); err != nil {
		return fmt.Errorf("callmeter store %s: refresh the model of session %q: %w", t.path, sessionID, err)
	}
	return nil
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
			model = `+sessionModel("?1")+`
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
