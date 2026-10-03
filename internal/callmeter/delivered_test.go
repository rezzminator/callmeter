package callmeter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDeliveredBytes(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "string", raw: `"hello world"`, want: 11},
		{name: "empty string", raw: `""`, want: 0},
		{
			name: "content block array, text summed",
			raw:  `[{"type":"text","text":"abc"},{"type":"text","text":"de"}]`,
			want: 5,
		},
		{
			name: "non-text block contributes nothing",
			raw:  `[{"type":"text","text":"abc"},{"type":"image","text":"ignored"}]`,
			want: 3,
		},
		{name: "empty array", raw: `[]`, want: 0},
		{name: "null", raw: `null`, want: 0},
		{name: "empty raw message", raw: ``, want: 0},
		{name: "object is unmeasurable", raw: `{"foo":"bar"}`, wantErr: true},
		{name: "number is unmeasurable", raw: `42`, wantErr: true},
		{name: "boolean is unmeasurable", raw: `true`, wantErr: true},
		{name: "malformed string errors", raw: `"unterminated`, wantErr: true},
		{name: "malformed array errors", raw: `[{"text":`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeliveredBytes(json.RawMessage(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DeliveredBytes(%q) = %d, nil; want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeliveredBytes(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("DeliveredBytes(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestResultOutcome(t *testing.T) {
	cases := []struct {
		name, text, outcome string
		failed              bool
	}{
		{"harness refusal", "<tool_use_error>String to replace not found in file.\nString: private-edit-text</tool_use_error>", OutcomeRefused, true},
		{"hook deny", "PreToolUse:Bash hook error: private-reason", OutcomeDeniedByHook, true},
		{"permission", "Claude requested permissions to write to /tmp/demo-proj/a.txt, but you haven't granted it yet.", OutcomeDeniedByPermission, true},
		{"auto-mode permission denial", "Permission for this action was denied", OutcomeDeniedByPermission, true},
		{"auto-mode denial with detail", "Permission for this action was denied by the auto mode classifier.", OutcomeDeniedByPermission, true},
		{"auto-mode no verdict", "The server-side auto mode classifier gave no verdict (error),", "", false},
		{"user rejection", "The user doesn't want to proceed with this tool use.", OutcomeRejectedByUser, true},
		{"ordinary output", "Exit code 137", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, failed := ResultOutcome(c.text)
			if outcome != c.outcome || failed != c.failed {
				t.Fatalf("ResultOutcome(%q) = (%q, %v), want (%q, %v)", c.text, outcome, failed, c.outcome, c.failed)
			}
		})
	}
}

// resultLine is a transcript's user entry carrying one tool_result, in the
// shape a live session wrote it (toolUseResult as the string a failure has).
func resultLine(t *testing.T, id, content string, isError bool) string {
	t.Helper()
	entry := map[string]any{
		"type": "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{
			"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isError,
		}}},
		"toolUseResult": "Error: " + content,
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode transcript line: %v", err)
	}
	return string(encoded) + "\n"
}

// TestFindResultsOutcome: a failed call settled from its transcript carries a
// defined error, never NULL: a harness refusal its label, a killed command
// its `Exit code N`, any other failure ErrorNotStored; no result text is kept.
func TestFindResultsOutcome(t *testing.T) {
	cases := []struct {
		name, content string
		isError       bool
		want          Result
	}{
		{"harness refusal", "<tool_use_error>String to replace not found in file.\nString: private-edit-text</tool_use_error>", true,
			Result{Failed: true, Outcome: OutcomeRefused, Refused: true}},
		{"killed command", "Exit code 137", true, Result{Failed: true, Outcome: "Exit code 137"}},
		{"failed command", "Exit code 1\nprivate-stderr-line", true, Result{Failed: true, Outcome: "Exit code 1"}},
		{"other failure", "File does not exist: private-path", true, Result{Failed: true, Outcome: ErrorNotStored}},
		// resultLine's toolUseResult is "Error: "+content, a string: RealBytes measures it as DeliveredBytes does.
		{"success", "private-output-line", false, Result{Real: Ptr(int64(len("Error: private-output-line")))}},
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var transcript strings.Builder
	ids := make([]string, len(cases))
	for i, c := range cases {
		ids[i] = "toolu_outcome0" + string(rune('a'+i))
		transcript.WriteString(resultLine(t, ids[i], c.content, c.isError))
	}
	if err := os.WriteFile(path, []byte(transcript.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	found, err := FindResults(path, ids)
	if err != nil {
		t.Fatalf("FindResults: %v", err)
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.want
			want.Bytes = int64(len(c.content))
			got, ok := found[ids[i]]
			if !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("FindResults[%s] = %+v (found %v), want %+v", c.name, got, ok, want)
			}
			if strings.Contains(got.Outcome, "private") {
				t.Fatalf("outcome %q keeps result text", got.Outcome)
			}
		})
	}
}

// TestRealBytes: the real size of a tool_response, in the precedence the
// PostToolUse hook stores it: persistedOutputSize, stdout, file.content, the
// delivered size of content, else the response JSON's own length.
func TestRealBytes(t *testing.T) {
	tests := []struct {
		name, raw string
		tool      string
		want      int64
		wantErr   bool
		wantNil   bool
	}{
		{name: "string", raw: `"hello world"`, want: 11},
		{name: "content block array", raw: `[{"type":"text","text":"abc"},{"type":"image","text":"x"}]`, want: 3},
		{name: "empty", raw: ``, want: 0},
		{name: "null", raw: `null`, want: 0},
		{name: "stdout", tool: "Bash", raw: `{"stdout":"hi\n","stderr":"err"}`, want: 3},
		{name: "empty stdout", raw: `{"stdout":"","stderr":"err"}`, want: 0},
		{name: "persistedOutputSize beats stdout", raw: `{"stdout":"preview","persistedOutputPath":"/tmp/o","persistedOutputSize":4096}`, want: 4096},
		{name: "persistedOutputSize 0 is a size", raw: `{"stdout":"preview","persistedOutputSize":0}`, want: 0},
		{name: "stdout beats file", raw: `{"stdout":"ab","file":{"content":"abcdef"}}`, want: 2},
		{name: "file.content", tool: "Read", raw: `{"type":"text","file":{"filePath":"/tmp/f","content":"abcdef"}}`, want: 6},
		{name: "content blocks", raw: `{"content":[{"type":"text","text":"abc"},{"type":"text","text":"de"}]}`, want: 5},
		{name: "content string", raw: `{"content":"abcd","prompt":"long prompt"}`, want: 4},
		{name: "null content falls back", raw: `{"content":null}`, want: int64(len(`{"content":null}`))},
		{name: "fallback is the response length", raw: `{"agentId":"a1","status":"completed"}`, want: int64(len(`{"agentId":"a1","status":"completed"}`))},
		{name: "unmeasurable content", raw: `{"content":{"x":1}}`, wantErr: true},
		{name: "number", raw: `42`, wantErr: true},
		{name: "malformed object", raw: `{"stdout":5}`, wantErr: true},
	}
	for _, response := range noRealOutputResponses(t) {
		tests = append(tests, struct {
			name, raw string
			tool      string
			want      int64
			wantErr   bool
			wantNil   bool
		}{name: response.name, tool: response.tool, raw: string(response.raw), wantNil: true})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RealBytes(tt.tool, json.RawMessage(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("RealBytes(%s) = %v, nil; want an error", tt.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RealBytes(%s) unexpected error: %v", tt.name, err)
			}
			if tt.wantNil {
				if got != nil {
					t.Fatalf("RealBytes(%s) = %d, want NULL", tt.tool, *got)
				}
				return
			}
			if got == nil || *got != tt.want {
				t.Fatalf("RealBytes(%s) = %v, want %d", tt.name, got, tt.want)
			}
		})
	}
}

// TestFindResultsReal: a successful call's Real is RealBytes of the line's
// toolUseResult; a line with none, a null one, a failed call and a line holding
// several tool_results have none, and a toolUseResult RealBytes cannot measure
// is an error naming the tool_use_id.
func TestFindResultsReal(t *testing.T) {
	line := func(useResult string, ids ...string) string {
		var blocks []string
		for _, id := range ids {
			blocks = append(blocks, `{"type":"tool_result","tool_use_id":"`+id+`","content":"preview"}`)
		}
		out := `{"type":"user","message":{"role":"user","content":[` + strings.Join(blocks, ",") + `]}`
		if useResult != "" {
			out += `,"toolUseResult":` + useResult
		}
		return out + "}\n"
	}
	write := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	t.Run("an object", func(t *testing.T) {
		path := write(t, line(`{"stdout":"preview","persistedOutputSize":4096}`, "toolu_a"), line(`{"agentId":"a1","status":"completed"}`, "toolu_b"))
		found, err := FindResults(path, []string{"toolu_a", "toolu_b"})
		if err != nil {
			t.Fatal(err)
		}
		if got := found["toolu_a"]; got.Real == nil || *got.Real != 4096 || got.Bytes != int64(len("preview")) {
			t.Errorf("toolu_a = %+v, want Real 4096 and Bytes 7", got)
		}
		if got := found["toolu_b"]; got.Real == nil || *got.Real != int64(len(`{"agentId":"a1","status":"completed"}`)) {
			t.Errorf("toolu_b = %+v, want the response's own length", got)
		}
	})
	for _, response := range noRealOutputResponses(t) {
		t.Run(response.name, func(t *testing.T) {
			use := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_a","name":"` + response.tool + `","input":{}}]}}` + "\n"
			path := write(t, use, line(string(response.raw), "toolu_a"))
			found, err := FindResults(path, []string{"toolu_a"})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := found["toolu_a"]; !ok || got.Real != nil || got.Failed || got.Bytes != 7 {
				t.Fatalf("result found=%t Real=%v Failed=%t Bytes=%d, want NULL, false, 7", ok, got.Real, got.Failed, got.Bytes)
			}
		})
	}
	t.Run("none", func(t *testing.T) {
		path := write(t, line("", "toolu_a"), line("null", "toolu_b"), line(`{"stdout":"x"}`, "toolu_c", "toolu_d"))
		found, err := FindResults(path, []string{"toolu_a", "toolu_b", "toolu_c", "toolu_d"})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"toolu_a", "toolu_b", "toolu_c", "toolu_d"} {
			if got, ok := found[id]; !ok || got.Real != nil {
				t.Errorf("%s = %+v (found %v), want a result with no Real", id, got, ok)
			}
		}
	})
	t.Run("a failed call", func(t *testing.T) {
		path := write(t, resultLine(t, "toolu_a", "Exit code 1", true))
		found, err := FindResults(path, []string{"toolu_a"})
		if err != nil {
			t.Fatal(err)
		}
		if got := found["toolu_a"]; !got.Failed || got.Real != nil {
			t.Errorf("toolu_a = %+v, want failed with no Real", got)
		}
	})
	t.Run("an unmeasurable toolUseResult", func(t *testing.T) {
		path := write(t, line(`{"content":{"x":1}}`, "toolu_a"))
		_, err := FindResults(path, []string{"toolu_a"})
		if err == nil || !strings.Contains(err.Error(), "toolu_a") {
			t.Fatalf("FindResults error = %v, want one naming toolu_a", err)
		}
	})
}

type capturedOutputResponse struct {
	name, tool string
	raw, input json.RawMessage
}

// noRealOutputResponses reads completed and async responses from captures;
// response bodies stay inside the test, never in its diagnostics.
func noRealOutputResponses(t *testing.T) []capturedOutputResponse {
	t.Helper()
	var responses []capturedOutputResponse
	seen := map[string]bool{}
	for _, file := range []string{"callmeter/scripted.jsonl", "gym/S2/payloads.jsonl"} {
		data, err := os.ReadFile(filepath.Join("..", "hookentry", "testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var p struct {
				Event    string          `json:"hook_event_name"`
				Tool     string          `json:"tool_name"`
				Response json.RawMessage `json:"tool_response"`
				Input    json.RawMessage `json:"tool_input"`
			}
			if err := json.Unmarshal([]byte(line), &p); err != nil {
				t.Fatal(err)
			}
			if p.Event != "PostToolUse" || (p.Tool != "Edit" && p.Tool != "Write" && p.Tool != "Agent") {
				continue
			}
			var shape struct {
				Async bool `json:"isAsync"`
			}
			if err := json.Unmarshal(p.Response, &shape); err != nil {
				t.Fatal(err)
			}
			name := p.Tool
			if shape.Async {
				name += " async"
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			responses = append(responses, capturedOutputResponse{name, p.Tool, p.Response, p.Input})
		}
	}
	if len(responses) != 4 {
		t.Fatalf("captured response shapes = %d, want 4", len(responses))
	}
	return responses
}
