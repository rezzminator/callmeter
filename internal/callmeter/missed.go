package callmeter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	modernsqlite "modernc.org/sqlite"

	"github.com/rezzminator/callmeter/internal/clock"
)

// ingestInfix names the file IngestMissed claims: {missed.log}.ingest-{pid}.
const ingestInfix = ".ingest-"

// doneInfix names an ingested claim kept for its grace period:
// {missed.log}.done-{bytes ingested}-{claim suffix}.
const doneInfix = ".done-"

// DoneGrace is how long an ingested claim is kept after its last write before
// it is removed: an appender that opened missed.log before the claim's rename
// writes into the claimed file within microseconds of its open.
const DoneGrace = 2 * time.Second

// unparsedLimit is how many bytes of a malformed missed.log line its fault keeps.
const unparsedLimit = 200

// IngestMissed turns the wrapper's missed.log into `binary` faults, and the
// lines the binary wrote itself (a reason opening TerminatedReason or
// StoreUnavailableReason, or PanicReason) into `terminated` faults, and
// returns how many it wrote. Each line is
// `{unix seconds}\t{event}\t{reason}`, optionally followed by
// `\t{session_id}`; the writer's pid sits at the reason's end as ` (pid N)`.
// It becomes a fault stamped seconds × 1000 whose error is
// `{event}: {reason}` and whose session is that session_id;
// a line that does not parse becomes a fault `unparsed missed.log line: …` of
// its first 200 bytes stamped at its file's last write, never a dropped line.
//
// The file is renamed to {path}.ingest-{pid} first, so the wrapper's next
// append starts a new missed.log, and every older {path}.ingest-* a crashed
// run left is ingested with it. Hooks run in parallel, so a claim is ingested
// only by the run holding its exclusive flock: a claim another live run holds
// is left to that run, and a claim whose run died has no lock left. All the
// faults go in one transaction. The claim this run just made has never been
// ingested, so each of its lines is a fault, a line identical to a stored fault
// included (two hooks of one session cancelled in one second, ingested by two
// runs). From every other file a fault is written only beyond the identical
// rows (ts, stage, session_id, tool_use_id, error) the store already holds: a
// claim whose run died after its commit is ingested again with no fault twice,
// and identical lines of one ingest stay as many faults. After the commit,
// still under its lock, each claim is renamed {path}.done-{bytes read}-… and
// kept until DoneGrace has passed since its last write: a line appended through
// a handle opened before the claim's rename lands there, and the first ingest
// after the grace writes the lines past the bytes read in its own transaction
// and removes the file after its commit. A failed commit leaves every file for
// the next run. A path with no file is 0, nil.
func (s *Store) IngestMissed(ctx context.Context, path string) (int, error) {
	return s.IngestMissedHeld(ctx, path, nil)
}

// IngestMissedHeld is IngestMissed calling hold, when not nil, once the
// faults are written and before they commit, and the release it returns once
// the files are moved or removed: a hook holds its signal handler off across
// commit and move, so a signal there waits for both.
func (s *Store) IngestMissedHeld(ctx context.Context, path string, hold func() (release func())) (n int, err error) {
	own, err := claimMissed(path)
	if err != nil {
		return 0, err
	}
	claims, err := ingestFiles(path, ingestInfix)
	if err != nil {
		return 0, err
	}
	dones, err := ingestFiles(path, doneInfix)
	if err != nil {
		return 0, err
	}
	now := clock.Real.Now()
	var taken []takenClaim
	var faults []Fault // from files a dead run may already have ingested
	var fresh []Fault  // from own: never ingested
	var locks []*os.File
	defer func() {
		for _, held := range locks {
			if closeErr := held.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("callmeter store %s: release %s: %w", s.path, held.Name(), closeErr))
			}
		}
	}()
	for _, file := range claims {
		held, data, info, err := lockClaim(file)
		if err != nil {
			return 0, fmt.Errorf("callmeter store %s: %w", s.path, err)
		}
		if held == nil {
			continue
		}
		locks = append(locks, held)
		taken = append(taken, takenClaim{file: file, read: len(data)})
		if file == own {
			fresh = append(fresh, parseMissed(data, info.ModTime().UnixMilli())...)
		} else {
			faults = append(faults, parseMissed(data, info.ModTime().UnixMilli())...)
		}
	}
	for _, file := range dones {
		if named, err := os.Lstat(file); err == nil && now.Sub(named.ModTime()) < DoneGrace {
			continue // inside its grace: a later run takes it
		}
		held, data, info, err := lockClaim(file)
		if err != nil {
			return 0, fmt.Errorf("callmeter store %s: %w", s.path, err)
		}
		if held == nil {
			continue
		}
		locks = append(locks, held)
		if now.Sub(info.ModTime()) < DoneGrace { // written since the listing
			continue
		}
		taken = append(taken, takenClaim{file: file, done: true})
		if read := doneRead(path, file); read < len(data) {
			faults = append(faults, parseMissed(data[read:], info.ModTime().UnixMilli())...)
		}
	}
	if len(taken) == 0 {
		return 0, nil
	}
	release := func() {}
	defer func() { release() }()
	if err := s.Batch(ctx, func(tx *Tx) error {
		// The other files first: their check counts only rows stored before this ingest.
		written, err := tx.addMissedFaults(ctx, faults)
		if err != nil {
			return err
		}
		for _, fault := range fresh {
			if err := tx.AddFault(ctx, fault); err != nil {
				return err
			}
			written++
		}
		n = written
		if hold != nil {
			release = hold()
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("callmeter store %s: ingest %d missed.log faults: %w", s.path, len(faults)+len(fresh), err)
	}
	var moveErr error
	for _, claim := range taken {
		if claim.done {
			if err := os.Remove(claim.file); err != nil {
				moveErr = errors.Join(moveErr, fmt.Errorf("callmeter store %s: remove ingested %s: %w", s.path, claim.file, err))
			}
			continue
		}
		if err := keepDone(path, claim, now); err != nil {
			moveErr = errors.Join(moveErr, fmt.Errorf("callmeter store %s: %w", s.path, err))
		}
	}
	return n, moveErr
}

// takenClaim is a file one ingest holds: a claim and the bytes read from it,
// or an ingested claim past its grace period (done).
type takenClaim struct {
	file string
	read int
	done bool
}

// keepDone renames an ingested claim {path}.done-{bytes read}-{claim suffix},
// never over another file, and then stamps its grace from now: a claim left
// unrenamed keeps its last write time, which its unparsed lines are stamped
// at, so the next ingest of it finds them stored.
func keepDone(path string, claim takenClaim, now time.Time) error {
	suffix := strings.TrimPrefix(filepath.Base(claim.file), filepath.Base(path)+ingestInfix)
	base := path + doneInfix + strconv.Itoa(claim.read) + "-" + suffix
	target := base
	for n := 1; ; n++ {
		_, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("look for %s: %w", target, err)
		}
		target = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(claim.file, target); err != nil {
		return fmt.Errorf("keep ingested %s as %s: %w", claim.file, target, err)
	}
	if err := os.Chtimes(target, now, now); err != nil {
		return fmt.Errorf("stamp ingested %s: %w", target, err)
	}
	return nil
}

// doneRead is the count of bytes the ingest that kept file had read, from its
// name; a name that holds none reads as 0, so every line is ingested again
// and only those not yet stored are written.
func doneRead(path, file string) int {
	field, _, _ := strings.Cut(strings.TrimPrefix(filepath.Base(file), filepath.Base(path)+doneInfix), "-")
	read, err := strconv.Atoi(field)
	if err != nil || read < 0 {
		return 0
	}
	return read
}

// addMissedFaults writes the faults of one ingest, each only beyond the
// identical rows the store already holds, and returns how many it wrote.
func (t *Tx) addMissedFaults(ctx context.Context, faults []Fault) (int, error) {
	stored := map[Fault]int{}
	for _, fault := range faults {
		if _, seen := stored[fault]; seen {
			continue
		}
		var n int
		if err := t.tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM faults WHERE ts = ? AND stage = ? AND session_id IS ? AND tool_use_id IS ? AND error = ?",
			fault.TS, fault.Stage, nullString(fault.SessionID), nullString(fault.ToolUseID), fault.Error,
		).Scan(&n); err != nil {
			return 0, fmt.Errorf("callmeter store %s: count stored %s faults: %w", t.path, fault.Stage, err)
		}
		stored[fault] = n
	}
	written := 0
	for _, fault := range faults {
		if stored[fault] > 0 {
			stored[fault]--
			continue
		}
		if err := t.AddFault(ctx, fault); err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

// claimMissed renames path to a file nothing appends to and returns that
// file's path; an absent path is "", not an error. An existing claim of the same pid (a crashed run's, its pid reused)
// is never overwritten: the new claim takes a numbered name beside it.
func claimMissed(path string) (string, error) {
	target := path + ingestInfix + strconv.Itoa(os.Getpid())
	for n := 1; ; n++ {
		_, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("callmeter missed log: look for %s: %w", target, err)
		}
		target = fmt.Sprintf("%s%s%d-%d", path, ingestInfix, os.Getpid(), n)
	}
	if err := os.Rename(path, target); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("callmeter missed log: claim %s as %s: %w", path, target, err)
	}
	return target, nil
}

// lockClaim opens a claimed file and takes its exclusive flock without
// waiting, returning the open file, which holds the lock, its content and its
// stat. A nil file and nil error mean the claim is not this run's: another run
// holds its lock, or took it, ingested it and removed or renamed it in between.
func lockClaim(file string) (*os.File, []byte, os.FileInfo, error) {
	held, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		// Gone since the listing: another run ingested it. A name that is
		// still there (a dangling link) is unreadable, never absent.
		if _, lstatErr := os.Lstat(file); errors.Is(lstatErr, os.ErrNotExist) {
			return nil, nil, nil, nil
		}
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open %s: %w", file, err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := held.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if closeErr != nil {
				return nil, nil, nil, fmt.Errorf("close %s held by another run: %w", file, closeErr)
			}
			return nil, nil, nil, nil
		}
		return nil, nil, nil, errors.Join(fmt.Errorf("lock %s: %w", file, err), closeErr)
	}
	// The lock is on the inode opened; the name must still point at it, or a
	// run that held the lock before us has already ingested and removed or
	// renamed it.
	opened, statErr := held.Stat()
	named, lstatErr := os.Lstat(file)
	if errors.Is(lstatErr, os.ErrNotExist) || (statErr == nil && lstatErr == nil && !os.SameFile(opened, named)) {
		if err := held.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close %s already ingested: %w", file, err)
		}
		return nil, nil, nil, nil
	}
	if err := errors.Join(statErr, lstatErr); err != nil {
		return nil, nil, nil, errors.Join(fmt.Errorf("stat %s: %w", file, err), held.Close())
	}
	data, err := io.ReadAll(held)
	if err != nil {
		return nil, nil, nil, errors.Join(fmt.Errorf("read %s: %w", file, err), held.Close())
	}
	// Stat again after the read: the stamp is the last write the data holds.
	if opened, err = held.Stat(); err != nil {
		return nil, nil, nil, errors.Join(fmt.Errorf("stat %s: %w", file, err), held.Close())
	}
	return held, data, opened, nil
}

// CountMissed counts, read-only, the lines of path (missed.log) and of every
// claim {path}.ingest-* a run left, by the stage IngestMissed would give their
// faults (StageBinary or StageTerminated, an unparsed line StageBinary): what
// a report says was lost when no store exists to ingest them into. An ingested
// claim {path}.done-* is not counted: its lines went into a store. A file gone
// between its listing and its read counts nothing.
func CountMissed(path string) (map[string]int, error) {
	claims, err := ingestFiles(path, ingestInfix)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, file := range append([]string{path}, claims...) {
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("callmeter missed log: read %s: %w", file, err)
		}
		for _, fault := range parseMissed(data, 0) {
			counts[fault.Stage]++
		}
	}
	return counts, nil
}

// ingestFiles lists {path}{infix}* in name order.
func ingestFiles(path, infix string) ([]string, error) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+infix
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("callmeter missed log: list %s: %w", dir, err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// parseMissed turns the lines of one missed.log into faults; written, the
// file's last write, stamps a line that carries no time of its own, so the
// same file ingested again gives the same faults.
func parseMissed(data []byte, written int64) []Fault {
	var faults []Fault
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		if fault, ok := parseMissedLine(string(line)); ok {
			faults = append(faults, fault)
			continue
		}
		kept := line
		if len(kept) > unparsedLimit {
			kept = kept[:unparsedLimit]
		}
		faults = append(faults, Fault{
			TS:    written,
			Stage: StageBinary,
			Error: "unparsed missed.log line: " + strings.ToValidUTF8(string(kept), ""),
		})
	}
	return faults
}

// TerminatedReason opens the reason of a missed.log line the binary wrote itself
// when a signal or its store-wait bound ended its run before it recorded:
// `terminated by SIGTERM`. parseMissedLine turns such a line, a
// StoreUnavailableReason one and a PanicReason one into a StageTerminated
// fault; every other line stays StageBinary.
const TerminatedReason = "terminated by "

// PanicReason is the reason of the binary's own line when its hook run
// panicked before being accounted for: `callmeter hook` recovers, exits 0 and
// leaves this line, with the event and session its payload named so far.
// parseMissedLine turns it into a StageTerminated fault, as a `terminated by` line.
const PanicReason = "panic"

// BatchWithoutCalls is the payload fault for a PostToolBatch naming no calls.
const BatchWithoutCalls = "PostToolBatch payload carries no tool_calls"

// TerminatedByStoreBusy is the reason of the binary's own line when its store
// wait hit its event's bound before it recorded: the store stayed locked, and
// the hook gave up inside its timeout instead of being killed by it.
const TerminatedByStoreBusy = TerminatedReason + "store busy"

// StoreUnavailableReason opens the reason of the binary's own line when its
// store could take neither its event nor the fault saying so, for a cause
// other than a busy store: `store unavailable: {class}`, the class one of the
// StoreClass labels (StoreFailureClass), never the error's own text.
// parseMissedLine turns it into a StageTerminated fault, as a `terminated by`
// line.
const StoreUnavailableReason = "store unavailable: "

// The StoreClass labels: why a store could not be written, as
// StoreFailureClass reads an error.
const (
	StoreClassNewerSchema = "newer schema" // its user_version is newer than SchemaVersion
	StoreClassFull        = "full"         // SQLITE_FULL, ENOSPC
	StoreClassReadonly    = "readonly"     // SQLITE_READONLY, SQLITE_PERM, EROFS, EACCES, EPERM
	StoreClassIO          = "io"           // SQLITE_IOERR, EIO
	StoreClassCorrupt     = "corrupt"      // SQLITE_CORRUPT, SQLITE_NOTADB
	StoreClassOpen        = "open"         // SQLITE_CANTOPEN, any other file-system error
	StoreClassOther       = "other"        // anything else, a constraint or a trigger's abort among them
)

// StoreFailureClass reads why a store could not be written as one of the
// StoreClass labels: a refused newer schema (ErrNewerSchema), else SQLite's
// primary result code, else the file system's errno, else any other
// file-system error as StoreClassOpen; the rest is StoreClassOther. The error's
// own text never reaches the label.
func StoreFailureClass(err error) string {
	if errors.Is(err, ErrNewerSchema) {
		return StoreClassNewerSchema
	}
	var sqliteError *modernsqlite.Error
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case sqliteFull:
			return StoreClassFull
		case sqliteReadonly, sqlitePerm:
			return StoreClassReadonly
		case sqliteIOErr:
			return StoreClassIO
		case sqliteCorrupt, sqliteNotADB:
			return StoreClassCorrupt
		case sqliteCantOpen:
			return StoreClassOpen
		}
		return StoreClassOther
	}
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return StoreClassFull
	case errors.Is(err, syscall.EROFS), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return StoreClassReadonly
	case errors.Is(err, syscall.EIO):
		return StoreClassIO
	}
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return StoreClassOpen
	}
	return StoreClassOther
}

// SQLite's primary result codes StoreFailureClass reads.
const (
	sqlitePerm     = 3
	sqliteReadonly = 8
	sqliteIOErr    = 10
	sqliteCorrupt  = 11
	sqliteFull     = 13
	sqliteCantOpen = 14
	sqliteNotADB   = 26
)

// parseMissedLine reads `{unix seconds}\t{event}\t{reason}[\t{session_id}]`;
// the writer's pid sits at the reason's end. False when the line has not that
// shape. The session is the reason's last tab
// field only when it is a session id (MissedSessionID), so a line written
// before the field existed, its reason holding a tab, keeps its reason whole.
func parseMissedLine(line string) (Fault, bool) {
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) != 3 {
		return Fault{}, false
	}
	seconds, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return Fault{}, false
	}
	reason, session := fields[2], ""
	if cut := strings.LastIndexByte(reason, '\t'); cut >= 0 && MissedSessionID(reason[cut+1:]) {
		reason, session = reason[:cut], reason[cut+1:]
	}
	stage := StageBinary
	if strings.HasPrefix(reason, TerminatedReason) || strings.HasPrefix(reason, StoreUnavailableReason) ||
		reason == PanicReason || strings.HasPrefix(reason, PanicReason+" (") {
		stage = StageTerminated // the binary's own line: a signal, a busy or an unavailable store, or a panic cut its run short
	}
	return Fault{TS: seconds * 1000, SessionID: session, Stage: stage, Error: fields[1] + ": " + reason}, true
}

// MissedSessionID reports whether s can be the session_id field of a
// missed.log line: non-empty, only ASCII letters, digits, `.`, `_` and `-`,
// the alphabet the wrapper and the hook binary write (a Claude Code session
// id is a UUID).
func MissedSessionID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
