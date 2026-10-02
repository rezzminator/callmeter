package report

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// transcriptFile writes {seat}/projects/{project}/{rel} of size bytes, modified age before testNow.
func transcriptFile(t *testing.T, seat, project, rel string, size int, age time.Duration) {
	t.Helper()
	path := filepath.Join(seat, "projects", project, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, testNow.Add(-age), testNow.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

// coverageLab holds two seats of transcripts and a store that recorded some of
// the sessions: s-rec (a sessions row, seat B) and s-calls (a calls row).
type coverageLab struct {
	store        *callmeter.Store
	seatA, seatB string
}

func newCoverageLab(t *testing.T) coverageLab {
	t.Helper()
	root := t.TempDir()
	lab := coverageLab{store: openStore(t), seatA: filepath.Join(root, "seat-a"), seatB: filepath.Join(root, "seat-b")}
	transcriptFile(t, lab.seatA, "-w-p", "s-rec.jsonl", 10, time.Hour)
	transcriptFile(t, lab.seatA, "-w-p", "s-miss.jsonl", 5, time.Hour)
	transcriptFile(t, lab.seatA, "-w-p", "s-old.jsonl", 7, 10*24*time.Hour)
	transcriptFile(t, lab.seatA, "-w-p", "s-miss/subagents/agent-x.jsonl", 3, time.Hour)
	transcriptFile(t, lab.seatA, "-w-p", "notes.txt", 3, time.Hour)
	transcriptFile(t, lab.seatB, "-w-q", "s-other.jsonl", 9, 2*time.Hour)
	transcriptFile(t, lab.seatB, "-w-q", "s-calls.jsonl", 4, 2*time.Hour)
	seedSession(t, lab.store, callmeter.Session{
		SessionID: "s-rec", TS: ms(time.Hour), SeatDir: callmeter.Ptr(lab.seatB),
	})
	seed(t, lab.store, mkCall("c1", "s-calls", time.Hour, "Read"))
	return lab
}

func (lab coverageLab) filter() Filter {
	return Filter{Since: testNow.Add(-7 * 24 * time.Hour), OwnSeat: lab.seatA}
}

func TestCoverageListsUnrecordedTranscriptsInTheWindowWithTheirCounts(t *testing.T) {
	lab := newCoverageLab(t)
	table, err := Coverage(context.Background(), lab.store, lab.filter(), chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	wantHeader(t, table, "SEAT", "PROJECT", "SESSION", "STARTED", "MODIFIED", "BYTES", "WHY")
	wantRows(t, table,
		lab.seatA+"|-w-p|s-miss|-|2026-09-23 11:00|5|no-turn",
		lab.seatB+"|-w-q|s-other|-|2026-09-23 10:00|9|no-turn",
	)
	if want := []string{"4 transcripts modified in window, 2 recorded, 2 unrecorded", "2 no-turn: no timestamped entry, so no turn ran"}; !slices.Equal(table.Notes, want) {
		t.Errorf("notes = %q, want only %q", table.Notes, want)
	}
}

func TestCoverageSessionAndLimitNarrowTheRowsNotTheCounts(t *testing.T) {
	lab := newCoverageLab(t)
	ctx := context.Background()
	filter := lab.filter()
	filter.Session = "s-miss"
	table, err := Coverage(ctx, lab.store, filter, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	wantRows(t, table, lab.seatA+"|-w-p|s-miss|-|2026-09-23 11:00|5|no-turn")
	if want := "1 transcripts modified in window, 0 recorded, 1 unrecorded"; !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
	filter = lab.filter()
	filter.Limit = 1
	table, err = Coverage(ctx, lab.store, filter, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(table.Rows) != 1 || !slices.Contains(table.Notes, "4 transcripts modified in window, 2 recorded, 2 unrecorded") {
		t.Errorf("limit 1: rows %v notes %q, want one row and the full counts", rowsOf(table), table.Notes)
	}
}

func TestCoverageSinceFloorsTheModifiedTime(t *testing.T) {
	lab := newCoverageLab(t)
	filter := lab.filter()
	filter.Since = testNow.Add(-90 * time.Minute)
	table, err := Coverage(context.Background(), lab.store, filter, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	wantRows(t, table, lab.seatA+"|-w-p|s-miss|-|2026-09-23 11:00|5|no-turn")
}

// TestCoverageUnreadableProjectsDirIsANameNeverZero: a seat whose projects
// directory cannot be read is a note naming it, and the other seats still list.
func TestCoverageUnreadableProjectsDirIsANameNeverZero(t *testing.T) {
	lab := newCoverageLab(t)
	broken := filepath.Join(t.TempDir(), "seat-broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "projects"), []byte("a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedSession(t, lab.store, callmeter.Session{SessionID: "s-broken", TS: ms(time.Hour), SeatDir: callmeter.Ptr(broken)})
	table, err := Coverage(context.Background(), lab.store, lab.filter(), chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(table.Rows) != 2 {
		t.Errorf("rows = %v, want the readable seats' two transcripts", rowsOf(table))
	}
	named := 0
	for _, note := range table.Notes {
		if strings.HasPrefix(note, "cannot read "+filepath.Join(broken, "projects")+": ") {
			named++
		}
	}
	if named != 1 {
		t.Errorf("notes = %q, want one note naming %s", table.Notes, filepath.Join(broken, "projects"))
	}
}

func TestCoverageWithoutAnOwnSeatSaysSo(t *testing.T) {
	lab := newCoverageLab(t)
	filter := lab.filter()
	filter.OwnSeat = ""
	table, err := Coverage(context.Background(), lab.store, filter, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if !slices.Contains(table.Notes, "the report's own Claude Code config dir could not be resolved (no CLAUDE_CONFIG_DIR or HOME)") {
		t.Errorf("notes = %q, want the unresolved own seat named", table.Notes)
	}
}

// TestCoverageWalkIsBounded: a tree past the entry budget stops the scan and
// the note says the coverage is partial. Serial: it lowers the package budget.
func TestCoverageWalkIsBounded(t *testing.T) {
	lab := newCoverageLab(t)
	previous := walkLimit
	walkLimit = 3
	t.Cleanup(func() { walkLimit = previous })
	table, err := Coverage(context.Background(), lab.store, lab.filter(), chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if want := "scan stopped after 3 directory entries under " + filepath.Join(lab.seatA, "projects") +
		": coverage is partial"; !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
}

func TestCoverageEmptyWindowHasHeaderCountsAndNoRows(t *testing.T) {
	store := openStore(t)
	table, err := Coverage(context.Background(), store, Filter{Since: testNow.Add(-time.Hour), OwnSeat: t.TempDir()}, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(table.Rows) != 0 || len(table.Header) != 7 || !slices.Contains(table.Notes, "0 transcripts modified in window, 0 recorded, 0 unrecorded") {
		t.Errorf("rows %v header %v notes %q, want no rows, the 7 columns and the zero counts", table.Rows, table.Header, table.Notes)
	}
}

// TestCoverageListsASharedProjectsDirOnceNamingEverySeat: two seats whose
// projects/ symlink to a third's list each transcript once, with all three seats
// on the row, and the count note counts it once.
func TestCoverageListsASharedProjectsDirOnceNamingEverySeat(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	two, three := filepath.Join(root, "two"), filepath.Join(root, "three")
	transcriptFile(t, real, "-w-p", "s-one.jsonl", 6, time.Hour)
	transcriptFile(t, real, "-w-p", "s-two.jsonl", 8, 2*time.Hour)
	for _, seat := range []string{two, three} {
		if err := os.MkdirAll(seat, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(real, "projects"), filepath.Join(seat, "projects")); err != nil {
			t.Fatal(err)
		}
	}
	store := openStore(t)
	for _, seat := range []string{two, three} {
		seedSession(t, store, callmeter.Session{SessionID: "s-seat-" + filepath.Base(seat), TS: ms(time.Hour), SeatDir: callmeter.Ptr(seat)})
	}
	filter := Filter{Since: testNow.Add(-7 * 24 * time.Hour), OwnSeat: real}
	table, err := Coverage(context.Background(), store, filter, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	seats := strings.Join([]string{real, three, two}, ", ")
	wantRows(t, table,
		seats+"|-w-p|s-one|-|2026-09-23 11:00|6|no-turn",
		seats+"|-w-p|s-two|-|2026-09-23 10:00|8|no-turn",
	)
	if want := "2 transcripts modified in window, 0 recorded, 2 unrecorded"; !slices.Contains(table.Notes, want) {
		t.Errorf("notes = %q, want %q", table.Notes, want)
	}
}

// transcriptLines writes {seat}/projects/{project}/{rel} holding lines, modified age before testNow.
func transcriptLines(t *testing.T, seat, project, rel string, age time.Duration, lines ...string) {
	t.Helper()
	path := filepath.Join(seat, "projects", project, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, testNow.Add(-age), testNow.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

// stopSummary is a transcript's stop-hook summary line naming commands.
func stopSummary(at string, commands ...string) string {
	infos := make([]string, len(commands))
	for i, c := range commands {
		infos[i] = `{"command":"` + c + `"}`
	}
	return `{"type":"system","subtype":"stop_hook_summary","timestamp":"` + at + `","hookCount":` +
		itoa(int64(len(commands))) + `,"hookInfos":[` + strings.Join(infos, ",") + `]}`
}

// TestCoverageNamesWhyEachTranscriptIsUnrecorded: each unrecorded row carries
// its first entry's time and its class, read from the transcript itself, and a
// note counts each class: before the store's earliest record, no turn,
// callmeter's hook never ran, callmeter's hook ran yet nothing is stored.
func TestCoverageNamesWhyEachTranscriptIsUnrecorded(t *testing.T) {
	seat := t.TempDir()
	const turn = `{"type":"user","timestamp":"%s","sessionId":"s"}`
	at := func(s string) string { return strings.Replace(turn, "%s", s, 1) }
	transcriptLines(t, seat, "-w-p", "s-before.jsonl", 30*time.Minute,
		`{"type":"mode","mode":"normal"}`, at("2026-09-23T07:00:00.000Z"), stopSummary("2026-09-23T07:01:00.000Z", "/opt/demo/libexec/callmeter hook"))
	transcriptLines(t, seat, "-w-p", "s-stub.jsonl", 40*time.Minute, `{"type":"custom-title","customTitle":"demo"}`)
	transcriptLines(t, seat, "-w-p", "s-nohook.jsonl", 50*time.Minute,
		at("2026-09-23T10:00:00.000Z"), stopSummary("2026-09-23T10:01:00.000Z", "bash /opt/demo/notify.sh stop"))
	transcriptLines(t, seat, "-w-p", "s-hook.jsonl", 60*time.Minute,
		at("2026-09-23T10:05:00.000Z"), stopSummary("2026-09-23T10:06:00.000Z", "bash /opt/demo/notify.sh stop", "${CLAUDE_PLUGIN_ROOT}/libexec/callmeter hook"))
	transcriptLines(t, seat, "-w-p", "s-hook-bare.jsonl", 70*time.Minute,
		at("2026-09-23T10:10:00.000Z"), stopSummary("2026-09-23T10:11:00.000Z", `\"${CLAUDE_PLUGIN_ROOT}/libexec/callmeter\"`))
	store := openStore(t)
	seedSession(t, store, callmeter.Session{SessionID: "s-stored", TS: ms(3 * time.Hour)})
	table, err := Coverage(context.Background(), store, Filter{Since: testNow.Add(-24 * time.Hour), OwnSeat: seat}, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	wantHeader(t, table, "SEAT", "PROJECT", "SESSION", "STARTED", "MODIFIED", "BYTES", "WHY")
	size := func(name string) string {
		info, err := os.Stat(filepath.Join(seat, "projects", "-w-p", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return itoa(info.Size())
	}
	wantRows(t, table,
		seat+"|-w-p|s-before|2026-09-23 07:00|2026-09-23 11:30|"+size("s-before")+"|before-store",
		seat+"|-w-p|s-stub|-|2026-09-23 11:20|"+size("s-stub")+"|no-turn",
		seat+"|-w-p|s-nohook|2026-09-23 10:00|2026-09-23 11:10|"+size("s-nohook")+"|no-hook",
		seat+"|-w-p|s-hook|2026-09-23 10:05|2026-09-23 11:00|"+size("s-hook")+"|hook-ran",
		seat+"|-w-p|s-hook-bare|2026-09-23 10:10|2026-09-23 10:50|"+size("s-hook-bare")+"|hook-ran",
	)
	for _, want := range []string{
		"5 transcripts modified in window, 0 recorded, 5 unrecorded",
		"1 before-store: started before the store's earliest record, 2026-09-23 09:00 UTC",
		"1 no-turn: no timestamped entry, so no turn ran",
		"1 no-hook: no stop-hook summary names callmeter's hook: the plugin was not loaded for that launch, or no turn ended",
		"2 hook-ran: callmeter's hook ran, yet this store holds no record: another CALLMETER_HOME, or a capture gap",
	} {
		if !slices.Contains(table.Notes, want) {
			t.Errorf("notes = %q, want %q", table.Notes, want)
		}
	}
}

// TestCoverageUnreadableTranscriptIsAClassAndANote: a transcript that cannot be
// opened is the unreadable class with a note naming it, never another class.
func TestCoverageUnreadableTranscriptIsAClassAndANote(t *testing.T) {
	seat := t.TempDir()
	transcriptLines(t, seat, "-w-p", "s-locked.jsonl", time.Hour, `{"type":"user","timestamp":"2026-09-23T10:00:00.000Z"}`)
	path := filepath.Join(seat, "projects", "-w-p", "s-locked.jsonl")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	table, err := Coverage(context.Background(), openStore(t), Filter{Since: testNow.Add(-24 * time.Hour), OwnSeat: seat}, chatOf)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	wantRows(t, table, seat+"|-w-p|s-locked|-|2026-09-23 11:00|55|unreadable")
	named := 0
	for _, note := range table.Notes {
		if strings.HasPrefix(note, "cannot read "+path+": ") {
			named++
		}
	}
	if named != 1 || !slices.Contains(table.Notes, "1 unreadable: the transcript could not be read") {
		t.Errorf("notes = %q, want the class counted and one note naming %s", table.Notes, path)
	}
}
