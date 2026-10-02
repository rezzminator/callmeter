package report

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	seat, project, session string // seat: every seat reaching the file, ", "-joined
	path                   string
	modified               time.Time
	bytes                  int64
}

// The classes of an unrecorded transcript, read from the transcript itself, in
// the order their notes print.
const (
	whyBeforeStore = "before-store" // its first entry predates the store's earliest record
	whyNoTurn      = "no-turn"      // no timestamped entry: no turn ran
	whyNoHook      = "no-hook"      // no stop-hook summary names callmeter's hook
	whyHookRan     = "hook-ran"     // callmeter's hook ran, yet the store holds nothing
	whyUnreadable  = "unreadable"   // the transcript could not be read; a note names it
)

var whyOrder = []string{whyBeforeStore, whyNoTurn, whyNoHook, whyHookRan, whyUnreadable}

// hookBinary ends the command plugins/callmeter/hooks/hooks.json registers for
// every hook event, Stop included, as a stop-hook summary names it.
const hookBinary = "/libexec/callmeter"

// Coverage lists the transcripts modified in the window that the store never
// recorded, latest first: one row per transcript under
// {seat}/projects/{project}/{session}.jsonl, for the report's own seat and every
// seat a stored session ran in. A sub-agent's transcript (under subagents/) is
// not a chat of its own and is never listed. A session is recorded when the
// sessions or calls table holds its id. The first note counts the transcripts
// modified, recorded and unrecorded; a projects directory that cannot be read
// is a note naming it, never a smaller count. Each row names when the transcript
// started (its first timestamped entry) and why the store holds nothing of it
// (WHY, see classify), and one note per class counts them. Only --since,
// --session and --limit narrow it: --project and --agent-type name no column of
// a transcript.
func Coverage(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Title:  f.title("coverage", n),
		Header: []string{"SEAT", "PROJECT", "SESSION", "STARTED", "MODIFIED", "BYTES", "WHY"},
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
	earliest, err := earliestRecord(ctx, store)
	if err != nil {
		return nil, err
	}
	walk := &walker{since: since, session: f.Session, remaining: walkLimit}
	dirs, err := projectsDirs(seats)
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		if err := walk.seat(ctx, dir); err != nil {
			return nil, err
		}
	}
	var missing []transcript
	for _, found := range walk.found {
		if !recorded[found.session] {
			missing = append(missing, found)
		}
	}
	started := make(map[string]time.Time, len(missing))
	why := make(map[string]string, len(missing))
	counts := map[string]int{}
	var readNotes []string
	for _, found := range missing {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("callmeter report: classify transcripts: %w", err)
		}
		at, class, err := classify(found.path, earliest)
		if err != nil {
			readNotes = append(readNotes, fmt.Sprintf("cannot read %s: %v", found.path, err))
		}
		started[found.path], why[found.path] = at, class
		counts[class]++
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
	for _, class := range whyOrder {
		if counts[class] > 0 {
			notes = append(notes, fmt.Sprintf("%d %s: %s", counts[class], class, whyMeaning(class, earliest)))
		}
	}
	notes = append(notes, walk.notes...)
	notes = append(notes, readNotes...)
	if len(missing) > f.limit() {
		missing = missing[:f.limit()]
	}
	for _, found := range missing {
		start := "-"
		if at := started[found.path]; !at.IsZero() {
			start = at.UTC().Format("2006-01-02 15:04")
		}
		t.Rows = append(t.Rows, []string{
			found.seat, found.project, found.session, start,
			found.modified.UTC().Format("2006-01-02 15:04"), itoa(found.bytes), why[found.path],
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

// projectsDir is one real projects directory and every seat that reaches it.
type projectsDir struct {
	path  string // the directory to read, symlinks resolved
	shown string // the first seat's {seat}/projects, what a note names
	seats []string
}

// projectsDirs resolves each seat's projects directory through symlinks and
// groups the seats that share one, so a transcript is walked once and names every
// seat. A directory that does not exist stays a group of its own under its
// unresolved path (the walk notes it); any other resolve error is returned.
func projectsDirs(seats []string) ([]projectsDir, error) {
	var dirs []projectsDir
	index := map[string]int{}
	for _, seat := range seats {
		projects := filepath.Join(seat, "projects")
		real, err := filepath.EvalSymlinks(projects)
		if errors.Is(err, fs.ErrNotExist) {
			real = projects
		} else if err != nil {
			return nil, fmt.Errorf("callmeter report: resolve %s: %w", projects, err)
		}
		i, ok := index[real]
		if !ok {
			i = len(dirs)
			index[real] = i
			dirs = append(dirs, projectsDir{path: real, shown: projects})
		}
		dirs[i].seats = append(dirs[i].seats, seat)
	}
	return dirs, nil
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

// earliestRecord is the first moment the store holds: the earliest session start
// or call, zero for an empty store.
func earliestRecord(ctx context.Context, store *callmeter.Store) (time.Time, error) {
	var earliest time.Time
	err := query(ctx, store, "earliest record",
		`SELECT MIN(t) FROM (SELECT MIN(first_ts) AS t FROM sessions UNION ALL SELECT MIN(ts) FROM calls)`, nil,
		func(r rowSource) error {
			var ms sql.NullInt64
			if err := r.Scan(&ms); err != nil {
				return err
			}
			if ms.Valid {
				earliest = time.UnixMilli(ms.Int64)
			}
			return nil
		})
	if err != nil {
		return time.Time{}, err
	}
	return earliest, nil
}

// whyMeaning is the note text of one class.
func whyMeaning(class string, earliest time.Time) string {
	switch class {
	case whyBeforeStore:
		return "started before the store's earliest record, " + earliest.UTC().Format("2006-01-02 15:04") + " UTC"
	case whyNoTurn:
		return "no timestamped entry, so no turn ran"
	case whyNoHook:
		return "no stop-hook summary names callmeter's hook: the plugin was not loaded for that launch, or no turn ended"
	case whyHookRan:
		return "callmeter's hook ran, yet this store holds no record: another CALLMETER_HOME, or a capture gap"
	default:
		return "the transcript could not be read"
	}
}

// classify reads a transcript line by line for when it started (its first
// top-level timestamp) and why the store holds none of it: before-store when
// it started before earliest (a non-zero store's first record), no-turn when no
// entry carries a timestamp, hook-ran once a stop_hook_summary names callmeter's
// hook, no-hook otherwise. Only the timestamp and the hook commands are decoded;
// nothing of the transcript leaves this function but the time and the class. A
// read failure is the unreadable class and the error.
func classify(path string, earliest time.Time) (time.Time, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, whyUnreadable, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	var started time.Time
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if started.IsZero() {
				var head struct {
					Timestamp string `json:"timestamp"`
				}
				if json.Unmarshal(line, &head) == nil && head.Timestamp != "" {
					if at, err := time.Parse(time.RFC3339Nano, head.Timestamp); err == nil {
						started = at
						if !earliest.IsZero() && at.Before(earliest) {
							return started, whyBeforeStore, nil
						}
					}
				}
			}
			if bytes.Contains(line, []byte(`"stop_hook_summary"`)) && namesCallmeterHook(line) {
				return started, whyHookRan, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return started, whyUnreadable, readErr
		}
	}
	if started.IsZero() {
		return started, whyNoTurn, nil
	}
	return started, whyNoHook, nil
}

// namesCallmeterHook reports whether a stop_hook_summary line lists a command
// whose program is callmeter's hook binary.
func namesCallmeterHook(line []byte) bool {
	var summary struct {
		Subtype   string `json:"subtype"`
		HookInfos []struct {
			Command string `json:"command"`
		} `json:"hookInfos"`
	}
	if json.Unmarshal(line, &summary) != nil || summary.Subtype != "stop_hook_summary" {
		return false
	}
	for _, info := range summary.HookInfos {
		fields := strings.Fields(info.Command)
		if len(fields) > 0 && strings.HasSuffix(strings.Trim(fields[0], `"'`), hookBinary) {
			return true
		}
	}
	return false
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

// seat reads {projects}/*/*.jsonl of one real projects directory, naming d.shown
// in its notes. A directory that cannot be read is a
// note; running out of the entry budget stops every later read with one note.
func (s *walker) seat(ctx context.Context, d projectsDir) error {
	projects := d.path
	seat := strings.Join(d.seats, ", ")
	entries, ok := s.readNamed(projects, d.shown)
	if !ok {
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("callmeter report: scan transcripts under %s: %w", projects, err)
		}
		if !s.spend(d.shown) {
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
			if !s.spend(d.shown) {
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
				seat: seat, project: entry.Name(), session: session, path: path, modified: info.ModTime(), bytes: info.Size(),
			})
		}
	}
	return nil
}

// read lists dir, spending one budget unit per entry; a failure is a note.
func (s *walker) read(dir string) ([]fs.DirEntry, bool) { return s.readNamed(dir, dir) }

// readNamed is read with the note naming shown, not the resolved dir.
func (s *walker) readNamed(dir, shown string) ([]fs.DirEntry, bool) {
	if s.stopped {
		return nil, false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.notes = append(s.notes, fmt.Sprintf("cannot read %s: %s", shown, strings.ReplaceAll(err.Error(), dir, shown)))
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
