package callmeter

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		{"success", "private-output-line", false, Result{}},
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
			if !ok || got != want {
				t.Fatalf("FindResults[%s] = %+v (found %v), want %+v", c.name, got, ok, want)
			}
			if strings.Contains(got.Outcome, "private") {
				t.Fatalf("outcome %q keeps result text", got.Outcome)
			}
		})
	}
}
