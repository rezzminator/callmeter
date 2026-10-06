package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedPrompts stores p1 (3 calls, one failed, 2 requests, 2 sub-agents), p2
// (1 call) and p3 (a request and no call, in another chat).
func seedPrompts(t *testing.T, store *callmeter.Store) {
	t.Helper()
	for _, c := range []struct {
		id        string
		age       time.Duration
		prompt    string
		agentID   string
		agentType string
		failed    bool
	}{
		{"c1", 3 * time.Hour, "p1", "", "", false},
		{"c2", 2 * time.Hour, "p1", "a1", "Explore", false},
		{"c3", time.Hour, "p1", "", "", true},
		{"c4", 30 * time.Minute, "p2", "", "", false},
	} {
		call := mkCall(c.id, "s1", c.age, "Read")
		call.PromptID, call.AgentID, call.Failed = callmeter.Ptr(c.prompt), callmeter.Ptr(c.agentID), callmeter.Ptr(c.failed)
		if c.agentType != "" {
			call.AgentType = callmeter.Ptr(c.agentType)
		}
		seed(t, store, call)
	}
	seedRequest(t, store, callmeter.Request{
		RequestID: "q1", SessionID: callmeter.Ptr("s1"), PromptID: callmeter.Ptr("p1"), TS: callmeter.Ptr(ms(3 * time.Hour)),
		ContextTokens: callmeter.Ptr(int64(100)), OutputTokens: callmeter.Ptr(int64(10)),
	})
	seedRequest(t, store, callmeter.Request{
		RequestID: "q2", SessionID: callmeter.Ptr("s1"), AgentID: callmeter.Ptr("a1"), PromptID: callmeter.Ptr("p1"),
		TS: callmeter.Ptr(ms(2 * time.Hour)), ContextTokens: callmeter.Ptr(int64(200)), OutputTokens: callmeter.Ptr(int64(20)),
	})
	seedRequest(t, store, callmeter.Request{
		RequestID: "q3", SessionID: callmeter.Ptr("s9"), PromptID: callmeter.Ptr("p3"), TS: callmeter.Ptr(ms(time.Hour)),
		ContextTokens: callmeter.Ptr(int64(50)), OutputTokens: callmeter.Ptr(int64(5)),
	})
	seedAgent(t, store, callmeter.Agent{
		AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore"), PromptID: callmeter.Ptr("p1"),
		Started: callmeter.Ptr(ms(2 * time.Hour)),
	})
	seedAgent(t, store, callmeter.Agent{
		AgentID: "a2", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Plan"), PromptID: callmeter.Ptr("p1"),
		Started: callmeter.Ptr(ms(2 * time.Hour)),
	})
	// p1 took two turns of 12 s and 3 s; p3's duration was never measured
	exec(t, store, `INSERT INTO turn_durations (entry_id, session_id, prompt_id, ts, duration_ms) VALUES
		('d1', 's1', 'p1', ?, 12000), ('d2', 's1', 'p1', ?, 3000), ('d3', 's9', 'p3', ?, NULL)`,
		ms(2*time.Hour), ms(time.Hour), ms(time.Hour))
}

func TestPromptsListsOneRowPerPromptMostCallsFirst(t *testing.T) {
	store := openStore(t)
	seedPrompts(t, store)
	table, err := Prompts(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	wantHeader(t, table,
		"PROMPT", "CHAT", "FIRST", "LAST", "CALLS", "FAILED", "AGENTS", "REQUESTS", "CONTEXT TOKENS", "OUTPUT TOKENS", "WALL S")
	wantRows(t, table,
		"p1|chat-s1|2026-09-23 09:00|2026-09-23 11:00|3|1|2|2|300|30|15.0",
		"p2|chat-s1|2026-09-23 11:30|2026-09-23 11:30|1|0|0|0|0|0|-",
		"p3|chat-s9|-|-|0|0|0|1|50|5|-",
	)
}

// TestPromptsAgentTypeNarrowsCallsAgentsAndRequests: the filter reaches the
// three tables a prompt row sums, the requests through their agent.
func TestPromptsAgentTypeNarrowsCallsAgentsAndRequests(t *testing.T) {
	store := openStore(t)
	seedPrompts(t, store)
	table, err := Prompts(context.Background(), store, Filter{AgentType: "Explore"}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	wantRows(t, table, "p1|chat-s1|2026-09-23 10:00|2026-09-23 10:00|1|0|1|1|200|20|15.0")
}

func TestPromptsSessionAndLimitNarrow(t *testing.T) {
	store := openStore(t)
	seedPrompts(t, store)
	ctx := context.Background()
	table, err := Prompts(ctx, store, Filter{Session: "s9"}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	wantRows(t, table, "p3|chat-s9|-|-|0|0|0|1|50|5|-")
	table, err = Prompts(ctx, store, Filter{Limit: 2}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	if len(table.Rows) != 2 || table.Rows[0][0] != "p1" {
		t.Errorf("limit 2 rows = %v, want the two busiest prompts", rowsOf(table))
	}
}

func TestPromptsPendingRequestsAreANote(t *testing.T) {
	store := openStore(t)
	seedPrompts(t, store)
	seedRequest(t, store, callmeter.Request{
		RequestID: "q4", SessionID: callmeter.Ptr("s1"), PromptID: callmeter.Ptr("p2"), TS: callmeter.Ptr(ms(time.Hour)),
		Pending: callmeter.Ptr(true),
	})
	table, err := Prompts(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	if len(table.Notes) != 1 || table.Notes[0] != "1 requests still pending (its session has not ended: still live, or killed before its Stop): context size unknown, not counted" {
		t.Errorf("notes = %q, want the pending note", table.Notes)
	}
}
