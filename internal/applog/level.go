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
	"syscall"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
)

const pluginLogCapBytes = 20971520
const rowMaxBytes = 4096

// Sink names the unchanged legacy log and the plugin's level directory.
type Sink struct{ Log, Levels string }

// Row contains metadata only, never prompt, transcript or tool-input bodies.
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
	return boundedLevelRow(row, false)
}

// boundedLevelRow keeps all identity fields. A derived cap notice may compact
// optional detail further when the full truncation marker cannot fit.
func boundedLevelRow(row Row, compactDetail bool) ([]byte, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("encode plugin log row: %w", err)
	}
	if len(encoded)+1 > rowMaxBytes {
		row.Detail = map[string]any{"truncated": true, "bytes": len(encoded) + 1}
		if compactDetail {
			minimum := row
			minimum.Msg = ""
			for _, detail := range []any{row.Detail, map[string]any{"truncated": true}, nil} {
				minimum.Detail = detail
				raw, marshalErr := json.Marshal(minimum)
				if marshalErr != nil {
					return nil, fmt.Errorf("encode minimum cap notice: %w", marshalErr)
				}
				row.Detail = detail
				if len(raw)+1 <= rowMaxBytes {
					break
				}
			}
		}
		encoded, err = json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("encode truncated plugin log row: %w", err)
		}
		if len(encoded)+1 > rowMaxBytes {
			runes := []rune(row.Msg)
			low, high := 0, len(runes)
			for low < high {
				mid := (low + high + 1) / 2
				row.Msg = string(runes[:mid])
				candidate, marshalErr := json.Marshal(row)
				if marshalErr != nil {
					return nil, fmt.Errorf("encode shortened plugin log row: %w", marshalErr)
				}
				if len(candidate)+1 <= rowMaxBytes {
					low = mid
				} else {
					high = mid - 1
				}
			}
			row.Msg = string(runes[:low])
			encoded, err = json.Marshal(row)
			if err != nil {
				return nil, fmt.Errorf("encode bounded plugin log row: %w", err)
			}
		}
	}
	if len(encoded)+1 > rowMaxBytes {
		return nil, errors.New("plugin log metadata exceeds 4096 bytes")
	}
	return append(encoded, '\n'), nil
}

func logCappedRow(source Row, file string) ([]byte, error) {
	warning := Row{TS: source.TS, Plugin: "callmeter", Level: "warning", Event: "log-capped", Msg: "level file moved aside after exceeding 20 MB", Where: "internal/applog/level.go#Level", Session: source.Session, Detail: map[string]any{"file": file, "bytes": pluginLogCapBytes}}
	encoded, err := boundedLevelRow(warning, true)
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

// Nil outside tests; tests induce native filesystem errors at the chmod sites.
var beforeLevelDirChmod func(string)
var beforeLevelFileChmod func(*os.File)

func writeLevel(dir string, row Row) error {
	file, err := levelFile(row.Level)
	if err != nil {
		return err
	}
	encoded, err := levelRow(row)
	if err != nil {
		return err
	}
	// Preflight the derived frame before changing files: complete immutable
	// metadata must fit both the source and its possible cap notice.
	warning, err := logCappedRow(row, file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create level directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect level directory %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm()&0300 != 0300 {
		return fmt.Errorf("level directory %s refuses creation or rotation: %w", dir, os.ErrPermission)
	}
	if beforeLevelDirChmod != nil {
		beforeLevelDirChmod(dir)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("chmod level directory %s: %w", dir, err)
	}
	for _, name := range []string{"errors.jsonl.1", "warnings.jsonl.1", "info.jsonl.1"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect level backup %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("level backup %s is not regular", path)
		}
		backup, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if err != nil {
			return fmt.Errorf("open level backup %s: %w", path, err)
		}
		if err := backup.Chmod(0600); err != nil {
			return fmt.Errorf("chmod level backup %s: %w", path, errors.Join(err, backup.Close()))
		}
		if err := backup.Close(); err != nil {
			return fmt.Errorf("close level backup %s: %w", path, err)
		}
	}
	capped, err := appendLevel(filepath.Join(dir, file), encoded)
	if err != nil {
		return err
	}
	if capped {
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
		file, err := openLevel(path)
		if err != nil {
			return false, err
		}
		if err := file.Close(); err != nil {
			return false, fmt.Errorf("close capped level file %s: %w", path, err)
		}
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
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open level file %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open level file %s: %w", path, errors.Join(err, file.Close()))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("level file %s is not regular", path), file.Close())
	}
	if beforeLevelFileChmod != nil {
		beforeLevelFileChmod(file)
	}
	if err := file.Chmod(0600); err != nil {
		return nil, fmt.Errorf("chmod level file %s: %w", path, errors.Join(err, file.Close()))
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
	return Row{Level: level, Event: event, Msg: strings.ReplaceAll(event, "-", " "), Where: "internal/hookentry/callmeter.go#" + stage, Session: session, Detail: map[string]string{"error_type": fmt.Sprintf("%T", cause), "target": target}}
}
