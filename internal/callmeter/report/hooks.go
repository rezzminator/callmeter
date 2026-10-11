package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Hooks totals the Stop-hook overhead per hook name, longest total first: the
// runs, how many of them carried a duration, and the total, average and
// longest time of those. An async hook carries no duration: it is counted in
// RUNS and left out of the time, and a note counts such runs.
func Hooks(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no Stop hook summaries in window",
		Title:  f.title("hooks", n),
		Header: []string{"HOOK", "RUNS", "TIMED", "TOTAL S", "AVG MS", "MAX MS"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "stop_hooks table")
		return t, err
	}
	where, args := f.scoped(scope{ts: "h.ts", session: "h.session_id"})
	err = query(ctx, store, "stop hook runs",
		`SELECT COALESCE(NULLIF(r.name, ''), '-'), COUNT(*), COUNT(r.duration_ms), SUM(r.duration_ms), MAX(r.duration_ms)
		FROM stop_hook_runs r JOIN stop_hooks h ON h.entry_id = r.entry_id WHERE `+where+`
		GROUP BY 1 ORDER BY SUM(r.duration_ms) IS NULL, SUM(r.duration_ms) DESC, COUNT(*) DESC, 1 LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var name string
			var runs, timed int64
			var total, longest *int64
			if err := r.Scan(&name, &runs, &timed, &total, &longest); err != nil {
				return err
			}
			avg := "-"
			if total != nil && timed > 0 {
				avg = fmt.Sprintf("%.0f", float64(*total)/float64(timed))
			}
			t.Rows = append(t.Rows, []string{name, itoa(runs), itoa(timed), tenths(total), avg, intCell(longest)})
			return nil
		})
	if err != nil {
		return nil, err
	}
	var summaries, errs, untimed int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(h.hook_errors), 0) FROM stop_hooks h WHERE `+where, args...,
	).Scan(&summaries, &errs); err != nil {
		return nil, fmt.Errorf("callmeter report: count the Stop hook summaries: %w", err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stop_hook_runs r JOIN stop_hooks h ON h.entry_id = r.entry_id
		WHERE r.duration_ms IS NULL AND `+where, args...,
	).Scan(&untimed); err != nil {
		return nil, fmt.Errorf("callmeter report: count the untimed Stop hook runs: %w", err)
	}
	if t.Notes, err = gainedNotes(ctx, store, f, n, "stop_hooks table"); err != nil {
		return nil, err
	}
	if summaries > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d Stop hook summaries, %d hook errors", summaries, errs))
	}
	if untimed > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf(
			"%d hook runs carry no duration (async hooks): counted in RUNS, not timed", untimed))
	}
	return t, nil
}
