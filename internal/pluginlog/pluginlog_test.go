package pluginlog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordKeepsARowOfAnUnknownLevel: a row whose level names no level file
// is kept in errors.jsonl with its level as given, never refused.
func TestRecordKeepsARowOfAnUnknownLevel(t *testing.T) {
	dir := t.TempDir()
	var screen bytes.Buffer
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	New("callmeter", dir, &screen, func() time.Time { return at }).
		Record(Row{Level: "notice", Event: "probe", Msg: "kept", Where: "internal/pluginlog/pluginlog_test.go"})
	path := filepath.Join(dir, "errors.jsonl")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v; screen %q", path, err, screen.String())
	}
	want := `{"ts":"2026-11-01T12:00:00.000Z","plugin":"callmeter","level":"notice","event":"probe","msg":"kept","where":"internal/pluginlog/pluginlog_test.go"}` + "\n"
	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
	if screen.Len() != 0 {
		t.Errorf("screen = %q, want nothing", screen.String())
	}
}
