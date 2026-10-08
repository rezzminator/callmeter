package cmdparse

import (
	"context"
	"reflect"
	"testing"
)

// Over every corpus command, the content cut (after the heredoc cut, as the
// store makes it) refuses only what parses as neither shell, gives back its
// own output unchanged with nothing removed, and leaves every part's program
// and attributed files as they were.
func TestCutContentOverCorpus(t *testing.T) {
	cut := 0
	for _, c := range loadCorpus(t) {
		redacted, _, ok := RedactHeredocs(c.Command)
		if !ok {
			continue
		}
		got, removed, ok := CutContent(redacted)
		if !ok {
			if _, err := parseShell(redacted, true); err == nil {
				t.Errorf("%s: CutContent refused a command that parses", c.Name)
			}
			continue
		}
		again, more, ok := CutContent(got)
		if !ok || again != got || more != 0 {
			t.Errorf("%s: cut again = %q, %d, %v; want %q, 0, true", c.Name, again, more, ok, got)
		}
		if removed == 0 {
			continue
		}
		cut++
		calls := []Call{{ID: "before", Command: redacted, Cwd: c.Cwd}, {ID: "after", Command: got, Cwd: c.Cwd}}
		parts, err := ParseBatch(context.Background(), calls, nil)
		if err != nil {
			t.Fatalf("%s: ParseBatch: %v", c.Name, err)
		}
		if !reflect.DeepEqual(shapeOf(parts["before"]), shapeOf(parts["after"])) {
			t.Errorf("%s: parts differ\nbefore %+v\nafter  %+v", c.Name, shapeOf(parts["before"]), shapeOf(parts["after"]))
		}
	}
	if cut == 0 {
		t.Fatal("no corpus command had content cut: the corpus no longer covers CutContent")
	}
}

// partShape is what a cut must leave of a part: its program and its files.
type partShape struct {
	Program string
	Files   []FileRef
}

func shapeOf(parts []Part) []partShape {
	out := make([]partShape, len(parts))
	for i, p := range parts {
		out[i] = partShape{Program: p.Program, Files: p.Files}
	}
	return out
}

// TestCutContent: a commit message and the text an echo or printf writes to a
// file are content, each replaced by CutPlaceholder and its bytes counted as
// removed; the rest of the command stays as written, and the cut form cut
// again comes back the same bytes with nothing removed.
func TestCutContent(t *testing.T) {
	cases := []struct {
		name, command, want string
		removed             int
	}{
		{"commit -m", `git commit -m "MSG-private"`, `git commit -m '[cut]'`, 13},
		{"commit -am", `git commit -am 'MSG-private'`, `git commit -am '[cut]'`, 13},
		{"commit --message=", `git commit --message=MSG-private`, `git commit --message='[cut]'`, 11},
		{"commit --message", `git commit --message "MSG-private" --no-verify`, `git commit --message '[cut]' --no-verify`, 13},
		{"commit -m attached", `git commit -mMSG-private`, `git commit -m'[cut]'`, 11},
		{"commit -m=", `git commit -m=MSG-private`, `git commit -m='[cut]'`, 11},
		{
			"commit global options, two messages, a trailer",
			`git -C /tmp/demo-proj commit -s -m "MSG-private one" -m 'MSG-private two' --trailer "Ack: MSG-private" && git push`,
			`git -C /tmp/demo-proj commit -s -m '[cut]' -m '[cut]' --trailer '[cut]' && git push`,
			17 + 17 + 18,
		},
		{"commit message keeps its substitution", `git commit -m "MSG-private $(git rev-parse HEAD)"`, `git commit -m '[cut]'"$(git rev-parse HEAD)"`, 12},
		{"echo > file", `echo "TXT-private" > f`, `echo '[cut]' > f`, 13},
		{"printf >> file", `printf '%s' TXT-private >> f`, `printf '[cut]' '[cut]' >> f`, 4 + 11},
		{"echo | tee", `echo TXT-private | tee f`, `echo '[cut]' | tee f`, 11},
		{"echo | sudo -u user tee", `echo TXT-private | sudo -u root tee /tmp/demo-proj/f`, `echo '[cut]' | sudo -u root tee /tmp/demo-proj/f`, 11},
		{"echo -n | sudo tee -a", `echo -n TXT-private | sudo tee -a /tmp/demo-proj/f > /dev/null`, `echo -n '[cut]' | sudo tee -a /tmp/demo-proj/f > /dev/null`, 11},
		{"block redirect", `{ echo TXT-private; echo "$X"; } >> /tmp/demo-proj/f`, `{ echo '[cut]'; echo "$X"; } >> /tmp/demo-proj/f`, 11},
		{"loop redirect", "for i in 1 2; do echo TXT-private $i; done > f", "for i in 1 2; do echo '[cut]' $i; done > f", 11},
		{"echo in a substitution", `x=$(echo TXT-private > f) && wc -l f`, `x=$(echo '[cut]' > f) && wc -l f`, 11},
		{"echo >| file", `echo TXT-private >| f`, `echo '[cut]' >| f`, 11},
		{"echo &> file", `echo TXT-private &> f`, `echo '[cut]' &> f`, 11},
		{"printf &>> file", `printf TXT-private &>> f`, `printf '[cut]' &>> f`, 11},
		{"subshell redirect", `(echo TXT-private; date) > f`, `(echo '[cut]'; date) > f`, 11},
		{"branch redirect", `if [ -n "$X" ]; then echo TXT-private; fi >> f`, `if [ -n "$X" ]; then echo '[cut]'; fi >> f`, 11},
		{
			"an operand's substitution is cut the same way",
			`echo "TXT-private $(printf TXT-private > g)" > f`,
			`echo '[cut]'"$(printf '[cut]' > g)" > f`,
			12 + 11,
		},
		{"sudo git commit", `sudo git commit -m "MSG-private"`, `sudo git commit -m '[cut]'`, 13},
		{"env git commit", `env A=1 git commit -m MSG-private`, `env A=1 git commit -m '[cut]'`, 11},
		{"nohup git commit", `nohup git commit -m MSG-private`, `nohup git commit -m '[cut]'`, 11},
		{"xargs git commit", `xargs git commit -m MSG-private`, `xargs git commit -m '[cut]'`, 11},
		{"timeout git commit", `timeout 30 git commit -m MSG-private`, `timeout 30 git commit -m '[cut]'`, 11},
		{"sudo -u env -u git commit", `sudo -u root env -u X git commit -mMSG-private`, `sudo -u root env -u X git commit -m'[cut]'`, 11},
		{"command echo > file", `command echo TXT-private > f`, `command echo '[cut]' > f`, 11},
		{"nice -n printf > file", `nice -n 5 printf TXT-private > f`, `nice -n 5 printf '[cut]' > f`, 11},
		{"sudo echo to the terminal stays", `sudo echo hi`, `sudo echo hi`, 0},
		{"exec > file", `exec > f; echo TXT-private`, `exec > f; echo '[cut]'`, 11},
		{"exec > /dev/null stays", `exec > /dev/null; echo hi`, `exec > /dev/null; echo hi`, 0},
		{"exec in a block", `{ exec > f; }; echo TXT-private`, `{ exec > f; }; echo '[cut]'`, 11},
		{"exec in a subshell stays there", `(exec > f); echo hi`, `(exec > f); echo hi`, 0},
		{"echo to fd 1 of a block writing a file", `{ echo TXT-private >&1; } > f`, `{ echo '[cut]' >&1; } > f`, 11},
		{"echo to /dev/stdout after exec > file", `exec > f; echo TXT-private > /dev/stdout`, `exec > f; echo '[cut]' > /dev/stdout`, 11},
		{"echo to stderr sent to a file", `echo TXT-private 2> f >&2`, `echo '[cut]' 2> f >&2`, 11},
		{"echo to stderr after exec 2> file", `exec 2> f; echo TXT-private >&2`, `exec 2> f; echo '[cut]' >&2`, 11},
		{"echo to stderr of a block writing a file", `{ echo TXT-private >&2; } 2>> f`, `{ echo '[cut]' >&2; } 2>> f`, 11},
		{"echo to /dev/stdout at the terminal stays", `echo hi > /dev/stdout`, `echo hi > /dev/stdout`, 0},
		{"echo to stderr then null stays", `echo hi 2>/dev/null >&2`, `echo hi 2>/dev/null >&2`, 0},
		{"echo to a descriptor the script opened", `exec 3> f; echo TXT-private >&3`, `exec 3> f; echo '[cut]' >&3`, 11},
		{"pipe ending in a file", `{ echo TXT-private | cat; } > f`, `{ echo '[cut]' | cat; } > f`, 11},
		{"pipe end's own redirect", `echo TXT-private | tr a-z A-Z > f`, `echo '[cut]' | tr a-z A-Z > f`, 11},
		{"echo 1<> file", `echo TXT-private 1<>f`, `echo '[cut]' 1<>f`, 11},
		{"echo > /dev/shm", `echo TXT-private > /dev/shm/x`, `echo '[cut]' > /dev/shm/x`, 11},
		{"echo <> stdin stays", `echo hi <>f`, `echo hi <>f`, 0},
		{"echo to /dev/stderr stays", `echo hi > /dev/stderr`, `echo hi > /dev/stderr`, 0},
		{"here-string", `git commit -F- <<< "MSG-private"`, `git commit -F- <<< '[cut]'`, 13},
		{"here-string into cat", `cat <<< "TXT-private" > f`, `cat <<< '[cut]' > f`, 13},
		{"here-string of a variable stays", `grep x <<< "$X"`, `grep x <<< "$X"`, 0},
		{"echo of a substitution stays", `echo "$(date)" > f`, `echo "$(date)" > f`, 0},
		{"bash -c stays", `bash -c 'echo hi > f'`, `bash -c 'echo hi > f'`, 0},
		{"echo hi stays", `echo hi`, `echo hi`, 0},
		{"echo into a pipe stays", `echo hi | grep h`, `echo hi | grep h`, 0},
		{"echo to stderr stays", `echo hi >&2`, `echo hi >&2`, 0},
		{"echo to /dev/null stays", `echo hi > /dev/null`, `echo hi > /dev/null`, 0},
		{"echo of a variable stays", `echo "$X" > f`, `echo "$X" > f`, 0},
		{"commit -F keeps its file", `git commit -F /tmp/demo-proj/msg.txt`, `git commit -F /tmp/demo-proj/msg.txt`, 0},
		{"git log -m stays", `git log -m --oneline`, `git log -m --oneline`, 0},
		{"python -c stays", `python3 -c 'print("echo x > f")'`, `python3 -c 'print("echo x > f")'`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, removed, ok := CutContent(c.command)
			if !ok || got != c.want || removed != c.removed {
				t.Fatalf("CutContent(%q) = %q, %d, %v\nwant              %q, %d, true", c.command, got, removed, ok, c.want, c.removed)
			}
			if again, more, ok := CutContent(got); !ok || again != got || more != 0 {
				t.Errorf("cut again = %q, %d, %v; want %q, 0, true", again, more, ok, got)
			}
		})
	}
}
