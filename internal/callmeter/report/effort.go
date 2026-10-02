package report

import (
	"context"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Effort counts, per effort level, permission mode and agent type, the calls,
// the turns (Stop and StopFailure events) and the failed calls. A value the
// store never recorded (the main chat has no agent type) shows "-".
func Effort(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no effort recorded in window",
		Title:  f.title("effort", n),
		Header: []string{"EFFORT", "PERMISSION MODE", "AGENT TYPE", "CALLS", "TURNS", "FAILED"},
	}
	callWhere, callArgs := f.where()
	turnWhere, turnArgs := f.scoped(scope{ts: "t.ts", session: "t.session_id", agentType: "t.agent_type"})
	err := query(ctx, store, "effort",
		`SELECT effort, permission_mode, agent_type, SUM(calls), SUM(turns), SUM(failed) FROM (
			SELECT COALESCE(NULLIF(c.effort, ''), '-') AS effort, COALESCE(NULLIF(c.permission_mode, ''), '-') AS permission_mode,
				COALESCE(NULLIF(c.agent_type, ''), '-') AS agent_type, 1 AS calls, 0 AS turns, COALESCE(c.failed, 0) AS failed
			FROM calls c WHERE `+callWhere+`
			UNION ALL
			SELECT COALESCE(NULLIF(t.effort, ''), '-'), COALESCE(NULLIF(t.permission_mode, ''), '-'),
				COALESCE(NULLIF(t.agent_type, ''), '-'), 0, 1, 0
			FROM turns t WHERE `+turnWhere+`
		) GROUP BY effort, permission_mode, agent_type
		ORDER BY SUM(calls) DESC, SUM(turns) DESC, effort, permission_mode, agent_type LIMIT ?`,
		append(append(callArgs, turnArgs...), f.limit()),
		func(r rowSource) error {
			var effort, mode, agentType string
			var calls, turns, failed int64
			if err := r.Scan(&effort, &mode, &agentType, &calls, &turns, &failed); err != nil {
				return err
			}
			t.Rows = append(t.Rows, []string{effort, mode, agentType, itoa(calls), itoa(turns), itoa(failed)})
			return nil
		})
	if err != nil {
		return nil, err
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	return t, nil
}
