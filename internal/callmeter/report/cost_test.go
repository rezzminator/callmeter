package report

import (
	"context"
	"testing"
	"time"
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
	wantHeader(t, table, "CHAT", "COST USD", "API S", "RETRY S", "TOOL S", "WALL S", "MODELS")
	wantRows(t, table,
		"chat-s2|4.50|1.0|-|-|3.0|-",
		"chat-s1|1.23|100.0|10.0|5.5|200.0|opus=1.00 haiku=0.23",
	)
	if len(table.Notes) != 1 || table.Notes[0] != "5.73 USD in total over 2 sessions (Claude Code's own accounting)" {
		t.Errorf("notes = %q, want the total over the window's sessions", table.Notes)
	}
}
