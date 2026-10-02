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
	"slices"
	"strconv"
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
// its stdin. FAKE_EXIT sets its exit status. FAKE_DIR names a directory where
// it records its pid (`pid`); with FAKE_SLEEP it also marks `started`, waits
// that many seconds and marks `finished`, or marks `term` and exits 0 when it
// receives SIGTERM first.
const fakeBinary = `#!/bin/sh
in=$(cat)
if [ -n "$FAKE_DIR" ]; then
  printf '%s' "$$" > "$FAKE_DIR/pid"
fi
if [ -n "$FAKE_SLEEP" ]; then
  sleep "$FAKE_SLEEP" &
  sleeper=$!
  trap 'kill "$sleeper" 2>/dev/null; printf term > "$FAKE_DIR/term"; exit 0' TERM
  : > "$FAKE_DIR/started"
  wait "$sleeper"
  printf done > "$FAKE_DIR/finished"
fi
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
	gate       chan struct{} // the first request gets half the asset, then waits for the gate to close
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
		hit := r.hits.Add(1)
		time.Sleep(opts.delay)
		if opts.status != http.StatusOK {
			http.Error(w, "refused", opts.status)
			return
		}
		if opts.gate != nil && hit == 1 {
			half := len(opts.served) / 2
			_, _ = w.Write([]byte(opts.served[:half]))
			w.(http.Flusher).Flush()
			select {
			case <-opts.gate:
			case <-req.Context().Done():
				return
			}
			_, _ = w.Write([]byte(opts.served[half:]))
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

// runShell runs the wrapper under the named shell to its end.
func (r *rig) runShell(shell, stdin string, extraEnv []string, args ...string) result {
	r.t.Helper()
	return r.start(shell, stdin, extraEnv, args...).wait()
}

// proc is one wrapper run in flight, in its own process group: a group signal
// reaches only it and what it started.
type proc struct {
	t              *testing.T
	shell          string
	cmd            *exec.Cmd
	stdout, stderr *bytes.Buffer
	began          time.Time
}

// start runs the wrapper under the named shell with a clean environment: only
// PATH, HOME, CALLMETER_HOME and CALLMETER_RELEASE_BASE, then extraEnv.
func (r *rig) start(shell, stdin string, extraEnv []string, args ...string) *proc {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &proc{t: r.t, shell: shell, cmd: cmd, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, began: time.Now()}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		r.t.Fatalf("start the wrapper under %s: %v", shell, err)
	}
	// A failed test leaves no stray run or fake binary behind.
	r.t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return p
}

func (p *proc) pid() int { return p.cmd.Process.Pid }

// signal sends sig to the wrapper's pid alone.
func (p *proc) signal(sig syscall.Signal) {
	p.t.Helper()
	if err := syscall.Kill(p.pid(), sig); err != nil {
		p.t.Fatalf("signal the wrapper: %v", err)
	}
}

// signalGroup sends sig to the wrapper's whole process group.
func (p *proc) signalGroup(sig syscall.Signal) {
	p.t.Helper()
	if err := syscall.Kill(-p.pid(), sig); err != nil {
		p.t.Fatalf("signal the wrapper's group: %v", err)
	}
}

func (p *proc) wait() result {
	p.t.Helper()
	runErr := p.cmd.Wait()
	res := result{stdout: p.stdout.String(), stderr: p.stderr.String(), elapsed: time.Since(p.began)}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		p.t.Fatalf("run the wrapper under %s: %v", p.shell, runErr)
	}
	return res
}

// waitFor polls until the condition holds, or fails the test after 15 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within 15 s", what)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	waitFor(t, path+" to appear", func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// fakeDir is a scratch directory the fake binary records into, and its
// FAKE_DIR entry.
func fakeDir(t *testing.T) (dir string, env string) {
	t.Helper()
	dir = t.TempDir()
	return dir, "FAKE_DIR=" + dir
}

// wantBinaryPid asserts the fake binary ran as process pid: it was exec'd by
// the wrapper, not started as its child.
func wantBinaryPid(t *testing.T, dir string, pid int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatalf("the fake binary recorded no pid: %v", err)
	}
	assertEqual(t, "the binary's pid", string(data), strconv.Itoa(pid))
}

// stampPath is the last-use stamp of a version's cache directory.
func (r *rig) stampPath(ver string) string {
	return filepath.Join(r.home, "bin", ver, ".last-use")
}

// wantStampNow asserts the version's stamp exists and was written just now.
func (r *rig) wantStampNow(ver string) {
	r.t.Helper()
	info, err := os.Stat(r.stampPath(ver))
	if err != nil {
		r.t.Fatalf("no last-use stamp for %s: %v", ver, err)
	}
	if age := time.Since(info.ModTime()); age > 30*time.Second || age < -30*time.Second {
		r.t.Fatalf("stamp for %s is %v old, want just written", ver, age)
	}
}

// ageStamp writes the version's stamp and sets its mtime d ago.
func (r *rig) ageStamp(ver string, d time.Duration) {
	r.t.Helper()
	writeFile(r.t, r.stampPath(ver), "1", 0o644)
	r.age(r.stampPath(ver), d)
}

// age sets a path's mtime d ago.
func (r *rig) age(path string, d time.Duration) {
	r.t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		r.t.Fatalf("age %s: %v", path, err)
	}
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
// with no session, stamped with unix seconds near now.
func (r *rig) wantMissed(event, reason string) {
	r.t.Helper()
	r.wantMissedSession(event, reason, "")
}

// wantMissedSession asserts missed.log holds exactly one line for event,
// reason and session, stamped with unix seconds near now; an empty session is
// a line without the session field.
func (r *rig) wantMissedSession(event, reason, session string) {
	r.t.Helper()
	lines := r.missed()
	if len(lines) != 1 {
		r.t.Fatalf("missed.log lines = %q, want exactly one", lines)
	}
	want := []string{event, reason}
	if session != "" {
		want = append(want, session)
	}
	fields := strings.Split(lines[0], "\t")
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(fields[0]) || !slices.Equal(fields[1:], want) {
		r.t.Fatalf("missed line = %q, want {unix seconds}\\t%s", lines[0], strings.Join(want, "\\t"))
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

// override exec: the override is handed off by exec under every subcommand, so
// the binary is the wrapper's own process.
func TestOverrideIsTheWrappersOwnProcess(t *testing.T) {
	for _, mode := range []string{"hook", "report"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			override := filepath.Join(t.TempDir(), "override")
			writeFile(t, override, fakeBinary, 0o755)
			dir, env := fakeDir(t)
			p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{"CALLMETER_BIN=" + override, env}, mode)
			res := p.wait()
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
			}
			wantBinaryPid(t, dir, p.pid())
		})
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

// cache-hit tools: a cache hit runs no external command, so it starts no
// process before the binary; with nothing on the PATH it still reads its
// version, execs the cached binary and leaves no missed line.
func TestCacheHitRunsNoExternalCommand(t *testing.T) {
	for _, shell := range []string{"sh", "dash", "bash"} {
		t.Run(shell, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.pathEnv = t.TempDir()
			r.seed(version, "#!/bin/sh\nprintf 'cache-ran %s\\n' \"$*\"\n")
			res := r.runShell(shell, `{"session_id":"s","hook_event_name":"Stop"}`, nil, "hook")
			if res.code != 0 || res.stdout != "cache-ran hook\n" {
				t.Fatalf("exit %d stdout %q stderr %q, want 0 and the cached binary's line", res.code, res.stdout, res.stderr)
			}
			if lines := r.missed(); lines != nil {
				t.Fatalf("missed.log = %q, want none", lines)
			}
		})
	}
}

// cache-hit exec: under hook and under report the cached binary is the
// wrapper's own process and the version's stamp is refreshed.
func TestCacheHitIsTheWrappersOwnProcessAndRefreshesTheStamp(t *testing.T) {
	for _, mode := range []string{"hook", "report"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.seed(version, fakeBinary)
			r.ageStamp(version, 3*24*time.Hour)
			dir, env := fakeDir(t)
			p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env}, mode)
			res := p.wait()
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
			}
			wantBinaryPid(t, dir, p.pid())
			r.wantStampNow(version)
			if lines := r.missed(); lines != nil {
				t.Fatalf("missed.log = %q, want none", lines)
			}
		})
	}
}

// cache-hit exec, error handling: a stamp that cannot be written never fails
// the run, under every shell. The stamp is a directory here, so no write can
// succeed, as root or not.
func TestUnwritableStampNeverFailsARun(t *testing.T) {
	for _, shell := range []string{"sh", "dash", "bash"} {
		t.Run(shell, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.seed(version, fakeBinary)
			if err := os.MkdirAll(r.stampPath(version), 0o755); err != nil {
				t.Fatal(err)
			}
			res := r.runShell(shell, "from stdin", nil, "report", "faults")
			assertEqual(t, "stdout", res.stdout, "fake-callmeter args=report faults stdin=from stdin\n")
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
			}
		})
	}
}

// lock-wait hit: the run that waits on another run's lock and finds the binary
// installed is handed off by exec too, with the stamp refreshed.
func TestLockWaitHitIsTheWrappersOwnProcess(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, env := fakeDir(t)
	p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env}, "hook")
	time.Sleep(1500 * time.Millisecond)
	if err := seedFile(r.cachePath(), fakeBinary); err != nil {
		t.Fatalf("the holder's install: %v", err)
	}
	res := p.wait()
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
	}
	wantBinaryPid(t, dir, p.pid())
	r.wantStampNow(version)
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0: the lock was the holder's", r.hits.Load())
	}
}

// The holder that finds the binary installed after taking the lock execs it
// too. A stat shim installs the binary while the run judges the lock stale,
// which is after its cache check and before its lock.
func TestHolderFindingTheInstalledBinaryIsTheWrappersOwnProcess(t *testing.T) {
	r := newRig(t, rigOpts{})
	lock := filepath.Join(r.home, "bin", ".lock-"+version)
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	r.age(lock, 300*time.Second)
	realStat, err := exec.LookPath("stat")
	if err != nil {
		t.Fatal(err)
	}
	r.pathEnv = pathWithout(t)
	shim := filepath.Join(r.pathEnv, "stat")
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(t.TempDir(), "staged")
	writeFile(t, staged, fakeBinary, 0o755)
	writeFile(t, shim, "#!/bin/sh\n/bin/mkdir -p \"$SHIM_DIR\" && /bin/cp \"$SHIM_SRC\" \"$SHIM_DIR/callmeter\" && /bin/chmod 755 \"$SHIM_DIR/callmeter\"\nexec "+realStat+" \"$@\"\n", 0o755)
	dir, env := fakeDir(t)
	p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env, "SHIM_SRC=" + staged, "SHIM_DIR=" + filepath.Dir(r.cachePath())}, "hook")
	res := p.wait()
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
	}
	wantBinaryPid(t, dir, p.pid())
	r.wantStampNow(version)
	if r.hits.Load() != 0 {
		t.Fatalf("server hit %d times, want 0: the binary was installed", r.hits.Load())
	}
}

// signal on a cache hit: a SIGTERM to the wrapper's pid is the binary's own
// SIGTERM, since the wrapper is gone once it hands off.
func TestSignalOnACacheHitReachesTheBinary(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	r.seed(version, fakeBinary)
	dir, env := fakeDir(t)
	p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env, "FAKE_SLEEP=10"}, "hook")
	waitForFile(t, filepath.Join(dir, "started"))
	p.signal(syscall.SIGTERM)
	res := p.wait()
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
	}
	waitForFile(t, filepath.Join(dir, "term"))
	if exists(filepath.Join(dir, "finished")) {
		t.Fatal("the binary ran to its end: it never received the signal")
	}
	if lines := r.missed(); lines != nil {
		t.Fatalf("missed.log = %q, want none", lines)
	}
}

// install stamp: an own download leaves the version's stamp.
func TestInstallWritesTheStamp(t *testing.T) {
	r := newRig(t, rigOpts{})
	res := r.run("", nil, "version")
	if res.code != 0 {
		t.Fatalf("exit = %d, stderr %q", res.code, res.stderr)
	}
	r.wantStampNow(version)
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
	r.wantStampNow(version)
}

// racing downloads: a lock taken over from a holder that is only slow (here
// held mid-download, as a suspended process is) leaves two downloads at once.
// Neither run may delete the other's temp file, and both run a verified binary.
func TestRacingDownloadsBothRunTheVerifiedBinary(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	r := newRig(t, rigOpts{gate: gate})
	t.Cleanup(release)
	bin := filepath.Join(r.home, "bin")

	first := r.start("sh", "", nil, "report", "files")
	// The first run holds the lock and its temp file has the first half of the asset.
	waitFor(t, "the first run's temp file", func() bool {
		temps, _ := filepath.Glob(filepath.Join(bin, ".tmp-*"))
		return len(temps) == 1
	})
	// Its lock is taken over: removed, as a waiter does to a stale lock.
	if err := os.Remove(filepath.Join(bin, ".lock-"+version)); err != nil {
		t.Fatalf("take over the first run's lock: %v", err)
	}
	second := r.start("sh", "", nil, "report", "files")
	resSecond := second.wait()
	release()
	resFirst := first.wait()

	for name, res := range map[string]result{"first": resFirst, "second": resSecond} {
		if res.code != 0 || res.stdout != "fake-callmeter args=report files stdin=\n" {
			t.Fatalf("%s run: exit %d stdout %q stderr %q", name, res.code, res.stdout, res.stderr)
		}
	}
	got, err := os.ReadFile(r.cachePath())
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	assertEqual(t, "the cached bytes", string(got), fakeBinary)
	if r.hits.Load() != 2 {
		t.Fatalf("server served the asset %d times, want 2", r.hits.Load())
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
	p := r.start("sh", `{"session_id":"s-killed","hook_event_name":"SessionEnd","reason":"other"}`, nil, "hook")
	time.Sleep(1500 * time.Millisecond)
	p.signal(syscall.SIGTERM)
	res := p.wait()
	if res.code != 0 {
		t.Fatalf("a killed hook must exit 0, got %d", res.code)
	}
	assertEqual(t, "stdout", res.stdout, "")
	r.wantMissedSession("SessionEnd", "killed by signal", "s-killed")
}

// kill during download: the signal is handled once curl, waiting on a slow
// server, returns: exit 0 and one missed line, no leftover.
func TestHookKilledDuringTheDownloadIsMissed(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{delay: 3 * time.Second})
	p := r.start("sh", `{"hook_event_name":"PostToolUse"}`, nil, "hook")
	waitFor(t, "the server's first request", func() bool { return r.hits.Load() == 1 })
	p.signal(syscall.SIGTERM)
	res := p.wait()
	if res.code != 0 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
	}
	r.wantMissed("PostToolUse", "killed by signal")
	r.wantNoCachedBinary()
	r.wantNoLockOrTemp()
}

// kill after install, pid only: the post-install run is a child, so a signal
// to the wrapper's pid lets the child finish its run; the wrapper then exits 0
// without a missed line.
func TestSignalToTheWrapperAfterInstallLetsTheChildFinish(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	dir, env := fakeDir(t)
	p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env, "FAKE_SLEEP=2"}, "hook")
	waitForFile(t, filepath.Join(dir, "started"))
	p.signal(syscall.SIGTERM)
	res := p.wait()
	if res.code != 0 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
	}
	if !exists(filepath.Join(dir, "finished")) {
		t.Fatal("the child did not finish its run")
	}
	if exists(filepath.Join(dir, "term")) {
		t.Fatal("the child received the signal sent to the wrapper's pid alone")
	}
	if lines := r.missed(); lines != nil {
		t.Fatalf("missed.log = %q, want none", lines)
	}
}

// kill after install, group: a signal to the process group reaches the child;
// the wrapper exits 0 and writes no line.
func TestSignalToTheGroupAfterInstallReachesTheChild(t *testing.T) {
	t.Parallel()
	r := newRig(t, rigOpts{})
	dir, env := fakeDir(t)
	p := r.start("sh", `{"hook_event_name":"Stop"}`, []string{env, "FAKE_SLEEP=10"}, "hook")
	waitForFile(t, filepath.Join(dir, "started"))
	p.signalGroup(syscall.SIGTERM)
	res := p.wait()
	if res.code != 0 || res.stdout != "" {
		t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
	}
	if !exists(filepath.Join(dir, "term")) {
		t.Fatal("the child never received the group's SIGTERM")
	}
	if exists(filepath.Join(dir, "finished")) {
		t.Fatal("the child ran to its end")
	}
	if lines := r.missed(); lines != nil {
		t.Fatalf("missed.log = %q, want none", lines)
	}
}

// version cleanup: after an install a version directory goes only when its
// stamp, else the directory itself, is 7 days old.
func TestVersionCleanupFollowsTheStampAge(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name    string
		stamp   time.Duration // the stamp's age; 0 writes none
		dirAge  time.Duration // the version directory's own age
		removed bool
	}{
		{"stamp 8 days old", 8 * day, 0, true},
		{"stamp 6 days old", 6 * day, 20 * day, false},
		{"no stamp, directory 8 days old", 0, 8 * day, true},
		{"no stamp, directory 1 hour old", 0, time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			old := r.seed("0.0.9", "#!/bin/sh\necho old\n")
			if tc.stamp > 0 {
				r.ageStamp("0.0.9", tc.stamp)
			}
			r.age(filepath.Dir(old), tc.dirAge)
			res := r.run("", nil, "version")
			if res.code != 0 {
				t.Fatalf("exit %d stderr %q", res.code, res.stderr)
			}
			if _, err := os.Stat(r.cachePath()); err != nil {
				t.Fatalf("new version missing: %v", err)
			}
			if present := exists(filepath.Dir(old)); present == tc.removed {
				t.Fatalf("bin/0.0.9 present = %v, want %v", present, !tc.removed)
			}
		})
	}
}

// dot entries: lock and temp entries are never version directories, however old.
func TestDotEntriesAreNeverVersions(t *testing.T) {
	r := newRig(t, rigOpts{})
	bin := filepath.Join(r.home, "bin")
	entries := []string{filepath.Join(bin, ".lock-0.0.7"), filepath.Join(bin, ".tmp-dir")}
	for _, e := range entries {
		if err := os.MkdirAll(e, 0o755); err != nil {
			t.Fatal(err)
		}
		r.age(e, 30*24*time.Hour)
	}
	res := r.run("", nil, "version")
	if res.code != 0 {
		t.Fatalf("exit %d stderr %q", res.code, res.stderr)
	}
	for _, e := range entries {
		if !exists(e) {
			t.Fatalf("%s was removed as a version directory", e)
		}
	}
}

// temp sweep: the lock holder removes a temp file only when it is older than 120 s.
func TestTempSweepKeepsAFreshForeignDownload(t *testing.T) {
	r := newRig(t, rigOpts{})
	bin := filepath.Join(r.home, "bin")
	fresh, stale := filepath.Join(bin, ".tmp-424242"), filepath.Join(bin, ".tmp-515151")
	for _, p := range []string{fresh, stale} {
		writeFile(t, p, "partial", 0o644)
	}
	r.age(fresh, 10*time.Second)
	r.age(stale, 300*time.Second)
	res := r.run("", nil, "version")
	if res.code != 0 {
		t.Fatalf("exit %d stderr %q", res.code, res.stderr)
	}
	if !exists(fresh) {
		t.Fatal("another run's temp file, written 10 s ago, was swept")
	}
	if exists(stale) {
		t.Fatal("a temp file older than 120 s survived the sweep")
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

// missed line: the event and the session come from stdin, the event `unknown`
// and the session field left out when there is none.
func TestMissedLineEvent(t *testing.T) {
	const uuid = "0b5c4c1e-1111-2222-3333-444455556666"
	cases := []struct {
		name, stdin, event, session string
	}{
		{"compact", `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash"}`, "PreToolUse", "s"},
		{"spaced and multi-line", "{\n  \"session_id\": \"s\",\n  \"hook_event_name\" : \"SessionEnd\"\n}\n", "SessionEnd", "s"},
		{"escaped decoy inside a prompt first", `{"prompt":"say \"hook_event_name\":\"Fake\" now","hook_event_name":"UserPromptSubmit"}`, "UserPromptSubmit", ""},
		{"first of two", `{"hook_event_name":"Stop","nested":{"hook_event_name":"Other"}}`, "Stop", ""},
		{"no key", `{"session_id":"s"}`, "unknown", "s"},
		{"empty stdin", ``, "unknown", ""},
		{"not json", "\x00\x01\x02 garbage \xff", "unknown", ""},
		{"value outside the event alphabet", `{"hook_event_name":"a\tb"}`, "unknown", ""},
		{"a captured payload's session", `{"session_id":"` + uuid + `","transcript_path":"/tmp/demo-home/t.jsonl","cwd":"/tmp/demo-proj","hook_event_name":"StopFailure"}`, "StopFailure", uuid},
		{"escaped session decoy inside a prompt first", `{"prompt":"say \"session_id\":\"fake\" now","session_id":"` + uuid + `","hook_event_name":"UserPromptSubmit"}`, "UserPromptSubmit", uuid},
		{"session first of two", `{"session_id":"outer","tool_input":{"session_id":"inner"},"hook_event_name":"PreToolUse"}`, "PreToolUse", "outer"},
		{"session outside the id alphabet", `{"session_id":"a b\tc","hook_event_name":"Stop"}`, "Stop", ""},
		{"session not a string", `{"session_id":42,"hook_event_name":"Stop"}`, "Stop", ""},
		{"keys after a 1 MB value", `{"tool_response":"` + strings.Repeat("x", 1<<20) + `","session_id":"` + uuid + `","hook_event_name":"PostToolUse"}`, "PostToolUse", uuid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{status: http.StatusNotFound})
			res := r.run(tc.stdin, nil, "hook")
			if res.code != 0 || res.stdout != "" {
				t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
			}
			r.wantMissedSession(tc.event, "download failed", tc.session)
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

// first run after install: a binary that crashes or cannot start under hook is
// a missed line, exit 0. Only the run right after the wrapper's own download is
// a child; a cached binary is exec'd, so its own failure is its own status.
func TestHookBinaryFailureIsMissed(t *testing.T) {
	t.Run("exits non-zero", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		res := r.run(`{"hook_event_name":"Stop"}`, []string{"FAKE_EXIT=3"}, "hook")
		if res.code != 0 || res.stdout != "" {
			t.Fatalf("exit %d stdout %q, want 0 and empty", res.code, res.stdout)
		}
		r.wantMissed("unknown", "binary exited 3")
	})
	t.Run("cannot start", func(t *testing.T) {
		r := newRig(t, rigOpts{served: "\x01\x02\x03 not an executable \x00\x00"})
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
	required := []string{"sh", "awk", "grep", "sed", "cat", "mkdir", "mv", "rm", "rmdir", "chmod", "date", "stat", "find", "uname", "dirname", "sleep"}
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
