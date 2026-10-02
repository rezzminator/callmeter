package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

func TestFaultsCountsByStageAndStatusThenLatest(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for i, stage := range []string{callmeter.StagePayload, callmeter.StagePayload, callmeter.StageParse} {
		if err := store.AddFault(ctx, callmeter.Fault{
			TS: ms(time.Duration(3-i) * time.Hour), Stage: stage, Error: stage + " broke",
		}); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), "node -e 'x'", 5))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Faults(ctx, store, Filter{Limit: 2}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	counts := map[string]string{}
	var latest []string
	for _, row := range table.Rows {
		switch row[0] {
		case "stage", "parse":
			counts[row[0]+":"+row[1]] = row[2]
		case "fault":
			latest = append(latest, row[6])
		}
	}
	for key, want := range map[string]string{"stage:payload": "2", "stage:parse": "1", "stage:store": "0", "stage:binary": "0", "parse:unparsed": "1"} {
		if counts[key] != want {
			t.Errorf("count %s = %q, want %s (rows %v)", key, counts[key], want, table.Rows)
		}
	}
	if len(latest) != 2 || latest[0] != "parse broke" {
		t.Errorf("latest faults = %v, want 2, newest first", latest)
	}
}

// A Python heredoc whose body the store cut parsed fine as shell: its part is
// counted under its own status, script-body, and adds no parse fault.
func TestFaultsScriptBodyIsNoParseFault(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), "python3 - <<'EOF'\nEOF\nls", 5))
	if _, err := EnsureParsed(ctx, store, "", nil); err != nil {
		t.Fatalf("EnsureParsed: %v", err)
	}
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	counts := map[string]string{}
	for _, row := range table.Rows {
		if row[0] == "fault" {
			t.Errorf("fault row %v, want none", row)
		}
		counts[row[0]+":"+row[1]] = row[2]
	}
	for key, want := range map[string]string{"stage:parse": "0", "parse:script-body": "1", "parse:ok": "1", "parse:unparsed": ""} {
		if counts[key] != want {
			t.Errorf("count %s = %q, want %q (rows %v)", key, counts[key], want, table.Rows)
		}
	}
}

func TestFaultsEmptyWindow(t *testing.T) {
	table, err := Faults(context.Background(), openStore(t), Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("empty store rows = %v, want none", table.Rows)
	}
}

// A fault carrying a session resolves its chat name through a NameOf that
// queries the same one-connection store: the report must return, not wait on
// its own open scan.
func TestFaultsSessionChatNameFromSameStore(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), Stage: callmeter.StageStore, SessionID: "s1", Error: "store broke",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	nameOf := func(session string) (string, error) {
		var n int64
		if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM faults").Scan(&n); err != nil {
			return "", err
		}
		return "chat-" + session, nil
	}
	done := make(chan struct{})
	var table *Table
	var err error
	go func() {
		defer close(done)
		table, err = Faults(ctx, store, Filter{}, nameOf)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Faults did not return: the name lookup waits on the open scan")
	}
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	for _, row := range table.Rows {
		if row[0] == "fault" {
			if row[4] != "chat-s1" {
				t.Errorf("fault CHAT = %q, want chat-s1 (row %v)", row[4], row)
			}
			return
		}
	}
	t.Fatalf("no fault row in %v", table.Rows)
}

func TestFaultsNameOfErrorSurfacesAsNote(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), Stage: callmeter.StageStore, SessionID: "s1", Error: "store broke",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Faults(ctx, store, Filter{}, func(string) (string, error) { return "", errNames })
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	if len(table.Notes) == 0 {
		t.Errorf("notes = none, want the NameOf error noted")
	}
	for _, row := range table.Rows {
		if row[0] == "fault" && row[4] != "?" {
			t.Errorf("fault CHAT = %q, want ?", row[4])
		}
	}
}

func TestFaultsBinaryStageListedNotUnknown(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for i := 0; i < 2; i++ {
		if err := store.AddFault(ctx, callmeter.Fault{
			TS: ms(time.Duration(i+1) * time.Hour), Stage: callmeter.StageBinary, Error: "binary missing",
		}); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var stageRows []string
	for _, row := range table.Rows {
		if row[0] != "stage" {
			continue
		}
		stageRows = append(stageRows, row[1]+"="+row[2])
	}
	want := []string{"payload=0", "store=0", "transcript=0", "parse=0", "binary=2", "terminated=0"}
	if strings.Join(stageRows, ",") != strings.Join(want, ",") {
		t.Errorf("stage rows = %v, want %v", stageRows, want)
	}
}

func TestFaultsStageListWithNoFaults(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, bash("b1", "s", "", ms(time.Hour), workDir(t), "ls", 5))
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var stageRows []string
	for _, row := range table.Rows {
		if row[0] == "stage" {
			stageRows = append(stageRows, row[1]+"="+row[2])
		}
	}
	want := []string{"payload=0", "store=0", "transcript=0", "parse=0", "binary=0", "terminated=0"}
	if strings.Join(stageRows, ",") != strings.Join(want, ",") {
		t.Errorf("stage rows = %v, want %v", stageRows, want)
	}
}

// TestFaultsTerminatedStageListedNotUnknown: a terminated fault is counted on
// its own stage row, in the contract's order, never as an unknown stage.
func TestFaultsTerminatedStageListedNotUnknown(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), Stage: callmeter.StageTerminated, Error: "SubagentStop: terminated by SIGTERM",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Faults(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var stageRows []string
	for _, row := range table.Rows {
		if row[0] == "stage" {
			stageRows = append(stageRows, row[1]+"="+row[2])
		}
	}
	want := []string{"payload=0", "store=0", "transcript=0", "parse=0", "binary=0", "terminated=1"}
	if strings.Join(stageRows, ",") != strings.Join(want, ",") {
		t.Errorf("stage rows = %v, want %v", stageRows, want)
	}
}

// Transcript lines in the shape Claude Code writes, invented values: a
// prompt, a model answer, the `<synthetic>` message an API error leaves in
// place of one, and the bookkeeping entries that follow it.
const (
	refusalPrompt  = `{"type":"user","timestamp":"2026-09-23T09:00:00.000Z","message":{"role":"user","content":"invented prompt"}}`
	refusalAnswer  = `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","message":{"id":"msg-demo-ok","model":"claude-demo","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"invented answer"}],"usage":{"input_tokens":3,"output_tokens":4}}}`
	refusalTail    = `{"type":"last-prompt","sessionId":"s-demo"}` + "\n" + `{"type":"cost-state","sessionId":"s-demo"}`
	refusalMessage = "invented refusal text"
)

func refusalLine(kind string) string {
	return `{"type":"assistant","timestamp":"2026-09-23T09:00:01.000Z","isApiErrorMessage":true,"error":"` + kind +
		`","message":{"id":"msg-demo-err","model":"<synthetic>","role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"` +
		refusalMessage + `"}],"usage":{"input_tokens":0,"output_tokens":0}}}`
}

// A turn refused by the API shows as a refusal with its error kind: from its
// StopFailure event, or, when the headless exit cancelled that async hook,
// from the `<synthetic>` message ending its transcript. The message text never
// reaches the report.
func TestFaultsListsRefusals(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	dir := t.TempDir()
	transcript := func(name string, lines ...string) string {
		path := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		return path
	}
	p := callmeter.Ptr[string]
	sessions := map[string]string{
		"s-hook":  transcript("s-hook", refusalPrompt, refusalLine("oauth_org_not_allowed"), refusalTail),
		"s-lost":  transcript("s-lost", refusalPrompt, refusalLine("rate_limit"), refusalTail),
		"s-odd":   transcript("s-odd", refusalPrompt, refusalLine("Not A Kind: "+refusalMessage), refusalTail),
		"s-open":  transcript("s-open", refusalPrompt, refusalAnswer, refusalTail),
		"s-done":  transcript("s-done", refusalPrompt, refusalLine("rate_limit"), refusalPrompt, refusalAnswer),
		"s-gone":  filepath.Join(dir, "absent.jsonl"),
		"s-built": transcript("s-built", refusalPrompt, refusalLine("rate_limit"), refusalTail),
	}
	for i, id := range []string{"s-hook", "s-lost", "s-odd", "s-open", "s-done", "s-gone", "s-built"} {
		at := ms(time.Duration(7-i) * time.Hour)
		batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
			return tx.TouchSession(ctx, callmeter.Session{SessionID: id, TS: at, TranscriptPath: p(sessions[id])})
		})
		seedEvent(t, store, callmeter.Event{EventID: id + "-up", Event: "UserPromptSubmit", TS: at, SessionID: p(id)})
		seedEvent(t, store, callmeter.Event{EventID: id + "-end", Event: callmeter.EventSessionEnd, TS: at + 2, SessionID: p(id), Reason: p("other")})
	}
	seedEvent(t, store, callmeter.Event{EventID: "s-hook-sf", Event: "StopFailure", TS: ms(7*time.Hour) + 1, SessionID: p("s-hook"), ErrorType: p("oauth_org_not_allowed")})
	// SessionEnd rebuilt this one from the transcript (RecoverStopFailure): the
	// row says so, never passing for the hook's own.
	seedEvent(t, store, callmeter.Event{EventID: "s-built-sf", Event: "StopFailure", TS: ms(time.Hour) + 1, SessionID: p("s-built"), ErrorType: p("rate_limit"), Detail: p(callmeter.RecoveredDetail)})
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		_, err := tx.InsertTurn(ctx, callmeter.Turn{EventID: "s-done-stop", Event: "Stop", SessionID: p("s-done"), TS: ms(3*time.Hour) + 1})
		return err
	})
	table, err := Faults(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var refusals []string
	for _, row := range table.Rows {
		if row[0] == "refusal" {
			refusals = append(refusals, strings.Join([]string{row[1], row[2], row[4], row[6]}, "|"))
		}
	}
	want := []string{
		"rate_limit|1|chat-s-built|StopFailure rebuilt from the transcript",
		"unknown|1|chat-s-odd|transcript, no StopFailure",
		"rate_limit|1|chat-s-lost|transcript, no StopFailure",
		"oauth_org_not_allowed|1|chat-s-hook|StopFailure",
	}
	if strings.Join(refusals, "\n") != strings.Join(want, "\n") {
		t.Errorf("refusal rows =\n%s\nwant\n%s\n(rows %v)", strings.Join(refusals, "\n"), strings.Join(want, "\n"), table.Rows)
	}
	notes := strings.Join(table.Notes, "\n")
	if !strings.Contains(notes, "1 sessions ending on an unanswered prompt could not be checked for an API error") ||
		!strings.Contains(notes, "absent.jsonl") {
		t.Errorf("notes = %q, want the unreadable transcript of s-gone named", notes)
	}
	if out := render(t, table); strings.Contains(out, refusalMessage) || strings.Contains(out, "invented") {
		t.Errorf("the report carries transcript text:\n%s", out)
	}
}

func TestFaultsReadsAMovedTranscript(t *testing.T) {
	for _, viaSeat := range []bool{false, true} {
		name := "stored root"
		if viaSeat {
			name = "seat root"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			seat := t.TempDir()
			projects := filepath.Join(seat, "projects")
			path := filepath.Join(projects, "-tmp-moved-proj", "s-moved.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Join([]string{refusalPrompt, refusalLine("rate_limit"), refusalTail}, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			dead := filepath.Join(projects, "-tmp-old-proj", "s-moved.jsonl")
			if viaSeat {
				dead = filepath.Join(t.TempDir(), "gone", "s-moved.jsonl")
			}
			batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
				return tx.TouchSession(ctx, callmeter.Session{SessionID: "s-moved", TS: ms(time.Hour), TranscriptPath: callmeter.Ptr(dead), SeatDir: callmeter.Ptr(seat)})
			})
			seedEvent(t, store, callmeter.Event{EventID: "moved-up", Event: "UserPromptSubmit", TS: ms(time.Hour), SessionID: callmeter.Ptr("s-moved")})
			table, err := Faults(ctx, store, Filter{}, chatOf)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range table.Rows {
				if row[0] == "refusal" && row[1] == "rate_limit" && row[6] == refusalFromTranscript {
					found = true
				}
			}
			if !found {
				t.Error("faults read the transcript error instead of the API error kind")
			}
			for _, note := range table.Notes {
				if strings.Contains(note, "could not be checked for an API error") {
					t.Error("moved transcript remained unchecked")
				}
			}
		})
	}
}

func TestFaultsLateStopLeavesTheNextPromptUnanswered(t *testing.T) {
	store := openStore(t)
	path := filepath.Join(t.TempDir(), "s-late.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{refusalPrompt, refusalLine("rate_limit"), refusalTail}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		return tx.TouchSession(ctx, callmeter.Session{SessionID: "s-late", TS: 200, TranscriptPath: callmeter.Ptr(path)})
	})
	seedEvent(t, store, callmeter.Event{EventID: "u1", Event: "UserPromptSubmit", TS: 100, SessionID: callmeter.Ptr("s-late"), PromptID: callmeter.Ptr("p-u1")})
	seedEvent(t, store, callmeter.Event{EventID: "u2", Event: "UserPromptSubmit", TS: 200, SessionID: callmeter.Ptr("s-late"), PromptID: callmeter.Ptr("p-u2")})
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		_, err := tx.InsertTurn(ctx, callmeter.Turn{EventID: "late-stop", Event: "Stop", TS: 210, SessionID: callmeter.Ptr("s-late"), PromptID: callmeter.Ptr("p-u1")})
		return err
	})
	table, err := Faults(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range table.Rows {
		if row[0] == "refusal" && row[1] == "rate_limit" && row[6] == refusalFromTranscript {
			return
		}
	}
	t.Error("an earlier prompt's late Stop suppressed the next prompt's transcript refusal")
}
