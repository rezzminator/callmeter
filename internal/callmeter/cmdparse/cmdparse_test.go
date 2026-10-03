package cmdparse

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rezzminator/callmeter/internal/runner"
)

// fixture makes a cwd holding the named files.
func fixture(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("line\n"), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

// parseOne runs ParseBatch on one call through the real python3 on PATH.
// python3 is required; its absence fails the test, never skips it.
func parseOne(t *testing.T, cwd, command string, py PythonRunner) []Part {
	t.Helper()
	if py == nil {
		py = Python3{Runner: runner.Real{}}
	}
	got, err := ParseBatch(context.Background(), []Call{{ID: "c1", Command: command, Cwd: cwd}}, py)
	if err != nil {
		t.Fatalf("ParseBatch(%q): %v", command, err)
	}
	return got["c1"]
}

func ref(cwd, name, action, rng string) FileRef {
	return FileRef{Path: filepath.Join(cwd, name), Action: action, Range: rng, Exists: true}
}

// missing is a reference to a path that did not exist at parse time.
func missing(cwd, name, action, rng string) FileRef {
	return FileRef{Path: filepath.Join(cwd, name), Action: action, Range: rng}
}

// allFiles flattens every part's files in order.
func allFiles(parts []Part) []FileRef {
	var out []FileRef
	for i := range parts {
		out = append(out, parts[i].Files...)
	}
	return out
}

func assertFiles(t *testing.T, parts []Part, want []FileRef) {
	t.Helper()
	if got := allFiles(parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %+v\nwant   %+v\nparts = %+v", got, want, parts)
	}
}

func TestShellParseErrorIsOneErrorPart(t *testing.T) {
	t.Parallel()
	parts := parseOne(t, fixture(t), `echo "unterminated`, nil)
	if len(parts) != 1 || parts[0].Status != StatusError || parts[0].Error == "" {
		t.Fatalf("parts = %+v, want one error part with the parser's message", parts)
	}
}

// An apostrophe inside a heredoc inside a single-quoted bash -c string closes
// the quote, leaving the rest of the line as shell words; a `(` there is a
// syntax error bash -n reports too (`line 3: syntax error near unexpected
// token '('`), so the parse is one error part at that line, never ok.
func TestQuoteClosedByApostropheIsBashsError(t *testing.T) {
	t.Parallel()
	command := "bash -c '\ncat > /tmp/n <<EOF\n- it's a note (x)\nEOF\n'"
	parts := parseOne(t, fixture(t), command, nil)
	if len(parts) != 1 || parts[0].Status != StatusError || !strings.HasPrefix(parts[0].Error, "3:") {
		t.Fatalf("parts = %+v, want one error part at line 3, as bash -n reports", parts)
	}
}

func TestNodeEvalIsUnparsed(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x.js")
	parts := parseOne(t, cwd, `node -e "require('fs').readFileSync('x.js')"`, nil)
	if len(parts) != 1 || parts[0].Lang != LangNode || parts[0].Status != StatusUnparsed || len(parts[0].Files) != 0 {
		t.Fatalf("parts = %+v, want one unparsed node part with no files", parts)
	}
}

func TestParseBatchRejectsRelativeCwd(t *testing.T) {
	t.Parallel()
	if _, err := ParseBatch(context.Background(), []Call{{ID: "a", Command: "true", Cwd: "rel"}}, nil); err == nil {
		t.Fatal("ParseBatch accepted a relative cwd")
	}
}

func TestConditionalParts(t *testing.T) {
	t.Parallel()
	type want struct {
		program     string
		conditional bool
	}
	cases := []struct {
		name    string
		command string
		want    []want
	}{
		{
			name:    "if test is certain, its branches are not",
			command: `if [ -f x ]; then cat x; else echo MISSING; fi`,
			want:    []want{{"[", false}, {"cat", true}, {"echo", true}},
		},
		{
			name:    "elif condition and every branch are conditional; after fi is certain",
			command: `if true; then a; elif b; then c; else d; fi; wc -l z`,
			want:    []want{{"true", false}, {"a", true}, {"b", true}, {"c", true}, {"d", true}, {"wc", false}},
		},
		{
			name:    "right of && and || is conditional",
			command: `a && cat y | wc -l; b || c`,
			want:    []want{{"a", false}, {"cat", true}, {"wc", true}, {"b", false}, {"c", true}},
		},
		{
			name:    "sequence is certain",
			command: `cat z; wc -l z`,
			want:    []want{{"cat", false}, {"wc", false}},
		},
		{
			name:    "case arms are conditional, the case word is not",
			command: `case $(uname) in Linux) cat l;; *) cat o;; esac`,
			want:    []want{{"uname", false}, {"cat", true}, {"cat", true}},
		},
		{
			name:    "python heredoc inside an if branch",
			command: "if [ -f s.txt ]; then python3 - <<'EOF'\nopen('s.txt').read()\nEOF\nfi",
			want:    []want{{"[", false}, {"python3", true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts := parseOne(t, fixture(t, "x", "y", "z", "s.txt"), tc.command, nil)
			var got []want
			for _, p := range parts {
				if p.Status != StatusOK {
					t.Fatalf("part %+v is not ok", p)
				}
				got = append(got, want{p.Program, p.Conditional})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parts = %+v\nwant    %+v", got, tc.want)
			}
		})
	}
}

// TestForLoopItemsGlobExpand: a for loop's items expand as any unquoted word
// does. A quoted glob stays literal; a glob that matched nothing binds the
// word as written and, through the loop variable, is never a missing file.
func TestForLoopItemsGlobExpand(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "a.md", "b.md", "c.txt")
	cases := []struct {
		name    string
		command string
		want    []FileRef
	}{
		{
			name:    "glob items expand against the directory",
			command: `for f in *.md; do grep x "$f"; done`,
			want:    []FileRef{ref(cwd, "a.md", ActionSearch, ""), ref(cwd, "b.md", ActionSearch, "")},
		},
		{
			name:    "an unmatched glob is not a missing file",
			command: `for f in *.log; do cat "$f"; done`,
		},
		{
			name:    "an unmatched glob beside a literal item",
			command: `for f in *.log c.txt; do cat "$f"; done`,
			want:    []FileRef{ref(cwd, "c.txt", ActionReadWhole, "")},
		},
		{
			name:    "an unmatched glob survives concatenation",
			command: `for f in *.log; do cat "$f.bak"; done`,
		},
		{
			// Unexpanded, so not a.md; and a literal "*.md" that does not exist is a
			// pattern nobody expanded, never a missing file.
			name:    "a quoted glob stays literal",
			command: `for f in "*.md"; do cat "$f"; done`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFiles(t, parseOne(t, cwd, tc.command, nil), tc.want)
		})
	}
}

// TestArrayKeepsUnmatchedGlob: an array element built from a glob that
// matched nothing is never a missing file, as for a loop variable; matched
// globs and literal missing paths are attributed as any argument is.
func TestArrayKeepsUnmatchedGlob(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "a.go", "b.go")
	cases := []struct {
		name    string
		command string
		want    []FileRef
	}{
		{"an unmatched glob element attributes nothing", `A=(*.nomatch); cat "${A[@]}"`, nil},
		{"an unmatched glob element through one index", `A=(*.nomatch); cat "${A[0]}"`, nil},
		{"an unmatched glob element survives concatenation", `A=(*.nomatch); cat "${A[@]}.bak"`, nil},
		{
			"an unmatched element beside a missing literal", `A=(*.nomatch gone.txt); cat "${A[@]}"`,
			[]FileRef{missing(cwd, "gone.txt", ActionReadWhole, "")},
		},
		{
			"a matched glob attributes its matches", `A=(*.go); cat "${A[@]}"`,
			[]FileRef{ref(cwd, "a.go", ActionReadWhole, ""), ref(cwd, "b.go", ActionReadWhole, "")},
		},
		{
			"a literal missing element is a missing file", `A=(gone.txt); cat "${A[@]}"`,
			[]FileRef{missing(cwd, "gone.txt", ActionReadWhole, "")},
		},
		{
			"a reassigned scalar drops the mark", `A=(*.nomatch); A=gone.txt; cat "$A"`,
			[]FileRef{missing(cwd, "gone.txt", ActionReadWhole, "")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFiles(t, parseOne(t, cwd, tc.command, nil), tc.want)
		})
	}
}

// TestUnquotedBackslashEscapes: an unquoted `\X` is X, as the shell passes
// it; an escaped glob character is literal; single quotes keep the backslash.
func TestUnquotedBackslashEscapes(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x y", "a.md", "*.md")
	if err := os.MkdirAll(filepath.Join(cwd, "Code Logs"), 0o700); err != nil {
		t.Fatalf("mkdir Code Logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "Code Logs", "y"), []byte("line\n"), 0o600); err != nil {
		t.Fatalf("write Code Logs/y: %v", err)
	}
	cases := []struct {
		name    string
		command string
		want    []FileRef
	}{
		{"an escaped space joins the word", `cat x\ y`, []FileRef{ref(cwd, "x y", ActionReadWhole, "")}},
		{
			"a cd to an escaped name moves there",
			`cd Code\ Logs && cat y`,
			[]FileRef{ref(cwd, "Code Logs/y", ActionReadWhole, "")},
		},
		{"an escaped glob character is literal", `cat \*.md`, []FileRef{ref(cwd, "*.md", ActionReadWhole, "")}},
		{"single quotes keep the backslash", `cat 'x\ y'`, []FileRef{missing(cwd, `x\ y`, ActionReadWhole, "")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertFiles(t, parseOne(t, cwd, tc.command, nil), tc.want)
		})
	}
}

// TestWordsExpandingToNothing: a command whose every word expands to nothing
// runs no program; a redirection on it still opens its file.
func TestWordsExpandingToNothing(t *testing.T) {
	t.Parallel()
	cwd := fixture(t)
	for _, command := range []string{`present=()&"${present[*]}"0`, `x=(); "${x[@]}" > out`} {
		parts := parseOne(t, cwd, command, nil)
		for _, part := range parts {
			if part.Program != "" {
				t.Errorf("%q: part %+v runs a program, want none", command, part)
			}
			if part.Status != StatusOK {
				t.Errorf("%q: part %+v has status %q, want %q", command, part, part.Status, StatusOK)
			}
		}
		if command == `x=(); "${x[@]}" > out` {
			if len(parts) != 1 || len(parts[0].Files) != 1 || parts[0].Files[0].Path != filepath.Join(cwd, "out") {
				t.Errorf("%q: parts = %+v, want one redirect-only part writing out", command, parts)
			}
		}
	}
}

// A command over maxCommandBytes is one unparsed part: no shell parse is
// attempted. One of exactly maxCommandBytes parses as any other.
func TestOversizeCommandIsOneUnparsedPart(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x")
	pad := func(n int) string {
		cmd := "cat x;"
		return cmd + strings.Repeat(" ", n-len(cmd))
	}
	at := parseOne(t, cwd, pad(maxCommandBytes), nil)
	assertFiles(t, at, []FileRef{ref(cwd, "x", ActionReadWhole, "")})
	over := parseOne(t, cwd, pad(maxCommandBytes+1), nil)
	want := []Part{{Seq: 0, Lang: LangSh, Status: StatusUnparsed, Error: "command over 65536 bytes"}}
	if !reflect.DeepEqual(over, want) {
		t.Fatalf("parts = %+v\nwant    %+v", over, want)
	}
}

// The Bash tool runs the user's $SHELL, zsh on macOS: a command bash cannot
// parse is parsed again as zsh, and a zsh parameter expansion leaves its word
// unknown while the program stays resolved. bash -c still parses as bash only.
func TestZshOnlyCommandParses(t *testing.T) {
	t.Parallel()
	cwd := fixture(t, "x.txt", "y.txt")
	for _, tc := range []struct {
		name, command string
		programs      []string
	}{
		{"split flag", `for id in ${=IDS}; do grep -c "$id" x.txt; done`, []string{"grep"}},
		{"param flags over a command substitution", `for f in ${(f)"$(git ls-files)"}; do wc -l "$f"; done`, []string{"git", "wc"}},
		{"split flag on a known value", `IDS=x.txt; cat ${=IDS}`, []string{"cat"}},
		{"param flag on a known value", `IDS=x.txt; cat ${(f)IDS}`, []string{"cat"}},
		{"zsh -c script", `zsh -c 'cat ${=IDS}'`, []string{"zsh", "cat"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts := parseOne(t, cwd, tc.command, nil)
			var programs []string
			for _, p := range parts {
				if p.Status == StatusError {
					t.Fatalf("parts = %+v, want no error part", parts)
				}
				if p.Program == "cat" && len(p.Files) != 0 {
					t.Fatalf("cat part = %+v, want no file: its word is a zsh split, unknown", p)
				}
				programs = append(programs, p.Program)
			}
			if strings.Join(programs, " ") != strings.Join(tc.programs, " ") {
				t.Fatalf("programs = %q, want %q (parts %+v)", programs, tc.programs, parts)
			}
		})
	}
	for _, command := range []string{`echo ${=IDS`, `bash -c 'cat ${=IDS}'`} {
		parts := parseOne(t, cwd, command, nil)
		var errs int
		for _, p := range parts {
			if p.Status == StatusError {
				errs++
			}
		}
		if errs != 1 {
			t.Fatalf("%q: parts = %+v, want one error part: invalid as bash, and zsh never runs it", command, parts)
		}
	}
}
