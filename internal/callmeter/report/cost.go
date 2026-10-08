package report

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Cost lists Claude Code's own accounting per session, most expensive first:
// the cost in USD, the seconds spent in API calls, in API retries (api minus
// api without retries), in tools and on the wall, and the cost per model. The
// figures are the session's latest cumulative cost-state; callmeter computes
// none of them. A figure the snapshot did not carry is "-".
func Cost(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no cost-state recorded in window",
		Title:  f.title("cost", n),
		Header: []string{"CHAT", "COST USD", "API S", "RETRY S", "TOOL S", "WALL S", "MODELS"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "session_costs table")
		return t, err
	}
	var sessions []*string
	where, args := f.scoped(scope{ts: "s.ts", session: "s.session_id"})
	err = query(ctx, store, "session costs",
		`SELECT s.session_id, s.cost_usd, s.api_ms, s.api_ms - s.api_no_retry_ms, s.tool_ms, s.wall_ms, s.model_costs
		FROM session_costs s WHERE `+where+` ORDER BY s.cost_usd IS NULL, s.cost_usd DESC, s.session_id LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var session, models *string
			var cost *float64
			var api, retry, tool, wall *int64
			if err := r.Scan(&session, &cost, &api, &retry, &tool, &wall, &models); err != nil {
				return err
			}
			usd := "-"
			sessions = append(sessions, session)
			if cost != nil {
				usd = fmt.Sprintf("%.2f", *cost)
			}
			t.Rows = append(t.Rows, []string{"", usd, tenths(api), tenths(retry), tenths(tool), tenths(wall), modelCosts(models)})
			return nil
		})
	if err != nil {
		return nil, err
	}
	nameChats(n, t, sessions)
	var count int64
	var total float64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(s.cost_usd), 0) FROM session_costs s WHERE `+where, args...,
	).Scan(&count, &total); err != nil {
		return nil, fmt.Errorf("callmeter report: total the session costs: %w", err)
	}
	if t.Notes, err = gainedNotes(ctx, store, f, n, "session_costs table"); err != nil {
		return nil, err
	}
	if count > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%.2f USD in total over %d sessions (Claude Code's own accounting)", total, count))
	}
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
