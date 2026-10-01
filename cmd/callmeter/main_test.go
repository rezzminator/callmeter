package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/testjail"
)

func TestMain(m *testing.M) { os.Exit(testjail.Run(m)) }

// scriptedPayloads is the captured chat of the hook entry's fixtures, its
// neutral roots rewritten to the given ones.
func scriptedPayloads(t *testing.T, home, proj string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "hookentry", "testdata", "callmeter", "scripted.jsonl"))
	if err != nil {
		t.Fatalf("read the captured payloads: %v", err)
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "/tmp/demo-home", home), "/tmp/demo-proj", proj)
	return strings.Split(strings.TrimSpace(text), "\n")
}

// cli is one binary invocation over a scratch state home.
type cli struct {
	t     *testing.T
	state string // CALLMETER_HOME
	user  string // HOME
}

func newCLI(t *testing.T) cli {
	t.Helper()
	root := t.TempDir()
	return cli{t: t, state: filepath.Join(root, "state"), user: filepath.Join(root, "user")}
}

func (c cli) store() string { return paths.Store(c.state) }

func (c cli) run(stdin string, args ...string) (code int, stdout, stderr string) {
	c.t.Helper()
	env := map[string]string{paths.EnvHome: c.state, "HOME": c.user}
	var out, errs bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errs, func(name string) string { return env[name] })
	return code, out.String(), errs.String()
}

// seed feeds the first captured payload (a whole Read of fixture.go) through
// the hook, so a store exists with one file call.
func (c cli) seed() {
	c.t.Helper()
	payload := scriptedPayloads(c.t, c.user, filepath.Join(filepath.Dir(c.state), "proj"))[0]
	if code, stdout, stderr := c.run(payload, "hook"); code != 0 || stdout != "" || stderr != "" {
		c.t.Fatalf("seed hook = %d, stdout %q, stderr %q; want 0 and both empty", code, stdout, stderr)
	}
}

func (c cli) count(query string) int {
	c.t.Helper()
	store, err := callmeter.OpenDB(context.Background(), c.store())
	if err != nil {
		c.t.Fatalf("open store: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			c.t.Errorf("close store: %v", err)
		}
	}()
	var n int
	if err := store.DB().QueryRow(query).Scan(&n); err != nil {
		c.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestHookRecordsRowsAndStaysQuiet(t *testing.T) {
	c := newCLI(t)
	c.seed()
	if n := c.count("SELECT count(*) FROM calls WHERE tool = 'Read'"); n != 1 {
		t.Fatalf("calls recorded = %d, want the one Read", n)
	}
	if _, err := os.Stat(c.store()); err != nil {
		t.Fatalf("no store under CALLMETER_HOME: %v", err)
	}
}

func TestHookStoreUnopenableExitsZeroNamingTheStoreError(t *testing.T) {
	c := newCLI(t)
	if err := os.MkdirAll(filepath.Dir(c.state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.state, []byte("a file where the home should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := scriptedPayloads(t, c.user, "/tmp/proj")[0]
	code, stdout, stderr := c.run(payload, "hook")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "callmeter: store: ") || strings.Contains(stderr, "panic") {
		t.Fatalf("hook over an unopenable store = %d, stdout %q, stderr %q; want 0, no stdout, the store error", code, stdout, stderr)
	}
}

func TestHookPayloadWithoutSessionIsLoggedAndFaulted(t *testing.T) {
	c := newCLI(t)
	payload := `{"hook_event_name":"PostToolUse","tool_use_id":"toolu_orphan","tool_name":"Bash"}`
	code, stdout, stderr := c.run(payload, "hook")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "carries no session_id") {
		t.Fatalf("hook = %d, stdout %q, stderr %q; want 0, no stdout, the missing session said", code, stdout, stderr)
	}
	if n := c.count("SELECT count(*) FROM faults WHERE stage = 'payload'"); n != 1 {
		t.Errorf("payload faults = %d, want 1", n)
	}
	logged, err := os.ReadFile(paths.Log(c.state))
	if err != nil || !strings.Contains(string(logged), `"step":"payload"`) {
		t.Errorf("callmeter.log = %q (%v), want a JSON line for the payload fault", logged, err)
	}
}

func TestHookGarbageStdinIsAPayloadFault(t *testing.T) {
	c := newCLI(t)
	code, stdout, _ := c.run("\x00\xffnot json", "hook")
	if code != 0 || stdout != "" {
		t.Fatalf("hook over garbage = %d, stdout %q; want 0 and no stdout", code, stdout)
	}
	if n := c.count("SELECT count(*) FROM faults WHERE stage = 'payload'"); n != 1 {
		t.Errorf("payload faults = %d, want 1", n)
	}
}

// TestHookPanicStillExitsZero: a hook exits 0 on every path; a panic would
// exit 2, which Claude Code reads as a blocking error.
func TestHookPanicStillExitsZero(t *testing.T) {
	previous := hook
	hook = func(io.Reader, io.Writer, paths.Getenv) int { panic("boom") }
	t.Cleanup(func() { hook = previous })
	code, stdout, stderr := newCLI(t).run("{}", "hook")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "hook panicked: boom") {
		t.Fatalf("hook that panics = %d, stdout %q, stderr %q; want 0, no stdout, the panic said", code, stdout, stderr)
	}
}

func TestReportFilesOverASeededStore(t *testing.T) {
	c := newCLI(t)
	c.seed()
	code, stdout, stderr := c.run("", "report", "files")
	title, _, _ := strings.Cut(stdout, "\n")
	if code != 0 || !strings.HasPrefix(title, "callmeter files") || !strings.Contains(title, "window: since ") ||
		!strings.Contains(stdout, "fixture.go") {
		t.Fatalf("report files = %d, want a table titled with its window\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestReportFilesFlagAfterTheTopic(t *testing.T) {
	c := newCLI(t)
	c.seed()
	code, stdout, stderr := c.run("", "report", "files", "--limit", "3")
	if code != 0 || strings.Count(stdout, "fixture.go") > 3 || !strings.Contains(stdout, "limit=3") {
		t.Fatalf("report files --limit 3 = %d, want a title carrying limit=3\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

func TestReportStoreErrorExitsOne(t *testing.T) {
	c := newCLI(t)
	if err := os.MkdirAll(c.store(), 0o700); err != nil { // a directory where the database file should be
		t.Fatal(err)
	}
	code, stdout, stderr := c.run("", "report", "files")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "callmeter: cannot open store "+c.store()) {
		t.Fatalf("report over a broken store = %d, stdout %q, stderr %q; want 1 and the store error", code, stdout, stderr)
	}
}

func TestReportWithoutStoreSaysSoAndCreatesNothing(t *testing.T) {
	c := newCLI(t)
	code, stdout, stderr := c.run("", "report", "files")
	want := "callmeter: no store at " + c.store() + ": nothing recorded yet\n"
	if code != 0 || stdout != want || stderr != "" {
		t.Fatalf("report without a store = %d, stdout %q, stderr %q; want 0 and %q", code, stdout, stderr, want)
	}
	if _, err := os.Stat(c.state); !os.IsNotExist(err) {
		t.Fatalf("report created %s (stat err %v)", c.state, err)
	}
}

func TestUsageErrorsExitTwoWithUsageOnStderr(t *testing.T) {
	for _, args := range [][]string{{"report"}, {"report", "nope"}, {"bogus"}, {}} {
		code, stdout, stderr := newCLI(t).run("", args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: callmeter") {
			t.Errorf("callmeter %q = %d, stdout %q, stderr %q; want 2, no stdout, usage on stderr", args, code, stdout, stderr)
		}
	}
}

func TestHelpPrintsUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, stdout, stderr := newCLI(t).run("", arg)
		if code != 0 || stderr != "" || !strings.Contains(stdout, "usage: callmeter {hook|report|version|help}") ||
			!strings.Contains(stdout, "usage: callmeter report") {
			t.Errorf("callmeter %s = %d, stdout %q, stderr %q; want 0 and the usage on stdout", arg, code, stdout, stderr)
		}
	}
}

func TestVersionPrintsTheLinkedVersion(t *testing.T) {
	if code, stdout, _ := newCLI(t).run("", "version"); code != 0 || stdout != "callmeter dev\n" {
		t.Fatalf("callmeter version = %d, %q; want 0 and \"callmeter dev\\n\" with no ldflags", code, stdout)
	}
	previous := version
	version = "9.9.9"
	t.Cleanup(func() { version = previous })
	if code, stdout, _ := newCLI(t).run("", "version"); code != 0 || stdout != "callmeter 9.9.9\n" {
		t.Fatalf("callmeter version = %d, %q; want 0 and the linked version", code, stdout)
	}
}
