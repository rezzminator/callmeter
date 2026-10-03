package report

import (
	"context"
	"fmt"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Sessions lists one row per session, latest first: when it started and last
// ran, its model, how it started and ended, its calls and sub-agents, and
// where and when it ran. A session is in the window when its last run is.
// --agent-type has no column on a session and does not narrow it.
func Sessions(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty: "callmeter: no sessions in window",
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
		beforeStart, idle            bool
	}
	var found []sessionRow
	err := query(ctx, store, "sessions",
		`SELECT s.session_id, s.first_ts, s.last_ts, s.model, s.start_source, s.end_reason,
			(SELECT COUNT(*) FROM calls c WHERE c.session_id = s.session_id),
			(SELECT COUNT(*) FROM agents a WHERE a.session_id = s.session_id),
			s.cwd, s.host, s.tz_name, s.tz_offset_minutes, `+endedBeforeStart("s.session_id")+`,
			`+ranNothing("s.session_id")+`
		FROM sessions s WHERE `+where+` ORDER BY s.last_ts DESC, s.session_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var row sessionRow
			if err := r.Scan(
				&row.session, &row.first, &row.last, &row.model, &row.start, &row.end, &row.calls, &row.agents,
				&row.cwd, &row.host, &row.tzName, &row.tzOffset, &row.beforeStart, &row.idle,
			); err != nil {
				return err
			}
			found = append(found, row)
			return nil
		})
	if err != nil {
		return nil, err
	}
	var beforeStart, idle, noEnd, lostEnd int64
	for _, note := range []struct {
		condition string
		count     *int64
	}{
		{endedBeforeStart("s.session_id"), &beforeStart},
		{ranNothing("s.session_id"), &idle},
		{"s.end_reason = '" + callmeter.EndReasonNever + "'", &noEnd},
		{"s.end_reason = '" + callmeter.EndReasonLost + "'", &lostEnd},
	} {
		if err := query(ctx, store, "session note count",
			"SELECT COUNT(*) FROM sessions s WHERE "+where+" AND "+note.condition, args,
			func(r rowSource) error { return r.Scan(note.count) }); err != nil {
			return nil, err
		}
	}
	for _, row := range found {
		start := textCell(row.start)
		if row.beforeStart {
			start = "never"
		}
		model := textCell(row.model)
		if row.idle {
			if row.model != nil && *row.model != "" {
				model += " (unused)"
			}
		}
		t.Rows = append(t.Rows, []string{
			n.of(row.session), row.session, stampCell(row.first), stampCell(row.last), model,
			start, textCell(row.end), itoa(row.calls), itoa(row.agents), textCell(row.cwd),
			textCell(row.host), zoneCell(row.tzName, row.tzOffset),
		})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	if beforeStart > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d sessions ended before they started (START never): Claude Code sent a "+
			"SessionEnd and no SessionStart, and nothing ran in them; exiting while a /clear still runs its "+
			"SessionEnd hooks ends the cleared chat's successor so", beforeStart))
	}
	if noEnd > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d sessions ended with no SessionEnd (END %s): Claude Code did not run their "+
			"SessionEnd hooks, and no lost one was recorded; once they went quiet, the report settled their turns and calls "+
			"from their transcripts as SessionEnd would; a session idle past the quiet hour reads so too, until its next hook",
			noEnd, callmeter.EndReasonNever))
	}
	if lostEnd > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d sessions lost their SessionEnd (END %s): the SessionEnd hook ran, but its "+
			"event was not recorded (the hook was killed, its binary was unavailable, or the store stayed busy past the "+
			"hook's timeout; see the faults topic); once they went quiet, the report settled their turns and calls from "+
			"their transcripts as SessionEnd would", lostEnd, callmeter.EndReasonLost))
	}
	if idle > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d sessions ran nothing (MODEL unused): they started, then ended or idled "+
			"with no prompt submitted and no request, call or sub-agent (only local commands such as /model or /effort, "+
			"or an exit at the prompt); the model is the one they started with, never one that answered", idle))
	}
	return t, nil
}

// lostEventWindowMS is how long before a SessionEnd a lost event that names no
// session (a missed.log line: the binary was unavailable) may be that
// session's SessionStart.
const lostEventWindowMS = int64(60 * 60 * 1000)

// endedBeforeStart is the SQL truth that the session named by the column
// expression session ended before it started: its only rows are SessionEnd
// events, so Claude Code never sent its SessionStart and nothing ran in it.
// Live, exiting while a /clear still runs its SessionEnd hooks does this to the
// cleared chat's successor id. A fault of the session, or a lost event naming
// no session in the hour before its SessionEnd, rules it out: the lost event
// may be its SessionStart, and an unknown start is never shown as none.
func endedBeforeStart(session string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event = '%[2]s')
		AND NOT EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event <> '%[2]s')
		AND NOT EXISTS (SELECT 1 FROM calls x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM requests x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM agents x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM turns x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM faults x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM faults x, events y WHERE x.session_id IS NULL AND x.stage IN ('%[3]s', '%[4]s')
			AND y.session_id = %[1]s AND y.event = '%[2]s' AND x.ts BETWEEN y.ts - %[5]d AND y.ts))`,
		session, callmeter.EventSessionEnd, callmeter.StageBinary, callmeter.StageTerminated, lostEventWindowMS)
}

// idleEvents are the events a session sends with nothing run in it: its start
// and end, the instructions it loads, and the notice that it waits at the
// prompt. A local command such as /model or /effort fires no hook.
var idleEvents = []string{callmeter.EventSessionStart, callmeter.EventSessionEnd, "InstructionsLoaded", "Notification"}

// ranNothing is the SQL truth that the session named by the column expression
// session started and then ran nothing: a SessionStart, no event beyond
// idleEvents (so no prompt was submitted), and no request, call, sub-agent or
// turn. A fault of the session, or a lost event naming no session from its
// start on, rules it out: the lost event may be its prompt, and an unknown run
// is never shown as none.
func ranNothing(session string) string {
	return fmt.Sprintf(`(EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event = '%[2]s')
		AND NOT EXISTS (SELECT 1 FROM events x WHERE x.session_id = %[1]s AND x.event NOT IN (%[3]s))
		AND NOT EXISTS (SELECT 1 FROM calls x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM requests x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM agents x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM turns x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM faults x WHERE x.session_id = %[1]s)
		AND NOT EXISTS (SELECT 1 FROM faults x WHERE x.session_id IS NULL AND x.stage IN ('%[4]s', '%[5]s')
			AND x.ts >= (SELECT MIN(y.ts) FROM events y WHERE y.session_id = %[1]s AND y.event = '%[2]s')))`,
		session, callmeter.EventSessionStart, "'"+strings.Join(idleEvents, "', '")+"'", callmeter.StageBinary, callmeter.StageTerminated)
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
