package callmeter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// RequestUsage is the model request that issued one tool call, as the
// transcript records it.
type RequestUsage struct {
	MessageID           string
	TS                  int64 // the entry's timestamp, Unix ms UTC
	Model               string
	StopReason          string // "" when the entry carries none
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CacheCreation5m and CacheCreation1h split CacheCreationTokens by cache
	// lifetime; nil when the usage carries no cache_creation object — a
	// missing split is never zero.
	CacheCreation5m *int64
	CacheCreation1h *int64
	ContextTokens   int64 // input + cache read + cache creation tokens
	OutputTokens    int64
	// ThinkingTokens is the share of OutputTokens spent thinking
	// (usage.output_tokens_details.thinking_tokens); nil when the usage carries
	// none (an older Claude Code) — a missing count is never zero.
	ThinkingTokens *int64
}

// SubagentTranscriptPath is the transcript of sub-agent agentID of the chat
// whose transcript is transcriptPath.
func SubagentTranscriptPath(transcriptPath, agentID string) string {
	return filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents", "agent-"+agentID+".jsonl")
}

// SubagentTranscripts lists the sub-agents whose transcript sits beside the
// main one: the ids of `{mainTranscript without .jsonl}/subagents/agent-*.jsonl`,
// in file-name order. No such directory is no sub-agent, not an error.
func SubagentTranscripts(mainTranscript string) (agentIDs []string, err error) {
	dir := filepath.Join(strings.TrimSuffix(mainTranscript, ".jsonl"), "subagents")
	files, err := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("list sub-agent transcripts in %s: %w", dir, err)
	}
	for _, file := range files {
		agentIDs = append(agentIDs, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "agent-"), ".jsonl"))
	}
	return agentIDs, nil
}

// UntypedAgentMissingTranscript reports whether a payload names one of Claude
// Code's own internal agents — agent_id set, agent_type empty — whose
// transcript was never written: no SubagentStart, nothing to meter. Shared by
// the hook's start, batch and stop and by the sweeps, so the exemption is
// checked once.
func UntypedAgentMissingTranscript(agentID, agentType, transcript string) bool {
	if agentID == "" || agentType != "" {
		return false
	}
	_, err := os.Stat(transcript)
	return errors.Is(err, fs.ErrNotExist)
}

// CallTranscripts groups unfinished calls by the transcript holding their
// result: the main chat's, or their sub-agent's own. transcripts lists each
// once, in the order of its first call; idsOf maps it to its calls' tool_use
// ids. A call of an untyped agent with no transcript on disk is left out
// (UntypedAgentMissingTranscript).
func CallTranscripts(mainTranscript string, calls []UnfinishedCall) (transcripts []string, idsOf map[string][]string) {
	idsOf = map[string][]string{}
	for _, call := range calls {
		transcript := mainTranscript
		if call.AgentID != nil {
			agentType := ""
			if call.AgentType != nil {
				agentType = *call.AgentType
			}
			transcript = SubagentTranscriptPath(mainTranscript, *call.AgentID)
			if UntypedAgentMissingTranscript(*call.AgentID, agentType, transcript) {
				continue
			}
		}
		if _, seen := idsOf[transcript]; !seen {
			transcripts = append(transcripts, transcript)
		}
		idsOf[transcript] = append(idsOf[transcript], call.ToolUseID)
	}
	return transcripts, idsOf
}

// presentString is a pointer to value, nil when it is empty: the way a column
// the source did not carry stays unprovided.
func presentString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ApplyUsage copies a transcript request's usage into a requests row: its
// time, model, stop reason and the token split. A cache-creation split the
// usage did not carry stays NULL.
func ApplyUsage(request *Request, usage RequestUsage) {
	request.TS = Ptr(usage.TS)
	request.Model = presentString(usage.Model)
	request.StopReason = presentString(usage.StopReason)
	request.InputTokens = Ptr(usage.InputTokens)
	request.CacheReadTokens = Ptr(usage.CacheReadTokens)
	request.CacheCreationTokens = Ptr(usage.CacheCreationTokens)
	request.CacheCreation5mTokens = usage.CacheCreation5m
	request.CacheCreation1hTokens = usage.CacheCreation1h
	request.ContextTokens = Ptr(usage.ContextTokens)
	request.OutputTokens = Ptr(usage.OutputTokens)
	request.ThinkingTokens = usage.ThinkingTokens
}

type transcriptEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type assistantMessage struct {
	timestamp  string          // its entry's, set by agentMessage
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason *string         `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      *struct {
		InputTokens              int64 `json:"input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		OutputTokensDetails      *struct {
			ThinkingTokens *int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
		CacheCreation *struct {
			Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

// toolUse is both a content block type and the stop reason of a message that
// calls tools: a message that stops with it is never an agent's last.
const toolUse = "tool_use"

type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// requestTailWindow is the transcript tail FindRequests reads first; each miss
// widens it fourfold until the whole file is read.
const requestTailWindow = 1 << 20

// FindRequests scans the transcript at path for the assistant entries whose
// content holds a tool_use block with one of toolUseIDs, and returns each
// found id's request at its final usage: Claude Code writes one entry per
// content block, all sharing the message id and each repeating the usage as it
// stood, and only the message's last entry carries the final output_tokens and
// stop_reason, so every later entry of a found message is folded in
// (foldEntry). The hook asks for the request it just saw, at the end of a
// transcript that grows to tens of MB, so the scan reads the last MB first and
// widens only while an id is missing; a line is decoded only when it mentions
// a wanted id or a found message. An id not yet on disk is absent from the
// map; a file that cannot be read, or a malformed line holding a wanted id or
// found message, is an error naming the path and the line's byte offset. A
// final line with no newline that does not parse is a writer mid-append and is
// skipped.
func FindRequests(path string, toolUseIDs []string) (found map[string]RequestUsage, err error) {
	found = map[string]RequestUsage{}
	wanted := map[string]bool{}
	for _, id := range toolUseIDs {
		wanted[id] = true
	}
	if len(wanted) == 0 {
		return found, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close transcript %s: %w", path, closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat transcript %s: %w", path, err)
	}
	size := info.Size()
	for window := int64(requestTailWindow); ; window *= 4 {
		start := max(0, size-window)
		scan := &requestScan{
			wanted: wanted, messages: map[string]*RequestUsage{}, owner: map[string]string{}, at: map[string]int64{},
		}
		if err := scan.read(file, path, start, size); err != nil {
			return nil, err
		}
		if len(scan.owner) == len(wanted) || start == 0 {
			found = map[string]RequestUsage{}
			for id, message := range scan.owner {
				request := *scan.messages[message]
				request.TS = scan.at[id]
				found[id] = request
			}
			return found, nil
		}
	}
}

// requestScan is one FindRequests pass: the messages holding a wanted id, at
// the usage of their latest entry read, the message owning each found id and
// the time of the entry naming it.
type requestScan struct {
	wanted   map[string]bool
	messages map[string]*RequestUsage // by message id
	owner    map[string]string        // tool_use id -> message id
	at       map[string]int64         // tool_use id -> its entry's timestamp, Unix ms UTC
}

// read scans [start, end) of the transcript line by line to its end, since a
// found message's final entry follows the entry naming the id; a start inside
// the file drops the first, partial line.
func (scan *requestScan) read(file *os.File, path string, start, end int64) error {
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, end-start), 1<<16)
	offset := start
	if start > 0 {
		skipped, err := reader.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read transcript %s at byte %d: %w", path, offset, err)
		}
		offset += int64(len(skipped))
	}
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read transcript %s at byte %d: %w", path, offset, readErr)
		}
		partial := errors.Is(readErr, io.EOF)
		if scan.mentioned(raw) {
			if err := scan.entry(raw); err != nil && !partial {
				return fmt.Errorf("transcript %s at byte %d: %w", path, offset, err)
			}
		}
		offset += int64(len(raw))
		if partial {
			return nil
		}
	}
}

// mentioned is the cheap pre-check before a line is decoded: whether it holds
// a wanted id not found yet or the id of a found message.
func (scan *requestScan) mentioned(raw []byte) bool {
	for id := range scan.wanted {
		if _, done := scan.owner[id]; !done && bytes.Contains(raw, []byte(id)) {
			return true
		}
	}
	for message := range scan.messages {
		if bytes.Contains(raw, []byte(message)) {
			return true
		}
	}
	return false
}

func (scan *requestScan) entry(raw []byte) error {
	var entry transcriptEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("malformed entry: %w", err)
	}
	if entry.Type != "assistant" || len(entry.Message) == 0 {
		return nil
	}
	var message assistantMessage
	if err := json.Unmarshal(entry.Message, &message); err != nil {
		return fmt.Errorf("malformed assistant message: %w", err)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return fmt.Errorf("assistant message %q: content is not a block list: %w", message.ID, err)
	}
	if request, known := scan.messages[message.ID]; known {
		foldEntry(request, &message)
	}
	for _, block := range blocks {
		if block.Type != toolUse || !scan.wanted[block.ID] {
			continue
		}
		if _, done := scan.owner[block.ID]; done {
			continue
		}
		if message.Usage == nil {
			return fmt.Errorf("assistant message %q holding %s has no usage", message.ID, block.ID)
		}
		at, err := entryTime(message.ID, entry.Timestamp)
		if err != nil {
			return err
		}
		if _, known := scan.messages[message.ID]; !known {
			request := &RequestUsage{MessageID: message.ID}
			foldEntry(request, &message)
			scan.messages[message.ID] = request
		}
		scan.owner[block.ID] = message.ID
		scan.at[block.ID] = at.UnixMilli()
	}
	return nil
}

// entryTime is the timestamp of the assistant entry carrying messageID. One
// that does not parse is an error naming the message id and the timestamp's
// size, never its value nor the parse error quoting it: the value may be any
// text, and the error reaches faults.error.
func entryTime(messageID, timestamp string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("assistant message %q: timestamp of %d bytes is not RFC 3339", messageID, len(timestamp))
	}
	return at, nil
}

// foldEntry folds one entry of a message into its request: the model, stop
// reason and usage the entry carries replace the earlier entries' (the last
// entry's are final); the message id and the first entry's time stay.
func foldEntry(request *RequestUsage, message *assistantMessage) {
	if message.Model != "" {
		request.Model = message.Model
	}
	if message.StopReason != nil && *message.StopReason != "" {
		request.StopReason = *message.StopReason
	}
	usage := message.Usage
	if usage == nil {
		return
	}
	request.InputTokens = usage.InputTokens
	request.CacheReadTokens = usage.CacheReadInputTokens
	request.CacheCreationTokens = usage.CacheCreationInputTokens
	request.ContextTokens = usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	request.OutputTokens = usage.OutputTokens
	request.ThinkingTokens = nil
	if usage.OutputTokensDetails != nil {
		request.ThinkingTokens = usage.OutputTokensDetails.ThinkingTokens
	}
	request.CacheCreation5m, request.CacheCreation1h = nil, nil
	if usage.CacheCreation != nil {
		request.CacheCreation5m = usage.CacheCreation.Ephemeral5mInputTokens
		request.CacheCreation1h = usage.CacheCreation.Ephemeral1hInputTokens
	}
}

// TranscriptRequest is one model request a transcript holds, at its final
// usage, with the prompt it answered and the calls it issued.
type TranscriptRequest struct {
	RequestUsage
	PromptID   string    // the promptId of the last user entry before it; "" when none carries one
	ToolUseIDs []string  // its tool_use blocks, in order
	ToolUses   []ToolUse // the same blocks with their tool and input
	Cwd        string    // the cwd of its first entry; "" when it carries none
	// EndTS is the timestamp of its entry that set a turn-ending stop_reason
	// (set, not tool_use), Unix ms UTC: the turn's end line. 0 while none did.
	EndTS int64
}

// ToolUse is one tool_use block of a transcript request: its id, its tool and
// its input as written, which only SanitizeInput may turn into a stored value.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// toolUseBlock is a content block read for its tool_use fields.
type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// SubagentMeta reads the agent type and the parent Agent call's tool_use id
// Claude Code writes beside a sub-agent's transcript
// (`agent-{agentID}.meta.json`); a missing file is an error wrapping
// fs.ErrNotExist.
func SubagentMeta(mainTranscript, agentID string) (agentType, toolUseID string, err error) {
	path := strings.TrimSuffix(SubagentTranscriptPath(mainTranscript, agentID), ".jsonl") + ".meta.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read sub-agent meta: %w", err)
	}
	var meta struct {
		AgentType string `json:"agentType"`
		ToolUseID string `json:"toolUseId"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", "", fmt.Errorf("sub-agent meta %s: %w", path, err)
	}
	return meta.AgentType, meta.ToolUseID, nil
}

// ReadRequests reads every model request of the transcript at path, in the
// order of their first entries, each at its final usage (foldEntry): a reply
// with no tool call too, which no PostToolBatch ever reads. final reports
// whether the last assistant entry ends the turn (a stop_reason other than
// tool_use), so the transcript holds the turn's last message. A message with no
// id, the synthetic message Claude Code writes itself (syntheticModel) and a
// message no entry gives usage are not requests. A file that cannot be read or
// a malformed line is an error naming the path and the line's byte offset; a
// final line with no newline that does not parse is a writer mid-append and is
// skipped. Only lines naming an assistant entry or a prompt are decoded.
func ReadRequests(path string) (requests []TranscriptRequest, final bool, err error) {
	return ReadTurnRequests(path, "")
}

// ReadTaskNotices reads only task-id and non-empty status tags of task
// notifications; nothing of the body is kept. Times are in file order.
func ReadTaskNotices(path string) (notices map[string][]int64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read task notices: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close task notices %s: %w", path, closeErr))
		}
	}()
	notices = map[string][]int64{}
	reader := bufio.NewReaderSize(file, 1<<16)
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read task notices %s: %w", path, readErr)
		}
		if bytes.Contains(raw, []byte("task-notification")) {
			var entry struct {
				Type      string `json:"type"`
				Timestamp string `json:"timestamp"`
				Origin    struct {
					Kind string `json:"kind"`
				} `json:"origin"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
				Attachment struct {
					Type   string `json:"type"`
					Origin struct {
						Kind string `json:"kind"`
					} `json:"origin"`
					Prompt json.RawMessage `json:"prompt"`
				} `json:"attachment"`
			}
			if json.Unmarshal(raw, &entry) == nil {
				// The body is a string, or a content-block array (a queued prompt
				// carrying an image) read from its "text" blocks; any other shape
				// is no notice, never a failed line.
				var body string
				switch {
				case entry.Type == "user" && entry.Origin.Kind == "task-notification":
					body, _ = ResultText(entry.Message.Content)
				case entry.Type == "attachment" && entry.Attachment.Type == "queued_command" && entry.Attachment.Origin.Kind == "task-notification":
					body, _ = ResultText(entry.Attachment.Prompt)
				}
				_, task, hasTask := strings.Cut(body, "<task-id>")
				agentID, _, taskClosed := strings.Cut(task, "</task-id>")
				_, status, hasStatus := strings.Cut(body, "<status>")
				status, _, statusClosed := strings.Cut(status, "</status>")
				at, timeErr := time.Parse(time.RFC3339Nano, entry.Timestamp)
				if hasTask && taskClosed && agentID != "" && hasStatus && statusClosed && status != "" && timeErr == nil {
					notices[agentID] = append(notices[agentID], at.UnixMilli())
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return notices, nil
}

// ReadTurnRequests is ReadRequests for the turn of promptID: final also needs
// no user entry carrying promptID after that last assistant entry. Such an
// entry (the turn's prompt, a tool_result, or the prompt that woke an agent
// whose earlier turn ended) is still to be answered, even when the entry
// before it ended an earlier turn. A promptID no entry carries, or "", is
// ReadRequests.
func ReadTurnRequests(path, promptID string) (requests []TranscriptRequest, final bool, err error) {
	read, err := ReadTranscript(path, promptID)
	if err != nil {
		return nil, false, err
	}
	return read.Requests, read.Final, nil
}

// TranscriptRead is what one pass over a transcript holds: its requests, the
// final flag of ReadTurnRequests and the numeric marks Claude Code writes
// beside them.
type TranscriptRead struct {
	Requests []TranscriptRequest
	Final    bool
	Marks    TranscriptMarks
}

// ReadTranscript is ReadTurnRequests in one pass that also collects the
// transcript's marks (TranscriptMarks): compactions, Stop-hook summaries, turn
// durations and the last cost-state. A mark line follows the rules of the
// others: a final line with no newline that does not parse is skipped, any
// other malformed line is an error naming the path and its byte offset. Only
// lines naming an assistant entry, a prompt or a mark are decoded.
func ReadTranscript(path, promptID string) (read TranscriptRead, err error) {
	file, err := os.Open(path)
	if err != nil {
		return TranscriptRead{}, fmt.Errorf("read transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close transcript %s: %w", path, closeErr))
		}
	}()
	scan := transcriptRequests{index: map[string]int{}, used: map[string]bool{}, turn: promptID, seen: map[string]bool{}}
	reader := bufio.NewReaderSize(file, 1<<16)
	var offset int64
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return TranscriptRead{}, fmt.Errorf("read transcript %s at byte %d: %w", path, offset, readErr)
		}
		partial := errors.Is(readErr, io.EOF)
		if bytes.Contains(raw, []byte(`"assistant"`)) || bytes.Contains(raw, []byte(`"promptId"`)) ||
			bytes.Contains(raw, []byte(`"compact_boundary"`)) || bytes.Contains(raw, []byte(`"stop_hook_summary"`)) ||
			bytes.Contains(raw, []byte(`"turn_duration"`)) || bytes.Contains(raw, []byte(`"cost-state"`)) {
			if err := scan.entry(raw); err != nil && !partial {
				return TranscriptRead{}, fmt.Errorf("transcript %s at byte %d: %w", path, offset, err)
			}
		}
		offset += int64(len(raw))
		if partial {
			break
		}
	}
	for _, request := range scan.requests {
		if scan.used[request.MessageID] {
			read.Requests = append(read.Requests, request)
		}
	}
	read.Final = scan.final
	read.Marks = scan.marks
	return read, nil
}

// transcriptRequests is one ReadRequests pass.
type transcriptRequests struct {
	requests []TranscriptRequest
	index    map[string]int  // message id -> its position in requests
	used     map[string]bool // message ids some entry gave usage
	prompt   string          // the latest user entry's promptId
	turn     string          // the promptId whose answer final waits for; "" for none
	final    bool
	marks    TranscriptMarks
	seen     map[string]bool // entry ids of the marks kept, so a repeated line counts once
}

func (scan *transcriptRequests) entry(raw []byte) error {
	var entry struct {
		transcriptEntry
		PromptID string `json:"promptId"`
		Cwd      string `json:"cwd"`
		UUID     string `json:"uuid"`
		Subtype  string `json:"subtype"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("malformed entry: %w", err)
	}
	switch {
	case entry.Type == "system":
		return scan.system(raw, entry.UUID, entry.Subtype, entry.Timestamp)
	case entry.Type == "cost-state":
		return scan.cost(raw)
	}
	if entry.Type == "user" && entry.PromptID != "" {
		scan.prompt = entry.PromptID
		if entry.PromptID == scan.turn {
			scan.final = false // the turn's answer to this entry is still to come
		}
		return nil
	}
	if entry.Type != "assistant" || len(entry.Message) == 0 {
		return nil
	}
	var message assistantMessage
	if err := json.Unmarshal(entry.Message, &message); err != nil {
		return fmt.Errorf("malformed assistant message: %w", err)
	}
	if message.ID == "" || message.Model == syntheticModel {
		return nil
	}
	var blocks []toolUseBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return fmt.Errorf("assistant message %q: content is not a block list: %w", message.ID, err)
	}
	i, known := scan.index[message.ID]
	if !known {
		at, err := entryTime(message.ID, entry.Timestamp)
		if err != nil {
			return err
		}
		i = len(scan.requests)
		scan.index[message.ID] = i
		scan.requests = append(scan.requests, TranscriptRequest{
			RequestUsage: RequestUsage{MessageID: message.ID, TS: at.UnixMilli()},
			PromptID:     scan.prompt,
			Cwd:          entry.Cwd,
		})
	}
	request := &scan.requests[i]
	foldEntry(&request.RequestUsage, &message)
	if message.StopReason != nil && *message.StopReason != "" && *message.StopReason != toolUse {
		at, err := entryTime(message.ID, entry.Timestamp)
		if err != nil {
			return err
		}
		request.EndTS = at.UnixMilli()
	}
	if message.Usage != nil {
		scan.used[message.ID] = true
	}
	for _, block := range blocks {
		if block.Type == toolUse && block.ID != "" && !slices.Contains(request.ToolUseIDs, block.ID) {
			request.ToolUseIDs = append(request.ToolUseIDs, block.ID)
			request.ToolUses = append(request.ToolUses, ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	scan.final = message.StopReason != nil && *message.StopReason != toolUse
	return nil
}

// AgentTotals is what a sub-agent's own transcript says it spent.
type AgentTotals struct {
	TotalTokens int64  // over distinct assistant message ids: input + cache read + cache creation + output
	ToolUses    int64  // distinct tool_use blocks
	Model       string // the last model its messages name; "" when none does
	// Final: the last assistant entry ends the agent's turn (a stop_reason
	// set and not tool_use) and no user entry of the turn read for follows it
	// (ReadTurnRequests), so the transcript holds the turn's last message.
	Final bool
	// LastTimestamp is the last assistant entry's timestamp as written (RFC
	// 3339): the agent's stop when Final.
	LastTimestamp string
}

// ReadAgentTotals sums the sub-agent transcript at path. One model message is
// split across several entries sharing its id, each repeating its usage, so a
// message's usage counts once (its last entry's, the final count). A file that
// cannot be read, a malformed line or an assistant message with no id is an
// error naming the path and line; a final line with no newline that does not
// parse is a writer mid-append and is skipped. promptID is the turn whose
// answer Final waits for, as in ReadTurnRequests; "" for none.
func ReadAgentTotals(path, promptID string) (AgentTotals, error) {
	var totals AgentTotals
	file, err := os.Open(path)
	if err != nil {
		return totals, fmt.Errorf("read agent transcript: %w", err)
	}
	usage := map[string]int64{}
	var order []string
	toolUses := map[string]bool{}
	reader := bufio.NewReaderSize(file, 1<<16)
	for line := 1; ; line++ {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return totals, errors.Join(
				fmt.Errorf("read agent transcript %s line %d: %w", path, line, readErr),
				file.Close(),
			)
		}
		partial := errors.Is(readErr, io.EOF)
		if strings.TrimSpace(string(raw)) != "" {
			message, prompt, err := agentMessage(raw)
			switch {
			case err != nil && !partial:
				return totals, errors.Join(fmt.Errorf("agent transcript %s line %d: %w", path, line, err), file.Close())
			case err == nil && promptID != "" && prompt == promptID:
				totals.Final = false // the turn's answer to this entry is still to come
			case err == nil && message != nil:
				if _, seen := usage[message.ID]; !seen {
					order = append(order, message.ID)
				}
				if message.Usage != nil {
					u := message.Usage
					usage[message.ID] = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
				}
				if message.Model != "" && message.Model != "<synthetic>" {
					totals.Model = message.Model
				}
				totals.Final = message.StopReason != nil && *message.StopReason != toolUse
				totals.LastTimestamp = message.timestamp
				var blocks []contentBlock
				if err := json.Unmarshal(message.Content, &blocks); err != nil {
					return totals, errors.Join(
						fmt.Errorf("agent transcript %s line %d: message %q content is not a block list: %w",
							path, line, message.ID, err),
						file.Close(),
					)
				}
				for _, block := range blocks {
					if block.Type == toolUse && block.ID != "" {
						toolUses[block.ID] = true
					}
				}
			}
		}
		if partial {
			break
		}
	}
	if err := file.Close(); err != nil {
		return totals, fmt.Errorf("close agent transcript %s: %w", path, err)
	}
	for _, id := range order {
		totals.TotalTokens += usage[id]
	}
	totals.ToolUses = int64(len(toolUses))
	return totals, nil
}

// agentMessage decodes one transcript line: its assistant message, or the
// promptId of a user entry; nil and "" for any other entry.
func agentMessage(raw []byte) (message *assistantMessage, prompt string, err error) {
	var entry struct {
		transcriptEntry
		PromptID string `json:"promptId"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, "", fmt.Errorf("malformed entry: %w", err)
	}
	if entry.Type == "user" {
		return nil, entry.PromptID, nil
	}
	if entry.Type != "assistant" || len(entry.Message) == 0 {
		return nil, "", nil
	}
	message = &assistantMessage{}
	if err := json.Unmarshal(entry.Message, message); err != nil {
		return nil, "", fmt.Errorf("malformed assistant message: %w", err)
	}
	if message.ID == "" {
		return nil, "", errors.New("assistant message carries no id")
	}
	message.timestamp = entry.Timestamp
	return message, "", nil
}

// syntheticModel is the model of a message Claude Code writes itself, never
// a model's answer: an API error, an interruption.
const syntheticModel = "<synthetic>"

// UnknownAPIError is the kind of an API error whose transcript entry names
// none in the shape of one.
const UnknownAPIError = "unknown"

// apiErrorKind is the shape of an error kind Claude Code writes (`rate_limit`,
// `oauth_org_not_allowed`): anything else is not passed on, so no message
// text leaves the transcript. A StopFailure hook's error passes the same gate
// to be stored as a label (detailLabels).
var apiErrorKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type messageEntry struct {
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	IsAPIError bool            `json:"isApiErrorMessage"`
	Error      json.RawMessage `json:"error"`
	Message    json.RawMessage `json:"message"`
}

// TranscriptAPIError reports the API error a transcript ends on: when its last
// message (user or assistant entry) is the `<synthetic>` assistant message
// Claude Code writes in place of an answer the API refused, the entry's error
// kind, else UnknownAPIError; otherwise "". Bookkeeping entries after the
// message do not count. The scan reads the tail first and widens fourfold
// while it holds no message. A file that cannot be read or a malformed line is
// an error naming the path and the line's byte offset; a final line with no
// newline that does not parse is a writer mid-append and is skipped.
func TranscriptAPIError(path string) (kind string, err error) {
	end, err := TranscriptTurnEnd(path)
	return end.ErrorType, err
}

// TurnEnd is how a main transcript says its latest turn ended. Event is
// EventStopFailure, ErrorType its kind, when the last user or assistant entry
// is the `<synthetic>` API-error entry (TranscriptAPIError); EventStop when it
// is a model answer whose stop_reason ends the turn (turnEndReasons); "" when
// the turn has no end on disk: a tool call or a prompt still awaiting its
// answer, an interruption, or no message at all. TS is the entry's own
// timestamp in Unix ms: 0 when Event is "" or the timestamp is absent or not
// RFC 3339.
type TurnEnd struct {
	Event     string
	ErrorType string
	TS        int64
}

// turnEndReasons are the stop_reason values of a model answer that ends its
// turn, after which Claude Code runs the Stop hooks. tool_use (a call to run)
// and pause_turn (a server tool resuming) continue the turn.
var turnEndReasons = map[string]bool{"end_turn": true, "stop_sequence": true}

// TranscriptTurnEnd reads how the transcript at path ends its latest turn.
func TranscriptTurnEnd(path string) (end TurnEnd, err error) {
	file, err := os.Open(path)
	if err != nil {
		return TurnEnd{}, fmt.Errorf("read transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close transcript %s: %w", path, closeErr))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return TurnEnd{}, fmt.Errorf("stat transcript %s: %w", path, err)
	}
	size := info.Size()
	for window := int64(requestTailWindow); ; window *= 4 {
		start := max(0, size-window)
		last, err := lastMessage(file, path, start, size)
		if err != nil {
			return TurnEnd{}, err
		}
		if last != nil || start == 0 {
			end = turnEndOf(last)
			if end.Event == "" {
				return TurnEnd{}, nil
			}
			if at, parseErr := time.Parse(time.RFC3339, last.Timestamp); parseErr == nil {
				end.TS = at.UnixMilli()
			}
			return end, nil
		}
	}
}

// turnEndOf is the turn end entry says, its TS unset: a StopFailure for an
// API-error entry, a Stop for a model answer ending the turn, else none.
func turnEndOf(entry *messageEntry) TurnEnd {
	if kind := apiErrorOf(entry); kind != "" {
		return TurnEnd{Event: EventStopFailure, ErrorType: kind}
	}
	if entry == nil || entry.Type != "assistant" {
		return TurnEnd{}
	}
	var message struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal(entry.Message, &message) != nil || message.Model == syntheticModel || !turnEndReasons[message.StopReason] {
		return TurnEnd{}
	}
	return TurnEnd{Event: EventStop}
}

// lastMessage is the last user or assistant entry in [start, end) of the
// transcript, nil when there is none; a start inside the file drops the
// first, partial line.
func lastMessage(file *os.File, path string, start, end int64) (*messageEntry, error) {
	reader := bufio.NewReaderSize(io.NewSectionReader(file, start, end-start), 1<<16)
	offset := start
	var last *messageEntry
	for first := true; ; first = false {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("read transcript %s at byte %d: %w", path, offset, readErr)
		}
		partial := errors.Is(readErr, io.EOF)
		if !(first && start > 0) && len(bytes.TrimSpace(raw)) > 0 {
			var entry messageEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				if !partial {
					return nil, fmt.Errorf("transcript %s at byte %d: malformed entry: %w", path, offset, err)
				}
			} else if entry.Type == "user" || entry.Type == "assistant" {
				last = &entry
			}
		}
		offset += int64(len(raw))
		if partial {
			return last, nil
		}
	}
}

// apiErrorOf is the error kind of entry when it is a `<synthetic>` API-error
// message, else "".
func apiErrorOf(entry *messageEntry) string {
	if entry == nil || entry.Type != "assistant" {
		return ""
	}
	var message struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(entry.Message, &message) != nil || message.Model != syntheticModel {
		return ""
	}
	if !entry.IsAPIError && absentJSON(entry.Error) {
		return "" // a synthetic message that is no API error: an interruption
	}
	var kind string
	if json.Unmarshal(entry.Error, &kind) != nil {
		return UnknownAPIError
	}
	return APIErrorKind(kind)
}

// APIErrorKind is s when it has the shape of an API error kind, else
// UnknownAPIError: the one door an error kind passes to a report.
func APIErrorKind(s string) string {
	if !apiErrorKind.MatchString(s) {
		return UnknownAPIError
	}
	return s
}

func absentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// TranscriptMarks are the numbers Claude Code writes into a transcript beside
// the messages: compactions, the Stop hooks of a turn, a turn's wall time and
// the session's cost-state. Nothing but numbers, a trigger label and a derived
// hook name is read: no prompt, message or command text is kept.
type TranscriptMarks struct {
	Compactions   []Compaction
	StopHooks     []StopHookSummary
	TurnDurations []TurnDuration
	Cost          *CostState // the last cost-state entry in file order; nil when none
}

// Compaction is one compact_boundary entry. A number the entry lacks is nil.
type Compaction struct {
	EntryID                 string
	TS                      int64  // Unix ms UTC
	Trigger                 string // "" unless it has the shape of a label (compactTrigger)
	PreTokens               *int64
	PostTokens              *int64
	CumulativeDroppedTokens *int64
	DurationMS              *int64
}

// StopHookSummary is one stop_hook_summary entry: the Stop hooks of a turn.
// PromptID is the latest user promptId before it, the turn it closes.
type StopHookSummary struct {
	EntryID    string
	TS         int64
	PromptID   string
	HookCount  int64
	HookErrors int64 // how many errors it lists, never their text
	Hooks      []HookRun
}

// HookRun is one hook a Stop ran. DurationMS is nil for an async hook, which
// the summary gives none.
type HookRun struct {
	Name         string // StopHookName of its command; never a word of a prompt
	CommandBytes int64  // the command's length; the command itself is never kept
	DurationMS   *int64
}

// TurnDuration is one turn_duration entry, written after the turn's Stop-hook
// summary and carrying no promptId: PromptID is the latest user promptId
// before it, the turn it closes.
type TurnDuration struct {
	EntryID          string
	TS               int64
	PromptID         string
	DurationMS       *int64
	MessageCount     *int64
	BackgroundAgents *int64
}

// CostState is one cost-state entry: the session's cumulative cost so far.
type CostState struct {
	SessionID    string
	CostUSD      *float64
	APIMS        *int64
	APINoRetryMS *int64
	ToolMS       *int64
	WallMS       *int64
	Started      *int64             // the session's start, Unix ms UTC
	ModelCosts   map[string]float64 // USD by model, keys of the shape of a model name only
}

var (
	compactTrigger = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	modelCostKey   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@\[\]/-]{0,127}$`)
	hookNameShape  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	// programName is the shape of every name HookName keeps: a lowercase
	// program name, or a file name ending in an extension. A capitalized word,
	// the way a prompt opens, is neither.
	programName   = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9_+-]*|[A-Za-z0-9][A-Za-z0-9._+-]*\.[A-Za-z0-9]+)$`)
	hashedHook    = regexp.MustCompile(`^(?:prompt)?#[0-9a-f]{8}$`)
	proseWord     = regexp.MustCompile(`^[A-Za-z][A-Za-z']*[,.:;!?]?$`)
	envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

// interpreters are the programs whose first argument, not their own name, says
// what a hook runs.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "env": true, "exec": true, "node": true,
	"python": true, "python3": true, "uv": true, "uvx": true, "npx": true, "bunx": true, "bun": true,
	"deno": true, "ruby": true, "perl": true,
}

// HookName is the stored name of a command hook. The name comes only from a
// token that says what runs: a path (it holds a `/`), whose basename is the
// name, or a known interpreter, whose first argument wins when that is a path
// or a file name with an extension (`python3 hook.py`), else the interpreter
// is the name. A bare word is never a name, so free prose yields none of its
// words. The name must also have a program's shape (programName); every other
// command is `#` and the first 8 hex digits of its SHA-256. The command is
// never kept, so no argument or path leaves the transcript.
func HookName(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 && envAssignment.MatchString(fields[0]) {
		fields = fields[1:]
	}
	name := ""
	if len(fields) > 0 {
		program := strings.Trim(fields[0], `"'`)
		base := program[strings.LastIndex(program, "/")+1:]
		switch {
		case interpreters[base]:
			name = base
			for _, field := range fields[1:] {
				field = strings.Trim(field, `"'`)
				if strings.HasPrefix(field, "-") || field == "run" || envAssignment.MatchString(field) {
					continue
				}
				if script := field[strings.LastIndex(field, "/")+1:]; strings.Contains(field, "/") || strings.Contains(script, ".") {
					name = script
				}
				break
			}
		case strings.Contains(program, "/"):
			name = base
		}
	}
	if hookNameShape.MatchString(name) && programName.MatchString(name) {
		return name
	}
	return "#" + shortSum(command)
}

// StopHookName is the stored name of one hookInfos entry of a Stop-hook
// summary. A prompt-type hook, whose entry carries promptText and whose
// command is the prompt itself, and a command that reads as prose are
// `prompt#` and the first 8 hex digits of the command's SHA-256; every other
// command is its HookName.
func StopHookName(command string, prompt bool) string {
	if prompt || readsAsProse(command) {
		return "prompt#" + shortSum(command)
	}
	return HookName(command)
}

// HookNameKept reports whether name is a shape StopHookName can store: a
// program name of programName's shape, or a hash. Redact rewrites every other
// stored name.
func HookNameKept(name string) bool {
	return hashedHook.MatchString(name) || hookNameShape.MatchString(name) && programName.MatchString(name)
}

// readsAsProse reports whether a hook command is a sentence rather than a
// command line: three or more fields, the first a plain word that is no known
// interpreter, and no field an option or holding a path or shell syntax.
func readsAsProse(command string) bool {
	fields := strings.Fields(command)
	if len(fields) < 3 || !proseWord.MatchString(fields[0]) || interpreters[fields[0]] {
		return false
	}
	for _, field := range fields {
		if strings.HasPrefix(field, "-") || strings.ContainsAny(field, "/$=|&<>`") {
			return false
		}
	}
	return true
}

// shortSum is the first 8 hex digits of text's SHA-256.
func shortSum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:8]
}

// wholeNumber is n as an integer, nil when it is absent; a number written with
// a fraction is cut, so a version that writes durations as floats never fails
// the read.
func wholeNumber(n *float64) *int64 {
	if n == nil {
		return nil
	}
	return Ptr(int64(*n))
}

// markTime is the timestamp of a mark entry, Unix ms UTC. An unparsable one is
// an error naming only its size, as entryTime does.
func markTime(timestamp string) (int64, error) {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("mark entry: timestamp of %d bytes is not RFC 3339", len(timestamp))
	}
	return at.UnixMilli(), nil
}

// system reads one system entry when it is a mark: a compaction, a Stop-hook
// summary or a turn duration. Any other subtype, and an entry with no uuid, is
// no mark.
func (scan *transcriptRequests) system(raw []byte, uuid, subtype, timestamp string) error {
	if uuid == "" || scan.seen[uuid] {
		return nil
	}
	switch subtype {
	case "compact_boundary":
		var entry struct {
			CompactMetadata struct {
				Trigger                 string   `json:"trigger"`
				PreTokens               *float64 `json:"preTokens"`
				PostTokens              *float64 `json:"postTokens"`
				CumulativeDroppedTokens *float64 `json:"cumulativeDroppedTokens"`
				DurationMS              *float64 `json:"durationMs"`
			} `json:"compactMetadata"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("malformed compact_boundary entry: %w", err)
		}
		at, err := markTime(timestamp)
		if err != nil {
			return err
		}
		meta := entry.CompactMetadata
		compaction := Compaction{
			EntryID: uuid, TS: at, PreTokens: wholeNumber(meta.PreTokens), PostTokens: wholeNumber(meta.PostTokens),
			CumulativeDroppedTokens: wholeNumber(meta.CumulativeDroppedTokens), DurationMS: wholeNumber(meta.DurationMS),
		}
		if compactTrigger.MatchString(meta.Trigger) {
			compaction.Trigger = meta.Trigger
		}
		scan.marks.Compactions = append(scan.marks.Compactions, compaction)
	case "stop_hook_summary":
		var entry struct {
			HookCount *float64 `json:"hookCount"`
			HookInfos []struct {
				Command    string          `json:"command"`
				DurationMS *float64        `json:"durationMs"`
				PromptText json.RawMessage `json:"promptText"` // only its presence is read
			} `json:"hookInfos"`
			HookErrors []json.RawMessage `json:"hookErrors"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("malformed stop_hook_summary entry: %w", err)
		}
		at, err := markTime(timestamp)
		if err != nil {
			return err
		}
		summary := StopHookSummary{
			EntryID: uuid, TS: at, PromptID: scan.prompt,
			HookCount: int64(len(entry.HookInfos)), HookErrors: int64(len(entry.HookErrors)),
		}
		if entry.HookCount != nil {
			summary.HookCount = *wholeNumber(entry.HookCount)
		}
		for _, info := range entry.HookInfos {
			summary.Hooks = append(summary.Hooks, HookRun{
				Name: StopHookName(info.Command, info.PromptText != nil), CommandBytes: int64(len(info.Command)), DurationMS: wholeNumber(info.DurationMS),
			})
		}
		scan.marks.StopHooks = append(scan.marks.StopHooks, summary)
	case "turn_duration":
		var entry struct {
			DurationMS       *float64 `json:"durationMs"`
			MessageCount     *float64 `json:"messageCount"`
			BackgroundAgents *float64 `json:"pendingBackgroundAgentCount"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("malformed turn_duration entry: %w", err)
		}
		at, err := markTime(timestamp)
		if err != nil {
			return err
		}
		scan.marks.TurnDurations = append(scan.marks.TurnDurations, TurnDuration{
			EntryID: uuid, TS: at, PromptID: scan.prompt, DurationMS: wholeNumber(entry.DurationMS),
			MessageCount: wholeNumber(entry.MessageCount), BackgroundAgents: wholeNumber(entry.BackgroundAgents),
		})
	default:
		return nil
	}
	scan.seen[uuid] = true
	return nil
}

// cost reads one cost-state entry; the last one read stands.
func (scan *transcriptRequests) cost(raw []byte) error {
	var entry struct {
		SessionID    string   `json:"sessionId"`
		CostUSD      *float64 `json:"totalCostUSD"`
		APIMS        *float64 `json:"totalAPIDuration"`
		APINoRetryMS *float64 `json:"totalAPIDurationWithoutRetries"`
		ToolMS       *float64 `json:"totalToolDuration"`
		WallMS       *float64 `json:"totalDuration"`
		Started      *float64 `json:"startTime"`
		ModelUsage   map[string]struct {
			CostUSD *float64 `json:"costUSD"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("malformed cost-state entry: %w", err)
	}
	cost := &CostState{
		SessionID: entry.SessionID, CostUSD: entry.CostUSD, APIMS: wholeNumber(entry.APIMS),
		APINoRetryMS: wholeNumber(entry.APINoRetryMS), ToolMS: wholeNumber(entry.ToolMS),
		WallMS: wholeNumber(entry.WallMS), Started: wholeNumber(entry.Started),
	}
	for model, usage := range entry.ModelUsage {
		if usage.CostUSD == nil || !modelCostKey.MatchString(model) {
			continue
		}
		if cost.ModelCosts == nil {
			cost.ModelCosts = map[string]float64{}
		}
		cost.ModelCosts[model] = *usage.CostUSD
	}
	scan.marks.Cost = cost
	return nil
}
