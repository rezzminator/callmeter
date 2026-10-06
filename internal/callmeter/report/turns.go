package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Turns lists one row per recorded turn duration, newest first: the chat, when,
// the first 8 characters of the prompt id, the wall seconds Claude Code
// measured, the messages and background agents of the turn, and the effort of
// the turn it attaches to (the Stop row of that prompt at or before the
// duration, "-" when none). The note gives the count, total, median and p90.
func Turns(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no turn durations in window",
		Title:  f.title("turns", n),
		Header: []string{"CHAT", "TIME", "PROMPT", "WALL S", "MESSAGES", "BG AGENTS", "EFFORT"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "turn_durations")
		return t, err
	}
	var sessions []*string
	where, args := f.scoped(scope{ts: "d.ts", session: "d.session_id"})
	err = query(ctx, store, "turn durations",
		`SELECT d.session_id, d.ts, d.prompt_id, d.duration_ms, d.message_count, d.background_agents,
			(SELECT t.effort FROM turns t WHERE t.session_id = d.session_id AND t.prompt_id = d.prompt_id AND t.ts <= d.ts
				ORDER BY t.ts DESC, t.event_id DESC LIMIT 1)
		FROM turn_durations d WHERE `+where+` ORDER BY d.ts DESC, d.entry_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var session, prompt, effort *string
			var ts, duration, messages, background *int64
			if err := r.Scan(&session, &ts, &prompt, &duration, &messages, &background, &effort); err != nil {
				return err
			}
			sessions = append(sessions, session)
			t.Rows = append(t.Rows, []string{
				"", stampCell(ts), shortID(prompt), tenths(duration), intCell(messages), intCell(background), textCell(effort),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	var walls []int64
	err = query(ctx, store, "turn duration totals",
		`SELECT d.duration_ms FROM turn_durations d WHERE d.duration_ms IS NOT NULL AND `+where, args,
		func(r rowSource) error {
			var ms int64
			if err := r.Scan(&ms); err != nil {
				return err
			}
			walls = append(walls, ms)
			return nil
		})
	if err != nil {
		return nil, err
	}
	if t.Notes, err = gainedNotes(ctx, store, f, n, "turn_durations"); err != nil {
		return nil, err
	}
	if len(walls) > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d turns, total %s s, median %s s, p90 %s s", len(walls),
			secondsCell(float64(sumMS(walls))), secondsCell(medianMS(walls)), secondsCell(percentileMS(walls, 90))))
	}
	return t, nil
}

// shortID is the first 8 characters of an id; NULL and "" are "-".
func shortID(id *string) string {
	if id == nil || *id == "" {
		return "-"
	}
	runes := 0
	for i := range *id {
		if runes == 8 {
			return (*id)[:i]
		}
		runes++
	}
	return *id
}
