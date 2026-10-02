package cmdparse

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// RedactHeredocs is command with the body of every heredoc (`<<` and `<<-`,
// quoted delimiter or not) cut out, found by the shell parser's own heredoc
// nodes: the redirect line and the closing delimiter stay, and so do the
// command substitutions (`$(…)`, `…`) of an unquoted body, one per line,
// because the shell runs them: the result parses to the same parts, and
// removed is the number of body bytes cut. A heredoc inside a kept
// substitution is cut the same way. A
// heredoc inside a command substitution, or inside a quoted string that is
// itself a shell script (`bash -c '…'`, `ssh host '…'`), is cut the same way.
// A `<<` inside quotes that holds no heredoc, inside arithmetic, or a
// here-string (`<<<`) is left as written.
//
// A double-quoted script with backslash escapes (`bash -c "cat <<\"EOF\"…"`),
// or a script written as one word of several literal quoted pieces
// (`P='cat <<EOF…'"'"'…'`), is cut in its value and written back quoted: its
// value, and so its parts, are the original's minus the body.
//
// ok is false when the command cannot be redacted safely, and the caller must
// then store none of it: the command does not parse; a quoted string holds a
// `<<` with a line after it yet is no script the parser reads, or is one whose
// text cannot be cut in place (a `$'…'` string, a double-quoted string with
// expansions); a backquoted substitution in a body holds a heredoc; or the
// result still carries a heredoc body.
func RedactHeredocs(command string) (redacted string, removed int, ok bool) {
	if !strings.Contains(command, "<<") {
		return command, 0, true
	}
	redacted, removed, ok = redactScript(command, 0)
	if !ok {
		return "", 0, false
	}
	if removed == 0 {
		return command, 0, true
	}
	if _, left, again := redactScript(redacted, 0); !again || left != 0 {
		return "", 0, false
	}
	return redacted, removed, true
}

// edit replaces src[start:end] with text.
type edit struct {
	start, end int
	text       string
}

// redactScript cuts the heredoc bodies of one shell script, recursing into
// the quoted scripts it carries up to maxShellDepth levels.
func redactScript(src string, depth int) (string, int, bool) {
	if depth > maxShellDepth {
		return "", 0, false
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		return "", 0, false
	}
	var edits []edit
	removed, ok := 0, true
	// quoted handles a quoted string whose content is src[start:end] and whose
	// value is value; encode writes a redacted value back as content (nil: the
	// content cannot be cut in place).
	quoted := func(start, end int, value string, encode func(string) string) {
		if !mayHoldHeredocBody(src[start:end]) {
			return
		}
		if encode == nil {
			ok = false
			return
		}
		inner, n, innerOK := redactScript(value, depth+1)
		switch {
		case !innerOK:
			ok = false
		case n > 0:
			edits = append(edits, edit{start: start, end: end, text: encode(inner)})
			removed += n
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if !ok {
			return false
		}
		switch x := node.(type) {
		case *syntax.Word:
			if len(x.Parts) < 2 {
				return true
			}
			start, end := int(x.Pos().Offset()), int(x.End().Offset())
			if !mayHoldHeredocBody(src[start:end]) {
				return true
			}
			value, literal := wordValue(x)
			if !literal {
				return true // its pieces are walked one by one
			}
			inner, n, innerOK := redactScript(value, depth+1)
			switch {
			case !innerOK:
				return true // no script as a whole: its pieces are judged one by one
			case n > 0:
				edits = append(edits, edit{start: start, end: end, text: singleQuoted(inner)})
				removed += n
			}
			return false
		case *syntax.Redirect:
			if x.Op != syntax.Hdoc && x.Op != syntax.DashHdoc {
				return true
			}
			if x.Hdoc != nil {
				start, end, cutOK := heredocBody(src, x)
				if !cutOK {
					ok = false
					return false
				}
				kept, written, nested, keptOK := keptSubsts(src, x.Hdoc, depth)
				if !keptOK {
					ok = false
					return false
				}
				if src[start:end] != kept {
					edits = append(edits, edit{start: start, end: end, text: kept})
					removed += end - start - written + nested
				}
			}
			return false // the delimiter word holds no heredoc, the body is gone
		case *syntax.SglQuoted:
			start := int(x.Left.Offset()) + 1
			if x.Dollar {
				start++
			}
			var encode func(string) string
			if !x.Dollar {
				encode = verbatim
			}
			quoted(start, int(x.Right.Offset()), x.Value, encode)
			return false
		case *syntax.DblQuoted:
			start := int(x.Left.Offset()) + 1
			if x.Dollar {
				start++
			}
			end := int(x.Right.Offset())
			if !mayHoldHeredocBody(src[start:end]) {
				return true // its expansions may still hold a heredoc
			}
			raw, literal := rawDouble(x)
			if !literal && !mayHoldHeredocBody(literalText(x)) {
				return true // the `<<` sits in an expansion, walked as its own node
			}
			var encode func(string) string
			switch {
			case !literal || x.Dollar || raw != src[start:end]:
			case !strings.Contains(raw, `\`):
				encode = verbatim
			default:
				encode = escapeDouble
			}
			quoted(start, end, unescapeDouble(raw), encode)
			return false
		}
		return true
	})
	if !ok {
		return "", 0, false
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := src
	for _, e := range edits {
		out = out[:e.start] + e.text + out[e.end:]
	}
	return out, removed, true
}

// keptSubsts is what a heredoc body keeps: each outermost command
// substitution of body (none in a quoted delimiter's body), in source order,
// one per line, any heredoc inside a `$(…)` cut by redactScript. written is
// their bytes in src, nested the bytes the inner cuts removed. ok is false
// when an inner script cannot be cut, or a backquoted one may hold a heredoc.
func keptSubsts(src string, body *syntax.Word, depth int) (kept string, written, nested int, ok bool) {
	var b strings.Builder
	ok = true
	syntax.Walk(body, func(node syntax.Node) bool {
		x, isSubst := node.(*syntax.CmdSubst)
		if !ok || !isSubst {
			return ok
		}
		start, end := int(x.Pos().Offset()), int(x.End().Offset())
		if start < 0 || end > len(src) || start > end {
			ok = false
			return false
		}
		text := src[start:end]
		written += len(text)
		if inner := text[1 : len(text)-1]; !x.Backquotes {
			inner = text[2 : len(text)-1]
			if mayHoldHeredocBody(inner) {
				cut, n, innerOK := redactScript(inner, depth+1)
				if !innerOK {
					ok = false
					return false
				}
				text, nested = "$("+cut+")", nested+n
			}
		} else if mayHoldHeredocBody(inner) {
			ok = false
			return false
		}
		b.WriteString(text)
		b.WriteByte('\n')
		return false
	})
	return b.String(), written, nested, ok
}

// heredocBody is the byte range of a heredoc's body in src. The parser's
// heredoc word runs from the body's first byte through the closing delimiter
// line, so the body ends where that last line starts; a last line that is not
// the delimiter (an unclosed heredoc) is not cut safely.
func heredocBody(src string, r *syntax.Redirect) (start, end int, ok bool) {
	start, stop := int(r.Hdoc.Pos().Offset()), int(r.Hdoc.End().Offset())
	if start < 0 || stop > len(src) || start > stop {
		return 0, 0, false
	}
	text := src[start:stop]
	last := strings.LastIndexByte(text, '\n') + 1
	closing := text[last:]
	if r.Op == syntax.DashHdoc {
		closing = strings.TrimLeft(closing, "\t")
	}
	delimiter := strings.NewReplacer(`'`, "", `"`, "", `\`, "").Replace(src[r.Word.Pos().Offset():r.Word.End().Offset()])
	if closing != delimiter {
		return 0, 0, false
	}
	return start, start + last, true
}

// mayHoldHeredocBody is whether text has a `<<` with a line after it (a
// newline or, in a `$'…'` string, a `\n` escape): only then can a heredoc
// body sit in it.
func mayHoldHeredocBody(text string) bool {
	i := strings.Index(text, "<<")
	return i >= 0 && (strings.Contains(text[i:], "\n") || strings.Contains(text[i:], `\n`))
}

// rawDouble is a double-quoted string's source text when it is literal text
// only, escapes included: no expansion in it.
func rawDouble(x *syntax.DblQuoted) (string, bool) {
	var b strings.Builder
	for _, part := range x.Parts {
		lit, isLit := part.(*syntax.Lit)
		if !isLit {
			return "", false
		}
		b.WriteString(lit.Value)
	}
	return b.String(), true
}

// unescapeDouble is the value of literal double-quoted text: a backslash
// before `$`, a backquote, `"` or `\` is dropped, a backslash-newline is
// dropped whole, and any other backslash stays.
func unescapeDouble(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			switch raw[i+1] {
			case '$', '`', '"', '\\':
				i++
				b.WriteByte(raw[i])
				continue
			case '\n':
				i++
				continue
			}
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

// escapeDouble is double-quoted content whose value is value.
var escapeDouble = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace

// wordValue is the value of a word whose every piece is literal: unquoted
// text with no escape, glob, tilde or brace, a '…' string, or a double-quoted
// string of literal text.
func wordValue(x *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, part := range x.Parts {
		switch piece := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(piece.Value, `\*?[~{`) {
				return "", false
			}
			b.WriteString(piece.Value)
		case *syntax.SglQuoted:
			if piece.Dollar {
				return "", false
			}
			b.WriteString(piece.Value)
		case *syntax.DblQuoted:
			raw, literal := rawDouble(piece)
			if !literal || piece.Dollar {
				return "", false
			}
			b.WriteString(unescapeDouble(raw))
		default:
			return "", false
		}
	}
	return b.String(), true
}

// singleQuoted is shell text whose value is value: '…' pieces, each
// apostrophe written as "'" between them.
func singleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// verbatim is quoted content whose value is the content itself.
func verbatim(value string) string { return value }

// literalText is the literal parts of a double-quoted string, joined: the
// text a `<<` of the string itself, not of an expansion inside it, sits in.
func literalText(x *syntax.DblQuoted) string {
	var b strings.Builder
	for _, part := range x.Parts {
		if lit, isLit := part.(*syntax.Lit); isLit {
			b.WriteString(lit.Value)
		}
	}
	return b.String()
}
