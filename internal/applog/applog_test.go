package applog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	Failure(&stderr, logPath, "store", "sess-1", "toolu_9", errors.New("disk full"))

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
	Failure(&stderr, logPath, "parse", "s", "t1", errors.New("one"))
	Failure(&stderr, logPath, "parse", "s", "t2", errors.New("two"))

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
	Failure(&stderr, logPath, "store", "s", "t", errors.New("after"))

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
	Failure(&bytes.Buffer{}, logPath, "store", "s", "t", errors.New("x"))
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
	var stderr bytes.Buffer
	Failure(&stderr, logPath, "store", "s", "t", errors.New("boom"))

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
}

// An empty log path is the hook's "no home resolved" case: the one stderr
// line, and no file created anywhere.
func TestFailureWithNoLogPathWritesOnlyStderr(t *testing.T) {
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	Failure(&stderr, "", "store", "", "", errors.New("resolve callmeter home: unset"))

	if got := strings.Count(stderr.String(), "\n"); got != 1 {
		t.Fatalf("stderr = %q, want exactly one line", stderr.String())
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("working directory holds %d entries after Failure with no path, want none", len(entries))
	}
}
