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
	wantHeader(t, table, "SEAT", "PROJECT", "SESSION", "MODIFIED", "BYTES")
	wantRows(t, table,
		lab.seatA+"|-w-p|s-miss|2026-09-23 11:00|5",
		lab.seatB+"|-w-q|s-other|2026-09-23 10:00|9",
	)
	if want := "4 transcripts modified in window, 2 recorded, 2 unrecorded"; len(table.Notes) != 1 || table.Notes[0] != want {
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
	wantRows(t, table, lab.seatA+"|-w-p|s-miss|2026-09-23 11:00|5")
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
	wantRows(t, table, lab.seatA+"|-w-p|s-miss|2026-09-23 11:00|5")
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
	if len(table.Rows) != 0 || len(table.Header) != 5 || !slices.Contains(table.Notes, "0 transcripts modified in window, 0 recorded, 0 unrecorded") {
		t.Errorf("rows %v header %v notes %q, want no rows, the 5 columns and the zero counts", table.Rows, table.Header, table.Notes)
	}
}
