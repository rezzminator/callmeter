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
	"os"
	"os/signal"
	"path/filepath"
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

// calls names the tool calls this payload is about, for a failure that loses
// its whole record: the tool_use_id, or a batch's ids joined by commas.
func (p callmeterPayload) calls() string {
	if p.ToolUseID != "" || len(p.ToolCalls) == 0 {
		return p.ToolUseID
	}
	ids := make([]string, 0, len(p.ToolCalls))
	for _, call := range p.ToolCalls {
		ids = append(ids, call.ToolUseID)
	}
	return strings.Join(ids, ",")
}

// callmeterResponse is the part of a PostToolUse tool_response callmeter
// reads for itself: the output sizes and an Agent call's sub-agent totals. The
// file columns are callmeter.FileColumnsFromResult's.
type callmeterResponse struct {
	Stdout              *string         `json:"stdout"`
	PersistedOutputPath *string         `json:"persistedOutputPath"`
	PersistedOutputSize *float64        `json:"persistedOutputSize"`
	Content             json.RawMessage `json:"content"`
	File                *struct {
		Content *string `json:"content"`
	} `json:"file"`
	AgentID           string   `json:"agentId"`
	AgentType         string   `json:"agentType"`
	TotalTokens       *float64 `json:"totalTokens"`
	TotalToolUseCount *float64 `json:"totalToolUseCount"`
	ResolvedModel     *string  `json:"resolvedModel"`
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
	now     int64
	raw     []byte // the payload as it arrived: detail's source
	eventID string // lowercase hex SHA-256 of raw
	payload callmeterPayload
	seat    callmeterSeat
	effort  *string // the effort rule of the store's schema (effortOf)

	// The rows every write of this run carries beside its own (runRows).
	event      *callmeter.Event
	turn       *callmeter.Turn
	agentTurns bool // rebuild the agent_turns of payload.AgentID
	refresh    bool // recompute the session's derived columns
	batches    int  // writes tried: a run that tried none writes its rows alone
}

// Callmeter is the hook entry: it records each hook event into the callmeter
// store under $CALLMETER_HOME. It exits 0 on every path and writes nothing to
// stdout — it changes nothing the model sees — and a failure to record is said
// on stderr, in callmeter.log with the session and tool_use_id, and as a faults
// row whenever the store itself is reachable. A SIGTERM, SIGINT or SIGHUP (a
// headless `claude -p` terminates its running async hooks at exit) that lands
// before the run's event is accounted for leaves one missed.log line instead
// (terminateOnSignal); one of them ignored on entry stays ignored.
func Callmeter(input io.Reader, stderr io.Writer, getenv paths.Getenv) int {
	home, err := paths.Home(getenv)
	if err != nil {
		// No home, so no log file either: stderr only.
		applog.Failure(stderr, "", callmeter.StageStore, "", "", fmt.Errorf("resolve callmeter home: %w", err))
		return 0
	}
	logPath := paths.Log(home)
	state := &terminationState{}
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
// that handles its termination signal: the event name once the payload
// decoded, and whether the run's event is accounted for, meaning its first
// write batch committed or its first fault row was written. mu is held, from
// the start of a write that can account for the run until that write's
// outcome is known, so a signal landing mid-commit waits for the outcome.
type terminationState struct {
	mu        sync.Mutex
	event     string
	accounted bool
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

// setEvent records the decoded hook_event_name for the handler's missed line.
func (state *terminationState) setEvent(name string) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.event = name
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
// the state's lock first, so a write in flight finishes and is judged by its
// outcome; the lock is not released before exit, so the run writes nothing
// after the decision. A failure to append the line is said on stderr and in
// the log, never a non-zero exit. done ends the wait without a signal.
func terminateOnSignal(
	signals <-chan os.Signal,
	done <-chan struct{},
	state *terminationState,
	missed, logPath string,
	stderr io.Writer,
	exit func(int),
) {
	var sig os.Signal
	select {
	case sig = <-signals:
	case <-done:
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.accounted {
		if err := appendTerminated(missed, state.event, sig, clock.Real.Now()); err != nil {
			applog.Failure(stderr, logPath, callmeter.StageTerminated, "", "", err)
		}
	}
	exit(0)
}

// appendTerminated appends `{unix seconds}\t{event}\tterminated by {SIGNAL}` to
// the missed.log at path, creating it and its directory when absent: one
// write, so concurrent appenders never interleave a line.
func appendTerminated(path, event string, sig os.Signal, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the directory of %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	line := fmt.Sprintf("%d\t%s\t%s%s\n", now.Unix(), missedEvent(event), callmeter.TerminatedReason, signalName(sig))
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
		state: terminationStateOf(ctx),
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
		run.state.setEvent(run.payload.HookEventName)
	}
	store, err := callmeter.OpenDB(ctx, files.store)
	if err != nil {
		run.fault(callmeter.StageStore, run.payload.calls(), errors.Join(payloadErr, err))
		return 0
	}
	run.store = store
	defer func() {
		if err := store.Close(); err != nil {
			run.store = nil // closed: the fault is said, never written
			run.fault(callmeter.StageStore, run.payload.calls(), err)
		}
	}()
	// The wrapper's own failures (no binary, a bad checksum) land as binary
	// faults on the next run that reaches the store; failing to ingest them
	// is one store fault and never stops this run's record.
	if _, err := store.IngestMissed(ctx, files.missed); err != nil {
		run.fault(callmeter.StageStore, "", fmt.Errorf("ingest %s: %w", files.missed, err))
	}
	if payloadErr != nil {
		run.fault(callmeter.StagePayload, run.payload.ToolUseID, payloadErr)
		return 0
	}
	run.effort = effortOf(run.payload.Effort, getenv)
	run.record()
	return 0
}

func (run *callmeterRun) record() {
	p := run.payload
	if p.SessionID == "" {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("%q payload carries no session_id", p.HookEventName))
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
		run.resolvePending("", p.TranscriptPath)
	default:
		if !lifecycleEvents[p.HookEventName] {
			run.fault(
				callmeter.StagePayload,
				p.ToolUseID,
				fmt.Errorf("event %q is not one callmeter records", p.HookEventName),
			)
			return
		}
		run.event = run.eventOf()
		run.refresh = p.HookEventName == callmeter.EventSessionStart || p.HookEventName == callmeter.EventSessionEnd
	}
	if run.batches == 0 {
		run.write(p.ToolUseID, nil) // this run's own rows: events, turns, the session
	}
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
	call.TS = callmeter.Ptr(run.now)
	call.Tool = callmeter.Ptr(p.ToolName)
	call.DurationMS = wholeNumber(p.DurationMS)
	call.Failed = callmeter.Ptr(failed)
	call.IsInterrupt = p.IsInterrupt
	run.inputColumns(&call)
	run.fileColumns(&call)
	var agent *callmeter.Agent
	if failed {
		call.Error = p.Error
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
		after := callmeter.Call{ToolUseID: p.ToolUseID, Cwd: presentString(p.Cwd)}
		if err := tx.UpsertCall(run.ctx, after, callmeter.FillEmpty); err != nil {
			return err
		}
		if agent == nil {
			return nil
		}
		// The totals only fill: the agent's own transcript, summed at its
		// stop, holds every turn (recordAgent).
		totals := callmeter.Agent{AgentID: agent.AgentID, TotalTokens: agent.TotalTokens, ToolUses: agent.ToolUses}
		agent.TotalTokens, agent.ToolUses = nil, nil
		if err := tx.UpsertAgent(run.ctx, *agent, callmeter.Overwrite); err != nil {
			return err
		}
		return tx.UpsertAgent(run.ctx, totals, callmeter.FillEmpty)
	})
}

// recordStartCwd stores the directory a Bash command starts in. PostToolUse's
// cwd follows the command's own `cd`, and the parser replays that `cd` from the
// stored cwd, so only PreToolUse's is right; it overwrites whatever PostToolUse
// filled, and PostToolUse never overwrites it, in either landing order. ts is
// set so a call that never finishes still ages out of the store. The command's
// input and test runner only fill, so a call whose PostToolUse is lost still
// has them, and a PostToolUse landing in either order overwrites them.
func (run *callmeterRun) recordStartCwd() {
	p := run.payload
	if p.ToolUseID == "" || p.Cwd == "" {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("PreToolUse payload lacks tool_use_id or cwd"))
		return
	}
	call := run.base(p.ToolUseID)
	call.Cwd = callmeter.Ptr(p.Cwd)
	// The start only fills: PostToolUse's prompt, effort and mode win.
	start := callmeter.Call{ToolUseID: p.ToolUseID, TS: callmeter.Ptr(run.now), Tool: presentString(p.ToolName)}
	run.tier1(&start)
	run.inputColumns(&start)
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
func (run *callmeterRun) inputColumns(call *callmeter.Call) {
	p := run.payload
	if len(p.ToolInput) == 0 {
		return
	}
	input, err := callmeter.SanitizeInput(p.ToolName, p.ToolInput)
	if err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
		return
	}
	call.Input = &input
	run.testRunner(call)
}

// testRunner sets the test runner a Bash call's command starts, on success and
// failure alike (callmeter.TestRunnerOf). Only a call whose input decoded
// reaches it: SanitizeInput has already faulted one that did not.
func (run *callmeterRun) testRunner(call *callmeter.Call) {
	p := run.payload
	if p.ToolName != "Bash" {
		return
	}
	command, err := callmeter.BashCommand(p.ToolInput)
	if err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
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
func (run *callmeterRun) fileColumns(call *callmeter.Call) {
	p := run.payload
	if err := callmeter.FileColumnsFromInput(call, p.ToolName, p.ToolInput, p.Cwd); err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, err)
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
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("stat %s: %w", path, err))
	}
}

// responseColumns reads the tool's own result: the real output size, the
// persisted path, the file columns, and — for an Agent or
// Task call — the sub-agent it spawned.
func (run *callmeterRun) responseColumns(call *callmeter.Call) *callmeter.Agent {
	p := run.payload
	trimmed := bytes.TrimSpace(p.ToolResponse)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		size, err := callmeter.DeliveredBytes(trimmed)
		if err != nil {
			run.fault(callmeter.StagePayload, p.ToolUseID, err)
			return nil
		}
		call.BytesReal = callmeter.Ptr(size)
		return nil
	}
	var response callmeterResponse
	if err := json.Unmarshal(trimmed, &response); err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("decode %s tool_response: %w", p.ToolName, err))
		return nil
	}
	switch {
	case response.PersistedOutputSize != nil:
		call.BytesReal = wholeNumber(response.PersistedOutputSize)
	case response.Stdout != nil:
		call.BytesReal = callmeter.Ptr(int64(len(*response.Stdout)))
	case response.File != nil && response.File.Content != nil:
		call.BytesReal = callmeter.Ptr(int64(len(*response.File.Content)))
	case len(response.Content) > 0 && !bytes.Equal(response.Content, []byte("null")):
		size, err := callmeter.DeliveredBytes(response.Content)
		if err != nil {
			run.fault(callmeter.StagePayload, p.ToolUseID, err)
		} else {
			call.BytesReal = callmeter.Ptr(size)
		}
	default:
		call.BytesReal = callmeter.Ptr(int64(len(trimmed)))
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
		TotalTokens:     wholeNumber(response.TotalTokens),
		ToolUses:        wholeNumber(response.TotalToolUseCount),
		Model:           response.ResolvedModel,
		Source:          callmeter.Ptr(callmeter.SourceHook),
		ConfigDir:       run.seat.configDir,
		SeatDir:         run.seat.dir,
	}
}

// applyUsage copies a transcript request's usage into a requests row: its
// time, model, stop reason and the token split. A cache-creation split the
// usage did not carry stays NULL.
func applyUsage(request *callmeter.Request, usage callmeter.RequestUsage) {
	request.TS = callmeter.Ptr(usage.TS)
	request.Model = presentString(usage.Model)
	request.StopReason = presentString(usage.StopReason)
	request.InputTokens = callmeter.Ptr(usage.InputTokens)
	request.CacheReadTokens = callmeter.Ptr(usage.CacheReadTokens)
	request.CacheCreationTokens = callmeter.Ptr(usage.CacheCreationTokens)
	request.CacheCreation5mTokens = usage.CacheCreation5m
	request.CacheCreation1hTokens = usage.CacheCreation1h
	request.ContextTokens = callmeter.Ptr(usage.ContextTokens)
	request.OutputTokens = callmeter.Ptr(usage.OutputTokens)
}

// recordBatch writes each call's delivered bytes and the model request that
// grouped them: the transcript's message when it is on disk, a provisional
// pending row when it is not yet, and a transcript fault besides when the
// transcript could not be read.
func (run *callmeterRun) recordBatch() {
	p := run.payload
	if len(p.ToolCalls) == 0 {
		run.fault(callmeter.StagePayload, "", errors.New("PostToolBatch payload carries no tool_calls"))
		return
	}
	transcript := p.TranscriptPath
	if p.AgentID != "" {
		transcript = callmeter.SubagentTranscriptPath(p.TranscriptPath, p.AgentID)
	}
	// Claude Code's own internal agents (agent_id set, agent_type empty) have
	// no SubagentStart and no transcript on disk: nothing to meter, so no
	// call, no request and no transcript fault (recordAgent's exemption,
	// shared by untypedAgentMissingTranscript).
	if untypedAgentMissingTranscript(p.AgentID, p.AgentType, transcript) {
		return
	}
	ids := make([]string, 0, len(p.ToolCalls))
	calls := make([]callmeter.Call, 0, len(p.ToolCalls))
	// What the batch knows of a call beside its delivery only fills: a call
	// the harness refused before any PreToolUse (a blocked command, a tool the
	// session lacks) has its batch entry alone, and PostToolUse's columns win
	// in either landing order.
	fills := make([]callmeter.Call, 0, len(p.ToolCalls))
	for _, toolCall := range p.ToolCalls {
		if toolCall.ToolUseID == "" {
			run.fault(callmeter.StagePayload, "", errors.New("PostToolBatch call carries no tool_use_id"))
			return
		}
		call := run.base(toolCall.ToolUseID)
		size, err := callmeter.DeliveredBytes(toolCall.ToolResponse)
		if err != nil {
			run.fault(callmeter.StagePayload, toolCall.ToolUseID, err)
		} else {
			call.BytesDelivered = callmeter.Ptr(size)
		}
		ids = append(ids, toolCall.ToolUseID)
		calls = append(calls, call)
		fill := callmeter.Call{ToolUseID: toolCall.ToolUseID, Tool: presentString(toolCall.ToolName)}
		run.tier1(&fill)
		if refusedByHarness(toolCall.ToolResponse) {
			fill.Failed = callmeter.Ptr(true)
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
				applyUsage(request, usage)
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

// toolUseError opens the batch result of a call the harness refused to run.
const toolUseError = "<tool_use_error>"

// refusedByHarness reports whether a batch entry's tool_response is the
// harness's refusal: a string opening with toolUseError.
func refusedByHarness(response json.RawMessage) bool {
	var text string
	return json.Unmarshal(response, &text) == nil && strings.HasPrefix(text, toolUseError)
}

// untypedAgentMissingTranscript reports whether p names one of Claude Code's
// own internal agents — agent_id set, agent_type empty — whose transcript was
// never written: no SubagentStart, nothing to meter. Shared by recordAgent
// and recordBatch so the exemption is checked once.
func untypedAgentMissingTranscript(agentID, agentType, transcript string) bool {
	if agentID == "" || agentType != "" {
		return false
	}
	_, err := os.Stat(transcript)
	return errors.Is(err, fs.ErrNotExist)
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
	if !stopped {
		// An agent woken for another turn starts again: started keeps the
		// earliest start, whatever order the starts land in, and the prompt
		// follows that start.
		start := callmeter.Agent{AgentID: p.AgentID, Started: callmeter.Ptr(run.now)}
		prompt := callmeter.Agent{AgentID: p.AgentID, PromptID: presentString(p.PromptID)}
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
	if untypedAgentMissingTranscript(p.AgentID, p.AgentType, transcript) {
		return
	}
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
		if totals == nil || !latest {
			return nil // no totals read, or an earlier stop than the one stored
		}
		if err := tx.UpsertAgent(run.ctx, *totals, callmeter.Overwrite); err != nil {
			return err
		}
		return tx.UpsertAgent(run.ctx, *model, callmeter.FillEmpty)
	})
	run.resolvePending(p.AgentID, transcript)
}

// agentSettle bounds how long SubagentStop waits for the agent's final
// message: Claude Code fires the hook 20-50 ms after stamping that message and
// before flushing its line, so an immediate read sums every message but the
// last. The hook is async, so the wait never holds the model.
var agentSettle = 3 * time.Second

// settledAgentTotals reads the agent transcript until its last assistant
// entry ends the turn (AgentTotals.Final), or agentSettle has passed — a
// killed agent never writes one — and returns the last read. It waits on
// another process's write, so it runs on the wall clock, never a test's fake.
func (run *callmeterRun) settledAgentTotals(transcript string) (callmeter.AgentTotals, error) {
	deadline := clock.Real.Now().Add(agentSettle)
	for {
		sum, err := callmeter.ReadAgentTotals(transcript)
		if err != nil || sum.Final || !clock.Real.Now().Before(deadline) {
			return sum, err
		}
		if err := clock.Real.Sleep(run.ctx, 25*time.Millisecond); err != nil {
			return sum, fmt.Errorf("wait for the final message of %s: %w", transcript, err)
		}
	}
}

// resolvePending fills the pending requests of one chat or sub-agent from its
// transcript. A request still absent stays pending — the next Stop tries again.
func (run *callmeterRun) resolvePending(agentID, transcript string) {
	p := run.payload
	pending, err := run.store.PendingRequests(run.ctx, p.SessionID, agentID)
	if err != nil {
		run.fault(callmeter.StageStore, "", err)
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
		for _, request := range pending {
			for _, id := range request.CallIDs {
				usage, ok := found[id]
				if !ok {
					continue
				}
				resolved := callmeter.Request{RequestID: usage.MessageID, Source: callmeter.Ptr(callmeter.SourceHook)}
				applyUsage(&resolved, usage)
				if err := tx.ResolveRequest(run.ctx, request.RequestID, resolved); err != nil {
					return err
				}
				break
			}
		}
		return nil
	})
}

// write runs one event's writes in one transaction, the run's own rows with
// them (runRows); a failure is a store fault. fn nil writes the run's rows only.
// While the run's event is not accounted for, the signal handler is held off
// until the batch committed or, failed, until its fault row was written.
func (run *callmeterRun) write(toolUseID string, fn func(*callmeter.Tx) error) {
	run.batches++
	hold := run.state.hold()
	defer hold.release()
	err := run.store.Batch(run.ctx, func(tx *callmeter.Tx) error {
		if fn != nil {
			if err := fn(tx); err != nil {
				return err
			}
		}
		return run.runRows(tx)
	})
	if err != nil {
		run.faultHeld(hold, callmeter.StageStore, toolUseID, err)
		return
	}
	hold.account()
}

// fault says a failure to record: stderr and callmeter.log always, and a
// faults row when the store is open. A fault the store refuses is logged only.
func (run *callmeterRun) fault(stage, toolUseID string, cause error) {
	hold := run.state.hold()
	defer hold.release()
	run.faultHeld(hold, stage, toolUseID, cause)
}

// faultHeld is fault for a caller that already holds the signal handler off
// (write, after its batch failed): hold is that caller's, released by it.
func (run *callmeterRun) faultHeld(hold termHold, stage, toolUseID string, cause error) {
	session := run.payload.SessionID
	applog.Failure(run.stderr, run.logPath, stage, session, toolUseID, cause)
	if run.store == nil {
		return
	}
	err := run.store.AddFault(run.ctx, callmeter.Fault{
		TS: run.now, SessionID: session, ToolUseID: toolUseID, Stage: stage, Error: cause.Error(),
	})
	if err != nil {
		applog.Failure(run.stderr, run.logPath, callmeter.StageStore, session, toolUseID, err)
		return
	}
	hold.account()
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
