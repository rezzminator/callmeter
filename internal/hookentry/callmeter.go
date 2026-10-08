package hookentry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/rezzminator/callmeter/internal/applog"
	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
)

// callmeterPayload is the part of a hook's stdin callmeter reads, across the
// events it is registered on (docs/design.md § What the harness gives a hook).
type callmeterPayload struct {
	HookEventName       string          `json:"hook_event_name"`
	SessionID           string          `json:"session_id"`
	TranscriptPath      string          `json:"transcript_path"`
	Cwd                 string          `json:"cwd"`
	AgentID             string          `json:"agent_id"`
	AgentType           string          `json:"agent_type"`
	AgentTranscriptPath string          `json:"agent_transcript_path"`
	ToolName            string          `json:"tool_name"`
	ToolUseID           string          `json:"tool_use_id"`
	ToolInput           json.RawMessage `json:"tool_input"`
	ToolResponse        json.RawMessage `json:"tool_response"`
	DurationMS          *float64        `json:"duration_ms"`
	Error               *string         `json:"error"`
	ToolCalls           []struct {
		ToolUseID    string          `json:"tool_use_id"`
		ToolName     string          `json:"tool_name"`
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
	} `json:"tool_calls"`
	PromptID       string          `json:"prompt_id"`
	PermissionMode string          `json:"permission_mode"`
	Effort         json.RawMessage `json:"effort"` // {"level": …}
	IsInterrupt    *bool           `json:"is_interrupt"`
	Source         string          `json:"source"`
	Model          string          `json:"model"`
	Reason         string          `json:"reason"`
	Trigger        string          `json:"trigger"`
	LoadReason     string          `json:"load_reason"`
	MemoryType     string          `json:"memory_type"`
	FilePath       string          `json:"file_path"`
	CommandName    string          `json:"command_name"`
	TaskID         string          `json:"task_id"`
	StopHookActive *bool           `json:"stop_hook_active"`
	// Free text and the lists that carry it: only measured or sanitized,
	// never stored as they are.
	Prompt               json.RawMessage `json:"prompt"`
	LastAssistantMessage json.RawMessage `json:"last_assistant_message"`
	BackgroundTasks      json.RawMessage `json:"background_tasks"`
	SessionCrons         json.RawMessage `json:"session_crons"`
}

// callIDs names each lost call separately; a payload without calls still
// gets one fault with an empty id.
func (p callmeterPayload) callIDs() []string {
	if p.ToolUseID != "" || len(p.ToolCalls) == 0 {
		return []string{p.ToolUseID}
	}
	ids := make([]string, 0, len(p.ToolCalls))
	for _, call := range p.ToolCalls {
		ids = append(ids, call.ToolUseID)
	}
	return ids
}

// callmeterResponse is the part of a PostToolUse tool_response callmeter
// reads for itself: the persisted path and the agent an Agent call spawned.
// The output size is callmeter.RealBytes's and the file columns
// callmeter.FileColumnsFromResult's.
type callmeterResponse struct {
	PersistedOutputPath *string `json:"persistedOutputPath"`
	AgentID             string  `json:"agentId"`
	AgentType           string  `json:"agentType"`
	ResolvedModel       *string `json:"resolvedModel"`
}

// callmeterRun is one `callmeter hook` invocation: one payload, one store, one
// clock reading.
type callmeterRun struct {
	ctx     context.Context
	stderr  io.Writer
	logPath string // callmeter.log; "" = stderr only
	getenv  paths.Getenv
	state   *terminationState // shared with the signal handler; nil = none installed
	store   *callmeter.Store
	missed  string // the wrapper's missed.log: where a run that gave up on a busy store leaves its line
	now     int64
	raw     []byte // the payload as it arrived: detail's source
	eventID string // lowercase hex SHA-256 of raw
	// deadline bounds SessionEnd's transcript reads (sessionEndBudget), zero
	// none; skipped counts the reads it cut off.
	deadline time.Time
	skipped  int
	payload  callmeterPayload
	seat     callmeterSeat
	effort   *string // the effort rule of the store's schema (effortOf)

	// The rows every write of this run carries beside its own (runRows).
	event      *callmeter.Event
	turn       *callmeter.Turn
	agentTurns bool // rebuild the agent_turns of payload.AgentID
	refresh    bool // recompute the session's derived columns
	endsTurn   bool // a Stop or StopFailure: its row replaces a rebuilt turn end
	batches    int  // writes tried: a run that tried none writes its rows alone
	// accounted: the run's event is accounted for, by a committed batch, a
	// fault row or the run's own missed.log line, whatever the signal state.
	// gaveUp: the store stayed locked past the run's wait (giveUpBusy) or
	// could take neither the event nor its fault (giveUp), the run left that
	// line, and it writes nothing more.
	accounted bool
	gaveUp    bool
}

// AccountedPanic carries a panic after the run's event was accounted for.
type AccountedPanic struct{ Value any }

// afterEventCommit is a test seam, nil in production.
var afterEventCommit func()

// closeStore closes the run's store; a variable so a test can make the close fail.
var closeStore = func(s *callmeter.Store) error { return s.Close() }

// Callmeter is the hook entry: it records each hook event into the callmeter
// store under $CALLMETER_HOME. It exits 0 on every path and writes nothing to
// stdout — it changes nothing the model sees — and a failure to record is said
// on stderr, in callmeter.log with the session and tool_use_id, and as a faults
// row whenever the store itself is reachable. A SIGTERM, SIGINT or SIGHUP (a
// headless `claude -p` terminates its running async hooks at exit) that lands
// before the run's event is accounted for leaves one missed.log line instead
// (terminateOnSignal); one of them ignored on entry stays ignored. A store that
// stays locked past the run's wait (storeWait, inside its event's hook timeout)
// leaves the line `terminated by store busy` the same way (giveUpBusy), and one
// that can take neither the event nor its fault the line `store unavailable:
// {class}` (giveUp).
func Callmeter(input io.Reader, stderr io.Writer, getenv paths.Getenv) int {
	home, err := paths.Home(getenv)
	if err != nil {
		// No home, so no log file either: stderr only.
		applog.Failure(stderr, "", callmeter.StageStore, "", "", fmt.Errorf("resolve callmeter home: %w", err))
		return 0
	}
	logPath := paths.Log(home)
	state := &terminationState{}
	defer func() {
		if recovered := recover(); recovered != nil {
			state.repanic(recovered)
		}
	}()
	signals := make(chan os.Signal, 1)
	// A signal ignored on entry (`nohup`, a background job of a non-interactive
	// shell) stays ignored: Notify would install a handler in its place and
	// turn a signal that ended nothing into a terminated run.
	var handled []os.Signal
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		if !signal.Ignored(sig) {
			handled = append(handled, sig)
		}
	}
	if len(handled) > 0 { // Notify with no signal would relay every signal
		signal.Notify(signals, handled...)
	}
	done := make(chan struct{})
	defer func() {
		signal.Stop(signals)
		close(done)
	}()
	go terminateOnSignal(signals, done, state, paths.Missed(home), logPath, stderr, os.Exit)
	seat, err := resolveCallmeterSeat(getenv)
	if err != nil {
		// The rows still land, without their seat; the gap is said, not hidden.
		applog.Failure(stderr, logPath, "seat", "", "", err)
	}
	return runCallmeter(withTerminationState(context.Background(), state), input, stderr, callmeterFiles{
		store: paths.Store(home), log: logPath, missed: paths.Missed(home),
	}, clock.Real, seat, getenv)
}

// terminationState is what a `callmeter hook` run shares with the goroutine
// that handles its termination signal: the event name and session id once the
// payload decoded, and whether the run's event is accounted for, meaning its first
// write batch committed or its first fault row was written. mu is held, from
// the moment a write that can account for the run holds the store's write lock
// until that write's outcome is known, so a signal landing mid-commit waits for
// the outcome; never across a wait for a busy store, so a signal landing there
// is decided at once, well inside the 1.5 s Claude Code gives a cancelled hook
// before its SIGKILL.
type terminationState struct {
	mu        sync.Mutex
	event     string
	session   string
	accounted bool
}

// repanic panics again with a run's recovered panic, for the caller to say:
// as an AccountedPanic when the run's event is accounted for. An unaccounted
// one keeps the state's lock, so the signal handler, which would add its own
// missed.log line beside the panic's, never takes it; the process ends with
// the panic's line alone.
func (state *terminationState) repanic(recovered any) {
	state.mu.Lock()
	if state.accounted {
		state.mu.Unlock()
		panic(AccountedPanic{Value: recovered})
	}
	panic(recovered)
}

// terminationKey carries the terminationState in the ctx runCallmeter gets;
// without it (tests, the fuzz target) a run behaves as if no handler existed.
type terminationKey struct{}

func withTerminationState(ctx context.Context, state *terminationState) context.Context {
	return context.WithValue(ctx, terminationKey{}, state)
}

func terminationStateOf(ctx context.Context) *terminationState {
	state, _ := ctx.Value(terminationKey{}).(*terminationState)
	return state
}

// setPayload records the decoded hook_event_name and session_id for the
// handler's missed line.
func (state *terminationState) setPayload(event, session string) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.event, state.session = event, session
}

// termHold is a write's hold on the terminationState: locked is false when
// there is no state, or the run was already accounted for and needs none.
type termHold struct {
	state  *terminationState
	locked bool
}

// hold takes the state's lock for one write unless the run is already
// accounted for; the caller releases it once the write's outcome is known.
func (state *terminationState) hold() termHold {
	if state == nil {
		return termHold{}
	}
	state.mu.Lock()
	if state.accounted {
		state.mu.Unlock()
		return termHold{}
	}
	return termHold{state: state, locked: true}
}

// account marks the run's event accounted for: its batch committed or its
// fault row was written.
func (hold termHold) account() {
	if hold.locked {
		hold.state.accounted = true
	}
}

func (hold termHold) release() {
	if hold.locked {
		hold.state.mu.Unlock()
	}
}

// terminateOnSignal waits for the first signal on signals and applies the
// rule: a run whose event is not accounted for appends one missed.log line
// (event `unknown` until the payload decoded), a run whose event is accounted
// for appends none, and either way the process exits 0 through exit. It takes
// the state's lock first, so a write in flight, one holding the store's write
// lock, finishes and is judged by its outcome, while a write still waiting on
// a busy store never got the lock and leaves the decision to the signal; the
// lock is not released before exit, so the run writes nothing after the
// decision. A failure to append the line is said on stderr and in
// the log, never a non-zero exit. done ends the wait without a signal. The
// line's reason is `terminated by {SIGNAL}`; a run whose store stays locked
// leaves `terminated by store busy` itself (giveUpBusy).
// A handler panic logs its stack and exits 0 with the lock held; it appends
// one panic line only when neither the run nor this handler accounted for it.
func terminateOnSignal(
	signals <-chan os.Signal,
	done <-chan struct{},
	state *terminationState,
	missed, logPath string,
	stderr io.Writer,
	exit func(int),
) {
	wrote := false
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			state.mu.Lock()
			defer state.mu.Unlock()
			if !state.accounted && !wrote {
				if err := AppendMissed(missed, state.event, state.session, callmeter.PanicReason, clock.Real.Now()); err != nil {
					applog.Failure(stderr, logPath, callmeter.StageTerminated, state.session, "", err)
				}
			}
			applog.Failure(stderr, logPath, callmeter.StageTerminated, state.session, "",
				fmt.Errorf("signal handler panicked: %v\n%s", recovered, stack))
			exit(0)
		}
	}()
	var sig os.Signal
	select {
	case sig = <-signals:
	case <-done:
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.accounted {
		if err := AppendMissed(
			missed, state.event, state.session, callmeter.TerminatedReason+signalName(sig), clock.Real.Now()); err != nil {
			applog.Failure(stderr, logPath, callmeter.StageTerminated, "", "", err)
		} else {
			wrote = true
		}
	}
	exit(0)
}

// AppendMissed appends the binary's own missed.log line,
// `{unix seconds}\t{event}\t{reason} (pid N)` with this process's pid
// (reason opens callmeter.TerminatedReason
// or callmeter.StoreUnavailableReason, or is callmeter.PanicReason), to the
// missed.log at path, followed by `\t{session_id}` when session is one
// (callmeter.MissedSessionID), the wrapper's own layout, so recovery ties a
// lost SessionEnd to its session; event is `unknown` when it is empty or would
// break the line (missedEvent). It creates the file and its directory when
// absent: one write, so concurrent appenders never interleave a line.
func AppendMissed(path, event, session, reason string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the directory of %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	line := fmt.Sprintf("%d\t%s\t%s (pid %d)", now.Unix(), missedEvent(event), reason, os.Getpid())
	if callmeter.MissedSessionID(session) {
		line += "\t" + session
	}
	line += "\n"
	if _, err := file.WriteString(line); err != nil {
		return errors.Join(fmt.Errorf("append to %s: %w", path, err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// missedEvent is the event field of a missed.log line: the payload's
// hook_event_name, or `unknown` when none decoded or it holds a character that
// would break the tab-separated line.
func missedEvent(name string) string {
	if name == "" || strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "unknown"
	}
	return name
}

// signalName is the conventional name of a signal callmeter handles.
func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return sig.String()
}

// callmeterFiles are the files of one run under $CALLMETER_HOME.
type callmeterFiles struct {
	store  string // callmeter.db
	log    string // callmeter.log; "" = stderr only
	missed string // the wrapper's missed.log, ingested on every run
}

func runCallmeter(
	ctx context.Context,
	input io.Reader,
	stderr io.Writer,
	files callmeterFiles,
	timing clock.Clock,
	seat callmeterSeat,
	getenv paths.Getenv,
) int {
	run := &callmeterRun{
		ctx: ctx, stderr: stderr, logPath: files.log, getenv: getenv, now: timing.Now().UnixMilli(), seat: seat,
		state: terminationStateOf(ctx), missed: files.missed,
	}
	raw, readErr := io.ReadAll(input)
	run.raw = raw
	digest := sha256.Sum256(raw)
	run.eventID = hex.EncodeToString(digest[:])
	var payloadErr error
	if readErr != nil {
		payloadErr = fmt.Errorf("read hook payload: %w", readErr)
	} else if err := json.Unmarshal(raw, &run.payload); err != nil {
		payloadErr = fmt.Errorf("decode hook payload (%d bytes): %w", len(raw), err)
	} else {
		run.state.setPayload(run.payload.HookEventName, run.payload.SessionID)
	}
	store, err := callmeter.OpenDBWaiting(ctx, files.store, storeWait(run.payload.HookEventName))
	if err != nil {
		for _, id := range run.payload.callIDs() {
			run.fault(callmeter.StageStore, id, errors.Join(payloadErr, err))
		}
		return 0
	}
	run.store = store
	defer func() {
		if err := closeStore(store); err != nil {
			run.store = nil // closed: the fault is said, never written
			for _, id := range run.payload.callIDs() {
				run.fault(callmeter.StageStore, id, err)
			}
		}
	}()
	// The wrapper's own failures (no binary, a bad checksum) land as binary
	// faults on the next run that reaches the store; failing to ingest them
	// is one store fault, except on a busy store: there the run gives up
	// (faultHeld, giveUpBusy) and leaves its own line in missed.log, since a
	// second wait could outlast a sync hook's timeout. The signal handler
	// is held off from the commit until the claim is moved, so a signal there
	// leaves no committed claim in place to be ingested again.
	if _, err := store.IngestMissedHeld(ctx, files.missed, func() func() { return run.state.hold().release }); err != nil {
		run.fault(callmeter.StageStore, "", fmt.Errorf("ingest %s: %w", files.missed, err))
	}
	if run.gaveUp {
		return 0
	}
	if payloadErr != nil {
		run.fault(callmeter.StagePayload, run.payload.ToolUseID, payloadErr)
		return 0
	}
	run.effort = effortOf(run.payload.Effort, run.payload.HookEventName, getenv, func() (string, error) {
		if run.payload.HookEventName == callmeter.EventSessionStart {
			return run.payload.Model, nil
		}
		return run.store.SessionModel(run.ctx, run.payload.SessionID)
	})
	run.record()
	return 0
}

// hookTimeouts are the timeouts of the sync hooks of plugins/callmeter/hooks/hooks.json,
// the one place they live here (TestHookTimeoutsAgreeWithHooksJSON): an event
// absent from it is async, with no timeout of its own.
var hookTimeouts = map[string]time.Duration{
	callmeter.EventSessionStart: 60 * time.Second,
	callmeter.EventSessionEnd:   2 * time.Second,
	callmeter.EventStopFailure:  2 * time.Second,
}

// asyncHookTimeout is Claude Code's default timeout for a command hook that
// sets none, which is every async hook of hooks.json.
const asyncHookTimeout = 600 * time.Second

// storeWaitReserve is what a hook keeps of its timeout beyond its store waits:
// process start, the payload read and the exit.
const storeWaitReserve = 400 * time.Millisecond

// storeWait is how long one statement on the store waits for a concurrent
// writer in a run of event, so that a run whose store stays locked gives up
// (giveUpBusy) inside the timeout of its hook instead of being killed by it. An
// event with no timeout in hookTimeouts gets asyncHookTimeout when callmeter
// records it, otherwise the strictest bound there is: an undecoded or unknown
// event may be any hook. The wait is half the timeout less the reserve, so a
// statement that succeeds after a full wait and the next that then fails still
// end inside it, and never more than callmeter.BusyTimeout.
func storeWait(event string) time.Duration {
	timeout, ok := hookTimeouts[event]
	if !ok {
		timeout = asyncHookTimeout
		if !recordedEvent(event) {
			for _, bound := range hookTimeouts {
				timeout = min(timeout, bound)
			}
		}
	}
	return min((timeout-storeWaitReserve)/2, callmeter.BusyTimeout)
}

// recordedEvent reports whether callmeter records the hook event name: one of
// lifecycleEvents, or a tool-call, batch, sub-agent or turn edge record
// dispatches itself.
func recordedEvent(name string) bool {
	switch name {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "PostToolBatch",
		callmeter.EventSubagentStart, callmeter.EventSubagentStop, eventStop:
		return true
	}
	return lifecycleEvents[name]
}

// faultEventName is a payload's hook_event_name as a fault states it: quoted when
// callmeter records that event, otherwise only its size, since a name off that
// list may be any text.
func faultEventName(name string) string {
	if recordedEvent(name) {
		return fmt.Sprintf("%q", name)
	}
	return fmt.Sprintf("(name not stored, %d bytes)", len(name))
}

func (run *callmeterRun) record() {
	p := run.payload
	if p.SessionID == "" {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("%s payload carries no session_id", faultEventName(p.HookEventName)))
		return
	}
	switch p.HookEventName {
	case "PreToolUse":
		run.recordStartCwd()
	case "PostToolUse", "PostToolUseFailure":
		run.recordCall(p.HookEventName == "PostToolUseFailure")
	case "PostToolBatch":
		run.recordBatch()
	case callmeter.EventSubagentStart, callmeter.EventSubagentStop:
		stopped := p.HookEventName == callmeter.EventSubagentStop
		run.event = run.eventOf()
		if stopped {
			run.turn = run.turnOf()
		}
		run.recordAgent(stopped)
	case eventStop:
		run.event, run.turn = run.eventOf(), run.turnOf()
		run.refresh = true // the main chat's requests resolve here
		run.endsTurn = true
		run.resolvePending("", p.TranscriptPath)
		run.resolveAgentsPending(p.TranscriptPath)
		run.resolveUnfinished()
		run.sweepSession(false)
	default:
		if !lifecycleEvents[p.HookEventName] {
			run.fault(
				callmeter.StagePayload,
				p.ToolUseID,
				fmt.Errorf("event %s is not one callmeter records", faultEventName(p.HookEventName)),
			)
			return
		}
		run.event = run.eventOf()
		run.refresh = p.HookEventName == callmeter.EventSessionStart || p.HookEventName == callmeter.EventSessionEnd
		if p.HookEventName == callmeter.EventSessionEnd {
			// Claude Code kills this sync hook at its timeout and at exit, so
			// the event's own rows commit before any transcript is read. A
			// session can end mid-turn, before the Stop that reads its
			// requests and settles its calls: the hooks' last chance is here,
			// within sessionEndBudget. Claude Code may end a session without
			// running SessionEnd; report-time recovery settles that session
			// once it is quiet (callmeter.RecoverQuiet).
			run.write(p.ToolUseID, nil)
			if run.gaveUp {
				return
			}
			run.deadline = clock.Real.Now().Add(sessionEndBudget)
			run.resolvePending("", p.TranscriptPath)
			run.resolveUnfinished()
			run.resolveAgentsPending(p.TranscriptPath)
			run.sweepSession(true)
			run.recoverTurnEnd()
			if run.skipped > 0 {
				run.fault(callmeter.StageTranscript, "", fmt.Errorf(
					"SessionEnd spent its %v budget: %d transcript reads skipped, their requests and calls left as their hooks wrote them",
					sessionEndBudget, run.skipped))
			}
		}
		run.endsTurn = p.HookEventName == callmeter.EventStopFailure
	}
	if run.batches == 0 {
		run.write(p.ToolUseID, nil) // this run's own rows: events, turns, the session
	}
}

// sessionEndBudget bounds SessionEnd's transcript reads, which start after its
// event committed: its hook times out at hookTimeouts, and a read started
// within the budget runs to its end.
var sessionEndBudget = 1200 * time.Millisecond

// unfinishedSettle bounds how long SessionEnd waits, per transcript, for the
// tool_result of a call that has none yet: Claude Code flushes a killed call's
// result 50-100 ms after SessionEnd starts, so a read at its start finds the
// transcript short. Bounded so the sweeps after it keep their share of
// sessionEndBudget.
var unfinishedSettle = 400 * time.Millisecond

// spent reports whether SessionEnd's budget has run out.
func (run *callmeterRun) spent() bool {
	return !run.deadline.IsZero() && !clock.Real.Now().Before(run.deadline)
}

// overBudget reports, and counts, a transcript read SessionEnd's budget cuts off.
func (run *callmeterRun) overBudget() bool {
	if !run.spent() {
		return false
	}
	run.skipped++
	return true
}

// base is the columns every hook-written call carries.
func (run *callmeterRun) base(toolUseID string) callmeter.Call {
	p := run.payload
	call := callmeter.Call{
		ToolUseID: toolUseID,
		SessionID: callmeter.Ptr(p.SessionID),
		Source:    callmeter.Ptr(callmeter.SourceHook),
	}
	if p.AgentID != "" {
		call.AgentID = callmeter.Ptr(p.AgentID)
		call.AgentType = presentString(p.AgentType)
	}
	call.ConfigDir, call.SeatDir = run.seat.configDir, run.seat.dir
	return call
}

// tier1 sets the prompt, effort and permission mode the payload ran under.
func (run *callmeterRun) tier1(call *callmeter.Call) {
	p := run.payload
	call.PromptID = presentString(p.PromptID)
	call.Effort = run.effort
	call.PermissionMode = presentString(p.PermissionMode)
}

func (run *callmeterRun) recordCall(failed bool) {
	p := run.payload
	if p.ToolUseID == "" || p.ToolName == "" {
		run.fault(
			callmeter.StagePayload,
			p.ToolUseID,
			fmt.Errorf("%s payload lacks tool_use_id or tool_name", p.HookEventName),
		)
		return
	}
	call := run.base(p.ToolUseID)
	run.tier1(&call)
	prompt := call.PromptID
	call.PromptID = nil
	call.TS = callmeter.Ptr(run.now)
	call.Tool = callmeter.Ptr(p.ToolName)
	call.DurationMS = wholeNumber(p.DurationMS)
	if call.DurationMS != nil && *call.DurationMS >= 0 {
		call.TS = callmeter.Ptr(run.now - *call.DurationMS)
	}
	call.Failed = callmeter.Ptr(failed)
	call.IsInterrupt = p.IsInterrupt
	run.inputColumns(&call, p.ToolUseID, p.ToolName, p.ToolInput)
	run.fileColumns(&call, p.ToolUseID, p.ToolName, p.ToolInput)
	var agent *callmeter.Agent
	if failed {
		if p.Error != nil {
			call.Error = callmeter.Ptr(callmeter.SanitizeError(*p.Error))
			if p.ToolName == "Bash" {
				command, _ := callmeter.BashCommand(p.ToolInput) // inputColumns already reports malformed input
				callmeter.CommitFromText(&call, command, *p.Error)
			}
		}
		// A failure's output reaches the model as this text: it is the size.
		if p.Error != nil {
			call.BytesReal = callmeter.Ptr(int64(len(*p.Error)))
		}
	} else if len(p.ToolResponse) > 0 {
		agent = run.responseColumns(&call)
	}
	run.write(p.ToolUseID, func(tx *callmeter.Tx) error {
		if err := tx.UpsertCall(run.ctx, call, callmeter.Overwrite); err != nil {
			return err
		}
		// This cwd is where the command left the shell; PreToolUse's, the
		// directory it started in, wins whenever it lands (recordStartCwd).
		after := callmeter.Call{ToolUseID: p.ToolUseID, Cwd: presentString(p.Cwd), PromptID: prompt}
		if err := tx.UpsertCall(run.ctx, after, callmeter.FillEmpty); err != nil {
			return err
		}
		if agent == nil {
			return nil
		}
		return tx.UpsertAgent(run.ctx, *agent, callmeter.Overwrite)
	})
}

// recordStartCwd stores the directory a Bash command starts in. PostToolUse's
// cwd follows the command's own `cd`, and the parser replays that `cd` from the
// stored cwd, so only PreToolUse's is right; it overwrites whatever PostToolUse
// filled, and PostToolUse never overwrites it, in either landing order. ts is
// set so a call that never finishes still ages out of the store; the earliest
// start wins, including PostToolUse's duration estimate (Call.TS). The command's
// input and test runner only fill, so a call whose PostToolUse is lost still
// has them, and a PostToolUse landing in either order overwrites them.
func (run *callmeterRun) recordStartCwd() {
	p := run.payload
	if p.ToolUseID == "" || p.Cwd == "" {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("PreToolUse payload lacks tool_use_id or cwd"))
		return
	}
	// One of Claude Code's own internal agents fires PreToolUse and nothing
	// after it that is metered (recordBatch, recordAgent): a start row would
	// wait forever for a size.
	if callmeter.UntypedAgentMissingTranscript(
		p.AgentID, p.AgentType, callmeter.SubagentTranscriptPath(p.TranscriptPath, p.AgentID),
	) {
		return
	}
	call := run.base(p.ToolUseID)
	call.Cwd = callmeter.Ptr(p.Cwd)
	call.PromptID = presentString(p.PromptID)
	// The issuing prompt wins; PostToolUse's effort and mode still win.
	start := callmeter.Call{ToolUseID: p.ToolUseID, TS: callmeter.Ptr(run.now), Tool: presentString(p.ToolName)}
	run.tier1(&start)
	start.PromptID = nil
	run.inputColumns(&start, p.ToolUseID, p.ToolName, p.ToolInput)
	run.write(p.ToolUseID, func(tx *callmeter.Tx) error {
		if err := tx.UpsertCall(run.ctx, call, callmeter.Overwrite); err != nil {
			return err
		}
		return tx.UpsertCall(run.ctx, start, callmeter.FillEmpty)
	})
}

// inputColumns sets the call's sanitized input and its test runner when the
// payload carries a tool_input; an input SanitizeInput refuses is a payload
// fault and leaves both unset, the rest of the row still lands.
func (run *callmeterRun) inputColumns(call *callmeter.Call, toolUseID, toolName string, toolInput json.RawMessage) {
	if len(toolInput) == 0 {
		return
	}
	input, err := callmeter.SanitizeInput(toolName, toolInput)
	if err != nil {
		run.fault(callmeter.StagePayload, toolUseID, err)
		return
	}
	call.Input = &input
	run.testRunner(call, toolUseID, toolName, toolInput)
}

// testRunner sets the test runner a Bash call's command starts, on success and
// failure alike (callmeter.TestRunnerOf). Only a call whose input decoded
// reaches it: SanitizeInput has already faulted one that did not.
func (run *callmeterRun) testRunner(call *callmeter.Call, toolUseID, toolName string, toolInput json.RawMessage) {
	if toolName != "Bash" {
		return
	}
	command, err := callmeter.BashCommand(toolInput)
	if err != nil {
		run.fault(callmeter.StagePayload, toolUseID, err)
		return
	}
	if runner := callmeter.TestRunnerOf(command); runner != "" {
		call.TestRunner = callmeter.Ptr(runner)
	}
}

// fileColumns sets the file columns a file tool's input names
// (callmeter.FileColumnsFromInput) and the file's size now. An input it cannot
// read is a fault; a missing file leaves file_bytes NULL; any other stat
// failure is a fault and leaves it NULL too.
func (run *callmeterRun) fileColumns(call *callmeter.Call, toolUseID, toolName string, toolInput json.RawMessage) {
	p := run.payload
	if err := callmeter.FileColumnsFromInput(call, toolName, toolInput, p.Cwd); err != nil {
		run.fault(callmeter.StagePayload, toolUseID, err)
		return
	}
	if call.FilePath == nil {
		return // not a file tool
	}
	path := *call.FilePath
	info, err := os.Stat(path)
	switch {
	case err == nil:
		call.FileBytes = callmeter.Ptr(info.Size())
	case !errors.Is(err, fs.ErrNotExist):
		run.fault(callmeter.StagePayload, toolUseID, fmt.Errorf("stat %s: %w", path, err))
	}
}

// responseColumns reads the tool's own result: the real output size, the
// persisted path, the file columns, and — for an Agent or
// Task call — the sub-agent it spawned.
func (run *callmeterRun) responseColumns(call *callmeter.Call) *callmeter.Agent {
	p := run.payload
	trimmed := bytes.TrimSpace(p.ToolResponse)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if !callmeter.HasRealOutput(p.ToolName) {
			if _, err := callmeter.DeliveredBytes(trimmed); err != nil {
				run.fault(callmeter.StagePayload, p.ToolUseID, err)
				return nil
			}
		}
		size, err := callmeter.RealBytes(p.ToolName, trimmed)
		if err != nil {
			run.fault(callmeter.StagePayload, p.ToolUseID, err)
			return nil
		}
		call.BytesReal = size
		return nil
	}
	var response callmeterResponse
	if err := json.Unmarshal(trimmed, &response); err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("decode %s tool_response: %w", p.ToolName, err))
		return nil
	}
	// The size is callmeter.RealBytes's, the one path a transcript's
	// toolUseResult is measured by too.
	size, err := callmeter.RealBytes(p.ToolName, trimmed)
	if err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
	} else {
		call.BytesReal = size
	}
	call.PersistedPath = response.PersistedOutputPath
	if err := callmeter.FileColumnsFromResult(call, p.ToolName, trimmed); err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
	}
	// An outcome that cannot be read leaves its columns NULL and is one fault:
	// the call row is written all the same.
	if err := callmeter.OutcomeColumns(call, p.ToolName, p.ToolInput, trimmed); err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
	}
	if (p.ToolName != "Agent" && p.ToolName != "Task") || response.AgentID == "" {
		return nil
	}
	return &callmeter.Agent{
		AgentID:         response.AgentID,
		SessionID:       callmeter.Ptr(p.SessionID),
		AgentType:       presentString(response.AgentType),
		ParentToolUseID: callmeter.Ptr(p.ToolUseID),
		Model:           response.ResolvedModel,
		Source:          callmeter.Ptr(callmeter.SourceHook),
		ConfigDir:       run.seat.configDir,
		SeatDir:         run.seat.dir,
	}
}

// recordBatch writes each call's delivered bytes and the model request that
// grouped them: the transcript's message when it is on disk, a provisional
// pending row when it is not yet, and a transcript fault besides when the
// transcript could not be read.
func (run *callmeterRun) recordBatch() {
	p := run.payload
	if len(p.ToolCalls) == 0 {
		run.fault(callmeter.StagePayload, "", errors.New(callmeter.BatchWithoutCalls))
		return
	}
	transcript := p.TranscriptPath
	if p.AgentID != "" {
		transcript = callmeter.SubagentTranscriptPath(p.TranscriptPath, p.AgentID)
	}
	// Claude Code's own internal agents (agent_id set, agent_type empty) have
	// no SubagentStart and no transcript on disk: nothing to meter, so no
	// call, no request and no transcript fault (recordAgent's exemption,
	// shared by callmeter.UntypedAgentMissingTranscript).
	if callmeter.UntypedAgentMissingTranscript(p.AgentID, p.AgentType, transcript) {
		return
	}
	ids := make([]string, 0, len(p.ToolCalls))
	calls := make([]callmeter.Call, 0, len(p.ToolCalls))
	// What the batch knows of a call beside its delivery only fills: a call
	// the harness refused before any PreToolUse (a blocked command, a tool the
	// session lacks) has its batch entry alone, and PostToolUse's columns win
	// in either landing order. That includes its input and file columns, from
	// the batch entry's own tool_input: only Bash has a PreToolUse hook, so a
	// refused Write or Edit would keep them NULL; an earlier hook's are never
	// overwritten (callmeter.FillEmpty).
	fills := make([]callmeter.Call, 0, len(p.ToolCalls))
	for _, toolCall := range p.ToolCalls {
		if toolCall.ToolUseID == "" {
			run.fault(callmeter.StagePayload, "", errors.New("PostToolBatch call carries no tool_use_id"))
			return
		}
		call := run.base(toolCall.ToolUseID)
		call.PromptID = presentString(p.PromptID)
		size, err := callmeter.DeliveredBytes(toolCall.ToolResponse)
		if err != nil {
			run.fault(callmeter.StagePayload, toolCall.ToolUseID, err)
		} else {
			call.BytesDelivered = callmeter.Ptr(size)
		}
		ids = append(ids, toolCall.ToolUseID)
		calls = append(calls, call)
		// The batch's ts is the call's own when no PreToolUse or PostToolUse
		// ever lands (a refused call); an earlier one wins (Call.TS).
		fill := callmeter.Call{ToolUseID: toolCall.ToolUseID, TS: callmeter.Ptr(run.now), Tool: presentString(toolCall.ToolName)}
		run.tier1(&fill)
		fill.PromptID = nil
		if len(toolCall.ToolInput) > 0 {
			run.inputColumns(&fill, toolCall.ToolUseID, toolCall.ToolName, toolCall.ToolInput)
			run.fileColumns(&fill, toolCall.ToolUseID, toolCall.ToolName, toolCall.ToolInput)
		}
		// A call Claude Code never ran — refused, denied by a hook or by
		// permission, rejected by the user — fires no PostToolUse: its batch
		// text alone says so, kept as an outcome label, never as that text.
		var text string
		if json.Unmarshal(toolCall.ToolResponse, &text) == nil {
			if outcome, failed := callmeter.ResultOutcome(text); failed {
				fill.Failed = callmeter.Ptr(true)
				fill.Error = presentString(outcome)
				// That text is the call's whole output, as a failure's is.
				fill.BytesReal = callmeter.Ptr(int64(len(text)))
			}
		}
		fills = append(fills, fill)
	}
	found, err := callmeter.FindRequests(transcript, ids)
	if err != nil {
		run.fault(callmeter.StageTranscript, ids[0], err)
		found = nil
	}
	requests := map[string]*callmeter.Request{}
	var order []string
	provisional := ""
	for i := range calls {
		key := ""
		usage, ok := found[calls[i].ToolUseID]
		switch {
		case ok:
			key = usage.MessageID
		case provisional == "":
			provisional = callmeter.ProvisionalKey(calls[i].ToolUseID)
			key = provisional
		default:
			key = provisional
		}
		calls[i].RequestID = callmeter.Ptr(key)
		request, seen := requests[key]
		if !seen {
			request = run.request(key)
			if ok {
				callmeter.ApplyUsage(request, usage)
				request.Pending = callmeter.Ptr(false)
			} else {
				request.TS = callmeter.Ptr(run.now)
				request.Pending = callmeter.Ptr(true)
			}
			request.Calls = callmeter.Ptr(int64(0))
			requests[key] = request
			order = append(order, key)
		}
		*request.Calls++
	}
	run.refresh = p.AgentID == "" // a main-chat request may name the session's model
	run.write(ids[0], func(tx *callmeter.Tx) error {
		for _, key := range order {
			if err := tx.UpsertRequest(run.ctx, *requests[key], callmeter.Overwrite); err != nil {
				return err
			}
		}
		for i := range calls {
			if err := tx.UpsertCall(run.ctx, calls[i], callmeter.Overwrite); err != nil {
				return err
			}
			if err := tx.UpsertCall(run.ctx, fills[i], callmeter.FillEmpty); err != nil {
				return err
			}
		}
		// A redelivered batch, or one whose provisional twin was resolved,
		// counts the calls carrying the key, never its own tally over them.
		for _, key := range order {
			if err := tx.RecountRequest(run.ctx, key); err != nil {
				return err
			}
		}
		return nil
	})
	// An earlier batch of this chat or sub-agent may have run before its
	// request reached the disk; retry it now rather than at the Stop, which
	// for a long sub-agent is an hour away. A transcript FindRequests could
	// not read would only fail the same way again: one fault, not two.
	if err == nil {
		run.resolvePending(p.AgentID, transcript)
	}
}

// request is a requests row carrying this payload's owner columns; the main
// chat's agent_id stays NULL, the key PendingRequests looks it up by.
func (run *callmeterRun) request(key string) *callmeter.Request {
	p := run.payload
	request := &callmeter.Request{
		RequestID: key,
		SessionID: callmeter.Ptr(p.SessionID),
		PromptID:  presentString(p.PromptID),
		Source:    callmeter.Ptr(callmeter.SourceHook),
		ConfigDir: run.seat.configDir,
		SeatDir:   run.seat.dir,
	}
	if p.AgentID != "" {
		request.AgentID = callmeter.Ptr(p.AgentID)
	}
	return request
}

func (run *callmeterRun) recordAgent(stopped bool) {
	p := run.payload
	if p.AgentID == "" {
		run.fault(callmeter.StagePayload, "", fmt.Errorf("%s payload carries no agent_id", p.HookEventName))
		return
	}
	agent := callmeter.Agent{
		AgentID:   p.AgentID,
		SessionID: callmeter.Ptr(p.SessionID),
		AgentType: presentString(p.AgentType),
		Source:    callmeter.Ptr(callmeter.SourceHook),
		ConfigDir: run.seat.configDir,
		SeatDir:   run.seat.dir,
	}
	meta := callmeter.Agent{AgentID: p.AgentID}
	readMeta := func() {
		if p.TranscriptPath != "" {
			if agentType, parent, err := callmeter.SubagentMeta(p.TranscriptPath, p.AgentID); err == nil {
				meta.ParentToolUseID = presentString(parent)
				meta.AgentType = presentString(agentType)
			}
		}
	}
	if !stopped {
		// An agent woken for another turn starts again: started keeps the
		// earliest start, whatever order the starts land in, and the prompt
		// follows that start.
		start := callmeter.Agent{AgentID: p.AgentID, Started: callmeter.Ptr(run.now)}
		prompt := callmeter.Agent{AgentID: p.AgentID, PromptID: presentString(p.PromptID)}
		readMeta()
		run.agentTurns = true
		run.write("", func(tx *callmeter.Tx) error {
			if err := tx.UpsertAgent(run.ctx, agent, callmeter.Overwrite); err != nil {
				return err
			}
			// Read inside the write, after its first write took the lock, so
			// no concurrent start lands between the read and the write.
			started, _, err := tx.AgentSpan(run.ctx, p.AgentID)
			if err != nil {
				return err
			}
			promptMode := callmeter.FillEmpty
			if !started.Valid || run.now <= started.Int64 {
				promptMode = callmeter.Overwrite
			}
			if err := tx.UpsertAgent(run.ctx, start, callmeter.KeepMin); err != nil {
				return err
			}
			if meta.ParentToolUseID != nil || meta.AgentType != nil {
				if err := tx.UpsertAgent(run.ctx, meta, callmeter.FillEmpty); err != nil {
					return err
				}
			}
			return tx.UpsertAgent(run.ctx, prompt, promptMode)
		})
		return
	}
	transcript := p.AgentTranscriptPath
	if transcript == "" {
		transcript = callmeter.SubagentTranscriptPath(p.TranscriptPath, p.AgentID)
	}
	// Claude Code stops its own internal agents (a compaction) with no
	// agent_type and no transcript on disk (never a SubagentStart): no
	// sub-agent to meter, so no agents row, no turn of it, no settle wait and
	// no fault; the run's events and turns rows still land. A typed agent
	// missing its transcript stays a fault.
	if callmeter.UntypedAgentMissingTranscript(p.AgentID, p.AgentType, transcript) {
		return
	}
	readMeta()
	agent.TranscriptPath = callmeter.Ptr(transcript)
	stop := callmeter.Agent{AgentID: p.AgentID, Stopped: callmeter.Ptr(run.now)}
	prompt := callmeter.Agent{AgentID: p.AgentID, PromptID: presentString(p.PromptID)}
	// The totals are the agent's own transcript summed at its latest stop:
	// every turn it ran. They overwrite only when this stop is the latest
	// stored, so a late-landing earlier stop never rolls them back; the model
	// only fills, so the Agent result's resolved model stays.
	var totals, model *callmeter.Agent
	if sum, err := run.settledAgentTotals(transcript); err != nil {
		run.fault(callmeter.StageTranscript, "", fmt.Errorf("agent %s totals: %w", p.AgentID, err))
	} else {
		totals = &callmeter.Agent{
			AgentID:     p.AgentID,
			TotalTokens: callmeter.Ptr(sum.TotalTokens),
			ToolUses:    callmeter.Ptr(sum.ToolUses),
		}
		model = &callmeter.Agent{AgentID: p.AgentID, Model: presentString(sum.Model)}
	}
	run.agentTurns = true
	run.write("", func(tx *callmeter.Tx) error {
		if err := tx.UpsertAgent(run.ctx, agent, callmeter.Overwrite); err != nil {
			return err
		}
		// Read inside the write, after its first write took the lock, so two
		// stops landing at once never both see themselves as the latest.
		_, stopped, err := tx.AgentSpan(run.ctx, p.AgentID)
		if err != nil {
			return err
		}
		latest := !stopped.Valid || run.now >= stopped.Int64
		if err := tx.UpsertAgent(run.ctx, stop, callmeter.KeepMax); err != nil {
			return err
		}
		if err := tx.UpsertAgent(run.ctx, prompt, callmeter.FillEmpty); err != nil {
			return err
		}
		if meta.ParentToolUseID != nil || meta.AgentType != nil {
			if err := tx.UpsertAgent(run.ctx, meta, callmeter.FillEmpty); err != nil {
				return err
			}
		}
		if totals == nil || !latest {
			return nil // no totals read, or an earlier stop than the one stored
		}
		if err := tx.UpsertAgent(run.ctx, *totals, callmeter.Overwrite); err != nil {
			return err
		}
		return tx.UpsertAgent(run.ctx, *model, callmeter.FillEmpty)
	})
	run.resolvePending(p.AgentID, transcript)
	run.sweepRequests(p.AgentID, transcript, false)
}

// agentSettle bounds how long SubagentStop waits for the agent's final
// message: Claude Code fires the hook 20-50 ms after stamping that message and
// before flushing its line, so an immediate read sums every message but the
// last. The hook is async, so the wait never holds the model.
var agentSettle = 3 * time.Second

// settledAgentTotals reads the agent transcript until its last assistant
// entry ends the turn of the stop's prompt_id, no user entry of that turn
// after it (AgentTotals.Final: an agent woken again has its earlier turn's end
// on disk, then the prompt that woke it, then this turn's answer), or
// agentSettle has passed — a
// killed agent never writes one — and returns the last read. It waits on
// another process's write, so it runs on the wall clock, never a test's fake.
func (run *callmeterRun) settledAgentTotals(transcript string) (callmeter.AgentTotals, error) {
	deadline := clock.Real.Now().Add(agentSettle)
	for {
		sum, err := callmeter.ReadAgentTotals(transcript, run.payload.PromptID)
		if err != nil || sum.Final || !clock.Real.Now().Before(deadline) {
			return sum, err
		}
		if err := clock.Real.Sleep(run.ctx, 25*time.Millisecond); err != nil {
			return sum, fmt.Errorf("wait for the final message of %s: %w", transcript, err)
		}
	}
}

// resolvePending fills the pending requests of one chat or sub-agent from its
// transcript. A request still absent stays pending — the next batch of the same
// chat or agent, its Stop or SubagentStop, the main chat's Stop and SessionEnd
// try again.
func (run *callmeterRun) resolvePending(agentID, transcript string) {
	p := run.payload
	pending, err := run.store.PendingRequests(run.ctx, p.SessionID, agentID)
	if err != nil {
		run.fault(callmeter.StageStore, "", err)
		return
	}
	if len(pending) > 0 && run.overBudget() {
		return
	}
	if len(pending) == 0 {
		return
	}
	var ids []string
	for _, request := range pending {
		ids = append(ids, request.CallIDs...)
	}
	found, err := callmeter.FindRequests(transcript, ids)
	if err != nil {
		run.fault(callmeter.StageTranscript, ids[0], err)
		return
	}
	run.write(ids[0], func(tx *callmeter.Tx) error {
		return tx.ResolvePendingFrom(run.ctx, pending, found, callmeter.SourceHook)
	})
}

// resolveAgentsPending fills, at the main chat's Stop and at SessionEnd, the
// pending requests of every sub-agent of the session from its own transcript:
// a sub-agent killed before its SubagentStop, or outliving a headless run, has
// no later hook of its own to read them. A transcript not on disk is skipped:
// its batch already recorded that transcript fault, and the report names the
// request by it.
func (run *callmeterRun) resolveAgentsPending(transcript string) {
	p := run.payload
	if transcript == "" {
		return // no transcript named: the requests stay pending, and the report says why
	}
	agents, err := run.store.PendingAgents(run.ctx, p.SessionID)
	if err != nil {
		run.fault(callmeter.StageStore, "", err)
		return
	}
	for _, agentID := range agents {
		path := callmeter.SubagentTranscriptPath(transcript, agentID)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		run.resolvePending(agentID, path)
	}
}

// sweepSession writes, at the main chat's Stop and at SessionEnd, every model
// request its transcripts hold at its final usage, a reply with no tool call
// included, which no PostToolBatch reads: the main chat's transcript, once its
// last message is on disk, and the sub-agents' — at SessionEnd every one on
// disk, those no SubagentStop after their last start swept first, at Stop those of agents with no stop recorded (killed, interrupted or
// still running), since a stopped agent's own SubagentStop swept it. A request
// stored from an earlier read is corrected to the transcript's final usage;
// one from before callmeter first saw the session is not back-filled
// (Tx.SettleRequest).
func (run *callmeterRun) sweepSession(allAgents bool) {
	p := run.payload
	if p.TranscriptPath == "" {
		return // no transcript named: nothing to read, and the batches' rows stand
	}
	// At Stop the turn's last message may not be flushed yet; at SessionEnd it
	// was, or the turn was cut off and never writes one: no wait.
	run.sweepRequests("", p.TranscriptPath, !allAgents)
	var agents, again []string
	if allAgents {
		settled, err := run.store.SettledAgents(run.ctx, p.SessionID)
		if err != nil {
			run.fault(callmeter.StageStore, "", err) // unknown: every agent on disk is read
		}
		ids, err := callmeter.SubagentTranscripts(p.TranscriptPath)
		if err != nil {
			run.fault(callmeter.StageTranscript, "", err)
			return
		}
		for _, agentID := range ids {
			if settled[agentID] {
				again = append(again, agentID)
			} else {
				agents = append(agents, agentID)
			}
		}
	} else {
		var err error
		if agents, err = run.store.UnstoppedAgents(run.ctx, p.SessionID); err != nil {
			run.fault(callmeter.StageStore, "", err)
			return
		}
	}
	for _, agentID := range agents {
		run.sweepRequests(agentID, callmeter.SubagentTranscriptPath(p.TranscriptPath, agentID), false)
	}
	// A settled agent's own stop swept it: it is read again only while the
	// budget lasts, for a stop cut off before its sweep committed, and one
	// left unread is no skip.
	for _, agentID := range again {
		if run.spent() {
			break
		}
		run.sweepRequests(agentID, callmeter.SubagentTranscriptPath(p.TranscriptPath, agentID), false)
	}
}

// sweepRequests writes every request of one chat's or sub-agent's transcript
// (callmeter.ReadTranscript) through Tx.SettleRequest, and the marks of the
// same read the store lacks through Tx.PutMarks. Every transcript and store
// read happens before the write closure, which only writes. settle waits, as
// SubagentStop does, until the last message is on disk: Claude Code fires the
// hook before flushing it. A path holding no transcript file is skipped, as
// resolveAgentsPending skips one: an untyped internal agent and a session run
// without persistence have none, and a request a batch saw there is already a
// transcript fault, named by the report.
func (run *callmeterRun) sweepRequests(agentID, transcript string, settle bool) {
	p := run.payload
	if run.overBudget() {
		return
	}
	if info, err := os.Stat(transcript); errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return
	}
	read, err := run.transcriptRequests(transcript, settle)
	if err != nil {
		run.fault(callmeter.StageTranscript, "", fmt.Errorf("requests of %s: %w", transcript, err))
		return
	}
	requests := read.Requests
	if len(requests) == 0 && read.Marks.Empty() {
		return
	}
	// A session with no run recorded yet back-fills nothing older than now:
	// its first run's row lands with this one, and the next sweep reads again.
	since, ok, err := run.store.SessionFirstTS(run.ctx, p.SessionID)
	if err != nil {
		run.fault(callmeter.StageStore, "", err)
		return
	}
	if !ok {
		since = run.now
	}
	// The marks the store lacks, filtered before the write like the requests:
	// a store that cannot say what it holds loses the marks, not the requests.
	marks := read.Marks
	if !marks.Empty() {
		known, err := run.store.KnownMarks(run.ctx, p.SessionID)
		if err != nil {
			run.fault(callmeter.StageStore, "", err)
			marks = callmeter.TranscriptMarks{}
		} else {
			marks = marks.Since(since, known)
		}
	}
	if len(requests) == 0 && marks.Empty() {
		return
	}
	seatDir := ""
	if run.seat.dir != nil {
		seatDir = *run.seat.dir
	}
	run.write("", func(tx *callmeter.Tx) error {
		for _, read := range requests {
			request := callmeter.Request{
				RequestID: read.MessageID,
				SessionID: callmeter.Ptr(p.SessionID),
				PromptID:  presentString(read.PromptID),
				Pending:   callmeter.Ptr(false),
				Source:    callmeter.Ptr(callmeter.SourceHook),
				ConfigDir: run.seat.configDir,
				SeatDir:   run.seat.dir,
			}
			if agentID != "" {
				request.AgentID = callmeter.Ptr(agentID)
			}
			callmeter.ApplyUsage(&request, read.RequestUsage)
			if err := tx.SettleRequest(run.ctx, request, read.ToolUseIDs, since); err != nil {
				return err
			}
		}
		return tx.PutMarks(run.ctx, p.SessionID, agentID, seatDir, run.now, marks)
	})
}

// transcriptRequests reads the transcript's requests and marks; settle re-reads until
// its last assistant entry ends the turn of the hook's prompt_id, no user
// entry of that turn after it (callmeter.TranscriptRead's Final), or
// agentSettle has passed (a turn
// interrupted mid-call never writes one), on the wall clock as
// settledAgentTotals does. A transcript with no request is not waited on.
func (run *callmeterRun) transcriptRequests(transcript string, settle bool) (callmeter.TranscriptRead, error) {
	deadline := clock.Real.Now().Add(agentSettle)
	for {
		read, err := callmeter.ReadTranscript(transcript, run.payload.PromptID)
		if err != nil || !settle || read.Final || len(read.Requests) == 0 || !clock.Real.Now().Before(deadline) {
			return read, err
		}
		if err := clock.Real.Sleep(run.ctx, 25*time.Millisecond); err != nil {
			return read, fmt.Errorf("wait for the final message of %s: %w", transcript, err)
		}
	}
}

// awaitResults is SessionEnd's wait for the results of the calls found lacks:
// the transcript is re-statted every 25 ms from the size its first read saw
// (-1 when that stat failed, so any later size counts as grown), and when it
// grew the missing ids are read again, until every id has a result,
// unfinishedSettle has passed since began, or SessionEnd's budget is spent. A
// re-read that fails stays pending while the file still grows (the line may be
// mid-append), and is the error returned when it is the last word.
func (run *callmeterRun) awaitResults(
	transcript string, ids []string, found map[string]callmeter.Result, size int64, began time.Time,
) (map[string]callmeter.Result, error) {
	var readErr error
	for {
		var missing []string
		for _, id := range ids {
			if _, ok := found[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 || run.spent() || !clock.Real.Now().Before(began.Add(unfinishedSettle)) {
			return found, readErr
		}
		if err := clock.Real.Sleep(run.ctx, 25*time.Millisecond); err != nil {
			return found, fmt.Errorf("wait for the results of %s: %w", transcript, err)
		}
		info, err := os.Stat(transcript)
		if err != nil {
			return found, fmt.Errorf("stat transcript %s: %w", transcript, err)
		}
		if info.Size() <= size {
			continue
		}
		size = info.Size()
		more, err := callmeter.FindResults(transcript, missing)
		readErr = err
		maps.Copy(found, more)
	}
}

// resolveUnfinished settles, at the main chat's Stop and at SessionEnd, each
// call of the session with no real size — its PreToolUse alone landed, as when
// the user interrupts a sub-agent mid-call, or Claude Code refused it before
// any PostToolUse — from the tool_result its transcript holds: the delivered
// size, failed, the outcome label and the real size (a failure's text, else
// RealBytes of the result's toolUseResult, as PostToolUse stores it), each
// only filling (callmeter.SettledCall). A call with no result on disk (still running, or its
// session killed) stays unknown, as does one whose typed agent's transcript is
// missing: that absence is already a transcript fault at its batch or stop. At
// Stop a call with no result is left alone: a background call may still be
// running. At SessionEnd (run.deadline set) the transcript can be short, since
// Claude Code flushes a killed call's result after the hook starts: the read
// repeats while the file grows, within unfinishedSettle (awaitResults), and
// each call still without a result is a transcript fault naming it.
func (run *callmeterRun) resolveUnfinished() {
	p := run.payload
	unfinished, err := run.store.UnfinishedCalls(run.ctx, p.SessionID)
	if err != nil {
		run.fault(callmeter.StageStore, "", err)
		return
	}
	noTS := map[string]bool{}
	for _, call := range unfinished {
		noTS[call.ToolUseID] = call.NoTS
	}
	transcripts, idsOf := callmeter.CallTranscripts(p.TranscriptPath, unfinished)
	var settled []callmeter.Call
	for _, transcript := range transcripts {
		if run.overBudget() {
			continue
		}
		ids := idsOf[transcript]
		began := clock.Real.Now()
		size := int64(-1)
		if !run.deadline.IsZero() {
			if info, err := os.Stat(transcript); err == nil {
				size = info.Size()
			}
		}
		found, err := callmeter.FindResults(transcript, ids)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			run.fault(callmeter.StageTranscript, ids[0], err)
			continue
		}
		if !run.deadline.IsZero() {
			var waitErr error
			found, waitErr = run.awaitResults(transcript, ids, found, size, began)
			if waitErr != nil {
				run.fault(callmeter.StageTranscript, ids[0], waitErr)
			}
			for _, id := range ids {
				if _, ok := found[id]; !ok {
					run.fault(callmeter.StageTranscript, id, fmt.Errorf(
						"SessionEnd: no tool_result for the call in %s after %v: left in flight", transcript, unfinishedSettle))
				}
			}
		}
		for _, id := range ids {
			if result, ok := found[id]; ok {
				call := callmeter.SettledCall(id, result)
				// Only a call stored before every hook set a ts has none
				// here, so this ts never depends on whether the Stop landed
				// before or after the call's own hooks.
				if noTS[id] {
					call.TS = callmeter.Ptr(run.now)
				}
				settled = append(settled, call)
			}
		}
	}
	if len(settled) == 0 {
		return
	}
	run.write(settled[0].ToolUseID, func(tx *callmeter.Tx) error {
		for _, call := range settled {
			if err := tx.UpsertCall(run.ctx, call, callmeter.FillEmpty); err != nil {
				return err
			}
		}
		return nil
	})
}

// write runs one event's writes in one transaction, the run's own rows with
// them (runRows); a failure is a store fault. fn nil writes the run's rows only.
// While the run's event is not accounted for, the signal handler is held off
// from the moment the batch holds the store's write lock until it committed
// or, failed, until its fault row was written when the store is free at once
// (faultHeld); never across the wait for a busy store, the fault's included,
// so a signal during that wait is decided at once.
func (run *callmeterRun) write(toolUseID string, fn func(*callmeter.Tx) error) {
	run.batches++
	if run.gaveUp {
		return
	}
	var hold termHold
	held := false
	defer func() { hold.release() }()
	err := run.store.BatchHeld(run.ctx, func() { hold, held = run.state.hold(), true }, func(tx *callmeter.Tx) error {
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		return run.runRows(tx)
	})
	if err != nil {
		run.faultHeld(&hold, held, callmeter.StageStore, toolUseID, err)
		return
	}
	hold.account()
	run.accounted = true
	if afterEventCommit != nil {
		afterEventCommit()
	}
}

// fault says a failure to record: stderr and callmeter.log always, and a
// faults row when the store is open. While the run's event is not accounted
// for, a fault with no store or one the store refuses leaves the run's
// missed.log line instead (giveUp); after it, such a fault is logged only, and
// so is every fault of a run that gave up.
func (run *callmeterRun) fault(stage, toolUseID string, cause error) {
	if run.gaveUp {
		applog.Failure(run.stderr, run.logPath, stage, run.payload.SessionID, toolUseID, cause)
		return
	}
	var hold termHold
	defer func() { hold.release() }()
	run.faultHeld(&hold, false, stage, toolUseID, cause)
}

// faultHeld is fault with the caller's hold on the signal handler, released by
// the caller: held is true when the caller already took it (write, after its
// batch failed), otherwise faultHeld takes it into hold, for a fault row once
// that row's transaction holds the store's write lock (AddFaultHeld), never
// across the wait for a busy store.
func (run *callmeterRun) faultHeld(hold *termHold, held bool, stage, toolUseID string, cause error) {
	take := func() {
		if !held {
			*hold, held = run.state.hold(), true
		}
	}
	if callmeter.IsBusy(cause) && !run.accounted {
		take()
		run.giveUpBusy(*hold, stage, toolUseID, cause)
		return
	}
	session := run.payload.SessionID
	applog.Failure(run.stderr, run.logPath, stage, session, toolUseID, cause)
	if run.store == nil { // no store opened (OpenDBWaiting), or it closed
		take()
		run.giveUp(*hold, toolUseID, cause)
		return
	}
	fault := callmeter.Fault{TS: run.now, SessionID: session, ToolUseID: toolUseID, Stage: stage, Error: cause.Error()}
	if held && hold.locked {
		// A failed batch's hold: its fault row goes in under it when the store
		// is free at once, so a signal that waited on the batch is judged by
		// that row. A writer that took the store since the rollback is waited
		// out without the hold, a signal meanwhile decided at once.
		written, err := run.store.AddFaultIfFree(run.ctx, fault)
		if err != nil {
			applog.Failure(run.stderr, run.logPath, callmeter.StageStore, session, toolUseID, err)
		}
		switch {
		case written: // the row stands, whatever failed after its commit
			hold.account()
			run.accounted = true
			return
		case err == nil: // busy
			hold.release()
			*hold, held = termHold{}, false
		default:
			run.giveUp(*hold, toolUseID, err)
			return
		}
	}
	err := run.store.AddFaultHeld(run.ctx, fault, take)
	if err != nil {
		applog.Failure(run.stderr, run.logPath, callmeter.StageStore, session, toolUseID, err)
		take()
		run.giveUp(*hold, toolUseID, err)
		return
	}
	hold.account()
	run.accounted = true
}

// giveUp ends a run whose store could take neither its event nor the fault
// saying so, before that event was accounted for: one missed.log line
// `store unavailable: {class}` (callmeter.StoreFailureClass of cause, never
// its text; `terminated by store busy` when cause is a busy store) carries the
// event and session to the next run that reaches the store (IngestMissed), and
// the run is accounted for and writes nothing more. A run already accounted
// for leaves no line. cause is already said; hold is the caller's, as in
// faultHeld.
func (run *callmeterRun) giveUp(hold termHold, toolUseID string, cause error) {
	if run.accounted {
		return
	}
	reason := callmeter.StoreUnavailableReason + callmeter.StoreFailureClass(cause)
	if callmeter.IsBusy(cause) {
		reason = callmeter.TerminatedByStoreBusy
	}
	run.leaveMissedLine(hold, toolUseID, reason)
}

// leaveMissedLine appends the run's own missed.log line with reason, accounts
// the run for and stops its store work (gaveUp); a failed append is said.
func (run *callmeterRun) leaveMissedLine(hold termHold, toolUseID, reason string) {
	session := run.payload.SessionID
	err := AppendMissed(run.missed, run.payload.HookEventName, session, reason, clock.Real.Now())
	if err != nil {
		applog.Failure(run.stderr, run.logPath, callmeter.StageTerminated, session, toolUseID, err)
	}
	hold.account()
	run.accounted = true
	run.gaveUp = true
}

// giveUpBusy ends a run whose store stayed locked for its whole wait before
// its event was accounted for: the failure is said, one missed.log line
// `terminated by store busy` carries the event and session so report-time
// recovery still sees them (IngestMissed, endLost), and the run is accounted
// for and writes nothing more (gaveUp). hold is the caller's, as in faultHeld.
func (run *callmeterRun) giveUpBusy(hold termHold, stage, toolUseID string, cause error) {
	applog.Failure(run.stderr, run.logPath, stage, run.payload.SessionID, toolUseID, cause)
	run.leaveMissedLine(hold, toolUseID, callmeter.TerminatedByStoreBusy)
}

func presentString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func wholeNumber(value *float64) *int64 {
	if value == nil {
		return nil
	}
	return callmeter.Ptr(int64(*value))
}

// callmeterSeat keeps the seat a hook ran from and the user's own Claude Code
// directory distinct.
type callmeterSeat struct {
	dir       *string // CLAUDE_CONFIG_DIR, absolute and clean, symlinks unresolved; nil = not resolved
	configDir *string // $HOME/.claude; nil = not resolved
}

// resolveCallmeterSeat reads the process seat without resolving symlinks. A
// directory that cannot be resolved is nil in the result and named by the
// returned error, never guessed.
func resolveCallmeterSeat(getenv paths.Getenv) (callmeterSeat, error) {
	var seat callmeterSeat
	dir, dirErr := paths.SeatDir(getenv)
	if dirErr == nil {
		seat.dir = &dir
	}
	configDir, configErr := paths.ConfigDir(getenv)
	if configErr == nil {
		seat.configDir = &configDir
	}
	switch {
	case dirErr != nil && configErr != nil:
		return seat, fmt.Errorf("resolve seat dir: %w; resolve config dir: %w", dirErr, configErr)
	case dirErr != nil:
		return seat, fmt.Errorf("resolve seat dir: %w", dirErr)
	case configErr != nil:
		return seat, fmt.Errorf("resolve config dir: %w", configErr)
	}
	return seat, nil
}
