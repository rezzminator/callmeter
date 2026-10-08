package callmeter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenDB(context.Background(), filepath.Join(t.TempDir(), "state", "callmeter.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// row reads one row of table as column name -> value (nil for NULL).
func row(t *testing.T, store *Store, table, where string, args ...any) map[string]any {
	t.Helper()
	rows, err := store.DB().Query(fmt.Sprintf("SELECT * FROM %s WHERE %s", table, where), args...)
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close %s rows: %v", table, err)
		}
	}()
	names, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		return nil
	}
	values := make([]any, len(names))
	pointers := make([]any, len(names))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		t.Fatalf("scan %s: %v", table, err)
	}
	out := map[string]any{}
	for i, name := range names {
		out[name] = values[i]
	}
	return out
}

func count(t *testing.T, store *Store, table string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOpenSetsPragmasAndVersion(t *testing.T) {
	store := openTestStore(t)
	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"busy_timeout": "5000",
		"foreign_keys": "0",
		"user_version": "1",
	} {
		var got string
		if err := store.DB().QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "callmeter.db")
	store, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := store.DB().Exec("PRAGMA user_version=2"); err != nil {
		t.Fatalf("raise version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenDB(ctx, path)
	if err == nil {
		_ = reopened.Close()
		t.Fatal("OpenDB of a version-2 store succeeded, want a refusal")
	}
	for _, want := range []string{"version 2", "version 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	// The refusal left the store as it was: a newer binary still reads it.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	}()
	var version int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 2 {
		t.Fatalf("user_version after refusal = %d, want 2", version)
	}
}

// keys lists one column of a query's rows, in the query's order, as "a,b".
func keys(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	}()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return strings.Join(out, ",")
}

func TestOpenFreshStoreIsEmptyAtCurrentVersion(t *testing.T) {
	store := openTestStore(t)
	if got := keys(t, store.DB(), "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
	for table := range storeColumns {
		if n := count(t, store, table); n != 0 {
			t.Errorf("fresh %s holds %d rows, want 0", table, n)
		}
	}
}

// storeColumns is the on-disk schema a new store gets: every table with its
// columns, in order (docs/design.md § The store).
var storeColumns = map[string]string{
	"calls": "tool_use_id,session_id,agent_id,agent_type,request_id,prompt_id,ts,tool,input,cwd,duration_ms,failed," +
		"is_interrupt,error,bytes_real,bytes_delivered,persisted_path,file_path,file_bytes,file_bytes_before," +
		"read_start,read_lines,read_total_lines,effort,permission_mode,lines_added,lines_removed,commit_sha," +
		"commit_branch,test_runner,source,config_dir,seat_dir",
	"requests": "request_id,session_id,agent_id,prompt_id,ts,model,stop_reason,input_tokens,cache_read_tokens," +
		"cache_creation_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,context_tokens,output_tokens," +
		"calls,pending,source,config_dir,seat_dir,thinking_tokens",
	"agents": "agent_id,session_id,agent_type,prompt_id,parent_tool_use_id,started,stopped,transcript_path," +
		"total_tokens,tool_uses,model,source,config_dir,seat_dir",
	"agent_turns": "agent_id,seq,session_id,agent_type,prompt_id,started,stopped,start_event_id,stop_event_id",
	"turns": "event_id,event,session_id,agent_id,agent_type,prompt_id,ts,effort,permission_mode,background_tasks," +
		"session_crons,last_assistant_message_bytes,stop_hook_active,seat_dir",
	"events": "event_id,event,ts,session_id,agent_id,agent_type,prompt_id,effort,permission_mode,source,model," +
		"reason,trigger,error_type,load_reason,memory_type,file_path,tool_name,command_name,task_id,prompt_bytes," +
		"detail,seat_dir",
	"sessions": "session_id,first_ts,last_ts,engine,model,start_source,end_reason,cwd,transcript_path,seat_dir," +
		"config_dir,host,tz_name,tz_offset_minutes,cwd_ts,transcript_path_ts,seat_dir_ts,config_dir_ts,host_ts,tz_name_ts," +
		"tz_offset_minutes_ts",
	"command_parts": "tool_use_id,seq,lang,program,args,files,parse_status,conditional,parser",
	"faults":        "fault_id,ts,session_id,tool_use_id,stage,error",
	"compactions": "entry_id,session_id,agent_id,ts,trigger,pre_tokens,post_tokens,cumulative_dropped_tokens,duration_ms," +
		"seat_dir",
	"session_costs":  "session_id,ts,started,cost_usd,api_ms,api_no_retry_ms,tool_ms,wall_ms,model_costs,seat_dir",
	"stop_hooks":     "entry_id,session_id,agent_id,prompt_id,ts,hook_count,hook_errors,seat_dir",
	"stop_hook_runs": "entry_id,seq,ts,name,command_bytes,duration_ms",
	"turn_durations": "entry_id,session_id,agent_id,prompt_id,ts,duration_ms,message_count,background_agents,seat_dir",
	"request_iterations": "request_id,seq,ts,type,model,input_tokens,cache_read_tokens,cache_creation_tokens," +
		"cache_creation_5m_tokens,cache_creation_1h_tokens,output_tokens",
}

// TestOpenFreshStoreCreatesTheVersionOneTables pins the on-disk schema a new
// store gets: every table with its columns, in order, and every index.
func TestOpenFreshStoreCreatesTheVersionOneTables(t *testing.T) {
	store := openTestStore(t)
	for table, want := range storeColumns {
		got := keys(t, store.DB(), "SELECT name FROM pragma_table_info('"+table+"') ORDER BY cid")
		if got != want {
			t.Errorf("%s columns = %s, want %s", table, got, want)
		}
	}
	gotTables := keys(t, store.DB(), "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if want := "agent_turns,agents,calls,command_parts,compactions,events,faults,request_iterations,requests,session_costs," +
		"sessions,stop_hook_runs,stop_hooks,turn_durations,turns"; gotTables != want {
		t.Errorf("tables = %s, want %s", gotTables, want)
	}
	gotIndexes := keys(t, store.DB(),
		"SELECT name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	wantIndexes := "calls_file_path,calls_prompt,calls_request,calls_session_agent_ts,calls_ts,compactions_session_ts," +
		"events_event_ts,events_session_ts,requests_session_agent_pending,requests_session_agent_ts,stop_hooks_session_ts," +
		"turn_durations_session_ts,turns_session_ts"
	if gotIndexes != wantIndexes {
		t.Errorf("indexes = %s, want %s", gotIndexes, wantIndexes)
	}
	if got := keys(t, store.DB(), "SELECT name FROM sqlite_master WHERE type = 'view' ORDER BY name"); got != "request_cache" {
		t.Errorf("views = %s, want request_cache", got)
	}
	if got := keys(t, store.DB(), "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
}

// queryPlan is the detail lines of EXPLAIN QUERY PLAN of query, joined by "; ".
func queryPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close plan rows: %v", err)
		}
	}()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan plan of %q: %v", query, err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read plan of %q: %v", query, err)
	}
	return strings.Join(details, "; ")
}

// TestOpenAddsTheCallsRequestIndexToAVersionOneStore: a version-1 store created
// before calls_request existed gets it on the next open, and the version stays 1.
func TestOpenAddsTheCallsRequestIndexToAVersionOneStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	old, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := old.DB().Exec("DROP INDEX calls_request"); err != nil {
		t.Fatalf("drop the index: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	store := openStoreAt(t, path)
	if got := keys(t, store.DB(), "SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'calls_request'"); got != "calls_request" {
		t.Errorf("calls_request after a reopen = %q, want present", got)
	}
	plan := queryPlan(t, store.DB(), "SELECT COUNT(*) FROM calls WHERE request_id = 'x'")
	if !strings.Contains(plan, "calls_request") || strings.Contains(plan, "SCAN calls") {
		t.Errorf("the recount plan = %q, want a search by calls_request and no SCAN calls", plan)
	}
	if got := keys(t, store.DB(), "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
}

// TestCreateSchemaWaitsOutAConcurrentWriter pins the schema transaction's lock
// wait: a writer holding the lock while the schema is created is waited out by
// the busy timeout, never an immediate SQLITE_BUSY that would lose a hook's
// whole record.
func TestCreateSchemaWaitsOutAConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	writerDB, err := sqlitedb.OpenStore(ctx, path, BusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writerDB.Close(); err != nil {
			t.Errorf("close writer: %v", err)
		}
	})
	creatorDB, err := sqlitedb.OpenStore(ctx, path, BusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := creatorDB.Close(); err != nil {
			t.Errorf("close creator: %v", err)
		}
	})
	writer, err := writerDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Errorf("close writer connection: %v", err)
		}
	})
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ExecContext(ctx, "CREATE TABLE other_writer (x TEXT)"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- createSchema(ctx, creatorDB, path) }()
	select {
	case err := <-done:
		t.Fatalf("createSchema returned (%v) while another writer held the lock, want it waiting", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := writer.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("createSchema after the writer committed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("createSchema did not finish within 5s of the writer's commit")
	}
	if got := keys(t, creatorDB, "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
}

// TestOpenDBRetriesABusyOpenUntilTheCreatorIsDone pins the busy-open retry: a
// store another process is creating at this instant answers SQLITE_BUSY to the
// switch to WAL without a busy wait, and the open that meets it is retried, not
// failed.
func TestOpenDBRetriesABusyOpenUntilTheCreatorIsDone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "callmeter.db")
	creatorDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := creatorDB.Close(); err != nil {
			t.Errorf("close creator: %v", err)
		}
	})
	creator, err := creatorDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := creator.Close(); err != nil {
			t.Errorf("close creator connection: %v", err)
		}
	})
	// A real creator is OpenDB, with a busy timeout: the retrying open holds a
	// SHARED lock while it reads, and a creator with none would fail its COMMIT
	// (which needs EXCLUSIVE) at once.
	if _, err := creator.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := creator.ExecContext(ctx, "CREATE TABLE half_created (x TEXT)"); err != nil {
		t.Fatal(err)
	}
	type opened struct {
		store *Store
		err   error
	}
	done := make(chan opened, 1)
	go func() {
		store, err := OpenDB(ctx, path)
		done <- opened{store, err}
	}()
	select {
	case got := <-done:
		if got.store != nil {
			_ = got.store.Close()
		}
		t.Fatalf("OpenDB returned (%v) while the creator held the store, want it retrying", got.err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := creator.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("OpenDB after the creator finished: %v", got.err)
		}
		if err := got.store.Close(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenDB did not finish within 5s of the creator's commit")
	}
}

// TestOpenConcurrentCreateOfOneNewFileSucceedsForEveryOpener pins the race the
// first async hooks of a session run: several opens of one store that does not
// exist yet all succeed — the schema transaction takes the write lock at BEGIN
// IMMEDIATE and a busy open is retried — and leave one store at version 1.
func TestOpenConcurrentCreateOfOneNewFileSucceedsForEveryOpener(t *testing.T) {
	const rounds, openers = 8, 6
	for round := 0; round < rounds; round++ {
		path := filepath.Join(t.TempDir(), "state", "callmeter.db")
		start := make(chan struct{})
		errs := make(chan error, openers)
		for i := 0; i < openers; i++ {
			go func() {
				<-start
				store, err := OpenDB(context.Background(), path)
				if err == nil {
					err = store.Close()
				}
				errs <- err
			}()
		}
		close(start)
		for i := 0; i < openers; i++ {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: a racing open failed: %v", round, err)
			}
		}
		store, err := OpenDB(context.Background(), path)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		if got := keys(t, store.DB(), "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
			t.Errorf("round %d: user_version = %s, want 1", round, got)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPruneByAge(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old, recent := cutoff.Add(-time.Hour).UnixMilli(), cutoff.Add(time.Hour).UnixMilli()
	for id, ts := range map[string]int64{"toolu_old": old, "toolu_new": recent} {
		if err := store.UpsertCall(ctx, Call{ToolUseID: id, TS: Ptr(ts)}, Overwrite); err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceCommandParts(ctx, id, []CommandPart{{Seq: 0, Lang: "sh", Program: "cat"}}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertRequest(ctx, Request{RequestID: "msg_" + id, TS: Ptr(ts)}, Overwrite); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertAgent(ctx, Agent{AgentID: "agent_" + id, Started: Ptr(ts)}, Overwrite); err != nil {
			t.Fatal(err)
		}
		if err := store.AddFault(ctx, Fault{TS: ts, ToolUseID: id, Stage: StageParse, Error: "boom"}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 5 {
		t.Errorf("Prune removed %d rows, want 5 (one per table)", removed)
	}
	for _, table := range []string{"calls", "requests", "agents", "faults", "command_parts"} {
		if n := count(t, store, table); n != 1 {
			t.Errorf("%s holds %d rows after prune, want 1", table, n)
		}
	}
	if row(t, store, "calls", "tool_use_id = ?", "toolu_new") == nil {
		t.Error("the recent call was pruned")
	}
	if row(t, store, "command_parts", "tool_use_id = ?", "toolu_new") == nil {
		t.Error("the recent call's command parts were pruned")
	}
}

// TestPruneWithNothingExpiredLeavesNoArchiveBesideTheStore: a prune with no
// row past the cutoff writes no file beside the store; the first row that ages
// out creates archive.db and is moved into it.
func TestPruneWithNothingExpiredLeavesNoArchiveBesideTheStore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	archivePath := filepath.Join(filepath.Dir(store.path), ArchiveFile)
	if err := store.UpsertCall(ctx, Call{ToolUseID: "toolu_new", TS: Ptr(cutoff.Add(time.Hour).UnixMilli())}, Overwrite); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Prune(ctx, cutoff)
	if err != nil || removed != 0 {
		t.Fatalf("Prune with nothing expired = %d, %v; want 0, nil", removed, err)
	}
	if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Prune with nothing expired left %s (stat err %v), want no file", archivePath, err)
	}
	if err := store.UpsertCall(ctx, Call{ToolUseID: "toolu_old", TS: Ptr(cutoff.Add(-time.Hour).UnixMilli())}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if removed, err = store.Prune(ctx, cutoff); err != nil || removed != 1 {
		t.Fatalf("Prune with one expired call = %d, %v; want 1, nil", removed, err)
	}
	if got := keys(t, openArchive(t, store), "SELECT tool_use_id FROM calls"); got != "toolu_old" {
		t.Errorf("archived calls = %s, want toolu_old", got)
	}
	if got := keys(t, store.DB(), "SELECT tool_use_id FROM calls"); got != "toolu_new" {
		t.Errorf("calls after prune = %s, want toolu_new", got)
	}
}

// TestPruneAgesABatchOnlyCallByItsRequest: a call whose PreToolUse and
// PostToolUse never landed has no ts; it ages by its request's, is archived
// and pruned with it, and stays when it has no request row to age by.
func TestPruneAgesABatchOnlyCallByItsRequest(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old, recent := cutoff.Add(-time.Hour).UnixMilli(), cutoff.Add(time.Hour).UnixMilli()
	for request, ts := range map[string]int64{"msg_old": old, "msg_new": recent} {
		if err := store.UpsertRequest(ctx, Request{RequestID: request, TS: Ptr(ts)}, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	for call, request := range map[string]*string{
		"toolu_old": Ptr("msg_old"), "toolu_new": Ptr("msg_new"), "toolu_no_request_row": Ptr("msg_gone"), "toolu_no_request": nil,
	} {
		if err := store.UpsertCall(ctx, Call{ToolUseID: call, RequestID: request, Tool: Ptr("Bash")}, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := keys(t, store.DB(), "SELECT tool_use_id FROM calls ORDER BY tool_use_id"); got != "toolu_new,toolu_no_request,toolu_no_request_row" {
		t.Errorf("calls after prune = %s, want toolu_new,toolu_no_request,toolu_no_request_row", got)
	}
	if got := keys(t, openArchive(t, store), "SELECT tool_use_id FROM calls"); got != "toolu_old" {
		t.Errorf("archived calls = %s, want toolu_old", got)
	}
}

func TestAddFaultStoresEmptyAsNull(t *testing.T) {
	store := openTestStore(t)
	if err := store.AddFault(context.Background(), Fault{TS: 1, Stage: StagePayload, Error: "bad json"}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "faults", "stage = ?", StagePayload)
	if got["session_id"] != nil || got["tool_use_id"] != nil {
		t.Fatalf("empty ids stored as %v / %v, want NULL", got["session_id"], got["tool_use_id"])
	}
}

// seedEveryTable writes one row into every table at ts, tagged so a pair of
// seeds never collide; the four archive-omitted columns carry text.
func seedEveryTable(t *testing.T, store *Store, tag string, ts int64) {
	t.Helper()
	ctx := context.Background()
	err := store.Batch(ctx, func(tx *Tx) error {
		if err := tx.UpsertCall(ctx, Call{
			ToolUseID: "toolu_" + tag, SessionID: Ptr("sess_" + tag), TS: Ptr(ts), Tool: Ptr("Bash"),
			Input: Ptr(`{"command":"secret"}`), Error: Ptr("boom " + tag), Cwd: Ptr("/tmp/demo-proj"),
		}, Overwrite); err != nil {
			return err
		}
		if err := tx.ReplaceCommandParts(ctx, "toolu_"+tag, []CommandPart{{Seq: 0, Lang: "sh", Program: "cat"}}); err != nil {
			return err
		}
		if err := tx.UpsertRequest(ctx, Request{RequestID: "msg_" + tag, TS: Ptr(ts), Model: Ptr("m"),
			Iterations: []Iteration{{Seq: 1, Type: "fallback_message", OutputTokens: Ptr(int64(3))}}}, Overwrite); err != nil {
			return err
		}
		if err := tx.UpsertAgent(ctx, Agent{AgentID: "agent_" + tag, Started: Ptr(ts)}, Overwrite); err != nil {
			return err
		}
		if err := tx.AddFault(ctx, Fault{TS: ts, ToolUseID: "toolu_" + tag, Stage: StagePayload, Error: "bad " + tag}); err != nil {
			return err
		}
		for i, event := range []string{"Notification", EventSubagentStart, EventSubagentStop} {
			if _, err := tx.InsertEvent(ctx, Event{
				EventID: fmt.Sprintf("event_%s_%d", tag, i), Event: event, TS: ts + int64(i), SessionID: Ptr("sess_" + tag),
				AgentID: Ptr("agent_" + tag), Detail: Ptr(`{"level":"info"}`),
			}); err != nil {
				return err
			}
		}
		if err := tx.RebuildAgentTurns(ctx, "agent_"+tag); err != nil {
			return err
		}
		if _, err := tx.InsertTurn(ctx, Turn{EventID: "turn_" + tag, Event: "Stop", TS: ts, SessionID: Ptr("sess_" + tag)}); err != nil {
			return err
		}
		for _, statement := range []string{
			"INSERT OR REPLACE INTO compactions (entry_id, session_id, ts) VALUES ('comp_%[1]s', 'sess_%[1]s', %[2]d)",
			"INSERT OR REPLACE INTO session_costs (session_id, ts, cost_usd) VALUES ('sess_%[1]s', %[2]d, 0.5)",
			"INSERT OR REPLACE INTO stop_hooks (entry_id, session_id, ts) VALUES ('stop_%[1]s', 'sess_%[1]s', %[2]d)",
			"INSERT OR REPLACE INTO stop_hook_runs (entry_id, seq, ts, name) VALUES ('stop_%[1]s', 0, %[2]d, 'hook')",
			"INSERT OR REPLACE INTO turn_durations (entry_id, session_id, ts) VALUES ('dur_%[1]s', 'sess_%[1]s', %[2]d)",
		} {
			if _, err := tx.tx.ExecContext(ctx, fmt.Sprintf(statement, tag, ts)); err != nil {
				return err
			}
		}
		return tx.TouchSession(ctx, Session{SessionID: "sess_" + tag, TS: ts})
	})
	if err != nil {
		t.Fatalf("seed %s: %v", tag, err)
	}
}

// openArchive opens the archive database beside the store.
func openArchive(t *testing.T, store *Store) *sql.DB {
	t.Helper()
	archive, err := sqlitedb.OpenReadWrite(filepath.Join(filepath.Dir(store.path), ArchiveFile), time.Second)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	t.Cleanup(func() {
		if err := archive.Close(); err != nil {
			t.Errorf("close archive: %v", err)
		}
	})
	return archive
}

func countIn(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// TestPruneArchivesEveryTableBeforeDeleting: the rows older than the cutoff of
// every table are copied into archive.db, without the four text columns, and
// then leave the store; the recent ones stay.
func TestPruneArchivesEveryTableBeforeDeleting(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	seedEveryTable(t, store, "new", cutoff.Add(time.Hour).UnixMilli())

	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	perTable := map[string]int{
		"calls": 1, "requests": 1, "turns": 1, "events": 3, "faults": 1, "agents": 1, "agent_turns": 1,
		"sessions": 1, "command_parts": 1, "compactions": 1, "session_costs": 1, "stop_hooks": 1, "stop_hook_runs": 1,
		"turn_durations": 1, "request_iterations": 1,
	}
	want := 0
	archive := openArchive(t, store)
	for table, n := range perTable {
		want += n
		if got := countIn(t, archive, table); got != n {
			t.Errorf("archive %s holds %d rows, want %d", table, got, n)
		}
		if got := count(t, store, table); got != n {
			t.Errorf("store %s holds %d rows after the prune, want the recent %d", table, got, n)
		}
	}
	if int(removed) != want {
		t.Errorf("Prune removed %d rows, want %d", removed, want)
	}
	if got := keys(t, archive, "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("archive user_version = %s, want 1", got)
	}
	// The archive's tables are the store's, less the four text columns.
	omitted := map[string]bool{"calls.input": true, "calls.error": true, "faults.error": true, "events.detail": true}
	for table, columns := range storeColumns {
		var kept []string
		for _, column := range strings.Split(columns, ",") {
			if !omitted[table+"."+column] {
				kept = append(kept, column)
			}
		}
		columns = strings.Join(kept, ",")
		got := keys(t, archive, "SELECT name FROM pragma_table_info('"+table+"') ORDER BY cid")
		if got != columns {
			t.Errorf("archive %s columns = %s, want %s", table, got, columns)
		}
	}
	if got := keys(t, archive, "SELECT tool_use_id || ':' || tool || ':' || cwd FROM calls"); got != "toolu_old:Bash:/tmp/demo-proj" {
		t.Errorf("archived call = %q, want toolu_old:Bash:/tmp/demo-proj", got)
	}
}

// TestPruneCopiesARowAnEarlierPruneAlreadyArchived: rows an earlier prune
// archived come back under the same keys; the next prune must not fail on them
// nor keep a second copy. A fault has no natural key, so the second seed's fault
// is a new fault with its own fault_id and a second archive row.
func TestPruneCopiesARowAnEarlierPruneAlreadyArchived(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour).UnixMilli()
	seedEveryTable(t, store, "old", old)
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("first Prune: %v", err)
	}
	seedEveryTable(t, store, "old", old)
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("Prune over rows already archived: %v", err)
	}
	archive := openArchive(t, store)
	for table, want := range map[string]int{
		"calls": 1, "requests": 1, "agents": 1, "agent_turns": 1, "sessions": 1, "command_parts": 1, "turns": 1, "events": 3,
		"faults": 2, "compactions": 1, "session_costs": 1, "stop_hooks": 1, "stop_hook_runs": 1, "turn_durations": 1,
	} {
		if got := countIn(t, archive, table); got != want {
			t.Errorf("archive %s holds %d rows after two prunes of one row, want %d", table, got, want)
		}
	}
}

// errStopPrune is the error the test seam returns between the two phases.
var errStopPrune = errors.New("stop between the prune phases")

// setPruneAfterArchive installs f as the seam between the two phases for the
// test; a test using it stays serial, the seam being a package variable.
func setPruneAfterArchive(t *testing.T, f func() error) {
	t.Helper()
	pruneAfterArchive = f
	t.Cleanup(func() { pruneAfterArchive = nil })
}

// seededRowsPerTable is what seedEveryTable writes into each table.
var seededRowsPerTable = map[string]int{
	"calls": 1, "requests": 1, "turns": 1, "events": 3, "faults": 1, "agents": 1, "agent_turns": 1, "sessions": 1,
	"command_parts": 1, "compactions": 1, "session_costs": 1, "stop_hooks": 1, "stop_hook_runs": 1, "turn_durations": 1,
	"request_iterations": 1,
}

// stopPruneBetweenPhases seeds an expired row into every table and runs a
// prune that stops once phase 1 has committed, so the rows sit in both files.
func stopPruneBetweenPhases(t *testing.T, store *Store, cutoff time.Time) {
	t.Helper()
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	setPruneAfterArchive(t, func() error { return errStopPrune })
	removed, err := store.Prune(context.Background(), cutoff)
	if !errors.Is(err, errStopPrune) {
		t.Fatalf("Prune stopped between the phases returned %v, want %v", err, errStopPrune)
	}
	if removed != 0 {
		t.Errorf("Prune stopped between the phases reports %d rows removed, want 0", removed)
	}
	if !strings.Contains(err.Error(), store.path) {
		t.Errorf("error %q does not name the store path %s", err, store.path)
	}
	setPruneAfterArchive(t, nil)
}

// TestPruneStopBetweenPhasesLeavesEveryRowInBothFilesAndTheNextPruneConverges:
// a prune that stops after phase 1 has archived every expired row and deleted
// none; running it again deletes them from the store and keeps one archive row
// each, faults included.
func TestPruneStopBetweenPhasesLeavesEveryRowInBothFilesAndTheNextPruneConverges(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stopPruneBetweenPhases(t, store, cutoff)
	archive := openArchive(t, store)
	for table, want := range seededRowsPerTable {
		if got := countIn(t, archive, table); got != want {
			t.Errorf("after the stop, archive %s holds %d rows, want %d", table, got, want)
		}
		if got := count(t, store, table); got != want {
			t.Errorf("after the stop, store %s holds %d rows, want %d", table, got, want)
		}
	}

	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune after the stop: %v", err)
	}
	want := 0
	for table, n := range seededRowsPerTable {
		want += n
		if got := count(t, store, table); got != 0 {
			t.Errorf("after the second prune, store %s holds %d rows, want 0", table, got)
		}
		if got := countIn(t, archive, table); got != n {
			t.Errorf("after the second prune, archive %s holds %d rows, want %d", table, got, n)
		}
	}
	if int(removed) != want {
		t.Errorf("the second Prune removed %d rows, want %d", removed, want)
	}
}

// TestPruneDeletesARowInBothFilesAndKeepsTheArchivedCopy: an expired row whose
// identical copy the archive already holds leaves the store and keeps that
// copy in the archive, for aggregates as well as facts.
func TestPruneDeletesARowInBothFilesAndKeepsTheArchivedCopy(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stopPruneBetweenPhases(t, store, cutoff)
	archive := openArchive(t, store)
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("Prune over rows in both files: %v", err)
	}
	for _, table := range []string{"calls", "faults"} {
		if got := count(t, store, table); got != 0 {
			t.Errorf("store %s holds %d rows, want 0", table, got)
		}
		if got := countIn(t, archive, table); got != 1 {
			t.Errorf("archive %s holds %d rows, want its one copy", table, got)
		}
	}
	if got := keys(t, archive, "SELECT tool FROM calls"); got != "Bash" {
		t.Errorf("the archived call's tool = %q, want the identical stored copy", got)
	}
}

// TestPruneLeavesAnExpiredRowTheArchiveLacks: an expired row whose archive copy
// is gone when phase 2 runs stays in the store, with the parts of its call,
// until the next prune archives it again and deletes it.
func TestPruneLeavesAnExpiredRowTheArchiveLacks(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	// The store keeps one connection and Prune holds it, so the seam reaches the
	// archive through a handle of its own.
	setPruneAfterArchive(t, func() error {
		archive := openArchive(t, store)
		for _, table := range []string{"calls", "command_parts"} {
			if _, err := archive.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
		return nil
	})
	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if want := 15; int(removed) != want { // the 17 seeded rows less the call and its part
		t.Errorf("Prune removed %d rows, want %d", removed, want)
	}
	archive := openArchive(t, store)
	for _, table := range []string{"calls", "command_parts"} {
		if got := count(t, store, table); got != 1 {
			t.Errorf("store %s holds %d rows, want the 1 the archive lacks", table, got)
		}
		if got := countIn(t, archive, table); got != 0 {
			t.Errorf("archive %s holds %d rows, want 0", table, got)
		}
	}

	setPruneAfterArchive(t, nil)
	if removed, err = store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("the second Prune removed %d rows, want the call and its part", removed)
	}
	for _, table := range []string{"calls", "command_parts"} {
		if got := count(t, store, table); got != 0 {
			t.Errorf("store %s holds %d rows after the second prune, want 0", table, got)
		}
		if got := countIn(t, archive, table); got != 1 {
			t.Errorf("archive %s holds %d rows after the second prune, want 1", table, got)
		}
	}
}

// TestPruneArchivesCommandPartsOfExpiringCallsAndOrphansInPhaseOne: the parts of
// a call about to be pruned and the parts of no call at all are copied by phase
// 1 and deleted by phase 2 with their call; the parts of a recent call stay.
func TestPruneArchivesCommandPartsOfExpiringCallsAndOrphansInPhaseOne(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old, recent := cutoff.Add(-time.Hour).UnixMilli(), cutoff.Add(time.Hour).UnixMilli()
	for id, ts := range map[string]*int64{"toolu_old": &old, "toolu_new": &recent, "toolu_orphan": nil} {
		if ts != nil {
			if err := store.UpsertCall(ctx, Call{ToolUseID: id, TS: ts}, Overwrite); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ReplaceCommandParts(ctx, id, []CommandPart{
			{Seq: 0, Lang: "sh", Program: "cat"}, {Seq: 1, Lang: "sh", Program: "wc"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setPruneAfterArchive(t, func() error { return errStopPrune })
	if _, err := store.Prune(ctx, cutoff); !errors.Is(err, errStopPrune) {
		t.Fatalf("Prune stopped between the phases returned %v, want %v", err, errStopPrune)
	}
	archive := openArchive(t, store)
	const partsQuery = "SELECT tool_use_id || ':' || seq FROM command_parts ORDER BY tool_use_id, seq"
	if got := keys(t, archive, partsQuery); got != "toolu_old:0,toolu_old:1,toolu_orphan:0,toolu_orphan:1" {
		t.Errorf("archive command_parts after phase 1 = %s, want the expiring call's and the orphan's parts", got)
	}
	if got := count(t, store, "command_parts"); got != 6 {
		t.Errorf("store command_parts after phase 1 = %d, want all 6", got)
	}

	setPruneAfterArchive(t, nil)
	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 5 { // the old call and its two parts, the orphan's two parts
		t.Errorf("Prune removed %d rows, want 5", removed)
	}
	if got := keys(t, store.DB(), partsQuery); got != "toolu_new:0,toolu_new:1" {
		t.Errorf("store command_parts after the prune = %s, want the recent call's two parts", got)
	}
	if got := keys(t, archive, partsQuery); got != "toolu_old:0,toolu_old:1,toolu_orphan:0,toolu_orphan:1" {
		t.Errorf("archive command_parts after the prune = %s, want each part once", got)
	}
}

// TestFaultsWithIdenticalColumnsAreTwoRowsAndTwoArchiveRows: a fault has its own
// fault_id, so two faults with the same columns are two rows in the store and,
// after a prune, two in the archive.
func TestFaultsWithIdenticalColumnsAreTwoRowsAndTwoArchiveRows(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	fault := Fault{TS: cutoff.Add(-time.Hour).UnixMilli(), SessionID: "sess-1", ToolUseID: "toolu_1", Stage: StageParse, Error: "boom"}
	for range 2 {
		if err := store.AddFault(ctx, fault); err != nil {
			t.Fatal(err)
		}
	}
	if got := keys(t, store.DB(), "SELECT fault_id FROM faults ORDER BY fault_id"); got != "1,2" {
		t.Errorf("fault_id of the two stored faults = %s, want 1,2", got)
	}
	removed, err := store.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Errorf("Prune removed %d rows, want both faults", removed)
	}
	if got := keys(t, openArchive(t, store), "SELECT fault_id FROM faults ORDER BY fault_id"); got != "1,2" {
		t.Errorf("archived fault_id = %s, want 1,2", got)
	}
}

// TestPruneNeverDeletesAFaultWhoseIDTheArchiveHoldsForAnother: a store
// recreated beside a kept archive numbers its faults from 1 again; an expired
// fault whose fault_id the archive already holds for a different fault is
// never deleted without its own archive copy.
func TestPruneNeverDeletesAFaultWhoseIDTheArchiveHoldsForAnother(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := first.AddFault(ctx, Fault{TS: cutoff.Add(-time.Hour).UnixMilli(), SessionID: "sess-old", Stage: StageParse, Error: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Prune(ctx, cutoff); err != nil {
		t.Fatalf("first Prune: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("remove %s: %v", file, err)
		}
	}
	second := openStoreAt(t, path)
	if err := second.AddFault(ctx, Fault{TS: cutoff.Add(-2 * time.Hour).UnixMilli(), SessionID: "sess-new", Stage: StageStore, Error: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, second.DB(), "SELECT fault_id FROM faults"); got != "1" {
		t.Fatalf("fault_id of the recreated store's fault = %s, want 1, the archived fault's id", got)
	}
	if _, err := second.Prune(ctx, cutoff); err != nil {
		t.Fatalf("second Prune: %v", err)
	}
	stored := countIn(t, second.DB(), "faults")
	var archived int
	if err := openArchive(t, second).QueryRow("SELECT COUNT(*) FROM faults WHERE session_id = 'sess-new'").Scan(&archived); err != nil {
		t.Fatalf("count the archived new fault: %v", err)
	}
	if stored == 0 && archived == 0 {
		t.Errorf("the new fault left the store with no archive copy of its own")
	}
}

// makeVersionOneStore creates a store, then takes from it what a version-1 store
// written before the metrics tables had never held: the requests.thinking_tokens
// column (when it has it), the five metrics tables, the calls_request index.
// One old request row goes in by plain SQL. The store is closed.
func makeVersionOneStore(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	old, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if got := keys(t, old.DB(), "SELECT name FROM pragma_table_info('requests') WHERE name = 'thinking_tokens'"); got != "" {
		if _, err := old.DB().Exec("ALTER TABLE requests DROP COLUMN thinking_tokens"); err != nil {
			t.Fatalf("drop thinking_tokens: %v", err)
		}
	}
	for _, statement := range []string{
		"DROP TABLE IF EXISTS compactions", "DROP TABLE IF EXISTS session_costs", "DROP TABLE IF EXISTS stop_hooks",
		"DROP TABLE IF EXISTS stop_hook_runs", "DROP TABLE IF EXISTS turn_durations", "DROP INDEX IF EXISTS calls_request",
		"DROP VIEW IF EXISTS request_cache", "DROP TABLE IF EXISTS request_iterations", "DROP INDEX IF EXISTS requests_session_agent_ts",
		"INSERT INTO requests (request_id, session_id, ts, model, output_tokens) VALUES ('msg_old', 'sess_old', 5, 'm', 9)",
	} {
		if _, err := old.DB().Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestOpenCompletesAVersionOneStore: a version-1 store written before the metrics
// tables and the requests.thinking_tokens column gets them on the next open, in
// the column order of a fresh store, with its rows intact and its version 1.
func TestOpenCompletesAVersionOneStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	makeVersionOneStore(t, path)
	store := openStoreAt(t, path)
	for table, want := range storeColumns {
		got := keys(t, store.DB(), "SELECT name FROM pragma_table_info('"+table+"') ORDER BY cid")
		if got != want {
			t.Errorf("%s columns after the reopen = %s, want %s", table, got, want)
		}
	}
	gotIndexes := keys(t, store.DB(),
		"SELECT name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	wantIndexes := "calls_file_path,calls_prompt,calls_request,calls_session_agent_ts,calls_ts,compactions_session_ts," +
		"events_event_ts,events_session_ts,requests_session_agent_pending,requests_session_agent_ts,stop_hooks_session_ts," +
		"turn_durations_session_ts,turns_session_ts"
	if gotIndexes != wantIndexes {
		t.Errorf("indexes after the reopen = %s, want %s", gotIndexes, wantIndexes)
	}
	if got := keys(t, store.DB(), "SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'request_cache'"); got != schemaViews["request_cache"] {
		t.Errorf("request_cache after the reopen = %q, want the shipped view", got)
	}
	if got := keys(t, store.DB(), "SELECT CAST(user_version AS TEXT) FROM pragma_user_version"); got != "1" {
		t.Errorf("user_version = %s, want 1", got)
	}
	got := row(t, store, "requests", "request_id = ?", "msg_old")
	if got == nil || got["session_id"] != "sess_old" || got["ts"] != int64(5) || got["output_tokens"] != int64(9) {
		t.Errorf("the old request row after the reopen = %v, want it intact", got)
	}
	if got != nil && got["thinking_tokens"] != nil {
		t.Errorf("thinking_tokens of the old row = %v, want NULL", got["thinking_tokens"])
	}
}

// TestOpenLeavesAStoreItCannotCompleteWorking: an open that meets the write lock
// held cannot add the missing columns and tables; it still succeeds, the store
// still takes a hook's write, the prune leaves it alone, and the next open that
// can completes it.
func TestOpenLeavesAStoreItCannotCompleteWorking(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	makeVersionOneStore(t, path)
	holderDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holderDB.Close(); err != nil {
			t.Errorf("close holder: %v", err)
		}
	})
	holder, err := holderDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holder.Close(); err != nil {
			t.Errorf("close holder connection: %v", err)
		}
	})
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenDBWaiting(ctx, path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenDBWaiting while the write lock is held: %v", err)
	}
	if store.SchemaComplete() {
		t.Error("SchemaComplete = true although the open could not write")
	}
	if _, err := holder.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := store.Batch(ctx, func(tx *Tx) error {
		return tx.UpsertRequest(ctx, Request{RequestID: "msg_new", TS: Ptr(int64(6)), Model: Ptr("m")}, Overwrite)
	}); err != nil {
		t.Fatalf("Batch on the incomplete store: %v", err)
	}
	if got := keys(t, store.DB(), "SELECT request_id FROM requests ORDER BY request_id"); got != "msg_new,msg_old" {
		t.Errorf("requests = %s, want msg_new,msg_old", got)
	}
	removed, err := store.Prune(ctx, time.UnixMilli(1_000_000))
	if err != nil || removed != 0 {
		t.Errorf("Prune on the incomplete store = %d, %v; want 0, nil", removed, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ArchiveFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Prune on the incomplete store left archive.db (stat err %v), want no file", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openStoreAt(t, path)
	if !reopened.SchemaComplete() {
		t.Error("SchemaComplete = false after a reopen that could write")
	}
	for table, want := range storeColumns {
		if got := keys(t, reopened.DB(), "SELECT name FROM pragma_table_info('"+table+"') ORDER BY cid"); got != want {
			t.Errorf("%s columns after the reopen = %s, want %s", table, got, want)
		}
	}
}

// TestPruneAddsMissingColumnsToAnOlderArchive: an archive written before
// requests.thinking_tokens gains the column on the next prune, and the archived
// row keeps its value.
func TestPruneAddsMissingColumnsToAnOlderArchive(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	archivePath := filepath.Join(filepath.Dir(store.path), ArchiveFile)
	older, err := sqlitedb.OpenReadWrite(archivePath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		strings.Replace(archiveTables["requests"].ddl, "archive.", "", 1),
		"ALTER TABLE requests DROP COLUMN thinking_tokens",
		"PRAGMA user_version=1",
	} {
		if _, err := older.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := older.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(
		"INSERT INTO requests (request_id, ts, thinking_tokens) VALUES ('msg_old', ?, 7)", cutoff.Add(-time.Hour).UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.Prune(ctx, cutoff); err != nil || removed != 1 {
		t.Fatalf("Prune = %d, %v; want 1, nil", removed, err)
	}
	archive := openArchive(t, store)
	if got, want := keys(t, archive, "SELECT name FROM pragma_table_info('requests') ORDER BY cid"), storeColumns["requests"]; got != want {
		t.Errorf("archive requests columns = %s, want %s", got, want)
	}
	if got := keys(t, archive, "SELECT CAST(thinking_tokens AS TEXT) FROM requests WHERE request_id = 'msg_old'"); got != "7" {
		t.Errorf("archived thinking_tokens = %q, want 7", got)
	}
}

// openStoreAt opens the store at path, closed at the test's end.
func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := OpenDB(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// TestStageTerminatedIsAFaultStage: the terminated stage is the string "terminated"
// and a fault written with it is stored under that stage.
func TestStageTerminatedIsAFaultStage(t *testing.T) {
	if StageTerminated != "terminated" {
		t.Errorf("StageTerminated = %q, want terminated", StageTerminated)
	}
	store := openTestStore(t)
	if err := store.AddFault(context.Background(), Fault{TS: 1, Stage: StageTerminated, Error: "Stop: terminated by SIGTERM"}); err != nil {
		t.Fatal(err)
	}
	if got := row(t, store, "faults", "stage = ?", "terminated"); got == nil {
		t.Error("no faults row of stage terminated after AddFault with StageTerminated")
	}
}

// TestPruneArchiveFailureDeletesNothing: an archive that cannot be written
// fails the prune and leaves every store row where it was.
func TestPruneArchiveFailureDeletesNothing(t *testing.T) {
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := cutoff.Add(-time.Hour).UnixMilli()
	for name, breakArchive := range map[string]func(t *testing.T, archivePath string){
		"archive.db is a directory": func(t *testing.T, archivePath string) {
			if err := os.Mkdir(archivePath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"the last table cannot take the rows": func(t *testing.T, archivePath string) {
			archive, err := sqlitedb.OpenReadWrite(archivePath, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Exec("CREATE TABLE faults (unrelated TEXT)"); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"the archive is newer": func(t *testing.T, archivePath string) {
			archive, err := sqlitedb.OpenReadWrite(archivePath, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Exec("PRAGMA user_version=2"); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			seedEveryTable(t, store, "old", old)
			breakArchive(t, filepath.Join(filepath.Dir(store.path), ArchiveFile))
			removed, err := store.Prune(context.Background(), cutoff)
			if err == nil {
				t.Fatalf("Prune with a broken archive removed %d rows and returned no error", removed)
			}
			if !strings.Contains(err.Error(), "archive") {
				t.Errorf("error %q does not name the archive", err)
			}
			for table := range storeColumns {
				if n := count(t, store, table); n == 0 {
					t.Errorf("%s lost its rows although the archive failed", table)
				}
			}
			// The store still prunes once the archive is whole again.
			if err := os.RemoveAll(filepath.Join(filepath.Dir(store.path), ArchiveFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Prune(context.Background(), cutoff); err != nil {
				t.Fatalf("Prune after the archive was repaired: %v", err)
			}
			if n := count(t, store, "calls"); n != 0 {
				t.Errorf("calls holds %d rows after the repaired prune, want 0", n)
			}
		})
	}
}

func TestArchiveKeepsTheNewerSessionOnKeyReuse(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	now := time.Now()
	day := 24 * time.Hour
	touch := func(ts time.Time, cwd string) {
		t.Helper()
		if err := store.Batch(ctx, func(tx *Tx) error {
			return tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: ts.UnixMilli(), Cwd: Ptr(cwd)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	touch(now.Add(-100*day), "/tmp/first-run")
	if _, err := store.Prune(ctx, now.Add(-90*day)); err != nil {
		t.Fatal(err)
	}
	touch(now.Add(-50*day), "/tmp/second-run")
	if _, err := store.Prune(ctx, now.Add(-30*day)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, "sessions"); n != 0 {
		t.Errorf("store sessions = %d, want 0", n)
	}
	var newer int
	if err := openArchive(t, store).QueryRow("SELECT COUNT(*) FROM sessions WHERE session_id = 'sess-1' AND cwd = '/tmp/second-run' AND last_ts = ?", now.Add(-50*day).UnixMilli()).Scan(&newer); err != nil {
		t.Fatal(err)
	}
	if newer != 1 {
		t.Error("archive lacks the resumed session row")
	}
}

func TestPruneKeepsANewerAgentTurnTheArchiveLacks(t *testing.T) {
	testPruneKeepsReusedFact(t, "agent_turns", "UPDATE agent_turns SET stopped = stopped + 10")
}

func TestPruneKeepsANewerEventTheArchiveLacks(t *testing.T) {
	testPruneKeepsReusedFact(t, "events", "UPDATE events SET ts = ts + 10")
}

func testPruneKeepsReusedFact(t *testing.T, table, update string) {
	t.Helper()
	store := openTestStore(t)
	cutoff := time.Now()
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	if _, err := store.Prune(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	archive := openArchive(t, store)
	// Recreate the facts under the same keys, then change an archived field.
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	if _, err := store.DB().Exec(update); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	want := seededRowsPerTable[table]
	if got := count(t, store, table); got != want {
		t.Errorf("store %s = %d rows, want %d newer facts kept", table, got, want)
	}
	if got := countIn(t, archive, table); got != want {
		t.Errorf("archive %s = %d rows, want %d older facts", table, got, want)
	}
}

func TestPruneRequiresEveryArchivedColumnToMatch(t *testing.T) {
	for _, table := range pruneTables {
		for _, column := range archiveTables[table.name].columns {
			t.Run(table.name+"/"+column, func(t *testing.T) {
				store := openTestStore(t)
				cutoff := time.Now()
				seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
				setPruneAfterArchive(t, func() error {
					archive := openArchive(t, store)
					_, err := archive.Exec(fmt.Sprintf("UPDATE %s SET %s = CASE WHEN typeof(%s) = 'integer' THEN %s + 1 ELSE COALESCE(%s, '') || '-changed' END", table.name, column, column, column, column))
					return err
				})
				if _, err := store.Prune(context.Background(), cutoff); err != nil {
					t.Fatal(err)
				}
				if got := count(t, store, table.name); got != seededRowsPerTable[table.name] {
					t.Errorf("%s rows retained = %d, want %d when archived %s differs", table.name, got, seededRowsPerTable[table.name], column)
				}
			})
		}
	}
}

// seedPruneLoad writes 50k expired rows without storing any content.
func seedPruneLoad(t *testing.T, store *Store, cutoff time.Time) {
	t.Helper()
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		"INSERT INTO calls (tool_use_id, ts, tool, session_id) SELECT 'call-' || n, ?1, 'Bash', 'session-' || n FROM rows",
		"INSERT INTO requests (request_id, ts, session_id) SELECT 'request-' || n, ?1, 'session-' || n FROM rows",
		"INSERT INTO events (event_id, ts, event, session_id) SELECT 'event-' || n, ?1, 'Notification', 'session-' || n FROM rows",
		"INSERT INTO turns (event_id, ts, event, session_id) SELECT 'turn-' || n, ?1, 'Stop', 'session-' || n FROM rows",
		"INSERT INTO sessions (session_id, first_ts, last_ts) SELECT 'session-' || n, ?1, ?1 FROM rows",
	} {
		if _, err := tx.Exec("WITH RECURSIVE rows(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM rows WHERE n < 10000) "+statement, cutoff.Add(-time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestPruneNeverHoldsTheWriteLockLong pins design.md's "a hook waits on one
// chunk at a time, within its own busy bound": a probe sampling BEGIN IMMEDIATE
// every 2 ms sees the delete phase's lock released between its chunks, many
// busy streaks rather than one per table or one for the phase, and no streak
// reaching BusyTimeout. A chunk's hold is wall time that a loaded host
// stretches (seen past 250 ms with every CPU busy), so the release is judged by
// the streak count, which load does not shrink: the read selecting the next
// chunk runs between the commits.
func TestPruneNeverHoldsTheWriteLockLong(t *testing.T) {
	store := openTestStore(t)
	cutoff := time.Now()
	seedPruneLoad(t, store, cutoff)
	probe, err := sqlitedb.OpenReadWrite(store.path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	ready, stop := make(chan struct{}), make(chan struct{})
	type observation struct {
		longest             time.Duration
		busy, free, streaks int
		err                 error
	}
	done := make(chan observation, 1)
	go func() {
		var result observation
		var busySince time.Time
		sample := func() {
			_, err := probe.Exec("BEGIN IMMEDIATE")
			now := time.Now()
			if IsBusy(err) {
				result.busy++
				if busySince.IsZero() {
					busySince = now
				}
			} else {
				if err == nil {
					result.free++
					_, err = probe.Exec("ROLLBACK")
				}
				if err != nil {
					result.err = err
				}
				if !busySince.IsZero() {
					result.streaks++
					if elapsed := now.Sub(busySince); elapsed > result.longest {
						result.longest = elapsed
					}
					busySince = time.Time{}
				}
			}
		}
		sample()
		close(ready)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sample()
			case <-stop:
				sample()
				done <- result
				return
			}
		}
	}()
	<-ready
	started := time.Now()
	removed, err := store.Prune(context.Background(), cutoff)
	took := time.Since(started)
	close(stop)
	result := <-done
	if err != nil {
		t.Fatal(err)
	}
	if result.err != nil {
		t.Fatalf("non-busy probe error: %v", result.err)
	}
	t.Logf("prune elapsed %v; longest busy streak %v; busy streaks %d; busy probes %d, free probes %d",
		took, result.longest, result.streaks, result.busy, result.free)
	if removed != 50000 {
		t.Errorf("removed %d, want 50000", removed)
	}
	if result.busy == 0 {
		t.Error("probe never observed the prune's write lock")
	}
	// 50,000 rows in 5 tables delete in 25 chunks of pruneChunk: one lock per
	// table would show 5 streaks, one for the phase 1.
	if result.streaks < 10 {
		t.Errorf("%d busy streaks, want at least 10 of the 25 delete chunks: the write lock is held across chunks", result.streaks)
	}
	if result.longest >= BusyTimeout {
		t.Errorf("longest busy streak %v, want under BusyTimeout %v", result.longest, BusyTimeout)
	}
}

func TestPruneStopBetweenChunksKeepsCopiesAndConverges(t *testing.T) {
	store := openTestStore(t)
	cutoff := time.Now()
	seedPruneLoad(t, store, cutoff)
	// Abort the second delete chunk after the first 2000 rows committed.
	if _, err := store.DB().Exec("CREATE TRIGGER stop_prune BEFORE DELETE ON calls WHEN (SELECT COUNT(*) FROM calls) <= 8000 BEGIN SELECT RAISE(ABORT, 'stop prune chunk'); END"); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Prune(context.Background(), cutoff)
	if err == nil || !strings.Contains(err.Error(), "stop prune chunk") {
		t.Fatalf("prune error = %v, want the second chunk's abort", err)
	}
	if removed != 2000 {
		t.Errorf("removed = %d, want 2000 committed rows", removed)
	}
	if got := count(t, store, "calls"); got != 8000 {
		t.Errorf("live calls = %d, want 8000 after one committed chunk", got)
	}
	archive := openArchive(t, store)
	var copies int
	if err := archive.QueryRow("SELECT COUNT(*) FROM calls WHERE ts = ? AND tool = 'Bash'", cutoff.Add(-time.Hour).UnixMilli()).Scan(&copies); err != nil {
		t.Fatal(err)
	}
	if copies != 10000 {
		t.Errorf("identical archived calls = %d, want 10000 including every deleted row", copies)
	}
	if _, err := store.DB().Exec("DROP TRIGGER stop_prune"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(context.Background(), cutoff); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"calls", "requests", "events", "turns", "sessions"} {
		if got := count(t, store, table); got != 0 {
			t.Errorf("live %s = %d, want 0 after resumed prune", table, got)
		}
		if got := countIn(t, archive, table); got != 10000 {
			t.Errorf("archive %s = %d, want 10000", table, got)
		}
	}
}

func TestPruneCancelledBetweenPhasesCanResume(t *testing.T) {
	store := openTestStore(t)
	cutoff := time.Now()
	seedEveryTable(t, store, "old", cutoff.Add(-time.Hour).UnixMilli())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setPruneAfterArchive(t, func() error { cancel(); return ctx.Err() })
	if removed, err := store.Prune(ctx, cutoff); !errors.Is(err, context.Canceled) || removed != 0 {
		t.Fatalf("cancelled prune = %d, %v; want 0, context.Canceled", removed, err)
	}
	for table, want := range seededRowsPerTable {
		if got := count(t, store, table); got != want {
			t.Errorf("cancelled prune retained %s = %d, want %d", table, got, want)
		}
	}
	setPruneAfterArchive(t, nil)
	if _, err := store.Prune(context.Background(), cutoff); err != nil {
		t.Fatalf("resume after cancellation: %v", err)
	}
	for table := range seededRowsPerTable {
		if got := count(t, store, table); got != 0 {
			t.Errorf("resumed prune retained %s = %d, want 0", table, got)
		}
	}
}

func TestArchiveReplacesChangedAggregates(t *testing.T) {
	for _, table := range []string{"calls", "requests", "agents"} {
		t.Run(table, func(t *testing.T) {
			store := openTestStore(t)
			cutoff := time.Now()
			old := cutoff.Add(-time.Hour).UnixMilli()
			seedEveryTable(t, store, "old", old)
			if _, err := store.Prune(context.Background(), cutoff); err != nil {
				t.Fatal(err)
			}
			seedEveryTable(t, store, "old", old)
			column := "tool"
			err := store.Batch(context.Background(), func(tx *Tx) error {
				switch table {
				case "calls":
					return tx.UpsertCall(context.Background(), Call{ToolUseID: "toolu_old", Tool: Ptr("Read")}, Overwrite)
				case "requests":
					column = "model"
					return tx.UpsertRequest(context.Background(), Request{RequestID: "msg_old", Model: Ptr("Read")}, Overwrite)
				default:
					column = "model"
					return tx.UpsertAgent(context.Background(), Agent{AgentID: "agent_old", Model: Ptr("Read")}, Overwrite)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Prune(context.Background(), cutoff); err != nil {
				t.Fatal(err)
			}
			if got := count(t, store, table); got != 0 {
				t.Errorf("live %s = %d, want 0", table, got)
			}
			if got := keys(t, openArchive(t, store), "SELECT "+column+" FROM "+table); got != "Read" {
				t.Errorf("archive %s.%s = %q, want the updated aggregate", table, column, got)
			}
		})
	}
}

// TestPruneMovesIterationsWithTheirRequest: an iteration ages with its
// request, never by its own ts: those of an expiring request go to the archive
// and leave the store with it, those of a recent request stay.
func TestPruneMovesIterationsWithTheirRequest(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old, recent := cutoff.Add(-time.Hour).UnixMilli(), cutoff.Add(time.Hour).UnixMilli()
	if err := store.Batch(ctx, func(tx *Tx) error {
		for id, ts := range map[string]int64{"msg_old": old, "msg_recent": recent} {
			if err := tx.UpsertRequest(ctx, Request{RequestID: id, TS: Ptr(ts),
				Iterations: []Iteration{{Seq: 1, Type: "fallback_message"}, {Seq: 2, Type: "fallback_message"}}}, Overwrite); err != nil {
				return err
			}
		}
		// each iteration's own ts says the opposite of its request's
		_, err := tx.tx.ExecContext(ctx, "UPDATE request_iterations SET ts = CASE request_id WHEN 'msg_old' THEN ? ELSE ? END", recent, old)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(ctx, cutoff); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := keys(t, store.DB(), "SELECT request_id || ':' || seq FROM request_iterations ORDER BY 1"); got != "msg_recent:1,msg_recent:2" {
		t.Errorf("store iterations = %s, want msg_recent's two", got)
	}
	if got := keys(t, openArchive(t, store), "SELECT request_id || ':' || seq FROM request_iterations ORDER BY 1"); got != "msg_old:1,msg_old:2" {
		t.Errorf("archive iterations = %s, want msg_old's two", got)
	}
}

// TestStoreWorksOpenedAlone: no table, view, index or trigger of the store
// names another database, so the file opened alone, with no ATTACH, as a
// person or a model opens it with sqlite3 or Python, reads every table and
// view.
func TestStoreWorksOpenedAlone(t *testing.T) {
	store := openTestStore(t)
	seedEveryTable(t, store, "now", time.Now().UnixMilli())
	plain, err := sqlitedb.OpenReadWrite(store.path, time.Second)
	if err != nil {
		t.Fatalf("open the store alone: %v", err)
	}
	t.Cleanup(func() {
		if err := plain.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	objects := keys(t, plain, "SELECT type || ' ' || name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY 1")
	for _, object := range strings.Split(objects, ",") {
		kind, name, _ := strings.Cut(object, " ")
		if kind != "table" && kind != "view" {
			continue
		}
		var n int
		if err := plain.QueryRow("SELECT COUNT(*) FROM " + name).Scan(&n); err != nil {
			t.Errorf("%s %s opened alone: %v", kind, name, err)
		}
	}
	if !strings.Contains(objects, "view request_cache") {
		t.Errorf("objects = %s, want the request_cache view among them", objects)
	}
	if got := keys(t, plain, "SELECT name FROM sqlite_master WHERE sql LIKE '%archive.%' OR sql LIKE '%main.%'"); got != "" {
		t.Errorf("objects naming another database: %s", got)
	}
	// a SQLite before 3.25 cannot parse a window function, and a reader that
	// cannot parse one view cannot read any table of the file
	if got := keys(t, plain, "SELECT name FROM sqlite_master WHERE type = 'view' AND (sql LIKE '% OVER %' OR sql LIKE '%WINDOW %')"); got != "" {
		t.Errorf("views using a window function: %s", got)
	}
}

// TestOpenWritesTheShippedViewAgain: an open finds the view of a store it
// created exactly as shipped, with nothing to write; a store holding another
// SQL under the view's name, with no version marker (an older binary's) or the
// shipped one's, gets the shipped view back on its next open, version still 1.
func TestOpenWritesTheShippedViewAgain(t *testing.T) {
	ctx := context.Background()
	shipped := viewVersion(schemaViews["request_cache"])
	for _, stale := range []string{
		"CREATE VIEW request_cache AS SELECT request_id, NULL AS gap_ms FROM requests",
		fmt.Sprintf("CREATE VIEW request_cache AS -- version %d\n  SELECT request_id, NULL AS gap_ms FROM requests", shipped),
	} {
		path := filepath.Join(t.TempDir(), "state", "callmeter.db")
		fresh := openStoreAt(t, path)
		alters, missing, err := missingFromSchema(ctx, fresh.DB())
		if err != nil || len(alters) != 0 || missing {
			t.Fatalf("missingFromSchema of a fresh store = %v, %v, %v; want nothing to write", alters, missing, err)
		}
		for _, statement := range []string{"DROP VIEW request_cache", stale} {
			if _, err := fresh.DB().Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if err := fresh.Close(); err != nil {
			t.Fatal(err)
		}
		store := openStoreAt(t, path)
		if !store.SchemaComplete() {
			t.Fatalf("SchemaComplete = false after the reopen over %q", stale)
		}
		if got := keys(t, store.DB(), "SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'request_cache'"); got != schemaViews["request_cache"] {
			t.Errorf("request_cache after the reopen over %q = %q, want the shipped view", stale, got)
		}
		if got := keys(t, store.DB(), "SELECT name FROM pragma_table_info('request_cache') ORDER BY cid"); got != "request_id,gap_ms,entry_ttl,expected_read,outcome,cause" {
			t.Errorf("request_cache columns after the reopen over %q = %s", stale, got)
		}
	}
}

// TestOpenLeavesANewerViewAlone: seats running two releases share one store,
// so an open replaces only a view whose version marker is older than the one
// it ships, never a newer release's: the two binaries never drop each other's
// view on every hook.
func TestOpenLeavesANewerViewAlone(t *testing.T) {
	ctx := context.Background()
	shipped := viewVersion(schemaViews["request_cache"])
	if shipped < 1 {
		t.Fatalf("the shipped request_cache carries version %d, want a marker of 1 or more", shipped)
	}
	newer := fmt.Sprintf("CREATE VIEW request_cache AS -- version %d\n  SELECT request_id, NULL AS gap_ms FROM requests", shipped+1)
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	fresh := openStoreAt(t, path)
	for _, statement := range []string{"DROP VIEW request_cache", newer} {
		if _, err := fresh.DB().Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	alters, missing, err := missingFromSchema(ctx, fresh.DB())
	if err != nil || len(alters) != 0 || missing {
		t.Errorf("missingFromSchema under a newer view = %v, %v, %v; want nothing to write", alters, missing, err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	store := openStoreAt(t, path)
	if !store.SchemaComplete() {
		t.Error("SchemaComplete = false under a newer view, want true")
	}
	if got := keys(t, store.DB(), "SELECT sql FROM sqlite_master WHERE type = 'view' AND name = 'request_cache'"); got != newer {
		t.Errorf("request_cache after the reopen = %q, want the newer release's view kept", got)
	}
}

// TestViewVersion: the marker is the first line's `-- version N`, read the
// same by every release whatever the spacing before it, 0 when absent; and
// each shipped view's SQL is the one recorded for its version, so a change to
// a view without a new version (two releases sharing one store would then drop
// each other's view on every open) fails here.
func TestViewVersion(t *testing.T) {
	for _, c := range []struct {
		sql  string
		want int
	}{
		{"CREATE VIEW v AS SELECT 1", 0},
		{"CREATE VIEW v AS -- version 1\n  SELECT 1", 1},
		{"CREATE VIEW v AS  -- version 12\n  SELECT 1", 12},
		{"CREATE VIEW v AS\n  -- version 3\n  SELECT 1", 0},
	} {
		if got := viewVersion(c.sql); got != c.want {
			t.Errorf("viewVersion(%q) = %d, want %d", c.sql, got, c.want)
		}
	}
	// a changed view raises its `-- version N` and records its sum here
	recorded := map[string]map[int]string{
		"request_cache": {1: "273afb523986dce2225859a17966c0f0c251f55f36fc23351d6120dfcc39fc6d"},
	}
	for name, text := range schemaViews {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
		if version := viewVersion(text); recorded[name][version] != sum {
			t.Errorf("view %s version %d has SQL sha256 %s, recorded %q: a changed view raises its version", name, version, sum, recorded[name][version])
		}
	}
}

// TestRequestCacheView: request_cache judges each settled request against the
// previous request of its session and agent: the gap, the TTL of the latest
// earlier cache write ('mixed' when it wrote both lifetimes), the previous
// request's context as the read to expect, the outcome (first, hit at 95 %,
// partial at 50 %, miss; NULL when a count is unknown) and, for a partial or a
// miss, the cause the stored columns prove (model_changed, compacted, expired
// past the entry's TTL, the shorter one when mixed), else unknown. A pending
// request is no request here.
func TestRequestCacheView(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	const minute = int64(60000)
	base := int64(1790000000000)
	type req struct {
		id, agent, model string
		ts               int64
		context          int64
		read             *int64
		write5m, write1h *int64
		pending          bool
	}
	n := func(v int64) *int64 { return &v }
	requests := []req{
		{id: "r1", model: "a", ts: base, context: 10000, read: n(0), write5m: n(0), write1h: n(10000)},
		{id: "r2", model: "a", ts: base + minute, context: 12000, read: n(9600), write5m: n(0), write1h: n(2000)},
		{id: "r3", model: "a", ts: base + minute + 120*minute, context: 13000, read: n(6000), write5m: n(7000), write1h: n(0)},
		{id: "r4", model: "a", ts: base + 122*minute, context: 14000, read: n(1000), write5m: n(6000), write1h: n(7000)},
		{id: "r5", model: "a", ts: base + 123*minute, context: 9000, read: n(0), write5m: n(9000), write1h: n(0)},
		{id: "r6", model: "b", ts: base + 130*minute, context: 9500, read: n(0), write5m: n(9500), write1h: n(0)},
		{id: "pending", model: "b", ts: base + 130*minute + 500, context: 0, pending: true},
		{id: "r7", model: "b", ts: base + 131*minute, context: 9800, read: nil, write5m: n(0), write1h: n(0)},
		{id: "s1", agent: "a1", model: "a", ts: base + minute, context: 500, read: n(0)},
		{id: "s2", agent: "a1", model: "a", ts: base + 11*minute, context: 600, read: n(0)},
		// each TTL's own expiry: 1 h not yet past at 30 minutes, mixed and 5m past at 6
		{id: "u1", agent: "a2", model: "a", ts: base, context: 1000, read: n(0), write5m: n(0), write1h: n(1000)},
		{id: "u2", agent: "a2", model: "a", ts: base + 30*minute, context: 1200, read: n(0), write5m: n(600), write1h: n(600)},
		{id: "u3", agent: "a2", model: "a", ts: base + 36*minute, context: 1300, read: n(0), write5m: n(1300), write1h: n(0)},
		{id: "u4", agent: "a2", model: "a", ts: base + 42*minute, context: 1400, read: n(0), write5m: n(0), write1h: n(0)},
	}
	if err := store.Batch(ctx, func(tx *Tx) error {
		for _, r := range requests {
			row := Request{RequestID: r.id, SessionID: Ptr("sess-1"), TS: Ptr(r.ts), Model: Ptr(r.model), Pending: Ptr(r.pending),
				ContextTokens: Ptr(r.context), CacheReadTokens: r.read, CacheCreation5mTokens: r.write5m, CacheCreation1hTokens: r.write1h}
			if r.agent != "" {
				row.AgentID = Ptr(r.agent)
			}
			if r.write5m != nil || r.write1h != nil {
				var written int64
				if r.write5m != nil {
					written += *r.write5m
				}
				if r.write1h != nil {
					written += *r.write1h
				}
				row.CacheCreationTokens = Ptr(written)
			} else if r.agent != "" {
				row.CacheCreationTokens = Ptr(int64(500)) // a write with no lifetime split
			}
			if err := tx.UpsertRequest(ctx, row, Overwrite); err != nil {
				return err
			}
		}
		_, err := tx.tx.ExecContext(ctx, "INSERT INTO compactions (entry_id, session_id, agent_id, ts) VALUES ('c1', 'sess-1', NULL, ?), ('c2', 'sess-1', 'a1', ?)",
			base+122*minute+30000, base+122*minute+30000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := keys(t, store.DB(), `SELECT request_id || '|' || COALESCE(gap_ms, '-') || '|' || COALESCE(entry_ttl, '-') || '|' ||
		COALESCE(expected_read, '-') || '|' || COALESCE(outcome, '-') || '|' || COALESCE(cause, '-')
		FROM request_cache ORDER BY request_id`)
	want := strings.Join([]string{
		"r1|-|-|-|first|-",
		"r2|60000|1h|10000|hit|-",
		"r3|7200000|1h|12000|partial|expired",
		"r4|60000|5m|13000|miss|unknown",
		"r5|60000|mixed|14000|miss|compacted",
		"r6|420000|5m|9000|miss|model_changed",
		"r7|60000|5m|9500|-|-",
		"s1|-|-|-|first|-",
		"s2|600000|-|500|miss|unknown",
		"u1|-|-|-|first|-",
		"u2|1800000|1h|1000|miss|unknown",
		"u3|360000|mixed|1200|miss|expired",
		"u4|360000|5m|1300|miss|expired",
	}, ",")
	if got != want {
		t.Errorf("request_cache =\n%s\nwant\n%s", strings.ReplaceAll(got, ",", "\n"), strings.ReplaceAll(want, ",", "\n"))
	}
}
