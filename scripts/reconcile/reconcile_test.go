package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// The fixture: one invented session with a Bash and an Agent call in one
// two-block message, a sub-agent making one Read call and a closing text
// message, and a store holding exactly the rows those transcripts imply.

const (
	fxSession = "11111111-aaaa-4bbb-8ccc-000000000001"
	fxAgent   = "a0000000000000001"
	fxPrompt  = "22222222-aaaa-4bbb-8ccc-000000000002"
	fxSecret  = "invented-secret-text"
)

var fxBase = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func fxTS(sec float64) string {
	return fxBase.Add(time.Duration(sec * float64(time.Second))).Format(time.RFC3339Nano)
}

func fxMS(sec float64) int64 {
	return fxBase.Add(time.Duration(sec * float64(time.Second))).UnixMilli()
}

func jsonLine(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture line: %v", err)
	}
	return string(b)
}

func assistantLine(t *testing.T, sec float64, msgID string, out int64, stop any, content ...map[string]any) string {
	return jsonLine(t, map[string]any{
		"type": "assistant", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli",
		"message": map[string]any{
			"id": msgID, "model": "claude-demo", "stop_reason": stop, "content": content,
			"usage": map[string]any{"input_tokens": 3, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 20},
		},
	})
}

func toolUse(id, name string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{"command": fxSecret}}
}

func toolResult(t *testing.T, sec float64, id string) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "is_error": false, "content": fxSecret}}},
	})
}

func promptLine(t *testing.T, sec float64) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli", "promptId": fxPrompt,
		"message": map[string]any{"role": "user", "content": fxSecret},
	})
}

type fixture struct {
	dir, db, projects, main, allow string
}

// mainLines and agentLines are the matching transcripts; a case appends to them.
func mainLines(t *testing.T) []string {
	return []string{
		promptLine(t, 1),
		// One message, two content blocks: the first line carries the streaming
		// partial usage, the last the final one.
		assistantLine(t, 2, "msg_A1", 5, nil, toolUse("toolu_A1", "Bash")),
		assistantLine(t, 2.1, "msg_A1", 50, "tool_use", toolUse("toolu_A2", "Agent")),
		toolResult(t, 3, "toolu_A1"),
		toolResult(t, 20, "toolu_A2"),
		jsonLine(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": fxTS(21), "sessionId": fxSession, "entrypoint": "cli"}),
	}
}

func agentLines(t *testing.T) []string {
	return []string{
		promptLine(t, 5),
		assistantLine(t, 6, "msg_B1", 40, "tool_use", toolUse("toolu_B1", "Read")),
		toolResult(t, 7, "toolu_B1"),
		assistantLine(t, 8, "msg_B2", 30, "end_turn", map[string]any{"type": "text", "text": fxSecret}),
	}
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newFixture(t *testing.T, extraMain, extraSQL []string) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{dir: dir, db: filepath.Join(dir, "snap", "callmeter.db"), projects: filepath.Join(dir, "projects"), allow: filepath.Join(dir, "allowlist.txt")}
	f.main = filepath.Join(f.projects, "-tmp-demo-proj", fxSession+".jsonl")
	writeLines(t, f.main, append(mainLines(t), extraMain...))
	sub := filepath.Join(f.projects, "-tmp-demo-proj", fxSession, "subagents")
	writeLines(t, filepath.Join(sub, "agent-"+fxAgent+".jsonl"), agentLines(t))
	if err := os.WriteFile(filepath.Join(sub, "agent-"+fxAgent+".meta.json"), []byte(`{"agentType":"demo-agent","toolUseId":"toolu_A2"}`), 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if err := os.WriteFile(f.allow, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}

	ctx := context.Background()
	store, err := callmeter.OpenDB(ctx, f.db)
	if err != nil {
		t.Fatalf("create fixture store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close fixture store: %v", err)
		}
	}()
	rows := []string{
		fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, model, start_source, transcript_path) VALUES('%s', %d, %d, 'claude-demo', 'startup', '%s')`, fxSession, fxMS(0.5), fxMS(21), f.main),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('e1', 'SessionStart', %d, '%s', 'startup')`, fxMS(0.5), fxSession),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e2', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(1), fxSession, fxPrompt),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, agent_id, agent_type) VALUES('e3', 'SubagentStart', %d, '%s', '%s', 'demo-agent')`, fxMS(5), fxSession, fxAgent),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, agent_id, agent_type) VALUES('e4', 'SubagentStop', %d, '%s', '%s', 'demo-agent')`, fxMS(9), fxSession, fxAgent),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e5', 'Stop', %d, '%s')`, fxMS(21), fxSession),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('e5end', 'SessionEnd', %d, '%s', 'other')`, fxMS(21), fxSession),
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real, failed) VALUES('toolu_A1', '%s', 'msg_A1', %d, 'Bash', 10, 0)`, fxSession, fxMS(2)),
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real, failed) VALUES('toolu_A2', '%s', 'msg_A1', %d, 'Agent', 10, 0)`, fxSession, fxMS(2.1)),
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, request_id, ts, tool, bytes_real, failed) VALUES('toolu_B1', '%s', '%s', 'demo-agent', 'msg_B1', %d, 'Read', 10, 0)`, fxSession, fxAgent, fxMS(6)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_A1', '%s', %d, 'claude-demo', 'tool_use', 3, 50, 100, 20, 2, 0)`, fxSession, fxMS(2)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B1', '%s', '%s', %d, 'claude-demo', 'tool_use', 3, 40, 100, 20, 1, 0)`, fxSession, fxAgent, fxMS(6)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B2', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 30, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(8)),
		fmt.Sprintf(`INSERT INTO agents(agent_id, session_id, agent_type, parent_tool_use_id, started, stopped, tool_uses) VALUES('%s', '%s', 'demo-agent', 'toolu_A2', %d, %d, 1)`, fxAgent, fxSession, fxMS(5), fxMS(9)),
		fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped) VALUES('%s', 1, '%s', 'demo-agent', %d, %d)`, fxAgent, fxSession, fxMS(5), fxMS(9)),
		`INSERT INTO command_parts(tool_use_id, seq, lang, program, parse_status, parser) VALUES('toolu_A1', 1, 'sh', 'echo', 'ok', 1)`,
	}
	for _, q := range append(rows, extraSQL...) {
		if _, err := store.DB().ExecContext(ctx, q); err != nil {
			t.Fatalf("fixture row %q: %v", q, err)
		}
	}
	return f
}

func (f fixture) run(t *testing.T, since string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run([]string{
		"--db", f.db, "--since", since, "--until", "2030-01-01T01:00:00Z",
		"--projects", f.projects, "--allowlist", f.allow, "--scratch", filepath.Join(f.dir, "scratch"),
	}, &out, &errOut)
	if code == 2 {
		t.Fatalf("reconcile could not run: %s", errOut.String())
	}
	if strings.Contains(out.String(), fxSecret) {
		t.Fatalf("output carries transcript content:\n%s", out.String())
	}
	return code, out.String()
}

func TestMatchingFixtureIsClean(t *testing.T) {
	f := newFixture(t, nil, nil)
	code, out := f.run(t, "2030-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(out, "sessions compared 1, transcripts read 2,") || !strings.Contains(out, "mismatches 0 unexplained 0 expected; by class: none") {
		t.Fatalf("exit %d, want 0 and a clean summary:\n%s", code, out)
	}
}

func TestSeededMismatchIsCaught(t *testing.T) {
	cases := []struct {
		name, class string
		extraMain   []string
		sql         []string
	}{
		// calls
		{name: "call row missing", class: "call-missing", sql: []string{`DELETE FROM calls WHERE tool_use_id='toolu_B1'`}},
		{name: "sub-agent call attributed to the main chat", class: "call-agent", sql: []string{`UPDATE calls SET agent_id=NULL WHERE tool_use_id='toolu_B1'`}},
		{name: "sub-agent call with the wrong agent type", class: "call-agent-type", sql: []string{`UPDATE calls SET agent_type='other' WHERE tool_use_id='toolu_B1'`}},
		{name: "call without a size", class: "call-no-size", sql: []string{`UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_A1'`}},
		{name: "call row without a ts", class: "call-no-ts", sql: []string{`UPDATE calls SET ts=NULL WHERE tool_use_id='toolu_A1'`}},
		{name: "store call absent from every transcript", class: "call-extra", sql: []string{fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, ts, tool) VALUES('toolu_Z9', '%s', %d, 'Bash')`, fxSession, fxMS(4))}},
		// requests and tokens
		{name: "request row missing", class: "request-missing", sql: []string{`DELETE FROM requests WHERE request_id='msg_A1'`}},
		{name: "request kept the first block's partial usage", class: "request-tokens", sql: []string{`UPDATE requests SET output_tokens=5 WHERE request_id='msg_A1'`}},
		{name: "token sums disagree", class: "token-sum", sql: []string{`UPDATE requests SET cache_read_tokens=1 WHERE request_id='msg_B1'`}},
		{name: "text-only message has no request row", class: "request-untracked", extraMain: textOnlyTurn(t), sql: textOnlyStop()},
		{name: "request still pending", class: "request-pending", sql: []string{`UPDATE requests SET pending=1 WHERE request_id='msg_B2'`}},
		// agents and agent_turns
		{name: "agent row missing", class: "agent-missing", sql: []string{`DELETE FROM agents`}},
		{name: "agent parent wrong", class: "agent-parent", sql: []string{`UPDATE agents SET parent_tool_use_id='toolu_A1'`}},
		{name: "extra agent turn", class: "agent-turns", sql: []string{fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, stopped) VALUES('%s', 2, '%s', %d)`, fxAgent, fxSession, fxMS(9.5))}},
		{name: "agent turn without a start", class: "agent-turn-no-start", sql: []string{`UPDATE agent_turns SET started=NULL`}},
		// command_parts
		{name: "Bash part unparsed", class: "parse-unparsed", sql: []string{`UPDATE command_parts SET parse_status='unparsed'`}},
		{name: "Bash part failed to parse", class: "parse-error", sql: []string{`UPDATE command_parts SET parse_status='error'`}},
		// lifecycle events
		{name: "Stop missing", class: "event-stop", sql: []string{`DELETE FROM events WHERE event='Stop'`}},
		{name: "SessionStart missing", class: "event-no-sessionstart", sql: []string{`DELETE FROM events WHERE event='SessionStart'`}},
		{name: "UserPromptSubmit missing", class: "event-prompt-missing", sql: []string{`DELETE FROM events WHERE event='UserPromptSubmit'`}},
		{name: "compaction without events", class: "event-compact", extraMain: []string{jsonLine(t, map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": fxTS(22), "sessionId": fxSession, "entrypoint": "cli"})}},
		{name: "API error without StopFailure", class: "event-stopfailure", extraMain: []string{jsonLine(t, map[string]any{"type": "assistant", "timestamp": fxTS(22), "sessionId": fxSession, "isApiErrorMessage": true, "message": map[string]any{"id": "msg_err", "model": "<synthetic>", "content": []map[string]any{}}})}},
		{name: "slash-led prompt bound for the model without UserPromptSubmit", class: "event-prompt-missing", extraMain: []string{userText(t, 22, fxPrompt2, "/tmp/demo-file "+fxSecret)}},
		// sessions
		{name: "session without a model", class: "session-no-model", sql: []string{`UPDATE sessions SET model=NULL`}},
		// the window edge: a call whose result is in, still without a request row
		{name: "returned call without a request row", class: "call-no-request", sql: []string{`UPDATE calls SET request_id=NULL WHERE tool_use_id='toolu_A1'`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, c.extraMain, c.sql)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != 1 || !strings.Contains(out, "MISMATCH "+c.class+" ") {
				t.Fatalf("exit %d, want 1 and a %s mismatch:\n%s", code, c.class, out)
			}
		})
	}
}

const (
	fxPrompt2  = "22222222-aaaa-4bbb-8ccc-000000000003"
	fxSession2 = "11111111-aaaa-4bbb-8ccc-000000000009"
)

// userText is a typed user line whose content is a plain string.
func userText(t *testing.T, sec float64, promptID, text string) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli", "promptId": promptID,
		"message": map[string]any{"role": "user", "content": text},
	})
}

// TestRealShapesAreReadRight seeds shapes Claude Code really writes that an
// earlier reading of the transcripts or the store took for a mismatch.
func TestRealShapesAreReadRight(t *testing.T) {
	cases := []struct {
		name         string
		extraMain    []string
		sql          []string
		want, absent []string // classes that must, and must not, print
		pending      []string // states not yet due at --until: PENDING, never a mismatch
		otherMain    []string // a second session's transcript; {sub} in sql names its directory
	}{
		{
			// A Stop hook that blocks the turn's end makes Claude Code fire Stop
			// again in the same turn with stop_hook_active true: one turn, two Stops.
			name: "blocked Stop fired again with stop_hook_active",
			sql: []string{
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e6', 'Stop', %d, '%s')`, fxMS(21.2), fxSession),
				fmt.Sprintf(`INSERT INTO turns(event_id, event, session_id, ts, stop_hook_active) VALUES('e5', 'Stop', '%s', %d, 0)`, fxSession, fxMS(21)),
				fmt.Sprintf(`INSERT INTO turns(event_id, event, session_id, ts, stop_hook_active) VALUES('e6', 'Stop', '%s', %d, 1)`, fxSession, fxMS(21.2)),
			},
			absent: []string{"event-stop"},
		},
		{
			// A typed /compact is written as a raw line with a promptId; it fires
			// PreCompact with that promptId, never UserPromptSubmit.
			name: "typed /compact and local commands are not model prompts",
			extraMain: []string{
				userText(t, 22, fxPrompt2, "/compact"),
				userText(t, 23, "22222222-aaaa-4bbb-8ccc-000000000004", "/compact keep "+fxSecret),
				userText(t, 24, "22222222-aaaa-4bbb-8ccc-000000000005", "/model"),
			},
			sql:    []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id, trigger) VALUES('e7', 'PreCompact', %d, '%s', '%s', 'manual')`, fxMS(22.05), fxSession, fxPrompt2)},
			absent: []string{"event-prompt-missing"},
		},
		{
			// A call still running at the snapshot: its request row is written at
			// PostToolBatch, after the call returns.
			name:      "call still running at the window's end",
			extraMain: []string{assistantLine(t, 22, "msg_A4", 7, "tool_use", toolUse("toolu_A4", "Read"))},
			sql:       []string{fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, ts, tool) VALUES('toolu_A4', '%s', %d, 'Read')`, fxSession, fxMS(22))},
			pending:   []string{"request-in-flight", "call-in-flight"},
			// its tokens wait with it: no store row can hold them yet
			absent: []string{"request-missing", "call-no-request", "token-sum"},
		},
		{
			// A text-only reply's request is written by the sweep at its turn's
			// Stop (SubagentStop, SessionEnd); before any of them it is open.
			name:      "text-only message whose turn has not ended yet",
			extraMain: []string{assistantLine(t, 22, "msg_A3", 9, "end_turn", map[string]any{"type": "text", "text": fxSecret})},
			pending:   []string{"request-untracked-open"},
			absent:    []string{"request-untracked"},
		},
		{
			// command_parts are written at report time only: a Bash call newer
			// than every parsed one waits for the next report.
			name:    "Bash call no report has parsed yet",
			sql:     []string{`DELETE FROM command_parts`},
			pending: []string{"parts-pending-report"},
			absent:  []string{"parts-missing"},
		},
		{
			// A batch whose message id was not yet on disk at PostToolBatch holds
			// the provisional key until the chat's next batch or Stop fills it; at
			// the snapshot none has come yet. One batch, reported once.
			name: "pending request with no later batch or Stop yet",
			extraMain: []string{
				assistantLine(t, 22, "msg_A4", 7, "tool_use", toolUse("toolu_A4", "Read")),
				toolResult(t, 22.5, "toolu_A4"),
				// the chat's next batch is still running: it fills nothing yet
				assistantLine(t, 23, "msg_A5", 7, "tool_use", toolUse("toolu_A5", "Read")),
			},
			sql: []string{
				fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, ts, tool) VALUES('toolu_A5', '%s', %d, 'Read')`, fxSession, fxMS(23)),
				fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real, failed) VALUES('toolu_A4', '%s', 'pending:toolu_A4', %d, 'Read', 10, 0)`, fxSession, fxMS(22)),
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, calls, pending) VALUES('pending:toolu_A4', '%s', %d, 1, 1)`, fxSession, fxMS(22)),
			},
			pending: []string{"request-pending-open"},
			absent:  []string{"request-missing", "request-pending ", "token-sum"},
		},
		{
			// The store recorded a session whose transcript is gone from disk: that
			// says nothing about whether it reached the model.
			name: "recorded session whose transcript is gone",
			sql: []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, model, start_source, transcript_path) VALUES('%s', %d, 'claude-demo', 'startup', '/nonexistent/demo/%s.jsonl')`, fxSession2, fxMS(1), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e8', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(1), fxSession2, fxPrompt2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e9', 'Stop', %d, '%s')`, fxMS(2), fxSession2),
			},
			want:   []string{"session-no-transcript"},
			absent: []string{"session-promptless"},
		},
		{
			// A session whose one turn the API refused (model_not_found, a rate
			// limit) holds a synthetic API error message and no model message:
			// it reached the model, and its StopFailure is checked on its own.
			name: "session whose only turn ended in an API error",
			sql: []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, start_source, transcript_path) VALUES('%s', %d, 'startup', '{sub}/%s.jsonl')`, fxSession2, fxMS(1), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e8', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(1), fxSession2, fxPrompt2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, error_type) VALUES('e9', 'StopFailure', %d, '%s', 'model_not_found')`, fxMS(2), fxSession2),
			},
			otherMain: []string{
				jsonLine(t, map[string]any{"type": "user", "timestamp": fxTS(1), "sessionId": fxSession2, "entrypoint": "sdk-cli", "promptId": fxPrompt2, "message": map[string]any{"role": "user", "content": fxSecret}}),
				jsonLine(t, map[string]any{"type": "assistant", "timestamp": fxTS(2), "sessionId": fxSession2, "entrypoint": "sdk-cli", "isApiErrorMessage": true, "message": map[string]any{"id": "msg_err2", "model": "<synthetic>", "content": []map[string]any{}}}),
			},
			absent: []string{"session-promptless", "event-stopfailure"},
		},
		{
			// A store row of a session first recorded before --since is not
			// compared: no transcript of it was read.
			name: "row of a session recorded before the window",
			sql: []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, model, transcript_path) VALUES('%s', %d, 'claude-demo', '/nonexistent/%s.jsonl')`, fxSession2, fxMS(-3600), fxSession2),
				fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real, failed) VALUES('toolu_C1', '%s', 'msg_C1', %d, 'Read', 10, 0)`, fxSession2, fxMS(3)),
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, model, stop_reason, calls, pending) VALUES('msg_C1', '%s', %d, 'claude-demo', 'tool_use', 1, 0)`, fxSession2, fxMS(3)),
			},
			absent: []string{"call-extra", "request-extra"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			sql := make([]string, len(c.sql))
			for i, q := range c.sql {
				sql[i] = strings.ReplaceAll(q, "{sub}", dir)
			}
			if len(c.otherMain) > 0 {
				writeLines(t, filepath.Join(dir, fxSession2+".jsonl"), c.otherMain)
			}
			f := newFixture(t, c.extraMain, sql)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			for _, class := range c.pending {
				if !strings.Contains(out, "PENDING "+class+" ") || strings.Contains(out, "MISMATCH "+class+" ") {
					t.Errorf("%s did not print as PENDING alone:\n%s", class, out)
				}
			}
			if len(c.pending) > 0 && len(c.want) == 0 && code != 0 {
				t.Errorf("exit %d, want 0: a state not yet due is no mismatch:\n%s", code, out)
			}
			for _, class := range c.absent {
				if strings.Contains(out, " "+class+" ") {
					t.Errorf("a %s mismatch printed for a real shape:\n%s", class, out)
				}
			}
			for _, class := range c.want {
				if !strings.Contains(out, "MISMATCH "+class+" ") {
					t.Errorf("no %s mismatch:\n%s", class, out)
				}
			}
		})
	}
}

func TestNoSessionComparedFails(t *testing.T) {
	f := newFixture(t, nil, nil)
	code, out := f.run(t, "2030-01-01T00:30:00Z")
	if code != 1 || !strings.Contains(out, "sessions compared 0,") || !strings.Contains(out, "FAIL: 0 sessions compared") {
		t.Fatalf("exit %d, want 1 and the 0-sessions failure:\n%s", code, out)
	}
}

func TestAllowlistedMismatchPrintsItsReason(t *testing.T) {
	f := newFixture(t, nil, []string{`UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_A1'`})
	if err := os.WriteFile(f.allow, []byte("call-no-size | toolu_A1 | invented reason for the test\n"), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	code, out := f.run(t, "2030-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(out, "EXPECTED call-no-size session="+fxSession+" ids=toolu_A1") || !strings.Contains(out, "expected: invented reason for the test") {
		t.Fatalf("exit %d, want 0 and the expected line:\n%s", code, out)
	}
}

func TestLiveStoreRefused(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	live := filepath.Join(home, ".local", "state", "callmeter")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"--db", filepath.Join(live, "callmeter.db"), "--since", "2030-01-01T00:00:00Z"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "live store") {
		t.Fatalf("exit %d, want 2 and a live-store refusal: %s", code, errOut.String())
	}
}

// TestWildcardIsRefused: an allowlist entry explains named rows, never a whole
// class, so `*` is refused for every class.
func TestWildcardIsRefused(t *testing.T) {
	for _, class := range []string{"call-no-ts", "call-no-size", "request-tokens"} {
		f := newFixture(t, nil, []string{`UPDATE calls SET ts=NULL WHERE tool_use_id='toolu_A1'`})
		if err := os.WriteFile(f.allow, []byte(class+" | * | invented reason for the test\n"), 0o600); err != nil {
			t.Fatalf("write allowlist: %v", err)
		}
		var out, errOut bytes.Buffer
		code := run([]string{
			"--db", f.db, "--since", "2030-01-01T00:00:00Z", "--until", "2030-01-01T01:00:00Z",
			"--projects", f.projects, "--allowlist", f.allow,
		}, &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), class+": `*` is refused") {
			t.Fatalf("exit %d, want 2 and a refusal naming %s: %s%s", code, class, errOut.String(), out.String())
		}
	}
}

// TestNoTSCallIsExplainedByIDAlone: a call row with no ts is a capture gap per
// call; a `ts<` scope cannot reach it, only its id.
func TestNoTSCallIsExplainedByIDAlone(t *testing.T) {
	f := newFixture(t, nil, []string{`UPDATE calls SET ts=NULL WHERE tool_use_id='toolu_A1'`})
	if err := os.WriteFile(f.allow, []byte("call-no-ts | ts<2031-01-01T00:00:00Z | invented reason for the test\n"), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH call-no-ts ") {
		t.Fatalf("exit %d: a ts scope explained a row without a ts:\n%s", code, out)
	}
	if err := os.WriteFile(f.allow, []byte("call-no-ts | toolu_A1 | invented reason for the test\n"), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	code, got := f.run(t, "2030-01-01T00:00:00Z")
	if code != 0 || !strings.Contains(got, "EXPECTED call-no-ts session="+fxSession+" ids=toolu_A1") {
		t.Fatalf("exit %d, want 0 and the expected call-no-ts line:\n%s", code, got)
	}
}

// TestBatchOnlyCallIsItsOwnClass: a tool a function-hooks plugin answers in
// its own tool.call hook (mcp__sub-agent-compact__compact) fires no PreToolUse,
// PostToolUse or PostToolUseFailure; only PostToolBatch names it, so its row
// has a delivered size and nothing else. That is one class, call-batch-only,
// never call-no-size or call-no-ts, and the allowlist explains it per tool.
func TestBatchOnlyCallIsItsOwnClass(t *testing.T) {
	batchOnly := `UPDATE calls SET failed=NULL, bytes_real=NULL, bytes_delivered=95 WHERE tool_use_id='toolu_A1'`
	for _, c := range []struct {
		name string
		sql  []string
	}{
		{name: "batch set the ts", sql: []string{batchOnly}},
		{name: "a legacy row without a ts", sql: []string{batchOnly, `UPDATE calls SET ts=NULL WHERE tool_use_id='toolu_A1'`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, c.sql)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != 1 || !strings.Contains(out, "MISMATCH call-batch-only session="+fxSession+" ids=toolu_A1,tool=Bash") {
				t.Fatalf("exit %d, want 1 and a call-batch-only mismatch naming its tool:\n%s", code, out)
			}
			for _, class := range []string{"call-no-size", "call-no-ts"} {
				if strings.Contains(out, " "+class+" ") {
					t.Errorf("a batch-only call also reads as %s:\n%s", class, out)
				}
			}
		})
	}
	t.Run("explained per tool, the tool name exact", func(t *testing.T) {
		f := newFixture(t, nil, []string{batchOnly})
		if err := os.WriteFile(f.allow, []byte("call-batch-only | tool=Bas | a prefix of the tool name\n"), 0o600); err != nil {
			t.Fatalf("write allowlist: %v", err)
		}
		if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || strings.Contains(out, "EXPECTED call-batch-only") {
			t.Fatalf("exit %d: a tool-name prefix explained the call:\n%s", code, out)
		}
		if err := os.WriteFile(f.allow, []byte("call-batch-only | tool=Bash | invented reason for the test\n"), 0o600); err != nil {
			t.Fatalf("write allowlist: %v", err)
		}
		code, out := f.run(t, "2030-01-01T00:00:00Z")
		if code != 0 || !strings.Contains(out, "EXPECTED call-batch-only session="+fxSession+" ids=toolu_A1,tool=Bash") || !strings.Contains(out, "expected: invented reason for the test") {
			t.Fatalf("exit %d, want 0 and the expected line:\n%s", code, out)
		}
	})
}

// TestTSScopeExplainsOnlyOlderRows: `ts<{time}` explains a mismatch whose row is
// older than the time, and nothing at or after it. Each per-row class carries
// its row's time: a call's, a request's, an untracked message's, an agent
// turn's, and token-sum the latest request behind it.
func TestTSScopeExplainsOnlyOlderRows(t *testing.T) {
	cases := []struct {
		name, class string
		extraMain   []string
		sql         []string
		rowSec      float64
	}{
		{name: "call", class: "call-no-size", sql: []string{`UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_A1'`}, rowSec: 2},
		{name: "request", class: "request-tokens", sql: []string{`UPDATE requests SET output_tokens=5 WHERE request_id='msg_B1'`}, rowSec: 6},
		{name: "token sum", class: "token-sum", sql: []string{`UPDATE requests SET output_tokens=5 WHERE request_id='msg_B1'`}, rowSec: 6},
		{name: "untracked message", class: "request-untracked", extraMain: textOnlyTurn(t), sql: textOnlyStop(), rowSec: 22},
		{name: "agent turn", class: "agent-turn-no-start", sql: []string{`UPDATE agent_turns SET started=NULL`}, rowSec: 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, tc := range []struct {
				bound    float64
				explains bool
			}{{c.rowSec + 0.5, true}, {c.rowSec, false}} {
				f := newFixture(t, c.extraMain, c.sql)
				entry := fmt.Sprintf("%s | ts<%s | invented reason for the test\n", c.class, fxTS(tc.bound))
				if err := os.WriteFile(f.allow, []byte(entry), 0o600); err != nil {
					t.Fatalf("write allowlist: %v", err)
				}
				_, out := f.run(t, "2030-01-01T00:00:00Z")
				if got := strings.Contains(out, "EXPECTED "+c.class+" "); got != tc.explains {
					t.Errorf("bound %s: explained %t, want %t:\n%s", fxTS(tc.bound), got, tc.explains, out)
				}
				if !tc.explains && !strings.Contains(out, "MISMATCH "+c.class+" ") {
					t.Errorf("bound %s: no %s mismatch:\n%s", fxTS(tc.bound), c.class, out)
				}
			}
		})
	}
}

// textOnlyTurn is a second turn ending in a text-only reply (msg_A3 at 22 s);
// textOnlyStop is its Stop, so the reply's request row is due.
func textOnlyTurn(t *testing.T) []string {
	return []string{
		assistantLine(t, 22, "msg_A3", 9, "end_turn", map[string]any{"type": "text", "text": fxSecret}),
		jsonLine(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": fxTS(23), "sessionId": fxSession, "entrypoint": "cli"}),
	}
}

// textOnlyStop ends the text-only turn; a later row moves the snapshot past
// hookLag, so the turn's sweep is due.
func textOnlyStop() []string {
	return []string{
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e7', 'Stop', %d, '%s')`, fxMS(23), fxSession),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e-later', 'Notification', %d, '%s')`, fxMS(60), fxSession),
	}
}

// TestSweptBatchOnlyCallStaysBatchOnly: the Stop and SessionEnd sweep
// (resolveUnfinished) fills a batch-only call's failed=0 from its transcript
// result, never its real size; the row is still the batch's alone.
func TestSweptBatchOnlyCallStaysBatchOnly(t *testing.T) {
	f := newFixture(t, nil, []string{`UPDATE calls SET failed=0, bytes_real=NULL, bytes_delivered=95 WHERE tool_use_id='toolu_A1'`})
	code, out := f.run(t, "2030-01-01T00:00:00Z")
	if code != 1 || !strings.Contains(out, "MISMATCH call-batch-only session="+fxSession+" ids=toolu_A1,tool=Bash") || strings.Contains(out, " call-no-size ") {
		t.Fatalf("exit %d, want a call-batch-only mismatch and no call-no-size:\n%s", code, out)
	}
}

// TestKilledStopFailureHookMatchesItsRow: the wrapper's `StopFailure: killed by
// signal` (no session) is expected only beside a StopFailure row of a session
// whose SessionEnd follows the kill, one row per fault.
func TestKilledStopFailureHookMatchesItsRow(t *testing.T) {
	for _, suffix := range []string{"", " (pid 4242)"} {
		t.Run("suffix"+suffix, func(t *testing.T) {
			apiError := jsonLine(t, map[string]any{"type": "assistant", "timestamp": fxTS(22), "sessionId": fxSession, "isApiErrorMessage": true, "message": map[string]any{"id": "msg_err", "model": "<synthetic>", "content": []map[string]any{}}})
			stopFailure := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, error_type, detail) VALUES('e8', 'StopFailure', %d, '%s', 'model_not_found', '{"from_transcript":true}')`, fxMS(22), fxSession)
			sessionEnd := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('e9', 'SessionEnd', %d, '%s', 'other')`, fxMS(22.6), fxSession)
			fault := func(sec float64, text string) string {
				return fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', '%s')`, fxMS(sec), text+suffix)
			}
			for _, c := range []struct {
				name, want string
				extraMain  []string
				sql        []string
			}{
				{name: "beside its row and its session's end", want: "EXPECTED fault-binary", extraMain: []string{apiError}, sql: []string{stopFailure, sessionEnd, fault(22, "StopFailure: killed by signal")}},
				{name: "no StopFailure row near it", want: "MISMATCH fault-binary", sql: []string{sessionEnd, fault(22, "StopFailure: killed by signal")}},
				{name: "two kills, one row", want: "MISMATCH fault-binary", extraMain: []string{apiError}, sql: []string{stopFailure, sessionEnd, fault(22, "StopFailure: killed by signal"), fault(22.001, "StopFailure: killed by signal")}},
				{name: "another event's kill", want: "MISMATCH fault-binary", extraMain: []string{apiError}, sql: []string{stopFailure, sessionEnd, fault(22, "Stop: killed by signal")}},
			} {
				t.Run(c.name, func(t *testing.T) {
					f := newFixture(t, c.extraMain, c.sql)
					_, out := f.run(t, "2030-01-01T00:00:00Z")
					if !strings.Contains(out, c.want+" ") {
						t.Fatalf("want %s:\n%s", c.want, out)
					}
				})
			}
		})
	}
}

// TestPromptlessLaunchHasNoTranscript: a `claude -p` given no prompt fires
// SessionStart and SessionEnd, exits 1 and never writes its transcript.
func TestReconcileSessionThatRanNothingIsExpected(t *testing.T) {
	const reason = "the session started and ran nothing: only idle lifecycle events, no calls, requests, agents or turns, and no fault or lost event that could hide work (internal/callmeter/report/sessions.go ranNothing)"
	for _, c := range []struct {
		name, source string
		sql          []string
		mismatch     bool
	}{
		{name: "SessionStart InstructionsLoaded SessionEnd", source: "startup"},
		{name: "resume", source: "resume"},
		{name: "clear", source: "clear"},
		{name: "fork", source: "fork"},
		{name: "Notification", source: "startup", sql: []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('notice', 'Notification', %d, '%s')`, fxMS(30.5), fxSession2)}},
		{name: "model already selected", source: "startup", sql: []string{fmt.Sprintf(`UPDATE sessions SET model='claude-demo' WHERE session_id='%s'`, fxSession2)}},
		{name: "UserPromptSubmit", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('prompt', 'UserPromptSubmit', %d, '%s')`, fxMS(30.5), fxSession2)}},
		{name: "calls", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, ts) VALUES('idle-call', '%s', %d)`, fxSession2, fxMS(30.5))}},
		{name: "requests", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts) VALUES('idle-request', '%s', %d)`, fxSession2, fxMS(30.5))}},
		{name: "agents", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO agents(agent_id, session_id, started) VALUES('idle-agent', '%s', %d)`, fxSession2, fxMS(30.5))}},
		{name: "turns with event", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO turns(event_id, event, session_id, ts) VALUES('idle-load', 'InstructionsLoaded', '%s', %d)`, fxSession2, fxMS(30.5))}},
		{name: "turns without event", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO turns(event_id, event, session_id, ts) VALUES('idle-turn', 'Stop', '%s', %d)`, fxSession2, fxMS(30.5))}},
		{name: "agent turns without event", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO turns(event_id, event, session_id, agent_id, ts) VALUES('idle-turn', 'SubagentStop', '%s', 'idle-agent', %d)`, fxSession2, fxMS(30.5))}},
		{name: "named fault", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id) VALUES(%d, 'transcript', 'open', '%s')`, fxMS(29), fxSession2)}},
		{name: "sessionless binary at start", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signal')`, fxMS(30))}},
		{name: "sessionless binary after start", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signal')`, fxMS(32))}},
		{name: "sessionless terminated at start", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'terminated', 'Stop: panic')`, fxMS(30))}},
		{name: "sessionless terminated after start", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'terminated', 'Stop: panic')`, fxMS(32))}},
		{name: "sessionless fault between starts", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('resumed', 'SessionStart', %d, '%s')`, fxMS(30.8), fxSession2), fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signal')`, fxMS(30.5))}},
		{name: "sessionless binary before start", source: "startup", sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signal')`, fxMS(29))}},
		{name: "sessionless other stage", source: "startup", sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'transcript', 'open')`, fxMS(32))}},
		{name: "another session's fault", source: "startup", sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id) VALUES(%d, 'terminated', 'Stop: panic', '%s')`, fxMS(32), fxSession)}},
		{name: "no SessionStart", source: "startup", mismatch: true, sql: []string{`DELETE FROM events WHERE event_id='idle-start'`}},
		{name: "agent work event", source: "startup", mismatch: true, sql: []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, agent_id) VALUES('agent-work', 'PreToolUse', %d, '%s', 'idle-agent')`, fxMS(30.5), fxSession2)}},
		{name: "agent idle event", source: "startup", sql: []string{fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, agent_id) VALUES('agent-idle', 'Notification', %d, '%s', 'idle-agent')`, fxMS(30.5), fxSession2)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			launch := []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, start_source, end_reason) VALUES('%s', %d, %d, '%s', 'other')`, fxSession2, fxMS(30), fxMS(31), c.source),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('idle-start', 'SessionStart', %d, '%s', '%s')`, fxMS(30), fxSession2, c.source),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('idle-load', 'InstructionsLoaded', %d, '%s')`, fxMS(30.5), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('idle-end', 'SessionEnd', %d, '%s')`, fxMS(31), fxSession2),
			}
			f := newFixture(t, nil, append(launch, c.sql...))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			want := "EXPECTED session-ran-nothing session=" + fxSession2
			if c.mismatch {
				want = "MISMATCH session-no-transcript session=" + fxSession2
				if code != 1 {
					t.Errorf("exit %d, want 1", code)
				}
			} else {
				if !strings.Contains(out, ": expected: "+reason+"\n") {
					t.Errorf("missing ran-nothing reason:\n%s", out)
				}
			}
			if !strings.Contains(out, want) {
				t.Fatalf("want %q:\n%s", want, out)
			}
		})
	}
}

// The killed launch writes a user prompt with promptId but no assistant line,
// alongside SessionStart, UserPromptSubmit and SessionEnd hook rows.
func TestKilledBeforeFirstReplyIsExpected(t *testing.T) {
	const reason = "the session was killed before the model's first reply: the store holds only its SessionStart, UserPromptSubmit and SessionEnd events, and its transcript holds a prompt and no assistant line (a launch killed or cleared mid-request)"
	for _, tc := range []struct {
		name, extraEvent   string
		noPrompt, noEvents bool
	}{
		{name: "killed after prompt"},
		{name: "Stop recorded", extraEvent: "Stop"},
		{name: "InstructionsLoaded recorded", extraEvent: "InstructionsLoaded"},
		{name: "no prompt", noPrompt: true},
		{name: "no events", noEvents: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, start_source, end_reason) VALUES('%s', %d, %d, 'startup', 'other')`, fxSession2, fxMS(30), fxMS(31)),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('p1', 'SessionStart', %d, '%s', 'startup')`, fxMS(30), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('p2', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(30.5), fxSession2, fxPrompt),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('p3', 'SessionEnd', %d, '%s', 'other')`, fxMS(31), fxSession2),
			}
			if tc.extraEvent != "" {
				rows = append(rows, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('p4', '%s', %d, '%s')`, tc.extraEvent, fxMS(30.8), fxSession2))
			}
			if tc.noEvents {
				rows = append(rows, fmt.Sprintf(`DELETE FROM events WHERE session_id='%s'`, fxSession2))
			}
			f := newFixture(t, nil, rows)
			path := filepath.Join(f.projects, "-tmp-demo-proj", fxSession2+".jsonl")
			lines := []string{jsonLine(t, map[string]any{"type": "user", "timestamp": fxTS(30.5), "sessionId": fxSession2, "entrypoint": "cli", "promptId": fxPrompt,
				"message": map[string]any{"role": "user", "content": fxSecret}})}
			if tc.noPrompt {
				lines = []string{jsonLine(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": fxTS(30.5), "sessionId": fxSession2})}
			}
			writeLines(t, path, lines)
			store, err := callmeter.OpenDB(context.Background(), f.db)
			if err != nil {
				t.Fatalf("open fixture store: %v", err)
			}
			_, updateErr := store.DB().Exec(`UPDATE sessions SET transcript_path=? WHERE session_id=?`, path, fxSession2)
			closeErr := store.Close()
			if updateErr != nil || closeErr != nil {
				t.Fatalf("set transcript path: %v; close store: %v", updateErr, closeErr)
			}
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			wantCode := 0
			want := "EXPECTED session-promptless session=" + fxSession2 + " model=- start_source=startup events=SessionEnd:1,SessionStart:1,UserPromptSubmit:1: expected: " + reason + "\n"
			if tc.extraEvent != "" || tc.noPrompt || tc.noEvents {
				wantCode, want = 1, "MISMATCH session-promptless session="+fxSession2
			}
			if code != wantCode || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, wantCode, want, out)
			}
		})
	}
}

// TestPromptlessLaunchHasNoTranscript: a `claude -p` given no prompt fires
// SessionStart and SessionEnd, exits 1 and never writes its transcript.
func TestPromptlessLaunchHasNoTranscript(t *testing.T) {
	launch := []string{
		fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, start_source, end_reason, transcript_path) VALUES('%s', %d, %d, 'startup', 'other', '/nonexistent/%s.jsonl')`, fxSession2, fxMS(30), fxMS(31), fxSession2),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('p1', 'SessionStart', %d, '%s', 'startup')`, fxMS(30), fxSession2),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('p2', 'SessionEnd', %d, '%s', 'other')`, fxMS(31), fxSession2),
	}
	f := newFixture(t, nil, launch)
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || !strings.Contains(out, "EXPECTED session-ran-nothing session="+fxSession2) {
		t.Fatalf("exit %d, want 0 and an expected session-ran-nothing:\n%s", code, out)
	}
	prompted := append(launch, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('p3', 'UserPromptSubmit', %d, '%s')`, fxMS(30.5), fxSession2))
	f = newFixture(t, nil, prompted)
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH session-no-transcript session="+fxSession2) {
		t.Fatalf("exit %d, want 1: a prompted session without a transcript stays a mismatch:\n%s", code, out)
	}
}

// TestPythonScriptParts: a script-body part (python3 - <<EOF, parser 6 on) is
// expected by design; a python part a parser before 6 marked unparsed is
// expected under its own class, since that parser did not tell a cut heredoc
// body from unresolved code; from parser 6 on an unparsed python part is a
// -c or here-string holding an unresolved expansion, expected under its own
// class. A shell part unparsed stays a mismatch (TestSeededMismatchIsCaught).
func TestPythonScriptParts(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
		code            int
		absent          []string
	}{
		{"script-body at parser 6", `UPDATE command_parts SET parse_status='script-body', lang='python', program='python3', parser=6`,
			"EXPECTED parse-script-body session=" + fxSession + " ids=toolu_A1", 0, []string{"parse-unparsed", "MISMATCH parse-script-body", "parts-stale"}},
		{"unparsed python before parser 6", `UPDATE command_parts SET parse_status='unparsed', lang='python', program='python3', parser=5`,
			"EXPECTED parse-python-unparsed-pre6 session=" + fxSession + " ids=toolu_A1", 0, []string{"MISMATCH", "parse-script-body"}},
		{"unparsed python at parser 6", `UPDATE command_parts SET parse_status='unparsed', lang='python', program='python3', parser=6`,
			"EXPECTED parse-python-code-unresolved session=" + fxSession + " ids=toolu_A1", 0, []string{"MISMATCH", "parse-unparsed", "parse-script-body", "parse-python-unparsed-pre6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, []string{tc.sql})
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, tc.code, tc.want, out)
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Fatalf("output holds %q:\n%s", a, out)
				}
			}
		})
	}
}

// TestSessionNoEndHook: a session whose latest run has no SessionEnd is the
// class session-no-end-hook, EXPECTED only when the store carries end_reason
// never and the transcript holds no SessionEnd hook entry; a run quiet less
// than an hour is PENDING, and never by wildcard. One whose store carries
// end_reason lost (its SessionEnd hook ran and was killed) is the EXPECTED
// class session-end-killed, with or without the transcript's hook entry; one
// still NULL whose transcript ran the end hook stays a MISMATCH.
func TestSessionNoEndHook(t *testing.T) {
	endHook := jsonLine(t, map[string]any{
		"type": "attachment", "timestamp": fxTS(22), "sessionId": fxSession, "entrypoint": "cli",
		"attachment": map[string]any{"type": "hook_success", "hookEvent": "SessionEnd"},
	})
	noEnd := `DELETE FROM events WHERE event='SessionEnd'`
	never := `UPDATE sessions SET end_reason='never'`
	lost := `UPDATE sessions SET end_reason='lost'`
	unmarked := `UPDATE sessions SET end_reason=NULL`
	// a row of another session an hour and more after this one's last row
	later := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('z1', 'SessionStart', %d, '%s')`, fxMS(21+3700), fxSession2)
	resumed := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('z2', 'SessionStart', %d, '%s', 'resume')`, fxMS(22), fxSession)
	for _, tc := range []struct {
		name      string
		extraMain []string
		sql       []string
		code      int
		want      []string
		absent    []string
	}{
		{"never marked, no end hook in the transcript", nil, []string{noEnd, never}, 0,
			[]string{"EXPECTED session-no-end-hook session=" + fxSession + " end_reason=never: expected: " + reasonNoEndHook}, []string{"MISMATCH", "PENDING"}},
		{"never marked, but the transcript ran the end hook", []string{endHook}, []string{noEnd, never}, 1,
			[]string{"MISMATCH session-no-end-hook session=" + fxSession + " transcript_end_hook=true end_reason=never"}, []string{"EXPECTED session-no-end-hook"}},
		{"lost, the transcript ran the end hook", []string{endHook}, []string{noEnd, lost}, 0,
			[]string{"EXPECTED session-end-killed session=" + fxSession + " transcript_end_hook=true end_reason=lost: expected: " + reasonEndKilled},
			[]string{"MISMATCH", "PENDING", "session-no-end-hook"}},
		{"lost, no end hook in the transcript", nil, []string{noEnd, lost}, 0,
			[]string{"EXPECTED session-end-killed session=" + fxSession + " transcript_end_hook=false end_reason=lost"},
			[]string{"MISMATCH", "PENDING", "session-no-end-hook"}},
		{"unmarked, but the transcript ran the end hook", []string{endHook}, []string{noEnd, unmarked}, 1,
			[]string{"MISMATCH session-no-end-hook session=" + fxSession + " transcript_end_hook=true end_reason=-"},
			[]string{"EXPECTED session-no-end-hook", "session-end-killed"}},
		{"unmarked, quiet less than an hour", nil, []string{noEnd}, 0,
			[]string{"PENDING session-no-end-hook-live session=" + fxSession + " end_reason=-"}, []string{"MISMATCH", "EXPECTED session-no-end-hook"}},
		{"unmarked, quiet past an hour", nil, []string{noEnd, later}, 1,
			[]string{"MISMATCH session-no-end-hook session=" + fxSession + " end_reason=- quiet past QuietAfter and unmarked"}, []string{"EXPECTED session-no-end-hook", "PENDING session-no-end-hook-live"}},
		{"ended after its latest start", nil, nil, 0,
			nil, []string{"session-no-end-hook"}},
		{"resumed after an end, never marked", nil, []string{resumed, never}, 0,
			[]string{"EXPECTED session-no-end-hook session=" + fxSession + " end_reason=never"}, []string{"MISMATCH"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.extraMain, tc.sql)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if strings.Count(out, w) != 1 {
					t.Fatalf("want exactly one %q:\n%s", w, out)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Fatalf("output holds %q:\n%s", a, out)
				}
			}
		})
	}
}

// TestReloadedChatHasNoSessionStart: a chat already running when a
// /reload-plugins loaded the hooks into it fires no SessionStart; its
// transcript holds that command before the session's first store row.
func TestReloadedChatHasNoSessionStart(t *testing.T) {
	// Synthetic lines in the real shape (Claude Code 2.1.283 to 2.1.287, seen in
	// session 3412b620): a typed /reload-plugins is a user line whose string
	// content opens <command-name>/reload-plugins</command-name>, followed by a
	// system local_command line.
	reload := func(sec float64) []string {
		return []string{
			jsonLine(t, map[string]any{
				"type": "user", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli",
				"message": map[string]any{"role": "user", "content": "<command-name>/reload-plugins</command-name>\n<command-message>reload-plugins</command-message>\n<command-args></command-args>"},
			}),
			jsonLine(t, map[string]any{"type": "system", "subtype": "local_command", "timestamp": fxTS(sec + 0.1), "sessionId": fxSession, "entrypoint": "cli", "level": "info", "content": "<local-command-stdout>reloaded</local-command-stdout>"}),
		}
	}
	noStart := `DELETE FROM events WHERE event='SessionStart'`
	for _, tc := range []struct {
		name      string
		extraMain []string
		sql       []string
		code      int
		want      []string
		absent    []string
	}{
		{"reloaded before its first store row", reload(0.2), []string{noStart}, 0,
			[]string{"EXPECTED event-no-sessionstart session=" + fxSession + " session_end=1 stop=1 reload_plugins=2030-01-01T00:00:00Z first_row=2030-01-01T00:00:00Z: expected: "},
			[]string{"MISMATCH"}},
		{"reloaded only after its first store row", reload(22), []string{noStart}, 1,
			[]string{"MISMATCH event-no-sessionstart session=" + fxSession + " session_end=1 stop=1"},
			[]string{"EXPECTED event-no-sessionstart"}},
		{"never reloaded", nil, []string{noStart}, 1,
			[]string{"MISMATCH event-no-sessionstart session=" + fxSession + " session_end=1 stop=1"},
			[]string{"EXPECTED event-no-sessionstart"}},
		{"reloaded, its SessionStart stored", reload(0.2), nil, 0,
			nil, []string{"event-no-sessionstart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.extraMain, tc.sql)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if strings.Count(out, w) != 1 {
					t.Fatalf("want exactly one %q:\n%s", w, out)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Fatalf("output holds %q:\n%s", a, out)
				}
			}
		})
	}
}

// TestHookLagIsPending: a transcript entry within hookLag of the store's
// latest row may still have its async hook in flight: PENDING, not a mismatch.
func TestHookLagIsPending(t *testing.T) {
	missing := `DELETE FROM requests WHERE request_id='msg_B1'`
	f := newFixture(t, nil, []string{missing, fmt.Sprintf(`UPDATE events SET ts=%d WHERE event IN ('Stop', 'SessionEnd')`, fxMS(12))})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || !strings.Contains(out, "PENDING request-missing-lag ") || strings.Contains(out, "MISMATCH") {
		t.Fatalf("exit %d, want 0 and a request-missing-lag PENDING:\n%s", code, out)
	}
	f = newFixture(t, nil, []string{missing})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH request-missing ") {
		t.Fatalf("exit %d, want 1: a row older than hookLag stays a mismatch:\n%s", code, out)
	}
}

// TestRedactedCommandHasNoParts: a Bash call whose stored input keeps only the
// command's size (the command itself was not stored) can never be parsed.
func TestRedactedCommandHasNoParts(t *testing.T) {
	f := newFixture(t, nil, []string{`UPDATE calls SET input='{"command_bytes":12,"description_bytes":3}' WHERE tool_use_id='toolu_A1'`, `UPDATE command_parts SET tool_use_id='toolu_B1'`})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || !strings.Contains(out, "EXPECTED parts-command-not-stored session="+fxSession+" ids=toolu_A1") {
		t.Fatalf("exit %d, want 0 and an expected parts-command-not-stored:\n%s", code, out)
	}
	f = newFixture(t, nil, []string{`UPDATE command_parts SET tool_use_id='toolu_B1'`})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH parts-missing ") {
		t.Fatalf("exit %d, want 1: a stored command without parts stays parts-missing:\n%s", code, out)
	}
}

// TestKilledUnknownHookIsVouchedByItsNeighbours: the wrapper's `unknown: killed
// by signal` names neither its event nor its session. It is expected only when
// every session with a store row within ±1 s of its second was compared (in the
// window, or over its whole history when it began before --since) and holds no
// unexplained mismatch, so a lost event would surface on that session instead;
// with no such session it stays a mismatch.
func TestKilledUnknownHookIsVouchedByItsNeighbours(t *testing.T) {
	for _, suffix := range []string{"", " (pid 4242)"} {
		t.Run("suffix"+suffix, func(t *testing.T) {
			const fxOrphan = "11111111-aaaa-4bbb-8ccc-00000000000f" // event rows, no sessions row
			fault := func(sec float64) string {
				return fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signal%s')`, fxMS(sec), suffix)
			}
			// fxSession's Stop at 21 lies within ±1 s of a kill in second 22.
			dropPrompt := `DELETE FROM events WHERE event_id = 'e2'`
			// The idle neighbour starts after the lost event: a loss from its start
			// on would leave its lack of work unknown under the product's rule.
			launch := []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, start_source, end_reason, transcript_path) VALUES('%s', %d, %d, 'startup', 'other', '/nonexistent/%s.jsonl')`, fxSession2, fxMS(30.5), fxMS(31), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('p1', 'SessionStart', %d, '%s', 'startup')`, fxMS(30.5), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('p2', 'SessionEnd', %d, '%s', 'other')`, fxMS(31), fxSession2),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('n1', 'Notification', %d, '%s')`, fxMS(30.5), fxSession),
				fault(30),
			}
			for _, c := range []struct {
				name, since, allow, want string
				sql                      []string
			}{
				{name: "signal suffix is not a kill", want: "MISMATCH fault-binary session=-", sql: []string{fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'binary', 'unknown: killed by signalx')`, fxMS(22))}},
				{name: "every neighbour reconciles", want: "EXPECTED fault-binary session=- event=unknown", sql: []string{fault(22)}},
				{name: "a neighbour has an unexplained mismatch", want: "MISMATCH fault-binary session=- event=unknown sessions_near=1 unvouched=" + fxSession + ":unexplained", sql: []string{dropPrompt, fault(22)}},
				{name: "its neighbour's mismatch is allowlisted", allow: "event-prompt-missing | 22222222 | seeded\n", want: "EXPECTED fault-binary session=- event=unknown", sql: []string{dropPrompt, fault(22)}},
				{name: "no session beside it", want: "MISMATCH fault-binary session=- event=unknown sessions_near=0", sql: []string{fault(40)}},
				{name: "every neighbour compared", want: "EXPECTED fault-binary session=- event=unknown", sql: launch},
				// The window limits which rows are judged, not which evidence is read:
				// a neighbour first recorded before --since vouches by its whole history.
				{name: "a neighbour before the window reconciles", since: "2030-01-01T00:00:25Z", want: "EXPECTED fault-binary session=- event=unknown", sql: launch},
				{name: "a neighbour before the window has an unexplained mismatch", since: "2030-01-01T00:00:25Z", want: "MISMATCH fault-binary session=- event=unknown sessions_near=2 unvouched=" + fxSession + ":unexplained", sql: append([]string{dropPrompt}, launch...)},
				{name: "a neighbour with no session row", want: "MISMATCH fault-binary session=- event=unknown sessions_near=2 unvouched=" + fxOrphan + ":not-compared", sql: []string{fault(22), fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('g1', 'Notification', %d, '%s')`, fxMS(22.5), fxOrphan)}},
			} {
				t.Run(c.name, func(t *testing.T) {
					f := newFixture(t, nil, c.sql)
					if c.allow != "" {
						if err := os.WriteFile(f.allow, []byte(c.allow), 0o600); err != nil {
							t.Fatalf("write allowlist: %v", err)
						}
					}
					since := c.since
					if since == "" {
						since = "2030-01-01T00:00:00Z"
					}
					_, out := f.run(t, since)
					if !strings.Contains(out, c.want+" ") {
						t.Fatalf("want %q:\n%s", c.want, out)
					}
				})
			}
		})
	}
}

// TestTerminatedStopNeedsItsTurnsStop: the binary's `Stop: terminated by
// SIGTERM` (no session) is a headless exit cancelling the async Stop hook. It is
// expected only beside a session ending right after it whose cancelled turn has
// a Stop row (hook, or rebuilt from the transcript), one per fault, while no
// other session ending there leaves its turn open.
func TestTerminatedStopNeedsItsTurnsStop(t *testing.T) {
	fault := fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'terminated', 'Stop: terminated by SIGTERM')`, fxMS(22))
	sessionEnd := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('e9', 'SessionEnd', %d, '%s', 'other')`, fxMS(22.6), fxSession)
	rebuilt := `UPDATE events SET detail = '{"from_transcript":true}' WHERE event_id = 'e5'`
	dropStop := `DELETE FROM events WHERE event_id = 'e5'`
	laterPrompt := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e10', 'UserPromptSubmit', %d, '%s', 'later-prompt')`, fxMS(21.5), fxSession)
	other := func(closing string) []string {
		rows := []string{
			fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('o1', 'UserPromptSubmit', %d, '%s')`, fxMS(20), fxSession2),
			fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('o2', 'SessionEnd', %d, '%s', 'other')`, fxMS(22.8), fxSession2),
		}
		if closing != "" {
			rows = append(rows, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('o3', '%s', %d, '%s')`, closing, fxMS(21.5), fxSession2))
		}
		return rows
	}
	expected := "EXPECTED fault-terminated session=" + fxSession + " event=Stop"
	cases := []struct {
		name, want string
		sql        []string
	}{
		{name: "its turn has a Stop rebuilt from the transcript", want: expected, sql: []string{rebuilt, sessionEnd, fault}},
		{name: "its turn has a hook Stop", want: expected, sql: []string{sessionEnd, fault}},
		{name: "its turn has no Stop", want: "MISMATCH fault-terminated session=- event=Stop sessions_ending=1 open_turn=" + fxSession, sql: []string{dropStop, sessionEnd, fault}},
		{name: "the Stop closes an earlier turn", want: "MISMATCH fault-terminated session=- event=Stop sessions_ending=1 open_turn=" + fxSession, sql: []string{laterPrompt, sessionEnd, fault}},
		{name: "no session ends after it", want: "MISMATCH fault-terminated session=- event=Stop", sql: []string{fault}},
		{name: "two faults, one Stop", want: "MISMATCH fault-terminated session=- event=Stop", sql: []string{sessionEnd, fault, fault}},
		{name: "another ending session's turn is open", want: "MISMATCH fault-terminated session=- event=Stop sessions_ending=2 open_turn=" + fxSession2, sql: append([]string{sessionEnd, fault}, other("")...)},
		{name: "another ending session's turn closed in a StopFailure", want: expected, sql: append([]string{sessionEnd, fault}, other("StopFailure")...)},
	}
	for _, reason := range []string{"terminated by SIGTERM", "store unavailable: full", "panic"} {
		for _, c := range cases {
			t.Run(c.name+"/"+reason, func(t *testing.T) {
				sql := make([]string, len(c.sql))
				for i, q := range c.sql {
					sql[i] = strings.ReplaceAll(q, "Stop: terminated by SIGTERM", "Stop: "+reason)
				}
				f := newFixture(t, nil, sql)
				_, out := f.run(t, "2030-01-01T00:00:00Z")
				if !strings.Contains(out, c.want+" ") {
					t.Fatalf("want %q:\n%s", c.want, out)
				}
			})
		}
	}
}

// TestTerminatedStopNamingItsSession: a `Stop: terminated by …` fault naming
// its session (`store busy`, or a signal after the payload was read) pairs with
// that session's turn alone, no SessionEnd needed: expected when a Stop row
// closes the turn, a mismatch naming the session when the turn is open, already
// paired, or the session is absent from the store. A sessionless fault beside
// it keeps its own window pairing.
func TestTerminatedStopNamingItsSession(t *testing.T) {
	named := func(session string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id) VALUES(%d, 'terminated', 'Stop: terminated by store busy', '%s')`, fxMS(22), session)
	}
	sessionless := fmt.Sprintf(`INSERT INTO faults(ts, stage, error) VALUES(%d, 'terminated', 'Stop: terminated by SIGTERM')`, fxMS(22))
	sessionEnd := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, reason) VALUES('e9', 'SessionEnd', %d, '%s', 'other')`, fxMS(22.6), fxSession)
	rebuilt := `UPDATE events SET detail = '{"from_transcript":true}' WHERE event_id = 'e5'`
	dropStop := `DELETE FROM events WHERE event_id = 'e5'`
	other := []string{
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('o1', 'UserPromptSubmit', %d, '%s')`, fxMS(20), fxSession2),
		fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('o3', 'Stop', %d, '%s')`, fxMS(21.5), fxSession2),
	}
	const absent = "11111111-aaaa-4bbb-8ccc-0000000000ff"
	expected := "EXPECTED fault-terminated session=" + fxSession + " event=Stop"
	for _, c := range []struct {
		name string
		want []string
		sql  []string
	}{
		{name: "its session's turn has a hook Stop", want: []string{expected + " at=2030-01-01T00:00:22Z: expected: " + reasonTerminatedStopNamed}, sql: []string{named(fxSession)}},
		{name: "its session's turn has a Stop rebuilt from the transcript", want: []string{expected}, sql: []string{rebuilt, named(fxSession)}},
		{name: "its session is absent from the store", want: []string{"MISMATCH fault-terminated session=" + absent + " event=Stop session_in_store=false"}, sql: []string{named(absent)}},
		{name: "its session's turn has no Stop", want: []string{"MISMATCH fault-terminated session=" + fxSession + " event=Stop open_turn=" + fxSession}, sql: []string{dropStop, named(fxSession)}},
		{name: "two faults, one Stop", want: []string{expected, "MISMATCH fault-terminated session=" + fxSession + " event=Stop unpaired_stop=0"}, sql: []string{named(fxSession), named(fxSession)}},
		{name: "a sessionless fault beside it", want: []string{expected, "EXPECTED fault-terminated session=" + fxSession2 + " event=Stop"}, sql: append([]string{sessionEnd, sessionless, named(fxSession2)}, other...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, c.sql)
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Fatalf("want %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, "MISMATCH fault-terminated session=- ") {
				t.Fatalf("a sessionless verdict for a named fault:\n%s", out)
			}
		})
	}
}

// TestLostHookFaultNamingItsSession: a SessionEnd fault naming its session (the
// binary's `terminated by …`, or the wrapper's `binary` line) is expected only
// when the session's latest run carries end_reason lost, the fault within that
// run; a StopFailure one only when a StopFailure row closes the session's turn.
// Every other one stays a mismatch naming its session.
func TestLostHookFaultNamingItsSession(t *testing.T) {
	fault := func(stage, err string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id) VALUES(%d, '%s', '%s', '%s')`, fxMS(22), stage, err, fxSession)
	}
	endTerminated := fault("terminated", "SessionEnd: terminated by store busy")
	endKilled := fault("binary", "SessionEnd: killed by signal")
	failTerminated := fault("terminated", "StopFailure: terminated by store busy")
	lost := `UPDATE sessions SET end_reason='lost'`
	unmarked := `UPDATE sessions SET end_reason=NULL`
	resumed := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('z2', 'SessionStart', %d, '%s', 'resume')`, fxMS(23), fxSession)
	stopFailure := `UPDATE events SET event='StopFailure' WHERE event_id='e5'`
	at := " at=2030-01-01T00:00:22Z"
	cases := []struct {
		name         string
		sql          []string
		want, absent string
	}{
		{"terminated SessionEnd, end_reason lost", []string{lost, endTerminated},
			"EXPECTED fault-terminated session=" + fxSession + " event=SessionEnd end_reason=lost" + at + ": expected: " + reasonEndKilled, "MISMATCH fault-"},
		{"wrapper SessionEnd, end_reason lost", []string{lost, endKilled},
			"EXPECTED fault-binary session=" + fxSession + " event=SessionEnd end_reason=lost" + at, "MISMATCH fault-"},
		{"terminated SessionEnd, end_reason NULL", []string{unmarked, endTerminated},
			"MISMATCH fault-terminated session=" + fxSession + at, "EXPECTED fault-"},
		{"terminated SessionEnd before the lost run's start", []string{lost, resumed, endTerminated},
			"MISMATCH fault-terminated session=" + fxSession + at, "EXPECTED fault-"},
		{"terminated StopFailure, its turn has a StopFailure row", []string{stopFailure, failTerminated},
			"EXPECTED fault-terminated session=" + fxSession + " event=StopFailure" + at, "MISMATCH fault-"},
		{"terminated StopFailure, its turn has none", []string{failTerminated},
			"MISMATCH fault-terminated session=" + fxSession + " event=StopFailure unpaired_stopfailure=0" + at, "EXPECTED fault-"},
		{"SessionEnd invalid terminated reason", []string{lost, fault("terminated", "SessionEnd: panic later")},
			"MISMATCH fault-terminated session=" + fxSession + at, "EXPECTED fault-"},
		{"StopFailure invalid terminated reason", []string{stopFailure, fault("terminated", "StopFailure: panic later")},
			"MISMATCH fault-terminated session=" + fxSession + at, "EXPECTED fault-"},
	}
	for _, event := range []string{"StopFailure", "SessionEnd"} {
		for _, reason := range []string{"terminated by store busy", "store unavailable: full", "panic"} {
			for _, paired := range []bool{true, false} {
				sql := []string{fault("terminated", event+": "+reason)}
				want := "MISMATCH fault-terminated session=" + fxSession
				absent := "EXPECTED fault-terminated"
				if paired {
					absent = "MISMATCH fault-terminated"
					if event == "SessionEnd" {
						sql = append(sql, lost)
						want = "EXPECTED fault-terminated session=" + fxSession + " event=SessionEnd end_reason=lost" + at + ": expected: " + reasonEndKilled
					} else {
						sql = append(sql, stopFailure)
						want = "EXPECTED fault-terminated session=" + fxSession + " event=StopFailure" + at + ": expected: " + reasonTerminatedFailureNamed
					}
				} else if event == "SessionEnd" {
					sql = append(sql, unmarked)
					want += at
				} else {
					want += " event=StopFailure unpaired_stopfailure=0" + at
				}
				cases = append(cases, struct {
					name         string
					sql          []string
					want, absent string
				}{fmt.Sprintf("%s %s paired=%t", event, reason, paired), sql, want, absent})
			}
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, c.sql)
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			if !strings.Contains(out, c.want) || strings.Contains(out, c.absent) {
				t.Fatalf("want %q, never %q:\n%s", c.want, c.absent, out)
			}
		})
	}
}

// TestLostHookPairsWithItsRow: a SubagentStop, PreToolUse, PostToolUse,
// PostToolUseFailure or PostToolBatch fault naming its session (the binary's
// `terminated by …`, or the wrapper's `binary` line), and a binary-stage Stop
// naming one, is expected only when the row that hook would have written
// exists (hook-written or rebuilt by recovery); otherwise it stays a mismatch
// naming the session. One fault pairs one candidate.
func TestLostHookPairsWithItsRow(t *testing.T) {
	fault := func(stage, err string, sec float64) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id) VALUES(%d, '%s', '%s', '%s')`, fxMS(sec), stage, err, fxSession)
	}
	marker := func(id string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, stage, error, session_id, tool_use_id) VALUES(%d, 'transcript', 'call %s: open', '%s', '%s')`, fxMS(22), id, fxSession, id)
	}
	// The fixture's transcript holds three calls (A1 results at 3, B1 at 7, A2 at 20).
	delivered := `UPDATE calls SET bytes_delivered=10, bytes_real=NULL`
	unsized := `UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_B1'`
	undelivered := `UPDATE calls SET bytes_delivered=NULL WHERE tool_use_id='toolu_B1'`
	pending := `UPDATE calls SET request_id='pending:toolu_B1' WHERE tool_use_id='toolu_B1'`
	dropB1 := `DELETE FROM calls WHERE tool_use_id='toolu_B1'`
	dropStop := `DELETE FROM events WHERE event_id = 'e5'`
	openTurn := `UPDATE agent_turns SET stopped=NULL`
	at := " at=2030-01-01T00:00:22Z"
	id := " session=" + fxSession
	cases := []struct {
		name string
		sql  []string
		want string
	}{
		{"SubagentStop, its turn stopped", []string{fault("terminated", "SubagentStop: terminated by store busy", 10)},
			"EXPECTED fault-terminated" + id + " event=SubagentStop at=2030-01-01T00:00:10Z: expected: " + reasonLostSubagentStop},
		{"SubagentStop wrapper line, its turn stopped", []string{fault("binary", "SubagentStop: killed by signal", 10)},
			"EXPECTED fault-binary" + id + " event=SubagentStop"},
		{"SubagentStop, two faults, one turn", []string{fault("terminated", "SubagentStop: terminated by store busy", 10), fault("terminated", "SubagentStop: terminated by store busy", 10)},
			"MISMATCH fault-terminated" + id + " event=SubagentStop unpaired_subagentstop=0"},
		{"SubagentStop, no turn stopped in its window", []string{fault("terminated", "SubagentStop: terminated by store busy", 100)},
			"MISMATCH fault-terminated" + id + " event=SubagentStop unpaired_subagentstop=0"},
		{"SubagentStop, a turn of the session is open", []string{openTurn, fault("terminated", "SubagentStop: terminated by store busy", 10)},
			"MISMATCH fault-terminated" + id + " event=SubagentStop open_agent_turn=" + fxAgent},

		{"PostToolUse, every call settled", []string{fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"EXPECTED fault-terminated" + id + " event=PostToolUse" + at + ": expected: " + reasonLostPostToolUse},
		{"PostToolUse, a landed Agent without a real size", []string{`UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_A2'`, fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"EXPECTED fault-terminated" + id + " event=PostToolUse" + at},
		{"PostToolUseFailure wrapper line, a landed Agent without a real size", []string{`UPDATE calls SET bytes_real=NULL WHERE tool_use_id='toolu_A2'`, fault("binary", "PostToolUseFailure: killed by signal", 22)},
			"EXPECTED fault-binary" + id + " event=PostToolUseFailure" + at},
		{"PostToolUse, an Agent without a landed outcome", []string{`UPDATE calls SET failed=NULL, bytes_real=NULL WHERE tool_use_id='toolu_A2'`, fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"MISMATCH fault-terminated" + id + " event=PostToolUse unsettled_call=toolu_A2"},
		{"PostToolUseFailure wrapper line, every call settled", []string{fault("binary", "PostToolUseFailure: killed by signal", 22)},
			"EXPECTED fault-binary" + id + " event=PostToolUseFailure"},
		{"PostToolUse, a call has no size", []string{unsized, fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"MISMATCH fault-terminated" + id + " event=PostToolUse unsettled_call=toolu_B1"},
		{"PostToolUse, the unsized call carries a marker", []string{unsized, marker("toolu_B1"), fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"EXPECTED fault-terminated" + id + " event=PostToolUse"},
		{"PostToolUse and PostToolUseFailure share three calls", []string{
			fault("terminated", "PostToolUse: terminated by store busy", 22), fault("terminated", "PostToolUseFailure: terminated by store busy", 22),
			fault("terminated", "PostToolUse: terminated by store busy", 22), fault("terminated", "PostToolUse: terminated by store busy", 22)},
			"MISMATCH fault-terminated" + id + " event=PostToolUse unpaired_call=0"},
		{"PostToolUse, no result in its window", []string{fault("terminated", "PostToolUse: terminated by store busy", 100)},
			"MISMATCH fault-terminated" + id + " event=PostToolUse unpaired_call=0"},

		{"PostToolBatch, every call delivered", []string{delivered, fault("terminated", "PostToolBatch: terminated by store busy", 22)},
			"EXPECTED fault-terminated" + id + " event=PostToolBatch" + at + ": expected: " + reasonLostPostToolBatch},
		{"PostToolBatch, a call not delivered", []string{delivered, undelivered, fault("terminated", "PostToolBatch: terminated by store busy", 22)},
			"MISMATCH fault-terminated" + id + " event=PostToolBatch unsettled_call=toolu_B1"},
		{"PostToolBatch, a call's request still provisional", []string{delivered, pending, fault("binary", "PostToolBatch: killed by signal", 22)},
			"MISMATCH fault-binary" + id + " event=PostToolBatch unsettled_call=toolu_B1"},
		{"PostToolBatch, none delivered, no result in its window", []string{fault("terminated", "PostToolBatch: terminated by store busy", 100)},
			"MISMATCH fault-terminated" + id + " event=PostToolBatch unpaired_call=0"},

		{"PreToolUse, every call has its row", []string{fault("terminated", "PreToolUse: terminated by store busy", 22)},
			"EXPECTED fault-terminated" + id + " event=PreToolUse" + at + ": expected: " + reasonLostPreToolUse},
		{"PreToolUse, a call has no row", []string{dropB1, fault("terminated", "PreToolUse: terminated by store busy", 22)},
			"MISMATCH fault-terminated" + id + " event=PreToolUse unsettled_call=toolu_B1"},
		{"PreToolUse, the missing row carries a marker", []string{dropB1, marker("toolu_B1"), fault("binary", "PreToolUse: killed by signal", 22)},
			"EXPECTED fault-binary" + id + " event=PreToolUse"},
		{"PreToolUse, no call in its window", []string{fault("terminated", "PreToolUse: terminated by store busy", 100)},
			"MISMATCH fault-terminated" + id + " event=PreToolUse unpaired_call=0"},

		{"wrapper Stop, its turn has a Stop", []string{fault("binary", "Stop: killed by signal", 22)},
			"EXPECTED fault-binary" + id + " event=Stop"},
		{"wrapper Stop, its turn has none", []string{dropStop, fault("binary", "Stop: killed by signal", 22)},
			"MISMATCH fault-binary" + id + " event=Stop open_turn=" + fxSession},
		{"wrapper StopFailure, its turn has none", []string{fault("binary", "StopFailure: killed by signal", 22)},
			"MISMATCH fault-binary" + id + " event=StopFailure unpaired_stopfailure=0"},
		{"PreToolUse invalid terminated reason", []string{fault("terminated", "PreToolUse: panic later", 22)},
			"MISMATCH fault-terminated" + id + at},
	}
	for _, event := range []string{"Stop", "SubagentStop", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch"} {
		for _, reason := range []string{"terminated by store busy", "store unavailable: full", "panic"} {
			for _, paired := range []bool{true, false} {
				sec := float64(22)
				var sql []string
				if event == "SubagentStop" {
					sec = 10
				}
				if event == "PostToolBatch" {
					sql = append(sql, delivered)
				}
				want := "EXPECTED fault-terminated" + id + " event=" + event
				why := lostHookReason[event]
				if event == "Stop" {
					why = reasonTerminatedStopNamed
				}
				if !paired {
					want = "MISMATCH fault-terminated" + id + " event=" + event
					if event == "Stop" {
						sql = append(sql, dropStop)
						want += " open_turn=" + fxSession
					} else if event == "SubagentStop" {
						sec = 100
						want += " unpaired_subagentstop=0"
					} else {
						sec = 100
						want += " unpaired_call=0"
					}
				}
				want += " at=" + fxTS(sec)
				if paired {
					want += ": expected: " + why
				}
				sql = append(sql, fault("terminated", event+": "+reason, sec))
				cases = append(cases, struct {
					name string
					sql  []string
					want string
				}{fmt.Sprintf("%s %s paired=%t", event, reason, paired), sql, want})
			}
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, c.sql)
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			if !strings.Contains(out, c.want) {
				t.Fatalf("want %q:\n%s", c.want, out)
			}
		})
	}
}

func TestTerminatedEvent(t *testing.T) {
	for _, c := range []struct {
		name, stage, err, event string
		ok                      bool
	}{
		{"signal", "terminated", "Stop: terminated by SIGTERM", "Stop", true},
		{"store unavailable", "terminated", "Stop: store unavailable: full", "Stop", true},
		{"panic", "terminated", "Stop: panic", "Stop", true},
		{"panic with pid", "terminated", "Stop: panic (pid 7)", "Stop", true},
		{"signal with pid", "terminated", "PostToolUse: terminated by SIGTERM (pid 7)", "PostToolUse", true},
		{"store unavailable with pid", "terminated", "PreToolUse: store unavailable: open (pid 7)", "PreToolUse", true},
		{"wrapper stage", "binary", "Stop: panic", "", false},
		{"other stage", "transcript", "Stop: panic", "", false},
		{"no delimiter", "terminated", "panic", "panic", false},
		{"panic suffix", "terminated", "Stop: panic later", "Stop", false},
		{"incomplete store reason", "terminated", "Stop: store unavailable", "Stop", false},
		{"incomplete termination reason", "terminated", "Stop: terminated by", "Stop", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			event, ok := terminatedEvent(sFault{stage: c.stage, err: c.err})
			if event != c.event || ok != c.ok {
				t.Fatalf("terminatedEvent = (%q, %t), want (%q, %t)", event, ok, c.event, c.ok)
			}
		})
	}
}

// TestAllowlistNamesLinesMatchingNothing: the summary names every allowlist
// line no mismatch matched, so a dead line shows.
func TestAllowlistNamesLinesMatchingNothing(t *testing.T) {
	f := newFixture(t, nil, []string{`DELETE FROM events WHERE event_id = 'e2'`})
	allow := "# a comment\nevent-prompt-missing | 22222222 | seeded\ncall-extra | toolu_none | matches nothing\n"
	if err := os.WriteFile(f.allow, []byte(allow), 0o600); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	if _, out := f.run(t, "2030-01-01T00:00:00Z"); !strings.Contains(out, "reconcile: allowlist lines matching no mismatch: 3\n") {
		t.Fatalf("want line 3 named as matching nothing:\n%s", out)
	}
	f = newFixture(t, nil, nil)
	if _, out := f.run(t, "2030-01-01T00:00:00Z"); !strings.Contains(out, "reconcile: allowlist lines matching no mismatch: none\n") {
		t.Fatalf("want none:\n%s", out)
	}
}

func agentFile(f fixture) string {
	return filepath.Join(f.projects, "-tmp-demo-proj", fxSession, "subagents", "agent-"+fxAgent+".jsonl")
}

func errorResult(t *testing.T, sec float64, id string) string {
	return jsonLine(t, map[string]any{
		"type": "user", "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "is_error": true, "content": fxSecret}}},
	})
}

// TestStoppedSubAgentCall: TaskStop cut a sub-agent's call; Claude Code wrote
// its error result itself and no PostToolUse, PostToolBatch or SubagentStop
// fired, so its request, its size and the agent's tool count never land.
func TestStoppedSubAgentCall(t *testing.T) {
	taskStop := map[string]any{"type": "tool_use", "id": "toolu_A3", "name": "TaskStop", "input": map[string]any{"task_id": fxAgent}}
	mainStop := []string{assistantLine(t, 10, "msg_A3", 5, "tool_use", taskStop), toolResult(t, 10.5, "toolu_A3")}
	rows := []string{
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real, failed) VALUES('toolu_A3', '%s', 'msg_A3', %d, 'TaskStop', 10, 0)`, fxSession, fxMS(10)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_A3', '%s', %d, 'claude-demo', 'tool_use', 3, 5, 100, 20, 1, 0)`, fxSession, fxMS(10)),
		fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started) VALUES('%s', 2, '%s', 'demo-agent', %d)`, fxAgent, fxSession, fxMS(9.5)),
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, ts, tool) VALUES('toolu_B3', '%s', '%s', 'demo-agent', %d, 'Bash')`, fxSession, fxAgent, fxMS(9.7)),
	}
	for _, tc := range []struct {
		name   string
		result float64
		code   int
		want   []string
		absent []string
	}{
		{"error result written at the stop", 10.03, 0,
			[]string{"EXPECTED call-no-request session=" + fxSession + " ids=toolu_B3", "EXPECTED call-no-size session=" + fxSession + " ids=toolu_B3",
				"EXPECTED request-missing session=" + fxSession + " ids=msg_B3", "EXPECTED agent-tool-uses session=" + fxSession + " ids=" + fxAgent + " transcript=2 store=1"},
			[]string{"MISMATCH", "token-sum"}},
		{"error result long after the stop", 18, 1,
			[]string{"MISMATCH call-no-request session=" + fxSession + " ids=toolu_B3", "MISMATCH request-missing session=" + fxSession + " ids=msg_B3", "MISMATCH agent-tool-uses"},
			[]string{"EXPECTED call-no-request"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, mainStop, rows)
			writeLines(t, agentFile(f), append(agentLines(t), promptLine(t, 9.5), assistantLine(t, 9.7, "msg_B3", 7, "tool_use", toolUse("toolu_B3", "Bash")), errorResult(t, tc.result, "toolu_B3")))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("want %q:\n%s", w, out)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Fatalf("output holds %q:\n%s", a, out)
				}
			}
		})
	}
}

func TestScheduledTaskIsAPrompt(t *testing.T) {
	const scheduledID = "33333333-aaaa-4bbb-8ccc-000000000005"
	for _, scheduled := range []bool{true, false} {
		t.Run(fmt.Sprintf("scheduled=%t", scheduled), func(t *testing.T) {
			// Shape of a real scheduled-task prompt line: isMeta true, promptSource "system", turnOrigin "scheduled", scheduledTaskId set.
			v := map[string]any{"type": "user", "timestamp": fxTS(15), "sessionId": fxSession, "entrypoint": "cli", "promptId": scheduledID, "isMeta": true,
				"promptSource": "system", "turnOrigin": "scheduled", "scheduledTaskId": "demo-task",
				"message": map[string]any{"role": "user", "content": fxSecret}}
			if !scheduled {
				delete(v, "turnOrigin")
			}
			row := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e9', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(15), fxSession, scheduledID)
			f := newFixture(t, []string{jsonLine(t, v)}, []string{row})
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if scheduled {
				if code != 0 || strings.Contains(out, "event-prompt-extra") {
					t.Fatalf("scheduled prompt: exit %d, want 0 and no extra prompt:\n%s", code, out)
				}
			} else if code != 1 || !strings.Contains(out, "MISMATCH event-prompt-extra session="+fxSession+" ids="+scheduledID) {
				t.Fatalf("plain isMeta: exit %d, want 1 and an extra prompt:\n%s", code, out)
			}
		})
	}
}

// TestPeerMessageIsAPrompt: a message a sub-agent sends the chat is written
// isMeta with origin.kind "peer", and fires UserPromptSubmit; a plain isMeta
// line fires none.
func TestPeerMessageIsAPrompt(t *testing.T) {
	const peerPrompt = "33333333-aaaa-4bbb-8ccc-000000000003"
	row := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e9', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(15), fxSession, peerPrompt)
	metaLine := func(origin map[string]any) string {
		v := map[string]any{"type": "user", "timestamp": fxTS(15), "sessionId": fxSession, "entrypoint": "cli", "promptId": peerPrompt, "isMeta": true,
			"message": map[string]any{"role": "user", "content": fxSecret}}
		if origin != nil {
			v["origin"] = origin
		}
		return jsonLine(t, v)
	}
	f := newFixture(t, []string{metaLine(map[string]any{"kind": "peer", "from": fxAgent})}, []string{row})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || strings.Contains(out, "event-prompt-extra") {
		t.Fatalf("a peer message's UserPromptSubmit is no extra prompt (exit %d):\n%s", code, out)
	}
	f = newFixture(t, []string{metaLine(nil)}, []string{row})
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH event-prompt-extra session="+fxSession+" ids="+peerPrompt) {
		t.Fatalf("a plain isMeta line is no prompt (exit %d):\n%s", code, out)
	}
}

// A wake adds another promptId line after a streamed final assistant message
// with null stop_reason; its SubagentStop still closed that earlier turn.
func TestNullStopTurnClosedByAWake(t *testing.T) {
	for _, turns := range []int{2, 1, 3} {
		t.Run(fmt.Sprintf("store turns=%d", turns), func(t *testing.T) {
			rows := []string{
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 7, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(9.7)),
			}
			for seq := 2; seq <= turns; seq++ {
				rows = append(rows, fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped) VALUES('%s', %d, '%s', 'demo-agent', %d, %d)`, fxAgent, seq, fxSession, fxMS(9.5), fxMS(9.8)))
			}
			f := newFixture(t, nil, rows)
			lines := agentLines(t)
			lines[len(lines)-1] = assistantLine(t, 8, "msg_B2", 30, nil, map[string]any{"type": "text", "text": fxSecret})
			writeLines(t, agentFile(f), append(lines, promptLine(t, 9.5), promptLine(t, 9.6), assistantLine(t, 9.7, "msg_B3", 7, "end_turn", map[string]any{"type": "text", "text": fxSecret})))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if turns == 2 {
				if code != 0 || strings.Contains(out, "agent-turns") {
					t.Fatalf("two turns: exit %d, want 0 and no turn mismatch:\n%s", code, out)
				}
			} else {
				want := fmt.Sprintf("MISMATCH agent-turns session=%s ids=%s transcript_turns=2 store_turns=%d", fxSession, fxAgent, turns)
				if code != 1 || !strings.Contains(out, want) {
					t.Fatalf("wrong turn count: exit %d, want 1 and %q:\n%s", code, want, out)
				}
			}
		})
	}
}

// TestTurnEndingWithNullStopReason: a sub-agent turn whose final message keeps
// a null stop_reason still counts as a turn; it never inherits the end_turn of
// the message before it.
func TestTurnEndingWithNullStopReason(t *testing.T) {
	f := newFixture(t, nil, []string{
		fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped) VALUES('%s', 2, '%s', 'demo-agent', %d, %d)`, fxAgent, fxSession, fxMS(9.5), fxMS(9.8)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 7, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(9.7)),
	})
	writeLines(t, agentFile(f), append(agentLines(t), promptLine(t, 9.5), assistantLine(t, 9.6, "msg_B3", 7, nil, map[string]any{"type": "thinking", "thinking": fxSecret}), assistantLine(t, 9.7, "msg_B3", 7, nil, map[string]any{"type": "text", "text": fxSecret})))
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || strings.Contains(out, "agent-turns") {
		t.Fatalf("two turns on both sides (exit %d):\n%s", code, out)
	}
}

// A scratch store can hold only a terminated PostToolUse fault naming the
// session_id: no SessionStart was served to create its sessions row.
func TestScratchStoreFaultOnlySessionIsRecorded(t *testing.T) {
	const scratchSession = "44444444-aaaa-4bbb-8ccc-000000000004"
	for _, tc := range []struct{ name, sessionSQL string }{
		{"named", "'" + scratchSession + "'"},
		{"null", "NULL"},
		{"empty", "''"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, nil)
			writeLines(t, filepath.Join(f.projects, "-tmp-demo-proj", scratchSession+".jsonl"), []string{promptLine(t, 30), assistantLine(t, 31, "msg_S1", 5, "end_turn", map[string]any{"type": "text", "text": fxSecret})})
			path := filepath.Join(f.dir, "scratch", "run1", "home", "callmeter.db")
			store, err := callmeter.OpenDB(context.Background(), path)
			if err != nil {
				t.Fatalf("create scratch store: %v", err)
			}
			_, insertErr := store.DB().Exec(fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, %s, 'terminated', 'PostToolUse: store unavailable: other')`, fxMS(30), tc.sessionSQL))
			closeErr := store.Close()
			if insertErr != nil || closeErr != nil {
				t.Fatalf("scratch fault: %v; close store: %v", insertErr, closeErr)
			}
			ids, err := scratchSessions(context.Background(), path)
			if err != nil {
				t.Fatalf("read scratch sessions: %v", err)
			}
			code, out := f.run(t, "2020-01-01T00:00:00Z")
			wantCode, want := 1, "MISMATCH session-unrecorded session="+scratchSession
			if tc.name == "named" {
				wantCode, want = 0, "EXPECTED session-unrecorded session="+scratchSession+" assistant_lines=1 tool_uses=0 subagents=0 scratch_store="+path+": expected: "+reasonScratchStore
				if len(ids) != 1 || ids[0] != scratchSession {
					t.Errorf("scratch session ids=%v, want only %s", ids, scratchSession)
				}
			} else if len(ids) != 0 {
				t.Errorf("unnamed fault names session ids=%v", ids)
			}
			if code != wantCode || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, wantCode, want, out)
			}
		})
	}
}

// TestScratchStoreRecordsUnrecordedSession: a session that ran with
// CALLMETER_HOME in a scratch store is unrecorded here and recorded there.
func TestScratchStoreRecordsUnrecordedSession(t *testing.T) {
	const scratchSession = "44444444-aaaa-4bbb-8ccc-000000000004"
	f := newFixture(t, nil, nil)
	writeLines(t, filepath.Join(f.projects, "-tmp-demo-proj", scratchSession+".jsonl"), []string{promptLine(t, 30), assistantLine(t, 31, "msg_S1", 5, "end_turn", map[string]any{"type": "text", "text": fxSecret})})
	code, out := f.run(t, "2020-01-01T00:00:00Z")
	if code != 1 || !strings.Contains(out, "MISMATCH session-unrecorded session="+scratchSession) {
		t.Fatalf("no scratch store: want the unexplained row (exit %d):\n%s", code, out)
	}
	ctx := context.Background()
	path := filepath.Join(f.dir, "scratch", "run1", "home", "callmeter.db")
	store, err := callmeter.OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("create scratch store: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts) VALUES('%s', %d)`, scratchSession, fxMS(30))); err != nil {
		t.Fatalf("scratch row: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close scratch store: %v", err)
	}
	code, out = f.run(t, "2020-01-01T00:00:00Z")
	want := "EXPECTED session-unrecorded session=" + scratchSession + " assistant_lines=1 tool_uses=0 subagents=0 scratch_store=" + path + ": expected: " + reasonScratchStore
	if code != 0 || !strings.Contains(out, want) || !strings.Contains(out, "reconcile: scratch stores under "+filepath.Join(f.dir, "scratch")+": 1 read, unreadable: none\n") {
		t.Fatalf("want %q and one store read (exit %d):\n%s", want, code, out)
	}
}

// TestQuietSessionWithAFreshTranscriptIsLive: recovery leaves a session whose
// transcript was written within the hour, so it is not yet due here either.
func TestQuietSessionWithAFreshTranscriptIsLive(t *testing.T) {
	later := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('z1', 'SessionStart', %d, '%s')`, fxMS(21+3700), fxSession2)
	f := newFixture(t, nil, []string{`DELETE FROM events WHERE event='SessionEnd'`, later})
	stale := fxBase.Add(21 * time.Second)
	if err := os.Chtimes(f.main, stale, stale); err != nil {
		t.Fatalf("age transcript: %v", err)
	}
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 1 || !strings.Contains(out, "MISMATCH session-no-end-hook session="+fxSession) {
		t.Fatalf("quiet transcript: want the unexplained row (exit %d):\n%s", code, out)
	}
	fresh := fxBase.Add((21 + 3650) * time.Second)
	if err := os.Chtimes(f.main, fresh, fresh); err != nil {
		t.Fatalf("touch transcript: %v", err)
	}
	if code, out := f.run(t, "2030-01-01T00:00:00Z"); code != 0 || !strings.Contains(out, "PENDING session-no-end-hook-live session="+fxSession+" end_reason=- transcript_written=2030-01-01T01:01:11Z") {
		t.Fatalf("fresh transcript: want PENDING live (exit %d):\n%s", code, out)
	}
}

// Parent toolUseId is present in the sub-agent meta before the Agent
// PostToolUse or meta fill writes agents.parent_tool_use_id.
func TestAgentParentOpenIsPendingWhileLive(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		quiet, fresh bool
		code         int
	}{
		{name: "recent session row", parent: "NULL"},
		{name: "fresh transcript", parent: "NULL", quiet: true, fresh: true},
		{name: "quiet session", parent: "NULL", quiet: true, code: 1},
		{name: "wrong parent while live", parent: "'toolu_A1'", code: 1},
		{name: "wrong parent while quiet", parent: "'toolu_A1'", quiet: true, code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []string{`UPDATE agents SET parent_tool_use_id=` + tc.parent}
			if tc.quiet {
				rows = append(rows, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('z1', 'SessionStart', %d, '%s')`, fxMS(21+3700), fxSession2))
			}
			f := newFixture(t, nil, rows)
			for _, path := range []string{f.main, agentFile(f)} {
				mtime := fxBase.Add(21 * time.Second)
				if tc.fresh && path == agentFile(f) {
					mtime = fxBase.Add((21 + 3650) * time.Second)
				}
				if err := os.Chtimes(path, mtime, mtime); err != nil {
					t.Fatalf("set transcript write time: %v", err)
				}
			}
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			want := "PENDING agent-parent-open session=" + fxSession + " ids=" + fxAgent + " transcript=toolu_A2 store=-"
			if tc.code == 1 {
				parent := "-"
				if tc.parent != "NULL" {
					parent = "toolu_A1"
				}
				want = "MISMATCH agent-parent session=" + fxSession + " ids=" + fxAgent + " transcript=toolu_A2 store=" + parent
			}
			if code != tc.code || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, tc.code, want, out)
			}
		})
	}
}

// Agent transcripts retain tool_use blocks issued before the session's first
// recorded row; ReadAgentTotals counts those along with later blocks.
func TestPreFirstRowAgentToolUsesCount(t *testing.T) {
	for _, count := range []int{2, 1, 3} {
		t.Run(fmt.Sprintf("store tool uses=%d", count), func(t *testing.T) {
			f := newFixture(t, nil, []string{
				fmt.Sprintf(`UPDATE sessions SET first_ts=%d`, fxMS(20)),
				fmt.Sprintf(`UPDATE agents SET tool_uses=%d`, count),
				fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, request_id, ts, tool, bytes_real, failed) VALUES('toolu_B3', '%s', '%s', 'demo-agent', 'msg_B3', %d, 'Read', 10, 0)`, fxSession, fxAgent, fxMS(12)),
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'tool_use', 3, 7, 100, 20, 1, 0)`, fxSession, fxAgent, fxMS(12)),
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B4', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 8, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(14)),
			})
			writeLines(t, agentFile(f), append(agentLines(t), assistantLine(t, 12, "msg_B3", 7, "tool_use", toolUse("toolu_B3", "Read")), toolResult(t, 13, "toolu_B3"), assistantLine(t, 14, "msg_B4", 8, "end_turn", map[string]any{"type": "text", "text": fxSecret})))
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			if count == 2 {
				if strings.Contains(out, "agent-tool-uses") {
					t.Fatalf("whole transcript count agrees:\n%s", out)
				}
			} else {
				want := fmt.Sprintf("MISMATCH agent-tool-uses session=%s ids=%s transcript=2 store=%d", fxSession, fxAgent, count)
				if !strings.Contains(out, want) {
					t.Fatalf("wrong count: want %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestOpenAgentTurnCountIsPending: an agent's tool count is written at
// SubagentStop, so one still in its turn is not yet due; once its session has
// ended the gap is a mismatch.
func TestOpenAgentTurnCountIsPending(t *testing.T) {
	rows := []string{
		fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started) VALUES('%s', 2, '%s', 'demo-agent', %d)`, fxAgent, fxSession, fxMS(9.5)),
		fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, request_id, ts, tool, bytes_real, failed) VALUES('toolu_B3', '%s', '%s', 'demo-agent', 'msg_B3', %d, 'Read', 10, 0)`, fxSession, fxAgent, fxMS(9.7)),
		fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'tool_use', 3, 7, 100, 20, 1, 0)`, fxSession, fxAgent, fxMS(9.7)),
	}
	for _, tc := range []struct {
		name string
		sql  []string
		code int
		want string
	}{
		{"session running", append([]string{`DELETE FROM events WHERE event='SessionEnd'`}, rows...), 0, "PENDING agent-tool-uses-open session=" + fxSession + " ids=" + fxAgent + " transcript=2 store=1"},
		{"session ended", rows, 1, "MISMATCH agent-tool-uses session=" + fxSession + " ids=" + fxAgent + " transcript=2 store=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, tc.sql)
			writeLines(t, agentFile(f), append(agentLines(t), promptLine(t, 9.5), assistantLine(t, 9.7, "msg_B3", 7, "tool_use", toolUse("toolu_B3", "Read")), toolResult(t, 9.8, "toolu_B3")))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != tc.code || !strings.Contains(out, tc.want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, tc.code, tc.want, out)
			}
		})
	}
}

// TestUnfillableMarkerIsExplainedByItsOwnParse: report-time recovery records a
// transcript fault for an agent turn or a Stop it read the transcripts in full
// for and could not fill. The fault is expected only when this check's own
// parse of the session's transcripts holds nothing that fills the turn at or
// after the session's first row; otherwise it stays a mismatch.
func TestUnfillableMarkerIsExplainedByItsOwnParse(t *testing.T) {
	agentFault := func(stopped float64, tail string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, '%s', 'transcript', 'agent %s turn stopped %d: %s')`,
			fxMS(stopped), fxSession, fxAgent, fxMS(stopped), strings.ReplaceAll(tail, "'", "''"))
	}
	stopFault := func(tail string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, '%s', 'transcript', 'prompt %s Stop e5: %s')`,
			fxMS(21), fxSession, fxPrompt, strings.ReplaceAll(tail, "'", "''"))
	}
	// A second agent turn, spanning (9, 9.8], and the reply msg_B3 at reply: in
	// that span (9.2) or in the first turn's (8.5).
	agentTurn := func(reply float64) (extraSQL []string, lines []string) {
		extraSQL = []string{
			fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped) VALUES('%s', 2, '%s', 'demo-agent', %d, %d)`, fxAgent, fxSession, fxMS(9.1), fxMS(9.8)),
			fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 7, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(reply)),
		}
		lines = append(agentLines(t), promptLine(t, 9.05), assistantLine(t, reply, "msg_B3", 7, "end_turn", map[string]any{"type": "text", "text": fxSecret}))
		return extraSQL, lines
	}
	stopReply := []string{assistantLine(t, 21.5, "msg_A2", 9, "end_turn", map[string]any{"type": "text", "text": fxSecret})}
	stopReplyRow := fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_A2', '%s', %d, 'claude-demo', 'end_turn', 3, 9, 100, 20, 0, 0)`, fxSession, fxMS(21.5))
	// The marker of a prompt with no turn end, stamped at sec (the session's last
	// hook when recovery marked it), naming prompt and its UserPromptSubmit event
	// (the fixture's e2).
	turnEndFault := func(sec float64, prompt, tail string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, '%s', 'transcript', 'prompt %s UserPromptSubmit e2: %s')`,
			fxMS(sec), fxSession, prompt, strings.ReplaceAll(tail, "'", "''"))
	}
	// A hook of the session after the turn-end marker: it reopened the session.
	laterHook := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e-later', 'Notification', %d, '%s')`, fxMS(30), fxSession)
	// The marker of an open call, stamped at sec (the session's last hook when
	// recovery marked it, 21 s in the fixture).
	callFault := func(sec float64, id, tail string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, session_id, tool_use_id, stage, error) VALUES(%d, '%s', '%s', 'transcript', 'call %s: %s')`,
			fxMS(sec), fxSession, id, id, strings.ReplaceAll(tail, "'", "''"))
	}
	// toolu_A9: a Bash call of msg_A9 at 2.5 s, its row with a request and,
	// when sized, a size; its result line at sec.
	openUse := []string{assistantLine(t, 2.5, "msg_A9", 9, "tool_use", toolUse("toolu_A9", "Bash"))}
	openRows := func(sized bool) []string {
		size := "NULL"
		if sized {
			size = "10"
		}
		return []string{
			fmt.Sprintf(`INSERT INTO requests(request_id, session_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_A9', '%s', %d, 'claude-demo', 'tool_use', 3, 9, 100, 20, 1, 0)`, fxSession, fxMS(2.5)),
			fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, request_id, ts, tool, bytes_real) VALUES('toolu_A9', '%s', 'msg_A9', %d, 'Bash', %s)`, fxSession, fxMS(2.5), size),
		}
	}
	openResult := func(sec float64) []string { return append(slices.Clone(openUse), toolResult(t, sec, "toolu_A9")) }
	reopen := fmt.Sprintf(`UPDATE sessions SET last_ts = %d WHERE session_id = '%s'`, fxMS(30), fxSession)
	expectedCall := "EXPECTED fault-transcript-unfillable session=" + fxSession + " call=toolu_A9 "
	expectedAgent := "EXPECTED fault-transcript-unfillable session=" + fxSession + " agent=" + fxAgent + " "
	expectedStop := "EXPECTED fault-transcript-unfillable session=" + fxSession + " prompt=" + fxPrompt + " "
	unexplained := "MISMATCH fault-transcript session=" + fxSession + " "
	for _, c := range []struct {
		name      string
		reply     float64 // the agent's second reply, 0: no agent turn
		extraMain []string
		sql       []string
		fault     string
		want      string
		code      int
	}{
		{name: "agent: no request in the span", reply: 8.5, fault: agentFault(9.8, unfilledAgentTurn), want: expectedAgent, code: 0},
		{name: "agent: the transcript holds a request in the span", reply: 9.2, fault: agentFault(9.8, unfilledAgentTurn), want: unexplained, code: 1},
		{name: "agent: a stop the store holds no turn for", reply: 8.5, fault: agentFault(9.9, unfilledAgentTurn), want: unexplained, code: 1},
		{name: "stop: no final request of the prompt", fault: stopFault(unfilledStopReply), want: expectedStop, code: 0},
		{name: "stop: the transcript holds a final request of the prompt", extraMain: stopReply, sql: []string{stopReplyRow}, fault: stopFault(unfilledStopReply), want: unexplained, code: 1},
		{name: "another text is no marker", fault: stopFault("something else"), want: unexplained, code: 1},
		// The reply ending the prompt's turn is on disk at 21.5 s.
		{name: "turn end: the reply came after the mark and a later hook reopened the session", extraMain: stopReply, sql: []string{stopReplyRow, laterHook},
			fault: turnEndFault(21.2, fxPrompt, unfilledTurnEnd), want: expectedStop, code: 0},
		{name: "turn end: the reply came after the mark and no store event follows it", extraMain: stopReply, sql: []string{stopReplyRow},
			fault: turnEndFault(21.2, fxPrompt, unfilledTurnEnd), want: unexplained, code: 1},
		{name: "turn end: the reply is at the marking time", extraMain: stopReply, sql: []string{stopReplyRow, laterHook},
			fault: turnEndFault(21.5, fxPrompt, unfilledTurnEnd), want: unexplained, code: 1},
		{name: "turn end: the transcript holds no turn end at all", fault: turnEndFault(22, fxPrompt, unfilledTurnEnd), want: expectedStop, code: 0},
		{name: "turn end: the reply was on disk by the marking time", extraMain: stopReply, sql: []string{stopReplyRow},
			fault: turnEndFault(22, fxPrompt, unfilledTurnEnd), want: unexplained, code: 1},
		{name: "turn end: a prompt the store does not hold", fault: turnEndFault(22, fxPrompt2, unfilledTurnEnd), want: unexplained, code: 1},
		{name: "turn end: another text is no marker", fault: turnEndFault(22, fxPrompt, "something else"), want: unexplained, code: 1},
		{name: "call: no result on disk", extraMain: openUse, sql: openRows(false), fault: callFault(21, "toolu_A9", unfilledCall), want: expectedCall, code: 0},
		{name: "call: the result was on disk by the mark", extraMain: openResult(2.6), sql: openRows(false), fault: callFault(21, "toolu_A9", unfilledCall), want: unexplained, code: 1},
		{name: "call: the result came after the mark and a later hook reopened it", extraMain: openResult(23), sql: append(openRows(true), reopen),
			fault: callFault(21, "toolu_A9", unfilledCall), want: expectedCall, code: 0},
		{name: "call: the result came after the mark and no hook followed", extraMain: openResult(23), sql: openRows(false),
			fault: callFault(21, "toolu_A9", unfilledCall), want: unexplained, code: 1},
		{name: "call: a call the store does not hold", fault: callFault(21, "toolu_A8", unfilledCall), want: unexplained, code: 1},
		{name: "call: another text is no marker", extraMain: openUse, sql: openRows(false), fault: callFault(21, "toolu_A9", "something else"), want: unexplained, code: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			sql := append(slices.Clone(c.sql), c.fault)
			var lines []string
			if c.reply != 0 {
				var turn []string
				turn, lines = agentTurn(c.reply)
				sql = append(sql, turn...)
			}
			f := newFixture(t, c.extraMain, sql)
			if lines != nil {
				writeLines(t, agentFile(f), lines)
			}
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != c.code || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, c.code, c.want, out)
			}
			if c.code == 0 && !strings.Contains(out, reasonUnfillable) {
				t.Fatalf("want the reason line %q:\n%s", reasonUnfillable, out)
			}
		})
	}
}

// TestOpenAgentTurnMarkerIsExplainedByItsOwnParse: recovery marks a
// sub-agent's latest turn that has no SubagentStop and whose quiet transcript
// shows no turn end. The fault is expected only when this check's own parse of
// the agent's transcript shows none at or after the turn's start (or only one
// after the mark with a later hook reopening it); otherwise it stays a mismatch.
func TestOpenAgentTurnMarkerIsExplainedByItsOwnParse(t *testing.T) {
	// The agent's second turn, open from 9.1 s: its tool_use msg_B3 at 9.2 s
	// (toolu_B9, no result), and when ended its reply msg_B4 at end.
	rows := func(end float64) []string {
		sql := []string{
			fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started) VALUES('%s', 2, '%s', 'demo-agent', %d)`, fxAgent, fxSession, fxMS(9.1)),
			fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'tool_use', 3, 7, 100, 20, 1, 0)`, fxSession, fxAgent, fxMS(9.2)),
			fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, request_id, ts, tool) VALUES('toolu_B9', '%s', '%s', 'demo-agent', 'msg_B3', %d, 'Bash')`, fxSession, fxAgent, fxMS(9.2)),
		}
		if end != 0 {
			sql = append(sql, fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B4', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 5, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(end)))
		}
		return sql
	}
	lines := func(end float64) []string {
		out := append(agentLines(t), promptLine(t, 9.05), assistantLine(t, 9.2, "msg_B3", 7, "tool_use", toolUse("toolu_B9", "Bash")))
		if end != 0 {
			out = append(out, assistantLine(t, end, "msg_B4", 5, "end_turn", map[string]any{"type": "text", "text": fxSecret}))
		}
		return out
	}
	fault := func(seq int, tail string) string {
		return fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, '%s', 'transcript', 'agent %s turn %d open: %s')`,
			fxMS(21), fxSession, fxAgent, seq, strings.ReplaceAll(tail, "'", "''"))
	}
	reopen := fmt.Sprintf(`UPDATE sessions SET last_ts = %d WHERE session_id = '%s'`, fxMS(30), fxSession)
	expected := "EXPECTED fault-transcript-unfillable session=" + fxSession + " agent=" + fxAgent + " turn-open "
	unexplained := "MISMATCH fault-transcript session=" + fxSession + " "
	for _, c := range []struct {
		name  string
		end   float64
		sql   []string
		fault string
		want  string
		code  int
	}{
		{name: "no turn end in the agent's transcript", fault: fault(2, unfilledAgentStop), want: expected, code: 0},
		{name: "a turn end before the mark", end: 9.4, fault: fault(2, unfilledAgentStop), want: unexplained, code: 1},
		{name: "a turn end after the mark and a later hook reopened it", end: 22, sql: []string{reopen,
			fmt.Sprintf(`UPDATE agent_turns SET stopped = %d WHERE agent_id = '%s' AND seq = 2`, fxMS(22.5), fxAgent),
			fmt.Sprintf(`UPDATE agents SET stopped = %d, tool_uses = 2 WHERE agent_id = '%s'`, fxMS(22.5), fxAgent)},
			fault: fault(2, unfilledAgentStop), want: expected, code: 0},
		{name: "a turn end after the mark and no hook followed", end: 22, fault: fault(2, unfilledAgentStop), want: unexplained, code: 1},
		{name: "a turn the store does not hold open", fault: fault(1, unfilledAgentStop), want: unexplained, code: 1},
		{name: "another text is no marker", fault: fault(2, "something else"), want: unexplained, code: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, append(append(rows(c.end), c.sql...), c.fault))
			writeLines(t, agentFile(f), lines(c.end))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != c.code || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want %d and %q:\n%s", code, c.code, c.want, out)
			}
		})
	}
}

// TestRebuiltAgentStopIsCheckedByItsOwnParse: recovery rebuilt the
// SubagentStop of a sub-agent turn whose hook event was lost, dated at the
// turn-end line of the agent's transcript. It stands only when this check's
// own parse of that transcript holds a turn end at that ts, at or after the
// turn's start; otherwise it is a mismatch.
func TestRebuiltAgentStopIsCheckedByItsOwnParse(t *testing.T) {
	if rebuiltDetail != callmeter.RecoveredDetail {
		t.Fatalf("rebuiltDetail = %q, want %q", rebuiltDetail, callmeter.RecoveredDetail)
	}
	// The agent's second turn from 9.1 s: its tool_use msg_B3 at 9.2 s, its
	// reply msg_B4 ending the turn at 9.4 s; the stop rebuilt at stopped.
	rows := func(stopped float64) []string {
		return []string{
			fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped, stop_event_id) VALUES('%s', 2, '%s', 'demo-agent', %d, %d, 'rb-stop')`, fxAgent, fxSession, fxMS(9.1), fxMS(stopped)),
			fmt.Sprintf(`INSERT INTO events(event_id, event, session_id, agent_id, ts, detail) VALUES('rb-stop', 'SubagentStop', '%s', '%s', %d, '%s')`, fxSession, fxAgent, fxMS(stopped), callmeter.RecoveredDetail),
			fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'tool_use', 3, 7, 100, 20, 1, 0)`, fxSession, fxAgent, fxMS(9.2)),
			fmt.Sprintf(`INSERT INTO calls(tool_use_id, session_id, agent_id, agent_type, request_id, ts, tool) VALUES('toolu_B9', '%s', '%s', 'demo-agent', 'msg_B3', %d, 'Bash')`, fxSession, fxAgent, fxMS(9.2)),
			fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B4', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 5, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(9.4)),
			fmt.Sprintf(`UPDATE agents SET stopped = %d, tool_uses = 2 WHERE agent_id = '%s'`, fxMS(stopped), fxAgent),
		}
	}
	lines := append(agentLines(t), promptLine(t, 9.05), assistantLine(t, 9.2, "msg_B3", 7, "tool_use", toolUse("toolu_B9", "Bash")),
		assistantLine(t, 9.4, "msg_B4", 5, "end_turn", map[string]any{"type": "text", "text": fxSecret}))
	for _, c := range []struct {
		name    string
		stopped float64
		code    int
		want    bool
	}{
		{name: "dated at the transcript's turn end", stopped: 9.4, code: 0},
		{name: "dated where the transcript shows no turn end", stopped: 9.6, code: 1, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, rows(c.stopped))
			writeLines(t, agentFile(f), lines)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			mismatch := "MISMATCH agent-stop-rebuilt session=" + fxSession + " "
			if code != c.code || strings.Contains(out, mismatch) != c.want {
				t.Fatalf("exit %d, want %d and %q shown %v:\n%s", code, c.code, mismatch, c.want, out)
			}
		})
	}
}

// TestNoticeEndedAgentTurnIsShown checks recovery's task-notification end of a
// background agent's turn whose final reply still has a null stop_reason.
func TestNoticeEndedAgentTurnIsShown(t *testing.T) {
	for _, form := range []string{"attachment", "user"} {
		t.Run(form, func(t *testing.T) {
			for _, c := range []struct {
				name, agent, status, origin string
				noticeTS, stopped           float64
				inAgent, arrayContent       bool
				wantMismatch                bool
			}{
				{name: "matching notice", agent: fxAgent, status: "<status>completed</status>", origin: "task-notification", noticeTS: 9, stopped: 9},
				{name: "another agent", agent: "another-agent", status: "<status>completed</status>", origin: "task-notification", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "another timestamp", agent: fxAgent, status: "<status>completed</status>", origin: "task-notification", noticeTS: 9.1, stopped: 9, wantMismatch: true},
				{name: "before turn start", agent: fxAgent, status: "<status>completed</status>", origin: "task-notification", noticeTS: 4, stopped: 4, wantMismatch: true},
				{name: "no status", agent: fxAgent, origin: "task-notification", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "empty status", agent: fxAgent, status: "<status></status>", origin: "task-notification", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "unclosed status", agent: fxAgent, status: "<status>completed", origin: "task-notification", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "empty task id", status: "<status>completed</status>", origin: "task-notification", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "notice in agent transcript", agent: fxAgent, status: "<status>completed</status>", origin: "task-notification", noticeTS: 9, stopped: 9, inAgent: true, wantMismatch: true},
				{name: "other origin", agent: fxAgent, status: "<status>completed</status>", origin: "peer", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "missing origin", agent: fxAgent, status: "<status>completed</status>", noticeTS: 9, stopped: 9, wantMismatch: true},
				{name: "content array", agent: fxAgent, status: "<status>completed</status>", origin: "task-notification", noticeTS: 9, stopped: 9, arrayContent: true},
			} {
				t.Run(c.name, func(t *testing.T) {
					body := "<task-notification><task-id>" + c.agent + "</task-id>" + c.status + "<summary>" + fxSecret + "</summary></task-notification>"
					origin := map[string]any{"kind": c.origin, "producer": "session-task"}
					// A content array (a queued prompt carrying an image is one) is
					// read from its "text" blocks, as prompt or message.content.
					var content any = body
					if c.arrayContent {
						content = []map[string]any{{"type": "text", "text": body}}
					}
					var notice string
					if form == "attachment" {
						// Gym S2 main transcript line 38: queued_command, prompt,
						// commandMode, origin.kind/producer and timestamp.
						a := map[string]any{"type": "queued_command", "prompt": content, "commandMode": "task-notification"}
						if c.origin != "" {
							a["origin"] = origin
						}
						notice = jsonLine(t, map[string]any{"type": "attachment", "timestamp": fxTS(c.noticeTS), "attachment": a})
					} else {
						// Gym S2 main transcript line 50: user, origin.kind/producer,
						// promptSource, turnOrigin and string message.content.
						l := map[string]any{"type": "user", "timestamp": fxTS(c.noticeTS), "promptSource": "system", "turnOrigin": "task_notification", "isMeta": true, "message": map[string]any{"role": "user", "content": content}}
						if c.origin != "" {
							l["origin"] = origin
						}
						notice = jsonLine(t, l)
					}
					rows := []string{
						`UPDATE requests SET stop_reason=NULL WHERE request_id='msg_B2'`,
						fmt.Sprintf(`UPDATE events SET ts=%d, detail='%s' WHERE event_id='e4'`, fxMS(c.stopped), callmeter.RecoveredDetail),
						fmt.Sprintf(`UPDATE agent_turns SET stopped=%d, stop_event_id='e4' WHERE agent_id='%s'`, fxMS(c.stopped), fxAgent),
						fmt.Sprintf(`UPDATE agents SET stopped=%d WHERE agent_id='%s'`, fxMS(c.stopped), fxAgent),
					}
					if form == "attachment" && !c.inAgent {
						rows = append(rows, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('notice-prompt', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(c.noticeTS), fxSession, fxPrompt))
					}
					var main []string
					if !c.inAgent {
						main = []string{notice}
					}
					f := newFixture(t, main, rows)
					lines := agentLines(t)
					lines[len(lines)-1] = assistantLine(t, 8, "msg_B2", 30, nil, map[string]any{"type": "text", "text": fxSecret})
					if c.inAgent {
						lines = append(lines, notice)
					}
					writeLines(t, agentFile(f), lines)
					code, out := f.run(t, "2030-01-01T00:00:00Z")
					mismatch := "MISMATCH agent-stop-rebuilt session=" + fxSession + " "
					wantCode := 0
					if c.wantMismatch {
						wantCode = 1
					}
					if code != wantCode || strings.Contains(out, mismatch) != c.wantMismatch {
						t.Fatalf("exit %d, want %d and rebuilt-stop mismatch shown %v:\n%s", code, wantCode, c.wantMismatch, out)
					}
				})
			}
		})
	}
}

func TestWithoutPid(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"legacy", "unknown: killed by signal", "unknown: killed by signal"},
		{"pid", "unknown: killed by signal (pid 4242)", "unknown: killed by signal"},
		{"one digit", "Stop: panic (pid 7)", "Stop: panic"},
		{"zero", "Stop: panic (pid 0)", "Stop: panic"},
		{"empty", "Stop: panic (pid )", "Stop: panic (pid )"},
		{"letters", "Stop: panic (pid x)", "Stop: panic (pid x)"},
		{"mixed", "Stop: panic (pid 7x)", "Stop: panic (pid 7x)"},
		{"signed", "Stop: panic (pid -7)", "Stop: panic (pid -7)"},
		{"unclosed", "Stop: panic (pid 7", "Stop: panic (pid 7"},
		{"not trailing", "Stop: panic (pid 7) later", "Stop: panic (pid 7) later"},
		{"only one", "Stop: panic (pid 7) (pid 8)", "Stop: panic (pid 7)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := withoutPid(c.text); got != c.want {
				t.Fatalf("reason = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNoticeAgent(t *testing.T) {
	// Notice body shape from gym S2 main transcript lines 38 and 50:
	// task-id and status, followed by private summary/result/usage tags.
	task := "<task-id>" + fxAgent + "</task-id>"
	status := "<status>completed</status>"
	private := "<summary>" + fxSecret + "</summary><result>" + fxSecret + "</result><usage>" + fxSecret + "</usage>"
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", task + status + private, true},
		{"no task tag", status + private, false},
		{"unclosed task tag", "<task-id>" + fxAgent + status + private, false},
		{"empty task", "<task-id></task-id>" + status + private, false},
		{"no status tag", task + private, false},
		{"unclosed status tag", task + "<status>completed" + private, false},
		{"empty status", task + "<status></status>" + private, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			agent, ok := noticeAgent(c.body)
			want := ""
			if c.ok {
				want = fxAgent
			}
			if ok != c.ok || agent != want {
				t.Fatalf("notice parsed: agent id matches=%t, ok=%t, want ok=%t", agent == want, ok, c.ok)
			}
		})
	}
}

func TestNoticeCollectionBounds(t *testing.T) {
	// Real shape (3e663996, a queued human prompt carrying an image): the prompt
	// is a content-block array, not a string,
	// {"type":"attachment","attachment":{"type":"queued_command","origin":{"kind":"human"},
	// "prompt":[{"type":"<str>","source":{"type":"<str>","media_type":"<str>","data":"<str>"}}, …]}}.
	// Such a line never fails the transcript, and a notice in that shape (as a
	// prompt or a user message's content) is read from its "text" blocks.
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}
	for _, form := range []string{"attachment", "user", "attachment blocks", "user blocks"} {
		t.Run(form, func(t *testing.T) {
			for _, c := range []struct {
				name               string
				since, until, from float64
				want               bool
			}{
				{name: "in window", since: 0, until: 30, want: true},
				{name: "before since", since: 10, until: 30},
				{name: "after until", since: 0, until: 8},
				{name: "before first row bound", since: 0, until: 30, from: 20},
				{name: "at first row bound", since: 0, until: 30, from: 19, want: true},
			} {
				t.Run(c.name, func(t *testing.T) {
					// Copy gym S2 lines 38/50: queued_command prompt with attachment
					// origin, or user string message.content with top-level origin.
					body := "<task-id>" + fxAgent + "</task-id><status>completed</status><summary>" + fxSecret + "</summary>"
					origin := map[string]any{"kind": "task-notification"}
					kind, blocks := strings.CutSuffix(form, " blocks")
					var prompt any = body
					if blocks {
						prompt = []any{map[string]any{"type": "text", "text": body}, image}
					}
					l := map[string]any{"type": kind, "timestamp": fxTS(9)}
					if kind == "attachment" {
						l["attachment"] = map[string]any{"type": "queued_command", "origin": origin, "prompt": prompt}
					} else {
						l["origin"] = origin
						l["message"] = map[string]any{"content": prompt}
					}
					human := map[string]any{"type": "attachment", "timestamp": fxTS(8), "attachment": map[string]any{
						"type": "queued_command", "origin": map[string]any{"kind": "human"}, "prompt": []any{image, map[string]any{"type": "text", "text": fxSecret}},
					}}
					path := filepath.Join(t.TempDir(), "notice.jsonl")
					writeLines(t, path, []string{jsonLine(t, human), jsonLine(t, l)})
					w := newWorld(fxMS(c.since), fxMS(c.until))
					if c.from != 0 {
						w.from[fxSession] = fxMS(c.from)
					}
					if _, err := w.readFile(path, fxSession, ""); err != nil {
						t.Fatalf("read notice: %v", err)
					}
					got := shownEnd(w, fxAgent, fxSession, sTurn{start: fxMS(5), ts: fxMS(9)})
					if got != c.want {
						t.Fatalf("notice shows end=%t, want %t", got, c.want)
					}
				})
			}
		})
	}
}

// TestUnfillableTailsAreTheStores: reconcile reuses nothing of
// internal/callmeter, so its copy of the four fault tails is pinned to the one
// RecoverQuiet writes.
func TestUnfillableTailsAreTheStores(t *testing.T) {
	if unfilledAgentTurn != callmeter.UnfilledAgentTurn || unfilledStopReply != callmeter.UnfilledStopReply || unfilledTurnEnd != callmeter.UnfilledTurnEnd ||
		unfilledCall != callmeter.UnfilledCall || unfilledAgentStop != callmeter.UnfilledAgentStop {
		t.Fatalf("tails = %q, %q, %q, %q, %q; want %q, %q, %q, %q, %q", unfilledAgentTurn, unfilledStopReply, unfilledTurnEnd, unfilledCall, unfilledAgentStop,
			callmeter.UnfilledAgentTurn, callmeter.UnfilledStopReply, callmeter.UnfilledTurnEnd, callmeter.UnfilledCall, callmeter.UnfilledAgentStop)
	}
}

func TestReconcileLandedCallWithoutARealSize(t *testing.T) {
	for _, c := range []struct {
		name, update, want string
		code               int
	}{
		{name: "landed without delivered", update: "bytes_real=NULL"},
		{name: "landed with delivered", update: "bytes_real=NULL, bytes_delivered=10"},
		{name: "batch only", update: "failed=NULL, bytes_real=NULL, bytes_delivered=95", code: 1, want: "MISMATCH call-batch-only session=" + fxSession + " ids=toolu_A2,tool=Agent"},
		{name: "unlanded", update: "failed=NULL, bytes_real=NULL", code: 1, want: "MISMATCH call-no-size session=" + fxSession + " ids=toolu_A2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil, []string{"UPDATE calls SET " + c.update + " WHERE tool_use_id='toolu_A2'"})
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if code != c.code {
				t.Errorf("exit=%d, want %d; %s", code, c.code, out)
			}
			if c.want != "" {
				if !strings.Contains(out, c.want) {
					t.Errorf("missing %q: %s", c.want, out)
				}
			} else if strings.Contains(out, "call-no-size") || strings.Contains(out, "call-batch-only") {
				t.Errorf("landed call misclassified: %s", out)
			}
		})
	}
}

// setTranscriptPath points a fixture session's sessions row at its transcript.
func setTranscriptPath(t *testing.T, f fixture, session, path string) {
	t.Helper()
	store, err := callmeter.OpenDB(context.Background(), f.db)
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	_, updateErr := store.DB().Exec(`UPDATE sessions SET transcript_path=? WHERE session_id=?`, path, session)
	closeErr := store.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("set transcript path: %v; close store: %v", updateErr, closeErr)
	}
}

// TestForkCopiedHistoryIsNotPreFirstRow: a fork's transcript opens with its
// parent's lines, same message ids and timestamps (real shape: the assistant
// lines of a `start_source=fork` session dated before its first row, whose
// message.id the store holds under the parent's session_id). They belong to
// the parent; the product never back-fills before a session's first row.
func TestForkCopiedHistoryIsNotPreFirstRow(t *testing.T) {
	for _, tc := range []struct {
		name, msgID string
		flagged     bool
	}{
		{name: "copied from the parent", msgID: "msg_A1"},
		{name: "its own pre-row line", msgID: "msg_F0", flagged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts, start_source) VALUES('%s', %d, %d, 'fork')`, fxSession2, fxMS(40), fxMS(41)),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, source) VALUES('k1', 'SessionStart', %d, '%s', 'fork')`, fxMS(40), fxSession2),
			})
			path := filepath.Join(f.projects, "-tmp-demo-proj", fxSession2+".jsonl")
			writeLines(t, path, []string{
				assistantLine(t, 2, tc.msgID, 5, nil, toolUse("toolu_A1", "Bash")),
				assistantLine(t, 2.1, tc.msgID, 50, "tool_use", toolUse("toolu_A2", "Agent")),
				jsonLine(t, map[string]any{"type": "system", "subtype": "local_command", "timestamp": fxTS(40.5), "sessionId": fxSession2, "content": fxSecret}),
			})
			setTranscriptPath(t, f, fxSession2, path)
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			want := "MISMATCH session-pre-first-row session=" + fxSession2 + " first_row=2030-01-01T00:00:40Z assistant_lines=2 tool_uses=2 start_source=fork"
			if got := strings.Contains(out, "session-pre-first-row session="+fxSession2); got != tc.flagged || (tc.flagged && !strings.Contains(out, want)) {
				t.Fatalf("pre-first-row flagged=%t, want %t (%q):\n%s", got, tc.flagged, want, out)
			}
		})
	}
}

// TestAgentToolUsesBeforeSince: ReadAgentTotals counts every distinct tool_use
// of the whole agent transcript, so one dated before --since still counts.
func TestAgentToolUsesBeforeSince(t *testing.T) {
	for _, count := range []int{2, 1} {
		t.Run(fmt.Sprintf("store tool uses=%d", count), func(t *testing.T) {
			f := newFixture(t, nil, []string{fmt.Sprintf(`UPDATE agents SET tool_uses=%d`, count)})
			writeLines(t, agentFile(f), append([]string{assistantLine(t, -10, "msg_B0", 4, "tool_use", toolUse("toolu_B0", "Read"))}, agentLines(t)...))
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			if count == 2 {
				if strings.Contains(out, "agent-tool-uses") {
					t.Fatalf("whole transcript count agrees:\n%s", out)
				}
				return
			}
			if want := fmt.Sprintf("MISMATCH agent-tool-uses session=%s ids=%s transcript=2 store=1", fxSession, fxAgent); !strings.Contains(out, want) {
				t.Fatalf("want %q:\n%s", want, out)
			}
		})
	}
}

// TestTaskNotificationWakeClosesAgentTurn: a sub-agent woken by a task
// notification gets the wake as a user line (real shape: isMeta true,
// origin.kind "task-notification", a promptId) after a final message whose
// stop_reason stayed null. Claude Code fired SubagentStop for that turn and
// SubagentStart for the wake: every pair is a turn.
func TestTaskNotificationWakeClosesAgentTurn(t *testing.T) {
	for _, turns := range []int{2, 1, 3} {
		t.Run(fmt.Sprintf("store turns=%d", turns), func(t *testing.T) {
			rows := []string{
				fmt.Sprintf(`INSERT INTO requests(request_id, session_id, agent_id, ts, model, stop_reason, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, calls, pending) VALUES('msg_B3', '%s', '%s', %d, 'claude-demo', 'end_turn', 3, 7, 100, 20, 0, 0)`, fxSession, fxAgent, fxMS(9.7)),
			}
			for seq := 2; seq <= turns; seq++ {
				rows = append(rows, fmt.Sprintf(`INSERT INTO agent_turns(agent_id, seq, session_id, agent_type, started, stopped) VALUES('%s', %d, '%s', 'demo-agent', %d, %d)`, fxAgent, seq, fxSession, fxMS(9.5), fxMS(9.8)))
			}
			f := newFixture(t, nil, rows)
			lines := agentLines(t)
			lines[len(lines)-1] = assistantLine(t, 8, "msg_B2", 30, nil, map[string]any{"type": "text", "text": fxSecret})
			wake := jsonLine(t, map[string]any{"type": "user", "timestamp": fxTS(9.5), "sessionId": fxSession, "entrypoint": "cli", "promptId": fxPrompt, "isMeta": true,
				"origin": map[string]any{"kind": "task-notification"}, "message": map[string]any{"role": "user", "content": fxSecret}})
			writeLines(t, agentFile(f), append(lines, wake, assistantLine(t, 9.7, "msg_B3", 7, "end_turn", map[string]any{"type": "text", "text": fxSecret})))
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if turns == 2 {
				if code != 0 || strings.Contains(out, "agent-turns") {
					t.Fatalf("two turns: exit %d, want 0 and no turn mismatch:\n%s", code, out)
				}
				return
			}
			if want := fmt.Sprintf("MISMATCH agent-turns session=%s ids=%s transcript_turns=2 store_turns=%d", fxSession, fxAgent, turns); code != 1 || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want 1 and %q:\n%s", code, want, out)
			}
		})
	}
}

// TestBlockedPromptIsExpected: a prompt another UserPromptSubmit hook blocked
// fired its UserPromptSubmit, and Claude Code wrote no prompt line, only a
// system line (real shape: subtype "informational", level "warning",
// preventContinuation true) right after it.
func TestBlockedPromptIsExpected(t *testing.T) {
	const blocked = "33333333-aaaa-4bbb-8ccc-000000000007"
	row := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id, prompt_id) VALUES('e9', 'UserPromptSubmit', %d, '%s', '%s')`, fxMS(30), fxSession, blocked)
	warning := func(sec float64) string {
		return jsonLine(t, map[string]any{"type": "system", "subtype": "informational", "level": "warning", "preventContinuation": true, "timestamp": fxTS(sec), "sessionId": fxSession, "entrypoint": "cli", "content": fxSecret})
	}
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"warning right after", []string{warning(30.2)}, "EXPECTED event-prompt-extra session=" + fxSession + " ids=" + blocked + " store_prompts=2: expected: " + reasonBlockedPrompt},
		{"no warning", nil, "MISMATCH event-prompt-extra session=" + fxSession + " ids=" + blocked},
		{"warning long after", []string{warning(40)}, "MISMATCH event-prompt-extra session=" + fxSession + " ids=" + blocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.extra, []string{row})
			if _, out := f.run(t, "2030-01-01T00:00:00Z"); !strings.Contains(out, tc.want) {
				t.Fatalf("want %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestTranscriptOnlyNotificationIsNoPrompt: a task notification Claude Code
// writes on resume without submitting it (real shape: a user line with
// origin.kind "task-notification", promptSource "system", queueTranscriptOnly
// and queueSkipAttachments true, its own promptId) fires no UserPromptSubmit.
func TestTranscriptOnlyNotificationIsNoPrompt(t *testing.T) {
	const pending = "33333333-aaaa-4bbb-8ccc-000000000008"
	for _, transcriptOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("queueTranscriptOnly=%t", transcriptOnly), func(t *testing.T) {
			v := map[string]any{"type": "user", "timestamp": fxTS(25), "sessionId": fxSession, "entrypoint": "cli", "promptId": pending,
				"origin": map[string]any{"kind": "task-notification"}, "promptSource": "system", "queueSkipAttachments": true,
				"message": map[string]any{"role": "user", "content": fxSecret}}
			if transcriptOnly {
				v["queueTranscriptOnly"] = true
			}
			f := newFixture(t, []string{jsonLine(t, v)}, nil)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if transcriptOnly {
				if code != 0 || strings.Contains(out, "event-prompt-missing") {
					t.Fatalf("a transcript-only notification is no prompt (exit %d):\n%s", code, out)
				}
				return
			}
			if want := "MISMATCH event-prompt-missing session=" + fxSession + " ids=" + pending; code != 1 || !strings.Contains(out, want) {
				t.Fatalf("exit %d, want 1 and %q:\n%s", code, want, out)
			}
		})
	}
}

// TestFailedTurnEndsInStopFailure: a turn ending in an API error (real shape:
// an assistant line with isApiErrorMessage true and model "<synthetic>") fires
// StopFailure, not Stop, and Claude Code may still write its turn_duration.
func TestFailedTurnEndsInStopFailure(t *testing.T) {
	apiError := jsonLine(t, map[string]any{"type": "assistant", "timestamp": fxTS(30), "sessionId": fxSession, "entrypoint": "cli", "isApiErrorMessage": true,
		"message": map[string]any{"id": "33333333-aaaa-4bbb-8ccc-00000000000a", "model": "<synthetic>", "stop_reason": "stop_sequence", "content": []map[string]any{{"type": "text", "text": fxSecret}}}})
	duration := jsonLine(t, map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": fxTS(30.1), "sessionId": fxSession, "entrypoint": "cli"})
	failure := fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('e9', 'StopFailure', %d, '%s')`, fxMS(30), fxSession)
	for _, tc := range []struct {
		name  string
		extra []string
		rows  []string
		want  string
	}{
		{name: "turn_duration after the error", extra: []string{apiError, duration}, rows: []string{failure}},
		{name: "no turn_duration after the error", extra: []string{apiError}, rows: []string{failure}},
		{name: "turn_duration with no error", extra: []string{duration}, rows: []string{failure}, want: "MISMATCH event-stop session=" + fxSession + " transcript_turns=2 store_stop=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.extra, tc.rows)
			code, out := f.run(t, "2030-01-01T00:00:00Z")
			if tc.want == "" {
				if code != 0 || strings.Contains(out, "event-stop") {
					t.Fatalf("Stop and StopFailure end the turns (exit %d):\n%s", code, out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("want %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestSessionEndBudgetFaultIsExpected: SessionEnd records its spent
// transcript-read budget as an id-less transcript fault by design
// (internal/hookentry/callmeter.go, sessionEndBudget); it hides nothing when
// none of the session's calls, requests or agents mismatch.
func TestSessionEndBudgetFaultIsExpected(t *testing.T) {
	fault := fmt.Sprintf(`INSERT INTO faults(ts, session_id, stage, error) VALUES(%d, '%s', 'transcript', 'SessionEnd spent its 1.2s budget: 1 transcript reads skipped, their requests and calls left as their hooks wrote them')`, fxMS(21), fxSession)
	for _, tc := range []struct {
		name, want string
		rows       []string
	}{
		{"session reconciles", "EXPECTED fault-transcript session=" + fxSession + " at=2030-01-01T00:00:21Z", []string{fault}},
		{"a request mismatches", "MISMATCH fault-transcript session=" + fxSession, []string{fault, `UPDATE requests SET output_tokens=1 WHERE request_id='msg_A1'`}},
		{"another transcript fault", "MISMATCH fault-transcript session=" + fxSession, []string{strings.Replace(fault, "SessionEnd spent its", "read stalled after", 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, tc.rows)
			if _, out := f.run(t, "2030-01-01T00:00:00Z"); !strings.Contains(out, tc.want) {
				t.Fatalf("want %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestIdleNotificationOnlySessionIsExpected: a chat already open when the hooks
// loaded (a /reload-plugins, a local command firing no SessionStart or
// UserPromptSubmit) whose first hook was an idle Notification. Real shape: the
// store's only rows are a sessions row and Notification events; the transcript
// holds a meta user line, a `<command-name>` user line and a system
// local_command line, and no assistant line.
func TestIdleNotificationOnlySessionIsExpected(t *testing.T) {
	for _, tc := range []struct {
		name, extra, want string
	}{
		{name: "Notification only", want: "EXPECTED session-promptless session=" + fxSession2 + " model=- start_source=- events=Notification:1: expected: " + reasonIdleNotification},
		{name: "with a prompt", extra: "UserPromptSubmit", want: "MISMATCH session-promptless session=" + fxSession2},
		{name: "with a SessionStart", extra: "SessionStart", want: "session-promptless session=" + fxSession2 + " model=- start_source=- events=Notification:1,SessionStart:1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []string{
				fmt.Sprintf(`INSERT INTO sessions(session_id, first_ts, last_ts) VALUES('%s', %d, %d)`, fxSession2, fxMS(40), fxMS(40)),
				fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('n1', 'Notification', %d, '%s')`, fxMS(40), fxSession2),
			}
			if tc.extra != "" {
				rows = append(rows, fmt.Sprintf(`INSERT INTO events(event_id, event, ts, session_id) VALUES('n2', '%s', %d, '%s')`, tc.extra, fxMS(39), fxSession2))
			}
			f := newFixture(t, nil, rows)
			path := filepath.Join(f.projects, "-tmp-demo-proj", fxSession2+".jsonl")
			writeLines(t, path, []string{
				jsonLine(t, map[string]any{"type": "user", "timestamp": fxTS(35), "sessionId": fxSession2, "isMeta": true, "message": map[string]any{"role": "user", "content": fxSecret}}),
				jsonLine(t, map[string]any{"type": "user", "timestamp": fxTS(35), "sessionId": fxSession2, "promptId": fxPrompt, "message": map[string]any{"role": "user", "content": "<command-name>/reload-plugins</command-name>"}}),
				jsonLine(t, map[string]any{"type": "system", "subtype": "local_command", "timestamp": fxTS(35.1), "sessionId": fxSession2, "content": fxSecret}),
			})
			setTranscriptPath(t, f, fxSession2, path)
			_, out := f.run(t, "2030-01-01T00:00:00Z")
			if !strings.Contains(out, tc.want) || (tc.name != "Notification only" && strings.Contains(out, "EXPECTED session-promptless")) {
				t.Fatalf("want %q:\n%s", tc.want, out)
			}
		})
	}
}
