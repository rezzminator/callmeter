package callmeter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

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
	NoEnds   int // sessions whose latest run got end_reason EndReasonNever (MarkNoSessionEnd)
	LostEnds int // sessions whose latest run got end_reason EndReasonLost (MarkLostSessionEnd)
	// Unfillable counts the transcript faults that mark an agent turn or a Stop
	// recovery read its transcripts in full for and could not fill.
	Unfillable int
	// Skipped holds the per-session transcript errors, each naming the session
	// and the path; the rest of the pass went on.
	Skipped []error
}

// The tails of the transcript faults RecoverQuiet records for what it read in
// full and could not fill (agentTurnMarker, stopReplyMarker); scripts/reconcile
// explains them by its own parse of the same transcripts, and a test there pins
// its copy of these two texts to them.
const (
	UnfilledAgentTurn = "no request in its span, and its quiet transcript holds none at or after the session's first hook"
	UnfilledStopReply = "no final reply request, and its quiet main transcript holds none at or after the session's first hook"
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
// point a call. A session whose call has no result on disk (the tool never
// returned) stays a candidate and is read again at every report; it writes
// nothing until the result lands. A session whose transcript is gone is
// skipped without a word: its batch or stop already recorded that fault.
//
// An agent turn with no request in its span, or a main-chat Stop with a reply
// size and no final reply request, is a candidate too, and recovery fills it
// from the transcripts like any request. When every transcript was read in full
// and the turn is still unfilled (the transcripts hold nothing at or after the
// session's first hook that fills it), recovery records that once as a
// transcript fault (MarkUnfillable) and the turn stops being a candidate, so
// later reports never read it again. A live session, or one with a transcript
// that could not be read in full, is never marked.
//
// The store holds one connection, so the candidates are read and closed before
// any transcript is read, and each session's reads precede its one write.
func (s *Store) RecoverQuiet(ctx context.Context, now time.Time, quiet time.Duration) (RecoverSummary, error) {
	var summary RecoverSummary
	cutoff := now.Add(-quiet)
	candidates, err := s.quietCandidates(ctx, cutoff.UnixMilli())
	if err != nil {
		return summary, err
	}
	for _, session := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, fmt.Errorf("callmeter store %s: recover quiet sessions: %w", s.path, err)
		}
		if err := s.recoverSession(ctx, session, cutoff, &summary); err != nil {
			return summary, fmt.Errorf("recover session %s: %w", session.id, err)
		}
	}
	return summary, nil
}

// quietCandidates lists the sessions whose last hook is before beforeMS, that
// name a transcript, and that hold something recovery could settle: a call with
// no size at all or no request, or on a provisional request, a pending request
// row, a sub-agent turn with a stop and no request of that agent since its
// previous stop (every turn makes one, so its stop read the transcript before
// the turn's reply was on disk and no later hook of the agent read it again;
// agentTurnUnfilled), a main-chat Stop whose final reply request was never
// stored (stopReplyUnfilled), each until recovery has read the transcripts in
// full and marked it unfillable (MarkUnfillable), a latest prompt with no turn end, or a latest run with no SessionEnd
// (endMissing) or a lost one of its own still unmarked (endLost).
func (s *Store) quietCandidates(ctx context.Context, beforeMS int64) ([]quietSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.session_id, s.transcript_path, s.seat_dir, s.config_dir FROM sessions s
		WHERE s.transcript_path IS NOT NULL AND s.transcript_path <> ''
		AND COALESCE(s.last_ts, 0) < ?1
		AND (EXISTS (SELECT 1 FROM calls c WHERE c.session_id = s.session_id AND (
				(c.bytes_real IS NULL AND c.bytes_delivered IS NULL)
				OR c.request_id IS NULL OR c.request_id LIKE ?2 || '%'))
			OR EXISTS (SELECT 1 FROM requests r WHERE r.session_id = s.session_id AND r.pending = 1)
			OR EXISTS (SELECT 1 FROM agent_turns t WHERE t.session_id = s.session_id AND `+agentTurnUnfilled("t")+`)
			OR EXISTS (SELECT 1 FROM turns t WHERE t.session_id = s.session_id AND `+stopReplyUnfilled("t")+`)
			OR `+turnEndMissing("s.session_id")+`
			OR `+endMissing("s.session_id")+`
			OR `+endLost("s.session_id")+`)
		ORDER BY s.session_id`, beforeMS, PendingPrefix)
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
func (s *Store) recoverSession(ctx context.Context, session quietSession, cutoff time.Time, summary *RecoverSummary) error {
	skip := func(path string, err error) {
		summary.Skipped = append(summary.Skipped, fmt.Errorf("session %s: %s: %w", session.id, path, err))
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
		pendingReads = append(pendingReads, pendingRead{pending: pending, found: found})
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
	var unknown []UnfinishedCall
	for _, call := range unfinished {
		if !call.Delivered {
			unknown = append(unknown, call)
		}
	}
	var settled []Call
	transcripts, idsOf := CallTranscripts(session.transcript, unknown)
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
			if result, ok := found[id]; ok {
				settled = append(settled, SettledCall(id, result))
			}
		}
	}

	since, haveSince, err := s.SessionFirstTS(ctx, session.id)
	if err != nil {
		return err
	}
	resolved := map[string]bool{} // message ids a pending request was resolved to
	resolvedCount := 0
	for _, read := range pendingReads {
		for _, request := range read.pending {
			if usage, ok := resolvedBy(request, read.found); ok {
				resolved[usage.MessageID] = true
				resolvedCount++
			}
		}
	}
	var writes []requestWrite
	// complete: every transcript of the session was read in full, so what no
	// request of theirs fills stays unfilled for good (MarkUnfillable).
	complete := haveSince
	if haveSince {
		for i, file := range files {
			agentID := ""
			if i > 0 {
				agentID = subagents[i-1]
			}
			requests, _, err := ReadRequests(file)
			if errors.Is(err, fs.ErrNotExist) {
				complete = false
				continue
			}
			if err != nil {
				complete = false
				skip(file, err)
				continue
			}
			for _, read := range requests {
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

	end, err := s.lostTurnEnd(ctx, session.id, session.transcript, skip)
	if err != nil {
		return err
	}
	noEnd, err := s.endIsMissing(ctx, session.id)
	if err != nil {
		return err
	}
	lostEnd, err := s.endIsLost(ctx, session.id)
	if err != nil {
		return err
	}

	unfilled := false
	if complete {
		if unfilled, err = s.unfilledPending(ctx, session.id); err != nil {
			return err
		}
	}
	if resolvedCount == 0 && len(settled) == 0 && len(writes) == 0 && end.Event == "" && !noEnd && !lostEnd && !unfilled {
		return nil
	}
	rebuilt, marked, lost, unfillable := false, false, false, 0
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
		for _, call := range settled {
			if err := tx.UpsertCall(ctx, call, FillEmpty); err != nil {
				return err
			}
		}
		for _, write := range writes {
			if err := tx.RecoverRequest(ctx, write.request, write.ids, since); err != nil {
				return err
			}
		}
		if end.Event != "" {
			var err error
			rebuilt, err = tx.RecoverTurnEnd(ctx, Event{Event: end.Event, SessionID: Ptr(session.id), TS: end.TS,
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
			unfillable, err = tx.MarkUnfillable(ctx, session.id)
		}
		return err
	})
	if err != nil {
		return err
	}
	if resolvedCount == 0 && len(settled) == 0 && len(writes) == 0 && !rebuilt && !marked && !lost && unfillable == 0 {
		return nil
	}
	if rebuilt {
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
// latest prompt has none stored, else the zero TurnEnd. A transcript that is
// gone is none; one that cannot be read is a Skipped entry and none.
func (s *Store) lostTurnEnd(ctx context.Context, sessionID, transcript string, skip func(string, error)) (TurnEnd, error) {
	var missing int
	if err := s.db.QueryRowContext(ctx, `SELECT `+turnEndMissing("?1"), sessionID).Scan(&missing); err != nil {
		return TurnEnd{}, fmt.Errorf("callmeter store %s: read the turn end of session %q: %w", s.path, sessionID, err)
	}
	if missing == 0 {
		return TurnEnd{}, nil
	}
	end, err := TranscriptTurnEnd(transcript)
	if errors.Is(err, fs.ErrNotExist) {
		return TurnEnd{}, nil
	}
	if err != nil {
		skip(transcript, err)
		return TurnEnd{}, nil
	}
	return end, nil
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
