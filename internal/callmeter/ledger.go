package callmeter

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
)

// The cost ledger is another tool's optional data: a directory of
// `{session_id}.jsonl` files, one JSON line per API call, each with Claude
// Code's own price of the call. callmeter reads it and never writes there; a
// missing directory is "no ledger", never an error and never a smaller number.
// A call the ledger holds and no transcript records (a side call: a WebSearch
// or WebFetch helper, an agent summary, a prompt suggestion) becomes a requests
// row with source 'ledger'; a call a transcript records keeps its own row,
// priced by the ledger. docs/design.md § The cost ledger.

// ledgerFields are the keys of a ledger line callmeter reads (parseLedgerLine
// names each); a line's other keys are ignored and never stored. Every one but
// RequestID is nil when the line did not carry it in a form callmeter reads.
type ledgerFields struct {
	RequestID     *string
	TS            *string
	AgentID       *string
	QuerySource   *string
	Model         *string
	Input         *int64
	Output        *int64
	CacheRead     *int64
	CacheCreation *int64
	Cost          *int64
	Join          *string
}

// ledgerRow is one ledger line naming a request id: the file's session and the
// line's fields; at is zero when the line's ts is unknown.
type ledgerRow struct {
	ledgerFields
	id      string
	session string
	at      time.Time
	rank    int
	partial bool // a field of the line was unknown (parseLedgerLine)
}

// keeps reports whether r takes the place of prior, an earlier line of the same
// request id: a better join does, and at the same join a later line does unless
// it is partial and prior is not, so no unknown field takes a known one back.
func (r ledgerRow) keeps(prior ledgerRow) bool {
	return r.rank > prior.rank || r.rank == prior.rank && (!r.partial || prior.partial)
}

// inWindow reports whether r is inside a retention window starting at cutoff:
// at or after it, or of an unknown ts, which no window can judge.
func (r ledgerRow) inWindow(cutoff time.Time) bool { return r.at.IsZero() || !r.at.Before(cutoff) }

// UnknownLedgerAgent is the agent_id a ledger row carries when its log half
// arrived and its agent could not be learned.
const UnknownLedgerAgent = "?"

// The largest count and price a ledger line may carry. Real calls are far
// below them (a million-token context is 1e6; a 1e9 USD call is not a call),
// and they keep every sum and conversion of one line inside int64: the three
// counts of context_tokens add to at most 3e12, a count times the dearest rate
// per token stays near 1e14 micros, and no price reaches the range where
// PriceMicros's float64 to int64 conversion is implementation-defined. A sum
// over many lines is not bounded by them (about 9,000 lines at the price bound
// fill an int64). A value past either is unknown: the line is kept with that
// field NULL and counted in Partial.
const (
	maxLedgerTokens = 1_000_000_000_000
	maxLedgerMicros = 1_000_000_000_000_000
)

// joinRank orders the ledger's joins: the row with the best one wins a request
// id (a log+span line is the exact one).
func joinRank(join string) int {
	switch join {
	case "log+span":
		return 3
	case "log":
		return 2
	case "span":
		return 1
	}
	return 0
}

// LedgerSummary is what one IngestLedger run found, for the report to name.
type LedgerSummary struct {
	Dir     string // the directory read; "" when none could be resolved
	Present bool   // the directory exists
	// Err is why the ledger could not be read; set by the caller that ran the
	// ingest, which reports it and goes on.
	Err string
	// SchemaIncomplete: the store lacks the cost columns, so nothing was ingested.
	SchemaIncomplete bool
	Files            int // ledger files read
	Rows             int // distinct calls read; inside the retention window when one is set
	Faults           int // lines skipped: not a JSON object, or naming no request_id
	Partial          int // lines kept with a field unknown: absent, of another JSON type, past its bound, or a ts that does not parse
	FilesSkipped     int // ledger files that could not be read whole (stat, open or read failed, or no regular file): their unread calls are not counted
	Held             int // calls a transcript of their session records
	HeldUnrecorded   int // held calls the store has no request for yet: not counted until a hook or recovery writes it
	Side             int // calls no transcript records: ingested as source 'ledger' requests
	Unreadable       int // sessions whose transcripts could not be read whole (none named, a file unread, a requestId line not JSON): their unknown calls count as side calls
}

// IngestLedger brings the cost ledger in dir into the store, idempotently:
//  1. the requestIds of the transcripts of every session holding a ledger call the
//     store does not know are read, and a stored request is given its requestId;
//  2. a ledger row whose call a stored transcript request now holds is dropped;
//  3. each call no transcript holds is written as a 'ledger' request, at the
//     ledger's price (the table's when its cost has not arrived yet);
//  4. the ledger's price is copied onto the requests transcripts hold.
//
// With a retention set (SetRetention) only calls inside its window are read, so
// the prune never takes one out for the next run to bring back. dir "" and a
// missing dir are "no ledger"; a dir that cannot be read is an error.
func (s *Store) IngestLedger(ctx context.Context, dir string, now time.Time) (LedgerSummary, error) {
	sum := LedgerSummary{Dir: dir}
	if dir == "" {
		return sum, nil
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return sum, nil
	}
	if err != nil {
		return sum, fmt.Errorf("callmeter ledger: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return sum, fmt.Errorf("callmeter ledger: %s is not a directory", dir)
	}
	sum.Present = true
	if !s.complete {
		sum.SchemaIncomplete = true
		return sum, nil
	}
	cutoff := s.retentionFloor(now)
	rows, err := readLedger(dir, cutoff, &sum)
	if err != nil {
		return sum, err
	}
	sum.Rows = len(rows)
	if len(rows) == 0 {
		return sum, nil
	}
	known, err := s.knownAPIRequests(ctx)
	if err != nil {
		return sum, err
	}
	scanned, err := s.scanUnknownSessions(ctx, rows, known, &sum)
	if err != nil {
		return sum, err
	}
	var side []ledgerRow
	var overlay []ledgerRow
	var fills []ledgerFill
	for _, r := range rows {
		stored, isKnown := known[r.id]
		switch {
		case isKnown && stored.source != SourceLedger:
			sum.Held++
			overlay = append(overlay, r)
		case isKnown:
			if _, held := scanned.held[r.id]; held && scanned.twin(r.id) {
				sum.Held++
				overlay = append(overlay, r)
				fills = append(fills, ledgerFill{r.id, scanned.held[r.id]})
				continue
			}
			side = append(side, r)
		default:
			messageID, held := scanned.held[r.id]
			if !held {
				side = append(side, r)
				continue
			}
			sum.Held++
			if scanned.stored[messageID] {
				overlay = append(overlay, r)
				fills = append(fills, ledgerFill{r.id, messageID})
			} else {
				sum.HeldUnrecorded++
			}
		}
	}
	err = s.Batch(ctx, func(tx *Tx) error { return tx.applyReportLedger(ctx, fills, overlay, side, &sum) })
	return sum, err
}

// applyReportLedger is a report run's applyLedger. Its side calls were decided
// before the write lock, so they are decided again under it: a call a request
// of another source holds by now (a hook wrote it in between) is held, priced
// like any held call, and counted so; sum's Side counts the rest.
func (t *Tx) applyReportLedger(ctx context.Context, fills []ledgerFill, overlay, side []ledgerRow, sum *LedgerSummary) error {
	known, err := t.knownLedgerRequests(ctx, side)
	if err != nil {
		return err
	}
	stillSide := side[:0:0]
	for _, r := range side {
		if stored, isKnown := known[r.id]; isKnown && stored.source != SourceLedger {
			sum.Held++
			overlay = append(overlay, r)
			continue
		}
		stillSide = append(stillSide, r)
	}
	sum.Side = len(stillSide)
	return t.applyLedger(ctx, fills, overlay, stillSide)
}

// ledgerFill gives the stored transcript request messageID the requestId apiID.
type ledgerFill struct{ apiID, messageID string }

// applyLedger writes what an ingest decided, in the order the report run
// settled on: the requestIds of the transcript requests that lack theirs, the
// ledger price (and the drop of a ledger row a transcript request now holds)
// for each held call, and each side call as a 'ledger' request.
func (t *Tx) applyLedger(ctx context.Context, fills []ledgerFill, overlay, side []ledgerRow) error {
	for _, f := range fills {
		if _, err := t.tx.ExecContext(ctx,
			`UPDATE requests SET api_request_id = ?1 WHERE request_id = ?2 AND api_request_id IS NULL AND source IS NOT ?3`,
			f.apiID, f.messageID, SourceLedger); err != nil {
			return fmt.Errorf("callmeter store %s: give request %q its requestId: %w", t.path, f.messageID, err)
		}
	}
	for _, r := range overlay {
		if _, err := t.tx.ExecContext(ctx,
			`DELETE FROM requests WHERE request_id = ?1 AND source = ?2
			AND EXISTS (SELECT 1 FROM requests t WHERE t.api_request_id = ?1 AND t.source IS NOT ?2)`,
			r.id, SourceLedger); err != nil {
			return fmt.Errorf("callmeter store %s: drop ledger row %q held by a transcript: %w", t.path, r.id, err)
		}
		if r.Cost == nil {
			continue
		}
		if _, err := t.tx.ExecContext(ctx,
			`UPDATE requests SET cost_usd_micros = ?1, cost_source = ?2, query_source = ?3
			WHERE api_request_id = ?4 AND source IS NOT ?2
			AND (cost_source IS NOT ?2 OR cost_usd_micros IS NOT ?1 OR query_source IS NOT ?3)`,
			*r.Cost, SourceLedger, nullablePtr(r.QuerySource), r.id); err != nil {
			return fmt.Errorf("callmeter store %s: price request of %q from the ledger: %w", t.path, r.id, err)
		}
	}
	for _, r := range side {
		if err := t.upsertSideCall(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// nullablePtr is an optional string as a bound value: NULL when it is absent or empty.
func nullablePtr(s *string) any {
	if s == nil {
		return nil
	}
	return nullable(*s)
}

// upsertSideCall writes a ledger row no transcript holds as a requests row,
// keyed by its request id; a row already there (a source 'ledger' one) is
// brought up to date, any other row is left alone. The caller decided that no
// transcript holds the call before it took the write lock, so the statement
// decides again under it: nothing is written (inserted or updated) while a
// request of another source holds the call's requestId, the guard the overlay
// DELETE of applyLedger shares, so a hook that gave a transcript request the
// call in between leaves it counted once.
func (t *Tx) upsertSideCall(ctx context.Context, r ledgerRow) error {
	var agent any
	if r.AgentID != nil && *r.AgentID != "" {
		agent = *r.AgentID
	}
	model := ""
	if r.Model != nil {
		model = *r.Model
	}
	// A field the line did not carry readably is NULL, and so is what needs it.
	counted := r.Input != nil && r.Output != nil && r.CacheRead != nil && r.CacheCreation != nil
	var cost, source, contextTokens, ts any
	switch {
	case r.Cost != nil:
		cost, source = *r.Cost, SourceLedger
	case counted:
		if micros, ok := PriceMicros(model, *r.Input, *r.Output, *r.CacheRead, *r.CacheCreation, nil, nil); ok {
			cost, source = micros, CostSourceTable
		}
	}
	if r.Input != nil && r.CacheRead != nil && r.CacheCreation != nil {
		contextTokens = *r.Input + *r.CacheRead + *r.CacheCreation
	}
	if !r.at.IsZero() {
		ts = r.at.UnixMilli()
	}
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO requests (request_id, session_id, agent_id, ts, model, input_tokens, cache_read_tokens,
			cache_creation_tokens, context_tokens, output_tokens, pending, source, api_request_id, query_source,
			cost_usd_micros, cost_source)
		SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, 0, ?11, ?1, ?12, ?13, ?14
		WHERE NOT EXISTS (SELECT 1 FROM requests t WHERE t.api_request_id = ?1 AND t.source IS NOT ?11)
		ON CONFLICT(request_id) DO UPDATE SET session_id = excluded.session_id, agent_id = excluded.agent_id,
			ts = excluded.ts, model = excluded.model, input_tokens = excluded.input_tokens,
			cache_read_tokens = excluded.cache_read_tokens, cache_creation_tokens = excluded.cache_creation_tokens,
			context_tokens = excluded.context_tokens, output_tokens = excluded.output_tokens,
			query_source = excluded.query_source, cost_usd_micros = excluded.cost_usd_micros,
			cost_source = excluded.cost_source
		WHERE requests.source = ?11
		AND (requests.session_id IS NOT excluded.session_id OR requests.agent_id IS NOT excluded.agent_id
			OR requests.ts IS NOT excluded.ts OR requests.model IS NOT excluded.model
			OR requests.input_tokens IS NOT excluded.input_tokens OR requests.cache_read_tokens IS NOT excluded.cache_read_tokens
			OR requests.cache_creation_tokens IS NOT excluded.cache_creation_tokens
			OR requests.output_tokens IS NOT excluded.output_tokens OR requests.query_source IS NOT excluded.query_source
			OR requests.cost_usd_micros IS NOT excluded.cost_usd_micros OR requests.cost_source IS NOT excluded.cost_source)`,
		r.id, r.session, agent, ts, nullablePtr(r.Model), r.Input, r.CacheRead, r.CacheCreation,
		contextTokens, r.Output, SourceLedger, nullablePtr(r.QuerySource), cost, source)
	if err != nil {
		return fmt.Errorf("callmeter store %s: write ledger request %q: %w", t.path, r.id, err)
	}
	return nil
}

// readLedger reads every ledger file of dir touched at or after cutoff and
// returns its distinct calls in the window (inWindow; all of them when cutoff
// is the zero time), the best join of each request id (a later line among
// equals), in a stable order. A line that is not a JSON object, or names no
// request id, is counted in sum.Faults; one kept with a field unknown in
// sum.Partial. A file that cannot be read whole is counted in
// sum.FilesSkipped, keeping the lines read before the failure; the other
// files are still read.
func readLedger(dir string, cutoff time.Time, sum *LedgerSummary) ([]ledgerRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("callmeter ledger: read %s: %w", dir, err)
	}
	best := map[string]ledgerRow{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			sum.FilesSkipped++
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		sum.Files++
		if err := readLedgerFile(path, strings.TrimSuffix(name, ".jsonl"), best, sum); err != nil {
			sum.FilesSkipped++ // the lines read before the failure stand
		}
	}
	rows := make([]ledgerRow, 0, len(best))
	for _, r := range best {
		if r.inWindow(cutoff) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].at.Equal(rows[j].at) {
			return rows[i].at.Before(rows[j].at)
		}
		return rows[i].id < rows[j].id
	})
	return rows, nil
}

func readLedgerFile(path, session string, best map[string]ledgerRow, sum *LedgerSummary) error {
	file, err := openRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			row, partial, ok := parseLedgerLine(line, session)
			switch {
			case !ok:
				sum.Faults++
			case partial:
				sum.Partial++
			}
			if prior, seen := best[row.id]; ok && (!seen || row.keeps(prior)) {
				best[row.id] = row
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("callmeter ledger: read %s: %w", path, err)
		}
	}
}

// parseLedgerLine reads one ledger line; ok is false only for a line that is
// not a JSON object or names no request_id, the one field a row is keyed by.
// Every other field is read on its own: one absent where the contract requires
// it (ts and the four counts), of another JSON type, or past its bound (a
// count above maxLedgerTokens, a price above maxLedgerMicros), and a ts that is
// no RFC 3339 time, is kept unknown (nil; at zero) and partial says so, the
// rest of the line standing.
func parseLedgerLine(line []byte, session string) (row ledgerRow, partial, ok bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &raw); err != nil || raw == nil {
		return ledgerRow{}, false, false
	}
	var f ledgerFields
	if f.RequestID = ledgerField[string](raw, "request_id", true, &partial); f.RequestID == nil || *f.RequestID == "" {
		return ledgerRow{}, false, false
	}
	bounded := func(key string, bound int64) *int64 {
		value := ledgerField[int64](raw, key, true, &partial)
		if value != nil && (*value < 0 || *value > bound) {
			partial = true
			return nil
		}
		return value
	}
	f.TS = ledgerField[string](raw, "ts", true, &partial)
	f.AgentID = ledgerField[string](raw, "agent_id", false, &partial)
	f.QuerySource = ledgerField[string](raw, "query_source", false, &partial)
	f.Model = ledgerField[string](raw, "model", false, &partial)
	f.Input = bounded("input_tokens", maxLedgerTokens)
	f.Output = bounded("output_tokens", maxLedgerTokens)
	f.CacheRead = bounded("cache_read_tokens", maxLedgerTokens)
	f.CacheCreation = bounded("cache_creation_tokens", maxLedgerTokens)
	f.Join = ledgerField[string](raw, "join", false, &partial)
	if f.Cost = ledgerField[int64](raw, "cost_usd_micros", false, &partial); f.Cost != nil && (*f.Cost < 0 || *f.Cost > maxLedgerMicros) {
		f.Cost, partial = nil, true
	}
	var at time.Time
	if f.TS != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *f.TS)
		if err != nil {
			partial = true
		} else {
			at = parsed
		}
	}
	join := ""
	if f.Join != nil {
		join = *f.Join
	}
	return ledgerRow{ledgerFields: f, id: *f.RequestID, session: session, at: at, rank: joinRank(join), partial: partial}, partial, true
}

// ledgerField is key of a ledger line's fields decoded as a T (decodeField):
// nil when absent, null or of another JSON type. bad is set for a value there
// that is not a T, and, when required, for an absent or null one.
func ledgerField[T any](raw map[string]json.RawMessage, key string, required bool, bad *bool) *T {
	var mistyped []error
	out := decodeField[*T](raw, key, &mistyped)
	if len(mistyped) > 0 || (out == nil && required) {
		*bad = true
	}
	return out
}

// storedAPIRequest is a stored request holding a requestId.
type storedAPIRequest struct {
	messageID string
	source    string
}

// knownAPIRequests maps each requestId the store holds to its request, a
// transcript's request before a ledger row's.
func (s *Store) knownAPIRequests(ctx context.Context) (map[string]storedAPIRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT api_request_id, request_id, COALESCE(source, '') FROM requests
		WHERE api_request_id IS NOT NULL AND api_request_id != ''`)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: list stored requestIds: %w", s.path, err)
	}
	known := map[string]storedAPIRequest{}
	for rows.Next() {
		var apiID string
		var stored storedAPIRequest
		if err := rows.Scan(&apiID, &stored.messageID, &stored.source); err != nil {
			return nil, errors.Join(fmt.Errorf("callmeter store %s: read stored requestId: %w", s.path, err), rows.Close())
		}
		if prior, ok := known[apiID]; !ok || prior.source == SourceLedger {
			known[apiID] = stored
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("callmeter store %s: list stored requestIds: %w", s.path, err)
	}
	return known, nil
}

// transcriptHold is what the transcripts of the sessions with unknown ledger
// calls hold: the requestIds they record, each with its message id, and which
// of those message ids the store has a request for.
type transcriptHold struct {
	held   map[string]string
	stored map[string]bool
}

// twin reports whether the transcript request holding requestID is in the store.
func (h transcriptHold) twin(requestID string) bool { return h.stored[h.held[requestID]] }

// scanUnknownSessions reads the transcripts of every session that holds a
// ledger call the store has no request for, and notes the sessions whose
// transcripts could not be read whole: no main transcript named, a file
// unread, or a line naming a requestId that is not JSON. What was read still holds its calls.
func (s *Store) scanUnknownSessions(ctx context.Context, rows []ledgerRow, known map[string]storedAPIRequest, sum *LedgerSummary) (transcriptHold, error) {
	hold := transcriptHold{held: map[string]string{}, stored: map[string]bool{}}
	sessions := map[string]bool{}
	for _, r := range rows {
		if _, isKnown := known[r.id]; !isKnown {
			sessions[r.session] = true
		} else if known[r.id].source == SourceLedger {
			sessions[r.session] = true
		}
	}
	ids := make([]string, 0, len(sessions))
	for session := range sessions {
		ids = append(ids, session)
	}
	sort.Strings(ids)
	for _, session := range ids {
		paths, err := s.sessionTranscripts(ctx, session)
		if err != nil {
			return hold, err
		}
		readable := paths.main != ""
		for _, path := range paths.files {
			found, whole, err := scanRequestIDs(path)
			switch {
			case err == nil:
				for apiID, messageID := range found {
					hold.held[apiID] = messageID
				}
				readable = readable && whole
			case errors.Is(err, fs.ErrNotExist) && path != paths.main:
				// A sub-agent that wrote no transcript holds no request.
			default:
				readable = false
			}
		}
		if !readable {
			sum.Unreadable++
		}
	}
	messageIDs := make([]string, 0, len(hold.held))
	for _, messageID := range hold.held {
		messageIDs = append(messageIDs, messageID)
	}
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		marks := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, 0, end-start)
		for _, id := range messageIDs[start:end] {
			args = append(args, id)
		}
		stored, err := s.db.QueryContext(ctx, "SELECT request_id FROM requests WHERE request_id IN ("+marks+")", args...)
		if err != nil {
			return hold, fmt.Errorf("callmeter store %s: find the requests of transcript messages: %w", s.path, err)
		}
		for stored.Next() {
			var id string
			if err := stored.Scan(&id); err != nil {
				return hold, errors.Join(fmt.Errorf("callmeter store %s: read a request id: %w", s.path, err), stored.Close())
			}
			hold.stored[id] = true
		}
		if err := errors.Join(stored.Err(), stored.Close()); err != nil {
			return hold, fmt.Errorf("callmeter store %s: find the requests of transcript messages: %w", s.path, err)
		}
	}
	return hold, nil
}

// transcriptFiles are the transcripts of one session: its main chat's and each
// sub-agent's the store or the disk names.
type transcriptFiles struct {
	main  string
	files []string
}

func (s *Store) sessionTranscripts(ctx context.Context, session string) (transcriptFiles, error) {
	var out transcriptFiles
	seen := map[string]bool{}
	add := func(path string) {
		if path != "" && !seen[path] {
			seen[path] = true
			out.files = append(out.files, path)
		}
	}
	var main sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT transcript_path FROM sessions WHERE session_id = ?", session).Scan(&main)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, fmt.Errorf("callmeter store %s: read the transcript of session %q: %w", s.path, session, err)
	}
	if main.String != "" {
		out.main = main.String
		add(main.String)
		for _, id := range SubagentTranscripts(main.String) {
			add(SubagentTranscriptPath(main.String, id))
		}
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT transcript_path FROM agents WHERE session_id = ? AND transcript_path IS NOT NULL AND transcript_path != ''", session)
	if err != nil {
		return out, fmt.Errorf("callmeter store %s: list the sub-agent transcripts of session %q: %w", s.path, session, err)
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return out, errors.Join(fmt.Errorf("callmeter store %s: read a sub-agent transcript path: %w", s.path, err), rows.Close())
		}
		add(path)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, fmt.Errorf("callmeter store %s: list the sub-agent transcripts of session %q: %w", s.path, session, err)
	}
	return out, nil
}

// scanRequestIDs reads a transcript for the requestId of each assistant message
// it records, mapped to the message's id. Nothing else of a line is kept.
// whole is false when a line naming a requestId is not JSON (a torn or
// malformed line): its call, if any, is not among found. A final line with no
// newline is a writer mid-append (readLines), never a torn one.
func scanRequestIDs(path string) (found map[string]string, whole bool, err error) {
	file, err := openRegular(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	found = map[string]string{}
	marker := []byte(`"requestId"`)
	bad, err := readLines(file, path, 0, false, func(line []byte) error {
		if !bytes.Contains(line, marker) {
			return nil
		}
		var entry struct {
			Type      string `json:"type"`
			RequestID string `json:"requestId"`
			Message   struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return err
		}
		if entry.Type == "assistant" && entry.RequestID != "" && entry.Message.ID != "" {
			found[entry.RequestID] = entry.Message.ID
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("callmeter ledger: %w", err)
	}
	return found, bad == nil, nil
}

// PriceUnpriced prices from the table every settled request that has no price
// yet (a row stored before the price columns existed), and counts the settled
// requests of a model the table does not list, which stay unpriced. It never
// prices a GPT row (neither counts it): the price is forward only, and every
// GPT request stored before the list named its models keeps its NULL.
func (s *Store) PriceUnpriced(ctx context.Context) (priced, unpriced int, err error) {
	if !s.complete {
		return 0, 0, nil
	}
	type unpricedRow struct {
		id                          string
		model                       string
		input, output, read, create int64
		write5m, write1h            sql.NullInt64
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id, model, input_tokens, output_tokens, COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
			cache_creation_5m_tokens, cache_creation_1h_tokens
		FROM requests WHERE cost_source IS NULL AND COALESCE(pending, 0) = 0 AND model IS NOT NULL AND model != '' AND lower(model) NOT LIKE 'gpt-%'
		AND input_tokens IS NOT NULL AND output_tokens IS NOT NULL`)
	if err != nil {
		return 0, 0, fmt.Errorf("callmeter store %s: list requests to price: %w", s.path, err)
	}
	var todo []unpricedRow
	for rows.Next() {
		var r unpricedRow
		if err := rows.Scan(&r.id, &r.model, &r.input, &r.output, &r.read, &r.create, &r.write5m, &r.write1h); err != nil {
			return 0, 0, errors.Join(fmt.Errorf("callmeter store %s: read a request to price: %w", s.path, err), rows.Close())
		}
		todo = append(todo, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, 0, fmt.Errorf("callmeter store %s: list requests to price: %w", s.path, err)
	}
	type price struct {
		id     string
		micros int64
	}
	var prices []price
	for _, r := range todo {
		var w5, w1 *int64
		if r.write5m.Valid {
			w5 = &r.write5m.Int64
		}
		if r.write1h.Valid {
			w1 = &r.write1h.Int64
		}
		if micros, ok := PriceMicros(r.model, r.input, r.output, r.read, r.create, w5, w1); ok {
			prices = append(prices, price{r.id, micros})
		} else {
			unpriced++
		}
	}
	if len(prices) == 0 {
		return 0, unpriced, nil
	}
	err = s.Batch(ctx, func(tx *Tx) error {
		for _, p := range prices {
			if _, err := tx.tx.ExecContext(ctx,
				"UPDATE requests SET cost_usd_micros = ?, cost_source = ? WHERE request_id = ? AND cost_source IS NULL",
				p.micros, CostSourceTable, p.id); err != nil {
				return fmt.Errorf("callmeter store %s: price request %q: %w", s.path, p.id, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, unpriced, err
	}
	return len(prices), unpriced, nil
}
