package callmeter

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// toolRead is the Read tool's name: its calls carry a line range.
const toolRead = "Read"

// FileColumnsFromInput sets, from a file tool's input, file_path — made
// absolute against cwd, left relative when there is none — and, for a Read,
// the range it asked for: offset (1 when absent or below 1) and limit. The
// file tools are Read, Write, Edit, MultiEdit (file_path) and NotebookEdit
// (notebook_path); any other tool is left untouched. file_bytes is never set:
// the hook stats the file, a transcript holds no size. Each field is decoded
// on its own: one of the wrong type stays NULL (a mistyped offset leaves the
// start NULL) and is named in the error, the others are set all the same. An
// input that is no JSON object, or a file tool's input naming no path, is the
// error with every column NULL; the caller turns it into its own fault.
func FileColumnsFromInput(c *Call, tool string, input json.RawMessage, cwd string) error {
	pathField := "file_path"
	switch tool {
	case toolRead, "Write", "Edit", "MultiEdit":
	case "NotebookEdit":
		pathField = "notebook_path"
	default:
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil {
		return fmt.Errorf("decode %s input (%d bytes): %w", tool, len(input), err)
	}
	var mistyped []error
	path := decodeField[string](object, pathField, &mistyped)
	if path == "" {
		if len(mistyped) > 0 {
			return fmt.Errorf("decode %s input (%d bytes): %w", tool, len(input), errors.Join(mistyped...))
		}
		return fmt.Errorf("%s input has no %s", tool, pathField)
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	c.FilePath = Ptr(path)
	if tool == toolRead {
		before := len(mistyped)
		offset := decodeField[*float64](object, "offset", &mistyped)
		if len(mistyped) == before {
			c.ReadStart = Ptr(int64(1))
			if offset != nil && *offset >= 1 {
				c.ReadStart = Ptr(int64(*offset))
			}
		}
		c.ReadLines = wholeOr(decodeField[*float64](object, "limit", &mistyped), nil)
	}
	if len(mistyped) > 0 {
		return fmt.Errorf("decode %s input (%d bytes): %w", tool, len(input), errors.Join(mistyped...))
	}
	return nil
}

// FileColumnsFromResult sets, from a file tool's result object, the range a
// Read actually read — each of startLine, numLines and totalLines present wins
// over the input's request — and a Write or Edit's prior size: originalFile's
// length, or 0 for a file the Write created (type "create", originalFile
// null). Any other tool is left untouched. Each field is decoded on its own:
// one of the wrong type is left out and named in the error, the others are set
// all the same; a result that is no JSON object is the error.
func FileColumnsFromResult(c *Call, tool string, result json.RawMessage) error {
	switch tool {
	case toolRead, "Write", "Edit":
	default:
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(result, &object); err != nil {
		return fmt.Errorf("decode %s result (%d bytes): %w", tool, len(result), err)
	}
	var mistyped []error
	if tool == toolRead {
		file := decodeField[map[string]json.RawMessage](object, "file", &mistyped)
		c.ReadStart = wholeOr(decodeField[*float64](file, "startLine", &mistyped), c.ReadStart)
		c.ReadLines = wholeOr(decodeField[*float64](file, "numLines", &mistyped), c.ReadLines)
		c.ReadTotalLines = wholeOr(decodeField[*float64](file, "totalLines", &mistyped), c.ReadTotalLines)
	} else {
		kind := decodeField[string](object, "type", &mistyped)
		before := len(mistyped)
		original := decodeField[*string](object, "originalFile", &mistyped)
		switch {
		case original != nil:
			c.FileBytesBefore = Ptr(int64(len(*original)))
		case kind == "create" && len(mistyped) == before: // a mistyped originalFile is no created file
			c.FileBytesBefore = Ptr(int64(0))
		}
	}
	if len(mistyped) > 0 {
		return fmt.Errorf("decode %s result (%d bytes): %w", tool, len(result), errors.Join(mistyped...))
	}
	return nil
}

// decodeField is object's name field decoded as T: the zero T when it is
// absent, and when it is of another type, which is appended to mistyped.
func decodeField[T any](object map[string]json.RawMessage, name string, mistyped *[]error) T {
	var value T
	raw, ok := object[name]
	if !ok {
		return value
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		*mistyped = append(*mistyped, fmt.Errorf("%s: %w", name, err))
		var zero T // a failed decode can leave value half set
		return zero
	}
	return value
}

// wholeOr is value truncated to a whole number, or fallback when it is absent.
func wholeOr(value *float64, fallback *int64) *int64 {
	if value == nil {
		return fallback
	}
	return Ptr(int64(*value))
}
