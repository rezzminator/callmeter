package callmeter

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	assertNoTranscriptText(t, q.store)
}

func assertNoTranscriptText(t *testing.T, store *Store) {
	t.Helper()
	tables, err := store.DB().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
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
		rows, err := store.DB().Query("SELECT * FROM " + table)
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
						t.Errorf("%s.%s holds transcript text", table, columns[i])
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

// TestRecoverQuietMarksAnOpenCallOnce: a call with no result in any transcript
// of its quiet session (the tool never returned, or its untyped agent wrote no
// transcript) is read once: one transcript fault names the call and the session
// stops being a candidate, so a later report never reads it again. Any later
// hook makes it a candidate again; a live session is never marked.
func TestRecoverQuietMarksAnOpenCallOnce(t *testing.T) {
	// open is a quiet store (transcripts written mtimeAge ago) with one more call
	// whose result is nowhere on disk, made by agent (none: the main chat).
	open := func(t *testing.T, mtimeAge time.Duration, agent *string) quietStore {
		t.Helper()
		q := newQuietStore(t, 2*time.Hour, mtimeAge)
		if err := q.store.UpsertCall(context.Background(), Call{
			ToolUseID: "toolu_open", SessionID: Ptr("sess-1"), AgentID: agent, TS: Ptr(q.callTS), Tool: Ptr("Bash"), Source: Ptr(SourceHook),
		}, Overwrite); err != nil {
			t.Fatal(err)
		}
		return q
	}
	want := "call toolu_open: " + UnfilledCall
	// marked is an open store after the pass that marks the call.
	marked := func(t *testing.T, agent *string) quietStore {
		t.Helper()
		q := open(t, 2*time.Hour, agent)
		if first := q.recover(t); first.Calls != 2 || first.Requests != 2 || first.Unfillable != 1 {
			t.Fatalf("first RecoverQuiet = %+v, want 2 calls, 2 requests and 1 unfillable: the open call", first)
		}
		lastHook := row(t, q.store, "sessions", "session_id = 'sess-1'")["last_ts"]
		fault := row(t, q.store, "faults", "stage = 'transcript'")
		if fault == nil || fault["session_id"] != "sess-1" || fault["tool_use_id"] != "toolu_open" || fault["ts"] != lastHook || fault["error"] != want {
			t.Fatalf("fault = %v, want session sess-1, tool_use_id toolu_open, ts %v (the session's last hook), error %q", fault, lastHook, want)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Fatalf("quietCandidates after the marker = %v, want none", got)
		}
		before := q.snapshot(t)
		if again := q.recover(t); again.Sessions != 0 || again.Calls != 0 || again.Requests != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
		}
		if n := count(t, q.store, "faults"); n != 1 {
			t.Fatalf("faults = %d, want 1: the marker is recorded once", n)
		}
		if call := row(t, q.store, "calls", "tool_use_id = 'toolu_open'"); call["bytes_delivered"] != nil || call["failed"] != nil {
			t.Errorf("open call = bytes_delivered %v failed %v, want both NULL: no result, no guess", call["bytes_delivered"], call["failed"])
		}
		return q
	}
	t.Run("a main-chat tool that never returned", func(t *testing.T) {
		marked(t, nil)
	})
	t.Run("a call of an untyped agent with no transcript", func(t *testing.T) {
		marked(t, Ptr("a-internal"))
	})
	t.Run("a later hook reopens it", func(t *testing.T) {
		q := marked(t, nil)
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			return tx.TouchSession(context.Background(), Session{SessionID: "sess-1", TS: q.now.Add(time.Second).UnixMilli()})
		}); err != nil {
			t.Fatal(err)
		}
		got, err := q.store.quietCandidates(context.Background(), q.now.Add(2*time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("quietCandidates after a later hook = %v, want the session again", got)
		}
		// The call itself is open again, not only the session's end, which the
		// later hook also reopens.
		calls, err := q.store.unmarkedOpenCalls(context.Background(), "sess-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].id != "toolu_open" || !calls[0].sizeOpen {
			t.Errorf("unmarkedOpenCalls after a later hook = %+v, want toolu_open with no size", calls)
		}
	})
	t.Run("a live session is never marked", func(t *testing.T) {
		q := open(t, 0, nil)
		if summary := q.recover(t); summary.Sessions != 0 || summary.Unfillable != 0 {
			t.Errorf("RecoverQuiet = %+v, want nothing for a live session", summary)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0: a live session's call may still return", n)
		}
		if got := q.candidates(t); len(got) != 1 {
			t.Errorf("quietCandidates = %v, want the live session kept", got)
		}
	})
}

// TestRecoverQuietRebuildsAnUnrecordedCall: a transcript tool_use no hook
// recorded (Claude Code records only a Bash call before it runs, so a session
// killed mid-call leaves any other call with no row) gets its row from the
// transcript once the session is quiet: its session, tool, request, prompt, ts
// and cwd, its input only through SanitizeInput, SourceTranscript. Its request
// counts it, the session takes that request's model, and its result settles it
// or, with none on disk or one that holds no toolUseResult to size it by, one
// call marker names it. Once: a second pass writes
// nothing.
func TestRecoverQuietRebuildsAnUnrecordedCall(t *testing.T) {
	lost := func(t *testing.T, answered bool) quietStore {
		t.Helper()
		q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
		start := q.now.Add(-2 * time.Hour)
		lines := []string{
			promptLine("prompt-1", secretPrompt, start),
			toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)),
			textLine("msg_main", secretMessage, 77, start.Add(2*time.Second)),
			quietResultLine(q.toolMain, failedResult, true, start.Add(3*time.Second)),
			toolUseLine("msg_lost", "toolu_lost", 9, start.Add(6*time.Second)),
		}
		if answered {
			lines = append(lines, resultWithUseResult("toolu_lost", secretResult, `{"stdout":"out","stderr":"","interrupted":false}`, start.Add(7*time.Second)))
		}
		q.writeTranscript(t, q.main, lines...)
		if err := os.Chtimes(q.main, start, start); err != nil {
			t.Fatal(err)
		}
		first := q.recover(t)
		if first.Rebuilt != 1 {
			t.Fatalf("RecoverQuiet = %+v, want 1 rebuilt call", first)
		}
		input, err := SanitizeInput("Bash", json.RawMessage(`{"command":"false","description":"Fail"}`))
		if err != nil {
			t.Fatal(err)
		}
		call := row(t, q.store, "calls", "tool_use_id = 'toolu_lost'")
		if call == nil || call["source"] != SourceTranscript || call["tool"] != "Bash" || call["request_id"] != "msg_lost" || call["session_id"] != "sess-1" ||
			call["prompt_id"] != "prompt-1" || call["cwd"] != "/tmp/demo-proj" || call["ts"] != start.Add(6*time.Second).UnixMilli() || call["input"] != input || call["agent_id"] != nil {
			t.Fatalf("rebuilt call = %v, want the transcript's session, tool, request, prompt, ts, cwd, sanitized input and source %q", call, SourceTranscript)
		}
		if request := row(t, q.store, "requests", "request_id = 'msg_lost'"); request == nil || request["calls"] != int64(1) {
			t.Errorf("request msg_lost = %v, want it to count the rebuilt call", request)
		}
		if model := row(t, q.store, "sessions", "session_id = 'sess-1'")["model"]; model != "claude-haiku-4-5-20251001" {
			t.Errorf("session model = %v, want its latest main-chat request's", model)
		}
		before := q.snapshot(t)
		if again := q.recover(t); again.Sessions != 0 || again.Rebuilt != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
		}
		return q
	}
	t.Run("its result on disk settles it", func(t *testing.T) {
		q := lost(t, true)
		if call := row(t, q.store, "calls", "tool_use_id = 'toolu_lost'"); call["bytes_delivered"] == nil || call["failed"] != int64(0) {
			t.Errorf("rebuilt call = bytes_delivered %v failed %v, want its result's size and 0", call["bytes_delivered"], call["failed"])
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0", n)
		}
	})
	t.Run("no result: one call marker", func(t *testing.T) {
		q := lost(t, false)
		fault := row(t, q.store, "faults", "tool_use_id = 'toolu_lost'")
		if fault == nil || fault["error"] != "call toolu_lost: "+UnfilledCall {
			t.Errorf("fault = %v, want the call marker of toolu_lost", fault)
		}
		if n := count(t, q.store, "faults"); n != 1 {
			t.Errorf("faults = %d, want 1", n)
		}
	})
}

// resultWithUseResult is a transcript's user line for one tool_result as Claude
// Code writes it: the block's content plus the top-level toolUseResult, the
// object PostToolUse's tool_response is.
func resultWithUseResult(toolUseID, text, useResult string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":%q,"tool_use_id":%q}]},"timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1","toolUseResult":%s}`,
		text, toolUseID, stamp(at), useResult)
}

// TestRecoverQuietSizesARebuiltCall: a rebuilt call whose transcript holds its
// tool_result is written in that same run with its size settled as PostToolUse
// would: bytes_real through RealBytes of the result's toolUseResult (the main
// chat's transcript, or the sub-agent's own), bytes_delivered, failed and no
// outcome label, and a second run changes nothing. One whose result is not on
// disk keeps the call marker (TestRecoverQuietRebuildsAnUnrecordedCall).
func TestRecoverQuietSizesARebuiltCall(t *testing.T) {
	type want struct{ real, delivered int64 }
	run := func(t *testing.T, id string, w want, build func(q quietStore, start time.Time)) {
		t.Helper()
		q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
		start := q.now.Add(-2 * time.Hour)
		build(q, start)
		for _, file := range []string{q.main, q.sub} {
			if err := os.Chtimes(file, start, start); err != nil {
				t.Fatal(err)
			}
		}
		if first := q.recover(t); first.Rebuilt != 1 {
			t.Fatalf("RecoverQuiet = %+v, want 1 rebuilt call", first)
		}
		where := "tool_use_id = '" + id + "'"
		call := row(t, q.store, "calls", where)
		if call == nil || call["bytes_real"] != w.real || call["bytes_delivered"] != w.delivered || call["failed"] != int64(0) || call["error"] != nil {
			t.Fatalf("rebuilt call = %v, want bytes_real %d bytes_delivered %d failed 0 and no outcome label", call, w.real, w.delivered)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0", n)
		}
		if again := q.recover(t); again.Sessions != 0 || again.Rebuilt != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := row(t, q.store, "calls", where); !reflect.DeepEqual(call, after) {
			t.Fatalf("second RecoverQuiet changed the call:\nbefore %v\nafter  %v", call, after)
		}
	}
	t.Run("a main-chat call", func(t *testing.T) {
		run(t, "toolu_lost", want{real: 4096, delivered: int64(len("preview of the output"))}, func(q quietStore, start time.Time) {
			q.writeTranscript(t, q.main,
				promptLine("prompt-1", secretPrompt, start),
				toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)),
				quietResultLine(q.toolMain, failedResult, true, start.Add(3*time.Second)),
				toolUseLine("msg_lost", "toolu_lost", 9, start.Add(6*time.Second)),
				resultWithUseResult("toolu_lost", "preview of the output", `{"stdout":"preview of the output","stderr":"","interrupted":false,"persistedOutputPath":"/tmp/x","persistedOutputSize":4096}`, start.Add(7*time.Second)),
			)
		})
	})
	t.Run("a sub-agent call", func(t *testing.T) {
		run(t, "toolu_lostsub", want{real: int64(len("the whole stdout line\n")), delivered: int64(len("sub"))}, func(q quietStore, start time.Time) {
			q.writeTranscript(t, q.sub,
				toolUseLine("msg_sub", q.toolSub, 31, start.Add(4*time.Second)),
				quietResultLine(q.toolSub, secretResult, false, start.Add(5*time.Second)),
				toolUseLine("msg_lostsub", "toolu_lostsub", 9, start.Add(6*time.Second)),
				resultWithUseResult("toolu_lostsub", "sub", `{"stdout":"the whole stdout line\n","stderr":"","interrupted":false}`, start.Add(7*time.Second)),
			)
		})
	})
}

// TestRecoverQuietSizesAnEarlierRebuiltCall: a call an earlier recovery
// rebuilt (source transcript, bytes_delivered set, bytes_real NULL, not failed)
// is still open for its real size. A quiet session holding it is a candidate
// even when every other call has a size; its result's toolUseResult fills
// bytes_real alone, leaving the delivered size as it is. A result with no
// toolUseResult cannot, so one call marker names it, and once marked it is no
// candidate. A second run writes nothing either way.
func TestRecoverQuietSizesAnEarlierRebuiltCall(t *testing.T) {
	earlier := func(t *testing.T, useResult string) quietStore {
		t.Helper()
		q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
		start := q.now.Add(-2 * time.Hour)
		// Every other call gets its size first, so only the rebuilt call is open.
		if first := q.recover(t); first.Calls != 2 {
			t.Fatalf("first RecoverQuiet = %+v, want the 2 recorded calls settled", first)
		}
		result := resultWithUseResult("toolu_lost", "preview of the output", useResult, start.Add(7*time.Second))
		if useResult == "" {
			result = quietResultLine("toolu_lost", "preview of the output", false, start.Add(7*time.Second))
		}
		q.writeTranscript(t, q.main,
			promptLine("prompt-1", secretPrompt, start),
			toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)),
			textLine("msg_main", secretMessage, 77, start.Add(2*time.Second)),
			quietResultLine(q.toolMain, failedResult, true, start.Add(3*time.Second)),
			toolUseLine("msg_lost", "toolu_lost", 9, start.Add(6*time.Second)),
			result,
		)
		if err := os.Chtimes(q.main, start, start); err != nil {
			t.Fatal(err)
		}
		rebuilt := RebuiltCall("sess-1", "", "", TranscriptRequest{MessageID: "msg_lost", PromptID: "prompt-1", TS: start.Add(6 * time.Second).UnixMilli(), Cwd: "/tmp/demo-proj"}, "toolu_lost", "Bash")
		rebuilt.BytesDelivered, rebuilt.Failed = Ptr(int64(674)), Ptr(false)
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			return tx.UpsertCall(context.Background(), rebuilt, FillEmpty)
		}); err != nil {
			t.Fatal(err)
		}
		if got := q.candidates(t); len(got) != 1 {
			t.Fatalf("quietCandidates = %v, want the session: its rebuilt call has no real size", got)
		}
		return q
	}
	where := "tool_use_id = 'toolu_lost'"
	t.Run("its toolUseResult fills the real size", func(t *testing.T) {
		q := earlier(t, `{"stdout":"preview of the output","persistedOutputSize":4096}`)
		if first := q.recover(t); first.Sessions != 1 {
			t.Fatalf("RecoverQuiet = %+v, want the session recovered", first)
		}
		call := row(t, q.store, "calls", where)
		if call["bytes_real"] != int64(4096) || call["bytes_delivered"] != int64(674) || call["failed"] != int64(0) || call["source"] != SourceTranscript || call["error"] != nil {
			t.Fatalf("call = %v, want bytes_real 4096 with bytes_delivered 674, failed 0 and source transcript untouched", call)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0", n)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Errorf("quietCandidates = %v, want none once sized", got)
		}
		if again := q.recover(t); again.Sessions != 0 || again.Calls != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := row(t, q.store, "calls", where); !reflect.DeepEqual(call, after) {
			t.Fatalf("second RecoverQuiet changed the call:\nbefore %v\nafter  %v", call, after)
		}
	})
	t.Run("no toolUseResult: one call marker", func(t *testing.T) {
		q := earlier(t, "")
		if first := q.recover(t); first.Unfillable != 1 {
			t.Fatalf("RecoverQuiet = %+v, want 1 unfillable call", first)
		}
		fault := row(t, q.store, "faults", "tool_use_id = 'toolu_lost'")
		if fault == nil || fault["error"] != "call toolu_lost: "+UnfilledCall {
			t.Fatalf("fault = %v, want the call marker of toolu_lost", fault)
		}
		call := row(t, q.store, "calls", where)
		if call["bytes_real"] != nil || call["bytes_delivered"] != int64(674) {
			t.Errorf("call = %v, want bytes_real NULL and bytes_delivered 674", call)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Errorf("quietCandidates = %v, want none once marked", got)
		}
		if again := q.recover(t); again.Sessions != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if n := count(t, q.store, "faults"); n != 1 {
			t.Errorf("faults = %d, want 1", n)
		}
		if after := row(t, q.store, "calls", where); !reflect.DeepEqual(call, after) {
			t.Fatalf("second RecoverQuiet changed the call:\nbefore %v\nafter  %v", call, after)
		}
	})
}

func TestRecoverQuietFillsAgentMetaWithoutWaiting(t *testing.T) {
	for _, test := range []struct {
		name       string
		meta       string
		directory  bool
		parent     any
		agentType  any
		transcript *string
		moved      bool
		wantParent any
		wantType   any
		wantCount  int
	}{
		{name: "live session", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, wantParent: "toolu_meta", wantType: "Explore", wantCount: 1},
		{name: "meta absent"},
		{name: "meta directory", directory: true},
		{name: "meta bad JSON", meta: `{`},
		{name: "meta empty", meta: `{}`},
		{name: "parent kept type filled", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, parent: "toolu_kept", wantParent: "toolu_kept", wantType: "Explore", wantCount: 1},
		{name: "type kept parent filled", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, agentType: "general-purpose", wantParent: "toolu_meta", wantType: "general-purpose", wantCount: 1},
		{name: "transcript NULL", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, transcript: Ptr("NULL")},
		{name: "transcript empty", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, transcript: Ptr("")},
		{name: "moved transcript", meta: `{"agentType":"Explore","toolUseId":"toolu_meta"}`, moved: true, wantParent: "toolu_meta", wantType: "Explore", wantCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := newQuietStore(t, time.Minute, 0)
			ctx := context.Background()
			meta := strings.TrimSuffix(q.sub, ".jsonl") + ".meta.json"
			if test.directory {
				if err := os.Mkdir(meta, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if test.meta != "" {
				// Claude Code's gym S2 meta shape: agentType and toolUseId.
				if err := os.WriteFile(meta, []byte(test.meta), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.store.Batch(ctx, func(tx *Tx) error {
				if _, err := tx.tx.ExecContext(ctx, `INSERT INTO agents(agent_id, session_id, parent_tool_use_id, agent_type) VALUES('a1', 'sess-1', ?1, ?2)`, test.parent, test.agentType); err != nil {
					return err
				}
				if test.transcript != nil {
					var path any = *test.transcript
					if *test.transcript == "NULL" {
						path = nil
					}
					_, err := tx.tx.ExecContext(ctx, `UPDATE sessions SET transcript_path = ?1, seat_dir = ?2 WHERE session_id = 'sess-1'`, path, filepath.Dir(filepath.Dir(filepath.Dir(q.main))))
					return err
				}
				if test.moved {
					_, err := tx.tx.ExecContext(ctx, `UPDATE sessions SET transcript_path = ?1, seat_dir = ?2 WHERE session_id = 'sess-1'`, filepath.Join(t.TempDir(), "missing", "sess-1.jsonl"), filepath.Dir(filepath.Dir(filepath.Dir(q.main))))
					return err
				}
				if _, err := tx.tx.ExecContext(ctx, `INSERT INTO sessions(session_id, transcript_path) VALUES('sess-no-path', '')`); err != nil {
					return err
				}
				_, err := tx.tx.ExecContext(ctx, `INSERT INTO agents(agent_id, session_id) VALUES('a2', 'sess-no-path')`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			first := q.recover(t)
			if first.Parents != test.wantCount || first.Sessions != 0 {
				t.Errorf("RecoverQuiet Parents = %d, Sessions = %d; want %d, 0", first.Parents, first.Sessions, test.wantCount)
			}
			agent := row(t, q.store, "agents", "agent_id = 'a1'")
			if agent["parent_tool_use_id"] != test.wantParent || agent["agent_type"] != test.wantType {
				t.Errorf("agent parent = %v, type = %v; want %v, %v", agent["parent_tool_use_id"], agent["agent_type"], test.wantParent, test.wantType)
			}
			if n := count(t, q.store, "faults"); n != 0 {
				t.Errorf("faults = %d, want 0", n)
			}
			if test.transcript == nil && !test.moved {
				unread := row(t, q.store, "agents", "agent_id = 'a2'")
				if unread["parent_tool_use_id"] != nil || unread["agent_type"] != nil {
					t.Errorf("transcript-less agent parent = %v, type = %v; want NULL, NULL", unread["parent_tool_use_id"], unread["agent_type"])
				}
			}
			if again := q.recover(t); again.Parents != 0 || again.Sessions != 0 {
				t.Errorf("second RecoverQuiet Parents = %d, Sessions = %d; want 0, 0", again.Parents, again.Sessions)
			}
		})
	}
}

func TestRecoverQuietAgentMetaStoreErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
		want string
	}{
		{name: "scan", sql: `INSERT INTO agents(agent_id, session_id) VALUES(NULL, 'sess-1')`, want: "scan an agent missing meta"},
		{name: "upsert", sql: `CREATE TRIGGER refuse_agent_meta BEFORE UPDATE ON agents BEGIN SELECT RAISE(ABORT, 'test meta fill failure'); END`, want: "test meta fill failure"},
		{name: "query", sql: `DROP TABLE agents`, want: "list agents missing meta"},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := newQuietStore(t, time.Minute, 0)
			ctx := context.Background()
			if _, err := q.store.DB().ExecContext(ctx, `INSERT INTO agents(agent_id, session_id) VALUES('a1', 'sess-1')`); err != nil {
				t.Fatal(err)
			}
			// Claude Code's gym S2 meta shape: agentType and toolUseId.
			if err := os.WriteFile(strings.TrimSuffix(q.sub, ".jsonl")+".meta.json", []byte(`{"agentType":"Explore","toolUseId":"toolu_meta"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := q.store.DB().ExecContext(ctx, test.sql); err != nil {
				t.Fatal(err)
			}
			summary, err := q.store.RecoverQuiet(ctx, q.now, QuietAfter)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RecoverQuiet error = %v, want %s", err, test.want)
			}
			if summary.Parents != 0 || summary.Sessions != 0 || len(summary.Skipped) != 0 {
				t.Errorf("RecoverQuiet Parents = %d, Sessions = %d, Skipped = %d; want 0, 0, 0", summary.Parents, summary.Sessions, len(summary.Skipped))
			}
			if test.name == "upsert" {
				agent := row(t, q.store, "agents", "agent_id = 'a1'")
				if agent["parent_tool_use_id"] != nil || agent["agent_type"] != nil {
					t.Errorf("failed fill parent = %v, type = %v; want NULL, NULL", agent["parent_tool_use_id"], agent["agent_type"])
				}
			}
		})
	}
}

// taskNoticeLine copies gym S2 main-transcript lines 38 (attachment.type,
// attachment.origin.kind, attachment.prompt) and 50 (origin.kind,
// message.content): both have a top-level timestamp and string notice body.
func taskNoticeLine(kind, agentID, status string, at time.Time) string {
	body := "<task-notification><task-id>" + agentID + "</task-id><status>" + status +
		"</status><summary>" + secretPrompt + "</summary><result>" + secretResult +
		"</result><usage><total_tokens>999999</total_tokens><tool_uses>999</tool_uses></usage></task-notification>"
	if kind == "attachment" {
		return fmt.Sprintf(`{"type":"attachment","timestamp":%q,"attachment":{"type":"queued_command","origin":{"kind":"task-notification"},"prompt":%q,"usage":{"totalTokens":999999,"toolUses":999}}}`, stamp(at), body)
	}
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"origin":{"kind":"task-notification"},"message":{"role":"user","content":%q}}`, stamp(at), body)
}

func TestRecoverQuietEndsAnAgentTurnAtItsNotice(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	notice := start.Add(20 * time.Second)
	for _, test := range []struct {
		name, kind, agent, status                                 string
		offset                                                    time.Duration
		woken, several, ended, live, noID, noStatus, noTranscript bool
	}{
		{name: "attachment notice", kind: "attachment"},
		{name: "user-entry notice", kind: "user"},
		{name: "failed status", status: "failed"},
		{name: "killed status", status: "killed"},
		{name: "woken agent", woken: true},
		{name: "several notices after the start", several: true},
		{name: "notice exactly at the start", offset: -20 * time.Second},
		{name: "notice before the start only", offset: -time.Second * 21},
		{name: "another agent", agent: "a2"},
		{name: "without task-id", noID: true},
		{name: "without status", noStatus: true},
		{name: "transcript turn end exists", ended: true, offset: -18 * time.Second},
		{name: "notice usage differs"},
		{name: "privacy"},
		{name: "live session", live: true},
		// A background agent failing before it wrote its transcript still gets
		// a notice: no totals to read, so the turn is marked once, as before.
		{name: "no agent transcript", noTranscript: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, agent, status := test.kind, test.agent, test.status
			if kind == "" {
				kind = "attachment"
			}
			if agent == "" {
				agent = "a1"
			}
			if status == "" {
				status = "completed"
			}
			noticeAt := notice.Add(test.offset)
			line := taskNoticeLine(kind, agent, status, noticeAt)
			if test.noID {
				line = strings.Replace(line, "<task-id>a1</task-id>", "", 1)
			}
			if test.noStatus {
				line = strings.Replace(line, "<status>completed</status>", "", 1)
			}
			mainLines := []string{line}
			if test.woken {
				mainLines = append(mainLines, taskNoticeLine(kind, "a1", status, start.Add(-time.Second)))
			}
			if test.several {
				// Deliberately out of time order: choose the earliest, not first.
				mainLines = append([]string{taskNoticeLine(kind, "a1", status, noticeAt.Add(time.Second))}, mainLines...)
			}
			subLines := []string{toolUseLine("msg_sub", "toolu_sub", 31, start.Add(time.Second)),
				quietResultLine("toolu_sub", secretResult, true, start.Add(1500*time.Millisecond)),
				strings.Replace(textLine("msg_sub", secretMessage, 77, start.Add(2*time.Second)), `"role":"assistant",`, `"role":"assistant","stop_reason":null,`, 1)}
			if test.offset == -20*time.Second {
				for i := range subLines {
					for _, offset := range []time.Duration{time.Second, 1500 * time.Millisecond, 2 * time.Second} {
						subLines[i] = strings.ReplaceAll(subLines[i], stamp(start.Add(offset)), stamp(start))
					}
				}
			}
			if test.woken {
				subLines = append([]string{endTurnLine("msg_old", 3, start.Add(-2*time.Second))}, subLines...)
			}
			wantEnd := noticeAt.UnixMilli()
			if test.ended {
				wantEnd = start.Add(3 * time.Second).UnixMilli()
				subLines = append(subLines, endTurnLine("msg_sub", 77, start.Add(3*time.Second)))
			}
			ctx := context.Background()
			q := newUnfilledStore(t, 2*time.Hour, start.Add(-time.Minute), mainLines, subLines, func(tx *Tx) error {
				if _, err := tx.tx.ExecContext(ctx, `INSERT INTO agents(agent_id, session_id, started) VALUES('a1', 'sess-1', ?)`, start.UnixMilli()); err != nil {
					return err
				}
				events := []Event{{EventID: "agent-start", Event: EventSubagentStart, SessionID: Ptr("sess-1"), AgentID: Ptr("a1"), TS: start.UnixMilli()}}
				if test.woken {
					events = append(events,
						Event{EventID: "old-start", Event: EventSubagentStart, SessionID: Ptr("sess-1"), AgentID: Ptr("a1"), TS: start.Add(-3 * time.Second).UnixMilli()},
						Event{EventID: "old-stop", Event: EventSubagentStop, SessionID: Ptr("sess-1"), AgentID: Ptr("a1"), TS: start.Add(-2 * time.Second).UnixMilli()})
				}
				for _, event := range events {
					if _, err := tx.InsertEvent(ctx, event); err != nil {
						return err
					}
				}
				return tx.RebuildAgentTurns(ctx, "a1")
			})
			if test.live {
				if err := q.store.Batch(ctx, func(tx *Tx) error {
					return tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: q.now.Add(-time.Minute).UnixMilli()})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if test.noTranscript {
				if err := os.Remove(q.sub); err != nil {
					t.Fatal(err)
				}
			}
			summary := q.recover(t)
			unfilled := test.offset < -20*time.Second || test.agent == "a2" || test.noID || test.noStatus || test.noTranscript
			if len(summary.Skipped) != 0 {
				t.Fatalf("RecoverQuiet skipped %v, want nothing skipped", summary.Skipped)
			}
			if test.live || unfilled {
				wantFaults := 0
				if unfilled {
					wantFaults = 1
				}
				if summary.AgentStops != 0 || summary.Unfillable != wantFaults || count(t, q.store, "faults") != wantFaults {
					t.Fatalf("RecoverQuiet = %+v, want no stops and %d unfillable faults", summary, wantFaults)
				}
				if turn := row(t, q.store, "agent_turns", "agent_id = 'a1' AND stopped IS NULL"); turn == nil {
					t.Fatal("open turn was closed")
				}
				if unfilled {
					if fault := row(t, q.store, "faults", "stage = 'transcript'"); fault["error"] != "agent a1 turn 1 open: "+UnfilledAgentStop {
						t.Fatal("unfilled stop fault text changed")
					}
					if again := q.recover(t); again.Unfillable != 0 || count(t, q.store, "faults") != 1 {
						t.Fatal("unfillable turn was marked again")
					}
				}
				return
			}
			if summary.AgentStops != 1 || summary.Unfillable != 0 || count(t, q.store, "faults") != 0 {
				t.Fatalf("RecoverQuiet = %+v, want 1 stop and no faults", summary)
			}
			stop := row(t, q.store, "events", "event = 'SubagentStop' AND detail = ?", RecoveredDetail)
			if stop == nil || stop["ts"] != wantEnd || stop["detail"] != RecoveredDetail {
				t.Fatalf("stop = %v, want recovered stop at %d", stop, wantEnd)
			}
			seq := 1
			if test.woken {
				seq = 2
			}
			turn := row(t, q.store, "agent_turns", "agent_id = 'a1' AND seq = ?", seq)
			if turn["stopped"] != wantEnd || turn["stop_event_id"] != stop["event_id"] {
				t.Fatal("agent turn did not close at the recovered stop")
			}
			if test.woken && row(t, q.store, "agent_turns", "agent_id = 'a1' AND seq = 1")["stopped"] != start.Add(-2*time.Second).UnixMilli() {
				t.Fatal("earlier turn changed")
			}
			totals, err := ReadAgentTotals(q.sub, "")
			if err != nil {
				t.Fatal(err)
			}
			agentRow := row(t, q.store, "agents", "agent_id = 'a1'")
			if agentRow["total_tokens"] != totals.TotalTokens || agentRow["tool_uses"] != totals.ToolUses {
				t.Fatal("agent totals differ from its own transcript")
			}
			assertNoTranscriptText(t, q.store)
			if again := q.recover(t); again.AgentStops != 0 || again.Unfillable != 0 {
				t.Fatal("recovery repeated the stop")
			}
		})
	}
}

// TestRecoverQuietMarksAnOpenAgentTurnOnce: a sub-agent's latest turn with no
// SubagentStop, transcript turn end or qualifying task notice is marked once
// by one transcript fault. Its parent Agent call and type come from the meta
// file. A later hook reopens the turn; a transcript turn end rebuilds its
// missing SubagentStop at that end line's own ts, once.
func TestRecoverQuietMarksAnOpenAgentTurnOnce(t *testing.T) {
	want := "agent a1 turn 1 open: " + UnfilledAgentStop
	open := func(t *testing.T, subLines ...string) quietStore {
		t.Helper()
		q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
		start := q.now.Add(-2 * time.Hour)
		if subLines != nil {
			q.writeTranscript(t, q.sub, subLines...)
		}
		meta := strings.TrimSuffix(q.sub, ".jsonl") + ".meta.json"
		if err := os.WriteFile(meta, []byte(`{"agentType":"Explore","toolUseId":"toolu_parent"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{q.sub, meta} {
			if err := os.Chtimes(file, start, start); err != nil {
				t.Fatal(err)
			}
		}
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			if _, err := tx.tx.ExecContext(context.Background(), `INSERT INTO agents(agent_id, session_id, started) VALUES('a1', 'sess-1', ?1)`, start.UnixMilli()); err != nil {
				return err
			}
			_, err := tx.tx.ExecContext(context.Background(), `INSERT INTO agent_turns(agent_id, seq, session_id, started) VALUES('a1', 1, 'sess-1', ?1)`, start.UnixMilli())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return q
	}
	marked := func(t *testing.T) quietStore {
		t.Helper()
		q := open(t)
		if first := q.recover(t); first.Unfillable != 1 || first.Parents != 1 {
			t.Fatalf("RecoverQuiet = %+v, want 1 unfillable (the open turn) and 1 agent linked", first)
		}
		lastHook := row(t, q.store, "sessions", "session_id = 'sess-1'")["last_ts"]
		if fault := row(t, q.store, "faults", "stage = 'transcript'"); fault == nil || fault["error"] != want || fault["ts"] != lastHook {
			t.Fatalf("fault = %v, want %q at %v (the session's last hook)", fault, want, lastHook)
		}
		if agent := row(t, q.store, "agents", "agent_id = 'a1'"); agent["parent_tool_use_id"] != "toolu_parent" || agent["agent_type"] != "Explore" {
			t.Errorf("agent = %v, want parent toolu_parent and type Explore from its meta file", agent)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Fatalf("quietCandidates after the marker = %v, want none", got)
		}
		before := q.snapshot(t)
		if again := q.recover(t); again.Sessions != 0 || again.Unfillable != 0 || again.Parents != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
		}
		return q
	}
	t.Run("marked once", func(t *testing.T) { marked(t) })
	t.Run("a later hook reopens it", func(t *testing.T) {
		q := marked(t)
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			return tx.TouchSession(context.Background(), Session{SessionID: "sess-1", TS: q.now.Add(time.Second).UnixMilli()})
		}); err != nil {
			t.Fatal(err)
		}
		turns, err := q.store.unmarkedOpenAgentTurns(context.Background(), "sess-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 1 || turns[0].agentID != "a1" || turns[0].seq != 1 {
			t.Errorf("unmarkedOpenAgentTurns after a later hook = %+v, want a1's turn 1", turns)
		}
	})
	t.Run("a turn end in its transcript is no open turn", func(t *testing.T) {
		start := time.Now().Add(-2 * time.Hour)
		q := open(t, toolUseLine("msg_sub", "toolu_sub", 31, start.Add(4*time.Second)),
			quietResultLine("toolu_sub", secretResult, false, start.Add(5*time.Second)),
			textLine("msg_sub_end", secretMessage, 3, start.Add(6*time.Second)),
			endTurnLine("msg_sub_end", 7, start.Add(6500*time.Millisecond)))
		if first := q.recover(t); first.AgentStops != 1 {
			t.Errorf("RecoverQuiet = %+v, want 1 agent stop rebuilt", first)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0: the transcript shows the turn's end", n)
		}
		end := start.Add(6500 * time.Millisecond).UnixMilli() // the end line's own ts, not the message's first line
		stop := row(t, q.store, "events", "event = 'SubagentStop'")
		if stop == nil || stop["agent_id"] != "a1" || stop["session_id"] != "sess-1" || stop["ts"] != end || stop["detail"] != RecoveredDetail {
			t.Fatalf("SubagentStop event = %v, want agent a1 of sess-1 at %d, %s", stop, end, RecoveredDetail)
		}
		turn := row(t, q.store, "turns", "event = 'SubagentStop'")
		if turn == nil || turn["event_id"] != stop["event_id"] || turn["agent_id"] != "a1" || turn["ts"] != end || turn["last_assistant_message_bytes"] != nil {
			t.Errorf("SubagentStop turn = %v, want the event's id, agent a1 at %d, no reply size", turn, end)
		}
		agentTurn := row(t, q.store, "agent_turns", "agent_id = 'a1'")
		if agentTurn["stopped"] != end || agentTurn["stop_event_id"] != stop["event_id"] || count(t, q.store, "agent_turns") != 1 {
			t.Errorf("agent turn = %v, want its one turn stopped at %d by the rebuilt event", agentTurn, end)
		}
		agent := row(t, q.store, "agents", "agent_id = 'a1'")
		if agent["stopped"] != end || agent["tool_uses"] != int64(1) || agent["total_tokens"] == nil {
			t.Errorf("agent = %v, want stopped at %d, 1 tool use and its tokens from the transcript", agent, end)
		}
		if reply := row(t, q.store, "requests", "request_id = 'msg_sub_end'"); reply == nil || reply["agent_id"] != "a1" {
			t.Errorf("reply request = %v, want msg_sub_end of agent a1", reply)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Fatalf("quietCandidates after the rebuild = %v, want none", got)
		}
		before := q.snapshot(t)
		events := count(t, q.store, "events")
		if again := q.recover(t); again.Sessions != 0 || again.AgentStops != 0 || again.Unfillable != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := q.snapshot(t); !reflect.DeepEqual(before, after) || count(t, q.store, "events") != events {
			t.Fatalf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
		}
	})
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

// unfilledTurnEndText is the fault text the turn-end marker carries, as Go
// formats it: the SQL of turnEndMarker must produce the same bytes.
func unfilledTurnEndText(promptID, eventID string) string {
	return fmt.Sprintf("prompt %s UserPromptSubmit %s: %s", promptID, eventID, UnfilledTurnEnd)
}

// submitPrompt stores a main-chat UserPromptSubmit event.
func submitPrompt(tx *Tx, eventID, promptID string, at time.Time) error {
	_, err := tx.InsertEvent(context.Background(), Event{EventID: eventID, Event: "UserPromptSubmit", SessionID: Ptr("sess-1"),
		PromptID: Ptr(promptID), TS: at.UnixMilli()})
	return err
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

// TestRecoverQuietMarksAnUnendedPromptOnce: a latest prompt with no Stop or
// StopFailure whose quiet main transcript shows no turn end after it is read
// once: one transcript fault names the prompt and the session stops being a
// candidate, so a later report never reads the transcript again. A later prompt
// or any later hook makes it a candidate again.
func TestRecoverQuietMarksAnUnendedPromptOnce(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	// toolu_x, the call running when the session went quiet, has its row: this
	// test is about the prompt alone (TestRecoverQuietRebuildsAnUnrecordedCall).
	seed := func(tx *Tx) error {
		if err := tx.UpsertCall(context.Background(), Call{ToolUseID: "toolu_x", SessionID: Ptr("sess-1"), RequestID: Ptr("msg_tool"),
			Tool: Ptr("Bash"), BytesReal: Ptr(int64(3)), Source: Ptr(SourceHook)}, Overwrite); err != nil {
			return err
		}
		return submitPrompt(tx, "prompt-ev", "prompt-P", start)
	}
	want := unfilledTurnEndText("prompt-P", "prompt-ev")
	// marked is a store whose prompt recovery has marked, after its first pass.
	marked := func(t *testing.T, mainLines ...string) quietStore {
		t.Helper()
		q := newUnfilledStore(t, 2*time.Hour, start.Add(-time.Minute), mainLines, nil, seed)
		if got := q.candidates(t); len(got) != 1 {
			t.Fatalf("quietCandidates = %v, want the session: its latest prompt has no turn end", got)
		}
		if summary := q.recover(t); summary.Unfillable != 1 {
			t.Fatalf("RecoverQuiet = %+v, want 1 unfillable: the unended prompt", summary)
		}
		// The fault sits inside the session's own span: at its last hook, not at the
		// report's marking time.
		lastHook := row(t, q.store, "sessions", "session_id = 'sess-1'")["last_ts"]
		fault := row(t, q.store, "faults", "stage = 'transcript'")
		if fault == nil || fault["session_id"] != "sess-1" || fault["ts"] != lastHook || fault["error"] != want {
			t.Fatalf("fault = %v, want session sess-1, ts %v (the session's last hook), error %q", fault, lastHook, want)
		}
		if got := q.candidates(t); len(got) != 0 {
			t.Fatalf("quietCandidates after the marker = %v, want none", got)
		}
		before := q.snapshot(t)
		if again := q.recover(t); again.Sessions != 0 || again.Unfillable != 0 || again.TurnEnds != 0 {
			t.Fatalf("second RecoverQuiet = %+v, want nothing", again)
		}
		if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
			t.Fatalf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
		}
		if n := count(t, q.store, "faults"); n != 1 {
			t.Fatalf("faults = %d, want 1: the marker is recorded once", n)
		}
		return q
	}
	running := []string{promptLine("prompt-P", secretPrompt, start), toolUseLine("msg_tool", "toolu_x", 5, start.Add(time.Second))}
	t.Run("a tool call and no turn end", func(t *testing.T) {
		marked(t, running...)
	})
	t.Run("a turn end older than the prompt", func(t *testing.T) {
		// The last message is an earlier turn's answer: RecoverTurnEnd refuses it.
		q := marked(t, promptLine("prompt-old", secretPrompt, start.Add(-3*time.Minute)), endTurnLine("msg_old", 9, start.Add(-2*time.Minute)))
		if n := count(t, q.store, "turns"); n != 0 {
			t.Errorf("turns = %d, want 0: the earlier answer is no turn end of this prompt", n)
		}
	})
	t.Run("a later prompt", func(t *testing.T) {
		q := marked(t, running...)
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			return submitPrompt(tx, "prompt-ev2", "prompt-Q", start.Add(time.Minute))
		}); err != nil {
			t.Fatal(err)
		}
		if got := q.candidates(t); len(got) != 1 {
			t.Errorf("quietCandidates after a later prompt = %v, want the session again", got)
		}
	})
	t.Run("a later hook", func(t *testing.T) {
		q := marked(t, running...)
		if err := q.store.Batch(context.Background(), func(tx *Tx) error {
			return tx.TouchSession(context.Background(), Session{SessionID: "sess-1", TS: q.now.Add(time.Second).UnixMilli()})
		}); err != nil {
			t.Fatal(err)
		}
		got, err := q.store.quietCandidates(context.Background(), q.now.Add(2*time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("quietCandidates after a later hook = %v, want the session again", got)
		}
	})
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
	t.Run("live with an unended prompt", func(t *testing.T) {
		q := newUnfilledStore(t, 0, start, []string{promptLine("prompt-P", secretPrompt, start)}, nil,
			func(tx *Tx) error { return submitPrompt(tx, "prompt-ev", "prompt-P", start) })
		if summary := q.recover(t); summary.Sessions != 0 || summary.Unfillable != 0 {
			t.Errorf("RecoverQuiet = %+v, want nothing for a live session", summary)
		}
		if n := count(t, q.store, "faults"); n != 0 {
			t.Errorf("faults = %d, want 0: a live session's prompt may still end", n)
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

// TestRecoverQuietSizesAHookCallFromItsToolUseResult: a hook-written call whose
// PostToolUse was lost (store busy) and that no Stop or SessionEnd sweep
// settled, its row the PreToolUse's alone or the batch's too, gets the real
// size PostToolUse would have stored: RealBytes of its result line's
// toolUseResult, not the delivered text's length. With no result on disk the
// call marker stands for it. A batch row with no ts (stored before every hook
// set one) keeps its batch size alone: recovery has no hook time to give it a
// ts, and sizing it would leave a sized call with none. Only sizes are stored,
// and a second pass writes nothing.
func TestRecoverQuietSizesAHookCallFromItsToolUseResult(t *testing.T) {
	stdout := secretResult + strings.Repeat("-", 40) // the delivered text is cut short of it
	for _, c := range []struct {
		name                string
		batch, noTS, result bool
	}{
		{name: "PreToolUse row only", result: true},
		{name: "batch row", batch: true, result: true},
		{name: "batch row with no ts", batch: true, noTS: true, result: true},
		{name: "no result on disk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
			start := time.UnixMilli(q.callTS)
			lines := []string{
				promptLine("prompt-1", secretPrompt, start),
				toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)),
				textLine("msg_main", secretMessage, 77, start.Add(2*time.Second)),
			}
			if c.result {
				lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":%q,"tool_use_id":%q}]},"toolUseResult":{"stdout":%q,"stderr":"","interrupted":false,"isImage":false},"timestamp":%q,"cwd":"/tmp/demo-proj","sessionId":"sess-1"}`,
					secretResult, q.toolMain, stdout, stamp(start.Add(3*time.Second))))
			}
			q.writeTranscript(t, q.main, lines...)
			mtime := q.now.Add(-2 * time.Hour)
			if err := os.Chtimes(q.main, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if c.batch {
				if err := q.store.UpsertCall(context.Background(), Call{ToolUseID: q.toolMain,
					BytesDelivered: Ptr(int64(len(secretResult))), RequestID: Ptr("msg_main")}, Overwrite); err != nil {
					t.Fatal(err)
				}
			}
			if c.noTS {
				if _, err := q.store.DB().Exec("UPDATE calls SET ts = NULL WHERE tool_use_id = ?", q.toolMain); err != nil {
					t.Fatal(err)
				}
			}
			q.recover(t)

			main := row(t, q.store, "calls", "tool_use_id = ?", q.toolMain)
			marks := row(t, q.store, "faults", "tool_use_id = ? AND stage = ? AND error LIKE ?", q.toolMain, StageTranscript, "call "+q.toolMain+": %")
			switch {
			case c.noTS:
				if main["bytes_real"] != nil || main["ts"] != nil || main["bytes_delivered"] != int64(len(secretResult)) {
					t.Errorf("ts-less batch call = bytes_real %v ts %v bytes_delivered %v, want NULL, NULL, %d: its batch size alone",
						main["bytes_real"], main["ts"], main["bytes_delivered"], len(secretResult))
				}
			case c.result:
				if main["bytes_real"] != int64(len(stdout)) || main["bytes_delivered"] != int64(len(secretResult)) || main["failed"] != int64(0) {
					t.Errorf("main call = bytes_real %v bytes_delivered %v failed %v, want %d (its toolUseResult's stdout), %d, 0",
						main["bytes_real"], main["bytes_delivered"], main["failed"], len(stdout), len(secretResult))
				}
				if marks != nil {
					t.Errorf("a sized call was marked: %v", marks)
				}
			default:
				if main["bytes_real"] != nil || marks == nil {
					t.Errorf("main call with no result = bytes_real %v, marker %v: want NULL and the call marker", main["bytes_real"], marks)
				}
			}
			for _, column := range []string{"input", "error"} {
				if text := fmt.Sprint(main[column]); strings.Contains(text, secretResult) {
					t.Errorf("calls.%s holds the result text", column)
				}
			}

			before := q.snapshot(t)
			if again := q.recover(t); again.Calls != 0 {
				t.Errorf("second RecoverQuiet = %+v, want 0 calls", again)
			}
			if after := q.snapshot(t); !reflect.DeepEqual(before, after) {
				t.Errorf("second RecoverQuiet changed the store:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// Walker C: a long-lived session whose early rows were pruned gets them back
// from recovery, which reads from sessions.first_ts.
func TestRecoverQuietNeverResurrectsPrunedRows(t *testing.T) {
	ctx := context.Background()
	q := quietStore{store: openTestStore(t), now: time.Now()}
	dir := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj")
	q.main = filepath.Join(dir, "sess-1.jsonl")
	old := q.now.Add(-40 * 24 * time.Hour)
	recent := q.now.Add(-2 * time.Hour)
	q.writeTranscript(t, q.main,
		promptLine("prompt-old", secretPrompt, old),
		toolUseLine("msg_old", "toolu_old", 5, old.Add(time.Second)),
		quietResultLine("toolu_old", "ok", false, old.Add(2*time.Second)),
		promptLine("prompt-new", secretPrompt, recent),
		toolUseLine("msg_new", "toolu_new", 5, recent.Add(time.Second)),
		quietResultLine("toolu_new", "ok", false, recent.Add(2*time.Second)),
	)
	if err := os.Chtimes(q.main, recent, recent); err != nil {
		t.Fatal(err)
	}
	err := q.store.Batch(ctx, func(tx *Tx) error {
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: old.UnixMilli(), TranscriptPath: Ptr(q.main)}); err != nil {
			return err
		}
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: recent.Add(3 * time.Second).UnixMilli()}); err != nil {
			return err
		}
		for _, c := range []Call{
			{ToolUseID: "toolu_old", SessionID: Ptr("sess-1"), TS: Ptr(old.Add(time.Second).UnixMilli()), Tool: Ptr("Bash"), Source: Ptr(SourceHook),
				RequestID: Ptr("msg_old"), BytesReal: Ptr(int64(2)), BytesDelivered: Ptr(int64(2)), Failed: Ptr(false)},
			{ToolUseID: "toolu_new", SessionID: Ptr("sess-1"), TS: Ptr(recent.Add(time.Second).UnixMilli()), Tool: Ptr("Bash"), Source: Ptr(SourceHook),
				RequestID: Ptr("msg_new"), BytesReal: Ptr(int64(2)), BytesDelivered: Ptr(int64(2)), Failed: Ptr(false)},
		} {
			if err := tx.UpsertCall(ctx, c, Overwrite); err != nil {
				return err
			}
		}
		for _, r := range []Request{
			{RequestID: "msg_old", SessionID: Ptr("sess-1"), TS: Ptr(old.Add(time.Second).UnixMilli()), Pending: Ptr(false), Source: Ptr(SourceHook), Model: Ptr("m"), StopReason: Ptr("tool_use"), InputTokens: Ptr(int64(1)), CacheReadTokens: Ptr(int64(1)), CacheCreationTokens: Ptr(int64(1)), ContextTokens: Ptr(int64(3)), OutputTokens: Ptr(int64(5)), Calls: Ptr(int64(1))},
			{RequestID: "msg_new", SessionID: Ptr("sess-1"), TS: Ptr(recent.Add(time.Second).UnixMilli()), Pending: Ptr(false), Source: Ptr(SourceHook), Model: Ptr("m"), StopReason: Ptr("tool_use"), Calls: Ptr(int64(1))},
		} {
			if err := tx.UpsertRequest(ctx, r, Overwrite); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := q.store.Prune(ctx, q.now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prune 1 removed %d rows; calls=%d requests=%d", removed, count(t, q.store, "calls"), count(t, q.store, "requests"))
	if row(t, q.store, "calls", "tool_use_id = 'toolu_old'") != nil {
		t.Fatalf("prune kept toolu_old")
	}
	summary, err := q.store.RecoverQuiet(ctx, q.now, QuietAfter)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("RecoverQuiet skipped %v", summary.Skipped)
	}
	c := row(t, q.store, "calls", "tool_use_id = 'toolu_old'")
	r := row(t, q.store, "requests", "request_id = 'msg_old'")
	t.Logf("after recovery: toolu_old=%v", c != nil)
	t.Logf("after recovery: msg_old=%v", r != nil)
	if c != nil {
		t.Logf("resurrected call source=%v ts=%v", c["source"], c["ts"])
	}
	cutoff := q.now.Add(-30 * 24 * time.Hour).UnixMilli()
	for _, table := range []string{"calls", "requests"} {
		var expired int
		if err := q.store.DB().QueryRow("SELECT COUNT(*) FROM "+table+" WHERE ts < ?", cutoff).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired != 0 {
			t.Errorf("recovery resurrected pruned rows: %s has %d expired rows", table, expired)
		}
	}
	if row(t, q.store, "requests", "request_id = 'msg_new'") == nil {
		t.Error("recovery lost the recent request")
	}
	if c != nil || r != nil {
		t.Errorf("recovery resurrected pruned rows: call %v request %v", c != nil, r != nil)
	}
}

// Walker C: a session row pruned and archived, then the session resumed and
// pruned again: the newer row is deleted while the archive keeps the old copy.

func TestRecoverQuietResolvedPendingRefreshesSessionModel(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	ctx := context.Background()
	key := ProvisionalKey(q.toolMain)
	if err := q.store.Batch(ctx, func(tx *Tx) error {
		if err := tx.UpsertRequest(ctx, Request{RequestID: key, SessionID: Ptr("sess-1"), TS: Ptr(q.callTS), Pending: Ptr(true),
			Calls: Ptr(int64(1)), Source: Ptr(SourceHook)}, Overwrite); err != nil {
			return err
		}
		return tx.UpsertCall(ctx, Call{ToolUseID: q.toolMain, RequestID: Ptr(key), BytesDelivered: Ptr(int64(5))}, Overwrite)
	}); err != nil {
		t.Fatal(err)
	}
	q.recover(t)
	req := row(t, q.store, "requests", "request_id = 'msg_main'")
	sess := row(t, q.store, "sessions", "session_id = 'sess-1'")
	t.Logf("resolved main request model=%v agent=%v; sessions.model=%v", req["model"], req["agent_id"], sess["model"])
	if req["model"] == nil || sess["model"] != req["model"] {
		t.Errorf("sessions.model stays NULL after recovery resolved the session's only main-chat request to model %v", req["model"])
	}
}

func TestRecoverQuietReadsTheNewestTranscriptPath(t *testing.T) {
	for i, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
			start := time.UnixMilli(q.firstTS).Add(time.Minute)
			q.writeTranscript(t, q.main, promptLine("prompt-1", secretPrompt, start), endTurnLine("msg_end", 9, start.Add(time.Second)))
			if err := os.Chtimes(q.main, start, start); err != nil {
				t.Fatal(err)
			}
			// A second run names the current file; the first run's root is gone.
			if _, err := q.store.DB().Exec("DELETE FROM sessions"); err != nil {
				t.Fatal(err)
			}
			runs := []Session{
				{SessionID: "sess-1", TS: q.firstTS, TranscriptPath: Ptr(filepath.Join(t.TempDir(), "gone", "sess-1.jsonl"))},
				{SessionID: "sess-1", TS: start.UnixMilli(), TranscriptPath: Ptr(q.main)},
			}
			if reverse {
				runs[0], runs[1] = runs[1], runs[0]
			}
			touchSessionRuns(t, q.store, runs)
			putRows(t, q.store, []Event{promptEvent("u1", "sess-1", start.UnixMilli())}, nil)
			q.recover(t)
			if n := countWhere(t, q.store, "SELECT COUNT(*) FROM turns WHERE event = 'Stop' AND session_id = 'sess-1'"); n != 1 {
				t.Errorf("rebuilt Stop rows = %d, want 1", n)
			}
		})
	}
}

func TestRecoverQuietFindsAMovedTranscript(t *testing.T) {
	for _, viaSeat := range []bool{false, true} {
		t.Run(fmt.Sprintf("seat=%v", viaSeat), func(t *testing.T) {
			q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
			start := time.UnixMilli(q.firstTS).Add(time.Minute)
			q.writeTranscript(t, q.main, promptLine("prompt-1", secretPrompt, start), endTurnLine("msg_end", 9, start.Add(time.Second)))
			if err := os.Chtimes(q.main, start, start); err != nil {
				t.Fatal(err)
			}
			projects := filepath.Dir(filepath.Dir(q.main))
			if err := os.Rename(filepath.Dir(q.main), filepath.Join(projects, "-tmp-moved-proj")); err != nil {
				t.Fatal(err)
			}
			if viaSeat {
				dead := filepath.Join(t.TempDir(), "gone", "sess-1.jsonl")
				if _, err := q.store.DB().Exec("UPDATE sessions SET transcript_path = ?, seat_dir = ?", dead, filepath.Dir(projects)); err != nil {
					t.Fatal(err)
				}
			}
			putRows(t, q.store, []Event{promptEvent("u1", "sess-1", start.UnixMilli())}, nil)
			q.recover(t)
			if n := countWhere(t, q.store, "SELECT COUNT(*) FROM turns WHERE event = 'Stop' AND session_id = 'sess-1'"); n != 1 {
				t.Errorf("rebuilt Stop rows = %d, want 1", n)
			}
		})
	}
}

func TestResolveTranscriptKeepsTheStoredPathOrFindsTheFirstMatch(t *testing.T) {
	root := t.TempDir()
	stored := filepath.Join(root, "old-projects", "old", "s1.jsonl")
	first := filepath.Join(root, "old-projects", "a", "s1.jsonl")
	second := filepath.Join(root, "old-projects", "z", "s1.jsonl")
	seat := filepath.Join(root, "seat")
	seatPath := filepath.Join(seat, "projects", "a", "s1.jsonl")
	for _, path := range []string{stored, second, first, seatPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name, path, seatDir, session, want string) {
		t.Helper()
		if got := ResolveTranscript(path, seatDir, session); got != want {
			t.Errorf("%s: resolved path = %q, want %q", name, got, want)
		}
	}
	check("existing stored path wins", stored, seat, "s1", stored)
	if err := os.Remove(stored); err != nil {
		t.Fatal(err)
	}
	check("stored root first sorted match", stored, seat, "s1", first)
	dead := filepath.Join(root, "gone-projects", "old", "s1.jsonl")
	check("seat root fallback", dead, seat, "s1", seatPath)
	check("empty stored path", "", seat, "s1", seatPath)
	check("no match preserves stored path", stored, seat, "absent", stored)
	check("no recorded roots", "", "", "s1", "")
}

func TestRecoverQuietSettlesAnEditWithoutARealSize(t *testing.T) {
	for _, response := range noRealOutputResponses(t) {
		for _, state := range []string{"lost PostToolUse", "batch only", "rebuilt", "landed"} {
			t.Run(response.name+"/"+state, func(t *testing.T) {
				q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
				start := time.UnixMilli(q.callTS)
				use := strings.Replace(toolUseLine("msg_main", q.toolMain, 5, start.Add(time.Second)), `"name":"Bash"`, `"name":"`+response.tool+`"`, 1)
				use = strings.Replace(use, `"input":{"command":"false","description":"Fail"}`, `"input":`+string(response.input), 1)
				q.writeTranscript(t, q.main,
					promptLine("prompt-1", secretPrompt, start), use,
					textLine("msg_main", secretMessage, 77, start.Add(2*time.Second)),
					resultWithUseResult(q.toolMain, secretResult, string(response.raw), start.Add(3*time.Second)),
				)
				if err := os.Chtimes(q.main, start, start); err != nil {
					t.Fatal(err)
				}
				call := Call{ToolUseID: q.toolMain, Tool: Ptr(response.tool)}
				if state == "batch only" {
					call.BytesDelivered = Ptr(int64(len(secretResult)))
				}
				if state == "landed" {
					call.Failed = Ptr(false)
					call.RequestID = Ptr("msg_main")
				}
				if state == "rebuilt" {
					if _, err := q.store.DB().Exec("DELETE FROM calls WHERE tool_use_id = ?", q.toolMain); err != nil {
						t.Fatal(err)
					}
				} else if err := q.store.UpsertCall(context.Background(), call, Overwrite); err != nil {
					t.Fatal(err)
				}
				first := q.recover(t)
				main := row(t, q.store, "calls", "tool_use_id = ?", q.toolMain)
				if main["bytes_real"] != nil || main["failed"] != int64(0) {
					t.Errorf("real=%v failed=%v, want NULL, 0", main["bytes_real"], main["failed"])
				}
				if state != "landed" && main["bytes_delivered"] != int64(len(secretResult)) {
					t.Errorf("delivered=%v, want %d", main["bytes_delivered"], len(secretResult))
				}
				if first.Unfillable != 0 {
					t.Errorf("RecoverQuiet unfillable=%d, want 0", first.Unfillable)
				}
				var n int
				if err := q.store.DB().QueryRow("SELECT COUNT(*) FROM faults WHERE tool_use_id = ? AND error LIKE ?", q.toolMain, "%"+UnfilledCall+"%").Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Errorf("call markers=%d, want 0", n)
				}
				if calls, err := q.store.UnfinishedCalls(context.Background(), "sess-1"); err != nil {
					t.Fatal(err)
				} else {
					for _, c := range calls {
						if c.ToolUseID == q.toolMain {
							t.Error("landed call remains unfinished")
						}
					}
				}
				if candidates := q.candidates(t); len(candidates) != 0 {
					t.Errorf("quietCandidates = %d, want 0", len(candidates))
				}
				before := q.snapshot(t)
				if again := q.recover(t); again.Sessions != 0 || again.Calls != 0 || again.Requests != 0 || again.Rebuilt != 0 || again.Unfillable != 0 {
					t.Errorf("second RecoverQuiet=%+v, want nothing to do", again)
				}
				if !reflect.DeepEqual(before, q.snapshot(t)) {
					t.Error("second recovery changed stored rows")
				}
				for _, r := range q.snapshot(t) {
					for _, value := range r {
						text := fmt.Sprint(value)
						if strings.Contains(text, secretResult) || strings.Contains(text, secretMessage) || strings.Contains(text, secretPrompt) {
							t.Fatal("transcript text stored")
						}
					}
				}
			})
		}
	}
}

// appendMarkLines appends the fixture's real mark lines (testdata/transcript-marks.jsonl)
// to a transcript, each restamped at `at` + its position and with its uuid
// suffixed by tag, so two transcripts hold distinct entries; the mtime is kept.
func (q quietStore) appendMarkLines(t *testing.T, path, tag string, at time.Time, kinds ...string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/transcript-marks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stamped := regexp.MustCompile(`"timestamp":"[^"]*"`)
	uuid := regexp.MustCompile(`"uuid":"([^"]*)"`)
	var add []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		for _, kind := range kinds {
			if !strings.Contains(line, kind) {
				continue
			}
			line = stamped.ReplaceAllString(line, `"timestamp":"`+stamp(at.Add(time.Duration(len(add))*time.Second))+`"`)
			line = uuid.ReplaceAllString(line, `"uuid":"${1}-`+tag+`"`)
			add = append(add, line)
			break
		}
	}
	if len(add) < len(kinds) {
		t.Fatalf("fixture holds %d lines for the %d kinds %v", len(add), len(kinds), kinds)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Join(add, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverQuietWritesTheMarksOfAQuietSession(t *testing.T) {
	q := newQuietStore(t, 2*time.Hour, 2*time.Hour)
	at := q.now.Add(-2*time.Hour + time.Minute)
	q.appendMarkLines(t, q.main, "main", at, `"stop_hook_summary"`, `"turn_duration"`, `"compact_boundary"`, `"cost-state"`)
	q.appendMarkLines(t, q.sub, "sub", at, `"turn_duration"`)
	q.recover(t)

	for table, want := range map[string]int{"compactions": 1, "stop_hooks": 1, "stop_hook_runs": 4, "turn_durations": 2, "session_costs": 1} {
		if got := count(t, q.store, table); got != want {
			t.Errorf("%s rows = %d, want %d", table, got, want)
		}
	}
	hook := row(t, q.store, "stop_hooks", "1 = 1")
	if hook["session_id"] != "sess-1" || hook["agent_id"] != nil || hook["prompt_id"] != "prompt-1" || hook["hook_count"] != int64(4) ||
		hook["seat_dir"] != "/tmp/demo-seat" {
		t.Errorf("stop_hooks row = %v", hook)
	}
	main := row(t, q.store, "turn_durations", "agent_id IS NULL")
	if main["prompt_id"] != "prompt-1" || main["duration_ms"] != int64(81557) {
		t.Errorf("main turn_durations row = %v, want prompt-1 and 81557 ms", main)
	}
	if sub := row(t, q.store, "turn_durations", "agent_id = 'a1'"); sub == nil || sub["duration_ms"] != int64(81557) {
		t.Errorf("sub-agent turn_durations row = %v", sub)
	}
	cost := row(t, q.store, "session_costs", "session_id = 'sess-1'")
	if cost["cost_usd"] != 59.30682170000001 || cost["wall_ms"] != int64(53062358) || cost["ts"] != q.now.UnixMilli() {
		t.Errorf("session_costs row = %v, want the last cost-state stamped now", cost)
	}
	if row(t, q.store, "compactions", "pre_tokens = 333677 AND post_tokens = 18677") == nil {
		t.Errorf("no compaction row of the real numbers")
	}

	// Read again, the same marks write nothing and the stored cost stands.
	again, err := q.store.RecoverQuiet(context.Background(), q.now.Add(time.Hour), QuietAfter)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sessions != 0 {
		t.Errorf("second RecoverQuiet = %+v, want nothing written", again)
	}
	for table, want := range map[string]int{"compactions": 1, "stop_hooks": 1, "stop_hook_runs": 4, "turn_durations": 2, "session_costs": 1} {
		if got := count(t, q.store, table); got != want {
			t.Errorf("%s rows after a second pass = %d, want %d", table, got, want)
		}
	}
	if got := row(t, q.store, "session_costs", "session_id = 'sess-1'")["ts"]; got != q.now.UnixMilli() {
		t.Errorf("session_costs.ts after a second pass = %v, want it unchanged", got)
	}
}

// TestRequestFillsAMissingIteration: recovery rereads a request whose row is
// complete but which lacks a row for an iteration its transcript carries (a
// binary before request_iterations wrote it), and writes it; once written, the
// request has nothing left to fill.
func TestRequestFillsAMissingIteration(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	r := Request{RequestID: "msg_1", SessionID: Ptr("sess-1"), TS: Ptr(int64(1788019350229)), Model: Ptr("claude-opus-4-8"),
		Pending: Ptr(false), Iterations: []Iteration{fallbackIteration()}}
	stored := r
	stored.Iterations = nil
	if err := store.Batch(ctx, func(tx *Tx) error { return tx.SettleRequest(ctx, stored, nil, 0) }); err != nil {
		t.Fatal(err)
	}
	fills, err := store.requestFills(ctx, r, nil, 0)
	if err != nil || !fills {
		t.Fatalf("requestFills without the iteration row = %v, %v; want true", fills, err)
	}
	if err := store.Batch(ctx, func(tx *Tx) error { return tx.RecoverRequest(ctx, r, nil, 0) }); err != nil {
		t.Fatal(err)
	}
	if fills, err = store.requestFills(ctx, r, nil, 0); err != nil || fills {
		t.Fatalf("requestFills with the iteration stored = %v, %v; want false", fills, err)
	}
	if got := count(t, store, "request_iterations"); got != 1 {
		t.Errorf("request_iterations holds %d rows, want 1", got)
	}
}
