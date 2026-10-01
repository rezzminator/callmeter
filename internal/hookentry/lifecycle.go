package hookentry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/applog"
	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/paths"
)

// eventStop is the main chat's turn end.
const eventStop = "Stop"

// lifecycleEvents are the registered events that are neither a tool call nor
// a turn edge: each run writes one events row.
var lifecycleEvents = map[string]bool{
	callmeter.EventSessionStart: true,
	callmeter.EventSessionEnd:   true,
	"Setup":                     true,
	"UserPromptSubmit":          true,
	"UserPromptExpansion":       true,
	"InstructionsLoaded":        true,
	"PreCompact":                true,
	"PostCompact":               true,
	"StopFailure":               true,
	"PermissionRequest":         true,
	"PermissionDenied":          true,
	"Notification":              true,
	"TaskCreated":               true,
	"TaskCompleted":             true,
}

// detailCommon are the keys every payload carries or that have a column on
// every events row: never repeated in detail.
var detailCommon = []string{
	"session_id", "transcript_path", "cwd", "hook_event_name", "prompt_id",
	"agent_id", "agent_type", "permission_mode", "effort",
}

// detailNamed are, per event, the keys stored in a named column of the
// events or turns row: the rest of the payload goes to detail.
var detailNamed = map[string][]string{
	callmeter.EventSessionStart: {"source", "model"},
	callmeter.EventSessionEnd:   {"reason"},
	"Setup":                     {"trigger"},
	"PreCompact":                {"trigger"},
	"PostCompact":               {"trigger"},
	"StopFailure":               {"error"},
	"InstructionsLoaded":        {"load_reason", "memory_type", "file_path"},
	"PermissionRequest":         {"tool_name"},
	"UserPromptSubmit":          {"prompt"},
	"UserPromptExpansion":       {"command_name", "prompt"},
	"TaskCreated":               {"task_id"},
	"TaskCompleted":             {"task_id"},
	eventStop:                   {"background_tasks", "session_crons", "last_assistant_message", "stop_hook_active"},
	callmeter.EventSubagentStop: {"background_tasks", "session_crons", "last_assistant_message", "stop_hook_active"},
}

// taskNotification opens the prompt Claude Code submits when a background
// task reports back.
const taskNotification = "<task-notification>"

// eventOf is this run's events row: the common columns, the named ones its
// event carries, and every other payload key in sanitized detail.
func (run *callmeterRun) eventOf() *callmeter.Event {
	p := run.payload
	e := &callmeter.Event{
		EventID:        run.eventID,
		Event:          p.HookEventName,
		TS:             run.now,
		SessionID:      callmeter.Ptr(p.SessionID),
		AgentID:        presentString(p.AgentID),
		AgentType:      presentString(p.AgentType),
		PromptID:       presentString(p.PromptID),
		Effort:         run.effort,
		PermissionMode: presentString(p.PermissionMode),
		SeatDir:        run.seat.dir,
	}
	switch p.HookEventName {
	case callmeter.EventSessionStart:
		e.Source, e.Model = presentString(p.Source), presentString(p.Model)
	case callmeter.EventSessionEnd:
		e.Reason = presentString(p.Reason)
	case "Setup", "PreCompact", "PostCompact":
		e.Trigger = presentString(p.Trigger)
	case "StopFailure":
		if p.Error != nil {
			e.ErrorType = presentString(*p.Error)
		}
	case "InstructionsLoaded":
		e.LoadReason, e.MemoryType, e.FilePath = presentString(p.LoadReason), presentString(p.MemoryType), presentString(p.FilePath)
	case "PermissionRequest":
		e.ToolName = presentString(p.ToolName)
	case "UserPromptSubmit":
		e.PromptBytes = run.stringBytes("prompt", p.Prompt)
	case "UserPromptExpansion":
		e.CommandName, e.PromptBytes = presentString(p.CommandName), run.stringBytes("prompt", p.Prompt)
	case "TaskCreated", "TaskCompleted":
		e.TaskID = presentString(p.TaskID)
	}
	omit := map[string]bool{}
	for _, key := range append(append([]string{}, detailCommon...), detailNamed[p.HookEventName]...) {
		omit[key] = true
	}
	detail, err := callmeter.SanitizeDetail(run.raw, omit)
	if err != nil {
		run.fault(callmeter.StagePayload, p.ToolUseID, fmt.Errorf("%s detail: %w", p.HookEventName, err))
		return e
	}
	if p.HookEventName == "UserPromptSubmit" && run.promptIsTaskNotification() {
		detail = withTrue(detail, "task_notification")
	}
	e.Detail = &detail
	return e
}

// promptIsTaskNotification reports whether the payload's prompt is a
// background task reporting back, read without keeping the text.
func (run *callmeterRun) promptIsTaskNotification() bool {
	var prompt string
	if json.Unmarshal(run.payload.Prompt, &prompt) != nil {
		return false
	}
	return strings.HasPrefix(prompt, taskNotification)
}

// withTrue adds `"key": true` at the end of the JSON object detail.
func withTrue(detail, key string) string {
	encoded, _ := json.Marshal(key) // a Go string always encodes
	body := strings.TrimSuffix(detail, "}")
	if body != "{" {
		body += ","
	}
	return body + string(encoded) + ":true}"
}

// turnOf is this run's turns row: a Stop or SubagentStop. The message text
// is only measured; the task and cron lists are sanitized.
func (run *callmeterRun) turnOf() *callmeter.Turn {
	p := run.payload
	turn := &callmeter.Turn{
		EventID:                   run.eventID,
		Event:                     p.HookEventName,
		SessionID:                 callmeter.Ptr(p.SessionID),
		AgentID:                   presentString(p.AgentID),
		AgentType:                 presentString(p.AgentType),
		PromptID:                  presentString(p.PromptID),
		TS:                        run.now,
		Effort:                    run.effort,
		PermissionMode:            presentString(p.PermissionMode),
		LastAssistantMessageBytes: run.stringBytes("last_assistant_message", p.LastAssistantMessage),
		StopHookActive:            p.StopHookActive,
		SeatDir:                   run.seat.dir,
	}
	turn.BackgroundTasks = run.sanitizedList("background_tasks", p.BackgroundTasks)
	turn.SessionCrons = run.sanitizedList("session_crons", p.SessionCrons)
	return turn
}

// sanitizedList is a payload list as stored; absent or null is NULL, and one
// that does not sanitize is a payload fault and NULL.
func (run *callmeterRun) sanitizedList(name string, raw json.RawMessage) *string {
	if absent(raw) {
		return nil
	}
	list, err := callmeter.SanitizeDetail(raw, nil)
	if err != nil {
		run.fault(callmeter.StagePayload, "", fmt.Errorf("%s %s: %w", run.payload.HookEventName, name, err))
		return nil
	}
	return &list
}

// stringBytes is the UTF-8 byte length of a payload string; absent or null is
// NULL, and a value that is not a string is a payload fault and NULL.
func (run *callmeterRun) stringBytes(name string, raw json.RawMessage) *int64 {
	if absent(raw) {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		run.fault(callmeter.StagePayload, "", fmt.Errorf("%s %s is not a string: %w", run.payload.HookEventName, name, err))
		return nil
	}
	return callmeter.Ptr(int64(len(s)))
}

func absent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// effortOf is the effort a payload ran under: its effort.level, else the
// hook process's CLAUDE_EFFORT, else nil.
func effortOf(raw json.RawMessage, getenv paths.Getenv) *string {
	var effort struct {
		Level string `json:"level"`
	}
	if !absent(raw) && json.Unmarshal(raw, &effort) == nil && effort.Level != "" {
		return &effort.Level
	}
	return presentString(getenv("CLAUDE_EFFORT"))
}

// runRows writes the rows this run owns beside its event's own: the events
// and turns rows, the agent's turns rebuilt from its events, and the session.
// Every write is idempotent, so each of the run's transactions carries them
// and the first to commit lands them.
func (run *callmeterRun) runRows(tx *callmeter.Tx) error {
	if run.event != nil {
		if _, err := tx.InsertEvent(run.ctx, *run.event); err != nil {
			return err
		}
	}
	if run.turn != nil {
		if _, err := tx.InsertTurn(run.ctx, *run.turn); err != nil {
			return err
		}
	}
	if run.agentTurns {
		if err := tx.RebuildAgentTurns(run.ctx, run.payload.AgentID); err != nil {
			return err
		}
	}
	if err := tx.TouchSession(run.ctx, run.sessionOf()); err != nil {
		return err
	}
	if run.refresh {
		return tx.RefreshSession(run.ctx, run.payload.SessionID)
	}
	return nil
}

// sessionOf is what this run says about its session.
func (run *callmeterRun) sessionOf() callmeter.Session {
	p := run.payload
	s := callmeter.Session{
		SessionID:      p.SessionID,
		TS:             run.now,
		Cwd:            presentString(p.Cwd),
		TranscriptPath: presentString(p.TranscriptPath),
		SeatDir:        run.seat.dir,
		ConfigDir:      run.seat.configDir,
	}
	host, err := os.Hostname()
	if err != nil {
		// The session row lands without its host; the gap is said, not hidden.
		applog.Failure(run.stderr, run.logPath, "host", p.SessionID, "", fmt.Errorf("read the host name: %w", err))
	} else {
		s.Host = presentString(host)
	}
	zone := hostZone(run.getenv, time.UnixMilli(run.now))
	s.TZName, s.TZOffsetMinutes = zone.name, &zone.offsetMinutes
	return s
}

// localtimePath is the host's zone link; a variable so a test can point it
// at a copy.
var localtimePath = "/etc/localtime"

// zone is the host's time zone: its IANA name when one is known, and its
// offset from UTC at a given instant.
type zone struct {
	name          *string
	offsetMinutes int64
}

// hostZone names the host's zone: TZ when it loads as a location, else the
// /etc/localtime link's target after zoneinfo/, else nil — a localtime that
// is a copy, not a link, names no zone, so the name stays NULL rather than a
// guess. The offset is that zone's (the process's local zone without a name)
// at now.
func hostZone(getenv paths.Getenv, now time.Time) zone {
	location := time.Local
	var name *string
	if tz := strings.TrimPrefix(getenv("TZ"), ":"); tz != "" && tz != "Local" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			location, name = loaded, &tz
		}
	}
	if name == nil {
		if target, err := os.Readlink(localtimePath); err == nil {
			if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
				linked := target[i+len("zoneinfo/"):]
				if loaded, err := time.LoadLocation(linked); err == nil {
					location, name = loaded, &linked
				}
			}
		}
	}
	_, offset := now.In(location).Zone()
	return zone{name: name, offsetMinutes: int64(offset / 60)}
}
