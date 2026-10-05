package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Compactions lists one row per context compaction, newest first: the chat,
// when, the trigger, the context size before and after, the tokens freed
// (before minus after), the running total Claude Code dropped in the session
// and how long the compaction took. A size the transcript did not carry is "-",
// never 0. The note totals the window's compactions.
func Compactions(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no compactions in window",
		Title:  f.title("compactions", n),
		Header: []string{"CHAT", "TIME", "TRIGGER", "BEFORE", "AFTER", "FREED", "DROPPED TOTAL", "SECONDS"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "compactions")
		return t, err
	}
	var sessions []*string
	where, args := f.scoped(scope{ts: "c.ts", session: "c.session_id"})
	err = query(ctx, store, "compactions",
		`SELECT c.session_id, c.ts, c.trigger, c.pre_tokens, c.post_tokens, c.pre_tokens - c.post_tokens,
			c.cumulative_dropped_tokens, c.duration_ms
		FROM compactions c WHERE `+where+` ORDER BY c.ts DESC, c.entry_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var session, trigger *string
			var ts, before, after, freed, dropped, duration *int64
			if err := r.Scan(&session, &ts, &trigger, &before, &after, &freed, &dropped, &duration); err != nil {
				return err
			}
			sessions = append(sessions, session)
			t.Rows = append(t.Rows, []string{
				"", stampCell(ts), textCell(trigger), intCell(before), intCell(after), intCell(freed), intCell(dropped),
				tenths(duration),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	var count, auto, manual, freed, duration int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(c.trigger = 'auto'), 0), COALESCE(SUM(c.trigger = 'manual'), 0),
			COALESCE(SUM(c.pre_tokens - c.post_tokens), 0), COALESCE(SUM(c.duration_ms), 0)
		FROM compactions c WHERE `+where, args...,
	).Scan(&count, &auto, &manual, &freed, &duration); err != nil {
		return nil, fmt.Errorf("callmeter report: total the compactions: %w", err)
	}
	if t.Notes, err = gainedNotes(ctx, store, f, n, "compactions"); err != nil {
		return nil, err
	}
	if count > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d compactions (%d auto, %d manual), %d tokens freed, %s s spent compacting",
			count, auto, manual, freed, secondsCell(float64(duration))))
	}
	return t, nil
}
