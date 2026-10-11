package report

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// notGained is the note of a topic that reads a table or view the store has
// not gained yet (Store.SchemaComplete is false): the topic answers no rows.
// object is "{name} table" or "{name} view".
func notGained(object string) string {
	return fmt.Sprintf("this store has not gained the %s yet; the next hook or report adds it", object)
}

// gainedNotes is the notes of a topic over a table or view that may be
// missing (object, as notGained names it): the not-gained note when the store
// is incomplete, then the lifecycle gap notes.
func gainedNotes(ctx context.Context, store *callmeter.Store, f Filter, n *names, object string) ([]string, error) {
	var notes []string
	if !store.SchemaComplete() {
		notes = append(notes, notGained(object))
	}
	more, err := topicNotes(ctx, store, f, n, false)
	if err != nil {
		return nil, err
	}
	return append(notes, more...), nil
}

// tenths is a millisecond count as seconds with one decimal; NULL is "-".
func tenths(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return secondsCell(float64(*ms))
}

// secondsCell is a millisecond count as seconds with one decimal.
func secondsCell(ms float64) string { return fmt.Sprintf("%.1f", ms/1000) }

// compactDur is a millisecond count as a compact duration, rounded to the
// second: "45s", "5m07s", "2h03m".
func compactDur(ms int64) string {
	secs := (ms + 500) / 1000
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs >= 3600:
		return fmt.Sprintf("%dh%02dm", secs/3600, secs%3600/60)
	case secs >= 60:
		return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	return fmt.Sprintf("%ds", secs)
}

// medianMS is the median of ms, the mean of the two middle values when their
// count is even; xs is not changed. Zero for none.
func medianMS(xs []int64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]int64(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return float64(sorted[mid])
	}
	return float64(sorted[mid-1]+sorted[mid]) / 2
}

// percentileMS is the nearest-rank p-th percentile (0 < p <= 100) of ms; xs is
// not changed. Zero for none.
func percentileMS(xs []int64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]int64(nil), xs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	rank = max(1, min(rank, len(sorted)))
	return float64(sorted[rank-1])
}

func sumMS(xs []int64) int64 {
	var total int64
	for _, x := range xs {
		total += x
	}
	return total
}

// nameChats fills the first cell of each row of t with the chat name of the
// session in the same position of sessions ("?" for none). It runs after the
// rows are read: the name lookup uses the store's one connection, which a row
// being read holds.
func nameChats(n *names, t *Table, sessions []*string) {
	for i, session := range sessions {
		t.Rows[i][0] = "?"
		if session != nil {
			t.Rows[i][0] = n.of(*session)
		}
	}
}
