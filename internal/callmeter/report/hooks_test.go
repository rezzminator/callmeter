package report

import (
	"context"
	"testing"
	"time"
)

func TestHooksTotalsPerNameAndCountsAsyncRunsApart(t *testing.T) {
	store := openStore(t)
	for _, h := range []struct {
		id          string
		age         time.Duration
		count, errs int
	}{{"h1", time.Hour, 3, 1}, {"h2", 2 * time.Hour, 2, 0}, {"h3", 10 * 24 * time.Hour, 1, 5}} {
		exec(t, store, `INSERT INTO stop_hooks (entry_id, session_id, ts, hook_count, hook_errors) VALUES (?, 's1', ?, ?, ?)`,
			h.id, ms(h.age), h.count, h.errs)
	}
	for _, r := range []struct {
		entry string
		seq   int
		name  string
		dur   any
	}{
		{"h1", 0, "lint.sh", 400}, {"h1", 1, "lint.sh", 600}, {"h1", 2, "#ab12cd34", nil},
		{"h2", 0, "lint.sh", 1000}, {"h2", 1, "notify", nil}, {"h3", 0, "lint.sh", 99999},
	} {
		exec(t, store, `INSERT INTO stop_hook_runs (entry_id, seq, name, command_bytes, duration_ms) VALUES (?, ?, ?, 10, ?)`,
			r.entry, r.seq, r.name, r.dur)
	}
	table, err := Hooks(context.Background(), store, Filter{Since: testNow.Add(-24 * time.Hour)}, chatOf)
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	wantHeader(t, table, "HOOK", "RUNS", "TIMED", "TOTAL S", "AVG MS", "MAX MS")
	wantRows(t, table,
		"lint.sh|3|3|2.0|667|1000",
		"#ab12cd34|1|0|-|-|-",
		"notify|1|0|-|-|-",
	)
	want := []string{
		"2 Stop hook summaries, 1 hook errors",
		"2 hook runs carry no duration (async hooks): counted in RUNS, not timed",
	}
	if len(table.Notes) != 2 || table.Notes[0] != want[0] || table.Notes[1] != want[1] {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
}
