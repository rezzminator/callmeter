package callmeter

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The result and message texts below must appear nowhere in the store: only
// sizes, flags and labels are stored (docs/testing.md § Bug classes, Privacy).
const (
	secretResult  = "SECRET-RESULT-TEXT"
	secretMessage = "SECRET-MESSAGE-TEXT"
	secretPrompt  = "SECRET-PROMPT-TEXT"
	failedResult  = "Exit code 1\n" + secretResult // SanitizeError stores its first line
)

// stamp is a transcript entry's timestamp, as Claude Code writes it.
func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }

// The line builders copy the shapes of a captured transcript
// (testdata/transcript.jsonl and the gym S2 capture): a prompt, an assistant
// entry per content block carrying the message's usage so far, a tool_result.
func promptLine(promptID, text string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","promptId":%q,"message":{"role":"user","content":%q},"timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1"}`,
		promptID, text, stamp(at))
}

func toolUseLine(messageID, toolUseID string, output int, at time.Time) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":%q,"type":"message","role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"false","description":"Fail"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":10789,"cache_read_input_tokens":13689,"output_tokens":%d}},"requestId":"req_demo_1","timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1"}`,
		messageID, toolUseID, output, stamp(at))
}

func textLine(messageID, text string, output int, at time.Time) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":%q,"type":"message","role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":10,"cache_creation_input_tokens":10789,"cache_read_input_tokens":13689,"output_tokens":%d}},"requestId":"req_demo_1","timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1"}`,
		messageID, text, output, stamp(at))
}

func quietResultLine(toolUseID, text string, isError bool, at time.Time) string {
	flag := ""
	if isError {
		flag = `"is_error":true,`
	}
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":%q,%s"tool_use_id":%q}]},"timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1"}`,
		text, flag, toolUseID, stamp(at))
}

// quietStore is a session whose hooks stopped after PreToolUse: a main-chat
// call and a sub-agent's, each with its start columns only, and the
// transcripts the two ran in.
type quietStore struct {
	store    *Store
	now      time.Time
	main     string // the main transcript
	sub      string // the sub-agent a1's transcript
	callTS   int64
	firstTS  int64
	toolMain string
	toolSub  string
}

// newQuietStore builds the store and the transcripts: the session's last hook
// was lastHook before now, and every transcript was last written mtimeAge
// before now.
func newQuietStore(t *testing.T, lastHook, mtimeAge time.Duration) quietStore {
	t.Helper()
	ctx := context.Background()
	q := quietStore{store: openTestStore(t), now: time.Now(), toolMain: "toolu_main", toolSub: "toolu_sub"}
	dir := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj")
	q.main = filepath.Join(dir, "sess-1.jsonl")
	q.sub = filepath.Join(dir, "sess-1", "subagents", "agent-a1.jsonl")
	start := q.now.Add(-2 * time.Hour)
	q.firstTS = start.Add(-time.Minute).UnixMilli()
	q.callTS = start.UnixMilli()
	// Two lines of one message: the later line's usage is the final one.
	q.writeTranscript(t, q.main,
		promptLine("prompt-1", secretPrompt, start),
		toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)),
		textLine("msg_main", secretMessage, 77, start.Add(2*time.Second)),
		quietResultLine(q.toolMain, failedResult, true, start.Add(3*time.Second)),
	)
	q.writeTranscript(t, q.sub,
		toolUseLine("msg_sub", q.toolSub, 31, start.Add(4*time.Second)),
		quietResultLine(q.toolSub, secretResult, false, start.Add(5*time.Second)),
	)
	mtime := q.now.Add(-mtimeAge)
	for _, file := range []string{q.main, q.sub} {
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatalf("set mtime of %s: %v", file, err)
		}
	}
	err := q.store.Batch(ctx, func(tx *Tx) error {
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: q.firstTS, TranscriptPath: Ptr(q.main),
			SeatDir: Ptr("/tmp/demo-seat"), ConfigDir: Ptr("/tmp/demo-config")}); err != nil {
			return err
		}
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: q.now.Add(-lastHook).UnixMilli()}); err != nil {
			return err
		}
		for _, call := range []Call{
			{ToolUseID: q.toolMain, SessionID: Ptr("sess-1"), TS: Ptr(q.callTS), Tool: Ptr("Bash"), Source: Ptr(SourceHook)},
			{ToolUseID: q.toolSub, SessionID: Ptr("sess-1"), AgentID: Ptr("a1"), AgentType: Ptr("Explore"), TS: Ptr(q.callTS),
				Tool: Ptr("Bash"), Source: Ptr(SourceHook)},
		} {
			if err := tx.UpsertCall(ctx, call, Overwrite); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the quiet session: %v", err)
	}
	return q
}

func (q quietStore) writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (q quietStore) recover(t *testing.T) RecoverSummary {
	t.Helper()
	summary, err := q.store.RecoverQuiet(context.Background(), q.now, QuietAfter)
	if err != nil {
		t.Fatalf("RecoverQuiet: %v", err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("RecoverQuiet skipped %v, want nothing skipped", summary.Skipped)
	}
	return summary
}

// snapshot is every calls and requests row, for an unchanged-store check.
func (q quietStore) snapshot(t *testing.T) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, where := range [][2]string{
		{"calls", "tool_use_id = 'toolu_main'"}, {"calls", "tool_use_id = 'toolu_sub'"},
		{"requests", "request_id = 'msg_main'"}, {"requests", "request_id = 'msg_sub'"},
	} {
		rows = append(rows, row(t, q.store, where[0], where[1]))
	}
	return rows
}

func TestRecoverQuietSettlesAKilledSessionFromItsTranscripts(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	summary := q.recover(t)
	if summary.Sessions != 1 || summary.Calls != 2 || summary.Requests != 2 {
		t.Fatalf("RecoverQuiet = %+v, want 1 session, 2 calls, 2 requests", summary)
	}

	main := row(t, q.store, "calls", "tool_use_id = 'toolu_main'")
	if main["bytes_delivered"] != int64(len(failedResult)) || main["failed"] != int64(1) ||
		main["error"] != "Exit code 1" || main["request_id"] != "msg_main" || main["ts"] != q.callTS {
		t.Errorf("main call = bytes_delivered %v failed %v error %v request_id %v ts %v, want %d, 1, %q, msg_main, %d",
			main["bytes_delivered"], main["failed"], main["error"], main["request_id"], main["ts"],
			len(failedResult), "Exit code 1", q.callTS)
	}
	if main["bytes_real"] != int64(len(failedResult)) {
		t.Errorf("main call bytes_real = %v, want %d: a failure's text is its whole output, as PostToolUseFailure stores it", main["bytes_real"], len(failedResult))
	}
	sub := row(t, q.store, "calls", "tool_use_id = 'toolu_sub'")
	if sub["bytes_delivered"] != int64(len(secretResult)) || sub["failed"] != int64(0) || sub["error"] != nil ||
		sub["request_id"] != "msg_sub" {
		t.Errorf("sub-agent call = bytes_delivered %v failed %v error %v request_id %v, want %d, 0, NULL, msg_sub",
			sub["bytes_delivered"], sub["failed"], sub["error"], sub["request_id"], len(secretResult))
	}
	if sub["bytes_real"] != nil {
		t.Errorf("sub-agent call bytes_real = %v, want NULL: a success's delivered text may be cut, never its real size", sub["bytes_real"])
	}

	request := row(t, q.store, "requests", "request_id = 'msg_main'")
	want := map[string]any{
		"output_tokens": int64(77), "input_tokens": int64(10), "cache_read_tokens": int64(13689),
		"cache_creation_tokens": int64(10789), "context_tokens": int64(24488), "source": SourceTranscript,
		"pending": int64(0), "calls": int64(1), "session_id": "sess-1", "prompt_id": "prompt-1", "agent_id": nil,
		"seat_dir": "/tmp/demo-seat", "config_dir": "/tmp/demo-config",
		"model": "claude-haiku-4-5-20251001",
	}
	for column, value := range want {
		if request[column] != value {
			t.Errorf("request msg_main %s = %v, want %v", column, request[column], value)
		}
	}
	subRequest := row(t, q.store, "requests", "request_id = 'msg_sub'")
	if subRequest["agent_id"] != "a1" || subRequest["output_tokens"] != int64(31) || subRequest["source"] != SourceTranscript ||
		subRequest["calls"] != int64(1) {
		t.Errorf("request msg_sub = agent %v output %v source %v calls %v, want a1, 31, %s, 1",
			subRequest["agent_id"], subRequest["output_tokens"], subRequest["source"], subRequest["calls"], SourceTranscript)
	}

	// A second pass finds nothing to settle and changes nothing.
	before := q.snapshot(t)
	again := q.recover(t)
	if again.Calls != 0 || again.Requests != 0 || again.Sessions != 0 {
		t.Errorf("second RecoverQuiet = %+v, want 0 sessions, 0 calls, 0 requests", again)
	}
	if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Errorf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
	}
}

func TestRecoverQuietLeavesALiveSessionAlone(t *testing.T) {
	for name, fixture := range map[string]struct{ lastHook, mtimeAge time.Duration }{
		"transcript written just now":   {lastHook: 2 * time.Hour, mtimeAge: 0},
		"transcript written 59 min ago": {lastHook: 2 * time.Hour, mtimeAge: 59 * time.Minute},
		"hook ran 10 min ago":           {lastHook: 10 * time.Minute, mtimeAge: 2 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			q := newQuietStore(t, fixture.lastHook, fixture.mtimeAge)
			before := q.snapshot(t)
			if summary := q.recover(t); summary.Sessions != 0 || summary.Calls != 0 || summary.Requests != 0 {
				t.Errorf("RecoverQuiet = %+v, want nothing written for a live session", summary)
			}
			if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
				t.Errorf("RecoverQuiet changed a live session:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

func TestRecoverQuietLeavesALiveSubagentSessionAlone(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	if err := os.Chtimes(q.sub, q.now, q.now); err != nil {
		t.Fatal(err)
	}
	before := q.snapshot(t)
	if summary := q.recover(t); summary.Sessions != 0 || summary.Calls != 0 || summary.Requests != 0 {
		t.Errorf("RecoverQuiet = %+v, want nothing written while a sub-agent transcript is live", summary)
	}
	if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Errorf("RecoverQuiet changed a session with a live sub-agent:\nbefore %v\nafter  %v", before, after)
	}
}

func TestRecoverQuietNeverOverwritesAStoredValue(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	ctx := context.Background()
	if err := q.store.UpsertRequest(ctx, Request{
		RequestID: "msg_main", SessionID: Ptr("sess-1"), TS: Ptr(q.callTS + 1000), OutputTokens: Ptr(int64(999)),
		Pending: Ptr(false), Source: Ptr(SourceHook),
	}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := q.store.UpsertCall(ctx, Call{ToolUseID: q.toolMain, Error: Ptr("Exit code 9"), Failed: Ptr(false)}, Overwrite); err != nil {
		t.Fatal(err)
	}
	q.recover(t)

	request := row(t, q.store, "requests", "request_id = 'msg_main'")
	if request["output_tokens"] != int64(999) || request["source"] != SourceHook || request["ts"] != q.callTS+1000 {
		t.Errorf("stored request = output %v source %v ts %v, want 999, hook, %d untouched",
			request["output_tokens"], request["source"], request["ts"], q.callTS+1000)
	}
	if request["input_tokens"] != int64(10) {
		t.Errorf("stored request input_tokens = %v, want 10: an empty column still fills", request["input_tokens"])
	}
	call := row(t, q.store, "calls", "tool_use_id = 'toolu_main'")
	if call["error"] != "Exit code 9" || call["failed"] != int64(0) {
		t.Errorf("stored call = error %v failed %v, want Exit code 9 and 0 untouched", call["error"], call["failed"])
	}
	if call["bytes_delivered"] != int64(len(failedResult)) || call["request_id"] != "msg_main" {
		t.Errorf("stored call = bytes_delivered %v request_id %v, want %d and msg_main filled",
			call["bytes_delivered"], call["request_id"], len(failedResult))
	}
}

func TestRecoverQuietStoresNoTranscriptText(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	if summary := q.recover(t); summary.Calls != 2 {
		t.Fatalf("RecoverQuiet = %+v, want 2 calls settled before the scan means anything", summary)
	}
	tables, err := q.store.DB().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range names {
		rows, err := q.store.DB().Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				text := fmt.Sprint(value)
				if b, ok := value.([]byte); ok {
					text = string(b)
				}
				for _, secret := range []string{secretResult, secretMessage, secretPrompt} {
					if strings.Contains(text, secret) {
						t.Errorf("%s.%s holds transcript text %q", table, columns[i], secret)
					}
				}
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoverQuietNamesAnUnreadableTranscriptAndGoesOn(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	q.writeTranscript(t, q.sub, `{"type":"user","message":{"content":[{"type":"tool_result" `+q.toolSub)
	mtime := q.now.Add(-2 * time.Hour)
	if err := os.Chtimes(q.sub, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	summary, err := q.store.RecoverQuiet(context.Background(), q.now, QuietAfter)
	if err != nil {
		t.Fatalf("RecoverQuiet: %v", err)
	}
	if len(summary.Skipped) == 0 || !strings.Contains(summary.Skipped[0].Error(), "session sess-1") ||
		!strings.Contains(summary.Skipped[0].Error(), q.sub) {
		t.Fatalf("RecoverQuiet skipped %v, want an error naming session sess-1 and %s", summary.Skipped, q.sub)
	}
	if main := row(t, q.store, "calls", "tool_use_id = 'toolu_main'"); main["bytes_delivered"] != int64(len(failedResult)) {
		t.Errorf("main call bytes_delivered = %v, want %d: the readable transcript still settles", main["bytes_delivered"], len(failedResult))
	}
}

func TestRecoverQuietSettlesAPendingRequestFromItsTranscript(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	ctx := context.Background()
	key := ProvisionalKey(q.toolMain)
	// What PostToolBatch stored when the message was not on disk yet.
	if err := q.store.Batch(ctx, func(tx *Tx) error {
		if err := tx.UpsertRequest(ctx, Request{RequestID: key, SessionID: Ptr("sess-1"), TS: Ptr(q.callTS), Pending: Ptr(true),
			Calls: Ptr(int64(1)), Source: Ptr(SourceHook)}, Overwrite); err != nil {
			return err
		}
		return tx.UpsertCall(ctx, Call{ToolUseID: q.toolMain, RequestID: Ptr(key), BytesDelivered: Ptr(int64(5))}, Overwrite)
	}); err != nil {
		t.Fatal(err)
	}
	summary := q.recover(t)
	if summary.Requests != 2 {
		t.Errorf("RecoverQuiet = %+v, want 2 requests: the pending one resolved and the sub-agent's written", summary)
	}
	if gone := row(t, q.store, "requests", "request_id = ?", key); gone != nil {
		t.Errorf("provisional request %s still stored: %v", key, gone)
	}
	request := row(t, q.store, "requests", "request_id = 'msg_main'")
	if request["pending"] != int64(0) || request["output_tokens"] == nil || request["calls"] != int64(1) {
		t.Errorf("resolved request = pending %v output %v calls %v, want 0, a value, 1",
			request["pending"], request["output_tokens"], request["calls"])
	}
	if call := row(t, q.store, "calls", "tool_use_id = 'toolu_main'"); call["request_id"] != "msg_main" {
		t.Errorf("main call request_id = %v, want msg_main", call["request_id"])
	}
}

func TestRecoverQuietAnOpenCallStaysACandidateAndWritesNothingTwice(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	// A call whose tool never returned: no result on disk, so the session is a
	// candidate at every pass.
	if err := q.store.UpsertCall(context.Background(), Call{
		ToolUseID: "toolu_open", SessionID: Ptr("sess-1"), TS: Ptr(q.callTS), Tool: Ptr("Bash"), Source: Ptr(SourceHook),
	}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if first := q.recover(t); first.Calls != 2 || first.Requests != 2 {
		t.Fatalf("first RecoverQuiet = %+v, want 2 calls and 2 requests", first)
	}
	before := q.snapshot(t)
	if again := q.recover(t); again.Sessions != 0 || again.Calls != 0 || again.Requests != 0 {
		t.Errorf("second RecoverQuiet = %+v, want 0 sessions, 0 calls, 0 requests", again)
	}
	if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
		t.Errorf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
	}
	if open := row(t, q.store, "calls", "tool_use_id = 'toolu_open'"); open["bytes_delivered"] != nil || open["failed"] != nil {
		t.Errorf("open call = bytes_delivered %v failed %v, want both NULL: no result, no guess", open["bytes_delivered"], open["failed"])
	}
}

// TestRecoverQuietRebuildsALostTurnEnd: a session whose turn-end hook was lost
// (a headless exit cancels the async Stop hook) and that never reached a
// SessionEnd that could rebuild it gets its turn end from the transcript once
// quiet: a Stop, events and turns rows, when the answer is on disk; a
// StopFailure, never a Stop, when the turn ended on an API error; nothing for a
// turn whose answer is not on disk, or a session still written to. Once: a
// second pass writes nothing.
func TestRecoverQuietRebuildsALostTurnEnd(t *testing.T) {
	ctx := context.Background()
	start := time.Now().Add(-2 * time.Hour)
	answered := strings.Replace(textLine("msg_end", secretMessage, 9, start.Add(3*time.Second)),
		`"role":"assistant",`, `"role":"assistant","stop_reason":"end_turn",`, 1)
	refused := `{"type":"assistant","timestamp":"` + stamp(start.Add(2*time.Second)) + `","isApiErrorMessage":true,"error":"rate_limit",` +
		`"message":{"id":"msg-err","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"` + secretMessage + `"}]}}`
	seed := func(t *testing.T, written time.Time, lines ...string) *Store {
		t.Helper()
		store := openTestStore(t)
		path := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-1.jsonl")
		quietStore{}.writeTranscript(t, path, lines...)
		if err := os.Chtimes(path, written, written); err != nil {
			t.Fatal(err)
		}
		if err := store.Batch(ctx, func(tx *Tx) error {
			if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: start.UnixMilli(), TranscriptPath: Ptr(path),
				SeatDir: Ptr("/tmp/demo-seat")}); err != nil {
				return err
			}
			_, err := tx.InsertEvent(ctx, promptEvent("u1", "sess-1", start.UnixMilli()))
			return err
		}); err != nil {
			t.Fatalf("seed the session: %v", err)
		}
		return store
	}
	pass := func(t *testing.T, store *Store) RecoverSummary {
		t.Helper()
		summary, err := store.RecoverQuiet(ctx, time.Now(), QuietAfter)
		if err != nil || len(summary.Skipped) != 0 {
			t.Fatalf("RecoverQuiet = %+v, %v", summary, err)
		}
		return summary
	}
	ends := func(store *Store) string {
		return keys(t, store.DB(), `SELECT e.event || '/' || COALESCE(e.prompt_id, '-') || '@' || (e.ts - `+fmt.Sprint(start.UnixMilli())+`) || '/' ||
			COALESCE(e.error_type, '-') || '/' || e.detail || '/' || (SELECT count(*) FROM turns t WHERE t.event_id = e.event_id)
			FROM events e WHERE e.event IN ('Stop', 'StopFailure') ORDER BY e.event`)
	}
	t.Run("answer on disk", func(t *testing.T) {
		store := seed(t, start.Add(time.Minute), promptLine("prompt-1", secretPrompt, start), answered)
		if got := pass(t, store); got.Sessions != 1 {
			t.Errorf("first RecoverQuiet = %+v, want 1 session", got)
		}
		want := "Stop/p-u1@3000/-/" + RecoveredDetail + "/1"
		if got := ends(store); got != want {
			t.Errorf("turn ends = %q, want %q", got, want)
		}
		if got := pass(t, store); got.Sessions != 0 {
			t.Errorf("second RecoverQuiet = %+v, want 0 sessions", got)
		}
		if got := ends(store); got != want {
			t.Errorf("turn ends after a second pass = %q, want %q", got, want)
		}
		var text int
		if err := store.DB().QueryRow(`SELECT count(*) FROM events WHERE detail LIKE '%' || ? || '%'`, secretMessage).Scan(&text); err != nil || text != 0 {
			t.Errorf("events holding the answer's text = %d, %v; want 0", text, err)
		}
	})
	t.Run("API error on disk", func(t *testing.T) {
		store := seed(t, start.Add(time.Minute), promptLine("prompt-1", secretPrompt, start), refused)
		pass(t, store)
		if got, want := ends(store), "StopFailure/p-u1@2000/rate_limit/"+RecoveredDetail+"/0"; got != want {
			t.Errorf("turn ends = %q, want %q", got, want)
		}
	})
	t.Run("answer not on disk", func(t *testing.T) {
		store := seed(t, start.Add(time.Minute), promptLine("prompt-1", secretPrompt, start),
			toolUseLine("msg_tool", "toolu_x", 5, start.Add(time.Second)))
		pass(t, store)
		if got := ends(store); got != "" {
			t.Errorf("turn ends = %q, want none", got)
		}
	})
	t.Run("transcript still written to", func(t *testing.T) {
		store := seed(t, time.Now(), promptLine("prompt-1", secretPrompt, start), answered)
		pass(t, store)
		if got := ends(store); got != "" {
			t.Errorf("turn ends = %q, want none: the session is not quiet", got)
		}
	})
}

// TestRecoverQuietWritesAWokenAgentsLostReply: an agent woken after its turn
// ended answered with one text reply whose line its SubagentStop read too
// early, and that stop was the agent's last: no later hook reads the reply.
// Every other row of the session is settled, so only the agent turn with no
// request between the previous stop and its own makes the session a
// candidate; recovery writes the reply, and a second pass finds nothing.
func TestRecoverQuietWritesAWokenAgentsLostReply(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	start := now.Add(-2 * time.Hour)
	dir := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj")
	main := filepath.Join(dir, "sess-1.jsonl")
	sub := filepath.Join(dir, "sess-1", "subagents", "agent-a1.jsonl")
	q := quietStore{store: store, now: now}
	q.writeTranscript(t, main,
		promptLine("prompt-1", secretPrompt, start),
		textLine("msg_main", secretMessage, 12, start.Add(time.Second)),
	)
	q.writeTranscript(t, sub,
		promptLine("prompt-a1", secretPrompt, start.Add(2*time.Second)),
		textLine("msg_first", secretMessage, 20, start.Add(3*time.Second)),
		promptLine("prompt-wake", secretPrompt, start.Add(10*time.Second)),
		textLine("msg_lost", secretMessage, 60, start.Add(11*time.Second)),
	)
	for _, file := range []string{main, sub} {
		if err := os.Chtimes(file, start, start); err != nil {
			t.Fatalf("set mtime of %s: %v", file, err)
		}
	}
	firstStop, lastStop := start.Add(4*time.Second).UnixMilli(), start.Add(11*time.Second+43*time.Millisecond).UnixMilli()
	err := store.Batch(ctx, func(tx *Tx) error {
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: start.Add(-time.Minute).UnixMilli(), TranscriptPath: Ptr(main)}); err != nil {
			return err
		}
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: lastStop}); err != nil {
			return err
		}
		// Every request the hooks read, as their sweeps wrote it: all but the
		// woken turn's reply.
		for _, file := range []string{main, sub} {
			requests, _, err := ReadRequests(file)
			if err != nil {
				return err
			}
			for _, read := range requests {
				if read.MessageID == "msg_lost" {
					continue
				}
				request := Request{RequestID: read.MessageID, SessionID: Ptr("sess-1"), PromptID: presentString(read.PromptID),
					Pending: Ptr(false), Source: Ptr(SourceHook)}
				if file == sub {
					request.AgentID = Ptr("a1")
				}
				ApplyUsage(&request, read.RequestUsage)
				if err := tx.SettleRequest(ctx, request, read.ToolUseIDs, 0); err != nil {
					return err
				}
			}
		}
		_, err := tx.tx.ExecContext(ctx, `INSERT INTO agent_turns (agent_id, seq, session_id, agent_type, started, stopped)
			VALUES ('a1', 1, 'sess-1', 'Explore', ?1, ?2), ('a1', 2, 'sess-1', 'Explore', ?3, ?4)`,
			start.Add(2*time.Second).UnixMilli(), firstStop, start.Add(10*time.Second).UnixMilli(), lastStop)
		return err
	})
	if err != nil {
		t.Fatalf("seed the session: %v", err)
	}
	if summary := q.recover(t); summary.Sessions != 1 || summary.Requests != 1 {
		t.Fatalf("RecoverQuiet = %+v, want 1 session and 1 request: the woken turn's reply", summary)
	}
	lost := row(t, store, "requests", "request_id = 'msg_lost'")
	if lost["agent_id"] != "a1" || lost["output_tokens"] != int64(60) || lost["source"] != SourceTranscript || lost["calls"] != int64(0) {
		t.Errorf("recovered reply = agent %v output %v source %v calls %v, want a1, 60, %s, 0",
			lost["agent_id"], lost["output_tokens"], lost["source"], lost["calls"], SourceTranscript)
	}
	if again := q.recover(t); again.Sessions != 0 || again.Requests != 0 {
		t.Errorf("second RecoverQuiet = %+v, want nothing: every agent turn has its request", again)
	}
}

// TestRecoverQuietMarksASessionWithNoSessionEnd: Claude Code may end a session
// without running its SessionEnd hooks. Once quiet, a session whose latest run
// (from its latest SessionStart on) has no SessionEnd gets end_reason
// EndReasonNever, never a silent NULL, beside the turn end and calls recovery
// settles; once: a second pass writes nothing. A SessionEnd of the latest run
// keeps its reason; one of an earlier run only does not. A lost SessionEnd of
// the session (a binary or terminated fault naming it and the session) gets
// end_reason EndReasonLost, once, never over a stored reason; one naming no
// session within a minute of its last hook leaves the end unknown (NULL),
// never "never" nor "lost"; so does a session still running. A session whose
// transcript is gone is marked too.
func TestRecoverQuietMarksASessionWithNoSessionEnd(t *testing.T) {
	ctx := context.Background()
	start := time.Now().Add(-2 * time.Hour)
	answered := strings.Replace(textLine("msg_end", secretMessage, 9, start.Add(3*time.Second)),
		`"role":"assistant",`, `"role":"assistant","stop_reason":"end_turn",`, 1)
	at := func(d time.Duration) int64 { return start.Add(d).UnixMilli() }
	event := func(id, name string, d time.Duration) Event {
		e := Event{EventID: id, Event: name, SessionID: Ptr("sess-1"), TS: at(d)}
		if name == EventSessionEnd {
			e.Reason = Ptr("other")
		}
		if name == EventSessionStart {
			e.Source = Ptr("startup")
		}
		return e
	}
	seed := func(t *testing.T, written time.Time, transcript bool, events []Event, faults ...Fault) *Store {
		t.Helper()
		store := openTestStore(t)
		path := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-1.jsonl")
		if transcript {
			quietStore{}.writeTranscript(t, path, promptLine("prompt-1", secretPrompt, start.Add(time.Second)), answered)
			if err := os.Chtimes(path, written, written); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Batch(ctx, func(tx *Tx) error {
			if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: at(0), TranscriptPath: Ptr(path)}); err != nil {
				return err
			}
			if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: at(10 * time.Second)}); err != nil {
				return err
			}
			for _, e := range events {
				if _, err := tx.InsertEvent(ctx, e); err != nil {
					return err
				}
			}
			return tx.RefreshSession(ctx, "sess-1")
		}); err != nil {
			t.Fatalf("seed the session: %v", err)
		}
		for _, f := range faults {
			if err := store.AddFault(ctx, f); err != nil {
				t.Fatalf("AddFault: %v", err)
			}
		}
		return store
	}
	pass := func(t *testing.T, store *Store) RecoverSummary {
		t.Helper()
		summary, err := store.RecoverQuiet(ctx, time.Now(), QuietAfter)
		if err != nil || len(summary.Skipped) != 0 {
			t.Fatalf("RecoverQuiet = %+v, %v", summary, err)
		}
		return summary
	}
	endReason := func(t *testing.T, store *Store) string {
		t.Helper()
		var reason sql.NullString
		if err := store.DB().QueryRow(`SELECT end_reason FROM sessions WHERE session_id = 'sess-1'`).Scan(&reason); err != nil {
			t.Fatalf("read end_reason: %v", err)
		}
		if !reason.Valid {
			return "<NULL>"
		}
		return reason.String
	}
	quiet := start.Add(time.Minute)
	prompted := []Event{event("s1", EventSessionStart, 0), promptEvent("u1", "sess-1", at(time.Second))}
	t.Run("no SessionEnd", func(t *testing.T) {
		store := seed(t, quiet, true, prompted)
		if got := pass(t, store); got.Sessions != 1 || got.TurnEnds != 1 {
			t.Errorf("first RecoverQuiet = %+v, want 1 session and its turn end", got)
		}
		if got := endReason(t, store); got != EndReasonNever {
			t.Errorf("end_reason = %q, want %q", got, EndReasonNever)
		}
		if got := pass(t, store); got.Sessions != 0 {
			t.Errorf("second RecoverQuiet = %+v, want 0 sessions", got)
		}
		if got := endReason(t, store); got != EndReasonNever {
			t.Errorf("end_reason after a second pass = %q, want %q", got, EndReasonNever)
		}
	})
	t.Run("SessionEnd of the latest run", func(t *testing.T) {
		store := seed(t, quiet, true, append(slices.Clone(prompted), event("e1", EventSessionEnd, 5*time.Second)))
		pass(t, store)
		if got := endReason(t, store); got != "other" {
			t.Errorf("end_reason = %q, want the hook's other", got)
		}
	})
	t.Run("SessionEnd of an earlier run only", func(t *testing.T) {
		store := seed(t, quiet, true, append(slices.Clone(prompted),
			event("e1", EventSessionEnd, 2*time.Second), event("s2", EventSessionStart, 4*time.Second)))
		pass(t, store)
		if got := endReason(t, store); got != EndReasonNever {
			t.Errorf("end_reason = %q, want %q: the resumed run never ended", got, EndReasonNever)
		}
	})
	t.Run("lost SessionEnd in its SessionStart's second", func(t *testing.T) {
		// A missed.log line is stamped in whole seconds, so the fault of a
		// SessionEnd lost within a second of its SessionStart reads earlier
		// than that start; it is still this run's.
		late := event("s1", EventSessionStart, 0)
		late.TS = at(0)/1000*1000 + 999
		store := seed(t, quiet, true, []Event{late, promptEvent("u1", "sess-1", at(time.Second))},
			Fault{TS: at(0) / 1000 * 1000, SessionID: "sess-1",
				Stage: StageTerminated, Error: EventSessionEnd + ": " + TerminatedByStoreBusy})
		if got := pass(t, store); got.LostEnds != 1 || got.NoEnds != 0 {
			t.Errorf("RecoverQuiet = %+v, want its lost end", got)
		}
		if got := endReason(t, store); got != EndReasonLost {
			t.Errorf("end_reason = %q, want %q", got, EndReasonLost)
		}
	})
	t.Run("lost SessionEnd of the session", func(t *testing.T) {
		store := seed(t, quiet, true, prompted, Fault{TS: at(11 * time.Second), SessionID: "sess-1",
			Stage: StageTerminated, Error: EventSessionEnd + ": " + TerminatedReason + "SIGTERM"})
		if got := pass(t, store); got.Sessions != 1 || got.LostEnds != 1 || got.NoEnds != 0 {
			t.Errorf("first RecoverQuiet = %+v, want 1 session and its lost end", got)
		}
		if got := endReason(t, store); got != EndReasonLost {
			t.Errorf("end_reason = %q, want %q: its SessionEnd ran and was lost", got, EndReasonLost)
		}
		if got := pass(t, store); got.Sessions != 0 {
			t.Errorf("second RecoverQuiet = %+v, want 0 sessions", got)
		}
	})
	t.Run("lost SessionEnd of the session, a reason stored", func(t *testing.T) {
		store := seed(t, quiet, true, append(slices.Clone(prompted),
			event("e1", EventSessionEnd, 2*time.Second), event("s2", EventSessionStart, 4*time.Second)),
			Fault{TS: at(11 * time.Second), SessionID: "sess-1",
				Stage: StageTerminated, Error: EventSessionEnd + ": " + TerminatedReason + "SIGTERM"})
		pass(t, store)
		if got := endReason(t, store); got != "other" {
			t.Errorf("end_reason = %q, want the stored other: a lost end never overwrites a reason", got)
		}
	})
	t.Run("lost SessionEnd of the session, transcript gone", func(t *testing.T) {
		store := seed(t, quiet, false, prompted, Fault{TS: at(11 * time.Second), SessionID: "sess-1",
			Stage: StageBinary, Error: EventSessionEnd + ": binary unavailable"})
		if got := pass(t, store); got.Sessions != 1 || got.LostEnds != 1 {
			t.Errorf("RecoverQuiet = %+v, want 1 session and its lost end", got)
		}
		if got := endReason(t, store); got != EndReasonLost {
			t.Errorf("end_reason = %q, want %q", got, EndReasonLost)
		}
	})
	t.Run("lost SessionEnd naming no session", func(t *testing.T) {
		store := seed(t, quiet, true, prompted, Fault{TS: at(30 * time.Second),
			Stage: StageBinary, Error: EventSessionEnd + ": binary unavailable"})
		pass(t, store)
		if got := endReason(t, store); got != "<NULL>" {
			t.Errorf("end_reason = %q, want NULL: a lost SessionEnd naming no session a minute after its last hook may be its own, "+
				"never known to be", got)
		}
	})
	t.Run("lost SessionEnd of another time", func(t *testing.T) {
		store := seed(t, quiet, true, prompted, Fault{TS: at(30 * time.Minute),
			Stage: StageBinary, Error: EventSessionEnd + ": binary unavailable"})
		pass(t, store)
		if got := endReason(t, store); got != EndReasonNever {
			t.Errorf("end_reason = %q, want %q: the lost SessionEnd is past its reach", got, EndReasonNever)
		}
	})
	t.Run("still running", func(t *testing.T) {
		store := seed(t, time.Now(), true, prompted)
		pass(t, store)
		if got := endReason(t, store); got != "<NULL>" {
			t.Errorf("end_reason = %q, want NULL: the session is not quiet", got)
		}
	})
	t.Run("transcript gone", func(t *testing.T) {
		store := seed(t, quiet, false, prompted)
		if got := pass(t, store); got.Sessions != 1 {
			t.Errorf("RecoverQuiet = %+v, want 1 session", got)
		}
		if got := endReason(t, store); got != EndReasonNever {
			t.Errorf("end_reason = %q, want %q", got, EndReasonNever)
		}
	})
}

// unfilledAgentText and unfilledStopText are the fault texts the markers carry,
// as Go formats them: the SQL of agentTurnMarker and stopReplyMarker must produce
// the same bytes (docs/testing.md § Bug classes, Privacy: ids and counts only).
func unfilledAgentText(agentID string, stopped int64) string {
	return fmt.Sprintf("agent %s turn stopped %d: %s", agentID, stopped, UnfilledAgentTurn)
}

func unfilledStopText(promptID, eventID string) string {
	return fmt.Sprintf("prompt %s Stop %s: %s", promptID, eventID, UnfilledStopReply)
}

// newUnfilledStore is a quiet session whose only open row is what seed adds: its
// SessionEnd is stored (so endMissing is false) and no call, pending request or
// prompt is left, so a candidate is one by seed's rows alone. Its first hook is
// firstTS, its transcripts were last written mtimeAge before now.
func newUnfilledStore(t *testing.T, mtimeAge time.Duration, firstTS time.Time, mainLines, subLines []string, seed func(tx *Tx) error) quietStore {
	t.Helper()
	ctx := context.Background()
	q := quietStore{store: openTestStore(t), now: time.Now(), firstTS: firstTS.UnixMilli()}
	dir := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj")
	q.main = filepath.Join(dir, "sess-1.jsonl")
	q.sub = filepath.Join(dir, "sess-1", "subagents", "agent-a1.jsonl")
	q.writeTranscript(t, q.main, mainLines...)
	files := []string{q.main}
	if subLines != nil {
		q.writeTranscript(t, q.sub, subLines...)
		files = append(files, q.sub)
	}
	mtime := q.now.Add(-mtimeAge)
	for _, file := range files {
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatalf("set mtime of %s: %v", file, err)
		}
	}
	err := q.store.Batch(ctx, func(tx *Tx) error {
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: q.firstTS, TranscriptPath: Ptr(q.main)}); err != nil {
			return err
		}
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: q.now.Add(-2 * time.Hour).UnixMilli()}); err != nil {
			return err
		}
		if _, err := tx.InsertEvent(ctx, Event{EventID: "end", Event: EventSessionEnd, SessionID: Ptr("sess-1"),
			TS: q.now.Add(-2 * time.Hour).UnixMilli(), Reason: Ptr("other")}); err != nil {
			return err
		}
		return seed(tx)
	})
	if err != nil {
		t.Fatalf("seed the unfilled session: %v", err)
	}
	return q
}

func (q quietStore) candidates(t *testing.T) []quietSession {
	t.Helper()
	got, err := q.store.quietCandidates(context.Background(), q.now.Add(-QuietAfter).UnixMilli())
	if err != nil {
		t.Fatalf("quietCandidates: %v", err)
	}
	return got
}

func stopTurn(tx *Tx, eventID, promptID string, at time.Time) error {
	_, err := tx.InsertTurn(context.Background(), Turn{EventID: eventID, Event: EventStop, SessionID: Ptr("sess-1"),
		PromptID: Ptr(promptID), TS: at.UnixMilli(), LastAssistantMessageBytes: Ptr(int64(42))})
	return err
}

// endTurnLine is a text reply that ended its turn.
func endTurnLine(messageID string, output int, at time.Time) string {
	return strings.Replace(textLine(messageID, secretMessage, output, at), `"role":"assistant",`, `"role":"assistant","stop_reason":"end_turn",`, 1)
}

// TestRecoverQuietBackfillsAStopsReplyNeverStored: a main-chat Stop is stored
// (its hook ran) but the turn's final text reply never was, so the Stop's own
// turn end made turnEndMissing false and nothing made the session a candidate.
// The Stop with a reply size and no final request of its prompt is one; recovery
// writes the reply from the transcript, carrying the prompt, and once it is
// there the session is no candidate.
func TestRecoverQuietBackfillsAStopsReplyNeverStored(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	q := newUnfilledStore(t, 2*time.Hour, start.Add(-time.Minute),
		[]string{promptLine("prompt-P", secretPrompt, start), endTurnLine("msg_reply", 9, start.Add(2*time.Second))}, nil,
		func(tx *Tx) error { return stopTurn(tx, "stop-1", "prompt-P", start.Add(3*time.Second)) })
	if got := q.candidates(t); len(got) != 1 {
		t.Fatalf("quietCandidates = %v, want the session: its Stop has no final reply request", got)
	}
	if summary := q.recover(t); summary.Sessions != 1 || summary.Requests != 1 || summary.Unfillable != 0 {
		t.Fatalf("RecoverQuiet = %+v, want 1 session and the 1 reply request, nothing unfillable", summary)
	}
	reply := row(t, q.store, "requests", "request_id = 'msg_reply'")
	if reply == nil || reply["prompt_id"] != "prompt-P" || reply["stop_reason"] != "end_turn" || reply["agent_id"] != nil ||
		reply["source"] != SourceTranscript {
		t.Errorf("recovered reply = %v, want prompt prompt-P, end_turn, no agent, source %s", reply, SourceTranscript)
	}
	if got := q.candidates(t); len(got) != 0 {
		t.Errorf("quietCandidates after recovery = %v, want none", got)
	}
	if again := q.recover(t); again.Sessions != 0 || again.Requests != 0 || again.Unfillable != 0 {
		t.Errorf("second RecoverQuiet = %+v, want nothing", again)
	}
	if n := count(t, q.store, "faults"); n != 0 {
		t.Errorf("faults = %d, want 0: the reply was filled", n)
	}
}

// TestRecoverQuietMarksAnUnfillableAgentTurnOnce: an agent turn with a stop and
// no request in its span whose transcript holds only a reply older than the
// session's first hook (requestFills never writes it) is read in full and still
// cannot be filled: one transcript fault says so, and the session stops being a
// candidate, so a later report never reads it again.
func TestRecoverQuietMarksAnUnfillableAgentTurnOnce(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	stopped := start.Add(time.Second).UnixMilli()
	q := newUnfilledStore(t, 2*time.Hour, start,
		[]string{promptLine("prompt-1", secretPrompt, start)},
		[]string{promptLine("prompt-a1", secretPrompt, start.Add(-3*time.Minute)), textLine("msg_old", secretMessage, 20, start.Add(-2*time.Minute))},
		func(tx *Tx) error {
			_, err := tx.tx.ExecContext(context.Background(), `INSERT INTO agent_turns (agent_id, seq, session_id, agent_type, started, stopped)
				VALUES ('a1', 1, 'sess-1', 'Explore', ?1, ?2)`, start.Add(-3*time.Minute).UnixMilli(), stopped)
			return err
		})
	if got := q.candidates(t); len(got) != 1 {
		t.Fatalf("quietCandidates = %v, want the session: its agent turn has no request", got)
	}
	if summary := q.recover(t); summary.Sessions != 1 || summary.Requests != 0 || summary.Unfillable != 1 {
		t.Fatalf("RecoverQuiet = %+v, want 1 session, 0 requests, 1 unfillable", summary)
	}
	fault := row(t, q.store, "faults", "stage = 'transcript'")
	if fault == nil || fault["session_id"] != "sess-1" || fault["ts"] != stopped || fault["error"] != unfilledAgentText("a1", stopped) {
		t.Errorf("fault = %v, want session sess-1, ts %d, error %q", fault, stopped, unfilledAgentText("a1", stopped))
	}
	if n := count(t, q.store, "requests"); n != 0 {
		t.Errorf("requests = %d, want 0: the only reply predates the session", n)
	}
	if got := q.candidates(t); len(got) != 0 {
		t.Errorf("quietCandidates after the marker = %v, want none", got)
	}
	if again := q.recover(t); again.Sessions != 0 || again.Unfillable != 0 {
		t.Errorf("second RecoverQuiet = %+v, want nothing", again)
	}
	if n := count(t, q.store, "faults"); n != 1 {
		t.Errorf("faults = %d, want 1: the marker is recorded once", n)
	}
}

// TestRecoverQuietMarksAnUnfillableStopOnce: the same for a Stop whose prompt
// has no reply request and whose main transcript holds none at or after the
// session's first hook.
func TestRecoverQuietMarksAnUnfillableStopOnce(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	stopAt := start.Add(time.Second)
	q := newUnfilledStore(t, 2*time.Hour, start,
		[]string{promptLine("prompt-P", secretPrompt, start.Add(-3*time.Minute)), endTurnLine("msg_old", 9, start.Add(-2*time.Minute))}, nil,
		func(tx *Tx) error { return stopTurn(tx, "stop-1", "prompt-P", stopAt) })
	if got := q.candidates(t); len(got) != 1 {
		t.Fatalf("quietCandidates = %v, want the session: its Stop has no final reply request", got)
	}
	if summary := q.recover(t); summary.Sessions != 1 || summary.Requests != 0 || summary.Unfillable != 1 {
		t.Fatalf("RecoverQuiet = %+v, want 1 session, 0 requests, 1 unfillable", summary)
	}
	fault := row(t, q.store, "faults", "stage = 'transcript'")
	if want := unfilledStopText("prompt-P", "stop-1"); fault == nil || fault["session_id"] != "sess-1" ||
		fault["ts"] != stopAt.UnixMilli() || fault["error"] != want {
		t.Errorf("fault = %v, want session sess-1, ts %d, error %q", fault, stopAt.UnixMilli(), want)
	}
	if n := count(t, q.store, "requests"); n != 0 {
		t.Errorf("requests = %d, want 0: the only reply predates the session", n)
	}
	if got := q.candidates(t); len(got) != 0 {
		t.Errorf("quietCandidates after the marker = %v, want none", got)
	}
	if again := q.recover(t); again.Sessions != 0 || again.Unfillable != 0 {
		t.Errorf("second RecoverQuiet = %+v, want nothing", again)
	}
	if n := count(t, q.store, "faults"); n != 1 {
		t.Errorf("faults = %d, want 1: the marker is recorded once", n)
	}
}

// TestRecoverQuietMarksNothingForALiveOrUnreadSession: a marker says recovery
// read every transcript in full. A session still written to is not read, and one
// whose sub-agent transcript cannot be read is not read in full: neither is
// marked, and the session stays a candidate.
func TestRecoverQuietMarksNothingForALiveOrUnreadSession(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	seedStop := func(tx *Tx) error { return stopTurn(tx, "stop-1", "prompt-P", start.Add(time.Second)) }
	old := []string{promptLine("prompt-P", secretPrompt, start.Add(-3*time.Minute)), endTurnLine("msg_old", 9, start.Add(-2*time.Minute))}
	t.Run("live", func(t *testing.T) {
		q := newUnfilledStore(t, 0, start, old, nil, seedStop)
		if summary := q.recover(t); summary.Sessions != 0 || summary.Requests != 0 || summary.Unfillable != 0 {
			t.Errorf("RecoverQuiet = %+v, want nothing for a live session", summary)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0", n)
		}
		if got := q.candidates(t); len(got) != 1 {
			t.Errorf("quietCandidates = %v, want the live session kept", got)
		}
	})
	t.Run("unreadable sub-agent transcript", func(t *testing.T) {
		q := newUnfilledStore(t, 2*time.Hour, start, old, []string{`{"type":"assistant" broken`}, seedStop)
		summary, err := q.store.RecoverQuiet(context.Background(), q.now, QuietAfter)
		if err != nil {
			t.Fatalf("RecoverQuiet: %v", err)
		}
		if len(summary.Skipped) != 1 || summary.Unfillable != 0 {
			t.Errorf("RecoverQuiet = %+v, want the one skipped transcript and no marker", summary)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0: a transcript was not read", n)
		}
	})
}
