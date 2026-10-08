package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// wireDDL is the wire_requests table of wire.db contract version 1.
const wireDDL = `CREATE TABLE wire_requests (
  request_id   TEXT PRIMARY KEY,
  ts           INTEGER NOT NULL,
  agent_id     TEXT,
  ttl_sent     TEXT,
  advisor_ttl  TEXT,
  advisor_added INTEGER,
  break_at     INTEGER,
  break_kind   TEXT,
  rl_status    TEXT,
  overage      TEXT
) WITHOUT ROWID`

// writeWire writes a wire.db at path as a producer does: WAL, user_version
// version, the statements, then one wire_requests row per id → break_kind (""
// stores NULL).
func writeWire(t *testing.T, path string, version int, statements []string, breaks map[string]string) {
	t.Helper()
	db, err := sqlitedb.OpenReadWrite(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close wire.db: %v", err)
		}
	}()
	for _, statement := range append([]string{"PRAGMA journal_mode=WAL", fmt.Sprintf("PRAGMA user_version=%d", version)}, statements...) {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for id, kind := range breaks {
		var value any
		if kind != "" {
			value = kind
		}
		if _, err := db.Exec("INSERT INTO wire_requests (request_id, ts, break_kind, rl_status) VALUES (?, 1, ?, 'allowed')", id, value); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
}

// TestOpenWireStatesEachWayTheFileCanBe: absent is absent, a file of the
// contract is ok, and every file it cannot read as the contract (unreadable, a
// wrong user_version, no table, a missing column) is a warning whose label the
// note shows and whose error names the path for the log, never absence.
func TestOpenWireStatesEachWayTheFileCanBe(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name, state string
		make        func(t *testing.T, path string)
	}{
		{"no path", "absent", nil},
		{"absent", "absent", func(*testing.T, string) {}},
		{"the contract", "ok", func(t *testing.T, path string) { writeWire(t, path, 1, []string{wireDDL}, nil) }},
		{"a newer version", "warning: user_version 2, this callmeter reads 1",
			func(t *testing.T, path string) { writeWire(t, path, 2, []string{wireDDL}, nil) }},
		{"no table", "warning: no wire_requests table",
			func(t *testing.T, path string) { writeWire(t, path, 1, []string{"CREATE TABLE other (id TEXT)"}, nil) }},
		{"a missing column", "warning: wire_requests lacks break_kind",
			func(t *testing.T, path string) {
				writeWire(t, path, 1, []string{strings.Replace(wireDDL, "  break_kind   TEXT,\n", "", 1)}, nil)
			}},
		{"not a database", "warning: unreadable, see callmeter.log",
			func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(strings.Repeat("not a database\n", 200)), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
		{"a directory", "warning: unreadable, see callmeter.log",
			func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.make != nil {
				path = filepath.Join(t.TempDir(), "wire.db")
				tt.make(t, path)
			}
			wire := openWire(ctx, path)
			defer func() {
				if err := wire.close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			if wire.state != tt.state {
				t.Errorf("state = %q, want %q", wire.state, tt.state)
			}
			warned := strings.HasPrefix(tt.state, "warning: ")
			if warned != (wire.err != nil) {
				t.Errorf("err = %v, want one exactly when the state is a warning", wire.err)
			}
			if wire.err != nil && !strings.Contains(wire.err.Error(), path) {
				t.Errorf("err = %v, want it to name %s", wire.err, path)
			}
		})
	}
}

// TestWireBreaksReadsTheNamedRequestsAcrossBatches: every id with a row comes
// back, a NULL break_kind as nil (unknown, never "none"), across the batch
// boundary; an id without a row does not.
func TestWireBreaksReadsTheNamedRequestsAcrossBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wire.db")
	writeWire(t, path, 1, []string{wireDDL}, map[string]string{
		"msg_0": "none", fmt.Sprintf("msg_%d", wireBatch): "system", "msg_7": "", "msg_elsewhere": "tools",
	})
	wire := openWire(context.Background(), path)
	defer func() {
		if err := wire.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	ids := make([]string, wireBatch+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("msg_%d", i)
	}
	got := wire.breaks(context.Background(), ids)
	if wire.state != "ok" || len(got) != 3 {
		t.Fatalf("breaks = %v (state %q), want 3 rows", got, wire.state)
	}
	last := fmt.Sprintf("msg_%d", wireBatch)
	if kind := got["msg_0"]; kind == nil || *kind != "none" {
		t.Errorf("msg_0 = %v, want none", kind)
	}
	if kind := got[last]; kind == nil || *kind != "system" {
		t.Errorf("%s = %v, want system", last, kind)
	}
	if kind, ok := got["msg_7"]; !ok || kind != nil {
		t.Errorf("msg_7 = %v, %v; want present and nil", kind, ok)
	}
}
