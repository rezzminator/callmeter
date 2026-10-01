package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedAgentTurns stores a1 (a closed turn of 90 s, then an open one) in s1 and
// a2 (a stop with no start) in s2.
func seedAgentTurns(t *testing.T, store *callmeter.Store) {
	t.Helper()
	edge := func(id, event, agent, session string, ts int64, prompt *string) callmeter.Event {
		return callmeter.Event{
			EventID: id, Event: event, TS: ts, SessionID: callmeter.Ptr(session), AgentID: callmeter.Ptr(agent),
			AgentType: callmeter.Ptr("Explore"), PromptID: prompt,
		}
	}
	first := ms(3 * time.Hour)
	seedEvent(t, store, edge("e1", callmeter.EventSubagentStart, "a1", "s1", first, callmeter.Ptr("p1")))
	seedEvent(t, store, edge("e2", callmeter.EventSubagentStop, "a1", "s1", first+90_000, callmeter.Ptr("p1")))
	seedEvent(t, store, edge("e3", callmeter.EventSubagentStart, "a1", "s1", ms(time.Hour), callmeter.Ptr("p1")))
	seedEvent(t, store, edge("e4", callmeter.EventSubagentStop, "a2", "s2", ms(30*time.Minute), nil))
}

func TestAgentsListsOneRowPerTurnWithAnOpenTurnDashed(t *testing.T) {
	store := openStore(t)
	seedAgentTurns(t, store)
	table, err := Agents(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	wantHeader(t, table, "AGENT", "TYPE", "CHAT", "TURN", "STARTED", "STOPPED", "SECONDS", "PROMPT")
	wantRows(t, table,
		"a2|Explore|chat-s2|1|-|2026-09-23 11:30|-|-",
		"a1|Explore|chat-s1|2|2026-09-23 11:00|-|-|p1",
		"a1|Explore|chat-s1|1|2026-09-23 09:00|2026-09-23 09:01|90|p1",
	)
}

func TestAgentsFiltersNarrowAndLimitCaps(t *testing.T) {
	store := openStore(t)
	seedAgentTurns(t, store)
	seedSession(t, store, callmeter.Session{SessionID: "s1", TS: ms(time.Hour), Cwd: callmeter.Ptr("/w/p")})
	ctx := context.Background()
	for name, c := range map[string]struct {
		filter Filter
		want   int
	}{
		"session":   {Filter{Session: "s1"}, 2},
		"project":   {Filter{Project: "/w/p"}, 2},
		"agentType": {Filter{AgentType: "Plan"}, 0},
		"since":     {Filter{Since: testNow.Add(-2 * time.Hour)}, 2},
		"limit":     {Filter{Limit: 1}, 1},
	} {
		table, err := Agents(ctx, store, c.filter, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(table.Rows) != c.want {
			t.Errorf("%s rows = %v, want %d", name, rowsOf(table), c.want)
		}
	}
}
