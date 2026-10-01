package callmeter

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// keptFields are stored whatever their size: file paths, search patterns and
// (for Bash) the command and its description.
var keptFields = map[string]bool{
	"file_path":     true,
	"notebook_path": true,
	"path":          true,
	"pattern":       true,
	"glob":          true,
}

// SanitizeInput is the stored form of a tool input: a JSON object keeping a
// Bash command and description, file paths, search patterns and the Read
// range, with every dropped field (and any other field whose value exceeds
// LongField bytes) replaced by its byte length under {name}_bytes. A string's
// length is its UTF-8 bytes; any other value's is its JSON encoding's.
func SanitizeInput(tool string, raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("sanitize %s input: not a JSON object: %w", tool, err)
	}
	if fields == nil {
		return "", fmt.Errorf("sanitize %s input: input is null", tool)
	}
	out := make(map[string]any, len(fields))
	for name, value := range fields {
		keep := keptFields[name] || (tool == "Bash" && (name == "command" || name == "description"))
		size, err := valueBytes(value)
		if err != nil {
			return "", fmt.Errorf("sanitize %s input field %q: %w", tool, name, err)
		}
		if droppedFields[name] || (!keep && size > LongField) {
			out[name+"_bytes"] = size
			continue
		}
		out[name] = value
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("sanitize %s input: encode: %w", tool, err)
	}
	return string(encoded), nil
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
	"id": true, "type": true, "status": true, "source": true, "reason": true, "trigger": true,
	"error": true, "level": true, "mode": true, "behavior": true, "destination": true,
	"notification_type": true, "load_reason": true, "memory_type": true, "command_name": true,
	"command_source": true, "expansion_type": true, "task_id": true, "tool_name": true,
	"model": true, "file_path": true, "trigger_file_path": true, "schedule": true, "cron": true,
}

// SanitizeDetail is the stored form of a hook payload's leftover keys (an
// events row's detail) or of a JSON array it carries (background_tasks,
// session_crons): objects and arrays keep their shape and key order, numbers,
// booleans and null stay, and a string is kept only under a key of
// detailKeptStrings. Any other string in an object becomes {key}_bytes, and in
// an array its byte count (UTF-8 bytes). omit names the top-level keys left
// out. raw must be a JSON object or array.
func SanitizeDetail(raw json.RawMessage, omit map[string]bool) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return "", fmt.Errorf("sanitize detail: not a JSON object or array (%d bytes)", len(trimmed))
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("sanitize detail: invalid JSON (%d bytes)", len(trimmed))
	}
	var out bytes.Buffer
	if err := sanitizeValue(&out, trimmed, "", false, omit); err != nil {
		return "", fmt.Errorf("sanitize detail: %w", err)
	}
	return out.String(), nil
}

// sanitizeValue writes the sanitized form of value, found under key (inside an
// array when inArray). omit applies to value's own keys.
func sanitizeValue(out *bytes.Buffer, value json.RawMessage, key string, inArray bool, omit map[string]bool) error {
	value = bytes.TrimSpace(value)
	switch value[0] {
	case '{':
		return sanitizeObject(out, value, omit)
	case '[':
		return sanitizeArray(out, value, key)
	case '"':
		if detailKeptStrings[key] {
			var s string
			if err := json.Unmarshal(value, &s); err != nil {
				return fmt.Errorf("decode the %q string: %w", key, err)
			}
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
		size, err := valueBytes(value)
		if err != nil {
			return fmt.Errorf("measure a %q array string: %w", key, err)
		}
		fmt.Fprint(out, size)
		return nil
	default:
		out.Write(value) // a number, true, false or null
		return nil
	}
}

func sanitizeObject(out *bytes.Buffer, value json.RawMessage, omit map[string]bool) error {
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
		if field[0] == '"' && !detailKeptStrings[name] {
			size, err := valueBytes(field)
			if err != nil {
				return fmt.Errorf("measure the %q string: %w", name, err)
			}
			writeKey(out, name+"_bytes")
			fmt.Fprint(out, size)
			continue
		}
		writeKey(out, name)
		if err := sanitizeValue(out, field, name, false, nil); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

func sanitizeArray(out *bytes.Buffer, value json.RawMessage, key string) error {
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
		if err := sanitizeValue(out, element, key, true, nil); err != nil {
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
