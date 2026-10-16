package callclass

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

var (
	readPrograms = set("cat head tail less more wc jq yq stat file tree diff cmp awk gawk cut sort uniq tr nl column xxd od hexdump strings " +
		"shasum md5 md5sum sha1sum sha256sum pwd which whereis type command du df realpath basename dirname readlink date env printenv " +
		"id whoami uname hostname ps lsof tac rev paste comm join fold base64 ls expr bc seq uptime nproc lscpu sw_vers")
	searchPrograms       = set("grep egrep fgrep rg ag ack find fd locate mdfind")
	housekeepingPrograms = set("mkdir rmdir rm mv cp chmod chown chgrp ln touch install truncate mktemp unlink rsync tar unzip zip mkfifo")
	// neutral programs print or set shell state: they carry no class of their own.
	neutralPrograms = set("cd true false exit set export unset local read test [ [[ : source . trap echo printf declare typeset alias pushd popd return")
	waitPrograms    = set("sleep wait")

	gitRead = set("log show diff status blame grep ls-files rev-parse cat-file rev-list merge-base describe ls-tree shortlog reflog " +
		"for-each-ref name-rev check-ignore count-objects whatchanged diff-tree diff-index diff-files var version help " +
		"archive ls-remote merge-tree show-ref cherry range-diff show-branch fsck verify-commit hash-object")
	// gitState verbs change state; with gitRead and gitListing they are the verbs the parser knows.
	gitState = set("add commit checkout switch reset merge rebase cherry-pick revert restore clean mv rm fetch pull push init clone am bisect gc prune " +
		"update-ref symbolic-ref filter-repo merge-file read-tree write-tree apply stash")
	// gitListing verbs read when called bare or with a listing argument, and change state otherwise.
	gitListing = set("branch remote worktree config stash tag submodule notes")
	gitListArg = set("list show get -l -a -v -vv -r --list --show-current --get --get-all --verbose --contains --merged --no-merged --points-at status")

	testScript = regexp.MustCompile(`^(check|checks|test|tests|verify|gate|lint|typecheck|e2e|leak-check|run-tests?|ci|vet)([-_.].*)?\.(sh|py|mjs|js|ts|bash)$|[-_](check|test|tests|gate)\.(sh|py|mjs|bash)$`)
	sqlWrite   = regexp.MustCompile(`(?i)\b(insert|update|delete|create|drop|alter|replace|vacuum|attach)\b`)
	pkgRunTest = regexp.MustCompile(`test|build|lint|check|typecheck|verify|tsc|vet|e2e`)
)

// fileOperands is how many operands a reader needs before it names a file: a
// reader below it filters its stdin (a pipe stage, a heredoc) and reads no
// file. A script or pattern operand counts (jq, awk, sed take one first).
var fileOperands = map[string]int{
	"cat": 1, "head": 1, "tail": 1, "less": 1, "more": 1, "wc": 1, "sort": 1, "uniq": 1, "cut": 1, "column": 1, "nl": 1, "tac": 1, "rev": 1,
	"paste": 1, "fold": 1, "base64": 1, "xxd": 1, "od": 1, "hexdump": 1, "strings": 1, "jq": 2, "yq": 2, "awk": 2, "gawk": 2, "sed": 2, "tr": 99,
}

// filters reports whether a reader named base, given args, reads only its stdin.
func filters(base string, args []string) bool {
	n, ok := fileOperands[base]
	return ok && len(operands(args)) < n
}

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

// partClasses labels one simple command. prior are the parts before it in the
// call: a tee reads what the part before it printed, an mv what an earlier part
// wrote.
func partClasses(p cmdparse.Part, prior []cmdparse.Part, rules Rules) []Class {
	base := filepath.Base(p.Program)
	args := p.Args
	out := ownClasses(p, base, args, rules)
	if p.ScriptWrites {
		out = append(out, Edit)
	}
	switch base {
	case "tee":
		if hasFileArg(args) {
			// tee writes the text it is fed: a heredoc, or what the part
			// piped into it authored
			if p.StdinText || len(prior) > 0 && (authors(prior[len(prior)-1]) || prior[len(prior)-1].ScriptWrites) {
				out = append(out, Edit)
			} else {
				out = append(out, Capture)
			}
		}
	case "mv", "cp":
		if src, dst := moveOperands(args); src != "" && transformedInPlace(prior, src, dst) {
			// a temporary captured from a file and moved over it is an in-place transform
			out = append(out, Edit)
		}
	}
	if len(p.Redirects) > 0 {
		// a redirect authors only when it takes the output of a program that
		// prints its own text; a bare `> f` runs no program and truncates
		if p.StdoutRedirect && authors(p) {
			out = append(out, Edit)
		} else {
			out = append(out, Capture)
		}
	}
	return out
}

// dateSubst is a timestamp substitution: the time of a line, not a command's output.
var dateSubst = regexp.MustCompile(`\$\(date[^)]*\)`)

// authors reports whether the program's own output is text the call wrote:
// echo, printf, or cat reading a heredoc or here-string.
func authors(p cmdparse.Part) bool {
	switch filepath.Base(p.Program) {
	case "echo", "printf":
		// a command's status or output echoed into a file is a log line; a
		// timestamp is not. A word the parse resolved is literal text, whatever
		// it holds (a quoted backtick); only an unresolved one is an expansion.
		for i, a := range p.Args {
			if i < len(p.ArgsKnown) && p.ArgsKnown[i] {
				continue
			}
			a = dateSubst.ReplaceAllString(a, "")
			if strings.Contains(a, "$?") || strings.Contains(a, "${?") || strings.Contains(a, "$(") || strings.Contains(a, "`") {
				return false
			}
		}
		return true
	case "cat":
		return p.StdinText && !hasFileArg(p.Args)
	}
	return false
}

// transformedInPlace reports whether an earlier part captured its output into
// src while reading dst: `jq … f > tmp && mv tmp f`.
func transformedInPlace(prior []cmdparse.Part, src, dst string) bool {
	for _, q := range prior {
		if slices.Contains(q.Redirects, src) && slices.ContainsFunc(q.Args, func(a string) bool { return filepath.Clean(a) == filepath.Clean(dst) }) {
			return true
		}
	}
	return false
}

// moveOperands is the first and the last operand of an mv or cp with two or
// more operands.
func moveOperands(args []string) (string, string) {
	ops := operands(args)
	if len(ops) < 2 {
		return "", ""
	}
	return ops[0], ops[len(ops)-1]
}

func operands(args []string) []string {
	var ops []string
	for _, a := range args {
		if a != "" && a != "-" && !strings.HasPrefix(a, "-") {
			ops = append(ops, a)
		}
	}
	return ops
}

func hasFileArg(args []string) bool {
	for _, a := range operands(args) {
		if a != "/dev/null" {
			return true
		}
	}
	return false
}

// ownClasses is what the program does, apart from where its output goes.
func ownClasses(p cmdparse.Part, base string, args []string, rules Rules) []Class {
	switch {
	case base == "":
		return nil
	case p.Lang == cmdparse.LangPython:
		if p.ScriptWrites {
			return nil
		}
		for _, f := range p.Files {
			switch f.Action {
			case cmdparse.ActionReadWhole, cmdparse.ActionReadRange, cmdparse.ActionSearch, cmdparse.ActionStat:
				return []Class{Read}
			}
		}
		return nil
	case isPython(base):
		return scriptRun(args, "-m", rules)
	case base == "node" || base == "nodejs" || base == "tsx" || base == "bash" || base == "sh" || base == "zsh":
		return scriptRun(args, "", rules)
	case base == "git":
		return gitClasses(args)
	case base == "sed":
		if inPlace(args, sedValueFlags) {
			return []Class{Edit}
		}
		if filters(base, args) {
			return nil
		}
		return []Class{Read}
	case base == "perl":
		if inPlace(args, perlValueFlags) {
			return []Class{Edit}
		}
		return nil
	case base == "ruby":
		if inPlace(args, rubyValueFlags) {
			return []Class{Edit}
		}
		return nil
	case base == "awk" || base == "gawk":
		if slices.Contains(args, "inplace") {
			return []Class{Edit}
		}
		if filters(base, args) {
			return nil
		}
		return []Class{Read}
	case base == "patch":
		if slices.Contains(args, "--dry-run") {
			return nil
		}
		return []Class{Edit}
	case base == "apply_patch":
		return []Class{Edit}
	case base == "sqlite3":
		return sqliteClass(args)
	case base == "find":
		out := []Class{Search}
		if slices.Contains(args, "-delete") {
			out = append(out, Housekeeping)
		}
		// -exec and its kin run a command per match: it does what it does
		for _, inner := range findExecs(args) {
			out = append(out, ownClasses(cmdparse.Part{Program: inner[0], Args: inner[1:]}, filepath.Base(inner[0]), inner[1:], rules)...)
		}
		return out
	case base == "yq" || base == "jq":
		if slices.Contains(args, "-i") || slices.Contains(args, "--inplace") {
			return []Class{Edit}
		}
		if filters(base, args) {
			return nil
		}
		return []Class{Read}
	}
	if rules.testRun(base, args) {
		return []Class{Test}
	}
	if c, ok := toolchain(base, args, rules); ok {
		return c
	}
	switch {
	case waitPrograms[base]:
		return []Class{Wait}
	case housekeepingPrograms[base]:
		return []Class{Housekeeping}
	case searchPrograms[base]:
		return []Class{Search}
	case readPrograms[base]:
		if filters(base, args) {
			return nil
		}
		return []Class{Read}
	case neutralPrograms[base]:
		return nil
	}
	if testScript.MatchString(base) {
		return []Class{Test}
	}
	return nil
}

// wrapped is the command string a wrapper program runs: the script of a
// `bash -c` / `sh -c` given to docker or podman, or the words a rules wrapper
// runs. Empty when the part is not a wrapper.
func wrapped(p cmdparse.Part, rules Rules) string {
	base, args := filepath.Base(p.Program), p.Args
	if base == "docker" || base == "podman" {
		for i := 0; i+2 < len(args); i++ {
			if (args[i] == "bash" || args[i] == "sh" || args[i] == "zsh") && args[i+1] == "-c" {
				return args[i+2]
			}
		}
	}
	return rules.wrapped(base, args)
}

func isPython(base string) bool {
	return base == "python" || strings.HasPrefix(base, "python3") || strings.HasPrefix(base, "python2")
}

func anyOf(args []string, words ...string) bool {
	for _, a := range args {
		if slices.Contains(words, a) {
			return true
		}
	}
	return false
}

// valueFlags are interpreter flags whose next word is their value, not the script.
var valueFlags = map[string]bool{"-W": true, "-X": true, "-r": true, "--require": true, "--import": true}

// scriptRun labels an interpreter run from what it executes: a test module
// (-m pytest, unittest) or a test script by its name; any other script is not
// known to do anything.
func scriptRun(args []string, moduleFlag string, rules Rules) []Class {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case moduleFlag != "" && a == moduleFlag && i+1 < len(args):
			if anyOf(args[i+1:i+2], "pytest", "unittest", "tox", "nox", "mypy", "ruff", "pyright", "doctest", "compileall") {
				return []Class{Test}
			}
			return nil
		case a == "--test" && moduleFlag == "":
			return []Class{Test}
		case valueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"):
			continue
		default:
			if testScript.MatchString(filepath.Base(a)) || rules.testRun(filepath.Base(a), args[i+1:]) {
				return []Class{Test}
			}
			return nil
		}
	}
	return nil
}

// The flag letters whose value follows in the same word (or the next): a
// cluster is read only up to one of them, since what follows is the value
// (perl -Mstrict, ruby -rdigest), never an -i.
const (
	sedValueFlags  = "ef"
	perlValueFlags = "eEMmIxF0CdD"
	rubyValueFlags = "eErICxF0KTW"
)

// findExecs is the command of each -exec, -execdir, -ok or -okdir of a find,
// up to its `;` or `+`.
func findExecs(args []string) [][]string {
	var out [][]string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-exec", "-execdir", "-ok", "-okdir":
			j := i + 1
			for j < len(args) && args[j] != ";" && args[j] != `\;` && args[j] != "+" {
				j++
			}
			if j > i+1 {
				out = append(out, args[i+1:j])
			}
			i = j
		}
	}
	return out
}

// inPlace reports a sed, perl or ruby in-place flag: -i, -i.bak, a cluster such
// as -pi or -Ei, or --in-place. A letter in stop starts the flag's own value
// (the script, a module), so a cluster is read only up to it.
func inPlace(args []string, stop string) bool {
	for _, a := range args {
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		for _, r := range a[1:] {
			if r == 'i' {
				return true
			}
			if strings.ContainsRune(stop, r) {
				break
			}
		}
	}
	return false
}

func sqliteClass(args []string) []Class {
	if anyOf(args, "-readonly") {
		return []Class{Read}
	}
	for _, a := range args {
		if strings.Contains(a, "mode=ro") || strings.Contains(a, "immutable=1") {
			return []Class{Read}
		}
	}
	if sqlWrite.MatchString(strings.Join(args, " ")) {
		return nil
	}
	return []Class{Read}
}

func gitClasses(args []string) []Class {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			rest = args[i:]
			i = len(args)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	verb, tail := rest[0], rest[1:]
	if !knownGit(verb) {
		// a loop variable that expands to several words (git -C $p …) leaves extra words before the verb
		for i, w := range rest {
			if knownGit(w) {
				verb, tail = w, rest[i+1:]
				break
			}
		}
	}
	switch {
	case verb == "stash":
		// bare, push, pop, -u…: only list and show read
		if len(tail) > 0 && (tail[0] == "list" || tail[0] == "show") {
			return []Class{Read}
		}
		return []Class{Git}
	case verb == "tag" && anyOf(tail, "-a", "--annotate", "-s", "--sign", "-u", "--local-user", "-m", "--message", "-F", "--file", "-f", "--force", "-d", "--delete"):
		return []Class{Git}
	case verb == "hash-object" && anyOf(tail, "-w"):
		return []Class{Git}
	case verb == "grep":
		return []Class{Search}
	case verb == "apply":
		if anyOf(tail, "--check", "--stat", "--numstat", "--summary") {
			return []Class{Read}
		}
		return []Class{Edit}
	case gitRead[verb]:
		return []Class{Read}
	case gitListing[verb]:
		if len(operands(tail)) == 0 || gitListArg[firstOrEmpty(tail)] || anyOf(tail, "-l", "--list", "-a", "-v", "-vv", "--show-current") {
			return []Class{Read}
		}
	}
	return []Class{Git}
}

func knownGit(w string) bool { return gitRead[w] || gitListing[w] || gitState[w] || w == "grep" }

func firstOrEmpty(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

// goVerb is the go subcommand, past a `-C dir` flag.
func goVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-C":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// toolchain labels the build, test and format programs; false means the
// program is not one of them.
func toolchain(base string, args []string, rules Rules) ([]Class, bool) {
	verb := ""
	if ops := operands(args); len(ops) > 0 {
		verb = ops[0]
	}
	has := func(words ...string) bool { return anyOf(args, words...) }
	test, edit, read := []Class{Test}, []Class{Edit}, []Class{Read}
	switch base {
	case "go":
		switch goVerb(args) {
		case "test", "build", "vet", "install":
			return test, true
		case "fmt":
			return edit, true
		}
		return nil, true
	case "gofmt", "goimports", "gofumpt", "shfmt", "autopep8", "isort", "rustfmt":
		if has("-w", "-i", "--write", "--in-place") || base == "rustfmt" && !has("--check") || base == "isort" && !has("--check", "--check-only", "-c", "--diff") {
			return edit, true
		}
		return read, true
	case "prettier":
		if has("--write", "-w") {
			return edit, true
		}
		return test, true
	case "black":
		if has("--check", "--diff") {
			return test, true
		}
		return edit, true
	case "ruff":
		if verb == "format" && !has("--check", "--diff") || verb == "check" && has("--fix", "--fix-only") {
			return edit, true
		}
		return test, true
	case "rumdl":
		if verb == "fmt" || has("--fix") {
			return edit, true
		}
		return test, true
	case "eslint", "golangci-lint":
		if has("--fix") {
			return edit, true
		}
		return test, true
	case "cargo":
		switch verb {
		case "fmt":
			if has("--check") {
				return test, true
			}
			return edit, true
		case "test", "build", "check", "clippy", "bench":
			return test, true
		}
		return nil, true
	case "terraform", "tofu":
		if verb == "fmt" && !has("-check") {
			return edit, true
		}
		return nil, true
	case "npm", "pnpm", "yarn", "bun":
		if verb == "test" || verb == "build" || verb == "lint" || verb == "typecheck" {
			return test, true
		}
		if verb == "run" || verb == "run-script" || verb == "exec" {
			if ops := operands(args); len(ops) > 1 && pkgRunTest.MatchString(ops[1]) {
				return test, true
			}
		}
		if base == "bun" {
			// bun is also the interpreter of the script it is given
			return scriptRun(args, "", rules), true
		}
		return nil, true
	case "deno":
		switch verb {
		case "test", "check", "lint", "bench":
			return test, true
		case "fmt":
			if has("--check") {
				return test, true
			}
			return edit, true
		case "run":
			return scriptRun(args[slices.Index(args, "run")+1:], "", rules), true
		}
		return scriptRun(args, "", rules), true
	case "npx", "bunx", "pnpx":
		switch {
		case slices.Contains([]string{"tsc", "vitest", "jest", "playwright", "mypy"}, verb):
			return test, true
		case verb == "prettier" && has("--write", "-w"), verb == "eslint" && has("--fix"):
			return edit, true
		case verb == "eslint":
			return test, true
		}
		return nil, true
	case "pytest", "tsc", "vitest", "jest", "mypy", "shellcheck", "make", "gmake", "gradle", "gradlew", "mvn", "ctest", "tox", "nox", "pyright", "playwright", "bats", "hadolint", "actionlint", "yamllint", "markdownlint", "staticcheck", "govulncheck":
		return test, true
	case "docker":
		if verb == "build" || verb == "buildx" || verb == "compose" {
			return test, true
		}
		return nil, true
	}
	return nil, false
}
