package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Sessions lists one row per session, latest first: when it started and last
// ran, its model, how it started and ended, its calls and sub-agents, and
// where and when it ran. A session is in the window when its last run is.
// --agent-type has no column on a session and does not narrow it.
func Sessions(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Title: f.title("sessions", n),
		Header: []string{
			"CHAT", "SESSION", "STARTED", "LAST", "MODEL", "START", "END", "CALLS", "AGENTS", "CWD", "HOST", "TZ",
		},
	}
	where, args := f.scoped(scope{ts: "s.last_ts", session: "s.session_id"})
	// The rows are read in full before any chat name is: a name lookup reads
	// the store, and the store has one connection.
	type sessionRow struct {
		session                      string
		first, last, tzOffset        *int64
		model, start, end, cwd, host *string
		tzName                       *string
		calls, agents                int64
	}
	var found []sessionRow
	err := query(ctx, store, "sessions",
		`SELECT s.session_id, s.first_ts, s.last_ts, s.model, s.start_source, s.end_reason,
			(SELECT COUNT(*) FROM calls c WHERE c.session_id = s.session_id),
			(SELECT COUNT(*) FROM agents a WHERE a.session_id = s.session_id),
			s.cwd, s.host, s.tz_name, s.tz_offset_minutes
		FROM sessions s WHERE `+where+` ORDER BY s.last_ts DESC, s.session_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var row sessionRow
			if err := r.Scan(
				&row.session, &row.first, &row.last, &row.model, &row.start, &row.end, &row.calls, &row.agents,
				&row.cwd, &row.host, &row.tzName, &row.tzOffset,
			); err != nil {
				return err
			}
			found = append(found, row)
			return nil
		})
	if err != nil {
		return nil, err
	}
	for _, row := range found {
		t.Rows = append(t.Rows, []string{
			n.of(row.session), row.session, stampCell(row.first), stampCell(row.last), textCell(row.model),
			textCell(row.start), textCell(row.end), itoa(row.calls), itoa(row.agents), textCell(row.cwd),
			textCell(row.host), zoneCell(row.tzName, row.tzOffset),
		})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	return t, nil
}

// zoneCell is the zone name a hook computed, else its UTC offset, else "-".
func zoneCell(name *string, offsetMinutes *int64) string {
	if name != nil && *name != "" {
		return *name
	}
	if offsetMinutes == nil {
		return "-"
	}
	sign, minutes := "+", *offsetMinutes
	if minutes < 0 {
		sign, minutes = "-", -minutes
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, minutes/60, minutes%60)
}
