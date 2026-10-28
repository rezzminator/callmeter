package faultlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/pluginlog"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestFailureWritesTheStderrLineAndOneJSONLine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "state", "callmeter.log")
	var stderr bytes.Buffer
	Failure(&stderr, Sink{Log: logPath, Levels: filepath.Dir(logPath)}, "store", "sess-1", "toolu_9", errors.New("disk full"))

	want := `callmeter: store: session "sess-1" call "toolu_9": disk full` + "\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
	lines := readLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1: %q", len(lines), lines)
	}
	var record map[string]string
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, lines[0])
	}
	for key, want := range map[string]string{
		"level": "error", "msg": "callmeter.record", "step": "store",
		"session": "sess-1", "target": "toolu_9", "err": "disk full",
	} {
		if record[key] != want {
			t.Errorf("%s = %q, want %q", key, record[key], want)
		}
	}
	ts, err := time.Parse("2006-01-02T15:04:05.000Z07:00", record["ts"])
	if err != nil {
		t.Fatalf("ts %q is not RFC 3339 with milliseconds: %v", record["ts"], err)
	}
	if !strings.HasSuffix(record["ts"], "Z") || time.Since(ts) > time.Minute {
		t.Errorf("ts = %q, want a UTC time from now", record["ts"])
	}
	if !strings.HasPrefix(lines[0], `{"ts":`) {
		t.Errorf("line = %s, want ts first", lines[0])
	}
}

func TestFailureLogModesAndAppend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	logPath := filepath.Join(dir, "callmeter.log")
	var stderr bytes.Buffer
	Failure(&stderr, Sink{Log: logPath, Levels: filepath.Dir(logPath)}, "parse", "s", "t1", errors.New("one"))
	Failure(&stderr, Sink{Log: logPath, Levels: filepath.Dir(logPath)}, "parse", "s", "t2", errors.New("two"))

	if got := len(readLines(t, logPath)); got != 2 {
		t.Fatalf("log has %d lines, want 2 appended", got)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("log directory mode = %o, want 700", got)
	}
	fileInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("log file mode = %o, want 600", got)
	}
}

func TestFailureRotatesAnOversizeLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "callmeter.log")
	if err := os.WriteFile(logPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(logPath, maxLogBytes+1); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	Failure(&stderr, Sink{Log: logPath, Levels: filepath.Dir(logPath)}, "store", "s", "t", errors.New("after"))

	rotated, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("rotated log missing: %v", err)
	}
	if rotated.Size() != maxLogBytes+1 {
		t.Errorf("rotated log size = %d, want the old file's %d", rotated.Size(), maxLogBytes+1)
	}
	if got := len(readLines(t, logPath)); got != 1 {
		t.Fatalf("fresh log has %d lines, want 1", got)
	}
	if stderr.Len() == 0 || strings.Count(stderr.String(), "\n") != 1 {
		t.Errorf("stderr = %q, want exactly the one failure line", stderr.String())
	}
}

func TestFailureKeepsALogAtTheCeiling(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "callmeter.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(logPath, maxLogBytes); err != nil {
		t.Fatal(err)
	}
	Failure(&bytes.Buffer{}, Sink{Log: logPath, Levels: filepath.Dir(logPath)}, "store", "s", "t", errors.New("x"))
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a log of exactly 10 MiB rotated (stat error %v), want it kept until past the ceiling", err)
	}
}

// TestFailureLogWriteErrorIsOneMoreStderrLine pins the broken state: a log
// path that cannot be written never panics and never silences the failure —
// stderr carries the original line and one line naming the log error.
func TestFailureLogWriteErrorIsOneMoreStderrLine(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(blocker, "sub", "callmeter.log")
	levels := t.TempDir()
	var stderr bytes.Buffer
	Failure(&stderr, Sink{Log: logPath, Levels: levels}, "store", "s", "t", errors.New("boom"))

	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr = %q, want the failure line plus one log-error line", stderr.String())
	}
	if !strings.HasPrefix(lines[0], "callmeter: store: session") {
		t.Errorf("first line = %q, want the failure line", lines[0])
	}
	if !strings.HasPrefix(lines[1], "callmeter: log "+logPath+": ") {
		t.Errorf("second line = %q, want it to name the log path and its cause", lines[1])
	}
	events := map[any]bool{}
	for _, row := range levelRecords(t, filepath.Join(levels, "errors.jsonl")) {
		events[row["event"]] = true
	}
	if !events["store-failed"] || !events["callmeter-log-failed"] {
		t.Errorf("error rows = %v, want the failure and the log write failure", events)
	}
}

// An empty sink is the "no log directory resolved" case: the failure line,
// at most the library's one "plugin log off" streak line, and no file created
// anywhere.
func TestFailureWithNoLogPathWritesOnlyStderr(t *testing.T) {
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	Failure(&stderr, Sink{}, "store", "", "", errors.New("resolve callmeter home: unset"))

	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if !strings.HasPrefix(lines[0], "callmeter: store: ") || len(lines) > 2 || (len(lines) == 2 && lines[1] != "callmeter: plugin log off: no PLUGIN_LOG_DIR and no HOME") {
		t.Fatalf("stderr = %q, want the failure line and at most the plugin-log-off line", stderr.String())
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("working directory holds %d entries after Failure with no path, want none", len(entries))
	}
}

// TestFailureShowsOnlyErrorLines: a failure whose row is a warning or info
// (the transcript stage, or a site's own level) is logged, its line never
// shown; an error's line is.
func TestFailureShowsOnlyErrorLines(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		metadata    pluginlog.Row
		shown       bool
	}{
		{"error", "store", pluginlog.Row{}, true},
		{"transcript warning", "transcript", pluginlog.Row{}, false},
		{"warning site", "store", pluginlog.Row{Level: "warning", Event: "store-busy-gave-up"}, false},
		{"info site", "store", pluginlog.Row{Level: "info", Event: "session-end-budget-spent"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sink := Sink{Log: filepath.Join(dir, "callmeter.log"), Levels: dir}
			var stderr bytes.Buffer
			FailureAt(&stderr, sink, tc.stage, "s1", "t1", errors.New("cause"), tc.metadata)
			if shown := stderr.Len() > 0; shown != tc.shown {
				t.Errorf("stderr = %q, want shown=%v: only an error row's line reaches the screen", stderr.String(), tc.shown)
			}
			if got := len(readLines(t, sink.Log)); got != 1 {
				t.Errorf("callmeter.log has %d lines, want the failure logged either way", got)
			}
		})
	}
}

func levelRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	lines := readLines(t, path)
	records := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &records[i]); err != nil {
			t.Fatalf("invalid row: %v", err)
		}
	}
	return records
}

// TestResolveSinkUsesThePluginLogDirectory: callmeter.log and the level files
// share $PLUGIN_LOG_DIR/callmeter, else $HOME/.local/state/callmeter/logs;
// CALLMETER_HOME places neither.
func TestResolveSinkUsesThePluginLogDirectory(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"shared", map[string]string{"PLUGIN_LOG_DIR": filepath.Join(root, "shared"), "HOME": filepath.Join(root, "home")}, filepath.Join(root, "shared", "callmeter")},
		{"home", map[string]string{"HOME": filepath.Join(root, "home"), "CALLMETER_HOME": filepath.Join(root, "store")}, filepath.Join(root, "home", ".local", "state", "callmeter", "logs")},
		{"neither", map[string]string{"CALLMETER_HOME": filepath.Join(root, "store")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := ResolveSink(func(key string) string { return tc.env[key] })
			wantLog := ""
			if tc.want != "" {
				wantLog = filepath.Join(tc.want, "callmeter.log")
			}
			if sink.Levels != tc.want || sink.Log != wantLog {
				t.Fatalf("sink = %+v, want levels %q and log %q", sink, tc.want, wantLog)
			}
		})
	}
	sink := ResolveSink(func(key string) string {
		return map[string]string{"HOME": filepath.Join(root, "home")}[key]
	})
	Failure(&bytes.Buffer{}, sink, "store", "s1", "", errors.New("database is locked"))
	row := levelRecords(t, filepath.Join(sink.Levels, "errors.jsonl"))[0]
	if row["event"] != "store-failed" || row["where"] != "internal/hookentry/callmeter.go#store" {
		t.Fatalf("row = %v", row)
	}
	if got := len(readLines(t, sink.Log)); got != 1 {
		t.Fatalf("callmeter.log has %d lines, want 1", got)
	}
}

func TestFailureKeepsLegacyAndMapsStages(t *testing.T) {
	for _, tc := range []struct{ name, stage, level, event, cause string }{
		{"store", "store", "error", "store-failed", "database is locked"},
		{"seat", "seat", "error", "store-failed", "database is locked"},
		{"parse", "parse", "error", "payload-unparsable", "invalid payload"},
		{"payload", "payload", "error", "payload-unparsable", "invalid payload"},
		{"panic", "panic", "error", "hook-panic", "hook panicked"},
		{"terminated", "terminated", "error", "hook-terminated", "terminated by signal"},
		{"transcript", "transcript", "warning", "transcript-unreadable", "transcript unreadable"},
		{"empty", "store", "error", "store-failed", ""},
		{"bounded-unicode", "store", "error", "store-failed", strings.Repeat("界<&\"\\", 3000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sink := Sink{Log: filepath.Join(dir, "callmeter.log"), Levels: filepath.Join(dir, "lv")}
			var stderr bytes.Buffer
			cause := errors.New(tc.cause)
			Failure(&stderr, sink, tc.stage, "s1", "t1", cause)
			want := ""
			if tc.level == "error" {
				want = "callmeter: " + tc.stage + ": session \"s1\" call \"t1\": " + cause.Error() + "\n"
			}
			if stderr.String() != want {
				t.Fatalf("stderr=%q, want %q", stderr.String(), want)
			}
			var legacy map[string]string
			if err := json.Unmarshal([]byte(readLines(t, sink.Log)[0]), &legacy); err != nil {
				t.Fatal(err)
			}
			if legacy["step"] != tc.stage || legacy["level"] != "error" || legacy["err"] != cause.Error() {
				t.Fatalf("legacy row=%v", legacy)
			}
			file := "errors.jsonl"
			if tc.level == "warning" {
				file = "warnings.jsonl"
			}
			row := levelRecords(t, filepath.Join(sink.Levels, file))[0]
			if row["event"] != tc.event || row["level"] != tc.level || row["where"] != "internal/hookentry/callmeter.go#"+tc.stage || row["session"] != "s1" {
				t.Fatalf("stage row=%v", row)
			}
			detail, ok := row["detail"].(map[string]any)
			if !ok {
				t.Fatalf("failure detail=%v, want cause", row["detail"])
			}
			if tc.name == "bounded-unicode" {
				if detail["truncated"] != true {
					t.Fatalf("bounded detail=%v, want the truncation marker", detail)
				}
			} else if detail["error"] != tc.cause {
				t.Fatalf("failure detail.error=%v, want %q", detail["error"], tc.cause)
			}
		})
	}
}
