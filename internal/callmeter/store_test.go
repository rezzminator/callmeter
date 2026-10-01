package callmeter

import (
	"context"
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
		"calls,pending,source,config_dir,seat_dir",
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
	if want := "agent_turns,agents,calls,command_parts,events,faults,requests,sessions,turns"; gotTables != want {
		t.Errorf("tables = %s, want %s", gotTables, want)
	}
	gotIndexes := keys(t, store.DB(),
		"SELECT name FROM sqlite_master WHERE type = 'index' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	wantIndexes := "calls_file_path,calls_prompt,calls_session_agent_ts,calls_ts,events_event_ts,events_session_ts," +
		"requests_session_agent_pending,turns_session_ts"
	if gotIndexes != wantIndexes {
		t.Errorf("indexes = %s, want %s", gotIndexes, wantIndexes)
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
	writerDB, err := sqlitedb.OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writerDB.Close(); err != nil {
			t.Errorf("close writer: %v", err)
		}
	})
	creatorDB, err := sqlitedb.OpenStore(ctx, path)
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
		if err := tx.UpsertRequest(ctx, Request{RequestID: "msg_" + tag, TS: Ptr(ts), Model: Ptr("m")}, Overwrite); err != nil {
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
		"sessions": 1, "command_parts": 1,
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
		"faults": 2,
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
	"command_parts": 1,
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
// key the archive already holds leaves the store and the archive keeps the
// copy it has, never overwritten. A fault is never updated, so its archived
// copy carries the store row's own columns, every one of them its key.
func TestPruneDeletesARowInBothFilesAndKeepsTheArchivedCopy(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	cutoff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stopPruneBetweenPhases(t, store, cutoff)
	archive := openArchive(t, store)
	if _, err := archive.Exec("UPDATE calls SET tool = 'archived copy'"); err != nil {
		t.Fatalf("mark the archived call: %v", err)
	}
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
	if got := keys(t, archive, "SELECT tool FROM calls"); got != "archived copy" {
		t.Errorf("the archived call's tool = %q, want the archive's own copy, not the store's", got)
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
	if want := 9; int(removed) != want { // the 11 seeded rows less the call and its part
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
