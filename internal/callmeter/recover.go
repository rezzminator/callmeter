package callmeter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Retention is the age window recovery keeps after old store rows are pruned.
const Retention = 30 * 24 * time.Hour

// ResolveTranscript keeps an existing stored path, otherwise finding the first
// sorted session transcript under the stored projects root, then the seat's.
// An unfound path is returned unchanged so callers retain their read errors.
func ResolveTranscript(stored, seatDir, sessionID string) string {
	if stored != "" {
		if _, err := os.Stat(stored); err == nil {
			return stored
		}
	}
	var roots []string
	if stored != "" {
		roots = append(roots, filepath.Dir(filepath.Dir(stored)))
	}
	if seatDir != "" {
		roots = append(roots, filepath.Join(seatDir, "projects"))
	}
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*", sessionID+".jsonl"))
		if len(matches) != 0 {
			return matches[0]
		}
	}
	return stored
}

// QuietAfter is how long a session must have gone with no hook and no
// transcript write before RecoverQuiet settles it from its transcripts. A live
// session can sit with a call in flight for hours (a permission prompt, a long
// tool), but every value recovery writes is final once on disk: a tool_result
// line is written once, and an assistant message's last line carries its final
// usage. An hour with no transcript write and no hook rules out a message still
// streaming, since an API stream never runs that long. A session that resumes
// later is settled again by its own Stop and SessionEnd sweep, which overwrite
// what recovery only filled.
const QuietAfter = time.Hour

// RecoverSummary is what one RecoverQuiet pass wrote.
type RecoverSummary struct {
	Sessions int // sessions recovered with at least one write
	Calls    int // calls settled from a tool_result
	Requests int // request rows written, filled or resolved
	TurnEnds int // main-chat turn ends rebuilt from a transcript (RecoverTurnEnd)
	// AgentStops counts the sub-agent turn ends rebuilt from a transcript
	// (RecoverAgentStop).
	AgentStops int
	NoEnds     int // sessions whose latest run got end_reason EndReasonNever (MarkNoSessionEnd)
	LostEnds   int // sessions whose latest run got end_reason EndReasonLost (MarkLostSessionEnd)
	// Unfillable counts the transcript faults that mark an agent turn, a Stop, a
	// prompt with no turn end or an open call that recovery read its transcripts
	// for and could not fill.
	Unfillable int
	// Rebuilt counts the call rows written from a transcript tool_use no hook
	// recorded (RebuiltCall).
	Rebuilt int
	// Parents counts agent rows given their parent Agent call or type from
	// the sub-agent's meta file, in every session, quiet or not.
	Parents int
	// Skipped holds the per-session transcript errors, each a *SkippedRead
	// naming the session and the path; the rest of the pass went on. One
	// transcript read more than once can be skipped more than once.
	Skipped []error
}

// SkippedRead is one Skipped entry: a transcript of Session at Path that
// recovery could not use, and why.
type SkippedRead struct {
	Session, Path string
	Err           error
}

func (e *SkippedRead) Error() string {
	return fmt.Sprintf("session %s: %s: %v", e.Session, e.Path, e.Err)
}

func (e *SkippedRead) Unwrap() error { return e.Err }

// The tails of the transcript faults RecoverQuiet records for what it read in
// full and could not fill (agentTurnMarker, stopReplyMarker, turnEndMarker,
// callMarker); scripts/reconcile explains them by its own parse of the same
// transcripts, and a test there pins its copy of these four texts to them.
const (
	UnfilledAgentTurn = "no request in its span, and its quiet transcript holds none at or after the session's first hook"
	UnfilledStopReply = "no final reply request, and its quiet main transcript holds none at or after the session's first hook"
	UnfilledTurnEnd   = "no Stop or StopFailure, and its quiet main transcript shows no turn end after it"
	UnfilledCall      = "no size or no request, and no quiet transcript of the session holds its result or, at or after the session's first hook, its request"
	UnfilledAgentStop = "no SubagentStop, and its quiet transcript shows no turn end at or after the turn's start"
)

// sqlText is s as an SQL string literal.
func sqlText(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// agentTurnMarker is the SQL text of the fault that marks the agent turn row
// aliased t unfillable; one source for the candidate query and the marking
// insert, and for the Go text a test pins it against.
func agentTurnMarker(t string) string {
	return fmt.Sprintf(`'agent ' || %[1]s.agent_id || ' turn stopped ' || %[1]s.stopped || ': ' || %[2]s`, t, sqlText(UnfilledAgentTurn))
}

// stopReplyMarker is the SQL text of the fault that marks the turns row
// aliased t unfillable.
func stopReplyMarker(t string) string {
	return fmt.Sprintf(`'prompt ' || %[1]s.prompt_id || ' Stop ' || %[1]s.event_id || ': ' || %[2]s`, t, sqlText(UnfilledStopReply))
}

// turnEndMarker is the SQL text of the fault that marks the main-chat
// UserPromptSubmit event row aliased p as a prompt whose turn end the quiet main
// transcript does not show.
func turnEndMarker(p string) string {
	return fmt.Sprintf(`'prompt ' || COALESCE(%[1]s.prompt_id, '-') || ' UserPromptSubmit ' || %[1]s.event_id || ': ' || %[2]s`, p, sqlText(UnfilledTurnEnd))
}

// callMarker is the SQL text of the fault that marks the calls row aliased c
// as a call recovery could not settle.
func callMarker(c string) string {
	return fmt.Sprintf(`'call ' || %[1]s.tool_use_id || ': ' || %[2]s`, c, sqlText(UnfilledCall))
}

// agentStopMarker is the SQL text of the fault that marks the agent_turns row
// aliased t as a turn with no SubagentStop whose quiet transcript shows no end.
func agentStopMarker(t string) string {
	return fmt.Sprintf(`'agent ' || %[1]s.agent_id || ' turn ' || %[1]s.seq || ' open: ' || %[2]s`, t, sqlText(UnfilledAgentStop))
}

// agentStopUnmarked is the SQL condition that the agent_turns row aliased t is
// its agent's latest turn, has no stop, and recovery has not marked it
// (agentStopMarker) since the session's last hook: as for turnEndUnmarked, any
// later hook reopens it.
func agentStopUnmarked(t string) string {
	return fmt.Sprintf(`(%[1]s.stopped IS NULL
		AND %[1]s.seq = (SELECT MAX(x.seq) FROM agent_turns x WHERE x.agent_id = %[1]s.agent_id)
		AND NOT EXISTS (SELECT 1 FROM faults f WHERE f.session_id = %[1]s.session_id AND f.stage = '%[2]s'
			AND f.ts >= COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = %[1]s.session_id), 0)
			AND f.error = %[3]s))`, t, StageTranscript, agentStopMarker(t))
}

// openAgentTurn is an agent turn agentStopUnmarked names.
type openAgentTurn struct {
	agentID string
	seq     int64
	started int64 // 0: NULL
}

// unmarkedOpenAgentTurns lists the session's agent turns agentStopUnmarked names.
func (s *Store) unmarkedOpenAgentTurns(ctx context.Context, sessionID string) ([]openAgentTurn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.agent_id, t.seq, COALESCE(t.started, 0) FROM agent_turns t WHERE t.session_id = ?1 AND `+agentStopUnmarked("t")+`
		ORDER BY t.agent_id, t.seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list the open agent turns of session %q: %w", s.path, sessionID, err)
	}
	var turns []openAgentTurn
	for rows.Next() {
		var turn openAgentTurn
		if err := rows.Scan(&turn.agentID, &turn.seq, &turn.started); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: scan an open agent turn of session %q: %w", s.path, sessionID, err), rows.Close())
		}
		turns = append(turns, turn)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read the open agent turns of session %q: %w", s.path, sessionID, err)
	}
	return turns, nil
}

// MarkAgentStopsUnfillable records, as one transcript fault each, that every
// turn of turns agentStopUnmarked still names, read under the transaction's
// lock, has no SubagentStop and no turn end in its quiet transcript, and
// reports how many faults it wrote. The fault's ts is the session's last hook
// when it was marked, as for MarkTurnEndUnfillable. Call it only once every
// transcript of the session was read in full, after the transaction's first
// write. It stores ids and times, never a transcript's text.
func (t *Tx) MarkAgentStopsUnfillable(ctx context.Context, sessionID string, turns []openAgentTurn) (int, error) {
	total := 0
	for _, turn := range turns {
		result, err := t.tx.ExecContext(ctx,
			`INSERT INTO faults (ts, session_id, stage, error)
			SELECT COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = ?1), t.started, 0), t.session_id, '`+StageTranscript+`', `+agentStopMarker("t")+`
			FROM agent_turns t WHERE t.session_id = ?1 AND t.agent_id = ?2 AND t.seq = ?3 AND `+agentStopUnmarked("t"), sessionID, turn.agentID, turn.seq)
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: mark the open turn %d of agent %q: %w", t.path, turn.seq, turn.agentID, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: count the marked open turn %d of agent %q: %w", t.path, turn.seq, turn.agentID, err)
		}
		total += int(n)
	}
	return total, nil
}

// callOpen is the SQL condition that the calls row aliased c holds something
// recovery could settle: no size at all (neither bytes_real nor
// bytes_delivered, unless a no-real-output outcome landed), no request,
// a provisional one, or a rebuilt call (source
// transcript, not failed) whose real size is still unknown, as an earlier
// recovery left it before it measured the result's toolUseResult.
func callOpen(c string) string {
	return fmt.Sprintf(`((%[4]s AND %[1]s.bytes_delivered IS NULL)
		OR %[1]s.request_id IS NULL OR %[1]s.request_id LIKE %[2]s
		OR (%[1]s.source = %[3]s AND %[4]s AND COALESCE(%[1]s.failed, 0) = 0))`, c, sqlText(PendingPrefix+"%"), sqlText(SourceTranscript), realSizeOpen(c))
}

// callUnmarked is the SQL condition that recovery has not marked the calls row
// aliased c (callMarker) since the session's last hook: as for turnEndUnmarked,
// the fault's ts is the last hook at marking, so it holds by equality, and any
// later hook (a permission prompt answered, a tool that did return) reopens the
// call.
func callUnmarked(c string) string {
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM faults f WHERE f.session_id = %[1]s.session_id AND f.stage = '%[2]s'
			AND f.tool_use_id = %[1]s.tool_use_id
			AND f.ts >= COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = %[1]s.session_id), 0)
			AND f.error = %[3]s)`, c, StageTranscript, callMarker(c))
}

// agentTurnUnfilled is the SQL condition that the agent_turns row aliased t has
// a stop and no request of its agent in its span, (the agent's previous stop,
// its own stop], and that no transcript fault marks it unfillable.
func agentTurnUnfilled(t string) string {
	return fmt.Sprintf(`(%[1]s.stopped IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM requests r WHERE r.session_id = %[1]s.session_id AND r.agent_id = %[1]s.agent_id
			AND r.ts <= %[1]s.stopped AND r.ts > COALESCE(
				(SELECT MAX(p.stopped) FROM agent_turns p WHERE p.agent_id = %[1]s.agent_id AND p.seq < %[1]s.seq), 0))
		AND NOT EXISTS (SELECT 1 FROM faults f WHERE f.session_id = %[1]s.session_id AND f.stage = '%[2]s'
			AND f.error = %[3]s))`, t, StageTranscript, agentTurnMarker(t))
}

// stopReplyUnfilled is the SQL condition that the turns row aliased t is a
// main-chat Stop of a prompt that ended with a reply (a size above 0) and has
// no final reply request, a main-chat request stamped with that prompt whose
// stop_reason is not tool_use, and that no transcript fault marks it
// unfillable.
func stopReplyUnfilled(t string) string {
	return fmt.Sprintf(`(%[1]s.event = '%[2]s' AND COALESCE(%[1]s.agent_id, '') = '' AND %[1]s.prompt_id IS NOT NULL
		AND COALESCE(%[1]s.last_assistant_message_bytes, 0) > 0
		AND NOT EXISTS (SELECT 1 FROM requests r WHERE r.session_id = %[1]s.session_id AND COALESCE(r.agent_id, '') = ''
			AND r.prompt_id = %[1]s.prompt_id AND COALESCE(r.stop_reason, '') <> 'tool_use')
		AND NOT EXISTS (SELECT 1 FROM faults f WHERE f.session_id = %[1]s.session_id AND f.stage = '%[3]s'
			AND f.error = %[4]s))`, t, EventStop, StageTranscript, stopReplyMarker(t))
}

// turnEndUnmarked is the SQL condition that the latest main-chat prompt of the
// session named by the SQL expression session has no turn end (turnEndMissing)
// and that recovery has not marked it: no transcript fault holds the turnEndMarker
// text of that prompt. The marker holds only for the latest prompt (a later
// prompt has another event_id, so a resumed session is a candidate again) and
// only while no hook came after it (f.ts >= last_ts; the fault's ts is the last
// hook at marking, so it holds by equality): any later hook means the
// session lived past the mark, so it is read again, the safe side for a session
// that was quiet but still live (waiting at a permission prompt, say).
func turnEndUnmarked(session string) string {
	return `(` + turnEndMissing(session) + `
		AND NOT EXISTS (SELECT 1 FROM faults f WHERE f.session_id = ` + session + ` AND f.stage = '` + StageTranscript + `'
			AND f.ts >= COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = ` + session + `), 0)
			AND f.error = (SELECT ` + turnEndMarker("p") + ` FROM events p WHERE p.session_id = ` + session + ` AND p.event = 'UserPromptSubmit'
				AND COALESCE(p.agent_id, '') = '' ORDER BY p.ts DESC, p.event_id DESC LIMIT 1)))`
}

// unfilledPending reports whether the session holds an agent turn or a Stop
// that agentTurnUnfilled or stopReplyUnfilled still names.
func (s *Store) unfilledPending(ctx context.Context, sessionID string) (bool, error) {
	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_turns t WHERE t.session_id = ?1 AND `+agentTurnUnfilled("t")+`)
			OR EXISTS (SELECT 1 FROM turns t WHERE t.session_id = ?1 AND `+stopReplyUnfilled("t")+`)`, sessionID).Scan(&pending); err != nil {
		return false, fmt.Errorf("callmeter store %s: read the unfilled turns of session %q: %w", s.path, sessionID, err)
	}
	return pending != 0, nil
}

// MarkUnfillable records, as one transcript fault each at the turn's own time,
// every agent turn and Stop of the session that agentTurnUnfilled and
// stopReplyUnfilled still name, and reports how many. Call it only once every
// transcript of the session was read in full and the requests they fill were
// written, after the transaction's first write, which holds the lock: what the
// marker says is that the transcripts hold nothing more to fill. It stores ids
// and times, never a transcript's text.
func (t *Tx) MarkUnfillable(ctx context.Context, sessionID string) (int, error) {
	total := 0
	for _, insert := range []struct{ what, query string }{
		{"agent turn", `INSERT INTO faults (ts, session_id, stage, error)
			SELECT t.stopped, t.session_id, '` + StageTranscript + `', ` + agentTurnMarker("t") + `
			FROM agent_turns t WHERE t.session_id = ?1 AND ` + agentTurnUnfilled("t")},
		{"Stop", `INSERT INTO faults (ts, session_id, stage, error)
			SELECT t.ts, t.session_id, '` + StageTranscript + `', ` + stopReplyMarker("t") + `
			FROM turns t WHERE t.session_id = ?1 AND ` + stopReplyUnfilled("t")},
	} {
		result, err := t.tx.ExecContext(ctx, insert.query, sessionID)
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: mark the unfillable %ss of session %q: %w", t.path, insert.what, sessionID, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: count the unfillable %ss of session %q: %w", t.path, insert.what, sessionID, err)
		}
		total += int(n)
	}
	return total, nil
}

// MarkTurnEndUnfillable records, as one transcript fault, that the session's
// latest main-chat prompt has no turn end and the quiet main transcript shows
// none after it, when turnEndUnmarked still holds, read under the transaction's
// lock, and reports how many faults it wrote (0 or 1). The fault's ts is the
// session's last hook when it was marked (the prompt's own ts for a session row
// with none), so it sits inside the session's own span, never at the report's
// time. Call it only after the main transcript was read and shows no turn end
// for that prompt that RecoverTurnEnd could store, after the transaction's first
// write. It stores ids and times, never a transcript's text.
func (t *Tx) MarkTurnEndUnfillable(ctx context.Context, sessionID string) (int, error) {
	result, err := t.tx.ExecContext(ctx,
		`INSERT INTO faults (ts, session_id, stage, error)
		SELECT COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = ?1), p.ts), p.session_id, '`+StageTranscript+`', `+turnEndMarker("p")+`
		FROM events p WHERE p.session_id = ?1 AND p.event = 'UserPromptSubmit' AND COALESCE(p.agent_id, '') = ''
		AND `+turnEndUnmarked("?1")+`
		ORDER BY p.ts DESC, p.event_id DESC LIMIT 1`, sessionID)
	if err != nil {
		return 0, fmt.Errorf("callmeter store %s: mark the unended prompt of session %q: %w", t.path, sessionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("callmeter store %s: count the marked unended prompt of session %q: %w", t.path, sessionID, err)
	}
	return int(n), nil
}

// openCall is a call that callOpen and callUnmarked name; sizeOpen: neither
// bytes_real nor bytes_delivered is set, so its result is what would settle it.
type openCall struct {
	id       string
	sizeOpen bool
}

// unmarkedOpenCalls lists the session's calls that callOpen and callUnmarked
// name.
func (s *Store) unmarkedOpenCalls(ctx context.Context, sessionID string) ([]openCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.tool_use_id, c.bytes_real IS NULL AND c.bytes_delivered IS NULL FROM calls c
		WHERE c.session_id = ?1 AND `+callOpen("c")+` AND `+callUnmarked("c")+`
		ORDER BY c.tool_use_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list the open calls of session %q: %w", s.path, sessionID, err)
	}
	var calls []openCall
	for rows.Next() {
		var c openCall
		if err := rows.Scan(&c.id, &c.sizeOpen); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: scan an open call of session %q: %w", s.path, sessionID, err), rows.Close())
		}
		calls = append(calls, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read the open calls of session %q: %w", s.path, sessionID, err)
	}
	return calls, nil
}

// MarkCallsUnfillable records, as one transcript fault naming the call each,
// that every call of ids that callOpen and callUnmarked still name, read under
// the transaction's lock after this pass's writes, has nothing on disk to settle
// it, and reports how many faults it wrote. The fault's ts is the session's last
// hook when it was marked (the call's own ts for a session row with none), as
// for MarkTurnEndUnfillable. Call it only once every transcript of the session
// was read in full and none holds a result for a call of ids, after the
// transaction's first write. It stores ids and times, never a transcript's text.
func (t *Tx) MarkCallsUnfillable(ctx context.Context, sessionID string, ids []string) (int, error) {
	total := 0
	for _, id := range ids {
		result, err := t.tx.ExecContext(ctx,
			`INSERT INTO faults (ts, session_id, tool_use_id, stage, error)
			SELECT COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = ?1), c.ts, 0), c.session_id, c.tool_use_id, '`+StageTranscript+`', `+callMarker("c")+`
			FROM calls c WHERE c.session_id = ?1 AND c.tool_use_id = ?2 AND `+callOpen("c")+` AND `+callUnmarked("c"), sessionID, id)
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: mark the open call %q of session %q: %w", t.path, id, sessionID, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("callmeter store %s: count the marked open call %q of session %q: %w", t.path, id, sessionID, err)
		}
		total += int(n)
	}
	return total, nil
}

// quietSession is one RecoverQuiet candidate: a sessions row.
type quietSession struct {
	id         string
	transcript string
	seatDir    *string
	configDir  *string
}

// pendingRead is one chat's or sub-agent's pending requests with the requests
// its transcript holds for their calls.
type pendingRead struct {
	agentID string
	pending []PendingRequest
	found   map[string]RequestUsage
}

// requestWrite is one transcript request to write, and the calls it issued.
type requestWrite struct {
	request Request
	ids     []string
}

// RecoverQuiet settles, from their transcripts, the in-flight calls and the
// missing requests of every session that has gone quiet: no hook for quiet
// (sessions.last_ts) and no write to its main or sub-agent transcripts for
// quiet (file mtimes). A hook that never ran again, a session killed with its
// calls in flight, a sub-agent outliving its chat, leaves them otherwise
// unknown for good.
// A light pass first fills missing agent parents and types from their meta
// files in every session, quiet or not, without reading the transcripts.
//
// What it writes is what a SessionEnd sweep reads, by the same code
// (CallTranscripts, SettledCall, ApplyUsage, ResolvePendingFrom): pending
// requests resolved from the transcript, each unfinished call with no stored
// size settled from its tool_result, each request the transcript holds written
// at its final usage. Every write only fills (FillEmpty, RecoverRequest), so a
// value a hook stored is never overwritten, and a request row it creates carries
// SourceTranscript (a call row keeps the source of the hook that created it). A
// latest prompt with no turn end gets the one its transcript shows, rebuilt as
// SessionEnd does (TranscriptTurnEnd, RecoverTurnEnd): a headless exit cancels
// the async Stop hook, and Claude Code may end a session without running its
// SessionEnd hooks, or kill it before. A session whose latest run has no
// SessionEnd, and no trace of a lost one, gets end_reason EndReasonNever
// (MarkNoSessionEnd), so the absence is a fact and never a silent NULL; one
// whose own lost SessionEnd a fault names gets EndReasonLost
// (MarkLostSessionEnd), so the loss is a fact too; both hold for a session
// whose transcript is gone too. It stores sizes, flags and
// labels, never a transcript's text. recovery never invents a ts.
//
// Idempotent: a second pass over the same store writes nothing and counts 0
// calls and requests, because a settled call is no longer unfinished and a
// request is counted only when a write would fill a column, create the row or
// point a call. A session whose transcript is gone is skipped without a word:
// its batch or stop already recorded that fault.
//
// An agent turn with no request in its span, or a main-chat Stop with a reply
// size and no final reply request, is a candidate too, and recovery fills it
// from the transcripts like any request. When every transcript was read in full
// and the turn is still unfilled (the transcripts hold nothing at or after the
// session's first hook that fills it), recovery records that once as a
// transcript fault (MarkUnfillable) and the turn stops being a candidate, so
// later reports never read it again. A latest prompt with no turn end is marked
// the same way once the main transcript was read and shows none that
// RecoverTurnEnd could store (MarkTurnEndUnfillable): the session stays out of
// the candidates until a later prompt or any later hook (turnEndUnmarked). A
// call still open once every transcript was read in full, with no result for it
// in any transcript of the session (the tool never returned, or its untyped
// agent wrote none) and no request naming it that recovery could store, is
// marked the same way (MarkCallsUnfillable): it stays out of the candidates
// until any later hook (callUnmarked). An agent's latest turn with no stop
// whose transcript, read in full, shows its end gets the SubagentStop its hook
// lost, rebuilt at that end (agentStopsToMark, RecoverAgentStop), or at its
// main-transcript task notice when no turn end exists. A turn with neither
// is marked (MarkAgentStopsUnfillable). A live session, or one with a transcript
// that could not be read in full, is never marked.
//
// The store holds one connection, so the candidates are read and closed before
// any transcript is read, and each session's reads precede its one write.
func (s *Store) RecoverQuiet(ctx context.Context, now time.Time, quiet time.Duration) (RecoverSummary, error) {
	var summary RecoverSummary
	if err := s.fillAgentMeta(ctx, &summary); err != nil {
		return summary, err
	}
	cutoff := now.Add(-quiet)
	candidates, err := s.quietCandidates(ctx, cutoff.UnixMilli())
	if err != nil {
		return summary, err
	}
	for _, session := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, fmt.Errorf("callmeter store %s: recover quiet sessions: %w", s.path, err)
		}
		if err := s.recoverSession(ctx, session, now, cutoff, &summary); err != nil {
			return summary, fmt.Errorf("recover session %s: %w", session.id, err)
		}
	}
	return summary, nil
}

// quietCandidates lists the sessions whose last hook is before beforeMS, that
// name a transcript, and that hold something recovery could settle: a call with
// no size at all or no request, or on a provisional request, until recovery has
// read the transcripts in full and marked it (callOpen, callUnmarked), a pending request
// row, a sub-agent turn with a stop and no request of that agent since its
// previous stop (every turn makes one, so its stop read the transcript before
// the turn's reply was on disk and no later hook of the agent read it again;
// agentTurnUnfilled), a main-chat Stop whose final reply request was never
// stored (stopReplyUnfilled), each until recovery has read the transcripts in
// full and marked it unfillable (MarkUnfillable), an agent's latest turn with
// no stop until recovery settled it from its transcript (RecoverAgentStop) or
// marked it (agentStopUnmarked), a latest prompt with no turn
// end until recovery marked it (turnEndUnmarked), or a latest run with no
// SessionEnd (endMissing) or a lost one of its own still unmarked (endLost).
func (s *Store) quietCandidates(ctx context.Context, beforeMS int64) ([]quietSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.session_id, s.transcript_path, s.seat_dir, s.config_dir FROM sessions s
		WHERE s.transcript_path IS NOT NULL AND s.transcript_path <> ''
		AND COALESCE(s.last_ts, 0) < ?1
		AND (EXISTS (SELECT 1 FROM calls c WHERE c.session_id = s.session_id AND `+callOpen("c")+` AND `+callUnmarked("c")+`)
			OR EXISTS (SELECT 1 FROM requests r WHERE r.session_id = s.session_id AND r.pending = 1)
			OR EXISTS (SELECT 1 FROM agent_turns t WHERE t.session_id = s.session_id AND `+agentTurnUnfilled("t")+`)
			OR EXISTS (SELECT 1 FROM turns t WHERE t.session_id = s.session_id AND `+stopReplyUnfilled("t")+`)
			OR EXISTS (SELECT 1 FROM agent_turns t WHERE t.session_id = s.session_id AND `+agentStopUnmarked("t")+`)
			OR `+turnEndUnmarked("s.session_id")+`
			OR `+endMissing("s.session_id")+`
			OR `+endLost("s.session_id")+`)
		ORDER BY s.session_id`, beforeMS)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list quiet sessions: %w", s.path, err)
	}
	var sessions []quietSession
	for rows.Next() {
		var session quietSession
		var seat, config sql.NullString
		if err := rows.Scan(&session.id, &session.transcript, &seat, &config); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: scan quiet session: %w", s.path, err), rows.Close())
		}
		if seat.Valid {
			session.seatDir = Ptr(seat.String)
		}
		if config.Valid {
			session.configDir = Ptr(config.String)
		}
		sessions = append(sessions, session)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read quiet sessions: %w", s.path, err)
	}
	return sessions, nil
}

// recoverSession reads one candidate's transcripts and writes what they settle,
// adding to summary. A transcript read that fails is a Skipped entry and the
// other reads go on; a store error is returned.
func (s *Store) recoverSession(ctx context.Context, session quietSession, now, cutoff time.Time, summary *RecoverSummary) error {
	seatDir := ""
	if session.seatDir != nil {
		seatDir = *session.seatDir
	}
	session.transcript = ResolveTranscript(session.transcript, seatDir, session.id)
	skip := func(path string, err error) {
		summary.Skipped = append(summary.Skipped, &SkippedRead{Session: session.id, Path: path, Err: err})
	}
	// Liveness, on disk: any transcript written since the cutoff means the
	// session is still running (or its message still streaming).
	subagents, err := SubagentTranscripts(session.transcript)
	if err != nil {
		skip(session.transcript, err)
		return nil
	}
	files := []string{session.transcript}
	for _, agentID := range subagents {
		files = append(files, SubagentTranscriptPath(session.transcript, agentID))
	}
	for i, file := range files {
		info, err := os.Stat(file)
		if errors.Is(err, fs.ErrNotExist) {
			if i == 0 {
				// No transcript: its batch or stop already recorded that. The
				// session is quiet by its hooks alone, and its end still settles.
				return s.markEnd(ctx, session.id, summary)
			}
			continue
		}
		if err != nil {
			skip(file, fmt.Errorf("stat transcript: %w", err))
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
	}

	// Reads, in the order SessionEnd runs them: pending requests, unfinished
	// calls, then every request.
	var pendingReads []pendingRead
	readPending := func(agentID, transcript string) error {
		pending, err := s.PendingRequests(ctx, session.id, agentID)
		if err != nil || len(pending) == 0 {
			return err
		}
		var ids []string
		for _, request := range pending {
			ids = append(ids, request.CallIDs...)
		}
		found, err := FindRequests(transcript, ids)
		if err != nil {
			skip(transcript, err)
			return nil
		}
		pendingReads = append(pendingReads, pendingRead{agentID: agentID, pending: pending, found: found})
		return nil
	}
	if err := readPending("", session.transcript); err != nil {
		return err
	}
	pendingAgents, err := s.PendingAgents(ctx, session.id)
	if err != nil {
		return err
	}
	for _, agentID := range pendingAgents {
		path := SubagentTranscriptPath(session.transcript, agentID)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := readPending(agentID, path); err != nil {
			return err
		}
	}

	unfinished, err := s.UnfinishedCalls(ctx, session.id)
	if err != nil {
		return err
	}
	// A call with no real size is looked up when it is one only its
	// PreToolUse wrote, one whose batch stored its delivered size while its
	// PostToolUse was lost (store busy, a signal) and no Stop or SessionEnd
	// sweep settled it, or a rebuilt one still missing its real size. A batch
	// row with no ts (stored before every hook set one) is not: the sweep gives
	// such a call the hook's time, recovery has none to give, and a real size
	// alone would leave a sized call with no ts.
	var lookup []UnfinishedCall
	unfinishedOf := map[string]UnfinishedCall{}
	for _, call := range unfinished {
		if call.Delivered && call.NoTS && !call.Rebuilt {
			continue
		}
		lookup = append(lookup, call)
		unfinishedOf[call.ToolUseID] = call
	}
	var settled []Call
	settledIDs := map[string]bool{}
	transcripts, idsOf := CallTranscripts(session.transcript, lookup)
	for _, transcript := range transcripts {
		ids := idsOf[transcript]
		found, err := FindResults(transcript, ids)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			skip(transcript, err)
			continue
		}
		for _, id := range ids {
			result, ok := found[id]
			if !ok {
				continue
			}
			sized := SettledCall(id, result)
			if unfinishedOf[id].Delivered {
				// Keep its delivered size; fill a real size when available,
				// or the outcome that settles a no-real-output tool.
				if sized.BytesReal != nil || !HasRealOutput(unfinishedOf[id].Tool) {
					settled = append(settled, sized)
				}
				continue
			}
			settled = append(settled, sized)
			settledIDs[id] = true
		}
	}

	since, haveSince, err := s.SessionFirstTS(ctx, session.id)
	if err != nil {
		return err
	}
	since = max(since, now.Add(-Retention).UnixMilli())
	mainRequest := false
	resolved := map[string]bool{} // message ids a pending request was resolved to
	resolvedCount := 0
	for _, read := range pendingReads {
		for _, request := range read.pending {
			if usage, ok := resolvedBy(request, read.found); ok {
				resolved[usage.MessageID] = true
				resolvedCount++
				mainRequest = mainRequest || read.agentID == ""
			}
		}
	}
	var writes []requestWrite
	built := map[string]requestWrite{} // every request read, by message id, so a rebuilt call's request is written
	var uses []rebuildUse              // every tool_use within the session and retention window
	ends := map[string][]int64{}       // agent id -> the ts of the turn-end entry of each of its requests that ends a turn
	var notices map[string][]int64     // agent id -> main-transcript task-notification times
	metas := map[string]subagentMeta{} // agent id -> its meta file
	// complete: every transcript of the session was read in full, so what no
	// request of theirs fills stays unfilled for good (MarkUnfillable).
	complete := haveSince
	var markReads []markWrite // the marks of each transcript read, before filtering
	var markWrites []markWrite
	if haveSince {
		for i, file := range files {
			agentID := ""
			if i > 0 {
				agentID = subagents[i-1]
				agentType, parent, err := SubagentMeta(session.transcript, agentID)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					skip(file, err)
				}
				metas[agentID] = subagentMeta{agentType: agentType, parent: parent}
			}
			read, err := ReadTranscript(file, "")
			if errors.Is(err, fs.ErrNotExist) {
				complete = false
				continue
			}
			if err != nil {
				complete = false
				skip(file, err)
				continue
			}
			requests := read.Requests
			markReads = append(markReads, markWrite{agentID: agentID, marks: read.Marks})
			if i == 0 {
				notices, err = ReadTaskNotices(file)
				if err != nil {
					skip(file, err)
					complete = false
				}
			}
			for _, read := range requests {
				if agentID != "" && read.StopReason != "" && read.StopReason != toolUse {
					ends[agentID] = append(ends[agentID], max(read.EndTS, read.TS))
				}
				if read.TS >= since {
					for _, use := range read.ToolUses {
						uses = append(uses, rebuildUse{use: use, request: read, agentID: agentID})
					}
				}
				if resolved[read.MessageID] {
					continue
				}
				request := Request{
					RequestID: read.MessageID,
					SessionID: Ptr(session.id),
					PromptID:  presentString(read.PromptID),
					Pending:   Ptr(false),
					Source:    Ptr(SourceTranscript),
					ConfigDir: session.configDir,
					SeatDir:   session.seatDir,
				}
				if agentID != "" {
					request.AgentID = Ptr(agentID)
				}
				ApplyUsage(&request, read.RequestUsage)
				built[read.MessageID] = requestWrite{request: request, ids: read.ToolUseIDs}
				fills, err := s.requestFills(ctx, request, read.ToolUseIDs, since)
				if err != nil {
					return err
				}
				if fills {
					writes = append(writes, requestWrite{request: request, ids: read.ToolUseIDs})
				}
			}
		}
	}

	if markWrites, err = s.marksToWrite(ctx, session.id, since, markReads); err != nil {
		return err
	}

	end, endRead, err := s.lostTurnEnd(ctx, session.id, session.transcript, skip)
	if err != nil {
		return err
	}
	// endUnfilled: the main transcript was read for a prompt with no turn end and
	// shows none (a turn end older than the prompt is none either).
	endUnfilled := endRead && end.Event == ""
	noEnd, err := s.endIsMissing(ctx, session.id)
	if err != nil {
		return err
	}
	lostEnd, err := s.endIsLost(ctx, session.id)
	if err != nil {
		return err
	}

	unfilled := false
	var markCalls []string
	var rebuilt []Call
	var parents []Agent
	var markTurns []openAgentTurn
	var settleStops []AgentStop
	if complete {
		if unfilled, err = s.unfilledPending(ctx, session.id); err != nil {
			return err
		}
		if rebuilt, err = s.rebuiltCalls(ctx, session, uses, metas, skip); err != nil {
			return err
		}
		var rebuiltOpen []openCall
		for _, call := range rebuilt {
			// A no-real-output call settles with its outcome. Other tools
			// stay open while a successful result's real size is unknown.
			hasRealOutput := call.Tool == nil || HasRealOutput(*call.Tool)
			if call.BytesReal == nil && (call.Failed == nil || (!*call.Failed && hasRealOutput)) {
				rebuiltOpen = append(rebuiltOpen, openCall{id: call.ToolUseID, sizeOpen: call.BytesDelivered == nil})
			}
			// The request that issued it is written, so it points the call
			// and counts it.
			if write, ok := built[*call.RequestID]; ok && !slices.ContainsFunc(writes, func(w requestWrite) bool { return w.request.RequestID == *call.RequestID }) {
				writes = append(writes, write)
			}
		}
		var found map[string]Result
		if markCalls, found, err = s.callsToMark(ctx, session.id, files, settledIDs, rebuiltOpen, skip); err != nil {
			return err
		}
		for _, open := range rebuiltOpen {
			if result, ok := found[open.id]; ok {
				settled = append(settled, SettledCall(open.id, result))
			}
		}
		if parents, err = s.agentParents(ctx, session.id, metas); err != nil {
			return err
		}
		if markTurns, settleStops, err = s.agentStopsToMark(ctx, session.id, session.transcript, ends, notices, skip); err != nil {
			return err
		}
	}
	if resolvedCount == 0 && len(settled) == 0 && len(writes) == 0 && end.Event == "" && !endUnfilled && !noEnd && !lostEnd && !unfilled &&
		len(markCalls) == 0 && len(rebuilt) == 0 && len(parents) == 0 && len(markTurns) == 0 && len(settleStops) == 0 && len(markWrites) == 0 {
		return nil
	}
	rebuiltEnd, marked, lost, unfillable, agentStops := false, false, false, 0, 0
	err = s.Batch(ctx, func(tx *Tx) error {
		// A write first, so the reads below run under the lock: a deferred
		// transaction that reads first fails busy at its first write whenever
		// a hook wrote in between.
		if _, err := tx.tx.ExecContext(ctx, `UPDATE sessions SET session_id = session_id WHERE session_id = ?`, session.id); err != nil {
			return fmt.Errorf("callmeter store %s: lock session %q: %w", s.path, session.id, err)
		}
		for _, read := range pendingReads {
			if err := tx.ResolvePendingFrom(ctx, read.pending, read.found, SourceTranscript); err != nil {
				return err
			}
		}
		for _, call := range rebuilt {
			if err := tx.UpsertCall(ctx, call, FillEmpty); err != nil {
				return err
			}
		}
		for _, call := range settled {
			if err := tx.UpsertCall(ctx, call, FillEmpty); err != nil {
				return err
			}
		}
		for _, write := range writes {
			if err := tx.RecoverRequest(ctx, write.request, write.ids, since); err != nil {
				return err
			}
			mainRequest = mainRequest || write.request.AgentID == nil
		}
		if mainRequest {
			if err := tx.RefreshSessionModel(ctx, session.id); err != nil {
				return err
			}
		}
		for _, write := range markWrites {
			if err := tx.PutMarks(ctx, session.id, write.agentID, seatDir, now.UnixMilli(), write.marks); err != nil {
				return err
			}
		}
		for _, agent := range parents {
			if err := tx.UpsertAgent(ctx, agent, FillEmpty); err != nil {
				return err
			}
		}
		for _, stop := range settleStops {
			stop.SeatDir = session.seatDir
			inserted, err := tx.RecoverAgentStop(ctx, stop)
			if err != nil {
				return err
			}
			if inserted {
				agentStops++
			}
		}
		if end.Event != "" {
			var err error
			rebuiltEnd, err = tx.RecoverTurnEnd(ctx, Event{Event: end.Event, SessionID: Ptr(session.id), TS: end.TS,
				ErrorType: presentString(end.ErrorType), SeatDir: session.seatDir})
			if err != nil {
				return err
			}
		}
		var err error
		marked, lost, err = tx.markSessionEnd(ctx, session.id, noEnd, lostEnd)
		if err != nil {
			return err
		}
		if unfilled {
			if unfillable, err = tx.MarkUnfillable(ctx, session.id); err != nil {
				return err
			}
		}
		if endRead && !rebuiltEnd {
			n, err := tx.MarkTurnEndUnfillable(ctx, session.id)
			if err != nil {
				return err
			}
			unfillable += n
		}
		n, err := tx.MarkCallsUnfillable(ctx, session.id, markCalls)
		if err != nil {
			return err
		}
		unfillable += n
		if n, err = tx.MarkAgentStopsUnfillable(ctx, session.id, markTurns); err != nil {
			return err
		}
		unfillable += n
		return nil
	})
	if err != nil {
		return err
	}
	if resolvedCount == 0 && len(settled) == 0 && len(writes) == 0 && !rebuiltEnd && !marked && !lost && unfillable == 0 &&
		len(rebuilt) == 0 && len(parents) == 0 && agentStops == 0 && len(markWrites) == 0 {
		return nil
	}
	summary.AgentStops += agentStops
	summary.Rebuilt += len(rebuilt)
	summary.Parents += len(parents)
	if rebuiltEnd {
		summary.TurnEnds++
	}
	if marked {
		summary.NoEnds++
	}
	if lost {
		summary.LostEnds++
	}
	summary.Sessions++
	summary.Unfillable += unfillable
	summary.Calls += len(settled)
	summary.Requests += resolvedCount + len(writes)
	return nil
}

// markWrite is the marks one transcript holds that the store lacks, with the
// agent it belongs to ("" for the main chat).
type markWrite struct {
	agentID string
	marks   TranscriptMarks
}

// marksToWrite filters the marks each transcript of a session holds (reads) to
// the ones the store lacks: those not older than since and not stored
// (KnownMarks), and a cost-state only when it differs from the stored one, so a
// session read again with nothing new writes nothing. It reads the store, never
// a transcript, and is called before the write transaction opens. A store whose
// schema is incomplete holds none of the tables, so nothing is written to it.
func (s *Store) marksToWrite(ctx context.Context, sessionID string, since int64, reads []markWrite) ([]markWrite, error) {
	if !s.complete {
		return nil, nil
	}
	anyMarks := false
	for _, read := range reads {
		anyMarks = anyMarks || !read.marks.Empty()
	}
	if !anyMarks {
		return nil, nil
	}
	known, err := s.KnownMarks(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var writes []markWrite
	for _, read := range reads {
		marks := read.marks.Since(since, known)
		if marks.Cost != nil && read.agentID == "" {
			var cost sql.NullFloat64
			var wall, api sql.NullInt64
			err := s.db.QueryRowContext(ctx, "SELECT cost_usd, wall_ms, api_ms FROM session_costs WHERE session_id = ?", sessionID).
				Scan(&cost, &wall, &api)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("callmeter store %s: read session cost of %q: %w", s.path, sessionID, err)
			}
			c := marks.Cost
			same := err == nil &&
				cost.Valid == (c.CostUSD != nil) && (c.CostUSD == nil || cost.Float64 == *c.CostUSD) &&
				wall.Valid == (c.WallMS != nil) && (c.WallMS == nil || wall.Int64 == *c.WallMS) &&
				api.Valid == (c.APIMS != nil) && (c.APIMS == nil || api.Int64 == *c.APIMS)
			if same {
				marks.Cost = nil
			}
		} else {
			marks.Cost = nil
		}
		if !marks.Empty() {
			writes = append(writes, markWrite{agentID: read.agentID, marks: marks})
		}
	}
	return writes, nil
}

// callsToMark lists the open, unmarked calls of a session whose transcripts
// were all read in full (unmarkedOpenCalls) that MarkCallsUnfillable may mark:
// each one this pass did not settle whose result no transcript of the session
// holds (files: the main one and every sub-agent's), searched in all of them,
// since a call whose own transcript was never written (an untyped agent's) has
// nowhere else its result could be. A transcript that cannot be searched is a
// Skipped entry, and then no call is marked. A persisted output under
// tool-results/ is named by its transcript tool_result line, so a call with no
// such line has none to read.
func (s *Store) callsToMark(ctx context.Context, sessionID string, files []string, settled map[string]bool, rebuilt []openCall, skip func(string, error)) ([]string, map[string]Result, error) {
	open, err := s.unmarkedOpenCalls(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	open = append(open, rebuilt...)
	if len(open) == 0 {
		return nil, nil, nil
	}
	var sizeOpen []string
	for _, call := range open {
		if call.sizeOpen && !settled[call.id] {
			sizeOpen = append(sizeOpen, call.id)
		}
	}
	found := map[string]Result{}
	if len(sizeOpen) > 0 {
		for _, file := range files {
			results, err := FindResults(file, sizeOpen)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				skip(file, err)
				return nil, nil, nil
			}
			for id, result := range results {
				found[id] = result
			}
		}
	}
	var ids []string
	for _, call := range open {
		if _, ok := found[call.id]; !ok {
			ids = append(ids, call.id)
		}
	}
	return ids, found, nil
}

// rebuildUse is one transcript tool_use recovery read, with the request that
// issued it and the sub-agent whose transcript holds it ("" for the main chat).
type rebuildUse struct {
	use     ToolUse
	request TranscriptRequest
	agentID string
}

// subagentMeta is what a sub-agent's meta file names: its type and the
// tool_use id of the Agent call that started it.
type subagentMeta struct {
	agentType, parent string
}

// rebuiltCalls is the call row of each tool_use of uses that no calls row
// holds (a PreToolUse and PostToolUse never recorded: Claude Code records only
// a Bash call before it runs, so a session killed mid-call leaves any other
// call with no row, and a hook may be lost): RebuiltCall's row, its input only
// through SanitizeInput. An input SanitizeInput refuses is left out and named
// as a Skipped entry; the row is written all the same.
func (s *Store) rebuiltCalls(ctx context.Context, session quietSession, uses []rebuildUse, metas map[string]subagentMeta, skip func(string, error)) ([]Call, error) {
	if len(uses) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(uses))
	for _, u := range uses {
		ids = append(ids, u.use.ID)
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: encode the tool_use ids of session %q: %w", s.path, session.id, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT j.value FROM json_each(?1) j WHERE NOT EXISTS (SELECT 1 FROM calls c WHERE c.tool_use_id = j.value)`, string(list))
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list the unrecorded calls of session %q: %w", s.path, session.id, err)
	}
	missing := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: scan an unrecorded call of session %q: %w", s.path, session.id, err), rows.Close())
		}
		missing[id] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read the unrecorded calls of session %q: %w", s.path, session.id, err)
	}
	var calls []Call
	for _, u := range uses {
		if !missing[u.use.ID] {
			continue
		}
		missing[u.use.ID] = false
		call := RebuiltCall(session.id, u.agentID, metas[u.agentID].agentType, u.request, u.use.ID, u.use.Name)
		call.ConfigDir, call.SeatDir = session.configDir, session.seatDir
		if len(u.use.Input) > 0 {
			input, err := SanitizeInput(u.use.Name, u.use.Input)
			if err != nil {
				skip(session.transcript, fmt.Errorf("call %s: sanitize its transcript input: %w", u.use.ID, err))
			} else {
				call.Input = Ptr(input)
			}
		}
		calls = append(calls, call)
	}
	sizeRebuilt(session, uses, calls, skip)
	return calls, nil
}

// sizeRebuilt settles the size of each rebuilt call whose tool_result its own
// transcript holds (the main chat's for agent "", else the sub-agent's), the
// way PostToolUse would: SettledCall's delivered size, failed and outcome
// label, and for a call that did not fail its real size, RealBytes of the
// result's toolUseResult. Sizes and labels only. A call with no result stays
// unsized for the call marker; a transcript that cannot be read is named
// through skip and leaves its calls unsized.
func sizeRebuilt(session quietSession, uses []rebuildUse, calls []Call, skip func(string, error)) {
	agentOf := map[string]string{}
	for _, u := range uses {
		agentOf[u.use.ID] = u.agentID
	}
	var transcripts []string
	idsOf := map[string][]string{}
	for _, call := range calls {
		transcript := session.transcript
		if agentID := agentOf[call.ToolUseID]; agentID != "" {
			transcript = SubagentTranscriptPath(session.transcript, agentID)
		}
		if _, ok := idsOf[transcript]; !ok {
			transcripts = append(transcripts, transcript)
		}
		idsOf[transcript] = append(idsOf[transcript], call.ToolUseID)
	}
	byID := map[string]int{}
	for i, call := range calls {
		byID[call.ToolUseID] = i
	}
	for _, transcript := range transcripts {
		found, err := FindResults(transcript, idsOf[transcript])
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			skip(transcript, err)
			continue
		}
		for id, result := range found {
			sized := SettledCall(id, result)
			call := &calls[byID[id]]
			call.BytesDelivered, call.Failed, call.Error, call.BytesReal = sized.BytesDelivered, sized.Failed, sized.Error, sized.BytesReal
		}
	}
}

// RebuiltCall is the call row of a transcript tool_use no hook recorded: its
// session, sub-agent and type, the request that issued it, its prompt, the
// request's ts, its tool, the cwd of the request's entry and SourceTranscript.
// It carries no input; the caller adds SanitizeInput's.
func RebuiltCall(sessionID, agentID, agentType string, request TranscriptRequest, toolUseID, tool string) Call {
	return Call{
		ToolUseID: toolUseID,
		SessionID: Ptr(sessionID),
		AgentID:   presentString(agentID),
		AgentType: presentString(agentType),
		RequestID: Ptr(request.MessageID),
		PromptID:  presentString(request.PromptID),
		TS:        Ptr(request.TS),
		Tool:      presentString(tool),
		Cwd:       presentString(request.Cwd),
		Source:    Ptr(SourceTranscript),
	}
}

// fillAgentMeta fills existing agents from their meta files without waiting
// for their sessions to go quiet.
func (s *Store) fillAgentMeta(ctx context.Context, summary *RecoverSummary) error {
	rows, err := s.db.QueryContext(ctx, `SELECT a.agent_id, s.session_id, s.transcript_path, s.seat_dir
		FROM agents a JOIN sessions s ON s.session_id = a.session_id
		WHERE (a.parent_tool_use_id IS NULL OR a.agent_type IS NULL)
		AND s.transcript_path IS NOT NULL AND s.transcript_path <> ''
		ORDER BY a.agent_id`)
	if err != nil {
		return fmt.Errorf("callmeter store %s: list agents missing meta: %w", s.path, err)
	}
	var candidates []struct {
		id, sessionID, transcript string
		seat                      sql.NullString
	}
	for rows.Next() {
		var candidate struct {
			id, sessionID, transcript string
			seat                      sql.NullString
		}
		if err := rows.Scan(&candidate.id, &candidate.sessionID, &candidate.transcript, &candidate.seat); err != nil {
			return errors.Join(fmt.Errorf("callmeter store %s: scan an agent missing meta: %w", s.path, err), rows.Close())
		}
		candidates = append(candidates, candidate)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("callmeter store %s: read agents missing meta: %w", s.path, err)
	}
	var fills []Agent
	for _, candidate := range candidates {
		transcript := ResolveTranscript(candidate.transcript, candidate.seat.String, candidate.sessionID)
		agentType, parent, err := SubagentMeta(transcript, candidate.id)
		if err != nil || (parent == "" && agentType == "") {
			continue
		}
		fills = append(fills, Agent{AgentID: candidate.id, ParentToolUseID: presentString(parent), AgentType: presentString(agentType)})
	}
	if len(fills) == 0 {
		return nil
	}
	if err := s.Batch(ctx, func(tx *Tx) error {
		for _, fill := range fills {
			if err := tx.UpsertAgent(ctx, fill, FillEmpty); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	summary.Parents += len(fills)
	return nil
}

// agentParents is the fill of each agent row of the session that has no parent
// Agent call or no type, from its meta file (metas), the way the Agent call's
// own PostToolUse would have set them.
func (s *Store) agentParents(ctx context.Context, sessionID string, metas map[string]subagentMeta) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id FROM agents WHERE session_id = ?1 AND (parent_tool_use_id IS NULL OR agent_type IS NULL) ORDER BY agent_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list the unlinked agents of session %q: %w", s.path, sessionID, err)
	}
	var agents []Agent
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: scan an unlinked agent of session %q: %w", s.path, sessionID, err), rows.Close())
		}
		if meta := metas[id]; meta.parent != "" || meta.agentType != "" {
			agents = append(agents, Agent{AgentID: id, ParentToolUseID: presentString(meta.parent), AgentType: presentString(meta.agentType)})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: read the unlinked agents of session %q: %w", s.path, sessionID, err)
	}
	return agents, nil
}

// agentStopsToMark splits the open latest agent turns (unmarkedOpenAgentTurns)
// by what the agent's transcript, read in full, holds at or after the turn's
// start (ends): a turn end settles the turn at the earliest such end. With no
// turn end, the earliest main-transcript task notice at or after the start
// settles it instead; neither means mark, and so does a notice for an agent
// with no transcript. Both stops (AgentStop,
// RecoverAgentStop) use only the agent's own transcript totals
// (ReadAgentTotals). A transcript whose totals cannot be read is a
// Skipped entry, and its turn is neither.
func (s *Store) agentStopsToMark(ctx context.Context, sessionID, transcript string, ends, notices map[string][]int64, skip func(string, error)) (mark []openAgentTurn, settle []AgentStop, err error) {
	turns, err := s.unmarkedOpenAgentTurns(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	for _, turn := range turns {
		end, found := int64(0), false
		for _, ts := range ends[turn.agentID] {
			if ts >= turn.started && (!found || ts < end) {
				end, found = ts, true
			}
		}
		if !found {
			for _, ts := range notices[turn.agentID] {
				if ts >= turn.started && (!found || ts < end) {
					end, found = ts, true
				}
			}
		}
		if !found {
			mark = append(mark, turn)
			continue
		}
		path := SubagentTranscriptPath(transcript, turn.agentID)
		totals, err := ReadAgentTotals(path, "")
		if errors.Is(err, fs.ErrNotExist) {
			// A notice for an agent that wrote no transcript: no totals, as with none.
			mark = append(mark, turn)
			continue
		}
		if err != nil {
			skip(path, err)
			continue
		}
		settle = append(settle, AgentStop{AgentID: turn.agentID, Seq: turn.seq, TS: end, Totals: totals})
	}
	return mark, settle, nil
}

// lostEndWindowMS is how long after a session's last hook a lost SessionEnd
// that names no session (a missed.log line written before the wrapper and the
// binary wrote the session field) may be that session's: a SessionEnd runs as
// its process exits, right after the session's last hook. Such a match only
// keeps the end unknown; EndReasonLost needs a fault naming the session.
const lostEndWindowMS = int64(60 * 1000)

// endMissing is the SQL truth that the latest run of the session named by the
// column expression session, from its latest main-chat SessionStart on (its
// whole history when it has none), has no SessionEnd stored and no trace of
// a lost one, and that recovery has not marked it yet: no main-chat SessionEnd
// event since that start, end_reason not EndReasonNever, and no binary or
// terminated fault of a SessionEnd naming the session since that start, or
// naming no session from a second before its last hook to lostEndWindowMS
// after. A lost SessionEnd ran, so its end is unknown, never "never".
func endMissing(session string) string {
	return fmt.Sprintf(`(COALESCE((SELECT x.end_reason FROM sessions x WHERE x.session_id = %[1]s), '') <> '%[2]s'
		AND NOT EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event = '%[3]s'
			AND COALESCE(x.agent_id, '') = '' AND x.ts >= %[4]s)
		AND NOT EXISTS (SELECT 1 FROM faults x WHERE x.stage IN ('%[5]s', '%[6]s') AND x.error LIKE '%[3]s:%%'
			AND ((x.session_id = %[1]s AND x.ts >= %[4]s / 1000 * 1000)
				OR (COALESCE(x.session_id, '') = '' AND x.ts BETWEEN %[7]s - 1000 AND %[7]s + %[8]d))))`,
		session, EndReasonNever, EventSessionEnd, latestStartTS(session),
		StageBinary, StageTerminated,
		`COALESCE((SELECT y.last_ts FROM sessions y WHERE y.session_id = `+session+`), 0)`, lostEndWindowMS)
}

// endIsMissing reads endMissing for sessionID.
func (s *Store) endIsMissing(ctx context.Context, sessionID string) (bool, error) {
	var missing int
	if err := s.db.QueryRowContext(ctx, `SELECT `+endMissing("?1"), sessionID).Scan(&missing); err != nil {
		return false, fmt.Errorf("callmeter store %s: read the end of session %q: %w", s.path, sessionID, err)
	}
	return missing != 0, nil
}

// MarkNoSessionEnd sets end_reason EndReasonNever on the session when
// endMissing still holds, read under the transaction's lock, and reports
// whether it did. Call it after the transaction's first write.
func (t *Tx) MarkNoSessionEnd(ctx context.Context, sessionID string) (bool, error) {
	result, err := t.tx.ExecContext(ctx,
		`UPDATE sessions SET end_reason = ?2 WHERE session_id = ?1 AND `+endMissing("?1"), sessionID, EndReasonNever)
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: mark session %q ended with no SessionEnd: %w", t.path, sessionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: count the sessions marked ended with no SessionEnd: %w", t.path, err)
	}
	return n > 0, nil
}

// endLost is the SQL truth that the latest run of the session named by the
// column expression session has a lost SessionEnd of its own and no end
// recorded yet: end_reason NULL, no main-chat SessionEnd event since its
// latest main-chat SessionStart (its whole history when it has none), and a
// binary or terminated fault of a SessionEnd naming the session since that
// start. A lost SessionEnd naming no session (lostEndWindowMS) never makes it.
func endLost(session string) string {
	return fmt.Sprintf(`((SELECT x.end_reason FROM sessions x WHERE x.session_id = %[1]s) IS NULL
		AND NOT EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event = '%[2]s'
			AND COALESCE(x.agent_id, '') = '' AND x.ts >= %[3]s)
		AND EXISTS (SELECT 1 FROM faults x WHERE x.stage IN ('%[4]s', '%[5]s') AND x.error LIKE '%[2]s:%%'
			AND x.session_id = %[1]s AND x.ts >= %[3]s / 1000 * 1000))`,
		session, EventSessionEnd, latestStartTS(session),
		StageBinary, StageTerminated)
}

// latestStartTS is the SQL ts of the session's latest main-chat SessionStart,
// 0 when it has none. A fault from a missed.log line is stamped in whole
// seconds, so endMissing and endLost compare a fault against the start of
// that second (ts / 1000 * 1000): a SessionEnd lost within a second of its
// start is still this run's.
func latestStartTS(session string) string {
	return `COALESCE((SELECT MAX(y.ts) FROM events y WHERE y.session_id = ` + session + ` AND y.event = '` + EventSessionStart + `'
			AND COALESCE(y.agent_id, '') = ''), 0)`
}

// endIsLost reads endLost for sessionID.
func (s *Store) endIsLost(ctx context.Context, sessionID string) (bool, error) {
	var lost int
	if err := s.db.QueryRowContext(ctx, `SELECT `+endLost("?1"), sessionID).Scan(&lost); err != nil {
		return false, fmt.Errorf("callmeter store %s: read the lost end of session %q: %w", s.path, sessionID, err)
	}
	return lost != 0, nil
}

// MarkLostSessionEnd sets end_reason EndReasonLost on the session when endLost
// still holds, read under the transaction's lock, and reports whether it did.
// Call it after the transaction's first write.
func (t *Tx) MarkLostSessionEnd(ctx context.Context, sessionID string) (bool, error) {
	result, err := t.tx.ExecContext(ctx,
		`UPDATE sessions SET end_reason = ?2 WHERE session_id = ?1 AND `+endLost("?1"), sessionID, EndReasonLost)
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: mark session %q ended with its SessionEnd lost: %w", t.path, sessionID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: count the sessions marked ended with their SessionEnd lost: %w", t.path, err)
	}
	return n > 0, nil
}

// markSessionEnd applies the end recovery read for the session: EndReasonNever
// when noEnd, else EndReasonLost when lostEnd; each re-read under the lock.
// It reports which it set.
func (t *Tx) markSessionEnd(ctx context.Context, sessionID string, noEnd, lostEnd bool) (marked, lost bool, err error) {
	switch {
	case noEnd:
		marked, err = t.MarkNoSessionEnd(ctx, sessionID)
	case lostEnd:
		lost, err = t.MarkLostSessionEnd(ctx, sessionID)
	}
	return marked, lost, err
}

// markEnd is recovery of a session with no transcript to read: only its end,
// when endMissing or endLost holds, in one write of its own.
func (s *Store) markEnd(ctx context.Context, sessionID string, summary *RecoverSummary) error {
	noEnd, err := s.endIsMissing(ctx, sessionID)
	if err != nil {
		return err
	}
	lostEnd, err := s.endIsLost(ctx, sessionID)
	if err != nil || !noEnd && !lostEnd {
		return err
	}
	marked, lost := false, false
	err = s.Batch(ctx, func(tx *Tx) error {
		var err error
		marked, lost, err = tx.markSessionEnd(ctx, sessionID, noEnd, lostEnd)
		return err
	})
	if err != nil {
		return err
	}
	if marked || lost {
		summary.Sessions++
	}
	if marked {
		summary.NoEnds++
	}
	if lost {
		summary.LostEnds++
	}
	return nil
}

// lostTurnEnd is the turn end the main transcript shows for a session whose
// latest prompt has none stored and that recovery has not marked
// (turnEndUnmarked), else the zero TurnEnd. read reports that the gate held and
// the transcript was read without error, so a zero end is what the transcript
// shows; a transcript that is gone is none and not read; one that cannot be
// read is a Skipped entry, none and not read.
func (s *Store) lostTurnEnd(ctx context.Context, sessionID, transcript string, skip func(string, error)) (end TurnEnd, read bool, err error) {
	var missing int
	if err := s.db.QueryRowContext(ctx, `SELECT `+turnEndUnmarked("?1"), sessionID).Scan(&missing); err != nil {
		return TurnEnd{}, false, fmt.Errorf("callmeter store %s: read the turn end of session %q: %w", s.path, sessionID, err)
	}
	if missing == 0 {
		return TurnEnd{}, false, nil
	}
	end, err = TranscriptTurnEnd(transcript)
	if errors.Is(err, fs.ErrNotExist) {
		return TurnEnd{}, false, nil
	}
	if err != nil {
		skip(transcript, err)
		return TurnEnd{}, false, nil
	}
	return end, true, nil
}

// requestFills reports whether RecoverRequest of r would change the store: the
// row is absent and not older than since (so it would be written), a column r
// provides is NULL in the stored row, or a call of toolUseIDs names no request.
// It reads outside the write transaction, so a fully settled request costs the
// report no write at all.
func (s *Store) requestFills(ctx context.Context, r Request, toolUseIDs []string, since int64) (bool, error) {
	columns := r.columns()
	names := make([]string, len(columns))
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i, c := range columns {
		names[i] = c.name
		targets[i] = &values[i]
	}
	err := s.db.QueryRowContext(ctx,
		"SELECT "+strings.Join(names, ", ")+" FROM requests WHERE request_id = ?", r.RequestID).Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return r.TS != nil && *r.TS >= since, nil
	}
	if err != nil {
		return false, fmt.Errorf("callmeter store %s: read stored request %q: %w", s.path, r.RequestID, err)
	}
	for _, value := range values {
		if value == nil {
			return true, nil
		}
	}
	for _, id := range toolUseIDs {
		var unnamed int
		if err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM calls WHERE tool_use_id = ? AND request_id IS NULL", id).Scan(&unnamed); err != nil {
			return false, fmt.Errorf("callmeter store %s: read request of call %q: %w", s.path, id, err)
		}
		if unnamed > 0 {
			return true, nil
		}
	}
	return false, nil
}
