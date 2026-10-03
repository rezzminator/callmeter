package cmdparse

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/runner"
)

func TestShellAttribution(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		files   []string
		command string
		want    func(cwd string) []FileRef
	}{
		{
			name:    "compound with a variable",
			files:   []string{"notes.txt"},
			command: `F=notes.txt; wc -l "$F" && grep -n func "$F"`,
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "notes.txt", ActionReadWhole, ""),
					ref(cwd, "notes.txt", ActionSearch, ""),
				}
			},
		},
		{
			// A test asks whether a file exists; it reads nothing. Counting it
			// made every guarded read (`if [ -f x ]; then cat x; fi`) certain.
			name:    "file tests read nothing",
			files:   []string{"a.go"},
			command: `[ -f a.go ] && [[ -s a.go ]]; test -e a.go`,
			want:    func(string) []FileRef { return nil },
		},
		{
			name:    "head piped to tail",
			files:   []string{"a.go"},
			command: `head -n 40 a.go | tail -n 10`,
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "a.go", ActionReadRange, "1,40")}
			},
		},
		{
			name:    "sed line range",
			files:   []string{"a.go"},
			command: `sed -n '10,20p' a.go`,
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "a.go", ActionReadRange, "10,20")}
			},
		},
		{
			name:    "for loop over two files",
			files:   []string{"a.go", "b.go"},
			command: `for f in a.go b.go; do cat "$f"; done`,
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "a.go", ActionReadWhole, ""), ref(cwd, "b.go", ActionReadWhole, "")}
			},
		},
		{
			name:    "glob, redirections and command substitution",
			files:   []string{"a.go", "b.go", "in.txt", "out.txt"},
			command: `wc -l $(ls *.go) < in.txt > out.txt`,
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "a.go", ActionStat, ""),
					ref(cwd, "b.go", ActionStat, ""),
					ref(cwd, "in.txt", ActionReadWhole, ""),
					ref(cwd, "out.txt", ActionWrite, ""),
				}
			},
		},
		{
			// Metadata is not a read: the corpus counted every `ls` and `du`
			// as a read of the file it named.
			name:    "metadata commands stat",
			files:   []string{"a.go", "b.go", "c.go"},
			command: `ls -l a.go; du -sh b.go; stat -c %s c.go; file a.go; realpath b.go; readlink -f c.go`,
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "a.go", ActionStat, ""),
					ref(cwd, "b.go", ActionStat, ""),
					ref(cwd, "c.go", ActionStat, ""),
					ref(cwd, "a.go", ActionStat, ""),
					ref(cwd, "b.go", ActionStat, ""),
					ref(cwd, "c.go", ActionStat, ""),
				}
			},
		},
		{
			// find's operands are its starting points; what follows the first
			// expression word is a pattern, never a file.
			name:    "find stats its starting points only",
			files:   []string{"x", "a.go"},
			command: `find . -name x; find -L a.go -newer x`,
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "a.go", ActionStat, "")}
			},
		},
		{
			name:    "redirects to /dev and fd duplications attribute nothing",
			files:   []string{"x"},
			command: `cat x > /dev/null 2>&1; cat x >/dev/stderr; cat x >&2`,
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "x", ActionReadWhole, ""),
					ref(cwd, "x", ActionReadWhole, ""),
					ref(cwd, "x", ActionReadWhole, ""),
				}
			},
		},
		{
			name:    "redirect targets",
			files:   []string{"x", "o1", "o2", "o3", "o4"},
			command: "cat < x; echo a > o1; echo b >> o2; echo c &> o3; echo d >| o4; cat <<EOF\nx\nEOF",
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "x", ActionReadWhole, ""),
					ref(cwd, "o1", ActionWrite, ""),
					ref(cwd, "o2", ActionWrite, ""),
					ref(cwd, "o3", ActionWrite, ""),
					ref(cwd, "o4", ActionWrite, ""),
				}
			},
		},
		{
			name:    "python script is an exec",
			files:   []string{"script.py"},
			command: `python3 script.py`,
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "script.py", ActionExec, "")}
			},
		},
		{
			name:    "awk inline program skips options and reads operands",
			files:   []string{"data.csv"},
			command: "awk -v FS=, -F , '{print $1}' data.csv",
			want: func(cwd string) []FileRef {
				return []FileRef{ref(cwd, "data.csv", ActionReadWhole, "")}
			},
		},
		{
			name:    "awk file program and every operand are reads",
			files:   []string{"rules.awk", "one.csv", "two.csv"},
			command: "awk -v OFS=, -F , -f rules.awk one.csv two.csv",
			want: func(cwd string) []FileRef {
				return []FileRef{
					ref(cwd, "rules.awk", ActionReadWhole, ""),
					ref(cwd, "one.csv", ActionReadWhole, ""),
					ref(cwd, "two.csv", ActionReadWhole, ""),
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd := fixture(t, tc.files...)
			parts := parseOne(t, cwd, tc.command, nil)
			for _, p := range parts {
				if p.Status != StatusOK {
					t.Fatalf("part %+v: status %q, want ok", p, p.Status)
				}
			}
			assertFiles(t, parts, tc.want(cwd))
		})
	}
}

// A known reader or writer naming a path that does not exist yet (a scratch
// file, a heredoc's write target) attributes it with Exists false; the corpus
// lost every `cat > f <<EOF` write. An unknown program's words, a metadata
// look and a glob that matched nothing stay unattributed.
func TestMissingFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		want    func(cwd string) []FileRef
	}{
		{"heredoc write target", "cat > f <<EOF\nx\nEOF", func(cwd string) []FileRef {
			return []FileRef{missing(cwd, "f", ActionWrite, "")}
		}},
		{"append target", "cat >> f", func(cwd string) []FileRef {
			return []FileRef{missing(cwd, "f", ActionWrite, "")}
		}},
		{"sed range of a gone file", "sed -n 1,5p gone.txt", func(cwd string) []FileRef {
			return []FileRef{missing(cwd, "gone.txt", ActionReadRange, "1,5")}
		}},
		{
			"readers and input redirect",
			"head -n 3 a; grep x b; wc -l c; sed -i s/a/b/ d; cat < e",
			func(cwd string) []FileRef {
				return []FileRef{
					missing(cwd, "a", ActionReadRange, "1,3"),
					missing(cwd, "b", ActionSearch, ""),
					missing(cwd, "c", ActionReadWhole, ""),
					missing(cwd, "d", ActionWrite, ""),
					missing(cwd, "e", ActionReadWhole, ""),
				}
			},
		},
		{"a missing file beside an existing one", "cat have.txt gone.txt", func(cwd string) []FileRef {
			return []FileRef{ref(cwd, "have.txt", ActionReadWhole, ""), missing(cwd, "gone.txt", ActionReadWhole, "")}
		}},
		{"unknown program", "foo gone.txt", func(string) []FileRef { return nil }},
		{"metadata look", "ls gone", func(string) []FileRef { return nil }},
		{"glob that matched nothing", "cat *.nomatch", func(string) []FileRef { return nil }},
		{"stdin and a directory", "cat - sub; tail -n 2 -", func(string) []FileRef { return nil }},
		{"dev targets", "cat x > /dev/nowhere-such", func(cwd string) []FileRef {
			return []FileRef{missing(cwd, "x", ActionReadWhole, "")}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cwd := fixture(t, "have.txt")
			if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
				t.Fatalf("mkdir sub: %v", err)
			}
			assertFiles(t, parseOne(t, cwd, tc.command, nil), tc.want(cwd))
		})
	}
}

// TestTildeAndPlaceholders covers the false missing files real Claude
// sessions showed: a `~` joined to the cwd as a literal directory (756 rows), and text
// the shell never expanded taken as a path — a `$X` inside a Python string,
// xargs's `{}`, a Python glob pattern.
func TestTildeAndPlaceholders(t *testing.T) {
	t.Parallel()
	home := fixture(t, ".zsh_history")
	cwd := fixture(t)
	hist := filepath.Join(home, ".zsh_history")
	cases := []struct {
		name, home, command string
		want                []FileRef
	}{
		{
			name:    "a leading tilde is the home directory",
			home:    home,
			command: `tail -n 5 ~/.zsh_history ~/gone.txt; F=~/.zsh_history; wc -l "$F"`,
			want: []FileRef{
				{Path: hist, Action: ActionReadRange, Range: "-5", Exists: true},
				{Path: filepath.Join(home, "gone.txt"), Action: ActionReadRange, Range: "-5"},
				{Path: hist, Action: ActionReadWhole, Exists: true},
			},
		},
		{name: "no home: a tilde path attributes nothing", command: `cat ~/.zsh_history`},
		{name: "another user's home attributes nothing", home: home, command: `cat ~root/.profile`},
		{
			name:    "xargs placeholder is never a file",
			home:    home,
			command: `ls | xargs -I{} sh -c 'git show HEAD:{} > /tmp/q/{}'`,
		},
		{name: "Python string with an unexpanded $X", home: home, command: `python3 -c "open('$P/package.json')"`},
		{name: "Python glob pattern", home: home, command: `python3 -c "open('tmp/*/package.json')"`},
		{
			name:    "Python expanduser string",
			home:    home,
			command: `python3 -c "import os; print(open(os.path.expanduser('~/.zsh_history')).read())"`,
			want:    []FileRef{{Path: hist, Action: ActionUnknown, Exists: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBatch(context.Background(),
				[]Call{{ID: "c1", Command: tc.command, Cwd: cwd, Home: tc.home}},
				Python3{Runner: runner.Real{}})
			if err != nil {
				t.Fatalf("ParseBatch(%q): %v", tc.command, err)
			}
			assertFiles(t, got["c1"], tc.want)
		})
	}
}

// parseWithin runs ParseBatch on one call in its own goroutine and fails the
// test once it runs past bound, so a parse that never returns still fails.
func parseWithin(t *testing.T, cwd, command string, bound time.Duration) []Part {
	t.Helper()
	type result struct {
		parts map[string][]Part
		err   error
	}
	done := make(chan result, 1)
	go func() {
		parts, err := ParseBatch(context.Background(), []Call{{ID: "c1", Command: command, Cwd: cwd, Home: cwd}}, failingPython{})
		done <- result{parts, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ParseBatch(%q): %v", command, r.err)
		}
		return r.parts["c1"]
	case <-time.After(bound):
		t.Fatalf("ParseBatch(%q) still running after %v", command, bound)
		return nil
	}
}

// TestGlobOverFilesystemRootReturnsInBound is the input class FuzzParse
// stalled on: `/*/*/*/*/a` (four levels, 0.2 s alone, over 1 s under ten
// fuzz workers) made the glob read every directory under the filesystem
// root; six levels (seed d32551b2ba7a921b) take seconds alone.
func TestGlobOverFilesystemRootReturnsInBound(t *testing.T) {
	t.Parallel()
	parts := parseWithin(t, fixture(t), "/*/*/*/*/*/*/a", time.Second)
	if len(parts) == 0 {
		t.Fatal("no parts")
	}
	for _, part := range parts {
		if part.Status == "" {
			t.Errorf("part %d (%s) carries no status", part.Seq, part.Program)
		}
	}
}

// A glob whose directory read blocks (macOS holding a stat behind a privacy
// prompt or an automount) stays as written once the call's maxGlobTime is
// spent, its part naming the bound; the parse never waits on the read. Not
// parallel: it replaces globReadDir.
func TestGlobOverTimeBoundStaysAsWritten(t *testing.T) {
	cwd := fixture(t, "top.txt")
	held := filepath.Join(cwd, "held")
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", held, err)
	}
	release := make(chan struct{})
	defer close(release)
	orig := globReadDir
	t.Cleanup(func() { globReadDir = orig })
	globReadDir = func(dir string) ([]string, bool) {
		if dir == held {
			<-release
		}
		return orig(dir)
	}
	parts := parseWithin(t, cwd, "cat *.txt held/*", time.Second)
	if len(parts) != 1 {
		t.Fatalf("parts = %+v, want one cat part", parts)
	}
	got := parts[0]
	if !reflect.DeepEqual(got.Args, []string{"top.txt", "held/*"}) || !strings.Contains(got.Error, "glob held/* over "+maxGlobTime.String()) {
		t.Errorf("part = %+v, want top.txt expanded, held/* as written, the time bound named in Error", got)
	}
}

func TestLookupOverTimeBoundStaysUnattributed(t *testing.T) {
	cwd := fixture(t, "held.txt", "later.txt")
	release, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	orig := lookupStat
	var laterCalls atomic.Int32
	lookupStat = func(path string) (fs.FileInfo, error) {
		if filepath.Base(path) == "held.txt" {
			defer close(finished)
			<-release
		} else {
			laterCalls.Add(1)
		}
		return orig(path)
	}
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		<-finished
		lookupStat = orig
	})
	type result struct {
		parts map[string][]Part
		err   error
	}
	done := make(chan result, 1)
	go func() {
		parts, err := ParseBatch(context.Background(), []Call{{ID: "c1", Cwd: cwd, Command: "cat held.txt later.txt"}}, failingPython{})
		done <- result{parts, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		once.Do(func() { close(release) })
		<-done
		t.Fatal("parse still waiting for stat after 1s")
	}
	if got.err != nil || len(got.parts["c1"]) != 1 {
		t.Fatalf("parse error = %v, part count = %d", got.err, len(got.parts["c1"]))
	}
	part := got.parts["c1"][0]
	want := "stat held.txt over " + maxGlobTime.String() + ", left unattributed; stat later.txt over " + maxGlobTime.String() + ", left unattributed"
	if part.Status != StatusOK || len(part.Files) != 0 || part.Error != want {
		t.Errorf("status = %q, files = %d, error = %q, want ok, no files, %q", part.Status, len(part.Files), part.Error, want)
	}
	if got := laterCalls.Load(); got != 0 {
		t.Errorf("later stat called %d times after deadline, want none", got)
	}
}

func TestLookupOrdinaryStat(t *testing.T) {
	cwd := fixture(t, "have.txt")
	if err := os.Mkdir(filepath.Join(cwd, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := lookupStat
	t.Cleanup(func() { lookupStat = orig })
	lookupStat = func(path string) (fs.FileInfo, error) {
		if filepath.Base(path) == "denied.txt" {
			return nil, fs.ErrPermission
		}
		return orig(path)
	}
	cases := []struct {
		name, val  string
		missingOK  bool
		path       string
		exists, ok bool
		note       string
	}{
		{"regular", "have.txt", false, filepath.Join(cwd, "have.txt"), true, true, ""},
		{"directory", "dir", false, filepath.Join(cwd, "dir"), true, false, ""},
		{"missing allowed", "gone.txt", true, filepath.Join(cwd, "gone.txt"), false, true, ""},
		{"missing excluded", "gone.txt", false, filepath.Join(cwd, "gone.txt"), false, false, ""},
		{"not a directory", "have.txt/child", true, "", false, false, ""},
		{"name too long", strings.Repeat("x", 256), true, "", false, false, ""},
		{"other error", "denied.txt", true, "", false, false, "stat " + filepath.Join(cwd, "denied.txt") + ": " + fs.ErrPermission.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &callParser{dir: cwd, globLookups: maxGlobLookups}
			path, exists, ok := p.lookup(tc.val, tc.missingOK)
			if path != tc.path || exists != tc.exists || ok != tc.ok || strings.Join(p.notes, "; ") != tc.note {
				t.Errorf("lookup = (%q, %v, %v), note = %q", path, exists, ok, strings.Join(p.notes, "; "))
			}
			if p.globLookups != maxGlobLookups {
				t.Errorf("stat changed lookup budget to %d", p.globLookups)
			}
		})
	}
}

// delayedPython returns the synthetic PyResult shape from python.go: each
// snippet ID and a Strings constant, as pyscan.py emits for a string literal.
type delayedPython struct{ delay time.Duration }

func (p delayedPython) Analyze(_ context.Context, snippets []Snippet) ([]PyResult, error) {
	time.Sleep(p.delay)
	results := make([]PyResult, len(snippets))
	for i, snippet := range snippets {
		results[i] = PyResult{ID: snippet.ID, Strings: []string{"python.txt"}}
	}
	return results, nil
}

func TestLookupDeadlinePausesWhilePythonRuns(t *testing.T) {
	cwd := fixture(t, "shell.txt", "python.txt")
	parts, err := ParseBatch(context.Background(), []Call{{ID: "c1", Cwd: cwd, Command: `cat shell.txt; python3 -c '"python.txt"'`}}, delayedPython{delay: maxGlobTime + 50*time.Millisecond})
	if err != nil || len(parts["c1"]) != 2 {
		t.Fatalf("parse error = %v, part count = %d", err, len(parts["c1"]))
	}
	assertFiles(t, parts["c1"][:1], []FileRef{ref(cwd, "shell.txt", ActionReadWhole, "")})
	assertFiles(t, parts["c1"][1:], []FileRef{ref(cwd, "python.txt", ActionUnknown, "")})
	if part := parts["c1"][1]; part.Status != StatusOK || part.Error != "" {
		t.Errorf("Python status = %q, error = %q", part.Status, part.Error)
	}
}

func TestLookupSpentDeadlineStaysSpentAcrossPython(t *testing.T) {
	cwd := fixture(t, "held.txt", "python.txt")
	release, finished := make(chan struct{}), make(chan struct{})
	orig := lookupStat
	lookupStat = func(path string) (fs.FileInfo, error) {
		if filepath.Base(path) == "held.txt" {
			defer close(finished)
			<-release
		}
		return orig(path)
	}
	t.Cleanup(func() {
		close(release)
		<-finished
		lookupStat = orig
	})
	parts := parseOne(t, cwd, `cat held.txt; python3 -c '"python.txt"'`, delayedPython{delay: maxGlobTime + 50*time.Millisecond})
	if len(parts) != 2 {
		t.Fatalf("part count = %d, want two", len(parts))
	}
	for i, val := range []string{"held.txt", "python.txt"} {
		want := "stat " + val + " over " + maxGlobTime.String() + ", left unattributed"
		if part := parts[i]; part.Status != StatusOK || len(part.Files) != 0 || part.Error != want {
			t.Errorf("part %d: status = %q, files = %d, error = %q, want %q", i, part.Status, len(part.Files), part.Error, want)
		}
	}
}

func TestIOPanicReachesTheParseGoroutine(t *testing.T) {
	for _, kind := range []string{"glob", "stat"} {
		t.Run(kind, func(t *testing.T) {
			cwd := fixture(t, "file.txt")
			origDir, origStat := globReadDir, lookupStat
			t.Cleanup(func() { globReadDir, lookupStat = origDir, origStat })
			command := "cat file.txt"
			if kind == "glob" {
				command = "cat *.txt"
				globReadDir = func(string) ([]string, bool) { panic("boom") }
			} else {
				lookupStat = func(string) (fs.FileInfo, error) { panic("boom") }
			}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_, _ = ParseBatch(context.Background(), []Call{{ID: "c1", Cwd: cwd, Command: command}}, failingPython{})
			}()
			text := fmt.Sprintf("%v", recovered)
			if !strings.Contains(text, "boom\ngoroutine ") || !strings.Contains(text, "TestIOPanicReachesTheParseGoroutine") {
				t.Error("caller did not recover boom with the IO goroutine's stack")
			}
		})
	}
}

type ioPanicBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	writes int
	done   chan struct{}
}

func (b *ioPanicBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(data)
	b.writes++
	b.mu.Unlock()
	b.done <- struct{}{}
	return n, err
}

func TestIOPanicAfterTheDeadlineIsWrittenAndDropped(t *testing.T) {
	for _, kind := range []string{"glob", "stat"} {
		t.Run(kind, func(t *testing.T) {
			cwd := fixture(t, "file.txt")
			release := make(chan struct{})
			var once sync.Once
			origDir, origStat, origOut := globReadDir, lookupStat, ioPanicOut
			out := &ioPanicBuffer{done: make(chan struct{}, 2)}
			ioPanicOut = out
			t.Cleanup(func() {
				once.Do(func() { close(release) })
				globReadDir, lookupStat, ioPanicOut = origDir, origStat, origOut
			})
			command, want := "cat file.txt", "stat file.txt over "+maxGlobTime.String()+", left unattributed"
			if kind == "glob" {
				command, want = "cat *.txt", "glob *.txt over "+maxGlobTime.String()+", left unexpanded; stat *.txt over "+maxGlobTime.String()+", left unattributed"
				globReadDir = func(string) ([]string, bool) { <-release; panic("boom") }
			} else {
				lookupStat = func(string) (fs.FileInfo, error) { <-release; panic("boom") }
			}
			parts := parseWithin(t, cwd, command, time.Second)
			if len(parts) != 1 || parts[0].Status != StatusOK || len(parts[0].Files) != 0 || parts[0].Error != want {
				t.Error("abandoned IO did not leave the complete bound note on an ok part")
			}
			once.Do(func() { close(release) })
			select {
			case <-out.done:
			case <-time.After(time.Second):
				t.Fatal("late panic was not written within 1s")
			}
			out.mu.Lock()
			defer out.mu.Unlock()
			text := out.buf.String()
			if out.writes != 1 || !strings.HasPrefix(text, "callmeter: a parse file read panicked after its deadline: boom\ngoroutine ") || !strings.Contains(text, "TestIOPanicAfterTheDeadlineIsWrittenAndDropped") {
				t.Error("late panic was not written once with boom and its IO stack")
			}
		})
	}
}

// A glob reading more than maxGlobEntries directory entries stays as
// written, its part naming the bound. Not parallel: it shrinks
// maxGlobEntries.
func TestGlobOverEntryBoundStaysAsWritten(t *testing.T) {
	cwd := fixture(t, "a1", "a2", "a3", "b1", "b2")
	orig := maxGlobEntries
	t.Cleanup(func() { maxGlobEntries = orig })
	maxGlobEntries = 4
	parts := parseWithin(t, cwd, "cat a*", time.Second)
	if len(parts) != 1 {
		t.Fatalf("parts = %+v, want one cat part", parts)
	}
	if got := parts[0]; !reflect.DeepEqual(got.Args, []string{"a*"}) || !strings.Contains(got.Error, "glob a* over 4 directory entries") {
		t.Errorf("part = %+v, want a* as written, the entry bound named in Error", got)
	}
}

// The globs of one call share maxGlobLookups directory reads and stats: a
// glob whose expansion needs more stays as written, like a pattern that
// matched nothing, and its part names the gap in Error; a glob earlier in the
// call, within the bound, expands as before.
func TestGlobOverLookupBoundStaysAsWritten(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "top.txt")
	for i := 0; i <= maxGlobLookups/2; i++ {
		dir := filepath.Join(cwd, fmt.Sprintf("d%04d", i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("line\n"), 0o600); err != nil {
			t.Fatalf("write %s/f: %v", dir, err)
		}
	}
	parts := parseWithin(t, cwd, "cat *.txt; cat */f", time.Second)
	if len(parts) != 2 {
		t.Fatalf("parts = %+v, want two cat parts", parts)
	}
	assertFiles(t, parts[:1], []FileRef{ref(cwd, "top.txt", ActionReadWhole, "")})
	if got := parts[1]; got.Status != StatusOK || len(got.Files) != 0 || !reflect.DeepEqual(got.Args, []string{"*/f"}) ||
		!strings.Contains(got.Error, "glob */f over") {
		t.Errorf("part 1 = %+v, want cat with */f as written, no file, the bound named in Error", got)
	}
}
