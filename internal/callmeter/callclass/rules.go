package callclass

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Rules is what a project adds to the classifier's neutral defaults: tool
// names mapped to classes, wrapper programs whose command string is judged as
// a command of its own, and programs that are tests. The zero value adds
// nothing. A rules file is JSON:
//
//	{
//	  "tools":    {"mcp__x__start": "spawn"},
//	  "wrappers": [{"program": "run-in-box", "after": ["exec"]}],
//	  "tests":    [{"program": "tasks.sh", "args": ["verify", "lint"]}]
//	}
type Rules struct {
	// Tools maps a tool name to a class; an entry beats the built-in mapping.
	Tools map[string]Class `json:"tools"`
	// Wrappers name the programs that run the command string after a word sequence.
	Wrappers []Wrapper `json:"wrappers"`
	// Tests name the programs that are tests when given one of the words.
	Tests []TestRule `json:"tests"`
}

// Wrapper is a program that runs a command: the words after the consecutive
// After words of its arguments are the command string, parsed as shell.
// Program matches the basename of the command or of any argument, so an
// interpreter or a lock runner in front of the program still matches.
type Wrapper struct {
	Program string   `json:"program"`
	After   []string `json:"after"`
}

// TestRule is a program run with any of Args among its arguments: a test, build
// or gate. Program matches as a Wrapper's does.
type TestRule struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
}

// maxRulesBytes caps a rules file: a project's names fit in far less.
const maxRulesBytes = 1 << 20

// LoadRules reads and validates a rules file. Every failure names the path and
// the cause: an unreadable, non-regular or oversized file, malformed JSON or
// trailing bytes, an unknown field, an unknown class, an empty name, a second
// document, a file naming no rule. It never returns an empty rule set.
func LoadRules(path string) (Rules, error) {
	raw, err := readRules(path)
	if err != nil {
		return Rules{}, fmt.Errorf("rules %s: %w", path, err)
	}
	var r Rules
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Rules{}, fmt.Errorf("rules %s: %w", path, err)
	}
	var next json.RawMessage
	switch err := dec.Decode(&next); {
	case err == nil:
		return Rules{}, fmt.Errorf("rules %s: more than one JSON document", path)
	case !errors.Is(err, io.EOF):
		return Rules{}, fmt.Errorf("rules %s: after the document: %w", path, err)
	}
	if len(r.Tools) == 0 && len(r.Wrappers) == 0 && len(r.Tests) == 0 {
		return Rules{}, fmt.Errorf("rules %s: names no rule (keys: tools, wrappers, tests)", path)
	}
	if err := r.validate(); err != nil {
		return Rules{}, fmt.Errorf("rules %s: %w", path, err)
	}
	return r, nil
}

// readRules reads a regular file of at most maxRulesBytes. A FIFO or a
// device (/dev/stdin) is refused before it is opened, since reading one would
// block the run or take the calls meant for stdin.
func readRules(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	if info.Size() > maxRulesBytes {
		return nil, fmt.Errorf("larger than %d bytes (%d)", maxRulesBytes, info.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRulesBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxRulesBytes {
		return nil, fmt.Errorf("larger than %d bytes (grew while read)", maxRulesBytes)
	}
	return raw, nil
}

func (r Rules) validate() error {
	for tool, class := range r.Tools {
		switch {
		case tool == "":
			return errors.New("tools: empty tool name")
		case tool == "Bash" || tool == "bash":
			return fmt.Errorf("tools: %q is judged by its command, never mapped to a class", tool)
		case !slices.Contains(Order, class):
			return fmt.Errorf("tools: %q maps to unknown class %q (classes: %s)", tool, class, classList())
		}
	}
	for i, w := range r.Wrappers {
		if w.Program == "" || strings.ContainsRune(w.Program, '/') {
			return fmt.Errorf("wrappers[%d]: program %q is not a bare program name", i, w.Program)
		}
		if err := checkWords(fmt.Sprintf("wrappers[%d].after", i), w.After); err != nil {
			return err
		}
	}
	for i, t := range r.Tests {
		if t.Program == "" || strings.ContainsRune(t.Program, '/') {
			return fmt.Errorf("tests[%d]: program %q is not a bare program name", i, t.Program)
		}
		if err := checkWords(fmt.Sprintf("tests[%d].args", i), t.Args); err != nil {
			return err
		}
	}
	return nil
}

func checkWords(where string, words []string) error {
	if len(words) == 0 {
		return fmt.Errorf("%s: no words", where)
	}
	if slices.Contains(words, "") {
		return fmt.Errorf("%s: empty word", where)
	}
	return nil
}

func classList() string {
	names := make([]string, len(Order))
	for i, c := range Order {
		names[i] = string(c)
	}
	return strings.Join(names, " ")
}

// matchesProgram reports whether program is the command's basename or the
// basename of one of its arguments (an interpreter or a lock runner in front),
// and the index of the matching argument, -1 for the command itself. A command
// that only prints, reads, searches or moves its arguments (echo, cat, grep,
// cp) never runs the program it names.
func matchesProgram(program, base string, args []string) (int, bool) {
	if base == program {
		return -1, true
	}
	if namesOnly(base) {
		return 0, false
	}
	for i, a := range args {
		if filepath.Base(a) == program {
			return i, true
		}
	}
	return 0, false
}

// namesOnly is a program that prints, reads, searches or moves its
// arguments: a program named among them is an operand, never run. env and
// command run theirs.
func namesOnly(base string) bool {
	return base == "echo" || base == "printf" || searchPrograms[base] || housekeepingPrograms[base] ||
		(readPrograms[base] && base != "env" && base != "command")
}

// wrapperSpan finds a rules wrapper among args: the index where its After
// words start and the index where its command string starts.
func (r Rules) wrapperSpan(base string, args []string) (after, cmd int, ok bool) {
	for _, w := range r.Wrappers {
		at, ok := matchesProgram(w.Program, base, args)
		if !ok {
			continue
		}
		for i := at + 1; i+len(w.After) <= len(args); i++ {
			if slices.Equal(args[i:i+len(w.After)], w.After) {
				return i, i + len(w.After), true
			}
		}
	}
	return 0, 0, false
}

// wrapped is the command string a rules wrapper runs, empty when none does.
func (r Rules) wrapped(base string, args []string) string {
	if _, cmd, ok := r.wrapperSpan(base, args); ok {
		return strings.Join(args[cmd:], " ")
	}
	return ""
}

// testRun reports a rules test: a rule's program, matched as a Wrapper's is,
// run with one of the rule's words among the arguments after it. The command
// string of a rules wrapper is no argument of the wrapper: it is judged on its
// own, so only the words before the wrapper's After words count.
func (r Rules) testRun(base string, args []string) bool {
	if after, _, ok := r.wrapperSpan(base, args); ok {
		args = args[:after]
	}
	for _, t := range r.Tests {
		if at, ok := matchesProgram(t.Program, base, args); ok && anyOf(args[at+1:], t.Args...) {
			return true
		}
	}
	return false
}
