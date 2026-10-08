package report

import (
	"context"
	"testing"
	"time"
)

// A resume whose Claude Code sent no cost fields is counted in a note, its
// cells "-": never a 0 and never a USD in the total.
func TestResumesListsColdResumesAndCountsTheOnesWithNoCostFields(t *testing.T) {
	store := openStore(t)
	for _, e := range []struct {
		id, session, source, detail string
		age                         time.Duration
	}{
		{"r1", "s1", "resume", `{"seconds_since_last_response":7380,"context_tokens":150000,"prompt_cache_likely_expired":true,"estimated_cache_write_usd":0.5625}`, time.Hour},
		{"r2", "s2", "resume", `{"seconds_since_last_response":90,"context_tokens":2000,"prompt_cache_likely_expired":false,"estimated_cache_write_usd":0.0123}`, 2 * time.Hour},
		{"r3", "s3", "resume", `{"context_tokens":5}`, 3 * time.Hour},
		{"r4", "s3", "startup", `{"context_tokens":7}`, 4 * time.Hour},
	} {
		exec(t, store, `INSERT INTO events (event_id, event, ts, session_id, source, detail)
			VALUES (?, 'SessionStart', ?, ?, ?, ?)`, e.id, ms(e.age), e.session, e.source, e.detail)
	}
	table, err := Resumes(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Resumes: %v", err)
	}
	wantHeader(t, table, "CHAT", "TIME", "IDLE", "CONTEXT", "CACHE EXPIRED", "EST CACHE WRITE USD")
	wantRows(t, table,
		"chat-s1|2026-09-23 11:00|2h03m|150000|yes|0.5625",
		"chat-s2|2026-09-23 10:00|1m30s|2000|no|0.0123",
		"chat-s3|2026-09-23 09:00|-|5|-|-",
	)
	want := []string{
		"3 resumes, 1 with the prompt cache likely expired, est. 0.5748 USD of cache writes",
		"1 resumes carry no cost fields (older Claude Code)",
	}
	if len(table.Notes) != 2 || table.Notes[0] != want[0] || table.Notes[1] != want[1] {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
}
