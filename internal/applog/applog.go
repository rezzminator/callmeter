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
// stderr, one JSON line appended to sink.Log (directory 0700, file 0600),
// then a plugin level row. Past 10 MiB the legacy log is renamed to .1. A
// legacy write failure is one extra stderr line; a plugin write failure is
// one notice per failing streak. Empty paths disable their respective output.
func Failure(stderr io.Writer, sink Sink, stage, session, target string, cause error) {
	FailureAt(stderr, sink, stage, session, target, cause, Row{})
}

// FailureAt preserves Failure's existing output while classifying a site whose
// legacy stage is shared by several different events.
func FailureAt(stderr io.Writer, sink Sink, stage, session, target string, cause error, metadata Row) {
	row := failureRow(stage, session, target, cause)
	if metadata.Level != "" {
		row.Level = metadata.Level
	}
	if metadata.Event != "" {
		row.Event = metadata.Event
	}
	if metadata.Where != "" {
		row.Where = metadata.Where
	}
	if metadata.Session != "" {
		row.Session = metadata.Session
	}
	fmt.Fprintf(stderr, "callmeter: %s: session %q call %q: %v\n", stage, session, target, cause)
	defer Level(stderr, sink, row)
	logPath := sink.Log
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
