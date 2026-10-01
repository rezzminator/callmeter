package command

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// writeTranscript writes {seat}/projects/{project}/{session}.jsonl with lines
// and returns its path.
func writeTranscript(t *testing.T, seat, project, session string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(seat, "projects", project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, session+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// namesLab is a transcriptNames over an empty store and a scratch user home.
type namesLab struct {
	names *transcriptNames
	seat  string // the report's own seat, $HOME/.claude
	db    *sql.DB
}

func newNamesLab(t *testing.T) namesLab {
	t.Helper()
	root := t.TempDir()
	store, err := callmeter.OpenDB(context.Background(), filepath.Join(root, "state", "callmeter.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	userHome := filepath.Join(root, "user")
	return namesLab{
		names: &transcriptNames{
			ctx: context.Background(), db: store.DB(), getenv: mapEnv(map[string]string{"HOME": userHome}),
		},
		seat: filepath.Join(userHome, ".claude"),
		db:   store.DB(),
	}
}

func TestTranscriptNamesTitledChatNamesTheLastCustomTitle(t *testing.T) {
	fixture := newNamesLab(t)
	writeTranscript(t, fixture.seat, "-work", "0a1b2c3d-sess",
		`{"type":"user","message":{"content":"hello"}}`,
		`{"type":"custom-title","customTitle":"first name","sessionId":"0a1b2c3d-sess"}`,
		`{"type":"ai-title","aiTitle":"generated name","sessionId":"0a1b2c3d-sess"}`,
		`this line is not JSON and holds no title marker`,
		`{"type":"custom-title","customTitle":"final name","sessionId":"0a1b2c3d-sess"}`,
		`{"type":"ai-title","aiTitle":"later generated name","sessionId":"0a1b2c3d-sess"}`)
	name, err := fixture.names.nameOf("0a1b2c3d-sess")
	if name != "final name" || err != nil {
		t.Fatalf("nameOf = %q, %v; want the last customTitle and no error", name, err)
	}
}

func TestTranscriptNamesAIOnlyChatNamesTheLastAITitle(t *testing.T) {
	fixture := newNamesLab(t)
	writeTranscript(t, fixture.seat, "-work", "sess-ai",
		`{"type":"ai-title","aiTitle":"first generated","sessionId":"sess-ai"}`,
		`{"type":"ai-title","aiTitle":"last generated","sessionId":"sess-ai"}`)
	name, err := fixture.names.nameOf("sess-ai")
	if name != "last generated" || err != nil {
		t.Fatalf("nameOf = %q, %v; want the last aiTitle and no error", name, err)
	}
}

func TestTranscriptNamesSummaryNamesAChatWithNoOtherTitle(t *testing.T) {
	fixture := newNamesLab(t)
	writeTranscript(t, fixture.seat, "-work", "sess-sum",
		`{"type":"summary","summary":"older summary","leafUuid":"a"}`,
		`{"type":"summary","summary":"newer summary","leafUuid":"b"}`)
	name, err := fixture.names.nameOf("sess-sum")
	if name != "newer summary" || err != nil {
		t.Fatalf("nameOf = %q, %v; want the last summary and no error", name, err)
	}
}

func TestTranscriptNamesUntitledOrMissingTranscriptNamesTheSessionIDStart(t *testing.T) {
	fixture := newNamesLab(t)
	writeTranscript(t, fixture.seat, "-work", "0123456789-untitled",
		`{"type":"user","message":{"content":"hello"}}`)
	for session, want := range map[string]string{
		"0123456789-untitled": "01234567", // a transcript with no title entry
		"fedcba9876-missing":  "fedcba98", // no transcript anywhere
		"short":               "short",    // under 8 characters
	} {
		name, err := fixture.names.nameOf(session)
		if name != want || err != nil {
			t.Errorf("nameOf(%q) = %q, %v; want %q and no error", session, name, err, want)
		}
	}
}

// TestTranscriptNamesLooksUnderTheSeatsTheStoreRecorded: a session run from a
// CLAUDE_CONFIG_DIR other than the report's own is found under that seat; the
// report's own seat is searched too.
func TestTranscriptNamesLooksUnderTheSeatsTheStoreRecorded(t *testing.T) {
	fixture := newNamesLab(t)
	storedSeat := filepath.Join(t.TempDir(), "seat-elsewhere")
	if _, err := fixture.db.Exec(
		`INSERT INTO calls (tool_use_id, session_id, seat_dir) VALUES ('toolu_1', 'sess-far', ?)`, storedSeat); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	writeTranscript(t, storedSeat, "-work", "sess-far", `{"type":"custom-title","customTitle":"far chat"}`)
	writeTranscript(t, fixture.seat, "-work", "sess-near", `{"type":"custom-title","customTitle":"near chat"}`)
	for session, want := range map[string]string{"sess-far": "far chat", "sess-near": "near chat"} {
		name, err := fixture.names.nameOf(session)
		if name != want || err != nil {
			t.Errorf("nameOf(%q) = %q, %v; want %q and no error", session, name, err, want)
		}
	}
}

// TestTranscriptNamesUnreadableTranscriptReturnsItsError: a transcript that
// exists but cannot be read is an error, never the session id start.
func TestTranscriptNamesUnreadableTranscriptReturnsItsError(t *testing.T) {
	fixture := newNamesLab(t)
	unreadable := filepath.Join(fixture.seat, "projects", "-work", "sess-bad.jsonl")
	if err := os.MkdirAll(unreadable, 0o700); err != nil { // opens, but reading it fails
		t.Fatal(err)
	}
	name, err := fixture.names.nameOf("sess-bad")
	if name != "" || err == nil || !strings.Contains(err.Error(), "read transcript "+unreadable) {
		t.Fatalf("nameOf = %q, %v; want blank and the read error naming %s", name, err, unreadable)
	}
}

func TestTranscriptNamesMalformedTitleEntryReturnsItsError(t *testing.T) {
	fixture := newNamesLab(t)
	path := writeTranscript(t, fixture.seat, "-work", "sess-torn",
		`{"type":"custom-title","customTitle":"fine"}`, `{"type":"ai-title","aiTitle":"torn`)
	_, err := fixture.names.nameOf("sess-torn")
	if err == nil || !strings.Contains(err.Error(), "decode a title entry in transcript "+path) {
		t.Fatalf("nameOf over a torn title entry = %v, want the decode error naming %s", err, path)
	}
}

// TestTranscriptNamesTitleStaysOnOneLine: control characters and runs of
// whitespace in a title never reach the table.
func TestTranscriptNamesTitleStaysOnOneLine(t *testing.T) {
	fixture := newNamesLab(t)
	writeTranscript(t, fixture.seat, "-work", "sess-wild",
		`{"type":"custom-title","customTitle":"a\tb\n\u001b[31mc\u001b[0m   d"}`)
	name, err := fixture.names.nameOf("sess-wild")
	if name != "a b [31mc [0m d" || err != nil {
		t.Fatalf("nameOf = %q, %v; want the title on one line without control characters", name, err)
	}
}

// TestTranscriptNamesSessionIDIsOneFileName: an id that is a path never reads
// outside the projects directories.
func TestTranscriptNamesSessionIDIsOneFileName(t *testing.T) {
	fixture := newNamesLab(t)
	outside := filepath.Join(fixture.seat, "escape.jsonl")
	if err := os.MkdirAll(filepath.Join(fixture.seat, "projects", "-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(`{"type":"custom-title","customTitle":"escaped"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := fixture.names.nameOf("../../escape")
	if name != "../../es" || err != nil {
		t.Fatalf("nameOf of a path-like id = %q, %v; want its first 8 characters and no error", name, err)
	}
}

func TestTranscriptNamesUnresolvableSeatIsAnError(t *testing.T) {
	fixture := newNamesLab(t)
	fixture.names.getenv = mapEnv(nil)
	if name, err := fixture.names.nameOf("sess-1"); name != "" || err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Fatalf("nameOf with no HOME and no stored seat = %q, %v; want blank and an error naming HOME", name, err)
	}
}

// storeSession records a sessions row whose transcript_path is path.
func storeSession(t *testing.T, fixture namesLab, session, path string) {
	t.Helper()
	if _, err := fixture.db.Exec(
		`INSERT INTO sessions (session_id, transcript_path) VALUES (?, ?)`, session, path); err != nil {
		t.Fatalf("seed session %s: %v", session, err)
	}
}

// TestTranscriptNamesReadsTheStoredTranscriptPathFirst: the title of the file
// sessions.transcript_path names wins over a transcript of the same session
// found by the seat search, and needs no seat at all.
func TestTranscriptNamesReadsTheStoredTranscriptPathFirst(t *testing.T) {
	fixture := newNamesLab(t)
	elsewhere := filepath.Join(t.TempDir(), "moved", "sess-path.jsonl")
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(elsewhere, []byte(`{"type":"custom-title","customTitle":"stored name"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, fixture.seat, "-work", "sess-path", `{"type":"custom-title","customTitle":"searched name"}`)
	storeSession(t, fixture, "sess-path", elsewhere)
	name, err := fixture.names.nameOf("sess-path")
	if name != "stored name" || err != nil {
		t.Fatalf("nameOf = %q, %v; want the title of the stored transcript path and no error", name, err)
	}
	fixture.names.getenv = mapEnv(nil)
	if name, err := fixture.names.nameOf("sess-path"); name != "stored name" || err != nil {
		t.Fatalf("nameOf with no seat = %q, %v; want the stored path to need none", name, err)
	}
}

// TestTranscriptNamesStoredPathThatIsGoneOrUntitledFallsToTheSearch: a path
// with no file, or a file with no title, leaves the name to the seat search.
func TestTranscriptNamesStoredPathThatIsGoneOrUntitledFallsToTheSearch(t *testing.T) {
	fixture := newNamesLab(t)
	untitled := filepath.Join(t.TempDir(), "untitled.jsonl")
	if err := os.WriteFile(untitled, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for session, path := range map[string]string{
		"sess-gone": filepath.Join(t.TempDir(), "gone.jsonl"), "sess-untitled": untitled, "sess-relative": "relative.jsonl",
	} {
		storeSession(t, fixture, session, path)
		writeTranscript(t, fixture.seat, "-work", session, `{"type":"custom-title","customTitle":"searched `+session+`"}`)
		name, err := fixture.names.nameOf(session)
		if want := "searched " + session; name != want || err != nil {
			t.Errorf("nameOf(%q) = %q, %v; want %q from the seat search", session, name, err, want)
		}
	}
}

// TestTranscriptNamesStoredPathThatCannotBeReadIsAnError: a directory where
// the transcript should be is an error, never the session id start.
func TestTranscriptNamesStoredPathThatCannotBeReadIsAnError(t *testing.T) {
	fixture := newNamesLab(t)
	dir := filepath.Join(t.TempDir(), "sess-dir.jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	storeSession(t, fixture, "sess-dir", dir)
	name, err := fixture.names.nameOf("sess-dir")
	if name != "" || err == nil || !strings.Contains(err.Error(), "transcript "+dir+" is not a regular file") {
		t.Fatalf("nameOf = %q, %v; want blank and the error naming %s", name, err, dir)
	}
}
