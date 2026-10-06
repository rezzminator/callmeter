package report

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter"
)

// exec runs one SQL statement over the store, to seed the tables the metrics
// topics read without a writer API.
func exec(t *testing.T, store *callmeter.Store, statement string, args ...any) {
	t.Helper()
	if _, err := store.DB().Exec(statement, args...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// incompleteStore is a store that opened while another connection held the
// write lock, after the metrics tables and the thinking column were taken from
// it: SchemaComplete is false and those tables are not there.
func incompleteStore(t *testing.T) *callmeter.Store {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir() + "/callmeter.db"
	first, err := callmeter.OpenDB(ctx, path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	for _, statement := range []string{
		"ALTER TABLE requests DROP COLUMN thinking_tokens", "DROP TABLE compactions", "DROP TABLE session_costs",
		"DROP TABLE stop_hooks", "DROP TABLE stop_hook_runs", "DROP TABLE turn_durations",
		"INSERT INTO requests (request_id, session_id, ts, model, output_tokens, prompt_id) VALUES ('q', 's1', 1, 'opus', 9, 'p1')",
		"INSERT INTO calls (tool_use_id, session_id, ts, prompt_id) VALUES ('c', 's1', 1, 'p1')",
	} {
		exec(t, first, statement)
	}
	holder, err := first.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	store, err := callmeter.OpenDBWaiting(ctx, path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenDBWaiting while the write lock is held: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if err := holder.Close(); err != nil {
			t.Errorf("close holder: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Errorf("close first: %v", err)
		}
	})
	if store.SchemaComplete() {
		t.Fatal("SchemaComplete = true although the open could not write")
	}
	return store
}

// TestIncompleteStoreAnswersEachNewTableTopicWithNoRowsAndTheNote: a store that
// has not gained the tables is not queried for them; the topic answers an empty
// table and names why.
func TestIncompleteStoreAnswersEachNewTableTopicWithNoRowsAndTheNote(t *testing.T) {
	store := incompleteStore(t)
	for table, topic := range map[string]func(context.Context, *callmeter.Store, Filter, NameOf) (*Table, error){
		"compactions": Compactions, "session_costs": Cost, "stop_hooks": Hooks, "turn_durations": Turns,
	} {
		got, err := topic(context.Background(), store, Filter{}, chatOf)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		want := "this store has not gained the " + table + " table yet; the next hook or report adds it"
		if len(got.Rows) != 0 || !slices.Contains(got.Notes, want) {
			t.Errorf("%s: rows %v notes %q, want no rows and %q", table, rowsOf(got), got.Notes, want)
		}
	}
}

// TestIncompleteStoreShowsTokensAndPromptsMetricsAsDashes: the thinking column
// and the turn_durations table are not named, and their cells are "-".
func TestIncompleteStoreShowsTokensAndPromptsMetricsAsDashes(t *testing.T) {
	store := incompleteStore(t)
	tokens, err := Tokens(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	wantRows(t, tokens, "opus|-|1|0|0|-|-|0|9|-|-|-")
	prompts, err := Prompts(context.Background(), store, Filter{}, chatOf)
	if err != nil {
		t.Fatalf("Prompts: %v", err)
	}
	wantRows(t, prompts, "p1|chat-s1|1970-01-01 00:00|1970-01-01 00:00|1|0|0|1|0|9|-")
}

func TestCompactDurIsHoursMinutesThenMinutesSecondsThenSeconds(t *testing.T) {
	for ms, want := range map[int64]string{
		0: "0s", 499: "0s", 500: "1s", 45_000: "45s", 307_000: "5m07s", 3_599_000: "59m59s", 7_380_000: "2h03m", -5: "0s",
	} {
		if got := compactDur(ms); got != want {
			t.Errorf("compactDur(%d) = %q, want %q", ms, got, want)
		}
	}
}

func TestMedianAndPercentile(t *testing.T) {
	xs := []int64{9, 1, 5, 3}
	if got := medianMS(xs); got != 4 {
		t.Errorf("median of 1 3 5 9 = %v, want 4", got)
	}
	if got := medianMS([]int64{7, 1, 3}); got != 3 {
		t.Errorf("median of 1 3 7 = %v, want 3", got)
	}
	if got := percentileMS(xs, 90); got != 9 {
		t.Errorf("p90 of 1 3 5 9 = %v, want 9", got)
	}
	ten := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if got := percentileMS(ten, 90); got != 9 {
		t.Errorf("p90 of 1..10 = %v, want 9", got)
	}
	if !slices.Equal(xs, []int64{9, 1, 5, 3}) {
		t.Errorf("the input was reordered: %v", xs)
	}
	if medianMS(nil) != 0 || percentileMS(nil, 90) != 0 {
		t.Error("an empty input must give 0")
	}
}

// TestChatNamesAreLookedUpAfterTheRowsAreRead: a chat-name lookup that queries
// the store (the CLI's does) must not run while a topic's rows are open: the
// store has one connection, and the lookup would wait on it for good.
func TestChatNamesAreLookedUpAfterTheRowsAreRead(t *testing.T) {
	store := openStore(t)
	exec(t, store, `INSERT INTO compactions (entry_id, session_id, ts, trigger) VALUES ('e', 's1', ?, 'auto')`, ms(time.Hour))
	exec(t, store, `INSERT INTO session_costs (session_id, ts, cost_usd) VALUES ('s1', ?, 1)`, ms(time.Hour))
	exec(t, store, `INSERT INTO turn_durations (entry_id, session_id, ts) VALUES ('d', 's1', ?)`, ms(time.Hour))
	exec(t, store, `INSERT INTO events (event_id, event, ts, session_id, source) VALUES ('r', 'SessionStart', ?, 's1', 'resume')`, ms(time.Hour))
	exec(t, store, `INSERT INTO events (event_id, event, ts, session_id, detail) VALUES ('n', 'Notification', ?, 's1',
		'{"notification_type":"idle_prompt"}')`, ms(time.Hour))
	lookup := func(session string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var one int
		if err := store.DB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return "", err
		}
		return "chat-" + session, nil
	}
	for name, topic := range map[string]func(context.Context, *callmeter.Store, Filter, NameOf) (*Table, error){
		"compactions": Compactions, "cost": Cost, "turns": Turns, "resumes": Resumes, "waiting": Waiting,
	} {
		table, err := topic(context.Background(), store, Filter{}, lookup)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(table.Rows) != 1 || table.Rows[0][0] != "chat-s1" {
			t.Errorf("%s rows = %v notes %q, want the chat name looked up", name, rowsOf(table), table.Notes)
		}
	}
}
