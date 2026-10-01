package report

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/clock"
)

// walkLimit bounds the directory entries one coverage run examines across every
// seat. A tree past it stops the scan and says so in a note: an unbounded walk
// stalls a report, and a silent cut would read as a smaller gap. A variable so
// a test can lower it.
var walkLimit = 200_000

// transcript is one main transcript file a seat holds.
type transcript struct {
	seat, project, session string
	modified               time.Time
	bytes                  int64
}

// Coverage lists the transcripts modified in the window that the store never
// recorded, latest first: one row per transcript under
// {seat}/projects/{project}/{session}.jsonl, for the report's own seat and every
// seat a stored session ran in. A sub-agent's transcript (under subagents/) is
// not a chat of its own and is never listed. A session is recorded when the
// sessions or calls table holds its id. The first note counts the transcripts
// modified, recorded and unrecorded; a projects directory that cannot be read
// is a note naming it, never a smaller count. Only --since, --session and
// --limit narrow it: --project and --agent-type name no column of a transcript.
func Coverage(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Title:  f.title("coverage", n),
		Header: []string{"SEAT", "PROJECT", "SESSION", "MODIFIED", "BYTES"},
	}
	since := f.Since
	if since.IsZero() {
		since = clock.Real.Now().Add(-Retention)
	}
	seats, err := coverageSeats(ctx, store, f.OwnSeat)
	if err != nil {
		return nil, err
	}
	recorded, err := recordedSessions(ctx, store)
	if err != nil {
		return nil, err
	}
	walk := &walker{since: since, session: f.Session, remaining: walkLimit}
	for _, seat := range seats {
		if err := walk.seat(ctx, seat); err != nil {
			return nil, err
		}
	}
	var missing []transcript
	for _, found := range walk.found {
		if !recorded[found.session] {
			missing = append(missing, found)
		}
	}
	sort.Slice(missing, func(i, j int) bool {
		a, b := missing[i], missing[j]
		if !a.modified.Equal(b.modified) {
			return a.modified.After(b.modified)
		}
		if a.seat != b.seat {
			return a.seat < b.seat
		}
		if a.project != b.project {
			return a.project < b.project
		}
		return a.session < b.session
	})
	notes := []string{fmt.Sprintf("%d transcripts modified in window, %d recorded, %d unrecorded",
		len(walk.found), len(walk.found)-len(missing), len(missing))}
	if f.OwnSeat == "" {
		notes = append(notes, "the report's own Claude Code config dir could not be resolved (no CLAUDE_CONFIG_DIR or HOME)")
	}
	notes = append(notes, walk.notes...)
	if len(missing) > f.limit() {
		missing = missing[:f.limit()]
	}
	for _, found := range missing {
		t.Rows = append(t.Rows, []string{
			found.seat, found.project, found.session,
			found.modified.UTC().Format("2006-01-02 15:04"), itoa(found.bytes),
		})
	}
	rest, err := topicNotes(ctx, store, f, n, false)
	if err != nil {
		return nil, err
	}
	t.Notes = append(notes, rest...)
	return t, nil
}

// coverageSeats are the Claude Code config dirs to scan: the report's own seat
// and every distinct seat_dir the sessions table holds, clean and sorted.
func coverageSeats(ctx context.Context, store *callmeter.Store, own string) ([]string, error) {
	set := map[string]bool{}
	if own != "" {
		set[filepath.Clean(own)] = true
	}
	err := query(ctx, store, "session seats",
		"SELECT DISTINCT seat_dir FROM sessions WHERE seat_dir IS NOT NULL AND seat_dir != ''", nil,
		func(r rowSource) error {
			var seat string
			if err := r.Scan(&seat); err != nil {
				return err
			}
			set[filepath.Clean(seat)] = true
			return nil
		})
	if err != nil {
		return nil, err
	}
	seats := make([]string, 0, len(set))
	for seat := range set {
		seats = append(seats, seat)
	}
	sort.Strings(seats)
	return seats, nil
}

// recordedSessions is every session id the sessions or calls table holds.
func recordedSessions(ctx context.Context, store *callmeter.Store) (map[string]bool, error) {
	recorded := map[string]bool{}
	err := query(ctx, store, "recorded sessions",
		`SELECT session_id FROM sessions WHERE session_id IS NOT NULL
		UNION SELECT session_id FROM calls WHERE session_id IS NOT NULL`, nil,
		func(r rowSource) error {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			recorded[id] = true
			return nil
		})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// walker walks the main transcripts of seats under one entry budget.
type walker struct {
	since     time.Time
	session   string // only this session's transcript, when set
	remaining int
	found     []transcript // modified in the window, recorded or not
	notes     []string
	stopped   bool
}

// seat reads {seat}/projects/*/*.jsonl. A directory that cannot be read is a
// note; running out of the entry budget stops every later read with one note.
func (s *walker) seat(ctx context.Context, seat string) error {
	projects := filepath.Join(seat, "projects")
	entries, ok := s.read(projects)
	if !ok {
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("callmeter report: scan transcripts under %s: %w", projects, err)
		}
		if !s.spend(projects) {
			return nil
		}
		dir := filepath.Join(projects, entry.Name())
		if entry.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(dir)
			if err != nil {
				s.notes = append(s.notes, fmt.Sprintf("cannot read %s: %v", dir, err))
				continue
			}
			if !info.IsDir() {
				continue
			}
		} else if !entry.IsDir() {
			continue
		}
		files, ok := s.read(dir)
		if !ok {
			continue
		}
		for _, file := range files {
			if !s.spend(projects) {
				return nil
			}
			if !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}
			session := strings.TrimSuffix(file.Name(), ".jsonl")
			if s.session != "" && session != s.session {
				continue
			}
			path := filepath.Join(dir, file.Name())
			info, err := file.Info()
			if file.Type()&fs.ModeSymlink != 0 {
				info, err = os.Stat(path)
			}
			if err != nil {
				s.notes = append(s.notes, fmt.Sprintf("cannot read %s: %v", path, err))
				continue
			}
			if !info.Mode().IsRegular() || info.ModTime().Before(s.since) {
				continue
			}
			s.found = append(s.found, transcript{
				seat: seat, project: entry.Name(), session: session, modified: info.ModTime(), bytes: info.Size(),
			})
		}
	}
	return nil
}

// read lists dir, spending one budget unit per entry; a failure is a note.
func (s *walker) read(dir string) ([]fs.DirEntry, bool) {
	if s.stopped {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.notes = append(s.notes, fmt.Sprintf("cannot read %s: %v", dir, err))
		return nil, false
	}
	return entries, true
}

// spend takes one entry from the budget; the last one stops the scan with a note.
func (s *walker) spend(where string) bool {
	if s.remaining <= 0 {
		if !s.stopped {
			s.stopped = true
			s.notes = append(s.notes, fmt.Sprintf(
				"scan stopped after %d directory entries under %s: coverage is partial", walkLimit, where))
		}
		return false
	}
	s.remaining--
	return true
}
