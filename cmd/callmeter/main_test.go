package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/hookentry"
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
// exit 2, which Claude Code reads as a blocking error. The panic still leaves
// its trace: a stack on stderr and, unless accounted for, one missed.log line
// with reason `panic`, the event and session
// the payload named (`unknown` and no session when it named none), one log
// line, and the next report counts the line as an unrecorded event.
func TestHookPanicStillExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name       string
		payload    func(c cli) string
		line       string // the missed.log line after its seconds field
		accounted  bool
		unwritable bool
	}{
		{
			name: "captured payload",
			payload: func(c cli) string {
				return scriptedPayloads(t, c.user, filepath.Join(filepath.Dir(c.state), "proj"))[0]
			},
			line: "\tPostToolUse\tpanic\tb2c7b094-91c1-4b76-8621-258b240b695a\n",
		},
		{name: "no event", payload: func(cli) string { return "{}" }, line: "\tunknown\tpanic\n"},
		{name: "accounted", payload: func(c cli) string {
			return scriptedPayloads(t, c.user, filepath.Join(filepath.Dir(c.state), "proj"))[0]
		}, accounted: true},
		{name: "unwritable missed log", payload: func(cli) string { return "{}" }, unwritable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCLI(t)
			c.seed() // a store for the report to read, through the real hook
			if tc.unwritable {
				if err := os.Mkdir(paths.Missed(c.state), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stdin := tc.payload(c)
			previous := hook
			hook = func(input io.Reader, _ io.Writer, _ paths.Getenv) int {
				if read, err := io.ReadAll(input); err != nil || string(read) != stdin {
					t.Errorf("the hook read %d bytes (%v), want the %d of stdin", len(read), err, len(stdin))
				}
				if tc.accounted {
					panic(hookentry.AccountedPanic{Value: "boom"})
				}
				panic("boom")
			}
			t.Cleanup(func() { hook = previous })
			code, stdout, stderr := c.run(stdin, "hook")
			hook = previous
			if code != 0 || stdout != "" || !strings.Contains(stderr, "hook panicked: boom") || !strings.Contains(stderr, "goroutine ") {
				t.Fatalf("hook that panics = %d, stdout %q, stderr %q; want 0, no stdout, the panic said", code, stdout, stderr)
			}
			missed, err := os.ReadFile(paths.Missed(c.state))
			if tc.unwritable {
				if err == nil || !strings.Contains(stderr, "open "+paths.Missed(c.state)) {
					t.Errorf("unwritable missed.log: read error %v, append error said %v", err, strings.Contains(stderr, "open "+paths.Missed(c.state)))
				}
				return
			}
			if tc.accounted {
				if !errors.Is(err, fs.ErrNotExist) && (err != nil || len(missed) != 0) {
					t.Fatalf("accounted panic left %d bytes in missed.log (%v), want no line", len(missed), err)
				}
				code, stdout, stderr = c.run("", "report", "files")
				if code != 0 || strings.Contains(stdout, "events unrecorded: hook terminated before recording") {
					t.Errorf("accounted panic report = %d, unrecorded event note %v; stderr %q", code,
						strings.Contains(stdout, "events unrecorded: hook terminated before recording"), stderr)
				}
				return
			}
			if err != nil || !regexp.MustCompile(`^[0-9]+`+regexp.QuoteMeta(tc.line)+`$`).Match(missed) {
				t.Fatalf("missed.log = %q (%v), want exactly one line {secs}%q", missed, err, tc.line)
			}
			logged, err := os.ReadFile(paths.Log(c.state))
			if err != nil || strings.Count(string(logged), "hook panicked: boom") != 1 {
				t.Errorf("callmeter.log = %q (%v), want one line saying the panic", logged, err)
			}
			code, stdout, stderr = c.run("", "report", "files")
			if code != 0 || !strings.Contains(stdout, "1 events unrecorded: hook terminated before recording") {
				t.Errorf("report files = %d, want the panic counted as an unrecorded event\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
		})
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
		if code != 0 || stderr != "" || !strings.Contains(stdout, "usage: callmeter {hook|report|redact|version|help}") ||
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

// bashFailure is the captured failed Bash call of the scripted chat with its
// command and error replaced by invented ones.
func bashFailure(t *testing.T, c cli, command, errorText string) string {
	t.Helper()
	payload := scriptedPayloads(t, c.user, filepath.Join(filepath.Dir(c.state), "proj"))[4]
	if !strings.Contains(payload, `"hook_event_name":"PostToolUseFailure"`) {
		t.Fatalf("payload 5 is not the captured Bash failure: %s", payload)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	fields["tool_input"].(map[string]any)["command"] = command
	fields["error"] = errorText
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return string(out)
}

func TestHookStoresNoHeredocBody(t *testing.T) {
	c := newCLI(t)
	body := "first-private-line\n\tsecond-private-line\n"
	payload := bashFailure(t, c, "cat > /tmp/demo-proj/notes.txt <<'EOF'\n"+body+"EOF\nwc -l /tmp/demo-proj/notes.txt", "Exit code 1")
	if code, stdout, stderr := c.run(payload, "hook"); code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("hook = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n := c.count(`SELECT count(*) FROM calls WHERE instr(json_extract(input, '$.command'), 'private-line') > 0 OR instr(input, 'private-line') > 0`); n != 0 {
		t.Fatalf("calls holding the heredoc body = %d, want 0", n)
	}
	want := `'cat > /tmp/demo-proj/notes.txt <<''EOF''' || char(10) || 'EOF' || char(10) || 'wc -l /tmp/demo-proj/notes.txt'`
	query := fmt.Sprintf(`SELECT count(*) FROM calls WHERE json_extract(input, '$.command') = %s AND json_extract(input, '$.heredoc_bytes') = %d`, want, len(body))
	if n := c.count(query); n != 1 {
		t.Fatalf("calls with the redirect, the delimiter and heredoc_bytes %d = %d, want 1", len(body), n)
	}
}

// Turns green once the store writes calls.error through
// callmeter.SanitizeError.
func TestHookStoresNoToolOutputAsError(t *testing.T) {
	c := newCLI(t)
	payload := bashFailure(t, c, "cat /tmp/demo-proj/missing.txt", "Exit code 1\ncat: private-output-line")
	if code, stdout, stderr := c.run(payload, "hook"); code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("hook = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if n := c.count(`SELECT count(*) FROM calls WHERE error = 'Exit code 1'`); n != 1 {
		t.Fatalf("calls whose error is the exit code line alone = %d, want 1", n)
	}
}

func TestRedactRewritesTheStoreAndPrintsCounts(t *testing.T) {
	c := newCLI(t)
	c.seed()
	store, err := callmeter.OpenDB(context.Background(), c.store())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = store.DB().Exec(`INSERT INTO calls (tool_use_id, tool, ts, cwd, input) VALUES ('toolu_old', 'Bash', 1, '/tmp/demo-proj', ?)`,
		`{"command":"cat <<EOF > /tmp/demo-proj/a\nprivate-line\nEOF\n"}`)
	if err := errors.Join(err, store.Close()); err != nil {
		t.Fatalf("seed an old row: %v", err)
	}
	code, stdout, stderr := c.run("", "redact")
	if code != 0 || stderr != "" {
		t.Fatalf("redact = %d, stderr %q", code, stderr)
	}
	for _, line := range []string{"calls.input     1 rows rewritten", "calls.error     0 rows rewritten", "events.detail   0 rows rewritten", "command_parts   0 rows deleted"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("redact stdout lacks %q:\n%s", line, stdout)
		}
	}
	if n := c.count(`SELECT count(*) FROM calls WHERE instr(input, 'private-line') > 0`); n != 0 {
		t.Fatalf("calls holding the body after redact = %d, want 0", n)
	}
}

func TestRedactWithoutStoreSaysSoAndCreatesNothing(t *testing.T) {
	c := newCLI(t)
	code, stdout, stderr := c.run("", "redact")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "no store at") {
		t.Fatalf("redact = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(c.store()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("redact created the store: %v", err)
	}
}
