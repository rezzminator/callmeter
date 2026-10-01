package hookentry

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/clock"
)

// FuzzHookPayload: whatever bytes arrive on stdin, the hook exits 0, writes
// nothing to stdout and never panics. Seeded with every captured payload line.
func FuzzHookPayload(f *testing.F) {
	fixtures := []string{"verify"}
	for _, session := range gymSessions {
		fixtures = append(fixtures, "gym/"+session)
	}
	for _, fixture := range fixtures {
		for _, line := range fixturePayloads(f, fixture) {
			f.Add([]byte(line))
		}
	}
	probes, err := filepath.Glob(filepath.Join("testdata", "callmeter", "*.jsonl"))
	if err != nil || len(probes) == 0 {
		f.Fatalf("no captured probe fixtures (%v)", err)
	}
	for _, probe := range probes {
		data, err := os.ReadFile(probe)
		if err != nil {
			f.Fatalf("read %s: %v", probe, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			f.Add([]byte(line))
		}
	}
	root := f.TempDir()
	files := callmeterFiles{
		store:  filepath.Join(root, "callmeter.db"),
		log:    filepath.Join(root, "callmeter.log"),
		missed: filepath.Join(root, "missed.log"),
	}
	timing := clock.NewFake(time.Date(2026, 9, 23, 1, 30, 0, 0, time.UTC))
	f.Fuzz(func(t *testing.T, payload []byte) {
		stdout, err := os.CreateTemp(root, "stdout-*")
		if err != nil {
			t.Fatalf("create the stdout capture: %v", err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		var stderr bytes.Buffer
		code := runCallmeter(context.Background(), bytes.NewReader(payload), &stderr, files, timing,
			callmeterSeat{}, mapEnv(map[string]string{}))
		os.Stdout = saved
		info, statErr := stdout.Stat()
		if err := stdout.Close(); err != nil {
			t.Errorf("close the stdout capture: %v", err)
		}
		if statErr != nil {
			t.Fatalf("stat the stdout capture: %v", statErr)
		}
		if code != 0 {
			t.Errorf("exit code %d, want 0 for any payload; stderr %q", code, stderr.String())
		}
		if info.Size() != 0 {
			t.Errorf("the hook wrote %d bytes to stdout, want none", info.Size())
		}
		if err := os.Remove(stdout.Name()); err != nil {
			t.Errorf("remove the stdout capture: %v", err)
		}
	})
}
