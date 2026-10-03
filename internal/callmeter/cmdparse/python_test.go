package cmdparse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/callmeter/internal/runner"
)

func TestPythonHeredocAttributesOpenedFile(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "data.json")
	command := "cd . && python3 - <<'EOF'\nimport json\nwith open(\"data.json\") as f:\n    print(json.load(f))\nEOF"
	parts := parseOne(t, cwd, command, nil)
	var py *Part
	for i := range parts {
		if parts[i].Lang == LangPython {
			py = &parts[i]
		}
	}
	if py == nil {
		t.Fatalf("no python part in %+v", parts)
	}
	if py.Status != StatusOK || py.Program != "python3" {
		t.Fatalf("python part = %+v, want ok python3", *py)
	}
	assertFiles(t, []Part{*py}, []FileRef{ref(cwd, "data.json", ActionReadWhole, "")})
}

func TestPythonDashCOpenForWriteIsAWrite(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x.txt")
	parts := parseOne(t, cwd, `python3 -c "open('x.txt','w')"`, nil)
	if len(parts) != 1 || parts[0].Lang != LangPython || parts[0].Status != StatusOK {
		t.Fatalf("parts = %+v, want one ok python part", parts)
	}
	assertFiles(t, parts, []FileRef{ref(cwd, "x.txt", ActionWrite, "")})
}

func TestPythonSyntaxErrorIsAnErrorPart(t *testing.T) {
	t.Parallel()
	cwd := fixture(t)
	parts := parseOne(t, cwd, `python3 -c "def (:"`, nil)
	if len(parts) != 1 || parts[0].Status != StatusError || !strings.Contains(parts[0].Error, "SyntaxError") {
		t.Fatalf("parts = %+v, want one error part carrying SyntaxError", parts)
	}
}

func TestMissingInterpreterMarksPythonUnavailable(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x.txt")
	py := Python3{Runner: runner.Real{}, Program: "callmeter-cmdparse-no-such-python"}
	got, err := ParseBatch(context.Background(), []Call{
		{ID: "a", Command: `python3 -c "open('x.txt')"`, Cwd: cwd},
		{ID: "b", Command: "cat x.txt; python3 <<EOF\nprint(1)\nEOF", Cwd: cwd},
	}, py)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	var unavailable int
	for id, parts := range got {
		for _, p := range parts {
			switch p.Lang {
			case LangPython:
				if p.Status != StatusPythonUnavailable || !strings.Contains(p.Error, "callmeter-cmdparse-no-such-python") {
					t.Fatalf("call %s python part = %+v, want python-unavailable naming the cause", id, p)
				}
				unavailable++
			case LangSh:
				if p.Status != StatusOK {
					t.Fatalf("call %s shell part = %+v, want ok", id, p)
				}
			}
		}
	}
	if unavailable != 2 {
		t.Fatalf("python-unavailable parts = %d, want 2 in %+v", unavailable, got)
	}
}

func TestPythonResolves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		py   PythonRunner
		want bool
	}{
		{"nil", nil, true},
		{"default", Python3{}, true},
		{"missing", Python3{Runner: runner.Real{}, Program: "callmeter-cmdparse-no-such-python"}, false},
		{"injected", failingPython{}, true},
		{"embedded-default", droppingPython{}, true},
		{"embedded-missing", droppingPython{Python3{Program: "callmeter-cmdparse-no-such-python"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PythonResolves(tc.py); got != tc.want {
				t.Errorf("PythonResolves = %t, want %t", got, tc.want)
			}
		})
	}
}

// droppingPython scans with the real interpreter, then loses one result.
type droppingPython struct{ Python3 }

func (p droppingPython) Analyze(ctx context.Context, snippets []Snippet) ([]PyResult, error) {
	results, err := p.Python3.Analyze(ctx, snippets)
	if err != nil {
		return nil, err
	}
	return results[:len(results)-1], nil
}

func TestParseBatchPythonCrashIsPythonError(t *testing.T) {
	for _, tc := range []struct {
		name, script string
	}{
		{"nonzero", "#!/bin/sh\nexit 7\n"},
		{"undecodable", "#!/bin/sh\nprintf '%s' 'not-json'\n"},
		{"wrong-count", "#!/bin/sh\nprintf '%s' '[]'\n"},
		{"wrong-id", "#!/bin/sh\nprintf '%s' '[{\"id\":\"other\"}]'\n"},
		{"cannot-start", "#!/no-such-interpreter\n"},
		{"missing-snippet", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := fixture(t)
			var py PythonRunner = droppingPython{}
			if tc.script != "" {
				program := filepath.Join(cwd, "broken-python")
				if err := os.WriteFile(program, []byte(tc.script), 0o700); err != nil {
					t.Fatal(err)
				}
				py = Python3{Program: program}
			}
			parts := parseOne(t, cwd, `python3 -c "print(1)"`, py)
			if len(parts) != 1 || parts[0].Status != "python-error" || parts[0].Error == "" {
				t.Fatalf("Python status = %s, want python-error with a cause", parts[0].Status)
			}
		})
	}
}

// A Python open() or Path(...) write of a file that does not exist yet is
// attributed, Exists false, in the directory the part ran in; a bare string
// naming nothing on disk stays unattributed.
func TestPythonWriteOfAMissingFile(t *testing.T) {
	t.Parallel()
	cwd := fixture(t)
	if err := os.Mkdir(filepath.Join(cwd, "X"), 0o700); err != nil {
		t.Fatalf("mkdir X: %v", err)
	}
	command := "cd X && python3 - <<'EOF'\nopen('f', 'w').write('x')\nfrom pathlib import Path\n" +
		"Path('g').write_text('y')\nprint('h')\nEOF"
	parts := parseOne(t, cwd, command, nil)
	var files []FileRef
	for _, p := range parts {
		if p.Lang == LangPython {
			if p.Status != StatusOK {
				t.Fatalf("python part = %+v, want ok", p)
			}
			files = append(files, p.Files...)
		}
	}
	assertFiles(t, []Part{{Files: files}}, []FileRef{
		// The scanner walks the tree breadth first: g's call sits higher.
		missing(cwd, "X/g", ActionWrite, ""),
		missing(cwd, "X/f", ActionWrite, ""),
	})
}

// A -c script or here-string holding an expansion the parse cannot know (a
// variable set from a command substitution) is an unknown snippet: one
// unparsed Python part keeping its program, never a Python syntax error on
// the unexpanded `$X`. A variable with a literal value is substituted and
// the snippet parses as before.
func TestPythonCodeWithAnUnresolvedExpansionIsUnparsed(t *testing.T) {
	t.Parallel()
	cwd := fixture(t)
	for _, command := range []string{
		`N=$(date +%s) && V=$(python3 -c "print($N/1000)") && echo "$V"`,
		`a=$(date +%s); b=$(date +%s); python3 -c "print(int(($b-$a)*1000))"`,
		`python3 -c "print($(date +%s)+1)"`,
		`python3 <<< "print($UNSET_VAR)"`,
	} {
		parts := parseOne(t, cwd, command, nil)
		var py []Part
		for _, part := range parts {
			if part.Status == StatusError {
				t.Errorf("%s: error part %+v", command, part)
			}
			if part.Lang == LangPython {
				py = append(py, part)
			}
		}
		if len(py) != 1 || py[0].Program != "python3" || py[0].Status != StatusUnparsed || py[0].Error != errCodeUnresolved {
			t.Errorf("%s: python parts = %+v, want one unparsed python3 part (%s)", command, py, errCodeUnresolved)
		}
	}
	parts := parseOne(t, cwd, `N=5 && python3 -c "print($N/1000)"`, nil)
	if len(parts) != 1 || parts[0].Lang != LangPython || parts[0].Status != StatusOK {
		t.Fatalf("parts = %+v, want one ok python part for a literal variable", parts)
	}
}
