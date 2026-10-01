package hookentry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/paths"
	"github.com/rezzminator/callmeter/internal/runner"
)

// hookProcesses is how many `callmeter hook` processes write the one store at
// once: more than any real session runs in parallel.
const hookProcesses = 64

// TestProcessConcurrentHooks feeds S1 and S2 through the built binary, every
// payload its own `callmeter hook` process, up to hookProcesses at once on one
// CALLMETER_HOME: every process exits 0 with nothing on stdout, the store
// never faults, and it holds what the in-order replay of the same payloads
// holds.
func TestProcessConcurrentHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lab := newCallmeterLab(t)
	binary := filepath.Join(lab.root, "bin", "callmeter")
	built, err := runner.Real{}.Run(ctx, []string{"go", "build", "-o", binary, "./cmd/callmeter"},
		runner.RunOptions{Dir: filepath.Join("..", "..")})
	if err != nil || built.ExitCode != 0 {
		t.Fatalf("build callmeter: %v (exit %d): %s", err, built.ExitCode, built.Stderr)
	}
	if err := os.RemoveAll(lab.home); err != nil {
		t.Fatalf("clear the lab home: %v", err)
	}
	callmeterHome := filepath.Join(lab.root, "callmeter-home")
	lab.storePath = paths.Store(callmeterHome)
	registered := registeredEvents(t)
	var payloads []string
	wantEvents, wantTurns := 0, 0
	ids := map[string]bool{}
	for _, session := range []string{"S1", "S2"} {
		// The two homes hold different session files: one home holds both.
		if err := os.CopyFS(lab.home, os.DirFS(filepath.Join("testdata", "gym", session, "home"))); err != nil {
			t.Fatalf("copy the %s home: %v", session, err)
		}
		for _, line := range fixturePayloads(t, "gym/"+session) {
			if registered[eventName(t, line)] {
				payloads = append(payloads, lab.rewrite(line))
			}
		}
		inOrder := newReplay(t, "gym/"+session)
		inOrder.feedInOrder()
		wantEvents += inOrder.lab.count("SELECT COUNT(*) FROM events")
		wantTurns += inOrder.lab.count("SELECT COUNT(*) FROM turns")
		for id := range inOrder.calls() {
			ids[id] = true
		}
	}
	env := append(os.Environ(),
		"CALLMETER_HOME="+callmeterHome, "HOME="+lab.home, "CLAUDE_CONFIG_DIR="+filepath.Join(lab.home, ".claude"))
	slots := make(chan struct{}, hookProcesses)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]runner.RunResult, len(payloads))
	failures := make([]error, len(payloads))
	for i, payload := range payloads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i], failures[i] = runner.Real{}.Run(ctx, []string{binary, "hook"},
				runner.RunOptions{Env: env, Stdin: []byte(payload)})
		}()
	}
	close(start)
	wg.Wait()
	for i, result := range results {
		if failures[i] != nil || result.ExitCode != 0 || len(result.Stdout) != 0 {
			t.Errorf("payload %d: err %v, exit %d, stdout %q, stderr %q",
				i, failures[i], result.ExitCode, result.Stdout, result.Stderr)
		}
	}
	if n := lab.count("SELECT COUNT(*) FROM faults WHERE stage = 'store'"); n != 0 {
		t.Errorf("%d store faults under %d concurrent processes:\n%s",
			n, hookProcesses, strings.Join(lab.dump()["faults"], "\n"))
	}
	for id := range ids {
		if n := lab.count("SELECT COUNT(*) FROM calls WHERE tool_use_id = ?", id); n != 1 {
			t.Errorf("call %s: %d rows, want 1", id, n)
		}
	}
	if n := lab.count("SELECT COUNT(*) FROM events"); n != wantEvents {
		t.Errorf("events holds %d rows, want the in-order replay's %d", n, wantEvents)
	}
	if n := lab.count("SELECT COUNT(*) FROM turns"); n != wantTurns {
		t.Errorf("turns holds %d rows, want the in-order replay's %d", n, wantTurns)
	}
}
