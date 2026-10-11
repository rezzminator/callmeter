package callmeter

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The hooks bring the cost ledger in as a session settles, so a request carries
// Claude Code's own price when the next hook runs and not only at a report run:
// IngestLedgerSession reads the new bytes of one session's file from a byte
// offset the store keeps (sessions.ledger_offset), and does for those lines
// what IngestLedger does for the whole directory. docs/design.md § The cost
// ledger.

// LedgerReadCap bounds the bytes one IngestLedgerSession reads: the rest of a
// larger ledger file waits for the session's next hook.
const LedgerReadCap = 4 << 20

// maxLedgerSessionID bounds a session id used as a file name, as the ledger's
// writer bounds it.
const maxLedgerSessionID = 128

// errLedgerRaced says another hook of the session moved the offset first: its
// run read the same bytes, so this one leaves them.
var errLedgerRaced = errors.New("callmeter ledger: another run moved the offset")

// LedgerSessionSummary is what one IngestLedgerSession run found.
type LedgerSessionSummary struct {
	Present bool  // the session's ledger file exists
	Bytes   int64 // bytes read this run
	Offset  int64 // the offset kept after this run
	Lines   int   // complete lines read
	Faults  int   // lines skipped: not a JSON object, or missing a field the contract requires
	Held    int   // calls a stored transcript request holds: priced from the ledger
	Side    int   // calls no stored transcript request holds: ingested as source 'ledger' requests
	More    bool  // bytes remain beyond this run's cap
	Reset   bool  // the file was shorter than the kept offset (or the offset no file position): read again from its start
}

// validLedgerSessionID reports whether id may name a ledger file: 1..128
// characters of [A-Za-z0-9._-], neither "." nor "..". A session id is joined
// into a path, so it is checked first.
func validLedgerSessionID(id string) bool {
	if id == "" || len(id) > maxLedgerSessionID || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// IngestLedgerSession brings the new lines of session's file in dir into the
// store, idempotently and incrementally:
//  1. it reads at most LedgerReadCap bytes from the offset the session's row
//     keeps, only complete lines (a torn last line waits for the next run),
//     and only from a regular file;
//  2. a call a stored transcript request holds gives it the ledger's price and
//     query source; every other call is stored as a 'ledger' request, which the
//     transcript request, once written, takes over (Tx.priceRequest);
//  3. the offset moves past the lines read in the same transaction, so a run
//     that lost the race to another hook of the session writes nothing.
//
// dir "" and a missing dir or file are "no ledger"; a path that is no regular
// file, an id that is no file name or a file that cannot be read is an error
// the caller says as a fault: the ledger is optional data, never a failed hook.
// A session the store has no row for, and a store that has not gained its
// columns yet, are left alone.
func (s *Store) IngestLedgerSession(ctx context.Context, dir, session string, now time.Time) (LedgerSessionSummary, error) {
	return s.ingestLedgerSession(ctx, dir, session, now, LedgerReadCap)
}

func (s *Store) ingestLedgerSession(ctx context.Context, dir, session string, now time.Time, limit int64) (LedgerSessionSummary, error) {
	var sum LedgerSessionSummary
	if dir == "" {
		return sum, nil
	}
	if !validLedgerSessionID(session) {
		return sum, fmt.Errorf("callmeter ledger: session id %q is not 1..%d characters of [A-Za-z0-9._-]", session, maxLedgerSessionID)
	}
	path := filepath.Join(dir, session+".jsonl")
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return sum, nil
	}
	if err != nil {
		return sum, fmt.Errorf("callmeter ledger: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return sum, fmt.Errorf("callmeter ledger: %s is not a regular file", path)
	}
	sum.Present = true
	if !s.complete {
		return sum, nil
	}
	var kept sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT ledger_offset FROM sessions WHERE session_id = ?", session).Scan(&kept)
	if errors.Is(err, sql.ErrNoRows) {
		return sum, nil
	}
	if err != nil {
		return sum, fmt.Errorf("callmeter store %s: read the ledger offset of session %q: %w", s.path, session, err)
	}
	offset, size := kept.Int64, info.Size()
	if size < offset || offset < 0 {
		offset, sum.Reset = 0, true
	}
	sum.Offset = offset
	if size == offset && !sum.Reset {
		return sum, nil
	}
	data, err := readLedgerSlice(path, offset, min(size-offset, limit))
	if err != nil {
		return sum, err
	}
	sum.Bytes = int64(len(data))
	end := int64(bytes.LastIndexByte(data, '\n')) + 1
	if end == 0 && int64(len(data)) >= limit {
		// A line longer than the cap is no ledger line: counted, passed.
		end, sum.Faults = int64(len(data)), 1
		data = nil
	}
	sum.More = offset+end < size
	if end == 0 {
		// Only a torn line is new: nothing to apply and no offset to move, so
		// the hook takes no write lock.
		return sum, nil
	}
	best := map[string]ledgerRow{}
	for _, line := range bytes.Split(data[:min(end, int64(len(data)))], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		sum.Lines++
		row, ok := parseLedgerLine(line, session)
		if !ok {
			sum.Faults++
		} else if prior, seen := best[row.id]; !seen || row.rank >= prior.rank {
			best[row.id] = row
		}
	}
	cutoff := s.retentionFloor(now)
	rows := make([]ledgerRow, 0, len(best))
	for _, r := range best {
		if !r.at.Before(cutoff) {
			rows = append(rows, r)
		}
	}
	sum.Offset = offset + end
	err = s.Batch(ctx, func(tx *Tx) error {
		moved, err := tx.tx.ExecContext(ctx,
			"UPDATE sessions SET ledger_offset = ?1 WHERE session_id = ?2 AND COALESCE(ledger_offset, 0) IS ?3",
			sum.Offset, session, kept.Int64)
		if err != nil {
			return fmt.Errorf("callmeter store %s: keep the ledger offset of session %q: %w", s.path, session, err)
		}
		if n, err := moved.RowsAffected(); err != nil || n == 0 {
			return errors.Join(err, errLedgerRaced)
		}
		// Which calls the store holds is read under the write lock, so a report
		// run's ingest committing first cannot leave a side row beside the
		// transcript request that now holds the call.
		known, err := tx.knownLedgerRequests(ctx, rows)
		if err != nil {
			return err
		}
		var overlay, side []ledgerRow
		for _, r := range rows {
			stored, isKnown := known[r.id]
			switch {
			case isKnown && r.Cost == nil && stored.costSource == SourceLedger:
				// A half-joined line after the joined one never takes Claude Code's price back.
			case isKnown && stored.source != SourceLedger:
				sum.Held++
				overlay = append(overlay, r)
			default:
				side = append(side, r)
			}
		}
		sum.Side = len(side)
		return tx.applyLedger(ctx, nil, overlay, side)
	})
	if errors.Is(err, errLedgerRaced) {
		return LedgerSessionSummary{Present: true}, nil
	}
	return sum, err
}

// readLedgerSlice reads size bytes of the regular file at path from offset.
// The file is opened non-blocking and checked again once open, so a path
// swapped for a pipe after its stat cannot stall a hook; a file that shrank
// since the stat gives the bytes it still has.
func readLedgerSlice(path string, offset, size int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("callmeter ledger: open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("callmeter ledger: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("callmeter ledger: %s is not a regular file", path)
	}
	data := make([]byte, size)
	n, err := file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("callmeter ledger: read %s: %w", path, err)
	}
	return data[:n], nil
}

// knownLedgerRequest is a stored request holding a requestId a ledger line names.
type knownLedgerRequest struct {
	source     string
	costSource string
}

// knownLedgerRequests maps each requestId of rows the store holds to its
// request, a transcript's before a ledger row's (knownAPIRequests, but only
// for these ids, through the requestId index).
func (t *Tx) knownLedgerRequests(ctx context.Context, rows []ledgerRow) (map[string]knownLedgerRequest, error) {
	known := map[string]knownLedgerRequest{}
	for start := 0; start < len(rows); start += 500 {
		end := min(start+500, len(rows))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, r := range rows[start:end] {
			args = append(args, r.id)
		}
		found, err := t.tx.QueryContext(ctx,
			"SELECT api_request_id, COALESCE(source, ''), COALESCE(cost_source, '') FROM requests WHERE api_request_id IN ("+marks+")", args...)
		if err != nil {
			return nil, fmt.Errorf("callmeter store %s: find the requests of ledger calls: %w", t.path, err)
		}
		for found.Next() {
			var id string
			var stored knownLedgerRequest
			if err := found.Scan(&id, &stored.source, &stored.costSource); err != nil {
				return nil, errors.Join(fmt.Errorf("callmeter store %s: read a stored requestId: %w", t.path, err), found.Close())
			}
			if prior, ok := known[id]; !ok || prior.source == SourceLedger {
				known[id] = stored
			}
		}
		if err := errors.Join(found.Err(), found.Close()); err != nil {
			return nil, fmt.Errorf("callmeter store %s: find the requests of ledger calls: %w", t.path, err)
		}
	}
	return known, nil
}
