package report

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// workDir is an absolute, symlink-free cwd holding the named files.
func workDir(t *testing.T, files ...string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("line\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// rejectingPython answers every snippet with a parser error.
type rejectingPython struct {
	arrived chan<- struct{}
	release <-chan struct{}
	err     error
}

func (p rejectingPython) Analyze(ctx context.Context, snippets []cmdparse.Snippet) ([]cmdparse.PyResult, error) {
	if p.arrived != nil {
		p.arrived <- struct{}{}
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	out := make([]cmdparse.PyResult, len(snippets))
	for i, s := range snippets {
		out[i] = cmdparse.PyResult{ID: s.ID, Error: "invalid syntax"}
	}
	return out, nil
}

func parseFaults(t *testing.T, store *callmeter.Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM faults WHERE stage = 'parse' AND tool_use_id = 'b1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEnsureParsedCrashBetweenPartsAndFaultLeavesTheFault(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "def ("`, 10))
	if _, err := store.DB().Exec(`CREATE TRIGGER refuse_parse_fault BEFORE INSERT ON faults
		WHEN NEW.stage = 'parse' BEGIN SELECT RAISE(ABORT, 'test fault write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureParsed(ctx, store, "", rejectingPython{}); err == nil || !strings.Contains(err.Error(), "test fault write failure") {
		t.Fatalf("EnsureParsed error = %v, want the injected fault write failure", err)
	}
	var parts int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM command_parts WHERE tool_use_id = 'b1'").Scan(&parts); err != nil {
		t.Fatal(err)
	}
	if parts != 0 {
		t.Errorf("parts after failed fault write = %d, want the entire parse rolled back", parts)
	}
	if _, err := store.DB().Exec("DROP TRIGGER refuse_parse_fault"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureParsed(ctx, store, "", rejectingPython{}); err != nil {
		t.Fatal(err)
	}
	if n := parseFaults(t, store); n != 1 {
		t.Errorf("parse faults of b1 = %d after the next report, want 1: its error part is cached and never re-parsed", n)
	}
}

func TestEnsureParsedInterleavedReportsKeepOneFault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "def ("`, 10))
	const reports = 32
	arrived := make(chan struct{}, reports)
	release := make(chan struct{})
	errs := make(chan error, reports)
	py := rejectingPython{arrived: arrived, release: release}
	for range reports {
		go func() {
			_, err := EnsureParsed(ctx, store, "", py)
			errs <- err
		}()
	}
	for range reports {
		select {
		case <-arrived:
		case <-ctx.Done():
			close(release)
			t.Fatal("reports did not all reach the Python scan")
		}
	}
	close(release)
	for range reports {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := parseFaults(t, store); n != 1 {
		t.Errorf("parse faults of b1 = %d, want 1 for its one error part", n)
	}
}

func TestEnsureParsedSkipsANonStringCommand(t *testing.T) {
	for _, value := range []string{"7", "{}", "[]", "null"} {
		t.Run(value, func(t *testing.T) {
			store := openStore(t)
			c := bash("b1", "s", "", ms(time.Hour), workDir(t), "", 10)
			c.Input = callmeter.Ptr(`{"command":` + value + `,"description_bytes":1}`)
			seed(t, store, c)
			got, err := EnsureParsed(context.Background(), store, "", nil)
			if err != nil {
				t.Fatalf("EnsureParsed: %v", err)
			}
			if got != (ParseSummary{SkippedNoInput: 1}) {
				t.Fatalf("summary = %+v, want one skipped input", got)
			}
		})
	}
}

func TestEnsureParsedRetriesAPythonFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		py   cmdparse.PythonRunner
	}{
		{"unavailable", cmdparse.Python3{Program: "python3-not-installed-for-this-test"}},
		{"error", rejectingPython{err: errors.New("test Python process failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "print(1)"`, 10))
			if _, err := EnsureParsed(ctx, store, "", tc.py); err != nil {
				t.Fatal(err)
			}
			got, err := EnsureParsed(ctx, store, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			var status string
			if err := store.DB().QueryRow("SELECT parse_status FROM command_parts WHERE tool_use_id = 'b1'").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != cmdparse.StatusOK || got.Parsed != 1 {
				t.Fatalf("retry part = %s, parsed %d, want ok and one re-parsed call", status, got.Parsed)
			}
		})
	}
}

func TestEnsureParsedRetriesPythonUnavailableOnlyWhenPython3Resolves(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "print(1)"`, 10))
	saved := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if got, err := EnsureParsed(ctx, store, "", nil); err != nil || got.Parsed != 1 {
		t.Fatalf("initial no-python parse = %+v, %v, want one parsed call", got, err)
	}
	got, err := EnsureParsed(ctx, store, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parsed != 0 {
		t.Errorf("no-python run rewrote %d calls, want 0: a python-unavailable call is re-parsed while python3 does not resolve", got.Parsed)
	}
	var status string
	var parser int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT parse_status, parser FROM command_parts WHERE tool_use_id = 'b1'").Scan(&status, &parser); err != nil {
		t.Fatal(err)
	}
	if status != cmdparse.StatusPythonUnavailable || parser != cmdparse.Version {
		t.Errorf("cached Python part = (%s, parser %d), want (python-unavailable, parser %d)", status, parser, cmdparse.Version)
	}
	if n := parseFaults(t, store); n != 0 {
		t.Errorf("parse faults = %d, want 0", n)
	}
	t.Setenv("PATH", saved)
	if got, err := EnsureParsed(ctx, store, "", nil); err != nil || got.Parsed != 1 {
		t.Fatalf("restored-python parse = %+v, %v, want one parsed call", got, err)
	}
	if err := store.DB().QueryRowContext(ctx,
		"SELECT parse_status, parser FROM command_parts WHERE tool_use_id = 'b1'").Scan(&status, &parser); err != nil {
		t.Fatal(err)
	}
	if status != cmdparse.StatusOK || parser != cmdparse.Version {
		t.Errorf("restored Python part = (%s, parser %d), want (ok, parser %d)", status, parser, cmdparse.Version)
	}
	if got, err := EnsureParsed(ctx, store, "", nil); err != nil || got.Parsed != 0 {
		t.Fatalf("cached restored parse = %+v, %v, want no parsed calls", got, err)
	}
}

func TestEnsureParsedCachesPythonUnavailableWithoutPython3(t *testing.T) {
	for _, name := range []string{"new", "older-parser"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "print(1)"`, 10))
			if name == "older-parser" {
				if err := store.ReplaceCommandParts(ctx, "b1", []callmeter.CommandPart{
					{Lang: cmdparse.LangPython, ParseStatus: cmdparse.StatusPythonUnavailable, Parser: cmdparse.Version - 1},
				}); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", t.TempDir())
			for run, want := range []int{1, 0, 0} {
				if got, err := EnsureParsed(ctx, store, "", nil); err != nil || got.Parsed != want {
					t.Errorf("no-python run %d parsed %d calls, error %v, want %d", run, got.Parsed, err, want)
				}
			}
			var status string
			var parser int
			if err := store.DB().QueryRowContext(ctx,
				"SELECT parse_status, parser FROM command_parts WHERE tool_use_id = 'b1'").Scan(&status, &parser); err != nil {
				t.Fatal(err)
			}
			if status != cmdparse.StatusPythonUnavailable || parser != cmdparse.Version {
				t.Errorf("cached Python part = (%s, parser %d), want (python-unavailable, parser %d)", status, parser, cmdparse.Version)
			}
		})
	}
}

func TestEnsureParsedRetriesPythonErrorEveryRun(t *testing.T) {
	for _, name := range []string{"resolving", "missing"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			dir := workDir(t)
			seed(t, store, bash("b1", "s", "", ms(time.Hour), dir, `python3 -c "print(1)"`, 10))
			if name == "missing" {
				t.Setenv("PATH", t.TempDir())
			}
			for run := range 2 {
				if err := store.ReplaceCommandParts(ctx, "b1", []callmeter.CommandPart{
					{Lang: cmdparse.LangPython, ParseStatus: cmdparse.StatusPythonError, Parser: cmdparse.Version},
				}); err != nil {
					t.Fatal(err)
				}
				if got, err := EnsureParsed(ctx, store, "", nil); err != nil || got.Parsed != 1 {
					t.Fatalf("python-error run %d parsed %d calls, error %v, want 1", run, got.Parsed, err)
				}
				var status string
				if err := store.DB().QueryRowContext(ctx,
					"SELECT parse_status FROM command_parts WHERE tool_use_id = 'b1'").Scan(&status); err != nil {
					t.Fatal(err)
				}
				want := cmdparse.StatusOK
				if name == "missing" {
					want = cmdparse.StatusPythonUnavailable
				}
				if status != want {
					t.Errorf("retry status = %s, want %s", status, want)
				}
			}
		})
	}
}

// A call retried at every report for its python-error part keeps one parse
// fault for its shell error part, never one more per report.
func TestEnsureParsedRetriedCallKeepsOneParseFault(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `bash -c "if" && python3 -c "print(1)"`, 10))
	py := rejectingPython{err: errors.New("test Python process failed")}
	for run := range 3 {
		got, err := EnsureParsed(ctx, store, "", py)
		if err != nil || got.Parsed != 1 {
			t.Fatalf("run %d parsed %d calls, error %v, want the python-error call re-parsed", run, got.Parsed, err)
		}
		if n := parseFaults(t, store); n != 1 {
			t.Errorf("parse faults of b1 after run %d = %d, want 1: a re-parse replaces the call's parse faults", run, n)
		}
	}
}

func TestEnsureParsedRetriesPythonUnavailableWithInjectedRunner(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "print(1)"`, 10))
	if err := store.ReplaceCommandParts(ctx, "b1", []callmeter.CommandPart{
		{Lang: cmdparse.LangPython, ParseStatus: cmdparse.StatusPythonUnavailable, Parser: cmdparse.Version},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if got, err := EnsureParsed(ctx, store, "", rejectingPython{}); err != nil || got.Parsed != 1 {
		t.Fatalf("injected runner parsed %d calls, error %v, want 1", got.Parsed, err)
	}
	if n := parseFaults(t, store); n != 1 {
		t.Errorf("injected runner parse faults = %d, want 1", n)
	}
}

func TestEnsureParsedReparsesAfterTheStartCwdLands(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	start := workDir(t, "a.txt")
	post := filepath.Join(start, "after")
	if err := os.Mkdir(post, 0o700); err != nil {
		t.Fatal(err)
	}
	seed(t, store, bash("b1", "s", "", ms(time.Hour), post, "cat a.txt", 10))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatal(err)
	}
	seed(t, store, callmeter.Call{ToolUseID: "b1", Cwd: callmeter.Ptr(start)})
	got, err := EnsureParsed(ctx, store, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := loadParts(ctx, store, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	files := parts["b1"][0].files
	if len(files) != 1 || files[0].Path != filepath.Join(start, "a.txt") || !files[0].Exists || got.Parsed != 1 {
		t.Fatalf("parts resolve against the old cwd; parsed %d calls, want one start-cwd re-parse", got.Parsed)
	}
}

func TestEnsureParsedRejectsMalformedStoredJSON(t *testing.T) {
	store := openStore(t)
	c := bash("b1", "s", "", ms(time.Hour), workDir(t), "", 10)
	c.Input = callmeter.Ptr("{")
	seed(t, store, c)
	if _, err := EnsureParsed(context.Background(), store, "", nil); err == nil || !strings.Contains(err.Error(), "decode input of call b1:") {
		t.Fatalf("EnsureParsed error = %v, want a stored JSON decode error", err)
	}
}

func TestEnsureParsedCachesPartsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "a.txt")
	seed(t, store, bash("b1", "s", "", ms(time.Hour), dir, `cat a.txt && python3 -c "open('a.txt')"`, 10))
	seed(t, store, bash("b2", "s", "", ms(time.Hour), "rel/dir", "ls", 10))
	missing := cmdparse.Python3{Program: "python3-not-installed-for-this-test"}
	got, err := EnsureParsed(ctx, store, "", missing)
	if err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	if got.Parsed != 1 || got.SkippedRelative != 1 {
		t.Errorf("first run = %+v, want 1 parsed, 1 skipped for a relative cwd", got)
	}
	var files string
	if err := store.DB().QueryRowContext(ctx,
		"SELECT files FROM command_parts WHERE tool_use_id = 'b1' AND seq = 0").Scan(&files); err != nil {
		t.Fatalf("read cached part: %v", err)
	}
	if !strings.Contains(files, `read-whole\t\t1\t`+filepath.Join(dir, "a.txt")) {
		t.Errorf("cached files = %s, want the encoded read-whole of a.txt", files)
	}
	again, err := EnsureParsed(ctx, store, "", missing)
	if err != nil {
		t.Fatalf("EnsureParsed again: %v", err)
	}
	if again.Parsed != 0 {
		t.Errorf("second run parsed %d calls, want 0: python3 does not resolve, the python-unavailable call stays as stored", again.Parsed)
	}
	table, err := Commands(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	notes := strings.Join(table.Notes, "\n")
	for _, want := range []string{"1 snippets unparsed: python-unavailable", "1 Bash calls skipped by the parser"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if recovered, err := EnsureParsed(ctx, store, "", nil); err != nil || recovered.Parsed != 1 {
		t.Fatalf("working Python retry = %+v, %v, want one parsed call", recovered, err)
	}
	if cached, err := EnsureParsed(ctx, store, "", nil); err != nil || cached.Parsed != 0 {
		t.Fatalf("cached working parse = %+v, %v, want no parsed calls", cached, err)
	}
}

func TestEnsureParsedRecordsParseFault(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `python3 -c "def ("`, 10))
	if _, err := EnsureParsed(ctx, store, "", rejectingPython{}); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	var stage, message string
	if err := store.DB().QueryRowContext(ctx,
		"SELECT stage, error FROM faults WHERE tool_use_id = 'b1'").Scan(&stage, &message); err != nil {
		t.Fatalf("read parse fault: %v", err)
	}
	if stage != "parse" || strings.Contains(message, "invalid syntax") ||
		!strings.Contains(message, callmeter.ErrorNotStored+" (") {
		t.Errorf("fault = %s %q, want a parse fault with the parser's message only sized: it quotes the command", stage, message)
	}
}

// A part that failed to parse before any program was read names
// "no program parsed" in its fault, never an empty pair of parentheses.
func TestEnsureParsedMarksAPartWithNoProgram(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), `echo private-word (`, 10))
	if _, err := EnsureParsed(ctx, store, "", rejectingPython{}); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	var message string
	if err := store.DB().QueryRowContext(ctx,
		"SELECT error FROM faults WHERE tool_use_id = 'b1' AND stage = 'parse'").Scan(&message); err != nil {
		t.Fatalf("read parse fault: %v", err)
	}
	if !regexp.MustCompile(`^part 0 \(no program parsed\): error text not stored \([0-9]+ bytes\)$`).MatchString(message) {
		t.Errorf("fault = %q, want part 0 (no program parsed) with the message sized", message)
	}
}

func TestFileRefRoundTrip(t *testing.T) {
	for _, ref := range []cmdparse.FileRef{
		{Path: "/w/a b.txt", Action: cmdparse.ActionReadRange, Range: "1,20", Exists: true},
		{Path: "/w/gone\tscratch.txt", Action: cmdparse.ActionWrite},
	} {
		got, err := DecodeFileRef(EncodeFileRef(ref))
		if err != nil || got != ref {
			t.Errorf("round trip = %+v, %v; want %+v", got, err, ref)
		}
	}
	// The three-field form predates exists; a stored value of any other shape
	// is an error naming it, never a guess.
	for _, stored := range []string{"no tabs", "read-whole\t\t/w/a.txt", "read-whole\t\tyes\t/w/a.txt"} {
		if _, err := DecodeFileRef(stored); err == nil || !strings.Contains(err.Error(), strconv.Quote(stored)) {
			t.Errorf("DecodeFileRef(%q) error = %v, want one naming the stored value", stored, err)
		}
	}
}

func TestEnsureParsedStoresConditional(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "a.txt")
	seed(t, store, bash("b1", "s", "", ms(time.Hour), dir, `test -f a.txt && cat a.txt`, 10))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	for seq, want := range []int64{0, 1} {
		var got int64
		if err := store.DB().QueryRowContext(ctx,
			"SELECT conditional FROM command_parts WHERE tool_use_id = 'b1' AND seq = ?", seq).Scan(&got); err != nil {
			t.Fatalf("read part %d: %v", seq, err)
		}
		if got != want {
			t.Errorf("part %d conditional = %d, want %d", seq, got, want)
		}
	}
}

// TestEnsureParsedReparsesAnOlderParsersParts: parts cached by an older
// parser are parsed again, and that parse's faults replace the old ones, so a
// parser fix reaches every call still in the window.
func TestEnsureParsedReparsesAnOlderParsersParts(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := workDir(t, "a.txt")
	seed(t, store, bash("b1", "s", "", ms(time.Hour), dir, "cat a.txt", 10))
	stale := []callmeter.CommandPart{
		{Seq: 0, Lang: cmdparse.LangSh, ParseStatus: cmdparse.StatusError, Parser: cmdparse.Version - 1},
	}
	if err := store.ReplaceCommandParts(ctx, "b1", stale); err != nil {
		t.Fatalf("seed stale parts: %v", err)
	}
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), SessionID: "s", ToolUseID: "b1", Stage: callmeter.StageParse, Error: "stale parse",
	}); err != nil {
		t.Fatalf("seed stale fault: %v", err)
	}
	got, err := EnsureParsed(ctx, store, "", nil)
	if err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	if got.Parsed != 1 {
		t.Errorf("parsed %d calls, want the stale call parsed again", got.Parsed)
	}
	var status string
	var parser int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT parse_status, parser FROM command_parts WHERE tool_use_id = 'b1' AND seq = 0").
		Scan(&status, &parser); err != nil {
		t.Fatalf("read part: %v", err)
	}
	if status != cmdparse.StatusOK || parser != cmdparse.Version {
		t.Errorf("part = (%s, parser %d), want (ok, parser %d)", status, parser, cmdparse.Version)
	}
	var faults int
	if err := store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM faults WHERE tool_use_id = 'b1' AND stage = ?", callmeter.StageParse).
		Scan(&faults); err != nil {
		t.Fatalf("count faults: %v", err)
	}
	if faults != 0 {
		t.Errorf("%d parse faults left from the stale parse, want 0", faults)
	}
}
