package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func i64(n int64) *int64 { return &n }

func seedTokens(t *testing.T, store *callmeter.Store) {
	t.Helper()
	seedRequest(t, store, callmeter.Request{
		RequestID: "r1", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)), Model: callmeter.Ptr("opus"),
		InputTokens: i64(100), CacheReadTokens: i64(800), CacheCreationTokens: i64(100),
		CacheCreation5mTokens: i64(60), CacheCreation1hTokens: i64(40), OutputTokens: i64(50),
	})
	seedRequest(t, store, callmeter.Request{
		RequestID: "r2", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)), Model: callmeter.Ptr("opus"),
		InputTokens: i64(100), CacheReadTokens: i64(700), CacheCreationTokens: i64(200),
		CacheCreation5mTokens: i64(200), CacheCreation1hTokens: i64(0), OutputTokens: i64(30),
	})
	seedAgent(t, store, callmeter.Agent{AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")})
	seedRequest(t, store, callmeter.Request{
		RequestID: "r3", SessionID: callmeter.Ptr("s1"), AgentID: callmeter.Ptr("a1"), TS: callmeter.Ptr(ms(time.Hour)),
		Model: callmeter.Ptr("haiku"), InputTokens: i64(0), CacheReadTokens: i64(0), CacheCreationTokens: i64(0),
		OutputTokens: i64(7),
	})
}

func TestTokensSumsTheSplitPerModelAndAgentType(t *testing.T) {
	store := openStore(t)
	seedTokens(t, store)
	table, err := Tokens(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	wantHeader(t, table, "MODEL", "AGENT TYPE", "REQUESTS", "INPUT", "CACHE READ", "CACHE WRITE 5M", "CACHE WRITE 1H",
		"CACHE WRITE", "OUTPUT", "CACHE HIT %")
	wantRows(t, table,
		"opus|-|2|200|1500|260|40|300|80|75.0",
		"haiku|Explore|1|0|0|-|-|0|7|-",
	)
	if len(table.Notes) != 0 {
		t.Errorf("notes = %q, want none", table.Notes)
	}
}

func TestHitRateIsOneDecimalAndDashOnAZeroDenominator(t *testing.T) {
	for _, c := range []struct {
		read, total int64
		want        string
	}{{1, 3, "33.3"}, {2, 3, "66.7"}, {0, 5, "0.0"}, {5, 5, "100.0"}, {0, 0, "-"}} {
		if got := hitRate(c.read, c.total); got != c.want {
			t.Errorf("hitRate(%d, %d) = %q, want %q", c.read, c.total, got, c.want)
		}
	}
}

func TestTokensAgentTypeNarrowsAndPendingIsANoteNotARow(t *testing.T) {
	store := openStore(t)
	seedTokens(t, store)
	seedRequest(t, store, callmeter.Request{
		RequestID: "r4", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)), Model: callmeter.Ptr("opus"),
		Pending: callmeter.Ptr(true),
	})
	ctx := context.Background()
	table, err := Tokens(ctx, store, Filter{AgentType: "Explore"}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	wantRows(t, table, "haiku|Explore|1|0|0|-|-|0|7|-")
	table, err = Tokens(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if table.Rows[0][2] != "2" {
		t.Errorf("opus requests = %s, want the pending request left out of the count", table.Rows[0][2])
	}
	if len(table.Notes) != 1 || table.Notes[0] != "1 requests still pending (context size not yet read from the transcript)" {
		t.Errorf("notes = %q, want the pending note", table.Notes)
	}
}

func TestTokensSessionSinceAndLimitNarrow(t *testing.T) {
	store := openStore(t)
	seedTokens(t, store)
	seedRequest(t, store, callmeter.Request{
		RequestID: "r5", SessionID: callmeter.Ptr("s2"), TS: callmeter.Ptr(ms(72 * time.Hour)), Model: callmeter.Ptr("sonnet"),
		InputTokens: i64(1), CacheReadTokens: i64(1), CacheCreationTokens: i64(0), OutputTokens: i64(1),
	})
	ctx := context.Background()
	table, err := Tokens(ctx, store, Filter{Session: "s2"}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	wantRows(t, table, "sonnet|-|1|1|1|-|-|0|1|50.0")
	table, err = Tokens(ctx, store, Filter{Since: testNow.Add(-24 * time.Hour)}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Errorf("since 24h rows = %v, want the old request gone", rowsOf(table))
	}
	table, err = Tokens(ctx, store, Filter{Limit: 1}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if len(table.Rows) != 1 {
		t.Errorf("limit 1 rows = %v, want 1", rowsOf(table))
	}
}
