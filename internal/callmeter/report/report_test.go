package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func openStore(t *testing.T) *callmeter.Store {
	t.Helper()
	store, err := callmeter.OpenDB(context.Background(), filepath.Join(t.TempDir(), "callmeter.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// seed writes one call through the store API.
func seed(t *testing.T, store *callmeter.Store, c callmeter.Call) {
	t.Helper()
	if err := store.UpsertCall(context.Background(), c, callmeter.Overwrite); err != nil {
		t.Fatalf("UpsertCall %s: %v", c.ToolUseID, err)
	}
}

// bash is a Bash call of session s, agent a, ts ms, with command in cwd.
func bash(id, s, a string, ts int64, cwd, command string, delivered int64) callmeter.Call {
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		panic(err)
	}
	return callmeter.Call{
		ToolUseID: id, SessionID: &s, AgentID: &a, TS: &ts, Tool: callmeter.Ptr("Bash"),
		Input: callmeter.Ptr(string(input)), Cwd: &cwd, BytesDelivered: &delivered,
	}
}

func ms(d time.Duration) int64 { return testNow.Add(-d).UnixMilli() }

func render(t *testing.T, table *Table) string {
	t.Helper()
	var out bytes.Buffer
	if err := table.Render(&out); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out.String()
}

func TestParseSince(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"", testNow.Add(-Retention), false},
		{"7d", testNow.Add(-7 * 24 * time.Hour), false},
		{"24h", testNow.Add(-24 * time.Hour), false},
		{"2026-09-01", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), false},
		{"7w", time.Time{}, true},
		{"yesterday", time.Time{}, true},
	}
	for _, c := range cases {
		got, err := ParseSince(c.in, testNow)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseSince(%q) error = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && !got.Equal(c.want) {
			t.Errorf("ParseSince(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestPruneExpiredPrunesOldRowsBeforeReport(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for id, age := range map[string]time.Duration{"old": 40 * 24 * time.Hour, "new": time.Hour} {
		seed(t, store, callmeter.Call{
			ToolUseID: id, SessionID: callmeter.Ptr("s"), TS: callmeter.Ptr(ms(age)), Tool: callmeter.Ptr("Read"),
			FilePath: callmeter.Ptr("/src/" + id + ".go"), BytesDelivered: callmeter.Ptr(int64(10)),
		})
	}
	removed, err := PruneExpired(ctx, store, testNow)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if removed != 1 {
		t.Errorf("PruneExpired removed %d rows, want 1", removed)
	}
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(table.Rows) != 1 || table.Rows[0][0] != "/src/new.go" {
		t.Errorf("rows after prune = %v, want only /src/new.go", table.Rows)
	}
}

func TestEmptyWindowPrintsOneLine(t *testing.T) {
	table, err := Files(context.Background(), openStore(t), Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(render(t, table)), "\n")
	if len(lines) != 2 || lines[1] != EmptyLine {
		t.Errorf("empty report = %q, want heading plus %q", lines, EmptyLine)
	}
}

func TestProjectFilterKeepsCwdAndBelow(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for id, cwd := range map[string]string{"in": "/w/p", "below": "/w/p/sub", "sibling": "/w/px"} {
		seed(t, store, callmeter.Call{
			ToolUseID: id, TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr("Read"), Cwd: callmeter.Ptr(cwd),
			FilePath: callmeter.Ptr("/f/" + id), BytesDelivered: callmeter.Ptr(int64(1)),
		})
	}
	table, err := Files(ctx, store, Filter{Project: "/w/p"}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Errorf("project rows = %v, want /f/in and /f/below", table.Rows)
	}
}

func TestNameOfErrorRendersQuestionMarkAndNote(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(time.Hour), SessionID: "s1", ToolUseID: "t1", Stage: callmeter.StageStore, Error: "disk full",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	failing := func(string) (string, error) { return "", errNames }
	table, err := Faults(ctx, store, Filter{}, failing)
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	var faultRow []string
	for _, row := range table.Rows {
		if row[0] == "fault" {
			faultRow = row
		}
	}
	if len(faultRow) < 5 || faultRow[4] != "?" {
		t.Errorf("fault row = %v, want chat column ?", faultRow)
	}
	out := render(t, table)
	if !strings.Contains(out, "note: chat names could not be read: session s1: transcript unreadable") {
		t.Errorf("report lacks the chat-name note:\n%s", out)
	}
	if !strings.Contains(out, "note: 1 calls not recorded") {
		t.Errorf("report lacks the unrecorded-calls note:\n%s", out)
	}
}

type namesError string

func (e namesError) Error() string { return string(e) }

const errNames = namesError("transcript unreadable")

// batch runs fn in one store transaction.
func batch(t *testing.T, store *callmeter.Store, fn func(ctx context.Context, tx *callmeter.Tx) error) {
	t.Helper()
	ctx := context.Background()
	if err := store.Batch(ctx, func(tx *callmeter.Tx) error { return fn(ctx, tx) }); err != nil {
		t.Fatalf("Batch: %v", err)
	}
}

// seedEvent stores one events row and rebuilds the sub-agent turns of its agent.
func seedEvent(t *testing.T, store *callmeter.Store, e callmeter.Event) {
	t.Helper()
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		if _, err := tx.InsertEvent(ctx, e); err != nil {
			return err
		}
		if e.AgentID != nil && *e.AgentID != "" {
			return tx.RebuildAgentTurns(ctx, *e.AgentID)
		}
		return nil
	})
}

// seedTurn stores one turns row.
func seedTurn(t *testing.T, store *callmeter.Store, turn callmeter.Turn) {
	t.Helper()
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		_, err := tx.InsertTurn(ctx, turn)
		return err
	})
}

// seedSession stores a sessions row first and last seen at ts.
func seedSession(t *testing.T, store *callmeter.Store, s callmeter.Session) {
	t.Helper()
	batch(t, store, func(ctx context.Context, tx *callmeter.Tx) error {
		if err := tx.TouchSession(ctx, s); err != nil {
			return err
		}
		return tx.RefreshSession(ctx, s.SessionID)
	})
}

func seedRequest(t *testing.T, store *callmeter.Store, r callmeter.Request) {
	t.Helper()
	if err := store.UpsertRequest(context.Background(), r, callmeter.Overwrite); err != nil {
		t.Fatalf("UpsertRequest %s: %v", r.RequestID, err)
	}
}

func seedAgent(t *testing.T, store *callmeter.Store, a callmeter.Agent) {
	t.Helper()
	if err := store.UpsertAgent(context.Background(), a, callmeter.Overwrite); err != nil {
		t.Fatalf("UpsertAgent %s: %v", a.AgentID, err)
	}
}

// chatOf names every chat "chat-{session}".
func chatOf(session string) (string, error) { return "chat-" + session, nil }

// cells is a row's cells joined by "|", for one-line comparisons.
func cells(row []string) string { return strings.Join(row, "|") }

// TestRenderJSONIsTheTableWithItsHeaderAsColumns: a ported topic's table
// decodes back to the object 0-surface names, `columns` equal to the text
// header and the notes without their prefix.
func TestRenderJSONIsTheTableWithItsHeaderAsColumns(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	seed(t, store, callmeter.Call{
		ToolUseID: "r1", SessionID: callmeter.Ptr("s"), TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr("Read"),
		FilePath: callmeter.Ptr("/src/a.go"), BytesDelivered: callmeter.Ptr(int64(10)),
	})
	table, err := Files(ctx, store, Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	table.Notes = append(table.Notes, "a gap")
	var out bytes.Buffer
	if err := table.RenderJSON(&out, "files"); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var got struct {
		Topic   string     `json:"topic"`
		Store   string     `json:"store"`
		Title   string     `json:"title"`
		Columns []string   `json:"columns"`
		Rows    [][]string `json:"rows"`
		Notes   []string   `json:"notes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if got.Topic != "files" || got.Store != "present" || got.Title != table.Title {
		t.Errorf("topic, store, title = %q, %q, %q; want files, present, %q", got.Topic, got.Store, got.Title, table.Title)
	}
	if cells(got.Columns) != cells(table.Header) || len(got.Rows) != 1 || cells(got.Rows[0]) != cells(table.Rows[0]) {
		t.Errorf("columns %v rows %v; want the header %v and rows %v", got.Columns, got.Rows, table.Header, table.Rows)
	}
	if len(got.Notes) == 0 || got.Notes[len(got.Notes)-1] != "a gap" {
		t.Errorf("notes = %v, want them without the note: prefix, ending in the added one", got.Notes)
	}
}

// TestRenderJSONEmptyWindowIsAnEmptyRowsArray: no rows is `[]`, never null,
// and the columns stay.
func TestRenderJSONEmptyWindowIsAnEmptyRowsArray(t *testing.T) {
	table, err := Files(context.Background(), openStore(t), Filter{}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var out bytes.Buffer
	if err := table.RenderJSON(&out, "files"); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(out.String(), `"rows":[]`) || !strings.Contains(out.String(), `"notes":[]`) ||
		strings.Contains(out.String(), "null") {
		t.Errorf("empty window JSON = %s, want rows and notes as [] and no null", out.String())
	}
	var got struct {
		Columns []string `json:"columns"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || cells(got.Columns) != cells(table.Header) {
		t.Errorf("columns = %v (%v), want the header %v", got.Columns, err, table.Header)
	}
}

// TestBinaryFaultsBecomeTheMissedEventsNoteOnEveryTopic: events the wrapper
// could not deliver are a note counting them, in the window, on a ported topic
// and on a new one.
func TestBinaryFaultsBecomeTheMissedEventsNoteOnEveryTopic(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for i, age := range []time.Duration{time.Hour, 2 * time.Hour, 3 * 24 * time.Hour} {
		if err := store.AddFault(ctx, callmeter.Fault{
			TS: ms(age), Stage: callmeter.StageBinary, Error: fmt.Sprintf("Stop: attempt %d", i),
		}); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	for name, topic := range map[string]func(context.Context, *callmeter.Store, Filter, NameOf) (*Table, error){
		"files": Files, "faults": Faults, "sessions": Sessions, "events": Events, "coverage": Coverage,
	} {
		table, err := topic(ctx, store, Filter{Since: testNow.Add(-24 * time.Hour), OwnSeat: t.TempDir()}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Contains(table.Notes, "2 events unrecorded: binary unavailable") {
			t.Errorf("%s notes = %q, want the 2 binary faults of the last day counted", name, table.Notes)
		}
	}
}

func TestInapplicableNotesNameEachFlagATopicCannotApply(t *testing.T) {
	both := Filter{Project: "/w/p", AgentType: "Explore", Session: "s", Limit: 3}
	for topic, want := range map[string]string{
		"sessions": "--agent-type does not apply to sessions",
		"coverage": "--project does not apply to coverage;--agent-type does not apply to coverage",
		"faults":   "--project does not apply to faults;--agent-type does not apply to faults",
		"tokens":   "",
		"files":    "",
	} {
		if got := strings.Join(InapplicableNotes(topic, both), ";"); got != want {
			t.Errorf("InapplicableNotes(%s) = %q, want %q", topic, got, want)
		}
		if got := InapplicableNotes(topic, Filter{Session: "s", Limit: 3}); len(got) != 0 {
			t.Errorf("InapplicableNotes(%s) without those flags = %v, want none", topic, got)
		}
	}
}

// TestProjectFilterCountsCharactersNotBytes: a project path with a non-ASCII
// name still matches the calls under it (substr counts characters).
func TestProjectFilterCountsCharactersNotBytes(t *testing.T) {
	store := openStore(t)
	for id, cwd := range map[string]string{"in": "/w/café", "below": "/w/café/sub", "sibling": "/w/cafés"} {
		seed(t, store, callmeter.Call{
			ToolUseID: id, TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr("Read"), Cwd: callmeter.Ptr(cwd),
			FilePath: callmeter.Ptr("/f/" + id), BytesDelivered: callmeter.Ptr(int64(1)),
		})
	}
	table, err := Files(context.Background(), store, Filter{Project: "/w/café"}, nil)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Errorf("project rows = %v, want /f/in and /f/below only", table.Rows)
	}
}

// mkCall is a call of session at age before testNow; the caller sets the rest.
func mkCall(id, session string, age time.Duration, tool string) callmeter.Call {
	return callmeter.Call{ToolUseID: id, SessionID: &session, TS: callmeter.Ptr(ms(age)), Tool: &tool}
}

// rowsOf is a table's rows as "a|b|c" lines.
func rowsOf(table *Table) []string {
	lines := make([]string, 0, len(table.Rows))
	for _, row := range table.Rows {
		lines = append(lines, cells(row))
	}
	return lines
}

// wantRows fails the test unless table's rows are exactly want, in order.
func wantRows(t *testing.T, table *Table, want ...string) {
	t.Helper()
	got := rowsOf(table)
	if len(got) != len(want) {
		t.Fatalf("%s rows = %q, want %q", table.Title, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s row %d = %q, want %q", table.Title, i, got[i], want[i])
		}
	}
}

// wantHeader fails the test unless table's header is exactly want.
func wantHeader(t *testing.T, table *Table, want ...string) {
	t.Helper()
	if cells(table.Header) != cells(want) {
		t.Errorf("%s header = %q, want %q", table.Title, table.Header, want)
	}
}

// TestUnrecordedCallsNoteCountsOnlyCallsMissingFromTheStore: a fault whose
// call row was still written (a stat failure, a refused input) is not a lost
// call, and a call that failed at two hook events is one lost call; the note
// counts calls absent from the store, never fault rows.
func TestUnrecordedCallsNoteCountsOnlyCallsMissingFromTheStore(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	if err := store.UpsertCall(ctx, callmeter.Call{
		ToolUseID: "t_written", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)),
		Tool: callmeter.Ptr("Read"),
	}, callmeter.Overwrite); err != nil {
		t.Fatalf("UpsertCall: %v", err)
	}
	for _, fault := range []callmeter.Fault{
		{TS: ms(time.Hour), SessionID: "s1", ToolUseID: "t_written", Stage: callmeter.StagePayload, Error: "stat failed"},
		{TS: ms(time.Hour), SessionID: "s1", ToolUseID: "t_lost", Stage: callmeter.StageStore, Error: "disk full"},
		{TS: ms(time.Hour), SessionID: "s1", ToolUseID: "t_lost", Stage: callmeter.StageStore, Error: "disk full"},
	} {
		if err := store.AddFault(ctx, fault); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	table, err := Faults(ctx, store, Filter{}, func(string) (string, error) { return "chat", nil })
	if err != nil {
		t.Fatalf("Faults: %v", err)
	}
	if out := render(t, table); !strings.Contains(out, "note: 1 calls not recorded") {
		t.Errorf("report does not count one lost call:\n%s", out)
	}
}
