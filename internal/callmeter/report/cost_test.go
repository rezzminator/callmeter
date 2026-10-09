package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func TestCostListsSessionsByCostWithRetryAndModels(t *testing.T) {
	store := openStore(t)
	exec(t, store, `INSERT INTO session_costs (session_id, ts, cost_usd, api_ms, api_no_retry_ms, tool_ms, wall_ms, model_costs)
		VALUES ('s1', ?, 1.234, 100000, 90000, 5500, 200000, '{"haiku":0.234,"opus":1.0}')`, ms(time.Hour))
	exec(t, store, `INSERT INTO session_costs (session_id, ts, cost_usd, api_ms, wall_ms) VALUES ('s2', ?, 4.5, 1000, 3000)`,
		ms(2*time.Hour))
	exec(t, store, `INSERT INTO session_costs (session_id, ts, cost_usd) VALUES ('s3', ?, 9)`, ms(10*24*time.Hour))
	table, err := Cost(context.Background(), store, Filter{Since: testNow.Add(-24 * time.Hour)}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantHeader(t, table, "CHAT", "REQUESTS USD", "CLAUDE CODE USD", "DIFF USD", "SIDE USD", "API S", "RETRY S", "TOOL S", "WALL S", "MODELS")
	// No request row of either chat: its request sum and difference are unknown, never zero.
	wantRows(t, table,
		"chat-s2|-|4.50|-|-|1.0|-|-|3.0|-",
		"chat-s1|-|1.23|-|-|100.0|10.0|5.5|200.0|opus=1.00 haiku=0.23",
	)
	hasNote(t, table, "5.73 USD in total over 2 sessions (Claude Code's own accounting)")
}

func hasNote(t *testing.T, table *Table, want string) {
	t.Helper()
	for _, note := range table.Notes {
		if note == want {
			return
		}
	}
	t.Errorf("notes = %q, want one equal to %q", table.Notes, want)
}

// seedPricedRequest writes a requests row with its price columns, which only
// the price pass and the ledger ingest set.
func seedPricedRequest(t *testing.T, store *callmeter.Store, id, session, agent string, at time.Duration, source string, micros any, costSource any, query any) {
	t.Helper()
	var agentID any
	if agent != "" {
		agentID = agent
	}
	exec(t, store, `INSERT INTO requests (request_id, session_id, agent_id, ts, model, input_tokens, output_tokens, cache_read_tokens,
		cache_creation_tokens, pending, source, cost_usd_micros, cost_source, query_source) VALUES (?, ?, ?, ?, 'claude-opus-5-5', 10, 20, 30, 40, 0, ?, ?, ?, ?)`,
		id, session, agentID, ms(at), source, micros, costSource, query)
}

// seedCostSession is chat s1: a main call and a sub-agent call priced by the
// ledger, a main call priced from the table, a side call, a side call whose
// ledger cost has not arrived, a side call of an unknown agent, and a call of
// a model the table does not list; Claude Code's own total is 1.00.
func seedCostSession(t *testing.T, store *callmeter.Store) {
	t.Helper()
	exec(t, store, `INSERT INTO session_costs (session_id, ts, cost_usd) VALUES ('s1', ?, 1.0)`, ms(time.Hour))
	seedAgent(t, store, callmeter.Agent{AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")})
	seedPricedRequest(t, store, "m1", "s1", "", 5*time.Hour, "hook", 400000, "ledger", "repl_main_thread")
	seedPricedRequest(t, store, "x1", "s1", "a1", 4*time.Hour, "hook", 100000, "ledger", "agent:builtin:Explore")
	seedPricedRequest(t, store, "m2", "s1", "", 3*time.Hour, "hook", 200000, "table", nil)
	seedPricedRequest(t, store, "req_side1", "s1", "a1", 2*time.Hour, "ledger", 150000, "ledger", "web_search_tool")
	seedPricedRequest(t, store, "req_side2", "s1", "", time.Hour, "ledger", 3000, "table", "agent_summary")
	seedPricedRequest(t, store, "req_side3", "s1", "?", 50*time.Minute, "ledger", 7000, "ledger", "prompt_suggestion")
	seedPricedRequest(t, store, "m3", "s1", "", 40*time.Minute, "hook", nil, nil, nil)
}

func TestCostChatComparesTheRequestSumWithClaudeCodesTotal(t *testing.T) {
	store := openStore(t)
	seedCostSession(t, store)
	ledger := &callmeter.LedgerSummary{Dir: "/ledger", Present: true, Files: 1, Rows: 6, Held: 2, Side: 3}
	table, err := Cost(context.Background(), store, Filter{Ledger: ledger}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// Priced requests: 0.40 + 0.10 + 0.20 + 0.15 + 0.003 + 0.007 = 0.86; side: 0.15 + 0.003 + 0.007.
	wantRows(t, table, "chat-s1|0.86|1.00|+0.14|0.16|-|-|-|-|-")
	for _, want := range []string{
		"REQUESTS USD sums each request's cost: 'ledger' is Claude Code's own price from the cost ledger, 'table' is callmeter's embedded price list at the row's tokens",
		"DIFF USD is Claude Code's total minus REQUESTS USD: spend Claude Code accounts for that no priced request row does",
		"SIDE USD is the part of REQUESTS USD from calls no transcript records (the cost ledger's side calls)",
		"1 request has no price (a model the table does not list): not in REQUESTS USD",
		"1 side call has no ledger price yet: priced from the table, its cache writes taken as 5-minute ones",
		"1 side call has an unknown agent (the ledger's log half arrived without its span)",
		"cost ledger /ledger: 6 calls in the window, 2 held by a transcript, 3 side calls",
	} {
		hasNote(t, table, want)
	}
}

func TestCostWithoutALedgerSaysSideCallsAreNotCounted(t *testing.T) {
	store := openStore(t)
	seedCostSession(t, store)
	for _, tc := range []struct {
		name   string
		ledger *callmeter.LedgerSummary
		want   string
	}{
		{"not looked for", nil, "side calls are not counted: no cost ledger was read"},
		{"directory absent", &callmeter.LedgerSummary{Dir: "/ledger"}, "side calls are not counted: no cost ledger at /ledger"},
	} {
		table, err := Cost(context.Background(), store, Filter{Ledger: tc.ledger}, chatOf)
		if err != nil {
			t.Fatalf("%s: Cost: %v", tc.name, err)
		}
		hasNote(t, table, tc.want)
	}
}

func TestCostNamesEveryLedgerGap(t *testing.T) {
	store := openStore(t)
	seedCostSession(t, store)
	table, err := Cost(context.Background(), store, Filter{Ledger: &callmeter.LedgerSummary{
		Dir: "/ledger", Present: true, Files: 2, Rows: 9, Faults: 3, Held: 4, HeldUnrecorded: 2, Side: 5, Unreadable: 1,
	}}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	for _, want := range []string{
		"3 ledger lines were skipped: not a JSON object, or missing a field the ledger contract requires",
		"2 ledger calls are held by a transcript the store has not recorded yet: not counted until a hook or recovery writes them",
		"1 session's transcripts could not be read: its ledger calls the store did not know are counted as side calls",
	} {
		hasNote(t, table, want)
	}
	table, err = Cost(context.Background(), store, Filter{Ledger: &callmeter.LedgerSummary{Dir: "/ledger", Present: true, SchemaIncomplete: true}}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	hasNote(t, table, "side calls are not counted: this store has not gained the cost columns yet; the next hook or report adds them")
}

func TestCostAgentViewSumsEachAgentWithItsSideCalls(t *testing.T) {
	store := openStore(t)
	seedCostSession(t, store)
	table, err := Cost(context.Background(), store, Filter{CostView: "agent"}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantHeader(t, table, "CHAT", "AGENT", "TYPE", "REQUESTS", "USD", "LEDGER USD", "TABLE USD", "SIDE USD")
	wantRows(t, table,
		"chat-s1|main|-|4|0.60|0.40|0.20|0.00",
		"chat-s1|a1|Explore|2|0.25|0.25|0.00|0.15",
		"chat-s1|? (unknown)|-|1|0.01|0.01|0.00|0.01",
	)
}

func TestCostCallViewListsTheDearestCallsFirst(t *testing.T) {
	store := openStore(t)
	seedCostSession(t, store)
	table, err := Cost(context.Background(), store, Filter{CostView: "call", Limit: 3}, chatOf)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	wantHeader(t, table, "CHAT", "TIME", "AGENT", "QUERY SOURCE", "MODEL", "IN", "CACHE READ", "CACHE WRITE", "OUT", "USD", "PRICE FROM", "SIDE CALL")
	wantRows(t, table,
		"chat-s1|2026-09-23 07:00|main|repl_main_thread|claude-opus-5-5|10|30|40|20|0.4000|ledger|-",
		"chat-s1|2026-09-23 09:00|main|-|claude-opus-5-5|10|30|40|20|0.2000|table|-",
		"chat-s1|2026-09-23 10:00|a1|web_search_tool|claude-opus-5-5|10|30|40|20|0.1500|ledger|yes",
	)
}
