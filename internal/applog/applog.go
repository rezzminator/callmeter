// Package applog is the one door a failed hook step reports through: one line
// on stderr and one JSON line appended to callmeter.log.
package applog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rezzminator/callmeter/internal/clock"
)

// maxLogBytes is the size past which the log is renamed aside before the next
// append.
const maxLogBytes = 10 << 20

// line is one log record; the field order is the order the JSON is written in.
type line struct {
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Step    string `json:"step"`
	Session string `json:"session"`
	Target  string `json:"target"`
	Err     string `json:"err"`
}

// Failure reports that stage failed for session and target: one line on
// stderr, one JSON line appended to logPath (directory 0700, file 0600). Past
// 10 MiB the log is first renamed to logPath+".1". A failure to write the log
// is one more stderr line, never a panic and never an error to the caller. An
// empty logPath writes the stderr line only.
func Failure(stderr io.Writer, logPath, stage, session, target string, cause error) {
	fmt.Fprintf(stderr, "callmeter: %s: session %q call %q: %v\n", stage, session, target, cause)
	if logPath == "" {
		return
	}
	if err := appendLine(logPath, line{
		TS:      clock.Real.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Level:   "error",
		Msg:     "callmeter.record",
		Step:    stage,
		Session: session,
		Target:  target,
		Err:     cause.Error(),
	}); err != nil {
		fmt.Fprintf(stderr, "callmeter: log %s: %v\n", logPath, err)
	}
}

func appendLine(logPath string, record line) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode log line: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create log directory: %w", err)
	}
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogBytes {
		if err := os.Rename(logPath, logPath+".1"); err != nil {
			return fmt.Errorf("rotate log: %w", err)
		}
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write log: %w", errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close log: %w", err)
	}
	return nil
}
