package callmeter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeInput(t *testing.T) {
	long := strings.Repeat("x", LongField+1)
	longCommand := "echo " + long
	cases := []struct {
		name, tool, raw, want string
	}{
		{
			"write drops content",
			"Write",
			`{"file_path":"/tmp/demo-proj/a.txt","content":"héllo"}`,
			`{"content_bytes":6,"file_path":"/tmp/demo-proj/a.txt"}`,
		},
		{
			"bash keeps command and description",
			"Bash",
			`{"command":"wc -l a.txt","description":"Count lines","timeout":5000}`,
			`{"command":"wc -l a.txt","description":"Count lines","timeout":5000}`,
		},
		{"bash keeps a long command", "Bash", `{"command":"` + longCommand + `"}`, `{"command":"` + longCommand + `"}`},
		{
			"edit drops strings",
			"Edit",
			`{"file_path":"/tmp/demo-proj/a.go","old_string":"ab","new_string":"abc","replace_all":true}`,
			`{"file_path":"/tmp/demo-proj/a.go","new_string_bytes":3,"old_string_bytes":2,"replace_all":true}`,
		},
		{
			"multiedit drops edits",
			"MultiEdit",
			`{"file_path":"/tmp/demo-proj/a.go","edits":[{"old_string":"a","new_string":"b"}]}`,
			`{"edits_bytes":37,"file_path":"/tmp/demo-proj/a.go"}`,
		},
		{
			"notebook drops new_source",
			"NotebookEdit",
			`{"notebook_path":"/tmp/demo-proj/n.ipynb","new_source":"x=1"}`,
			`{"new_source_bytes":3,"notebook_path":"/tmp/demo-proj/n.ipynb"}`,
		},
		{
			"agent drops prompt",
			"Agent",
			`{"description":"scan","prompt":"do it","subagent_type":"general"}`,
			`{"description":"scan","prompt_bytes":5,"subagent_type":"general"}`,
		},
		{
			"read keeps range",
			"Read",
			`{"file_path":"/tmp/demo-proj/a.go","offset":10,"limit":20}`,
			`{"file_path":"/tmp/demo-proj/a.go","limit":20,"offset":10}`,
		},
		{
			"grep keeps pattern",
			"Grep",
			`{"pattern":"func [A-Z]","path":"/tmp/demo-proj","glob":"*.go"}`,
			`{"glob":"*.go","path":"/tmp/demo-proj","pattern":"func [A-Z]"}`,
		},
		{
			"other long field is sized",
			"WebFetch",
			`{"url":"https://example.com","query":"` + long + `"}`,
			`{"query_bytes":4097,"url":"https://example.com"}`,
		},
		{"non-bash long command is sized", "Custom", `{"command":"` + longCommand + `"}`, `{"command_bytes":4102}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SanitizeInput(c.tool, json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("SanitizeInput: %v", err)
			}
			if got != c.want {
				t.Fatalf("SanitizeInput = %s\nwant             %s", got, c.want)
			}
		})
	}
}

func TestSanitizeInputRejectsNonObject(t *testing.T) {
	for _, raw := range []string{``, `null`, `[1]`, `"text"`, `{"broken"`} {
		if got, err := SanitizeInput("Bash", json.RawMessage(raw)); err == nil {
			t.Errorf("SanitizeInput(%q) = %q, want an error", raw, got)
		}
	}
}

func TestSanitizeDetail(t *testing.T) {
	cases := []struct {
		name, raw string
		omit      map[string]bool
		want      string
	}{
		{"kept keys stay", `{"source":"startup","model":"m-1","load_reason":"session_start"}`, nil,
			`{"source":"startup","model":"m-1","load_reason":"session_start"}`},
		{"free text becomes bytes", `{"prompt":"héllo","last_assistant_message":"hi"}`, nil,
			`{"prompt_bytes":6,"last_assistant_message_bytes":2}`},
		{"numbers booleans null stay", `{"context_tokens":1234,"estimated_cache_write_usd":0.42,"prompt_cache_likely_expired":true,"x":null}`, nil,
			`{"context_tokens":1234,"estimated_cache_write_usd":0.42,"prompt_cache_likely_expired":true,"x":null}`},
		{"array of objects keeps its shape", `[{"id":"a1","type":"subagent","status":"running","description":"x"}]`, nil,
			`[{"id":"a1","type":"subagent","status":"running","description_bytes":1}]`},
		{"empty array stays", `[]`, nil, `[]`},
		{"strings in an unkept array become counts", `{"args":["ab","héllo"],"id":["k1"]}`, nil,
			`{"args":[2,6],"id":["k1"]}`},
		{"nested objects", `{"tool_input":{"command":"rm -rf x","timeout":5},"permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"session"}]}`,
			nil,
			`{"tool_input":{"command_bytes":8,"timeout":5},"permission_suggestions":[{"type":"addRules","rules":[{"toolName_bytes":4,"ruleContent_bytes":8}],"behavior":"allow","destination":"session"}]}`},
		{"omit is top-level only", `{"session_id":"s","cwd":"/x","nested":{"session_id":"t"}}`, map[string]bool{"session_id": true, "cwd": true},
			`{"nested":{"session_id_bytes":1}}`},
		{"everything omitted", `{"session_id":"s"}`, map[string]bool{"session_id": true}, `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SanitizeDetail(json.RawMessage(c.raw), c.omit)
			if err != nil {
				t.Fatalf("SanitizeDetail: %v", err)
			}
			if got != c.want {
				t.Fatalf("SanitizeDetail = %s\nwant              %s", got, c.want)
			}
		})
	}
}

func TestSanitizeDetailRejectsNonContainer(t *testing.T) {
	for _, raw := range []string{``, `null`, `"text"`, `12`, `{"broken"`} {
		if got, err := SanitizeDetail(json.RawMessage(raw), nil); err == nil {
			t.Errorf("SanitizeDetail(%q) = %q, want an error", raw, got)
		}
	}
}
