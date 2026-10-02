package callmeter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// The tools whose outcome OutcomeColumns reads.
const (
	toolBash      = "Bash"
	toolWrite     = "Write"
	toolEdit      = "Edit"
	toolMultiEdit = "MultiEdit"
)

// outcomeResponse is the part of a successful tool_response the outcome
// columns read.
type outcomeResponse struct {
	Type            string          `json:"type"`
	StructuredPatch json.RawMessage `json:"structuredPatch"`
	Stdout          *string         `json:"stdout"`
}

// OutcomeColumns sets, from a successful call's own input and response, what
// the call did: lines_added and lines_removed for Write, Edit and MultiEdit
// (the `+` and `-` lines of every structuredPatch hunk; a Write that created a
// file has no hunks, so its input content's lines are the added ones), and
// commit_sha and commit_branch for a Bash `git … commit` whose stdout carries
// git's `[branch sha] subject` line. The amend of a commit counts: it made a
// new one. Any other tool is left untouched.
//
// A response that cannot be read is the error, and every outcome column stays
// NULL — never a partial count; the caller turns the error into its own fault.
// A commit whose stdout shows no such line sets nothing and is no error: the
// call failed to commit, it did not fail to be read.
func OutcomeColumns(c *Call, tool string, input, response json.RawMessage) error {
	switch tool {
	case toolWrite, toolEdit, toolMultiEdit:
		return lineColumns(c, tool, input, response)
	case toolBash:
		return commitColumns(c, input, response)
	}
	return nil
}

func lineColumns(c *Call, tool string, input, response json.RawMessage) error {
	var fields outcomeResponse
	if err := json.Unmarshal(response, &fields); err != nil {
		return fmt.Errorf("decode %s response (%d bytes): %w", tool, len(response), err)
	}
	patch := bytes.TrimSpace(fields.StructuredPatch)
	if len(patch) == 0 || bytes.Equal(patch, []byte("null")) {
		return fmt.Errorf("%s response has no structuredPatch", tool)
	}
	var hunks []struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(patch, &hunks); err != nil {
		return fmt.Errorf("%s response: structuredPatch is not a list of hunks with lines: %w", tool, err)
	}
	var added, removed int64
	for _, hunk := range hunks {
		for _, line := range hunk.Lines {
			switch {
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				removed++
			}
		}
	}
	if tool == toolWrite && fields.Type == "create" && len(hunks) == 0 {
		var written struct {
			Content *string `json:"content"`
		}
		if err := json.Unmarshal(input, &written); err != nil {
			return fmt.Errorf("decode Write input (%d bytes): %w", len(input), err)
		}
		if written.Content == nil {
			return fmt.Errorf("Write input has no content to count the lines of a created file")
		}
		added = countLines(*written.Content)
	}
	c.LinesAdded, c.LinesRemoved = Ptr(added), Ptr(removed)
	return nil
}

// countLines is how many lines s holds: a last line without its newline counts.
func countLines(s string) int64 {
	n := int64(strings.Count(s, "\n"))
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// gitCommitCommand finds `git` before `commit` anywhere in a command, so
// `git -C dir commit`, `git commit --amend` and `git add . && git commit`
// all count; the result line is what proves a commit was made.
var gitCommitCommand = regexp.MustCompile(`(?s)\bgit\b.*\bcommit\b`)

// commitLine is git's own summary of a commit, `[branch sha] subject`; a first
// commit reads `[main (root-commit) sha]` and a detached head
// `[detached HEAD sha]`.
var commitLine = regexp.MustCompile(`^\[(detached HEAD|[^\s\]]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\]`)

func commitColumns(c *Call, input, response json.RawMessage) error {
	command, err := BashCommand(input)
	if err != nil {
		return err
	}
	if !gitCommitCommand.MatchString(command) {
		return nil
	}
	var fields outcomeResponse
	if err := json.Unmarshal(response, &fields); err != nil {
		return fmt.Errorf("decode Bash response (%d bytes): %w", len(response), err)
	}
	if fields.Stdout == nil {
		return nil
	}
	CommitFromText(c, command, *fields.Stdout)
	return nil
}

// CommitFromText sets a git commit's SHA and branch from its summary in text,
// whether stdout or an error result, without retaining the commit message.
func CommitFromText(c *Call, command, text string) {
	if !gitCommitCommand.MatchString(command) {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		if match := commitLine.FindStringSubmatch(line); match != nil {
			c.CommitBranch, c.CommitSHA = Ptr(match[1]), Ptr(match[2])
			return
		}
	}
}

// BashCommand is the command a Bash call's tool_input carries.
func BashCommand(input json.RawMessage) (string, error) {
	var fields struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(input, &fields); err != nil {
		return "", fmt.Errorf("decode Bash input (%d bytes): %w", len(input), err)
	}
	return fields.Command, nil
}

// TestRunnerOf names the test runner a shell command starts: go, pytest, npm,
// vitest or cargo, "" when none. It reads the first segment (split on `;`,
// `&&`, `||`, `|`, `&` and a newline, never inside quotes) whose leading words —
// after NAME=value assignments and env, timeout or nice wrappers — are
// `go [-C dir] test`, `pytest`,
// `python[3] -m pytest`, `npm test`, `npm run test`, `npm t`, `[npx ]vitest` or
// `cargo test`. A runner's name mentioned as an argument (`echo go test`) is
// not a run.
func TestRunnerOf(command string) string {
	for _, segment := range shellSegments(command) {
		words := leadingWords(segment)
		if runner := runnerOfWords(words); runner != "" {
			return runner
		}
	}
	return ""
}

var (
	assignment    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	pythonCommand = regexp.MustCompile(`^python3?(\.[0-9]+)?$`)
)

func runnerOfWords(words []string) string {
	// Wrapper options mirror env, timeout and nice in cmdparse/wrappers.go.
unwrap:
	for len(words) > 0 {
		if assignment.MatchString(words[0]) {
			words = words[1:]
			continue
		}
		var valueShort string
		var valueLong map[string]bool
		var assigns, operand, dash bool
		switch words[0] {
		case "env":
			valueShort, assigns, dash = "uCS", true, true
			valueLong = map[string]bool{"--unset": true, "--chdir": true, "--split-string": true}
		case "timeout":
			valueShort, operand = "ks", true
			valueLong = map[string]bool{"--kill-after": true, "--signal": true}
		case "nice":
			valueShort = "n"
			valueLong = map[string]bool{"--adjustment": true}
		default:
			break unwrap
		}
		words = words[1:]
		flags, operandSeen := true, false
		skip := 0
	options:
		for skip < len(words) {
			word := words[skip]
			skip++
			switch {
			case flags && word == "--":
				flags = false
			case flags && dash && word == "-":
			case flags && strings.HasPrefix(word, "--"):
				name, _, hasValue := strings.Cut(word, "=")
				if !hasValue && valueLong[name] && skip < len(words) {
					skip++
				}
			case flags && len(word) > 1 && word[0] == '-':
				for j := 1; j < len(word); j++ {
					if strings.IndexByte(valueShort, word[j]) < 0 {
						continue
					}
					if j == len(word)-1 && skip < len(words) {
						skip++
					}
					break
				}
			case assigns && assignment.MatchString(word):
			case operand && !operandSeen:
				operandSeen = true
			default:
				skip--
				break options
			}
		}
		words = words[skip:]
	}
	at := func(i int) string {
		if i < len(words) {
			return words[i]
		}
		return ""
	}
	switch {
	case at(0) == "go" && (at(1) == "test" || at(1) == "-C" && at(2) != "" && at(3) == "test"):
		return "go"
	case at(0) == "pytest", pythonCommand.MatchString(at(0)) && at(1) == "-m" && at(2) == "pytest":
		return "pytest"
	case at(0) == "npm" && (at(1) == "test" || at(1) == "t" || at(1) == "run" && at(2) == "test"):
		return "npm"
	case at(0) == "vitest", at(0) == "npx" && at(1) == "vitest":
		return "vitest"
	case at(0) == "cargo" && at(1) == "test":
		return "cargo"
	}
	return ""
}

// shellSegments splits command at the separators outside quotes.
func shellSegments(command string) []string {
	var segments []string
	var current strings.Builder
	var quote rune
	escaped := false
	for _, r := range command {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ';' || r == '&' || r == '|' || r == '\n':
			segments = append(segments, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(r)
	}
	return append(segments, current.String())
}

// leadingWords splits one segment into words at whitespace outside quotes,
// dropping the quote characters and the parentheses or braces a subshell or
// group puts around a command.
func leadingWords(segment string) []string {
	var words []string
	var current strings.Builder
	var quote rune
	escaped, inWord := false, false
	flush := func() {
		if word := strings.Trim(current.String(), "(){}"); inWord && word != "" {
			words = append(words, word)
		}
		current.Reset()
		inWord = false
	}
	for _, r := range segment {
		switch {
		case escaped:
			escaped = false
			current.WriteRune(r)
			inWord = true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}
