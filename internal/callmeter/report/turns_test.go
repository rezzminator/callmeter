package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func TestTurnsListsDurationsWithTheEffortOfTheirTurnAndTheStats(t *testing.T) {
	store := openStore(t)
	p := callmeter.Ptr[string]
	for _, turn := range []callmeter.Turn{
		{EventID: "t1", Event: "Stop", TS: ms(65 * time.Minute), SessionID: p("s1"), PromptID: p("abcdef1234"), Effort: p("high")},
		{EventID: "t2", Event: "Stop", TS: ms(35 * time.Minute), SessionID: p("s1"), PromptID: p("abcdef1234"), Effort: p("low")},
		{EventID: "t3", Event: "Stop", TS: ms(10 * time.Minute), SessionID: p("s1"), PromptID: p("abcdef1234"), Effort: p("max")},
	} {
		seedTurn(t, store, turn)
	}
	for _, d := range []struct {
		id, session string
		prompt, dur any
		age         time.Duration
		messages    int
		background  int
	}{
		{"d1", "s1", "abcdef1234", 12000, time.Hour, 5, 0},
		{"d2", "s1", "abcdef1234", 3000, 30 * time.Minute, 2, 1},
		{"d3", "s2", nil, nil, 2 * time.Hour, 3, 0},
	} {
		exec(t, store, `INSERT INTO turn_durations (entry_id, session_id, prompt_id, ts, duration_ms, message_count, background_agents)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, d.id, d.session, d.prompt, ms(d.age), d.dur, d.messages, d.background)
	}
	table, err := Turns(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Turns: %v", err)
	}
	wantHeader(t, table, "CHAT", "TIME", "PROMPT", "WALL S", "MESSAGES", "BG AGENTS", "EFFORT")
	wantRows(t, table,
		"chat-s1|2026-09-23 11:30|abcdef12|3.0|2|1|low",
		"chat-s1|2026-09-23 11:00|abcdef12|12.0|5|0|high",
		"chat-s2|2026-09-23 10:00|-|-|3|0|-",
	)
	if len(table.Notes) != 1 || table.Notes[0] != "2 turns, total 15.0 s, median 7.5 s, p90 12.0 s" {
		t.Errorf("notes = %q, want the stats over the timed turns", table.Notes)
	}
}
