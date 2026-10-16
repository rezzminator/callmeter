package cmdparse

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// pyWrites reports whether a scanned Python snippet opens a file for writing,
// whatever the path: open() with a literal write mode (the path is often a
// variable, which pyFiles cannot attribute), or write_text/write_bytes on any
// receiver. A mode that is not a literal is not a write: it is unknown.
func pyWrites(res PyResult) bool {
	for _, c := range res.Calls {
		switch {
		case openFuncs[c.Func]:
			mode := strAt(c.Args, 1)
			if m, ok := c.Kwargs["mode"]; ok {
				mode = m
			}
			if writeMode(mode) {
				return true
			}
		case isMethod(c.Func, "write_text") || isMethod(c.Func, "write_bytes"):
			return true
		case compressedOpen[c.Func]:
			// gzip/bz2/lzma.open(path, mode) and tarfile.open(path, 'w:gz'):
			// the mode is the second argument, a tarfile one before its ':'.
			mode := strAt(c.Args, 1)
			if m, ok := c.Kwargs["mode"]; ok {
				mode = m
			}
			if mode, _, _ = strings.Cut(mode, ":"); writeMode(mode) {
				return true
			}
		case strings.HasSuffix(c.Func, "().open") || strings.HasSuffix(c.Func, ".open"):
			mode := strAt(c.Args, 0)
			if m, ok := c.Kwargs["mode"]; ok {
				mode = m
			}
			if writeMode(mode) {
				return true
			}
		}
	}
	return false
}

// compressedOpen names the module open() calls whose mode is the second
// argument; left out of openFuncs, whose stored file reads they are not.
var compressedOpen = map[string]bool{"gzip.open": true, "bz2.open": true, "lzma.open": true, "tarfile.open": true}

// isMethod reports whether the scanned call name is the method name on any
// receiver: the scanner names a call on an expression receiver, such as
// (root / "a").write_text(), by the method alone.
func isMethod(fn, name string) bool { return fn == name || strings.HasSuffix(fn, "."+name) }

// writeMode reports whether m is an open() mode string that writes: only mode
// letters, at least one of w a x +. A path or flag word passed where a mode
// would be (os.open, webbrowser.open) is not one.
func writeMode(m string) bool {
	return m != "" && strings.Trim(m, "rwxabtU+") == "" && strings.ContainsAny(m, "wax+")
}

// nodeWriteAPI names the node file-writing calls: fs.writeFile and its sync,
// promise and append forms, createWriteStream, and fs.cp/copyFile are left out
// (a copy authors nothing).
var nodeWriteAPI = regexp.MustCompile(`\b(writeFile|writeFileSync|appendFile|appendFileSync|createWriteStream)\s*\(`)

// nodeWrites reports whether the script a node part ran writes a file: its
// `-e`/`--eval`/`-p`/`--print` code or a heredoc or here-string on stdin. A
// script that is not literal here (`node run.js`, an unresolved expansion) is
// not known to write.
func (p *callParser) nodeWrites(rest []arg, redirs []*syntax.Redirect) bool {
	for i := 0; i < len(rest); i++ {
		switch rest[i].text {
		case "-e", "--eval", "-p", "--print":
			if i+1 < len(rest) {
				code, known := p.text(rest[i+1].word)
				return known && nodeWriteAPI.MatchString(code)
			}
			return false
		}
	}
	for _, r := range redirs {
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			if r.Hdoc != nil {
				code, known := p.text(r.Hdoc)
				return known && nodeWriteAPI.MatchString(code)
			}
		case syntax.WordHdoc:
			code, known := p.text(r.Word)
			return known && nodeWriteAPI.MatchString(code)
		}
	}
	return false
}
