package callmeter

import (
	"encoding/json"
	"strconv"
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
			"bash keeps command, sizes description",
			"Bash",
			`{"command":"wc -l a.txt","description":"Count lines","timeout":5000}`,
			`{"command":"wc -l a.txt","description_bytes":11,"timeout":5000}`,
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
			`{"description":"scan","prompt":"do it","subagent_type":"general","model":"haiku"}`,
			`{"description_bytes":4,"model":"haiku","prompt_bytes":5,"subagent_type":"general"}`,
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
		{
			"skill and grep output mode kept",
			"Skill",
			`{"skill":"report","output_mode":"content","args":"private args"}`,
			`{"args_bytes":12,"output_mode":"content","skill":"report"}`,
		},
		{
			"free-text labels are sized",
			"ToolSearch",
			`{"query":"select:Read","max_results":5,"target":"x"}`,
			`{"max_results":5,"query_bytes":11,"target_bytes":1}`,
		},
		{
			"enums, ids and addressees are kept",
			"SendMessage",
			`{"to":"helper","type":"message","message":"hi","summary":"greet","task_id":"t-1","shell_id":"s-1"}`,
			`{"message_bytes":2,"shell_id":"s-1","summary_bytes":5,"task_id":"t-1","to":"helper","type":"message"}`,
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
		{"an error or reason of an event with no labels becomes bytes", `{"hook_event_name":"PermissionDenied","reason":"private why","error":"rate_limit","nested":{"error":"boom"}}`,
			map[string]bool{"hook_event_name": true}, `{"reason_bytes":11,"error_bytes":10,"nested":{"error_bytes":4}}`},
		{"an mcp__ tool's input keeps no string", `{"hook_event_name":"PermissionRequest","tool_name":"mcp__mail__send","tool_input":{"destination":"alice@x","id":"tok","source":"print(1)","nested":{"file_path":"/p"},"count":2}}`,
			map[string]bool{"hook_event_name": true, "tool_name": true}, `{"tool_input":{"destination_bytes":7,"id_bytes":3,"source_bytes":8,"nested":{"file_path_bytes":2},"count":2}}`},
		{"a built-in tool's input keeps its kept keys", `{"hook_event_name":"PermissionRequest","tool_name":"Read","tool_input":{"file_path":"/p"}}`,
			map[string]bool{"hook_event_name": true, "tool_name": true}, `{"tool_input":{"file_path":"/p"}}`},
		{"a tool_use_id stays an id", `{"hook_event_name":"PermissionDenied","tool_name":"Bash","tool_use_id":"toolu_denied","tool_input":{"command":"ls"}}`,
			map[string]bool{"hook_event_name": true, "tool_name": true}, `{"tool_use_id":"toolu_denied","tool_input":{"command_bytes":2}}`},
		{"an unknown value of a labelled key becomes bytes", `{"hook_event_name":"StopFailure","error":"private failure text"}`,
			map[string]bool{"hook_event_name": true}, `{"error_bytes":20}`},
		{"a known label stays", `{"hook_event_name":"StopFailure","error":"max_output_tokens"}`,
			map[string]bool{"hook_event_name": true}, `{"error":"max_output_tokens"}`},
		{"an error with no event becomes bytes", `[{"id":"a1","error":"rate_limit"}]`, nil, `[{"id":"a1","error_bytes":10}]`},
		{"overloaded is a label", `{"hook_event_name":"StopFailure","error":"overloaded"}`,
			map[string]bool{"hook_event_name": true}, `{"error":"overloaded"}`},
		{"account_on_hold is a label", `{"hook_event_name":"StopFailure","error":"account_on_hold"}`,
			map[string]bool{"hook_event_name": true}, `{"error":"account_on_hold"}`},
		{"cloud_credential_error is a label", `{"hook_event_name":"StopFailure","error":"cloud_credential_error"}`,
			map[string]bool{"hook_event_name": true}, `{"error":"cloud_credential_error"}`},
		{"an error in words with spaces becomes bytes", `{"hook_event_name":"StopFailure","error":"overloaded try again"}`,
			map[string]bool{"hook_event_name": true}, `{"error_bytes":20}`},
		{"an error off the kind shape becomes bytes", `{"hook_event_name":"StopFailure","error":"Overloaded"}`,
			map[string]bool{"hook_event_name": true}, `{"error_bytes":10}`},
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

// heredocBody is invented file content a heredoc carries: it must never reach
// the store.
const heredocBody = "alpha-private-line\n\tbeta-private-line $HOME\n"

func TestSanitizeInputCutsHeredocBodies(t *testing.T) {
	cases := []struct {
		name, command, want string
		bytes               int // heredoc_bytes, 0 for absent
	}{
		{
			"heredoc keeps redirect and delimiter",
			"cat > /tmp/demo-proj/notes.txt <<'EOF'\n" + heredocBody + "EOF\nwc -l /tmp/demo-proj/notes.txt",
			"cat > /tmp/demo-proj/notes.txt <<'EOF'\nEOF\nwc -l /tmp/demo-proj/notes.txt",
			len(heredocBody),
		},
		{
			"dash heredoc",
			"cat <<-END > /tmp/demo-proj/n.txt\n\t" + heredocBody + "\tEND\n",
			"cat <<-END > /tmp/demo-proj/n.txt\n\tEND\n",
			len(heredocBody) + 1,
		},
		{
			"two heredocs sum",
			"cat <<A > /tmp/demo-proj/a.txt\n" + heredocBody + "A\ncat <<\"B\" > /tmp/demo-proj/b.txt\n" + heredocBody + "B\n",
			"cat <<A > /tmp/demo-proj/a.txt\nA\ncat <<\"B\" > /tmp/demo-proj/b.txt\nB\n",
			2 * len(heredocBody),
		},
		{
			"nested bash -c",
			"bash -c 'cat > /tmp/demo-proj/n.txt <<EOF\n" + heredocBody + "EOF\n'",
			"bash -c 'cat > /tmp/demo-proj/n.txt <<EOF\nEOF\n'",
			len(heredocBody),
		},
		{
			"heredoc in a command substitution",
			"git commit -m \"$(cat <<'EOF'\n" + heredocBody + "EOF\n)\"",
			"git commit -m \"$(cat <<'EOF'\nEOF\n)\"",
			len(heredocBody),
		},
		{"quoted << untouched", "echo \"a << b\" 'c << d'\necho e", "echo \"a << b\" 'c << d'\necho e", 0},
		{"a here-string is no heredoc", "cat <<< \"inline words\"", "cat <<< '[cut]'", 0},
		{"arithmetic shift untouched", "echo $((1 << 4))", "echo $((1 << 4))", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"command": c.command, "description": "Write notes"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := SanitizeInput("Bash", raw)
			if err != nil {
				t.Fatalf("SanitizeInput: %v", err)
			}
			if strings.Contains(got, "private-line") {
				t.Fatalf("stored input carries the heredoc body: %s", got)
			}
			var fields struct {
				Command      *string `json:"command"`
				HeredocBytes *int    `json:"heredoc_bytes"`
			}
			if err := json.Unmarshal([]byte(got), &fields); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if fields.Command == nil || *fields.Command != c.want {
				t.Fatalf("command = %q\nwant      %q", derefString(fields.Command), c.want)
			}
			gotBytes := 0
			if fields.HeredocBytes != nil {
				gotBytes = *fields.HeredocBytes
				if gotBytes == 0 {
					t.Fatalf("heredoc_bytes present as 0 in %s", got)
				}
			}
			if gotBytes != c.bytes {
				t.Fatalf("heredoc_bytes = %d, want %d (input %s)", gotBytes, c.bytes, got)
			}
		})
	}
}

func TestSanitizeInputUnsplittableCommandKeepsOnlyItsSize(t *testing.T) {
	for _, command := range []string{
		"cat <<EOF > /tmp/demo-proj/x.txt\n" + heredocBody + "EOF\nif then fi )",
		"bash -c \"cat <<EOF\n" + heredocBody + "EOF\n$X\"",
		"bash -c 'cat <<EOF\n" + heredocBody + "EOF\nif then fi )'",
		"git commit -m \"unterminated private message",
		"x=`echo private > f`",
		"git commit -m \"private `echo hi`\"",
	} {
		raw, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			t.Fatal(err)
		}
		got, err := SanitizeInput("Bash", raw)
		if err != nil {
			t.Fatalf("SanitizeInput: %v", err)
		}
		want := `{"command_bytes":` + strconv.Itoa(len(command)) + `}`
		if got != want {
			t.Errorf("SanitizeInput(%q) = %s, want %s", command, got, want)
		}
	}
}

func TestSanitizeInputSizesFreeText(t *testing.T) {
	cases := []struct{ name, tool, raw, want string }{
		{
			"a message is sized",
			"SendMessage",
			`{"to":"helper","summary":"status","message":"private words"}`,
			`{"message_bytes":13,"summary_bytes":6,"to":"helper"}`,
		},
		{"skill args are sized", "Skill", `{"skill":"review","args":"private words"}`, `{"args_bytes":13,"skill":"review"}`},
		{
			"questions and answers are sized",
			"AskUserQuestion",
			`{"questions":[{"question":"private?"}],"answers":{"q":"private"}}`,
			`{"answers_bytes":15,"questions_bytes":25}`,
		},
		{
			"an mcp message is sized, its scalars stay",
			"mcp__demo__send",
			`{"target":"t1","message":"private words","limit":3,"all":true}`,
			`{"all":true,"limit":3,"message_bytes":13,"target_bytes":2}`,
		},
		{"monitor command is a shell command", "Monitor", `{"command":"tail -f /tmp/demo-proj/log","timeout_ms":5}`, `{"command":"tail -f /tmp/demo-proj/log","timeout_ms":5}`},
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

func derefString(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}

// A kept field is kept only as a string: an object or array under a kept key
// of a built-in tool is sized like any other field, so no string nested in it
// is stored. (An mcp__ tool sizes every string anyway, so it cannot show this.)
func TestSanitizeInputSizesKeptFieldContainers(t *testing.T) {
	const secret = "SENTINEL_KEPT_NESTED"
	values := map[string]string{
		"object": `{"content":"` + secret + `"}`,
		"array":  `["` + secret + `"]`,
	}
	for name := range keptFields {
		for shape, value := range values {
			t.Run(name+" "+shape, func(t *testing.T) {
				raw := `{"` + name + `":` + value + `,"timeout":3}`
				got, err := SanitizeInput("Read", json.RawMessage(raw))
				if err != nil {
					t.Fatalf("SanitizeInput: %v", err)
				}
				want := `{"` + name + `_bytes":` + strconv.Itoa(len(value)) + `,"timeout":3}`
				if got != want {
					t.Errorf("SanitizeInput = %s\nwant             %s", got, want)
				}
				if strings.Contains(got, secret) {
					t.Errorf("SanitizeInput kept the nested string: %s", got)
				}
			})
		}
	}
}

// An MCP tool's keys mean what its server says: a `to` may be a recipient
// address, a `type` or `model` free text, a `path` a private name. Every string
// of an mcp__ tool is sized, kept and label fields included; its numbers,
// booleans and null stay.
func TestSanitizeInputSizesEveryMCPString(t *testing.T) {
	const secret = "SENTINEL_MCP"
	raw := `{"to":"alice@` + secret + `.example","type":"` + secret + `_TYPE","model":"` + secret + `_MODEL",` +
		`"url":"https://x.example/cb?token=` + secret + `","path":"/tmp/` + secret + `","limit":3,"all":true,"none":null}`
	got, err := SanitizeInput("mcp__x__y", json.RawMessage(raw))
	if err != nil {
		t.Fatalf("SanitizeInput: %v", err)
	}
	want := `{"all":true,"limit":3,"model_bytes":18,"none":null,"path_bytes":17,"to_bytes":26,"type_bytes":17,"url_bytes":39}`
	if got != want {
		t.Errorf("SanitizeInput = %s\nwant             %s", got, want)
	}
	if strings.Contains(got, secret) {
		t.Errorf("SanitizeInput kept an mcp string: %s", got)
	}
}

// A kept url loses its userinfo, query and fragment, which may carry a
// credential or a token.
func TestSanitizeInputCutsURLQueryAndFragment(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"query and fragment", `{"url":"https://x.example/cb?token=SENTINEL_URL#frag-SENTINEL_URL","prompt":"p"}`, `{"prompt_bytes":1,"url":"https://x.example/cb"}`},
		{"fragment alone", `{"url":"https://x.example/doc#SENTINEL_URL"}`, `{"url":"https://x.example/doc"}`},
		{"fragment holding a question mark", `{"url":"https://x.example/doc#a?SENTINEL_URL"}`, `{"url":"https://x.example/doc"}`},
		{"no query stays", `{"url":"https://x.example/a/b"}`, `{"url":"https://x.example/a/b"}`},
		{"userinfo with query and fragment", `{"url":"https://u:p@example.com/a?q#f"}`, `{"url":"https://example.com/a"}`},
		{"userinfo alone", `{"url":"https://SENTINEL_URL:SENTINEL_URL@example.com/a/b"}`, `{"url":"https://example.com/a/b"}`},
		{"userinfo with no path", `{"url":"https://SENTINEL_URL@example.com"}`, `{"url":"https://example.com"}`},
		{"userinfo holding an at sign", `{"url":"https://a@SENTINEL_URL:p@example.com/"}`, `{"url":"https://example.com/"}`},
		{"an at sign in the path stays", `{"url":"https://x.example/@scope/pkg"}`, `{"url":"https://x.example/@scope/pkg"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SanitizeInput("WebFetch", json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("SanitizeInput: %v", err)
			}
			if got != c.want {
				t.Errorf("SanitizeInput = %s\nwant             %s", got, c.want)
			}
			if strings.Contains(got, "SENTINEL") {
				t.Errorf("SanitizeInput kept the userinfo, query or fragment: %s", got)
			}
			again, err := SanitizeInput("WebFetch", json.RawMessage(got))
			if err != nil || again != got {
				t.Errorf("sanitized again = %s, %v; want the same bytes", again, err)
			}
		})
	}
}

// TestSanitizeInputCutsCommandContent: a stored command is cut by both cutters
// in turn (cmdparse.RedactHeredocs, then cmdparse.CutContent, whose cases live
// in its own test), the operand bytes under operand_bytes beside heredoc_bytes,
// absent when nothing was cut; a stored input sanitized again comes back the
// same bytes.
func TestSanitizeInputCutsCommandContent(t *testing.T) {
	cases := []struct {
		name, command, want string
		bytes, heredoc      int // operand_bytes and heredoc_bytes, 0 for absent
	}{
		{"commit -m", `git commit -m "MSG-private"`, `git commit -m '[cut]'`, 13, 0},
		{"a heredoc body and an echo to a file", "cat > f <<'EOF'\nTXT-private\nEOF\necho TXT-private >> f", "cat > f <<'EOF'\nEOF\necho '[cut]' >> f", 11, 12},
		{"a command with no content stays as it arrived", `echo hi | grep h`, `echo hi | grep h`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"command": c.command})
			if err != nil {
				t.Fatal(err)
			}
			got, err := SanitizeInput("Bash", raw)
			if err != nil {
				t.Fatalf("SanitizeInput: %v", err)
			}
			if strings.Contains(got, "private") {
				t.Fatalf("stored input carries content: %s", got)
			}
			var fields struct {
				Command      *string `json:"command"`
				OperandBytes *int    `json:"operand_bytes"`
				HeredocBytes int     `json:"heredoc_bytes"`
			}
			if err := json.Unmarshal([]byte(got), &fields); err != nil {
				t.Fatalf("decode %s: %v", got, err)
			}
			if fields.Command == nil || *fields.Command != c.want {
				t.Fatalf("command = %q\nwant      %q", derefString(fields.Command), c.want)
			}
			gotBytes := 0
			if fields.OperandBytes != nil {
				gotBytes = *fields.OperandBytes
				if gotBytes == 0 {
					t.Fatalf("operand_bytes present as 0 in %s", got)
				}
			}
			if gotBytes != c.bytes {
				t.Fatalf("operand_bytes = %d, want %d (input %s)", gotBytes, c.bytes, got)
			}
			if fields.HeredocBytes != c.heredoc {
				t.Fatalf("heredoc_bytes = %d, want %d (input %s)", fields.HeredocBytes, c.heredoc, got)
			}
			again, err := SanitizeInput("Bash", json.RawMessage(got))
			if err != nil {
				t.Fatalf("SanitizeInput again: %v", err)
			}
			if again != got {
				t.Fatalf("sanitized again:\n%s\nwant\n%s", again, got)
			}
		})
	}
}
