package report

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedSessions stores s1 (two runs, a SessionStart and SessionEnd, a main-chat
// request, two calls, one sub-agent) and s2 (one run, one call, a UTC offset
// and no zone name).
func seedSessions(t *testing.T, store *callmeter.Store) {
	t.Helper()
	seedEvent(t, store, callmeter.Event{
		EventID: "e-start", Event: callmeter.EventSessionStart, TS: ms(5 * time.Hour), SessionID: callmeter.Ptr("s1"),
		Source: callmeter.Ptr("startup"), Model: callmeter.Ptr("m-start"),
	})
	seedEvent(t, store, callmeter.Event{
		EventID: "e-end", Event: callmeter.EventSessionEnd, TS: ms(time.Hour), SessionID: callmeter.Ptr("s1"),
		Reason: callmeter.Ptr("clear"),
	})
	seedRequest(t, store, callmeter.Request{
		RequestID: "q1", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(2 * time.Hour)), Model: callmeter.Ptr("m-main"),
	})
	seed(t, store, mkCall("c1", "s1", 4*time.Hour, "Read"))
	seed(t, store, mkCall("c2", "s1", 3*time.Hour, "Read"))
	seed(t, store, mkCall("c3", "s2", 30*time.Minute, "Read"))
	seedAgent(t, store, callmeter.Agent{AgentID: "a1", SessionID: callmeter.Ptr("s1"), AgentType: callmeter.Ptr("Explore")})
	seedSession(t, store, callmeter.Session{
		SessionID: "s1", TS: ms(5 * time.Hour), Cwd: callmeter.Ptr("/w/p"), Host: callmeter.Ptr("mac1"),
		TZName: callmeter.Ptr("Europe/Berlin"), TZOffsetMinutes: callmeter.Ptr(int64(120)),
	})
	seedSession(t, store, callmeter.Session{SessionID: "s1", TS: ms(time.Hour)})
	seedSession(t, store, callmeter.Session{
		SessionID: "s2", TS: ms(30 * time.Minute), Cwd: callmeter.Ptr("/w/q"), TZOffsetMinutes: callmeter.Ptr(int64(-90)),
	})
}

func TestSessionsListsOneRowPerSessionLatestFirst(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	table, err := Sessions(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	wantHeader(t, table, "CHAT", "SESSION", "STARTED", "LAST", "MODEL", "START", "END", "CALLS", "AGENTS", "CWD", "HOST", "TZ")
	wantRows(t, table,
		"chat-s2|s2|2026-09-23 11:30|2026-09-23 11:30|-|-|-|1|0|/w/q|-|UTC-01:30",
		"chat-s1|s1|2026-09-23 07:00|2026-09-23 11:00|m-main|startup|clear|2|1|/w/p|mac1|Europe/Berlin",
	)
}

func TestSessionsFiltersNarrowAndLimitCaps(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	// s3's own cwd is elsewhere, but one of its calls ran under /w/p/sub.
	call := mkCall("c4", "s3", 20*time.Minute, "Read")
	call.Cwd = callmeter.Ptr("/w/p/sub")
	seed(t, store, call)
	seedSession(t, store, callmeter.Session{SessionID: "s3", TS: ms(20 * time.Minute), Cwd: callmeter.Ptr("/w/z")})
	ctx := context.Background()
	for name, c := range map[string]struct {
		filter Filter
		want   []string
	}{
		"session": {Filter{Session: "s1"}, []string{"s1"}},
		"project": {Filter{Project: "/w/p"}, []string{"s3", "s1"}},
		"since":   {Filter{Since: testNow.Add(-45 * time.Minute)}, []string{"s3", "s2"}},
		"limit":   {Filter{Limit: 1}, []string{"s3"}},
	} {
		table, err := Sessions(ctx, store, c.filter, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, row := range table.Rows {
			got = append(got, row[1])
		}
		if cells(got) != cells(c.want) {
			t.Errorf("%s sessions = %v, want %v", name, got, c.want)
		}
	}
}

func TestSessionsEmptyStoreHasHeaderAndNoRows(t *testing.T) {
	table, err := Sessions(context.Background(), openStore(t), Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(table.Rows) != 0 || len(table.Header) != 12 {
		t.Errorf("empty store: rows %v header %v, want no rows and the 12 columns", table.Rows, table.Header)
	}
}

// A session whose only row is a SessionEnd ended before it started: Claude Code
// never sent its SessionStart and nothing ran in it. Its START reads "never"
// and a note names it, while a session missing only its SessionStart but that
// ran something, or whose lost hook left a fault, keeps the unknown "-".
func TestSessionsNamesASessionEndedBeforeItStarted(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	ctx := context.Background()
	for id, ago := range map[string]time.Duration{"s7": 3 * time.Hour, "s8": 10 * time.Minute, "s9": 10 * time.Minute} {
		seedEvent(t, store, callmeter.Event{
			EventID: "end-" + id, Event: callmeter.EventSessionEnd, TS: ms(ago), SessionID: callmeter.Ptr(id),
			Reason: callmeter.Ptr("prompt_input_exit"),
		})
		seedSession(t, store, callmeter.Session{SessionID: id, TS: ms(ago)})
	}
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(11 * time.Minute), SessionID: "s8", Stage: callmeter.StageTerminated, Error: "signal",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	// s7's SessionEnd has a lost event naming no session 5 minutes before it:
	// the binary was unavailable, and that event may be s7's SessionStart. s9's
	// SessionEnd is hours later, past its reach.
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(3*time.Hour + 5*time.Minute), Stage: callmeter.StageBinary, Error: "SessionStart: download failed",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Sessions(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	wantRows(t, table,
		"chat-s8|s8|2026-09-23 11:50|2026-09-23 11:50|-|-|prompt_input_exit|0|0|-|-|-",
		"chat-s9|s9|2026-09-23 11:50|2026-09-23 11:50|-|never|prompt_input_exit|0|0|-|-|-",
		"chat-s2|s2|2026-09-23 11:30|2026-09-23 11:30|-|-|-|1|0|/w/q|-|UTC-01:30",
		"chat-s1|s1|2026-09-23 07:00|2026-09-23 11:00|m-main|startup|clear|2|1|/w/p|mac1|Europe/Berlin",
		"chat-s7|s7|2026-09-23 09:00|2026-09-23 09:00|-|-|prompt_input_exit|0|0|-|-|-",
	)
	want := "1 sessions ended before they started (START never): Claude Code sent a SessionEnd and no SessionStart, " +
		"and nothing ran in them; exiting while a /clear still runs its SessionEnd hooks ends the cleared chat's successor so"
	if !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
	table, err = Sessions(ctx, store, Filter{Session: "s1"}, chatOf)
	if err != nil {
		t.Fatalf("Sessions s1: %v", err)
	}
	for _, note := range table.Notes {
		if strings.Contains(note, "before they started") {
			t.Errorf("s1 notes = %q, want no ended-before-start note outside the filter", table.Notes)
		}
	}
}

// A session that started and then ran nothing (no prompt submitted, no request,
// call, sub-agent or turn: only local commands such as /model, or an exit at
// the prompt) keeps the model it started with, marked unused, and a note
// names it. A session with a submitted prompt, or a fault that may hide one,
// keeps its plain model.
func TestSessionsNamesASessionThatRanNothing(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	ctx := context.Background()
	for id, ago := range map[string]time.Duration{"s4": 40 * time.Minute, "s5": 20 * time.Minute, "s6": 15 * time.Minute} {
		seedEvent(t, store, callmeter.Event{
			EventID: "start-" + id, Event: callmeter.EventSessionStart, TS: ms(ago + time.Minute), SessionID: callmeter.Ptr(id),
			Source: callmeter.Ptr("startup"), Model: callmeter.Ptr("m-idle"),
		})
		seedEvent(t, store, callmeter.Event{
			EventID: "loaded-" + id, Event: "InstructionsLoaded", TS: ms(ago + time.Minute), SessionID: callmeter.Ptr(id),
			LoadReason: callmeter.Ptr("session_start"),
		})
		seedEvent(t, store, callmeter.Event{
			EventID: "end-" + id, Event: callmeter.EventSessionEnd, TS: ms(ago), SessionID: callmeter.Ptr(id),
			Reason: callmeter.Ptr("prompt_input_exit"),
		})
		seedSession(t, store, callmeter.Session{SessionID: id, TS: ms(ago + time.Minute)})
		seedSession(t, store, callmeter.Session{SessionID: id, TS: ms(ago)})
	}
	// s6 submitted a prompt the API refused: no request, but not nothing.
	seedEvent(t, store, callmeter.Event{
		EventID: "prompt-s6", Event: "UserPromptSubmit", TS: ms(15*time.Minute + 30*time.Second), SessionID: callmeter.Ptr("s6"),
		PromptBytes: callmeter.Ptr(int64(3)),
	})
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(40*time.Minute + 30*time.Second), SessionID: "s4", Stage: callmeter.StageTerminated, Error: "signal",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Sessions(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	rows := map[string]string{}
	for _, row := range table.Rows {
		rows[row[1]] = row[4]
	}
	for id, want := range map[string]string{"s4": "m-idle", "s5": "m-idle (unused)", "s6": "m-idle", "s1": "m-main"} {
		if rows[id] != want {
			t.Errorf("%s MODEL = %q, want %q (rows %v)", id, rows[id], want, table.Rows)
		}
	}
	want := "1 sessions ran nothing (MODEL unused): they started, then ended or idled with no prompt submitted and " +
		"no request, call or sub-agent (only local commands such as /model or /effort, or an exit at the prompt); " +
		"the model is the one they started with, never one that answered"
	if !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
}

// A session whose own SessionEnd hook ran and was lost (end_reason
// callmeter.EndReasonLost) reads END "lost", and a note says the hook ran and
// its event was lost; a session ending with a SessionEnd reason gets no such
// note.
func TestSessionsNamesASessionWithALostSessionEnd(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	if _, err := store.DB().Exec(`UPDATE sessions SET end_reason = ? WHERE session_id = 's2'`, callmeter.EndReasonLost); err != nil {
		t.Fatalf("mark s2: %v", err)
	}
	ctx := context.Background()
	table, err := Sessions(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := "1 sessions lost their SessionEnd (END lost): the SessionEnd hook ran, but its event was not recorded " +
		"(the hook was killed, its binary was unavailable, or the store stayed busy past the hook's timeout; see the " +
		"faults topic); once they went quiet, the report settled their turns and calls from their transcripts as " +
		"SessionEnd would"
	if !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
	table, err = Sessions(ctx, store, Filter{Session: "s1"}, chatOf)
	if err != nil {
		t.Fatalf("Sessions s1: %v", err)
	}
	for _, note := range table.Notes {
		if strings.Contains(note, "lost their SessionEnd") {
			t.Errorf("s1 notes = %q, want no lost-SessionEnd note outside the filter", table.Notes)
		}
	}
}

// A session report recovery marked as ended with no SessionEnd (end_reason
// callmeter.EndReasonNever) reads END "never", and a note says what that
// means; a session ending with a SessionEnd reason gets no such note.
func TestSessionsNamesASessionWithNoSessionEnd(t *testing.T) {
	store := openStore(t)
	seedSessions(t, store)
	if _, err := store.DB().Exec(`UPDATE sessions SET end_reason = ? WHERE session_id = 's2'`, callmeter.EndReasonNever); err != nil {
		t.Fatalf("mark s2: %v", err)
	}
	ctx := context.Background()
	table, err := Sessions(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	wantRows(t, table,
		"chat-s2|s2|2026-09-23 11:30|2026-09-23 11:30|-|-|never|1|0|/w/q|-|UTC-01:30",
		"chat-s1|s1|2026-09-23 07:00|2026-09-23 11:00|m-main|startup|clear|2|1|/w/p|mac1|Europe/Berlin",
	)
	want := "1 sessions ended with no SessionEnd (END never): Claude Code did not run their SessionEnd hooks, " +
		"and no lost one was recorded; once they went quiet, the report settled their turns and calls from their " +
		"transcripts as SessionEnd would; a session idle past the quiet hour reads so too, until its next hook"
	if !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
	table, err = Sessions(ctx, store, Filter{Session: "s1"}, chatOf)
	if err != nil {
		t.Fatalf("Sessions s1: %v", err)
	}
	for _, note := range table.Notes {
		if strings.Contains(note, "no SessionEnd") {
			t.Errorf("s1 notes = %q, want no no-SessionEnd note outside the filter", table.Notes)
		}
	}
}
