package command

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
	if err := os.MkdirAll(filepath.Join(fixture.seat, "projects", "-work", "sess-1.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := fixture.run("report", "files", "--session", "sess-1")
	title, _, _ := strings.Cut(stdout, "\n")
	if code != 0 || !strings.Contains(title, "session=sess-1 (?)") ||
		strings.Count(stdout, "note: chat names could not be read: session sess-1: ") != 1 {
		t.Fatalf("report files --session = %d, want a ? chat and one note\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// allTopics are the 14 report topics, the six ported then the eight of 5-b.
var allTopics = []string{
	"files", "writes", "commands", "context", "sequences", "faults",
	"sessions", "prompts", "effort", "tokens", "agents", "outcomes", "coverage", "events",
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

// TestCallmeterCLIReportEveryNewTopicShowsItsSeededRows: the eight new topics
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
	missed := paths.Missed(filepath.Dir(fixture.storePath))
	if err := os.MkdirAll(filepath.Dir(missed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missed, []byte("1\tStop\tno binary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if _, err := os.Stat(missed); err != nil {
		t.Errorf("a report with no store touched missed.log: %v", err)
	}
}

func TestCallmeterCLIReportEmptyWindowIsOneLineAndEmptyRows(t *testing.T) {
	fixture := newLab(t)
	fixture.seedRead(t, "toolu_1", "sess-1", "/work/one.md", fixture.seat)
	for _, topic := range []string{"prompts", "outcomes", "events", "agents", "tokens"} {
		code, stdout, stderr := fixture.run("report", topic)
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if code != 0 || len(lines) < 2 || lines[1] != report.EmptyLine {
			t.Errorf("report %s over an empty window = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
		code, stdout, stderr = fixture.run("report", topic, "--json")
		if got := decodeReport(t, stdout); code != 0 || got.Rows == nil || len(got.Rows) != 0 || !strings.Contains(stdout, `"rows":[]`) {
			t.Errorf("report %s --json over an empty window = %d\nstdout:\n%s\nstderr:\n%s", topic, code, stdout, stderr)
		}
	}
}

func TestCallmeterCLIUnknownTopicListsAllFourteen(t *testing.T) {
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
		"faults":   {"--project does not apply to faults", "--agent-type does not apply to faults"},
		"sessions": {"--agent-type does not apply to sessions"},
		"coverage": {"--project does not apply to coverage", "--agent-type does not apply to coverage"},
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
// 14 topics, and the file is consumed.
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
