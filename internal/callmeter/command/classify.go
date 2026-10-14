package command

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter/callclass"
	"github.com/rezzminator/callmeter/internal/paths"
)

// ClassifyUsage is the classify action's usage text.
const ClassifyUsage = `usage: callmeter classify [--rules FILE] < calls.jsonl
  label tool calls by what they did, one JSON object per line in, one per line out, in order:
  in  {"id": "...", "tool": "Bash", "command": "...", "cwd": "/abs/dir"}   (command and cwd for Bash only)
  out {"id": "...", "class": "edit", "classes": ["edit", "test"], "status": "ok"}
  classes: edit test git housekeeping search read capture wait spawn other; class is the first held, in that order
  status: ok, or the cause the command could not be fully parsed (the call is then labelled from its parsed parts)
  --rules FILE: a JSON rules file adding to the built-in defaults; a bad file stops the run with its path and cause
    {"tools": {"<tool name>": "<class>"},
     "wrappers": [{"program": "<name>", "after": ["<word>", ...]}],   the words after those words are a command of their own
     "tests": [{"program": "<name>", "args": ["<word>", ...]}]}       the program run with any of those words after it is a test
    program matches the command's name or the name of any argument (an interpreter or a lock runner in front),
    never an argument of echo, printf or a read, search or housekeeping program`

// classifyIn is one input line of `callmeter classify`.
type classifyIn struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
}

// classifyOut is one output line of `callmeter classify`.
type classifyOut struct {
	ID      string   `json:"id"`
	Class   string   `json:"class"`
	Classes []string `json:"classes"`
	Status  string   `json:"status"`
}

// classifyArgs reads `classify [--rules FILE | --rules=FILE]`: the rules path
// (empty: none given) and whether the arguments are valid.
func classifyArgs(args []string) (string, bool) {
	if len(args) == 0 || args[0] != "classify" {
		return "", false
	}
	var path string
	for i := 1; i < len(args); i++ {
		var value string
		switch {
		case args[i] == "--rules" && i+1 < len(args):
			i++
			value = args[i]
		case strings.HasPrefix(args[i], "--rules="):
			value = strings.TrimPrefix(args[i], "--rules=")
		default:
			return "", false
		}
		if value == "" || path != "" {
			return "", false
		}
		path = value
	}
	return path, true
}

// Classify is `callmeter classify`: callclass.Classify over the calls on
// stdin, the whole input one batch. It touches no store. args starts with
// "classify", then at most one --rules FILE. It returns the process exit code:
// 0 done, 1 an unreadable input line, an unusable rules file or a classify
// error, 2 usage.
func Classify(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv paths.Getenv) int {
	rulesPath, ok := classifyArgs(args)
	if !ok {
		fmt.Fprintln(stderr, ClassifyUsage)
		return 2
	}
	var rules callclass.Rules
	if rulesPath != "" {
		var err error
		if rules, err = callclass.LoadRules(rulesPath); err != nil {
			fmt.Fprintf(stderr, "callmeter classify: %v\n", err)
			return 1
		}
	}
	var ins []classifyIn
	var calls []callclass.Call
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var in classifyIn
		if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
			fmt.Fprintf(stderr, "callmeter classify: input line %d: %v\n", n, err)
			return 1
		}
		if in.ID == "" {
			fmt.Fprintf(stderr, "callmeter classify: input line %d: no id\n", n)
			return 1
		}
		ins = append(ins, in)
		calls = append(calls, callclass.Call{ID: in.ID, Tool: in.Tool, Command: in.Command, Cwd: in.Cwd})
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(stderr, "callmeter classify: read input: %v\n", err)
		return 1
	}
	res, err := callclass.Classify(context.Background(), calls, callclass.Options{Home: getenv("HOME"), Rules: rules})
	if err != nil {
		fmt.Fprintf(stderr, "callmeter classify: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, in := range ins {
		r := res[in.ID]
		out := classifyOut{ID: in.ID, Class: string(r.Class), Status: r.Status}
		for _, c := range r.Classes {
			out.Classes = append(out.Classes, string(c))
		}
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "callmeter classify: write output: %v\n", err)
			return 1
		}
	}
	return 0
}
