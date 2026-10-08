package hookentry

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/runner"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
	"modernc.org/sqlite"
)

// buildCallmeter builds ./cmd/callmeter to binary.
func buildCallmeter(ctx context.Context, t *testing.T, binary string) {
	t.Helper()
	built, err := runner.Real{}.Run(ctx, []string{"go", "build", "-o", binary, "./cmd/callmeter"},
		runner.RunOptions{Dir: filepath.Join("..", "..")})
	if err != nil || built.ExitCode != 0 {
		t.Fatalf("build callmeter: %v (exit %d): %s", err, built.ExitCode, built.Stderr)
	}
}

// hookProcesses is how many `callmeter hook` processes write the one store at
// once: more than any real session runs in parallel.
const hookProcesses = 64

// TestProcessConcurrentHooks feeds S1 and S2 through the built binary, every
// payload its own `callmeter hook` process, up to hookProcesses at once on one
// CALLMETER_HOME: every process exits 0 with nothing on stdout, the store
// never faults, and it holds what the in-order replay of the same payloads
// holds. A run whose hook timeout keeps its store wait under BusyTimeout
// (storeWait: SessionEnd's 2 s gives 800 ms) can meet a queue that long on a
// loaded host and gives up as designed: its `terminated by store busy` line
// (ingested by a later run, or still in missed.log) stands in for its event
// row, and a busy store fault of a later batch of such a run whose event
// committed is its own. Every other run waits BusyTimeout and lands.
func TestProcessConcurrentHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lab := newCallmeterLab(t)
	binary := filepath.Join(lab.root, "bin", "callmeter")
	buildCallmeter(ctx, t, binary)
	if err := os.RemoveAll(lab.home); err != nil {
		t.Fatalf("clear the lab home: %v", err)
	}
	callmeterHome := filepath.Join(lab.root, "callmeter-home")
	lab.storePath = paths.Store(callmeterHome)
	registered := registeredEvents(t)
	var payloads []string
	// The concurrent run fires every payload at once, so the same bytes the
	// in-order replay feeds seconds apart (an agent woken twice) may land within
	// RedeliveryWindow and be one occurrence: the counts lie between the in-order
	// replay's distinct payload hashes and its rows.
	wantEvents, wantTurns, minEvents, minTurns := 0, 0, 0, 0
	ids := map[string]bool{}
	for _, session := range []string{"S1", "S2"} {
		// The two homes hold different session files: one home holds both.
		if err := os.CopyFS(lab.home, os.DirFS(filepath.Join("testdata", "gym", session, "home"))); err != nil {
			t.Fatalf("copy the %s home: %v", session, err)
		}
		for _, line := range fixturePayloads(t, "gym/"+session) {
			if registered[eventName(t, line)] {
				payloads = append(payloads, lab.rewrite(line))
			}
		}
		inOrder := newReplay(t, "gym/"+session)
		inOrder.feedInOrder()
		wantEvents += inOrder.lab.count("SELECT COUNT(*) FROM events")
		wantTurns += inOrder.lab.count("SELECT COUNT(*) FROM turns")
		minEvents += inOrder.lab.count("SELECT COUNT(DISTINCT substr(event_id, 1, 64)) FROM events")
		minTurns += inOrder.lab.count("SELECT COUNT(DISTINCT substr(event_id, 1, 64)) FROM turns")
		for id := range inOrder.calls() {
			ids[id] = true
		}
	}
	env := append(os.Environ(),
		"CALLMETER_HOME="+callmeterHome, "HOME="+lab.home, "CLAUDE_CONFIG_DIR="+filepath.Join(lab.home, ".claude"))
	slots := make(chan struct{}, hookProcesses)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]runner.RunResult, len(payloads))
	failures := make([]error, len(payloads))
	for i, payload := range payloads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i], failures[i] = runner.Real{}.Run(ctx, []string{binary, "hook"},
				runner.RunOptions{Env: env, Stdin: []byte(payload)})
		}()
	}
	close(start)
	wg.Wait()
	for i, result := range results {
		if failures[i] != nil || result.ExitCode != 0 || len(result.Stdout) != 0 {
			t.Errorf("payload %d: err %v, exit %d, stdout %q, stderr %q",
				i, failures[i], result.ExitCode, result.Stdout, result.Stderr)
		}
	}
	var shortWait []any
	for event := range registered {
		if storeWait(event) < callmeter.BusyTimeout {
			shortWait = append(shortWait, event)
		}
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(shortWait)), ",")
	if n := lab.count(`SELECT COUNT(*) FROM faults f WHERE f.stage = 'store' AND NOT (f.error LIKE '%SQLITE_BUSY%'
		AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = f.session_id AND e.ts = f.ts AND e.event IN (`+marks+`)))`,
		shortWait...); n != 0 {
		t.Errorf("%d store faults under %d concurrent processes:\n%s",
			n, hookProcesses, strings.Join(lab.dump()["faults"], "\n"))
	}
	missed, err := os.ReadFile(paths.Missed(callmeterHome))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read missed.log: %v", err)
	}
	gaveUp, gaveUpTurns := 0, 0
	for _, event := range shortWait {
		n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = 'terminated' AND error LIKE ?",
			fmt.Sprint(event)+": "+callmeter.TerminatedByStoreBusy+"%")
		for _, line := range strings.Split(string(missed), "\n") {
			if fields := strings.Split(line, "\t"); len(fields) >= 3 && fields[1] == event &&
				strings.HasPrefix(fields[2], callmeter.TerminatedByStoreBusy) {
				n++
			}
		}
		gaveUp += n
		if event == callmeter.EventStopFailure {
			gaveUpTurns += n
		}
	}
	for id := range ids {
		if n := lab.count("SELECT COUNT(*) FROM calls WHERE tool_use_id = ?", id); n != 1 {
			t.Errorf("call %s: %d rows, want 1", id, n)
		}
	}
	t.Logf("short-wait runs given up busy: %d", gaveUp)
	if n := lab.hookRows("events"); n < minEvents-gaveUp || n > wantEvents {
		t.Errorf("events holds %d hook rows, want from the in-order replay's %d distinct payloads (same bytes within "+
			"RedeliveryWindow are one occurrence) less %d short-wait runs given up busy, to its %d rows:\n%s\nmissed.log:\n%s",
			n, minEvents, gaveUp, wantEvents, lab.turnEndRows(), missed)
	}
	if n := lab.hookRows("turns"); n < minTurns-gaveUpTurns || n > wantTurns {
		t.Errorf("turns holds %d hook rows, want from the in-order replay's %d distinct payloads (same bytes within "+
			"RedeliveryWindow are one occurrence) less %d given up busy, to its %d rows:\n%s",
			n, minTurns, gaveUpTurns, wantTurns, lab.turnEndRows())
	}
	if n := lab.strayRebuiltEnds(); n != 0 {
		t.Errorf("%d turn ends rebuilt from a transcript that no SessionEnd running before its session's prompt "+
			"explains:\n%s", n, lab.turnEndRows())
	}
}

// rebuiltEnds lists the event ids of the turn ends rebuilt from a transcript
// (callmeter.RecoveredDetail, bound to ?1) rather than written by a hook.
const rebuiltEnds = "SELECT event_id FROM events WHERE detail = ?1"

// hookRows counts the rows of table, events or turns, that a hook wrote: a
// turn end rebuilt from a transcript is left out, its events row and its turns
// row.
func (lab *callmeterLab) hookRows(table string) int {
	lab.t.Helper()
	return lab.count("SELECT COUNT(*) FROM "+table+" WHERE event_id NOT IN ("+rebuiltEnds+")", callmeter.RecoveredDetail)
}

// strayRebuiltEnds counts the turn ends rebuilt from a transcript that the
// concurrent run does not explain. The run stamps every hook with the real
// clock while the gym transcripts keep their capture time, so a SessionEnd
// committing before its session's prompt and Stop rebuilds a Stop with no
// prompt_id, dated before the prompt that lands next; the Stop hook's drop
// then rightly takes it for an earlier turn's answer and keeps it. Every other
// rebuilt row (one naming a prompt, a StopFailure, one dated at or after its
// session's first prompt, one in a session with no prompt) is stray.
func (lab *callmeterLab) strayRebuiltEnds() int {
	lab.t.Helper()
	return lab.count(`SELECT COUNT(*) FROM events e WHERE e.detail = ?1 AND NOT EXISTS (
		SELECT 1 FROM turns t WHERE t.event_id = e.event_id AND t.event = 'Stop' AND t.prompt_id IS NULL
			AND t.ts < (SELECT COALESCE(MIN(p.ts), 0) FROM events p WHERE p.session_id = t.session_id
				AND p.event = 'UserPromptSubmit' AND COALESCE(p.agent_id, '') = ''))`, callmeter.RecoveredDetail)
}

// turnEndRows is the store's turns rows and its prompt, turn-end and
// SessionEnd events rows, for a failure message.
func (lab *callmeterLab) turnEndRows() string {
	lab.t.Helper()
	dump := lab.dump()
	var ends []string
	for _, row := range dump["events"] {
		if strings.Contains(row, "UserPromptSubmit") || strings.Contains(row, "Stop") || strings.Contains(row, "SessionEnd") {
			ends = append(ends, row)
		}
	}
	return "turns:\n" + strings.Join(dump["turns"], "\n") + "\nprompt, turn-end and SessionEnd events:\n" + strings.Join(ends, "\n")
}

// TestProcessConcurrentCountsLeaveOutATurnEndRebuiltBeforeItsPrompt forces the
// interleaving TestProcessConcurrentHooks meets under load: S1's SessionEnd
// commits first, on a clock past the capture-time transcript, then its prompt
// and its Stop. The rebuilt Stop stays beside the hook's; the counts the
// concurrent run is held to leave it out, and explain it.
func TestProcessConcurrentCountsLeaveOutATurnEndRebuiltBeforeItsPrompt(t *testing.T) {
	r := newReplay(t, "gym/S1")
	first := func(event string) string {
		t.Helper()
		found := r.matching(event, nil)
		if len(found) == 0 {
			t.Fatalf("gym/S1 holds no %s payload", event)
		}
		return r.payloads[found[0]]
	}
	stop := first("Stop")
	transcript, _ := payloadField(t, stop, "transcript_path").(string)
	end, err := callmeter.TranscriptTurnEnd(transcript)
	if err != nil || end.Event != callmeter.EventStop || end.TS == 0 {
		t.Fatalf("TranscriptTurnEnd(%s) = %+v, %v; want a dated Stop", transcript, end, err)
	}
	after := end.TS + time.Minute.Milliseconds()
	r.lab.feedAt(after, first("SessionEnd"))
	r.lab.feedAt(after+20, first("UserPromptSubmit"))
	r.lab.feedAt(after+20, stop)
	if n := r.lab.count("SELECT COUNT(*) FROM turns WHERE event_id IN ("+rebuiltEnds+")", callmeter.RecoveredDetail); n != 1 {
		t.Fatalf("the forced order left %d rebuilt turn ends, want 1: the interleaving did not happen\n%s", n, r.lab.turnEndRows())
	}
	if n := r.lab.hookRows("turns"); n != 1 {
		t.Errorf("turns holds %d hook rows, want 1, the Stop hook's:\n%s", n, r.lab.turnEndRows())
	}
	if n := r.lab.hookRows("events"); n != 3 {
		t.Errorf("events holds %d hook rows, want 3: SessionEnd, UserPromptSubmit, Stop\n%s", n, r.lab.turnEndRows())
	}
	if n := r.lab.strayRebuiltEnds(); n != 0 {
		t.Errorf("%d stray rebuilt turn ends, want 0:\n%s", n, r.lab.turnEndRows())
	}
	// A rebuilt row naming a prompt is no timing artifact: it must read as stray.
	if _, err := r.lab.db().DB().ExecContext(r.lab.ctx, "UPDATE turns SET prompt_id = 'p' WHERE event_id IN ("+rebuiltEnds+")",
		callmeter.RecoveredDetail); err != nil {
		t.Fatalf("name a prompt on the rebuilt turn: %v", err)
	}
	if n := r.lab.strayRebuiltEnds(); n != 1 {
		t.Errorf("a rebuilt turn naming a prompt: %d stray, want 1", n)
	}
}

// signalDelay is how long a signal test lets a hook process run before it
// signals it: well past the process's start, well inside the 3 s settle wait
// of a SubagentStop whose transcript lacks its final message.
const signalDelay = 700 * time.Millisecond

// missedLine is one line the binary appends to missed.log under a signal, its
// session field present when the payload decoded with a session id.
var missedLine = regexp.MustCompile(`^(\d+)\t(\S+)\tterminated by (SIG[A-Z]+) \(pid ([1-9][0-9]*)\)(?:\t([A-Za-z0-9._-]+))?\n$`)

// signalScene is one hermetic run of the built binary: its own CALLMETER_HOME,
// seat and the gym/S2 session's transcripts.
type signalScene struct {
	lab    *callmeterLab
	binary string
	home   string // CALLMETER_HOME
	env    []string
}

func newSignalScene(t *testing.T, binary string) *signalScene {
	t.Helper()
	lab := newCallmeterLab(t)
	if err := os.RemoveAll(lab.home); err != nil {
		t.Fatalf("clear the lab home: %v", err)
	}
	if err := os.CopyFS(lab.home, os.DirFS(filepath.Join("testdata", "gym", "S2", "home"))); err != nil {
		t.Fatalf("copy the S2 home: %v", err)
	}
	home := filepath.Join(lab.root, "callmeter-home")
	lab.storePath = paths.Store(home)
	lab.missed = paths.Missed(home)
	lab.logPath = paths.Log(home)
	return &signalScene{
		lab: lab, binary: binary, home: home,
		env: append(os.Environ(),
			"CALLMETER_HOME="+home, "HOME="+lab.home, "CLAUDE_CONFIG_DIR="+filepath.Join(lab.home, ".claude")),
	}
}

// payload is the first captured payload of event in gym/S2, rewritten by the lab.
func (scene *signalScene) payload(t *testing.T, event string) string {
	t.Helper()
	for _, line := range fixturePayloads(t, "gym/S2") {
		if eventName(t, line) == event {
			return scene.lab.rewrite(line)
		}
	}
	t.Fatalf("gym/S2 holds no %s payload", event)
	return ""
}

// unfinishedStop is a typed SubagentStop whose sub-agent transcript lacks its
// final message: its run waits out the settle wait before it records.
func (scene *signalScene) unfinishedStop(t *testing.T) string {
	t.Helper()
	payload := scene.payload(t, "SubagentStop")
	transcript, _ := payloadField(t, payload, "agent_transcript_path").(string)
	full, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read the sub-agent transcript: %v", err)
	}
	lines := strings.SplitAfter(string(full), "\n")
	for len(lines) > 0 && (lines[len(lines)-1] == "" || strings.Contains(lines[len(lines)-1], `"stop_reason":"end_turn"`)) {
		lines = lines[:len(lines)-1]
	}
	scene.lab.write(transcript, []byte(strings.Join(lines, "")))
	return payload
}

// hookProcess is a started `callmeter hook` the test signals mid-run.
type hookProcess struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout, stderr *bytes.Buffer
	started        time.Time
}

// start spawns `callmeter hook`; a payload is written to its stdin and the
// pipe closed, "" leaves stdin open and unread.
func (scene *signalScene) start(t *testing.T, payload string) *hookProcess {
	t.Helper()
	return scene.startCmd(t, payload, exec.Command(scene.binary, "hook"))
}

// startIgnoring spawns `callmeter hook` with the signals named by traps (the
// sh names, `HUP INT`) ignored on entry, as `nohup` or a non-interactive
// shell's background job leaves them: sh sets the disposition and execs the
// binary in its own pid.
func (scene *signalScene) startIgnoring(t *testing.T, payload, traps string) *hookProcess {
	t.Helper()
	return scene.startCmd(t, payload,
		exec.Command("/bin/sh", "-c", `trap '' `+traps+`; exec "$0" hook`, scene.binary))
}

func (scene *signalScene) startCmd(t *testing.T, payload string, cmd *exec.Cmd) *hookProcess {
	t.Helper()
	proc := &hookProcess{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	proc.cmd = cmd
	proc.cmd.Env = scene.env
	proc.cmd.Stdout, proc.cmd.Stderr = proc.stdout, proc.stderr
	stdin, err := proc.cmd.StdinPipe()
	if err != nil {
		t.Fatalf("pipe stdin: %v", err)
	}
	proc.stdin = stdin
	proc.started = time.Now()
	if err := proc.cmd.Start(); err != nil {
		t.Fatalf("start callmeter hook: %v", err)
	}
	t.Cleanup(func() {
		if err := proc.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill callmeter hook: %v", err)
		}
	})
	if payload != "" {
		if _, err := io.WriteString(stdin, payload); err != nil {
			t.Fatalf("write the payload: %v", err)
		}
		if err := stdin.Close(); err != nil {
			t.Fatalf("close stdin: %v", err)
		}
	}
	return proc
}

func (proc *hookProcess) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := proc.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("send %v: %v", sig, err)
	}
}

// exit waits for the process and returns its exit code, -1 when a signal killed it.
func (proc *hookProcess) exit(t *testing.T) int {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- proc.cmd.Wait() }()
	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("wait for callmeter hook: %v", err)
		}
		return proc.cmd.ProcessState.ExitCode()
	case <-time.After(30 * time.Second):
		t.Fatalf("callmeter hook still running 30 s on; stderr %q", proc.stderr)
		return 0
	}
}

// missedLines is the content of the scene's missed.log, "" when there is none.
func (scene *signalScene) missedLines(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(paths.Missed(scene.home))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read missed.log: %v", err)
	}
	return string(data)
}

// wantTerminatedLine checks that missed.log holds exactly one line of event,
// signal and session ("" = no session field), stamped within the run's own
// seconds.
func (scene *signalScene) wantTerminatedLine(t *testing.T, proc *hookProcess, event, signal, session string) {
	t.Helper()
	got := scene.missedLines(t)
	match := missedLine.FindStringSubmatch(got)
	if match == nil || strings.Count(got, "\n") != 1 {
		t.Fatalf("missed.log = %q, want one line `{secs}\\t%s\\tterminated by %s`", got, event, signal)
	}
	secs, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || secs < proc.started.Unix() || secs > time.Now().Unix() {
		t.Errorf("missed.log stamp = %q, want Unix seconds of this run (%d..%d)", match[1], proc.started.Unix(), time.Now().Unix())
	}
	if match[2] != event || match[3] != signal || match[4] != strconv.Itoa(proc.cmd.Process.Pid) || match[5] != session {
		t.Errorf("missed.log line = %q, want event %s signal %s pid %d session %q", got, event, signal, proc.cmd.Process.Pid, session)
	}
}

// wantQuietExit checks the exit contract of every signalled run: exit 0 and
// nothing on stdout.
func wantQuietExit(t *testing.T, proc *hookProcess) {
	t.Helper()
	if code := proc.exit(t); code != 0 || proc.stdout.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 0 and an empty stdout", code, proc.stdout, proc.stderr)
	}
}

// holdStore takes the store's write lock through a second connection, so a
// hook's first write batch waits on the busy timeout until release.
func (scene *signalScene) holdStore(t *testing.T) (release func()) {
	t.Helper()
	store := scene.lab.db() // creates the schema
	conn, err := store.DB().Conn(scene.lab.ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	if _, err := conn.ExecContext(scene.lab.ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the store's write lock: %v", err)
	}
	return func() {
		if _, err := conn.ExecContext(scene.lab.ctx, "ROLLBACK"); err != nil {
			t.Errorf("release the store's write lock: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close the lock connection: %v", err)
		}
	}
}

// TestProcessSignalledHook drives the built binary under SIGTERM, SIGINT and
// SIGHUP: a hook a signal ends before its event is accounted for leaves one
// missed.log line, one it ends after leaves none, and every one exits 0
// silently.
func TestProcessSignalledHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "callmeter")
	buildCallmeter(ctx, t, binary)

	for sig, name := range map[syscall.Signal]string{
		syscall.SIGTERM: "SIGTERM", syscall.SIGINT: "SIGINT", syscall.SIGHUP: "SIGHUP",
	} {
		t.Run("killed before recording by "+name, func(t *testing.T) {
			scene := newSignalScene(t, binary)
			payload := scene.unfinishedStop(t)
			session, _ := payloadField(t, payload, "session_id").(string)
			if session == "" {
				t.Fatal("the SubagentStop payload carries no session_id")
			}
			proc := scene.start(t, payload)
			time.Sleep(signalDelay)
			proc.signal(t, sig)
			wantQuietExit(t, proc)
			scene.wantTerminatedLine(t, proc, "SubagentStop", name, session)
			if n := scene.lab.count("SELECT COUNT(*) FROM events"); n != 0 {
				t.Errorf("events holds %d rows, want none for the killed run", n)
			}
		})
	}

	for sig, name := range map[syscall.Signal]string{syscall.SIGINT: "SIGINT", syscall.SIGHUP: "SIGHUP"} {
		t.Run(name+" ignored on entry stays ignored", func(t *testing.T) {
			scene := newSignalScene(t, binary)
			proc := scene.startIgnoring(t, scene.unfinishedStop(t), "HUP INT")
			time.Sleep(signalDelay)
			proc.signal(t, sig)
			wantQuietExit(t, proc)
			if got := scene.missedLines(t); got != "" {
				t.Errorf("missed.log = %q, want no line: an ignored signal ends nothing", got)
			}
			if n := scene.lab.count("SELECT COUNT(*) FROM events"); n != 1 {
				t.Errorf("events holds %d rows, want the run's one: it finished despite the ignored %s", n, name)
			}
		})
	}

	t.Run("killed before decode", func(t *testing.T) {
		scene := newSignalScene(t, binary)
		proc := scene.start(t, "") // stdin open, nothing to read
		time.Sleep(signalDelay)
		proc.signal(t, syscall.SIGTERM)
		wantQuietExit(t, proc)
		scene.wantTerminatedLine(t, proc, "unknown", "SIGTERM", "")
	})

	t.Run("missed.log cannot be written", func(t *testing.T) {
		scene := newSignalScene(t, binary)
		if err := os.MkdirAll(paths.Missed(scene.home), 0o755); err != nil { // a directory where the file goes
			t.Fatalf("make missed.log a directory: %v", err)
		}
		proc := scene.start(t, "")
		time.Sleep(signalDelay)
		proc.signal(t, syscall.SIGTERM)
		wantQuietExit(t, proc)
		logged, err := os.ReadFile(paths.Log(scene.home))
		if err != nil {
			t.Fatalf("read callmeter.log: %v", err)
		}
		for name, text := range map[string]string{"stderr": proc.stderr.String(), "callmeter.log": string(logged)} {
			if !strings.Contains(text, "missed.log") || !strings.Contains(text, callmeter.StageTerminated) {
				t.Errorf("%s = %q, want the failed append to missed.log said", name, text)
			}
		}
	})

	t.Run("home cannot be written", func(t *testing.T) {
		scene := newSignalScene(t, binary)
		parent := filepath.Join(scene.lab.root, "not-a-directory")
		if err := os.WriteFile(parent, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", parent, err)
		}
		scene.home = filepath.Join(parent, "home") // creating it fails whoever runs the test
		scene.env = append(scene.env, "CALLMETER_HOME="+scene.home)
		proc := scene.start(t, "")
		time.Sleep(signalDelay)
		proc.signal(t, syscall.SIGTERM)
		wantQuietExit(t, proc)
		if !strings.Contains(proc.stderr.String(), "missed.log") {
			t.Errorf("stderr = %q, want the failed append to missed.log said", proc.stderr)
		}
	})
}

// terminate starts the signal handler for sig as the binary does, in its own
// goroutine, and returns the channel its exit code arrives on.
func (lab *callmeterLab) terminate(state *terminationState, sig os.Signal, stderr io.Writer) <-chan int {
	codes := make(chan int, 1)
	lab.terminateWithExit(state, sig, stderr, func(code int) { codes <- code })
	return codes
}

func (lab *callmeterLab) terminateWithExit(state *terminationState, sig os.Signal, stderr io.Writer, exit func(int)) {
	signals := make(chan os.Signal, 1)
	signals <- sig
	go terminateOnSignal(signals, nil, state, lab.missed, lab.logPath, stderr, exit)
}

// await returns the handler's exit code, failing when it does not exit.
func await(t *testing.T, codes <-chan int) int {
	t.Helper()
	select {
	case code := <-codes:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("the signal handler did not exit")
		return 0
	}
}

// feedWithState runs one payload as the binary does: a terminationState in the ctx.
func (lab *callmeterLab) feedWithState(state *terminationState, payload string) int {
	shared := filepath.Join(lab.home, ".claude")
	var stderr bytes.Buffer
	return runCallmeter(withTerminationState(context.Background(), state), strings.NewReader(payload), &stderr,
		lab.files(), lab.clock, callmeterSeat{dir: lab.seatDir, configDir: &shared}, mapEnv(lab.env))
}

// TestTerminationAfterRecordingWritesNoLine: a signal after the run's first
// batch committed, or its first fault row was written, ends the run with no
// missed line and the rows in place.
func TestTerminationAfterRecordingWritesNoLine(t *testing.T) {
	scene := newSignalScene(t, "")
	lab := scene.lab
	for name, run := range map[string]struct {
		payload string
		rows    string
		want    int
	}{
		"batch committed": {
			scene.payload(t, "PreToolUse"),
			"SELECT COUNT(*) FROM calls", 1,
		},
		"fault row written": {
			"not a hook payload",
			"SELECT COUNT(*) FROM faults WHERE stage = 'payload'", 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := &terminationState{}
			if code := lab.feedWithState(state, run.payload); code != 0 {
				t.Fatalf("run exit code = %d, want 0", code)
			}
			var stderr bytes.Buffer
			if code := await(t, lab.terminate(state, syscall.SIGTERM, &stderr)); code != 0 {
				t.Errorf("handler exit code = %d, want 0", code)
			}
			if got := scene.missedLines(t); got != "" {
				t.Errorf("missed.log = %q, want no line for an event already accounted for", got)
			}
			if n := lab.count(run.rows); n != run.want {
				t.Errorf("%q = %d, want %d", run.rows, n, run.want)
			}
		})
	}
}

// TestTerminationBeforeRecordingWritesTheLine: a run with nothing accounted
// for leaves one line naming its event, `unknown` when the payload never
// decoded or its name would break the line, followed by the payload's session
// id as a fourth field, the wrapper's layout, so a killed SessionEnd names its
// session; a session id outside the field's alphabet is left out, never
// written to break the line. The home is created when absent.
func TestTerminationBeforeRecordingWritesTheLine(t *testing.T) {
	pid := fmt.Sprintf(" (pid %d)", os.Getpid())
	for name, run := range map[string]struct{ event, session, want string }{
		"decoded":                       {"Stop", "", "\tStop\tterminated by SIGINT" + pid + "\n"},
		"a SessionEnd with its session": {"SessionEnd", "sess_1.b-2", "\tSessionEnd\tterminated by SIGINT" + pid + "\tsess_1.b-2\n"},
		"a session with a tab":          {"SessionEnd", "sess\t1", "\tSessionEnd\tterminated by SIGINT" + pid + "\n"},
		"not decoded":                   {"", "", "\tunknown\tterminated by SIGINT" + pid + "\n"},
		"a name with a tab":             {"Pre\tToolUse", "sess-1", "\tunknown\tterminated by SIGINT" + pid + "\tsess-1\n"},
		"a name with a line feed":       {"Pre\nToolUse", "", "\tunknown\tterminated by SIGINT" + pid + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			scene := newSignalScene(t, "")
			state := &terminationState{}
			state.setPayload(run.event, run.session)
			var stderr bytes.Buffer
			if code := await(t, scene.lab.terminate(state, syscall.SIGINT, &stderr)); code != 0 {
				t.Errorf("handler exit code = %d, want 0", code)
			}
			if got := scene.missedLines(t); !missedLine.MatchString(got) || !strings.HasSuffix(got, run.want) {
				t.Errorf("missed.log = %q, want one line ending %q", got, run.want)
			}
			info, err := os.Stat(scene.home)
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Errorf("home = %v, %v; want it created 0755", info, err)
			}
			if stderr.Len() != 0 {
				t.Errorf("handler stderr = %q, want nothing said for a line that was written", stderr.String())
			}
		})
	}

	t.Run("store unreachable", func(t *testing.T) {
		scene := newSignalScene(t, "")
		lab := scene.lab
		parent := filepath.Join(lab.root, "not-a-directory")
		if err := os.WriteFile(parent, nil, 0o644); err != nil {
			t.Fatalf("write %s: %v", parent, err)
		}
		lab.storePath = filepath.Join(parent, "callmeter.db")
		state := &terminationState{}
		if code := lab.feedWithState(state, scene.payload(t, "PreToolUse")); code != 0 {
			t.Fatalf("run exit code = %d, want 0", code)
		}
		var stderr bytes.Buffer
		if code := await(t, lab.terminate(state, syscall.SIGHUP, &stderr)); code != 0 {
			t.Errorf("handler exit code = %d, want 0", code)
		}
		session, _ := payloadField(t, scene.payload(t, "PreToolUse"), "session_id").(string)
		want := regexp.MustCompile(`^\d+\tPreToolUse\t` + regexp.QuoteMeta(fmt.Sprintf("%s%s (pid %d)", callmeter.StoreUnavailableReason, callmeter.StoreClassOpen, os.Getpid())) +
			`\t` + regexp.QuoteMeta(session) + `\n$`)
		if got := scene.missedLines(t); session == "" || !want.MatchString(got) {
			t.Errorf("missed.log = %q, want the run's own line matching %s and none from the signal: that line accounts for the event", got, want)
		}
	})
}

type panicSignal struct{}

func (panicSignal) Signal()        {}
func (panicSignal) String() string { panic("boom") }

func TestSignalHandlerPanicKeepsTheAccountedRule(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sig         os.Signal
		accounted   bool
		panicAtExit bool
		unwritable  bool
		undecoded   bool
		wantReason  string
	}{
		{name: "before its line", sig: panicSignal{}, wantReason: callmeter.PanicReason},
		{name: "before payload decoded", sig: panicSignal{}, undecoded: true, wantReason: callmeter.PanicReason},
		{name: "unwritable missed log", sig: panicSignal{}, unwritable: true},
		{name: "after its line", sig: syscall.SIGTERM, panicAtExit: true, wantReason: callmeter.TerminatedReason + "SIGTERM"},
		{name: "accounted", sig: syscall.SIGTERM, accounted: true, panicAtExit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lab := newCallmeterLab(t)
			state := &terminationState{accounted: tc.accounted}
			event, session := "SessionEnd", cmSessionB
			if tc.undecoded {
				event, session = "unknown", ""
			} else {
				state.setPayload(event, session)
			}
			if tc.unwritable {
				if err := os.MkdirAll(lab.missed, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			codes := make(chan int, 1)
			calls := 0
			lab.terminateWithExit(state, tc.sig, &stderr, func(code int) {
				if state.mu.TryLock() {
					state.mu.Unlock()
					t.Error("termination lock released before exit")
				}
				calls++
				if tc.panicAtExit && calls == 1 {
					panic("boom")
				}
				codes <- code
			})
			if code := await(t, codes); code != 0 {
				t.Errorf("handler exit code = %d, want 0", code)
			}
			missed, err := os.ReadFile(lab.missed)
			if tc.wantReason != "" {
				line := fmt.Sprintf("\t%s\t%s (pid %d)", event, tc.wantReason, os.Getpid())
				if session != "" {
					line += "\t" + session
				}
				want := regexp.MustCompile(`^\d+` + regexp.QuoteMeta(line+"\n") + `$`)
				if err != nil || !want.Match(missed) {
					t.Errorf("missed.log has %d bytes (%v), want exactly one line for %s", len(missed), err, tc.wantReason)
				}
			} else if tc.unwritable {
				if err == nil {
					t.Error("missed.log directory unexpectedly readable as a file")
				}
			} else if !errors.Is(err, os.ErrNotExist) && (err != nil || len(missed) != 0) {
				t.Errorf("accounted panic left %d bytes in missed.log (%v)", len(missed), err)
			}
			prefix := fmt.Sprintf("callmeter: terminated: session %q call %q: ", session, "")
			wantFailures := 1
			if tc.unwritable {
				wantFailures++
				if !strings.Contains(stderr.String(), prefix+"open "+lab.missed+":") {
					t.Error("stderr does not name the missed append failure")
				}
			}
			if strings.Count(stderr.String(), prefix) != wantFailures ||
				!strings.Contains(stderr.String(), prefix+"signal handler panicked: boom\ngoroutine ") {
				t.Error("stderr does not hold the expected terminated failures and panic stack")
			}
			logged, err := os.ReadFile(lab.logPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
			if len(lines) != wantFailures {
				t.Fatalf("log has %d lines, want %d", len(lines), wantFailures)
			}
			for i, line := range lines {
				var record map[string]string
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["step"] != callmeter.StageTerminated || record["session"] != session || record["target"] != "" {
					t.Error("log failure does not name the terminated stage and session")
				}
				if i == len(lines)-1 {
					if !strings.HasPrefix(record["err"], "signal handler panicked: boom\ngoroutine ") {
						t.Error("log panic lacks the value and stack")
					}
				} else if !strings.HasPrefix(record["err"], "open "+lab.missed+":") {
					t.Error("log does not name the missed append failure")
				}
			}
		})
	}
}

// waitHeld waits until the run sits inside a write holding the state's lock,
// past any momentary take of it.
func waitHeld(t *testing.T, state *terminationState) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for stable := 0; stable < 3; {
		if time.Now().After(deadline) {
			t.Fatal("the run never held the signal state's lock inside its write")
		}
		time.Sleep(20 * time.Millisecond)
		if state.mu.TryLock() {
			state.mu.Unlock()
			stable = 0
		} else {
			stable++
		}
	}
}

// TestTerminationWaitsOutAnInFlightCommit: a signal landing while the run's
// first batch holds the store's write lock waits for its outcome — committed,
// no line and the rows; failed, its store fault and no line.
func TestTerminationWaitsOutAnInFlightCommit(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := "the batch commits"
		if failing {
			name = "the batch fails"
		}
		t.Run(name, func(t *testing.T) {
			scene := newSignalScene(t, "")
			lab := scene.lab
			payload := scene.payload(t, "PreToolUse")
			// The batch holds the store's write lock, not waiting on it: a
			// trigger stalls its calls insert (callmeter_test_stall) and, when
			// failing, then refuses it.
			body := "SELECT callmeter_test_stall();"
			if failing {
				body += " SELECT RAISE(ABORT, 'refused by the test');"
			}
			if _, err := lab.db().DB().ExecContext(lab.ctx,
				"CREATE TRIGGER stall_calls BEFORE INSERT ON calls BEGIN "+body+" END"); err != nil {
				t.Fatalf("create the stalling trigger: %v", err)
			}
			stall := &batchStall{entered: make(chan struct{}), release: make(chan struct{})}
			currentStall.Store(stall)
			defer currentStall.Store(nil)
			state := &terminationState{}
			ran := make(chan int, 1)
			go func() { ran <- lab.feedWithState(state, payload) }()
			select {
			case <-stall.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the run's batch never reached its calls insert")
			}
			waitHeld(t, state)

			var stderr bytes.Buffer
			codes := lab.terminate(state, syscall.SIGTERM, &stderr)
			select {
			case code := <-codes:
				t.Fatalf("the handler exited %d while the commit was in flight", code)
			case <-time.After(300 * time.Millisecond):
			}
			close(stall.release)
			if code := <-ran; code != 0 {
				t.Errorf("run exit code = %d, want 0", code)
			}
			if code := await(t, codes); code != 0 {
				t.Errorf("handler exit code = %d, want 0", code)
			}
			if got := scene.missedLines(t); got != "" {
				t.Errorf("missed.log = %q, want no line: the commit's outcome accounts for the event", got)
			}
			calls := lab.count("SELECT COUNT(*) FROM calls")
			faults := lab.count("SELECT COUNT(*) FROM faults WHERE stage = ? AND error LIKE '%refused by the test%'", callmeter.StageStore)
			switch {
			case failing && (calls != 0 || faults != 1):
				t.Errorf("after a failed batch: %d calls, %d store faults; want 0 and 1", calls, faults)
			case !failing && (calls != 1 || faults != 0):
				t.Errorf("after a committed batch: %d calls, %d store faults; want 1 and 0", calls, faults)
			}
		})
	}
}

// holdStoreExclusive takes the whole store through a second connection in
// exclusive locking mode: the lock stays after the commit until the connection
// closes, and every other connection, even one that only opens, answers
// SQLITE_BUSY. The scene's own store handle stays unopened until the release.
func (scene *signalScene) holdStoreExclusive(t *testing.T) (release func()) {
	t.Helper()
	database, err := sqlitedb.OpenReadWrite(paths.Store(scene.home), time.Second)
	if err != nil {
		t.Fatalf("open the store to hold it: %v", err)
	}
	conn, err := database.Conn(scene.lab.ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	for _, statement := range []string{
		"PRAGMA locking_mode=EXCLUSIVE", "BEGIN EXCLUSIVE", "CREATE TABLE IF NOT EXISTS held_by_test (x TEXT)", "COMMIT",
	} {
		if _, err := conn.ExecContext(scene.lab.ctx, statement); err != nil {
			t.Fatalf("hold the store exclusively (%s): %v", statement, err)
		}
	}
	return func() {
		if err := errors.Join(conn.Close(), database.Close()); err != nil {
			t.Errorf("release the exclusive store: %v", err)
		}
	}
}

// TestProcessStoreBusySessionEndLeavesAMissedLine drives the built binary's
// SessionEnd against a store that stays locked: the hook gives up inside its
// event's timeout with one missed.log line carrying the event and session, exit
// 0 and a silent stdout, and report-time recovery then marks that session's end
// `lost`.
func TestProcessStoreBusySessionEndLeavesAMissedLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "callmeter")
	buildCallmeter(ctx, t, binary)

	holds := map[string]func(*signalScene, *testing.T) func(){
		"exclusive":  (*signalScene).holdStoreExclusive,
		"write lock": (*signalScene).holdStore,
	}
	for name, hold := range holds {
		t.Run(name, func(t *testing.T) {
			scene := newSignalScene(t, binary)
			start, end := scene.payload(t, "SessionStart"), scene.payload(t, "SessionEnd")
			session, _ := payloadField(t, end, "session_id").(string)
			if session == "" || payloadField(t, start, "session_id") != session {
				t.Fatalf("the SessionStart and SessionEnd payloads must share a session_id; SessionEnd has %q", session)
			}
			wantQuietExit(t, scene.start(t, start)) // the sessions row exists before the store is held
			release := hold(scene, t)
			released := false
			defer func() {
				if !released {
					release()
				}
			}()

			proc := scene.start(t, end)
			wantQuietExit(t, proc)
			took := time.Since(proc.started)
			if took >= hookTimeouts[callmeter.EventSessionEnd] {
				t.Errorf("SessionEnd took %v against a held store, want under its %v hook timeout", took, hookTimeouts[callmeter.EventSessionEnd])
			}
			want := regexp.MustCompile(`^\d+\tSessionEnd\t` + regexp.QuoteMeta(fmt.Sprintf("terminated by store busy (pid %d)", proc.cmd.Process.Pid)) + `\t` + regexp.QuoteMeta(session) + `\n$`)
			if got := scene.missedLines(t); !want.MatchString(got) {
				t.Errorf("missed.log = %q, want one line matching %s", got, want)
			}

			release()
			released = true
			if n := scene.lab.count("SELECT COUNT(*) FROM events WHERE event = ?1", callmeter.EventSessionEnd); n != 0 {
				t.Errorf("events holds %d SessionEnd rows, want none for the run that gave up", n)
			}
			store := scene.lab.db()
			if _, err := store.IngestMissed(ctx, paths.Missed(scene.home)); err != nil {
				t.Fatalf("ingest missed.log: %v", err)
			}
			if _, err := store.RecoverQuiet(ctx, time.Now().Add(time.Hour), 0); err != nil {
				t.Fatalf("recover quiet sessions: %v", err)
			}
			if got := scene.lab.row("SELECT end_reason FROM sessions WHERE session_id = ?1", session)["end_reason"]; got != callmeter.EndReasonLost {
				t.Errorf("sessions.end_reason = %q, want %q", got, callmeter.EndReasonLost)
			}
		})
	}
}

// TestIngestCommitWaitsForTheTerminationHold: the hook's missed.log ingest
// commits and moves its claim under the signal state's lock, so a signal
// handler holding it (about to exit) never sees faults committed whose claim
// is still in place for the next run to ingest again. The payload does not
// decode, so the run takes the lock first at its ingest, not at setPayload.
func TestIngestCommitWaitsForTheTerminationHold(t *testing.T) {
	scene := newSignalScene(t, "")
	lab := scene.lab
	payload := "not a hook payload"
	lab.count("SELECT COUNT(*) FROM faults") // the store exists before the run
	if err := os.WriteFile(lab.missed, []byte("1790000001\tStop\tbinary exited 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &terminationState{}
	state.mu.Lock() // the signal handler, deciding
	ran := make(chan int, 1)
	go func() { ran <- lab.feedWithState(state, payload) }()
	time.Sleep(300 * time.Millisecond)
	if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = 'binary'"); n != 0 {
		t.Errorf("binary faults committed while the handler held the lock = %d, want 0", n)
	}
	state.mu.Unlock()
	if code := <-ran; code != 0 {
		t.Errorf("run exit code = %d, want 0", code)
	}
	if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = 'binary'"); n != 1 {
		t.Errorf("binary faults after the run = %d, want 1", n)
	}
}

// claudeKillGrace is Claude Code 2.1.287's measured gap between the SIGTERM it
// sends a cancelled async hook and the SIGKILL that follows (1.52 s and 1.47 s
// in two headless runs).
const claudeKillGrace = 1470 * time.Millisecond

// signalLineWithin is how soon after a signal a run waiting on a busy store
// leaves its missed.log line: well inside claudeKillGrace.
const signalLineWithin = time.Second

// TestProcessSignalOnABusyStoreBeatsTheKill drives Claude Code's cancel of an
// async hook, SIGTERM and then SIGKILL claudeKillGrace later, into a
// PostToolUse whose first batch waits on a write-locked store: the wait gives
// way to the signal, so the run's `terminated by SIGTERM` line lands within
// signalLineWithin, the process exits 0 before the SIGKILL, and no row is
// written for it, even once the store frees.
func TestProcessSignalOnABusyStoreBeatsTheKill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "callmeter")
	buildCallmeter(ctx, t, binary)
	scene := newSignalScene(t, binary)
	payload := scene.payload(t, "PostToolUse")
	session, _ := payloadField(t, payload, "session_id").(string)
	id, _ := payloadField(t, payload, "tool_use_id").(string)
	release := scene.holdStore(t)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	proc := scene.start(t, payload)
	waited := make(chan error, 1)
	time.Sleep(signalDelay)
	go func() { waited <- proc.cmd.Wait() }()
	proc.signal(t, syscall.SIGTERM)
	signalled := time.Now()
	lineAt := time.Duration(-1)
	for time.Since(signalled) < claudeKillGrace {
		if lineAt < 0 && scene.missedLines(t) != "" {
			lineAt = time.Since(signalled)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-waited:
	default:
		proc.signal(t, syscall.SIGKILL) // Claude Code's kill
		<-waited
	}
	code := proc.cmd.ProcessState.ExitCode()
	if lineAt < 0 || lineAt > signalLineWithin {
		t.Errorf("missed.log line %v after SIGTERM (-1 = none before the SIGKILL at %v), want within %v",
			lineAt, claudeKillGrace, signalLineWithin)
	}
	if code != 0 || proc.stdout.Len() != 0 {
		t.Errorf("exit %d (-1 = killed), stdout %q, stderr %q; want exit 0 before the SIGKILL and an empty stdout",
			code, proc.stdout, proc.stderr)
	}
	scene.wantTerminatedLine(t, proc, "PostToolUse", "SIGTERM", session)
	release()
	released = true
	if n := scene.lab.count("SELECT COUNT(*) FROM calls WHERE tool_use_id = ?", id); n != 0 {
		t.Errorf("calls holds %d rows for %s, want none: the run gave its batch up at the signal", n, id)
	}
}

// batchStall is the gate of callmeter_test_stall, a SQL function a test's
// trigger calls inside a run's batch: with no gate it returns at once,
// otherwise it closes entered once and returns when release closes.
type batchStall struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

var currentStall atomic.Pointer[batchStall]

func init() {
	sqlite.MustRegisterScalarFunction("callmeter_test_stall", 0,
		func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			if stall := currentStall.Load(); stall != nil {
				stall.once.Do(func() { close(stall.entered) })
				<-stall.release
			}
			return nil, nil
		})
}

// TestTerminationHoldIsNotTakenAcrossABusyWait: a run whose batch or fault row
// waits on a write-locked store holds nothing the signal handler waits on, so
// a signal during that wait is decided at once; once the store frees, the run
// records as usual.
func TestTerminationHoldIsNotTakenAcrossABusyWait(t *testing.T) {
	for _, tc := range []struct {
		name, event string // event "" feeds an undecodable payload, which faults
		want        string
	}{
		{"a batch", "PreToolUse", "SELECT COUNT(*) FROM calls"},
		{"a fault", "", "SELECT COUNT(*) FROM faults WHERE stage = '" + callmeter.StagePayload + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scene := newSignalScene(t, "")
			lab := scene.lab
			payload := "not a hook payload"
			if tc.event != "" {
				payload = scene.payload(t, tc.event)
			}
			release := scene.holdStore(t)
			state := &terminationState{}
			ran := make(chan int, 1)
			go func() { ran <- lab.feedWithState(state, payload) }()
			// Inside the wait of both: an undecodable payload's is 0.8 s.
			time.Sleep(200 * time.Millisecond)
			for until := time.Now().Add(400 * time.Millisecond); time.Now().Before(until); {
				if !state.mu.TryLock() {
					t.Error("the run held the signal state's lock while it waited on the busy store")
					break
				}
				state.mu.Unlock()
				time.Sleep(10 * time.Millisecond)
			}
			release()
			if code := <-ran; code != 0 {
				t.Errorf("run exit code = %d, want 0", code)
			}
			if n := lab.count(tc.want); n != 1 {
				t.Errorf("%s = %d after the store freed, want 1", tc.want, n)
			}
			if got := scene.missedLines(t); got != "" {
				t.Errorf("missed.log = %q, want no line for a run that recorded", got)
			}
		})
	}
}

// TestTerminationHoldIsNotKeptFromAFailedBatchIntoItsFaultsWait: a batch that
// held the store's write lock and failed lets the signal handler go before its
// fault row waits on a store another writer took meanwhile, so a signal during
// that wait is decided at once; once the store frees, the fault row lands. The
// competing writer must win the lock between the rollback and the fault's
// BEGIN, so a round the run wins is run again, up to 20.
func TestTerminationHoldIsNotKeptFromAFailedBatchIntoItsFaultsWait(t *testing.T) {
	for round := 1; ; round++ {
		if round > 20 {
			t.Fatal("the competing writer never took the store between the batch's rollback and its fault")
		}
		if failedBatchRound(t) {
			return
		}
	}
}

// failedBatchRound runs one round of the test above; false when the run's
// fault took the lock before the competing writer did.
func failedBatchRound(t *testing.T) (decided bool) {
	scene := newSignalScene(t, "")
	lab := scene.lab
	payload := scene.payload(t, "PreToolUse")
	if _, err := lab.db().DB().ExecContext(lab.ctx, "CREATE TRIGGER stall_calls BEFORE INSERT ON calls BEGIN "+
		"SELECT callmeter_test_stall(); SELECT RAISE(ABORT, 'refused by the test'); END"); err != nil {
		t.Fatalf("create the stalling trigger: %v", err)
	}
	stall := &batchStall{entered: make(chan struct{}), release: make(chan struct{})}
	currentStall.Store(stall)
	defer currentStall.Store(nil)
	competitor, err := sqlitedb.OpenReadWrite(paths.Store(scene.home), 0)
	if err != nil {
		t.Fatalf("open the competing writer: %v", err)
	}
	defer func() {
		if err := competitor.Close(); err != nil {
			t.Errorf("close the competing writer: %v", err)
		}
	}()
	conn, err := competitor.Conn(lab.ctx)
	if err != nil {
		t.Fatalf("take a connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	state := &terminationState{}
	ran := make(chan int, 1)
	go func() { ran <- lab.feedWithState(state, payload) }()
	select {
	case <-stall.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run's batch never reached its calls insert")
	}
	grabbed := make(chan error, 1)
	go func() { // spin, no busy wait, to take the lock the rollback frees
		for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
			if _, err := conn.ExecContext(lab.ctx, "BEGIN IMMEDIATE"); err == nil {
				grabbed <- nil
				return
			}
		}
		grabbed <- errors.New("the competing writer never took the store")
	}()
	close(stall.release)
	if err := <-grabbed; err != nil {
		t.Fatal(err)
	}
	var faults int
	if err := conn.QueryRowContext(lab.ctx, "SELECT COUNT(*) FROM faults WHERE error LIKE '%refused by the test%'").Scan(&faults); err != nil {
		t.Fatalf("count faults: %v", err)
	}
	if faults != 0 { // the run's fault went first: no wait to judge
		if _, err := conn.ExecContext(lab.ctx, "ROLLBACK"); err != nil {
			t.Fatalf("release the store: %v", err)
		}
		<-ran
		return false
	}
	// The store stays taken past half the run's wait: the hold must be let go
	// within it, and stay free while the fault waits.
	released := false
	for until := time.Now().Add(storeWait("PreToolUse") / 2); !released && time.Now().Before(until); {
		if released = state.mu.TryLock(); released {
			state.mu.Unlock()
		} else {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !released {
		t.Error("the run kept the signal state's lock from its failed batch into its fault's wait on the busy store")
	}
	for until := time.Now().Add(300 * time.Millisecond); released && time.Now().Before(until); {
		if !state.mu.TryLock() {
			t.Error("the run took the signal state's lock again during its fault's wait on the busy store")
			break
		}
		state.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := conn.ExecContext(lab.ctx, "ROLLBACK"); err != nil {
		t.Fatalf("release the store: %v", err)
	}
	if code := <-ran; code != 0 {
		t.Errorf("run exit code = %d, want 0", code)
	}
	if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = ? AND error LIKE '%refused by the test%'", callmeter.StageStore); n != 1 {
		t.Errorf("store faults of the refused batch = %d, want 1", n)
	}
	if got := scene.missedLines(t); got != "" {
		t.Errorf("missed.log = %q, want no line: the fault row accounts for the event", got)
	}
	return true
}

// The archive trigger holds phase 1 open while a real SessionEnd writes the
// main WAL. This makes overlap observable even on a fast host.
func TestProcessPruneLetsASessionEndLand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "callmeter")
	buildCallmeter(ctx, t, binary)
	scene := newSignalScene(t, binary)
	wantQuietExit(t, scene.start(t, scene.payload(t, "SessionStart")))
	store := scene.lab.db()
	cutoff := time.Now().Add(-24 * time.Hour)
	// Create the archive through the production path before installing its gate.
	if _, err := store.DB().Exec("INSERT INTO calls (tool_use_id, ts) VALUES ('archive-init', ?)", cutoff.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"INSERT INTO calls (tool_use_id, ts, tool) SELECT 'call-' || n, ?1, 'Bash' FROM rows",
		"INSERT INTO requests (request_id, ts) SELECT 'request-' || n, ?1 FROM rows",
		"INSERT INTO events (event_id, ts, event) SELECT 'event-' || n, ?1, 'Notification' FROM rows",
		"INSERT INTO turns (event_id, ts, event) SELECT 'turn-' || n, ?1, 'Stop' FROM rows",
		"INSERT INTO sessions (session_id, first_ts, last_ts) SELECT 'session-' || n, ?1, ?1 FROM rows",
	} {
		if _, err := tx.ExecContext(ctx, "WITH RECURSIVE rows(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM rows WHERE n < 10000) "+statement, cutoff.Add(-time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	archive, err := sqlitedb.OpenReadWrite(filepath.Join(filepath.Dir(scene.lab.storePath), callmeter.ArchiveFile), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Exec("CREATE TRIGGER pause_archive AFTER INSERT ON calls BEGIN SELECT callmeter_test_stall(); END"); err != nil {
		archive.Close()
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	stall := &batchStall{entered: make(chan struct{}), release: make(chan struct{})}
	currentStall.Store(stall)
	defer currentStall.Store(nil)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(stall.release) }) }
	defer release()
	type pruneResult struct {
		removed int64
		err     error
	}
	done := make(chan pruneResult, 1)
	started := time.Now()
	go func() { n, err := store.Prune(ctx, cutoff); done <- pruneResult{n, err} }()
	select {
	case <-stall.entered:
	case result := <-done:
		t.Fatalf("prune ended before the archive gate: %v", result.err)
	case <-ctx.Done():
		t.Fatal("prune never reached the archive gate")
	}
	proc := scene.start(t, scene.payload(t, "SessionEnd"))
	wantQuietExit(t, proc)
	probe, err := sqlitedb.OpenReadWrite(scene.lab.storePath, 0)
	if err != nil {
		release()
		<-done
		t.Fatal(err)
	}
	var events int
	err = probe.QueryRow("SELECT COUNT(*) FROM events WHERE event = ?", callmeter.EventSessionEnd).Scan(&events)
	probe.Close()
	if err != nil {
		t.Errorf("read live SessionEnd events: %v", err)
	}
	if events != 1 {
		t.Errorf("SessionEnd events during archive = %d, want 1", events)
	}
	if n := len(scene.missedLines(t)); n != 0 {
		t.Errorf("missed.log holds %d bytes, want none", n)
	}
	t.Logf("archive held open for hook observation: %v; hook elapsed %v", time.Since(started), time.Since(proc.started))
	release()
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.removed < 50000 {
		t.Errorf("prune removed %d, want at least the 50000 seeded expired rows", result.removed)
	}
	if got := scene.lab.count("SELECT COUNT(*) FROM events WHERE event = ?", callmeter.EventSessionEnd); got != 1 {
		t.Errorf("SessionEnd events after prune = %d, want 1", got)
	}
}
