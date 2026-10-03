package report

import (
	"context"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// seedOutcomes stores two edits of one file and one of another, a commit and
// four test runs (two passed, one failed, one with no result).
func seedOutcomes(t *testing.T, store *callmeter.Store) {
	t.Helper()
	edit := func(id, file string, age time.Duration, added, removed int64) {
		call := mkCall(id, "s1", age, "Edit")
		call.FilePath, call.LinesAdded, call.LinesRemoved = callmeter.Ptr(file), i64(added), i64(removed)
		seed(t, store, call)
	}
	edit("e1", "/w/a.go", 3*time.Hour, 5, 2)
	edit("e2", "/w/a.go", 2*time.Hour, 3, 0)
	edit("e3", "/w/b.go", 5*time.Hour, 4, 0)
	commit := mkCall("g1", "s1", 90*time.Minute, "Bash")
	commit.CommitSHA, commit.CommitBranch = callmeter.Ptr("abc1234"), callmeter.Ptr("main")
	seed(t, store, commit)
	for _, run := range []struct {
		id     string
		failed *bool
	}{{"t1", callmeter.Ptr(false)}, {"t2", callmeter.Ptr(false)}, {"t3", callmeter.Ptr(true)}, {"t4", nil}} {
		call := mkCall(run.id, "s1", time.Hour, "Bash")
		call.TestRunner, call.Failed = callmeter.Ptr("go test"), run.failed
		seed(t, store, call)
	}
	seed(t, store, mkCall("plain", "s1", time.Hour, "Read"))
}

func TestOutcomesFoldsEditsCommitsAndTestsIntoOneTable(t *testing.T) {
	store := openStore(t)
	seedOutcomes(t, store)
	table, err := Outcomes(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Outcomes: %v", err)
	}
	wantHeader(t, table, "KIND", "CHAT", "SUBJECT", "BRANCH", "ADDED", "REMOVED", "PASSED", "FAILED", "LAST")
	wantRows(t, table,
		"test|chat-s1|go test|-|-|-|2|1|2026-09-23 11:00",
		"commit|chat-s1|abc1234|main|-|-|-|-|2026-09-23 10:30",
		"edit|chat-s1|/w/a.go|-|8|2|-|-|2026-09-23 10:00",
		"edit|chat-s1|/w/b.go|-|4|0|-|-|2026-09-23 07:00",
	)
}

func TestOutcomesFiltersNarrowAndLimitCaps(t *testing.T) {
	store := openStore(t)
	seedOutcomes(t, store)
	other := mkCall("o1", "s2", 20*time.Minute, "Edit")
	other.FilePath, other.LinesAdded, other.LinesRemoved = callmeter.Ptr("/w/c.go"), i64(1), i64(1)
	other.Cwd, other.AgentType = callmeter.Ptr("/w/q"), callmeter.Ptr("Explore")
	seed(t, store, other)
	ctx := context.Background()
	for name, c := range map[string]struct {
		filter Filter
		want   int
	}{
		"limit 3":   {Filter{Limit: 3}, 3},
		"session":   {Filter{Session: "s2"}, 1},
		"project":   {Filter{Project: "/w/q"}, 1},
		"agentType": {Filter{AgentType: "Explore"}, 1},
		"since":     {Filter{Since: testNow.Add(-100 * time.Minute)}, 3},
	} {
		table, err := Outcomes(ctx, store, c.filter, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(table.Rows) != c.want {
			t.Errorf("%s rows = %v, want %d", name, rowsOf(table), c.want)
		}
	}
}
