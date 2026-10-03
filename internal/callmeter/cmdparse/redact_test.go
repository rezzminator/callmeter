package cmdparse

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// privateBody is invented file content a heredoc carries.
const privateBody = "first-private-line\n  second-private-line `x` $(y)\n"

// escapedBody is invented file content as written inside a double-quoted
// script, its quotes escaped.
const escapedBody = `first-private-line \"q\"` + "\n"

// privateSubsts is what an unquoted heredoc keeps of privateBody: its command
// substitutions, one per line; privateKept is their bytes as written.
const (
	privateSubsts = "`x`\n$(y)\n"
	privateKept   = len("`x`") + len("$(y)")
)

// substBody is an unquoted heredoc body holding command substitutions in
// text, in a parameter default and in arithmetic.
const substBody = "hello $(date +%s) there `whoami`\n${X:-$(id -u)} $(( $(nproc) + 1 )) private-tail\n"

func TestRedactHeredocs(t *testing.T) {
	cases := []struct {
		name, command, want string
		removed             int
	}{
		{"plain keeps the command substitutions", "cat > /tmp/demo-proj/a <<EOF\n" + privateBody + "EOF\n",
			"cat > /tmp/demo-proj/a <<EOF\n" + privateSubsts + "EOF\n", len(privateBody) - privateKept},
		{"substitutions in text, a default and arithmetic", "cat > a <<EOF\n" + substBody + "EOF\nls",
			"cat > a <<EOF\n$(date +%s)\n`whoami`\n$(id -u)\n$(nproc)\nEOF\nls",
			len(substBody) - len("$(date +%s)`whoami`$(id -u)$(nproc)")},
		{"a heredoc in a kept substitution is cut", "cat <<A\nx $(cat <<B\ninner-private\nB\n) y\nA\n",
			"cat <<A\n$(cat <<B\nB\n)\nA\n", len("x  y\n") + len("inner-private\n")},
		{"a body of substitutions only", "cat <<EOF\n$(a)\n`b`\nEOF\n", "cat <<EOF\n$(a)\n`b`\nEOF\n", 0},
		{"quoted delimiter", "cat <<'EOF' > /tmp/demo-proj/a\n" + privateBody + "EOF", "cat <<'EOF' > /tmp/demo-proj/a\nEOF", len(privateBody)},
		{"double-quoted delimiter", "cat <<\"EOF\"\n" + privateBody + "EOF\nls", "cat <<\"EOF\"\nEOF\nls", len(privateBody)},
		{"dash heredoc keeps the delimiter's tabs", "cat <<-EOF\n\t" + privateBody + "\t\tEOF\n", "cat <<-EOF\n" + privateSubsts + "\t\tEOF\n", 1 + len(privateBody) - privateKept},
		{"empty body", "cat <<EOF\nEOF\n", "cat <<EOF\nEOF\n", 0},
		{"nested bash -c", "timeout 9 bash -lc 'cd /tmp/demo-proj && cat > b <<EOF\n" + privateBody + "EOF\n'", "timeout 9 bash -lc 'cd /tmp/demo-proj && cat > b <<EOF\n" + privateSubsts + "EOF\n'", len(privateBody) - privateKept},
		{"escaped double-quoted script", `bash -c "cat > a <<\"EOF\"` + "\n" + escapedBody + "EOF\n" + `echo \$HOME \\n done"`,
			`bash -c "cat > a <<\"EOF\"` + "\nEOF\n" + `echo \$HOME \\n done"`, len(`first-private-line "q"`) + 1},
		{"script in quoted pieces", "P='echo '\"'\"'a'\"'\"' && cat > a <<\"EOF\"\n" + privateBody + "EOF\n'\nbash -c \"$P\"",
			"P='echo '\"'\"'a'\"'\"' && cat > a <<\"EOF\"\nEOF\n'\nbash -c \"$P\"", len(privateBody)},
		{"nested twice", "bash -c \"sh -c 'cat <<E\n" + "inner-private\n" + "E\n'\"", "bash -c \"sh -c 'cat <<E\nE\n'\"", len("inner-private\n")},
		{"here-string", "cat <<< 'one << two'\nls", "cat <<< 'one << two'\nls", 0},
		{"quoted <<", "echo 'a << b' \"c << d\"", "echo 'a << b' \"c << d\"", 0},
		{"arithmetic", "echo $(( 1 << 3 ))\nls", "echo $(( 1 << 3 ))\nls", 0},
		{"no <<", "ls -la /tmp/demo-proj", "ls -la /tmp/demo-proj", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, removed, ok := RedactHeredocs(c.command)
			if !ok || got != c.want || removed != c.removed {
				t.Fatalf("RedactHeredocs = (%q, %d, %v)\nwant              (%q, %d, true)", got, removed, ok, c.want, c.removed)
			}
		})
	}
}

func TestRedactHeredocsRefusesWhatItCannotCut(t *testing.T) {
	for _, command := range []string{
		"cat <<EOF\n" + privateBody + "EOF\nif then fi )",           // does not parse
		"cat <<EOF\n" + privateBody,                                 // unclosed
		"bash -c \"cat <<EOF\n" + privateBody + "EOF\n$X\"",         // double-quoted script with an expansion
		"bash -c $'cat <<EOF\\n" + "inner-private" + "\\nEOF\\n'",   // $'…' script
		"bash -c 'cat <<EOF\n" + privateBody + "EOF\nif then fi )'", // quoted script that does not parse
		"cat <<A\nx `cat <<B\ninner-private\nB\n` y\nA\n",           // a heredoc in a backquoted substitution
	} {
		if got, removed, ok := RedactHeredocs(command); ok {
			t.Errorf("RedactHeredocs(%q) = (%q, %d, true), want a refusal", command, got, removed)
		}
	}
}

// The redacted command parses to the parts of the original: a body that is
// only text never was a part, a file or an argument.
func TestRedactHeredocsKeepsTheParts(t *testing.T) {
	dir := t.TempDir()
	commands := []string{
		"cat > " + dir + "/a.txt <<'EOF'\n" + privateBody + "EOF\nwc -l " + dir + "/a.txt",
		"cd " + dir + " && tee b.txt <<-'EOF' >/dev/null\n\t" + privateBody + "\tEOF\n",
		// An unquoted body's command substitutions run: they stay parts.
		"cd " + dir + " && cat > c.txt <<EOF\nrev $(git -C " + dir + " rev-parse HEAD) private-tail\n$(wc -l a.txt)\nEOF\n",
	}
	for i, command := range commands {
		redacted, _, ok := RedactHeredocs(command)
		if !ok {
			t.Fatalf("RedactHeredocs(%q) refused", command)
		}
		if strings.Contains(redacted, "private-") {
			t.Errorf("command %d: redacted %q keeps body text", i, redacted)
		}
		parts := parseBoth(t, dir, command, redacted)
		if !reflect.DeepEqual(parts["orig"], parts["redacted"]) {
			t.Errorf("command %d: parts differ\norig     %+v\nredacted %+v", i, parts["orig"], parts["redacted"])
		}
	}
}

// A heredoc read into an argument (a commit message) put its body in the
// part's args; the redacted command's parts carry none of it.
func TestRedactHeredocsLeavesNoBodyInArgs(t *testing.T) {
	dir := t.TempDir()
	command := "git commit -m \"$(cat <<'EOF'\n" + privateBody + "EOF\n)\" && ls " + dir
	redacted, _, ok := RedactHeredocs(command)
	if !ok {
		t.Fatalf("RedactHeredocs(%q) refused", command)
	}
	parts := parseBoth(t, dir, command, redacted)
	if !strings.Contains(fmt.Sprint(parts["orig"]), "first-private-line") {
		t.Fatalf("the original's parts carry no body, the case proves nothing: %+v", parts["orig"])
	}
	if got := fmt.Sprint(parts["redacted"]); strings.Contains(got, "private-line") {
		t.Fatalf("redacted parts carry the body: %s", got)
	}
	if len(parts["orig"]) != len(parts["redacted"]) {
		t.Fatalf("part count %d, want the original's %d", len(parts["redacted"]), len(parts["orig"]))
	}
}

// A script in an escaped double-quoted string, or in one word of several
// quoted pieces, is cut, never refused: the inner script's parts are the
// original's, and the script argument carries no body.
func TestRedactHeredocsCutsAQuotedScript(t *testing.T) {
	dir := t.TempDir()
	for _, command := range []string{
		`bash -c "cd ` + dir + ` && cat > c.txt <<\"EOF\"` + "\n" + escapedBody + "EOF\n" + `wc -l c.txt"`,
		"P='cd " + dir + " && echo '\"'\"'a'\"'\"' && cat > c.txt <<\"EOF\"\n" + privateBody + "EOF\nwc -l c.txt'\nbash -c \"$P\"",
	} {
		redacted, _, ok := RedactHeredocs(command)
		if !ok {
			t.Fatalf("RedactHeredocs(%q) refused", command)
		}
		parts := parseBoth(t, dir, command, redacted)
		orig, cut := parts["orig"], parts["redacted"]
		if len(orig) < 4 || len(cut) != len(orig) || !reflect.DeepEqual(cut[1:], orig[1:]) || cut[0].Program != orig[0].Program {
			t.Fatalf("parts differ\norig     %+v\nredacted %+v", orig, cut)
		}
		if got := fmt.Sprint(cut); strings.Contains(got, "private-line") {
			t.Fatalf("redacted parts carry the body: %s", got)
		}
	}
}

func parseBoth(t *testing.T, dir, command, redacted string) map[string][]Part {
	t.Helper()
	calls := []Call{{ID: "orig", Command: command, Cwd: dir}, {ID: "redacted", Command: redacted, Cwd: dir}}
	parts, err := ParseBatch(context.Background(), calls, nil)
	if err != nil {
		t.Fatalf("ParseBatch: %v", err)
	}
	return parts
}

// A Python heredoc whose body the store cut is a script-body part naming
// why, never a snippet that attributes nothing nor a parse fault; an unquoted
// one keeps its command substitutions as parts beside it.
func TestRedactedPythonHeredocIsScriptBody(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		command, cause string
		parts          int
	}{
		{"python3 - <<'EOF'\nprint(open('/tmp/demo-proj/x').read())\nEOF", errHeredocNotStored, 1},
		{"python3 - <<EOF\nprint(open('$(pwd)/x').read())\nEOF", errCodeUnresolved, 2},
	} {
		redacted, _, ok := RedactHeredocs(c.command)
		if !ok {
			t.Fatalf("RedactHeredocs(%q) refused", c.command)
		}
		parts := parseBoth(t, dir, c.command, redacted)
		got := parts["redacted"]
		py := got[len(got)-1] // a substitution in the body runs first
		if len(got) != c.parts || py.Lang != LangPython || py.Status != StatusScriptBody || py.Error != c.cause {
			t.Fatalf("%q: parts = %+v, want %d, the last a %s python part (%s)", redacted, got, c.parts, StatusScriptBody, c.cause)
		}
		if c.parts > 1 && !reflect.DeepEqual(got, parts["orig"]) {
			t.Fatalf("parts differ\norig     %+v\nredacted %+v", parts["orig"], got)
		}
	}
}

// A Python heredoc keeps its part through the cut: the same program, args and
// shell-level files, the parts around it unchanged; only the snippet, and so
// what the snippet itself opened, is unknown.
func TestRedactedPythonHeredocKeepsItsPart(t *testing.T) {
	dir := t.TempDir()
	read := dir + "/private.txt"
	if err := os.WriteFile(read, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "cd " + dir + " && python3 - --flag <<'PY' > out.txt\nprint(open('" + read + "').read())\nPY\nwc -l out.txt"
	redacted, _, ok := RedactHeredocs(command)
	if !ok {
		t.Fatalf("RedactHeredocs(%q) refused", command)
	}
	parts := parseBoth(t, dir, command, redacted)
	orig, cut := parts["orig"], parts["redacted"]
	if len(orig) != 3 || orig[1].Lang != LangPython || orig[1].Status != StatusOK || !strings.Contains(fmt.Sprint(orig[1].Files), read) {
		t.Fatalf("the original is no parsed Python heredoc reading %s, the case proves nothing: %+v", read, orig)
	}
	if len(cut) != len(orig) {
		t.Fatalf("redacted parts %+v, want the original's %d", cut, len(orig))
	}
	for i := range orig {
		if cut[i].Lang != orig[i].Lang || cut[i].Program != orig[i].Program || !reflect.DeepEqual(cut[i].Args, orig[i].Args) {
			t.Errorf("part %d: redacted %+v, want the original's program and args %+v", i, cut[i], orig[i])
		}
	}
	if !reflect.DeepEqual(cut[0], orig[0]) || !reflect.DeepEqual(cut[2], orig[2]) {
		t.Errorf("the parts around the heredoc changed:\norig     %+v\nredacted %+v", orig, cut)
	}
	want := []FileRef{{Path: dir + "/out.txt", Action: ActionWrite}}
	if cut[1].Status != StatusScriptBody || cut[1].Error != errHeredocNotStored || !reflect.DeepEqual(cut[1].Files, want) {
		t.Errorf("python part = %+v, want %s (%s) writing only out.txt", cut[1], StatusScriptBody, errHeredocNotStored)
	}
}
