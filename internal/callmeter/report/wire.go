package report

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/sqlitedb"
)

// WireVersion is the wire.db contract version (its user_version) this
// callmeter reads.
const WireVersion = 1

// wireColumns are the wire_requests columns of contract version 1; a table
// lacking one is not the contract.
var wireColumns = []string{
	"request_id", "ts", "agent_id", "ttl_sent", "advisor_ttl", "advisor_added", "break_at", "break_kind",
	"rl_status", "overage",
}

// The reader's states as the coverage note shows them; a warning reads
// "warning: {label}".
const (
	wireAbsent     = "absent"
	wireOK         = "ok"
	wireUnreadable = "unreadable, see callmeter.log"
)

// wireBatch is how many request ids one wire_requests query names.
const wireBatch = 500

// wireFacts is the optional wire.db beside the store, open read-only for one
// report. callmeter never writes, creates, prunes or archives it, and nothing
// in the store refers to it: its facts join the store's requests by
// request_id in memory, at report time only.
type wireFacts struct {
	path  string
	db    *sql.DB // nil unless the state is ok
	state string  // wireAbsent, wireOK or "warning: {label}"
	err   error   // the warning with its path and cause, for the log; nil when absent or ok
}

// openWire opens path read-only and checks it is the contract: user_version
// WireVersion and a wire_requests table with every column. A missing file (or
// no path) is absent; any other way it cannot be read as the contract is a
// warning, never absence.
func openWire(ctx context.Context, path string) *wireFacts {
	if path == "" {
		return &wireFacts{state: wireAbsent}
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return &wireFacts{state: wireAbsent}
	} else if err != nil {
		return warnWire(wireUnreadable, fmt.Errorf("stat wire.db %s: %w", path, err))
	}
	db, err := sqlitedb.OpenReadOnly(path, callmeter.BusyTimeout)
	if err != nil {
		return warnWire(wireUnreadable, err)
	}
	w := &wireFacts{path: path, db: db, state: wireOK}
	if label, err := checkWire(ctx, db, path); err != nil {
		w.warn(label, err)
	}
	return w
}

// checkWire is the warning label and error of a database that is not the
// contract; nil when it is.
func checkWire(ctx context.Context, db *sql.DB, path string) (string, error) {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return wireUnreadable, fmt.Errorf("read wire.db %s: user_version: %w", path, err)
	}
	if version != WireVersion {
		return fmt.Sprintf("user_version %d, this callmeter reads %d", version, WireVersion),
			fmt.Errorf("read wire.db %s: user_version %d, this callmeter reads %d", path, version, WireVersion)
	}
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('wire_requests')")
	if err != nil {
		return wireUnreadable, fmt.Errorf("read wire.db %s: wire_requests columns: %w", path, err)
	}
	var columns []string
	err = readRows(rows, "wire_requests columns", func(r rowSource) error {
		var name string
		if err := r.Scan(&name); err != nil {
			return err
		}
		columns = append(columns, name)
		return nil
	})
	if err != nil {
		return wireUnreadable, fmt.Errorf("read wire.db %s: %w", path, err)
	}
	if len(columns) == 0 {
		return "no wire_requests table", fmt.Errorf("read wire.db %s: no wire_requests table", path)
	}
	for _, column := range wireColumns {
		if !slices.Contains(columns, column) {
			return "wire_requests lacks " + column, fmt.Errorf("read wire.db %s: wire_requests lacks %s", path, column)
		}
	}
	return "", nil
}

func warnWire(label string, err error) *wireFacts {
	w := &wireFacts{}
	w.warn(label, err)
	return w
}

// warn turns the reader into a warning: nothing more is read from it.
func (w *wireFacts) warn(label string, err error) {
	w.state, w.err = "warning: "+label, err
}

// breaks reads the break_kind of each of ids that has a wire_requests row: a
// NULL kind is nil (unknown, never "none"); an id with no row is not in the
// map. A read failure turns the reader into a warning and returns nothing, so
// no request is counted on part of the file.
func (w *wireFacts) breaks(ctx context.Context, ids []string) map[string]*string {
	if w.state != wireOK {
		return nil
	}
	got := map[string]*string{}
	for batch := range slices.Chunk(ids, wireBatch) {
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := w.db.QueryContext(ctx,
			"SELECT request_id, break_kind FROM wire_requests WHERE request_id IN (?"+
				strings.Repeat(", ?", len(batch)-1)+")", args...)
		if err == nil {
			err = readRows(rows, "wire_requests", func(r rowSource) error {
				var id string
				var kind *string
				if err := r.Scan(&id, &kind); err != nil {
					return err
				}
				got[id] = kind
				return nil
			})
		}
		if err != nil {
			w.warn(wireUnreadable, fmt.Errorf("read wire.db %s: %w", w.path, err))
			return nil
		}
	}
	return got
}

// close closes the read-only handle, if one is open.
func (w *wireFacts) close() error {
	if w.db == nil {
		return nil
	}
	if err := w.db.Close(); err != nil {
		return fmt.Errorf("close wire.db %s: %w", w.path, err)
	}
	return nil
}
