package callmeter

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// missedPath is a missed.log beside the store, as the wrapper writes it.
func missedPath(store *Store) string {
	return filepath.Join(filepath.Dir(store.path), "missed.log")
}

func writeMissed(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// binaryFaults lists the binary faults as "ts|error", by ts.
func binaryFaults(t *testing.T, store *Store) string {
	t.Helper()
	return keys(t, store.DB(), "SELECT ts || '|' || error FROM faults WHERE stage = 'binary' ORDER BY ts, rowid")
}

func TestIngestMissedTurnsEachLineIntoABinaryFault(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path,
		"1790000001\tPostToolUse\tcannot download the binary\n"+
			"1790000002\tStop\tchecksum mismatch\n"+
			"1790000003\tunknown\tbinary will not start: exec format error\n")
	n, err := store.IngestMissed(ctx, path)
	if err != nil {
		t.Fatalf("IngestMissed: %v", err)
	}
	if n != 3 {
		t.Errorf("IngestMissed = %d, want 3", n)
	}
	want := "1790000001000|PostToolUse: cannot download the binary," +
		"1790000002000|Stop: checksum mismatch," +
		"1790000003000|unknown: binary will not start: exec format error"
	if got := binaryFaults(t, store); got != want {
		t.Errorf("binary faults = %s\nwant %s", got, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("missed.log still there after the ingest (stat err %v)", err)
	}
	if left := ingestLeftovers(t, path); len(left) != 0 {
		t.Errorf("claimed files left behind: %v", left)
	}
	fault := row(t, store, "faults", "stage = 'binary' AND ts = 1790000001000")
	if fault["session_id"] != nil || fault["tool_use_id"] != nil {
		t.Errorf("binary fault carries a session or call: %v", fault)
	}
}

// TestIngestMissedTerminatedLinesAreTerminatedFaults: a line the binary wrote
// itself (reason `terminated by …`, `store unavailable: …` or `panic`) becomes
// a terminated fault and every other line stays a binary fault; both keep the
// `{event}: {reason}` text.
func TestIngestMissedTerminatedLinesAreTerminatedFaults(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path,
		"1790000001\tSubagentStop\tterminated by SIGTERM\n"+
			"1790000002\tStop\tdownload failed\n"+
			"1790000003\tunknown\tterminated by SIGHUP\n"+
			"1790000004\tStop\tbinary exited terminated by signal\n"+
			"1790000005\tPreToolUse\tstore unavailable: full\n"+
			"1790000006\tPostToolUse\tpanic\n"+
			"1790000007\tStop\tpanic of another kind\n")
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 7 {
		t.Fatalf("IngestMissed = %d, %v; want 7, nil", n, err)
	}
	got := keys(t, store.DB(), "SELECT stage || '|' || ts || '|' || error FROM faults ORDER BY ts")
	want := "terminated|1790000001000|SubagentStop: terminated by SIGTERM," +
		"binary|1790000002000|Stop: download failed," +
		"terminated|1790000003000|unknown: terminated by SIGHUP," +
		"binary|1790000004000|Stop: binary exited terminated by signal," +
		"terminated|1790000005000|PreToolUse: store unavailable: full," +
		"terminated|1790000006000|PostToolUse: panic," +
		"binary|1790000007000|Stop: panic of another kind"
	if got != want {
		t.Errorf("faults = %s\nwant %s", got, want)
	}
}

// TestIngestMissedLinesCarryTheirSession: a line's optional fourth field, the
// hook payload's session_id, becomes the fault's session; a three-field line
// keeps no session, and a reason holding a tab whose last part is no session
// id stays whole.
func TestIngestMissedLinesCarryTheirSession(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path,
		"1790000001\tStop\tdownload failed\t0b5c4c1e-1111-2222-3333-444455556666\n"+
			"1790000002\tPostToolUse\tkilled by signal\n"+
			"1790000003\tSubagentStop\tterminated by SIGTERM\tsess_2.b\n"+
			"1790000004\tStop\tcannot install /tmp/demo\tdir/callmeter\n")
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 4 {
		t.Fatalf("IngestMissed = %d, %v; want 4, nil", n, err)
	}
	got := keys(t, store.DB(), "SELECT ts || '|' || stage || '|' || COALESCE(session_id, 'NULL') || '|' || error FROM faults ORDER BY ts")
	want := "1790000001000|binary|0b5c4c1e-1111-2222-3333-444455556666|Stop: download failed," +
		"1790000002000|binary|NULL|PostToolUse: killed by signal," +
		"1790000003000|terminated|sess_2.b|SubagentStop: terminated by SIGTERM," +
		"1790000004000|binary|NULL|Stop: cannot install /tmp/demo\tdir/callmeter"
	if got != want {
		t.Errorf("faults = %q\nwant %q", got, want)
	}
}

func ingestLeftovers(t *testing.T, path string) []string {
	t.Helper()
	left, err := filepath.Glob(path + ".ingest-*")
	if err != nil {
		t.Fatal(err)
	}
	return left
}

func TestIngestMissedMalformedLineIsAFault(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	long := strings.Repeat("x", 300)
	writeMissed(t, path, "no tabs at all\n"+"notanumber\tStop\treason\n"+long+"\n"+"1790000009\tStop\tfine\n")
	n, err := store.IngestMissed(ctx, path)
	if err != nil {
		t.Fatalf("IngestMissed: %v", err)
	}
	if n != 4 {
		t.Errorf("IngestMissed = %d, want 4 (every line is a fault)", n)
	}
	var texts []string
	rows, err := store.DB().Query("SELECT error FROM faults WHERE stage = 'binary' ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close rows: %v", err)
		}
	}()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, text)
	}
	want := []string{
		"unparsed missed.log line: no tabs at all",
		"unparsed missed.log line: notanumber\tStop\treason",
		"unparsed missed.log line: " + strings.Repeat("x", 200),
		"Stop: fine",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("faults = %q\nwant %q", texts, want)
	}
}

func TestIngestMissedAbsentFileIsZero(t *testing.T) {
	store := openTestStore(t)
	n, err := store.IngestMissed(context.Background(), missedPath(store))
	if err != nil || n != 0 {
		t.Errorf("IngestMissed with no file = %d, %v; want 0, nil", n, err)
	}
	if got := count(t, store, "faults"); got != 0 {
		t.Errorf("faults holds %d rows, want 0", got)
	}
}

// TestIngestMissedPicksUpACrashLeftover: a run that claimed the file and died
// before it committed left missed.log.ingest-*; the next call ingests it with
// the fresh missed.log and leaves no claim behind (each is kept as .done-*).
func TestIngestMissedPicksUpACrashLeftover(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path+".ingest-4242", "1790000001\tStop\told run\n")
	writeMissed(t, path, "1790000002\tStop\tnew run\n")
	n, err := store.IngestMissed(ctx, path)
	if err != nil {
		t.Fatalf("IngestMissed: %v", err)
	}
	if n != 2 {
		t.Errorf("IngestMissed = %d, want 2", n)
	}
	if got, want := binaryFaults(t, store), "1790000001000|Stop: old run,1790000002000|Stop: new run"; got != want {
		t.Errorf("binary faults = %s, want %s", got, want)
	}
	if left := ingestLeftovers(t, path); len(left) != 0 {
		t.Errorf("claimed files left behind: %v", left)
	}

	// Only a leftover, no fresh file.
	writeMissed(t, path+".ingest-4243", "1790000003\tStop\tthird\n")
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
		t.Errorf("IngestMissed with only a leftover = %d, %v; want 1, nil", n, err)
	}
}

// TestIngestMissedKeepsAClaimOfTheSamePid: a leftover named for this very pid
// (a pid reused after a crash) is ingested, never overwritten by the claim.
func TestIngestMissedKeepsAClaimOfTheSamePid(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path+ingestInfix+strconv.Itoa(os.Getpid()), "1790000001\tStop\tleft by the same pid\n")
	writeMissed(t, path, "1790000002\tStop\tfresh\n")
	n, err := store.IngestMissed(ctx, path)
	if err != nil || n != 2 {
		t.Fatalf("IngestMissed = %d, %v; want 2, nil", n, err)
	}
}

// TestIngestMissedKeepsTheFilesWhenTheStoreRefuses: a commit that fails leaves
// the claimed file for the next run, and the faults are not half written.
func TestIngestMissedKeepsTheFilesWhenTheStoreRefuses(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path, "1790000001\tStop\tone\n1790000002\tStop\ttwo\n")
	if _, err := store.DB().Exec("DROP TABLE faults"); err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err == nil || n != 0 {
		t.Fatalf("IngestMissed into a store with no faults table = %d, %v; want 0 and an error", n, err)
	}
	left := ingestLeftovers(t, path)
	if len(left) != 1 {
		t.Fatalf("claimed files after the failed ingest = %v, want exactly the one claim", left)
	}
	data, err := os.ReadFile(left[0])
	if err != nil || !strings.Contains(string(data), "two") {
		t.Errorf("the claim lost its lines: %q, %v", data, err)
	}
	// The store is whole again: the next call ingests the claim.
	if _, err := store.DB().Exec(`CREATE TABLE faults (ts INTEGER, session_id TEXT, tool_use_id TEXT, stage TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 2 {
		t.Errorf("IngestMissed after the repair = %d, %v; want 2, nil", n, err)
	}
}

// TestIngestMissedLeavesAClaimAnotherRunHolds: hooks run in parallel, and a
// claim a live run holds (its flock taken) is that run's to ingest; a second
// run that ingested it too would write every binary fault twice.
func TestIngestMissedLeavesAClaimAnotherRunHolds(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	claim := path + ingestInfix + "999999"
	writeMissed(t, claim, "1790000001\tStop\tthe other run's\n")
	held, err := os.Open(claim)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("take the other run's lock: %v", err)
	}
	writeMissed(t, path, "1790000002\tStop\tmine\n")
	n, err := store.IngestMissed(ctx, path)
	if err != nil || n != 1 {
		t.Fatalf("IngestMissed = %d, %v; want 1, nil", n, err)
	}
	if got, want := binaryFaults(t, store), "1790000002000|Stop: mine"; got != want {
		t.Errorf("binary faults = %s, want %s", got, want)
	}
	if _, err := os.Stat(claim); err != nil {
		t.Errorf("the held claim was taken from its run: %v", err)
	}
	// Its run gone, the lock goes with it, and the next ingest takes it.
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
		t.Errorf("IngestMissed after the holder ended = %d, %v; want 1, nil", n, err)
	}
}

// TestStoreFailureClass: each store failure a hook can meet reads as its fixed
// label; a real store refusing a newer schema, a file that is no database and
// a trigger's abort among them, and an error joined to another keeps its class.
func TestStoreFailureClass(t *testing.T) {
	ctx := context.Background()
	newer := filepath.Join(t.TempDir(), "callmeter.db")
	store, err := OpenDB(ctx, newer)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(SchemaVersion+1)); err != nil {
		t.Fatalf("raise user_version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, newerErr := OpenDB(ctx, newer)
	notADB := filepath.Join(t.TempDir(), "callmeter.db")
	if err := os.WriteFile(notADB, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatalf("write %s: %v", notADB, err)
	}
	_, notADBErr := OpenDB(ctx, notADB)
	refusing := openTestStore(t)
	if _, err := refusing.DB().ExecContext(ctx,
		"CREATE TRIGGER refuse_faults BEFORE INSERT ON faults BEGIN SELECT RAISE(ABORT, 'refused by the test'); END"); err != nil {
		t.Fatalf("create the refusing trigger: %v", err)
	}
	refusedErr := refusing.AddFault(ctx, Fault{TS: 1, Stage: StageStore, Error: "x"})
	var syntax *json.SyntaxError
	payloadErr := json.Unmarshal([]byte("{"), &syntax)
	pathErr := func(errno syscall.Errno) error { return &fs.PathError{Op: "open", Path: "callmeter.db", Err: errno} }
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"newer schema":               {newerErr, StoreClassNewerSchema},
		"newer schema, joined":       {errors.Join(payloadErr, newerErr), StoreClassNewerSchema},
		"not a database":             {notADBErr, StoreClassCorrupt},
		"a trigger's abort":          {refusedErr, StoreClassOther},
		"no space":                   {pathErr(syscall.ENOSPC), StoreClassFull},
		"read-only file system":      {pathErr(syscall.EROFS), StoreClassReadonly},
		"permission denied":          {pathErr(syscall.EACCES), StoreClassReadonly},
		"I/O error":                  {pathErr(syscall.EIO), StoreClassIO},
		"a directory that is a file": {pathErr(syscall.ENOTDIR), StoreClassOpen},
		"anything else":              {errors.New("x"), StoreClassOther},
	} {
		if c.err == nil {
			t.Errorf("%s: no error to classify", name)
			continue
		}
		if got := StoreFailureClass(c.err); got != c.want {
			t.Errorf("StoreFailureClass(%s: %v) = %q, want %q", name, c.err, got, c.want)
		}
	}
}

// missedRows counts the binary and terminated faults, the rows missed.log gives.
func missedRows(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM faults WHERE stage IN ('binary', 'terminated')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestIngestMissedReingestAddsNoDuplicate: a run killed after its ingest
// committed but before it moved its claim leaves the claim as it was; the next
// ingest of it writes no fault twice, an unparsed line's included, while two
// identical lines of one file stay two faults.
func TestIngestMissedReingestAddsNoDuplicate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	content := "1790000001\tPreToolUse\tbinary exited 1\tsess-1\n" +
		"1790000001\tPreToolUse\tbinary exited 1\tsess-1\n" +
		"1790000002\tStop\tterminated by SIGTERM\tsess-1\n" +
		"not a missed line\n"
	writeMissed(t, path, content)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 4 {
		t.Fatalf("IngestMissed = %d, %v; want 4, nil", n, err)
	}
	// The crash: the claim is still there, unchanged, after the commit.
	claim := path + ingestInfix + "4242"
	writeMissed(t, claim, content)
	if err := os.Chtimes(claim, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 0 {
		t.Errorf("IngestMissed of the committed claim = %d, %v; want 0, nil", n, err)
	}
	if n := missedRows(t, store); n != 4 {
		t.Errorf("faults from missed.log = %d, want 4: re-ingesting a committed claim duplicated its faults", n)
	}
}

// TestIngestMissedUnmovedClaimAddsNoDuplicate: a claim left in place after its
// ingest committed (the run killed, or its move failing, before the rename)
// keeps its last write time, so the next ingest of it writes no unparsed line
// twice: an unparsed line is stamped at that time.
func TestIngestMissedUnmovedClaimAddsNoDuplicate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "missed.log")
	writeMissed(t, path, "not a missed line\n")
	written := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	n, err := store.IngestMissedHeld(ctx, path, func() func() {
		if err := os.Chmod(dir, 0o500); err != nil { // the claim can no longer be renamed
			t.Errorf("chmod %s: %v", dir, err)
		}
		return func() {}
	})
	if err == nil || n != 1 {
		t.Fatalf("IngestMissedHeld with an unmovable claim = %d, %v; want 1 and the move's error", n, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 0 {
		t.Errorf("IngestMissed of the committed, unmoved claim = %d, %v; want 0, nil", n, err)
	}
	if n := missedRows(t, store); n != 1 {
		t.Errorf("faults from missed.log = %d, want 1: the unmoved claim's unparsed line was written again", n)
	}
}

// backdateDone ages every ingested claim kept for its grace period by age.
func backdateDone(t *testing.T, path string, age time.Duration) []string {
	t.Helper()
	done, err := filepath.Glob(path + doneInfix + "*")
	if err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	for _, file := range done {
		if err := os.Chtimes(file, then, then); err != nil {
			t.Fatal(err)
		}
	}
	return done
}

// TestIngestMissedKeepsALateAppend: an appender (the wrapper's `>>`,
// AppendMissed) that opened missed.log before the ingest's rename but writes
// after its read writes into the claimed file; the claim is kept for its
// grace period and the next ingest after it takes only the late line.
func TestIngestMissedKeepsALateAppend(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	writeMissed(t, path, "1790000000\tPostToolUse\tbinary exited 1\n")
	late, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644) // opened before the claim
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
		t.Fatalf("IngestMissed = %d, %v; want 1, nil", n, err)
	}
	if _, err := late.WriteString("1790000001\tStop\tterminated by SIGTERM\tsess-1\n"); err != nil { // written after the read
		t.Fatal(err)
	}
	if err := late.Close(); err != nil {
		t.Fatal(err)
	}
	// Inside the grace period the claim stays and nothing is taken from it.
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 0 {
		t.Errorf("IngestMissed inside the grace period = %d, %v; want 0, nil", n, err)
	}
	if done := backdateDone(t, path, 3*time.Second); len(done) != 1 {
		t.Errorf("claims kept for the grace period = %v, want one", done)
	}
	if _, err := store.IngestMissed(ctx, path); err != nil {
		t.Fatal(err)
	}
	if n := missedRows(t, store); n != 2 {
		t.Errorf("faults from missed.log = %d, want 2: the line appended through an fd opened before the claim is gone", n)
	}
	if done := backdateDone(t, path, 3*time.Second); len(done) != 0 {
		t.Errorf("claims left after their grace period = %v, want none", done)
	}

	t.Run("no late line", func(t *testing.T) {
		store := openTestStore(t)
		path := missedPath(store)
		writeMissed(t, path, "1790000000\tPostToolUse\tbinary exited 1\n")
		if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
			t.Fatalf("IngestMissed = %d, %v; want 1, nil", n, err)
		}
		backdateDone(t, path, 3*time.Second)
		if n, err := store.IngestMissed(ctx, path); err != nil || n != 0 {
			t.Errorf("IngestMissed after the grace period = %d, %v; want 0, nil", n, err)
		}
		if n := missedRows(t, store); n != 1 {
			t.Errorf("faults from missed.log = %d, want 1", n)
		}
		if done := backdateDone(t, path, 0); len(done) != 0 {
			t.Errorf("claims left after their grace period = %v, want none", done)
		}
	})
}

// TestIngestMissedKeepsTwinLostEventsOfLaterIngests: a headless exit cancels
// the running hooks of one session within one second, and each leaves the
// same line; a run that claims missed.log between two of those appends
// ingests the first, and the next run's fresh claim holds its twin, a second
// lost event that is still counted.
func TestIngestMissedKeepsTwinLostEventsOfLaterIngests(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	path := missedPath(store)
	line := "1790000001\tPostToolUse\tterminated by SIGTERM\tsess-1\n"
	writeMissed(t, path, line)
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
		t.Fatalf("first IngestMissed = %d, %v; want 1, nil", n, err)
	}
	writeMissed(t, path, line)
	if n, err := store.IngestMissed(ctx, path); err != nil || n != 1 {
		t.Errorf("IngestMissed of the twin's fresh claim = %d, %v; want 1, nil", n, err)
	}
	if n := missedRows(t, store); n != 2 {
		t.Errorf("faults from missed.log = %d, want 2: a second lost event was read as a re-ingest", n)
	}
}
