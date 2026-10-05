package callmeter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReadTaskNoticesReadsOnlyIdAndStatus(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 52, 21, 292000000, time.UTC)
	attachment := taskNoticeLine("attachment", "a1", "completed", at)
	user := taskNoticeLine("user", "a1", "failed", at.Add(time.Second))
	// Real shape (a queued human prompt carrying an image): the prompt, or a
	// user message's content, is a content-block array, not a string,
	// {"type":"attachment","attachment":{"type":"queued_command","origin":{"kind":"human"},
	// "prompt":[{"type":"text","text":…},{"type":"image","source":{"type":…,"media_type":…,"data":…}}]}}.
	// A notice in that shape is read from its "text" blocks.
	asBlocks := func(line string) string {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		holder, key := entry["message"], "content"
		if entry["type"] == "attachment" {
			holder, key = entry["attachment"], "prompt"
		}
		fields, ok := holder.(map[string]any)
		if !ok {
			t.Fatalf("no %s to wrap in %s", key, line)
		}
		fields[key] = []any{
			map[string]any{"type": "text", "text": fields[key]},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}},
		}
		out, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	human := strings.Replace(asBlocks(taskNoticeLine("attachment", "h1", "completed", at)), `"kind":"task-notification"`, `"kind":"human"`, 1)
	lines := []string{
		attachment, user, taskNoticeLine("user", "a2", "killed", at.Add(2*time.Second)),
		asBlocks(taskNoticeLine("attachment", "a3", "completed", at.Add(3*time.Second))),
		asBlocks(taskNoticeLine("user", "a3", "failed", at.Add(4*time.Second))),
		human,
		// Synthetic queue-operation shape from gym S2: content is not a notice.
		`{"type":"queue-operation","content":"<task-notification><task-id>queued</task-id><status>completed</status></task-notification>","timestamp":"2026-10-01T00:52:21.292Z"}`,
		`{"type":"attachment","task-notification": broken`,
		`not JSON and no notice marker`,
		strings.Replace(user, "<task-id>a1</task-id>", "", 1),
		strings.Replace(user, "<task-id>a1</task-id>", "<task-id></task-id>", 1),
		strings.Replace(user, "</task-id>", "", 1),
		strings.Replace(user, "<status>failed</status>", "", 1),
		strings.Replace(user, "<status>failed</status>", "<status></status>", 1),
		strings.Replace(user, "</status>", "", 1),
		strings.Replace(user, stamp(at.Add(time.Second)), "invalid", 1),
		strings.Replace(user, `"kind":"task-notification"`, `"kind":"peer"`, 1),
		strings.Replace(attachment, `"type":"queued_command"`, `"type":"other"`, 1),
		strings.Replace(attachment, `"kind":"task-notification"`, `"kind":"peer"`, 1),
		// File order is kept, including a final line without a newline.
		taskNoticeLine("attachment", "a1", "killed", at.Add(-time.Second)),
	}
	path := filepath.Join(t.TempDir(), "notices.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTaskNotices(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]int64{
		"a1": {at.UnixMilli(), at.Add(time.Second).UnixMilli(), at.Add(-time.Second).UnixMilli()}, "a2": {at.Add(2 * time.Second).UnixMilli()},
		"a3": {at.Add(3 * time.Second).UnixMilli(), at.Add(4 * time.Second).UnixMilli()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadTaskNotices = %v, want %v", got, want)
	}
	if _, err := ReadTaskNotices(filepath.Join(t.TempDir(), "missing.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing transcript error = %v, want fs.ErrNotExist", err)
	}
	if _, err := ReadTaskNotices(t.TempDir()); err == nil {
		t.Fatal("reading a directory succeeded")
	}
}

func TestFindRequests(t *testing.T) {
	path := filepath.Join("testdata", "transcript.jsonl")
	got, err := FindRequests(path, []string{"toolu_demo_A", "toolu_demo_B", "toolu_demo_C", "toolu_absent"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	want := map[string]RequestUsage{
		"toolu_demo_A": {
			MessageID:           "msg_demo_1",
			TS:                  1790119005062,
			Model:               "claude-haiku-4-5-20251001",
			InputTokens:         10,
			CacheReadTokens:     13689,
			CacheCreationTokens: 10789,
			ContextTokens:       10 + 13689 + 10789,
			OutputTokens:        276,
		},
		"toolu_demo_B": {
			MessageID:           "msg_demo_1",
			TS:                  1790119005300,
			Model:               "claude-haiku-4-5-20251001",
			InputTokens:         10,
			CacheReadTokens:     13689,
			CacheCreationTokens: 10789,
			ContextTokens:       10 + 13689 + 10789,
			OutputTokens:        276,
		},
	}
	// toolu_demo_C sits on the trailing partial line and toolu_absent nowhere:
	// both are absent, neither is an error.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindRequests = %+v\nwant %+v", got, want)
	}
}

func TestFindRequestsUnreadableIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.jsonl")
	got, err := FindRequests(missing, []string{"toolu_demo_A"})
	if err == nil {
		t.Fatalf("FindRequests on a missing file = %v, nil; want an error", got)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error %q does not name %s", err, missing)
	}
}

func TestFindRequestsMalformedLineNamesIt(t *testing.T) {
	path := filepath.Join("testdata", "malformed.jsonl")
	_, err := FindRequests(path, []string{"toolu_x"})
	if err == nil {
		t.Fatal("FindRequests on a malformed middle line succeeded")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "at byte 96") {
		t.Fatalf("error %q does not name %s at byte 96, the malformed line holding toolu_x", err, path)
	}
}

func TestSubagentTranscriptPath(t *testing.T) {
	got := SubagentTranscriptPath("/tmp/demo-config/projects/-tmp-demo-proj/sess-1.jsonl", "a1b2")
	if want := "/tmp/demo-config/projects/-tmp-demo-proj/sess-1/subagents/agent-a1b2.jsonl"; got != want {
		t.Fatalf("SubagentTranscriptPath = %q, want %q", got, want)
	}
}

// bigTranscript writes head, then filler user lines past FindRequests' first
// tail window, then tail: the shape of a long chat whose newest request is at
// the end.
func bigTranscript(t *testing.T, head, tail string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(head)
	filler := `{"type":"user","message":{"role":"user","content":"` + strings.Repeat("x", 1000) +
		`"},"timestamp":"2026-09-22T23:16:40.000Z"}` + "\n"
	for b.Len() < 3<<20 {
		b.WriteString(filler)
	}
	b.WriteString(tail)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const tailRequest = `{"type":"assistant","timestamp":"2026-09-22T23:17:00.000Z","message":{"id":"msg_tail",` +
	`"content":[{"type":"tool_use","id":"toolu_tail"}],"usage":{"input_tokens":1,"cache_read_input_tokens":2,` +
	`"cache_creation_input_tokens":3,"output_tokens":4}}}` + "\n"

// TestFindRequestsReadsTheTailFirst: the hook looks up the request it just
// saw, which sits at the end of a transcript that grows to tens of MB; the
// lookup reads the tail and never parses the head, so a malformed line far
// from the wanted ids blinds nothing.
func TestFindRequestsReadsTheTailFirst(t *testing.T) {
	path := bigTranscript(t, `{"type":"assistant","message":{"id":"msg_broken"`+"\n", tailRequest)
	got, err := FindRequests(path, []string{"toolu_tail"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	want := RequestUsage{
		MessageID: "msg_tail", TS: 1790119020000, InputTokens: 1, CacheReadTokens: 2, CacheCreationTokens: 3,
		ContextTokens: 6, OutputTokens: 4,
	}
	if !reflect.DeepEqual(got["toolu_tail"], want) {
		t.Fatalf("toolu_tail = %+v, want %+v", got["toolu_tail"], want)
	}
}

// TestFindRequestsFallsBackToTheWholeFile: an id older than the tail window is
// still found.
func TestFindRequestsFallsBackToTheWholeFile(t *testing.T) {
	head := strings.NewReplacer("toolu_tail", "toolu_head", "msg_tail", "msg_head").Replace(tailRequest)
	path := bigTranscript(t, head, "")
	got, err := FindRequests(path, []string{"toolu_head", "toolu_absent"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	if got["toolu_head"].MessageID != "msg_head" || len(got) != 1 {
		t.Fatalf("FindRequests = %+v, want only toolu_head from msg_head", got)
	}
}

// usageEntry is one assistant transcript line holding one tool_use block, its
// usage object as given.
func usageEntry(message, toolUseID, stopReason, usage string) string {
	return `{"type":"assistant","timestamp":"2026-09-22T23:17:00.000Z","message":{"id":"` + message +
		`","model":"claude-sonnet-4-5","stop_reason":` + stopReason + `,"content":[{"type":"tool_use","id":"` + toolUseID +
		`"}],"usage":` + usage + `}}` + "\n"
}

// TestFindRequestsReadsTheTokenSplit: the usage of a real transcript (a
// cache_creation object with both lifetimes) yields the whole split, the model
// and the stop reason; a usage with no cache_creation object leaves the 5m and
// 1h counts nil, never zero.
func TestFindRequestsReadsTheTokenSplit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "split.jsonl")
	transcript := usageEntry("msg_split", "toolu_split", `"tool_use"`,
		`{"input_tokens":2,"cache_creation_input_tokens":5206,"cache_read_input_tokens":12015,"output_tokens":374,`+
			`"cache_creation":{"ephemeral_1h_input_tokens":5206,"ephemeral_5m_input_tokens":0}}`) +
		usageEntry("msg_flat", "toolu_flat", `null`,
			`{"input_tokens":7,"cache_creation_input_tokens":8,"cache_read_input_tokens":9,"output_tokens":1}`)
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FindRequests(path, []string{"toolu_split", "toolu_flat"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	want := map[string]RequestUsage{
		"toolu_split": {
			MessageID: "msg_split", TS: 1790119020000, Model: "claude-sonnet-4-5", StopReason: "tool_use",
			InputTokens: 2, CacheReadTokens: 12015, CacheCreationTokens: 5206,
			CacheCreation5m: Ptr(int64(0)), CacheCreation1h: Ptr(int64(5206)),
			ContextTokens: 2 + 12015 + 5206, OutputTokens: 374,
		},
		"toolu_flat": {
			MessageID: "msg_flat", TS: 1790119020000, Model: "claude-sonnet-4-5",
			InputTokens: 7, CacheReadTokens: 9, CacheCreationTokens: 8, ContextTokens: 7 + 9 + 8, OutputTokens: 1,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindRequests = %+v\nwant %+v", got, want)
	}
}

// TestFindRequestsWidensPastALargeTail: the wanted tool_use sits on the first
// line of a transcript over 8 MiB; FindRequests widens its window until it
// reaches it, and an id the file never holds is absent without an error.
func TestFindRequestsWidensPastALargeTail(t *testing.T) {
	const first = `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_large_1","type":"message",` +
		`"role":"assistant","content":[{"type":"tool_use","id":"toolu_large_first","name":"Read","input":{"file_path":"/tmp/demo-proj/a.txt"}}],` +
		`"usage":{"input_tokens":3,"cache_creation_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":11}},` +
		`"requestId":"req_large_1","timestamp":"2026-09-22T23:16:45.062Z","cwd":"/tmp/demo-proj","sessionId":"sess-large"}` + "\n"
	filler := `{"type":"user","message":{"role":"user","content":"` + strings.Repeat("x", 4000) + `"},` +
		`"timestamp":"2026-09-22T23:16:46.000Z","cwd":"/tmp/demo-proj","sessionId":"sess-large"}` + "\n"
	var transcript strings.Builder
	transcript.WriteString(first)
	for transcript.Len() <= 8<<20 {
		transcript.WriteString(filler)
	}
	if transcript.Len() <= 8*requestTailWindow {
		t.Fatalf("the transcript is %d bytes, want over 8 tail windows", transcript.Len())
	}
	path := filepath.Join(t.TempDir(), "large.jsonl")
	if err := os.WriteFile(path, []byte(transcript.String()), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	got, err := FindRequests(path, []string{"toolu_large_first", "toolu_large_absent"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	usage, ok := got["toolu_large_first"]
	if !ok || usage.MessageID != "msg_large_1" || usage.ContextTokens != 3+7+5 {
		t.Errorf("toolu_large_first = %+v (found %v), want msg_large_1 with context 15", usage, ok)
	}
	if _, ok := got["toolu_large_absent"]; ok {
		t.Error("an id the transcript never holds is in the map")
	}
}

// TranscriptAPIError reads the error kind of the `<synthetic>` message an API
// error leaves where a model answer would be, only when it is the
// transcript's last message; a non-kind error string reads as "unknown", so no
// message text ever leaves the transcript.
func TestTranscriptAPIError(t *testing.T) {
	const (
		prompt = `{"type":"user","timestamp":"2026-09-23T09:00:00.000Z","message":{"role":"user","content":"invented prompt"}}`
		answer = `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","message":{"id":"msg-demo-ok","model":"claude-demo","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"invented answer"}]}}`
		tail   = `{"type":"last-prompt","sessionId":"s-demo"}` + "\n" + `{"type":"cost-state","sessionId":"s-demo"}`
	)
	refusal := func(kind string) string {
		return `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","isApiErrorMessage":true,"error":"` + kind +
			`","message":{"id":"msg-demo-err","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"invented refusal text"}]}}`
	}
	cases := []struct {
		name, body, want string
	}{
		{"refused then bookkeeping", prompt + "\n" + refusal("oauth_org_not_allowed") + "\n" + tail + "\n", "oauth_org_not_allowed"},
		{"refused, no trailing newline", prompt + "\n" + refusal("rate_limit"), "rate_limit"},
		{"error not a kind", prompt + "\n" + refusal("Weekly Limit: invented refusal text") + "\n", "unknown"},
		{"no error field", prompt + "\n" + strings.Replace(refusal("x"), `"error":"x",`, "", 1) + "\n", "unknown"},
		{"answered", prompt + "\n" + answer + "\n" + tail + "\n", ""},
		{"refused, then answered", prompt + "\n" + refusal("rate_limit") + "\n" + prompt + "\n" + answer + "\n", ""},
		{"refused, then a new prompt", prompt + "\n" + refusal("rate_limit") + "\n" + prompt + "\n", ""},
		{"partial last line", prompt + "\n" + refusal("rate_limit") + "\n" + `{"type":"assist`, "rate_limit"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatalf("write transcript: %v", err)
			}
			got, err := TranscriptAPIError(path)
			if err != nil {
				t.Fatalf("TranscriptAPIError: %v", err)
			}
			if got != c.want {
				t.Errorf("TranscriptAPIError = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("refusal before a large tail", func(t *testing.T) {
		big := `{"type":"attachment","pad":"` + strings.Repeat("x", 2*requestTailWindow) + `"}`
		path := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(path, []byte(prompt+"\n"+refusal("rate_limit")+"\n"+big+"\n"), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		if got, err := TranscriptAPIError(path); err != nil || got != "rate_limit" {
			t.Errorf("TranscriptAPIError = %q, %v, want rate_limit", got, err)
		}
	})
	t.Run("malformed line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "t.jsonl")
		if err := os.WriteFile(path, []byte(prompt+"\n{bad\n"), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		if _, err := TranscriptAPIError(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("TranscriptAPIError err = %v, want one naming %s", err, path)
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		if _, err := TranscriptAPIError(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
			t.Error("TranscriptAPIError on an absent file returned no error")
		}
	})
}

// multiBlockMessage is one model message as Claude Code writes it: one entry
// per content block, all sharing the id, the usage repeated as it stood when
// the block was written; only the last entry carries the final output_tokens
// and stop_reason.
func multiBlockMessage(id string, toolUseIDs ...string) string {
	usage := func(output int) string {
		return fmt.Sprintf(`{"input_tokens":3,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,"output_tokens":%d}`, output)
	}
	text := `{"type":"assistant","timestamp":"2026-09-23T01:00:00.000Z","message":{"id":"` + id +
		`","model":"claude-opus-4-1","stop_reason":null,"content":[{"type":"thinking","thinking":"invented"}],"usage":` + usage(8) + `}}` + "\n"
	for i, toolUseID := range toolUseIDs {
		stop, output := `null`, 8
		if i == len(toolUseIDs)-1 {
			stop, output = `"tool_use"`, 643
		}
		text += fmt.Sprintf(`{"type":"assistant","timestamp":"2026-09-23T01:00:0%d.000Z","message":{"id":"%s","model":"claude-opus-4-1",`+
			`"stop_reason":%s,"content":[{"type":"tool_use","id":"%s","name":"Bash","input":{}}],"usage":%s}}`+"\n",
			i+1, id, stop, toolUseID, usage(output))
		text += `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + toolUseID + `","content":"ok"}]}}` + "\n"
	}
	return text
}

// TestFindRequestsTakesTheMessagesFinalEntry: a message of several tool_use
// blocks is several entries; the request of every one of its calls is the
// message's final usage and stop reason, never the partial usage of the entry
// naming the call (the store kept output 8 against 643 and no stop reason).
func TestFindRequestsTakesTheMessagesFinalEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocks.jsonl")
	if err := os.WriteFile(path, []byte(multiBlockMessage("msg_blocks", "toolu_first", "toolu_second", "toolu_last")), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FindRequests(path, []string{"toolu_first", "toolu_second"})
	if err != nil {
		t.Fatalf("FindRequests: %v", err)
	}
	for _, id := range []string{"toolu_first", "toolu_second"} {
		r := got[id]
		if r.MessageID != "msg_blocks" || r.OutputTokens != 643 || r.StopReason != "tool_use" || r.ContextTokens != 48 {
			t.Errorf("request of %s = %+v, want msg_blocks with output 643, stop tool_use, context 48", id, r)
		}
	}
}

// TestReadRequestsReadsEveryRequestAtItsFinalUsage: ReadRequests returns each
// message once, in order, at its last entry's usage and stop reason, a reply
// with no tool call included, with the prompt it answered; the synthetic
// message and a message with no usage are not requests; final follows the last
// assistant entry.
func TestReadRequestsReadsEveryRequestAtItsFinalUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.jsonl")
	reply := func(stop string, output int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-09-23T01:00:09.000Z","message":{"id":"msg_reply","model":"claude-opus-4-1",`+
			`"stop_reason":%s,"content":[{"type":"text","text":"invented reply"}],"usage":{"input_tokens":1,"cache_read_input_tokens":90,`+
			`"cache_creation_input_tokens":2,"output_tokens":%d}}}`+"\n", stop, output)
	}
	transcript := `{"type":"user","promptId":"prompt-invented-1","message":{"role":"user","content":"invented"}}` + "\n" +
		multiBlockMessage("msg_blocks", "toolu_first", "toolu_last") +
		`{"type":"assistant","timestamp":"2026-09-23T01:00:08.000Z","message":{"id":"msg_nousage","model":"claude-opus-4-1","content":[]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-09-23T01:00:08.500Z","message":{"id":"msg_synthetic","model":"<synthetic>",` +
		`"stop_reason":"stop_sequence","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n" +
		reply(`null`, 4)
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, final, err := ReadRequests(path)
	if err != nil {
		t.Fatalf("ReadRequests: %v", err)
	}
	if final {
		t.Errorf("final = true before the reply's last entry")
	}
	if err := os.WriteFile(path, []byte(transcript+reply(`"end_turn"`, 212)), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, final, err = ReadRequests(path)
	if err != nil {
		t.Fatalf("ReadRequests: %v", err)
	}
	if !final {
		t.Errorf("final = false after an end_turn entry")
	}
	want := []TranscriptRequest{
		{
			RequestUsage: RequestUsage{
				MessageID: "msg_blocks", TS: 1790125200000, Model: "claude-opus-4-1", StopReason: "tool_use",
				InputTokens: 3, CacheReadTokens: 40, CacheCreationTokens: 5, ContextTokens: 48, OutputTokens: 643,
			},
			PromptID: "prompt-invented-1", ToolUseIDs: []string{"toolu_first", "toolu_last"},
			ToolUses: []ToolUse{{ID: "toolu_first", Name: "Bash", Input: json.RawMessage(`{}`)}, {ID: "toolu_last", Name: "Bash", Input: json.RawMessage(`{}`)}},
		},
		{
			RequestUsage: RequestUsage{
				MessageID: "msg_reply", TS: 1790125209000, Model: "claude-opus-4-1", StopReason: "end_turn",
				InputTokens: 1, CacheReadTokens: 90, CacheCreationTokens: 2, ContextTokens: 93, OutputTokens: 212,
			},
			PromptID: "prompt-invented-1", EndTS: 1790125209000,
		},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("ReadRequests = %+v\nwant %+v", requests, want)
	}
}

// TestTranscriptTimestampErrorStatesOnlyItsSize: an assistant entry whose
// timestamp does not parse is an error naming the message id and the
// timestamp's size, never the value, which may be any text.
func TestTranscriptTimestampErrorStatesOnlyItsSize(t *testing.T) {
	const private = "SENTINEL private words"
	path := filepath.Join(t.TempDir(), "main.jsonl")
	entry := `{"type":"assistant","timestamp":"` + private + `","message":{"id":"msg_badtime","model":"claude-opus-4-1",` +
		`"content":[{"type":"tool_use","id":"toolu_badtime","name":"Bash","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	_, findErr := FindRequests(path, []string{"toolu_badtime"})
	_, _, readErr := ReadRequests(path)
	want := fmt.Sprintf(`assistant message "msg_badtime": timestamp of %d bytes is not RFC 3339`, len(private))
	for name, err := range map[string]error{"FindRequests": findErr, "ReadRequests": readErr} {
		if err == nil {
			t.Errorf("%s on an unparsable timestamp succeeded", name)
			continue
		}
		if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), private) {
			t.Errorf("%s error = %q, want it to hold %q and never the timestamp", name, err, want)
		}
	}
}

// TranscriptTurnEnd is how the transcript ends its latest turn and the
// entry's own timestamp in Unix ms, which a rebuilt turn end is stamped with: a
// StopFailure for an API-error entry, a Stop for an answer ending the turn,
// none for a turn still awaiting its answer or interrupted; 0 when there is no
// turn end or the entry's timestamp is no RFC 3339.
func TestTranscriptTurnEnd(t *testing.T) {
	const (
		prompt  = `{"type":"user","timestamp":"2026-09-23T09:00:00.000Z","message":{"role":"user","content":"invented prompt"}}`
		answer  = `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","message":{"id":"msg-demo-ok","model":"claude-demo","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"invented answer"}]}}`
		tool    = `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","message":{"id":"msg-demo-tool","model":"claude-demo","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_demo","name":"Bash","input":{}}]}}`
		result  = `{"type":"user","timestamp":"2026-09-23T09:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_demo","content":"invented output"}]}}`
		halted  = `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","message":{"id":"msg-demo-int","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"invented interruption"}]}}`
		summary = `{"type":"system","subtype":"stop_hook_summary","timestamp":"2026-09-23T09:00:03.000Z"}`
	)
	refusal := func(stamp string) string {
		return `{"type":"assistant",` + stamp + `"isApiErrorMessage":true,"error":"model_not_found",` +
			`"message":{"id":"msg-demo-err","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"invented refusal text"}]}}`
	}
	cases := []struct {
		name, body, event, kind string
		ts                      int64
	}{
		{"timestamped", prompt + "\n" + refusal(`"timestamp":"2026-09-23T09:00:01.250Z",`) + "\n", EventStopFailure, "model_not_found", 1790154001250},
		{"no timestamp", prompt + "\n" + refusal("") + "\n", EventStopFailure, "model_not_found", 0},
		{"unparsable timestamp", prompt + "\n" + refusal(`"timestamp":"yesterday",`) + "\n", EventStopFailure, "model_not_found", 0},
		{"answered", prompt + "\n" + answer + "\n", EventStop, "", 1790154001000},
		{"answered, then bookkeeping", prompt + "\n" + answer + "\n" + summary + "\n", EventStop, "", 1790154001000},
		{"tool call awaiting its result", prompt + "\n" + tool + "\n", "", "", 0},
		{"tool result awaiting the answer", prompt + "\n" + tool + "\n" + result + "\n", "", "", 0},
		{"interrupted", prompt + "\n" + halted + "\n", "", "", 0},
		{"prompt awaiting the answer", answer + "\n" + prompt + "\n", "", "", 0},
		{"empty", "", "", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "t.jsonl")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatalf("write transcript: %v", err)
			}
			end, err := TranscriptTurnEnd(path)
			if err != nil {
				t.Fatalf("TranscriptTurnEnd: %v", err)
			}
			if want := (TurnEnd{Event: c.event, ErrorType: c.kind, TS: c.ts}); end != want {
				t.Errorf("TranscriptTurnEnd = %+v, want %+v", end, want)
			}
		})
	}
	if _, err := TranscriptTurnEnd(filepath.Join(t.TempDir(), "absent.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("TranscriptTurnEnd on a missing transcript = %v, want an error wrapping fs.ErrNotExist", err)
	}
}

// marksFixture is real Claude Code 2.1.289 lines (sanitized): a prompt, two
// replies carrying output_tokens_details, a stop_hook_summary and the
// turn_duration after it, a second prompt answered by a reply of an older
// version with no details, a compact_boundary and two cost-state lines.
const marksFixture = "testdata/transcript-marks.jsonl"

const (
	marksPrompt1 = "af8da24f-ac08-45b1-9c9e-b2015b11f360"
	marksPrompt2 = "11111111-2222-4333-8444-555555555555"
)

func readMarksFixture(t *testing.T) TranscriptRead {
	t.Helper()
	read, err := ReadTranscript(marksFixture, "")
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	return read
}

// writeMarksVariant writes the fixture with each of its replacements applied
// (a value changed in place, never a shape made up) and reads it.
func readMarksVariant(t *testing.T, replacements ...string) (TranscriptRead, error) {
	t.Helper()
	raw, err := os.ReadFile(marksFixture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for i := 0; i < len(replacements); i += 2 {
		if !strings.Contains(text, replacements[i]) {
			t.Fatalf("fixture lacks %q", replacements[i])
		}
		text = strings.Replace(text, replacements[i], replacements[i+1], 1)
	}
	path := filepath.Join(t.TempDir(), "main.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return ReadTranscript(path, "")
}

func TestReadTranscriptThinkingTokens(t *testing.T) {
	read := readMarksFixture(t)
	got := map[string]*int64{}
	for _, request := range read.Requests {
		got[request.MessageID] = request.ThinkingTokens
	}
	if len(got) != 3 {
		t.Fatalf("requests = %v, want 3", got)
	}
	if v := got["msg_011Cfg5BFXP8ktxFkALGfUL3"]; v == nil || *v != 118 {
		t.Errorf("thinking of the reply with 118 = %v, want 118", v)
	}
	if v := got["msg_011Cfg5GzuUN3vDHbBc86k21"]; v == nil || *v != 0 {
		t.Errorf("thinking of the reply with an explicit 0 = %v, want 0", v)
	}
	if v, ok := got["msg_01ApKzvSCV54JEQyKJBGg11V"]; !ok || v != nil {
		t.Errorf("thinking of the reply with no output_tokens_details = %v, want nil (a missing count is never zero)", v)
	}
	var usage RequestUsage
	for _, request := range read.Requests {
		if request.ThinkingTokens != nil && *request.ThinkingTokens == 118 {
			usage = request.RequestUsage
		}
	}
	var request Request
	ApplyUsage(&request, usage)
	if request.ThinkingTokens == nil || *request.ThinkingTokens != 118 {
		t.Errorf("ApplyUsage thinking = %v, want 118", request.ThinkingTokens)
	}
}

func TestReadTranscriptThinkingTokensLastEntryWins(t *testing.T) {
	entry := func(output, thinking int) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-09-23T01:00:09.000Z","message":{"id":"msg_one","model":"m",`+
			`"content":[],"usage":{"input_tokens":1,"output_tokens":%d,"output_tokens_details":{"thinking_tokens":%d}}}}`+"\n", output, thinking)
	}
	path := filepath.Join(t.TempDir(), "main.jsonl")
	if err := os.WriteFile(path, []byte(entry(10, 3)+entry(40, 25)), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadTranscript(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Requests) != 1 || read.Requests[0].ThinkingTokens == nil || *read.Requests[0].ThinkingTokens != 25 {
		t.Fatalf("requests = %+v, want one at thinking 25", read.Requests)
	}
}

func TestReadTranscriptCompaction(t *testing.T) {
	marks := readMarksFixture(t).Marks
	want := []Compaction{{
		EntryID: "4c8d0498-fbb2-45a0-b6ac-7279953c4fd7", TS: 1791113158553, Trigger: "auto",
		PreTokens: Ptr(int64(333677)), PostTokens: Ptr(int64(18677)), CumulativeDroppedTokens: Ptr(int64(315000)), DurationMS: Ptr(int64(72119)),
	}}
	if !reflect.DeepEqual(marks.Compactions, want) {
		t.Fatalf("Compactions = %+v\nwant %+v", marks.Compactions, want)
	}
	read, err := readMarksVariant(t, `"trigger":"auto"`, `"trigger":"Not A Label"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Marks.Compactions[0].Trigger; got != "" {
		t.Errorf("trigger of the wrong shape = %q, want it dropped", got)
	}
}

func TestReadTranscriptCostStateTheLastWins(t *testing.T) {
	cost := readMarksFixture(t).Marks.Cost
	want := &CostState{
		SessionID: "sess-demo", CostUSD: Ptr(59.30682170000001), APIMS: Ptr(int64(9190688)), APINoRetryMS: Ptr(int64(9181361)),
		ToolMS: Ptr(int64(3682088)), WallMS: Ptr(int64(53062358)), Started: Ptr(int64(1791065248155)),
		ModelCosts: map[string]float64{"claude-opus-5-5": 45.53485040000004, "claude-sonnet-5-5": 13.771971300000002},
	}
	if !reflect.DeepEqual(cost, want) {
		t.Fatalf("Cost = %+v\nwant %+v", cost, want)
	}
	read, err := readMarksVariant(t, `"claude-sonnet-5-5":{`, `"not a model key!":{`,
		`"claude-sonnet-5-5":{`, `"not a model key!":{`)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Marks.Cost.ModelCosts; len(got) != 1 || got["claude-opus-5-5"] != 45.53485040000004 {
		t.Errorf("model costs with a key of the wrong shape = %v, want only the opus key", got)
	}
}

func TestReadTranscriptStopHooks(t *testing.T) {
	marks := readMarksFixture(t).Marks
	if len(marks.StopHooks) != 1 {
		t.Fatalf("StopHooks = %+v, want 1", marks.StopHooks)
	}
	hook := marks.StopHooks[0]
	if hook.EntryID != "8b7a2d0e-b849-4d10-a191-b1cd766d87f3" || hook.PromptID != marksPrompt1 || hook.TS != 1791065682790 ||
		hook.HookCount != 4 || hook.HookErrors != 0 {
		t.Errorf("stop hook = %+v", hook)
	}
	type run struct {
		name     string
		bytes    int64
		duration *int64
	}
	var got []run
	for _, h := range hook.Hooks {
		got = append(got, run{h.Name, h.CommandBytes, h.DurationMS})
	}
	want := []run{
		{"notify.sh", int64(len("$CLAUDE_PROJECT_DIR/.claude/scripts/notify.sh stop")), Ptr(int64(139))},
		{"guard-stamp.sh", int64(len("$CLAUDE_PROJECT_DIR/.claude/scripts/guard-stamp.sh stop")), Ptr(int64(66))},
		{"codex-sync.sh", int64(len("$CLAUDE_PROJECT_DIR/.claude/scripts/codex-sync.sh sync")), Ptr(int64(30))},
		{"callmeter", int64(len("${CLAUDE_PLUGIN_ROOT}/libexec/callmeter hook")), nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hook runs = %+v\nwant %+v", got, want)
	}
	read, err := readMarksVariant(t, `"hookErrors":[]`, `"hookErrors":["placeholder","placeholder"]`)
	if err != nil {
		t.Fatal(err)
	}
	if n := read.Marks.StopHooks[0].HookErrors; n != 2 {
		t.Errorf("HookErrors with two listed = %d, want 2", n)
	}
}

func TestReadTranscriptTurnDurationTakesThePrecedingPrompt(t *testing.T) {
	marks := readMarksFixture(t).Marks
	want := []TurnDuration{{
		EntryID: "463cc8cc-6d27-4c84-b49d-06fa061a443b", TS: 1791065682814, PromptID: marksPrompt1,
		DurationMS: Ptr(int64(81557)), MessageCount: Ptr(int64(60)), BackgroundAgents: Ptr(int64(4)),
	}}
	if !reflect.DeepEqual(marks.TurnDurations, want) {
		t.Fatalf("TurnDurations = %+v\nwant %+v", marks.TurnDurations, want)
	}
}

func TestReadTranscriptMarkLineRules(t *testing.T) {
	// An entry with no uuid is no mark.
	read, err := readMarksVariant(t, `"uuid":"4c8d0498-fbb2-45a0-b6ac-7279953c4fd7"`, `"nouuid":"x"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Marks.Compactions) != 0 {
		t.Errorf("a compact_boundary with no uuid gave %+v", read.Marks.Compactions)
	}
	raw, err := os.ReadFile(marksFixture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(raw), "\n")
	var turn string
	for _, line := range lines {
		if strings.Contains(line, `"turn_duration"`) {
			turn = line
		}
	}
	cutTurn := turn[:strings.Index(turn, "turn_duration")+20] // names the mark, ends inside it
	dir := t.TempDir()
	// A partial last mark line is a writer mid-append: skipped.
	partial := filepath.Join(dir, "partial.jsonl")
	if err := os.WriteFile(partial, []byte(strings.Join(lines, "")+cutTurn), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTranscript(partial, ""); err != nil {
		t.Errorf("a partial last mark line: %v, want it skipped", err)
	}
	// Any other malformed mark line is an error naming the path.
	malformed := filepath.Join(dir, "malformed.jsonl")
	if err := os.WriteFile(malformed, []byte(cutTurn+"\n"+strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTranscript(malformed, ""); err == nil || !strings.Contains(err.Error(), malformed) {
		t.Errorf("a malformed mark line: %v, want an error naming %s", err, malformed)
	}
}

func TestHookName(t *testing.T) {
	sum := func(command string) string {
		digest := sha256.Sum256([]byte(command))
		return "#" + hex.EncodeToString(digest[:])[:8]
	}
	for command, want := range map[string]string{
		"$CLAUDE_PROJECT_DIR/.claude/scripts/notify.sh stop":      "notify.sh",
		"${CLAUDE_PLUGIN_ROOT}/libexec/callmeter hook":            "callmeter",
		"/usr/local/bin/tool":                                     "tool",
		"CALLMETER_HOME=/tmp/x FOO=1 ${CLAUDE_PLUGIN_ROOT}/bin/x": "x",
		`"/opt/hooks/stop.sh" --flag`:                             "stop.sh",
		"bash /opt/hooks/after-turn.sh":                           "after-turn.sh",
		"sh -e /opt/hooks/after-turn.sh arg":                      "after-turn.sh",
		"python3 -u /opt/hooks/summarize.py":                      "summarize.py",
		"uv run scripts/gate.py":                                  "gate.py",
		"node":                                                    "node",
		"":                                                        "#e3b0c442",
		"$(curl evil)":                                            sum("$(curl evil)"),
		"/opt/hooks/wé.sh":                                        sum("/opt/hooks/wé.sh"),
		"./" + strings.Repeat("a", 65):                            sum("./" + strings.Repeat("a", 65)),
		"CALLMETER_HOME=/tmp/x":                                   sum("CALLMETER_HOME=/tmp/x"),
	} {
		if got := HookName(command); got != want {
			t.Errorf("HookName(%q) = %q, want %q", command, got, want)
		}
	}
}
