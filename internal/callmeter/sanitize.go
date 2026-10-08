package callmeter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// LongField is the size above which a field not on the keep list is stored as
// its byte length.
const LongField = 4096

// droppedFields are never stored: each becomes {name}_bytes.
var droppedFields = map[string]bool{
	"content":    true,
	"old_string": true,
	"new_string": true,
	"edits":      true,
	"new_source": true,
	"prompt":     true,
}

// keptFields are stored whatever their size when their value is a string and
// the tool is not an mcp__ tool: file paths and search patterns. An object or
// array under one is sized.
var keptFields = map[string]bool{
	"file_path":     true,
	"notebook_path": true,
	"path":          true,
	"pattern":       true,
	"glob":          true,
}

// labelFields are short labels, identifiers and enumerations of Claude Code's
// built-in tools, stored as strings up to LongField bytes: an agent type, a
// model, a skill name, a task, shell or notebook cell id, a fetched URL (its
// userinfo, query and fragment cut, see cutURL), a Grep output mode or file
// type, a notebook cell type or edit mode, a message's addressee. Model-written free
// text is never a label: a description or a search query is sized. An MCP
// tool's keys mean what its server says, so no label applies to it.
var labelFields = map[string]bool{
	"subagent_type": true, "model": true, "skill": true, "task_id": true,
	"shell_id": true, "url": true, "output_mode": true, "type": true,
	"cell_id": true, "cell_type": true, "edit_mode": true, "to": true,
}

// shellTools are the tools whose command field is a shell command line.
var shellTools = map[string]bool{"Bash": true, "Monitor": true}

// mcpToolPrefix starts the name of every MCP server's tool.
const mcpToolPrefix = "mcp__"

// SanitizeInput is the stored form of a tool input: a JSON object keeping
// file paths and search patterns as strings, the labelFields strings up to
// LongField bytes (a url without its userinfo, query and fragment), and every
// number, boolean and null (the Read range, a timeout). An mcp__ tool keeps no string
// at all: every string field, kept and label fields included, is sized, and
// only its numbers, booleans and null stay. A Bash or
// Monitor command is kept with every heredoc body cut out
// (cmdparse.RedactHeredocs) and the bytes cut under heredoc_bytes, then every
// commit message and every operand of an echo or printf writing to a file
// replaced by cmdparse.CutPlaceholder (cmdparse.CutContent) and the bytes cut
// under operand_bytes; a command that cannot be cut safely is stored only as
// command_bytes. Every other
// field (a dropped field, free text, an object or array, a label over
// LongField) is replaced by its byte length under {name}_bytes. A string's
// length is its UTF-8 bytes; any other value's is its JSON encoding's.
func SanitizeInput(tool string, raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("sanitize %s input: not a JSON object: %w", tool, err)
	}
	if fields == nil {
		return "", fmt.Errorf("sanitize %s input: input is null", tool)
	}
	mcp := strings.HasPrefix(tool, mcpToolPrefix)
	out := make(map[string]any, len(fields))
	for name, value := range fields {
		size, err := valueBytes(value)
		if err != nil {
			return "", fmt.Errorf("sanitize %s input field %q: %w", tool, name, err)
		}
		kind := jsonKind(value)
		switch {
		case droppedFields[name]:
			out[name+"_bytes"] = size
		case name == "command" && shellTools[tool] && kind == '"':
			if err := putCommand(out, value, size); err != nil {
				return "", fmt.Errorf("sanitize %s input: %w", tool, err)
			}
		case kind != '"' && kind != '{' && kind != '[':
			out[name] = value
		case mcp:
			out[name+"_bytes"] = size
		case keptFields[name] && kind == '"':
			out[name] = value
		case name == "url" && kind == '"' && size <= LongField:
			url, err := cutURL(value)
			if err != nil {
				return "", fmt.Errorf("sanitize %s input field %q: %w", tool, name, err)
			}
			out[name] = url
		case labelFields[name] && kind == '"' && size <= LongField:
			out[name] = value
		default:
			out[name+"_bytes"] = size
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("sanitize %s input: encode: %w", tool, err)
	}
	return string(encoded), nil
}

// cutURL is a url string without its userinfo, query and fragment, each of
// which may carry a credential or a token: everything from the first '?' or
// '#' is cut, then the `user:password@` before the host (through the last '@'
// of the authority, the part after `scheme://` or a leading `//` up to the
// next '/'). A url with none of them is returned as it arrived, so a stored
// input sanitized again stays the same bytes.
func cutURL(value json.RawMessage) (json.RawMessage, error) {
	var url string
	if err := json.Unmarshal(value, &url); err != nil {
		return nil, fmt.Errorf("decode the url: %w", err)
	}
	cut := url
	if end := strings.IndexAny(cut, "?#"); end >= 0 {
		cut = cut[:end]
	}
	if start := authorityStart(cut); start >= 0 {
		authority := cut[start:]
		if end := strings.IndexByte(authority, '/'); end >= 0 {
			authority = authority[:end]
		}
		if at := strings.LastIndexByte(authority, '@'); at >= 0 {
			cut = cut[:start] + cut[start+at+1:]
		}
	}
	if cut == url {
		return value, nil
	}
	return json.Marshal(cut)
}

// authorityStart is the index where a url's authority begins: after the
// `//` that follows its scheme's ':' or opens the url, else -1 (a url with no
// authority, whose '@' is part of its path).
func authorityStart(url string) int {
	if strings.HasPrefix(url, "//") {
		return 2
	}
	colon := strings.IndexByte(url, ':')
	if colon <= 0 || strings.ContainsAny(url[:colon], "/@") || !strings.HasPrefix(url[colon+1:], "//") {
		return -1
	}
	return colon + 3
}

// putCommand stores a shell command with its heredoc bodies cut out
// (heredoc_bytes) and then its content operands (operand_bytes: a commit
// message, the text an echo or printf writes to a file), or only its size
// when either cut cannot be made safely. Both cuts leave their own output
// unchanged, so a stored input sanitized again stays the same bytes.
func putCommand(out map[string]any, value json.RawMessage, size int) error {
	var command string
	if err := json.Unmarshal(value, &command); err != nil {
		return fmt.Errorf("decode the command: %w", err)
	}
	redacted, heredoc, ok := cmdparse.RedactHeredocs(command)
	if !ok {
		out["command_bytes"] = size
		return nil
	}
	redacted, operands, ok := cmdparse.CutContent(redacted)
	if !ok {
		out["command_bytes"] = size
		return nil
	}
	if heredoc == 0 && operands == 0 {
		out["command"] = value // as it arrived: a stored input sanitized again stays the same bytes
		return nil
	}
	out["command"] = redacted
	if heredoc > 0 {
		out["heredoc_bytes"] = heredoc
	}
	if operands > 0 {
		out["operand_bytes"] = operands
	}
	return nil
}

// jsonKind is the first byte of a JSON value: '"', '{', '[', or a scalar's.
func jsonKind(value json.RawMessage) byte {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return 0
	}
	return trimmed[0]
}

// ErrorNotStored is what calls.error holds for a tool error whose text is
// not stored: the text is the tool's output, and its size is bytes_real.
const ErrorNotStored = "error text not stored"

// exitCodeLine is the first line of a failed Bash call's error.
var exitCodeLine = regexp.MustCompile(`^Exit code -?[0-9]+$`)

// SanitizeError is the stored form of a call's error: an outcome label as it
// is, a failed command's first line when it is only `Exit code N`, and
// ErrorNotStored for any other text, which is the tool's output.
func SanitizeError(text string) string {
	switch text {
	case OutcomeDeniedByHook, OutcomeDeniedByPermission, OutcomeRejectedByUser, OutcomeRefused, ErrorNotStored:
		return text
	}
	first, _, _ := strings.Cut(text, "\n")
	if exitCodeLine.MatchString(first) {
		return first
	}
	return ErrorNotStored
}

// NoProgramParsed stands in a parse fault for the program of a part that has
// none: the whole command failed to parse, so no program was read from it.
const NoProgramParsed = "no program parsed"

// ParseFault is the error a parse fault on part seq stores: the part's
// program, or NoProgramParsed, then ErrorNotStored and the size of the
// parser's message, never the message, which quotes the command it read.
func ParseFault(seq int, program, message string) string {
	if program == "" {
		program = NoProgramParsed
	}
	return fmt.Sprintf("part %d (%s): %s (%d bytes)", seq, program, ErrorNotStored, len(message))
}

// parseFaultLine is a stored parse fault: `part N (program): message`.
var parseFaultLine = regexp.MustCompile(`(?s)^part ([0-9]+) \((.*?)\): (.*)$`)

// sizedText is free text already stored as ErrorNotStored and its size.
var sizedText = regexp.MustCompile(`^` + regexp.QuoteMeta(ErrorNotStored) + ` \([0-9]+ bytes\)$`)

// sanitizeFault is the stored form of a fault's error, as Redact rewrites it:
// a parse fault as ParseFault writes it, and a parse fault of any other shape
// as ErrorNotStored and its size. Every other stage's error is kept as it is:
// it is written by callmeter or its wrapper, never quoted from a command.
func sanitizeFault(stage, text string) string {
	if stage != StageParse || sizedText.MatchString(text) {
		return text
	}
	m := parseFaultLine.FindStringSubmatch(text)
	if m == nil {
		return fmt.Sprintf("%s (%d bytes)", ErrorNotStored, len(text))
	}
	if m[2] != "" && sizedText.MatchString(m[3]) {
		return text
	}
	seq, err := strconv.Atoi(m[1])
	if err != nil {
		return fmt.Sprintf("%s (%d bytes)", ErrorNotStored, len(text))
	}
	return ParseFault(seq, m[2], m[3])
}

func valueBytes(value json.RawMessage) (int, error) {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0, fmt.Errorf("decode string: %w", err)
		}
		return len(s), nil
	}
	return len(trimmed), nil
}

// detailKeptStrings are the keys whose string values a sanitized detail keeps:
// identifiers, enumerations, model names and paths, never free text.
var detailKeptStrings = map[string]bool{
	"id": true, "type": true, "status": true, "source": true, "trigger": true,
	"level": true, "mode": true, "behavior": true, "destination": true,
	"notification_type": true, "load_reason": true, "memory_type": true, "command_name": true,
	"command_source": true, "expansion_type": true, "task_id": true, "tool_name": true, "tool_use_id": true,
	"model": true, "file_path": true, "trigger_file_path": true, "schedule": true, "cron": true,
}

// detailLabels are the keys whose string value may be free text (an error
// body, a reason in words), kept only when it is a label of its event: per
// event, per key, the test a label passes. A StopFailure error is a label when
// it has the shape of an API error kind (apiErrorKind), the one gate the
// transcript path uses too (APIErrorKind): Claude Code documents rate_limit,
// overloaded, authentication_failed, oauth_org_not_allowed, account_on_hold,
// billing_error, cloud_credential_error, invalid_request, server_error,
// max_output_tokens, model_not_found and unknown, and a kind it adds later
// passes as well. A SessionEnd reason is a label only on its list. Any other
// value, and the key under any other event, becomes {key}_bytes.
var detailLabels = map[string]map[string]func(string) bool{
	EventStopFailure: {"error": apiErrorKind.MatchString},
	EventSessionEnd: {"reason": labelSet(
		"clear", "resume", "logout", "prompt_input_exit", "bypass_permissions_disabled", "other",
	)},
}

// labelSet is the label test of a closed list of labels.
func labelSet(labels ...string) func(string) bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		set[label] = true
	}
	return func(s string) bool { return set[s] }
}

// isLabel reports whether s is a label of event under key (detailLabels).
func isLabel(event, key, s string) bool {
	test := detailLabels[event][key]
	return test != nil && test(s)
}

// keptString reports whether a detail keeps the string value s under key in a
// payload of event: a key of detailKeptStrings, or a label of detailLabels.
func keptString(event, key, s string) bool {
	return detailKeptStrings[key] || isLabel(event, key, s)
}

// detailHead is the hook event a payload names in hook_event_name, empty for
// an array or a payload naming none (its labels are then never kept), and the
// tool it names in tool_name.
func detailHead(raw json.RawMessage) (event, tool string) {
	var head struct {
		Event string `json:"hook_event_name"`
		Tool  string `json:"tool_name"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return "", ""
	}
	return head.Event, head.Tool
}

// SanitizeDetail is the stored form of a hook payload's leftover keys (an
// events row's detail) or of a JSON array it carries (background_tasks,
// session_crons): objects and arrays keep their shape and key order, numbers,
// booleans and null stay, and a string is kept only under a key of
// detailKeptStrings or as a label of detailLabels for the event raw names in
// hook_event_name. Any other string in an object becomes {key}_bytes, and in
// an array its byte count (UTF-8 bytes). The tool_input of a payload whose
// tool_name is an mcp__ tool keeps no string at all: its keys mean what its
// server says, as in SanitizeInput. omit names the top-level keys left out.
// raw must be a JSON object or array.
func SanitizeDetail(raw json.RawMessage, omit map[string]bool) (string, error) {
	event, tool := detailHead(raw)
	return sanitizeEventDetail(event, tool, raw, omit)
}

// sanitizeEventDetail is SanitizeDetail for a payload of event naming tool,
// which a stored detail no longer names.
func sanitizeEventDetail(event, tool string, raw json.RawMessage, omit map[string]bool) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", fmt.Errorf("sanitize detail: not a JSON object or array (%d bytes)", len(trimmed))
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("sanitize detail: invalid JSON (%d bytes)", len(trimmed))
	}
	var out bytes.Buffer
	var sizeAll map[string]bool // the top-level keys whose strings are all sized
	if strings.HasPrefix(tool, mcpToolPrefix) {
		sizeAll = map[string]bool{"tool_input": true}
	}
	if err := sanitizeValue(&out, trimmed, event, "", false, false, omit, sizeAll); err != nil {
		return "", fmt.Errorf("sanitize detail: %w", err)
	}
	return out.String(), nil
}

// sanitizeValue writes the sanitized form of value, found under key (inside an
// array when inArray) in a payload of event; sized keeps no string of it. omit
// applies to value's own keys, and so does sizeAll, the keys under which
// every string is sized.
func sanitizeValue(out *bytes.Buffer, value json.RawMessage, event, key string, inArray, sized bool, omit, sizeAll map[string]bool) error {
	value = bytes.TrimSpace(value)
	switch value[0] {
	case '{':
		return sanitizeObject(out, value, event, sized, omit, sizeAll)
	case '[':
		return sanitizeArray(out, value, event, key, sized)
	case '"':
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return fmt.Errorf("decode the %q string: %w", key, err)
		}
		if !sized && keptString(event, key, s) {
			encoded, err := json.Marshal(s)
			if err != nil {
				return fmt.Errorf("encode the %q string: %w", key, err)
			}
			out.Write(encoded)
			return nil
		}
		if !inArray {
			return fmt.Errorf("the %q string reached the value writer unmeasured", key)
		}
		fmt.Fprint(out, len(s))
		return nil
	default:
		out.Write(value) // a number, true, false or null
		return nil
	}
}

func sanitizeObject(out *bytes.Buffer, value json.RawMessage, event string, sized bool, omit, sizeAll map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(value))
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("open an object: %w", err)
	}
	out.WriteByte('{')
	first := true
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return fmt.Errorf("read an object key: %w", err)
		}
		name, _ := token.(string)
		var field json.RawMessage
		if err := dec.Decode(&field); err != nil {
			return fmt.Errorf("read the %q value: %w", name, err)
		}
		if omit[name] {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		fieldSized := sized || sizeAll[name]
		if field[0] == '"' {
			var s string
			if err := json.Unmarshal(field, &s); err != nil {
				return fmt.Errorf("measure the %q string: %w", name, err)
			}
			if fieldSized || !keptString(event, name, s) {
				writeKey(out, name+"_bytes")
				fmt.Fprint(out, len(s))
				continue
			}
		}
		writeKey(out, name)
		if err := sanitizeValue(out, field, event, name, false, fieldSized, nil, nil); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

func sanitizeArray(out *bytes.Buffer, value json.RawMessage, event, key string, sized bool) error {
	dec := json.NewDecoder(bytes.NewReader(value))
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("open the %q array: %w", key, err)
	}
	out.WriteByte('[')
	for i := 0; dec.More(); i++ {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return fmt.Errorf("read element %d of the %q array: %w", i, key, err)
		}
		if i > 0 {
			out.WriteByte(',')
		}
		if err := sanitizeValue(out, element, event, key, true, sized, nil, nil); err != nil {
			return err
		}
	}
	out.WriteByte(']')
	return nil
}

func writeKey(out *bytes.Buffer, name string) {
	encoded, _ := json.Marshal(name) // a Go string always encodes
	out.Write(encoded)
	out.WriteByte(':')
}
