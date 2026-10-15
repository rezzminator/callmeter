package applog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
)

const pluginLogCapBytes = 20971520
const rowMaxBytes = 4096

// Sink names the unchanged legacy log and the plugin's level directory.
type Sink struct{ Log, Levels string }

// Row contains one bounded plugin log event.
// Plugin and timestamp are supplied by the writer.
type Row struct {
	TS      string `json:"ts"`
	Plugin  string `json:"plugin"`
	Level   string `json:"level"`
	Event   string `json:"event"`
	Msg     string `json:"msg"`
	Where   string `json:"where"`
	Session string `json:"session,omitempty"`
	Detail  any    `json:"detail,omitempty"`
}

// LevelDir uses a resolved callmeter home, preserving paths.Home's test jail.
func LevelDir(getenv paths.Getenv, home string) string {
	if dir := getenv("PLUGIN_LOG_DIR"); dir != "" {
		return filepath.Join(dir, "callmeter")
	}
	if home != "" {
		return filepath.Join(home, "logs")
	}
	return ""
}

// ResolveSink keeps level output available even if the legacy home is unknown.
func ResolveSink(getenv paths.Getenv, home string) Sink {
	sink := Sink{Levels: LevelDir(getenv, home)}
	if home != "" {
		sink.Log = paths.Log(home)
	}
	return sink
}

var levelFailures = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: make(map[string]bool)}

// Level appends one whole contract row and never changes the caller's result.
// A failed streak emits one extra stderr line; any success re-arms it.
func Level(stderr io.Writer, sink Sink, row Row) {
	if sink.Levels == "" {
		return
	}
	if row.TS == "" {
		row.TS = clock.Real.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	row.Plugin = "callmeter"
	err := writeLevel(sink.Levels, row)
	levelFailures.Lock()
	defer levelFailures.Unlock()
	if err != nil {
		if !levelFailures.dirs[sink.Levels] {
			fmt.Fprintf(stderr, "callmeter: plugin log %s: %s\n", sink.Levels, strings.NewReplacer("\r", " ", "\n", "; ").Replace(err.Error()))
		}
		levelFailures.dirs[sink.Levels] = true
	} else {
		delete(levelFailures.dirs, sink.Levels)
	}
}

func levelRow(row Row) ([]byte, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("encode plugin log row: %w", err)
	}
	if len(encoded)+1 > rowMaxBytes {
		detail := map[string]any{"truncated": true, "bytes": len(encoded) + 1}
		if original, ok := row.Detail.(map[string]string); ok {
			if cause, ok := original["error"]; ok {
				detail["error"] = cause
			}
		}
		row.Detail = detail
		for len(encoded)+1 > rowMaxBytes {
			cause, _ := detail["error"].(string)
			switch {
			case len(cause) > len(row.Msg):
				runes := []rune(cause)
				detail["error"] = string(runes[:len(runes)/2])
			case row.Msg != "":
				runes := []rune(row.Msg)
				row.Msg = string(runes[:len(runes)/2])
			case detail["bytes"] != nil:
				delete(detail, "bytes")
			case detail["error"] != nil:
				delete(detail, "error")
			case row.Detail != nil:
				row.Detail = nil
			default:
				return nil, errors.New("plugin log metadata exceeds 4096 bytes")
			}
			encoded, err = json.Marshal(row)
			if err != nil {
				return nil, fmt.Errorf("encode truncated plugin log row: %w", err)
			}
		}
	}
	return append(encoded, '\n'), nil
}

func logCappedRow(source Row, file string) ([]byte, error) {
	warning := Row{TS: source.TS, Plugin: "callmeter", Level: "warning", Event: "log-capped", Msg: "level file moved aside after exceeding 20 MB", Where: "internal/applog/level.go#Level", Session: source.Session, Detail: map[string]any{"file": file, "bytes": pluginLogCapBytes}}
	encoded, err := levelRow(warning)
	if err != nil {
		return nil, fmt.Errorf("encode log-capped notice: %w", err)
	}
	return encoded, nil
}

func levelFile(level string) (string, error) {
	switch level {
	case "error":
		return "errors.jsonl", nil
	case "warning":
		return "warnings.jsonl", nil
	case "info":
		return "info.jsonl", nil
	}
	return "", fmt.Errorf("unknown plugin log level %q", level)
}

func writeLevel(dir string, row Row) error {
	file, err := levelFile(row.Level)
	if err != nil {
		return err
	}
	encoded, err := levelRow(row)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create level directory: %w", err)
	}
	capped, err := appendLevel(filepath.Join(dir, file), encoded)
	if err != nil {
		return err
	}
	if capped {
		warning, err := logCappedRow(row, file)
		if err != nil {
			return err
		}
		warningsPath := filepath.Join(dir, "warnings.jsonl")
		warningsCapped, err := appendLevel(warningsPath, warning)
		if err != nil {
			return err
		}
		if warningsCapped {
			warning, err = logCappedRow(row, "warnings.jsonl")
			if err != nil {
				return err
			}
			if _, err = appendLevel(warningsPath, warning); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendLevel(path string, encoded []byte) (bool, error) {
	capped := false
	info, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("stat level file %s: %w", path, err)
	}
	if err == nil && info.Size() > pluginLogCapBytes {
		err = os.Rename(path, path+".1")
		if err != nil && !os.IsNotExist(err) {
			return false, fmt.Errorf("cap level file %s: %w", path, err)
		}
		capped = err == nil
	}
	file, err := openLevel(path)
	if err != nil {
		return capped, err
	}
	n, err := file.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return capped, fmt.Errorf("append level file %s: %w", path, errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		return capped, fmt.Errorf("close level file %s: %w", path, err)
	}
	return capped, nil
}

func openLevel(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open level file %s: %w", path, err)
	}
	return file, nil
}

func failureRow(stage, session, target string, cause error) Row {
	level, event := "error", stage+"-failed"
	switch stage {
	case "panic", "report":
		event = "hook-panic"
	case "terminated":
		event = "hook-terminated"
	case "store", "seat":
		event = "store-failed"
	case "parse", "payload":
		event = "payload-unparsable"
	case "transcript":
		level, event = "warning", "transcript-unreadable"
	}
	return Row{Level: level, Event: event, Msg: strings.ReplaceAll(event, "-", " "), Where: "internal/hookentry/callmeter.go#" + stage, Session: session, Detail: map[string]string{"error_type": fmt.Sprintf("%T", cause), "target": target, "error": cause.Error()}}
}
