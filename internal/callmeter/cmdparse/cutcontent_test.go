package cmdparse

import (
	"context"
	"reflect"
	"testing"
)

// Over every corpus command, the content cut (after the heredoc cut, as the
// store makes it) refuses only what parses as neither shell, gives back its
// own output unchanged with nothing removed, and leaves every part's program
// and attributed files as they were.
func TestCutContentOverCorpus(t *testing.T) {
	cut := 0
	for _, c := range loadCorpus(t) {
		redacted, _, ok := RedactHeredocs(c.Command)
		if !ok {
			continue
		}
		got, removed, ok := CutContent(redacted)
		if !ok {
			if _, err := parseShell(redacted, true); err == nil {
				t.Errorf("%s: CutContent refused a command that parses", c.Name)
			}
			continue
		}
		again, more, ok := CutContent(got)
		if !ok || again != got || more != 0 {
			t.Errorf("%s: cut again = %q, %d, %v; want %q, 0, true", c.Name, again, more, ok, got)
		}
		if removed == 0 {
			continue
		}
		cut++
		calls := []Call{{ID: "before", Command: redacted, Cwd: c.Cwd}, {ID: "after", Command: got, Cwd: c.Cwd}}
		parts, err := ParseBatch(context.Background(), calls, nil)
		if err != nil {
			t.Fatalf("%s: ParseBatch: %v", c.Name, err)
		}
		if !reflect.DeepEqual(shapeOf(parts["before"]), shapeOf(parts["after"])) {
			t.Errorf("%s: parts differ\nbefore %+v\nafter  %+v", c.Name, shapeOf(parts["before"]), shapeOf(parts["after"]))
		}
	}
	if cut == 0 {
		t.Fatal("no corpus command had content cut: the corpus no longer covers CutContent")
	}
}

// partShape is what a cut must leave of a part: its program and its files.
type partShape struct {
	Program string
	Files   []FileRef
}

func shapeOf(parts []Part) []partShape {
	out := make([]partShape, len(parts))
	for i, p := range parts {
		out[i] = partShape{Program: p.Program, Files: p.Files}
	}
	return out
}
