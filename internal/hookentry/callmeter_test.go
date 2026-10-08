package hookentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
	"github.com/rezzminator/callmeter/internal/callmeter/report"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// The captured payloads (testdata/callmeter/*.jsonl) name these neutral
// roots; a lab rewrites them to its own temp directory.
const (
	cmDemoHome  = "/tmp/demo-home"
	cmDemoProj  = "/tmp/demo-proj"
	cmSessionA  = "b2c7b094-91c1-4b76-8621-258b240b695a"
	cmSessionB  = "406b5ade-ce88-4f9f-8ea1-7e9aa4fd2a31"
	cmSubagent  = "a06aef038839fab57"
	cmEchoCall  = "toolu_01MMa5wYFxvUL9L8JdPvB3hV"
	cmBatchLead = "toolu_01UGaJs715PJE1hKtJWM2ANC"
)

type callmeterLab struct {
	t         *testing.T
	ctx       context.Context
	root      string
	home      string
	proj      string
	storePath string
	logPath   string
	missed    string // the wrapper's missed.log
	clock     *clock.Fake
	store     *callmeter.Store
	env       map[string]string // the hook's environment beyond the seat
	seatDir   *string           // the seat the hook runs from; nil = unresolved
}

func newCallmeterLab(t *testing.T) *callmeterLab {
	t.Helper()
	root := t.TempDir()
	lab := &callmeterLab{
		t:         t,
		root:      root,
		home:      filepath.Join(root, "demo-home"),
		proj:      filepath.Join(root, "demo-proj"),
		storePath: filepath.Join(root, "state", "callmeter.db"),
		logPath:   filepath.Join(root, "state", "callmeter.log"),
		missed:    filepath.Join(root, "state", "missed.log"),
		clock:     clock.NewFake(time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC)),
		ctx:       context.Background(),
		env:       map[string]string{},
	}
	source := filepath.Join("testdata", "callmeter", "demo-home")
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(lab.home, strings.TrimPrefix(path, source))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy transcript fixtures: %v", err)
	}
	if err := os.MkdirAll(lab.proj, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	return lab
}

// payloads reads one captured fixture, every neutral root rewritten to the lab's.
func (lab *callmeterLab) payloads(name string) []string {
	lab.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "callmeter", name))
	if err != nil {
		lab.t.Fatalf("read fixture %s: %v", name, err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		lines = append(lines, lab.rewrite(line))
	}
	return lines
}

// rewrite points a payload's neutral roots at the lab's.
func (lab *callmeterLab) rewrite(payload string) string {
	return strings.ReplaceAll(strings.ReplaceAll(payload, cmDemoHome, lab.home), cmDemoProj, lab.proj)
}

func (lab *callmeterLab) feed(payloads ...string) {
	lab.t.Helper()
	shared := filepath.Join(lab.home, ".claude")
	for _, payload := range payloads {
		var stderr bytes.Buffer
		if code := runCallmeter(
			lab.ctx,
			strings.NewReader(payload),
			&stderr,
			lab.files(),
			lab.clock,
			callmeterSeat{dir: lab.seatDir, configDir: &shared},
			mapEnv(lab.env),
		); code != 0 {
			lab.t.Fatalf("exit code = %d, want 0 on every path; stderr = %q", code, stderr.String())
		}
	}
}

// files are the lab's store, log and missed.log.
func (lab *callmeterLab) files() callmeterFiles {
	return callmeterFiles{store: lab.storePath, log: lab.logPath, missed: lab.missed}
}

func (lab *callmeterLab) transcript(session string) string {
	return filepath.Join(lab.home, ".claude", "projects", "-tmp-demo-proj", session+".jsonl")
}

// truncate keeps the first n lines of a transcript: the later requests are
// "not on disk yet". It returns the full content for restore.
func (lab *callmeterLab) truncate(path string, n int) []byte {
	lab.t.Helper()
	full, err := os.ReadFile(path)
	if err != nil {
		lab.t.Fatalf("read transcript: %v", err)
	}
	lines := strings.SplitAfter(string(full), "\n")
	if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "")), 0o644); err != nil {
		lab.t.Fatalf("truncate transcript: %v", err)
	}
	return full
}

func (lab *callmeterLab) write(path string, data []byte) {
	lab.t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		lab.t.Fatalf("write %s: %v", path, err)
	}
}

func (lab *callmeterLab) db() *callmeter.Store {
	lab.t.Helper()
	if lab.store == nil {
		store, err := callmeter.OpenDB(context.Background(), lab.storePath)
		if err != nil {
			lab.t.Fatalf("open store: %v", err)
		}
		lab.t.Cleanup(func() {
			if err := store.Close(); err != nil {
				lab.t.Errorf("close store: %v", err)
			}
		})
		lab.store = store
	}
	return lab.store
}

// row reads one row as column -> fmt.Sprint(value); NULL reads "<nil>".
func (lab *callmeterLab) row(query string, args ...any) map[string]string {
	lab.t.Helper()
	rows, err := lab.db().DB().QueryContext(context.Background(), query, args...)
	if err != nil {
		lab.t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		lab.t.Fatalf("columns of %q: %v", query, err)
	}
	if !rows.Next() {
		lab.t.Fatalf("query %q %v: no row (err %v)", query, args, rows.Err())
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		lab.t.Fatalf("scan %q: %v", query, err)
	}
	out := map[string]string{}
	for i, name := range columns {
		out[name] = fmt.Sprint(values[i])
	}
	return out
}

func (lab *callmeterLab) count(query string, args ...any) int {
	lab.t.Helper()
	n, err := func() (int, error) {
		var n int
		err := lab.db().DB().QueryRowContext(context.Background(), query, args...).Scan(&n)
		return n, err
	}()
	if err != nil {
		lab.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (lab *callmeterLab) call(id string) map[string]string {
	lab.t.Helper()
	return lab.row("SELECT * FROM calls WHERE tool_use_id = ?", id)
}

func expect(t *testing.T, what string, row map[string]string, want map[string]any) {
	t.Helper()
	for column, value := range want {
		if got := row[column]; got != fmt.Sprint(value) {
			t.Errorf("%s.%s = %q, want %q", what, column, got, fmt.Sprint(value))
		}
	}
}

// payloadField digs one value out of a raw payload line by its key path.
func payloadField(t *testing.T, line string, path ...string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(line), &value); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, key := range path {
		switch node := value.(type) {
		case map[string]any:
			value = node[key]
		case []any:
			var index int
			if _, err := fmt.Sscan(key, &index); err != nil {
				t.Fatalf("index %q: %v", key, err)
			}
			value = node[index]
		default:
			t.Fatalf("payload path %v: %q is not a container", path, key)
		}
	}
	return value
}

func TestCallmeterCapturedPayloads(t *testing.T) {
	lab := newCallmeterLab(t)
	configDir, err := filepath.EvalSymlinks(filepath.Join(lab.home, ".claude"))
	if err != nil {
		t.Fatalf("resolve config dir: %v", err)
	}
	fixture := strings.Repeat("package demo\n", 100)
	lab.write(filepath.Join(lab.proj, "fixture.go"), []byte(fixture))

	// The chat and its sub-agent. The capture predates PostToolBatch, so the
	// sub-agent's two batches are built here in the harness's shape; the
	// second lands before its request reaches the sub-agent transcript.
	scripted := lab.payloads("scripted.jsonl")
	subTranscript := callmeter.SubagentTranscriptPath(lab.transcript(cmSessionA), cmSubagent)
	subBatch := func(id, response string) string {
		return fmt.Sprintf(
			`{"session_id":%q,"transcript_path":%q,"cwd":%q,"agent_id":%q,"agent_type":"general-purpose",`+
				`"hook_event_name":"PostToolBatch","tool_calls":[{"tool_name":"Read","tool_input":{"file_path":%q},"tool_use_id":%q,"tool_response":%s}]}`,
			cmSessionA, lab.transcript(cmSessionA), lab.proj, cmSubagent, filepath.Join(lab.proj, "fixture.go"), id, response)
	}
	lab.feed(scripted[:7]...)
	lab.feed(subBatch("toolu_01V8dv8UQGCZREurDdztPdMd", `[{"type":"text","text":"abc"},{"type":"text","text":"defg"}]`))
	lab.feed(scripted[7])
	full := lab.truncate(subTranscript, 3)
	lab.feed(subBatch("toolu_01PjfJjL3jCooHMyVrkf8UxA", `"151 fixture.go"`))
	expect(t, "sub-agent pending request", lab.row(
		"SELECT * FROM requests WHERE request_id = ?", callmeter.ProvisionalKey("toolu_01PjfJjL3jCooHMyVrkf8UxA"),
	), map[string]any{"pending": 1, "agent_id": cmSubagent, "calls": 1})
	lab.write(subTranscript, full)
	lab.feed(scripted[8:]...)

	readContent, _ := payloadField(t, scripted[0], "tool_response", "file", "content").(string)
	expect(t, "whole Read", lab.call("toolu_0184pmUECyYH9FvhUrZmgYSB"), map[string]any{
		"tool": "Read", "failed": 0, "bytes_real": len(readContent), "file_path": filepath.Join(lab.proj, "fixture.go"),
		"file_bytes": len(fixture), "read_start": 1, "read_lines": 152, "read_total_lines": 152,
		"session_id": cmSessionA, "agent_id": nil, "cwd": lab.proj, "source": callmeter.SourceHook,
		"config_dir": configDir, "ts": lab.clock.Now().UnixMilli(), "duration_ms": 2,
	})
	expect(t, "ranged Read", lab.call("toolu_01BQAycMmK5m23hu7UWYnSHr"), map[string]any{
		"read_start": 20, "read_lines": 15, "read_total_lines": 152,
	})
	write := lab.call("toolu_01T4ttjYv9CKMKP9fdobnE3Y")
	expect(t, "Write", write, map[string]any{"tool": "Write", "file_bytes": nil, "file_bytes_before": 0})
	if strings.Contains(write["input"], `"content":`) || !strings.Contains(write["input"], `"content_bytes":14`) {
		t.Errorf("Write input = %s, want content replaced by content_bytes", write["input"])
	}
	expect(
		t,
		"Edit",
		lab.call("toolu_01TM3J5vmCgPRboMQG6spZnF"),
		map[string]any{"tool": "Edit", "file_bytes_before": 14},
	)
	failure := lab.call("toolu_0159VRFgcqEF4RCnEtEF1gjm")
	expect(t, "failed Bash", failure,
		map[string]any{"tool": "Bash", "failed": 1, "bytes_real": len("Exit code 1\ncat: does-not-exist.txt: No such file or directory"), "duration_ms": 11})
	if failure["error"] != "Exit code 1" {
		t.Errorf("failed Bash error = %q", failure["error"])
	}
	expect(t, "sub-agent Read", lab.call("toolu_01V8dv8UQGCZREurDdztPdMd"), map[string]any{
		"agent_id": cmSubagent, "agent_type": "general-purpose", "bytes_delivered": 7, "request_id": "msg_demo_S1",
	})
	expect(t, "sub-agent Bash", lab.call("toolu_01PjfJjL3jCooHMyVrkf8UxA"), map[string]any{
		"agent_id": cmSubagent, "bytes_delivered": 14, "request_id": "msg_demo_S2",
	})
	expect(
		t,
		"resolved sub-agent request",
		lab.row("SELECT * FROM requests WHERE request_id = 'msg_demo_S2'"),
		map[string]any{
			"pending":        0,
			"agent_id":       cmSubagent,
			"context_tokens": 6 + 5390 + 2100,
			"output_tokens":  45,
			"calls":          1,
		},
	)
	expect(t, "Agent call", lab.call("toolu_01WWJxCy1xfH6wrvmTc7oF5c"), map[string]any{
		"tool": "Agent", "bytes_real": 3, "duration_ms": 4365,
	})
	agentTranscript, _ := payloadField(t, scripted[8], "agent_transcript_path").(string)
	expect(t, "agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
		"parent_tool_use_id": "toolu_01WWJxCy1xfH6wrvmTc7oF5c",
		"agent_type":         "general-purpose",
		"session_id":         cmSessionA,
		"total_tokens":       18107,
		"tool_uses":          2,
		"model":              "claude-haiku-4-5-20251001",
		"started":            lab.clock.Now().UnixMilli(),
		"stopped":            lab.clock.Now().UnixMilli(),
		"transcript_path":    agentTranscript,
		"config_dir":         configDir,
	})

	// Three parallel calls in one batch, then one call whose request is not
	// on disk yet: pending until the Stop.
	probe5 := lab.payloads("probe5.jsonl")
	fullB := lab.truncate(lab.transcript(cmSessionB), 7)
	lab.feed(probe5[:6]...)
	expect(t, "echo call before Stop", lab.call(cmEchoCall), map[string]any{
		"request_id": callmeter.ProvisionalKey(cmEchoCall), "bytes_delivered": 3, "bytes_real": 3,
	})
	expect(
		t,
		"pending request",
		lab.row("SELECT * FROM requests WHERE request_id = ?", callmeter.ProvisionalKey(cmEchoCall)),
		map[string]any{"pending": 1, "calls": 1, "agent_id": nil, "session_id": cmSessionB},
	)
	lab.write(lab.transcript(cmSessionB), fullB)
	lab.feed(probe5[6])
	expect(t, "three-call request", lab.row("SELECT * FROM requests WHERE request_id = 'msg_demo_B1'"),
		map[string]any{"pending": 0, "calls": 3, "context_tokens": 4 + 11200 + 1850, "output_tokens": 210})
	for id, delivered := range map[string]int{
		cmBatchLead: 17, "toolu_014w33S7y4Hmv2iQj3NzzEWV": 19, "toolu_01LV57SCFxiU1LaMWZm3ixg6": 17,
	} {
		expect(
			t,
			"batched call "+id,
			lab.call(id),
			map[string]any{"request_id": "msg_demo_B1", "bytes_delivered": delivered},
		)
	}
	expect(t, "echo call after Stop", lab.call(cmEchoCall), map[string]any{"request_id": "msg_demo_B2"})
	expect(t, "Stop-resolved request", lab.row("SELECT * FROM requests WHERE request_id = 'msg_demo_B2'"),
		map[string]any{"pending": 0, "calls": 1, "context_tokens": 2 + 13050 + 120})

	// seq 1 7000: real 33,893 bytes, persisted, delivered as the preview.
	probe6 := lab.payloads("probe6.jsonl")
	lab.feed(probe6...)
	preview, _ := payloadField(t, probe6[2], "tool_calls", "0", "tool_response").(string)
	persisted, _ := payloadField(t, probe6[0], "tool_response", "persistedOutputPath").(string)
	if !strings.HasPrefix(preview, "<persisted-output>") {
		t.Fatalf("fixture drifted: seq batch response = %.40q", preview)
	}
	expect(t, "persisted Bash", lab.call("toolu_01CpbGnnWxfXpSqFSVgNHY34"), map[string]any{
		"bytes_real": 33893, "bytes_delivered": len(preview), "persisted_path": persisted, "request_id": "msg_demo_C1",
	})
	expect(
		t,
		"Read limit 5",
		lab.call("toolu_01WDSsG632Trh15WvoNiAPZd"),
		map[string]any{"read_lines": 5, "request_id": "msg_demo_C1"},
	)

	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0 for the captured run: %s", n, lab.faultsText())
	}
	if n := lab.count("SELECT count(*) FROM requests WHERE pending = 1"); n != 0 {
		t.Errorf("pending requests = %d, want 0 after every Stop", n)
	}
}

func TestCallmeterBatchBeforePostToolUse(t *testing.T) {
	rows := make([]map[string]string, 0, 2)
	for _, batchFirst := range []bool{false, true} {
		lab := newCallmeterLab(t)
		probe5 := lab.payloads("probe5.jsonl")
		use, batch := probe5[4], probe5[5]
		if batchFirst {
			use, batch = batch, use
		}
		lab.feed(use, batch)
		row := lab.call(cmEchoCall)
		for column, value := range row {
			row[column] = strings.ReplaceAll(value, lab.root, "{lab}") // each lab has its own temp root
		}
		rows = append(rows, row)
	}
	expect(t, "echo call", rows[0], map[string]any{"bytes_real": 3, "bytes_delivered": 3, "request_id": "msg_demo_B2"})
	for column, value := range rows[0] {
		if rows[1][column] != value {
			t.Errorf("column %s: PostToolUse first = %q, PostToolBatch first = %q", column, value, rows[1][column])
		}
	}
}

// TestCallmeterHookRowsReachTheReports: the reports read only what the hook
// stored. probe5 fed whole records its Bash calls; EnsureParsed parses every
// one of them, and fixture.go's BASH BYTES is the delivered bytes of the two
// calls that read it (each credits that one file, so its share is all of it).
func TestCallmeterHookRowsReachTheReports(t *testing.T) {
	lab := newCallmeterLab(t)
	fixture := filepath.Join(lab.proj, "fixture.go")
	lab.write(fixture, []byte(strings.Repeat("package demo\n", 100)))
	lab.feed(lab.payloads("probe5.jsonl")...)
	store := lab.db()
	bash := lab.count("SELECT COUNT(*) FROM calls WHERE tool = 'Bash'")
	if bash != 3 {
		t.Fatalf("the hook stored %d Bash calls, want probe5's 3", bash)
	}
	summary, err := report.EnsureParsed(lab.ctx, store, lab.home, nil)
	if err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	if summary.Parsed != bash || summary.SkippedRelative != 0 || summary.SkippedNoInput != 0 {
		t.Fatalf("EnsureParsed = %+v, want all %d hook-recorded Bash calls parsed", summary, bash)
	}
	if unparsed := lab.count(`SELECT COUNT(*) FROM calls c WHERE c.tool = 'Bash' AND NOT EXISTS
		(SELECT 1 FROM command_parts p WHERE p.tool_use_id = c.tool_use_id AND p.parser = ?)`, cmdparse.Version); unparsed != 0 {
		t.Fatalf("%d Bash calls have no parts from this parser", unparsed)
	}
	var want int64
	for _, id := range []string{"toolu_014w33S7y4Hmv2iQj3NzzEWV", "toolu_01LV57SCFxiU1LaMWZm3ixg6"} {
		stored := lab.call(id)["bytes_delivered"]
		delivered, err := strconv.ParseInt(stored, 10, 64)
		if err != nil || delivered == 0 {
			t.Fatalf("call %s bytes_delivered = %q (%v), want the hook's count", id, stored, err)
		}
		want += delivered
	}
	table, err := report.Files(lab.ctx, store, report.Filter{}, nil)
	if err != nil {
		t.Fatalf("report.Files: %v", err)
	}
	column := map[string]int{}
	for i, name := range table.Header {
		column[name] = i
	}
	for _, row := range table.Rows {
		if row[column["FILE"]] != fixture {
			continue
		}
		if got := row[column["BASH BYTES"]]; got != strconv.FormatInt(want, 10) {
			t.Fatalf("fixture.go BASH BYTES = %s, want %d: the hook's bytes of the two calls that read it", got, want)
		}
		return
	}
	t.Fatalf("report.Files has no row for %s: %v", fixture, table.Rows)
}

func TestCallmeterGarbagePayload(t *testing.T) {
	for name, payload := range map[string]string{
		"unparsable":    `{"hook_event_name": "PostToolUse", "tool_use_id": `,
		"unknown event": `{"hook_event_name": "MessageDisplay", "session_id": "s1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			lab := newCallmeterLab(t)
			lab.feed(payload)
			if n := lab.count("SELECT count(*) FROM faults WHERE stage = ?", callmeter.StagePayload); n != 1 {
				t.Errorf("payload faults = %d, want 1", n)
			}
			if n := lab.count("SELECT count(*) FROM faults"); n != 1 {
				t.Errorf("faults = %d, want exactly the one payload fault", n)
			}
			if n := lab.count("SELECT count(*) FROM calls"); n != 0 {
				t.Errorf("calls = %d, want 0", n)
			}
			assertLogged(t, lab.logPath, callmeter.StagePayload)
		})
	}
}

func TestCallmeterUnreadableTranscript(t *testing.T) {
	lab := newCallmeterLab(t)
	// Malformed and holding the batch lead: the line the lookup must read. A
	// malformed line holding no wanted id is never decoded (callmeter.FindRequests).
	lab.write(lab.transcript(cmSessionB), []byte("this line is not JSON, yet names "+cmBatchLead+"\n"))
	probe5 := lab.payloads("probe5.jsonl")
	lab.feed(probe5[3])
	key := callmeter.ProvisionalKey(cmBatchLead)
	expect(t, "provisional request", lab.row("SELECT * FROM requests WHERE request_id = ?", key),
		map[string]any{"pending": 1, "calls": 3, "session_id": cmSessionB})
	expect(
		t,
		"batched call",
		lab.call("toolu_01LV57SCFxiU1LaMWZm3ixg6"),
		map[string]any{"request_id": key, "bytes_delivered": 17},
	)
	if n := lab.count(
		"SELECT count(*) FROM faults WHERE stage = ? AND tool_use_id = ?",
		callmeter.StageTranscript,
		cmBatchLead,
	); n != 1 {
		t.Errorf("transcript faults = %d, want 1", n)
	}
	assertLogged(t, lab.logPath, callmeter.StageTranscript)
}

func TestCallmeterStoreUnopenable(t *testing.T) {
	lab := newCallmeterLab(t)
	blocker := filepath.Join(lab.root, "blocker")
	lab.write(blocker, []byte("a file where the store's directory should be"))
	lab.storePath = filepath.Join(blocker, "callmeter.db")
	var stderr bytes.Buffer
	if code := runCallmeter(
		lab.ctx,
		strings.NewReader(lab.payloads("probe5.jsonl")[0]),
		&stderr,
		lab.files(),
		lab.clock,
		callmeterSeat{},
		mapEnv(nil),
	); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "toolu_01UGaJs715PJE1hKtJWM2ANC") {
		t.Errorf("stderr = %q, want the failure naming the call", stderr.String())
	}
	assertLogged(t, lab.logPath, callmeter.StageStore)
}

// TestCallmeterEntryUsesHome: the store lands under CALLMETER_HOME, never
// under the user's HOME the environment also carries — a hook that ignored
// CALLMETER_HOME wrote a test run's rows into the operator's real store.
func TestCallmeterEntryUsesHome(t *testing.T) {
	lab := newCallmeterLab(t)
	var stderr bytes.Buffer
	payload := lab.payloads("probe5.jsonl")[1]
	userHome := t.TempDir()
	jailed := mapEnv(map[string]string{paths.EnvHome: lab.root, "HOME": userHome})
	if code := Callmeter(strings.NewReader(payload), &stderr, jailed); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(userHome, ".local", "state", "callmeter", "callmeter.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store written under the user's HOME %s despite CALLMETER_HOME (stat err %v)", userHome, err)
	}
	lab.storePath = paths.Store(lab.root)
	expect(t, "Bash call", lab.call("toolu_014w33S7y4Hmv2iQj3NzzEWV"), map[string]any{
		"tool": "Bash", "bytes_real": 19,
		"seat_dir": filepath.Join(userHome, ".claude"), "config_dir": filepath.Join(userHome, ".claude"),
	})
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing from a clean record", stderr.String())
	}
}

// TestCallmeterEntryHomeUnresolvableIsSaidOnStderr: with no home to resolve
// the hook still exits 0 and says why on stderr; there is no log file to write.
func TestCallmeterEntryHomeUnresolvableIsSaidOnStderr(t *testing.T) {
	lab := newCallmeterLab(t)
	var stderr bytes.Buffer
	if code := Callmeter(strings.NewReader(lab.payloads("probe5.jsonl")[1]), &stderr, mapEnv(nil)); code != 0 {
		t.Fatalf("exit code = %d, want 0 with no home", code)
	}
	if !strings.Contains(stderr.String(), "resolve callmeter home") || !strings.Contains(stderr.String(), paths.EnvHome) {
		t.Errorf("stderr = %q, want the home error said", stderr.String())
	}
}

// TestCallmeterEntryStoreUnopenableIsSaid: CALLMETER_HOME is a regular file,
// so neither the store, the log nor missed.log can be created: exit 0, a
// stderr line for the store error and one for the failed missed.log append,
// each followed by its failed log append, no panic.
func TestCallmeterEntryStoreUnopenableIsSaid(t *testing.T) {
	lab := newCallmeterLab(t)
	blocker := filepath.Join(lab.root, "a-file")
	lab.write(blocker, []byte("a file where the home should be"))
	var stderr bytes.Buffer
	env := mapEnv(map[string]string{paths.EnvHome: blocker, "HOME": lab.home})
	if code := Callmeter(strings.NewReader(lab.payloads("probe5.jsonl")[0]), &stderr, env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "callmeter: store: ") ||
		!strings.Contains(lines[0], "toolu_01UGaJs715PJE1hKtJWM2ANC") ||
		!strings.HasPrefix(lines[1], "callmeter: log "+paths.Log(blocker)) ||
		!strings.HasPrefix(lines[2], "callmeter: "+callmeter.StageTerminated+": ") ||
		!strings.Contains(lines[2], paths.Missed(blocker)) ||
		!strings.HasPrefix(lines[3], "callmeter: log "+paths.Log(blocker)) {
		t.Errorf("stderr = %q, want the store error and the failed missed.log append, each then the failed log append", stderr.String())
	}
}

// TestCallmeterEntryPayloadWithoutSessionIsLogged: a payload with no
// session_id is a faults row (stage payload), a stderr line and a JSON line in
// callmeter.log.
func TestCallmeterEntryPayloadWithoutSessionIsLogged(t *testing.T) {
	lab := newCallmeterLab(t)
	var stderr bytes.Buffer
	env := mapEnv(map[string]string{paths.EnvHome: lab.root, "HOME": lab.home})
	payload := `{"hook_event_name":"PostToolUse","tool_use_id":"toolu_orphan","tool_name":"Bash"}`
	if code := Callmeter(strings.NewReader(payload), &stderr, env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lab.storePath = paths.Store(lab.root)
	expect(t, "payload fault", lab.row("SELECT * FROM faults WHERE stage = ?", callmeter.StagePayload),
		map[string]any{"tool_use_id": "toolu_orphan"})
	if !strings.Contains(stderr.String(), "carries no session_id") {
		t.Errorf("stderr = %q, want the missing session_id said", stderr.String())
	}
	assertLogged(t, paths.Log(lab.root), callmeter.StagePayload)
	if n := lab.count("SELECT count(*) FROM calls"); n != 0 {
		t.Errorf("calls = %d, want 0", n)
	}
}

// TestCallmeterEntryGarbageStdinIsAPayloadFault: bytes that are not JSON are
// exit 0 and a payload fault.
func TestCallmeterEntryGarbageStdinIsAPayloadFault(t *testing.T) {
	lab := newCallmeterLab(t)
	var stderr bytes.Buffer
	env := mapEnv(map[string]string{paths.EnvHome: lab.root, "HOME": lab.home})
	if code := Callmeter(strings.NewReader("\x00\xffnot json at all"), &stderr, env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lab.storePath = paths.Store(lab.root)
	if n := lab.count("SELECT count(*) FROM faults WHERE stage = ?", callmeter.StagePayload); n != 1 {
		t.Errorf("payload faults = %d, want 1", n)
	}
	assertLogged(t, paths.Log(lab.root), callmeter.StagePayload)
}

// TestCallmeterEntrySeatUnresolvableStillRecords: with no HOME the seat cannot
// be resolved; the call is recorded without its seat and the gap is logged.
func TestCallmeterEntrySeatUnresolvableStillRecords(t *testing.T) {
	lab := newCallmeterLab(t)
	var stderr bytes.Buffer
	env := mapEnv(map[string]string{paths.EnvHome: lab.root})
	if code := Callmeter(strings.NewReader(lab.payloads("probe5.jsonl")[1]), &stderr, env); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	lab.storePath = paths.Store(lab.root)
	expect(t, "Bash call", lab.call("toolu_014w33S7y4Hmv2iQj3NzzEWV"),
		map[string]any{"tool": "Bash", "seat_dir": nil, "config_dir": nil})
	if !strings.Contains(stderr.String(), "resolve seat dir") {
		t.Errorf("stderr = %q, want the unresolved seat said", stderr.String())
	}
	assertLogged(t, paths.Log(lab.root), "seat")
}

// assertLogged is one ERROR JSON line with step stage and a session field in
// the log at logPath.
func assertLogged(t *testing.T, logPath, stage string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %s: %v", logPath, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record["step"] == stage && record["level"] == "error" {
			if _, ok := record["session"]; !ok {
				t.Errorf("log record %v carries no session", record)
			}
			return
		}
	}
	t.Errorf("no error log record with step=%s in %s:\n%s", stage, logPath, data)
}

// Two async hooks of one tool call (its PostToolUse and the PostToolBatch)
// fire together: the store open of one must wait out the other's write, never
// lose its whole record. A store open that wrote the schema version in a
// deferred transaction failed at once with SQLITE_BUSY, no busy wait.
func TestCallmeterStoreOpenWaitsOutAConcurrentWriter(t *testing.T) {
	lab := newCallmeterLab(t)
	probe5 := lab.payloads("probe5.jsonl")
	lab.feed(probe5[0]) // the first hook of the session created the store
	conn, err := lab.db().DB().Conn(context.Background())
	if err != nil {
		t.Fatalf("take a writer connection: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold the write lock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
			t.Errorf("release the write lock: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close the writer connection: %v", err)
		}
	}()
	lab.feed(probe5[3])
	<-released
	expect(t, "batched call", lab.call("toolu_01LV57SCFxiU1LaMWZm3ixg6"), map[string]any{
		"request_id": "msg_demo_B1", "bytes_delivered": 17,
	})
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0", n)
	}
}

// The first hooks of a session race to create the store: the one that loses
// the switch to WAL must wait and retry, never lose its record.
func TestCallmeterFirstStoreOpenWaitsOutARacingCreator(t *testing.T) {
	lab := newCallmeterLab(t)
	if err := os.MkdirAll(filepath.Dir(lab.storePath), 0o700); err != nil {
		t.Fatalf("create store dir: %v", err)
	}
	creator, err := sqlitedb.OpenReadWrite(lab.storePath, time.Second)
	if err != nil {
		t.Fatalf("open the racing creator: %v", err)
	}
	t.Cleanup(func() {
		if err := creator.Close(); err != nil {
			t.Errorf("close the racing creator: %v", err)
		}
	})
	ctx := context.Background()
	if _, err := creator.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold the brand-new store's write lock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(300 * time.Millisecond)
		if _, err := creator.ExecContext(ctx, "COMMIT"); err != nil {
			t.Errorf("release the write lock: %v", err)
		}
	}()
	lab.feed(lab.payloads("probe5.jsonl")[3])
	<-released
	expect(t, "batched call", lab.call("toolu_01LV57SCFxiU1LaMWZm3ixg6"), map[string]any{
		"request_id": "msg_demo_B1", "bytes_delivered": 17,
	})
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0", n)
	}
}

// asyncAgentResult is the Agent call of scripted.jsonl as a background launch
// returns it: no totals, only the agent id and its resolved model.
func asyncAgentResult(t *testing.T, payload string) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		t.Fatalf("decode Agent payload: %v", err)
	}
	fields["tool_response"] = map[string]any{
		"status":        "async_launched",
		"isAsync":       true,
		"agentId":       cmSubagent,
		"resolvedModel": "claude-haiku-4-5-20251001",
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode Agent payload: %v", err)
	}
	return string(out)
}

func TestCallmeterSubagentStopFillsTotals(t *testing.T) {
	agentRow := func(lab *callmeterLab) map[string]string {
		return lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent)
	}
	t.Run("background agent", func(t *testing.T) {
		lab := newCallmeterLab(t)
		scripted := lab.payloads("scripted.jsonl")
		lab.feed(scripted[5], asyncAgentResult(t, scripted[9]), scripted[8])
		// The final message msg_demo_S3 is split across two entries, each
		// repeating its usage: it counts once, at its last entry's count.
		expect(t, "background agent", agentRow(lab), map[string]any{
			"total_tokens": (8 + 0 + 5390 + 60) + (6 + 5390 + 2100 + 45) + (2 + 4990 + 96 + 20),
			"tool_uses":    2,
			"model":        "claude-haiku-4-5-20251001", // the Agent result's, never the transcript's
		})
		if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
			t.Errorf("faults = %d, want 0", n)
		}
	})
	t.Run("agent result first", func(t *testing.T) {
		lab := newCallmeterLab(t)
		scripted := lab.payloads("scripted.jsonl")
		lab.feed(scripted[5], scripted[9], scripted[8])
		expect(t, "foreground agent", agentRow(lab), map[string]any{
			"total_tokens": 18107, "tool_uses": 2, "model": "claude-haiku-4-5-20251001",
		})
	})
	t.Run("unreadable agent transcript", func(t *testing.T) {
		lab := newCallmeterLab(t)
		scripted := lab.payloads("scripted.jsonl")
		agentTranscript, _ := payloadField(t, scripted[8], "agent_transcript_path").(string)
		lab.write(agentTranscript, []byte("this line is not JSON\n"))
		lab.feed(scripted[5], scripted[8])
		expect(t, "agent", agentRow(lab), map[string]any{
			"total_tokens": nil, "stopped": lab.clock.Now().UnixMilli(),
		})
		if n := lab.count("SELECT count(*) FROM faults WHERE stage = ?", callmeter.StageTranscript); n != 1 {
			t.Errorf("transcript faults = %d, want 1", n)
		}
		assertLogged(t, lab.logPath, callmeter.StageTranscript)
	})
}

// TestCallmeterSubagentStopWaitsForTheFinalMessage: Claude Code fires
// SubagentStop 20-50 ms after the agent's final message is stamped but before
// that line is flushed to its transcript (live: 31 of 33 agents summed without
// their final message). The hook re-reads until the last assistant entry
// carries a final stop_reason, so the totals hold that message.
func TestCallmeterSubagentStopWaitsForTheFinalMessage(t *testing.T) {
	lab := newCallmeterLab(t)
	scripted := lab.payloads("scripted.jsonl")
	agentTranscript, _ := payloadField(t, scripted[8], "agent_transcript_path").(string)
	full, err := os.ReadFile(agentTranscript)
	if err != nil {
		t.Fatalf("read agent transcript: %v", err)
	}
	cut := bytes.Index(full, []byte(`"uuid":"s-a3"`))
	cut = bytes.LastIndexByte(full[:cut], '\n') + 1
	lab.write(agentTranscript, full[:cut])
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(agentTranscript, full, 0o644); err != nil {
			t.Errorf("flush the final message: %v", err)
		}
	}()
	lab.feed(scripted[5], asyncAgentResult(t, scripted[9]), scripted[8])
	<-flushed
	expect(t, "agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
		"total_tokens": 18107, "tool_uses": 2,
	})
}

// secondAgentTurn is a second turn of the scripted agent, appended to its
// transcript: one more tool use and 7573 + 7610 tokens.
const secondAgentTurn = `{"type":"user","uuid":"s-u2","timestamp":"2026-09-23T01:40:00.000Z","message":{"role":"user","content":"count again"}}` + "\n" +
	`{"type":"assistant","uuid":"s-a5","timestamp":"2026-09-23T01:40:01.000Z","message":{"id":"msg_demo_S4","model":"claude-sonnet-4-5",` +
	`"content":[{"type":"tool_use","id":"toolu_demo_again","name":"Bash","input":{"command":"wc -l fixture.go"}}],"stop_reason":"tool_use",` +
	`"usage":{"input_tokens":3,"cache_read_input_tokens":7500,"cache_creation_input_tokens":40,"output_tokens":30}}}` + "\n" +
	`{"type":"assistant","uuid":"s-a6","timestamp":"2026-09-23T01:40:02.000Z","message":{"id":"msg_demo_S5","model":"claude-sonnet-4-5",` +
	`"content":[{"type":"text","text":"151"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":2,"cache_read_input_tokens":7540,"cache_creation_input_tokens":60,"output_tokens":8}}}` + "\n"

// TestCallmeterResumedAgentKeepsItsFirstStartAndLatestTotals: an agent that
// ends a turn and is woken again (an orchestrator waiting on its children, a
// SendMessage) fires SubagentStart and SubagentStop once per turn (live: an
// orchestrator with 86 tool uses stored as 10, started after it stopped).
// started stays the first start, stopped and the totals follow the last stop.
func TestCallmeterResumedAgentKeepsItsFirstStartAndLatestTotals(t *testing.T) {
	lab := newCallmeterLab(t)
	scripted := lab.payloads("scripted.jsonl")
	agentTranscript, _ := payloadField(t, scripted[8], "agent_transcript_path").(string)
	first := lab.clock.Now().UnixMilli()
	lab.feed(scripted[5], asyncAgentResult(t, scripted[9]), scripted[8])
	full, err := os.ReadFile(agentTranscript)
	if err != nil {
		t.Fatalf("read agent transcript: %v", err)
	}
	lab.write(agentTranscript, append(full, secondAgentTurn...))
	lab.clock.Advance(10 * time.Minute)
	lab.feed(scripted[5], scripted[8])
	expect(t, "resumed agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
		"started":      first,
		"stopped":      lab.clock.Now().UnixMilli(),
		"total_tokens": 18107 + (3 + 7500 + 40 + 30) + (2 + 7540 + 60 + 8),
		"tool_uses":    3,
	})
}

// wakePrompt is the user entry Claude Code writes when it wakes an agent
// whose turn ended (a task notification), carrying the main chat's prompt id,
// which its SubagentStop carries too (live: both the same id).
func wakePrompt(promptID string) string {
	return `{"type":"user","uuid":"s-u3","isMeta":true,"promptId":"` + promptID + `","timestamp":"2026-09-23T01:50:00.000Z",` +
		`"message":{"role":"user","content":"invented wake-up text"}}` + "\n"
}

// wakeReply is the woken agent's whole turn: one text reply, no tool call.
const wakeReply = `{"type":"assistant","uuid":"s-a7","timestamp":"2026-09-23T01:50:05.000Z","message":{"id":"msg_demo_S7","model":"claude-sonnet-4-5",` +
	`"content":[{"type":"text","text":"invented reply text"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":2,"cache_read_input_tokens":7600,"cache_creation_input_tokens":20,"output_tokens":60}}}` + "\n"

// TestCallmeterSubagentStopWaitsForAWokenTurnsTextReply: an agent woken after
// its turn ended (live: an orchestrator woken by a task notification) answered
// with one text reply, and its SubagentStop fired 43 ms after that reply was
// stamped, before its line was flushed. The previous turn's end_turn was then
// the last assistant entry on disk, read as the turn's tail, so the hook did
// not wait and the reply was never recorded. A prompt after the last assistant
// entry means the turn's answer is still to come: the hook waits for it, and
// stops as soon as it is on disk.
func TestCallmeterSubagentStopWaitsForAWokenTurnsTextReply(t *testing.T) {
	lab := newCallmeterLab(t)
	scripted := lab.payloads("scripted.jsonl")
	agentTranscript, _ := payloadField(t, scripted[8], "agent_transcript_path").(string)
	lab.feed(scripted[5], asyncAgentResult(t, scripted[9]), scripted[8])
	full, err := os.ReadFile(agentTranscript)
	if err != nil {
		t.Fatalf("read agent transcript: %v", err)
	}
	promptID, _ := payloadField(t, scripted[8], "prompt_id").(string)
	if promptID == "" {
		t.Fatal("the captured SubagentStop carries no prompt_id")
	}
	woken := append(append([]byte{}, full...), wakePrompt(promptID)...)
	lab.write(agentTranscript, woken)
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(agentTranscript, append(woken, wakeReply...), 0o644); err != nil {
			t.Errorf("flush the woken turn's reply: %v", err)
		}
	}()
	lab.clock.Advance(20 * time.Minute)
	began := time.Now()
	lab.feed(scripted[8])
	took := time.Since(began)
	<-flushed
	expect(t, "woken turn's reply", lab.row("SELECT * FROM requests WHERE request_id = 'msg_demo_S7'"), map[string]any{
		"agent_id": cmSubagent, "output_tokens": 60, "stop_reason": "end_turn", "calls": 0, "pending": 0,
	})
	expect(t, "woken agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
		"total_tokens": 18107 + (2 + 7600 + 20 + 60), "tool_uses": 2,
	})
	if took >= agentSettle {
		t.Errorf("SubagentStop took %v, want under agentSettle %v: it stops once the reply is on disk", took, agentSettle)
	}
}

// TestCallmeterStopWaitsForASecondTurnsTextReply: the main chat's Stop, like
// SubagentStop, fires before the turn's last line is flushed. A second turn
// answered by one text reply had the first turn's end_turn as the last
// assistant entry on disk, read as the tail, so the sweep did not wait and the
// reply was never recorded. The prompt after it means the answer is to come.
func TestCallmeterStopWaitsForASecondTurnsTextReply(t *testing.T) {
	lab := newCallmeterLab(t)
	main := lab.transcript(cmSessionA)
	const before = 1790125140000 // 2026-09-23T00:59:00Z, before the transcript's entries
	first := `{"type":"user","promptId":"prompt-turn-1","message":{"role":"user","content":"invented"}}` + "\n" +
		sweepEntry("msg_turnone0001", "01", `"end_turn"`, 5, `{"type":"text","text":"invented"}`)
	lab.write(main, []byte(first))
	lab.feedAt(before, hookPayload(t, eventStop, map[string]any{"transcript_path": main, "prompt_id": "prompt-turn-1"}))
	turns := first + `{"type":"user","promptId":"prompt-turn-2","message":{"role":"user","content":"invented"}}` + "\n"
	lab.write(main, []byte(turns))
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		time.Sleep(150 * time.Millisecond)
		if err := os.WriteFile(main, []byte(turns+
			sweepEntry("msg_turntwo0001", "05", `"end_turn"`, 44, `{"type":"text","text":"invented"}`)), 0o644); err != nil {
			t.Errorf("flush the second turn's reply: %v", err)
		}
	}()
	began := time.Now()
	lab.feedAt(before+60000, hookPayload(t, eventStop, map[string]any{"transcript_path": main, "prompt_id": "prompt-turn-2"}))
	took := time.Since(began)
	<-flushed
	expect(t, "second turn's reply", lab.row("SELECT * FROM requests WHERE request_id = 'msg_turntwo0001'"), map[string]any{
		"output_tokens": 44, "stop_reason": "end_turn", "prompt_id": "prompt-turn-2", "agent_id": "<nil>", "pending": 0,
	})
	if took >= agentSettle {
		t.Errorf("Stop took %v, want under agentSettle %v: it stops once the reply is on disk", took, agentSettle)
	}
}

// A PostToolBatch carries no top-level tool_use_id: a store it cannot open
// still names every call whose record is lost.
func TestCallmeterStoreUnopenableNamesBatchCalls(t *testing.T) {
	lab := newCallmeterLab(t)
	blocker := filepath.Join(lab.root, "blocker")
	lab.write(blocker, []byte("a file where the store's directory should be"))
	var stderr bytes.Buffer
	batch := lab.payloads("probe5.jsonl")[3]
	storePath := filepath.Join(blocker, "callmeter.db")
	if code := runCallmeter(
		lab.ctx,
		strings.NewReader(batch),
		&stderr,
		callmeterFiles{store: storePath, log: lab.logPath},
		lab.clock,
		callmeterSeat{},
		mapEnv(nil),
	); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, id := range []string{cmBatchLead, "toolu_014w33S7y4Hmv2iQj3NzzEWV", "toolu_01LV57SCFxiU1LaMWZm3ixg6"} {
		if !strings.Contains(stderr.String(), id) {
			t.Errorf("stderr = %q, want the lost call %s named", stderr.String(), id)
		}
	}
	assertLogged(t, lab.logPath, callmeter.StageStore)
}

// TestCallmeterBashCwdIsTheDirectoryBeforeTheCommand: PostToolUse's cwd
// follows the command's own `cd` (captured: PreToolUse /x, PostToolUse /x/sub
// for `cd /x/sub && …`), and the parser replays that `cd` from the stored
// cwd. The pre-command directory comes from PreToolUse; either hook may land
// first, and a call whose PreToolUse was never recorded keeps PostToolUse's.
func TestCallmeterBashCwdIsTheDirectoryBeforeTheCommand(t *testing.T) {
	payload := func(event, id, cwd string) string {
		m := map[string]any{
			"session_id": cmSessionA, "cwd": cwd, "hook_event_name": event,
			"transcript_path": cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl",
			"tool_name":       "Bash", "tool_use_id": id,
			"tool_input": map[string]any{"command": "cd sub && cat a.txt", "description": "read a"},
		}
		if event == "PostToolUse" {
			m["tool_response"] = map[string]any{"stdout": "line", "stderr": "", "interrupted": false}
			m["duration_ms"] = 5
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode %s payload: %v", event, err)
		}
		return string(encoded)
	}
	cases := []struct {
		name  string
		order []string
		want  string
	}{
		{"PreToolUse lands first", []string{"PreToolUse", "PostToolUse"}, cmDemoProj},
		{"PostToolUse lands first", []string{"PostToolUse", "PreToolUse"}, cmDemoProj},
		{"no PreToolUse recorded", []string{"PostToolUse"}, cmDemoProj + "/sub"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lab := newCallmeterLab(t)
			id := fmt.Sprintf("toolu_cwd%d", i)
			for _, event := range tc.order {
				cwd := cmDemoProj
				if event == "PostToolUse" {
					cwd += "/sub"
				}
				lab.feed(payload(event, id, cwd))
			}
			expect(t, "call", lab.call(id), map[string]any{"cwd": tc.want, "tool": "Bash", "duration_ms": 5})
		})
	}
}

// TestCallmeterUntypedAgentWithoutTranscriptIsNotRecorded: live, Claude Code
// fires SubagentStop for its own internal agents with no agent_type, no
// SubagentStart and no transcript on disk (16 in the first minutes of a busy
// chat). Such a stop is no sub-agent to meter: no row, no fault. A typed agent
// whose transcript is missing stays a fault.
func TestCallmeterUntypedAgentWithoutTranscriptIsNotRecorded(t *testing.T) {
	stop := func(agentID, agentType string) string {
		m := map[string]any{
			"session_id": cmSessionA, "hook_event_name": "SubagentStop", "agent_id": agentID,
			"transcript_path":       cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl",
			"agent_transcript_path": "/nonexistent/subagents/agent-" + agentID + ".jsonl",
		}
		if agentType != "" {
			m["agent_type"] = agentType
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode SubagentStop: %v", err)
		}
		return string(encoded)
	}
	lab := newCallmeterLab(t)
	lab.feed(stop("ainternal0000001", ""), stop("atyped0000000001", "general-purpose"))
	if n := lab.count("SELECT count(*) FROM agents WHERE agent_id = 'ainternal0000001'"); n != 0 {
		t.Errorf("untyped transcript-less agent rows = %d, want 0", n)
	}
	if n := lab.count("SELECT count(*) FROM faults WHERE error LIKE '%ainternal0000001%'"); n != 0 {
		t.Errorf("faults naming the untyped agent = %d, want 0", n)
	}
	if n := lab.count("SELECT count(*) FROM faults WHERE stage = ? AND error LIKE '%atyped0000000001%'",
		callmeter.StageTranscript); n != 1 {
		t.Errorf("transcript faults naming the typed agent = %d, want 1", n)
	}
}

// TestCallmeterFailedBashCountsItsOutput: a failed Bash call's output reaches
// the model as the PostToolUseFailure `error` text (live: "Exit code 1\n…",
// thousands of bytes), so that text is the call's real size; a failing suite
// is exactly the costly output the commands report exists to find.
func TestCallmeterFailedBashCountsItsOutput(t *testing.T) {
	lab := newCallmeterLab(t)
	output := "Exit code 1\n" + strings.Repeat("FAIL some_test\n", 40)
	encoded, err := json.Marshal(map[string]any{
		"session_id": cmSessionA, "hook_event_name": "PostToolUseFailure", "cwd": cmDemoProj,
		"transcript_path": cmDemoHome + "/.claude/projects/-tmp-demo-proj/" + cmSessionA + ".jsonl",
		"tool_name":       "Bash", "tool_use_id": "toolu_failed01",
		"tool_input": map[string]any{"command": "go test ./..."}, "error": output,
	})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	lab.feed(string(encoded))
	expect(t, "failed call", lab.call("toolu_failed01"), map[string]any{"failed": 1, "bytes_real": len(output)})
}

// TestCallmeterBatchResolvesEarlierPending: a batch whose request is not on
// disk yet goes pending, and used to wait for the chat's Stop — for a
// sub-agent running an hour, its context sizes stayed unknown the whole hour
// (live: 11 pending, every id already on disk). The next batch of the same
// chat resolves it.
func TestCallmeterBatchResolvesEarlierPending(t *testing.T) {
	lab := newCallmeterLab(t)
	transcript := lab.transcript(cmSessionA)
	batch := func(id string) string {
		encoded, err := json.Marshal(map[string]any{
			"session_id": cmSessionA, "hook_event_name": "PostToolBatch", "transcript_path": transcript,
			"tool_calls": []map[string]any{{"tool_use_id": id, "tool_response": "ok"}},
		})
		if err != nil {
			t.Fatalf("encode batch: %v", err)
		}
		return string(encoded)
	}
	entry := func(msg, id string) string {
		return `{"type":"assistant","timestamp":"2026-09-23T01:00:00.000Z","message":{"id":"` + msg +
			`","content":[{"type":"tool_use","id":"` + id + `"}],"usage":{"input_tokens":5,` +
			`"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}` + "\n"
	}
	lab.write(transcript, []byte(""))
	lab.feed(batch("toolu_early"))
	lab.write(transcript, []byte(entry("msg_early", "toolu_early")+entry("msg_late", "toolu_late")))
	lab.feed(batch("toolu_late"))
	if n := lab.count("SELECT count(*) FROM requests WHERE pending = 1"); n != 0 {
		t.Errorf("pending requests after the next batch = %d, want 0", n)
	}
	expect(t, "early call", lab.call("toolu_early"), map[string]any{"request_id": "msg_early"})
}

// TestCallmeterSessionEdgeResolvesAnAbandonedAgentsPending: a sub-agent whose
// last batch missed the disk and that never fires SubagentStop (killed, or
// outlived a headless run) kept that request pending forever: the main chat's
// Stop resolved only the main chat's own (live e2e: 1 pending, its message on
// disk 1 s before the Stop). The main chat's Stop and SessionEnd now resolve
// every agent's; an agent whose transcript is missing stays pending with the
// one transcript fault of its batch, never a second.
func TestCallmeterSessionEdgeResolvesAnAbandonedAgentsPending(t *testing.T) {
	for _, edge := range []string{eventStop, callmeter.EventSessionEnd} {
		t.Run(edge, func(t *testing.T) {
			lab := newCallmeterLab(t)
			transcript := lab.transcript(cmSessionA)
			agent := callmeter.SubagentTranscriptPath(transcript, "aabandoned000001")
			if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
				t.Fatalf("create subagents dir: %v", err)
			}
			batch := func(agentID, id string) string {
				encoded, err := json.Marshal(map[string]any{
					"session_id": cmSessionA, "hook_event_name": "PostToolBatch", "transcript_path": transcript,
					"agent_id": agentID, "agent_type": "general-purpose",
					"tool_calls": []map[string]any{{"tool_use_id": id, "tool_response": "ok"}},
				})
				if err != nil {
					t.Fatalf("encode batch: %v", err)
				}
				return string(encoded)
			}
			lab.write(agent, []byte(""))
			lab.feed(batch("aabandoned000001", "toolu_abandoned1"))
			lab.feed(batch("amissing00000001", "toolu_missing001"))
			if n := lab.count("SELECT count(*) FROM requests WHERE pending = 1"); n != 2 {
				t.Fatalf("pending requests after the batches = %d, want 2", n)
			}
			lab.write(agent, []byte(`{"type":"assistant","timestamp":"2026-09-23T01:00:00.000Z","message":{"id":"msg_abandoned",`+
				`"content":[{"type":"tool_use","id":"toolu_abandoned1"}],"usage":{"input_tokens":5,`+
				`"cache_read_input_tokens":7,"cache_creation_input_tokens":0,"output_tokens":1}}}`+"\n"))
			encoded, err := json.Marshal(map[string]any{
				"session_id": cmSessionA, "hook_event_name": edge, "transcript_path": transcript, "reason": "other",
			})
			if err != nil {
				t.Fatalf("encode %s: %v", edge, err)
			}
			lab.feed(string(encoded))
			expect(t, "abandoned agent's call", lab.call("toolu_abandoned1"), map[string]any{"request_id": "msg_abandoned"})
			if n := lab.count("SELECT count(*) FROM requests WHERE request_id = 'msg_abandoned' AND pending = 0 AND context_tokens = 12"); n != 1 {
				t.Errorf("resolved request rows = %d, want 1 with context_tokens 12", n)
			}
			if n := lab.count("SELECT count(*) FROM requests WHERE pending = 1 AND agent_id = 'amissing00000001'"); n != 1 {
				t.Errorf("pending requests of the agent without a transcript = %d, want 1", n)
			}
			if n := lab.count("SELECT count(*) FROM faults WHERE stage = ? AND error LIKE '%amissing00000001%'",
				callmeter.StageTranscript); n != 1 {
				t.Errorf("transcript faults naming the agent without a transcript = %d, want 1", n)
			}
		})
	}
}

// TestCallmeterBatchUntypedAgentWithoutTranscriptIsNotRecorded:
// PostToolBatch used to call callmeter.FindRequests against the missing
// transcript of one of Claude Code's own internal agents (agent_id set,
// agent_type empty, never a SubagentStart), write a fault, then write a
// pending request and call resolvePending against the same missing file for
// a second fault — 2 faults, 1 call and 1 pending request for an agent
// nothing meters. recordAgent already exempts this class (os.Stat,
// fs.ErrNotExist); recordBatch now shares the same check.
func TestCallmeterBatchUntypedAgentWithoutTranscriptIsNotRecorded(t *testing.T) {
	lab := newCallmeterLab(t)
	batch := func(agentID, agentType, id string) string {
		m := map[string]any{
			"session_id": cmSessionA, "hook_event_name": "PostToolBatch",
			"transcript_path": lab.transcript(cmSessionA), "agent_id": agentID,
			"tool_calls": []map[string]any{{"tool_use_id": id, "tool_response": "ok"}},
		}
		if agentType != "" {
			m["agent_type"] = agentType
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode batch: %v", err)
		}
		return string(encoded)
	}
	lab.feed(batch("ainternal0000001", "", "toolu_untyped001"))
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults for the untyped agent's batch = %d, want 0", n)
	}
	if n := lab.count("SELECT count(*) FROM calls WHERE tool_use_id = 'toolu_untyped001'"); n != 0 {
		t.Errorf("calls for the untyped agent's batch = %d, want 0", n)
	}
	if n := lab.count("SELECT count(*) FROM requests"); n != 0 {
		t.Errorf("requests for the untyped agent's batch = %d, want 0", n)
	}
	// A typed agent's missing transcript still names exactly one transcript
	// fault: FindRequests fails once, and resolvePending is skipped rather
	// than failing the same read again.
	lab.feed(batch("atyped0000000001", "general-purpose", "toolu_typed001"))
	if n := lab.count("SELECT count(*) FROM faults WHERE stage = ? AND error LIKE '%atyped0000000001%'",
		callmeter.StageTranscript); n != 1 {
		t.Errorf("transcript faults naming the typed agent = %d, want 1", n)
	}
}

// mapEnv is a Getenv over a fixed map.
func mapEnv(env map[string]string) paths.Getenv {
	return func(name string) string { return env[name] }
}

// splitUsage is the usage object of a real transcript line (gym S1): a
// cache_creation object with both cache lifetimes.
const splitUsage = `{"input_tokens":2,"cache_creation_input_tokens":5206,"cache_read_input_tokens":12015,"output_tokens":374,` +
	`"cache_creation":{"ephemeral_1h_input_tokens":5206,"ephemeral_5m_input_tokens":0}}`

// flatUsage carries no cache_creation object.
const flatUsage = `{"input_tokens":7,"cache_creation_input_tokens":8,"cache_read_input_tokens":9,"output_tokens":1}`

// requestEntry is one assistant transcript line: message msg calling toolUseID.
func requestEntry(msg, toolUseID, model, stopReason, usage string) string {
	return `{"type":"assistant","timestamp":"2026-09-23T01:00:00.000Z","message":{"id":"` + msg + `","model":"` + model +
		`","stop_reason":"` + stopReason + `","content":[{"type":"tool_use","id":"` + toolUseID + `"}],"usage":` + usage + `}}` + "\n"
}

// toolPayload is a hook payload for one tool call in session A: the first
// main-chat capture of event with tool_name tool (else the event's first
// main-chat capture; see capturedLine), tool_use_id, tool_input and extra
// applied over it, the event's own keys (a tool_response, an error) in extra,
// an extra valued dropKey removing its key.
func toolPayload(t *testing.T, event, toolUseID, tool string, input any, extra map[string]any) string {
	t.Helper()
	payload := capturedLine(t, event, tool)
	setSession(payload)
	payload["tool_name"] = tool
	payload["tool_use_id"] = toolUseID
	payload["tool_input"] = input
	applyFields(payload, extra)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return string(encoded)
}

// batchPayload is a PostToolBatch payload in session A: the first main-chat
// capture of the event, transcript_path set to transcript, and tool_calls one
// entry per id, each cloned from the captured first entry with tool_use_id set
// and tool_response "ok".
func batchPayload(t *testing.T, transcript string, ids ...string) string {
	t.Helper()
	payload := capturedLine(t, "PostToolBatch", "")
	captured, _ := payload["tool_calls"].([]any)
	if len(captured) == 0 {
		t.Fatalf("the captured PostToolBatch payload holds no tool_calls entry to clone")
	}
	first, ok := captured[0].(map[string]any)
	if !ok {
		t.Fatalf("the captured PostToolBatch first tool_calls entry is %T, want an object", captured[0])
	}
	calls := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		call := make(map[string]any, len(first))
		for key, value := range first {
			call[key] = value
		}
		call["tool_use_id"] = id
		call["tool_response"] = "ok"
		calls = append(calls, call)
	}
	payload["session_id"] = cmSessionA
	payload["transcript_path"] = transcript
	payload["tool_calls"] = calls
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	return string(encoded)
}

// TestToolPayloadStartsFromACapture: toolPayload starts from the first
// main-chat capture of its event and tool, falling back to the event's first
// main-chat capture when the pair was never captured.
func TestToolPayloadStartsFromACapture(t *testing.T) {
	t.Run("the captured pair keeps its keys", func(t *testing.T) {
		// Only the gym S1 capture holds an Edit, and it carries an effort.
		got := decoded(t, toolPayload(t, "PostToolUse", "toolu_x", "Edit", map[string]any{"file_path": "f"}, nil))
		for _, key := range []string{"effort", "duration_ms", "permission_mode", "prompt_id", "tool_response"} {
			if _, ok := got[key]; !ok {
				t.Errorf("PostToolUse Edit payload lacks the captured key %q: %v", key, got)
			}
		}
	})
	t.Run("the call's own keys are overridden", func(t *testing.T) {
		got := decoded(t, toolPayload(t, "PostToolUse", "toolu_x", "Bash", map[string]any{"command": "echo hi"},
			map[string]any{"tool_response": "mine", "duration_ms": 5.0}))
		if got["tool_use_id"] != "toolu_x" || got["tool_name"] != "Bash" || got["tool_response"] != "mine" || got["duration_ms"] != 5.0 {
			t.Errorf("tool_use_id, tool_name, tool_response, duration_ms = %v, %v, %v, %v", got["tool_use_id"], got["tool_name"], got["tool_response"], got["duration_ms"])
		}
		if input, _ := got["tool_input"].(map[string]any); input["command"] != "echo hi" || len(input) != 1 {
			t.Errorf("tool_input = %v, want only the command", got["tool_input"])
		}
		if got["session_id"] != cmSessionA || got["cwd"] != cmDemoProj {
			t.Errorf("session_id, cwd = %v, %v; want session A, %s", got["session_id"], got["cwd"], cmDemoProj)
		}
	})
	t.Run("an extra valued dropKey leaves its key out", func(t *testing.T) {
		got := decoded(t, toolPayload(t, "PostToolUse", "toolu_x", "Edit", map[string]any{}, map[string]any{"effort": dropKey}))
		if _, ok := got["effort"]; ok {
			t.Errorf("effort is present after dropKey: %v", got)
		}
	})
	t.Run("a failure event keeps its error keys", func(t *testing.T) {
		got := decoded(t, toolPayload(t, "PostToolUseFailure", "toolu_x", "Bash", map[string]any{}, nil))
		if got["error"] == nil || got["is_interrupt"] == nil {
			t.Errorf("PostToolUseFailure payload lacks the captured error or is_interrupt: %v", got)
		}
	})
	t.Run("a pair no capture holds falls back to the event's first main-chat capture", func(t *testing.T) {
		got := decoded(t, toolPayload(t, "PostToolUse", "toolu_x", "NoSuchTool", map[string]any{}, nil))
		if got["tool_name"] != "NoSuchTool" {
			t.Errorf("tool_name = %v, want NoSuchTool", got["tool_name"])
		}
		for _, key := range []string{"duration_ms", "permission_mode", "prompt_id", "tool_response"} {
			if _, ok := got[key]; !ok {
				t.Errorf("the fallback payload lacks the captured PostToolUse key %q: %v", key, got)
			}
		}
		if _, ok := got["agent_id"]; ok {
			t.Errorf("the fallback payload is a sub-agent's: %v", got)
		}
	})
}

// TestBatchPayloadStartsFromACapture: batchPayload starts from the first
// main-chat PostToolBatch capture, one tool_calls entry per id cloned from the
// captured first entry.
func TestBatchPayloadStartsFromACapture(t *testing.T) {
	got := decoded(t, batchPayload(t, "/tmp/demo-home/t.jsonl", "toolu_a", "toolu_b", "toolu_c"))
	if got["session_id"] != cmSessionA || got["transcript_path"] != "/tmp/demo-home/t.jsonl" || got["hook_event_name"] != "PostToolBatch" {
		t.Errorf("session_id, transcript_path, hook_event_name = %v, %v, %v", got["session_id"], got["transcript_path"], got["hook_event_name"])
	}
	for _, key := range []string{"permission_mode", "prompt_id", "cwd"} {
		if _, ok := got[key]; !ok {
			t.Errorf("batch payload lacks the captured key %q: %v", key, got)
		}
	}
	if _, ok := got["agent_id"]; ok {
		t.Errorf("batch payload is a sub-agent's: %v", got)
	}
	calls, _ := got["tool_calls"].([]any)
	if len(calls) != 3 {
		t.Fatalf("tool_calls holds %d entries, want 3: %v", len(calls), got["tool_calls"])
	}
	for i, id := range []string{"toolu_a", "toolu_b", "toolu_c"} {
		call, _ := calls[i].(map[string]any)
		if call["tool_use_id"] != id || call["tool_response"] != "ok" {
			t.Errorf("entry %d tool_use_id, tool_response = %v, %v; want %s, ok", i, call["tool_use_id"], call["tool_response"], id)
		}
		if call["tool_name"] == nil || call["tool_input"] == nil {
			t.Errorf("entry %d lacks the captured first entry's tool_name or tool_input: %v", i, call)
		}
	}
}

// TestCallmeterRecordsTheTokenSplitAndAnEditsLines replays an Edit and two
// other calls whose requests the transcript holds: the requests rows carry the
// split, model and stop reason (the split NULL when the usage has no
// cache_creation object), and the Edit row its added and removed lines.
func TestCallmeterRecordsTheTokenSplitAndAnEditsLines(t *testing.T) {
	lab := newCallmeterLab(t)
	transcript := lab.transcript(cmSessionA)
	lab.write(transcript, []byte(
		requestEntry("msg_edit", "toolu_edit", "claude-sonnet-4-5", "tool_use", splitUsage)+
			requestEntry("msg_flat", "toolu_flat", "claude-haiku-4-5", "tool_use", flatUsage)))
	edit := toolPayload(t, "PostToolUse", "toolu_edit", "Edit",
		map[string]any{"file_path": cmDemoProj + "/a.txt", "old_string": "two", "new_string": "TWO"},
		map[string]any{"tool_response": json.RawMessage(
			`{"filePath":"/tmp/demo-proj/a.txt","oldString":"two","newString":"TWO","originalFile":"one\ntwo\nthree\n",` +
				`"replaceAll":false,"userModified":false,` +
				`"structuredPatch":[{"oldStart":1,"oldLines":3,"newStart":1,"newLines":3,"lines":[" one","-two","+TWO"," three"]}]}`)})
	lab.feed(edit, batchPayload(t, transcript, "toolu_edit", "toolu_flat"))

	expect(t, "edit call", lab.call("toolu_edit"), map[string]any{
		"lines_added": 1, "lines_removed": 1, "request_id": "msg_edit", "commit_sha": "<nil>", "test_runner": "<nil>",
	})
	expect(t, "split request", lab.row("SELECT * FROM requests WHERE request_id = 'msg_edit'"), map[string]any{
		"model": "claude-sonnet-4-5", "stop_reason": "tool_use", "input_tokens": 2, "cache_read_tokens": 12015,
		"cache_creation_tokens": 5206, "cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 5206,
		"context_tokens": 2 + 12015 + 5206, "output_tokens": 374, "pending": 0,
	})
	expect(t, "flat request", lab.row("SELECT * FROM requests WHERE request_id = 'msg_flat'"), map[string]any{
		"model": "claude-haiku-4-5", "input_tokens": 7, "cache_read_tokens": 9, "cache_creation_tokens": 8,
		"cache_creation_5m_tokens": "<nil>", "cache_creation_1h_tokens": "<nil>", "context_tokens": 24,
	})
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0", n)
	}
}

// TestCallmeterResolvedPendingRequestCarriesTheSplit: a batch whose request is
// not on disk goes pending; the Stop that finds it writes the split, model and
// stop reason on the resolved row.
func TestCallmeterResolvedPendingRequestCarriesTheSplit(t *testing.T) {
	lab := newCallmeterLab(t)
	transcript := lab.transcript(cmSessionA)
	lab.write(transcript, []byte(""))
	lab.feed(batchPayload(t, transcript, "toolu_late"))
	if n := lab.count("SELECT count(*) FROM requests WHERE pending = 1"); n != 1 {
		t.Fatalf("pending requests = %d, want 1", n)
	}
	lab.write(transcript, []byte(requestEntry("msg_late", "toolu_late", "claude-sonnet-4-5", "tool_use", splitUsage)))
	stop, err := json.Marshal(map[string]any{
		"session_id": cmSessionA, "hook_event_name": "Stop", "transcript_path": transcript,
	})
	if err != nil {
		t.Fatal(err)
	}
	lab.feed(string(stop))
	expect(t, "resolved request", lab.row("SELECT * FROM requests WHERE request_id = 'msg_late'"), map[string]any{
		"pending": 0, "model": "claude-sonnet-4-5", "stop_reason": "tool_use", "input_tokens": 2, "cache_read_tokens": 12015,
		"cache_creation_tokens": 5206, "cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 5206,
		"context_tokens": 17223, "output_tokens": 374,
	})
	if n := lab.count("SELECT count(*) FROM requests"); n != 1 {
		t.Errorf("requests = %d after the resolution, want 1", n)
	}
}

// TestCallmeterOutcomeColumns: the outcome of each call kind, on success and on
// failure alike where the test runner is concerned.
func TestCallmeterOutcomeColumns(t *testing.T) {
	lab := newCallmeterLab(t)
	bash := func(command string) map[string]any { return map[string]any{"command": command} }
	bashOut := func(stdout string) map[string]any {
		return map[string]any{"tool_response": map[string]any{"stdout": stdout, "stderr": "", "interrupted": false}}
	}
	lab.feed(
		toolPayload(t, "PostToolUse", "toolu_write", "Write",
			map[string]any{"file_path": cmDemoProj + "/new.txt", "content": "one\ntwo\nthree\n"},
			map[string]any{"tool_response": json.RawMessage(
				`{"type":"create","filePath":"/tmp/demo-proj/new.txt","content":"one\ntwo\nthree\n","structuredPatch":[],"originalFile":null,"userModified":false}`)}),
		toolPayload(t, "PostToolUse", "toolu_commit", "Bash", bash(`git commit -m "fix: thing"`),
			bashOut("[develop 1a2b3c4] fix: thing\n 1 file changed, 2 insertions(+)\n")),
		toolPayload(t, "PostToolUse", "toolu_root", "Bash", bash(`git commit -m init`), bashOut("[main (root-commit) abc1234] init\n")),
		toolPayload(t, "PostToolUse", "toolu_detached", "Bash", bash(`git commit -m x`), bashOut("[detached HEAD abc1234] x\n")),
		toolPayload(t, "PostToolUseFailure", "toolu_nocommit", "Bash", bash(`git commit -m x`),
			map[string]any{"error": "Exit code 1\nnothing to commit"}),
		toolPayload(t, "PostToolUse", "toolu_gotest", "Bash", bash(`go test ./...`), bashOut("ok  \tdemo\t0.1s\n")),
		toolPayload(t, "PostToolUseFailure", "toolu_pytest", "Bash", bash(`pytest -q`),
			map[string]any{"error": "Exit code 1\n1 failed"}),
		toolPayload(t, "PostToolUse", "toolu_build", "Bash", bash(`go build ./... && echo "go test"`), bashOut("go test\n")),
	)
	for id, want := range map[string]map[string]any{
		"toolu_write":    {"lines_added": 3, "lines_removed": 0, "commit_sha": "<nil>"},
		"toolu_commit":   {"commit_sha": "1a2b3c4", "commit_branch": "develop", "test_runner": "<nil>"},
		"toolu_root":     {"commit_sha": "abc1234", "commit_branch": "main"},
		"toolu_detached": {"commit_sha": "abc1234", "commit_branch": "detached HEAD"},
		"toolu_nocommit": {"commit_sha": "<nil>", "commit_branch": "<nil>", "failed": 1},
		"toolu_gotest":   {"test_runner": "go", "failed": 0},
		"toolu_pytest":   {"test_runner": "pytest", "failed": 1},
		"toolu_build":    {"test_runner": "<nil>"},
	} {
		expect(t, id, lab.call(id), want)
	}
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0", n)
	}
}

// TestCallmeterMalformedPatchLeavesTheOutcomeNull: a structuredPatch that is
// not a list is one payload fault naming the call and the field; the call is
// still recorded, its outcome columns NULL.
func TestCallmeterMalformedPatchLeavesTheOutcomeNull(t *testing.T) {
	lab := newCallmeterLab(t)
	lab.feed(toolPayload(t, "PostToolUse", "toolu_badpatch", "Edit",
		map[string]any{"file_path": cmDemoProj + "/a.txt"},
		map[string]any{"tool_response": json.RawMessage(`{"filePath":"/tmp/demo-proj/a.txt","structuredPatch":"nope"}`)}))
	expect(t, "call", lab.call("toolu_badpatch"), map[string]any{
		"tool": "Edit", "failed": 0, "lines_added": "<nil>", "lines_removed": "<nil>",
	})
	if n := lab.count("SELECT count(*) FROM faults"); n != 1 {
		t.Fatalf("faults = %d, want 1", n)
	}
	fault := lab.row("SELECT * FROM faults")
	expect(t, "fault", fault, map[string]any{"stage": callmeter.StagePayload, "tool_use_id": "toolu_badpatch"})
	if !strings.Contains(fault["error"], "structuredPatch") {
		t.Errorf("fault error %q does not name structuredPatch", fault["error"])
	}
}

// storeText is every value of every table of the store as one string.
func (lab *callmeterLab) storeText() string {
	lab.t.Helper()
	db := lab.db().DB()
	tables, err := db.QueryContext(lab.ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		lab.t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			lab.t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := errors.Join(tables.Err(), tables.Close()); err != nil {
		lab.t.Fatalf("read table names: %v", err)
	}
	var text strings.Builder
	for _, name := range names {
		rows, err := db.QueryContext(lab.ctx, "SELECT * FROM "+name)
		if err != nil {
			lab.t.Fatalf("read %s: %v", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			lab.t.Fatalf("columns of %s: %v", name, err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				lab.t.Fatalf("scan %s: %v", name, err)
			}
			text.WriteString(fmt.Sprintln(values...))
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			lab.t.Fatalf("read rows of %s: %v", name, err)
		}
	}
	return text.String()
}

// TestCallmeterOutcomesStoreCountsNotText: the outcome columns hold counts, a
// sha and a branch; no patch line, written content or commit output reaches
// the store.
func TestCallmeterOutcomesStoreCountsNotText(t *testing.T) {
	lab := newCallmeterLab(t)
	lab.feed(
		toolPayload(t, "PostToolUse", "toolu_edit", "Edit",
			map[string]any{"file_path": cmDemoProj + "/a.txt", "old_string": "old-secret-line", "new_string": "new-secret-line"},
			map[string]any{"tool_response": json.RawMessage(
				`{"filePath":"/tmp/demo-proj/a.txt","oldString":"old-secret-line","newString":"new-secret-line",` +
					`"originalFile":"old-secret-line\n","userModified":false,` +
					`"structuredPatch":[{"lines":["-old-secret-line","+new-secret-line"]}]}`)}),
		toolPayload(t, "PostToolUse", "toolu_write", "Write",
			map[string]any{"file_path": cmDemoProj + "/new.txt", "content": "alpha-secret-content\n"},
			map[string]any{"tool_response": json.RawMessage(
				`{"type":"create","filePath":"/tmp/demo-proj/new.txt","content":"alpha-secret-content\n","structuredPatch":[],"originalFile":null}`)}),
		toolPayload(t, "PostToolUse", "toolu_commit", "Bash", map[string]any{"command": "git commit -F msg.txt"},
			map[string]any{"tool_response": map[string]any{
				"stdout": "[develop 1a2b3c4] subject-from-stdout-only\n 2 files changed, 9 insertions(+)\n", "stderr": "",
			}}),
	)
	expect(t, "edit", lab.call("toolu_edit"), map[string]any{"lines_added": 1, "lines_removed": 1})
	expect(t, "write", lab.call("toolu_write"), map[string]any{"lines_added": 1, "lines_removed": 0})
	expect(t, "commit", lab.call("toolu_commit"), map[string]any{"commit_sha": "1a2b3c4", "commit_branch": "develop"})
	text := lab.storeText()
	for _, secret := range []string{"old-secret-line", "new-secret-line", "alpha-secret-content", "subject-from-stdout-only", "2 files changed"} {
		if strings.Contains(text, secret) {
			t.Errorf("the store holds %q, which must stay out of it", secret)
		}
	}
}

// withFields is payload with fields set over its own keys.
func withFields(t *testing.T, payload string, fields map[string]any) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for key, value := range fields {
		decoded[key] = value
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return string(encoded)
}

// feedAt feeds payload with the hook's clock reading at ms.
func (lab *callmeterLab) feedAt(ms int64, payload string) {
	lab.t.Helper()
	lab.clock = clock.NewFake(time.UnixMilli(ms))
	lab.feed(payload)
}

func TestCallmeterTier1OnCalls(t *testing.T) {
	echo := map[string]any{"command": "echo hi"}
	t.Run("payload fields", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(toolPayload(t, "PostToolUse", "toolu_t1", "Bash", echo, map[string]any{
			"prompt_id": "p-1", "effort": map[string]any{"level": "high"}, "permission_mode": "acceptEdits",
			"tool_response": map[string]any{"stdout": "hi\n"},
		}))
		expect(t, "call", lab.call("toolu_t1"), map[string]any{"prompt_id": "p-1", "effort": "high", "permission_mode": "acceptEdits"})
	})
	t.Run("effort from env", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.env["CLAUDE_EFFORT"] = "xhigh"
		lab.feed(
			toolPayload(t, "PostToolUse", "toolu_t1", "Bash", echo, map[string]any{"tool_response": map[string]any{"stdout": "hi\n"}}),
			hookPayload(t, "Stop", map[string]any{"stop_hook_active": false}),
		)
		expect(t, "call", lab.call("toolu_t1"), map[string]any{"effort": "xhigh"})
		expect(t, "turn", lab.row("SELECT effort FROM turns"), map[string]any{"effort": "xhigh"})
		expect(t, "event", lab.row("SELECT effort FROM events"), map[string]any{"effort": "xhigh"})
	})
	t.Run("effort absent", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(
			toolPayload(t, "PostToolUse", "toolu_t1", "Bash", echo, map[string]any{"tool_response": map[string]any{"stdout": "hi\n"}}),
			hookPayload(t, "Stop", nil),
		)
		expect(t, "call", lab.call("toolu_t1"), map[string]any{"effort": nil})
		expect(t, "turn", lab.row("SELECT effort FROM turns"), map[string]any{"effort": nil})
		expect(t, "event", lab.row("SELECT effort FROM events"), map[string]any{"effort": nil})
	})
	t.Run("interrupt", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(
			toolPayload(t, "PostToolUseFailure", "toolu_no", "Bash", echo, map[string]any{"error": "exit 1", "is_interrupt": false}),
			toolPayload(t, "PostToolUseFailure", "toolu_yes", "Bash", echo, map[string]any{"error": "interrupted", "is_interrupt": true}),
		)
		expect(t, "not interrupted", lab.call("toolu_no"), map[string]any{"is_interrupt": 0})
		expect(t, "interrupted", lab.call("toolu_yes"), map[string]any{"is_interrupt": 1})
	})
	t.Run("pre fields", func(t *testing.T) {
		pre := toolPayload(t, "PreToolUse", "toolu_t1", "Bash", echo, map[string]any{"prompt_id": "p-pre", "permission_mode": "plan"})
		post := toolPayload(t, "PostToolUse", "toolu_t1", "Bash", echo, map[string]any{
			"prompt_id": "p-post", "permission_mode": "default", "tool_response": map[string]any{"stdout": "hi\n"},
		})
		alone := newCallmeterLab(t)
		alone.feed(pre)
		expect(t, "pre alone", alone.call("toolu_t1"), map[string]any{"prompt_id": "p-pre", "permission_mode": "plan"})
		for name, order := range map[string][]string{"pre first": {pre, post}, "post first": {post, pre}} {
			lab := newCallmeterLab(t)
			lab.feed(order...)
			expect(t, name, lab.call("toolu_t1"), map[string]any{"prompt_id": "p-post", "permission_mode": "default"})
		}
	})
}

func TestCallmeterRequestCarriesThePrompt(t *testing.T) {
	lab := newCallmeterLab(t)
	transcript := filepath.Join(lab.root, "main.jsonl")
	lab.write(transcript, []byte(requestEntry("msg_p1", "toolu_p1", "claude-opus-4-1", "tool_use", flatUsage)))
	lab.feed(withFields(t, batchPayload(t, transcript, "toolu_p1"), map[string]any{"prompt_id": "p-batch"}))
	expect(t, "request", lab.row("SELECT * FROM requests WHERE request_id = ?", "msg_p1"), map[string]any{"prompt_id": "p-batch"})
}

// TestCallmeterAgentSpanInAnyOrder: an agent woken twice fires two starts and
// two stops; whatever order they land in, started is the first start, stopped
// the last stop, the totals the last stop's, and agent_turns pairs them.
func TestCallmeterAgentSpanInAnyOrder(t *testing.T) {
	base := time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC).UnixMilli()
	at := func(n int64) int64 { return base + n*1000 }
	type edge struct {
		n       int64
		payload func(lab *callmeterLab) string
		turns   int // the agent transcript's turns on disk when it lands
	}
	start := func(prompt string) func(lab *callmeterLab) string {
		return func(lab *callmeterLab) string {
			return withFields(t, lab.payloads("scripted.jsonl")[5], map[string]any{"prompt_id": prompt})
		}
	}
	stop := func(prompt string) func(lab *callmeterLab) string {
		return func(lab *callmeterLab) string {
			return withFields(t, lab.payloads("scripted.jsonl")[8], map[string]any{"prompt_id": prompt})
		}
	}
	edges := []edge{{1, start("p-1"), 1}, {3, stop("p-1"), 1}, {5, start("p-5"), 2}, {9, stop("p-5"), 2}}
	var orders [][]int
	var permute func(prefix []int, rest []int)
	permute = func(prefix []int, rest []int) {
		if len(rest) == 0 {
			orders = append(orders, prefix)
			return
		}
		for i := range rest {
			next := append(append([]int{}, rest[:i]...), rest[i+1:]...)
			permute(append(append([]int{}, prefix...), rest[i]), next)
		}
	}
	permute(nil, []int{0, 1, 2, 3})
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			lab := newCallmeterLab(t)
			agentTranscript, _ := payloadField(t, lab.payloads("scripted.jsonl")[8], "agent_transcript_path").(string)
			first, err := os.ReadFile(agentTranscript)
			if err != nil {
				t.Fatalf("read agent transcript: %v", err)
			}
			for _, i := range order {
				content := first
				if edges[i].turns == 2 {
					content = append(append([]byte{}, first...), secondAgentTurn...)
				}
				lab.write(agentTranscript, content)
				lab.feedAt(at(edges[i].n), edges[i].payload(lab))
			}
			expect(t, "agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
				"prompt_id": "p-1", "started": at(1), "stopped": at(9),
				"total_tokens": 18107 + (3 + 7500 + 40 + 30) + (2 + 7540 + 60 + 8), "tool_uses": 3,
			})
			if n := lab.count("SELECT count(*) FROM agent_turns WHERE agent_id = ?", cmSubagent); n != 2 {
				t.Fatalf("agent_turns = %d, want 2", n)
			}
			for seq, span := range [][2]int64{{at(1), at(3)}, {at(5), at(9)}} {
				expect(t, fmt.Sprintf("turn %d", seq+1),
					lab.row("SELECT * FROM agent_turns WHERE agent_id = ? AND seq = ?", cmSubagent, seq+1),
					map[string]any{"started": span[0], "stopped": span[1]})
			}
		})
	}
}

// TestCallmeterConcurrentStopsKeepTheLatest: two SubagentStop runs of one
// agent write at once, each with the transcript its stop saw. The latest stop
// and its totals win, however the two transactions interleave.
func TestCallmeterConcurrentStopsKeepTheLatest(t *testing.T) {
	base := time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC).UnixMilli()
	for round := range 24 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			lab := newCallmeterLab(t)
			stop := lab.payloads("scripted.jsonl")[8]
			agentTranscript, _ := payloadField(t, stop, "agent_transcript_path").(string)
			first, err := os.ReadFile(agentTranscript)
			if err != nil {
				t.Fatalf("read agent transcript: %v", err)
			}
			later := filepath.Join(filepath.Dir(agentTranscript), "later.jsonl")
			lab.write(later, append(append([]byte{}, first...), secondAgentTurn...))
			runs := []struct {
				ts         int64
				transcript string
			}{{base + 3000, agentTranscript}, {base + 9000, later}}
			shared := filepath.Join(lab.home, ".claude")
			gate := make(chan struct{})
			codes := make([]int, len(runs))
			stderrs := make([]bytes.Buffer, len(runs))
			var wg sync.WaitGroup
			for i, run := range runs {
				payload := withFields(t, stop, map[string]any{"agent_transcript_path": run.transcript})
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					codes[i] = runCallmeter(lab.ctx, strings.NewReader(payload), &stderrs[i], lab.files(),
						clock.NewFake(time.UnixMilli(run.ts)), callmeterSeat{configDir: &shared}, mapEnv(lab.env))
				}()
			}
			close(gate)
			wg.Wait()
			for i, code := range codes {
				if code != 0 || stderrs[i].Len() > 0 {
					t.Fatalf("stop %d: exit %d, stderr %q; want 0 and silent", i, code, stderrs[i].String())
				}
			}
			expect(t, "agent", lab.row("SELECT * FROM agents WHERE agent_id = ?", cmSubagent), map[string]any{
				"stopped": base + 9000, "total_tokens": 18107 + (3 + 7500 + 40 + 30) + (2 + 7540 + 60 + 8), "tool_uses": 3,
			})
			if n := lab.count("SELECT COUNT(*) FROM faults"); n != 0 {
				t.Errorf("faults = %d, want 0", n)
			}
		})
	}
}

// TestCallmeterPreToolUseFillsTheInput: a Bash call whose PostToolUse is lost
// keeps PreToolUse's input and test runner; a later PostToolUse overwrites
// them; an input SanitizeInput refuses is a payload fault, and the rest of the
// start still lands.
func TestCallmeterPreToolUseFillsTheInput(t *testing.T) {
	pre := toolPayload(t, "PreToolUse", "toolu_in", "Bash", map[string]any{"command": "go test ./..."}, nil)
	t.Run("lost PostToolUse", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(pre)
		expect(t, "call", lab.call("toolu_in"), map[string]any{"input": `{"command":"go test ./..."}`, "test_runner": "go", "cwd": cmDemoProj})
	})
	t.Run("later PostToolUse", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(pre, toolPayload(t, "PostToolUse", "toolu_in", "Bash", map[string]any{"command": "pytest -q"},
			map[string]any{"tool_response": map[string]any{"stdout": "ok\n"}}))
		expect(t, "call", lab.call("toolu_in"), map[string]any{"input": `{"command":"pytest -q"}`, "test_runner": "pytest"})
	})
	t.Run("refused input", func(t *testing.T) {
		lab := newCallmeterLab(t)
		lab.feed(toolPayload(t, "PreToolUse", "toolu_in", "Bash", "not an object", nil))
		expect(t, "call", lab.call("toolu_in"), map[string]any{"input": nil, "test_runner": nil, "cwd": cmDemoProj, "tool": "Bash"})
		if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = ? AND tool_use_id = ?", callmeter.StagePayload, "toolu_in"); n != 1 {
			t.Errorf("payload faults = %d, want 1", n)
		}
	})
}

// TestCallmeterRedeliveredBatchCountsItsCallsOnce: async hooks are sometimes
// delivered twice, and a request's calls is the number of calls carrying it in
// any landing order. A batch first landing before its request reached the disk
// (provisional) and again after it (resolved) must leave calls = 2, not the
// provisional count summed onto the resolved one.
func TestCallmeterRedeliveredBatchCountsItsCallsOnce(t *testing.T) {
	for _, order := range []string{"provisional-first", "resolved-first"} {
		t.Run(order, func(t *testing.T) {
			lab := newCallmeterLab(t)
			transcript := lab.transcript(cmSessionA)
			onDisk := requestEntry("msg_both", "toolu_a", "claude-haiku", "tool_use", flatUsage) +
				requestEntry("msg_both", "toolu_b", "claude-haiku", "tool_use", flatUsage)
			if order == "provisional-first" {
				lab.write(transcript, []byte(""))
				lab.feed(batchPayload(t, transcript, "toolu_a", "toolu_b"))
				lab.write(transcript, []byte(onDisk))
				lab.feed(batchPayload(t, transcript, "toolu_a", "toolu_b"))
			} else {
				lab.write(transcript, []byte(onDisk))
				lab.feed(batchPayload(t, transcript, "toolu_a", "toolu_b"))
				lab.feed(batchPayload(t, transcript, "toolu_a", "toolu_b"))
			}
			expect(t, "request", lab.row("SELECT * FROM requests WHERE request_id = ?", "msg_both"),
				map[string]any{"calls": "2", "pending": "0"})
			if n := lab.count("SELECT count(*) FROM requests"); n != 1 {
				t.Errorf("requests rows = %d, want 1", n)
			}
		})
	}
}

// TestCallmeterPreToolUseOfUntypedAgentWithoutTranscriptIsNotRecorded: live,
// Claude Code's own internal agents (no agent_type, no transcript on disk)
// fire PreToolUse for a Bash call and nothing after it — no PostToolUse, and
// their PostToolBatch is not metered — so a start row would wait forever for
// a size it can never get (7 such rows in the live store). Their PreToolUse
// writes no call, as their batch and stop write nothing; a typed agent's start
// is still recorded.
func TestCallmeterPreToolUseOfUntypedAgentWithoutTranscriptIsNotRecorded(t *testing.T) {
	lab := newCallmeterLab(t)
	start := func(agentID, agentType, id string) string {
		m := map[string]any{
			"session_id": cmSessionA, "hook_event_name": "PreToolUse", "cwd": cmDemoProj,
			"transcript_path": lab.transcript(cmSessionA), "agent_id": agentID,
			"tool_name": "Bash", "tool_use_id": id,
			"tool_input": map[string]any{"command": "ls", "description": "list"},
		}
		if agentType != "" {
			m["agent_type"] = agentType
		}
		encoded, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("encode PreToolUse: %v", err)
		}
		return string(encoded)
	}
	lab.feed(
		start("ainternal0000001", "", "toolu_untypedstart1"),
		start("atyped0000000001", "general-purpose", "toolu_typedstart1"),
	)
	if n := lab.count("SELECT count(*) FROM calls WHERE tool_use_id = 'toolu_untypedstart1'"); n != 0 {
		t.Errorf("calls for the untyped agent's PreToolUse = %d, want 0", n)
	}
	if n := lab.count("SELECT count(*) FROM calls WHERE tool_use_id = 'toolu_typedstart1'"); n != 1 {
		t.Errorf("calls for the typed agent's PreToolUse = %d, want 1", n)
	}
	if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
		t.Errorf("faults = %d, want 0", n)
	}
}

// TestCallmeterBatchMarksDeniedAndRejectedCalls: live (claude -p with a
// PreToolUse deny hook and an ungranted Write), a denied call fires neither
// PostToolUse nor PostToolUseFailure; its PostToolBatch alone carries the
// harness's text. The batch marks it failed with its outcome label — never
// the hook's or the prompt's own words — and an ordinary result stays unmarked.
func TestCallmeterBatchMarksDeniedAndRejectedCalls(t *testing.T) {
	lab := newCallmeterLab(t)
	responses := []struct{ id, text, outcome string }{
		{"toolu_hookdeny01", "PreToolUse:Bash hook error: invented reason", callmeter.OutcomeDeniedByHook},
		{"toolu_permdeny01", "Claude requested permissions to write to /tmp/demo-proj/out.txt, but you haven't granted it yet.",
			callmeter.OutcomeDeniedByPermission},
		{"toolu_rejected01", "The user doesn't want to proceed with this tool use. The tool use was rejected.",
			callmeter.OutcomeRejectedByUser},
		{"toolu_ordinary01", "PreToolUse is a hook event", ""},
	}
	var calls []map[string]any
	for _, r := range responses {
		calls = append(calls, map[string]any{"tool_use_id": r.id, "tool_name": "Bash", "tool_response": r.text})
	}
	encoded, err := json.Marshal(map[string]any{
		"session_id": cmSessionA, "hook_event_name": "PostToolBatch",
		"transcript_path": lab.transcript(cmSessionA), "tool_calls": calls,
	})
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	lab.feed(string(encoded))
	for _, r := range responses {
		want := map[string]any{"bytes_delivered": len(r.text), "bytes_real": len(r.text), "failed": 1, "error": r.outcome}
		if r.outcome == "" {
			want["bytes_real"], want["failed"], want["error"] = "<nil>", "<nil>", "<nil>"
		}
		expect(t, r.id, lab.call(r.id), want)
	}
}

// TestCallmeterBatchFillsInputAndFileOfAWriteNoHookSaw: only Bash has a
// PreToolUse hook, so a Write the harness refused before it ran has its batch
// entry alone, and that entry's tool_input is the only place its input and
// file_path can come from. The input is stored sanitized (the content text is
// nowhere in the store); a Write whose PostToolUse landed first keeps the
// input and file_path that hook stored, whatever the batch's entry says.
func TestCallmeterBatchFillsInputAndFileOfAWriteNoHookSaw(t *testing.T) {
	const content = "invented-write-content-words"
	const refusal = "Claude requested permissions to write to %s, but you haven't granted it yet."
	batch := func(t *testing.T, lab *callmeterLab, id, path string) string {
		t.Helper()
		encoded, err := json.Marshal(map[string]any{
			"session_id": cmSessionA, "hook_event_name": "PostToolBatch", "transcript_path": lab.transcript(cmSessionA),
			"tool_calls": []map[string]any{{
				"tool_use_id": id, "tool_name": "Write",
				"tool_input":    map[string]any{"file_path": path, "content": content},
				"tool_response": fmt.Sprintf(refusal, path),
			}},
		})
		if err != nil {
			t.Fatalf("encode batch: %v", err)
		}
		return string(encoded)
	}
	t.Run("batch alone", func(t *testing.T) {
		lab := newCallmeterLab(t)
		path := filepath.Join(lab.proj, "refused.txt")
		lab.feed(batch(t, lab, "toolu_refusedwr1", path))
		row := lab.call("toolu_refusedwr1")
		expect(t, "call", row, map[string]any{"file_path": path, "failed": 1, "error": callmeter.OutcomeDeniedByPermission})
		if row["input"] == "<nil>" || row["input"] == "" {
			t.Errorf("input = %q, want the batch's sanitized tool_input", row["input"])
		}
		if strings.Contains(lab.storeText(), content) {
			t.Errorf("the store holds the Write's content text")
		}
	})
	t.Run("earlier hook's columns stay", func(t *testing.T) {
		lab := newCallmeterLab(t)
		const id = "toolu_keptwrite01"
		first := filepath.Join(lab.proj, "first.txt")
		lab.feed(toolPayload(t, "PostToolUse", id, "Write",
			map[string]any{"file_path": first, "content": "other-invented-words"}, map[string]any{"tool_response": map[string]any{}}))
		before := lab.call(id)
		if before["file_path"] != first || before["input"] == "<nil>" {
			t.Fatalf("PostToolUse stored file_path %q, input %q, want %q and an input", before["file_path"], before["input"], first)
		}
		lab.feed(batch(t, lab, id, filepath.Join(lab.proj, "second.txt")))
		after := lab.call(id)
		for _, column := range []string{"input", "file_path"} {
			if after[column] != before[column] {
				t.Errorf("%s = %q after the batch, want the hook's %q", column, after[column], before[column])
			}
		}
		if after["bytes_delivered"] == "<nil>" {
			t.Errorf("bytes_delivered = %q, want the batch's size", after["bytes_delivered"])
		}
	})
}

// TestCallmeterBatchLabelsRefusalBesideItsStart: live (reconcile over the
// 2026-10-01 store, 10 calls), a call a PreToolUse hook blocked or permission
// denied fired no PostToolUse or PostToolUseFailure, and the store kept only
// its batch's delivered size. A Bash call had callmeter's own PreToolUse row
// first (its matcher is Bash), in a sub-agent too; an Edit or a Write had
// none. Its batch still marks it failed with its outcome label, whichever of
// PreToolUse and PostToolBatch lands first and when the batch lands twice, and
// the hook's own words reach no column.
func TestCallmeterBatchLabelsRefusalBesideItsStart(t *testing.T) {
	const reason = "invented-hook-reason-words"
	cases := []struct {
		name, tool, agent, response, outcome string
		started                              bool
	}{
		{"main Bash", "Bash", "", "PreToolUse:Bash hook error: " + reason, callmeter.OutcomeDeniedByHook, true},
		{"sub-agent Bash", "Bash", "agitter000000002", "PreToolUse:Bash hook error: " + reason, callmeter.OutcomeDeniedByHook, true},
		{"Edit", "Edit", "", "PreToolUse:Edit hook error: " + reason, callmeter.OutcomeDeniedByHook, false},
		{"Write", "Write", "", "Claude requested permissions to write to /tmp/demo-proj/out.txt, but you haven't granted it yet.",
			callmeter.OutcomeDeniedByPermission, false},
	}
	for _, c := range cases {
		orders := [][]string{{"batch"}, {"batch", "batch"}}
		if c.started {
			orders = [][]string{{"start", "batch"}, {"batch", "start"}, {"start", "batch", "batch"}}
		}
		for _, order := range orders {
			t.Run(c.name+"/"+strings.Join(order, "-"), func(t *testing.T) {
				lab := newCallmeterLab(t)
				const id = "toolu_refusedcall1"
				fields := func(event string) map[string]any {
					f := map[string]any{"session_id": cmSessionA, "hook_event_name": event, "cwd": cmDemoProj,
						"transcript_path": lab.transcript(cmSessionA)}
					if c.agent != "" {
						f["agent_id"], f["agent_type"] = c.agent, "gitter"
					}
					return f
				}
				if c.agent != "" {
					path := callmeter.SubagentTranscriptPath(lab.transcript(cmSessionA), c.agent)
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatalf("create subagents dir: %v", err)
					}
					lab.write(path, nil)
				}
				start := fields("PreToolUse")
				start["tool_name"], start["tool_use_id"] = c.tool, id
				start["tool_input"] = map[string]any{"command": "git status", "description": "status"}
				batch := fields("PostToolBatch")
				batch["tool_calls"] = []map[string]any{{"tool_use_id": id, "tool_name": c.tool, "tool_response": c.response}}
				payloads := map[string]map[string]any{"start": start, "batch": batch}
				for _, step := range order {
					encoded, err := json.Marshal(payloads[step])
					if err != nil {
						t.Fatalf("encode %s: %v", step, err)
					}
					lab.feed(string(encoded))
				}
				row := lab.call(id)
				expect(t, c.name, row, map[string]any{
					"tool": c.tool, "bytes_delivered": len(c.response), "bytes_real": len(c.response),
					"failed": 1, "error": c.outcome,
				})
				if row["ts"] == "<nil>" {
					t.Errorf("%s.ts is NULL, want the earliest hook's", c.name)
				}
				if strings.Contains(lab.storeText(), reason) {
					t.Errorf("the store holds the hook's reason %q, which must stay out of it", reason)
				}
			})
		}
	}
}

// TestCallmeterSessionEdgeSettlesUnfinishedCalls: live, a sub-agent's Bash
// call the user interrupted fired PreToolUse and nothing else — no
// PostToolUseFailure, no PostToolBatch, no SubagentStop — while its transcript
// holds the tool_result the model received; and a call Claude Code refused
// before any PostToolUse kept only its batch's delivered size. The chat's next
// Stop, or its SessionEnd when the user quits without another prompt, fills
// each from that result: the size, failed and the outcome label, and a
// refusal's real size, its text being the whole output. A call with no result
// yet stays unknown.
func TestCallmeterSessionEdgeSettlesUnfinishedCalls(t *testing.T) {
	for _, edge := range []string{eventStop, callmeter.EventSessionEnd} {
		t.Run(edge, func(t *testing.T) {
			lab := newCallmeterLab(t)
			const agent = "agitter000000001"
			start := func(id string) string {
				encoded, err := json.Marshal(map[string]any{
					"session_id": cmSessionA, "hook_event_name": "PreToolUse", "cwd": cmDemoProj,
					"transcript_path": lab.transcript(cmSessionA), "agent_id": agent, "agent_type": "gitter",
					"tool_name": "Bash", "tool_use_id": id,
					"tool_input": map[string]any{"command": "git push", "description": "push"},
				})
				if err != nil {
					t.Fatalf("encode PreToolUse: %v", err)
				}
				return string(encoded)
			}
			const rejected = "The user doesn't want to proceed with this tool use. The tool use was rejected."
			lines := []map[string]any{
				{"type": "assistant", "timestamp": "2026-09-23T01:00:00.000Z", "message": map[string]any{"id": "msg_invented01", "content": []map[string]any{
					{"type": "tool_use", "id": "toolu_interrupted1", "name": "Bash", "input": map[string]any{"command": "git push"}},
				}}},
				{"type": "user", "message": map[string]any{"content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_interrupted1", "content": rejected, "is_error": true},
				}}},
				{"type": "user", "message": map[string]any{"content": []map[string]any{
					{"type": "text", "text": "[Request interrupted by user for tool use]"},
				}}},
			}
			var transcript []byte
			for _, line := range lines {
				encoded, err := json.Marshal(line)
				if err != nil {
					t.Fatalf("encode transcript line: %v", err)
				}
				transcript = append(append(transcript, encoded...), '\n')
			}
			path := callmeter.SubagentTranscriptPath(lab.transcript(cmSessionA), agent)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("create subagents dir: %v", err)
			}
			lab.write(path, transcript)
			const denied = "PreToolUse:Bash hook error: invented reason"
			main := lab.transcript(cmSessionA)
			lab.write(main, []byte(sweepEntry("msg_invented02", "01", `"tool_use"`, 5,
				`{"type":"tool_use","id":"toolu_refusedold1","name":"Bash","input":{"command":"git status"}}`)+
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_refusedold1","content":"`+
				denied+`","is_error":true}]}}`+"\n"+
				sweepEntry("msg_invented03", "02", `"end_turn"`, 3, `{"type":"text","text":"ok"}`)))
			// The batch's text, not the harness's opening: no label, no real size.
			batch, err := json.Marshal(map[string]any{
				"session_id": cmSessionA, "hook_event_name": "PostToolBatch", "transcript_path": main,
				"tool_calls": []map[string]any{{"tool_use_id": "toolu_refusedold1", "tool_name": "Bash", "tool_response": "Error: " + denied}},
			})
			if err != nil {
				t.Fatalf("encode PostToolBatch: %v", err)
			}
			ending := map[string]any{"session_id": cmSessionA, "hook_event_name": edge, "transcript_path": main}
			if edge == callmeter.EventSessionEnd {
				ending["reason"] = "other"
			}
			end, err := json.Marshal(ending)
			if err != nil {
				t.Fatalf("encode %s: %v", edge, err)
			}
			lab.feed(start("toolu_interrupted1"), start("toolu_stillrunning1"), string(batch), string(end))
			expect(t, "interrupted call", lab.call("toolu_interrupted1"), map[string]any{
				"bytes_delivered": len(rejected), "bytes_real": len(rejected), "failed": 1, "error": callmeter.OutcomeRejectedByUser,
			})
			expect(t, "refused call", lab.call("toolu_refusedold1"), map[string]any{
				"bytes_real": len(denied), "failed": 1, "error": callmeter.OutcomeDeniedByHook,
			})
			expect(t, "running call", lab.call("toolu_stillrunning1"), map[string]any{
				"bytes_delivered": "<nil>", "bytes_real": "<nil>", "failed": "<nil>", "error": "<nil>",
			})
			// At SessionEnd the call still running has no result and will get none:
			// that is a fault naming it; at Stop it may be a background call.
			wantFaults := 0
			if edge == callmeter.EventSessionEnd {
				wantFaults = 1
				if n := lab.count("SELECT count(*) FROM faults WHERE tool_use_id = 'toolu_stillrunning1' AND stage = ?", callmeter.StageTranscript); n != 1 {
					t.Errorf("faults naming the call with no result = %d, want 1", n)
				}
			}
			if n := lab.count("SELECT count(*) FROM faults"); n != wantFaults {
				t.Errorf("faults = %d, want %d: %s", n, wantFaults, lab.faultsText())
			}
		})
	}
}

// sweepEntry is one transcript entry of message id as Claude Code writes it:
// one content block per entry, the usage as it stood when the block was
// written, stop null on every entry but the last.
func sweepEntry(id, second, stop string, output int, block string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":"2026-09-23T01:00:%s.000Z","message":{"id":"%s","model":"claude-opus-4-1",`+
		`"stop_reason":%s,"content":[%s],"usage":{"input_tokens":3,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,`+
		`"output_tokens":%d}}}`+"\n", second, id, stop, block, output)
}

// TestCallmeterSessionEdgeSweepsEveryRequest: the main chat's Stop and
// SessionEnd write every request its transcripts hold at its final usage. A
// row its batch stored from a message's first entry (output 8, no stop
// reason: the batch read before the rest of the message was on disk) is
// corrected to the final entry's; the turn's closing reply, which has no tool
// call and so no batch, gets a row with 0 calls, its prompt and its tokens,
// never its text; and an interrupted sub-agent's call, whose batch never ran,
// is pointed at its request. A reply from before the session's first recorded
// run (callmeter enabled mid-session) is not back-filled.
func TestCallmeterSessionEdgeSweepsEveryRequest(t *testing.T) {
	for _, edge := range []string{eventStop, callmeter.EventSessionEnd} {
		t.Run(edge, func(t *testing.T) {
			lab := newCallmeterLab(t)
			main := lab.transcript(cmSessionA)
			const agent = "asweep0000000001"
			encode := func(fields map[string]any) string {
				fields["session_id"], fields["transcript_path"] = cmSessionA, main
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatalf("encode payload: %v", err)
				}
				return string(encoded)
			}
			const secret = "invented closing reply text"
			prompt := sweepEntry("msg_sweep_old", "00", `"end_turn"`, 31, `{"type":"text","text":"invented"}`) +
				`{"type":"user","promptId":"prompt-sweep-1","message":{"role":"user","content":"invented"}}` + "\n"
			prompt = strings.Replace(prompt, "01:00:00.000Z", "00:59:00.000Z", 1)
			first := sweepEntry("msg_sweep_calls", "01", "null", 8, `{"type":"tool_use","id":"toolu_sweep_a","name":"Bash","input":{}}`)
			lab.write(main, []byte(prompt+first))
			lab.feedAt(1790125199000, batchPayload(t, main, "toolu_sweep_a"))
			expect(t, "batch-time request", lab.row("SELECT output_tokens, stop_reason FROM requests WHERE request_id = 'msg_sweep_calls'"),
				map[string]any{"output_tokens": 8, "stop_reason": "<nil>"})
			lab.write(main, []byte(prompt+first+
				sweepEntry("msg_sweep_calls", "02", `"tool_use"`, 643, `{"type":"tool_use","id":"toolu_sweep_b","name":"Bash","input":{}}`)+
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_sweep_a","content":"ok"}]}}`+"\n"+
				sweepEntry("msg_sweep_reply", "05", "null", 9, `{"type":"thinking","thinking":"invented"}`)+
				sweepEntry("msg_sweep_reply", "06", `"end_turn"`, 212, `{"type":"text","text":"`+secret+`"}`)))
			agentPath := callmeter.SubagentTranscriptPath(main, agent)
			if err := os.MkdirAll(filepath.Dir(agentPath), 0o755); err != nil {
				t.Fatalf("create subagents dir: %v", err)
			}
			lab.write(agentPath, []byte(
				sweepEntry("msg_sweep_agent", "03", `"tool_use"`, 77, `{"type":"tool_use","id":"toolu_sweep_cut","name":"Bash","input":{}}`)))
			lab.clock = clock.NewFake(time.UnixMilli(1790125300000))
			lab.feed(
				encode(map[string]any{"hook_event_name": "SubagentStart", "agent_id": agent, "agent_type": "gitter"}),
				encode(map[string]any{
					"hook_event_name": "PreToolUse", "cwd": cmDemoProj, "agent_id": agent, "agent_type": "gitter",
					"tool_name": "Bash", "tool_use_id": "toolu_sweep_cut", "tool_input": map[string]any{"command": "git push"},
				}),
				encode(map[string]any{"hook_event_name": edge, "reason": "other"}),
			)
			expect(t, "corrected request", lab.row(
				"SELECT output_tokens, stop_reason, context_tokens, calls, ts FROM requests WHERE request_id = 'msg_sweep_calls'"),
				map[string]any{"output_tokens": 643, "stop_reason": "tool_use", "context_tokens": 48, "calls": 1, "ts": 1790125201000})
			expect(t, "closing reply", lab.row(
				"SELECT output_tokens, stop_reason, calls, prompt_id, agent_id, session_id, pending, ts FROM requests WHERE request_id = 'msg_sweep_reply'"),
				map[string]any{
					"output_tokens": 212, "stop_reason": "end_turn", "calls": 0, "prompt_id": "prompt-sweep-1",
					"agent_id": "<nil>", "session_id": cmSessionA, "pending": 0, "ts": 1790125205000,
				})
			expect(t, "interrupted agent's request", lab.row(
				"SELECT output_tokens, agent_id, calls FROM requests WHERE request_id = 'msg_sweep_agent'"),
				map[string]any{"output_tokens": 77, "agent_id": agent, "calls": 1})
			expect(t, "interrupted agent's call", lab.call("toolu_sweep_cut"), map[string]any{"request_id": "msg_sweep_agent"})
			if n := lab.count("SELECT count(*) FROM requests WHERE request_id = 'msg_sweep_old'"); n != 0 {
				t.Errorf("rows of the reply before the session's first run = %d, want 0", n)
			}
			if strings.Contains(lab.storeText(), secret) {
				t.Errorf("the store holds the reply's text")
			}
			// The interrupted sub-agent's call has no result on disk: at SessionEnd,
			// the hooks' last chance to find one, that is said as a fault naming it.
			wantFaults := 0
			if edge == callmeter.EventSessionEnd {
				wantFaults = 1
				if n := lab.count("SELECT count(*) FROM faults WHERE tool_use_id = 'toolu_sweep_cut' AND stage = ?", callmeter.StageTranscript); n != 1 {
					t.Errorf("faults naming the call with no result = %d, want 1", n)
				}
			}
			if n := lab.count("SELECT count(*) FROM faults"); n != wantFaults {
				t.Errorf("faults = %d, want %d: %s", n, wantFaults, lab.faultsText())
			}
		})
	}
}

// TestCallmeterSessionEndReadsASettledAgentLast: a sub-agent whose
// SubagentStop came after its last start had its transcript swept by that
// stop. SessionEnd reads it again only after every agent no stop settled, and
// only while its budget lasts, so a read of it the budget cuts off is no skip;
// with time left it is read, for a stop cut off before its sweep committed.
func TestCallmeterSessionEndReadsASettledAgentLast(t *testing.T) {
	for _, budget := range []time.Duration{0, sessionEndBudget} {
		t.Run(budget.String(), func(t *testing.T) {
			saved := sessionEndBudget
			sessionEndBudget = budget
			t.Cleanup(func() { sessionEndBudget = saved })
			lab := newCallmeterLab(t)
			main := lab.transcript(cmSessionA)
			lab.write(main, []byte(sweepEntry("msg_mainreply01", "01", `"end_turn"`, 5, `{"type":"text","text":"ok"}`)))
			const settled, running = "asettled00000001", "arunning00000001"
			for _, agent := range []string{settled, running} {
				path := callmeter.SubagentTranscriptPath(main, agent)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("create subagents dir: %v", err)
				}
				lab.write(path, []byte(sweepEntry("msg_"+agent, "01", `"end_turn"`, 5, `{"type":"text","text":"done"}`)))
			}
			path := callmeter.SubagentTranscriptPath(main, settled)
			edge := func(event string) string {
				return hookPayload(t, event, map[string]any{
					"agent_id": settled, "agent_type": "gitter", "transcript_path": main, "agent_transcript_path": path,
				})
			}
			const before = 1790125140000 // 2026-09-23T00:59:00Z, before the transcripts' entries
			lab.feedAt(before, edge(callmeter.EventSubagentStart))
			lab.feedAt(before+1000, edge(callmeter.EventSubagentStop))
			lab.write(path, []byte(sweepEntry("msg_"+settled, "01", `"end_turn"`, 5, `{"type":"text","text":"done"}`)+
				sweepEntry("msg_agentlate01", "02", `"end_turn"`, 7, `{"type":"text","text":"later"}`)))
			lab.feedAt(before+2000, hookPayload(t, callmeter.EventSessionEnd, map[string]any{"reason": "other", "transcript_path": main}))
			if budget == 0 {
				// The main transcript and every agent on disk but the settled
				// one are skipped reads; the settled one is not counted.
				files, err := filepath.Glob(filepath.Join(filepath.Dir(path), "agent-*.jsonl"))
				if err != nil {
					t.Fatalf("list sub-agent transcripts: %v", err)
				}
				// ... and the main transcript's turn-end read (recoverTurnEnd).
				want := fmt.Sprintf(": %d transcript reads skipped", len(files)+1)
				if n := lab.count("SELECT count(*) FROM faults WHERE instr(error, ?) > 0", want); n != 1 {
					t.Errorf("faults naming %q = %d, want 1: %v", want, n, lab.row("SELECT stage, error FROM faults"))
				}
				return
			}
			for _, id := range []string{"msg_mainreply01", "msg_" + running, "msg_agentlate01"} {
				if n := lab.count("SELECT count(*) FROM requests WHERE request_id = ?", id); n != 1 {
					t.Errorf("request %s stored %d times, want 1", id, n)
				}
			}
			if n := lab.count("SELECT count(*) FROM faults"); n != 0 {
				t.Errorf("faults = %d, want 0: %v", n, lab.row("SELECT stage, error FROM faults"))
			}
		})
	}
}

// TestCallmeterSessionEndStopsReadingAtItsBudget: SessionEnd commits its event
// first, then reads transcripts only within sessionEndBudget; a read the budget
// cuts off is a transcript fault naming the count, never silence. Serial: it
// sets the package's budget.
func TestCallmeterSessionEndStopsReadingAtItsBudget(t *testing.T) {
	saved := sessionEndBudget
	sessionEndBudget = 0
	t.Cleanup(func() { sessionEndBudget = saved })
	lab := newCallmeterLab(t)
	main := lab.transcript(cmSessionA)
	lab.write(main, []byte(sweepEntry("msg_budget00001", "01", `"end_turn"`, 5, `{"type":"text","text":"bye"}`)))
	lab.feed(hookPayload(t, callmeter.EventSessionEnd, map[string]any{"reason": "other", "transcript_path": main}))
	expect(t, "event", lab.event(callmeter.EventSessionEnd), map[string]any{"reason": "other"})
	if n := lab.count("SELECT count(*) FROM requests"); n != 0 {
		t.Errorf("requests = %d, want 0: the budget was spent before any read", n)
	}
	if n := lab.count("SELECT count(*) FROM faults WHERE stage = ? AND error LIKE '%budget%'", callmeter.StageTranscript); n != 1 {
		t.Errorf("budget faults = %d, want 1", n)
	}
}

// toolResultLine is a transcript's user entry holding one tool_result.
func toolResultLine(id, text string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":"` + text + `","is_error":true}]}}` + "\n"
}

// TestCallmeterSessionEndWaitsForAnInFlightCallsResult: live, a headless
// session's SessionEnd read the transcript 10 ms after it started, before
// Claude Code had flushed the killed Bash call's `Exit code 137` result 50-100
// ms later, and settled nothing, silently. SessionEnd now re-reads while the
// transcript grows, for at most unfinishedSettle: a result that lands settles
// the call; one that never does is a transcript fault naming the call, its
// outcome columns left NULL, and the run ends inside sessionEndBudget.
func TestCallmeterSessionEndWaitsForAnInFlightCallsResult(t *testing.T) {
	const id = "toolu_inflight001"
	setup := func(t *testing.T) (*callmeterLab, string) {
		lab := newCallmeterLab(t)
		main := lab.transcript(cmSessionA)
		lab.write(main, []byte(sweepEntry("msg_inflight001", "01", `"tool_use"`, 5,
			`{"type":"tool_use","id":"`+id+`","name":"Bash","input":{"command":"sleep 99"}}`)+
			sweepEntry("msg_inflight002", "02", `"end_turn"`, 3, `{"type":"text","text":"ok"}`)))
		lab.feed(toolPayload(t, "PreToolUse", id, "Bash", map[string]any{"command": "sleep 99"}, map[string]any{"cwd": lab.proj}))
		return lab, main
	}
	end := func(t *testing.T, main string) string {
		return hookPayload(t, callmeter.EventSessionEnd, map[string]any{"reason": "other", "transcript_path": main})
	}
	t.Run("result lands after SessionEnd starts", func(t *testing.T) {
		lab, main := setup(t)
		appended := make(chan error, 1)
		go func() {
			time.Sleep(100 * time.Millisecond)
			file, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				appended <- err
				return
			}
			_, err = file.WriteString(toolResultLine(id, "Exit code 137"))
			appended <- errors.Join(err, file.Close())
		}()
		lab.feed(end(t, main))
		if err := <-appended; err != nil {
			t.Fatalf("append the tool_result: %v", err)
		}
		expect(t, "call", lab.call(id), map[string]any{"failed": 1, "bytes_delivered": len("Exit code 137"), "error": "Exit code 137"})
		if n := lab.count("SELECT count(*) FROM faults WHERE tool_use_id = ?", id); n != 0 {
			t.Errorf("faults naming the call = %d, want 0: its result was found", n)
		}
	})
	t.Run("result never lands", func(t *testing.T) {
		lab, main := setup(t)
		began := time.Now()
		lab.feed(end(t, main))
		took := time.Since(began)
		if took < unfinishedSettle || took >= sessionEndBudget {
			t.Errorf("SessionEnd took %v, want from unfinishedSettle %v to under sessionEndBudget %v", took, unfinishedSettle, sessionEndBudget)
		}
		expect(t, "call", lab.call(id), map[string]any{"failed": "<nil>", "bytes_delivered": "<nil>", "error": "<nil>"})
		fault := lab.row("SELECT * FROM faults WHERE tool_use_id = ?", id)
		expect(t, "fault", fault, map[string]any{"stage": callmeter.StageTranscript})
		if !strings.Contains(fault["error"], "no tool_result") || !strings.Contains(fault["error"], "left in flight") {
			t.Errorf("fault error = %q, want it to say the call has no tool_result and was left in flight", fault["error"])
		}
	})
	t.Run("Stop leaves a running call alone", func(t *testing.T) {
		lab, main := setup(t)
		began := time.Now()
		lab.feed(hookPayload(t, eventStop, map[string]any{"transcript_path": main}))
		if took := time.Since(began); took >= unfinishedSettle {
			t.Errorf("Stop took %v, want no wait: a background call may still be running", took)
		}
		if n := lab.count("SELECT count(*) FROM faults WHERE tool_use_id = ?", id); n != 0 {
			t.Errorf("faults naming the call = %d, want 0 at Stop", n)
		}
	})
}

// TestCallmeterCallTSIsTheEarliestHook: a call's ts is the earliest hook that
// saw it, in every landing order of its PreToolUse, PostToolUse and batch; a
// call Claude Code refused before any PreToolUse (here a tool name it does not
// know) has its batch alone, and carries the batch's ts.
func TestCallmeterCallTSIsTheEarliestHook(t *testing.T) {
	const id = "toolu_tsorder01"
	echo := map[string]any{"command": "echo hi"}
	type delivery struct {
		at      int64
		payload func(*testing.T, *callmeterLab) string
	}
	pre := delivery{3000, func(t *testing.T, _ *callmeterLab) string {
		return toolPayload(t, "PreToolUse", id, "Bash", echo, nil)
	}}
	post := delivery{4000, func(t *testing.T, _ *callmeterLab) string {
		return toolPayload(t, "PostToolUse", id, "Bash", echo, nil)
	}}
	batch := delivery{5000, func(t *testing.T, lab *callmeterLab) string {
		return batchPayload(t, lab.transcript(cmSessionA), id)
	}}
	for name, order := range map[string][]delivery{
		"pre post batch": {pre, post, batch}, "pre batch post": {pre, batch, post},
		"post pre batch": {post, pre, batch}, "post batch pre": {post, batch, pre},
		"batch pre post": {batch, pre, post}, "batch post pre": {batch, post, pre},
	} {
		t.Run(name, func(t *testing.T) {
			lab := newCallmeterLab(t)
			for _, d := range order {
				lab.feedAt(d.at, d.payload(t, lab))
			}
			expect(t, "call", lab.call(id), map[string]any{"ts": 3000})
		})
	}
	t.Run("refused before any PreToolUse", func(t *testing.T) {
		lab := newCallmeterLab(t)
		const refused = "toolu_tsrefused1"
		encoded, err := json.Marshal(map[string]any{
			"session_id": cmSessionA, "hook_event_name": "PostToolBatch", "transcript_path": lab.transcript(cmSessionA),
			"tool_calls": []map[string]any{{
				"tool_use_id": refused, "tool_name": "bash",
				"tool_response": "<tool_use_error>Error: No such tool available: bash</tool_use_error>",
			}},
		})
		if err != nil {
			t.Fatalf("encode PostToolBatch: %v", err)
		}
		lab.feedAt(5000, string(encoded))
		expect(t, "refused call", lab.call(refused), map[string]any{"ts": 5000, "tool": "bash"})
	})
	// A call stored before every hook set a ts gets the Stop's, the settle
	// reading its result; the earlier ts of a call that has one stands.
	t.Run("stored without a ts, settled at the Stop", func(t *testing.T) {
		lab := newCallmeterLab(t)
		const legacy, started = "toolu_tslegacy01", "toolu_tsstarted01"
		lab.feedAt(3000, toolPayload(t, "PreToolUse", started, "Bash", echo, nil))
		if err := lab.db().UpsertCall(context.Background(), callmeter.Call{
			ToolUseID: legacy, SessionID: callmeter.Ptr(cmSessionA), Tool: callmeter.Ptr("bash"),
			Source: callmeter.Ptr(callmeter.SourceHook),
		}, callmeter.Overwrite); err != nil {
			t.Fatalf("seed a call without a ts: %v", err)
		}
		main := lab.transcript(cmSessionA)
		var transcript string
		for _, id := range []string{legacy, started} {
			transcript += sweepEntry("msg_"+id, "01", `"tool_use"`, 5,
				`{"type":"tool_use","id":"`+id+`","name":"Bash","input":{"command":"echo hi"}}`) +
				`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id +
				`","content":"<tool_use_error>Error: No such tool available: bash</tool_use_error>","is_error":true}]}}` + "\n"
		}
		lab.write(main, []byte(transcript))
		stop, err := json.Marshal(map[string]any{"session_id": cmSessionA, "hook_event_name": eventStop, "transcript_path": main})
		if err != nil {
			t.Fatalf("encode Stop: %v", err)
		}
		lab.feedAt(9000, string(stop))
		expect(t, "legacy call", lab.call(legacy), map[string]any{"ts": 9000, "failed": 1})
		expect(t, "started call", lab.call(started), map[string]any{"ts": 3000, "failed": 1})
	})
}

// faultsText lists the faults rows as "stage tool_use_id error", for a failure message.
func (lab *callmeterLab) faultsText() string {
	lab.t.Helper()
	rows, err := lab.db().DB().QueryContext(lab.ctx, "SELECT stage, COALESCE(tool_use_id, ''), error FROM faults ORDER BY ts, rowid")
	if err != nil {
		lab.t.Fatalf("read faults: %v", err)
	}
	var out []string
	for rows.Next() {
		var stage, id, text string
		if err := rows.Scan(&stage, &id, &text); err != nil {
			lab.t.Fatalf("scan a fault: %v", err)
		}
		out = append(out, fmt.Sprintf("[%s %s %s]", stage, id, text))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		lab.t.Fatalf("read faults: %v", err)
	}
	return strings.Join(out, " ")
}

// TestHookTimeoutsAgreeWithHooksJSON pins hookTimeouts to the plugin's
// hooks.json: a sync hook's timeout is the table's, an async hook has none (and
// no table row), and every event's store wait ends, twice over with the reserve,
// inside the timeout Claude Code gives it.
func TestHookTimeoutsAgreeWithHooksJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "plugins", "callmeter", "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var config struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout *float64 `json:"timeout"`
				Async   bool     `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode hooks.json: %v", err)
	}
	if len(config.Hooks) == 0 {
		t.Fatal("hooks.json registers no event")
	}
	for event := range hookTimeouts {
		if _, ok := config.Hooks[event]; !ok {
			t.Errorf("hookTimeouts names %s, which hooks.json does not register", event)
		}
	}
	for event, entries := range config.Hooks {
		for _, entry := range entries {
			for _, hook := range entry.Hooks {
				timeout := asyncHookTimeout
				if hook.Timeout != nil {
					timeout = time.Duration(*hook.Timeout * float64(time.Second))
					if hook.Async {
						t.Errorf("%s: a hook with a timeout must be sync", event)
					}
					if hookTimeouts[event] != timeout {
						t.Errorf("hookTimeouts[%s] = %v, hooks.json says %v", event, hookTimeouts[event], timeout)
					}
				} else {
					if !hook.Async {
						t.Errorf("%s: a hook with no timeout must be async", event)
					}
					if _, ok := hookTimeouts[event]; ok {
						t.Errorf("hookTimeouts holds %s, which hooks.json runs async with no timeout", event)
					}
				}
				if got := 2*storeWait(event) + storeWaitReserve; got > timeout {
					t.Errorf("%s: two store waits of %v plus the %v reserve = %v, over its %v timeout",
						event, storeWait(event), storeWaitReserve, got, timeout)
				}
			}
		}
	}
}

// TestCallmeterStoreUnavailableLeavesAMissedLine: a hook whose store can take
// neither its event nor its fault (a store a newer callmeter raised the schema
// of, a store refusing every insert as a full disk would) leaves exactly one
// missed.log line `store unavailable: {class}` naming its event and session,
// never the raw error, and a report over the healed store counts that event.
func TestCallmeterStoreUnavailableLeavesAMissedLine(t *testing.T) {
	refusals := []string{"calls", "faults"}
	cases := map[string]struct {
		event, class string
		refuse, heal func(*callmeterLab)
	}{
		"newer schema": {
			event: "PostToolUse", class: callmeter.StoreClassNewerSchema,
			refuse: func(lab *callmeterLab) {
				store, err := callmeter.OpenDB(lab.ctx, lab.storePath)
				if err != nil {
					lab.t.Fatalf("create the store: %v", err)
				}
				if err := store.Close(); err != nil {
					lab.t.Fatalf("close the store: %v", err)
				}
				setUserVersion(lab.t, lab.storePath, callmeter.SchemaVersion+1)
			},
			heal: func(lab *callmeterLab) { setUserVersion(lab.t, lab.storePath, callmeter.SchemaVersion) },
		},
		"every insert refused": {
			event: "PreToolUse", class: callmeter.StoreClassOther,
			refuse: func(lab *callmeterLab) {
				for _, table := range refusals {
					if _, err := lab.db().DB().ExecContext(lab.ctx, "CREATE TRIGGER refuse_"+table+" BEFORE INSERT ON "+table+
						" BEGIN SELECT RAISE(ABORT, 'refused by the test'); END"); err != nil {
						lab.t.Fatalf("create the refusing trigger on %s: %v", table, err)
					}
				}
			},
			heal: func(lab *callmeterLab) {
				for _, table := range refusals {
					if _, err := lab.db().DB().ExecContext(lab.ctx, "DROP TRIGGER refuse_"+table); err != nil {
						lab.t.Fatalf("drop the refusing trigger on %s: %v", table, err)
					}
				}
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			lab := newCallmeterLab(t)
			c.refuse(lab)
			lab.feed(hookPayload(t, c.event, nil))
			missed, err := os.ReadFile(lab.missed)
			if err != nil {
				t.Fatalf("read missed.log: %v", err)
			}
			reason := callmeter.StoreUnavailableReason + c.class
			want := regexp.MustCompile(`^\d+\t` + c.event + `\t` + regexp.QuoteMeta(reason) + `\t` + cmSessionA + `\n$`)
			if !want.Match(missed) {
				t.Fatalf("missed.log = %q, want one line matching %s", missed, want)
			}

			c.heal(lab)
			lab.feed(hookPayload(t, callmeter.EventSessionStart, map[string]any{"source": "startup"}))
			if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = ? AND session_id = ? AND error = ?",
				callmeter.StageTerminated, cmSessionA, c.event+": "+reason); n != 1 {
				t.Errorf("terminated faults of the lost %s = %d, want 1", c.event, n)
			}
			table, err := report.Faults(lab.ctx, lab.db(), report.Filter{}, nil)
			if err != nil {
				t.Fatalf("faults report: %v", err)
			}
			if note := "1 events unrecorded: hook terminated before recording"; !slices.Contains(table.Notes, note) {
				t.Errorf("report notes = %q, want %q", table.Notes, note)
			}
		})
	}
}

// setUserVersion sets the store's schema version, a store OpenDB would refuse
// included.
func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sqlitedb.OpenReadWrite(path, time.Second)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Errorf("set user_version %d: %v", version, err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("close %s: %v", path, err)
	}
}
