// Command reconcile is a dev-only check, never shipped: scripts/build-release.sh
// builds ./cmd/callmeter alone. It compares every session a callmeter store
// snapshot recorded against the Claude Code transcripts of that session, parsed
// here on their own (the judge is never the thing being judged: nothing of
// internal/callmeter's transcript readers or derivations is reused), and prints
// every mismatch by class.
//
// It prints ids, counts, token numbers, tool and event names only, never a
// command, prompt or message text.
//
// Usage, from the repository root:
//
//	go run ./scripts/reconcile --db /tmp/callmeter/audit/snap/reconcile/callmeter.db
//
// With no --since the window opens at the snapshot's earliest session: all
// history.
//
// A state not yet due at --until (a call still running, a provisional request
// with no fill due, a Bash call no report has parsed yet) prints as PENDING and
// is counted apart: the window's end, never a mismatch.
//
// Exit 0: every mismatch is allowlisted. Exit 1: an unexplained mismatch, or
// no session compared. Exit 2: the check could not run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type config struct {
	db        string
	allowlist string
	projects  []string
	since     int64 // unix ms; unset: the store's earliest session
	sinceSet  bool
	until     int64  // unix ms; 0: the store's latest row
	scratch   string // the directory searched for scratch stores; "": none

	scratchStores     map[string]string // session id -> the scratch store recording it
	scratchRead       int
	scratchUnreadable []string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "the store snapshot to read (a copy; the live store is refused)")
	since := fs.String("since", "", "RFC 3339 time: compare sessions first recorded at or after it, and transcript entries from it on (default: the snapshot's earliest session, all history)")
	until := fs.String("until", "", "RFC 3339 time: ignore transcript entries after it (default: the snapshot's latest row)")
	projects := fs.String("projects", "", "comma-separated Claude Code projects dirs (default: ~/.claude/projects,~/.cc/2/projects,~/.cc/3/projects)")
	scratch := fs.String("scratch", "/tmp/callmeter", "a directory searched for scratch stores (every callmeter.db under it, read-only): a disk session one records by a sessions row or a fault naming it is the expected session-unrecorded; empty: none")
	allow := fs.String("allowlist", filepath.Join("scripts", "reconcile", "allowlist.txt"), "the allowlist file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := buildConfig(*db, *since, *until, *projects, *allow)
	cfg.scratch = *scratch
	if err != nil {
		fmt.Fprintf(stderr, "reconcile: %v\n", err)
		return 2
	}
	rep, err := reconcile(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(stderr, "reconcile: %v\n", err)
		return 2
	}
	return rep.print(stdout)
}

func buildConfig(db, since, until, projects, allow string) (config, error) {
	cfg := config{db: db, allowlist: allow}
	if db == "" {
		return cfg, errors.New("--db is required: a snapshot copy of the store")
	}
	if since != "" {
		s, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return cfg, fmt.Errorf("--since %q: %w", since, err)
		}
		cfg.since = s.UnixMilli()
		cfg.sinceSet = true
	}
	if until != "" {
		u, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return cfg, fmt.Errorf("--until %q: %w", until, err)
		}
		cfg.until = u.UnixMilli()
	}
	if projects == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg, fmt.Errorf("resolve home for the default projects dirs: %w", err)
		}
		cfg.projects = []string{
			filepath.Join(home, ".claude", "projects"),
			filepath.Join(home, ".cc", "2", "projects"),
			filepath.Join(home, ".cc", "3", "projects"),
		}
	} else {
		for _, p := range strings.Split(projects, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.projects = append(cfg.projects, p)
			}
		}
	}
	if err := refuseLiveStore(db); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// refuseLiveStore refuses a path inside the live store's directory: the check
// reads a snapshot copy only, never the store hooks are writing.
func refuseLiveStore(db string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home to guard the live store: %w", err)
	}
	live, err := filepath.EvalSymlinks(filepath.Join(home, ".local", "state", "callmeter"))
	if err != nil {
		return nil // no live store on this host
	}
	abs, err := filepath.Abs(db)
	if err != nil {
		return fmt.Errorf("resolve --db %s: %w", db, err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return fmt.Errorf("resolve --db directory %s: %w", filepath.Dir(abs), err)
	}
	if dir == live {
		return fmt.Errorf("--db %s is in the live store's directory: snapshot it first and pass the copy", db)
	}
	return nil
}

// mismatch is one disagreement between the store and the transcripts.
type mismatch struct {
	class   string
	session string
	ts      int64 // the row's time (unix ms); 0: none, so no `ts<` entry matches it
	ids     []string
	detail  string
	reason  string // the allowlist's reason; empty when unexplained
}

type report struct {
	cfg               config
	sessionsCompared  int
	transcriptsRead   int
	diskUnrecorded    int
	mismatches        []mismatch
	pending           []mismatch // states not yet due at --until; never mismatches
	tokenTranscript   tokens
	tokenStore        tokens
	untrackedMessages int
	untrackedTokens   tokens
	allowUnused       []int // allowlist lines no mismatch matched
}

// add records a mismatch; ts is its row's time, 0 for a class that spans rows
// (a session's event counts, an agent's turn count), which only an id scopes.
func (r *report) add(class, session string, ts int64, detail string, ids ...string) {
	r.mismatches = append(r.mismatches, mismatch{class: class, session: session, ts: ts, ids: ids, detail: detail})
}

// expect records a mismatch a classification in compare.go explains by its
// own evidence, with the reason it prints; no allowlist line is needed.
func (r *report) expect(class, session string, ts int64, reason, detail string, ids ...string) {
	r.mismatches = append(r.mismatches, mismatch{class: class, session: session, ts: ts, ids: ids, detail: detail, reason: reason})
}

// edge records a state not yet due by --until: the window's end, not a
// disagreement, so the allowlist never sees it.
func (r *report) edge(class, session string, ts int64, detail string, ids ...string) {
	r.pending = append(r.pending, mismatch{class: class, session: session, ts: ts, ids: ids, detail: detail})
}

func sortMismatches(ms []mismatch) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if a.session != b.session {
			return a.session < b.session
		}
		return strings.Join(a.ids, ",") < strings.Join(b.ids, ",")
	})
}

func (m mismatch) line() string {
	line := fmt.Sprintf("%s session=%s", m.class, orDash(m.session))
	if len(m.ids) > 0 {
		line += " ids=" + strings.Join(m.ids, ",")
	}
	if m.detail != "" {
		line += " " + m.detail
	}
	if m.ts > 0 {
		line += " at=" + msString(m.ts)
	}
	return line
}

func msString(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

// print writes every mismatch, the per-class counts and the summary line, and
// returns the exit code.
func (r *report) print(w io.Writer) int {
	sortMismatches(r.mismatches)
	sortMismatches(r.pending)
	fmt.Fprintf(w, "reconcile: db %s, window %s .. %s\n", r.cfg.db, msString(r.cfg.since), msString(r.cfg.until))
	type tally struct{ unexplained, expected int }
	byClass := map[string]*tally{}
	var classes []string
	unexplained, expected := 0, 0
	for _, m := range r.mismatches {
		t := byClass[m.class]
		if t == nil {
			t = &tally{}
			byClass[m.class] = t
			classes = append(classes, m.class)
		}
		line := m.line()
		if m.reason != "" {
			t.expected++
			expected++
			fmt.Fprintf(w, "EXPECTED %s: expected: %s\n", line, m.reason)
		} else {
			t.unexplained++
			unexplained++
			fmt.Fprintf(w, "MISMATCH %s\n", line)
		}
	}
	pendingBy := map[string]int{}
	var pendingClasses []string
	for _, m := range r.pending {
		if pendingBy[m.class] == 0 {
			pendingClasses = append(pendingClasses, m.class)
		}
		pendingBy[m.class]++
		fmt.Fprintf(w, "PENDING %s\n", m.line())
	}
	pendingParts := []string{"none"}
	if len(pendingClasses) > 0 {
		pendingParts = nil
		for _, c := range pendingClasses {
			pendingParts = append(pendingParts, fmt.Sprintf("%s=%d", c, pendingBy[c]))
		}
	}
	fmt.Fprintf(w, "reconcile: pending at --until, not yet due: %s\n", strings.Join(pendingParts, " "))
	fmt.Fprintf(w, "tokens (tool-call requests): transcript %s | store %s\n", r.tokenTranscript, r.tokenStore)
	fmt.Fprintf(w, "tokens (requests without a tool call, no store row): %d messages, %s\n", r.untrackedMessages, r.untrackedTokens)
	var parts []string
	for _, c := range classes {
		t := byClass[c]
		s := fmt.Sprintf("%s=%d", c, t.unexplained+t.expected)
		if t.expected > 0 {
			s += fmt.Sprintf("(%d expected)", t.expected)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	unused := "none"
	if len(r.allowUnused) > 0 {
		lines := make([]string, len(r.allowUnused))
		for i, n := range r.allowUnused {
			lines[i] = fmt.Sprint(n)
		}
		unused = strings.Join(lines, " ")
	}
	fmt.Fprintf(w, "reconcile: allowlist lines matching no mismatch: %s\n", unused)
	if r.cfg.scratch != "" {
		bad := "none"
		if len(r.cfg.scratchUnreadable) > 0 {
			bad = strings.Join(r.cfg.scratchUnreadable, ", ")
		}
		fmt.Fprintf(w, "reconcile: scratch stores under %s: %d read, unreadable: %s\n", r.cfg.scratch, r.cfg.scratchRead, bad)
	}
	fmt.Fprintf(w, "reconcile: sessions compared %d, transcripts read %d, unrecorded disk sessions %d, mismatches %d unexplained %d expected; by class: %s\n",
		r.sessionsCompared, r.transcriptsRead, r.diskUnrecorded, unexplained, expected, strings.Join(parts, " "))
	if r.sessionsCompared == 0 {
		fmt.Fprintln(w, "reconcile: FAIL: 0 sessions compared: nothing was checked")
		return 1
	}
	if unexplained > 0 {
		return 1
	}
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
