package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
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
	if !strings.Contains(out, "note: chat names could not be read for 1 sessions (first: unreadable)") ||
		strings.Contains(out, "transcript unreadable") {
		t.Errorf("report lacks the chat-name note:\n%s", out)
	}
	if !strings.Contains(out, "note: 1 calls not recorded") {
		t.Errorf("report lacks the unrecorded-calls note:\n%s", out)
	}
}

func TestNamesNoteCountsSessionsWithASafeLabel(t *testing.T) {
	cases := []struct {
		name     string
		fn       NameOf
		sessions []string
		want     string
	}{
		{
			name: "distinct failing sessions and first failure",
			fn: func(session string) (string, error) {
				if session == "b" {
					return "", fmt.Errorf("lookup: %w", fs.ErrPermission)
				}
				return "", errNames
			},
			sessions: []string{"a", "b", "a"},
			want:     "chat names could not be read for 2 sessions (first: unreadable)",
		},
		{
			name:     "permission denied",
			fn:       func(string) (string, error) { return "", fmt.Errorf("lookup: %w", fs.ErrPermission) },
			sessions: []string{"a"},
			want:     "chat names could not be read for 1 sessions (first: permission denied)",
		},
		{
			name:     "not found",
			fn:       func(string) (string, error) { return "", fmt.Errorf("lookup: %w", fs.ErrNotExist) },
			sessions: []string{"a"},
			want:     "chat names could not be read for 1 sessions (first: not found)",
		},
		{
			name:     "other error",
			fn:       func(string) (string, error) { return "", errNames },
			sessions: []string{"a"},
			want:     "chat names could not be read for 1 sessions (first: unreadable)",
		},
		{
			name:     "no name source",
			sessions: []string{"a", "b", "a"},
			want:     "chat names could not be read for 2 sessions (first: unreadable)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names := newNames(tc.fn)
			for _, session := range tc.sessions {
				names.of(session)
			}
			if got := names.notes(); len(got) != 1 || got[0] != tc.want {
				t.Errorf("notes = %q, want [%q]", got, tc.want)
			}
		})
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

// TestTerminatedFaultsBecomeTheUnrecordedEventsNoteOnEveryTopic: hook runs a
// signal cut short before they recorded are a note counting them, in the
// window, after the binary note; a binary fault is never counted in it.
func TestTerminatedFaultsBecomeTheUnrecordedEventsNoteOnEveryTopic(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	faults := []callmeter.Fault{
		{TS: ms(time.Hour), Stage: callmeter.StageTerminated, Error: "SubagentStop: terminated by SIGTERM"},
		{TS: ms(2 * time.Hour), Stage: callmeter.StageTerminated, Error: "unknown: terminated by SIGINT"},
		{TS: ms(3 * 24 * time.Hour), Stage: callmeter.StageTerminated, Error: "Stop: terminated by SIGHUP"},
		{TS: ms(time.Hour), Stage: callmeter.StageBinary, Error: "Stop: download failed"},
	}
	for _, fault := range faults {
		if err := store.AddFault(ctx, fault); err != nil {
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
		binary := slices.Index(table.Notes, "1 events unrecorded: binary unavailable")
		terminated := slices.Index(table.Notes, "2 events unrecorded: hook terminated before recording")
		if binary < 0 || terminated != binary+1 {
			t.Errorf("%s notes = %q, want the binary note then the 2 terminated faults of the last day", name, table.Notes)
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

// everyTopicNotes runs the five lifecycle topics over store and returns each
// one's notes by topic name.
func everyTopicNotes(t *testing.T, store *callmeter.Store) map[string][]string {
	t.Helper()
	notes := map[string][]string{}
	for name, topic := range map[string]func(context.Context, *callmeter.Store, Filter, NameOf) (*Table, error){
		"files": Files, "faults": Faults, "sessions": Sessions, "events": Events, "coverage": Coverage,
	} {
		table, err := topic(context.Background(), store, Filter{OwnSeat: t.TempDir()}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		notes[name] = table.Notes
	}
	return notes
}

// TestRecoveryMarkersAreTheirOwnNoteNeverLostCalls: the transcript faults
// recovery writes for a turn or agent turn it read and could not settle carry
// no tool_use_id and name no call; they are counted in their own note, never as
// calls not recorded. The texts are the markers' own, as the live store holds
// them.
func TestRecoveryMarkersAreTheirOwnNoteNeverLostCalls(t *testing.T) {
	markers := map[string]string{
		"turn end":   "prompt p6 UserPromptSubmit e6: " + callmeter.UnfilledTurnEnd,
		"agent stop": "agent a1 turn 1 open: " + callmeter.UnfilledAgentStop,
		"agent turn": "agent a1 turn stopped 7: " + callmeter.UnfilledAgentTurn,
		"stop reply": "prompt p6 Stop e7: " + callmeter.UnfilledStopReply,
	}
	for _, tc := range []struct {
		name  string
		kinds []string
		want  string
	}{
		{"only an unended prompt", []string{"turn end"},
			"1 turns or agent turns recovery could not settle from transcripts; see the faults topic"},
		{"every marker kind", []string{"turn end", "agent stop", "agent turn", "stop reply"},
			"4 turns or agent turns recovery could not settle from transcripts; see the faults topic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t)
			for _, kind := range tc.kinds {
				if err := store.AddFault(ctx, callmeter.Fault{
					TS: ms(time.Hour), SessionID: "s6", Stage: callmeter.StageTranscript, Error: markers[kind],
				}); err != nil {
					t.Fatalf("AddFault: %v", err)
				}
			}
			for name, notes := range everyTopicNotes(t, store) {
				for _, note := range notes {
					if strings.Contains(note, "calls not recorded") || strings.Contains(note, "naming no call") {
						t.Errorf("%s notes = %q: a recovery marker counted as a lost call or another fault", name, notes)
					}
				}
				if !slices.Contains(notes, tc.want) {
					t.Errorf("%s notes = %q, want %q", name, notes, tc.want)
				}
			}
		})
	}
}

// TestLostCallsAreToolEventFaultsAloneOtherIdlessFaultsAreNamedApart: a fault
// with no tool_use_id names a lost call only when it is a payload or store
// fault of a tool event; every other one (a PostToolBatch with no tool_calls,
// an ingest or SessionEnd budget fault, an unreadable payload) is named apart,
// and a terminated hook is counted among the events unrecorded alone.
func TestLostCallsAreToolEventFaultsAloneOtherIdlessFaultsAreNamedApart(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	for _, fault := range []callmeter.Fault{
		// Lost calls: the texts hookentry writes for a tool event's payload.
		{Stage: callmeter.StagePayload, Error: "PostToolUse payload lacks tool_use_id or tool_name"},
		{Stage: callmeter.StagePayload, Error: `"PreToolUse" payload carries no session_id`},
		{Stage: callmeter.StagePayload, Error: "PostToolBatch call carries no tool_use_id"},
		// Faults naming no call.
		{Stage: callmeter.StagePayload, Error: callmeter.BatchWithoutCalls},
		{Stage: callmeter.StageStore, Error: "ingest missed.log: database is locked"},
		{Stage: callmeter.StageTranscript, Error: "SessionEnd spent its 1m0s budget: 2 transcript reads skipped, " +
			"their requests and calls left as their hooks wrote them"},
		{Stage: callmeter.StagePayload, Error: "decode hook payload (12 bytes): unexpected end of JSON input"},
		// Hooks terminated before recording, from missed.log.
		{Stage: callmeter.StageTerminated, Error: "PostToolUse: store unavailable: full"},
		{Stage: callmeter.StageTerminated, Error: "PostToolUse: panic"},
	} {
		fault.TS, fault.SessionID = ms(time.Hour), "s1"
		if err := store.AddFault(ctx, fault); err != nil {
			t.Fatalf("AddFault: %v", err)
		}
	}
	want := []string{
		"3 calls not recorded (payload, store or transcript faults; see the faults topic)",
		"4 other payload, store or transcript faults, naming no call (see the faults topic)",
		"2 events unrecorded: hook terminated before recording",
	}
	for name, notes := range everyTopicNotes(t, store) {
		at := -1
		for _, note := range want {
			i := slices.Index(notes, note)
			if i <= at {
				t.Errorf("%s notes = %q, want %q in order", name, notes, want)
				break
			}
			at = i
		}
	}
}

// TestAbsentSizesAreClassifiedNeverCountedAsZero: a call with no delivered
// size is unknown, not 0 bytes. The note says why each has none, by what the
// store holds: a start alone (interrupted, killed or still running), a result
// whose batch never landed, an internal agent's call; a denied or rejected
// call is named as that whether or not its size is known.
func TestAbsentSizesAreClassifiedNeverCountedAsZero(t *testing.T) {
	store := openStore(t)
	call := func(id, agent, agentType string, failed *bool, outcome string, delivered *int64) callmeter.Call {
		c := callmeter.Call{
			ToolUseID: id, SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)),
			Tool: callmeter.Ptr("Bash"), Failed: failed, BytesDelivered: delivered,
		}
		if agent != "" {
			c.AgentID = callmeter.Ptr(agent)
		}
		if agentType != "" {
			c.AgentType = callmeter.Ptr(agentType)
		}
		if outcome != "" {
			c.Error = callmeter.Ptr(outcome)
		}
		return c
	}
	seed(t, store, call("t_started", "a1", "gitter", nil, "", nil))
	seed(t, store, call("t_unbatched", "", "", callmeter.Ptr(false), "", nil))
	seed(t, store, call("t_internal", "a2", "", nil, "", nil))
	seed(t, store, call("t_hookdeny", "", "", callmeter.Ptr(true), callmeter.OutcomeDeniedByHook, callmeter.Ptr(int64(38))))
	seed(t, store, call("t_rejected", "a1", "gitter", callmeter.Ptr(true), callmeter.OutcomeRejectedByUser,
		callmeter.Ptr(int64(80))))
	seed(t, store, call("t_refused", "", "", callmeter.Ptr(true), callmeter.OutcomeRefused, nil))
	table, err := Files(context.Background(), store, Filter{}, func(string) (string, error) { return "chat", nil })
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	out := render(t, table)
	for _, want := range []string{
		"1 calls have no delivered size (only PreToolUse recorded: interrupted, killed or still running): size unknown, not counted",
		"1 calls have no delivered size (ran, no PostToolBatch recorded: its turn was abandoned or is still running): size unknown, not counted",
		"1 calls have no delivered size (a Claude Code internal agent's, no transcript): size unknown, not counted",
		"1 calls " + callmeter.OutcomeDeniedByHook,
		"1 calls " + callmeter.OutcomeRejectedByUser,
		"1 calls " + callmeter.OutcomeRefused + ": size unknown, not counted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "counted as 0 bytes") {
		t.Errorf("report counts an absent size as 0 bytes:\n%s", out)
	}
}

// TestBatchOnlyCallsAreNamedByTool: a tool a function-hooks plugin answers in
// its own tool.call hook fires no PostToolUse, so its row holds only the
// PostToolBatch size: it is named by its tool, never left a silent row with
// its real size, duration and outcome missing.
func TestBatchOnlyCallsAreNamedByTool(t *testing.T) {
	store := openStore(t)
	seed(t, store, callmeter.Call{
		ToolUseID: "t_served", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)),
		Tool: callmeter.Ptr("mcp__demo-plugin__probe"), BytesDelivered: callmeter.Ptr(int64(95)),
	})
	seed(t, store, callmeter.Call{
		ToolUseID: "t_ran", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr("Bash"),
		Failed: callmeter.Ptr(false), BytesReal: callmeter.Ptr(int64(5)), BytesDelivered: callmeter.Ptr(int64(5)),
	})
	// A row from before the batch set a ts: named with its class in any
	// window, never as a call stored before every hook set a ts.
	seed(t, store, callmeter.Call{
		ToolUseID: "t_served_legacy", SessionID: callmeter.Ptr("s1"),
		Tool: callmeter.Ptr("mcp__demo-plugin__probe"), BytesDelivered: callmeter.Ptr(int64(95)),
	})
	for _, f := range []Filter{{}, {Since: time.UnixMilli(ms(2 * time.Hour))}} {
		table, err := Files(context.Background(), store, f, func(string) (string, error) { return "chat", nil })
		if err != nil {
			t.Fatalf("Files: %v", err)
		}
		out := render(t, table)
		want := "note: 2 calls of mcp__demo-plugin__probe have only their PostToolBatch size"
		if !strings.Contains(out, want) || strings.Contains(out, "calls of Bash") || strings.Contains(out, "have no ts") {
			t.Errorf("since %v: report does not name the batch-only calls by their tool alone (want %q):\n%s", f.Since, want, out)
		}
	}
}

// TestPendingNotesNameTheirReason: a request still pending was one count with
// no reason, so a request stuck forever read the same as one a live chat is
// about to resolve. Each is now named by why its size is unread: its
// transcript faulted, its session ended first, or its session has not ended.
func TestPendingNotesNameTheirReason(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	pending := func(id, session string) {
		seedRequest(t, store, callmeter.Request{
			RequestID: callmeter.ProvisionalKey(id), SessionID: callmeter.Ptr(session), TS: callmeter.Ptr(ms(2 * time.Hour)),
			Pending: callmeter.Ptr(true),
		})
		seed(t, store, callmeter.Call{
			ToolUseID: id, SessionID: callmeter.Ptr(session), TS: callmeter.Ptr(ms(2 * time.Hour)),
			Tool: callmeter.Ptr("Bash"), RequestID: callmeter.Ptr(callmeter.ProvisionalKey(id)),
		})
	}
	pending("t_live", "s_live")
	pending("t_ended", "s_ended")
	pending("t_ended2", "s_ended")
	pending("t_unread", "s_unread")
	seedEvent(t, store, callmeter.Event{EventID: "e1", Event: callmeter.EventSessionEnd, TS: ms(time.Hour), SessionID: callmeter.Ptr("s_ended")})
	if err := store.AddFault(ctx, callmeter.Fault{
		TS: ms(2 * time.Hour), SessionID: "s_unread", ToolUseID: "t_unread", Stage: callmeter.StageTranscript,
		Error: "read transcript: no such file",
	}); err != nil {
		t.Fatalf("AddFault: %v", err)
	}
	table, err := Files(ctx, store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	out := render(t, table)
	for _, want := range []string{
		"1 requests still pending (its transcript could not be read): context size unknown, not counted",
		"2 requests still pending (its session ended before the transcript held it): context size unknown, not counted",
		"1 requests still pending (its session has not ended: still live, or killed before its Stop): context size unknown, not counted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// TestCallsWithoutTSAreNamedInANote: a call row with no ts (written before
// every hook set one) falls outside any --since window, so a note names how
// many there are, windowed or not, never a silent drop.
func TestCallsWithoutTSAreNamedInANote(t *testing.T) {
	store := openStore(t)
	seed(t, store, callmeter.Call{ToolUseID: "t_nots", SessionID: callmeter.Ptr("s1"), Tool: callmeter.Ptr("Bash")})
	seed(t, store, callmeter.Call{
		ToolUseID: "t_ts", SessionID: callmeter.Ptr("s1"), TS: callmeter.Ptr(ms(time.Hour)), Tool: callmeter.Ptr("Bash"),
	})
	for _, f := range []Filter{{}, {Since: time.UnixMilli(ms(2 * time.Hour))}} {
		table, err := Faults(context.Background(), store, f, func(string) (string, error) { return "chat", nil })
		if err != nil {
			t.Fatalf("Faults: %v", err)
		}
		if out := render(t, table); !strings.Contains(out, "note: 1 calls have no ts") {
			t.Errorf("since %v: report does not name the call without a ts:\n%s", f.Since, out)
		}
	}
}

func TestEmptyTopicsNameWhatTheyFoundNone(t *testing.T) {
	cases := []struct {
		name  string
		topic func(context.Context, *callmeter.Store, Filter, NameOf) (*Table, error)
		want  string
	}{
		{"context", Context, "callmeter: no sized requests in window"},
		{"tokens", Tokens, "callmeter: no requests in window"},
		{"sessions", Sessions, "callmeter: no sessions in window"},
		{"prompts", Prompts, "callmeter: no prompts in window"},
		{"faults", Faults, "callmeter: no faults in window"},
		{"agents", Agents, "callmeter: no sub-agents in window"},
		{"events", Events, "callmeter: no events in window"},
		{"effort", Effort, "callmeter: no effort recorded in window"},
		{"files", Files, EmptyLine},
		{"writes", Writes, EmptyLine},
		{"commands", Commands, EmptyLine},
		{"sequences", Sequences, EmptyLine},
		{"outcomes", Outcomes, EmptyLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t)
			// The window contains a call; the topic's filter finds no rows.
			seed(t, store, mkCall("c", "recorded", time.Hour, "Read"))
			table, err := tc.topic(context.Background(), store, Filter{Session: "empty"}, chatOf)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(render(t, table)), "\n")
			if len(table.Rows) != 0 || len(lines) < 2 || lines[1] != tc.want {
				t.Errorf("empty body = %q, want %q", lines, tc.want)
			}
		})
	}
}
