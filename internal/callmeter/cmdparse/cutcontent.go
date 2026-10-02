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
//   - the message of a `git commit`: the value of `-m`, `--message` and
//     `--trailer`, attached (`-mMSG`, `-m=MSG`, `--message=MSG`, a cluster
//     `-am MSG`) or the next word; `-F FILE` is a path and stays;
//   - every operand but the leading flags of an `echo` or `printf` whose
//     standard output goes to a file (`>`, `>>`, `>|`, `&>`, `&>>` to a target
//     outside /dev/) or into `tee`, the redirect inherited from an enclosing
//     block, subshell, list, loop or branch.
//
// An operand holding no literal text (`"$X"`, `"$(…)"`) stays as written. One
// holding literal text keeps its command substitutions after the placeholder,
// each cut the same way, because the shell runs them. A word already in its
// cut form is left as it is, so a stored command cut again comes back the same
// bytes. The script string of `bash -c` is code and is not entered.
//
// ok is false when the command cannot be cut safely and the caller must then
// store none of it: it names `commit`, `echo` or `printf` yet parses as
// neither bash nor zsh, or a backquoted substitution in a cut word or a
// redirected command holds one of those names.
func CutContent(command string) (cut string, removed int, ok bool) {
	if !mayHoldContent(command) {
		return command, 0, true
	}
	return cutContent(command, 0)
}

// mayHoldContent is whether text names a program CutContent cuts operands of.
func mayHoldContent(text string) bool {
	return strings.Contains(text, "commit") || strings.Contains(text, "echo") || strings.Contains(text, "printf")
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
	c.stmts(file.Stmts, false)
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

func (c *contentCutter) stmts(list []*syntax.Stmt, toFile bool) {
	for _, s := range list {
		c.stmt(s, toFile)
	}
}

// stmt walks one statement; toFile is whether its standard output reaches a
// file when its own redirects say nothing.
func (c *contentCutter) stmt(s *syntax.Stmt, toFile bool) {
	if s == nil || !c.ok {
		return
	}
	if dest, set := stdoutToFile(s.Redirs); set {
		toFile = dest
	}
	for _, r := range s.Redirs {
		if r.Word != nil {
			c.nested(r.Word)
		}
		if r.Hdoc != nil {
			c.nested(r.Hdoc) // the substitutions an unquoted body keeps run
		}
	}
	switch x := s.Cmd.(type) {
	case *syntax.CallExpr:
		c.call(x, toFile)
	case *syntax.BinaryCmd:
		if x.Op == syntax.Pipe || x.Op == syntax.PipeAll {
			c.stmt(x.X, intoTee(x.Y))
		} else {
			c.stmt(x.X, toFile)
		}
		c.stmt(x.Y, toFile)
	case *syntax.Block:
		c.stmts(x.Stmts, toFile)
	case *syntax.Subshell:
		c.stmts(x.Stmts, toFile)
	case *syntax.IfClause:
		for clause := x; clause != nil; clause = clause.Else {
			c.stmts(clause.Cond, toFile)
			c.stmts(clause.Then, toFile)
		}
	case *syntax.WhileClause:
		c.stmts(x.Cond, toFile)
		c.stmts(x.Do, toFile)
	case *syntax.ForClause:
		c.nested(x.Loop)
		c.stmts(x.Do, toFile)
	case *syntax.CaseClause:
		c.nested(x.Word)
		for _, item := range x.Items {
			for _, p := range item.Patterns {
				c.nested(p)
			}
			c.stmts(item.Stmts, toFile)
		}
	case *syntax.TimeClause:
		c.stmt(x.Stmt, toFile)
	case *syntax.CoprocClause:
		c.stmt(x.Stmt, false)
	case *syntax.FuncDecl:
		c.stmt(x.Body, false)
	case *syntax.TestDecl:
		c.stmt(x.Body, false)
	default:
		c.nested(s.Cmd)
	}
}

// stdoutToFile is where a statement's own redirects send its standard output:
// set when one of them redirects it, toFile when the last such names a file.
func stdoutToFile(redirs []*syntax.Redirect) (toFile, set bool) {
	for _, r := range redirs {
		if r.N != nil && r.N.Value != "1" {
			continue
		}
		target, literal := wordValue(r.Word)
		switch r.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob, syntax.RdrAll, syntax.AppAll:
			toFile, set = !literal || !strings.HasPrefix(target, "/dev/"), true
		case syntax.DplOut:
			toFile, set = !literal || !fdTarget.MatchString(target), true
		}
	}
	return toFile, set
}

// fdTarget is a `>&` target naming a file descriptor or closing one.
var fdTarget = regexp.MustCompile(`^([0-9]+-?|-)$`)

// intoTee is whether a pipe's right side starts with `tee` or `sudo tee`,
// past sudo's options and their values (wrappers' sudo entry).
func intoTee(s *syntax.Stmt) bool {
	for s != nil {
		switch x := s.Cmd.(type) {
		case *syntax.BinaryCmd:
			s = x.X
			continue
		case *syntax.CallExpr:
			if len(x.Args) == 0 {
				return false
			}
			name, _ := wordValue(x.Args[0])
			if path.Base(name) == "sudo" {
				rest := make([]arg, 0, len(x.Args)-1)
				for _, w := range x.Args[1:] {
					text, _ := wordValue(w)
					rest = append(rest, arg{text: text, word: w})
				}
				n, _ := wrappers["sudo"].skip(rest)
				if n >= len(rest) {
					return false
				}
				name = rest[n].text
			}
			return path.Base(name) == "tee"
		}
		return false
	}
	return false
}

// literalWord is a word's value when it is literal, else "".
func literalWord(w *syntax.Word) string {
	value, _ := wordValue(w)
	return value
}

// call cuts the content operands of one simple command.
func (c *contentCutter) call(x *syntax.CallExpr, toFile bool) {
	cut := map[*syntax.Word]string{} // a cut word → the flag text kept before its placeholder
	if len(x.Args) > 0 {
		switch name := path.Base(literalWord(x.Args[0])); {
		case name == "git":
			commitMessages(x.Args, cut)
		case (name == "echo" || name == "printf") && toFile:
			writtenOperands(name, x.Args, cut)
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
			c.stmts(x.Stmts, false)
			return false
		case *syntax.ProcSubst:
			c.stmts(x.Stmts, false)
			return false
		}
		return true
	})
}
