// Package faultlog is the one door a failed hook step reports through: one JSON
// line appended to callmeter.log, one level row through the vendored plugin log
// (internal/pluginlog), and, for an error row only, one line on stderr. Both
// files sit in the plugin log directory (paths.LogDir).
package faultlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rezzminator/callmeter/internal/clock"
	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/pluginlog"
)

// plugin is callmeter's name in its level rows and its plugin log directory.
const plugin = "callmeter"

// maxLogBytes is the size past which the log is renamed aside before the next
// append.
const maxLogBytes = 10 << 20

// Sink names callmeter.log and the plugin's level directory; "" disables its
// output.
type Sink struct{ Log, Levels string }

// ResolveSink is callmeter.log and the level files in the plugin log directory,
// both "" when none resolves.
func ResolveSink(getenv paths.Getenv) Sink {
	dir := paths.LogDir(getenv)
	return Sink{Log: paths.Log(dir), Levels: dir}
}

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

// Failure reports that stage failed for session and target: one JSON line
// appended to sink.Log (directory 0700, file 0600), a plugin level row, and
// one stderr line when that row is an error. Past 10 MiB the legacy log is
// renamed to .1. A legacy write failure is an error row of its own, its line
// on stderr; a plugin write failure is one notice per failing streak.
func Failure(stderr io.Writer, sink Sink, stage, session, target string, cause error) {
	FailureAt(stderr, sink, stage, session, target, cause, pluginlog.Row{})
}

// FailureAt is Failure classifying a site whose legacy stage is shared by
// several different events: metadata's non-empty fields replace the stage's.
func FailureAt(stderr io.Writer, sink Sink, stage, session, target string, cause error, metadata pluginlog.Row) {
	row := failureRow(stage, session, target, cause)
	if metadata.Level != "" {
		row.Level = metadata.Level
	}
	if metadata.Event != "" {
		row.Event = metadata.Event
	}
	if metadata.Msg != "" {
		row.Msg = metadata.Msg
	}
	if metadata.Where != "" {
		row.Where = metadata.Where
	}
	if metadata.Session != "" {
		row.Session = metadata.Session
	}
	if pluginlog.OnScreen(row.Level) {
		fmt.Fprintf(stderr, "callmeter: %s: session %q call %q: %v\n", stage, session, target, cause)
	}
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
		Level(stderr, sink, pluginlog.Row{Level: "error", Event: "callmeter-log-failed", Msg: "callmeter.log append failed", Where: "internal/faultlog/faultlog.go#FailureAt", Session: session, Detail: map[string]string{"error": err.Error()}})
	}
}

// Level writes one row to the plugin log and never changes the caller's
// result; the library says a failing streak once on stderr.
func Level(stderr io.Writer, sink Sink, row pluginlog.Row) {
	pluginlog.For(plugin, sink.Levels, stderr).Record(row)
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

// failureRow maps a legacy stage to its level row.
func failureRow(stage, session, target string, cause error) pluginlog.Row {
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
	return pluginlog.Row{Level: level, Event: event, Msg: strings.ReplaceAll(event, "-", " "), Where: "internal/hookentry/callmeter.go#" + stage, Session: session, Detail: map[string]string{"error_type": fmt.Sprintf("%T", cause), "target": target, "error": cause.Error()}}
}
