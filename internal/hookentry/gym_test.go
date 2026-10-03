package hookentry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/report"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/runner"
)

// The real-session replays: testdata/verify (the lifecycle events of twelve
// headless sessions) and testdata/gym/{S} (the gymnastics sessions). Every
// expected value is read from the fixture files themselves.

// toolEvents are the payloads that write calls rows, never an events row.
var toolEvents = map[string]bool{"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true, "PostToolBatch": true}

// eventIDOf is the event_id of a payload as fed: the SHA-256 of its bytes.
func eventIDOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// text is fields[key] as a string; absent or not a string reads "".
func text(fields map[string]any, key string) string {
	value, _ := fields[key].(string)
	return value
}

// object is fields[key] as an object; absent reads nil.
func object(fields map[string]any, key string) map[string]any {
	value, _ := fields[key].(map[string]any)
	return value
}

// nullable is how lab.row renders a payload string: "" is a NULL column.
func nullable(value string) string {
	if value == "" {
		return "<nil>"
	}
	return value
}

// gymCall is one tool call as the gym payloads tell it.
type gymCall struct {
	tool, prompt, preCwd string
	batch                string // the batch entry's tool_response
	failed               bool
	pre, post            map[string]any
}

// calls gathers every tool call of the replay by tool_use_id.
func (r *replay) calls() map[string]*gymCall {
	r.t.Helper()
	calls := map[string]*gymCall{}
	get := func(id string) *gymCall {
		if calls[id] == nil {
			calls[id] = &gymCall{}
		}
		return calls[id]
	}
	for _, payload := range r.payloads {
		fields := decoded(r.t, payload)
		event := text(fields, "hook_event_name")
		if event == "PostToolBatch" {
			// A call the harness refused has its batch entry alone.
			for _, entry := range fields["tool_calls"].([]any) {
				call := get(text(entry.(map[string]any), "tool_use_id"))
				call.batch = text(entry.(map[string]any), "tool_response")
				if call.tool == "" {
					call.tool, call.prompt = text(entry.(map[string]any), "tool_name"), text(fields, "prompt_id")
				}
			}
			continue
		}
		if !toolEvents[event] {
			continue
		}
		call := get(text(fields, "tool_use_id"))
		call.tool, call.prompt = text(fields, "tool_name"), text(fields, "prompt_id")
		switch event {
		case "PreToolUse":
			call.pre, call.preCwd = fields, text(fields, "cwd")
		case "PostToolUseFailure":
			call.failed, call.post = true, fields
		default:
			call.post = fields
		}
	}
	return calls
}

// TestReplayVerify: every lifecycle payload of the verify capture is one
// events row carrying its named columns, and every Stop and SubagentStop one
// turns row. No payload of it faults: every transcript a payload needs was
// written by the capture.
func TestReplayVerify(t *testing.T) {
	r := newReplay(t, "verify")
	r.feedInOrder()
	// payload key -> events column, per event (0-schema.md § events).
	named := map[string]map[string]string{
		callmeter.EventSessionStart: {"source": "source"},
		callmeter.EventSessionEnd:   {"reason": "reason"},
		"Setup":                     {"trigger": "trigger"},
		"PreCompact":                {"trigger": "trigger"},
		"PostCompact":               {"trigger": "trigger"},
		"StopFailure":               {"error": "error_type"},
		"InstructionsLoaded":        {"load_reason": "load_reason", "memory_type": "memory_type", "file_path": "file_path"},
		"PermissionRequest":         {"tool_name": "tool_name"},
		"PermissionDenied":          {"tool_name": "tool_name"},
		"UserPromptExpansion":       {"command_name": "command_name"},
		"TaskCreated":               {"task_id": "task_id"},
		"TaskCompleted":             {"task_id": "task_id"},
	}
	seen := map[string]bool{}
	turns := 0
	for _, payload := range r.payloads {
		fields := decoded(t, payload)
		event := text(fields, "hook_event_name")
		if toolEvents[event] {
			continue
		}
		id := eventIDOf(payload)
		row := r.lab.row("SELECT * FROM events WHERE event_id = ?", id)
		want := map[string]any{"event": event, "session_id": text(fields, "session_id")}
		for key, column := range named[event] {
			want[column] = nullable(text(fields, key))
			seen[event+" "+column+"="+text(fields, key)] = true
		}
		seen[event] = true
		expect(t, event+" "+id[:8], row, want)
		if event == eventStop || event == callmeter.EventSubagentStop {
			turns++
			if n := r.lab.count("SELECT COUNT(*) FROM turns WHERE event_id = ?", id); n != 1 {
				t.Errorf("%s %s: %d turns rows, want 1", event, id[:8], n)
			}
		}
	}
	for _, want := range []string{
		"SessionStart source=startup", "SessionStart source=resume", "SessionStart source=compact",
		"SessionEnd reason=other", "InstructionsLoaded", "PreCompact trigger=manual", "PostCompact trigger=manual",
		"StopFailure error_type=model_not_found", "StopFailure error_type=max_output_tokens",
		"PermissionRequest", "UserPromptExpansion", "Setup trigger=init", "TaskCreated", "TaskCompleted",
	} {
		if !seen[want] {
			t.Errorf("the verify fixture never fed %q: the replay proves less than it claims", want)
		}
	}
	if n := r.lab.count("SELECT COUNT(*) FROM turns"); n != turns {
		t.Errorf("turns holds %d rows, want one per Stop and SubagentStop payload: %d", n, turns)
	}
	if faults := r.faults(); len(faults) != 0 {
		t.Errorf("the verify replay faulted:\n%s", strings.Join(faults, "\n"))
	}
}

// TestReplayGym: every tool call of every gym session has a calls row with
// its tool, failed flag and prompt; after the last Stop no request is pending
// and nothing faulted.
func TestReplayGym(t *testing.T) {
	for _, session := range gymSessions {
		t.Run(session, func(t *testing.T) {
			t.Parallel()
			r := newReplay(t, "gym/"+session)
			r.feedInOrder()
			calls := r.calls()
			if n := r.lab.count("SELECT COUNT(*) FROM calls"); n != len(calls) {
				t.Errorf("calls holds %d rows, want the %d tool calls of the payloads", n, len(calls))
			}
			for id, call := range calls {
				want := map[string]any{"tool": call.tool, "failed": 0}
				refused := call.pre == nil && call.post == nil && strings.HasPrefix(call.batch, "<tool_use_error>")
				if call.failed || refused {
					want["failed"] = 1
				}
				if call.prompt != "" {
					want["prompt_id"] = call.prompt
				}
				expect(t, "call "+id, r.lab.call(id), want)
			}
			if n := r.lab.count("SELECT COUNT(*) FROM requests"); n == 0 {
				t.Error("no requests row: the replay resolved nothing")
			}
			if n := r.lab.count("SELECT COUNT(*) FROM requests WHERE pending != 0"); n != 0 {
				t.Errorf("%d requests still pending after the last Stop", n)
			}
			if faults := r.faults(); len(faults) != 0 {
				t.Errorf("the %s replay faulted:\n%s", session, strings.Join(faults, "\n"))
			}
		})
	}
}

// commandCall is the id and Post payload of the gym call running command.
func (r *replay) commandCall(command string) (string, *gymCall) {
	r.t.Helper()
	for id, call := range r.calls() {
		if call.post != nil && text(object(call.post, "tool_input"), "command") == command {
			return id, call
		}
	}
	r.t.Fatalf("%s has no call of %q", r.fixture, command)
	return "", nil
}

// batchResponse is the tool_response string a PostToolBatch carries for id.
func (r *replay) batchResponse(id string) string {
	r.t.Helper()
	for _, i := range r.matching("PostToolBatch", nil) {
		for _, entry := range decoded(r.t, r.payloads[i])["tool_calls"].([]any) {
			if text(entry.(map[string]any), "tool_use_id") == id {
				return text(entry.(map[string]any), "tool_response")
			}
		}
	}
	r.t.Fatalf("%s: no batch entry for %s", r.fixture, id)
	return ""
}

func TestReplayPersistedOutput(t *testing.T) {
	r := newReplay(t, "gym/S1")
	r.feedInOrder()
	id, call := r.commandCall("seq 1 9000")
	persisted := text(object(call.post, "tool_response"), "persistedOutputPath")
	info, err := os.Stat(persisted)
	if err != nil {
		t.Fatalf("the persisted output the payload names is not in the fixture: %v", err)
	}
	wrapper := r.batchResponse(id)
	if !strings.HasPrefix(wrapper, "<persisted-output>") {
		t.Fatalf("the batch entry of %s is not the persisted-output wrapper", id)
	}
	expect(t, "seq call", r.lab.call(id), map[string]any{
		"bytes_real": info.Size(), "bytes_delivered": len(wrapper), "persisted_path": persisted,
	})
}

func TestReplayOversizedRead(t *testing.T) {
	r := newReplay(t, "gym/S1b")
	r.feedInOrder()
	for id, call := range r.calls() {
		if call.tool != "Read" || !strings.HasSuffix(text(object(call.post, "tool_input"), "file_path"), "/wide.txt") {
			continue
		}
		expect(t, "wide.txt Read", r.lab.call(id), map[string]any{"failed": 1, "error": callmeter.ErrorNotStored})
		return
	}
	t.Fatal("S1b has no Read of wide.txt")
}

// TestReplayCwdPersistence: S1's `cd sub && …` moved the Bash tool's
// directory for the calls after it in the same batch; each call's cwd is the
// directory its PreToolUse reported, the one before its command ran.
func TestReplayCwdPersistence(t *testing.T) {
	r := newReplay(t, "gym/S1")
	r.feedInOrder()
	cdID, cd := r.commandCall("cd sub && grep -n hello notes.txt | head -n 1")
	if cd.preCwd == text(cd.post, "cwd") {
		t.Fatalf("the cd call's PreToolUse and PostToolUse report one cwd %q: the fixture shows no move", cd.preCwd)
	}
	expect(t, "cd call", r.lab.call(cdID), map[string]any{"cwd": cd.preCwd})
	moved := 0
	for id, call := range r.calls() {
		if call.tool != "Bash" {
			continue
		}
		expect(t, "Bash "+id, r.lab.call(id), map[string]any{"cwd": call.preCwd})
		if call.failed && call.preCwd == text(cd.post, "cwd") {
			moved++
		}
	}
	if moved == 0 {
		t.Error("no failed Bash call ran in the directory the cd left behind")
	}
}

// TestReplayAgentTotalsWaitForTheStop: totals wait for SubagentStop hooks;
// quiet recovery rebuilds missing stops from turn ends or task notifications.
func TestReplayAgentTotalsWaitForTheStop(t *testing.T) {
	full := newReplay(t, "gym/S2")
	full.feedInOrder()
	r := newReplay(t, "gym/S2")
	for i, payload := range r.payloads {
		if eventName(t, payload) == callmeter.EventSubagentStop {
			continue
		}
		r.feedIndex(i, payload)
		if n := r.lab.count("SELECT COUNT(*) FROM agents WHERE total_tokens IS NOT NULL OR tool_uses IS NOT NULL"); n != 0 {
			t.Errorf("hook %d: %d agents have totals before their stop", i, n)
		}
	}
	// Recover through the replay's real store, after both hook times and
	// the copied transcripts' real mtimes have been quiet for QuietAfter.
	if _, err := r.lab.db().RecoverQuiet(r.lab.ctx, clock.Real.Now().Add(2*callmeter.QuietAfter), callmeter.QuietAfter); err != nil {
		t.Fatalf("recover agent stops: %v", err)
	}
	rows, err := r.lab.db().DB().QueryContext(r.lab.ctx,
		"SELECT DISTINCT agent_id FROM events WHERE event = ? AND detail = ?", callmeter.EventSubagentStop, callmeter.RecoveredDetail)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	recovered := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		recovered[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for id := range recovered {
		want := full.lab.row("SELECT total_tokens, tool_uses FROM agents WHERE agent_id = ?", id)
		expect(t, "recovered agent "+id, r.lab.row("SELECT * FROM agents WHERE agent_id = ?", id),
			map[string]any{"total_tokens": want["total_tokens"], "tool_uses": want["tool_uses"]})
	}
	for _, id := range []string{"a17591d0a08cc3b13", "a88a0d95598144e27", "a8a1da5926e2e3168"} {
		if !recovered[id] {
			t.Errorf("agent %s: no recovered SubagentStop", id)
		}
	}
	// S2's nested background agent has no turn end; its attachment notice
	// supplies the stop time, while its own transcript supplies the totals.
	const nested = "a8a1da5926e2e3168"
	notice, err := time.Parse(time.RFC3339Nano, "2026-10-01T00:52:21.292Z")
	if err != nil {
		t.Fatal(err)
	}
	expect(t, "notice stop "+nested,
		r.lab.row("SELECT ts FROM events WHERE event = ? AND agent_id = ? AND detail = ?", callmeter.EventSubagentStop, nested, callmeter.RecoveredDetail),
		map[string]any{"ts": notice.UnixMilli()})
	if n := r.lab.count("SELECT COUNT(*) FROM faults WHERE stage = ? AND error LIKE ? AND error LIKE ?", callmeter.StageTranscript,
		"agent "+nested+" turn % open: %", "%"+callmeter.UnfilledAgentStop); n != 0 {
		t.Errorf("agent %s: %d unfilled stop faults, want 0", nested, n)
	}
}

func TestReplayAgents(t *testing.T) {
	const background, parent, nested = "a88a0d95598144e27", "a17591d0a08cc3b13", "a8a1da5926e2e3168"
	r := newReplay(t, "gym/S2")
	r.feedInOrder()
	if n := r.lab.count("SELECT COUNT(*) FROM agent_turns WHERE agent_id = ?", background); n != 2 {
		t.Errorf("background agent: %d agent_turns rows, want 2", n)
	}
	// Woken twice in one prompt, the agent's two SubagentStart payloads are the
	// same bytes seconds apart: two occurrences, so each turn has its own start.
	first := r.lab.row("SELECT started, stopped FROM agent_turns WHERE agent_id = ? AND seq = 1", background)
	if first["started"] == "<nil>" || first["stopped"] == "<nil>" {
		t.Errorf("background agent turn 1: started %s, stopped %s; want both set", first["started"], first["stopped"])
	}
	second := r.lab.row("SELECT started, stopped FROM agent_turns WHERE agent_id = ? AND seq = 2", background)
	if second["started"] == "<nil>" || second["stopped"] == "<nil>" {
		t.Errorf("background agent turn 2: started %s, stopped %s; want both set", second["started"], second["stopped"])
	}
	if n := r.lab.count("SELECT COUNT(*) FROM agent_turns WHERE agent_id = ?", nested); n != 1 {
		t.Errorf("nested agent: %d agent_turns rows, want 1", n)
	}
	parentStop := r.lab.row("SELECT stopped FROM agent_turns WHERE agent_id = ?", parent)["stopped"]
	nestedStop := r.lab.row("SELECT stopped FROM agent_turns WHERE agent_id = ?", nested)["stopped"]
	if a, b := atoi(t, parentStop), atoi(t, nestedStop); b <= a {
		t.Errorf("nested agent stopped at %d, want after its parent's stop %d", b, a)
	}
	stops := map[string]bool{}
	for _, i := range append(r.matching(eventStop, nil), r.matching(callmeter.EventSubagentStop, nil)...) {
		stops[eventIDOf(r.payloads[i])] = true
	}
	if n := r.lab.count("SELECT COUNT(*) FROM turns"); n != len(stops) {
		t.Errorf("turns holds %d rows, want one per distinct Stop and SubagentStop payload: %d", n, len(stops))
	}
	notifications := 0
	for _, i := range r.matching("UserPromptSubmit", nil) {
		fields := decoded(t, r.payloads[i])
		detail := r.lab.row("SELECT detail FROM events WHERE event_id = ?", eventIDOf(r.payloads[i]))["detail"]
		marked := strings.Contains(detail, `"task_notification":true`)
		isNotification := strings.HasPrefix(text(fields, "prompt"), "<task-notification>")
		if marked != isNotification {
			t.Errorf("prompt %d: task_notification marked %v, want %v", i, marked, isNotification)
		}
		if isNotification {
			notifications++
		}
	}
	if notifications == 0 {
		t.Error("S2 fed no task-notification prompt")
	}
}

func atoi(t *testing.T, value string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("not a number: %q", value)
	}
	return n
}

// TestReplayCompactions: S3's two compactions each fired a SubagentStop for
// an internal agent with no type and no transcript: a turns row each, no
// agent rows. Five processes resumed one session: one sessions row.
func TestReplayCompactions(t *testing.T) {
	r := newReplay(t, "gym/S3")
	r.feedInOrder()
	compactions := r.matching(callmeter.EventSubagentStop, func(f map[string]any) bool { return text(f, "agent_type") == "" })
	if len(compactions) != 2 {
		t.Fatalf("S3 holds %d untyped SubagentStops, want its 2 compactions", len(compactions))
	}
	for _, i := range compactions {
		agent := text(decoded(t, r.payloads[i]), "agent_id")
		if n := r.lab.count("SELECT COUNT(*) FROM agents WHERE agent_id = ?", agent) +
			r.lab.count("SELECT COUNT(*) FROM agent_turns WHERE agent_id = ?", agent); n != 0 {
			t.Errorf("compaction agent %s has %d agents/agent_turns rows, want none", agent, n)
		}
	}
	if n := r.lab.count("SELECT COUNT(*) FROM turns WHERE event = 'SubagentStop'"); n != 2 {
		t.Errorf("%d SubagentStop turns rows, want 2", n)
	}
	var wantSources []string
	for _, i := range r.matching(callmeter.EventSessionStart, nil) {
		wantSources = append(wantSources, text(decoded(t, r.payloads[i]), "source"))
	}
	var gotSources []string
	for _, row := range r.lab.dump()["events"] {
		if strings.Contains(row, `event="SessionStart"`) {
			gotSources = append(gotSources, row)
		}
	}
	slices.SortFunc(gotSources, func(a, b string) int { return strings.Compare(column(a, "ts"), column(b, "ts")) })
	for i := range gotSources {
		gotSources[i] = strings.Trim(column(gotSources[i], "source"), `"`)
	}
	if !slices.Equal(gotSources, wantSources) {
		t.Errorf("SessionStart sources by ts = %v, want the payload order %v", gotSources, wantSources)
	}
	if n := r.lab.count("SELECT COUNT(*) FROM sessions"); n != 1 {
		t.Errorf("%d sessions rows, want 1 across %d processes", n, len(r.matching(callmeter.EventSessionEnd, nil)))
	}
	expect(t, "session", r.lab.row("SELECT start_source FROM sessions"), map[string]any{"start_source": "startup"})
}

// column is one `name=value` cell of a dump row.
func column(row, name string) string {
	for _, cell := range strings.Split(row, " | ") {
		if value, ok := strings.CutPrefix(cell, name+"="); ok {
			return value
		}
	}
	return ""
}

func TestReplayTokenSplit(t *testing.T) {
	for _, session := range gymSessions {
		t.Run(session, func(t *testing.T) {
			t.Parallel()
			r := newReplay(t, "gym/"+session)
			r.feedInOrder()
			if n := r.lab.count("SELECT COUNT(*) FROM requests WHERE context_tokens IS NOT NULL"); n == 0 {
				t.Fatal("no request carries tokens")
			}
			if n := r.lab.count(`SELECT COUNT(*) FROM requests WHERE context_tokens IS NULL
				OR input_tokens + cache_read_tokens + cache_creation_tokens != context_tokens`); n != 0 {
				t.Errorf("%d requests whose input + cache_read + cache_creation is not context_tokens", n)
			}
			if n := r.lab.count(`SELECT COUNT(*) FROM requests WHERE cache_creation_5m_tokens IS NOT NULL
				AND cache_creation_1h_tokens IS NOT NULL
				AND cache_creation_5m_tokens + cache_creation_1h_tokens != cache_creation_tokens`); n != 0 {
				t.Errorf("%d requests whose 5m + 1h is not cache_creation", n)
			}
		})
	}
}

// TestReplayOddPaths: S4's paths with spaces, unicode, a symlink, a deleted
// file and an image are stored byte-exact; the deleted file's Read failed and
// the Bash timeout is a failure, not an interrupt.
func TestReplayOddPaths(t *testing.T) {
	r := newReplay(t, "gym/S4")
	r.feedInOrder()
	kinds := map[string]bool{}
	for id, call := range r.calls() {
		input := object(call.post, "tool_input")
		if path := text(input, "file_path"); path != "" {
			expect(t, call.tool+" "+id, r.lab.call(id), map[string]any{"file_path": path})
			for kind, marker := range map[string]string{"space": " ", "symlink": "link-to-", "image": ".png", "deleted": "doomed"} {
				if strings.Contains(path, marker) {
					kinds[kind] = true
				}
			}
			if strings.ContainsFunc(path, func(c rune) bool { return c > 127 }) {
				kinds["unicode"] = true
			}
			if call.tool == "Read" && strings.HasSuffix(path, "/doomed.txt") {
				expect(t, "deleted-file Read", r.lab.call(id), map[string]any{"failed": 1})
			}
		}
		if _, ok := input["timeout"]; ok && call.tool == "Bash" {
			kinds["timeout"] = true
			expect(t, "timed-out Bash", r.lab.call(id), map[string]any{"failed": 1, "is_interrupt": 0})
		}
	}
	for _, kind := range []string{"space", "unicode", "symlink", "image", "deleted", "timeout"} {
		if !kinds[kind] {
			t.Errorf("S4 fed no %s path or call", kind)
		}
	}
}

// patchLines counts the + and - lines of a Write or Edit PostToolUse's
// structuredPatch; a Write that created its file counts its content's lines.
func patchLines(response, input map[string]any) (added, removed int) {
	hunks, _ := response["structuredPatch"].([]any)
	if text(response, "type") == "create" && len(hunks) == 0 {
		content := text(input, "content")
		added = strings.Count(content, "\n")
		if content != "" && !strings.HasSuffix(content, "\n") {
			added++
		}
		return added, 0
	}
	for _, hunk := range hunks {
		lines, _ := hunk.(map[string]any)["lines"].([]any)
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line.(string), "+"):
				added++
			case strings.HasPrefix(line.(string), "-"):
				removed++
			}
		}
	}
	return added, removed
}

func TestReplayOutcomes(t *testing.T) {
	for session, want := range map[string][]string{"S1": {"Write", "Edit"}, "S3": {"Edit"}} {
		t.Run(session, func(t *testing.T) {
			t.Parallel()
			r := newReplay(t, "gym/"+session)
			r.feedInOrder()
			seen := map[string]int{}
			for id, call := range r.calls() {
				if !slices.Contains(want, call.tool) || call.post == nil {
					continue
				}
				seen[call.tool]++
				added, removed := patchLines(object(call.post, "tool_response"), object(call.post, "tool_input"))
				expect(t, call.tool+" "+id, r.lab.call(id), map[string]any{"lines_added": added, "lines_removed": removed})
			}
			for _, tool := range want {
				if seen[tool] == 0 {
					t.Errorf("%s fed no %s call", session, tool)
				}
			}
		})
	}
}

// permutationSeeds fix the arrival orders: a failing seed replays exactly.
var permutationSeeds = []uint64{1, 7, 42, 1009, 65537}

// TestReplayPermutedArrival: the store is a function of the set of (payload,
// ts) pairs, never of their arrival order.
func TestReplayPermutedArrival(t *testing.T) {
	for _, session := range gymSessions {
		t.Run(session, func(t *testing.T) {
			t.Parallel()
			for _, seed := range permutationSeeds {
				t.Run(fmt.Sprint(seed), func(t *testing.T) {
					t.Parallel()
					// Both labs under one test: their roots, which sizes and
					// hashes carry, have one length.
					inOrder := newReplay(t, "gym/"+session)
					inOrder.feedInOrder()
					want := inOrder.normalized()
					r := newReplay(t, "gym/"+session)
					r.feedOrder(rand.New(rand.NewPCG(seed, seed)).Perm(len(r.payloads)))
					sameDump(t, fmt.Sprintf("%s seed %d", session, seed), want, r.normalized())
				})
			}
		})
	}
}

// TestReplayDuplicates: every payload delivered twice stores what one
// delivery stores.
func TestReplayDuplicates(t *testing.T) {
	for _, session := range gymSessions {
		t.Run(session, func(t *testing.T) {
			t.Parallel()
			once := newReplay(t, "gym/"+session)
			once.feedInOrder()
			twice := newReplay(t, "gym/"+session)
			twice.feedInOrder()
			twice.feedInOrder()
			sameDump(t, session+" fed twice", once.normalized(), twice.normalized())
		})
	}
}

// TestReplayLostPostToolUse: a call whose PostToolUse never arrived keeps
// what its PreToolUse and its batch said, the main chat's Stop sweep fills the
// real size PostToolUse would have stored from the transcript result's
// toolUseResult, and the reports still run.
func TestReplayLostPostToolUse(t *testing.T) {
	full := newReplay(t, "gym/S1")
	full.feedInOrder()
	r := newReplay(t, "gym/S1")
	id, call := r.commandCall("wc -l data.csv")
	wantReal := full.lab.call(id)["bytes_real"]
	if wantReal == "<nil>" {
		t.Fatalf("the full replay stored no real size for %s", id)
	}
	lost := -1
	for i, payload := range r.payloads {
		fields := decoded(t, payload)
		if text(fields, "hook_event_name") == "PostToolUse" && text(fields, "tool_use_id") == id {
			lost = i
		}
	}
	for i, payload := range r.payloads {
		if i != lost {
			r.feedIndex(i, payload)
		}
	}
	row := r.lab.call(id)
	expect(t, "call without PostToolUse", row, map[string]any{
		"tool": "Bash", "cwd": call.preCwd, "bytes_delivered": len(r.batchResponse(id)), "prompt_id": call.prompt,
		"bytes_real": wantReal,
	})
	for _, column := range []string{"ts", "request_id"} {
		if row[column] == "<nil>" {
			t.Errorf("call without PostToolUse lost %s", column)
		}
	}
	if faults := r.faults(); len(faults) != 0 {
		t.Errorf("a lost PostToolUse faulted:\n%s", strings.Join(faults, "\n"))
	}
	store := r.lab.db()
	if _, err := report.EnsureParsed(r.lab.ctx, store, r.lab.home, nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	if _, err := report.Files(r.lab.ctx, store, report.Filter{}, nil); err != nil {
		t.Errorf("report files: %v", err)
	}
	if _, err := report.Commands(r.lab.ctx, store, report.Filter{}, nil); err != nil {
		t.Errorf("report commands: %v", err)
	}
}

// TestReplayBlockedCall: a call a hook blocked has a PreToolUse and nothing
// after it: a calls row of what PreToolUse knows, the result columns NULL.
func TestReplayBlockedCall(t *testing.T) {
	r := newReplay(t, "gym/S1")
	_, call := r.commandCall("wc -l data.csv")
	blocked := withFields(t, r.payloads[slices.IndexFunc(r.payloads, func(p string) bool {
		f := decoded(t, p)
		return text(f, "hook_event_name") == "PreToolUse" && text(object(f, "tool_input"), "command") == "wc -l data.csv"
	})], map[string]any{"tool_use_id": "toolu_blocked_synthetic"})
	r.feedIndex(0, blocked)
	row := r.lab.call("toolu_blocked_synthetic")
	expect(t, "blocked call", row, map[string]any{"ts": replayEpoch, "tool": "Bash", "cwd": call.preCwd})
	for _, column := range []string{"duration_ms", "failed", "is_interrupt", "error", "bytes_real", "bytes_delivered", "request_id"} {
		if row[column] != "<nil>" {
			t.Errorf("blocked call %s = %s, want NULL: nothing ran", column, row[column])
		}
	}
	if faults := r.faults(); len(faults) != 0 {
		t.Errorf("a blocked call faulted:\n%s", strings.Join(faults, "\n"))
	}
}

// TestReplayTruncatedTranscript: a batch whose request is not on disk yet (the
// transcript ends mid-line before it) leaves the request pending without a
// fault; the Stop after the transcript caught up resolves it.
func TestReplayTruncatedTranscript(t *testing.T) {
	r := newReplay(t, "gym/S1")
	batch := r.matching("PostToolBatch", nil)[0]
	id := text(decoded(t, r.payloads[batch])["tool_calls"].([]any)[0].(map[string]any), "tool_use_id")
	transcript := text(decoded(t, r.payloads[0]), "transcript_path")
	full, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	lines := strings.SplitAfter(string(full), "\n")
	at := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, `"id":"`+id+`"`) })
	if at < 0 {
		t.Fatalf("the transcript holds no tool_use %s", id)
	}
	r.lab.write(transcript, []byte(strings.Join(lines[:at], "")+lines[at][:len(lines[at])/2]))
	for i := 0; i <= batch; i++ {
		r.feedIndex(i, r.payloads[i])
	}
	request := r.lab.call(id)["request_id"]
	expect(t, "request before the Stop", r.lab.row("SELECT pending FROM requests WHERE request_id = ?", request),
		map[string]any{"pending": 1})
	r.lab.write(transcript, full)
	for i := batch + 1; i < len(r.payloads); i++ {
		r.feedIndex(i, r.payloads[i])
	}
	if n := r.lab.count("SELECT COUNT(*) FROM requests WHERE pending != 0"); n != 0 {
		t.Errorf("%d requests pending after the Stop", n)
	}
	if faults := r.faults(); len(faults) != 0 {
		t.Errorf("a truncated transcript faulted:\n%s", strings.Join(faults, "\n"))
	}
}

// TestReplaySyntheticTools: the tools no capture holds — MultiEdit, Grep,
// Glob — built from S1's real Edit and Read payloads, and an opus request in
// a synthesized transcript.
func TestReplaySyntheticTools(t *testing.T) {
	const session, opus = "5e55a0f1-0000-4000-8000-000000000001", "claude-opus-4-1-20250805"
	r := newReplay(t, "gym/S1")
	var edits []map[string]any
	var read map[string]any
	for _, payload := range r.payloads {
		fields := decoded(t, payload)
		if text(fields, "hook_event_name") != "PostToolUse" {
			continue
		}
		switch text(fields, "tool_name") {
		case "Edit":
			edits = append(edits, fields)
		case "Read":
			if read == nil {
				read = fields
			}
		}
	}
	if len(edits) < 2 || read == nil {
		t.Fatalf("S1 holds %d Edit and no Read PostToolUse to build from", len(edits))
	}
	transcript := r.lab.transcript(session)
	retarget := func(fields map[string]any, id, tool string, input, response any) map[string]any {
		out := map[string]any{}
		for key, value := range fields {
			out[key] = value
		}
		out["session_id"], out["transcript_path"], out["tool_use_id"], out["tool_name"] = session, transcript, id, tool
		out["tool_input"], out["tool_response"] = input, response
		return out
	}
	// MultiEdit: one file, every Edit's hunk.
	var hunks, editInputs []any
	wantAdded, wantRemoved := 0, 0
	for _, edit := range edits {
		response := object(edit, "tool_response")
		hunks = append(hunks, response["structuredPatch"].([]any)...)
		input := object(edit, "tool_input")
		editInputs = append(editInputs, map[string]any{"old_string": input["old_string"], "new_string": input["new_string"]})
		added, removed := patchLines(response, input)
		wantAdded, wantRemoved = wantAdded+added, wantRemoved+removed
	}
	path := text(object(edits[0], "tool_input"), "file_path")
	multiResponse := map[string]any{}
	for key, value := range object(edits[0], "tool_response") {
		multiResponse[key] = value
	}
	multiResponse["structuredPatch"], multiResponse["edits"] = hunks, editInputs
	stats := text(object(read, "tool_input"), "file_path")
	payloads := []map[string]any{
		retarget(edits[0], "toolu_synthetic_multiedit", "MultiEdit",
			map[string]any{"file_path": path, "edits": editInputs}, multiResponse),
		retarget(read, "toolu_synthetic_grep", "Grep",
			map[string]any{"pattern": "func", "path": r.lab.proj, "output_mode": "files_with_matches"},
			map[string]any{"mode": "files_with_matches", "filenames": []any{stats}, "numFiles": 1}),
		retarget(read, "toolu_synthetic_glob", "Glob",
			map[string]any{"pattern": "**/*.md", "path": r.lab.proj},
			map[string]any{"filenames": []any{stats}, "numFiles": 1, "durationMs": 3, "truncated": false}),
	}
	// The transcript: S1's first tool-use request, its model opus and its
	// tool_use blocks the three synthetic calls.
	full, err := os.ReadFile(text(decoded(t, r.payloads[0]), "transcript_path"))
	if err != nil {
		t.Fatalf("read S1 transcript: %v", err)
	}
	var entry map[string]any
	for _, line := range strings.Split(string(full), "\n") {
		if strings.Contains(line, `"type":"tool_use"`) && strings.Contains(line, `"usage"`) {
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("decode transcript entry: %v", err)
			}
			break
		}
	}
	message := object(entry, "message")
	var block map[string]any
	for _, content := range message["content"].([]any) {
		if text(content.(map[string]any), "type") == "tool_use" {
			block = content.(map[string]any)
		}
	}
	var blocks []any
	var batch []any
	for _, payload := range payloads {
		use := map[string]any{}
		for key, value := range block {
			use[key] = value
		}
		use["id"], use["name"], use["input"] = payload["tool_use_id"], payload["tool_name"], payload["tool_input"]
		blocks = append(blocks, use)
		batch = append(batch, map[string]any{
			"tool_name": payload["tool_name"], "tool_input": payload["tool_input"], "tool_use_id": payload["tool_use_id"],
			"tool_response": "synthetic result",
		})
	}
	message["model"], message["id"], message["content"] = opus, "msg_synthetic_opus", blocks
	entry["sessionId"] = session
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("encode transcript entry: %v", err)
	}
	r.lab.write(transcript, append(line, '\n'))
	batchPayload := map[string]any{}
	for key, value := range decoded(t, r.payloads[r.matching("PostToolBatch", nil)[0]]) {
		batchPayload[key] = value
	}
	batchPayload["session_id"], batchPayload["transcript_path"], batchPayload["tool_calls"] = session, transcript, batch
	for i, payload := range append(payloads, batchPayload) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		r.feedIndex(i, string(encoded))
	}
	expect(t, "MultiEdit", r.lab.call("toolu_synthetic_multiedit"), map[string]any{
		"tool": "MultiEdit", "lines_added": wantAdded, "lines_removed": wantRemoved, "file_path": path,
	})
	expect(t, "Grep", r.lab.call("toolu_synthetic_grep"), map[string]any{"tool": "Grep"})
	expect(t, "Glob", r.lab.call("toolu_synthetic_glob"), map[string]any{"tool": "Glob"})
	request := r.lab.call("toolu_synthetic_grep")["request_id"]
	expect(t, "opus request", r.lab.row("SELECT model, pending FROM requests WHERE request_id = ?", request),
		map[string]any{"model": opus, "pending": 0})
	if faults := r.faults(); len(faults) != 0 {
		t.Errorf("the synthetic tools faulted:\n%s", strings.Join(faults, "\n"))
	}
}

// leftovers are what scripts/sanitize-capture.py never lets through, spelled
// so this file does not match the leak gate's own patterns.
var leftovers = []string{"/Us" + "ers/", "/ho" + "me/", "/pri" + "vate/", "/tmp/call" + "meter/", "claude-" + "501"}

var emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// TestFixturesSanitized: no fixture file under testdata/verify or
// testdata/gym holds a machine path or an email other than the neutral one
// and Claude Code's own noreply; and the sanitizer refuses a leftover, naming
// its file and line, writing nothing. The private terms are scripts/leak-check.sh's.
func TestFixturesSanitized(t *testing.T) {
	files := 0
	for _, dir := range []string{"verify", "gym"} {
		err := filepath.WalkDir(filepath.Join("testdata", dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			files++
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for n, line := range bytes.Split(data, []byte("\n")) {
				for _, bad := range leftovers {
					if bytes.Contains(line, []byte(bad)) {
						t.Errorf("%s:%d holds %q", path, n+1, bad)
					}
				}
				for _, address := range emailAddress.FindAll(line, -1) {
					if a := string(address); a != "noreply@anthropic.com" && a != "user@example.com" {
						t.Errorf("%s:%d holds an email address other than user@example.com", path, n+1)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk testdata/%s: %v", dir, err)
		}
	}
	if files == 0 {
		t.Fatal("no fixture file scanned")
	}

	root := t.TempDir()
	in, out, terms := filepath.Join(root, "in"), filepath.Join(root, "out"), filepath.Join(root, "terms.txt")
	machine := "/Us" + "ers/someone"
	linuxMachine := "/ho" + "me/someone"
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatalf("create input: %v", err)
	}
	for name, content := range map[string]string{
		"a.jsonl": "clean line\n" + machine + "/proj/x.go\nSecretTerm inside\n",
		// The second address follows a JSON newline escape: its n stays.
		"b.txt": "write to me" + "@corp.example" + ".net\\nme" + "@corp.example" + ".net\n",
		// A capture taken on Linux, the fence's machine.
		"c.txt": linuxMachine + "/proj/y.go\n",
	} {
		if err := os.WriteFile(filepath.Join(in, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.WriteFile(terms, []byte("# a test term\nsecretterm\n"), 0o644); err != nil {
		t.Fatalf("write terms: %v", err)
	}
	script := filepath.Join("..", "..", "scripts", "sanitize-capture.py")
	sanitize := func(maps ...string) runner.RunResult {
		t.Helper()
		argv := append(append([]string{"python3", script}, maps...), "--terms", terms, in, out)
		result, err := runner.Real{}.Run(context.Background(), argv, runner.RunOptions{})
		if err != nil {
			t.Fatalf("run the sanitizer: %v", err)
		}
		return result
	}
	refused := sanitize()
	stderr := string(refused.Stderr)
	if refused.ExitCode != 1 || !strings.Contains(stderr, filepath.Join(out, "a.jsonl")+":2") ||
		!strings.Contains(stderr, filepath.Join(out, "a.jsonl")+":3") || strings.Contains(stderr, "a.jsonl:1") ||
		!strings.Contains(stderr, filepath.Join(out, "c.txt")+":1") {
		t.Errorf("sanitizer with leftovers: exit %d, stderr %q; want 1 naming a.jsonl:2, a.jsonl:3 and c.txt:1 only", refused.ExitCode, stderr)
	}
	if strings.Contains(stderr, "SecretTerm") || strings.Contains(stderr, machine) {
		t.Errorf("the refusal printed a leftover itself: %q", stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote %s (stat err %v)", out, err)
	}
	clean := sanitize("--map="+machine+"=/tmp/demo-home", "--map="+linuxMachine+"=/tmp/demo-home", "--map=SecretTerm=neutral")
	if clean.ExitCode != 0 {
		t.Fatalf("sanitizer with every leftover mapped: exit %d, stderr %q", clean.ExitCode, clean.Stderr)
	}
	for name, want := range map[string]string{
		"a.jsonl": "clean line\n/tmp/demo-home/proj/x.go\nneutral inside\n",
		"b.txt":   "write to user@example.com\\nuser@example.com\n",
		"c.txt":   "/tmp/demo-home/proj/y.go\n",
	} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
}
