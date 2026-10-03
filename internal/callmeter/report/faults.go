package report

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// stages are every fault stage, always listed so a zero is visible.
var stages = []string{
	callmeter.StagePayload, callmeter.StageStore, callmeter.StageTranscript,
	callmeter.StageParse, callmeter.StageBinary, callmeter.StageTerminated,
}

// Faults counts faults by stage and command parts by parse status, then lists
// the latest Limit faults, then the latest Limit refusals (refusalsOf). Faults
// and refusals narrow by window and session only: a call that failed to
// record has no calls row to carry the other filters.
func Faults(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no faults in window",
		Title:  f.title("faults", n),
		Header: []string{"KIND", "STAGE/STATUS", "COUNT", "WHEN", "CHAT", "CALL", "ERROR"},
	}
	faultWhere, faultArgs := faultFilter(f, "faults")
	byStage := map[string]int64{}
	var total int64
	err := query(ctx, store, "faults by stage",
		"SELECT COALESCE(stage, ''), COUNT(*) FROM faults WHERE "+faultWhere+" GROUP BY stage", faultArgs,
		func(r rowSource) error {
			var stage string
			var count int64
			if err := r.Scan(&stage, &count); err != nil {
				return err
			}
			byStage[stage] = count
			total += count
			return nil
		})
	if err != nil {
		return nil, err
	}
	where, args := f.where()
	var calls int64
	if err := store.DB().
		QueryRowContext(ctx, "SELECT COUNT(*) FROM calls c WHERE "+where, args...).
		Scan(&calls); err != nil {
		return nil, fmt.Errorf("callmeter report: count calls in window: %w", err)
	}
	refusals, unchecked, err := refusalsOf(ctx, store, f)
	if err != nil {
		return nil, err
	}
	if total == 0 && calls == 0 && len(refusals) == 0 && unchecked == nil {
		return t, nil
	}
	for _, stage := range stages {
		t.Rows = append(t.Rows, []string{"stage", stage, itoa(byStage[stage]), "", "", "", ""})
		delete(byStage, stage)
	}
	for stage, count := range byStage {
		t.Rows = append(t.Rows, []string{"stage", stage + " (unknown stage)", itoa(count), "", "", "", ""})
	}
	err = query(ctx, store, "parts by parse status",
		`SELECT COALESCE(p.parse_status, '(none)'), COUNT(*) FROM command_parts p
		JOIN calls c ON c.tool_use_id = p.tool_use_id WHERE `+where+` GROUP BY p.parse_status ORDER BY p.parse_status`, args,
		func(r rowSource) error {
			var status string
			var count int64
			if err := r.Scan(&status, &count); err != nil {
				return err
			}
			t.Rows = append(t.Rows, []string{"parse", status, itoa(count), "", "", "", ""})
			return nil
		})
	if err != nil {
		return nil, err
	}
	type latestFault struct {
		ts                   int64
		stage, session, call string
		message              string
	}
	var found []latestFault
	err = query(
		ctx,
		store,
		"latest faults",
		`SELECT COALESCE(ts, 0), COALESCE(stage, ''), COALESCE(session_id, ''), COALESCE(tool_use_id, ''), COALESCE(error, '')
		FROM faults WHERE `+faultWhere+` ORDER BY ts DESC LIMIT ?`,
		append(faultArgs, f.limit()),
		func(r rowSource) error {
			var row latestFault
			if err := r.Scan(&row.ts, &row.stage, &row.session, &row.call, &row.message); err != nil {
				return err
			}
			found = append(found, row)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	// The store holds one connection: names resolve only after the scan closes.
	for _, row := range found {
		chat := "-"
		if row.session != "" {
			chat = n.of(row.session)
		}
		t.Rows = append(t.Rows, []string{
			"fault", row.stage, "1", time.UnixMilli(row.ts).UTC().Format("2006-01-02 15:04:05"), chat, row.call, row.message,
		})
	}
	for _, r := range refusals {
		t.Rows = append(t.Rows, []string{
			"refusal", r.kind, "1", time.UnixMilli(r.ts).UTC().Format("2006-01-02 15:04:05"), n.of(r.session), "", r.source,
		})
	}
	notes, err := gapNotes(ctx, store, f)
	if err != nil {
		return nil, err
	}
	t.Notes = notes
	if unchecked != nil {
		t.Notes = append(t.Notes, fmt.Sprintf(
			"%d sessions ending on an unanswered prompt could not be checked for an API error: %v", unchecked.count, unchecked.first))
	}
	t.Notes = append(t.Notes, n.notes()...)
	return t, nil
}

// Where a refusal row's kind was read.
const (
	refusalFromHook       = "StopFailure"
	refusalFromTranscript = "transcript, no StopFailure"
	// refusalRebuilt is a StopFailure row rebuilt from the transcript
	// (callmeter.RecoveredDetail), at SessionEnd or at report time, its hook
	// lost.
	refusalRebuilt = "StopFailure rebuilt from the transcript"
)

// refusal is one turn the API refused: its error kind, never its message.
type refusal struct {
	ts                    int64
	session, kind, source string
}

// uncheckedRefusals counts the sessions whose transcript could not be read
// to tell a refusal from a turn still open.
type uncheckedRefusals struct {
	count int
	first error
}

// refusalsOf lists the turns the API refused, latest first, at most Limit:
// each StopFailure event in the window (one rebuilt from the transcript, at
// SessionEnd or at report time, says so), and each session active in the
// window whose latest main-chat prompt got neither a Stop nor a StopFailure
// and whose transcript ends on the `<synthetic>` API-error message, because a
// headless exit cancels the async StopFailure hook before it records. A
// transcript that cannot be read is counted in unchecked, never read as no
// refusal.
func refusalsOf(ctx context.Context, store *callmeter.Store, f Filter) ([]refusal, *uncheckedRefusals, error) {
	var found []refusal
	eventWhere, eventArgs := faultFilter(f, "e")
	err := query(ctx, store, "stop failures",
		`SELECT COALESCE(e.ts, 0), COALESCE(e.session_id, ''), COALESCE(e.error_type, ''),
		COALESCE(e.detail, '') = ?
		FROM events e WHERE e.event = 'StopFailure' AND `+eventWhere, append([]any{callmeter.RecoveredDetail}, eventArgs...),
		func(r rowSource) error {
			var row refusal
			var rebuilt bool
			if err := r.Scan(&row.ts, &row.session, &row.kind, &rebuilt); err != nil {
				return err
			}
			row.kind, row.source = callmeter.APIErrorKind(row.kind), refusalFromHook
			if rebuilt {
				row.source = refusalRebuilt
			}
			found = append(found, row)
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	conds, args := "1=1", []any{}
	if !f.Since.IsZero() {
		conds += " AND s.last_ts >= ?"
		args = append(args, f.Since.UnixMilli())
	}
	if f.Session != "" {
		conds += " AND s.session_id = ?"
		args = append(args, f.Session)
	}
	type openTurn struct {
		ts                           int64
		session, transcript, seatDir string
	}
	var open []openTurn
	prompt := `(SELECT prompt_id FROM events WHERE session_id = s.session_id
		AND event = 'UserPromptSubmit' AND COALESCE(agent_id, '') = '' ORDER BY ts DESC, event_id DESC LIMIT 1)`
	err = query(ctx, store, "sessions ending on an unanswered prompt",
		`SELECT s.session_id, COALESCE(s.last_ts, 0), COALESCE(s.transcript_path, ''), COALESCE(s.seat_dir, '') FROM sessions s
		JOIN (SELECT session_id, MAX(ts) AS asked FROM events
			WHERE event = 'UserPromptSubmit' AND COALESCE(agent_id, '') = '' GROUP BY session_id) u
			ON u.session_id = s.session_id
		WHERE `+conds+`
		AND NOT EXISTS (SELECT 1 FROM turns t WHERE t.session_id = s.session_id AND t.event = 'Stop'
			AND COALESCE(t.agent_id, '') = '' AND t.ts >= u.asked
			AND (t.prompt_id IS NULL OR `+prompt+` IS NULL OR t.prompt_id = `+prompt+`))
		AND NOT EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.session_id AND e.event = 'StopFailure'
			AND COALESCE(e.agent_id, '') = '' AND e.ts >= u.asked
			AND (e.prompt_id IS NULL OR `+prompt+` IS NULL OR e.prompt_id = `+prompt+`))`, args,
		func(r rowSource) error {
			var row openTurn
			if err := r.Scan(&row.session, &row.ts, &row.transcript, &row.seatDir); err != nil {
				return err
			}
			open = append(open, row)
			return nil
		})
	if err != nil {
		return nil, nil, err
	}
	// The store holds one connection: transcripts are read after the scan closes.
	var unchecked *uncheckedRefusals
	for _, turn := range open {
		turn.transcript = callmeter.ResolveTranscript(turn.transcript, turn.seatDir, turn.session)
		kind, err := "", errors.New("no transcript path recorded for session "+turn.session)
		if turn.transcript != "" {
			kind, err = callmeter.TranscriptAPIError(turn.transcript)
		}
		if err != nil {
			if unchecked == nil {
				unchecked = &uncheckedRefusals{first: err}
			}
			unchecked.count++
			continue
		}
		if kind != "" {
			found = append(found, refusal{ts: turn.ts, session: turn.session, kind: kind, source: refusalFromTranscript})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].ts > found[j].ts })
	if len(found) > f.limit() {
		found = found[:f.limit()]
	}
	return found, unchecked, nil
}
