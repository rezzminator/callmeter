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
	MessageID string
	// NoUsage: no entry of the message carried usage, so its token counts are
	// unknown, never 0 (ApplyUsage leaves them NULL).
	NoUsage bool
	// APIRequestID is the requestId of the transcript line, the API request id
	// the cost ledger keys by; "" when the line carries none.
	APIRequestID        string
	TS                  int64 // the entry's timestamp, Unix ms UTC; 0 when it is not RFC 3339 (unknown)
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
	// Iterations are the entries of usage.iterations that are not the message
	// itself (type "message"): a fallback model's answer, another model's call
	// inside the request. nil when the usage carries none.
	Iterations []Iteration
}

// Iteration is one entry of a request's usage.iterations, as the transcript
// records it: its numbers copied, a number the entry did not carry nil.
type Iteration struct {
	Seq                 int    // its index in usage.iterations
	Type                string // "" when the entry names none
	Model               string // "" when the entry names none
	InputTokens         *int64
	CacheReadTokens     *int64
	CacheCreationTokens *int64
	CacheCreation5m     *int64 // the split of CacheCreationTokens by cache lifetime; nil without a cache_creation object
	CacheCreation1h     *int64
	OutputTokens        *int64
}

// SubagentTranscriptPath is the transcript of sub-agent agentID of the chat
// whose transcript is transcriptPath.
func SubagentTranscriptPath(transcriptPath, agentID string) string {
	return filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents", "agent-"+agentID+".jsonl")
}

// GlobLiteral escapes a path so filepath.Glob reads it as written: a projects
// directory may hold `[`, `*`, `?` or `\` (the escape is Unix-only, as
// callmeter is).
var GlobLiteral = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)

// SubagentTranscripts lists the sub-agents whose transcript sits beside the
// main one: the ids of `{mainTranscript without .jsonl}/subagents/agent-*.jsonl`,
// in file-name order, the directory matched as written (GlobLiteral). No such
// directory is no sub-agent.
func SubagentTranscripts(mainTranscript string) (agentIDs []string) {
	dir := filepath.Join(strings.TrimSuffix(mainTranscript, ".jsonl"), "subagents")
	// Glob's only error is a malformed pattern, and an escaped directory before
	// a fixed name makes none.
	files, _ := filepath.Glob(filepath.Join(GlobLiteral.Replace(dir), "agent-*.jsonl"))
	for _, file := range files {
		agentIDs = append(agentIDs, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(file), "agent-"), ".jsonl"))
	}
	return agentIDs
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
// time, model, stop reason, the token split and the iterations that are not
// the message itself. A cache-creation split the usage did not carry stays
// NULL, and an unknown time (TS 0) and the token counts of a message no entry
// gave usage (NoUsage) are left as the row holds them.
func ApplyUsage(request *Request, usage RequestUsage) {
	if usage.TS != 0 {
		request.TS = Ptr(usage.TS)
	}
	request.Model = presentString(usage.Model)
	request.StopReason = presentString(usage.StopReason)
	request.APIRequestID = presentString(usage.APIRequestID)
	request.Iterations = usage.Iterations
	if usage.NoUsage {
		return
	}
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
	RequestID string          `json:"requestId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type assistantMessage struct {
	requestID  string          // its entry's requestId, set where an entry's message is decoded
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
		CacheCreation *cacheCreation   `json:"cache_creation"`
		Iterations    []usageIteration `json:"iterations"`
	} `json:"usage"`
}

// cacheCreation is a usage's split of its cache writes by cache lifetime.
type cacheCreation struct {
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
}

// usageIteration is one entry of usage.iterations as Claude Code writes it.
type usageIteration struct {
	Type                     string         `json:"type"`
	Model                    *string        `json:"model"`
	InputTokens              *int64         `json:"input_tokens"`
	CacheReadInputTokens     *int64         `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64         `json:"cache_creation_input_tokens"`
	OutputTokens             *int64         `json:"output_tokens"`
	CacheCreation            *cacheCreation `json:"cache_creation"`
}

// messageIteration is the type of the iteration that is the message itself;
// every other type is kept (Iteration).
const messageIteration = "message"

// iterationsOf keeps the entries of usage.iterations that are not the message
// itself, each at its index in the array; nil when there is none.
func iterationsOf(entries []usageIteration) []Iteration {
	var kept []Iteration
	for i, entry := range entries {
		if entry.Type == messageIteration {
			continue
		}
		iteration := Iteration{
			Seq: i, Type: entry.Type, InputTokens: entry.InputTokens, CacheReadTokens: entry.CacheReadInputTokens,
			CacheCreationTokens: entry.CacheCreationInputTokens, OutputTokens: entry.OutputTokens,
		}
		if entry.Model != nil {
			iteration.Model = *entry.Model
		}
		if entry.CacheCreation != nil {
			iteration.CacheCreation5m = entry.CacheCreation.Ephemeral5mInputTokens
			iteration.CacheCreation1h = entry.CacheCreation.Ephemeral1hInputTokens
		}
		kept = append(kept, iteration)
	}
	return kept
}

// toolUse is both a content block type and the stop reason of a message that
// calls tools: a message that stops with it is never an agent's last.
const toolUse = "tool_use"

type contentBlock struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// BadLines is the error a transcript read returns beside everything it kept:
// Count lines, or parts of a line, that did not decode were each skipped
// alone, First the first of them, naming the path and its byte offset. The
// read's result is whole but for those (LinesSkipped).
type BadLines struct {
	Count int
	First error
}

func (e *BadLines) Error() string {
	return fmt.Sprintf("%d undecodable transcript line(s) skipped, the first: %v", e.Count, e.First)
}

func (e *BadLines) Unwrap() error { return e.First }

// err is e as an error, nil when no line was skipped.
func (e *BadLines) err() error {
	if e == nil {
		return nil
	}
	return e
}

// LinesSkipped reports whether err holds a *BadLines: the read went on past
// its bad lines, and what it returned is kept.
func LinesSkipped(err error) bool {
	var bad *BadLines
	return errors.As(err, &bad)
}

// readLines is the one line-decode door of a transcript read: it reads r,
// which starts at byte offset of the transcript at path, line by line and hands
// each line to decode. A line decode fails on (or on part of, keeping the rest)
// costs that line alone: it is counted in the *BadLines returned, nil when
// none. A final line with no newline that fails is a writer mid-append, never
// a bad line. skipFirst drops the first, partial line of a read starting
// inside the file. Only a failed read ends it early, as the error.
func readLines(r io.Reader, path string, offset int64, skipFirst bool, decode func(raw []byte) error) (*BadLines, error) {
	reader := bufio.NewReaderSize(r, 1<<16)
	var bad *BadLines
	for first := true; ; first = false {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return bad, fmt.Errorf("read transcript %s at byte %d: %w", path, offset, readErr)
		}
		partial := errors.Is(readErr, io.EOF)
		if !first || !skipFirst {
			if err := decode(raw); err != nil && !partial {
				if bad == nil {
					bad = &BadLines{First: fmt.Errorf("transcript %s at byte %d: %w", path, offset, err)}
				}
				bad.Count++
			}
		}
		offset += int64(len(raw))
		if partial {
			return bad, nil
		}
	}
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
// map; a file that cannot be read is an error naming the path. A malformed
// line holding a wanted id or found message costs that line alone: the map
// holds every other request, beside a *BadLines (readLines). A request whose
// message no entry gave usage is kept with NoUsage, one whose time is not
// RFC 3339 with TS 0, that timestamp counted as a bad line.
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
		bad, err := scan.read(file, path, start, size)
		if err != nil {
			return nil, err
		}
		if len(scan.owner) == len(wanted) || start == 0 {
			found = map[string]RequestUsage{}
			for id, message := range scan.owner {
				request := *scan.messages[message]
				request.TS = scan.at[id]
				found[id] = request
			}
			return found, bad.err()
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
func (scan *requestScan) read(file *os.File, path string, start, end int64) (*BadLines, error) {
	return readLines(io.NewSectionReader(file, start, end-start), path, start, start > 0, func(raw []byte) error {
		if !scan.mentioned(raw) {
			return nil
		}
		return scan.entry(raw)
	})
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
	message.requestID = entry.RequestID
	if request, known := scan.messages[message.ID]; known {
		foldEntry(request, &message)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return fmt.Errorf("assistant message %q: content is not a block list: %w", message.ID, err)
	}
	var timeErr error
	for _, block := range blocks {
		if block.Type != toolUse || !scan.wanted[block.ID] {
			continue
		}
		if _, done := scan.owner[block.ID]; done {
			continue
		}
		if _, known := scan.messages[message.ID]; !known {
			request := &RequestUsage{MessageID: message.ID, NoUsage: true}
			foldEntry(request, &message)
			scan.messages[message.ID] = request
		}
		scan.owner[block.ID] = message.ID
		at, err := entryTime(message.ID, entry.Timestamp)
		if err != nil {
			timeErr = err // the request stays, its time unknown
			continue
		}
		scan.at[block.ID] = at
	}
	return timeErr
}

// entryTime is the timestamp of the assistant entry carrying messageID, Unix
// ms UTC. One that does not parse is 0, unknown, and an error naming the
// message id and the timestamp's size, never its value nor the parse error
// quoting it: the value may be any text, and the error reaches faults.error.
func entryTime(messageID, timestamp string) (int64, error) {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("assistant message %q: timestamp of %d bytes is not RFC 3339", messageID, len(timestamp))
	}
	return at.UnixMilli(), nil
}

// foldEntry folds one entry of a message into its request: the model, stop
// reason and usage the entry carries replace the earlier entries' (the last
// entry's are final); the message id and the first entry's time stay.
func foldEntry(request *RequestUsage, message *assistantMessage) {
	if message.requestID != "" {
		request.APIRequestID = message.requestID
	}
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
	request.NoUsage = false
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
	request.Iterations = iterationsOf(usage.Iterations)
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
// id and the synthetic message Claude Code writes itself (syntheticModel) are
// not requests; a message no entry gives usage is one, NoUsage. A file that
// cannot be read is an error; a malformed line costs that line alone, as in
// ReadTranscript. Only lines naming an assistant entry or a prompt are decoded.
func ReadRequests(path string) (requests []TranscriptRequest, final bool, err error) {
	return ReadTurnRequests(path, "")
}

// ReadTaskNotices reads only task-id and non-empty status tags of task
// notifications; nothing of the body is kept. Times are in file order. A
// notice line that does not decode, or a notice whose time is not RFC 3339
// (the time is all a notice gives its reader), costs that line alone, beside
// a *BadLines (readLines).
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
	bad, err := readLines(file, path, 0, false, func(raw []byte) error {
		if !bytes.Contains(raw, []byte("task-notification")) {
			return nil
		}
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
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("malformed task notice entry: %w", err)
		}
		// The body is a string, or a content-block array (a queued prompt
		// carrying an image) read from its "text" blocks; a body of any other
		// shape is read as its raw text.
		var content json.RawMessage
		switch {
		case entry.Type == "user" && entry.Origin.Kind == "task-notification":
			content = entry.Message.Content
		case entry.Type == "attachment" && entry.Attachment.Type == "queued_command" && entry.Attachment.Origin.Kind == "task-notification":
			content = entry.Attachment.Prompt
		default:
			return nil
		}
		body, err := ResultText(content)
		if err != nil {
			body = string(content)
		}
		_, task, hasTask := strings.Cut(body, "<task-id>")
		agentID, _, taskClosed := strings.Cut(task, "</task-id>")
		_, status, hasStatus := strings.Cut(body, "<status>")
		status, _, statusClosed := strings.Cut(status, "</status>")
		if !hasTask || !taskClosed || agentID == "" || !hasStatus || !statusClosed || status == "" {
			return nil
		}
		at, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if err != nil {
			return fmt.Errorf("task notice: timestamp of %d bytes is not RFC 3339", len(entry.Timestamp))
		}
		notices[agentID] = append(notices[agentID], at.UnixMilli())
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read task notices: %w", err)
	}
	return notices, bad.err()
}

// ReadTurnRequests is ReadRequests for the turn of promptID: final also needs
// no user entry carrying promptID after that last assistant entry. Such an
// entry (the turn's prompt, a tool_result, or the prompt that woke an agent
// whose earlier turn ended) is still to be answered, even when the entry
// before it ended an earlier turn. A promptID no entry carries, or "", is
// ReadRequests.
func ReadTurnRequests(path, promptID string) (requests []TranscriptRequest, final bool, err error) {
	read, err := ReadTranscript(path, promptID)
	return read.Requests, read.Final, err
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
// durations and the last cost-state. A line that does not decode, or the part
// of one (a content that is no block list, a timestamp that is not RFC 3339),
// costs that line or part alone: read holds everything else, beside a
// *BadLines (readLines). Only lines naming an assistant entry, a prompt or a
// mark are decoded.
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
	scan := transcriptRequests{index: map[string]int{}, turn: promptID, seen: map[string]bool{}}
	bad, err := readLines(file, path, 0, false, func(raw []byte) error {
		if bytes.Contains(raw, []byte(`"assistant"`)) || bytes.Contains(raw, []byte(`"promptId"`)) ||
			bytes.Contains(raw, []byte(`"compact_boundary"`)) || bytes.Contains(raw, []byte(`"stop_hook_summary"`)) ||
			bytes.Contains(raw, []byte(`"turn_duration"`)) || bytes.Contains(raw, []byte(`"cost-state"`)) {
			return scan.entry(raw)
		}
		return nil
	})
	if err != nil {
		return TranscriptRead{}, err
	}
	read.Requests = scan.requests
	read.Final = scan.final
	read.Marks = scan.marks
	return read, bad.err()
}

// transcriptRequests is one ReadRequests pass.
type transcriptRequests struct {
	requests []TranscriptRequest
	index    map[string]int // message id -> its position in requests
	prompt   string         // the latest user entry's promptId
	turn     string         // the promptId whose answer final waits for; "" for none
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
	message.requestID = entry.RequestID
	if message.ID == "" || message.Model == syntheticModel {
		return nil
	}
	// What fails below costs only its part: the request stays, its time
	// unknown (0) or without this entry's tool calls, and the line counts as bad.
	var lost []error
	at, timeErr := entryTime(message.ID, entry.Timestamp)
	i, known := scan.index[message.ID]
	if !known {
		if timeErr != nil {
			lost = append(lost, timeErr)
		}
		i = len(scan.requests)
		scan.index[message.ID] = i
		scan.requests = append(scan.requests, TranscriptRequest{
			RequestUsage: RequestUsage{MessageID: message.ID, TS: at, NoUsage: true},
			PromptID:     scan.prompt,
			Cwd:          entry.Cwd,
		})
	}
	request := &scan.requests[i]
	foldEntry(&request.RequestUsage, &message)
	if message.StopReason != nil && *message.StopReason != "" && *message.StopReason != toolUse {
		if timeErr != nil && known {
			lost = append(lost, timeErr)
		}
		request.EndTS = at
	}
	var blocks []toolUseBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		lost = append(lost, fmt.Errorf("assistant message %q: content is not a block list: %w", message.ID, err))
	}
	for _, block := range blocks {
		if block.Type == toolUse && block.ID != "" && !slices.Contains(request.ToolUseIDs, block.ID) {
			request.ToolUseIDs = append(request.ToolUseIDs, block.ID)
			request.ToolUses = append(request.ToolUses, ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	scan.final = message.StopReason != nil && *message.StopReason != toolUse
	return errors.Join(lost...)
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
}

// ReadAgentTotals sums the sub-agent transcript at path. One model message is
// split across several entries sharing its id, each repeating its usage, so a
// message's usage counts once (its last entry's, the final count). A file that
// cannot be read is an error naming the path; a malformed line or an assistant
// message with no id costs that line alone, and a content that is no block list
// only that entry's tool uses, beside a *BadLines (readLines); bad lines with
// no message read at all are a failed read, never zero totals. promptID is the
// turn whose answer Final waits for, as in ReadTurnRequests; "" for none.
func ReadAgentTotals(path, promptID string) (totals AgentTotals, err error) {
	file, err := os.Open(path)
	if err != nil {
		return totals, fmt.Errorf("read agent transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close agent transcript %s: %w", path, closeErr))
		}
	}()
	usage := map[string]int64{}
	var order []string
	toolUses := map[string]bool{}
	bad, err := readLines(file, path, 0, false, func(raw []byte) error {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		message, prompt, err := agentMessage(raw)
		switch {
		case err != nil:
			return err
		case promptID != "" && prompt == promptID:
			totals.Final = false // the turn's answer to this entry is still to come
		case message != nil:
			if _, seen := usage[message.ID]; !seen {
				order = append(order, message.ID)
			}
			if message.Usage != nil {
				u := message.Usage
				usage[message.ID] = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens
			}
			if message.Model != "" && message.Model != syntheticModel {
				totals.Model = message.Model
			}
			totals.Final = message.StopReason != nil && *message.StopReason != toolUse
			var blocks []contentBlock
			if err := json.Unmarshal(message.Content, &blocks); err != nil {
				return fmt.Errorf("message %q: content is not a block list: %w", message.ID, err)
			}
			for _, block := range blocks {
				if block.Type == toolUse && block.ID != "" {
					toolUses[block.ID] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return AgentTotals{}, fmt.Errorf("read agent transcript: %w", err)
	}
	for _, id := range order {
		totals.TotalTokens += usage[id]
	}
	if bad != nil && len(order) == 0 {
		// Bad lines and no message read: zero totals would read as an agent
		// that used nothing, so this read failed rather than skipped.
		return AgentTotals{}, fmt.Errorf("read agent transcript: no message read past %d bad line(s), the first: %w", bad.Count, bad.First)
	}
	totals.ToolUses = int64(len(toolUses))
	return totals, bad.err()
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
// while it holds no message. A file that cannot be read is an error naming the
// path; a malformed line costs that line alone, the last good message read
// beside a *BadLines (lastMessage).
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
		last, bad, err := lastMessage(file, path, start, size)
		if err != nil {
			return TurnEnd{}, err
		}
		if last != nil || start == 0 {
			end = turnEndOf(last)
			if end.Event == "" {
				return TurnEnd{}, bad.err()
			}
			if at, parseErr := time.Parse(time.RFC3339, last.Timestamp); parseErr == nil {
				end.TS = at.UnixMilli()
			}
			return end, bad.err()
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
	var message turnMessage
	if json.Unmarshal(entry.Message, &message) != nil || message.Model == syntheticModel || !turnEndReasons[message.StopReason] {
		return TurnEnd{}
	}
	return TurnEnd{Event: EventStop}
}

// turnMessage is the part of an assistant message a turn end reads.
type turnMessage struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
}

// lastMessage is the last user or assistant entry in [start, end) of the
// transcript, nil when there is none; a start inside the file drops the
// first, partial line. A line that does not decode, or an assistant entry
// whose message is no object, costs that line alone (readLines): the last
// good message stands, beside the *BadLines.
func lastMessage(file *os.File, path string, start, end int64) (*messageEntry, *BadLines, error) {
	var last *messageEntry
	bad, err := readLines(io.NewSectionReader(file, start, end-start), path, start, start > 0, func(raw []byte) error {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		var entry messageEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("malformed entry: %w", err)
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			return nil
		}
		if entry.Type == "assistant" {
			var message turnMessage
			if err := json.Unmarshal(entry.Message, &message); err != nil {
				return fmt.Errorf("malformed assistant message: %w", err)
			}
		}
		last = &entry
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return last, bad, nil
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
// 0, unknown, and an error naming only its size, as entryTime does.
func markTime(timestamp string) (int64, error) {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("mark entry: timestamp of %d bytes is not RFC 3339", len(timestamp))
	}
	return at.UnixMilli(), nil
}

// system reads one system entry when it is a mark: a compaction, a Stop-hook
// summary or a turn duration; any other subtype is no mark. An entry with no
// uuid is keyed by its line (lineKey). A mark whose timestamp is not RFC 3339
// is kept with TS 0, unknown, its error returned for the line's count; one
// that does not decode is dropped alone, its error returned.
func (scan *transcriptRequests) system(raw []byte, uuid, subtype, timestamp string) error {
	if uuid == "" {
		uuid = lineKey(raw)
	}
	if scan.seen[uuid] {
		return nil
	}
	var timeErr error
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
		timeErr = err
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
		timeErr = err
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
		timeErr = err
		scan.marks.TurnDurations = append(scan.marks.TurnDurations, TurnDuration{
			EntryID: uuid, TS: at, PromptID: scan.prompt, DurationMS: wholeNumber(entry.DurationMS),
			MessageCount: wholeNumber(entry.MessageCount), BackgroundAgents: wholeNumber(entry.BackgroundAgents),
		})
	default:
		return nil
	}
	scan.seen[uuid] = true
	return timeErr
}

// lineKey is the entry id of a mark line that carries no uuid: "line#" and the
// first 16 hex digits of the line's SHA-256, so a repeated line is one mark.
func lineKey(raw []byte) string {
	sum := sha256.Sum256(bytes.TrimSpace(raw))
	return "line#" + hex.EncodeToString(sum[:])[:16]
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
