// Package callclass labels each tool call of a run with what it did: whether
// it authored file content (edit), ran a test, build or gate (test), changed
// repository state (git), moved or removed files (housekeeping), searched,
// read, captured a command's output into a file (capture), waited, started a
// sub-agent (spawn), or none of these (other). A Bash command is judged by the
// programs it runs, parsed by cmdparse (shell grammar and Python's own ast),
// never by a substring of its text. Design: docs/design.md § Classifying a call.
package callclass

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// Class is the label of a call or of one effect of it.
type Class string

// The alphabet. Primary precedence is the order below: a call that edits and
// tests is an edit, so an edit is never hidden behind a test.
const (
	// Edit: the call authors file content, wherever the file lives.
	Edit Class = "edit"
	// Test: a test, build, typecheck, lint or gate run.
	Test Class = "test"
	// Git: a change of repository state (add, commit, checkout, reset, merge...).
	Git Class = "git"
	// Housekeeping: mkdir, rm, mv, cp, chmod, ln, touch and the like.
	Housekeeping Class = "housekeeping"
	// Search: grep, rg, find, and the Grep and Glob tools.
	Search Class = "search"
	// Read: the Read tool and commands that only print or inspect.
	Read Class = "read"
	// Capture: a command's output redirected into a file that the command did not author.
	Capture Class = "capture"
	// Wait: sleep, polling and monitor tools.
	Wait Class = "wait"
	// Spawn: starting or messaging a sub-agent or chat.
	Spawn Class = "spawn"
	// Other: no class above; Result.Status says whether the call failed to parse.
	Other Class = "other"
)

// Order is the primary precedence, first to last.
var Order = []Class{Edit, Test, Git, Housekeeping, Search, Read, Capture, Wait, Spawn, Other}

// Call is one tool call. Tool is the tool name; Command is the Bash command
// string and is read only for Bash. Cwd is the absolute directory it ran in
// (empty: "/").
type Call struct {
	ID, Tool, Command, Cwd string
}

// Result labels one call. Class is the first of Order that Classes holds.
// Status is "ok", or the cause the command could not be fully parsed (the call
// is then labelled from the parts that did parse, other when none did).
type Result struct {
	Class   Class
	Classes []Class
	Status  string
}

// Options configure Classify. Home is what a leading unquoted `~` expands to;
// Python scans embedded Python (nil: python3 on PATH); Rules is what a project
// adds to the neutral defaults (the zero value adds nothing).
type Options struct {
	Home   string
	Python cmdparse.PythonRunner
	Rules  Rules
}

var toolClass = map[string]Class{
	"Edit": Edit, "Write": Edit, "MultiEdit": Edit, "NotebookEdit": Edit, "apply_patch": Edit,
	"Read": Read, "NotebookRead": Read, "LS": Read,
	"Grep": Search, "Glob": Search,
	"Agent": Spawn, "Task": Spawn, "SendMessage": Spawn,
	"TaskOutput": Wait, "BashOutput": Wait, "Monitor": Wait, "AgentOutputTool": Wait, "KillShell": Wait,
	"TaskStop": Wait, "ScheduleWakeup": Wait,
}

// Classify labels every call, keyed by Call.ID. The error return is for a
// malformed batch (duplicate id) or a cancelled ctx, never for a command that
// did not parse.
func Classify(ctx context.Context, calls []Call, opt Options) (map[string]Result, error) {
	out := make(map[string]Result, len(calls))
	var bash []cmdparse.Call
	for _, c := range calls {
		if _, dup := out[c.ID]; dup {
			return nil, fmt.Errorf("callclass: duplicate call id %q", c.ID)
		}
		if c.Tool == "Bash" || c.Tool == "bash" {
			cwd := c.Cwd
			if !filepath.IsAbs(cwd) {
				cwd = "/"
			}
			bash = append(bash, cmdparse.Call{ID: c.ID, Command: c.Command, Cwd: cwd, Home: opt.Home})
			out[c.ID] = Result{}
			continue
		}
		class, ok := opt.Rules.Tools[c.Tool]
		if !ok {
			class, ok = toolClass[c.Tool]
		}
		if !ok {
			class = Other
		}
		out[c.ID] = Result{Class: class, Classes: []Class{class}, Status: "ok"}
	}
	parsed, err := cmdparse.ParseBatch(ctx, bash, opt.Python)
	if err != nil {
		return nil, fmt.Errorf("callclass: parse %d bash commands: %w", len(bash), err)
	}
	// A command string handed to a wrapper (docker run ... bash -c '...', or a rules wrapper)
	// is judged as a command of its own and its parts join the call's, down to
	// maxWrapDepth wrappers deep (a docker bash -c inside a rules wrapper).
	type level struct {
		id, root, cwd string
		parts         []cmdparse.Part
	}
	var frontier []level
	for _, c := range bash {
		frontier = append(frontier, level{id: c.ID, root: c.ID, cwd: c.Cwd, parts: parsed[c.ID]})
	}
	nested := map[string][][]cmdparse.Part{}
	for depth := 0; depth < maxWrapDepth && len(frontier) > 0; depth++ {
		var inner []cmdparse.Call
		var next []level
		for _, f := range frontier {
			for k, p := range f.parts {
				if cmd := wrapped(p, opt.Rules); cmd != "" {
					id := fmt.Sprintf("%s\x00%d", f.id, k)
					inner = append(inner, cmdparse.Call{ID: id, Command: cmd, Cwd: f.cwd, Home: opt.Home})
					next = append(next, level{id: id, root: f.root, cwd: f.cwd})
				}
			}
		}
		if len(inner) == 0 {
			break
		}
		innerParsed, err := cmdparse.ParseBatch(ctx, inner, opt.Python)
		if err != nil {
			return nil, fmt.Errorf("callclass: parse %d wrapped commands at depth %d: %w", len(inner), depth+1, err)
		}
		for i := range next {
			next[i].parts = innerParsed[next[i].id]
			nested[next[i].root] = append(nested[next[i].root], next[i].parts)
		}
		frontier = next
	}
	for _, c := range bash {
		out[c.ID] = fromParts(parsed[c.ID], nested[c.ID], opt.Rules)
	}
	return out, nil
}

// maxWrapDepth bounds the wrappers looked through inside one another.
const maxWrapDepth = 3

// fromParts labels one Bash call from its parts, in source order, and from the
// parts of the commands its wrappers run (nested).
func fromParts(parts []cmdparse.Part, nested [][]cmdparse.Part, rules Rules) Result {
	held := map[Class]bool{}
	status := "ok"
	for _, group := range append([][]cmdparse.Part{parts}, nested...) {
		for i, p := range group {
			for _, c := range partClasses(p, group[:i], rules) {
				held[c] = true
			}
			if status == "ok" && partFailed(p) {
				status = p.Status
				if p.Error != "" {
					status += ": " + p.Error
				}
			}
		}
	}
	var classes []Class
	for _, c := range Order {
		if held[c] {
			classes = append(classes, c)
		}
	}
	if len(classes) == 0 {
		classes = []Class{Other}
	}
	return Result{Class: classes[0], Classes: classes, Status: status}
}

// partFailed reports whether the parse could not see into the part. A node -e
// part is unparsed by design (its source is scanned for writes instead), so it
// is no failure; a script-body part (python3 - <<EOF, whose body the store
// does not keep) is one, since the script it ran is unknown.
func partFailed(p cmdparse.Part) bool {
	switch p.Status {
	case cmdparse.StatusError, cmdparse.StatusPythonError, cmdparse.StatusPythonUnavailable, cmdparse.StatusScriptBody:
		return true
	case cmdparse.StatusUnparsed:
		return p.Lang != cmdparse.LangNode
	}
	return false
}
