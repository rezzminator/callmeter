package callmeter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

const ledgerSession = "00000000-0000-4000-8000-0000000000a1"

// sampleRequestID is the sample's request id of its n-th row.
func sampleRequestID(n int) string { return fmt.Sprintf("req_011CFixture%08d", n) }

// ledgerEnv is a store, a ledger directory and the transcripts of a session
// the sample's calls ran in.
type ledgerEnv struct {
	t      *testing.T
	store  *Store
	dir    string
	proj   string
	now    time.Time
	sample []string // the sample's lines
}

// testRetention is the window the ledger and recovery tests give their store:
// the store keeps rows forever until SetRetention says otherwise.
const testRetention = 30 * 24 * time.Hour

func newLedgerEnv(t *testing.T) *ledgerEnv {
	t.Helper()
	root := t.TempDir()
	raw, err := os.ReadFile("testdata/ledger/sample-15.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	e := &ledgerEnv{
		t: t, store: openTestStore(t), dir: filepath.Join(root, "cost-ledger"), proj: filepath.Join(root, "proj"),
		now:    time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC),
		sample: strings.Split(strings.TrimSpace(string(raw)), "\n"),
	}
	e.store.SetRetention(testRetention)
	for _, dir := range []string{e.dir, e.proj} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *ledgerEnv) writeLedger(session string, lines ...string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// line is a sample row n with the given fields replaced.
func (e *ledgerEnv) line(n int, replace map[string]any) string {
	e.t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(e.sample[n-1]), &fields); err != nil {
		e.t.Fatal(err)
	}
	for k, v := range replace {
		fields[k] = v
	}
	out, err := json.Marshal(fields)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(out)
}

// transcribe writes a transcript of the session holding the given requests
// (sample row numbers) as assistant entries, the main chat's when agent is "",
// else that sub-agent's, and records the session's transcript path.
func (e *ledgerEnv) transcribe(agent string, rows ...int) {
	e.t.Helper()
	main := filepath.Join(e.proj, ledgerSession+".jsonl")
	path := main
	if agent != "" {
		path = SubagentTranscriptPath(main, agent)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			e.t.Fatal(err)
		}
	}
	var b strings.Builder
	for _, n := range rows {
		fmt.Fprintf(&b, `{"type":"assistant","requestId":%q,"timestamp":"2026-10-09T18:27:28.025Z","message":{"id":"msg_invented_%d","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`+"\n",
			sampleRequestID(n), n)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.DB().Exec(`INSERT INTO sessions (session_id, transcript_path) VALUES (?, ?)
		ON CONFLICT(session_id) DO UPDATE SET transcript_path = excluded.transcript_path`, ledgerSession, main); err != nil {
		e.t.Fatal(err)
	}
}

// storeRequest is what the hooks wrote for sample row n: its msg id row, priced
// from the table, with the requestId when the binary that wrote it stored one.
func (e *ledgerEnv) storeRequest(n int, withAPIRequestID bool) {
	e.t.Helper()
	r := Request{
		RequestID: fmt.Sprintf("msg_invented_%d", n), SessionID: Ptr(ledgerSession), TS: Ptr(e.now.Add(-time.Hour).UnixMilli()),
		Model: Ptr("claude-opus-5-5"), InputTokens: Ptr(int64(1)), OutputTokens: Ptr(int64(1)), Pending: Ptr(false),
		Source: Ptr(SourceHook),
	}
	if withAPIRequestID {
		r.APIRequestID = Ptr(sampleRequestID(n))
	}
	if err := e.store.UpsertRequest(context.Background(), r, Overwrite); err != nil {
		e.t.Fatal(err)
	}
}

func (e *ledgerEnv) ingest() LedgerSummary {
	e.t.Helper()
	got, err := e.store.IngestLedger(context.Background(), e.dir, e.now)
	if err != nil {
		e.t.Fatalf("IngestLedger: %v", err)
	}
	return got
}

func (e *ledgerEnv) sum(where string) (rows int, micros int64) {
	e.t.Helper()
	if err := e.store.DB().QueryRow("SELECT COUNT(*), COALESCE(SUM(cost_usd_micros), 0) FROM requests WHERE "+where).Scan(&rows, &micros); err != nil {
		e.t.Fatal(err)
	}
	return rows, micros
}

// TestIngestLedgerSideCallsAndHeldRows: the sample's 15 calls, seven of them
// recorded by a transcript (the main chat's and a sub-agent's, some stored with
// their requestId and some by an older binary without), end as 15 priced
// requests whose cost sums to the ledger's 1294100 micros: the eight calls no
// transcript holds ingested as 'ledger' rows, the seven held ones keeping their
// own row, priced by the ledger.
func TestIngestLedgerSideCallsAndHeldRows(t *testing.T) {
	e := newLedgerEnv(t)
	e.writeLedger(ledgerSession, e.sample...)
	e.transcribe("", 1, 2, 15)
	e.transcribe("a0000000000000002", 4, 6, 12, 14)
	for _, n := range []int{1, 2, 4, 6} {
		e.storeRequest(n, true)
	}
	for _, n := range []int{12, 14, 15} {
		e.storeRequest(n, false)
	}
	got := e.ingest()
	if !got.Present || got.Files != 1 || got.Rows != 15 || got.Faults != 0 || got.Held != 7 || got.Side != 8 || got.Unreadable != 0 {
		t.Fatalf("summary = %+v, want 1 file, 15 rows, 0 faults, 7 held, 8 side, 0 unreadable", got)
	}
	if n, micros := e.sum("1 = 1"); n != 15 || micros != 1294100 {
		t.Errorf("requests = %d rows, %d micros; want 15 rows, 1294100 micros", n, micros)
	}
	if n, micros := e.sum("source = 'ledger'"); n != 8 || micros != 1294100-149149-160066-347298-46463-18088-375586-31036 {
		t.Errorf("ledger rows = %d, %d micros; want the 8 side calls", n, micros)
	}
	if n, _ := e.sum("cost_source = 'ledger'"); n != 15 {
		t.Errorf("rows priced by the ledger = %d, want 15", n)
	}
	held := row(t, e.store, "requests", "request_id = 'msg_invented_12'")
	if held["cost_usd_micros"] != int64(18088) || held["api_request_id"] != sampleRequestID(12) || held["query_source"] != "agent:builtin:general-purpose" {
		t.Errorf("held request = cost %v api %v query %v, want 18088, its requestId and its query source",
			held["cost_usd_micros"], held["api_request_id"], held["query_source"])
	}
	side := row(t, e.store, "requests", "request_id = ?", sampleRequestID(7))
	for column, want := range map[string]any{
		"source": "ledger", "session_id": ledgerSession, "agent_id": "a0000000000000001", "query_source": "web_search_tool",
		"model": "claude-opus-5-5", "input_tokens": int64(12409), "output_tokens": int64(716), "cache_read_tokens": int64(0),
		"cache_creation_tokens": int64(0), "context_tokens": int64(12409), "cost_usd_micros": int64(73956), "cost_source": "ledger",
		"api_request_id": sampleRequestID(7), "pending": int64(0),
	} {
		if side[column] != want {
			t.Errorf("side call %s = %v, want %v", column, side[column], want)
		}
	}
	if side["ts"] != time.Date(2026, 10, 9, 18, 27, 40, 395_000_000, time.UTC).UnixMilli() {
		t.Errorf("side call ts = %v, want the ledger's 18:27:40.395Z", side["ts"])
	}
	if main := row(t, e.store, "requests", "request_id = ?", sampleRequestID(9)); main == nil {
		t.Errorf("side call 9 not stored")
	}
}

// TestIngestLedgerIsIdempotent: a second run over the same ledger changes
// nothing, a run after the prune of the window changes nothing, and neither
// double-counts a side call.
func TestIngestLedgerIsIdempotent(t *testing.T) {
	e := newLedgerEnv(t)
	e.writeLedger(ledgerSession, e.sample...)
	e.transcribe("", 1, 2, 15)
	e.storeRequest(1, true)
	first := e.ingest()
	var before string
	snapshot := func() string {
		return keys(t, e.store.DB(), "SELECT request_id || '|' || COALESCE(agent_id, '-') || '|' || COALESCE(cost_usd_micros, -1) || '|' || COALESCE(cost_source, '-') || '|' || COALESCE(api_request_id, '-') FROM requests ORDER BY request_id")
	}
	before = snapshot()
	if first.Held != 3 || first.HeldUnrecorded != 2 || first.Side != 12 {
		t.Errorf("first summary = %+v, want 3 held (2 not stored yet) and 12 side calls", first)
	}
	second := e.ingest()
	if second != first {
		t.Errorf("second summary = %+v, want %+v", second, first)
	}
	if after := snapshot(); after != before {
		t.Errorf("second run changed the store:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := e.store.Prune(context.Background(), e.now.Add(-testRetention)); err != nil {
		t.Fatal(err)
	}
	e.ingest()
	if after := snapshot(); after != before {
		t.Errorf("a run after the prune changed the store:\nbefore %s\nafter  %s", before, after)
	}
	// Rows 2 and 15 are held by the transcript but not stored yet: they are no
	// request until a hook writes them, and no side call.
	if n, micros := e.sum("1 = 1"); n != 13 || micros != 1294100-160066-31036 {
		t.Errorf("requests = %d rows, %d micros; want 13 rows, %d micros", n, micros, 1294100-160066-31036)
	}
}

// TestIngestLedgerMergesATranscriptLineThatArrivesLate: a call the ledger holds
// before any transcript does is a side call; once its transcript request is
// stored, the next run drops the ledger row and prices the transcript's own
// row, so the call is counted once.
func TestIngestLedgerMergesATranscriptLineThatArrivesLate(t *testing.T) {
	e := newLedgerEnv(t)
	e.writeLedger(ledgerSession, e.sample[:3]...)
	e.transcribe("", 1, 2)
	e.storeRequest(1, true)
	e.storeRequest(2, true)
	got := e.ingest()
	if got.Side != 1 || got.Held != 2 {
		t.Fatalf("summary = %+v, want 2 held and 1 side (row 3)", got)
	}
	if n, micros := e.sum("1 = 1"); n != 3 || micros != 149149+160066+2118 {
		t.Fatalf("requests = %d, %d micros; want 3 rows, 311333", n, micros)
	}
	// Row 3's transcript line is written and a hook stores it.
	e.transcribe("", 1, 2, 3)
	e.storeRequest(3, true)
	got = e.ingest()
	if got.Side != 0 || got.Held != 3 {
		t.Errorf("summary = %+v, want 3 held and no side call", got)
	}
	if n, micros := e.sum("1 = 1"); n != 3 || micros != 149149+160066+2118 {
		t.Errorf("requests = %d, %d micros; want the same 3 rows and total, nothing counted twice", n, micros)
	}
	if n, _ := e.sum("source = 'ledger'"); n != 0 {
		t.Errorf("ledger rows = %d, want 0: the transcript row holds the call", n)
	}
	if twin := row(t, e.store, "requests", "request_id = 'msg_invented_3'"); twin["cost_source"] != "ledger" || twin["cost_usd_micros"] != int64(2118) {
		t.Errorf("transcript row = %v / %v, want 2118 from the ledger", twin["cost_usd_micros"], twin["cost_source"])
	}
}

// TestIngestLedgerNamesEveryGap: a torn or foreign line is a counted fault and
// never an error or a smaller number; a duplicate request id keeps the best
// join; a half-joined row (no cost yet) is priced from the table until its
// upgraded line lands; a log-only row keeps its unknown agent.
func TestIngestLedgerNamesEveryGap(t *testing.T) {
	e := newLedgerEnv(t)
	spanOnly := e.line(7, map[string]any{"cost_usd_micros": nil, "join": "span"})
	upgraded := e.line(7, nil)
	logOnly := e.line(9, map[string]any{"agent_id": "?", "parent_agent_id": "", "join": "log"})
	e.writeLedger(ledgerSession,
		e.sample[0],
		spanOnly, // row 7 span-only first ...
		`{"request_id":"req_torn","ts":"2026-10-09T18:2`,                 // a torn line
		`["not","an","object"]`,                                          // not an object
		`{"request_id":"","ts":"2026-10-09T18:27:40.395Z","join":"log"}`, // no request id
		logOnly,
		e.line(1, map[string]any{"join": "log", "cost_usd_micros": 1}), // worse join of row 1: ignored
	)
	got := e.ingest()
	if got.Faults != 3 || got.Rows != 3 || got.Side != 3 {
		t.Fatalf("summary = %+v, want 3 faults, 3 rows (1, 7, 9) and 3 side calls", got)
	}
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(1)); r["cost_usd_micros"] != int64(149149) || r["agent_id"] != nil {
		t.Errorf("row 1 = cost %v agent %v, want 149149 and no agent (the main chat): the log+span line beat the log line",
			r["cost_usd_micros"], r["agent_id"])
	}
	span := row(t, e.store, "requests", "request_id = ?", sampleRequestID(7))
	// 12409 in, 716 out at Opus 5.5 list: 49636 + 14320 micros, short of the ledger's 73956 by the search fee.
	if span["cost_source"] != "table" || span["cost_usd_micros"] != int64(63956) {
		t.Errorf("span-only row = %v from %v, want 63956 from the table", span["cost_usd_micros"], span["cost_source"])
	}
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(9)); r["agent_id"] != "?" {
		t.Errorf("log-only row agent = %v, want ?", r["agent_id"])
	}
	// The upgraded line lands later in the same file.
	e.writeLedger(ledgerSession, e.sample[0], spanOnly, upgraded, logOnly)
	e.ingest()
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(7)); r["cost_source"] != "ledger" || r["cost_usd_micros"] != int64(73956) {
		t.Errorf("upgraded row = %v from %v, want 73956 from the ledger", r["cost_usd_micros"], r["cost_source"])
	}
}

// TestIngestLedgerWithoutALedger: no directory is "no ledger", never an error
// or a change; a path that is not a directory is an error.
func TestIngestLedgerWithoutALedger(t *testing.T) {
	e := newLedgerEnv(t)
	e.storeRequest(1, true)
	got, err := e.store.IngestLedger(context.Background(), filepath.Join(e.dir, "absent"), e.now)
	if err != nil || got.Present || got.Rows != 0 {
		t.Errorf("IngestLedger of an absent directory = %+v, %v; want not present, no error", got, err)
	}
	got, err = e.store.IngestLedger(context.Background(), "", e.now)
	if err != nil || got.Present {
		t.Errorf("IngestLedger of no directory = %+v, %v; want not present, no error", got, err)
	}
	file := filepath.Join(e.proj, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.IngestLedger(context.Background(), file, e.now); err == nil {
		t.Errorf("IngestLedger of a file = nil error, want one: an unreadable ledger is never read as none")
	}
	if n := count(t, e.store, "requests"); n != 1 {
		t.Errorf("requests = %d, want the 1 stored request untouched", n)
	}
}

// TestIngestLedgerOnlyTakesTheRetentionWindow: a row older than the store's
// retention is not brought in (the prune would take it out again), and a
// session whose transcripts cannot be read has its rows counted as side calls
// and named.
func TestIngestLedgerOnlyTakesTheRetentionWindow(t *testing.T) {
	e := newLedgerEnv(t)
	old := e.now.Add(-testRetention - time.Hour).Format("2006-01-02T15:04:05.000Z")
	e.writeLedger(ledgerSession, e.line(1, map[string]any{"ts": old}), e.sample[2])
	got := e.ingest()
	if got.Rows != 1 || got.Side != 1 || got.Unreadable != 1 {
		t.Errorf("summary = %+v, want 1 row in the window, 1 side call, 1 session unreadable (no transcript known)", got)
	}
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(1)); r != nil {
		t.Errorf("row older than retention stored: %v", r)
	}
}

// TestIngestLedgerWithoutARetentionTakesEveryRow: a store that keeps rows
// forever brings in a row of any age, since no prune will take it out again.
func TestIngestLedgerWithoutARetentionTakesEveryRow(t *testing.T) {
	e := newLedgerEnv(t)
	e.store.SetRetention(0)
	old := e.now.Add(-400 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	e.writeLedger(ledgerSession, e.line(1, map[string]any{"ts": old}), e.sample[2])
	got := e.ingest()
	if got.Rows != 2 || got.Side != 2 {
		t.Errorf("summary = %+v, want both rows read, the 400-day-old one included, as side calls", got)
	}
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(1)); r == nil {
		t.Error("the 400-day-old row was not stored in a store that keeps every row")
	}
}

// TestIngestLedgerStoresOnlyTheContractFields: a ledger line carrying a field
// the contract does not list (here an identity one) leaves it nowhere in the
// store.
func TestIngestLedgerStoresOnlyTheContractFields(t *testing.T) {
	e := newLedgerEnv(t)
	e.writeLedger(ledgerSession, e.line(3, map[string]any{"user_email": "invented identity value", "prompt": "invented prompt text"}))
	e.ingest()
	dump := keys(t, e.store.DB(), "SELECT COALESCE(request_id,'') || COALESCE(session_id,'') || COALESCE(agent_id,'') || COALESCE(query_source,'') || COALESCE(model,'') FROM requests")
	for _, leaked := range []string{"invented identity value", "invented prompt text"} {
		if strings.Contains(dump, leaked) {
			t.Errorf("store holds %q from a ledger line", leaked)
		}
	}
}

// TestPriceUnpricedPricesLegacyRows: a request stored before the price columns
// existed gets its table price; a row of an unknown model stays unpriced and is
// counted.
func TestPriceUnpricedPricesLegacyRows(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for _, stmt := range []string{
		`INSERT INTO requests (request_id, model, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens, pending)
			VALUES ('msg_legacy', 'claude-opus-5-5', 2, 323, 76447, 15924, 0, 15924, 0)`,
		`INSERT INTO requests (request_id, model, input_tokens, output_tokens, pending) VALUES ('msg_unknown', '<synthetic>', 0, 0, 0)`,
		`INSERT INTO requests (request_id, model, pending) VALUES ('msg_provisional', 'claude-opus-5-5', 1)`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	priced, unpriced, err := store.PriceUnpriced(ctx)
	if err != nil || priced != 1 || unpriced != 1 {
		t.Fatalf("PriceUnpriced = %d priced, %d unpriced, %v; want 1, 1, nil", priced, unpriced, err)
	}
	if r := row(t, store, "requests", "request_id = 'msg_legacy'"); r["cost_usd_micros"] != int64(149149) || r["cost_source"] != "table" {
		t.Errorf("legacy row = %v from %v, want 149149 from the table", r["cost_usd_micros"], r["cost_source"])
	}
	if r := row(t, store, "requests", "request_id = 'msg_unknown'"); r["cost_usd_micros"] != nil || r["cost_source"] != nil {
		t.Errorf("unknown-model row = %v / %v, want NULL / NULL", r["cost_usd_micros"], r["cost_source"])
	}
}

// holdAll stores every sample request a transcript holds, with its requestId:
// the main chat's 1, 2 and 15 and the sub-agent's 4, 6, 12 and 14. The other
// eight calls are side calls. A run over this store reads the same under the
// report's whole-directory ingest and the hooks' per-session one.
func (e *ledgerEnv) holdAll() {
	e.t.Helper()
	e.transcribe("", 1, 2, 15)
	e.transcribe("a0000000000000002", 4, 6, 12, 14)
	for _, n := range []int{1, 2, 4, 6, 12, 14, 15} {
		e.storeRequest(n, true)
	}
}

func (e *ledgerEnv) ingestSession(limit int64) LedgerSessionSummary {
	e.t.Helper()
	got, err := e.store.ingestLedgerSession(context.Background(), e.dir, ledgerSession, e.now, limit)
	if err != nil {
		e.t.Fatalf("ingestLedgerSession: %v", err)
	}
	return got
}

func (e *ledgerEnv) requestsDump() string {
	e.t.Helper()
	return keys(e.t, e.store.DB(), `SELECT request_id || '|' || COALESCE(session_id, '-') || '|' || COALESCE(agent_id, '-') || '|' || COALESCE(source, '-') || '|' ||
		COALESCE(cost_usd_micros, -1) || '|' || COALESCE(cost_source, '-') || '|' || COALESCE(api_request_id, '-') || '|' ||
		COALESCE(query_source, '-') || '|' || COALESCE(input_tokens, -1) FROM requests ORDER BY request_id`)
}

func (e *ledgerEnv) offset() int64 {
	e.t.Helper()
	var offset *int64
	if err := e.store.DB().QueryRow("SELECT ledger_offset FROM sessions WHERE session_id = ?", ledgerSession).Scan(&offset); err != nil {
		e.t.Fatal(err)
	}
	if offset == nil {
		return -1
	}
	return *offset
}

func (e *ledgerEnv) append(lines ...string) {
	e.t.Helper()
	f, err := os.OpenFile(filepath.Join(e.dir, ledgerSession+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
}

func byteLen(lines ...string) int64 {
	var n int64
	for _, l := range lines {
		n += int64(len(l)) + 1
	}
	return n
}

// TestIngestLedgerSessionReadsOnlyNewBytesAndMatchesTheReportRun: the hooks'
// ingest of one session's file, run as the file grows, leaves the store as the
// report's whole-directory ingest of the same file does; it keeps its byte
// offset in the store, reads nothing when no byte is new, and is idempotent
// with the report run in either order.
func TestIngestLedgerSessionReadsOnlyNewBytesAndMatchesTheReportRun(t *testing.T) {
	hooks := newLedgerEnv(t)
	hooks.holdAll()
	report := newLedgerEnv(t)
	report.holdAll()

	hooks.writeLedger(ledgerSession, hooks.sample[:8]...)
	report.writeLedger(ledgerSession, report.sample[:8]...)
	first := hooks.ingestSession(LedgerReadCap)
	report.ingest()
	if first.Lines != 8 || first.Bytes != byteLen(hooks.sample[:8]...) || first.Faults != 0 || first.More {
		t.Errorf("first run = %+v, want 8 lines of %d bytes, no faults, nothing more", first, byteLen(hooks.sample[:8]...))
	}
	if got := hooks.offset(); got != byteLen(hooks.sample[:8]...) {
		t.Errorf("offset after the first run = %d, want %d", got, byteLen(hooks.sample[:8]...))
	}
	if h, r := hooks.requestsDump(), report.requestsDump(); h != r {
		t.Errorf("after 8 lines the hooks' ingest differs from the report's:\nhooks  %s\nreport %s", h, r)
	}

	hooks.append(hooks.sample[8:]...)
	report.append(report.sample[8:]...)
	second := hooks.ingestSession(LedgerReadCap)
	report.ingest()
	if second.Lines != 7 || second.Bytes != byteLen(hooks.sample[8:]...) {
		t.Errorf("second run = %+v, want only the 7 new lines (%d bytes)", second, byteLen(hooks.sample[8:]...))
	}
	if h, r := hooks.requestsDump(), report.requestsDump(); h != r {
		t.Errorf("after 15 lines the hooks' ingest differs from the report's:\nhooks  %s\nreport %s", h, r)
	}
	if n, micros := hooks.sum("1 = 1"); n != 15 || micros != 1294100 {
		t.Errorf("requests = %d rows, %d micros; want the sample's 15 calls and 1294100 micros", n, micros)
	}

	before := hooks.requestsDump()
	third := hooks.ingestSession(LedgerReadCap)
	if third.Bytes != 0 || third.Lines != 0 || hooks.requestsDump() != before {
		t.Errorf("a run with no new byte = %+v, want nothing read and nothing changed", third)
	}
	// Idempotent with the report run, in this order and in the other.
	hooks.ingest()
	if after := hooks.requestsDump(); after != before {
		t.Errorf("the report run after the hooks' changed the store:\nbefore %s\nafter  %s", before, after)
	}
	other := newLedgerEnv(t)
	other.holdAll()
	other.writeLedger(ledgerSession, other.sample...)
	other.ingest()
	settled := other.requestsDump()
	other.ingestSession(LedgerReadCap)
	if after := other.requestsDump(); after != settled {
		t.Errorf("the hooks' run after the report's changed the store:\nbefore %s\nafter  %s", settled, after)
	}
}

// TestIngestLedgerSessionDefersATornLastLine: a line the ledger writer has not
// finished stays unread, the offset stopping before it; the next run reads it
// whole.
func TestIngestLedgerSessionDefersATornLastLine(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	torn := e.sample[3][:100]
	if err := os.WriteFile(filepath.Join(e.dir, ledgerSession+".jsonl"),
		[]byte(strings.Join(e.sample[:3], "\n")+"\n"+torn), 0o600); err != nil {
		t.Fatal(err)
	}
	got := e.ingestSession(LedgerReadCap)
	if got.Lines != 3 || got.Faults != 0 || e.offset() != byteLen(e.sample[:3]...) {
		t.Errorf("run over a torn tail = %+v, offset %d; want 3 lines, no fault, offset %d", got, e.offset(), byteLen(e.sample[:3]...))
	}
	if n, _ := e.sum("1 = 1"); n != 7+1 {
		t.Errorf("requests = %d, want the 7 held plus side call 3", n)
	}
	// The next hook meets only the torn line again: it writes nothing, so it
	// takes no write lock beside the hook's own.
	changes := func() (n int64) {
		if err := e.store.DB().QueryRow("SELECT total_changes()").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := changes()
	if got = e.ingestSession(LedgerReadCap); got.Lines != 0 || changes() != before {
		t.Errorf("run over the torn tail alone = %+v with %d rows written, want nothing read and nothing written", got, changes()-before)
	}
	e.append(e.sample[3][100:]) // the writer finishes line 4
	got = e.ingestSession(LedgerReadCap)
	if got.Lines != 1 || got.Faults != 0 || e.offset() != byteLen(e.sample[:4]...) {
		t.Errorf("run after the line completed = %+v, offset %d; want 1 line, no fault, offset %d", got, e.offset(), byteLen(e.sample[:4]...))
	}
}

// TestIngestLedgerSessionCapsEveryRead: a run reads at most its cap; the rest
// of a larger file waits for the next run, and the runs together end where the
// report run does (a line longer than the cap:
// TestIngestLedgerSessionPassesALineLongerThanItsCap).
func TestIngestLedgerSessionCapsEveryRead(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	report := newLedgerEnv(t)
	report.holdAll()
	e.writeLedger(ledgerSession, e.sample...)
	report.writeLedger(ledgerSession, report.sample...)
	report.ingest()
	limit := byteLen(e.sample[:3]...) + 10 // three lines and a part of the fourth
	runs := 0
	for {
		got := e.ingestSession(limit)
		runs++
		if got.Bytes > limit {
			t.Fatalf("run %d read %d bytes, over its cap of %d", runs, got.Bytes, limit)
		}
		if !got.More {
			break
		}
		if runs > 15 {
			t.Fatal("the offset does not advance")
		}
	}
	if runs < 5 {
		t.Errorf("a %d-byte file read in %d runs of %d bytes, want at least 5", byteLen(e.sample...), runs, limit)
	}
	if h, r := e.requestsDump(), report.requestsDump(); h != r {
		t.Errorf("capped runs differ from the report's:\nhooks  %s\nreport %s", h, r)
	}
}

// TestIngestLedgerSessionLedgerIsOptionalData: no directory, no file, a file
// that shrank, a path that is no regular file and a session id that is no file
// name are never a smaller number, a hang or a read outside the directory.
func TestIngestLedgerSessionLedgerIsOptionalData(t *testing.T) {
	ctx := context.Background()
	e := newLedgerEnv(t)
	e.holdAll()
	for _, dir := range []string{"", filepath.Join(e.dir, "absent")} {
		if got, err := e.store.IngestLedgerSession(ctx, dir, ledgerSession, e.now); err != nil || got.Present {
			t.Errorf("IngestLedgerSession(%q) = %+v, %v; want no ledger, no error", dir, got, err)
		}
	}
	if got, err := e.store.IngestLedgerSession(ctx, e.dir, ledgerSession, e.now); err != nil || got.Present {
		t.Errorf("a session with no file = %+v, %v; want no ledger, no error", got, err)
	}
	for _, bad := range []string{"", ".", "..", "../" + ledgerSession, "a/b", strings.Repeat("a", 200)} {
		if _, err := e.store.IngestLedgerSession(ctx, e.dir, bad, e.now); err == nil {
			t.Errorf("session id %q was read as a file name", bad)
		}
	}

	// A named pipe where the file should be: refused before it can block a hook.
	fifo := filepath.Join(e.dir, "pipe-session.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.dir, "dir-session.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"pipe-session", "dir-session"} {
		done := make(chan error, 1)
		go func() {
			_, err := e.store.IngestLedgerSession(ctx, e.dir, session, e.now)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a path that is no regular file was read", session)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the ingest blocked on a path that is no regular file", session)
		}
	}

	// A file that shrank below the offset is read again from its start.
	e.writeLedger(ledgerSession, e.sample[:5]...)
	e.ingestSession(LedgerReadCap)
	e.writeLedger(ledgerSession, e.sample[:2]...)
	before := e.requestsDump()
	got := e.ingestSession(LedgerReadCap)
	if !got.Reset || got.Lines != 2 || e.offset() != byteLen(e.sample[:2]...) || e.requestsDump() != before {
		t.Errorf("run after the file shrank = %+v, offset %d; want a reset, 2 lines read again, offset %d, nothing changed",
			got, e.offset(), byteLen(e.sample[:2]...))
	}
}

// TestLedgerReadsRefuseAPathThatIsNoRegularFile: the report run's ledger read
// and the transcript scan open a file out of a directory or a path the store
// names; a pipe there is refused with an error before it can stall a run.
func TestLedgerReadsRefuseAPathThatIsNoRegularFile(t *testing.T) {
	e := newLedgerEnv(t)
	fifo := filepath.Join(e.dir, "pipe-session.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	// A reader blocked in open(2) is released by a writer, so a failing run
	// leaks no goroutine.
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			f.Close()
		}
	})
	runs := []struct {
		name string
		run  func() error
	}{
		{"the report run's ledger read", func() error {
			_, err := e.store.IngestLedger(context.Background(), e.dir, e.now)
			return err
		}},
		{"the transcript scan", func() error {
			_, err := scanRequestIDs(fifo)
			return err
		}},
	}
	for _, tc := range runs {
		done := make(chan error, 1)
		go func() { done <- tc.run() }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
				t.Errorf("%s: err = %v; want a refusal naming a path that is no regular file", tc.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: blocked on a pipe", tc.name)
		}
	}
}

// TestIngestLedgerSessionCountsBadLinesAndKeepsALedgerPrice: a line that is no
// JSON object is a counted fault the offset passes; a worse line of a call
// whose ledger price is stored never takes it back to the table's.
func TestIngestLedgerSessionCountsBadLinesAndKeepsALedgerPrice(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	e.writeLedger(ledgerSession, e.sample[6], `["not","an","object"]`, `{"request_id":"req_torn"`)
	got := e.ingestSession(LedgerReadCap)
	if got.Faults != 2 || got.Lines != 3 || e.offset() != byteLen(e.sample[6], `["not","an","object"]`, `{"request_id":"req_torn"`) {
		t.Errorf("run = %+v, offset %d; want 2 faults over 3 lines and the offset past them", got, e.offset())
	}
	e.append(e.line(7, map[string]any{"cost_usd_micros": nil, "join": "span"}))
	e.ingestSession(LedgerReadCap)
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(7)); r["cost_source"] != "ledger" || r["cost_usd_micros"] != int64(73956) {
		t.Errorf("row 7 = %v from %v after a span-only line, want 73956 from the ledger", r["cost_usd_micros"], r["cost_source"])
	}
}

// TestIngestLedgerSessionNeedsTheSessionsRow: the offset lives on the session's
// row; a session the store does not know is left alone, not half-read.
func TestIngestLedgerSessionNeedsTheSessionsRow(t *testing.T) {
	e := newLedgerEnv(t)
	e.writeLedger(ledgerSession, e.sample[:3]...)
	got := e.ingestSession(LedgerReadCap)
	if got.Lines != 0 || count(t, e.store, "requests") != 0 {
		t.Errorf("run with no sessions row = %+v with %d requests, want nothing read", got, count(t, e.store, "requests"))
	}
}

// TestIngestLedgerSessionRequestWrittenAfterItsLedgerLine: a ledger line read
// before its transcript request was written is stored as a side call row; the
// request, written next, takes the row's price and replaces it, so the call
// counts once.
func TestIngestLedgerSessionRequestWrittenAfterItsLedgerLine(t *testing.T) {
	e := newLedgerEnv(t)
	e.transcribe("", 15)
	e.writeLedger(ledgerSession, e.sample[14])
	e.ingestSession(LedgerReadCap)
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(15)); r == nil || r["source"] != "ledger" {
		t.Fatalf("the unmatched line is no side call row: %v", r)
	}
	e.storeRequest(15, true)
	if n, micros := e.sum("1 = 1"); n != 1 || micros != 31036 {
		t.Errorf("after the request: %d rows, %d micros; want one row at the ledger's 31036", n, micros)
	}
	if r := row(t, e.store, "requests", "request_id = 'msg_invented_15'"); r["cost_source"] != "ledger" || r["query_source"] == nil {
		t.Errorf("request 15 = %v from %v, query %v; want the ledger's price and query source", r["cost_usd_micros"], r["cost_source"], r["query_source"])
	}
	before := e.requestsDump()
	e.ingest() // the report run finds nothing to change
	if after := e.requestsDump(); after != before {
		t.Errorf("the report run changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

// sideRow parses sample row n as a ledger row of the test session.
func (e *ledgerEnv) sideRow(n int) ledgerRow {
	e.t.Helper()
	r, ok := parseLedgerLine([]byte(e.sample[n-1]), ledgerSession)
	if !ok {
		e.t.Fatalf("sample row %d does not parse", n)
	}
	return r
}

// applySide runs the write half of an ingest over side rows only.
func (e *ledgerEnv) applySide(rows ...ledgerRow) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.store.Batch(ctx, func(tx *Tx) error { return tx.applyLedger(ctx, nil, nil, rows) }); err != nil {
		e.t.Fatalf("applyLedger: %v", err)
	}
}

// TestApplyLedgerWritesNoSideRowBesideATranscriptRequest: a report run decides
// which calls are side calls before it takes the write lock, so a hook can give
// a transcript request the call in between. The side upsert decides again under
// the lock: no 'ledger' row is written (inserted or updated) while a request of
// another source holds the call's requestId, so the call counts once.
func TestApplyLedgerWritesNoSideRowBesideATranscriptRequest(t *testing.T) {
	e := newLedgerEnv(t)
	e.transcribe("", 15)
	e.storeRequest(15, true) // a hook wrote the transcript request, requestId and all
	held := row(t, e.store, "requests", "request_id = 'msg_invented_15'")

	e.applySide(e.sideRow(15), e.sideRow(7))
	if n, _ := e.sum("source = 'ledger' AND api_request_id = '" + sampleRequestID(15) + "'"); n != 0 {
		t.Errorf("a side row was written beside the transcript request holding %s: %d rows", sampleRequestID(15), n)
	}
	if after := row(t, e.store, "requests", "request_id = 'msg_invented_15'"); fmt.Sprint(after) != fmt.Sprint(held) {
		t.Errorf("the transcript request changed:\nbefore %v\nafter  %v", held, after)
	}
	// A call no transcript request holds is still a side call.
	if n, micros := e.sum("source = 'ledger'"); n != 1 || micros != 73956 {
		t.Errorf("side rows = %d at %d micros, want only call 7 at 73956", n, micros)
	}

	// A ledger row left beside a transcript request is not brought up to date
	// either: the next report run's overlay drops it.
	if _, err := e.store.DB().Exec(`INSERT INTO requests (request_id, session_id, ts, source, api_request_id, cost_usd_micros, cost_source)
		VALUES (?1, ?2, 1, ?3, ?1, 1, ?3)`, sampleRequestID(15), ledgerSession, SourceLedger); err != nil {
		t.Fatal(err)
	}
	e.applySide(e.sideRow(15))
	if r := row(t, e.store, "requests", "request_id = ?", sampleRequestID(15)); r["cost_usd_micros"] != int64(1) {
		t.Errorf("the left-over ledger row was updated to %v, want it untouched at 1", r["cost_usd_micros"])
	}
	e.writeLedger(ledgerSession, e.sample[14])
	e.ingest()
	if n, _ := e.sum("source = 'ledger' AND api_request_id = '" + sampleRequestID(15) + "'"); n != 0 {
		t.Errorf("the report run left %d ledger rows beside the transcript request", n)
	}
}

// TestIngestLedgerHoldsASideCallAHookStoredInBetween: a report run decides its
// side calls before it takes the write lock; a call a hook gave a transcript
// request in between is decided again under the lock, so it is counted held,
// not side, and that request takes the ledger's price, as on the hook path.
func TestIngestLedgerHoldsASideCallAHookStoredInBetween(t *testing.T) {
	e := newLedgerEnv(t)
	e.transcribe("", 15)
	e.storeRequest(15, true) // the hook, after the report read which calls the store holds
	sum := LedgerSummary{Held: 4}
	ctx := context.Background()
	if err := e.store.Batch(ctx, func(tx *Tx) error {
		return tx.applyReportLedger(ctx, nil, nil, []ledgerRow{e.sideRow(15), e.sideRow(7)}, &sum)
	}); err != nil {
		t.Fatal(err)
	}
	if sum.Side != 1 || sum.Held != 5 {
		t.Errorf("summary side %d, held %d; want 1 side call (7) and call 15 held on top of 4", sum.Side, sum.Held)
	}
	if n, _ := e.sum("source = 'ledger' AND api_request_id = '" + sampleRequestID(15) + "'"); n != 0 {
		t.Errorf("%d ledger rows beside the transcript request holding %s, want none", n, sampleRequestID(15))
	}
	want := *e.sideRow(15).Cost
	if r := row(t, e.store, "requests", "request_id = 'msg_invented_15'"); r["cost_source"] != SourceLedger || r["cost_usd_micros"] != want {
		t.Errorf("the transcript request = %v from %v, want the ledger's %d", r["cost_usd_micros"], r["cost_source"], want)
	}
}

// TestIngestLedgerSessionReadsAReplacedFileFromItsStart: the stored offset is
// the end of a complete line, so the byte before it is a newline; when it is
// not (the file was replaced by one that has grown past the offset) the file is
// read from its start, and the store ends as an ingest of that file fresh.
func TestIngestLedgerSessionReadsAReplacedFileFromItsStart(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	e.writeLedger(ledgerSession, e.sample[:3]...)
	e.ingestSession(LedgerReadCap)
	old := e.offset()

	var replacement []string
	for n := 1; n <= 8; n++ {
		replacement = append(replacement, e.line(n, map[string]any{"cost_usd_micros": 5000 + n, "query_source": "replaced"}))
	}
	e.writeLedger(ledgerSession, replacement...)
	raw, err := os.ReadFile(filepath.Join(e.dir, ledgerSession+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) <= old || raw[old-1] == '\n' {
		t.Fatalf("the replacement must be longer than the old offset %d and have no line end before it", old)
	}
	got := e.ingestSession(LedgerReadCap)
	if !got.Reset || got.Lines != 8 || got.Faults != 0 || e.offset() != int64(len(raw)) {
		t.Errorf("run over a replaced file = %+v, offset %d; want a reset, 8 lines, no fault, offset %d", got, e.offset(), len(raw))
	}
	fresh := newLedgerEnv(t)
	fresh.holdAll()
	fresh.writeLedger(ledgerSession, replacement...)
	fresh.ingestSession(LedgerReadCap)
	if h, f := e.requestsDump(), fresh.requestsDump(); h != f {
		t.Errorf("a replaced file differs from the same file ingested fresh:\nreplaced %s\nfresh    %s", h, f)
	}
}

// TestIngestLedgerSessionKeepsAResetThatReadNoLine: a file replaced by an empty
// one or by a torn line (shorter than the old offset, or longer with no line end
// before it) is a reset that moves the stored offset to 0 even though the run
// read no complete line, so the file, grown later, is read from its first line.
func TestIngestLedgerSessionKeepsAResetThatReadNoLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content func(e *ledgerEnv, old int64) string
	}{
		{"an empty file", func(*ledgerEnv, int64) string { return "" }},
		{"a torn line shorter than the offset", func(e *ledgerEnv, _ int64) string { return e.sample[3][:100] }},
		{"a torn line longer than the offset", func(e *ledgerEnv, old int64) string {
			return strings.Repeat("x", int(old)+50)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLedgerEnv(t)
			e.holdAll()
			e.writeLedger(ledgerSession, e.sample[:5]...)
			e.ingestSession(LedgerReadCap)
			old := e.offset()
			path := filepath.Join(e.dir, ledgerSession+".jsonl")
			if err := os.WriteFile(path, []byte(tc.content(e, old)), 0o600); err != nil {
				t.Fatal(err)
			}
			got := e.ingestSession(LedgerReadCap)
			if !got.Reset || got.Lines != 0 || e.offset() != 0 {
				t.Errorf("run over %s = %+v, offset %d; want a reset, no line, offset 0 kept", tc.name, got, e.offset())
			}
			// The writer goes on: the file grows past the old offset.
			e.writeLedger(ledgerSession, e.sample[:8]...)
			got = e.ingestSession(LedgerReadCap)
			if got.Reset || got.Lines != 8 || e.offset() != byteLen(e.sample[:8]...) {
				t.Errorf("run after the file grew = %+v, offset %d; want its 8 lines from the first, offset %d", got, e.offset(), byteLen(e.sample[:8]...))
			}
			fresh := newLedgerEnv(t)
			fresh.holdAll()
			fresh.writeLedger(ledgerSession, fresh.sample[:8]...)
			fresh.ingestSession(LedgerReadCap)
			if h, f := e.requestsDump(), fresh.requestsDump(); h != f {
				t.Errorf("after %s the store differs from an ingest of the file fresh:\nhooks %s\nfresh %s", tc.name, h, f)
			}
		})
	}
}

// TestIngestLedgerSessionWaitsForTheLockNoLongerThanItsDeadline: the store's
// busy wait ignores a ctx deadline, so a hook ingesting under one (SessionEnd's
// budget) waits for a busy store's write lock only until that deadline, never
// the whole busy wait; the offset stays, and the next run reads the bytes.
func TestIngestLedgerSessionWaitsForTheLockNoLongerThanItsDeadline(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	e.writeLedger(ledgerSession, e.sample[:3]...)
	probe, err := sqlitedb.OpenReadWrite(e.store.path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err := probe.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	const budget = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	_, err = e.store.IngestLedgerSession(ctx, e.dir, ledgerSession, e.now)
	if waited := time.Since(start); waited > budget+BusyTimeout/2 || err == nil {
		t.Errorf("an ingest at a busy store with a %v deadline returned %v after %v, want an error by its deadline", budget, err, waited)
	}
	if _, err := probe.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if e.offset() != -1 {
		t.Errorf("offset after the busy run = %d, want it unmoved (-1: NULL)", e.offset())
	}
	if got := e.ingestSession(LedgerReadCap); got.Lines != 3 || e.offset() != byteLen(e.sample[:3]...) {
		t.Errorf("the next run = %+v, offset %d; want the 3 lines read", got, e.offset())
	}
}

// TestIngestLedgerSessionPassesALineLongerThanItsCap: a line longer than the cap
// is counted and passed to the end of that line, so the offset still stands at a
// line end and the next run does not take it for a replaced file; a long line
// whose end has not been written yet waits like any torn line.
func TestIngestLedgerSessionPassesALineLongerThanItsCap(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	path := filepath.Join(e.dir, ledgerSession+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 3000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.ingestSession(1000); got.Lines != 0 || got.Faults != 0 || e.offset() != -1 {
		t.Errorf("a long line without its end = %+v, offset %d; want it to wait, nothing counted, the offset unmoved (-1: NULL)", got, e.offset())
	}
	// The scan for the line's end stops when its run's ctx ends (a SessionEnd
	// budget), however far the line runs.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, found, err := lineEndAfter(ended, path, 1000, 3000); !errors.Is(err, context.Canceled) || found {
		t.Errorf("a scan under an ended ctx = found %v, err %v; want ctx's error", found, err)
	}
	e.append("") // the writer ends the line
	want := int64(3001)
	if got := e.ingestSession(1000); got.Faults != 1 || got.Reset || got.More || e.offset() != want {
		t.Errorf("the long line ended = %+v, offset %d; want one fault, nothing more, and the offset %d past its end", got, e.offset(), want)
	}
	e.append(e.sample[6])
	got := e.ingestSession(1000)
	if got.Reset || got.Lines != 1 || got.Faults != 0 || e.offset() != want+byteLen(e.sample[6]) {
		t.Errorf("the line after it = %+v, offset %d; want it read, no reset, offset %d", got, e.offset(), want+byteLen(e.sample[6]))
	}
}

// TestLedgerLinesPastTheBoundsAreFaults: a count above 1e12 or a price above 1e15
// micros is no ledger line, so no sum of a row's counts can wrap and no price
// leaves the range of an int64; a value at the bound parses. Both ingest paths
// count the line in Faults and store nothing.
func TestLedgerLinesPastTheBoundsAreFaults(t *testing.T) {
	const bigCount, bigMicros = int64(9_000_000_000_000_000_000), int64(9_000_000_000_000_000_000)
	for _, tc := range []struct {
		field string
		value int64
		ok    bool
	}{
		{"input_tokens", bigCount, false}, {"input_tokens", 1_000_000_000_001, false}, {"input_tokens", 1_000_000_000_000, true},
		{"output_tokens", bigCount, false}, {"output_tokens", 1_000_000_000_001, false}, {"output_tokens", 1_000_000_000_000, true},
		{"cache_read_tokens", bigCount, false}, {"cache_read_tokens", 1_000_000_000_001, false}, {"cache_read_tokens", 1_000_000_000_000, true},
		{"cache_creation_tokens", bigCount, false}, {"cache_creation_tokens", 1_000_000_000_001, false}, {"cache_creation_tokens", 1_000_000_000_000, true},
		{"cost_usd_micros", bigMicros, false}, {"cost_usd_micros", 1_000_000_000_000_001, false}, {"cost_usd_micros", 1_000_000_000_000_000, true},
	} {
		t.Run(fmt.Sprintf("%s=%d", tc.field, tc.value), func(t *testing.T) {
			wantRows, wantFaults := 1, 0
			if !tc.ok {
				wantRows, wantFaults = 0, 1
			}
			report := newLedgerEnv(t)
			line := report.line(7, map[string]any{tc.field: tc.value})
			if _, ok := parseLedgerLine([]byte(line), ledgerSession); ok != tc.ok {
				t.Errorf("parseLedgerLine ok = %v, want %v", ok, tc.ok)
			}
			report.writeLedger(ledgerSession, line)
			sum := report.ingest()
			if n, _ := report.sum("source = 'ledger'"); sum.Faults != wantFaults || n != wantRows {
				t.Errorf("report run: %d faults, %d rows; want %d, %d", sum.Faults, n, wantFaults, wantRows)
			}
			hooks := newLedgerEnv(t)
			hooks.holdAll()
			hooks.writeLedger(ledgerSession, line)
			got := hooks.ingestSession(LedgerReadCap)
			if n, _ := hooks.sum("source = 'ledger'"); got.Faults != wantFaults || n != wantRows || got.Lines != 1 {
				t.Errorf("hook run = %+v with %d side rows; want %d faults, %d rows from 1 line", got, n, wantFaults, wantRows)
			}
		})
	}
}

// writeBigLedger writes a ledger of about size bytes: sample row 7's line
// repeated under distinct request ids.
func (e *ledgerEnv) writeBigLedger(size int64) {
	e.t.Helper()
	file, err := os.Create(filepath.Join(e.dir, ledgerSession+".jsonl"))
	if err != nil {
		e.t.Fatal(err)
	}
	w := bufio.NewWriterSize(file, 1<<20)
	template := e.sample[6]
	for n, written := 0, int64(0); written < size; n++ {
		line := strings.Replace(template, sampleRequestID(7), fmt.Sprintf("req_big%012d", n), 1) + "\n"
		if _, err := w.WriteString(line); err != nil {
			e.t.Fatal(err)
		}
		written += int64(len(line))
	}
	if err := errors.Join(w.Flush(), file.Close()); err != nil {
		e.t.Fatal(err)
	}
}

// TestIngestLedgerSessionStaysWithinItsCapOnAHugeLedger: a hook meeting a
// ledger far larger than its cap reads the cap and no more, and a run with no
// new byte costs a stat and a lookup, nowhere near a read.
func TestIngestLedgerSessionStaysWithinItsCapOnAHugeLedger(t *testing.T) {
	e := newLedgerEnv(t)
	e.holdAll()
	e.writeBigLedger(24 << 20)
	got := e.ingestSession(LedgerReadCap)
	if got.Bytes > LedgerReadCap || !got.More || got.Lines < 1000 {
		t.Fatalf("run over a 24 MiB ledger = %+v, want at most %d bytes read, more waiting", got, LedgerReadCap)
	}
	// Catch the offset up to the end, then time the hook with no new byte.
	for e.ingestSession(LedgerReadCap).More {
	}
	const runs = 200
	began := time.Now()
	for range runs {
		if again := e.ingestSession(LedgerReadCap); again.Bytes != 0 || again.Lines != 0 {
			t.Fatalf("a run with no new byte = %+v", again)
		}
	}
	if per := time.Since(began) / runs; per > 5*time.Millisecond {
		t.Errorf("a hook's ingest with no new byte took %v, want a few milliseconds at most", per)
	}
}

func BenchmarkIngestLedgerSessionNoNewBytes(b *testing.B) {
	root := b.TempDir()
	store, err := OpenDB(context.Background(), filepath.Join(root, "callmeter.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	dir := filepath.Join(root, "ledger")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/ledger/sample-15.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ledgerSession+".jsonl"), raw, 0o600); err != nil {
		b.Fatal(err)
	}
	if _, err := store.DB().Exec("INSERT INTO sessions (session_id) VALUES (?)", ledgerSession); err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC)
	if _, err := store.IngestLedgerSession(context.Background(), dir, ledgerSession, now); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := store.IngestLedgerSession(context.Background(), dir, ledgerSession, now); err != nil {
			b.Fatal(err)
		}
	}
}
