package callmeter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RedactCount is how many rows of one column a Redact pass changed.
type RedactCount struct {
	Column string
	Rows   int64
}

// Redact rewrites the rows already stored the way they are written now, in one
// transaction that takes the write lock at BEGIN IMMEDIATE: every calls.input
// through SanitizeInput (a Bash command's heredoc bodies cut out, free text
// sized), every calls.error through SanitizeError, every events.detail through
// SanitizeDetail for its event, every faults.error through sanitizeFault, every stop_hook_runs.name
// StopHookName could not store through redactHookName. A call whose input changed loses its command_parts rows,
// which the next report parses again from the redacted input. A row that
// cannot be rewritten fails the whole pass and changes nothing. Redact is an
// explicit command, never a migration; a second pass changes nothing. It
// returns the rows changed per column: calls.input, calls.error,
// events.detail, faults.error, stop_hook_runs.name, then the command_parts
// rows deleted.
func (s *Store) Redact(ctx context.Context) (counts []RedactCount, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("callmeter store %s: take a connection to redact: %w", s.path, err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: release the redact connection: %w", s.path, closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("callmeter store %s: begin redact: %w", s.path, err)
	}
	counts, err = redactRows(ctx, conn)
	if err == nil {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err == nil {
			return counts, nil
		}
		err = fmt.Errorf("commit: %w", err)
	}
	err = fmt.Errorf("callmeter store %s: redact: %w", s.path, err)
	if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
		err = errors.Join(err, fmt.Errorf("callmeter store %s: roll back redact: %w", s.path, rollbackErr))
	}
	return nil, err
}

// redactRows is Redact's work inside its transaction.
func redactRows(ctx context.Context, conn *sql.Conn) ([]RedactCount, error) {
	inputs, err := rewriteColumn(ctx, conn, "calls.input",
		`SELECT tool_use_id, COALESCE(tool, ''), input FROM calls WHERE input IS NOT NULL`,
		`UPDATE calls SET input = ? WHERE tool_use_id = ?`,
		func(tool, input string) (string, error) { return SanitizeInput(tool, json.RawMessage(input)) })
	if err != nil {
		return nil, err
	}
	var parts int64
	for _, id := range inputs {
		result, err := conn.ExecContext(ctx, `DELETE FROM command_parts WHERE tool_use_id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("delete the command_parts of call %v: %w", id, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count the command_parts deleted for call %v: %w", id, err)
		}
		parts += n
	}
	errorRows, err := rewriteColumn(ctx, conn, "calls.error",
		`SELECT tool_use_id, '', error FROM calls WHERE error IS NOT NULL`,
		`UPDATE calls SET error = ? WHERE tool_use_id = ?`,
		func(_, text string) (string, error) { return SanitizeError(text), nil })
	if err != nil {
		return nil, err
	}
	details, err := rewriteColumn(ctx, conn, "events.detail",
		`SELECT event_id, COALESCE(event, '') || char(9) || COALESCE(tool_name, ''), detail FROM events WHERE detail IS NOT NULL`,
		`UPDATE events SET detail = ? WHERE event_id = ?`,
		func(eventTool, detail string) (string, error) {
			// The tool from its column, which omits it from the detail; a
			// PermissionDenied stored before it filled the column names it there.
			event, tool, _ := strings.Cut(eventTool, "\t")
			if tool == "" {
				_, tool = detailHead(json.RawMessage(detail))
			}
			return sanitizeEventDetail(event, tool, json.RawMessage(detail), nil)
		})
	if err != nil {
		return nil, err
	}
	faults, err := rewriteColumn(ctx, conn, "faults.error",
		`SELECT fault_id, COALESCE(stage, ''), error FROM faults WHERE error IS NOT NULL`,
		`UPDATE faults SET error = ? WHERE fault_id = ?`,
		func(stage, text string) (string, error) { return sanitizeFault(stage, text), nil })
	if err != nil {
		return nil, err
	}
	hookNames, err := rewriteColumn(ctx, conn, "stop_hook_runs.name",
		`SELECT rowid, entry_id || char(0) || seq, name FROM stop_hook_runs WHERE name IS NOT NULL`,
		`UPDATE stop_hook_runs SET name = ? WHERE rowid = ?`,
		func(row, name string) (string, error) { return redactHookName(row, name), nil })
	if err != nil {
		return nil, err
	}
	return []RedactCount{
		{Column: "calls.input", Rows: int64(len(inputs))},
		{Column: "calls.error", Rows: int64(len(errorRows))},
		{Column: "events.detail", Rows: int64(len(details))},
		{Column: "faults.error", Rows: int64(len(faults))},
		{Column: "stop_hook_runs.name", Rows: int64(len(hookNames))},
		{Column: "command_parts", Rows: parts},
	}, nil
}

// redactHookName is a stored Stop-hook name as StopHookName could store it.
// The command is gone, so the name's shape decides: one HookNameKept accepts
// stays; any other came from a prompt hook's prose under an older namer and
// becomes `prompt#` and 8 hex digits of the SHA-256 of the old name salted
// with its row (entry_id and seq), since a lone word's hash is reversed by
// trying a dictionary. The result is a kept shape, so a second pass keeps it.
func redactHookName(row, name string) string {
	if HookNameKept(name) {
		return name
	}
	return "prompt#" + shortSum(name+"\x00"+row)
}

// rewriteColumn reads every (key, aux, value) row selectRows yields, then
// writes each value rewrite changes back through update (value, key). It
// returns the keys of the rows it changed.
func rewriteColumn(
	ctx context.Context,
	conn *sql.Conn,
	column, selectRows, update string,
	rewrite func(aux, value string) (string, error),
) ([]any, error) {
	type row struct {
		key        any
		aux, value string
	}
	var rows []row
	result, err := conn.QueryContext(ctx, selectRows)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", column, err)
	}
	for result.Next() {
		var r row
		if err := result.Scan(&r.key, &r.aux, &r.value); err != nil {
			return nil, errors.Join(fmt.Errorf("scan %s: %w", column, err), result.Close())
		}
		rows = append(rows, r)
	}
	if err := errors.Join(result.Err(), result.Close()); err != nil {
		return nil, fmt.Errorf("read %s: %w", column, err)
	}
	var changed []any
	for _, r := range rows {
		value, err := rewrite(r.aux, r.value)
		if err != nil {
			return nil, fmt.Errorf("%s of row %v: %w", column, r.key, err)
		}
		if value == r.value {
			continue
		}
		if _, err := conn.ExecContext(ctx, update, value, r.key); err != nil {
			return nil, fmt.Errorf("write %s of row %v: %w", column, r.key, err)
		}
		changed = append(changed, r.key)
	}
	return changed, nil
}

// UnredactedCalls counts the calls Redact would rewrite: a stored input that
// SanitizeInput, run again over it, changes or cannot read, or a stored error
// that SanitizeError changes. Both sanitizers leave their own output
// unchanged, so a call written under the current rules never counts and a
// rule added later counts every call stored before it. It reads the whole
// calls table, whatever a report's window, and writes nothing.
func (s *Store) UnredactedCalls(ctx context.Context) (n int64, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT COALESCE(tool, ''), input, error FROM calls WHERE input IS NOT NULL OR error IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("callmeter store %s: read calls to check their privacy form: %w", s.path, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("callmeter store %s: close calls read: %w", s.path, closeErr))
		}
	}()
	for rows.Next() {
		var tool string
		var input, text sql.NullString
		if err := rows.Scan(&tool, &input, &text); err != nil {
			return 0, fmt.Errorf("callmeter store %s: scan a call's input and error: %w", s.path, err)
		}
		if input.Valid {
			if again, err := SanitizeInput(tool, json.RawMessage(input.String)); err != nil || again != input.String {
				n++
				continue
			}
		}
		if text.Valid && SanitizeError(text.String) != text.String {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("callmeter store %s: read calls to check their privacy form: %w", s.path, err)
	}
	return n, nil
}
