package sqlitedb

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pragma(t *testing.T, database *sql.DB, name string) string {
	t.Helper()
	var value string
	if err := database.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

// storeTestBusy is the busy timeout the tests open a store with.
const storeTestBusy = time.Second

// TestOpenStoreAppliesTheStorePragmaSet pins the one pragma set every
// callmeter store runs on: WAL, synchronous=NORMAL (1), foreign keys on, and
// the busy timeout the caller passed.
func TestOpenStoreAppliesTheStorePragmaSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "store.db")
	database, err := OpenStore(context.Background(), path, storeTestBusy)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	}()
	for name, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "1", "foreign_keys": "1", "busy_timeout": "1000",
	} {
		if got := pragma(t, database, name); got != want {
			t.Errorf("PRAGMA %s = %q, want %q", name, got, want)
		}
	}
}

// TestOpenStoreCreatesItsDirectoryPrivate pins the store directory's mode: a
// missing parent chain is created 0700, never wider.
func TestOpenStoreCreatesItsDirectoryPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "callmeter")
	database, err := OpenStore(context.Background(), filepath.Join(dir, "callmeter.db"), storeTestBusy)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("store directory mode = %o, want 700", got)
	}
}

// TestStorePragmasRefuseAJournalThatIsNotWAL pins the WAL verification: a
// connection whose journal_mode stays something else (an in-memory database
// answers "memory") is an error naming the mode, never a store that silently
// runs in rollback mode.
func TestStorePragmasRefuseAJournalThatIsNotWAL(t *testing.T) {
	database, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	}()
	err = storePragmas(context.Background(), database, storeTestBusy)
	if err == nil {
		t.Fatal("storePragmas accepted a connection that cannot run WAL")
	}
	if !strings.Contains(err.Error(), `journal_mode is "memory"`) {
		t.Fatalf("error = %v, want it to name the journal mode", err)
	}
}

// TestOpenReadWriteKeepsTheOwnersSettings pins the other program's store:
// read-write can write, carries the caller's busy timeout, and does not
// switch the owner's rollback journal to WAL.
func TestOpenReadWriteKeepsTheOwnersSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.db")
	owner, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec("CREATE TABLE threads (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	readWrite, err := OpenReadWrite(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := readWrite.Close(); err != nil {
			t.Errorf("close readWrite: %v", err)
		}
	}()
	if _, err := readWrite.Exec("INSERT INTO threads VALUES ('a')"); err != nil {
		t.Fatalf("read-write insert: %v", err)
	}
	if got := pragma(t, readWrite, "busy_timeout"); got != "5000" {
		t.Fatalf("read-write busy_timeout = %q, want 5000", got)
	}
	if got := pragma(t, readWrite, "journal_mode"); got != "delete" {
		t.Fatalf("owner's journal_mode = %q, want its own rollback journal kept", got)
	}
}

// TestOpenersReachAPathWithURICharacters pins that a path is a path, not URI
// text: a CALLMETER_HOME directory holding "?", "#" or "%41" must open that
// very file — never a truncated or percent-decoded neighbor, which would read
// as an empty database.
func TestOpenersReachAPathWithURICharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd?dir#%41", "store.db")
	store, err := OpenStore(context.Background(), path, storeTestBusy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Exec("CREATE TABLE marker (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("OpenStore did not create the named file: %v", err)
	}
	database, err := OpenReadWrite(path, time.Second)
	if err != nil {
		t.Fatalf("read-write open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	}()
	var count int
	if err := database.QueryRow("SELECT count(*) FROM marker").Scan(&count); err != nil {
		t.Fatalf("read-write handle does not see the store's table: %v", err)
	}
}
