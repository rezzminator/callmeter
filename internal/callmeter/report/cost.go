package report

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
	"github.com/rezzminator/callmeter/internal/paths"
)

// CostViews are the views of the cost topic, the first the default.
var CostViews = []string{"chat", "agent", "call"}

// Cost is the money the store knows, in one of three views (Filter.CostView).
//
// chat: per chat, the sum of its requests' prices (REQUESTS USD), Claude Code's
// own total for it (CLAUDE CODE USD, the session's latest cumulative cost-state;
// callmeter computes none of that), their difference (DIFF USD = Claude Code's
// total minus the request sum: the spend no priced request row accounts for),
// the part of the request sum from calls no transcript records (SIDE USD), the
// spend the cost ledger never saw (PRE-LEDGER USD = Claude Code's total minus
// the ledger-priced calls up to the exit that total was written at: a resumed
// chat keeps its restored total, so what the ledger never saw stays in it), and
// Claude Code's seconds in API calls, retries, tools and on the wall, and its
// cost per model. A figure the store does not hold is "-", never zero.
//
// agent: per chat and agent, its requests, price and the parts of it priced by
// the ledger, priced from the table and from side calls.
//
// call: the dearest requests, each with its price and where the price came from.
//
// A request's price is cost_usd_micros: cost_source 'ledger' is Claude Code's
// own price of the call from the cost ledger, 'table' callmeter's embedded price
// list at the row's tokens, and NULL a model the list does not name. A GPT
// request's table price is a list-equivalent, not a bill: that route runs on a
// ChatGPT subscription, and every view notes the GPT part of its sum.
func Cost(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	switch f.CostView {
	case "", "chat":
		return costChats(ctx, store, f, n)
	case "agent":
		return costAgents(ctx, store, f, n)
	case "call":
		return costCalls(ctx, store, f, n)
	}
	return nil, fmt.Errorf("callmeter report: cost view %q is none of %s", f.CostView, strings.Join(CostViews, ", "))
}

// exitGraceMS is how long after the exit a cost-state was written at a ledger-
// priced call still belongs to that launch: a call's ledger row trails its
// transcript line by up to a quarter second, and a reload starts the next
// launch's first call seconds later.
const exitGraceMS = 2000

// costExit is the SQL for the exit a session's latest cost-state was written
// at, Unix ms, NULL when unknown: the later of started + wall_ms and the
// session's latest SessionEnd event (the session_costs row aliased s, the
// SessionEnd event name its one argument). started + wall_ms is the exit's
// wall clock on a session's first launch only: Claude Code keeps startTime
// across a resume while totalDuration counts no time between launches, so a
// resumed session's clock lags its exit by those gaps, and the SessionEnd hook,
// which fires at the exit that wrote the latest cost-state, places it.
const costExit = `(SELECT MAX(COALESCE(clock, ended), COALESCE(ended, clock)) FROM (SELECT
		CASE WHEN s.started > 0 AND s.wall_ms > 0 THEN s.started + s.wall_ms END AS clock,
		(SELECT MAX(e.ts) FROM events e WHERE e.session_id = s.session_id AND e.event = ?) AS ended))`

// usd is a USD-micros sum as dollars; NULL is "-".
func usd(micros sql.NullInt64, places int) string {
	if !micros.Valid {
		return "-"
	}
	return fmt.Sprintf("%.*f", places, float64(micros.Int64)/1e6)
}

// costWhere is the requests filter of the cost views: settled requests of a
// known session inside the Filter's window and session.
func costWhere(f Filter) (string, []any) {
	where, args := f.scoped(scope{ts: "r.ts", session: "r.session_id"})
	return "COALESCE(r.pending, 0) = 0 AND r.session_id IS NOT NULL AND " + where, args
}

type chatCost struct {
	session                string
	cc                     *float64
	exit                   *int64 // costExit
	api, retry, tool, wall *int64
	models                 *string
	requests               int64
	micros, side           sql.NullInt64
	ledgered               int64 // the ledger-priced calls of the chat
	ledgeredToExit         int64 // their price in USD micros, the calls up to its cost-state's exit
}

// preLedger is the chat's PRE-LEDGER USD cell: Claude Code's total minus the
// ledger-priced calls up to the exit its cost-state was written at (costExit),
// floored at zero. "-" while any of the three is unknown: the chat has no
// ledger-priced call (no ledger to precede), no total, or no exit (no clock
// and no SessionEnd).
func (c *chatCost) preLedger() string {
	if c.cc == nil || c.ledgered == 0 || c.exit == nil {
		return "-"
	}
	before := max(int64(math.Round(*c.cc*1e6))-c.ledgeredToExit, 0)
	return fmt.Sprintf("%.2f", float64(before)/1e6)
}

func costChats(ctx context.Context, store *callmeter.Store, f Filter, n *names) (*Table, error) {
	t := &Table{
		Empty:  "callmeter: no cost recorded in window",
		Title:  f.title("cost", n),
		Header: []string{"CHAT", "REQUESTS USD", "CLAUDE CODE USD", "DIFF USD", "PRE-LEDGER USD", "SIDE USD", "API S", "RETRY S", "TOOL S", "WALL S", "MODELS"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "session_costs table")
		t.Notes = append(t.Notes, "side calls are not counted: this store has not gained the cost columns yet; the next hook or report adds them")
		return t, err
	}
	chats := map[string]*chatCost{}
	where, args := f.scoped(scope{ts: "s.ts", session: "s.session_id"})
	err = query(ctx, store, "session costs",
		`SELECT s.session_id, s.cost_usd, `+costExit+`, s.api_ms, s.api_ms - s.api_no_retry_ms, s.tool_ms, s.wall_ms, s.model_costs
		FROM session_costs s WHERE `+where,
		append([]any{callmeter.EventSessionEnd}, args...),
		func(r rowSource) error {
			c := &chatCost{}
			if err := r.Scan(&c.session, &c.cc, &c.exit, &c.api, &c.retry, &c.tool, &c.wall, &c.models); err != nil {
				return err
			}
			chats[c.session] = c
			return nil
		})
	if err != nil {
		return nil, err
	}
	err = query(ctx, store, "ledger-priced calls",
		`SELECT s.session_id, COUNT(*),
			COALESCE(SUM(CASE WHEN r.ts <= `+costExit+` + ? THEN r.cost_usd_micros END), 0)
		FROM session_costs s JOIN requests r ON r.session_id = s.session_id
		WHERE r.cost_source = 'ledger' AND r.cost_usd_micros IS NOT NULL AND COALESCE(r.pending, 0) = 0 AND `+where+`
		GROUP BY s.session_id`,
		append([]any{callmeter.EventSessionEnd, exitGraceMS}, args...),
		func(r rowSource) error {
			var session string
			var count, toExit int64
			if err := r.Scan(&session, &count, &toExit); err != nil {
				return err
			}
			if c := chats[session]; c != nil {
				c.ledgered, c.ledgeredToExit = count, toExit
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	var ccCount int64
	var ccTotal float64
	for _, c := range chats {
		ccCount++
		if c.cc != nil {
			ccTotal += *c.cc
		}
	}
	reqWhere, reqArgs := costWhere(f)
	err = query(ctx, store, "request costs",
		`SELECT r.session_id, COUNT(*), SUM(r.cost_usd_micros), SUM(CASE WHEN r.source = 'ledger' THEN r.cost_usd_micros END)
		FROM requests r WHERE `+reqWhere+` GROUP BY r.session_id`,
		reqArgs,
		func(r rowSource) error {
			var session string
			var count int64
			var micros, side sql.NullInt64
			if err := r.Scan(&session, &count, &micros, &side); err != nil {
				return err
			}
			c := chats[session]
			if c == nil {
				c = &chatCost{session: session}
				chats[session] = c
			}
			c.requests, c.micros, c.side = count, micros, side
			return nil
		})
	if err != nil {
		return nil, err
	}
	ordered := make([]*chatCost, 0, len(chats))
	for _, c := range chats {
		ordered = append(ordered, c)
	}
	dearest := func(c *chatCost) float64 {
		most := 0.0
		if c.cc != nil {
			most = *c.cc
		}
		if c.micros.Valid {
			most = max(most, float64(c.micros.Int64)/1e6)
		}
		return most
	}
	sort.Slice(ordered, func(i, j int) bool {
		if a, b := dearest(ordered[i]), dearest(ordered[j]); a != b {
			return a > b
		}
		return ordered[i].session < ordered[j].session
	})
	if len(ordered) > f.limit() {
		ordered = ordered[:f.limit()]
	}
	ledgerRead := f.Ledger != nil && f.Ledger.Present
	var sessions []*string
	for _, c := range ordered {
		session := c.session
		sessions = append(sessions, &session)
		cc, diff := "-", "-"
		if c.cc != nil {
			cc = fmt.Sprintf("%.2f", *c.cc)
			if c.micros.Valid {
				diff = fmt.Sprintf("%+.2f", *c.cc-float64(c.micros.Int64)/1e6)
			}
		}
		side := usd(c.side, 2)
		if !c.side.Valid && c.micros.Valid && ledgerRead {
			side = "0.00"
		}
		t.Rows = append(t.Rows, []string{"", usd(c.micros, 2), cc, diff, c.preLedger(), side, tenths(c.api), tenths(c.retry), tenths(c.tool), tenths(c.wall), modelCosts(c.models)})
	}
	nameChats(n, t, sessions)
	if t.Notes, err = gainedNotes(ctx, store, f, n, "session_costs table"); err != nil {
		return nil, err
	}
	if len(t.Rows) > 0 {
		t.Notes = append(t.Notes,
			"REQUESTS USD sums each request's cost: 'ledger' is Claude Code's own price from the cost ledger, 'table' is callmeter's embedded price list at the row's tokens",
			"DIFF USD is Claude Code's total minus REQUESTS USD: spend Claude Code accounts for that no priced request row does",
			"SIDE USD is the part of REQUESTS USD from calls no transcript records (the cost ledger's side calls)",
			"PRE-LEDGER USD is Claude Code's total minus the cost ledger's calls up to the exit that total was written at: spend the ledger never saw (before it began, in a launch without it, or calls it never received), that no agent owns; '-' when the chat has no ledger-priced call or no known exit")
	}
	gaps, err := costGaps(ctx, store, reqWhere, reqArgs)
	if err != nil {
		return nil, err
	}
	t.Notes = append(t.Notes, gaps...)
	t.Notes = append(t.Notes, ledgerNotes(f.Ledger)...)
	if ccCount > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%.2f USD in total over %d sessions (Claude Code's own accounting)", ccTotal, ccCount))
	}
	return t, nil
}

// costGaps names what the request prices leave out, counted over the window.
func costGaps(ctx context.Context, store *callmeter.Store, where string, args []any) ([]string, error) {
	var unpriced, undecided, unknownAgent, gpt, gptMicros int64
	err := store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(r.cost_usd_micros IS NULL), 0),
			COALESCE(SUM(r.source = 'ledger' AND r.cost_source = 'table'), 0),
			COALESCE(SUM(r.source = 'ledger' AND r.agent_id = '?'), 0),
			COALESCE(SUM(lower(r.model) LIKE 'gpt-%' AND r.cost_usd_micros IS NOT NULL), 0),
			COALESCE(SUM(CASE WHEN lower(r.model) LIKE 'gpt-%' THEN r.cost_usd_micros END), 0)
		FROM requests r WHERE `+where, args...).Scan(&unpriced, &undecided, &unknownAgent, &gpt, &gptMicros)
	if err != nil {
		return nil, fmt.Errorf("callmeter report: count the unpriced requests: %w", err)
	}
	var notes []string
	if unpriced > 0 {
		notes = append(notes, plural(unpriced,
			"1 request has no price (a model the table does not list): not in REQUESTS USD",
			"%d requests have no price (a model the table does not list): not in REQUESTS USD"))
	}
	if undecided > 0 {
		notes = append(notes, plural(undecided,
			"1 side call has no ledger price yet: priced from the table, its cache writes taken as 5-minute ones",
			"%d side calls have no ledger price yet: priced from the table, their cache writes taken as 5-minute ones"))
	}
	if unknownAgent > 0 {
		notes = append(notes, plural(unknownAgent,
			"1 side call has an unknown agent (the ledger's log half arrived without its span)",
			"%d side calls have an unknown agent (the ledger's log half arrived without its span)"))
	}
	if gpt > 0 {
		notes = append(notes, plural(gpt,
			fmt.Sprintf("1 GPT request, %.2f USD of the total, is list-equivalent: the GPT route runs on a ChatGPT subscription, so there is no per-token bill for it", float64(gptMicros)/1e6),
			fmt.Sprintf("%%d GPT requests, %.2f USD of the total, are list-equivalent: the GPT route runs on a ChatGPT subscription, so there is no per-token bill for them", float64(gptMicros)/1e6)))
	}
	return notes, nil
}

// plural is one for n == 1, else many with n filled in.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

// ledgerNotes names the cost ledger's state: whether side calls are counted at
// all, and every gap its ingest found.
func ledgerNotes(l *callmeter.LedgerSummary) []string {
	switch {
	case l != nil && l.Err != "":
		return []string{"side calls are not counted: the cost ledger could not be read: " + l.Err}
	case l == nil:
		return []string{"side calls are not counted: no cost ledger was read"}
	case !l.Present && l.Dir == "":
		return []string{"side calls are not counted: no cost ledger is set; " + paths.EnvCostLedgerDir + " names its directory"}
	case !l.Present:
		return []string{"side calls are not counted: no cost ledger at " + l.Dir}
	case l.SchemaIncomplete:
		return []string{"side calls are not counted: this store has not gained the cost columns yet; the next hook or report adds them"}
	}
	notes := []string{fmt.Sprintf("cost ledger %s: %d calls in the window, %d held by a transcript, %d side calls", l.Dir, l.Rows, l.Held, l.Side)}
	if l.Faults > 0 {
		notes = append(notes, plural(int64(l.Faults),
			"1 ledger line was skipped: not a JSON object, or missing a field the ledger contract requires",
			"%d ledger lines were skipped: not a JSON object, or missing a field the ledger contract requires"))
	}
	if l.HeldUnrecorded > 0 {
		notes = append(notes, plural(int64(l.HeldUnrecorded),
			"1 ledger call is held by a transcript the store has not recorded yet: not counted until a hook or recovery writes it",
			"%d ledger calls are held by a transcript the store has not recorded yet: not counted until a hook or recovery writes them"))
	}
	if l.Unreadable > 0 {
		notes = append(notes, plural(int64(l.Unreadable),
			"1 session's transcripts could not be read: its ledger calls the store did not know are counted as side calls",
			"%d sessions' transcripts could not be read: their ledger calls the store did not know are counted as side calls"))
	}
	return notes
}

func costAgents(ctx context.Context, store *callmeter.Store, f Filter, n *names) (*Table, error) {
	t := &Table{
		Empty:  "callmeter: no requests in window",
		Title:  f.title("cost · agent view", n),
		Header: []string{"CHAT", "AGENT", "TYPE", "REQUESTS", "USD", "LEDGER USD", "TABLE USD", "SIDE USD"},
	}
	if !store.SchemaComplete() {
		notes, err := gainedNotes(ctx, store, f, n, "cost columns")
		t.Notes = notes
		return t, err
	}
	where, args := costWhere(f)
	var sessions []*string
	err := query(ctx, store, "agent costs",
		`SELECT r.session_id, COALESCE(r.agent_id, ''), COALESCE(NULLIF(a.agent_type, ''), '-'), COUNT(*), SUM(r.cost_usd_micros),
			COALESCE(SUM(CASE WHEN r.cost_source = 'ledger' THEN r.cost_usd_micros END), 0),
			COALESCE(SUM(CASE WHEN r.cost_source = 'table' THEN r.cost_usd_micros END), 0),
			COALESCE(SUM(CASE WHEN r.source = 'ledger' THEN r.cost_usd_micros END), 0)
		FROM requests r LEFT JOIN agents a ON a.agent_id = r.agent_id
		WHERE `+where+` GROUP BY r.session_id, COALESCE(r.agent_id, '')
		ORDER BY COALESCE(SUM(r.cost_usd_micros), 0) DESC, r.session_id, COALESCE(r.agent_id, '') LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var session, agent, agentType string
			var count int64
			var total sql.NullInt64
			var ledger, table, side int64
			if err := r.Scan(&session, &agent, &agentType, &count, &total, &ledger, &table, &side); err != nil {
				return err
			}
			switch agent {
			case "":
				agent = "main"
			case callmeter.UnknownLedgerAgent:
				agent = "? (unknown)"
			}
			sessions = append(sessions, &session)
			t.Rows = append(t.Rows, []string{"", agent, agentType, itoa(count), usd(total, 2),
				usd(sql.NullInt64{Int64: ledger, Valid: true}, 2), usd(sql.NullInt64{Int64: table, Valid: true}, 2),
				usd(sql.NullInt64{Int64: side, Valid: true}, 2)})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	var gaps []string
	if t.Notes, err = gainedNotes(ctx, store, f, n, "cost columns"); err != nil {
		return nil, err
	}
	if gaps, err = costGaps(ctx, store, where, args); err != nil {
		return nil, err
	}
	t.Notes = append(t.Notes,
		"USD sums each request's cost: LEDGER USD the part Claude Code priced itself (cost ledger), TABLE USD the part priced from callmeter's embedded price list, SIDE USD the part from calls no transcript records",
		"an agent's USD includes its own side calls (web searches, summaries, suggestions), not its sub-agents' requests",
		"the spend before the ledger belongs to no agent: the chat view's PRE-LEDGER USD names it")
	t.Notes = append(t.Notes, gaps...)
	t.Notes = append(t.Notes, ledgerNotes(f.Ledger)...)
	return t, nil
}

func costCalls(ctx context.Context, store *callmeter.Store, f Filter, n *names) (*Table, error) {
	t := &Table{
		Empty:  "callmeter: no requests in window",
		Title:  f.title("cost · call view", n),
		Header: []string{"CHAT", "TIME", "AGENT", "QUERY SOURCE", "MODEL", "IN", "CACHE READ", "CACHE WRITE", "OUT", "USD", "PRICE FROM", "SIDE CALL"},
	}
	if !store.SchemaComplete() {
		notes, err := gainedNotes(ctx, store, f, n, "cost columns")
		t.Notes = notes
		return t, err
	}
	where, args := costWhere(f)
	var sessions []*string
	err := query(ctx, store, "call costs",
		`SELECT r.session_id, r.ts, COALESCE(r.agent_id, ''), r.query_source, r.model, r.input_tokens, r.cache_read_tokens,
			r.cache_creation_tokens, r.output_tokens, r.cost_usd_micros, r.cost_source, COALESCE(r.source, '')
		FROM requests r WHERE `+where+`
		ORDER BY r.cost_usd_micros IS NULL, r.cost_usd_micros DESC, r.ts DESC, r.request_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var session, source string
			var ts, in, read, write, out *int64
			var agent string
			var querySource, model, priceFrom *string
			var micros sql.NullInt64
			if err := r.Scan(&session, &ts, &agent, &querySource, &model, &in, &read, &write, &out, &micros, &priceFrom, &source); err != nil {
				return err
			}
			if agent == "" {
				agent = "main"
			}
			side := "-"
			if source == callmeter.SourceLedger {
				side = "yes"
			}
			sessions = append(sessions, &session)
			t.Rows = append(t.Rows, []string{"", stampCell(ts), agent, textCell(querySource), textCell(model), intCell(in), intCell(read),
				intCell(write), intCell(out), usd(micros, 4), textCell(priceFrom), side})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	var err2 error
	if t.Notes, err2 = gainedNotes(ctx, store, f, n, "cost columns"); err2 != nil {
		return nil, err2
	}
	gaps, err := costGaps(ctx, store, where, args)
	if err != nil {
		return nil, err
	}
	t.Notes = append(t.Notes, "USD is the request's cost; PRICE FROM is 'ledger' (Claude Code's own price from the cost ledger), 'table' (callmeter's embedded price list at the row's tokens; for a GPT model a list-equivalent, not a bill) or - (a model the list does not name); SIDE CALL marks calls no transcript records")
	t.Notes = append(t.Notes, gaps...)
	t.Notes = append(t.Notes, ledgerNotes(f.Ledger)...)
	return t, nil
}

// modelCosts is the stored {model: usd} JSON as `model=usd` pairs, the most
// expensive first, two decimals; "-" when there is none or it is not that JSON.
func modelCosts(raw *string) string {
	if raw == nil {
		return "-"
	}
	var costs map[string]float64
	if err := json.Unmarshal([]byte(*raw), &costs); err != nil || len(costs) == 0 {
		return "-"
	}
	models := make([]string, 0, len(costs))
	for model := range costs {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool {
		if costs[models[i]] != costs[models[j]] {
			return costs[models[i]] > costs[models[j]]
		}
		return models[i] < models[j]
	})
	pairs := make([]string, len(models))
	for i, model := range models {
		pairs[i] = fmt.Sprintf("%s=%.2f", model, costs[model])
	}
	return strings.Join(pairs, " ")
}
