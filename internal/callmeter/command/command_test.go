package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/report"
	"github.com/rezzminator/callmeter/internal/paths"
)

// mapEnv is a Getenv over a fixed map.
func mapEnv(env map[string]string) paths.Getenv {
	return func(name string) string { return env[name] }
}

// lab is a scratch state home and a scratch user home with its own seat.
type lab struct {
	getenv    paths.Getenv
	userHome  string
	seat      string // the user's own Claude Code dir, $HOME/.claude
	storePath string
}

func newLab(t *testing.T) lab {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userHome := filepath.Join(root, "user")
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return lab{
		getenv:    mapEnv(map[string]string{paths.EnvHome: state, "HOME": userHome}),
		userHome:  userHome,
		seat:      filepath.Join(userHome, ".claude"),
		storePath: paths.Store(state),
	}
}

func (fixture lab) run(args ...string) (code int, stdout, stderr string) {
	var out, errs bytes.Buffer
	code = CLI(args, &out, &errs, fixture.getenv)
	return code, out.String(), errs.String()
}

// seedRead records one Read call of file in session, run from seat.
func (fixture lab) seedRead(t *testing.T, id, session, file, seat string) {
	t.Helper()
	ctx := context.Background()
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	call := callmeter.Call{
		ToolUseID: id, SessionID: callmeter.Ptr(session), AgentID: callmeter.Ptr(""),
		TS: callmeter.Ptr(time.Now().UnixMilli()), Tool: callmeter.Ptr("Read"),
		Cwd: callmeter.Ptr(filepath.Dir(file)), FilePath: callmeter.Ptr(file),
		// failed=0 is what PostToolUse sets on a Read that ran; with it NULL the row
		// reads as PostToolBatch-only and the report names it in a note.
		Failed:         callmeter.Ptr(false),
		BytesDelivered: callmeter.Ptr(int64(1234)), Source: callmeter.Ptr(callmeter.SourceHook),
		SeatDir: callmeter.Ptr(seat),
	}
	if err := store.UpsertCall(ctx, call, callmeter.Overwrite); err != nil {
		t.Fatalf("seed call: %v", err)
	}
}

func TestCallmeterCLIReportOverASeededStoreRendersTheTable(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	code, stdout, stderr := fixture.run("report", "files")
	if code != 0 || stderr != "" {
		t.Fatalf("report files = %d, want 0 and a quiet stderr\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	title, _, _ := strings.Cut(stdout, "\n")
	if !strings.HasPrefix(title, "callmeter files") || !strings.Contains(title, "window: since ") ||
		!strings.Contains(stdout, "/work/one.md") {
		t.Fatalf("report files stdout = %q, want a title naming the window and the seeded file", stdout)
	}
}

func TestCallmeterCLIReportWithoutStoreSaysSoAndCreatesNothing(t *testing.T) {
	fixture := newLab(t)
	code, stdout, stderr := fixture.run("report", "files")
	want := "callmeter: no store at " + fixture.storePath + ": nothing recorded yet"
	if code != 0 || !strings.Contains(stdout, want) {
		t.Fatalf("report without store = %d, want 0 and %q\nstdout:\n%s\nstderr:\n%s", code, want, stdout, stderr)
	}
	if _, err := os.Stat(fixture.storePath); !os.IsNotExist(err) {
		t.Fatalf("report created the store %s (stat err %v)", fixture.storePath, err)
	}
}

func TestCallmeterCLIReportUnreadableStoreFails(t *testing.T) {
	for name, stage := range map[string]func(path string) error{
		"directory": func(path string) error { return os.MkdirAll(path, 0o700) },
		"garbage": func(path string) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return os.WriteFile(path, bytes.Repeat([]byte("not a database "), 512), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newLab(t)
			if err := stage(fixture.storePath); err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := fixture.run("report", "files")
			if code != 1 || !strings.Contains(stderr, "callmeter: cannot open store "+fixture.storePath) {
				t.Fatalf("report over a %s store = %d\nstdout:\n%s\nstderr:\n%s", name, code, stdout, stderr)
			}
		})
	}
}

func TestCallmeterCLIUnknownTopicOrActionPrintsUsage(t *testing.T) {
	fixture := newLab(t)
	for _, args := range [][]string{{"report", "bogus"}, {"report"}, {"bogus"}, {"report", "files", "--bogus"}, {}} {
		code, _, stderr := fixture.run(args...)
		if code != 2 || !strings.Contains(stderr, "usage: callmeter report") {
			t.Fatalf("callmeter %q = %d, want 2 with usage\nstderr:\n%s", args, code, stderr)
		}
	}
}

// TestCallmeterCLIFlagsMayFollowTheTopic: a flag after the topic is read, a
// bare -- ends flag parsing so a later flag-looking word is a second topic
// word and a usage error.
func TestCallmeterCLIFlagsMayFollowTheTopic(t *testing.T) {
	fixture := newLab(t)
	for index := range 6 {
		fixture.seedRead(t, fmt.Sprintf("toolu_%d", index), "sess-1", fmt.Sprintf("/work/f%d.md", index), fixture.seat)
	}
	code, stdout, stderr := fixture.run("report", "files", "--limit", "3")
	if code != 0 {
		t.Fatalf("report files --limit 3 = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if rows := strings.Count(stdout, "/work/f"); rows != 3 {
		t.Fatalf("report files --limit 3 printed %d file rows, want 3\nstdout:\n%s", rows, stdout)
	}
	code, stdout, stderr = fixture.run("report", "--limit", "2", "files")
	if code != 0 || strings.Count(stdout, "/work/f") != 2 {
		t.Fatalf("report --limit 2 files = %d, want 2 rows\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	code, _, stderr = fixture.run("report", "files", "--", "--limit", "3")
	if code != 2 || !strings.Contains(stderr, "want one topic") {
		t.Fatalf("report files -- --limit 3 = %d, want 2 for the extra words\nstderr:\n%s", code, stderr)
	}
}

// TestCallmeterCLINamesAChatFromItsTranscript: the --session title carries the
// chat name read from the transcript under the seat the store recorded.
func TestCallmeterCLINamesAChatFromItsTranscript(t *testing.T) {
	fixture := newLab(t)
	otherSeat := filepath.Join(filepath.Dir(fixture.userHome), "seat-two")
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", otherSeat)
	writeTranscript(t, otherSeat, "-work", "sess-1",
		`{"type":"ai-title","aiTitle":"generated name","sessionId":"sess-1"}`,
		`{"type":"custom-title","customTitle":"chosen name","sessionId":"sess-1"}`)
	code, stdout, stderr := fixture.run("report", "files", "--session", "sess-1")
	title, _, _ := strings.Cut(stdout, "\n")
	if code != 0 || !strings.Contains(title, "session=sess-1 (chosen name)") || strings.Contains(stdout, "note:") {
		t.Fatalf("report files --session = %d, want the chat name in the title and no note\nstdout:\n%s\nstderr:\n%s",
			code, stdout, stderr)
	}
}

// TestCallmeterCLIUnreadableTranscriptShowsAQuestionMarkAndOneNote: a
// transcript that exists but cannot be read is a "?" chat and one note, never
// a quiet fallback to the session id.
func TestCallmeterCLIUnreadableTranscriptShowsAQuestionMarkAndOneNote(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	unreadable := filepath.Join(fixture.seat, "projects", "-work", "sess-1.jsonl")
	if err := os.MkdirAll(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := fixture.run("report", "files", "--session", "sess-1")
	title, _, _ := strings.Cut(stdout, "\n")
	stderrLines := strings.Split(strings.TrimSpace(stderr), "\n")
	if code != 0 || !strings.Contains(title, "session=sess-1 (?)") ||
		strings.Count(stdout, "note: chat names could not be read for 1 sessions (first: unreadable)") != 1 ||
		strings.Contains(stdout, unreadable) || len(stderrLines) != 1 ||
		!strings.HasPrefix(stderrLines[0], "callmeter: chat name: session sess-1: ") ||
		!strings.Contains(stderrLines[0], unreadable) {
		t.Fatalf("report files --session = %d, want a ? chat and one note\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// allTopics are the 21 report topics: the six ported, the eight of 5-b, the six
// over the transcript metrics, then cache.
var allTopics = []string{
	"files", "writes", "commands", "context", "sequences", "faults",
	"sessions", "prompts", "effort", "tokens", "agents", "outcomes", "coverage", "events",
	"compactions", "cost", "hooks", "turns", "resumes", "waiting", "cache",
}

// seedEverything records something every one of the new topics reports, and an
// unrecorded transcript under the user's own seat for coverage, all within the
// last hours of the real clock.
func (fixture lab) seedEverything(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	ts := time.Now().Add(-time.Hour).UnixMilli()
	call := callmeter.Call{
		ToolUseID: "toolu_2", SessionID: callmeter.Ptr("sess-1"), AgentID: callmeter.Ptr("a1"), AgentType: callmeter.Ptr("Explore"),
		PromptID: callmeter.Ptr("p1"), TS: callmeter.Ptr(ts), Tool: callmeter.Ptr("Bash"), Cwd: callmeter.Ptr("/work"),
		Effort: callmeter.Ptr("high"), PermissionMode: callmeter.Ptr("default"), Failed: callmeter.Ptr(false),
		CommitSHA: callmeter.Ptr("abc1234"), CommitBranch: callmeter.Ptr("main"), TestRunner: callmeter.Ptr("go test"),
		SeatDir: callmeter.Ptr(fixture.seat),
	}
	if err := store.UpsertCall(ctx, call, callmeter.Overwrite); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	if err := store.UpsertRequest(ctx, callmeter.Request{
		RequestID: "q1", SessionID: callmeter.Ptr("sess-1"), PromptID: callmeter.Ptr("p1"), TS: callmeter.Ptr(ts),
		Model: callmeter.Ptr("opus"), InputTokens: callmeter.Ptr(int64(10)), CacheReadTokens: callmeter.Ptr(int64(90)),
		CacheCreationTokens: callmeter.Ptr(int64(0)), ContextTokens: callmeter.Ptr(int64(100)), OutputTokens: callmeter.Ptr(int64(5)),
	}, callmeter.Overwrite); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	if err := store.Batch(ctx, func(tx *callmeter.Tx) error {
		for _, event := range []callmeter.Event{
			{EventID: "ev1", Event: callmeter.EventSessionStart, TS: ts, SessionID: callmeter.Ptr("sess-1"), Source: callmeter.Ptr("startup")},
			{EventID: "ev2", Event: callmeter.EventSubagentStart, TS: ts, SessionID: callmeter.Ptr("sess-1"),
				AgentID: callmeter.Ptr("a1"), AgentType: callmeter.Ptr("Explore")},
		} {
			if _, err := tx.InsertEvent(ctx, event); err != nil {
				return err
			}
		}
		if err := tx.RebuildAgentTurns(ctx, "a1"); err != nil {
			return err
		}
		if _, err := tx.InsertTurn(ctx, callmeter.Turn{
			EventID: "tu1", Event: "Stop", TS: ts, SessionID: callmeter.Ptr("sess-1"), Effort: callmeter.Ptr("high"),
		}); err != nil {
			return err
		}
		if err := tx.TouchSession(ctx, callmeter.Session{SessionID: "sess-1", TS: ts, Cwd: callmeter.Ptr("/work")}); err != nil {
			return err
		}
		return tx.RefreshSession(ctx, "sess-1")
	}); err != nil {
		t.Fatalf("seed lifecycle rows: %v", err)
	}
	// rows of the transcript-metrics tables, by plain SQL: the topics read what is stored
	for _, statement := range []string{
		`INSERT INTO compactions (entry_id, session_id, ts, trigger, pre_tokens, post_tokens) VALUES ('cp1', 'sess-1', {ts}, 'auto', 9000, 1000)`,
		`INSERT INTO session_costs (session_id, ts, cost_usd) VALUES ('sess-1', {ts}, 1.5)`,
		`INSERT INTO stop_hooks (entry_id, session_id, ts, hook_count, hook_errors) VALUES ('sh1', 'sess-1', {ts}, 1, 0)`,
		`INSERT INTO stop_hook_runs (entry_id, seq, name, command_bytes, duration_ms) VALUES ('sh1', 0, 'lint.sh', 20, 250)`,
		`INSERT INTO turn_durations (entry_id, session_id, prompt_id, ts, duration_ms) VALUES ('td1', 'sess-1', 'p1', {ts}, 4000)`,
		`INSERT INTO events (event_id, event, ts, session_id, source, detail) VALUES ('rs1', 'SessionStart', {ts}, 'sess-1', 'resume', '{}')`,
		`INSERT INTO events (event_id, event, ts, session_id, detail) VALUES ('nt1', 'Notification', {ts}, 'sess-1', '{"notification_type":"idle_prompt"}')`,
	} {
		if _, err := store.DB().ExecContext(ctx, strings.ReplaceAll(statement, "{ts}", fmt.Sprint(ts))); err != nil {
			t.Fatalf("seed %s: %v", statement, err)
		}
	}
	writeTranscript(t, fixture.seat, "-work", "sess-unrecorded", `{"type":"user"}`)
}

// jsonReport is the --json object of a report over a store that is there.
type jsonReport struct {
	Topic   string     `json:"topic"`
	Store   string     `json:"store"`
	Title   string     `json:"title"`
	Path    string     `json:"path"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	Notes   []string   `json:"notes"`
}

func decodeReport(t *testing.T, stdout string) jsonReport {
	t.Helper()
	var got jsonReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, stdout)
	}
	return got
}

// squash is s with every run of whitespace one space.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestCallmeterCLIReportJSONOnEveryTopicMatchesTheText: --json parses with
// encoding/json, carries the topic and the title the text prints, and its
// columns are the text header.
func TestCallmeterCLIReportJSONOnEveryTopicMatchesTheText(t *testing.T) {
	fixture := newLab(t)
	fixture.seedEverything(t)
	for _, topic := range allTopics {
		code, text, stderr := fixture.run("report", topic)
		if code != 0 {
			t.Fatalf("report %s = %d\nstderr:\n%s", topic, code, stderr)
		}
		code, stdout, stderr := fixture.run("report", topic, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("report %s --json = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		got := decodeReport(t, stdout)
		lines := strings.Split(text, "\n")
		if got.Topic != topic || got.Store != "present" || got.Title != lines[0] || len(got.Columns) == 0 || got.Rows == nil || got.Notes == nil {
			t.Errorf("report %s --json = %+v, want the topic, store present, the text title %q, columns and arrays", topic, got, lines[0])
		}
		if len(got.Rows) == 0 {
			continue
		}
		if squash(strings.Join(got.Columns, " ")) != squash(lines[1]) {
			t.Errorf("report %s columns %q differ from the text header %q", topic, got.Columns, lines[1])
		}
		if len(got.Rows[0]) != len(got.Columns) {
			t.Errorf("report %s row %q has %d cells for %d columns", topic, got.Rows[0], len(got.Rows[0]), len(got.Columns))
		}
	}
}

// TestCallmeterCLIReportEveryNewTopicShowsItsSeededRows: the fifteen new topics
// each print the rows the seeded store holds.
func TestCallmeterCLIReportEveryNewTopicShowsItsSeededRows(t *testing.T) {
	fixture := newLab(t)
	fixture.seedEverything(t)
	for _, topic := range allTopics[6:] {
		code, stdout, stderr := fixture.run("report", topic, "--json")
		if code != 0 {
			t.Fatalf("report %s --json = %d\nstderr:\n%s", topic, code, stderr)
		}
		if got := decodeReport(t, stdout); len(got.Rows) == 0 {
			t.Errorf("report %s has no rows over the seeded store\n%s", topic, stdout)
		}
	}
}

func TestCallmeterCLIReportJSONWithoutStoreIsAbsentAndCreatesNothing(t *testing.T) {
	fixture := newLab(t)
	for _, topic := range allTopics {
		code, stdout, stderr := fixture.run("report", topic, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("report %s --json without a store = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		got := decodeReport(t, stdout)
		if got.Topic != topic || got.Store != "absent" || got.Path != fixture.storePath || got.Title != "" ||
			got.Columns == nil || len(got.Columns) != 0 || got.Rows == nil || len(got.Rows) != 0 || got.Notes == nil || len(got.Notes) != 0 {
			t.Errorf("report %s --json without a store = %+v, want absent, the path and three empty arrays", topic, got)
		}
	}
	if _, err := os.Stat(fixture.storePath); !os.IsNotExist(err) {
		t.Fatalf("report created the store %s (stat err %v)", fixture.storePath, err)
	}
}

// TestCallmeterCLIReportWithoutStoreCountsMissedLines: with no store, the lines
// of missed.log and of every claim missed.log.ingest-* are lost events, named
// in one failure note grouped as the store's own note groups them, never
// "nothing recorded yet"; exit 0, no store created, no file moved or changed.
// An ingested claim missed.log.done-* is not counted: its lines went into a
// store. The lines are derived from the wrapper's and the binary's own shapes.
func TestCallmeterCLIReportWithoutStoreCountsMissedLines(t *testing.T) {
	const session = "0b5e1c2a-7d4f-4e8b-9a61-3f2c8d9e0a17"
	for name, tc := range map[string]struct {
		files map[string]string // file name under CALLMETER_HOME → content
		note  string
	}{
		"missed.log only": {
			files: map[string]string{
				"missed.log": "1700000000\tStop\tdownload failed\n" +
					"1700000001\tPreToolUse\tdownload failed\t" + session + "\n" +
					"1700000002\tPostToolUse\tstore unavailable: open\t" + session + "\n" +
					"not a missed line\n",
			},
			note: "4 events unrecorded: binary unavailable: 3, hook terminated before recording: 1",
		},
		"with a claim": {
			files: map[string]string{
				"missed.log": "1700000003\tStop\tlock wait timed out\n",
				"missed.log.ingest-4242": "1700000000\tSessionStart\tdownload failed\n" +
					"1700000001\tPostToolUse\tterminated by SIGTERM\t" + session + "\n" +
					"1700000002\tStop\tpanic\t" + session + "\n",
				"missed.log.done-40-4241": "1699999999\tStop\tdownload failed\n",
			},
			note: "4 events unrecorded: binary unavailable: 2, hook terminated before recording: 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newLab(t)
			home := filepath.Dir(fixture.storePath)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			for file, content := range tc.files {
				if err := os.WriteFile(filepath.Join(home, file), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, stdout, stderr := fixture.run("report", "files")
			want := "callmeter: no store at " + fixture.storePath + "\nnote: " + tc.note + "\n"
			if code != 0 || stderr != "" || stdout != want || strings.Contains(stdout, "nothing recorded yet") {
				t.Errorf("report files without a store = %d, want 0\nstdout:\n%s\nwant:\n%s\nstderr:\n%s", code, stdout, want, stderr)
			}
			code, stdout, stderr = fixture.run("report", "files", "--json")
			if code != 0 || stderr != "" || strings.Contains(stdout, "nothing recorded yet") {
				t.Fatalf("report files --json without a store = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			got := decodeReport(t, stdout)
			if got.Store != "absent" || got.Path != fixture.storePath || len(got.Columns) != 0 || len(got.Rows) != 0 ||
				!slices.Equal(got.Notes, []string{tc.note}) {
				t.Errorf("report files --json without a store = %+v, want absent and the note %q", got, tc.note)
			}
			if _, err := os.Stat(fixture.storePath); !os.IsNotExist(err) {
				t.Errorf("report created the store %s (stat err %v)", fixture.storePath, err)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(tc.files) {
				t.Errorf("CALLMETER_HOME holds %d entries after the report, want the %d staged", len(entries), len(tc.files))
			}
			for file, content := range tc.files {
				if data, err := os.ReadFile(filepath.Join(home, file)); err != nil || string(data) != content {
					t.Errorf("report changed %s (err %v)", file, err)
				}
			}
		})
	}
}

func TestCallmeterCLIReportEmptyWindowIsOneLineAndEmptyRows(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	for topic, empty := range map[string]string{
		"prompts":     "callmeter: no prompts in window",
		"outcomes":    report.EmptyLine,
		"events":      "callmeter: no events in window",
		"agents":      "callmeter: no sub-agents in window",
		"tokens":      "callmeter: no requests in window",
		"compactions": "callmeter: no compactions in window",
		"cost":        "callmeter: no cost-state recorded in window",
		"hooks":       "callmeter: no Stop hook summaries in window",
		"turns":       "callmeter: no turn durations in window",
		"resumes":     "callmeter: no resumes in window",
		"waiting":     "callmeter: no waits in window",
	} {
		code, stdout, stderr := fixture.run("report", topic)
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if code != 0 || len(lines) < 2 || lines[1] != empty {
			t.Errorf("report %s over an empty window = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		code, stdout, stderr = fixture.run("report", topic, "--json")
		if got := decodeReport(t, stdout); code != 0 || got.Rows == nil || len(got.Rows) != 0 || !strings.Contains(stdout, `"rows":[]`) {
			t.Errorf("report %s --json over an empty window = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
	}
}

func TestCallmeterCLIUnknownTopicListsAllTwentyOne(t *testing.T) {
	fixture := newLab(t)
	code, stdout, stderr := fixture.run("report", "nope")
	if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "callmeter report: want one topic") {
		t.Fatalf("report nope = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	usage, _, _ := strings.Cut(strings.TrimPrefix(stderr[strings.Index(stderr, "usage:"):], "usage: "), "\n")
	for _, topic := range allTopics {
		if !strings.Contains(usage, topic) {
			t.Errorf("usage first line %q lacks the topic %s", usage, topic)
		}
	}
	if !strings.Contains(stderr, "--json") {
		t.Errorf("usage lacks --json:\n%s", stderr)
	}
}

// TestCallmeterCLIFiltersNarrowEveryTopicOrSayTheyCannot: all five flags on
// every topic exit 0, print at most 3 rows, and name the filter a topic
// cannot apply.
func TestCallmeterCLIFiltersNarrowEveryTopicOrSayTheyCannot(t *testing.T) {
	fixture := newLab(t)
	fixture.seedEverything(t)
	cannot := map[string][]string{
		"faults":      {"--project does not apply to faults", "--agent-type does not apply to faults"},
		"sessions":    {"--agent-type does not apply to sessions"},
		"coverage":    {"--project does not apply to coverage", "--agent-type does not apply to coverage"},
		"compactions": {"--agent-type does not apply to compactions"},
		"cost":        {"--agent-type does not apply to cost"},
		"hooks":       {"--agent-type does not apply to hooks"},
		"turns":       {"--agent-type does not apply to turns"},
		"resumes":     {"--agent-type does not apply to resumes"},
		"waiting":     {"--agent-type does not apply to waiting"},
	}
	for _, topic := range allTopics {
		code, stdout, stderr := fixture.run("report", topic, "--since", "7d", "--project", "/work", "--session", "sess-1",
			"--agent-type", "Explore", "--limit", "3", "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("report %s with every filter = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		got := decodeReport(t, stdout)
		// faults prints its fixed stage rows beside the latest --limit faults.
		if len(got.Rows) > 3 && topic != "faults" {
			t.Errorf("report %s --limit 3 printed %d rows", topic, len(got.Rows))
		}
		for _, note := range cannot[topic] {
			if !slices.Contains(got.Notes, note) {
				t.Errorf("report %s notes = %q, want %q", topic, got.Notes, note)
			}
		}
		if len(cannot[topic]) == 0 {
			for _, note := range got.Notes {
				if strings.Contains(note, "does not apply") {
					t.Errorf("report %s says %q, but applies every flag", topic, note)
				}
			}
		}
	}
	code, stdout, _ := fixture.run("report", "sessions", "--agent-type", "Explore")
	if code != 0 || !strings.Contains(stdout, "note: --agent-type does not apply to sessions\n") {
		t.Errorf("text report sessions --agent-type = %d, want the note line\n%s", code, stdout)
	}
}

// TestCallmeterCLIReportIngestsMissedLogBeforeEveryTopic: a missed.log present
// when a report runs is turned into faults first, so the note counts it on all
// 21 topics, and the file is consumed.
func TestCallmeterCLIReportIngestsMissedLogBeforeEveryTopic(t *testing.T) {
	fixture := newLab(t)
	fixture.seedEverything(t)
	missed := paths.Missed(filepath.Dir(fixture.storePath))
	now := time.Now().Unix()
	line := fmt.Sprintf("%d\tStop\tno binary\n%d\tunknown\tdownload failed\n", now, now)
	for index, topic := range allTopics {
		if index > 0 {
			// the second run finds nothing to ingest: only the first run's two faults exist
			line = ""
		}
		if line != "" {
			if err := os.WriteFile(missed, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, stdout, stderr := fixture.run("report", topic, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("report %s = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		if got := decodeReport(t, stdout); !slices.Contains(got.Notes, "2 events unrecorded: binary unavailable") {
			t.Errorf("report %s notes = %q, want the two missed events counted", topic, got.Notes)
		}
		if _, err := os.Stat(missed); !os.IsNotExist(err) {
			t.Errorf("report %s left missed.log behind (stat err %v)", topic, err)
		}
	}
}

func TestCallmeterCLIReportMissedLogFailureIsAStderrLineAndExit1(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	missed := paths.Missed(filepath.Dir(fixture.storePath))
	// A leftover claim that is a dangling link: listed for ingest, unreadable.
	if err := os.Symlink(filepath.Join(filepath.Dir(missed), "nowhere"), missed+".ingest-1"); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := fixture.run("report", "files")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "callmeter: ingest missed.log: ") {
		t.Fatalf("report with an unreadable missed.log = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// seedQuiet records a session whose hooks stopped after one PreToolUse two
// hours ago, with a transcript holding lines, last written two hours ago too.
func (fixture lab) seedQuiet(t *testing.T, session, toolUseID, transcript string, lines ...string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	quiet := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(transcript, quiet, quiet); err != nil {
		t.Fatal(err)
	}
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	err = store.Batch(ctx, func(tx *callmeter.Tx) error {
		if err := tx.TouchSession(ctx, callmeter.Session{
			SessionID: session, TS: quiet.Add(-time.Minute).UnixMilli(), TranscriptPath: callmeter.Ptr(transcript),
		}); err != nil {
			return err
		}
		return tx.UpsertCall(ctx, callmeter.Call{
			ToolUseID: toolUseID, SessionID: callmeter.Ptr(session), TS: callmeter.Ptr(quiet.UnixMilli()),
			Tool: callmeter.Ptr("Bash"), Source: callmeter.Ptr(callmeter.SourceHook),
		}, callmeter.Overwrite)
	})
	if err != nil {
		t.Fatalf("seed quiet session: %v", err)
	}
}

// callRow reads the delivered size and request of one call from the store.
func (fixture lab) callRow(t *testing.T, toolUseID string) (delivered, request *string) {
	t.Helper()
	ctx := context.Background()
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	if err := store.DB().QueryRowContext(ctx,
		"SELECT CAST(bytes_delivered AS TEXT), request_id FROM calls WHERE tool_use_id = ?", toolUseID).Scan(&delivered, &request); err != nil {
		t.Fatalf("read call %s: %v", toolUseID, err)
	}
	return delivered, request
}

// TestCallmeterCLIReportSettlesAQuietSessionFromItsTranscript: a session whose
// hooks stopped an hour or more ago with a call in flight is settled by the
// report that reads the store, before the topic runs.
func TestCallmeterCLIReportSettlesAQuietSessionFromItsTranscript(t *testing.T) {
	fixture := newLab(t)
	transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-quiet.jsonl")
	at := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	fixture.seedQuiet(t, "sess-quiet", "toolu_quiet", transcript,
		`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_quiet","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_quiet","name":"Bash","input":{"command":"wc -l notes.md","description":"Count lines"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":10789,"cache_read_input_tokens":13689,"output_tokens":276}},"requestId":"req_demo_1","timestamp":"`+at+`","cwd":"/tmp/demo-proj","sessionId":"sess-quiet"}`,
		`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_quiet","type":"tool_result","content":"1\tnotes\n"}]},"timestamp":"`+at+`","cwd":"/tmp/demo-proj","sessionId":"sess-quiet"}`,
	)
	if delivered, _ := fixture.callRow(t, "toolu_quiet"); delivered != nil {
		t.Fatalf("seeded call bytes_delivered = %q, want NULL before the report", *delivered)
	}
	code, stdout, stderr := fixture.run("report", "sessions")
	if code != 0 || stderr != "" {
		t.Fatalf("report sessions = %d, want 0 and a quiet stderr\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	delivered, request := fixture.callRow(t, "toolu_quiet")
	if delivered == nil || *delivered != "8" || request == nil || *request != "msg_quiet" {
		t.Errorf("settled call = bytes_delivered %v request_id %v, want 8 and msg_quiet", delivered, request)
	}
}

// TestCallmeterCLIReportNamesAnUnreadableQuietTranscriptAndGoesOn: a transcript
// recovery cannot read is one stderr line naming the session and the path; the
// report still runs.
func TestCallmeterCLIReportNamesAnUnreadableQuietTranscriptAndGoesOn(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-bad.jsonl")
	fixture.seedQuiet(t, "sess-bad", "toolu_bad", transcript, `{"type":"user","message":{"content":[{"type":"tool_result" toolu_bad`)
	code, stdout, stderr := fixture.run("report", "files")
	want := "callmeter: recover quiet sessions: skipped: session sess-bad: " + transcript
	if code != 0 || !strings.Contains(stderr, want) || !strings.Contains(stdout, "/work/one.md") {
		t.Fatalf("report files = %d, want 0, the skip line %q and the table\nstdout:\n%s\nstderr:\n%s", code, want, stdout, stderr)
	}
}

// TestCallmeterCLIReportQuietAfterFromTheEnvironment: CALLMETER_QUIET_AFTER, a
// Go duration, replaces QuietAfter for one report run (the e2e's seam): a
// session quiet for two hours is left alone under 3h; a value that is not a
// non-negative duration is a usage error naming it, and the store is not read.
func TestCallmeterCLIReportQuietAfterFromTheEnvironment(t *testing.T) {
	transcriptLines := func(at string) []string {
		return []string{
			`{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_quiet","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_quiet","name":"Bash","input":{"command":"wc -l notes.md","description":"Count lines"}}],"usage":{"input_tokens":10,"cache_creation_input_tokens":10789,"cache_read_input_tokens":13689,"output_tokens":276}},"requestId":"req_demo_1","timestamp":"` + at + `","cwd":"/tmp/demo-proj","sessionId":"sess-quiet"}`,
			`{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_quiet","type":"tool_result","content":"1\tnotes\n"}]},"timestamp":"` + at + `","cwd":"/tmp/demo-proj","sessionId":"sess-quiet"}`,
		}
	}
	at := time.Now().Add(-2 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	withQuiet := func(fixture lab, value string) lab {
		base := fixture.getenv
		fixture.getenv = func(key string) string {
			if key == "CALLMETER_QUIET_AFTER" {
				return value
			}
			return base(key)
		}
		return fixture
	}
	t.Run("longer than the quiet", func(t *testing.T) {
		fixture := newLab(t)
		transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-quiet.jsonl")
		fixture.seedQuiet(t, "sess-quiet", "toolu_quiet", transcript, transcriptLines(at)...)
		code, stdout, stderr := withQuiet(fixture, "3h").run("report", "sessions")
		if code != 0 || stderr != "" {
			t.Fatalf("report sessions = %d, want 0 and a quiet stderr\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if delivered, _ := fixture.callRow(t, "toolu_quiet"); delivered != nil {
			t.Errorf("call bytes_delivered = %q, want NULL: two hours quiet is not 3h", *delivered)
		}
	})
	for _, value := range []string{"soon", "-1m"} {
		t.Run("invalid "+value, func(t *testing.T) {
			fixture := newLab(t)
			transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-quiet.jsonl")
			fixture.seedQuiet(t, "sess-quiet", "toolu_quiet", transcript, transcriptLines(at)...)
			code, stdout, stderr := withQuiet(fixture, value).run("report", "sessions")
			if code != 2 || stdout != "" || !strings.Contains(stderr, "CALLMETER_QUIET_AFTER") || !strings.Contains(stderr, value) {
				t.Fatalf("report sessions with CALLMETER_QUIET_AFTER=%s = %d, want 2 and a stderr naming it\nstdout:\n%s\nstderr:\n%s", value, code, stdout, stderr)
			}
			if delivered, _ := fixture.callRow(t, "toolu_quiet"); delivered != nil {
				t.Errorf("call bytes_delivered = %q, want NULL: a usage error reads nothing", *delivered)
			}
		})
	}
}

// seedStoredCall writes one call row of tool holding input and errText as
// stored, in the window, failed when errText is set.
func (fixture lab) seedStoredCall(t *testing.T, id, tool, input, errText string) {
	t.Helper()
	ctx := context.Background()
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	call := callmeter.Call{
		ToolUseID: id, SessionID: callmeter.Ptr("sess-old"), AgentID: callmeter.Ptr(""),
		TS: callmeter.Ptr(time.Now().UnixMilli()), Tool: callmeter.Ptr(tool), Cwd: callmeter.Ptr("/work"),
		Input: callmeter.Ptr(input), Failed: callmeter.Ptr(errText != ""), BytesDelivered: callmeter.Ptr(int64(10)),
		Source: callmeter.Ptr(callmeter.SourceHook), SeatDir: callmeter.Ptr(fixture.seat),
	}
	if errText != "" {
		call.Error = callmeter.Ptr(errText)
	}
	if err := store.UpsertCall(ctx, call, callmeter.Overwrite); err != nil {
		t.Fatalf("seed call %s: %v", id, err)
	}
}

// olderRulesNote is the note every topic prints for n calls Redact would rewrite.
func olderRulesNote(n int) string {
	return fmt.Sprintf("%d calls hold text stored under older privacy rules; run `callmeter redact` to scrub them", n)
}

// TestCallmeterCLIReportNotesCallsStoredUnderOlderPrivacyRules: calls stored
// before a privacy rule (a text description, a multi-line error, a kept field
// holding an object, a commit message, an MCP tool's labels, a url's query)
// are counted by one note on every topic, text and --json, never quoted;
// `callmeter redact` rewrites every one of them and the note goes.
func TestCallmeterCLIReportNotesCallsStoredUnderOlderPrivacyRules(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_ok", "sess-1", "/work/one.md", fixture.seat)
	secrets := []string{"LISTDESC", "STDERRLINE", "NESTEDSECRET", "COMMITMSG", "MAILTO", "TOKENQ", "FRAGQ"}
	fixture.seedStoredCall(t, "toolu_desc", "Bash", `{"command":"ls","description":"LISTDESC files"}`, "Exit code 1\nSTDERRLINE")
	fixture.seedStoredCall(t, "toolu_b4", "Read", `{"file_path":{"x":"NESTEDSECRET"}}`, "")
	fixture.seedStoredCall(t, "toolu_b5", "Bash", `{"command":"git commit -m \"COMMITMSG\""}`, "")
	fixture.seedStoredCall(t, "toolu_b7", "mcp__mail__send", `{"to":"MAILTO@example.com","type":"plain"}`, "")
	fixture.seedStoredCall(t, "toolu_url", "WebFetch", `{"url":"https://x.example/cb?t=TOKENQ#FRAGQ"}`, "")
	want := olderRulesNote(5)
	for _, topic := range allTopics {
		code, stdout, stderr := fixture.run("report", topic)
		if code != 0 || !strings.Contains(stdout, "note: "+want+"\n") || strings.Count(stdout, "older privacy rules") != 1 {
			t.Errorf("report %s = %d, want one note %q\nstdout:\n%s\nstderr:\n%s", topic, code, want, stdout, stderr)
		}
		code, stdout, _ = fixture.run("report", topic, "--json")
		if got := decodeReport(t, stdout); code != 0 || !slices.Contains(got.Notes, want) {
			t.Errorf("report %s --json notes = %q, want %q", topic, got.Notes, want)
		}
	}
	var out, errs bytes.Buffer
	code := Redact([]string{"redact"}, &out, &errs, fixture.getenv)
	stdout, stderr := out.String(), errs.String()
	if code != 0 || !strings.Contains(stdout, "calls.input     5 ") || !strings.Contains(stdout, "calls.error     1 ") {
		t.Fatalf("redact = %d, want 5 inputs and 1 error rewritten\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	for _, topic := range allTopics {
		if _, stdout, _ := fixture.run("report", topic); strings.Contains(stdout, "older privacy rules") {
			t.Errorf("report %s after redact still notes older rows:\n%s", topic, stdout)
		}
	}
	ctx := context.Background()
	store, err := callmeter.OpenDB(ctx, fixture.storePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	inputs := map[string]string{}
	rows, err := store.DB().QueryContext(ctx, `SELECT tool_use_id, COALESCE(input, '') || '|' || COALESCE(error, '') FROM calls`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, stored string
		if err := rows.Scan(&id, &stored); err != nil {
			t.Fatal(err)
		}
		inputs[id] = stored
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	wantStored := map[string]string{
		"toolu_desc": `{"command":"ls","description_bytes":14}|Exit code 1`,
		"toolu_b4":   `{"file_path_bytes":20}|`,
		"toolu_b5":   `{"command":"git commit -m '[cut]'","operand_bytes":11}|`,
		"toolu_b7":   `{"to_bytes":18,"type_bytes":5}|`,
		"toolu_url":  `{"url":"https://x.example/cb"}|`,
	}
	for id, w := range wantStored {
		if inputs[id] != w {
			t.Errorf("call %s after redact = %q, want %q", id, inputs[id], w)
		}
	}
	for id, stored := range inputs {
		for _, secret := range secrets {
			if strings.Contains(stored, secret) {
				t.Errorf("call %s still holds %s after redact", id, secret)
			}
		}
	}
}

// TestCallmeterCLIReportNoOlderRulesNoteOverConformingCalls: calls stored as
// the sanitizers write them now (a sized description, a cut heredoc and commit
// message, an MCP tool's sizes, a cut url, each error form) print no note.
func TestCallmeterCLIReportNoOlderRulesNoteOverConformingCalls(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_ok", "sess-1", "/work/one.md", fixture.seat)
	raw := []struct{ id, tool, input, errText string }{
		{"toolu_a", "Bash", `{"command":"ls","description":"list files"}`, "Exit code 2\nstderr text"},
		{"toolu_b", "Bash", "{\"command\":\"cat <<EOF > f\\nbody\\nEOF\\ngit commit -m msg\"}", "free error text"},
		{"toolu_c", "mcp__mail__send", `{"to":"x@example.com","count":3}`, callmeter.OutcomeRefused},
		{"toolu_d", "WebFetch", `{"url":"https://x.example/cb?t=q"}`, callmeter.OutcomeDeniedByPermission},
		{"toolu_e", "Read", `{"file_path":["a","b"]}`, callmeter.ErrorNotStored},
	}
	for _, c := range raw {
		input, err := callmeter.SanitizeInput(c.tool, json.RawMessage(c.input))
		if err != nil {
			t.Fatalf("SanitizeInput %s: %v", c.id, err)
		}
		fixture.seedStoredCall(t, c.id, c.tool, input, callmeter.SanitizeError(c.errText))
	}
	for _, topic := range allTopics {
		code, stdout, stderr := fixture.run("report", topic)
		if code != 0 || strings.Contains(stdout, "older privacy rules") {
			t.Errorf("report %s over conforming calls = %d, want no older-rules note\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
	}
}

func TestCallmeterCLIReportNotesAnUnreadableQuietTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		directory   bool
	}{
		{"permission", "permission denied", false},
		{"other-error", "unreadable", true},
	} {
		for _, asJSON := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/json=%t", tc.name, asJSON), func(t *testing.T) {
				fixture := newLab(t)
				transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-unreadable.jsonl")
				fixture.seedQuiet(t, "sess-unreadable", "toolu_unreadable", transcript)
				// This quiet session has no call in flight: recovery reads its requests once.
				store, err := callmeter.OpenDB(context.Background(), fixture.storePath)
				if err != nil {
					t.Fatal(err)
				}
				_, deleteErr := store.DB().Exec("DELETE FROM calls WHERE tool_use_id = ?", "toolu_unreadable")
				if err := errors.Join(deleteErr, store.Close()); err != nil {
					t.Fatal(err)
				}
				if tc.directory {
					if err := os.Remove(transcript); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(transcript, 0o700); err != nil {
						t.Fatal(err)
					}
					quiet := time.Now().Add(-2 * time.Hour)
					if err := os.Chtimes(transcript, quiet, quiet); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Chmod(transcript, 0o000); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(transcript, 0o600) })
				}
				want := "1 transcripts could not be read by recovery (first: " + tc.label + ")"
				args := []string{"report", "sessions"}
				if asJSON {
					args = append(args, "--json")
				}
				code, stdout, stderr := fixture.run(args...)
				if code != 0 || !strings.Contains(stderr, "callmeter: recover quiet sessions: skipped: session sess-unreadable: "+transcript) {
					t.Fatalf("report failed or lost the stderr skip line: code %d", code)
				}
				if asJSON {
					if !slices.Contains(decodeReport(t, stdout).Notes, want) {
						t.Errorf("JSON lacks recovery note %q", want)
					}
				} else if !strings.Contains(stdout, "\nnote: "+want+"\n") {
					t.Errorf("text lacks recovery note %q", want)
				}
			})
		}
	}
}

// A quiet session with a call in flight has its one transcript read for its
// requests and again for the call's result: both reads fail, and the note
// still counts one transcript.
func TestCallmeterCLIReportCountsAnUnreadableTranscriptOnce(t *testing.T) {
	fixture := newLab(t)
	transcript := filepath.Join(t.TempDir(), "projects", "-tmp-demo-proj", "sess-unreadable.jsonl")
	fixture.seedQuiet(t, "sess-unreadable", "toolu_unreadable", transcript)
	if err := os.Chmod(transcript, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(transcript, 0o600) })
	code, stdout, stderr := fixture.run("report", "sessions")
	if code != 0 || strings.Count(stderr, "callmeter: recover quiet sessions: skipped: session sess-unreadable: "+transcript) < 2 {
		t.Fatalf("report = %d, want 0 and the transcript skipped by more than one read", code)
	}
	if want := "\nnote: 1 transcripts could not be read by recovery (first: permission denied)\n"; !strings.Contains(stdout, want) {
		t.Errorf("stdout lacks %q", want)
	}
}

func TestRecoveryNotesUseOnlySafeLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		skipped []error
		want    []string
	}{
		{"none", nil, nil},
		{"permission", []error{fmt.Errorf("session demo: /tmp/demo-home/transcript.jsonl: %w", fs.ErrPermission)}, []string{"1 transcripts could not be read by recovery (first: permission denied)"}},
		{"missing", []error{fmt.Errorf("session demo: /tmp/demo-home/transcript.jsonl: %w", fs.ErrNotExist)}, []string{"1 transcripts could not be read by recovery (first: not found)"}},
		{"other-first", []error{fmt.Errorf("session demo: /tmp/demo-home/transcript.jsonl: %w", fs.ErrInvalid), fs.ErrPermission}, []string{"2 transcripts could not be read by recovery (first: unreadable)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryNotes(tc.skipped); !slices.Equal(got, tc.want) {
				t.Errorf("notes = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCallmeterCLIReportCacheStatesTheWireFile: the cache topic reads
// {CALLMETER_HOME}/wire.db; absent, the coverage note says so and nothing is
// logged; unreadable, the note shows the warning and callmeter.log holds it
// with the file's path, never read as absence.
func TestCallmeterCLIReportCacheStatesTheWireFile(t *testing.T) {
	fixture := newLab(t)
	fixture.seedEverything(t)
	home := filepath.Dir(fixture.storePath)
	code, stdout, stderr := fixture.run("report", "cache")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "note: wire data for 0 of 1 requests (wire.db: absent)\n") {
		t.Fatalf("report cache without wire.db = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if _, err := os.Stat(paths.Log(home)); err == nil {
		t.Errorf("an absent wire.db wrote %s", paths.Log(home))
	}
	wire := filepath.Join(home, "wire.db")
	if err := os.WriteFile(wire, []byte(strings.Repeat("not a database\n", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = fixture.run("report", "cache")
	if code != 0 || !strings.Contains(stdout, "note: wire data for 0 of 1 requests (wire.db: warning: unreadable, see callmeter.log)\n") ||
		!strings.Contains(stderr, "callmeter: wire: ") {
		t.Fatalf("report cache over an unreadable wire.db = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	logged, err := os.ReadFile(paths.Log(home))
	if err != nil || !strings.Contains(string(logged), `"step":"wire"`) || !strings.Contains(string(logged), wire) {
		t.Errorf("callmeter.log = %q, %v; want the wire step naming %s", logged, err, wire)
	}
}
