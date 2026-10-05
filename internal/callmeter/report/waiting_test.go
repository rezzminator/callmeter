package report

import (
	"context"
	"testing"
	"time"
)

// TestWaitingCountsEachEndOnceEndsPermissionsAtTheNextRequestAndLeavesOpenOnesUnsummed:
// s1 has two idle notifications before one prompt (one wait, from the earlier),
// a second idle wait ended by a later prompt, and two permission notifications
// before one request (one wait, ended by that request though a prompt comes
// later); s2 has two open idle notifications (one open wait: a request does not
// end an idle wait) and one open permission wait.
func TestWaitingCountsEachEndOnceEndsPermissionsAtTheNextRequestAndLeavesOpenOnesUnsummed(t *testing.T) {
	store := openStore(t)
	for _, e := range []struct {
		id, session, event, kind string
		age                      time.Duration
	}{
		{"n1", "s1", "Notification", "idle_prompt", 5 * time.Hour},
		{"n2", "s1", "Notification", "idle_prompt", 4*time.Hour + 50*time.Minute},
		{"u1", "s1", "UserPromptSubmit", "", 4 * time.Hour},
		{"n3", "s1", "Notification", "idle_prompt", 2 * time.Hour},
		{"n4", "s1", "Notification", "permission_prompt", 3*time.Hour + 30*time.Minute},
		{"n5", "s1", "Notification", "permission_prompt", 3*time.Hour + 25*time.Minute},
		{"u2", "s1", "UserPromptSubmit", "", 90 * time.Minute},
		{"n6", "s2", "Notification", "idle_prompt", 30 * time.Minute},
		{"n7", "s2", "Notification", "idle_prompt", 25 * time.Minute},
		{"n8", "s2", "Notification", "permission_prompt", 5 * time.Minute},
		{"n9", "s2", "Notification", "auth_success", 20 * time.Minute},
	} {
		detail := any(nil)
		if e.kind != "" {
			detail = `{"notification_type":"` + e.kind + `"}`
		}
		exec(t, store, `INSERT INTO events (event_id, event, ts, session_id, detail) VALUES (?, ?, ?, ?, ?)`,
			e.id, e.event, ms(e.age), e.session, detail)
	}
	exec(t, store, `INSERT INTO requests (request_id, session_id, ts) VALUES ('q0', 's1', ?), ('q1', 's1', ?), ('q2', 's2', ?)`,
		ms(4*time.Hour), ms(3*time.Hour+20*time.Minute), ms(10*time.Minute))
	table, err := Waiting(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Waiting: %v", err)
	}
	wantHeader(t, table, "CHAT", "KIND", "WAITS", "TOTAL", "MEDIAN", "MAX")
	wantRows(t, table,
		"chat-s1|idle_prompt|2|1h30m|45m00s|1h00m",
		"chat-s1|permission_prompt|1|10m00s|10m00s|10m00s",
		"chat-s2|idle_prompt|1|-|-|-",
		"chat-s2|permission_prompt|1|-|-|-",
	)
	want := []string{
		"idle_prompt: 3 waits, total 1h30m, median 45m00s",
		"permission_prompt: 2 waits, total 10m00s, median 10m00s",
		"2 waits still open",
	}
	if len(table.Notes) != len(want) {
		t.Fatalf("notes = %q, want %q", table.Notes, want)
	}
	for i := range want {
		if table.Notes[i] != want[i] {
			t.Errorf("note %d = %q, want %q", i, table.Notes[i], want[i])
		}
	}
}
