package report

import (
	"context"
	"sort"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// promptRow is what the store holds about one prompt id.
type promptRow struct {
	id, session               string
	first, last               *int64
	calls, failed, agents     int64
	requests, context, output int64
}

// Prompts lists one row per prompt, most calls first: the prompt's first and
// last call, its calls (and how many failed), the sub-agents and the model
// requests it caused, and their context and output tokens, and WALL S, the
// seconds Claude Code measured for the prompt's turns (the sum of its turn
// durations, one decimal), "-" when none was recorded or the store has not
// gained the turn_durations table yet. A prompt is listed when any of its
// calls, requests or sub-agents is in the window; its wall time is the whole
// prompt's, whatever --since and --agent-type narrow.
func Prompts(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty: "callmeter: no prompts in window",
		Title: f.title("prompts", n),
		Header: []string{
			"PROMPT", "CHAT", "FIRST", "LAST", "CALLS", "FAILED", "AGENTS", "REQUESTS", "CONTEXT TOKENS", "OUTPUT TOKENS", "WALL S",
		},
	}
	byID := map[string]*promptRow{}
	row := func(id string, session *string) *promptRow {
		p := byID[id]
		if p == nil {
			p = &promptRow{id: id}
			byID[id] = p
		}
		if p.session == "" && session != nil {
			p.session = *session
		}
		return p
	}
	where, args := f.where()
	err := query(ctx, store, "prompt calls",
		`SELECT c.prompt_id, MIN(c.session_id), MIN(c.ts), MAX(c.ts), COUNT(*), COALESCE(SUM(c.failed), 0)
		FROM calls c WHERE c.prompt_id IS NOT NULL AND c.prompt_id != '' AND `+where+` GROUP BY c.prompt_id`, args,
		func(r rowSource) error {
			var id string
			var session *string
			var first, last *int64
			var calls, failed int64
			if err := r.Scan(&id, &session, &first, &last, &calls, &failed); err != nil {
				return err
			}
			p := row(id, session)
			p.first, p.last, p.calls, p.failed = first, last, calls, failed
			return nil
		})
	if err != nil {
		return nil, err
	}
	reqWhere, reqArgs := f.scoped(scope{
		ts: "r.ts", session: "r.session_id", agentType: "(SELECT a.agent_type FROM agents a WHERE a.agent_id = r.agent_id)",
	})
	err = query(ctx, store, "prompt requests",
		`SELECT r.prompt_id, MIN(r.session_id), COUNT(*), COALESCE(SUM(r.context_tokens), 0), COALESCE(SUM(r.output_tokens), 0)
		FROM requests r WHERE r.prompt_id IS NOT NULL AND r.prompt_id != '' AND `+reqWhere+` GROUP BY r.prompt_id`, reqArgs,
		func(r rowSource) error {
			var id string
			var session *string
			var requests, contextTokens, output int64
			if err := r.Scan(&id, &session, &requests, &contextTokens, &output); err != nil {
				return err
			}
			p := row(id, session)
			p.requests, p.context, p.output = requests, contextTokens, output
			return nil
		})
	if err != nil {
		return nil, err
	}
	agentWhere, agentArgs := f.scoped(scope{
		ts: "COALESCE(a.stopped, a.started)", session: "a.session_id", agentType: "a.agent_type",
	})
	err = query(ctx, store, "prompt agents",
		`SELECT a.prompt_id, MIN(a.session_id), COUNT(*) FROM agents a
		WHERE a.prompt_id IS NOT NULL AND a.prompt_id != '' AND `+agentWhere+` GROUP BY a.prompt_id`, agentArgs,
		func(r rowSource) error {
			var id string
			var session *string
			var agents int64
			if err := r.Scan(&id, &session, &agents); err != nil {
				return err
			}
			row(id, session).agents = agents
			return nil
		})
	if err != nil {
		return nil, err
	}
	walls := map[[2]string]int64{}
	if store.SchemaComplete() {
		wallWhere, wallArgs := f.scoped(scope{session: "d.session_id"})
		err = query(ctx, store, "prompt wall time",
			`SELECT d.session_id, d.prompt_id, SUM(d.duration_ms) FROM turn_durations d
			WHERE d.prompt_id IS NOT NULL AND d.prompt_id != '' AND d.duration_ms IS NOT NULL AND `+wallWhere+`
			GROUP BY d.session_id, d.prompt_id`, wallArgs,
			func(r rowSource) error {
				var session, id string
				var wall int64
				if err := r.Scan(&session, &id, &wall); err != nil {
					return err
				}
				walls[[2]string{session, id}] = wall
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	rows := make([]*promptRow, 0, len(byID))
	for _, p := range byID {
		rows = append(rows, p)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.calls != b.calls {
			return a.calls > b.calls
		}
		if la, lb := orZero(a.last), orZero(b.last); la != lb {
			return la > lb
		}
		return a.id < b.id
	})
	if len(rows) > f.limit() {
		rows = rows[:f.limit()]
	}
	for _, p := range rows {
		t.Rows = append(t.Rows, []string{
			p.id, n.of(p.session), stampCell(p.first), stampCell(p.last), itoa(p.calls), itoa(p.failed),
			itoa(p.agents), itoa(p.requests), itoa(p.context), itoa(p.output), wallCell(walls, p),
		})
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, true); err != nil {
		return nil, err
	}
	return t, nil
}

// wallCell is the prompt's summed turn duration in seconds; "-" when it has none.
func wallCell(walls map[[2]string]int64, p *promptRow) string {
	ms, ok := walls[[2]string{p.session, p.id}]
	if !ok {
		return "-"
	}
	return tenths(&ms)
}

func orZero(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
