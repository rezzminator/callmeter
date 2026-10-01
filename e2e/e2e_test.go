package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/runner"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

const (
	gateVar     = "CALLMETER_E2E"
	gateMessage = "CALLMETER_E2E=1 runs the live e2e (costs tokens)"
	scratchBase = "/tmp/callmeter/e2e"
	// sessionTimeout bounds each claude process: a slow network is a slow
	// session, a hang is a failure.
	sessionTimeout = 400 * time.Second
	// settleLimit bounds the waits for hooks that outlive their session.
	settleLimit = 20 * time.Second
	// sentinel is a word that appears only in run 1's prose, never in a
	// step's arguments, and so must appear nowhere in what callmeter stores.
	sentinel = "quokkaplinth"
)

// Programs the live runs need; one missing fails the test naming it.
var requiredPrograms = []string{"claude", "curl", "git", "go", "sh"}

// reportTopics are the fourteen topics of `callmeter report`.
var reportTopics = []string{
	"files", "writes", "commands", "context", "sequences", "faults",
	"sessions", "prompts", "effort", "tokens", "agents", "outcomes", "coverage", "events",
}

// run1Prompt asks for the steps in order; the sentinel sits in a sentence of
// its own, outside every step. The Agent step runs in the background and the
// SubagentStop hook waits up to 3 s for the transcript to settle, and a
// headless session cancels its running async hooks at exit: step 9 keeps the
// session alive until that hook has recorded the sub-agent's stop.
var run1Prompt = strings.Join([]string{
	"You are running a scripted check. Do the numbered steps strictly in order, one tool call per step, with no commentary between them.",
	"Background that has nothing to do with any step: the " + sentinel + " harbor is quiet today.",
	"1. Use the Read tool on README.md.",
	"2. Use the Bash tool to run: ls",
	"3. Use the Bash tool to run: ls /nonexistent_dir_e2e",
	"4. Use the Write tool to create notes.txt holding exactly three lines: alpha, beta, gamma.",
	"5. Use the Edit tool on notes.txt to change the line beta into delta.",
	"6. Use the Bash tool to run: go test ./...",
	"7. Use the Bash tool to run: git add -A && git commit -m e2e-commit",
	"8. Use the Agent tool with the general-purpose subagent type, asking it to read go.mod with the Read tool and report the module name.",
	"9. Use the Bash tool to run: sleep 15",
	"After step 9 reply with the single word done.",
}, "\n")

// requireE2E is every live test's first line: the gate skips, a missing
// program fails.
func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv(gateVar) != "1" {
		t.Skip(gateMessage)
	}
	for _, name := range requiredPrograms {
		if _, err := (runner.Real{}).LookPath(name); err != nil {
			t.Fatalf("%s=1 needs %q on PATH and it is missing: %v", gateVar, name, err)
		}
	}
}

var scratch struct {
	once sync.Once
	dir  string
	err  error
}

// scratchRoot is /tmp/callmeter/e2e/{unix seconds}/, created once per test
// process and kept after it.
func scratchRoot(t *testing.T) string {
	t.Helper()
	scratch.once.Do(func() {
		scratch.dir = filepath.Join(scratchBase, fmt.Sprint(clock.Real.Now().Unix()))
		scratch.err = os.MkdirAll(scratch.dir, 0o700)
	})
	if scratch.err != nil {
		t.Fatalf("create scratch root %s: %v", scratch.dir, scratch.err)
	}
	return scratch.dir
}

var built struct {
	once sync.Once
	path string
	err  error
}

// pluginDir is the worktree's plugin, the directory --plugin-dir names for run 1.
func pluginDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "plugins", "callmeter"))
	if err != nil {
		t.Fatalf("resolve the plugin directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatalf("plugin manifest: %v", err)
	}
	return dir
}

// pluginVersion is plugin.json's version, the one the binary is stamped with.
func pluginVersion(t *testing.T, plugin string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(plugin, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version == "" {
		t.Fatalf("plugin.json has no version (decode error: %v)", err)
	}
	return manifest.Version
}

// builtBinary builds ./cmd/callmeter once per process with the release flags,
// stamped with the plugin's version, into the scratch root.
func builtBinary(t *testing.T) string {
	t.Helper()
	root := scratchRoot(t)
	version := pluginVersion(t, pluginDir(t))
	built.once.Do(func() {
		repo, err := filepath.Abs("..")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(root, "callmeter")
		res, err := runner.Real{}.Run(context.Background(), []string{
			"go", "build", "-trimpath", "-buildvcs=false",
			"-ldflags", "-s -w -buildid= -X main.version=" + version,
			"-o", built.path, "./cmd/callmeter",
		}, runner.RunOptions{Dir: repo, Env: append(os.Environ(), "CGO_ENABLED=0")})
		if err != nil {
			built.err = err
			return
		}
		if res.ExitCode != 0 {
			built.err = fmt.Errorf("go build exited %d: %s", res.ExitCode, res.Stderr)
		}
	})
	if built.err != nil {
		t.Fatalf("build the callmeter binary: %v", built.err)
	}
	return built.path
}

// dropFromChild lists the variables of the surrounding Claude Code session
// that must not reach a child session.
var dropFromChild = []string{
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_BRIDGE_SESSION_ID", "CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_EFFORT",
	"CLAUDE_PLUGIN_ROOT", "CLAUDE_PLUGIN_DATA", "CLAUDE_PID",
}

// childEnv is the test process's environment without callmeter's own
// variables and the surrounding session's, plus extra. The real HOME and
// CLAUDE_CONFIG_DIR stay: Claude Code needs its login.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CALLMETER_") || slices.Contains(dropFromChild, name) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// canonical resolves symlinks when the path exists, else cleans it: on macOS
// /tmp is a symlink to /private/tmp.
func canonical(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// refuseRealState stops the test when home is, or sits outside the scratch
// root, or resolves to a default callmeter state directory.
func refuseRealState(t *testing.T, home string) {
	t.Helper()
	defaults := []string{}
	if user, err := os.UserHomeDir(); err == nil {
		defaults = append(defaults, filepath.Join(user, ".local", "state", "callmeter"))
	}
	if state := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		defaults = append(defaults, filepath.Join(state, "callmeter"))
	}
	resolved := canonical(home)
	for _, def := range defaults {
		if resolved == canonical(def) {
			t.Fatalf("CALLMETER_HOME %s resolves to the real state directory %s; refusing to run", home, def)
		}
	}
	base := canonical(scratchBase) + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), base) {
		t.Fatalf("CALLMETER_HOME %s is outside %s; refusing to run", home, scratchBase)
	}
}

// liveRun is one scratch directory: its CALLMETER_HOME and its project.
type liveRun struct {
	root    string
	home    string
	project string
}

func newLiveRun(t *testing.T, name string) *liveRun {
	t.Helper()
	root := filepath.Join(scratchRoot(t), name)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create run directory %s: %v", root, err)
	}
	r := &liveRun{root: root, home: filepath.Join(root, "home"), project: filepath.Join(root, "project")}
	if err := os.Mkdir(r.home, 0o700); err != nil {
		t.Fatalf("create CALLMETER_HOME %s: %v", r.home, err)
	}
	refuseRealState(t, r.home)
	t.Logf("%s: scratch %s (kept after the run)", name, root)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s: the hook logs of %s, for the diagnosis of the failure above\n%s", name, r.home, r.logTails())
		}
	})
	return r
}

// logTailLines is how much of each hook log a failed test prints.
const logTailLines = 40

// lastLines is the last n lines of data.
func lastLines(data []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// logTail renders the last n lines of the file at path under its path; an
// absent file is a line saying so and an unreadable one is named with its error.
func logTail(path string, n int) string {
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return fmt.Sprintf("%s: absent", path)
	case err != nil:
		return fmt.Sprintf("%s: unreadable: %v", path, err)
	case len(data) == 0:
		return fmt.Sprintf("--- %s: empty ---", path)
	}
	return fmt.Sprintf("--- last %d lines of %s ---\n%s", n, path, lastLines(data, n))
}

// logTails is the tail of callmeter.log and of missed.log of the run's home.
func (r *liveRun) logTails() string {
	return logTail(paths.Log(r.home), logTailLines) + "\n" + logTail(paths.Missed(r.home), logTailLines)
}

// gitEnv makes git deterministic and independent of the host's configuration.
var gitEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	"GIT_AUTHOR_NAME=demo", "GIT_AUTHOR_EMAIL=demo@example.com",
	"GIT_COMMITTER_NAME=demo", "GIT_COMMITTER_EMAIL=demo@example.com",
}

// execute runs argv to completion under timeout and fails on a start error, a
// timeout or, when wantExit is true, a nonzero exit.
func execute(t *testing.T, label string, timeout time.Duration, argv []string, dir string, env []string, wantExit bool) runner.RunResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := runner.Real{}.Run(ctx, argv, runner.RunOptions{Env: env, Dir: dir})
	if ctx.Err() != nil {
		t.Fatalf("%s: still running after %s: %q", label, timeout, argv)
	}
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if wantExit && res.ExitCode != 0 {
		t.Fatalf("%s: exit %d\nstderr: %s\nstdout: %s", label, res.ExitCode, tail(res.Stderr), tail(res.Stdout))
	}
	return res
}

func tail(b []byte) string {
	const limit = 2000
	if len(b) > limit {
		b = b[len(b)-limit:]
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// createProject makes the scratch project: a git repository holding a go
// module with one passing test, a one-line CLAUDE.md and a README.md.
func (r *liveRun) createProject(t *testing.T) {
	t.Helper()
	dir := r.project
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/demo\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "calc", "calc.go"), "// Package calc adds numbers.\npackage calc\n\n// Add returns a plus b.\nfunc Add(a, b int) int { return a + b }\n")
	writeFile(t, filepath.Join(dir, "calc", "calc_test.go"), "package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2, 3) != 5\")\n\t}\n}\n")
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), "Demo project for a scripted check.\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# demo\n\nA toy Go module.\n")
	env := childEnv(gitEnv...)
	for _, argv := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "add", "-A"},
		{"git", "-c", "user.name=demo", "-c", "user.email=demo@example.com", "commit", "-q", "-m", "init"},
	} {
		execute(t, strings.Join(argv, " "), time.Minute, argv, dir, env, true)
	}
}

// newSessionID is a random version-4 UUID.
func newSessionID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("session id: %v", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// session is one finished claude process.
type session struct {
	id     string
	stream []byte
}

// claude runs one headless session: haiku, project and local settings only,
// the plugin loaded per session, stdin closed. resume names the session to
// continue; otherwise a fresh id is minted. env carries the plugin's binary
// path or release server.
func (r *liveRun) claude(t *testing.T, label, plugin, prompt, resume string, env ...string) session {
	t.Helper()
	args := []string{"claude", "-p", "--plugin-dir", plugin, "--model", "haiku", "--setting-sources", "project,local", "--permission-mode", "bypassPermissions"}
	id := resume
	if resume != "" {
		args = append(args, "--resume", resume)
	} else {
		id = newSessionID(t)
		args = append(args, "--session-id", id)
	}
	args = append(args, "--output-format", "stream-json", "--verbose", prompt)
	full := childEnv(append([]string{"CALLMETER_HOME=" + r.home, "CLAUDE_CODE_SUBAGENT_MODEL=haiku"}, append(gitEnv, env...)...)...)
	res := execute(t, label, sessionTimeout, args, r.project, full, false)
	streamPath := filepath.Join(r.root, label+".stream.jsonl")
	if err := os.WriteFile(streamPath, res.Stdout, 0o600); err != nil {
		t.Fatalf("%s: keep the stream: %v", label, err)
	}
	if err := os.WriteFile(filepath.Join(r.root, label+".stderr.txt"), res.Stderr, 0o600); err != nil {
		t.Fatalf("%s: keep stderr: %v", label, err)
	}
	t.Logf("%s: session %s, exit %d, stream %s", label, id, res.ExitCode, streamPath)
	if res.ExitCode != 0 {
		t.Fatalf("%s: claude exited %d\nstderr: %s", label, res.ExitCode, tail(res.Stderr))
	}
	return session{id: id, stream: res.Stdout}
}

// callmeter runs the built binary against this run's store.
func (r *liveRun) callmeter(t *testing.T, bin string, args ...string) runner.RunResult {
	t.Helper()
	return execute(t, "callmeter "+strings.Join(args, " "), time.Minute, append([]string{bin}, args...), r.project,
		childEnv("CALLMETER_HOME="+r.home), false)
}

// openStore opens the run's store for reading; the handle closes with t.
func openStore(t *testing.T, home string) *sql.DB {
	t.Helper()
	path := paths.Store(home)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the run left no store: %v", err)
	}
	db, err := sqlitedb.OpenReadWrite(path, 10*time.Second)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store %s: %v", path, err)
		}
	})
	return db
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// describeWhere is a condition with the arguments its placeholders stand for.
func describeWhere(where string, args []any) string {
	if len(args) == 0 {
		return where
	}
	return fmt.Sprintf("%s with %v", where, args)
}

// wantCount fails, naming the table, the condition, got and want, unless
// exactly want rows of table match where.
func wantCount(t *testing.T, db *sql.DB, table, where string, want int, args ...any) bool {
	t.Helper()
	n := count(t, db, "SELECT COUNT(*) FROM "+table+" WHERE "+where, args...)
	if n != want {
		t.Errorf("%s: %d rows where %s, want %d", table, n, describeWhere(where, args), want)
		return false
	}
	return true
}

// wantSome fails, naming the table, the condition, got and want, unless at
// least one row of table matches where.
func wantSome(t *testing.T, db *sql.DB, table, where string, args ...any) bool {
	t.Helper()
	n := count(t, db, "SELECT COUNT(*) FROM "+table+" WHERE "+where, args...)
	if n == 0 {
		t.Errorf("%s: %d rows where %s, want at least 1", table, n, describeWhere(where, args))
		return false
	}
	return true
}

const (
	cellNull  = "<NULL>"
	cellNoRow = "<no row>"
)

// cell is the value of column col in the row of table whose idCol is id, as
// text; a NULL is cellNull and a missing row is cellNoRow.
func cell(t *testing.T, db *sql.DB, table, col, idCol, id string) string {
	t.Helper()
	var v sql.NullString
	err := db.QueryRowContext(context.Background(), fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?", col, table, idCol), id).Scan(&v)
	switch {
	case err == sql.ErrNoRows:
		return cellNoRow
	case err != nil:
		t.Fatalf("read %s.%s for %s %s: %v", table, col, idCol, id, err)
	case !v.Valid:
		return cellNull
	}
	return v.String
}

// isSet is true for a cell holding a non-empty value.
func isSet(got string) bool { return got != "" && got != cellNull && got != cellNoRow }

// wantCell fails, naming table, column, the row's id, got and want, unless ok
// accepts the cell.
func wantCell(t *testing.T, db *sql.DB, table, col, idCol, id, want string, ok func(got string) bool) bool {
	t.Helper()
	got := cell(t, db, table, col, idCol, id)
	if ok(got) {
		return true
	}
	t.Errorf("%s.%s: row of %s %s: got %q, want %s", table, col, idCol, id, got, want)
	return false
}

// rowsText renders up to 50 rows of a query, one per line, columns joined by
// " | ", a NULL as NULL; a failed query renders as the failure, never as no rows.
func rowsText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		return fmt.Sprintf("(query %q failed: %v)", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Logf("close rows of %q: %v", query, err)
		}
	}()
	cols, err := rows.Columns()
	if err != nil {
		return fmt.Sprintf("(columns of %q failed: %v)", query, err)
	}
	var lines []string
	more := 0
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Sprintf("(scan of %q failed: %v)", query, err)
		}
		if len(lines) == 50 {
			more++
			continue
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = "NULL"
			if v.Valid {
				parts[i] = v.String
			}
		}
		lines = append(lines, strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("(read of %q failed after %d rows: %v)", query, len(lines), err)
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("... and %d more rows", more))
	}
	if len(lines) == 0 {
		return fmt.Sprintf("(no rows from %q)", query)
	}
	return "  " + strings.Join(cols, " | ") + "\n  " + strings.Join(lines, "\n  ")
}

// column returns the first column of every row, a NULL as "".
func column(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows of %q: %v", query, err)
		}
	}()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, v.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows of %q: %v", query, err)
	}
	return out
}

// rowTotal is how many rows the store's data tables hold, -1 without a store.
func rowTotal(db *sql.DB) (int, error) {
	total := 0
	for _, table := range []string{"calls", "requests", "agents", "agent_turns", "turns", "events", "sessions", "faults"} {
		var n int
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			return 0, fmt.Errorf("count %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}

// settle waits until the store stops changing: async hooks of a finished
// session can outlive it by a moment.
func settle(t *testing.T, home string) {
	t.Helper()
	db := openStore(t, home)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store after settling: %v", err)
		}
	}()
	last, steady := -1, 0
	deadline := clock.Real.Now().Add(settleLimit)
	for clock.Real.Now().Before(deadline) {
		n, err := rowTotal(db)
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if n == last {
			steady++
		} else {
			last, steady = n, 0
		}
		if steady >= 8 {
			return
		}
		<-clock.Real.After(250 * time.Millisecond)
	}
	t.Logf("the store was still changing after %s", settleLimit)
}

// eventually runs check until it returns no problems or settleLimit passes,
// then reports every problem of the last attempt and, from each seen, the
// rows the store held at that moment.
func eventually(t *testing.T, what string, check func() []string, seen ...func() string) {
	t.Helper()
	deadline := clock.Real.Now().Add(settleLimit)
	for {
		problems := check()
		if len(problems) == 0 {
			return
		}
		if clock.Real.Now().After(deadline) {
			for _, p := range problems {
				t.Errorf("waited for %s: not met after %s: %s", what, settleLimit, p)
			}
			for _, rows := range seen {
				t.Logf("waited for %s: the last rows seen\n%s", what, rows())
			}
			return
		}
		<-clock.Real.After(time.Second)
	}
}

// toolUse is one tool_use block of a transcript.
type toolUse struct {
	ID, Name, MessageID, Command string
	IsError                      bool // the transcript's tool_result for it says so
}

// transcript is what one transcript file shows.
type transcript struct {
	path    string
	uses    []toolUse
	partial bool // the last line was cut off mid-write and skipped
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// readTranscript parses a transcript file. A final line without its newline
// that does not parse is a writer cut off mid-append: it is skipped and noted.
// Any other malformed line fails the test naming path and line.
func readTranscript(t *testing.T, path string) transcript {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	out := transcript{path: path}
	errored := map[string]bool{}
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			if i == len(lines)-1 {
				out.partial = true
				t.Logf("%s: last line is cut off mid-write; skipped", path)
				continue
			}
			t.Fatalf("%s:%d: malformed transcript line: %v", path, i+1, err)
		}
		var blocks []block
		if json.Unmarshal(entry.Message.Content, &blocks) != nil {
			continue // a plain-string content holds no tool block
		}
		for _, b := range blocks {
			switch {
			case entry.Type == "assistant" && b.Type == "tool_use":
				var input struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(b.Input, &input) // only a Bash input has a command
				out.uses = append(out.uses, toolUse{ID: b.ID, Name: b.Name, MessageID: entry.Message.ID, Command: input.Command})
			case entry.Type == "user" && b.Type == "tool_result" && b.IsError:
				errored[b.ToolUseID] = true
			}
		}
	}
	for i := range out.uses {
		out.uses[i].IsError = errored[out.uses[i].ID]
	}
	return out
}

// transcripts finds a session's main transcript and its sub-agent transcripts
// (keyed by agent id) under the seat the session ran from.
func transcripts(t *testing.T, sessionID string) (main transcript, subs map[string]transcript) {
	t.Helper()
	seat := os.Getenv("CLAUDE_CONFIG_DIR")
	if seat == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("home directory: %v", err)
		}
		seat = filepath.Join(user, ".claude")
	}
	matches, err := filepath.Glob(filepath.Join(seat, "projects", "*", sessionID+".jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want one transcript for session %s under %s/projects, found %v (glob error: %v)", sessionID, seat, matches, err)
	}
	main = readTranscript(t, matches[0])
	subs = map[string]transcript{}
	files, err := filepath.Glob(filepath.Join(strings.TrimSuffix(matches[0], ".jsonl"), "subagents", "agent-*.jsonl"))
	if err != nil {
		t.Fatalf("glob sub-agent transcripts: %v", err)
	}
	for _, file := range files {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "agent-"), ".jsonl")
		subs[id] = readTranscript(t, file)
	}
	return main, subs
}

// allUses is every tool_use of the main and sub-agent transcripts.
func allUses(main transcript, subs map[string]transcript) []toolUse {
	uses := slices.Clone(main.uses)
	for _, sub := range subs {
		uses = append(uses, sub.uses...)
	}
	return uses
}

type callRow struct{ requestID, tool string }

type requestRow struct{ pending, input, cacheRead, cacheCreation, cache5m, cache1h, context, output sql.NullInt64 }

func loadCalls(t *testing.T, db *sql.DB) map[string]callRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), "SELECT tool_use_id, COALESCE(request_id, ''), COALESCE(tool, '') FROM calls")
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close calls: %v", err)
		}
	}()
	out := map[string]callRow{}
	for rows.Next() {
		var id string
		var c callRow
		if err := rows.Scan(&id, &c.requestID, &c.tool); err != nil {
			t.Fatalf("scan calls: %v", err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read calls: %v", err)
	}
	return out
}

func loadRequests(t *testing.T, db *sql.DB) map[string]requestRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT request_id, pending, input_tokens, cache_read_tokens,
		cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, context_tokens, output_tokens FROM requests`)
	if err != nil {
		t.Fatalf("read requests: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close requests: %v", err)
		}
	}()
	out := map[string]requestRow{}
	for rows.Next() {
		var id string
		var r requestRow
		if err := rows.Scan(&id, &r.pending, &r.input, &r.cacheRead, &r.cacheCreation, &r.cache5m, &r.cache1h, &r.context, &r.output); err != nil {
			t.Fatalf("scan requests: %v", err)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read requests: %v", err)
	}
	return out
}

// completenessProblems lists every id of the transcripts the store lacks.
func completenessProblems(t *testing.T, db *sql.DB, main transcript, subs map[string]transcript) []string {
	t.Helper()
	calls, requests := loadCalls(t, db), loadRequests(t, db)
	var problems []string
	seenMessage := map[string]bool{}
	for _, use := range allUses(main, subs) {
		call, ok := calls[use.ID]
		if !ok {
			problems = append(problems, fmt.Sprintf("calls.tool_use_id: no row for %s (%s)", use.ID, use.Name))
			continue
		}
		if call.requestID != use.MessageID {
			problems = append(problems, fmt.Sprintf("calls.request_id: for %s (%s): got %q, want %q (the transcript's message)", use.ID, use.Name, call.requestID, use.MessageID))
		}
		if seenMessage[use.MessageID] {
			continue
		}
		seenMessage[use.MessageID] = true
		req, ok := requests[use.MessageID]
		if !ok {
			problems = append(problems, fmt.Sprintf("requests.request_id: no row for %s (carrying tool_use %s)", use.MessageID, use.ID))
			continue
		}
		if !req.pending.Valid || req.pending.Int64 != 0 {
			problems = append(problems, fmt.Sprintf("requests.pending: for %s: got %v, want 0", use.MessageID, req.pending))
		}
		split := map[string]sql.NullInt64{
			"input_tokens": req.input, "cache_read_tokens": req.cacheRead, "cache_creation_tokens": req.cacheCreation,
			"cache_creation_5m_tokens": req.cache5m, "cache_creation_1h_tokens": req.cache1h,
			"context_tokens": req.context, "output_tokens": req.output,
		}
		for name, v := range split {
			if !v.Valid {
				problems = append(problems, fmt.Sprintf("requests.%s: for %s: got NULL, want a count", name, use.MessageID))
			}
		}
		if req.input.Valid && req.cacheRead.Valid && req.cacheCreation.Valid && req.context.Valid &&
			req.context.Int64 != req.input.Int64+req.cacheRead.Int64+req.cacheCreation.Int64 {
			problems = append(problems, fmt.Sprintf("requests.context_tokens: for %s: got %d, want %d (input + cache read + cache creation)", use.MessageID, req.context.Int64, req.input.Int64+req.cacheRead.Int64+req.cacheCreation.Int64))
		}
		if req.cacheCreation.Valid && req.cache5m.Valid && req.cache1h.Valid &&
			req.cache5m.Int64+req.cache1h.Int64 != req.cacheCreation.Int64 {
			problems = append(problems, fmt.Sprintf("requests.cache_creation_tokens: for %s: got %d, want %d (5m + 1h cache creation)", use.MessageID, req.cacheCreation.Int64, req.cache5m.Int64+req.cache1h.Int64))
		}
	}
	return problems
}

// agentProblems lists the Agent tool_uses and sub-agent transcripts the store
// lacks an agents or agent_turns row for.
func agentProblems(t *testing.T, db *sql.DB, main transcript, subs map[string]transcript) []string {
	t.Helper()
	var problems []string
	for _, use := range main.uses {
		if use.Name != "Agent" && use.Name != "Task" {
			continue
		}
		if count(t, db, "SELECT COUNT(*) FROM agents WHERE parent_tool_use_id = ?", use.ID) == 0 {
			problems = append(problems, fmt.Sprintf("agents.parent_tool_use_id: 0 rows for Agent tool_use %s, want at least 1", use.ID))
		}
	}
	for id := range subs {
		if count(t, db, "SELECT COUNT(*) FROM agents WHERE agent_id = ?", id) == 0 {
			problems = append(problems, fmt.Sprintf("agents.agent_id: 0 rows for sub-agent %s (it has a transcript), want at least 1", id))
		}
		if count(t, db, "SELECT COUNT(*) FROM agent_turns WHERE agent_id = ?", id) == 0 {
			problems = append(problems, fmt.Sprintf("agent_turns.agent_id: 0 rows for sub-agent %s, want at least 1", id))
		}
	}
	return problems
}

// assertNoFaults fails on any payload, store or binary fault row, printing
// every fault row first.
func assertNoFaults(t *testing.T, db *sql.DB) {
	t.Helper()
	rows := column(t, db, `SELECT 'ts=' || COALESCE(ts, '') || ' stage=' || COALESCE(stage, '') || ' session=' || COALESCE(session_id, '') ||
		' tool_use=' || COALESCE(tool_use_id, '') || ' error=' || COALESCE(error, '') FROM faults ORDER BY ts`)
	for _, row := range rows {
		t.Logf("fault row: %s", row)
	}
	// A terminated row is the truthful record of a hook cut short by a
	// headless exit: logged above, never a failure.
	wantCount(t, db, "faults", "stage IN ('payload', 'store', 'binary')", 0)
}

// unexpectedMissed is every line of a missed.log whose reason, the third tab
// field, does not start with "terminated by ".
func unexpectedMissed(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "terminated by ") {
			out = append(out, line)
		}
	}
	return out
}

// assertSilentStream checks the stream-json output of a session: callmeter's
// hooks added nothing to it.
func assertSilentStream(t *testing.T, label string, stream []byte) {
	t.Helper()
	sessionStarts := 0
	for i, line := range bytes.Split(stream, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if bytes.Contains(line, []byte("additionalContext")) || bytes.Contains(line, []byte("hook_additional_context")) {
			t.Errorf("%s: stream line %d carries hook-added context", label, i+1)
		}
		var event struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			HookEvent string `json:"hook_event"`
			Output    string `json:"output"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			ExitCode  *int   `json:"exit_code"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("%s: stream line %d is not JSON: %v", label, i+1, err)
		}
		if event.Type != "system" || event.Subtype != "hook_response" {
			continue
		}
		if event.HookEvent == "SessionStart" {
			sessionStarts++
		}
		if event.Output != "" || event.Stdout != "" || event.Stderr != "" {
			t.Errorf("%s: stream line %d: a %s hook printed output (%d bytes), want none", label, i+1, event.HookEvent, len(event.Output)+len(event.Stdout)+len(event.Stderr))
		}
		if event.ExitCode != nil && *event.ExitCode != 0 {
			t.Errorf("%s: stream line %d: a %s hook exited %d", label, i+1, event.HookEvent, *event.ExitCode)
		}
	}
	if sessionStarts == 0 {
		t.Errorf("%s: the stream shows no SessionStart hook response, so its silence proves nothing", label)
	}
}

// storeBytes is every byte callmeter keeps for a home: the store, its WAL and the log.
func storeBytes(t *testing.T, home string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, path := range []string{paths.Store(home), paths.Store(home) + "-wal", paths.Log(home)} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out[path] = data
	}
	if _, ok := out[paths.Store(home)]; !ok {
		t.Fatalf("the run left no store at %s", paths.Store(home))
	}
	return out
}

// storeWithoutCalls is the bytes of a vacuumed copy of the store whose named
// calls carry no input, no error and no command parts: what a scan for text
// the model itself put into those commands must not count.
func storeWithoutCalls(t *testing.T, home string, ids map[string]bool) []byte {
	t.Helper()
	dir := t.TempDir()
	copyPath, scrubbedPath := filepath.Join(dir, "copy.db"), filepath.Join(dir, "scrubbed.db")
	if _, err := openStore(t, home).Exec("VACUUM INTO ?", copyPath); err != nil {
		t.Fatalf("copy the store: %v", err)
	}
	db, err := sqlitedb.OpenReadWrite(copyPath, 10*time.Second)
	if err != nil {
		t.Fatalf("open the store copy: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store copy: %v", err)
		}
	}()
	for id := range ids {
		for _, statement := range []string{
			"UPDATE calls SET input = NULL, error = NULL WHERE tool_use_id = ?",
			"DELETE FROM command_parts WHERE tool_use_id = ?",
		} {
			if _, err := db.Exec(statement, id); err != nil {
				t.Fatalf("scrub call %s in the store copy: %v", id, err)
			}
		}
	}
	if _, err := db.Exec("VACUUM INTO ?", scrubbedPath); err != nil {
		t.Fatalf("vacuum the scrubbed copy: %v", err)
	}
	return readAll(t, scrubbedPath)
}

func TestLiveSessionFillsEveryTable(t *testing.T) {
	requireE2E(t)
	bin := builtBinary(t)
	plugin := pluginDir(t)
	r := newLiveRun(t, "run1")
	r.createProject(t)

	first := r.claude(t, "run1", plugin, run1Prompt, "", "CALLMETER_BIN="+bin)
	settle(t, r.home)
	main, subs := transcripts(t, first.id)
	t.Logf("run1: %d tool_use blocks in the main transcript, %d sub-agent transcripts", len(main.uses), len(subs))
	if len(main.uses) == 0 {
		t.Fatalf("run1: the session made no tool call; its transcript %s proves nothing", main.path)
	}
	agentRan := slices.ContainsFunc(main.uses, func(u toolUse) bool { return u.Name == "Agent" || u.Name == "Task" })

	t.Run("lifecycle", func(t *testing.T) {
		db := openStore(t, r.home)
		for _, want := range []string{"SessionStart", "UserPromptSubmit", "InstructionsLoaded", "SessionEnd"} {
			wantSome(t, db, "events", "event = ? AND session_id = ?", want, first.id)
		}
		wantSome(t, db, "events", "event = 'SessionStart' AND source = 'startup' AND session_id = ?", first.id)
		wantSome(t, db, "events", "event = 'UserPromptSubmit' AND prompt_bytes > 0 AND session_id = ?", first.id)
		if !wantCount(t, db, "sessions", "1 = 1", 1) {
			t.FailNow()
		}
		equals := func(v string) func(string) bool { return func(got string) bool { return got == v } }
		for _, check := range []struct {
			column, want string
			ok           func(string) bool
		}{
			{"engine", `"claude"`, equals("claude")},
			{"host", "a non-empty value", isSet},
			{"start_source", `"startup"`, equals("startup")},
			{"end_reason", "a non-empty value", isSet},
			{"model", "a non-empty value", isSet},
		} {
			wantCell(t, db, "sessions", check.column, "session_id", first.id, check.want, check.ok)
		}
	})

	t.Run("completeness", func(t *testing.T) {
		db := openStore(t, r.home)
		eventually(t, "calls and requests", func() []string { return completenessProblems(t, db, main, subs) },
			func() string { return rowsText(t, db, "SELECT tool_use_id, request_id, tool FROM calls") },
			func() string {
				return rowsText(t, db, `SELECT request_id, pending, input_tokens, cache_read_tokens, cache_creation_tokens,
					cache_creation_5m_tokens, cache_creation_1h_tokens, context_tokens, output_tokens FROM requests`)
			})
		if agentRan {
			eventually(t, "agents", func() []string { return agentProblems(t, db, main, subs) },
				func() string {
					return rowsText(t, db, "SELECT agent_id, parent_tool_use_id, session_id, stopped FROM agents")
				},
				func() string { return rowsText(t, db, "SELECT agent_id, COUNT(*) FROM agent_turns GROUP BY agent_id") })
		} else {
			t.Logf("the model skipped the Agent step: agents and agent_turns rows are not asserted")
		}
	})

	t.Run("turns", func(t *testing.T) {
		db := openStore(t, r.home)
		wantSome(t, db, "turns", "event = 'Stop' AND session_id = ?", first.id)
		if agentRan {
			// Every sub-agent that stopped fired one SubagentStop.
			stopped := count(t, db, "SELECT COUNT(*) FROM agents WHERE stopped IS NOT NULL AND session_id = ?", first.id)
			if n := count(t, db, "SELECT COUNT(*) FROM turns WHERE event = 'SubagentStop' AND session_id = ?", first.id); n < stopped || n == 0 {
				t.Errorf("turns: %d rows where event = 'SubagentStop' and session_id = %s, want at least %d (the stopped agents rows) and at least 1", n, first.id, stopped)
			}
		}
		wantCount(t, db, "turns", "event IN ('Stop', 'SubagentStop') AND last_assistant_message_bytes IS NULL", 0)
		wantCount(t, db, "turns", "event IN ('Stop', 'SubagentStop') AND typeof(last_assistant_message_bytes) <> 'integer'", 0)
		for _, name := range column(t, db, "SELECT name FROM pragma_table_info('turns')") {
			if strings.Contains(name, "message") && name != "last_assistant_message_bytes" {
				t.Errorf("turns.%s: a column that could hold message text, want only last_assistant_message_bytes", name)
			}
		}
	})

	t.Run("outcomes", func(t *testing.T) {
		db := openStore(t, r.home)
		for _, use := range main.uses {
			if use.IsError {
				t.Logf("%s %s: the transcript shows it failed; its outcome columns are not asserted", use.Name, use.ID)
				continue
			}
			eventually(t, use.Name+" "+use.ID, func() []string {
				switch {
				case use.Name == "Write" || use.Name == "Edit":
					if got := cell(t, db, "calls", "lines_added", "tool_use_id", use.ID); got == cellNull || got == cellNoRow {
						return []string{fmt.Sprintf("calls.lines_added: row of tool_use_id %s: got %s, want a count", use.ID, got)}
					}
				case use.Name == "Bash" && strings.Contains(use.Command, "go test"):
					if got := cell(t, db, "calls", "test_runner", "tool_use_id", use.ID); got != "go" {
						return []string{fmt.Sprintf("calls.test_runner: row of tool_use_id %s: got %q, want \"go\"", use.ID, got)}
					}
				case use.Name == "Bash" && strings.Contains(use.Command, "git commit"):
					var problems []string
					if got := cell(t, db, "calls", "commit_sha", "tool_use_id", use.ID); !isSet(got) {
						problems = append(problems, fmt.Sprintf("calls.commit_sha: row of tool_use_id %s: got %q, want a non-empty value", use.ID, got))
					}
					if got := cell(t, db, "calls", "commit_branch", "tool_use_id", use.ID); got != "main" {
						problems = append(problems, fmt.Sprintf("calls.commit_branch: row of tool_use_id %s: got %q, want \"main\"", use.ID, got))
					}
					return problems
				}
				return nil
			}, func() string {
				return rowsText(t, db, "SELECT tool_use_id, tool, lines_added, test_runner, commit_sha, commit_branch FROM calls WHERE tool_use_id = ?", use.ID)
			})
		}
		for _, step := range []struct{ label, tool, contains string }{
			{"Write", "Write", ""}, {"Edit", "Edit", ""}, {"go test", "Bash", "go test"}, {"git commit", "Bash", "git commit"},
		} {
			if !slices.ContainsFunc(main.uses, func(u toolUse) bool {
				return u.Name == step.tool && strings.Contains(u.Command, step.contains) && !u.IsError
			}) {
				t.Logf("the %s step is absent or failed in the transcript: not asserted", step.label)
			}
		}
		if shas := column(t, db, "SELECT commit_sha FROM calls WHERE commit_sha IS NOT NULL ORDER BY ts DESC LIMIT 1"); len(shas) == 1 {
			head := execute(t, "git rev-parse HEAD", time.Minute, []string{"git", "rev-parse", "HEAD"}, r.project, childEnv(gitEnv...), true)
			if !strings.HasPrefix(strings.TrimSpace(string(head.Stdout)), shas[0]) {
				t.Errorf("commit_sha %s is not a prefix of HEAD %s", shas[0], strings.TrimSpace(string(head.Stdout)))
			}
		}
	})

	t.Run("faults after run 1", func(t *testing.T) {
		assertNoFaults(t, openStore(t, r.home))
	})

	t.Run("no stdout", func(t *testing.T) {
		assertSilentStream(t, "run1", first.stream)
	})

	t.Run("compaction", func(t *testing.T) {
		db := openStore(t, r.home)
		agentsBefore := count(t, db, "SELECT COUNT(*) FROM agents")
		_, subsBefore := transcripts(t, first.id)
		if err := db.Close(); err != nil {
			t.Fatalf("close store before the compaction run: %v", err)
		}
		compact := r.claude(t, "run1-compact", plugin, "/compact", first.id, "CALLMETER_BIN="+bin)
		settle(t, r.home)
		assertSilentStream(t, "run1-compact", compact.stream)
		db = openStore(t, r.home)
		for _, event := range []string{"PreCompact", "PostCompact"} {
			wantSome(t, db, "events", `event = ? AND "trigger" = 'manual'`, event)
		}
		for _, source := range []string{"resume", "compact"} {
			wantSome(t, db, "events", "event = 'SessionStart' AND source = ?", source)
		}
		if n := count(t, db, "SELECT COUNT(*) FROM agents"); n != agentsBefore {
			t.Errorf("agents: %d rows after the compaction, want %d (the count before: the compaction agent got a row)", n, agentsBefore)
		}
		_, subsAfter := transcripts(t, first.id)
		for id := range subsAfter {
			if _, known := subsBefore[id]; known {
				continue
			}
			wantCount(t, db, "agents", "agent_id = ?", 0, id)
		}
	})

	t.Run("privacy", func(t *testing.T) {
		if !bytes.Contains(readAll(t, main.path), []byte(sentinel)) {
			t.Fatalf("the sentinel is not in the session's own transcript %s: the prompt did not carry it", main.path)
		}
		files := storeBytes(t, r.home)
		copied := map[string]bool{}
		for _, use := range allUses(main, subs) {
			if strings.Contains(use.Command, sentinel) {
				copied[use.ID] = true
			}
		}
		if len(copied) > 0 {
			// The model copied the sentinel into a command, whose text a call
			// keeps by design: scan a copy of the store without those calls'
			// text, so every other place is still proven free of it.
			t.Logf("the model copied the sentinel into %d call(s); scanning the store without their text", len(copied))
			delete(files, paths.Store(r.home))
			delete(files, paths.Store(r.home)+"-wal")
			files["the store less the copied calls' text"] = storeWithoutCalls(t, r.home, copied)
		}
		for path, data := range files {
			if bytes.Contains(bytes.ToLower(data), []byte(sentinel)) {
				t.Errorf("%s holds the prompt-only word %q", path, sentinel)
			}
		}
	})

	t.Run("reports", func(t *testing.T) {
		for _, topic := range reportTopics {
			assertReport(t, r, bin, topic)
		}
	})

	t.Run("faults at the end", func(t *testing.T) {
		assertNoFaults(t, openStore(t, r.home))
	})
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// assertReport runs one topic as text and as --json over the run's store.
func assertReport(t *testing.T, r *liveRun, bin, topic string) {
	t.Helper()
	text := r.callmeter(t, bin, "report", topic)
	if text.ExitCode != 0 {
		t.Errorf("report %s: exit %d: %s", topic, text.ExitCode, tail(text.Stderr))
		return
	}
	asJSON := r.callmeter(t, bin, "report", topic, "--json")
	if asJSON.ExitCode != 0 {
		t.Errorf("report %s --json: exit %d: %s", topic, asJSON.ExitCode, tail(asJSON.Stderr))
		return
	}
	var got struct {
		Topic   string     `json:"topic"`
		Store   string     `json:"store"`
		Title   string     `json:"title"`
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
		Notes   []string   `json:"notes"`
	}
	if err := json.Unmarshal(asJSON.Stdout, &got); err != nil {
		t.Errorf("report %s --json: not one JSON object: %v: %s", topic, err, tail(asJSON.Stdout))
		return
	}
	if got.Topic != topic || got.Store != "present" {
		t.Errorf("report %s --json: topic %q, store %q, want %q and present", topic, got.Topic, got.Store, topic)
	}
	lines := strings.Split(strings.TrimRight(string(text.Stdout), "\n"), "\n")
	if len(lines) < 2 || lines[0] != got.Title {
		t.Errorf("report %s: text heading does not match the JSON title %q: %q", topic, got.Title, lines)
		return
	}
	notes := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "note: ") {
			notes++
		}
	}
	if notes != len(got.Notes) {
		t.Errorf("report %s: %d note lines in the text, %d notes in the JSON", topic, notes, len(got.Notes))
	}
	if len(got.Rows) == 0 {
		return // an empty window prints no header
	}
	if want := 2 + len(got.Rows) + notes; len(lines) != want {
		t.Errorf("report %s: %d text lines, want heading + header + %d rows + %d notes", topic, len(lines), len(got.Rows), notes)
		return
	}
	if header, columns := strings.Fields(lines[1]), strings.Fields(strings.Join(got.Columns, " ")); !slices.Equal(header, columns) {
		t.Errorf("report %s: text header %q, JSON columns %q", topic, header, columns)
	}
}

// copyTree copies dir into dst, keeping file modes.
func copyTree(t *testing.T, dir, dst string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatalf("copy %s to %s: %v", dir, dst, err)
	}
}

func TestLiveDownloadPath(t *testing.T) {
	requireE2E(t)
	bin := builtBinary(t)
	r := newLiveRun(t, "run2")
	r.createProject(t)
	version := pluginVersion(t, pluginDir(t))

	asset := fmt.Sprintf("callmeter_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
	binary := readAll(t, bin)
	srvRoot := filepath.Join(r.root, "srv")
	if err := os.MkdirAll(filepath.Join(srvRoot, "callmeter--v"+version), 0o755); err != nil {
		t.Fatalf("create the release directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srvRoot, "callmeter--v"+version, asset), binary, 0o755); err != nil {
		t.Fatalf("write the release asset: %v", err)
	}
	var served atomic.Int64
	files := http.FileServer(http.Dir(srvRoot))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/"+asset) {
			served.Add(1)
		}
		files.ServeHTTP(w, req)
	}))
	defer srv.Close()

	plugin := filepath.Join(r.root, "plugin")
	copyTree(t, pluginDir(t), plugin)
	sum := sha256.Sum256(binary)
	sums := hex.EncodeToString(sum[:]) + "  " + asset + "\n"
	if err := os.WriteFile(filepath.Join(plugin, "libexec", "SHA256SUMS"), []byte(sums), 0o644); err != nil {
		t.Fatalf("write the copied plugin's SHA256SUMS: %v", err)
	}

	second := r.claude(t, "run2", plugin, "Run Bash: echo ok", "", "CALLMETER_RELEASE_BASE="+srv.URL)
	settle(t, r.home)

	installed := paths.BinCache(r.home, version)
	if got, err := os.ReadFile(installed); err != nil {
		t.Errorf("the wrapper installed nothing at %s: %v", installed, err)
	} else if !bytes.Equal(got, binary) {
		t.Errorf("%s differs from the served build", installed)
	}
	if info, err := os.Stat(installed); err == nil && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable", installed)
	}
	if served.Load() == 0 {
		t.Errorf("the local release server never served %s", asset)
	}
	// A hook a headless exit cut short leaves a "terminated by" line; any
	// other line is a binary that did not run.
	if _, err := os.Stat(paths.Missed(r.home)); err == nil {
		for _, line := range unexpectedMissed(readAll(t, paths.Missed(r.home))) {
			t.Errorf("missed.log line whose reason does not start with %q: %q", "terminated by ", line)
		}
	} else if !os.IsNotExist(err) {
		t.Errorf("stat missed.log: %v", err)
	}
	db := openStore(t, r.home)
	eventually(t, "the Bash call", func() []string {
		if n := count(t, db, "SELECT COUNT(*) FROM calls WHERE tool = 'Bash' AND session_id = ?", second.id); n == 0 {
			return []string{"calls: 0 rows where tool = 'Bash' and session_id = " + second.id + ", want at least 1"}
		}
		return nil
	}, func() string {
		return rowsText(t, db, "SELECT tool_use_id, tool, session_id FROM calls")
	})
	assertNoFaults(t, db)
	assertSilentStream(t, "run2", second.stream)
}

func TestLogTailRendersTheLastLinesAbsenceAndErrors(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	long := filepath.Join(dir, "callmeter.log")
	writeFile(t, long, strings.Join(lines, "\n")+"\n")
	got := logTail(long, logTailLines)
	if !strings.HasPrefix(got, "--- last 40 lines of "+long+" ---\n") || !strings.HasSuffix(got, "\nline 100") || !strings.Contains(got, "\nline 61\n") || strings.Contains(got, "line 60\n") {
		t.Errorf("tail of a 100-line log: %q", got)
	}
	if got := logTail(filepath.Join(dir, "missed.log"), logTailLines); got != filepath.Join(dir, "missed.log")+": absent" {
		t.Errorf("tail of an absent file: %q", got)
	}
	empty := filepath.Join(dir, "empty.log")
	writeFile(t, empty, "")
	if got := logTail(empty, logTailLines); !strings.Contains(got, "empty") {
		t.Errorf("tail of an empty file: %q", got)
	}
	// A directory is readable as a path and unreadable as a file.
	if got := logTail(dir, logTailLines); !strings.HasPrefix(got, dir+": unreadable: ") {
		t.Errorf("tail of an unreadable path: %q", got)
	}
}

func TestLiveRunLogTailsNameBothLogs(t *testing.T) {
	r := &liveRun{home: t.TempDir()}
	writeFile(t, paths.Log(r.home), "hook said hello\n")
	got := r.logTails()
	if !strings.Contains(got, paths.Log(r.home)+" ---\nhook said hello") || !strings.Contains(got, paths.Missed(r.home)+": absent") {
		t.Errorf("log tails of a run with callmeter.log and no missed.log: %q", got)
	}
}

func TestUnexpectedMissedAcceptsOnlyTerminatedLines(t *testing.T) {
	data := "1700000000\tPreToolUse\tterminated by SIGTERM\n" +
		"1700000001\tunknown\tterminated by SIGHUP\n" +
		"\n" +
		"1700000002\tStop\tbinary exited 2\n" +
		"1700000003\tStop\n"
	got := unexpectedMissed([]byte(data))
	want := []string{"1700000002\tStop\tbinary exited 2", "1700000003\tStop"}
	if !slices.Equal(got, want) {
		t.Errorf("unexpected missed lines %q, want %q", got, want)
	}
	if got := unexpectedMissed(nil); len(got) != 0 {
		t.Errorf("unexpected lines of an empty log: %q", got)
	}
}

func TestCellAndRowsTextNameWhatTheyFound(t *testing.T) {
	db, err := sqlitedb.OpenReadWrite(filepath.Join(t.TempDir(), "probe.db"), time.Second)
	if err != nil {
		t.Fatalf("open probe database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close probe database: %v", err)
		}
	})
	if _, err := db.Exec(`CREATE TABLE sessions (session_id TEXT, engine TEXT, host TEXT);
		INSERT INTO sessions VALUES ('s1', 'claude', NULL)`); err != nil {
		t.Fatalf("seed probe database: %v", err)
	}
	if got := cell(t, db, "sessions", "engine", "session_id", "s1"); got != "claude" {
		t.Errorf("engine of s1: %q", got)
	}
	if got := cell(t, db, "sessions", "host", "session_id", "s1"); got != cellNull || isSet(got) {
		t.Errorf("host of s1: %q", got)
	}
	if got := cell(t, db, "sessions", "engine", "session_id", "s2"); got != cellNoRow || isSet(got) {
		t.Errorf("engine of s2: %q", got)
	}
	if got := rowsText(t, db, "SELECT session_id, host FROM sessions"); got != "  session_id | host\n  s1 | NULL" {
		t.Errorf("rows text: %q", got)
	}
	if got := rowsText(t, db, "SELECT session_id FROM sessions WHERE session_id = 'none'"); !strings.Contains(got, "no rows") {
		t.Errorf("rows text of an empty result: %q", got)
	}
	if got := rowsText(t, db, "SELECT nope FROM sessions"); !strings.Contains(got, "failed") {
		t.Errorf("rows text of a broken query: %q", got)
	}
}
