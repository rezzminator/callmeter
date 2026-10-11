package callmeter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
