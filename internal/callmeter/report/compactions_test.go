package report

import (
	"context"
	"testing"
	"time"
)

func TestCompactionsListsNewestFirstWithFreedAndTheWindowNote(t *testing.T) {
	store := openStore(t)
	for _, c := range []struct {
		id, session, trigger string
		age                  time.Duration
		pre, post, dropped   any
		duration             any
	}{
		{"e1", "s1", "auto", 3 * time.Hour, 150000, 20000, 130000, 42500},
		{"e2", "s1", "manual", time.Hour, 100000, 15000, 215000, 12000},
		{"e3", "s2", "auto", 5 * time.Hour, 90000, nil, nil, nil},
	} {
		exec(t, store, `INSERT INTO compactions (entry_id, session_id, ts, trigger, pre_tokens, post_tokens,
			cumulative_dropped_tokens, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.id, c.session, ms(c.age), c.trigger, c.pre, c.post, c.dropped, c.duration)
	}
	table, err := Compactions(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Compactions: %v", err)
	}
	wantHeader(t, table, "CHAT", "TIME", "TRIGGER", "BEFORE", "AFTER", "FREED", "DROPPED TOTAL", "SECONDS")
	wantRows(t, table,
		"chat-s1|2026-09-23 11:00|manual|100000|15000|85000|215000|12.0",
		"chat-s1|2026-09-23 09:00|auto|150000|20000|130000|130000|42.5",
		"chat-s2|2026-09-23 07:00|auto|90000|-|-|-|-",
	)
	if len(table.Notes) != 1 ||
		table.Notes[0] != "3 compactions (2 auto, 1 manual), 215000 tokens freed, 54.5 s spent compacting" {
		t.Errorf("notes = %q, want the window total", table.Notes)
	}
	table, err = Compactions(context.Background(), store, Filter{Session: "s2", Limit: 1}, chatOf)
	if err != nil {
		t.Fatalf("Compactions s2: %v", err)
	}
	wantRows(t, table, "chat-s2|2026-09-23 07:00|auto|90000|-|-|-|-")
}
