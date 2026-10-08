package report

import (
	"context"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Agents lists one row per sub-agent turn, latest first: the agent and its
// type, the chat, the turn's number, when it started and stopped, how many
// seconds it ran and the prompt it served. An open turn shows stopped "-" and
// no seconds; a stop with no start shows started "-".
func Agents(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no sub-agents in window",
		Title:  f.title("agents", n),
		Header: []string{"AGENT", "TYPE", "CHAT", "TURN", "STARTED", "STOPPED", "SECONDS", "PROMPT"},
	}
	where, args := f.scoped(scope{
		ts: "COALESCE(t.stopped, t.started)", session: "t.session_id", agentType: "t.agent_type",
	})
	// The rows are read in full before any chat name is: a name lookup reads
	// the store, and the store has one connection.
	type turnRow struct {
		agent                      string
		agentType, session, prompt *string
		seq                        int64
		started, stopped           *int64
	}
	var found []turnRow
	err := query(ctx, store, "agent turns",
		`SELECT t.agent_id, t.agent_type, t.session_id, t.seq, t.started, t.stopped, t.prompt_id
		FROM agent_turns t WHERE `+where+`
		ORDER BY COALESCE(t.started, t.stopped) DESC, t.agent_id, t.seq LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var row turnRow
			if err := r.Scan(&row.agent, &row.agentType, &row.session, &row.seq, &row.started, &row.stopped, &row.prompt); err != nil {
				return err
			}
			found = append(found, row)
			return nil
		})
	if err != nil {
		return nil, err
	}
	for _, row := range found {
		chat := "-"
		if row.session != nil && *row.session != "" {
			chat = n.of(*row.session)
		}
		seconds := "-"
		if row.started != nil && row.stopped != nil {
			seconds = itoa((*row.stopped - *row.started) / 1000)
		}
		t.Rows = append(t.Rows, []string{
			row.agent, textCell(row.agentType), chat, itoa(row.seq), stampCell(row.started), stampCell(row.stopped),
			seconds, textCell(row.prompt),
		})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, false); err != nil {
		return nil, err
	}
	return t, nil
}
