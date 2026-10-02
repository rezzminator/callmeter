package callmeter

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tool_response shapes are those a real Claude Code PostToolUse carries:
// Write {content, filePath, originalFile, structuredPatch, type, userModified}
// and Edit {filePath, newString, oldString, originalFile, replaceAll,
// structuredPatch, userModified}.
const (
	editResponse = `{"filePath":"/tmp/demo-proj/a.txt","oldString":"two","newString":"TWO","originalFile":"one\ntwo\nthree\n",` +
		`"replaceAll":false,"userModified":false,` +
		`"structuredPatch":[{"oldStart":1,"oldLines":3,"newStart":1,"newLines":3,"lines":[" one","-two","+TWO"," three"]}]}`
	writeCreateResponse = `{"type":"create","filePath":"/tmp/demo-proj/new.txt","content":"one\ntwo\nthree\n",` +
		`"structuredPatch":[],"originalFile":null,"userModified":false}`
)

func TestOutcomeColumnsLines(t *testing.T) {
	cases := map[string]struct {
		tool, input, response string
		added, removed        int64
	}{
		"Edit lines": {"Edit", `{"file_path":"/tmp/demo-proj/a.txt"}`, editResponse, 1, 1},
		"Write create counts the content": {
			"Write", `{"file_path":"/tmp/demo-proj/new.txt","content":"one\ntwo\nthree\n"}`, writeCreateResponse, 3, 0,
		},
		"Write create, last line without a newline": {
			"Write", `{"file_path":"x","content":"one\ntwo"}`, `{"type":"create","structuredPatch":[]}`, 2, 0,
		},
		"Write create of an empty file": {
			"Write", `{"file_path":"x","content":""}`, `{"type":"create","structuredPatch":[]}`, 0, 0,
		},
		"Write update sums every hunk": {
			"Write", `{"file_path":"x","content":"ignored"}`,
			`{"type":"update","structuredPatch":[` +
				`{"oldStart":1,"oldLines":2,"newStart":1,"newLines":3,"lines":[" a","+b","+c"," d"]},` +
				`{"oldStart":9,"oldLines":2,"newStart":10,"newLines":1,"lines":["-x","-y","+z"]}]}`,
			3, 2,
		},
		"MultiEdit sums every hunk": {
			"MultiEdit", `{"file_path":"x","edits":[]}`,
			`{"structuredPatch":[{"lines":["-a","+b"]},{"lines":["+c"]},{"lines":["-d"," e"]}]}`, 2, 2,
		},
		"a no-newline marker is neither": {
			"Edit", `{"file_path":"x"}`, `{"structuredPatch":[{"lines":["-a","\\ No newline at end of file","+b",""]}]}`, 1, 1,
		},
		"an update with no hunks changed nothing": {"Edit", `{"file_path":"x"}`, `{"structuredPatch":[]}`, 0, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c Call
			if err := OutcomeColumns(&c, tc.tool, json.RawMessage(tc.input), json.RawMessage(tc.response)); err != nil {
				t.Fatalf("OutcomeColumns: %v", err)
			}
			if c.LinesAdded == nil || c.LinesRemoved == nil || *c.LinesAdded != tc.added || *c.LinesRemoved != tc.removed {
				t.Errorf("lines added, removed = %v, %v; want %d, %d", deref(c.LinesAdded), deref(c.LinesRemoved), tc.added, tc.removed)
			}
		})
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestOutcomeColumnsMalformedResponse: a response whose patch cannot be read
// is an error naming the field, and every outcome column stays NULL.
func TestOutcomeColumnsMalformedResponse(t *testing.T) {
	cases := map[string]struct{ tool, input, response, field string }{
		"patch a string":         {"Edit", `{}`, `{"structuredPatch":"nope"}`, "structuredPatch"},
		"patch an object":        {"Edit", `{}`, `{"structuredPatch":{"lines":["+a"]}}`, "structuredPatch"},
		"hunk lines not a list":  {"Edit", `{}`, `{"structuredPatch":[{"lines":"+a"}]}`, "structuredPatch"},
		"patch absent":           {"Write", `{"content":"a"}`, `{"type":"create"}`, "structuredPatch"},
		"patch null":             {"MultiEdit", `{}`, `{"structuredPatch":null}`, "structuredPatch"},
		"response not an object": {"Edit", `{}`, `"done"`, "Edit response"},
		"create without content": {"Write", `{"file_path":"x"}`, `{"type":"create","structuredPatch":[]}`, "content"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c Call
			err := OutcomeColumns(&c, tc.tool, json.RawMessage(tc.input), json.RawMessage(tc.response))
			if err == nil {
				t.Fatal("OutcomeColumns returned no error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name %q", err, tc.field)
			}
			if c.LinesAdded != nil || c.LinesRemoved != nil {
				t.Errorf("lines = %v, %v after an error, want NULL, NULL", deref(c.LinesAdded), deref(c.LinesRemoved))
			}
		})
	}
}

func TestOutcomeColumnsGitCommit(t *testing.T) {
	cases := map[string]struct{ command, stdout, sha, branch string }{
		"commit": {
			`git commit -m "fix: thing"`, "[develop 1a2b3c4] fix: thing\n 1 file changed, 2 insertions(+)\n", "1a2b3c4", "develop",
		},
		"root commit":     {`git commit -m init`, "[main (root-commit) abc1234] init\n", "abc1234", "main"},
		"detached head":   {`git commit -m x`, "[detached HEAD abc1234] x\n", "abc1234", "detached HEAD"},
		"git -C dir":      {`git -C /tmp/demo-proj commit -m x`, "[develop 1a2b3c4] x\n", "1a2b3c4", "develop"},
		"amend":           {`git commit --amend --no-edit`, "[develop 9f8e7d6] x\n Date: Mon\n", "9f8e7d6", "develop"},
		"add then commit": {`git add . && git commit -m x`, "hook ran\n[feat/x 1a2b3c4d5e6f] x\n", "1a2b3c4d5e6f", "feat/x"},
		"full-length sha": {
			`git commit -m x`, "[develop " + strings.Repeat("a1", 20) + "] x\n", strings.Repeat("a1", 20), "develop",
		},
		"first matching line wins": {`git commit -m x`, "[a 1111111] one\n[b 2222222] two\n", "1111111", "a"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"command": tc.command})
			response, _ := json.Marshal(map[string]any{"stdout": tc.stdout, "stderr": "", "interrupted": false})
			var c Call
			if err := OutcomeColumns(&c, "Bash", input, response); err != nil {
				t.Fatalf("OutcomeColumns: %v", err)
			}
			if c.CommitSHA == nil || c.CommitBranch == nil || *c.CommitSHA != tc.sha || *c.CommitBranch != tc.branch {
				t.Errorf("commit sha, branch = %v, %v; want %s, %s", c.CommitSHA, c.CommitBranch, tc.sha, tc.branch)
			}
		})
	}
}

// TestOutcomeColumnsNoCommit: what is not a commit sets nothing and is no error.
func TestOutcomeColumnsNoCommit(t *testing.T) {
	cases := map[string]struct{ tool, command, stdout string }{
		"not a commit command":           {"Bash", `git status`, "[develop 1a2b3c4] looks like one\n"},
		"commit command, no summary":     {"Bash", `git commit -m x`, "nothing to commit, working tree clean\n"},
		"a sha too short":                {"Bash", `git commit -m x`, "[develop abc12] x\n"},
		"a summary away from line start": {"Bash", `git commit -m x`, "log: [develop 1a2b3c4] x\n"},
		"commit written before git":      {"Bash", `echo commit; ls`, "[develop 1a2b3c4] x\n"},
		"another tool":                   {"Read", `git commit -m x`, "[develop 1a2b3c4] x\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			input, _ := json.Marshal(map[string]any{"command": tc.command})
			response, _ := json.Marshal(map[string]any{"stdout": tc.stdout})
			c := Call{ToolUseID: "toolu_x"}
			if err := OutcomeColumns(&c, tc.tool, input, response); err != nil {
				t.Fatalf("OutcomeColumns: %v", err)
			}
			if c.CommitSHA != nil || c.CommitBranch != nil {
				t.Errorf("commit sha, branch = %v, %v; want NULL, NULL", c.CommitSHA, c.CommitBranch)
			}
		})
	}
}

func TestOutcomeColumnsBashUnreadable(t *testing.T) {
	var c Call
	if err := OutcomeColumns(&c, "Bash", json.RawMessage(`not json`), json.RawMessage(`{"stdout":""}`)); err == nil {
		t.Error("an undecodable Bash input returned no error")
	}
	if err := OutcomeColumns(&c, "Bash", json.RawMessage(`{"command":"git commit -m x"}`), json.RawMessage(`"plain text"`)); err == nil {
		t.Error("a Bash commit with a response that is not an object returned no error")
	}
	if c.CommitSHA != nil || c.CommitBranch != nil {
		t.Errorf("commit columns set to %v, %v after errors", c.CommitSHA, c.CommitBranch)
	}
}

func TestCommitFromText(t *testing.T) {
	cases := map[string]struct{ command, text, sha, branch string }{
		"commit":                       {`git commit -m x`, "[develop 1a2b3c4] x\n 1 file changed\n", "1a2b3c4", "develop"},
		"error after commit":           {`git commit -m x`, "hook ran\n[develop 1a2b3c4] x\npost-commit hook failed\n", "1a2b3c4", "develop"},
		"root commit":                  {`git commit -m init`, "[main (root-commit) abc1234] init\n", "abc1234", "main"},
		"detached head":                {`git commit -m x`, "[detached HEAD abc1234] x\n", "abc1234", "detached HEAD"},
		"git -C dir":                   {`git -C /tmp/demo-proj commit -m x`, "[develop 1a2b3c4] x\n", "1a2b3c4", "develop"},
		"amend":                        {`git commit --amend --no-edit`, "[develop 9f8e7d6] x\n Date: Mon\n", "9f8e7d6", "develop"},
		"add then commit":              {`git add . && git commit -m x`, "hook ran\n[feat/x 1a2b3c4d5e6f] x\n", "1a2b3c4d5e6f", "feat/x"},
		"full-length sha":              {`git commit -m x`, "[develop " + strings.Repeat("a1", 20) + "] x\n", strings.Repeat("a1", 20), "develop"},
		"first matching line wins":     {`git commit -m x`, "[a 1111111] one\n[b 2222222] two\n", "1111111", "a"},
		"not a commit command":         {`git status`, "[develop 1a2b3c4] x\n", "", ""},
		"no summary":                   {`git commit -m x`, "nothing to commit, working tree clean\n", "", ""},
		"short sha":                    {`git commit -m x`, "[develop abc12] x\n", "", ""},
		"summary away from line start": {`git commit -m x`, "log: [develop 1a2b3c4] x\n", "", ""},
		"empty text":                   {`git commit -m x`, "", "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var c Call
			CommitFromText(&c, tc.command, tc.text)
			if tc.sha == "" {
				if c.CommitSHA != nil || c.CommitBranch != nil {
					t.Error("commit columns set without a commit summary and command")
				}
				return
			}
			if c.CommitSHA == nil || c.CommitBranch == nil || *c.CommitSHA != tc.sha || *c.CommitBranch != tc.branch {
				t.Errorf("commit sha, branch = %v, %v; want %s, %s", c.CommitSHA, c.CommitBranch, tc.sha, tc.branch)
			}
		})
	}
}

func TestTestRunnerOf(t *testing.T) {
	cases := map[string]string{
		"go -C pfm test ./...":                             "go",
		"go -C":                                            "",
		"go -C pfm":                                        "",
		"timeout 600 go test ./...":                        "go",
		"timeout -s KILL 600 go test ./...":                "go",
		"timeout -k 5 600 go test ./...":                   "go",
		"timeout -sKILL -k5 600 go test":                   "go",
		"timeout --signal KILL --kill-after 5 600 go test": "go",
		"timeout --signal=KILL --kill-after=5 600 go test": "go",
		"timeout --foreground -- 600 go test":              "go",
		"timeout 600":                                      "",
		"timeout -s":                                       "",
		"timeout":                                          "",
		"env FOO=1 go test":                                "go",
		"env -i -u FOO -C pfm -S 'go test' go test":        "go",
		"env -uFOO -Cpfm -Signored go test":                "go",
		"env --unset FOO --chdir pfm --split-string ignored go test": "go",
		"env --unset=FOO --chdir=pfm --split-string=ignored go test": "go",
		"env - -- FOO=1 go test":                                     "go",
		"env FOO=1":                                                  "",
		"env --unset":                                                "",
		"env":                                                        "",
		"nice -n 5 go test":                                          "go",
		"nice -5 go test":                                            "go",
		"nice -n5 go test":                                           "go",
		"nice --adjustment=5 go test":                                "go",
		"nice --adjustment 5 go test":                                "go",
		"nice -- go test":                                            "go",
		"nice -n":                                                    "",
		"nice":                                                       "",
		"FOO=1 env BAR=2 timeout -k 5 600 nice -n 5 go -C pfm test ./...": "go",
		"nice -n 5 timeout 600 env FOO=1 pytest":                          "pytest",
		"env nice timeout 600 cargo test":                                 "cargo",
		"timeout 600 echo go test":                                        "",
		"env FOO=1 go build":                                              "",
		"go test ./...":                                                   "go",
		"pytest -q":                                                       "pytest",
		"python3 -m pytest":                                               "pytest",
		"python -m pytest tests/":                                         "pytest",
		"npm test":                                                        "npm",
		"npm run test":                                                    "npm",
		"npm t":                                                           "npm",
		"npx vitest run":                                                  "vitest",
		"vitest":                                                          "vitest",
		"cargo test":                                                      "cargo",
		"cargo test -- --nocapture":                                       "cargo",
		"cd pkg && go test ./...":                                         "go",
		"go build ./... && pytest -x":                                     "pytest",
		"FOO=1 BAR=two pytest":                                            "pytest",
		"make; npm test":                                                  "npm",
		"false || cargo test":                                             "cargo",
		"go test ./... 2>&1 | tee out.txt":                                "go",
		"echo hi\nvitest run":                                             "vitest",
		"(cd web && npm test)":                                            "npm",
		`"go" test`:                                                       "go",
		"go build ./...":                                                  "",
		"echo go test":                                                    "",
		`echo "go test ./..."`:                                            "",
		`echo 'a; go test' && ls`:                                         "",
		`echo "a && pytest"`:                                              "",
		"ls | grep go test":                                               "",
		"npm run build":                                                   "",
		"npm install":                                                     "",
		"cargo build":                                                     "",
		"go testing":                                                      "",
		"pytest-xdist":                                                    "",
		"python3 script.py":                                               "",
		"":                                                                "",
		"git commit -m 'run go test first'":                               "",
		`git commit -m "npm test is green"`:                               "",
		`grep -r "vitest" .`:                                              "",
		`FOO="a b" go test`:                                               "go",
	}
	for command, want := range cases {
		if got := TestRunnerOf(command); got != want {
			t.Errorf("TestRunnerOf(%q) = %q, want %q", command, got, want)
		}
	}
}
