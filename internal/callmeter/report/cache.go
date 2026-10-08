package report

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// Cache counts the window's requests by how the prompt cache served them, from
// the request_cache view: per party (the main chat or a sub-agent), the TTL of
// the cache entry the request could read (ENTRY TTL), its outcome and the
// cause the stored columns prove, and BREAK, the break kind wire.db records
// for the request ("-" when it has no row, "unknown" when its row has none);
// most requests first, --limit rows. A request whose read or expected read is
// unknown has outcome "-", never a miss. The coverage note counts the
// requests wire.db has a row for and states the reader: absent, ok or the
// warning. wire.db never changes an outcome or a cause. A newer release's view
// (Store.ViewVersions) is never read as this binary's: the topic answers a
// note naming both versions.
func Cache(ctx context.Context, store *callmeter.Store, f Filter, nameOf NameOf) (*Table, error) {
	n := newNames(nameOf)
	t := &Table{
		Empty:  "callmeter: no requests in window",
		Title:  f.title("cache", n),
		Header: []string{"PARTY", "ENTRY TTL", "OUTCOME", "CAUSE", "BREAK", "REQUESTS"},
	}
	var err error
	if !store.SchemaComplete() {
		t.Notes, err = gainedNotes(ctx, store, f, n, "request_cache view")
		return t, err
	}
	stored, shipped, err := store.ViewVersions(ctx, "request_cache")
	if err != nil {
		return nil, err
	}
	if stored > shipped {
		// a newer release's view: its columns and meanings are not this binary's
		t.Empty = "callmeter: the request_cache view is a newer callmeter's"
		t.Notes = []string{fmt.Sprintf(
			"this store's request_cache view is version %d, newer than this callmeter's %d; a newer callmeter reads it",
			stored, shipped)}
		return t, nil
	}
	type judged struct{ id, party, ttl, outcome, cause string }
	var requests []judged
	where, args := f.scoped(scope{ts: "r.ts", session: "r.session_id", agentType: "a.agent_type"})
	err = query(ctx, store, "request_cache",
		`SELECT rc.request_id, CASE WHEN COALESCE(r.agent_id, '') = '' THEN 'main chat' ELSE 'sub-agent' END,
			COALESCE(rc.entry_ttl, '-'), COALESCE(rc.outcome, '-'), COALESCE(rc.cause, '-')
		FROM request_cache rc JOIN requests r ON r.request_id = rc.request_id
		LEFT JOIN agents a ON a.agent_id = r.agent_id
		WHERE `+where, args,
		func(r rowSource) error {
			var j judged
			if err := r.Scan(&j.id, &j.party, &j.ttl, &j.outcome, &j.cause); err != nil {
				return err
			}
			requests = append(requests, j)
			return nil
		})
	if err != nil {
		return nil, err
	}
	wire := openWire(ctx, f.Wire)
	ids := make([]string, len(requests))
	for i, j := range requests {
		ids[i] = j.id
	}
	breaks := wire.breaks(ctx, ids)
	if err := wire.close(); err != nil {
		t.Failures = append(t.Failures, err)
	}
	if wire.err != nil {
		t.Failures = append(t.Failures, wire.err)
	}
	counts := map[[5]string]int64{}
	for _, j := range requests {
		brk := "-"
		if kind, ok := breaks[j.id]; ok {
			brk = "unknown"
			if kind != nil {
				brk = *kind
			}
		}
		counts[[5]string{j.party, j.ttl, j.outcome, j.cause, brk}]++
	}
	for key, count := range counts {
		t.Rows = append(t.Rows, append(key[:], itoa(count)))
	}
	slices.SortFunc(t.Rows, func(a, b []string) int {
		if c := counts[[5]string(b[:5])] - counts[[5]string(a[:5])]; c != 0 {
			if c > 0 {
				return 1
			}
			return -1
		}
		return strings.Compare(strings.Join(a, "\x00"), strings.Join(b, "\x00"))
	})
	if len(t.Rows) > f.limit() {
		t.Rows = t.Rows[:f.limit()]
	}
	if t.Notes, err = topicNotes(ctx, store, f, n, true); err != nil {
		return nil, err
	}
	t.Notes = append(t.Notes, fmt.Sprintf("wire data for %d of %d requests (wire.db: %s)", len(breaks), len(requests), wire.state))
	return t, nil
}
