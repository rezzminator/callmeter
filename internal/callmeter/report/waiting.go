package report

import (
	"context"
	"fmt"
	"sort"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// waitKinds are the Notification types that mean Claude Code waits on the
// user, in the order their notes print.
var waitKinds = []string{"idle_prompt", "permission_prompt"}

// The hook events a wait is read from.
const (
	eventNotification     = "Notification"
	eventUserPromptSubmit = "UserPromptSubmit"
)

// wait is one stretch the session waited on the user: from the notification's
// ts to end, none while it is still open.
type wait struct {
	session, kind string
	start         int64
	end           *int64
}

// waitStats is the closed waits of a session and kind, and how many are open.
type waitStats struct {
	session, kind string
	waits, open   int
	durations     []int64
}

func (s *waitStats) add(w wait) {
	s.waits++
	if w.end == nil {
		s.open++
		return
	}
	s.durations = append(s.durations, *w.end-w.start)
}

// Waiting lists, per session and kind of wait (idle_prompt, permission_prompt),
// how long Claude Code waited on the user, longest total first. A wait runs
// from the Notification to the session's next UserPromptSubmit; a permission
// wait ends at the earlier of that and the session's next request (an answered
// permission lets the model's next request run). Notifications of one session
// and kind before the same end count once, from the earliest. A wait with no
// end yet is open: counted in WAITS, never summed.
func Waiting(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no waits in window",
		Title:  f.title("waiting", n),
		Header: []string{"CHAT", "KIND", "WAITS", "TOTAL", "MEDIAN", "MAX"},
	}
	where, args := f.scoped(scope{ts: "e.ts", session: "e.session_id"})
	kind := `CASE WHEN json_valid(e.detail) THEN json_extract(e.detail, '$.notification_type') END`
	// one wait per session, kind and end: the earliest notification of each
	type key struct {
		session, kind string
		end           int64
		open          bool
	}
	earliest := map[key]wait{}
	err := query(ctx, store, "waits",
		`SELECT e.session_id, `+kind+`, e.ts,
			(SELECT MIN(u.ts) FROM events u WHERE u.session_id = e.session_id AND u.event = ? AND u.ts > e.ts),
			(SELECT MIN(r.ts) FROM requests r WHERE r.session_id = e.session_id AND r.ts > e.ts)
		FROM events e WHERE e.event = ? AND `+kind+` IN (?, ?) AND e.session_id IS NOT NULL AND e.ts IS NOT NULL AND `+where,
		append([]any{eventUserPromptSubmit, eventNotification, waitKinds[0], waitKinds[1]}, args...),
		func(r rowSource) error {
			var session, notification string
			var ts int64
			var submit, request *int64
			if err := r.Scan(&session, &notification, &ts, &submit, &request); err != nil {
				return err
			}
			end := submit
			if notification == "permission_prompt" && request != nil && (end == nil || *request < *end) {
				end = request
			}
			w := wait{session: session, kind: notification, start: ts, end: end}
			k := key{session: session, kind: notification, open: end == nil}
			if end != nil {
				k.end = *end
			}
			if held, ok := earliest[k]; !ok || ts < held.start {
				earliest[k] = w
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	perRow := map[[2]string]*waitStats{}
	perKind := map[string]*waitStats{}
	for _, w := range earliest {
		if perKind[w.kind] == nil {
			perKind[w.kind] = &waitStats{kind: w.kind}
		}
		perKind[w.kind].add(w)
		rk := [2]string{w.session, w.kind}
		if perRow[rk] == nil {
			perRow[rk] = &waitStats{session: w.session, kind: w.kind}
		}
		perRow[rk].add(w)
	}
	rows := make([]*waitStats, 0, len(perRow))
	for _, s := range perRow {
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ta, tb := sumMS(a.durations), sumMS(b.durations); ta != tb {
			return ta > tb
		}
		if a.session != b.session {
			return a.session < b.session
		}
		return a.kind < b.kind
	})
	if len(rows) > f.limit() {
		rows = rows[:f.limit()]
	}
	for _, s := range rows {
		total, median, longest := "-", "-", "-"
		if len(s.durations) > 0 {
			total = compactDur(sumMS(s.durations))
			median = compactDur(int64(medianMS(s.durations)))
			longest = compactDur(int64(percentileMS(s.durations, 100)))
		}
		t.Rows = append(t.Rows, []string{n.of(s.session), s.kind, itoa(int64(s.waits)), total, median, longest})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	open := 0
	for _, k := range waitKinds {
		s := perKind[k]
		if s == nil {
			continue
		}
		open += s.open
		total, median := "-", "-"
		if len(s.durations) > 0 {
			total, median = compactDur(sumMS(s.durations)), compactDur(int64(medianMS(s.durations)))
		}
		t.Notes = append(t.Notes, fmt.Sprintf("%s: %d waits, total %s, median %s", k, s.waits, total, median))
	}
	if open > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d waits still open", open))
	}
	return t, nil
}
