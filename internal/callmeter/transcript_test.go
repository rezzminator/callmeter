package callmeter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
