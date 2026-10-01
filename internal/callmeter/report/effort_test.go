package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func seedEffort(t *testing.T, store *callmeter.Store) {
	t.Helper()
	for _, c := range []struct {
		id, effort, mode, agentType string
		failed                      bool
	}{
		{"c1", "high", "default", "", false},
		{"c2", "high", "default", "", true},
		{"c3", "", "plan", "Explore", false},
	} {
		call := mkCall(c.id, "s1", time.Hour, "Read")
		call.Failed = callmeter.Ptr(c.failed)
		if c.effort != "" {
			call.Effort = callmeter.Ptr(c.effort)
		}
		call.PermissionMode = callmeter.Ptr(c.mode)
		if c.agentType != "" {
			call.AgentType = callmeter.Ptr(c.agentType)
		}
		seed(t, store, call)
	}
	for _, turn := range []callmeter.Turn{
		{EventID: "t1", Event: "Stop", TS: ms(time.Hour), SessionID: callmeter.Ptr("s1"), Effort: callmeter.Ptr("high"),
			PermissionMode: callmeter.Ptr("default")},
		{EventID: "t2", Event: "StopFailure", TS: ms(time.Hour), SessionID: callmeter.Ptr("s1"), Effort: callmeter.Ptr("high"),
			PermissionMode: callmeter.Ptr("default")},
		{EventID: "t3", Event: "Stop", TS: ms(time.Hour), SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")},
	} {
		seedTurn(t, store, turn)
	}
}

func TestEffortCountsCallsTurnsAndFailedPerLevelModeAndAgentType(t *testing.T) {
	store := openStore(t)
	seedEffort(t, store)
	table, err := Effort(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	wantHeader(t, table, "EFFORT", "PERMISSION MODE", "AGENT TYPE", "CALLS", "TURNS", "FAILED")
	wantRows(t, table,
		"high|default|-|2|2|1",
		"-|plan|Explore|1|0|0",
		"-|-|Explore|0|1|0",
	)
}

func TestEffortAgentTypeNarrowsCallsAndTurns(t *testing.T) {
	store := openStore(t)
	seedEffort(t, store)
	table, err := Effort(context.Background(), store, Filter{AgentType: "Explore"}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	wantRows(t, table, "-|plan|Explore|1|0|0", "-|-|Explore|0|1|0")
}

// TestEffortProjectNarrowsTurnsThroughTheirSession: a turn has no cwd, so it
// follows its session's cwd.
func TestEffortProjectNarrowsTurnsThroughTheirSession(t *testing.T) {
	store := openStore(t)
	for session, cwd := range map[string]string{"sx": "/w/p", "sy": "/w/other"} {
		seedSession(t, store, callmeter.Session{SessionID: session, TS: ms(time.Hour), Cwd: callmeter.Ptr(cwd)})
		seedTurn(t, store, callmeter.Turn{
			EventID: "turn-" + session, Event: "Stop", TS: ms(time.Hour), SessionID: callmeter.Ptr(session),
			Effort: callmeter.Ptr("e-" + session),
		})
	}
	table, err := Effort(context.Background(), store, Filter{Project: "/w/p"}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	wantRows(t, table, "e-sx|-|-|0|1|0")
}

func TestEffortSessionSinceAndLimitNarrow(t *testing.T) {
	store := openStore(t)
	seedEffort(t, store)
	seedTurn(t, store, callmeter.Turn{
		EventID: "t-old", Event: "Stop", TS: ms(48 * time.Hour), SessionID: callmeter.Ptr("s2"), Effort: callmeter.Ptr("low"),
	})
	ctx := context.Background()
	table, err := Effort(ctx, store, Filter{Session: "s2"}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	wantRows(t, table, "low|-|-|0|1|0")
	table, err = Effort(ctx, store, Filter{Since: testNow.Add(-24 * time.Hour)}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	if len(table.Rows) != 3 {
		t.Errorf("since 24h rows = %v, want the old turn gone", rowsOf(table))
	}
	table, err = Effort(ctx, store, Filter{Limit: 1}, chatOf)
	if err != nil {
		t.Fatalf("Effort: %v", err)
	}
	if len(table.Rows) != 1 {
		t.Errorf("limit 1 rows = %v, want 1", rowsOf(table))
	}
}
