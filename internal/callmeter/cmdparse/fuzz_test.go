package cmdparse

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// failingPython is the Python runner of the fuzz target: a snippet never
// reaches python3, and every Python part takes the runner's failure.
type failingPython struct{}

func (failingPython) Analyze(context.Context, []Snippet) ([]PyResult, error) {
	return nil, errors.New("fuzz: the Python runner stub fails")
}

// FuzzParse: no command panics the parser, and every part it returns carries
// a status. Seeded with every corpus command.
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
		parts, err := ParseBatch(context.Background(), []Call{{ID: "fuzz", Command: command, Cwd: dir, Home: dir}}, failingPython{})
		if err != nil {
			t.Fatalf("ParseBatch(%q) = %v: a command that does not parse is a part, never an error", command, err)
		}
		for _, part := range parts["fuzz"] {
			if part.Status == "" {
				t.Errorf("ParseBatch(%q) part %d (%s) carries no status", command, part.Seq, part.Program)
			}
		}
	})
}
