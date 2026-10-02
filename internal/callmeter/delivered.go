package callmeter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// DeliveredBytes is the byte length of a tool result's content as the model
// received it: a string's UTF-8 bytes, or the summed text of a content-block
// array's "text" blocks (a non-text block contributes nothing — the model
// never read it as text). Absent, empty or null content is 0 bytes. Any other
// JSON shape (an object, a number, a boolean, malformed JSON) cannot be
// measured this way and is an error naming the shape, never a silent 0 or a
// guess at the raw encoding's length.
func DeliveredBytes(raw json.RawMessage) (int64, error) {
	text, err := ResultText(raw)
	return int64(len(text)), err
}

// ResultText is a tool result's content as the model read it, under
// DeliveredBytes's rules: a string as it is, a content-block array's "text"
// blocks joined, absent, empty or null content "", any other shape an error.
// It is only measured and classified, never stored.
func ResultText(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		return "", nil
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return "", fmt.Errorf("decode tool result text: %w", err)
		}
		return text, nil
	case trimmed[0] == '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &blocks); err != nil {
			return "", fmt.Errorf("decode tool result content blocks: %w", err)
		}
		var text strings.Builder
		for _, block := range blocks {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		return text.String(), nil
	default:
		return "", fmt.Errorf("unmeasurable tool result shape: %s", shapeName(trimmed))
	}
}

// realResponse is the part of a tool_response RealBytes measures.
type realResponse struct {
	Stdout              *string         `json:"stdout"`
	PersistedOutputSize *float64        `json:"persistedOutputSize"`
	Content             json.RawMessage `json:"content"`
	File                *struct {
		Content *string `json:"content"`
	} `json:"file"`
}

// RealBytes is the real output size of a tool's response: the size PostToolUse
// stores in bytes_real, and the transcript's toolUseResult is the same object.
// A response that is not an object is measured as DeliveredBytes measures it;
// an object is the first of persistedOutputSize, the length of stdout, of
// file.content, DeliveredBytes of content, else the length of the response
// JSON itself. A response that does not decode, or whose content cannot be
// measured, is an error, never a guess.
func RealBytes(response json.RawMessage) (*int64, error) {
	trimmed := bytes.TrimSpace(response)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		size, err := DeliveredBytes(trimmed)
		if err != nil {
			return nil, err
		}
		return Ptr(size), nil
	}
	var decoded realResponse
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil, fmt.Errorf("decode tool_response: %w", err)
	}
	switch {
	case decoded.PersistedOutputSize != nil:
		return Ptr(int64(*decoded.PersistedOutputSize)), nil
	case decoded.Stdout != nil:
		return Ptr(int64(len(*decoded.Stdout))), nil
	case decoded.File != nil && decoded.File.Content != nil:
		return Ptr(int64(len(*decoded.File.Content))), nil
	case len(decoded.Content) > 0 && !bytes.Equal(decoded.Content, []byte("null")):
		size, err := DeliveredBytes(decoded.Content)
		if err != nil {
			return nil, err
		}
		return Ptr(size), nil
	default:
		return Ptr(int64(len(trimmed))), nil
	}
}

// The outcome of a call Claude Code never ran, stored in its error column: a
// fixed label, never the result's own text — a hook's or a prompt's reason is
// content.
const (
	OutcomeDeniedByHook       = "denied by a PreToolUse hook"
	OutcomeDeniedByPermission = "denied by permission"
	OutcomeRejectedByUser     = "rejected or interrupted by the user"
	OutcomeRefused            = "refused by Claude Code"
)

// toolUseError opens the result of a call the harness refused to run (a
// blocked command, a tool the session lacks, an Edit whose old_string is not
// in the file): its outcome is OutcomeRefused, never the error's own text.
const toolUseError = "<tool_use_error>"

// outcomeOpenings are the openings of the result text Claude Code writes in
// place of a call it never ran, each seen live: a PreToolUse deny hook and an
// ungranted Write under `claude -p`, an Escape mid-call in a sub-agent.
var outcomeOpenings = []struct{ prefix, outcome string }{
	{"PreToolUse:", OutcomeDeniedByHook},
	{"Claude requested permissions to ", OutcomeDeniedByPermission},
	{"The user doesn't want to ", OutcomeRejectedByUser},
}

// ResultOutcome classifies a tool result by the harness's own opening text:
// a refused, denied or user-rejected call is its outcome label and failed,
// any other result is "" and not failed. The text alone never says a command
// failed or was killed: `Exit code N` is also what a command may print.
func ResultOutcome(text string) (outcome string, failed bool) {
	if strings.HasPrefix(text, toolUseError) {
		return OutcomeRefused, true
	}
	for _, opening := range outcomeOpenings {
		if strings.HasPrefix(text, opening.prefix) {
			return opening.outcome, true
		}
	}
	return "", false
}

// Result is what a transcript's tool_result says of one call: the bytes the
// model received, whether it failed (Claude Code's is_error, or a denial by
// its text), its stored error, and whether Claude Code refused the call.
// A failed call's Outcome is never "": a refusal's label, else the result
// text as SanitizeError stores it (`Exit code N` for a failed or killed
// command, ErrorNotStored for any other failure). The transcript carries no
// interrupt flag for a killed call (its toolUseResult is the error string),
// so a result never says whether the call was interrupted.
type Result struct {
	Bytes   int64
	Failed  bool
	Outcome string
	// Refused: the result is Claude Code's own text in place of a run (a
	// harness refusal, a denial, a rejection), so that text is the whole output.
	Refused bool
	// Real is the real output size (RealBytes) of the line's toolUseResult;
	// nil for a failed call (its text is its whole output), and when the line
	// carries none or holds more than one tool_result.
	Real *int64
}

// SettledCall is the calls row a transcript's result settles: the delivered
// size, failed and the outcome label (nil when the result has none), and for a
// failed call (a refusal included) the real size too: a failure's text is its
// whole output, the size PostToolUseFailure stores, and a refusal's is Claude
// Code's own text.
// It holds sizes and labels only, never the result's text, and no ts: the
// caller sets one only for a call stored before every hook set it.
func SettledCall(toolUseID string, r Result) Call {
	call := Call{
		ToolUseID:      toolUseID,
		BytesDelivered: Ptr(r.Bytes),
		Failed:         Ptr(r.Failed),
		Error:          presentString(r.Outcome),
	}
	if r.Failed {
		call.BytesReal = Ptr(r.Bytes)
	}
	return call
}

// FindResults reads a transcript for the tool_result of each wanted call; a
// call with no result on disk is absent from the map. A line is decoded only
// when it names a tool_result and a wanted id, and such a line that does not
// decode is an error at its line number. A missing transcript is an error
// wrapping fs.ErrNotExist.
func FindResults(path string, ids []string) (map[string]Result, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript %s: %w", path, err)
	}
	found := map[string]Result{}
	reader := bufio.NewReader(file)
	for n := 1; ; n++ {
		line, readErr := reader.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"tool_result"`)) && namesAny(line, ids) {
			if err := resultsOf(line, ids, found); err != nil {
				return nil, errors.Join(fmt.Errorf("transcript %s line %d: %w", path, n, err), file.Close())
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, errors.Join(fmt.Errorf("read transcript %s: %w", path, readErr), file.Close())
		}
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close transcript %s: %w", path, err)
	}
	return found, nil
}

func namesAny(line []byte, ids []string) bool {
	for _, id := range ids {
		if bytes.Contains(line, []byte(id)) {
			return true
		}
	}
	return false
}

// resultsOf adds the line's tool_result blocks for the wanted ids to found; a
// message whose content is a string holds no tool_result.
func resultsOf(line []byte, ids []string, found map[string]Result) error {
	var entry struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		ToolUseResult json.RawMessage `json:"toolUseResult"`
	}
	if err := json.Unmarshal(line, &entry); err != nil {
		return fmt.Errorf("decode transcript line: %w", err)
	}
	content := bytes.TrimSpace(entry.Message.Content)
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return fmt.Errorf("decode message content: %w", err)
	}
	// Claude Code writes one tool_result per line, so the line's toolUseResult
	// is that call's; a line holding several results has no way to say whose.
	results := 0
	for _, block := range blocks {
		if block.Type == "tool_result" {
			results++
		}
	}
	useResult := bytes.TrimSpace(entry.ToolUseResult)
	hasUseResult := results == 1 && len(useResult) > 0 && !bytes.Equal(useResult, []byte("null"))
	for _, block := range blocks {
		if block.Type != "tool_result" || !slices.Contains(ids, block.ToolUseID) {
			continue
		}
		text, err := ResultText(block.Content)
		if err != nil {
			return fmt.Errorf("tool_result %s: %w", block.ToolUseID, err)
		}
		outcome, refused := ResultOutcome(text)
		failed := block.IsError || refused
		if failed && outcome == "" {
			outcome = SanitizeError(text)
		}
		result := Result{Bytes: int64(len(text)), Failed: failed, Outcome: outcome, Refused: refused}
		if hasUseResult && !failed { // a failed call's whole output is its text
			if result.Real, err = RealBytes(useResult); err != nil {
				return fmt.Errorf("tool_result %s: toolUseResult: %w", block.ToolUseID, err)
			}
		}
		found[block.ToolUseID] = result
	}
	return nil
}

// shapeName names the JSON shape at trimmed's first byte, for an error that
// tells a reader what it could not measure.
func shapeName(trimmed []byte) string {
	switch trimmed[0] {
	case '{':
		return "object"
	case 't', 'f':
		return "boolean"
	default:
		if trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9') {
			return "number"
		}
		return "unrecognized JSON"
	}
}
