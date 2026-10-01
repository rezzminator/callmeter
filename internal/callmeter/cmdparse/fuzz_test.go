package cmdparse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// parseBound is the longest one ParseBatch call of the fuzz target may take:
// a slower parse could hold a report run.
const parseBound = time.Second

// quoteCut quotes a fuzz input cut to its first 200 bytes, so a failure on a
// huge input stays readable.
func quoteCut(command string) string {
	if len(command) > 200 {
		return fmt.Sprintf("%q… (%d bytes)", command[:200], len(command))
	}
	return fmt.Sprintf("%q", command)
}

// failingPython is the Python runner of the fuzz target: a snippet never
// reaches python3, and every Python part takes the runner's failure.
type failingPython struct{}

func (failingPython) Analyze(context.Context, []Snippet) ([]PyResult, error) {
	return nil, errors.New("fuzz: the Python runner stub fails")
}

// FuzzParse: no command panics the parser, every part it returns carries a
// status, and no parse takes longer than parseBound. Seeded with every corpus
// command.
func FuzzParse(f *testing.F) {
	raw, err := os.ReadFile(corpusFixture)
	if err != nil {
		f.Fatalf("read corpus %s: %v", corpusFixture, err)
	}
	var cases []struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		f.Fatalf("decode corpus %s: %v", corpusFixture, err)
	}
	if len(cases) < corpusFloor {
		f.Fatalf("corpus %s holds %d cases, want at least %d", corpusFixture, len(cases), corpusFloor)
	}
	for _, c := range cases {
		f.Add(c.Command)
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, command string) {
		// The parse runs in its own goroutine so a parse that never returns
		// still fails the target at the bound, naming its input.
		type result struct {
			parts map[string][]Part
			err   error
		}
		done := make(chan result, 1)
		start := time.Now()
		go func() {
			parts, err := ParseBatch(context.Background(), []Call{{ID: "fuzz", Command: command, Cwd: dir, Home: dir}}, failingPython{})
			done <- result{parts, err}
		}()
		var r result
		select {
		case r = <-done:
		case <-time.After(parseBound):
			t.Fatalf("ParseBatch(%s) still running after %v, over the %v bound", quoteCut(command), time.Since(start), parseBound)
		}
		if elapsed := time.Since(start); elapsed > parseBound {
			t.Fatalf("ParseBatch(%s) took %v, over the %v bound", quoteCut(command), elapsed, parseBound)
		}
		parts, err := r.parts, r.err
		if err != nil {
			t.Fatalf("ParseBatch(%s) = %v: a command that does not parse is a part, never an error", quoteCut(command), err)
		}
		for _, part := range parts["fuzz"] {
			if part.Status == "" {
				t.Errorf("ParseBatch(%s) part %d (%s) carries no status", quoteCut(command), part.Seq, part.Program)
			}
		}
	})
}
