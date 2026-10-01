package report

import (
	"context"
	"fmt"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Tokens sums the token split of the requests per model and agent type:
// requests, input, cache read, cache write (at 5 m and at 1 h when the usage
// carried the split, "-" when no request did), output, and the cache hit rate
// cache_read / (input + cache_read + cache_creation) as a percentage with one
// decimal, "-" when the denominator is zero. A request still pending has no
// tokens read yet: it is not counted, and the pending note names it.
func Tokens(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Title: f.title("tokens", n),
		Header: []string{
			"MODEL", "AGENT TYPE", "REQUESTS", "INPUT", "CACHE READ", "CACHE WRITE 5M", "CACHE WRITE 1H", "CACHE WRITE",
			"OUTPUT", "CACHE HIT %",
		},
	}
	where, args := f.scoped(scope{ts: "r.ts", session: "r.session_id", agentType: "a.agent_type"})
	err := query(ctx, store, "tokens",
		`SELECT COALESCE(NULLIF(r.model, ''), '-'), COALESCE(NULLIF(a.agent_type, ''), '-'), COUNT(*),
			COALESCE(SUM(r.input_tokens), 0), COALESCE(SUM(r.cache_read_tokens), 0),
			SUM(r.cache_creation_5m_tokens), SUM(r.cache_creation_1h_tokens),
			COALESCE(SUM(r.cache_creation_tokens), 0), COALESCE(SUM(r.output_tokens), 0)
		FROM requests r LEFT JOIN agents a ON a.agent_id = r.agent_id
		WHERE COALESCE(r.pending, 0) = 0 AND `+where+`
		GROUP BY 1, 2 ORDER BY COUNT(*) DESC, 1, 2 LIMIT ?`,
		append(args, f.limit()),
		func(r rowSource) error {
			var model, agentType string
			var requests, input, read, write, output int64
			var write5m, write1h *int64
			if err := r.Scan(&model, &agentType, &requests, &input, &read, &write5m, &write1h, &write, &output); err != nil {
				return err
			}
			t.Rows = append(t.Rows, []string{
				model, agentType, itoa(requests), itoa(input), itoa(read), intCell(write5m), intCell(write1h),
				itoa(write), itoa(output), hitRate(read, input+read+write),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, true); err != nil {
		return nil, err
	}
	return t, nil
}

// hitRate is read as a percentage of total with one decimal; a zero total is
// "-", never 0.0.
func hitRate(read, total int64) string {
	if total <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", float64(read)*100/float64(total))
}
