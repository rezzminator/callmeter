package cmdparse

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// CutPlaceholder is the word that stands in a stored command for an operand
// cut as content. It is one single-quoted literal word: it parses to one
// argument, names no file and expands to nothing.
const CutPlaceholder = "'[cut]'"

// CutContent is command with its content operands replaced by CutPlaceholder,
// and removed the bytes cut:
//
//   - the message of a `git commit`, also under a wrapper (programAt): the
//     value of `-m`, `--message` and
//     `--trailer`, attached (`-mMSG`, `-m=MSG`, `--message=MSG`, a cluster
//     `-am MSG`) or the next word; `-F FILE` is a path and stays;
//   - every operand but the leading flags of an `echo` or `printf` whose
//     standard output goes to a file (`>`, `>>`, `>|`, `&>`, `&>>`, `1<>` to
//     any target but a non-file device), into `tee` or into a pipe whose
//     output goes to a file, the redirect inherited from an enclosing block,
//     subshell, list, loop or branch, or from a bare `exec` before it (one
//     in a `{ … }` block, a `&&` or `||` list or an `if` branch included);
//     a pipe into a block or loop writing a file goes to a file; standard
//     output and error are followed
//     apart (`>&2`, `/dev/stdout`: redirected), and a `>&N` to a descriptor
//     the script opened (3 and up) is a file;
//   - every here-string (`<<< word`): a body, as a heredoc's is.
//
// An operand holding no literal text (`"$X"`, `"$(…)"`) stays as written. One
// holding literal text keeps its command substitutions after the placeholder,
// each cut the same way, because the shell runs them. A word already in its
// cut form is left as it is, so a stored command cut again comes back the same
// bytes. The script string of `bash -c` is code and is not entered.
//
// ok is false when the command cannot be cut safely and the caller must then
// store none of it: it names `commit`, `echo` or `printf` or holds a `<<<`
// yet parses as neither bash nor zsh, or a backquoted substitution anywhere in
// it holds one of those (a backquoted text is not cut in place).
func CutContent(command string) (cut string, removed int, ok bool) {
	if !mayHoldContent(command) {
		return command, 0, true
	}
	return cutContent(command, 0)
}

// mayHoldContent is whether text names a program CutContent cuts operands of,
// or holds a here-string.
func mayHoldContent(text string) bool {
	return strings.Contains(text, "commit") || strings.Contains(text, "echo") || strings.Contains(text, "printf") ||
		strings.Contains(text, "<<<")
}

// contentCutter collects the edits of one script.
type contentCutter struct {
	src     string
	depth   int
	edits   []edit
	removed int
	ok      bool
}

func cutContent(src string, depth int) (string, int, bool) {
	if depth > maxShellDepth {
		return "", 0, false
	}
	file, err := parseShell(src, true)
	if err != nil {
		return "", 0, false
	}
	c := &contentCutter{src: src, depth: depth, ok: true}
	c.stmts(file.Stmts, streams{})
	if !c.ok {
		return "", 0, false
	}
	sort.Slice(c.edits, func(i, j int) bool { return c.edits[i].start > c.edits[j].start })
	out := src
	for _, e := range c.edits {
		out = out[:e.start] + e.text + out[e.end:]
	}
	return out, c.removed, true
}

// streams is where a statement's standard output and standard error go:
// whether each may reach a file.
type streams struct{ out, err bool }

// stmts walks a list; a bare `exec` redirecting standard output or error sends
// every later statement's stream where it says.
func (c *contentCutter) stmts(list []*syntax.Stmt, in streams) {
	for _, s := range list {
		c.stmt(s, in)
		if dest, set := execRedirect(s, in); set {
			in = dest
		}
	}
}

// execRedirect is where a bare `exec` (no command) sends the shell's streams
// from then on, given in: set when s is one, or holds one in a statement that
// runs in the same shell: a `{ … }` block (each applied in turn), either side
// of a `&&` or `||` list, or an `if` branch. One that may not run (the right
// side of a list, a branch) leaves each stream reaching a file when either
// way does.
func execRedirect(s *syntax.Stmt, in streams) (dest streams, set bool) {
	if s == nil {
		return in, false
	}
	switch x := s.Cmd.(type) {
	case *syntax.Block:
		return execRedirects(x.Stmts, in)
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			return in, false // each side of a pipe runs in a subshell
		}
		dest, set = execRedirect(x.X, in)
		if d, ySet := execRedirect(x.Y, dest); ySet {
			dest, set = either(dest, d), true
		}
		return dest, set
	case *syntax.IfClause:
		dest = in
		for clause := x; clause != nil; clause = clause.Else {
			for _, list := range [][]*syntax.Stmt{clause.Cond, clause.Then} {
				if d, listSet := execRedirects(list, in); listSet {
					dest, set = either(dest, d), true
				}
			}
		}
		return dest, set
	case *syntax.CallExpr:
		if len(x.Args) == 1 && literalWord(x.Args[0]) == "exec" {
			return redirected(s.Redirs, in), true
		}
	}
	return in, false
}

// execRedirects is execRedirect over a list run in turn.
func execRedirects(list []*syntax.Stmt, in streams) (dest streams, set bool) {
	dest = in
	for _, s := range list {
		if d, sSet := execRedirect(s, dest); sSet {
			dest, set = d, true
		}
	}
	return dest, set
}

// either is where streams go when they go to a or to b.
func either(a, b streams) streams {
	return streams{out: a.out || b.out, err: a.err || b.err}
}

// stmt walks one statement; in is where its streams go when its own redirects
// say nothing.
func (c *contentCutter) stmt(s *syntax.Stmt, in streams) {
	if s == nil || !c.ok {
		return
	}
	st := redirected(s.Redirs, in)
	for _, r := range s.Redirs {
		switch {
		case r.Op == syntax.WordHdoc && r.Word != nil:
			c.cutWord(r.Word, "") // a here-string is a body, as a heredoc's is
		case r.Word != nil:
			c.nested(r.Word)
		}
		if r.Hdoc != nil {
			c.nested(r.Hdoc) // the substitutions an unquoted body keeps run
		}
	}
	switch x := s.Cmd.(type) {
	case *syntax.CallExpr:
		c.call(x, st.out)
	case *syntax.BinaryCmd:
		next := st
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			left := streams{out: intoTee(x.Y) || outToFile(x.Y, st), err: st.err}
			if x.Op == syntax.PipeAll {
				left.err = left.out
			}
			c.stmt(x.X, left)
		} else {
			c.stmt(x.X, st)
			if dest, set := execRedirect(x.X, st); set {
				next = dest
			}
		}
		c.stmt(x.Y, next)
	case *syntax.Block:
		c.stmts(x.Stmts, st)
	case *syntax.Subshell:
		c.stmts(x.Stmts, st)
	case *syntax.IfClause:
		for clause := x; clause != nil; clause = clause.Else {
			c.stmts(clause.Cond, st)
			c.stmts(clause.Then, st)
		}
	case *syntax.WhileClause:
		c.stmts(x.Cond, st)
		c.stmts(x.Do, st)
	case *syntax.ForClause:
		c.nested(x.Loop)
		c.stmts(x.Do, st)
	case *syntax.CaseClause:
		c.nested(x.Word)
		for _, item := range x.Items {
			for _, p := range item.Patterns {
				c.nested(p)
			}
			c.stmts(item.Stmts, st)
		}
	case *syntax.TimeClause:
		c.stmt(x.Stmt, st)
	case *syntax.CoprocClause:
		c.stmt(x.Stmt, streams{})
	case *syntax.FuncDecl:
		c.stmt(x.Body, streams{})
	case *syntax.TestDecl:
		c.stmt(x.Body, streams{})
	default:
		c.nested(s.Cmd)
	}
}

// outToFile is whether s's standard output reaches a file, given in: its own
// redirects, then a pipeline's last command, and of a compound command (a
// block, a loop) any file one of its commands writes or tees into. Whatever a
// pipe carries may reach that file, so its left side writes there too.
func outToFile(s *syntax.Stmt, in streams) bool {
	if s == nil {
		return in.out
	}
	st := redirected(s.Redirs, in)
	switch x := s.Cmd.(type) {
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			return outToFile(x.Y, st)
		}
	case *syntax.CallExpr, nil:
		return st.out
	}
	return st.out || writesFile(s.Cmd, st)
}

// writesFile is whether a compound command may write what it reads to a file:
// one of the statements in it sends its standard output to a file, or runs
// tee, given in.
func writesFile(cmd syntax.Command, in streams) bool {
	found := false
	syntax.Walk(cmd, func(node syntax.Node) bool {
		switch x := node.(type) {
		case *syntax.Stmt:
			found = found || redirected(x.Redirs, in).out
		case *syntax.CallExpr:
			p := programAt(x.Args)
			found = found || (p < len(x.Args) && path.Base(literalWord(x.Args[p])) == "tee")
		}
		return !found
	})
	return found
}

// redirected is where a statement's streams go after its own redirects of fd
// 1 and fd 2, applied in order from in: a path names a file unless it is a
// non-file device (nonFileDevice) or names a stream itself (/dev/stdout,
// /dev/stderr, /dev/fd/1, /dev/fd/2, which go where that stream goes); a `>&`
// to 1 or 2 goes where that stream goes, to 0 or closed nowhere, to a
// descriptor the script opened (3 and up) or a word that is not literal
// possibly a file. `<>` opens a stream only when it names its fd.
func redirected(redirs []*syntax.Redirect, in streams) streams {
	st := in
	for _, r := range redirs {
		fd := "1"
		if r.N != nil {
			fd = r.N.Value
		}
		if fd != "1" && fd != "2" {
			continue
		}
		target, literal := wordValue(r.Word)
		var dest bool
		switch r.Op {
		case syntax.RdrAll, syntax.AppAll:
			dest = pathDest(target, literal, st)
			st.out, st.err = dest, dest
			continue
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob:
			dest = pathDest(target, literal, st)
		case syntax.RdrInOut:
			if r.N == nil {
				continue
			}
			dest = pathDest(target, literal, st)
		case syntax.DplOut:
			dest = fdDest(target, literal, st)
		default:
			continue
		}
		if fd == "1" {
			st.out = dest
		} else {
			st.err = dest
		}
	}
	return st
}

// pathDest is whether a redirect to path target may reach a file, given where
// the streams go now.
func pathDest(target string, literal bool, st streams) bool {
	if !literal {
		return true
	}
	switch target {
	case "/dev/stdout", "/dev/fd/1":
		return st.out
	case "/dev/stderr", "/dev/fd/2":
		return st.err
	}
	if fd, isFd := strings.CutPrefix(target, "/dev/fd/"); isFd {
		return fdDest(fd, true, st)
	}
	return !nonFileDevice.MatchString(target)
}

// fdDest is whether a `>&` to target may reach a file, given where the
// streams go now.
func fdDest(target string, literal bool, st streams) bool {
	if !literal {
		return true
	}
	switch strings.TrimSuffix(target, "-") {
	case "1":
		return st.out
	case "2":
		return st.err
	case "0", "":
		return false
	}
	return true
}

// nonFileDevice is a redirect target that keeps nothing: the null device, the
// terminal or standard input. Any other path under /dev/ (/dev/shm, a disk)
// is a file.
var nonFileDevice = regexp.MustCompile(`^/dev/(null|stdin|tty)$`)

// intoTee is whether a pipe's right side starts with `tee`, after any
// wrapper running it (`sudo -u USER tee`, programAt).
func intoTee(s *syntax.Stmt) bool {
	for s != nil {
		switch x := s.Cmd.(type) {
		case *syntax.BinaryCmd:
			s = x.X
			continue
		case *syntax.CallExpr:
			p := programAt(x.Args)
			return p < len(x.Args) && path.Base(literalWord(x.Args[p])) == "tee"
		}
		return false
	}
	return false
}

// programAt is the index in words of the program a simple command runs, past
// every wrapper and its options as the parser strips them (unwrapProgram:
// `sudo git commit`, `env A=1 echo`, `timeout 30 git commit`, `xargs -I{}
// git commit`); len(words) when it runs none (`command -v X`, a bare `env`).
// A word read as "" (wrapperArgText) is never a wrapper.
func programAt(words []*syntax.Word) int {
	if len(words) == 0 {
		return 0
	}
	args := make([]arg, len(words))
	for i, w := range words {
		args[i] = arg{text: wrapperArgText(w), word: w}
	}
	program, runs := unwrapProgram(args)
	if !runs {
		return len(words)
	}
	return len(words) - len(program)
}

// wrapperArgText is a word as programAt reads it: its literal value, or the
// text of a word of unquoted text only (`-I{}`), which the parser reads as
// written too; any other word is "".
func wrapperArgText(w *syntax.Word) string {
	if text := literalWord(w); text != "" {
		return text
	}
	var b strings.Builder
	for _, part := range w.Parts {
		lit, isLit := part.(*syntax.Lit)
		if !isLit {
			return ""
		}
		b.WriteString(lit.Value)
	}
	return b.String()
}

// literalWord is a word's value when it is literal, else "".
func literalWord(w *syntax.Word) string {
	value, _ := wordValue(w)
	return value
}

// call cuts the content operands of one simple command.
func (c *contentCutter) call(x *syntax.CallExpr, toFile bool) {
	cut := map[*syntax.Word]string{} // a cut word → the flag text kept before its placeholder
	if p := programAt(x.Args); p < len(x.Args) {
		args := x.Args[p:]
		switch name := path.Base(literalWord(args[0])); {
		case name == "git":
			commitMessages(args, cut)
		case (name == "echo" || name == "printf") && toFile:
			writtenOperands(name, args, cut)
		}
	}
	for _, a := range x.Assigns {
		c.nested(a)
	}
	for _, w := range x.Args {
		if prefix, isCut := cut[w]; isCut {
			c.cutWord(w, prefix)
		} else {
			c.nested(w)
		}
	}
}

// writtenOperands marks every operand of an echo or printf after its leading
// flags (`-n`, `-e`, `-E`; `-v NAME`, `--`).
func writtenOperands(name string, args []*syntax.Word, cut map[*syntax.Word]string) {
	i := 1
	for ; i < len(args); i++ {
		flag, literal := wordValue(args[i])
		if !literal {
			break
		}
		if name == "echo" && echoFlags.MatchString(flag) {
			continue
		}
		if name == "printf" && flag == "-v" {
			i++
			continue
		}
		if name == "printf" && flag == "--" {
			i++
		}
		break
	}
	for ; i < len(args); i++ {
		cut[args[i]] = ""
	}
}

var echoFlags = regexp.MustCompile(`^-[neE]+$`)

// gitGlobalWithValue is the git options before the subcommand that take the
// next word as their value.
var gitGlobalWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--super-prefix": true, "--config-env": true,
}

// commitLongWithValue is the `git commit` long options that take the next
// word as their value when it is not attached.
var commitLongWithValue = map[string]bool{
	"file": true, "author": true, "date": true, "template": true, "reuse-message": true,
	"reedit-message": true, "fixup": true, "squash": true, "cleanup": true, "pathspec-from-file": true,
}

// commitMessages marks the message words of a `git commit`, keyed to the
// flag text kept before the placeholder ("" for a separate word).
func commitMessages(args []*syntax.Word, cut map[*syntax.Word]string) {
	i := 1
	for ; i < len(args); i++ {
		word, literal := wordValue(args[i])
		switch {
		case !literal:
			return
		case gitGlobalWithValue[word]:
			i++
		case strings.HasPrefix(word, "-"):
		case word == "commit":
			i++
			goto options
		default:
			return
		}
	}
	return
options:
	for ; i < len(args); i++ {
		lead, whole := literalLead(args[i])
		next := func(prefix string) {
			if i+1 < len(args) {
				i++
				cut[args[i]] = prefix
			}
		}
		switch {
		case lead == "--" && whole:
			return
		case strings.HasPrefix(lead, "--"):
			name, _, attached := strings.Cut(lead[2:], "=")
			switch {
			case isMessageOption(name) && attached:
				cut[args[i]] = "--" + name + "="
			case isMessageOption(name) && whole:
				next("")
			case commitLongWithValue[name] && !attached && whole:
				i++
			}
		case strings.HasPrefix(lead, "-") && len(lead) > 1:
			for k := 1; k < len(lead); k++ {
				letter := lead[k]
				rest := k+1 < len(lead) || !whole
				switch {
				case letter == 'm' && rest:
					prefix := lead[:k+1]
					if k+1 < len(lead) && lead[k+1] == '=' {
						prefix += "="
					}
					cut[args[i]] = prefix
				case letter == 'm':
					next("")
				case strings.IndexByte("cCFt", letter) >= 0 && !rest:
					i++
				case strings.IndexByte("cCFtSu", letter) < 0:
					continue
				}
				break
			}
		}
	}
}

// isMessageOption is whether a `git commit` long option name, abbreviated or
// not, carries message text: `--message` or `--trailer`.
func isMessageOption(name string) bool {
	return name != "" && strings.HasPrefix("message", name) || len(name) >= 2 && strings.HasPrefix("trailer", name)
}

// literalLead is the value of a word's leading literal pieces, and whether
// those are the whole word.
func literalLead(w *syntax.Word) (lead string, whole bool) {
	var b strings.Builder
	for _, part := range w.Parts {
		switch piece := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(piece.Value, `\*?[~{`) {
				return b.String(), false
			}
			b.WriteString(piece.Value)
		case *syntax.SglQuoted:
			if piece.Dollar {
				return b.String(), false
			}
			b.WriteString(piece.Value)
		case *syntax.DblQuoted:
			raw, literal := rawDouble(piece)
			if !literal || piece.Dollar {
				return b.String(), false
			}
			b.WriteString(unescapeDouble(raw))
		default:
			return b.String(), false
		}
	}
	return b.String(), true
}

// simpleParam is a parameter expansion carrying no literal text: `$X`,
// `${X}`, `$1`, `$@`.
var simpleParam = regexp.MustCompile(`^\$(\{[A-Za-z_][A-Za-z0-9_]*\}|[A-Za-z_][A-Za-z0-9_]*|[0-9@*#?$!-])$`)

// cutWord replaces a content word with prefix, the placeholder and the word's
// command substitutions, unless it holds no literal text beyond prefix or is
// already that text.
func (c *contentCutter) cutWord(w *syntax.Word, prefix string) {
	start, end := int(w.Pos().Offset()), int(w.End().Offset())
	var substs strings.Builder
	// kept: the bytes the cut form keeps of the word, prefix, substitutions as
	// written and the quotes of a double-quoted piece holding any; inner: the
	// bytes cut inside those substitutions.
	literal, content, kept, inner, substCount := 0, false, len(prefix), 0, 0
	var piece func(part syntax.WordPart, inDouble bool)
	piece = func(part syntax.WordPart, inDouble bool) {
		switch x := part.(type) {
		case *syntax.Lit:
			literal += len(x.Value)
		case *syntax.SglQuoted:
			literal += len(x.Value)
		case *syntax.DblQuoted:
			before := substCount
			for _, inner := range x.Parts {
				piece(inner, true)
			}
			if substCount > before {
				kept += 2 // its quotes, written again around each substitution
			}
		case *syntax.CmdSubst, *syntax.ProcSubst:
			text, cut := c.substText(x)
			kept += int(x.End().Offset() - x.Pos().Offset())
			inner += cut
			substCount++
			if inDouble {
				text = `"` + text + `"`
			}
			substs.WriteString(text)
		case *syntax.ParamExp:
			if !simpleParam.MatchString(c.src[x.Pos().Offset():x.End().Offset()]) {
				content = true
			}
		default:
			content = true
		}
	}
	for _, part := range w.Parts {
		piece(part, false)
	}
	if !c.ok {
		return
	}
	if !content && literal <= len(prefix) {
		c.nested(w)
		return
	}
	text := prefix + CutPlaceholder + substs.String()
	if text == c.src[start:end] {
		return
	}
	c.edits = append(c.edits, edit{start: start, end: end, text: text})
	// Never below one byte: a word that is cut held literal text or content.
	c.removed += max(end-start-kept+inner, 1)
}

// substText is a command substitution's text with its own content cut, and
// the bytes that cut removed.
func (c *contentCutter) substText(node syntax.Node) (string, int) {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	text := c.src[start:end]
	if !mayHoldContent(text) {
		return text, 0
	}
	open := 2 // `$(` or `<(`, `>(`
	if x, isSubst := node.(*syntax.CmdSubst); isSubst && (x.Backquotes || x.TempFile || x.ReplyVar) {
		c.ok = false
		return "", 0
	}
	inner, removed, ok := cutContent(text[open:len(text)-1], c.depth+1)
	if !ok {
		c.ok = false
		return "", 0
	}
	return text[:open] + inner + text[len(text)-1:], removed
}

// nested walks the command and process substitutions in node: the shell runs
// them, and their output goes to the word they sit in.
func (c *contentCutter) nested(node syntax.Node) {
	if node == nil || !c.ok {
		return
	}
	syntax.Walk(node, func(n syntax.Node) bool {
		if !c.ok {
			return false
		}
		switch x := n.(type) {
		case *syntax.CmdSubst:
			if x.Backquotes || x.TempFile || x.ReplyVar {
				start, end := int(x.Pos().Offset()), int(x.End().Offset())
				if mayHoldContent(c.src[start:end]) {
					c.ok = false
				}
				return false
			}
			c.stmts(x.Stmts, streams{})
			return false
		case *syntax.ProcSubst:
			c.stmts(x.Stmts, streams{})
			return false
		}
		return true
	})
}
