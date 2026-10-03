package hookentry

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/clock"
)

// FuzzHookPayload: whatever bytes arrive on stdin, the hook exits 0, writes
// nothing to stdout and never panics. Seeded with every captured payload line.
// The SubagentStop settle wait is zeroed for the target's run: it tests input
// handling, not timing, and a real settle per input would starve the engine.
// Coverage minimization is switched off too: the engine hands every input that
// found new coverage to one worker to shrink for up to -fuzzminimizetime (60 s),
// each attempt a full hook run over a payload of up to tens of KB, and those
// attempts are not counted as execs, so a few such inputs held at once leave
// every worker minimizing and the run reporting 0 execs/s.
func FuzzHookPayload(f *testing.F) {
	settle := agentSettle
	f.Cleanup(func() {
		if agentSettle != settle {
			f.Errorf("agentSettle is %v after the fuzz target, want it restored to %v", agentSettle, settle)
		}
	})
	f.Cleanup(func() { agentSettle = settle })
	agentSettle = 0
	minimizationOff(f)
	fixtures := []string{"verify"}
	for _, session := range gymSessions {
		fixtures = append(fixtures, "gym/"+session)
	}
	for _, fixture := range fixtures {
		for _, line := range fixturePayloads(f, fixture) {
			f.Add([]byte(line))
		}
	}
	var probeLines []string
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
			probeLines = append(probeLines, line)
		}
	}
	root := f.TempDir()
	unsettledTranscript := filepath.Join(root, "agent-unsettled.jsonl")
	if err := os.WriteFile(unsettledTranscript, nil, 0o600); err != nil {
		f.Fatalf("write the empty agent transcript: %v", err)
	}
	unsettled := unsettledSubagentStop(f, probeLines, unsettledTranscript)
	f.Add(unsettled)
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
		began := time.Now()
		code := runCallmeter(context.Background(), bytes.NewReader(payload), &stderr, files, timing,
			callmeterSeat{}, mapEnv(map[string]string{}))
		os.Stdout = saved
		if took := time.Since(began); bytes.Equal(payload, unsettled) && took >= settle {
			t.Errorf("a SubagentStop with no final message took %v, want no settle wait (%v)", took, settle)
		}
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

// unsettledSubagentStop is a captured SubagentStop whose transcript path names
// the given empty file: the one input that would wait out the settle, since
// the file exists and holds no final message.
func unsettledSubagentStop(tb testing.TB, captured []string, transcript string) []byte {
	tb.Helper()
	for _, line := range captured {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if payload["hook_event_name"] != "SubagentStop" || payload["agent_type"] == nil {
			continue
		}
		payload["agent_transcript_path"] = transcript
		data, err := json.Marshal(payload)
		if err != nil {
			tb.Fatalf("encode the captured SubagentStop: %v", err)
		}
		return data
	}
	tb.Fatal("no captured typed SubagentStop among the probe fixtures")
	return nil
}

// minimizationOff switches the fuzz engine's minimization off, unless the
// command line names -fuzzminimizetime: an explicit value wins. It sets the
// flag only while it reads nonzero, since a set test.fuzzminimizetime refuses
// zero (testing's flag type drops its allowZero on Set), so a second run of
// the target in one process (-count=2) would otherwise fail its own switch.
func minimizationOff(tb testing.TB) {
	tb.Helper()
	minimize := flag.Lookup("test.fuzzminimizetime")
	if minimize == nil {
		tb.Fatal("the test.fuzzminimizetime flag is not registered; the engine's minimization cannot be switched off")
	}
	explicit := false
	flag.Visit(func(set *flag.Flag) { explicit = explicit || set == minimize })
	if explicit || minimize.Value.String() == "0s" {
		return
	}
	if err := minimize.Value.Set("0s"); err != nil {
		tb.Fatalf("switch off the engine's minimization: %v", err)
	}
}

// A second run of FuzzHookPayload in one process (go test -count=2) switches
// minimization off again without failing; a command-line value stays.
func TestMinimizationOffHoldsOnASecondRun(t *testing.T) {
	minimize := flag.Lookup("test.fuzzminimizetime")
	want := "0s"
	flag.Visit(func(set *flag.Flag) {
		if set == minimize {
			want = minimize.Value.String()
		}
	})
	minimizationOff(t)
	minimizationOff(t)
	if got := minimize.Value.String(); got != want {
		t.Errorf("test.fuzzminimizetime is %s, want %s", got, want)
	}
}
