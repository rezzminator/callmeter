package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// tokens is one usage split.
type tokens struct{ input, output, cacheRead, cacheCreation int64 }

func (t *tokens) addTo(o tokens) {
	t.input += o.input
	t.output += o.output
	t.cacheRead += o.cacheRead
	t.cacheCreation += o.cacheCreation
}

func (t tokens) String() string {
	return fmt.Sprintf("input=%d output=%d cache_read=%d cache_creation=%d", t.input, t.output, t.cacheRead, t.cacheCreation)
}

// use is one tool_use block of a transcript, inside the window.
type use struct {
	id, name, session, agent, msgID string
	ts                              int64
}

// result is one tool_result block, any time.
type result struct {
	ts      int64
	isError bool
}

// message is one model message, deduplicated by its id: Claude Code writes one
// transcript line per content block, each carrying the same usage.
type message struct {
	id, session, agent, model, stopReason string
	promptID                              string // the latest user line's promptId when the message began
	ts                                    int64
	endTS                                 int64 // the line that set a turn-ending stop_reason (set, not tool_use); 0: none
	usage                                 tokens
	toolIDs                               map[string]bool
}

// agentMeta is the part of a sub-agent's .meta.json this check reads.
type agentMeta struct {
	AgentType string `json:"agentType"`
	ToolUseID string `json:"toolUseId"`
}

// transcript is one file read: a session's main transcript, or one sub-agent's.
// Its counts cover the window from its session's first store row (or --since,
// whichever is later) to --until; the pre counts cover --since up to that row.
type transcript struct {
	path, session, agent string // agent empty for the main transcript
	meta                 agentMeta
	windowLines          int
	prompts              map[string]bool // promptIds of model-bound prompt lines
	queued               int             // queued_command attachments
	compacts             int
	turnDurations        int
	apiErrors            int
	assistants           int // non-synthetic assistant lines
	toolUses             map[string]bool
	endTurns             map[string]bool // message ids that ended with end_turn
	nullEnds             map[string]bool // message ids whose null stop_reason a later prompt closed
	entrypoints          map[string]bool
	lastStop             string // the latest message's stop_reason, "" while it has none
	lastMsg              string // the latest message's id
	lastAssistant        int64  // the latest assistant line (unix ms)
	lastTS               int64
	prompt               string // the latest user line's promptId, whatever the window
	endHook              bool   // any line is a SessionEnd hook attachment, in or out of the window
	preLines             int
	preAssistants        int
	preToolUses          map[string]bool
}

// world is everything the transcripts say, inside the window.
type world struct {
	since, until int64
	notices      map[string][]int64 // agent id -> ts of each task-notification naming it, main transcripts only
	taskStops    map[string][]int64 // agent id -> each TaskStop naming it (unix ms)
	writes       map[string]int64   // session -> its newest transcript file's mtime (unix ms)
	uses         map[string]*use
	results      map[string]result
	messages     map[string]*message
	mains        map[string]*transcript   // by session
	agents       map[string]*transcript   // by agent id
	subagents    map[string][]*transcript // by session
	read         map[string]bool          // resolved paths read
	from         map[string]int64         // by session: its first store row
}

func newWorld(since, until int64) *world {
	return &world{
		since: since, until: until,
		uses: map[string]*use{}, results: map[string]result{}, messages: map[string]*message{},
		mains: map[string]*transcript{}, agents: map[string]*transcript{}, subagents: map[string][]*transcript{},
		read: map[string]bool{}, from: map[string]int64{},
		notices: map[string][]int64{}, taskStops: map[string][]int64{}, writes: map[string]int64{},
	}
}

func (w *world) inWindow(ts int64) bool { return ts >= w.since && ts <= w.until }

// lower is where a session's comparison starts: its first store row, when
// that is later than --since. A session already running when the plugin was
// enabled has transcript lines before any hook recorded it. The grace keeps the
// line a first hook reports on (a prompt written just before its
// UserPromptSubmit) inside the window.
func (w *world) lower(session string) int64 {
	if w.from[session] == 0 {
		return w.since
	}
	return max(w.since, w.from[session]-firstRowGrace)
}

// firstRowGrace is how far before a session's first store row its transcript
// is still compared.
const firstRowGrace = 10_000

// diskIndex maps a session id to its main transcript, over every projects dir,
// symlinks resolved so a dir shared by several seats is listed once.
func diskIndex(projects []string) (map[string]string, error) {
	index := map[string]string{}
	seen := map[string]bool{}
	for _, p := range projects {
		real, err := filepath.EvalSymlinks(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve projects dir %s: %w", p, err)
		}
		if seen[real] {
			continue
		}
		seen[real] = true
		files, err := filepath.Glob(filepath.Join(real, "*", "*.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("list transcripts under %s: %w", real, err)
		}
		for _, f := range files {
			id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
			if _, ok := index[id]; !ok {
				index[id] = f
			}
		}
	}
	return index, nil
}

// readSession reads a session's main transcript and every sub-agent transcript
// beside it, each resolved file once.
func (w *world) readSession(session, path string) error {
	main, err := w.readFile(path, session, "")
	if err != nil {
		return err
	}
	if main == nil {
		return nil
	}
	w.mains[session] = main
	dir := filepath.Join(filepath.Dir(path), session, "subagents")
	files, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return fmt.Errorf("list sub-agent transcripts in %s: %w", dir, err)
	}
	sort.Strings(files)
	for _, f := range files {
		agent := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "agent-"), ".jsonl")
		t, err := w.readFile(f, session, agent)
		if err != nil {
			return err
		}
		if t == nil {
			continue
		}
		metaPath := strings.TrimSuffix(f, ".jsonl") + ".meta.json"
		if raw, err := os.ReadFile(metaPath); err == nil {
			if err := json.Unmarshal(raw, &t.meta); err != nil {
				return fmt.Errorf("parse sub-agent meta %s: %w", metaPath, err)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read sub-agent meta %s: %w", metaPath, err)
		}
		w.agents[agent] = t
		w.subagents[session] = append(w.subagents[session], t)
	}
	return nil
}

type line struct {
	Type          string `json:"type"`
	Subtype       string `json:"subtype"`
	Timestamp     string `json:"timestamp"`
	PromptID      string `json:"promptId"`
	IsMeta        bool   `json:"isMeta"`
	TurnOrigin    string `json:"turnOrigin"`
	IsCompactSumm bool   `json:"isCompactSummary"`
	IsAPIError    bool   `json:"isApiErrorMessage"`
	Entrypoint    string `json:"entrypoint"`
	Origin        *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Attachment *struct {
		Type      string `json:"type"`
		HookEvent string `json:"hookEvent"`
		Origin    *struct {
			Kind string `json:"kind"`
		} `json:"origin"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
	Message *struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason *string         `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Input     json.RawMessage `json:"input"`
}

// readFile parses one transcript; nil when this resolved path was read before.
func (w *world) readFile(path, session, agent string) (*transcript, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve transcript %s: %w", path, err)
	}
	if w.read[real] {
		return nil, nil
	}
	w.read[real] = true
	f, err := os.Open(real)
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", real, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat transcript %s: %w", real, err)
	}
	w.writes[session] = max(w.writes[session], info.ModTime().UnixMilli())
	t := &transcript{
		path: real, session: session, agent: agent,
		prompts: map[string]bool{}, toolUses: map[string]bool{}, endTurns: map[string]bool{}, nullEnds: map[string]bool{},
		entrypoints: map[string]bool{}, preToolUses: map[string]bool{},
	}
	lower := w.lower(session)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<28)
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("parse transcript %s line %d: %w", real, n, err)
		}
		// Claude Code records a hook it ran as an attachment line (hook_success,
		// hook_non_blocking_error, ...), each naming its hookEvent: any such line
		// for SessionEnd is evidence the hooks ran.
		if l.Type == "attachment" && l.Attachment != nil && l.Attachment.HookEvent == "SessionEnd" {
			t.endHook = true
		}
		if l.Type == "user" && l.PromptID != "" {
			t.prompt = l.PromptID
		}
		if l.Timestamp == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("parse timestamp in %s line %d: %w", real, n, err)
		}
		ms := ts.UnixMilli()
		var blocks []block
		if l.Message != nil && len(l.Message.Content) > 0 && l.Message.Content[0] == '[' {
			if err := json.Unmarshal(l.Message.Content, &blocks); err != nil {
				return nil, fmt.Errorf("parse content blocks in %s line %d: %w", real, n, err)
			}
		}
		// A tool_result counts whenever it landed: the check on it skips one past the window's end.
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				w.results[b.ToolUseID] = result{ts: ms, isError: b.IsError}
			}
		}
		if !w.inWindow(ms) {
			continue
		}
		if ms < lower {
			t.preLines++
			if l.Type == "assistant" {
				t.preAssistants++
				for _, b := range blocks {
					if b.Type == "tool_use" {
						t.preToolUses[b.ID] = true
					}
				}
			}
			continue
		}
		t.windowLines++
		if l.Entrypoint != "" {
			t.entrypoints[l.Entrypoint] = true
		}
		if ms > t.lastTS {
			t.lastTS = ms
		}
		switch l.Type {
		case "assistant":
			if err := w.assistant(t, &l, blocks, ms); err != nil {
				return nil, fmt.Errorf("transcript %s line %d: %w", real, n, err)
			}
		case "user":
			if t.agent == "" {
				w.notice(&l, ms)
			}
			if (l.IsMeta && !peerMessage(&l) && !scheduledPrompt(&l)) || l.IsCompactSumm || l.Message == nil {
				continue
			}
			if l.PromptID != "" && isPrompt(l.Message.Content, blocks) {
				t.prompts[l.PromptID] = true
				// The hook's SubagentStop ends the turn whatever the stop_reason.
				if t.lastMsg != "" && t.lastStop == "" {
					t.nullEnds[t.lastMsg] = true
				}
			}
		case "attachment":
			if t.agent == "" {
				w.notice(&l, ms)
			}
			if l.Attachment != nil && l.Attachment.Type == "queued_command" {
				t.queued++
			}
		case "system":
			switch l.Subtype {
			case "compact_boundary":
				t.compacts++
			case "turn_duration":
				t.turnDurations++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read transcript %s: %w", real, err)
	}
	return t, nil
}

// notice collects only the agent id and timestamp of a main-transcript notice.
func (w *world) notice(l *line, ms int64) {
	var body string
	switch l.Type {
	case "user":
		if l.Origin == nil || l.Origin.Kind != "task-notification" || l.Message == nil {
			return
		}
		if err := json.Unmarshal(l.Message.Content, &body); err != nil {
			return
		}
	case "attachment":
		a := l.Attachment
		if a == nil || a.Type != "queued_command" || a.Origin == nil || a.Origin.Kind != "task-notification" {
			return
		}
		body = a.Prompt
	}
	if agent, ok := noticeAgent(body); ok {
		w.notices[agent] = append(w.notices[agent], ms)
	}
}

// noticeAgent reads only the task id and the presence of a non-empty status.
func noticeAgent(body string) (agentID string, ok bool) {
	_, task, found := strings.Cut(body, "<task-id>")
	if !found {
		return "", false
	}
	agentID, _, found = strings.Cut(task, "</task-id>")
	if !found || agentID == "" {
		return "", false
	}
	_, status, found := strings.Cut(body, "<status>")
	if !found {
		return "", false
	}
	status, _, found = strings.Cut(status, "</status>")
	if !found || status == "" {
		return "", false
	}
	return agentID, true
}

// peerMessage reports whether a user line is a message another agent sent the
// chat (SendMessage to the main chat): Claude Code writes it isMeta, with
// origin.kind "peer" and a promptId, and fires UserPromptSubmit for it, so it
// is a model-bound prompt like a typed one. A plain isMeta line (a
// system-reminder, a local-command caveat) fires none.
func peerMessage(l *line) bool { return l.Origin != nil && l.Origin.Kind == "peer" }

// scheduledPrompt reports whether a scheduled task's prompt is written isMeta
// with turnOrigin "scheduled" and fires UserPromptSubmit.
func scheduledPrompt(l *line) bool { return l.TurnOrigin == "scheduled" }

// localCommand marks the lines of a local slash command (`/model`, `/effort`):
// they carry a promptId but never reach the model, so no UserPromptSubmit.
var localCommand = []string{"<command-name>", "<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>"}

// isPrompt reports whether a user line is a prompt bound for the model: not a
// tool result, not a local command. The text is inspected, never kept.
func isPrompt(content json.RawMessage, blocks []block) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return false
		}
	}
	if len(content) > 0 && content[0] == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return false
		}
		for _, p := range localCommand {
			if strings.HasPrefix(text, p) {
				return false
			}
		}
		if cmd, _, _ := strings.Cut(strings.TrimSpace(text), " "); typedLocal[strings.SplitN(cmd, "\n", 2)[0]] {
			return false
		}
	}
	return true
}

// typedLocal are built-in commands Claude Code runs itself. Typed, one is
// written as a raw `/compact …` line carrying a promptId but fires no
// UserPromptSubmit: a manual /compact fires PreCompact with that promptId.
var typedLocal = map[string]bool{"/compact": true, "/clear": true, "/model": true, "/effort": true, "/reload": true}

func (w *world) assistant(t *transcript, l *line, blocks []block, ms int64) error {
	if l.IsAPIError {
		t.apiErrors++
	}
	m := l.Message
	if m == nil || m.ID == "" || m.Model == "<synthetic>" {
		return nil
	}
	t.assistants++
	t.lastAssistant = max(t.lastAssistant, ms)
	// A message's lines share its id; only its own lines set lastStop, so a
	// final message whose stop_reason Claude Code left null (a turn's reply
	// written while streaming and never rewritten) leaves it "", never the
	// end_turn of the message before it.
	if m.ID != t.lastMsg {
		t.lastMsg = m.ID
		t.lastStop = ""
	}
	msg := w.messages[m.ID]
	if msg == nil {
		msg = &message{id: m.ID, session: t.session, agent: t.agent, model: m.Model, ts: ms, promptID: t.prompt, toolIDs: map[string]bool{}}
		w.messages[m.ID] = msg
	}
	if m.Usage != nil {
		msg.usage = tokens{input: m.Usage.Input, output: m.Usage.Output, cacheRead: m.Usage.CacheRead, cacheCreation: m.Usage.CacheCreation}
	}
	if m.StopReason != nil && *m.StopReason != "" {
		msg.stopReason = *m.StopReason
		t.lastStop = *m.StopReason
		if msg.stopReason != "tool_use" {
			msg.endTS = ms
		}
		if msg.stopReason == "end_turn" {
			t.endTurns[m.ID] = true
		}
	}
	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		msg.toolIDs[b.ID] = true
		t.toolUses[b.ID] = true
		if b.Name == "TaskStop" {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if err := json.Unmarshal(b.Input, &in); err != nil {
				return fmt.Errorf("TaskStop %s input: %w", b.ID, err)
			}
			if in.TaskID != "" {
				w.taskStops[in.TaskID] = append(w.taskStops[in.TaskID], ms)
			}
		}
		if _, ok := w.uses[b.ID]; !ok {
			w.uses[b.ID] = &use{id: b.ID, name: b.Name, session: t.session, agent: t.agent, msgID: m.ID, ts: ms}
		}
	}
	return nil
}
