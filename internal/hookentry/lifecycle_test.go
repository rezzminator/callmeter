package hookentry

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// dropKey as a field value removes that key from the payload a helper builds:
// the captured line carries keys (an effort, a prompt_id, a permission_mode) a
// test's input must not have.
type dropKeyMarker struct{}

var dropKey = dropKeyMarker{}

// captureKey names a captured line by its event and, for a tool event, its tool.
type captureKey struct{ event, tool string }

// captureIndex is the first line of each kind among the committed captures,
// kept as raw JSON so every use decodes its own copy.
type captureIndex struct {
	mainTool  map[captureKey]string // (event, tool) -> first main-chat line
	mainEvent map[string]string     // event -> first main-chat line, any tool
	anyEvent  map[string]string     // event -> first line of any agent, for the events only a sub-agent sends
	searched  []string              // the files read, in search order
}

// derivedEvents are the events no capture holds: each starts from the captured
// line of another event, drops that event's own keys and takes the event name.
var derivedEvents = map[string]struct {
	from string
	drop []string
}{
	"Notification":     {from: "Stop", drop: []string{"background_tasks", "last_assistant_message", "session_crons", "stop_hook_active"}},
	"PermissionDenied": {from: "PermissionRequest", drop: []string{"permission_suggestions"}},
}

var captures struct {
	sync.Mutex
	index *captureIndex // nil until a load succeeds, so a failed load is retried and fails again
}

func newCaptureIndex() *captureIndex {
	return &captureIndex{mainTool: map[captureKey]string{}, mainEvent: map[string]string{}, anyEvent: map[string]string{}}
}

// add reads the lines of one capture file, in order: the first line of each
// kind wins, and a sub-agent's line (one with an agent_id) never counts as a
// main-chat one.
func (index *captureIndex) add(t *testing.T, source string, lines []string) {
	t.Helper()
	index.searched = append(index.searched, source)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("decode a capture line of %s: %v", source, err)
		}
		event, _ := fields["hook_event_name"].(string)
		tool, _ := fields["tool_name"].(string)
		if _, ok := index.anyEvent[event]; !ok {
			index.anyEvent[event] = line
		}
		if _, subagent := fields["agent_id"]; subagent {
			continue
		}
		if _, ok := index.mainEvent[event]; !ok {
			index.mainEvent[event] = line
		}
		if _, ok := index.mainTool[captureKey{event, tool}]; !ok {
			index.mainTool[captureKey{event, tool}] = line
		}
	}
}

// capturedIndex is the capture index, loaded once per test binary from, in
// order, testdata/verify, testdata/gym/{S1,S1b,S2,S3,S4} and
// testdata/callmeter/*.jsonl; the first line of a kind wins.
func capturedIndex(t *testing.T) *captureIndex {
	t.Helper()
	captures.Lock()
	defer captures.Unlock()
	if captures.index != nil {
		return captures.index
	}
	index := newCaptureIndex()
	add := func(source string, lines []string) { index.add(t, source, lines) }
	add(filepath.Join("testdata", "verify", "payloads.jsonl"), fixturePayloads(t, "verify"))
	for _, session := range gymSessions {
		add(filepath.Join("testdata", "gym", session, "payloads.jsonl"), fixturePayloads(t, "gym/"+session))
	}
	files, err := filepath.Glob(filepath.Join("testdata", "callmeter", "*.jsonl"))
	if err != nil {
		t.Fatalf("list the callmeter captures: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read capture %s: %v", file, err)
		}
		add(file, strings.Split(string(data), "\n"))
	}
	captures.index = index
	return index
}

// capturedLine is a fresh decoded copy of the capture a helper starts from,
// by lookupCapture's rule; no capture at all fails the test.
func capturedLine(t *testing.T, event, tool string) map[string]any {
	t.Helper()
	line, err := lookupCapture(capturedIndex(t), event, tool)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// lookupCapture is the first main-chat line of event with tool_name tool (tool
// "" or no such pair: the event's first main-chat line), else the event's
// first line of any agent, else, for a derivedEvents event, the line of the
// event it derives from. No capture at all is an error naming the event and
// the files searched.
func lookupCapture(index *captureIndex, event, tool string) (map[string]any, error) {
	raw, ok := "", false
	if tool != "" {
		raw, ok = index.mainTool[captureKey{event, tool}]
	}
	if !ok {
		raw, ok = index.mainEvent[event]
	}
	if !ok {
		raw, ok = index.anyEvent[event]
	}
	if !ok {
		derived, isDerived := derivedEvents[event]
		if !isDerived {
			return nil, fmt.Errorf("no captured %s payload to start from (tool %q); searched %s", event, tool, strings.Join(index.searched, ", "))
		}
		line, err := lookupCapture(index, derived.from, "")
		if err != nil {
			return nil, fmt.Errorf("%s derives from %s: %w", event, derived.from, err)
		}
		line["hook_event_name"] = event
		for _, key := range derived.drop {
			delete(line, key)
		}
		return line, nil
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		return nil, fmt.Errorf("decode the captured %s payload: %w", event, err)
	}
	return line, nil
}

// setSession points a payload at session A under the neutral roots.
func setSession(payload map[string]any) {
	payload["session_id"] = cmSessionA
	payload["cwd"] = cmDemoProj
	payload["transcript_path"] = cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl"
}

// applyFields overrides payload's keys with fields; a field valued dropKey
// removes its key.
func applyFields(payload, fields map[string]any) {
	for key, value := range fields {
		if _, remove := value.(dropKeyMarker); remove {
			delete(payload, key)
			continue
		}
		payload[key] = value
	}
}

// hookPayload is a payload of event in session A: the first main-chat line of
// event among the captures (see capturedIndex; Notification and
// PermissionDenied derive from Stop and PermissionRequest), every captured key
// kept, session A's session_id, cwd and transcript_path set, then fields
// applied, a field valued dropKey removing its key.
func hookPayload(t *testing.T, event string, fields map[string]any) string {
	t.Helper()
	payload := capturedLine(t, event, "")
	setSession(payload)
	applyFields(payload, fields)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode %s payload: %v", event, err)
	}
	return string(encoded)
}

// TestHookPayloadStartsFromACapture: hookPayload starts from the first
// main-chat capture of its event, keeping every captured key; fields override
// and dropKey removes.
func TestHookPayloadStartsFromACapture(t *testing.T) {
	t.Run("every captured key is kept", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "StopFailure", nil))
		for _, key := range []string{"effort", "error", "last_assistant_message", "prompt_id"} {
			if _, ok := got[key]; !ok {
				t.Errorf("StopFailure payload lacks the captured key %q: %v", key, got)
			}
		}
		if got["session_id"] != cmSessionA || got["cwd"] != cmDemoProj || got["hook_event_name"] != "StopFailure" {
			t.Errorf("session keys = %v, %v, %v; want session A, %s, StopFailure", got["session_id"], got["cwd"], got["hook_event_name"], cmDemoProj)
		}
		if want := cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl"; got["transcript_path"] != want {
			t.Errorf("transcript_path = %v, want %s", got["transcript_path"], want)
		}
	})
	t.Run("fields are applied over the capture", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "StopFailure", map[string]any{"error": "mine", "added": 1.0, "cwd": "/elsewhere"}))
		if got["error"] != "mine" || got["added"] != 1.0 || got["cwd"] != "/elsewhere" {
			t.Errorf("error, added, cwd = %v, %v, %v; want mine, 1, /elsewhere", got["error"], got["added"], got["cwd"])
		}
	})
	t.Run("a field valued dropKey leaves its key out", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "StopFailure", map[string]any{"effort": dropKey, "error": "mine"}))
		if _, ok := got["effort"]; ok {
			t.Errorf("effort is present after dropKey: %v", got)
		}
		if got["error"] != "mine" || got["prompt_id"] == nil {
			t.Errorf("the other keys changed: %v", got)
		}
	})
	t.Run("a main-chat capture carries no agent_id", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "PostToolUse", nil))
		for _, key := range []string{"agent_id", "agent_type"} {
			if _, ok := got[key]; ok {
				t.Errorf("a PostToolUse payload carries %q: %v", key, got)
			}
		}
	})
	t.Run("an event only a sub-agent sends starts from its capture", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "SubagentStop", nil))
		if got["agent_id"] == nil || got["agent_transcript_path"] == nil {
			t.Errorf("SubagentStop payload lacks the captured agent_id or agent_transcript_path: %v", got)
		}
	})
	t.Run("Notification starts from the Stop capture without Stop's own keys", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "Notification", map[string]any{"message": "m"}))
		if got["hook_event_name"] != "Notification" || got["message"] != "m" {
			t.Errorf("hook_event_name, message = %v, %v; want Notification, m", got["hook_event_name"], got["message"])
		}
		for _, key := range []string{"background_tasks", "last_assistant_message", "session_crons", "stop_hook_active"} {
			if _, ok := got[key]; ok {
				t.Errorf("a Notification payload carries Stop's key %q: %v", key, got)
			}
		}
		if got["permission_mode"] == nil || got["prompt_id"] == nil {
			t.Errorf("a Notification payload lacks the captured permission_mode or prompt_id: %v", got)
		}
	})
	t.Run("PermissionDenied starts from the PermissionRequest capture without its suggestions", func(t *testing.T) {
		got := decoded(t, hookPayload(t, "PermissionDenied", map[string]any{"tool_use_id": "toolu_denied"}))
		if got["hook_event_name"] != "PermissionDenied" || got["tool_use_id"] != "toolu_denied" {
			t.Errorf("hook_event_name, tool_use_id = %v, %v; want PermissionDenied, toolu_denied", got["hook_event_name"], got["tool_use_id"])
		}
		if _, ok := got["permission_suggestions"]; ok {
			t.Errorf("a PermissionDenied payload carries permission_suggestions: %v", got)
		}
		if got["tool_name"] == nil || got["tool_input"] == nil || got["prompt_id"] == nil {
			t.Errorf("a PermissionDenied payload lacks the captured tool_name, tool_input or prompt_id: %v", got)
		}
	})
}

// TestLookupCapturePrefersTheMainChat: a sub-agent's line ahead of the main
// chat's in the search order never becomes the starting capture; an event only
// a sub-agent sends does. The lines are the real capture's PostToolUse Read
// and SubagentStop with and without an agent_id.
func TestLookupCapturePrefersTheMainChat(t *testing.T) {
	main := capturedLine(t, "PostToolUse", "Read")
	subagent := capturedLine(t, "PostToolUse", "Read")
	subagent["agent_id"], subagent["agent_type"] = "asub01", "Explore"
	stop := capturedLine(t, "SubagentStop", "")
	encode := func(line map[string]any) string {
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatalf("encode a capture line: %v", err)
		}
		return string(encoded)
	}
	index := newCaptureIndex()
	index.add(t, "synthetic", []string{encode(subagent), encode(main), encode(stop)})
	for _, tool := range []string{"Read", ""} {
		got, err := lookupCapture(index, "PostToolUse", tool)
		if err != nil {
			t.Fatalf("lookupCapture PostToolUse %q: %v", tool, err)
		}
		if _, ok := got["agent_id"]; ok {
			t.Errorf("lookupCapture PostToolUse %q returned the sub-agent's line: %v", tool, got)
		}
	}
	got, err := lookupCapture(index, "SubagentStop", "")
	if err != nil {
		t.Fatalf("lookupCapture SubagentStop: %v", err)
	}
	if got["agent_id"] == nil {
		t.Errorf("lookupCapture SubagentStop lost its agent_id: %v", got)
	}
}

// TestLookupCaptureWithoutACapture: an event no capture holds and no
// derivation covers is an error naming the event and every file searched, and
// the search order is verify, the gym sessions, then the callmeter captures.
func TestLookupCaptureWithoutACapture(t *testing.T) {
	index := capturedIndex(t)
	_, err := lookupCapture(index, "NoSuchEvent", "")
	if err == nil {
		t.Fatal("lookupCapture of an uncaptured event returned no error")
	}
	if !strings.Contains(err.Error(), "NoSuchEvent") {
		t.Errorf("the error does not name the event: %v", err)
	}
	for _, file := range index.searched {
		if !strings.Contains(err.Error(), file) {
			t.Errorf("the error does not name the searched file %s: %v", file, err)
		}
	}
	want := []string{filepath.Join("testdata", "verify", "payloads.jsonl")}
	for _, session := range gymSessions {
		want = append(want, filepath.Join("testdata", "gym", session, "payloads.jsonl"))
	}
	if len(index.searched) <= len(want) || !slices.Equal(index.searched[:len(want)], want) {
		t.Errorf("searched = %v, want %v then the callmeter captures", index.searched, want)
	}
	for _, file := range index.searched[len(want):] {
		if filepath.Dir(file) != filepath.Join("testdata", "callmeter") {
			t.Errorf("searched %s after the gym sessions, want a testdata/callmeter capture", file)
		}
	}
}

// lifecyclePayloads is one payload per recorded non-tool event, each
// free-text field carrying text; the keys are the verify capture's.
func lifecyclePayloads(t *testing.T, text string) map[string]string {
	t.Helper()
	return map[string]string{
		"SessionStart": hookPayload(t, "SessionStart", map[string]any{"source": "startup"}),
		"Setup":        hookPayload(t, "Setup", map[string]any{"trigger": "init"}),
		"UserPromptSubmit": hookPayload(t, "UserPromptSubmit", map[string]any{
			"permission_mode": "default", "prompt": text, "prompt_id": "p-1",
		}),
		"UserPromptExpansion": hookPayload(t, "UserPromptExpansion", map[string]any{
			"command_args": text, "command_name": "x:hello", "command_source": "plugin", "expansion_type": "slash_command",
			"permission_mode": "default", "prompt": text, "prompt_id": "p-1",
		}),
		"PermissionRequest": hookPayload(t, "PermissionRequest", map[string]any{
			"permission_mode": "default", "prompt_id": "p-1", "tool_name": "Bash", "tool_input": map[string]any{"command": text},
			"permission_suggestions": []any{map[string]any{
				"type": "addRules", "rules": []any{map[string]any{"toolName": "Bash", "ruleContent": text}},
				"behavior": "allow", "destination": "session",
			}},
		}),
		"TaskCreated": hookPayload(t, "TaskCreated", map[string]any{
			"prompt_id": "p-1", "task_description": text, "task_id": "t-1", "task_subject": text,
		}),
		"TaskCompleted": hookPayload(t, "TaskCompleted", map[string]any{
			"prompt_id": "p-1", "task_description": text, "task_id": "t-1", "task_subject": text,
		}),
		"Stop": hookPayload(t, "Stop", map[string]any{
			"background_tasks":       []any{map[string]any{"id": "a1", "type": "subagent", "status": "running", "description": text}},
			"last_assistant_message": text, "permission_mode": "default", "prompt_id": "p-1",
			"session_crons": []any{map[string]any{"id": "c1", "cron": "*/5 * * * *", "prompt": text}}, "stop_hook_active": false,
		}),
		"SubagentStop": hookPayload(t, "SubagentStop", map[string]any{
			"agent_id": "acompact01", "agent_transcript_path": cmDemoHome + "/absent/agent-acompact01.jsonl", "agent_type": "",
			"background_tasks": []any{}, "last_assistant_message": text, "permission_mode": "default", "prompt_id": "p-1",
			"session_crons": []any{}, "stop_hook_active": false,
		}),
		"StopFailure": hookPayload(t, "StopFailure", map[string]any{
			"effort": map[string]any{"level": "low"}, "error": "model_not_found", "last_assistant_message": text, "prompt_id": "p-1",
		}),
		"InstructionsLoaded": hookPayload(t, "InstructionsLoaded", map[string]any{
			"file_path": cmDemoProj + "/AGENTS.md", "load_reason": "session_start", "memory_type": "Project",
		}),
		"PreCompact":  hookPayload(t, "PreCompact", map[string]any{"custom_instructions": text, "prompt_id": "p-1", "trigger": "manual"}),
		"PostCompact": hookPayload(t, "PostCompact", map[string]any{"compact_summary": text, "prompt_id": "p-1", "trigger": "manual"}),
		"SessionEnd":  hookPayload(t, "SessionEnd", map[string]any{"prompt_id": "p-1", "reason": "other"}),
		"Notification": hookPayload(t, "Notification", map[string]any{
			"message": text, "title": text, "notification_type": "idle_prompt",
		}),
		"PermissionDenied": hookPayload(t, "PermissionDenied", map[string]any{
			"tool_name": "Bash", "tool_input": map[string]any{"command": text}, "tool_use_id": "toolu_denied",
		}),
	}
}

// event is the events row of the one event of its name.
func (lab *callmeterLab) event(name string) map[string]string {
	lab.t.Helper()
	return lab.row("SELECT * FROM events WHERE event = ?", name)
}

// detail decodes an events row's detail.
func detailOf(t *testing.T, row map[string]string) map[string]any {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal([]byte(row["detail"]), &detail); err != nil {
		t.Fatalf("decode detail %q: %v", row["detail"], err)
	}
	return detail
}

func TestLifecycleStopTurn(t *testing.T) {
	lab := newCallmeterLab(t)
	lab.feed(hookPayload(t, "Stop", map[string]any{
		"background_tasks":       json.RawMessage(`[{"id":"a1","type":"subagent","status":"running","description":"x"}]`),
		"last_assistant_message": "hello", "permission_mode": "default", "prompt_id": "p-1", "session_crons": []any{},
		"stop_hook_active": false,
	}))
	expect(t, "turn", lab.row("SELECT * FROM turns"), map[string]any{
		"event": "Stop", "agent_id": nil, "prompt_id": "p-1", "permission_mode": "default",
		"background_tasks":             `[{"id":"a1","type":"subagent","status":"running","description_bytes":1}]`,
		"session_crons":                "[]",
		"last_assistant_message_bytes": 5,
		"stop_hook_active":             0,
	})
	if n := lab.count("SELECT count(*) FROM events WHERE event = 'Stop'"); n != 1 {
		t.Errorf("Stop events = %d, want 1", n)
	}
}

func TestLifecycleSubagentStopTurn(t *testing.T) {
	lab := newCallmeterLab(t)
	scripted := lab.payloads("scripted.jsonl")
	lab.feed(scripted[5], scripted[8])
	expect(t, "turn", lab.row("SELECT * FROM turns"), map[string]any{
		"event": "SubagentStop", "agent_id": cmSubagent, "agent_type": "general-purpose",
		"last_assistant_message_bytes": 3, "stop_hook_active": 0, "background_tasks": "[]",
	})
}

// A compaction ends with an untyped SubagentStop whose transcript was never
// written: its events and turns rows land, no agent, no fault, no wait.
func TestLifecycleCompactionStop(t *testing.T) {
	lab := newCallmeterLab(t)
	began := time.Now()
	lab.feed(lifecyclePayloads(t, "x")["SubagentStop"])
	if took := time.Since(began); took >= agentSettle {
		t.Errorf("compaction stop took %v, want no settle wait (%v)", took, agentSettle)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM events WHERE event = 'SubagentStop'": 1,
		"SELECT count(*) FROM turns WHERE agent_id = 'acompact01'": 1,
		"SELECT count(*) FROM agents":                              0,
		"SELECT count(*) FROM agent_turns":                         0,
		"SELECT count(*) FROM faults":                              0,
	} {
		if n := lab.count(query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}
}

func TestLifecycleSessionStart(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(hookPayload(t, "SessionStart", map[string]any{"source": "startup"}))
		expect(t, "event", lab.event("SessionStart"), map[string]any{"source": "startup", "model": nil})
		expect(t, "session", lab.row("SELECT * FROM sessions"), map[string]any{"start_source": "startup", "model": nil})
	})
	t.Run("compact", func(t *testing.T) {
		compact := hookPayload(t, "SessionStart", map[string]any{"source": "compact", "model": "m-compact", "prompt_id": "p-1"})
		lab := newCallmeterLab(t)
		lab.feed(compact)
		expect(t, "event", lab.event("SessionStart"), map[string]any{"model": "m-compact"})
		expect(t, "session", lab.row("SELECT * FROM sessions"), map[string]any{"model": "m-compact"})
		for _, startFirst := range []bool{true, false} {
			lab := newCallmeterLab(t)
			transcript := filepath.Join(lab.root, "main.jsonl")
			lab.write(transcript, []byte(requestEntry("msg_m1", "toolu_m1", "m-request", "tool_use", flatUsage)))
			batch := batchPayload(t, transcript, "toolu_m1")
			if startFirst {
				lab.feed(compact, batch)
			} else {
				lab.feed(batch, compact)
			}
			expect(t, "session after a request", lab.row("SELECT * FROM sessions"), map[string]any{"model": "m-request"})
		}
	})
	t.Run("resume", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(hookPayload(t, "SessionStart", map[string]any{
			"source": "resume", "seconds_since_last_response": 4200, "context_tokens": 81234,
			"prompt_cache_likely_expired": true, "estimated_cache_write_usd": 0.31,
		}))
		detail := detailOf(t, lab.event("SessionStart"))
		for key, want := range map[string]any{
			"seconds_since_last_response": 4200.0, "context_tokens": 81234.0,
			"prompt_cache_likely_expired": true, "estimated_cache_write_usd": 0.31,
		} {
			if detail[key] != want {
				t.Errorf("detail[%s] = %#v, want %#v", key, detail[key], want)
			}
		}
	})
}

func TestLifecycleSessionEnd(t *testing.T) {
	lab := newCallmeterLab(t)
	unreadable := filepath.Join(lab.root, "not-a-transcript")
	if err := os.MkdirAll(unreadable, 0o755); err != nil {
		t.Fatalf("create a directory where the transcript is: %v", err)
	}
	began := time.Now()
	lab.feed(hookPayload(t, "SessionEnd", map[string]any{"reason": "other", "transcript_path": unreadable}))
	if took := time.Since(began); took >= agentSettle {
		t.Errorf("SessionEnd took %v, want no settle wait", took)
	}
	expect(t, "event", lab.event("SessionEnd"), map[string]any{"reason": "other"})
	expect(t, "session", lab.row("SELECT * FROM sessions"), map[string]any{"end_reason": "other"})
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0: SessionEnd reads no transcript", n)
	}
}

func TestLifecyclePrompts(t *testing.T) {
	t.Run("UserPromptSubmit", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(hookPayload(t, "UserPromptSubmit", map[string]any{"prompt": "hello world", "prompt_id": "p-1"}))
		row := lab.event("UserPromptSubmit")
		expect(t, "event", row, map[string]any{"prompt_bytes": 11, "prompt_id": "p-1"})
		if _, ok := detailOf(t, row)["task_notification"]; ok {
			t.Errorf("detail = %s, want no task_notification for a typed prompt", row["detail"])
		}
		if strings.Contains(lab.storeText(), "hello world") {
			t.Errorf("the store holds the prompt text")
		}
	})
	t.Run("task notification", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(hookPayload(t, "UserPromptSubmit", map[string]any{"prompt": "<task-notification>done</task-notification>"}))
		if got := detailOf(t, lab.event("UserPromptSubmit"))["task_notification"]; got != true {
			t.Errorf("detail.task_notification = %#v, want true", got)
		}
	})
	t.Run("UserPromptExpansion", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(lifecyclePayloads(t, "four")["UserPromptExpansion"])
		row := lab.event("UserPromptExpansion")
		expect(t, "event", row, map[string]any{"command_name": "x:hello", "prompt_bytes": 4})
		detail := detailOf(t, row)
		if detail["command_source"] != "plugin" || detail["expansion_type"] != "slash_command" || detail["command_args_bytes"] != 4.0 {
			t.Errorf("detail = %s, want command_source, expansion_type and command_args_bytes", row["detail"])
		}
	})
}

func TestLifecycleNamedColumns(t *testing.T) {
	lab := newCallmeterLab(t)
	payloads := lifecyclePayloads(t, "summary")
	for _, name := range []string{
		"InstructionsLoaded", "PreCompact", "PostCompact", "StopFailure", "PermissionRequest",
		"Notification", "PermissionDenied", "Setup", "TaskCreated", "TaskCompleted",
	} {
		lab.feed(payloads[name])
	}
	expect(t, "InstructionsLoaded", lab.event("InstructionsLoaded"), map[string]any{
		"file_path": cmDemoProj + "/AGENTS.md", "memory_type": "Project", "load_reason": "session_start",
	})
	expect(t, "PreCompact", lab.event("PreCompact"), map[string]any{"trigger": "manual"})
	post := lab.event("PostCompact")
	expect(t, "PostCompact", post, map[string]any{"trigger": "manual"})
	if got := detailOf(t, post)["compact_summary_bytes"]; got != 7.0 {
		t.Errorf("PostCompact detail = %s, want compact_summary_bytes 7", post["detail"])
	}
	expect(t, "StopFailure", lab.event("StopFailure"), map[string]any{"error_type": "model_not_found", "effort": "low"})
	request := lab.event("PermissionRequest")
	expect(t, "PermissionRequest", request, map[string]any{"tool_name": "Bash"})
	input, _ := detailOf(t, request)["tool_input"].(map[string]any)
	if input["command_bytes"] != 7.0 {
		t.Errorf("PermissionRequest detail = %s, want tool_input.command_bytes 7", request["detail"])
	}
	expect(t, "Setup", lab.event("Setup"), map[string]any{"trigger": "init"})
	expect(t, "TaskCreated", lab.event("TaskCreated"), map[string]any{"task_id": "t-1"})
	expect(t, "TaskCompleted", lab.event("TaskCompleted"), map[string]any{"task_id": "t-1"})
	if got := detailOf(t, lab.event("Notification"))["notification_type"]; got != "idle_prompt" {
		t.Errorf("Notification detail.notification_type = %#v, want idle_prompt", got)
	}
	denied := detailOf(t, lab.event("PermissionDenied"))
	if denied["tool_name"] != "Bash" || denied["tool_use_id_bytes"] == nil {
		t.Errorf("PermissionDenied detail = %v, want every field sanitized in detail", denied)
	}
	if n := lab.count("SELECT count(*) FROM events"); n != 10 {
		t.Errorf("events = %d, want one per payload", n)
	}
}

func TestLifecycleDuplicateDelivery(t *testing.T) {
	lab := newCallmeterLab(t)
	post := toolPayload(t, "PostToolUse", "toolu_dup", "Bash", map[string]any{"command": "echo hi"},
		map[string]any{"tool_response": map[string]any{"stdout": "hi\n"}})
	stop := lifecyclePayloads(t, "x")["Stop"]
	lab.feed(post, stop)
	before := lab.call("toolu_dup")
	lab.clock.Advance(time.Minute)
	lab.feed(stop, stop)
	lab.clock.Advance(-time.Minute)
	lab.feed(post)
	if n := lab.count("SELECT count(*) FROM events"); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
	if n := lab.count("SELECT count(*) FROM turns"); n != 1 {
		t.Errorf("turns = %d, want 1", n)
	}
	after := lab.call("toolu_dup")
	for column, value := range before {
		if after[column] != value {
			t.Errorf("calls.%s = %q after the duplicate, want %q", column, after[column], value)
		}
	}
}

// storeRows is every row of every table, root-relative and sorted.
func (lab *callmeterLab) storeRows() []string {
	lab.t.Helper()
	rows := strings.Split(strings.ReplaceAll(lab.storeText(), lab.root, "{root}"), "\n")
	sort.Strings(rows)
	return rows
}

// TestLifecyclePermutedArrival: async hooks land in any order; a session's
// payloads, each at its own fixed ts, leave the same rows in every order.
func TestLifecyclePermutedArrival(t *testing.T) {
	base := time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC).UnixMilli()
	type timed struct {
		ts      int64
		payload func(lab *callmeterLab) string
	}
	session := func(t *testing.T) []timed {
		fixed := func(payload string) func(*callmeterLab) string { return func(*callmeterLab) string { return payload } }
		scripted := func(i int) func(*callmeterLab) string {
			return func(lab *callmeterLab) string { return lab.payloads("scripted.jsonl")[i] }
		}
		second := func(event string) func(*callmeterLab) string {
			return func(lab *callmeterLab) string {
				return hookPayload(t, event, map[string]any{
					"agent_id": "a2second", "agent_type": "Explore", "prompt_id": "p-2",
					"agent_transcript_path": filepath.Join(lab.root, "agent-a2second.jsonl"),
				})
			}
		}
		lifecycle := lifecyclePayloads(t, "x")
		echo := map[string]any{"command": "echo hi"}
		return []timed{
			{1, fixed(lifecycle["SessionStart"])},
			{2, fixed(lifecycle["InstructionsLoaded"])},
			{3, fixed(lifecycle["UserPromptSubmit"])},
			{4, fixed(toolPayload(t, "PreToolUse", "toolu_perm", "Bash", echo, map[string]any{"prompt_id": "p-1"}))},
			{5, fixed(toolPayload(t, "PostToolUse", "toolu_perm", "Bash", echo, map[string]any{
				"prompt_id": "p-1", "tool_response": map[string]any{"stdout": "hi\n"},
			}))},
			{6, scripted(5)},
			{7, second("SubagentStart")},
			{8, scripted(8)},
			{9, second("SubagentStop")},
			{10, fixed(lifecycle["Stop"])},
			{11, fixed(lifecycle["SessionEnd"])},
		}
	}
	// One lab for every order, its store reset between them: the payloads
	// name the lab's paths, so their bytes (and event ids) are the same.
	lab := newCallmeterLab(t)
	lab.write(filepath.Join(lab.root, "agent-a2second.jsonl"),
		[]byte(requestEntry("msg_second", "toolu_second", "claude-haiku-4-5", "end_turn", flatUsage)))
	run := func(t *testing.T, order []int) []string {
		if lab.store != nil {
			if err := lab.store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}
			lab.store = nil
		}
		if err := os.RemoveAll(filepath.Dir(lab.storePath)); err != nil {
			t.Fatalf("reset store: %v", err)
		}
		lab.t = t
		payloads := session(t)
		for _, i := range order {
			lab.feedAt(base+payloads[i].ts*1000, lab.rewrite(payloads[i].payload(lab)))
		}
		if n := lab.count("SELECT count(*) FROM agent_turns"); n != 2 {
			t.Fatalf("agent_turns = %d, want one per agent", n)
		}
		return lab.storeRows()
	}
	forward := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	want := run(t, forward)
	for name, order := range map[string][]int{
		"reversed": {10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		"shuffled": {7, 2, 10, 4, 0, 8, 3, 9, 1, 6, 5},
	} {
		t.Run(name, func(t *testing.T) {
			got := run(t, order)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("rows in order %v differ from arrival order:\ngot  %q\nwant %q", order, got, want)
			}
		})
	}
}

func TestLifecycleSessions(t *testing.T) {
	t.Run("columns", func(t *testing.T) {
		lab := newCallmeterLab(t)
		seat := filepath.Join(lab.root, "seat")
		lab.seatDir = &seat
		lab.env["TZ"] = "America/New_York"
		start := lab.clock.Now().UnixMilli()
		lab.feedAt(start+time.Hour.Milliseconds(), hookPayload(t, "Notification", map[string]any{"message": "late", "notification_type": "idle_prompt"}))
		lab.feedAt(start, hookPayload(t, "SessionStart", map[string]any{"source": "startup"}))
		host, err := os.Hostname()
		if err != nil {
			t.Fatalf("hostname: %v", err)
		}
		expect(t, "session", lab.row("SELECT * FROM sessions WHERE session_id = ?", cmSessionA), map[string]any{
			"first_ts": start, "last_ts": start + time.Hour.Milliseconds(), "engine": "claude", "cwd": cmDemoProj,
			"transcript_path": cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl",
			"seat_dir":        seat, "config_dir": filepath.Join(lab.home, ".claude"), "host": host,
			"tz_name": "America/New_York", "tz_offset_minutes": -240,
		})
	})
	t.Run("unreadable zone", func(t *testing.T) {
		copied := filepath.Join(t.TempDir(), "localtime")
		if err := os.WriteFile(copied, []byte("TZif, a copy, not a link"), 0o644); err != nil {
			t.Fatalf("write a localtime copy: %v", err)
		}
		saved := localtimePath
		localtimePath = copied
		t.Cleanup(func() { localtimePath = saved })
		lab := newCallmeterLab(t)
		lab.env["TZ"] = "Not/AZone"
		lab.feed(hookPayload(t, "SessionStart", map[string]any{"source": "startup"}))
		row := lab.row("SELECT * FROM sessions")
		expect(t, "session", row, map[string]any{"tz_name": nil})
		if row["tz_offset_minutes"] == "<nil>" {
			t.Errorf("tz_offset_minutes is NULL, want the local offset")
		}
	})
	t.Run("model", func(t *testing.T) {
		older := requestEntry("msg_old", "toolu_old", "m-old", "tool_use", flatUsage)
		newer := strings.Replace(requestEntry("msg_new", "toolu_new", "m-new", "tool_use", flatUsage), "01:00:00.000Z", "01:05:00.000Z", 1)
		for name, ids := range map[string][]string{"old first": {"toolu_old", "toolu_new"}, "new first": {"toolu_new", "toolu_old"}} {
			lab := newCallmeterLab(t)
			transcript := filepath.Join(lab.root, "main.jsonl")
			lab.write(transcript, []byte(older+newer))
			lab.feed(batchPayload(t, transcript, ids[0]), batchPayload(t, transcript, ids[1]))
			expect(t, name, lab.row("SELECT * FROM sessions"), map[string]any{"model": "m-new"})
		}
	})
}

func TestLifecycleMissedIngest(t *testing.T) {
	t.Run("ingested", func(t *testing.T) {
		lab := newCallmeterLab(t)
		missed := lab.files().missed
		if err := os.MkdirAll(filepath.Dir(missed), 0o755); err != nil {
			t.Fatalf("create state dir: %v", err)
		}
		lab.write(missed, []byte("1790000000\tPostToolUse\tno binary\n1790000001\tStop\tchecksum mismatch\n"))
		lab.feed(hookPayload(t, "SessionStart", map[string]any{"source": "startup"}))
		if n := lab.count("SELECT count(*) FROM faults WHERE stage = ?", callmeter.StageBinary); n != 2 {
			t.Errorf("binary faults = %d, want 2", n)
		}
		if _, err := os.Stat(missed); !os.IsNotExist(err) {
			t.Errorf("missed.log stat = %v, want it gone", err)
		}
	})
	t.Run("ingest error", func(t *testing.T) {
		lab := newCallmeterLab(t)
		blocker := filepath.Join(lab.root, "blocker")
		lab.write(blocker, []byte("a file where missed.log's directory should be"))
		lab.missed = filepath.Join(blocker, "missed.log")
		lab.feed(hookPayload(t, "SessionStart", map[string]any{"source": "startup"}))
		if n := lab.count("SELECT count(*) FROM faults WHERE stage = ?", callmeter.StageStore); n != 1 {
			t.Errorf("store faults = %d, want 1", n)
		}
		if n := lab.count("SELECT count(*) FROM events WHERE event = 'SessionStart'"); n != 1 {
			t.Errorf("SessionStart events = %d, want the record to continue", n)
		}
	})
}

const privacySentinel = "SENTINEL-free-text-7f3a"

func TestLifecyclePrivacySweep(t *testing.T) {
	lab := newCallmeterLab(t)
	payloads := lifecyclePayloads(t, privacySentinel)
	for _, payload := range payloads {
		lab.feed(payload)
	}
	if n := lab.count("SELECT count(*) FROM events"); n != len(payloads) {
		t.Fatalf("events = %d, want one per payload", n)
	}
	if strings.Contains(lab.storeText(), privacySentinel) {
		t.Errorf("a column of the store holds the free text %q", privacySentinel)
	}
}

func TestLifecycleUnregisteredEvent(t *testing.T) {
	lab := newCallmeterLab(t)
	lab.feed(hookPayload(t, "MessageDisplay", map[string]any{"message": "x"}))
	expect(t, "fault", lab.row("SELECT * FROM faults"), map[string]any{
		"stage": callmeter.StagePayload, "error": `event "MessageDisplay" is not one callmeter records`,
	})
	for _, table := range []string{"events", "turns", "sessions", "calls", "agents"} {
		if n := lab.count("SELECT count(*) FROM " + table); n != 0 {
			t.Errorf("%s = %d, want no row", table, n)
		}
	}
}

// TestLifecycleExitAndStdout: every payload exits 0 (feed's check) and
// writes nothing to stdout.
func TestLifecycleExitAndStdout(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = writer
	lab := newCallmeterLab(t)
	scripted := lab.payloads("scripted.jsonl")
	lab.feed(scripted...)
	for _, payload := range lifecyclePayloads(t, "x") {
		lab.feed(payload)
	}
	lab.feed(hookPayload(t, "MessageDisplay", nil))
	os.Stdout = saved
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q, want empty", out)
	}
}
