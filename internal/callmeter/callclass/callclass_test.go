package callclass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A case is one real mis-parsed or edge command, genericized: invented paths
// and names. Classes are listed in Order; the first is the primary class.
type classCase struct {
	Name    string   `json:"name"`
	Covers  string   `json:"covers"`
	Tool    string   `json:"tool"`
	Command string   `json:"command"`
	Classes []string `json:"classes"`
	Status  string   `json:"status"`
	// Rules: the case runs with testdata/rules.json; false runs it neutral.
	Rules bool `json:"rules"`
}

// fixtureRules is the generic rules fixture: invented wrapper, test and tool names.
func fixtureRules(t *testing.T) Rules {
	t.Helper()
	r, err := LoadRules("testdata/rules.json")
	if err != nil {
		t.Fatalf("LoadRules fixture: %v", err)
	}
	return r
}

func loadCases(t *testing.T) []classCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read cases: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cases []classCase
	if err := dec.Decode(&cases); err != nil {
		t.Fatalf("decode cases: %v", err)
	}
	return cases
}

func TestClassifyCases(t *testing.T) {
	t.Parallel()
	cases := loadCases(t)
	covered := map[string]bool{}
	var calls []Call
	for i, tc := range cases {
		covered[tc.Covers] = true
		calls = append(calls, Call{ID: fmt.Sprint(i), Tool: tc.Tool, Command: tc.Command, Cwd: "/tmp"})
	}
	for _, bucket := range []string{"T-but-inspection", "X-but-output-capture", "?-but-script-edit", "X-but-heredoc-body-has->", "T-but-edit", "X-but-git-state"} {
		if !covered[bucket] {
			t.Errorf("no case covers misparse bucket %q", bucket)
		}
	}
	// one batch per rule set: the cases with rules run under the fixture, the rest neutral
	got := map[string]Result{}
	for _, withRules := range []bool{false, true} {
		var batch []Call
		for i, tc := range cases {
			if tc.Rules == withRules {
				batch = append(batch, calls[i])
			}
		}
		opt := Options{}
		if withRules {
			opt.Rules = fixtureRules(t)
		}
		res, err := Classify(context.Background(), batch, opt)
		if err != nil {
			t.Fatalf("Classify (rules %t): %v", withRules, err)
		}
		for id, r := range res {
			got[id] = r
		}
	}
	for i, tc := range cases {
		res := got[fmt.Sprint(i)]
		var classes []string
		for _, c := range res.Classes {
			classes = append(classes, string(c))
		}
		wantStatus := tc.Status
		if wantStatus == "" {
			wantStatus = "ok"
		}
		if !slices.Equal(classes, tc.Classes) || string(res.Class) != tc.Classes[0] || !strings.HasPrefix(res.Status, wantStatus) {
			t.Errorf("%s [%s]\n  command %q\n  got  class=%s classes=%v status=%q\n  want class=%s classes=%v status=%q",
				tc.Name, tc.Covers, tc.Command, res.Class, classes, res.Status, tc.Classes[0], tc.Classes, wantStatus)
		}
	}
}

func TestLoadRulesRefusesABadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, tc := range []struct{ name, body, cause string }{
		{"malformed json", `{"tools": `, "unexpected EOF"},
		{"empty file", ``, "EOF"},
		{"unknown field", `{"tool": {}}`, `unknown field "tool"`},
		{"unknown class", `{"tools": {"mcp__x__go": "edits"}}`, `unknown class "edits"`},
		{"empty tool name", `{"tools": {"": "edit"}}`, "empty tool name"},
		{"bash tool", `{"tools": {"Bash": "edit"}}`, "judged by its command"},
		{"wrapper without program", `{"wrappers": [{"after": ["run"]}]}`, "wrappers[0]: program"},
		{"wrapper program with a path", `{"wrappers": [{"program": "bin/box", "after": ["run"]}]}`, `program "bin/box"`},
		{"wrapper without after", `{"wrappers": [{"program": "box"}]}`, "wrappers[0].after: no words"},
		{"test with an empty word", `{"tests": [{"program": "box", "args": ["a", ""]}]}`, "tests[0].args: empty word"},
		{"test without args", `{"tests": [{"program": "box", "args": []}]}`, "tests[0].args: no words"},
		{"two documents", `{"tests": [{"program": "box", "args": ["a"]}]} {}`, "more than one JSON document"},
		{"trailing garbage", `{"tests": [{"program": "box", "args": ["a"]}]} garbage`, "invalid character 'g'"},
		{"null", `null`, "names no rule"},
		{"empty object", `{}`, "names no rule"},
		{"null tools", `{"tools": null}`, "names no rule"},
		{"empty lists", `{"tools": {}, "wrappers": [], "tests": []}`, "names no rule"},
		{"oversized", `{"tools": {"mcp__x__go": "edit"}}` + strings.Repeat(" ", 1<<20), "larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := LoadRules(path)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("LoadRules = %+v, %v; want an error naming %s and %q", r, err, path, tc.cause)
			}
		})
	}
	// A FIFO (or /dev/stdin) would block the run before it reads a call: it is refused unopened.
	fifo := filepath.Join(dir, "rules.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadRules(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), fifo) || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("LoadRules(fifo) = %v, want an error naming %s as not a regular file", err, fifo)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("LoadRules(fifo) still blocked after 10s")
	}
	missing := filepath.Join(dir, "no-such-rules.json")
	if _, err := LoadRules(missing); err == nil || !strings.Contains(err.Error(), missing) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadRules(missing) = %v, want an error naming %s wrapping ErrNotExist", err, missing)
	}
}

func TestLoadRulesReadsTheFixture(t *testing.T) {
	t.Parallel()
	r := fixtureRules(t)
	if r.Tools["mcp__x__start"] != Spawn || len(r.Wrappers) != 2 || len(r.Tests) != 3 {
		t.Fatalf("fixture rules = %+v", r)
	}
}

func TestClassifyRejectsDuplicateIDs(t *testing.T) {
	t.Parallel()
	_, err := Classify(context.Background(), []Call{{ID: "a", Tool: "Read"}, {ID: "a", Tool: "Edit"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "duplicate call id") {
		t.Fatalf("err = %v, want duplicate call id", err)
	}
}

func TestOrderHoldsEveryClassOnce(t *testing.T) {
	t.Parallel()
	seen := map[Class]bool{}
	for _, c := range Order {
		if seen[c] {
			t.Errorf("class %q twice in Order", c)
		}
		seen[c] = true
	}
	for _, c := range []Class{Edit, Test, Git, Housekeeping, Search, Read, Capture, Wait, Spawn, Other} {
		if !seen[c] {
			t.Errorf("class %q missing from Order", c)
		}
	}
}
