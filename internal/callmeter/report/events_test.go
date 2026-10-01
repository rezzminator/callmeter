package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedEvents stores one event of each value source, two SessionStart startup
// events, a notification typed by its detail and one whose detail is not JSON.
func seedEvents(t *testing.T, store *callmeter.Store) {
	t.Helper()
	p := callmeter.Ptr[string]
	for _, e := range []callmeter.Event{
		{EventID: "s1", Event: "SessionStart", TS: ms(5 * time.Hour), SessionID: p("s1"), Source: p("startup")},
		{EventID: "s2", Event: "SessionStart", TS: ms(4 * time.Hour), SessionID: p("s2"), Source: p("startup")},
		{EventID: "s3", Event: "SessionStart", TS: ms(3 * time.Hour), SessionID: p("s1"), Source: p("resume"), Reason: p("ignored")},
		{EventID: "n1", Event: "Notification", TS: ms(2 * time.Hour), SessionID: p("s1"), Detail: p(`{"notification_type":"idle_prompt"}`)},
		{EventID: "n2", Event: "Notification", TS: ms(2 * time.Hour), SessionID: p("s1"), Detail: p(`{bad`)},
		{EventID: "i1", Event: "InstructionsLoaded", TS: ms(time.Hour), SessionID: p("s1"), LoadReason: p("session_start")},
		{EventID: "c1", Event: "PreCompact", TS: ms(time.Hour), SessionID: p("s1"), Trigger: p("auto")},
		{EventID: "x1", Event: "UserPromptExpansion", TS: ms(time.Hour), SessionID: p("s1"), CommandName: p("commit")},
		{EventID: "t1", Event: "TaskCreated", TS: ms(time.Hour), SessionID: p("s1"), ToolName: p("Bash"), AgentType: p("Explore")},
		{EventID: "f1", Event: "StopFailure", TS: ms(time.Hour), SessionID: p("s1"), ErrorType: p("rate_limit")},
		{EventID: "e1", Event: "SessionEnd", TS: ms(30 * time.Minute), SessionID: p("s1"), Reason: p("clear")},
	} {
		seedEvent(t, store, e)
	}
}

func TestEventsCountsByKindAndValueWithFirstAndLast(t *testing.T) {
	store := openStore(t)
	seedEvents(t, store)
	table, err := Events(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantHeader(t, table, "EVENT", "VALUE", "COUNT", "FIRST", "LAST")
	wantRows(t, table,
		"SessionStart|startup|2|2026-09-23 07:00|2026-09-23 08:00",
		"InstructionsLoaded|session_start|1|2026-09-23 11:00|2026-09-23 11:00",
		"Notification||1|2026-09-23 10:00|2026-09-23 10:00",
		"Notification|idle_prompt|1|2026-09-23 10:00|2026-09-23 10:00",
		"PreCompact|auto|1|2026-09-23 11:00|2026-09-23 11:00",
		"SessionEnd|clear|1|2026-09-23 11:30|2026-09-23 11:30",
		"SessionStart|resume|1|2026-09-23 09:00|2026-09-23 09:00",
		"StopFailure|rate_limit|1|2026-09-23 11:00|2026-09-23 11:00",
		"TaskCreated|Bash|1|2026-09-23 11:00|2026-09-23 11:00",
		"UserPromptExpansion|commit|1|2026-09-23 11:00|2026-09-23 11:00",
	)
}

func TestEventsFiltersNarrowAndLimitCaps(t *testing.T) {
	store := openStore(t)
	seedEvents(t, store)
	seedSession(t, store, callmeter.Session{SessionID: "s2", TS: ms(4 * time.Hour), Cwd: callmeter.Ptr("/w/p")})
	ctx := context.Background()
	for name, c := range map[string]struct {
		filter Filter
		want   int
	}{
		"session":   {Filter{Session: "s2"}, 1},
		"project":   {Filter{Project: "/w/p"}, 1},
		"agentType": {Filter{AgentType: "Explore"}, 1},
		"since":     {Filter{Since: testNow.Add(-45 * time.Minute)}, 1},
		"limit":     {Filter{Limit: 3}, 3},
	} {
		table, err := Events(ctx, store, c.filter, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(table.Rows) != c.want {
			t.Errorf("%s rows = %v, want %d", name, rowsOf(table), c.want)
		}
	}
}
