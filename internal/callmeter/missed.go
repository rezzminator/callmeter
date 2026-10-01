package callmeter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/rezzminator/callmeter/internal/clock"
)

// ingestInfix names the file IngestMissed claims: {missed.log}.ingest-{pid}.
const ingestInfix = ".ingest-"

// unparsedLimit is how many bytes of a malformed missed.log line its fault keeps.
const unparsedLimit = 200

// IngestMissed turns the wrapper's missed.log into `binary` faults and
// returns how many it wrote. Each line is `{unix seconds}\t{event}\t{reason}`
// and becomes a fault stamped seconds × 1000 whose error is `{event}: {reason}`;
// a line that does not parse becomes a fault `unparsed missed.log line: …` of
// its first 200 bytes, never a dropped line.
//
// The file is renamed to {path}.ingest-{pid} first, so the wrapper's next
// append starts a new missed.log, and every older {path}.ingest-* a crashed
// run left is ingested with it. Hooks run in parallel, so a claim is ingested
// only by the run holding its exclusive flock: a claim another live run holds
// is left to that run, and a claim whose run died has no lock left. All the
// faults go in one transaction and the claimed files are removed after its
// commit, under their locks: a failed commit leaves them for the next run. A
// path with no file is 0, nil.
func (s *Store) IngestMissed(ctx context.Context, path string) (n int, err error) {
	if err := claimMissed(path); err != nil {
		return 0, err
	}
	candidates, err := ingestFiles(path)
	if err != nil {
		return 0, err
	}
	var files []string
	var faults []Fault
	var locks []*os.File
	defer func() {
		for _, held := range locks {
			if closeErr := held.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("callmeter store %s: release %s: %w", s.path, held.Name(), closeErr))
			}
		}
	}()
	for _, file := range candidates {
		held, data, err := lockClaim(file)
		if err != nil {
			return 0, fmt.Errorf("callmeter store %s: %w", s.path, err)
		}
		if held == nil {
			continue
		}
		locks = append(locks, held)
		files = append(files, file)
		faults = append(faults, parseMissed(data, clock.Real.Now().UnixMilli())...)
	}
	if len(files) == 0 {
		return 0, nil
	}
	if err := s.Batch(ctx, func(tx *Tx) error {
		for _, fault := range faults {
			if err := tx.AddFault(ctx, fault); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("callmeter store %s: ingest %d missed.log faults: %w", s.path, len(faults), err)
	}
	var removeErr error
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("callmeter store %s: remove ingested %s: %w", s.path, file, err))
		}
	}
	return len(faults), removeErr
}

// claimMissed renames path to a file nothing appends to; an absent path is not
// an error. An existing claim of the same pid (a crashed run's, its pid reused)
// is never overwritten: the new claim takes a numbered name beside it.
func claimMissed(path string) error {
	target := path + ingestInfix + strconv.Itoa(os.Getpid())
	for n := 1; ; n++ {
		_, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("callmeter missed log: look for %s: %w", target, err)
		}
		target = fmt.Sprintf("%s%s%d-%d", path, ingestInfix, os.Getpid(), n)
	}
	if err := os.Rename(path, target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("callmeter missed log: claim %s as %s: %w", path, target, err)
	}
	return nil
}

// lockClaim opens a claimed file and takes its exclusive flock without
// waiting, returning the open file, which holds the lock, and its content. A
// nil file and nil error mean the claim is not this run's: another run holds
// its lock, or took it, ingested it and removed or renamed it in between.
func lockClaim(file string) (*os.File, []byte, error) {
	held, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		// Gone since the listing: another run ingested it. A name that is
		// still there (a dangling link) is unreadable, never absent.
		if _, lstatErr := os.Lstat(file); errors.Is(lstatErr, os.ErrNotExist) {
			return nil, nil, nil
		}
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", file, err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := held.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if closeErr != nil {
				return nil, nil, fmt.Errorf("close %s held by another run: %w", file, closeErr)
			}
			return nil, nil, nil
		}
		return nil, nil, errors.Join(fmt.Errorf("lock %s: %w", file, err), closeErr)
	}
	// The lock is on the inode opened; the name must still point at it, or a
	// run that held the lock before us has already ingested and removed it.
	opened, statErr := held.Stat()
	named, lstatErr := os.Lstat(file)
	if errors.Is(lstatErr, os.ErrNotExist) || (statErr == nil && lstatErr == nil && !os.SameFile(opened, named)) {
		if err := held.Close(); err != nil {
			return nil, nil, fmt.Errorf("close %s already ingested: %w", file, err)
		}
		return nil, nil, nil
	}
	if err := errors.Join(statErr, lstatErr); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("stat %s: %w", file, err), held.Close())
	}
	data, err := io.ReadAll(held)
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("read %s: %w", file, err), held.Close())
	}
	return held, data, nil
}

// ingestFiles lists {path}.ingest-* in name order.
func ingestFiles(path string) ([]string, error) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+ingestInfix
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

// parseMissed turns the lines of one missed.log into faults; now stamps a
// line that carries no time of its own.
func parseMissed(data []byte, now int64) []Fault {
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
			TS:    now,
			Stage: StageBinary,
			Error: "unparsed missed.log line: " + strings.ToValidUTF8(string(kept), ""),
		})
	}
	return faults
}

// parseMissedLine reads `{unix seconds}\t{event}\t{reason}`; false when the
// line has not that shape.
func parseMissedLine(line string) (Fault, bool) {
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) != 3 {
		return Fault{}, false
	}
	seconds, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return Fault{}, false
	}
	return Fault{TS: seconds * 1000, Stage: StageBinary, Error: fields[1] + ": " + fields[2]}, true
}
