package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// eventValue is the one value an event row is counted under: its source,
// reason, trigger, error type, load reason, tool name or command name, else a
// notification's type from the sanitized detail; empty when it carries none.
// A detail that is not valid JSON names nothing and fails nothing.
const eventValue = `COALESCE(NULLIF(e.source, ''), NULLIF(e.reason, ''), NULLIF(e.trigger, ''), NULLIF(e.error_type, ''),
	NULLIF(e.load_reason, ''), NULLIF(e.tool_name, ''), NULLIF(e.command_name, ''),
	CASE WHEN json_valid(e.detail) THEN json_extract(e.detail, '$.notification_type') END, '')`

// Events counts the lifecycle events by kind and value, most frequent first,
// with when each was first and last seen.
func Events(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no events in window",
		Title:  f.title("events", n),
		Header: []string{"EVENT", "VALUE", "COUNT", "FIRST", "LAST"},
	}
	where, args := f.scoped(scope{ts: "e.ts", session: "e.session_id", agentType: "e.agent_type"})
	err := query(ctx, store, "events",
		`SELECT COALESCE(e.event, ''), `+eventValue+` AS value, COUNT(*), MIN(e.ts), MAX(e.ts)
		FROM events e WHERE `+where+` GROUP BY e.event, value
		ORDER BY COUNT(*) DESC, e.event, value LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var event, value string
			var events int64
			var first, last *int64
			if err := r.Scan(&event, &value, &events, &first, &last); err != nil {
				return err
			}
			t.Rows = append(t.Rows, []string{event, value, itoa(events), stampCell(first), stampCell(last)})
			return nil
		})
	if err != nil {
		return nil, err
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	// A SessionEnd of a session that never started is counted under its reason
	// above like any other; the note says how many of them end such a session.
	var beforeStart int64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events e WHERE e.event = ? AND `+endedBeforeStart("e.session_id")+` AND `+where,
		append([]any{callmeter.EventSessionEnd}, args...)...,
	).Scan(&beforeStart); err != nil {
		return nil, fmt.Errorf("callmeter report: count the SessionEnd events of sessions that never started: %w", err)
	}
	if beforeStart > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf(
			"%d SessionEnd events end a session that never started (no SessionStart, nothing ran; see the sessions topic)",
			beforeStart))
	}
	return t, nil
}
