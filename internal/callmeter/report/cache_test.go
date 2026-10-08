package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedCacheRequests writes a main chat (m1 first, m2 a hit on a 1 h entry, m3
// a miss) and one Explore sub-agent (a1 first, then three hits on 5 m
// entries) into session s1.
func seedCacheRequests(t *testing.T, store *callmeter.Store) {
	t.Helper()
	base := ms(3 * time.Hour)
	minute := int64(time.Minute / time.Millisecond)
	n := callmeter.Ptr[int64]
	seedAgent(t, store, callmeter.Agent{AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")})
	for _, r := range []struct {
		id, agent                   string
		at, context, read, w5m, w1h int64
	}{
		{"m1", "", 0, 10000, 0, 0, 10000},
		{"m2", "", 1, 12000, 9600, 0, 2000},
		{"m3", "", 2, 13000, 1000, 12000, 0},
		{"a1", "a1", 0, 500, 0, 500, 0},
		{"a2", "a1", 1, 600, 500, 100, 0},
		{"a3", "a1", 2, 700, 600, 100, 0},
		{"a4", "a1", 3, 800, 700, 100, 0},
	} {
		row := callmeter.Request{
			RequestID: r.id, SessionID: callmeter.Ptr("s1"), TS: n(base + r.at*minute), Model: callmeter.Ptr("opus"),
			Pending: callmeter.Ptr(false), ContextTokens: n(r.context), CacheReadTokens: n(r.read),
			CacheCreationTokens: n(r.w5m + r.w1h), CacheCreation5mTokens: n(r.w5m), CacheCreation1hTokens: n(r.w1h),
		}
		if r.agent != "" {
			row.AgentID = callmeter.Ptr(r.agent)
		}
		seedRequest(t, store, row)
	}
}

// TestCacheCountsOutcomesPerPartyAndTTLWithTheWireBreak: the requests are
// counted per party, entry TTL, outcome, cause and the break kind wire.db
// records (a NULL kind is unknown, no row is "-"), most first; the coverage
// note counts the requests with a row and states the reader. Without wire.db,
// or with one it cannot read, every outcome and cause stays as it was; only
// the break column and the note change, and a warning is also a failure the
// run logs.
func TestCacheCountsOutcomesPerPartyAndTTLWithTheWireBreak(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seedCacheRequests(t, store)
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok", "wire.db")
	newer := filepath.Join(dir, "newer", "wire.db")
	for _, path := range []string{ok, newer} {
		version := 1
		if path == newer {
			version = 2
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeWire(t, path, version, []string{wireDDL}, map[string]string{"m2": "none", "m3": "system", "a2": "", "msg_elsewhere": "tools"})
	}
	withoutWire := []string{
		"sub-agent|5m|hit|-|-|3",
		"main chat|-|first|-|-|1",
		"main chat|1h|hit|-|-|1",
		"main chat|1h|miss|unknown|-|1",
		"sub-agent|-|first|-|-|1",
	}
	for _, tt := range []struct {
		name   string
		filter Filter
		rows   []string
		note   string
		failed bool
	}{
		{"wire facts", Filter{Wire: ok}, []string{
			"sub-agent|5m|hit|-|-|2",
			"main chat|-|first|-|-|1",
			"main chat|1h|hit|-|none|1",
			"main chat|1h|miss|unknown|system|1",
			"sub-agent|-|first|-|-|1",
			"sub-agent|5m|hit|-|unknown|1",
		}, "wire data for 3 of 7 requests (wire.db: ok)", false},
		{"one agent type", Filter{Wire: ok, AgentType: "Explore"}, []string{
			"sub-agent|5m|hit|-|-|2",
			"sub-agent|-|first|-|-|1",
			"sub-agent|5m|hit|-|unknown|1",
		}, "wire data for 1 of 4 requests (wire.db: ok)", false},
		{"absent", Filter{Wire: filepath.Join(dir, "none", "wire.db")}, withoutWire,
			"wire data for 0 of 7 requests (wire.db: absent)", false},
		{"unreadable", Filter{Wire: newer}, withoutWire,
			"wire data for 0 of 7 requests (wire.db: warning: user_version 2, this callmeter reads 1)", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			table, err := Cache(ctx, store, tt.filter, chatOf)
			if err != nil {
				t.Fatalf("Cache: %v", err)
			}
			wantHeader(t, table, "PARTY", "ENTRY TTL", "OUTCOME", "CAUSE", "BREAK", "REQUESTS")
			wantRows(t, table, tt.rows...)
			if !slices.Contains(table.Notes, tt.note) {
				t.Errorf("notes = %q, want %q", table.Notes, tt.note)
			}
			if tt.failed != (len(table.Failures) == 1) || len(table.Failures) > 1 {
				t.Errorf("failures = %v, want one: %v", table.Failures, tt.failed)
			}
			if tt.failed && !strings.Contains(table.Failures[0].Error(), tt.filter.Wire) {
				t.Errorf("failure = %v, want it to name %s", table.Failures[0], tt.filter.Wire)
			}
		})
	}
}

// TestCacheOverAViewItCannotReadAnswersTheNote: a store that has not gained
// the request_cache view, or holds a newer release's, is not queried for it.
func TestCacheOverAViewItCannotReadAnswersTheNote(t *testing.T) {
	newer := openStore(t)
	_, shipped, err := newer.ViewVersions(context.Background(), "request_cache")
	if err != nil {
		t.Fatalf("ViewVersions: %v", err)
	}
	exec(t, newer, "INSERT INTO requests (request_id, session_id, ts, model) VALUES ('q', 's1', 1, 'opus')")
	exec(t, newer, "DROP VIEW request_cache")
	exec(t, newer, fmt.Sprintf("CREATE VIEW request_cache AS -- version %d\n  SELECT request_id, 'hit' AS verdict FROM requests", shipped+1))
	for _, c := range []struct {
		name  string
		store *callmeter.Store
		want  string
	}{
		{"without the view", incompleteStore(t),
			"this store has not gained the request_cache view yet; the next hook or report adds it"},
		{"a newer release's view", newer, fmt.Sprintf(
			"this store's request_cache view is version %d, newer than this callmeter's %d; a newer callmeter reads it",
			shipped+1, shipped)},
	} {
		table, err := Cache(context.Background(), c.store, Filter{}, chatOf)
		if err != nil {
			t.Fatalf("%s: Cache: %v", c.name, err)
		}
		if len(table.Rows) != 0 || !slices.Contains(table.Notes, c.want) {
			t.Errorf("%s: rows %v notes %q, want no rows and %q", c.name, rowsOf(table), table.Notes, c.want)
		}
	}
}
