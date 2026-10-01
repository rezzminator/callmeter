package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedSessions stores s1 (two runs, a SessionStart and SessionEnd, a main-chat
// request, two calls, one sub-agent) and s2 (one run, one call, a UTC offset
// and no zone name).
func seedSessions(t *testing.T, store *callmeter.Store) {
	t.Helper()
	seedEvent(t, store, callmeter.Event{
		EventID: "e-start", Event: callmeter.EventSessionStart, TS: ms(5 * time.Hour), SessionID: callmeter.Ptr("s1"),
		Source: callmeter.Ptr("startup"), Model: callmeter.Ptr("m-start"),
	})
	seedEvent(t, store, callmeter.Event{
		EventID: "e-end", Event: callmeter.EventSessionEnd, TS: ms(time.Hour), SessionID: callmeter.Ptr("s1"),
		Reason: callmeter.Ptr("clear"),
	})
	seedRequest(t, store, callmeter.Request{
		RequestID: "q1", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(2 * time.Hour)), Model: callmeter.Ptr("m-main"),
	})
	seed(t, store, mkCall("c1", "s1", 4*time.Hour, "Read"))
	seed(t, store, mkCall("c2", "s1", 3*time.Hour, "Read"))
	seed(t, store, mkCall("c3", "s2", 30*time.Minute, "Read"))
	seedAgent(t, store, callmeter.Agent{AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")})
	seedSession(t, store, callmeter.Session{
		SessionID: "s1", TS: ms(5 * time.Hour), Cwd: callmeter.Ptr("/w/p"), Host: callmeter.Ptr("mac1"),
		TZName: callmeter.Ptr("Europe/Berlin"), TZOffsetMinutes: callmeter.Ptr(int64(120)),
	})
	seedSession(t, store, callmeter.Session{SessionID: "s1", TS: ms(time.Hour)})
	seedSession(t, store, callmeter.Session{
		SessionID: "s2", TS: ms(30 * time.Minute), Cwd: callmeter.Ptr("/w/q"), TZOffsetMinutes: callmeter.Ptr(int64(-90)),
	})
}

func TestSessionsListsOneRowPerSessionLatestFirst(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	table, err := Sessions(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	wantHeader(t, table, "CHAT", "SESSION", "STARTED", "LAST", "MODEL", "START", "END", "CALLS", "AGENTS", "CWD", "HOST", "TZ")
	wantRows(t, table,
		"chat-s2|s2|2026-09-23 11:30|2026-09-23 11:30|-|-|-|1|0|/w/q|-|UTC-01:30",
		"chat-s1|s1|2026-09-23 07:00|2026-09-23 11:00|m-main|startup|clear|2|1|/w/p|mac1|Europe/Berlin",
	)
}

func TestSessionsFiltersNarrowAndLimitCaps(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	// s3's own cwd is elsewhere, but one of its calls ran under /w/p/sub.
	call := mkCall("c4", "s3", 20*time.Minute, "Read")
	call.Cwd = callmeter.Ptr("/w/p/sub")
	seed(t, store, call)
	seedSession(t, store, callmeter.Session{SessionID: "s3", TS: ms(20 * time.Minute), Cwd: callmeter.Ptr("/w/z")})
	ctx := context.Background()
	for name, c := range map[string]struct {
		filter Filter
		want   []string
	}{
		"session": {Filter{Session: "s1"}, []string{"s1"}},
		"project": {Filter{Project: "/w/p"}, []string{"s3", "s1"}},
		"since":   {Filter{Since: testNow.Add(-45 * time.Minute)}, []string{"s3", "s2"}},
		"limit":   {Filter{Limit: 1}, []string{"s3"}},
	} {
		table, err := Sessions(ctx, store, c.filter, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, row := range table.Rows {
			got = append(got, row[1])
		}
		if cells(got) != cells(c.want) {
			t.Errorf("%s sessions = %v, want %v", name, got, c.want)
		}
	}
}

func TestSessionsEmptyStoreHasHeaderAndNoRows(t *testing.T) {
	table, err := Sessions(context.Background(), openStore(t), Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(table.Rows) != 0 || len(table.Header) != 12 {
		t.Errorf("empty store: rows %v header %v, want no rows and the 12 columns", table.Rows, table.Header)
	}
}
