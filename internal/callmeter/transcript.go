package callmeter

import (
	"bufio"
	"bytes"
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
		CacheCreation            *struct {
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
	PromptID   string   // the promptId of the last user entry before it; "" when none carries one
	ToolUseIDs []string // its tool_use blocks, in order
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

// ReadTurnRequests is ReadRequests for the turn of promptID: final also needs
// no user entry carrying promptID after that last assistant entry. Such an
// entry (the turn's prompt, a tool_result, or the prompt that woke an agent
// whose earlier turn ended) is still to be answered, even when the entry
// before it ended an earlier turn. A promptID no entry carries, or "", is
// ReadRequests.
func ReadTurnRequests(path, promptID string) (requests []TranscriptRequest, final bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("read transcript: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close transcript %s: %w", path, closeErr))
		}
	}()
	scan := transcriptRequests{index: map[string]int{}, used: map[string]bool{}, turn: promptID}
	reader := bufio.NewReaderSize(file, 1<<16)
	var offset int64
	for {
		raw, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, false, fmt.Errorf("read transcript %s at byte %d: %w", path, offset, readErr)
		}
		partial := errors.Is(readErr, io.EOF)
		if bytes.Contains(raw, []byte(`"assistant"`)) || bytes.Contains(raw, []byte(`"promptId"`)) {
			if err := scan.entry(raw); err != nil && !partial {
				return nil, false, fmt.Errorf("transcript %s at byte %d: %w", path, offset, err)
			}
		}
		offset += int64(len(raw))
		if partial {
			break
		}
	}
	for _, request := range scan.requests {
		if scan.used[request.MessageID] {
			requests = append(requests, request)
		}
	}
	return requests, scan.final, nil
}

// transcriptRequests is one ReadRequests pass.
type transcriptRequests struct {
	requests []TranscriptRequest
	index    map[string]int  // message id -> its position in requests
	used     map[string]bool // message ids some entry gave usage
	prompt   string          // the latest user entry's promptId
	turn     string          // the promptId whose answer final waits for; "" for none
	final    bool
}

func (scan *transcriptRequests) entry(raw []byte) error {
	var entry struct {
		transcriptEntry
		PromptID string `json:"promptId"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fmt.Errorf("malformed entry: %w", err)
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
	var blocks []contentBlock
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
		})
	}
	request := &scan.requests[i]
	foldEntry(&request.RequestUsage, &message)
	if message.Usage != nil {
		scan.used[message.ID] = true
	}
	for _, block := range blocks {
		if block.Type == toolUse && block.ID != "" && !slices.Contains(request.ToolUseIDs, block.ID) {
			request.ToolUseIDs = append(request.ToolUseIDs, block.ID)
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
// text leaves the transcript.
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
