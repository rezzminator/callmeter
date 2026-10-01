package wrappertest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const version = "0.1.0"

// fakeBinary is the asset every download serves: a sh script. Under `hook` it
// prints nothing and records its stdin; otherwise it prints its arguments and
// its stdin. FAKE_EXIT sets its exit status.
const fakeBinary = `#!/bin/sh
in=$(cat)
if [ "$1" = hook ]; then
  printf '%s' "$in" > "$CALLMETER_HOME/hook-stdin"
  exit "${FAKE_EXIT:-0}"
fi
printf 'fake-callmeter args=%s stdin=%s\n' "$*" "$in"
`

// rigOpts bends one part of the default rig: a good server, a matching
// SHA256SUMS, an immediate answer.
type rigOpts struct {
	served     string        // bytes the server sends for the asset
	summed     string        // bytes whose sha256 SHA256SUMS records
	status     int           // the server's status for the asset
	delay      time.Duration // the server's wait before it answers
	serverDown bool          // the server is closed before the wrapper runs
	pluginJSON string        // the plugin.json the wrapper reads
	sums       string        // the whole SHA256SUMS, replacing the derived one
	noSums     bool          // no SHA256SUMS file at all
}

// rig is one plugin root, one CALLMETER_HOME and one download server.
type rig struct {
	t       *testing.T
	root    string
	home    string
	asset   string
	base    string
	hits    atomic.Int32
	pathEnv string
	cwd     string
}

type result struct {
	stdout  string
	stderr  string
	code    int
	elapsed time.Duration
}

func assetName(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Fatalf("unsupported GOOS %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Fatalf("unsupported GOARCH %s", runtime.GOARCH)
	}
	return fmt.Sprintf("callmeter_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
}

// sumsFor is a SHA256SUMS of the four release assets, sorted by name, where
// the rig's own asset carries sum and the others a made-up one.
func sumsFor(own, sum string) string {
	var b strings.Builder
	for i, platform := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		name := fmt.Sprintf("callmeter_%s_%s", version, platform)
		line := strings.Repeat(string(rune('a'+i)), 64)
		if name == own {
			line = sum
		}
		fmt.Fprintf(&b, "%s  %s\n", line, name)
	}
	return b.String()
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newRig(t *testing.T, opts rigOpts) *rig {
	t.Helper()
	if opts.served == "" {
		opts.served = fakeBinary
	}
	if opts.summed == "" {
		opts.summed = opts.served
	}
	if opts.status == 0 {
		opts.status = http.StatusOK
	}
	if opts.pluginJSON == "" {
		opts.pluginJSON = fmt.Sprintf("{\n  \"name\": \"callmeter\",\n  \"version\": %q\n}\n", version)
	}
	r := &rig{t: t, root: filepath.Join(t.TempDir(), "plugin"), home: filepath.Join(t.TempDir(), "home"), asset: assetName(t), pathEnv: os.Getenv("PATH"), cwd: t.TempDir()}

	src, err := os.ReadFile(filepath.Join("..", "..", "plugins", "callmeter", "libexec", "callmeter"))
	if err != nil {
		t.Fatalf("read the shipped wrapper: %v", err)
	}
	r.write(filepath.Join("libexec", "callmeter"), string(src), 0o755)
	r.write(filepath.Join(".claude-plugin", "plugin.json"), opts.pluginJSON, 0o644)
	sums := opts.sums
	if sums == "" {
		sums = sumsFor(r.asset, sha(opts.summed))
	}
	if !opts.noSums {
		r.write(filepath.Join("libexec", "SHA256SUMS"), sums, 0o644)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/callmeter--v"+version+"/"+r.asset {
			http.NotFound(w, req)
			return
		}
		r.hits.Add(1)
		time.Sleep(opts.delay)
		if opts.status != http.StatusOK {
			http.Error(w, "refused", opts.status)
			return
		}
		_, _ = w.Write([]byte(opts.served))
	}))
	t.Cleanup(server.Close)
	r.base = server.URL
	if opts.serverDown {
		server.Close()
	}
	return r
}

func (r *rig) write(rel, content string, mode os.FileMode) {
	r.t.Helper()
	path := filepath.Join(r.root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		r.t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		r.t.Fatalf("chmod %s: %v", path, err)
	}
}

// seed installs a script at the cache path of the given version.
func (r *rig) seed(ver, content string) string {
	r.t.Helper()
	path := filepath.Join(r.home, "bin", ver, "callmeter")
	if err := seedFile(path, content); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// seedFile writes an executable file by rename, so a reader never sees it half written.
func seedFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".seed"
	if err := os.WriteFile(tmp, []byte(content), 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *rig) cachePath() string  { return filepath.Join(r.home, "bin", version, "callmeter") }
func (r *rig) missedPath() string { return filepath.Join(r.home, "missed.log") }

func (r *rig) run(stdin string, extraEnv []string, args ...string) result {
	return r.runShell("sh", stdin, extraEnv, args...)
}

// runShell runs the wrapper under the named shell with a clean environment:
// only PATH, HOME, CALLMETER_HOME and CALLMETER_RELEASE_BASE, then extraEnv.
func (r *rig) runShell(shell, stdin string, extraEnv []string, args ...string) result {
	r.t.Helper()
	shellPath, err := exec.LookPath(shell)
	if err != nil {
		r.t.Fatalf("shell %s not found: %v", shell, err)
	}
	cmd := exec.Command(shellPath, append([]string{filepath.Join(r.root, "libexec", "callmeter")}, args...)...)
	cmd.Dir = r.cwd
	cmd.Env = append([]string{
		"PATH=" + r.pathEnv,
		"HOME=" + filepath.Join(filepath.Dir(r.home), "userhome"),
		"CALLMETER_HOME=" + r.home,
		"CALLMETER_RELEASE_BASE=" + r.base,
	}, extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	runErr := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		r.t.Fatalf("run the wrapper under %s: %v", shell, runErr)
	}
	return res
}

func (r *rig) missed() []string {
	r.t.Helper()
	data, err := os.ReadFile(r.missedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("read missed.log: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// wantMissed asserts missed.log holds exactly one line for event and reason,
// stamped with unix seconds near now.
func (r *rig) wantMissed(event, reason string) {
	r.t.Helper()
	lines := r.missed()
	if len(lines) != 1 {
		r.t.Fatalf("missed.log lines = %q, want exactly one", lines)
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 3 || !regexp.MustCompile(`^[0-9]+$`).MatchString(fields[0]) || fields[1] != event || fields[2] != reason {
		r.t.Fatalf("missed line = %q, want {unix seconds}\\t%s\\t%s", lines[0], event, reason)
	}
	var seconds int64
	if _, err := fmt.Sscan(fields[0], &seconds); err != nil || time.Now().Unix()-seconds > 30 || seconds-time.Now().Unix() > 30 {
		r.t.Fatalf("missed line time %q is not near now", fields[0])
	}
}

func (r *rig) wantNoCachedBinary() {
	r.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.home, "bin"))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		r.t.Fatalf("read bin dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || e.Name() == version {
			r.t.Fatalf("bin dir holds %s after a failed install", e.Name())
		}
	}
}

func (r *rig) wantNoLockOrTemp() {
	r.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.home, "bin"))
	if err != nil {
		r.t.Fatalf("read bin dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			r.t.Fatalf("bin dir holds leftover %s", e.Name())
		}
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func assertEqual(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

func TestWrapperIsExecutable(t *testing.T) {
	info, err := os.Stat(filepath.Join("..", "..", "plugins", "callmeter", "libexec", "callmeter"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("wrapper mode = %v, want 0755", info.Mode().Perm())
	}
}

// override: CALLMETER_BIN runs with its arguments and stdin, no download, no cache write.
func TestOverrideRunsTheNamedBinary(t *testing.T) {
	r := newRig(t, rigOpts{})
	override := filepath.Join(t.TempDir(), "override")
	writeFile(t, override, fakeBinary, 0o755)
	res := r.run("piped input", []string{"CALLMETER_BIN=" + override}, "report", "files", "--since", "7d")
	assertEqual(t, "stdout", res.stdout, "fake-callmeter args=report files --since 7d stdin=piped input\n")
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
	}
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0", r.hits.Load())
	}
	if _, err := os.Stat(r.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the override run touched CALLMETER_HOME (stat err %v)", err)
	}
}

func TestOverrideNotExecutableFallsThroughToTheCache(t *testing.T) {
	r := newRig(t, rigOpts{})
	override := filepath.Join(t.TempDir(), "override")
	writeFile(t, override, "#!/bin/sh\necho override-ran\n", 0o644)
	r.seed(version, "#!/bin/sh\necho cache-ran\n")
	res := r.run("", []string{"CALLMETER_BIN=" + override}, "version")
	assertEqual(t, "stdout", res.stdout, "cache-ran\n")
}

// cache hit: execs the cached binary, no network.
func TestCacheHitRunsTheCachedBinary(t *testing.T) {
	r := newRig(t, rigOpts{})
	// A cache hit needs no download tool: curl and the sha256 tools are off the PATH.
	r.pathEnv = pathWithout(t, "curl", "shasum", "sha256sum")
	r.seed(version, fakeBinary)
	res := r.run("from stdin", nil, "report", "faults")
	assertEqual(t, "stdout", res.stdout, "fake-callmeter args=report faults stdin=from stdin\n")
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0", r.hits.Load())
	}
}

// download ok: installs at the cache path, executable, by atomic rename.
func TestDownloadInstallsAndRuns(t *testing.T) {
	for _, shell := range []string{"sh", "dash", "bash"} {
		t.Run(shell, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			res := r.runShell(shell, "in", nil, "report", "files")
			assertEqual(t, "stdout", res.stdout, "fake-callmeter args=report files stdin=in\n")
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
			}
			info, err := os.Stat(r.cachePath())
			if err != nil {
				t.Fatalf("binary not installed: %v", err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("cached mode = %v, want 0755", info.Mode().Perm())
			}
			r.wantNoLockOrTemp()
			if r.hits.Load() != 1 {
				t.Fatalf("server hit %d times, want 1", r.hits.Load())
			}
			again := r.runShell(shell, "", nil, "version")
			if again.code != 0 || r.hits.Load() != 1 {
				t.Fatalf("second run: exit %d, hits %d; want a cache hit", again.code, r.hits.Load())
			}
		})
	}
}

// checksum mismatch: under hook exit 0, empty stdout, one missed line, nothing cached, no retry.
func TestChecksumMismatchUnderHook(t *testing.T) {
	r := newRig(t, rigOpts{served: "#!/bin/sh\necho tampered\n", summed: fakeBinary})
	res := r.run(`{"hook_event_name":"PreToolUse"}`, nil, "hook")
	if res.code != 0 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
	}
	r.wantMissed("PreToolUse", "checksum mismatch")
	r.wantNoCachedBinary()
	if r.hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1 (no retry)", r.hits.Load())
	}
}

// download fails: a 404 and a server that is down.
func TestDownloadFailsUnderHook(t *testing.T) {
	cases := map[string]rigOpts{
		"404":         {status: http.StatusNotFound},
		"server down": {serverDown: true},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, opts)
			res := r.run(`{"hook_event_name":"PostToolUse"}`, nil, "hook")
			if res.code != 0 || res.stdout != "" {
				t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
			}
			r.wantMissed("PostToolUse", "download failed")
			r.wantNoCachedBinary()
		})
	}
}

// other subcommand fails: stderr line, exit 1, no missed line.
func TestDownloadFailsUnderReport(t *testing.T) {
	r := newRig(t, rigOpts{status: http.StatusNotFound})
	res := r.run("", nil, "report", "files")
	if res.code != 1 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 1 and empty", res.code, res.stdout)
	}
	assertEqual(t, "stderr", res.stderr, "callmeter: binary unavailable: download failed\n")
	if lines := r.missed(); lines != nil {
		t.Fatalf("missed.log = %q, want none", lines)
	}
}

// concurrent first run: 8 wrappers, the asset served once, all 8 run the binary.
func TestConcurrentFirstRunDownloadsOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{delay: 1500 * time.Millisecond})
	const runs = 8
	results := make([]result, runs)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = r.run("", nil, "report", "files")
		}()
	}
	close(start)
	wg.Wait()
	for i, res := range results {
		if res.code != 0 || res.stdout != "fake-callmeter args=report files stdin=\n" {
			t.Fatalf("run %d: exit %d stdout %q stderr %q", i, res.code, res.stdout, res.stderr)
		}
	}
	if r.hits.Load() != 1 {
		t.Fatalf("server served the asset %d times, want 1", r.hits.Load())
	}
	r.wantNoLockOrTemp()
}

// concurrent first run, error handling: a waiter gives up after 50 s. Real
// time: the bound is the contract, and there is no knob to shorten it.
func TestLockWaiterTimesOutAfterFiftySeconds(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	res := r.run("", nil, "report", "files")
	if res.code != 1 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 1 and empty", res.code, res.stdout)
	}
	assertEqual(t, "stderr", res.stderr, "callmeter: binary unavailable: lock wait timed out\n")
	if res.elapsed < 49*time.Second || res.elapsed > 58*time.Second {
		t.Fatalf("waited %v, want about 50 s", res.elapsed)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a waiter removed a lock it never held: %v", err)
	}
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0", r.hits.Load())
	}
}

// stale lock: older than 120 s, removed, the download proceeds.
func TestStaleLockIsRemoved(t *testing.T) {
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-200 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	res := r.run("", nil, "report", "files")
	if res.code != 0 || res.stdout != "fake-callmeter args=report files stdin=\n" {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if r.hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", r.hits.Load())
	}
	r.wantNoLockOrTemp()
}

// A 100 s old lock is a live holder's: it is waited for, never removed.
func TestFreshEnoughLockIsKept(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	young := time.Now().Add(-100 * time.Second)
	if err := os.Chtimes(lock, young, young); err != nil {
		t.Fatal(err)
	}
	// The holder finishes its install two seconds in.
	seeded := make(chan error, 1)
	go func() {
		time.Sleep(2 * time.Second)
		seeded <- seedFile(r.cachePath(), fakeBinary)
	}()
	res := r.run("", nil, "report", "files")
	if err := <-seeded; err != nil {
		t.Fatalf("the holder's install: %v", err)
	}
	if res.code != 0 || res.stdout != "fake-callmeter args=report files stdin=\n" {
		t.Fatalf("exit %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0: the lock was the holder's", r.hits.Load())
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a waiter removed a live holder's lock: %v", err)
	}
}

// A hook killed before its binary ran — Claude Code ends a hook past its
// timeout, here while it waits on another run's lock — is a missed event: one
// missed.log line, exit 0, never a silent loss.
func TestHookKilledBeforeTheBinaryIsMissed(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(r.root, "libexec", "callmeter"), "hook")
	cmd.Dir = r.cwd
	cmd.Env = []string{
		"PATH=" + r.pathEnv,
		"HOME=" + filepath.Join(filepath.Dir(r.home), "userhome"),
		"CALLMETER_HOME=" + r.home,
		"CALLMETER_RELEASE_BASE=" + r.base,
	}
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"SessionEnd","reason":"other"}`)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the wrapper: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the wrapper: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("a killed hook must exit 0: %v", err)
	}
	assertEqual(t, "stdout", stdout.String(), "")
	r.wantMissed("SessionEnd", "killed by signal")
}

// old versions: removed after the new version installs.
func TestOldVersionsAreRemoved(t *testing.T) {
	r := newRig(t, rigOpts{})
	old := r.seed("0.0.9", "#!/bin/sh\necho old\n")
	res := r.run("", nil, "version")
	if res.code != 0 {
		t.Fatalf("exit %d stderr %q", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Dir(old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bin/0.0.9 still present (stat err %v)", err)
	}
	if _, err := os.Stat(r.cachePath()); err != nil {
		t.Fatalf("new version missing: %v", err)
	}
}

// CALLMETER_HOME unset: XDG_STATE_HOME when absolute, else $HOME/.local/state.
func TestHomeResolution(t *testing.T) {
	cases := []struct {
		name string
		env  func(userHome, xdg string) []string
		want func(userHome, xdg string) string
	}{
		{"absolute XDG_STATE_HOME", func(_, xdg string) []string { return []string{"XDG_STATE_HOME=" + xdg} }, func(_, xdg string) string { return xdg }},
		{"relative XDG_STATE_HOME is ignored", func(_, _ string) []string { return []string{"XDG_STATE_HOME=relative/state"} }, func(h, _ string) string { return filepath.Join(h, ".local", "state") }},
		{"no XDG_STATE_HOME", func(_, _ string) []string { return nil }, func(h, _ string) string { return filepath.Join(h, ".local", "state") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			userHome := filepath.Join(filepath.Dir(r.home), "userhome")
			xdg := filepath.Join(t.TempDir(), "xdg")
			cached := filepath.Join(tc.want(userHome, xdg), "callmeter", "bin", version, "callmeter")
			if err := seedFile(cached, "#!/bin/sh\necho resolved-here\n"); err != nil {
				t.Fatal(err)
			}
			res := r.run("", append([]string{"CALLMETER_HOME="}, tc.env(userHome, xdg)...), "version")
			assertEqual(t, "stdout", res.stdout, "resolved-here\n")
			if _, err := os.Stat(filepath.Join(r.cwd, "relative")); !os.IsNotExist(err) {
				t.Errorf("the wrapper's working directory gained a relative entry (stat error %v), want none", err)
			}
		})
	}
}

// missed line: the event comes from stdin, `unknown` when there is none.
func TestMissedLineEvent(t *testing.T) {
	cases := []struct {
		name, stdin, event string
	}{
		{"compact", `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash"}`, "PreToolUse"},
		{"spaced and multi-line", "{\n  \"session_id\": \"s\",\n  \"hook_event_name\" : \"SessionEnd\"\n}\n", "SessionEnd"},
		{"escaped decoy inside a prompt first", `{"prompt":"say \"hook_event_name\":\"Fake\" now","hook_event_name":"UserPromptSubmit"}`, "UserPromptSubmit"},
		{"first of two", `{"hook_event_name":"Stop","nested":{"hook_event_name":"Other"}}`, "Stop"},
		{"no key", `{"session_id":"s"}`, "unknown"},
		{"empty stdin", ``, "unknown"},
		{"not json", "\x00\x01\x02 garbage \xff", "unknown"},
		{"value outside the event alphabet", `{"hook_event_name":"a\tb"}`, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{status: http.StatusNotFound})
			res := r.run(tc.stdin, nil, "hook")
			if res.code != 0 || res.stdout != "" {
				t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
			}
			r.wantMissed(tc.event, "download failed")
		})
	}
}

// missed line: every failure appends its own line, in order.
func TestMissedLinesAppend(t *testing.T) {
	r := newRig(t, rigOpts{status: http.StatusNotFound})
	r.run(`{"hook_event_name":"Stop"}`, nil, "hook")
	r.run(`{"hook_event_name":"SubagentStop"}`, nil, "hook")
	lines := r.missed()
	if len(lines) != 2 || !strings.Contains(lines[0], "\tStop\t") || !strings.Contains(lines[1], "\tSubagentStop\t") {
		t.Fatalf("missed.log = %q, want a Stop line then a SubagentStop line", lines)
	}
}

// SessionStart: stdout stays empty and exit 0 on every outcome.
func TestSessionStartStdoutStaysEmpty(t *testing.T) {
	const payload = `{"hook_event_name":"SessionStart","source":"startup"}`
	cases := []struct {
		name   string
		opts   rigOpts
		reason string
	}{
		{"download failed", rigOpts{status: http.StatusNotFound}, "download failed"},
		{"checksum mismatch", rigOpts{served: "#!/bin/sh\necho tampered\n", summed: fakeBinary}, "checksum mismatch"},
		{"no checksum for the asset", rigOpts{sums: strings.Repeat("a", 64) + "  callmeter_9.9.9_linux_amd64\n"}, "no checksum for " + assetName(t)},
		{"no plugin version", rigOpts{pluginJSON: "{}\n"}, "no plugin version"},
		{"unreadable checksums", rigOpts{noSums: true}, "checksums file unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.opts)
			res := r.run(payload, nil, "hook")
			if res.code != 0 || res.stdout != "" {
				t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
			}
			r.wantMissed("SessionStart", tc.reason)
		})
	}
	t.Run("success", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		res := r.run(payload, nil, "hook")
		if res.code != 0 || res.stdout != "" {
			t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
		}
		data, err := os.ReadFile(filepath.Join(r.home, "hook-stdin"))
		if err != nil || string(data) != payload {
			t.Fatalf("the binary's stdin = %q (err %v), want the untouched payload", data, err)
		}
		if lines := r.missed(); lines != nil {
			t.Fatalf("missed.log = %q, want none", lines)
		}
	})
}

// A binary that crashes or cannot start under hook is a missed line, exit 0.
func TestHookBinaryFailureIsMissed(t *testing.T) {
	t.Run("exits non-zero", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.seed(version, fakeBinary)
		res := r.run(`{"hook_event_name":"Stop"}`, []string{"FAKE_EXIT=3"}, "hook")
		if res.code != 0 || res.stdout != "" {
			t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
		}
		r.wantMissed("unknown", "binary exited 3")
	})
	t.Run("cannot start", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.seed(version, "\x01\x02\x03 not an executable \x00\x00")
		res := r.run(`{"hook_event_name":"Stop"}`, nil, "hook")
		if res.code != 0 || res.stdout != "" {
			t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
		}
		lines := r.missed()
		if len(lines) != 1 || !regexp.MustCompile(`^[0-9]+\tunknown\tbinary exited (126|127|1)$`).MatchString(lines[0]) {
			t.Fatalf("missed.log = %q, want one `binary exited` line", lines)
		}
	})
}

// A tool the wrapper needs and the PATH lacks is a named reason, never a hang.
func TestMissingToolsAreNamed(t *testing.T) {
	cases := []struct {
		name   string
		omit   []string
		reason string
	}{
		{"no curl", []string{"curl"}, "curl not found"},
		{"no sha256 tool", []string{"shasum", "sha256sum"}, "no sha256 tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.pathEnv = pathWithout(t, tc.omit...)
			res := r.run("", nil, "report", "files")
			if res.code != 1 || res.stdout != "" {
				t.Fatalf("exit %d stdout %q, want 1 and empty", res.code, res.stdout)
			}
			assertEqual(t, "stderr", res.stderr, "callmeter: binary unavailable: "+tc.reason+"\n")
			if r.hits.Load() != 0 {
				t.Fatalf("server hit %d times, want 0", r.hits.Load())
			}
		})
	}
}

// pathWithout is a PATH directory of symlinks to every tool the wrapper uses,
// minus the omitted ones.
func pathWithout(t *testing.T, omit ...string) string {
	t.Helper()
	skip := map[string]bool{}
	for _, name := range omit {
		skip[name] = true
	}
	dir := t.TempDir()
	required := []string{"sh", "awk", "grep", "sed", "cat", "mkdir", "mv", "rm", "rmdir", "chmod", "date", "stat", "uname", "dirname", "sleep"}
	optional := []string{"curl", "shasum", "sha256sum"}
	for _, name := range append(append([]string{}, required...), optional...) {
		if skip[name] {
			continue
		}
		path, err := exec.LookPath(name)
		if err != nil {
			if contains(optional, name) {
				continue
			}
			t.Fatalf("tool %s not found: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
