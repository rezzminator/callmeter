package cmdparse

import (
	"reflect"
	"testing"
)

func TestPartRedirectsAndScriptWrites(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		command      string
		program      string
		redirects    []string
		scriptWrites bool
	}{
		{"redirect target unresolved", `go test ./... > $OUT 2>&1`, "go", []string{"$OUT"}, false},
		{"append and clobber targets", `echo hi >> notes.txt; echo ho >| other.txt`, "echo", []string{"notes.txt"}, false},
		{"dev targets and fd dups name no file", `make build > /dev/null 2>&1`, "make", nil, false},
		{"stdin redirect is not a write target", `wc -l < in.txt`, "wc", nil, false},
		{"python opens a variable path for writing", "python3 - <<'EOF'\np = 'x.go'\ns = open(p).read()\nopen(p, 'w').write(s.replace('a', 'b'))\nEOF", "python3", nil, true},
		{"python write_text on a path variable", "python3 -c \"import pathlib; q = pathlib.Path('x'); q.write_text('y')\"", "python3", nil, true},
		{"python write_text on a path expression", "python3 -c \"import pathlib; (pathlib.Path('d') / 'x').write_text('y')\"", "python3", nil, true},
		{"python only reads", "python3 - <<'EOF'\nprint(open('x.go').read())\nEOF", "python3", nil, false},
		{"python path word in a mode slot is no write", `python3 -c "import os; os.open('data.txt', 0)"`, "python3", nil, false},
		{"node -e writes a file", `node -e "require('fs').writeFileSync('a.json', '{}')"`, "node", nil, true},
		{"node heredoc appends a file", "node - <<'EOF'\nconst fs = require('fs');\nfs.appendFileSync('log.txt', 'x');\nEOF", "node", nil, true},
		{"node script file is not known to write", `node run.mjs`, "node", nil, false},
		{"group redirect is carried to the group's first command", `{ echo a; echo b; } >> "$LOG"`, "echo", []string{`"$LOG"`}, false},
		{"group redirect skips a substitution in the first command's words", `{ echo "# Report $(date)"; cat data; } > report.md`, "echo", []string{"report.md"}, false},
		{"group redirect skips a process substitution in the first command's words", `{ diff <(sort a) b; echo x; } > d.txt`, "diff", []string{"d.txt"}, false},
		{"group redirect passes an inner group with no command", `{ { x=1; } 2>/dev/null; echo "# Notes"; } > notes.md`, "echo", []string{"notes.md"}, false},
		{"python gzip.open in a text write mode", "python3 -c \"import gzip; p = 'a.gz'; gzip.open(p, 'wt').write('x')\"", "python3", nil, true},
		{"python bz2.open in a write mode", "python3 -c \"import bz2; p = 'a.bz2'; bz2.open(p, 'wb').write(b'x')\"", "python3", nil, true},
		{"python lzma.open read only", "python3 -c \"import lzma; p = 'a.xz'; print(lzma.open(p).read())\"", "python3", nil, false},
		{"python tarfile.open with a compressed write mode", "python3 -c \"import tarfile; p = 'a.tgz'; tarfile.open(p, 'w:gz').close()\"", "python3", nil, true},
		{"python tarfile.open with a compressed read mode", "python3 -c \"import tarfile; p = 'a.tgz'; tarfile.open(p, mode='r:gz').close()\"", "python3", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts := parseOne(t, fixture(t), tc.command, nil)
			var got *Part
			for i := range parts {
				if parts[i].Program == tc.program {
					got = &parts[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("no %s part in %+v", tc.program, parts)
			}
			if !reflect.DeepEqual(got.Redirects, tc.redirects) {
				t.Errorf("Redirects = %v, want %v", got.Redirects, tc.redirects)
			}
			if got.ScriptWrites != tc.scriptWrites {
				t.Errorf("ScriptWrites = %v, want %v", got.ScriptWrites, tc.scriptWrites)
			}
		})
	}
}
