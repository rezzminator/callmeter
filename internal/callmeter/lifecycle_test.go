package callmeter

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// permutations lists every ordering of items.
func permutations[T any](items []T) [][]T {
	if len(items) <= 1 {
		return [][]T{append([]T(nil), items...)}
	}
	var out [][]T
	for i := range items {
		rest := append(append([]T(nil), items[:i]...), items[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]T{items[i]}, tail...))
		}
	}
	return out
}

// subagentEdge is a SubagentStart or SubagentStop event of agent "a1".
func subagentEdge(id, event string, ts int64) Event {
	return Event{EventID: id, Event: event, TS: ts, SessionID: Ptr("sess-1"), AgentID: Ptr("a1"), AgentType: Ptr("Explore")}
}

// agentTurnRows lists the agent_turns of one agent as "seq(started,stopped)",
// NULL as "-".
func agentTurnRows(t *testing.T, store *Store, agentID string) string {
	t.Helper()
	return keys(t, store.DB(), fmt.Sprintf(`SELECT seq || '(' || COALESCE(started, '-') || ',' || COALESCE(stopped, '-') || ')'
		FROM agent_turns WHERE agent_id = '%s' ORDER BY seq`, agentID))
}

func TestInsertEventDedupsOnEventID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	event := Event{
		EventID: "abc123", Event: "Notification", TS: 5, SessionID: Ptr("sess-1"), PromptBytes: Ptr(int64(42)),
		Detail: Ptr(`{"level":"info"}`),
	}
	var inserted []bool
	if err := store.Batch(ctx, func(tx *Tx) error {
		for range 2 {
			ok, err := tx.InsertEvent(ctx, event)
			if err != nil {
				return err
			}
			inserted = append(inserted, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 2 || !inserted[0] || inserted[1] {
		t.Errorf("InsertEvent twice returned %v, want [true false]", inserted)
	}
	if n := count(t, store, "events"); n != 1 {
		t.Fatalf("events holds %d rows, want 1", n)
	}
	got := row(t, store, "events", "event_id = ?", "abc123")
	if got["event"] != "Notification" || got["ts"] != int64(5) || got["prompt_bytes"] != int64(42) || got["detail"] != `{"level":"info"}` || got["agent_id"] != nil {
		t.Errorf("event row = %v", got)
	}
}

func TestInsertTurnDedupsOnEventID(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	turn := Turn{
		EventID: "def456", Event: "Stop", TS: 9, SessionID: Ptr("sess-1"), LastAssistantMessageBytes: Ptr(int64(120)),
		StopHookActive: Ptr(true), BackgroundTasks: Ptr(`[{"id":"b1"}]`),
	}
	var inserted []bool
	if err := store.Batch(ctx, func(tx *Tx) error {
		for range 2 {
			ok, err := tx.InsertTurn(ctx, turn)
			if err != nil {
				return err
			}
			inserted = append(inserted, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 2 || !inserted[0] || inserted[1] {
		t.Errorf("InsertTurn twice returned %v, want [true false]", inserted)
	}
	got := row(t, store, "turns", "event_id = ?", "def456")
	if n := count(t, store, "turns"); n != 1 || got["last_assistant_message_bytes"] != int64(120) || got["stop_hook_active"] != int64(1) {
		t.Errorf("turns holds %d rows, row = %v; want 1 with 120 bytes and stop_hook_active 1", n, got)
	}
}

func TestInsertRefusesAnEmptyKeyOrName(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	for name, insert := range map[string]func(*Tx) (bool, error){
		"event without an id":  func(tx *Tx) (bool, error) { return tx.InsertEvent(ctx, Event{Event: "Notification"}) },
		"event without a name": func(tx *Tx) (bool, error) { return tx.InsertEvent(ctx, Event{EventID: "x"}) },
		"turn without an id":   func(tx *Tx) (bool, error) { return tx.InsertTurn(ctx, Turn{Event: "Stop"}) },
		"turn without a name":  func(tx *Tx) (bool, error) { return tx.InsertTurn(ctx, Turn{EventID: "x"}) },
	} {
		err := store.Batch(ctx, func(tx *Tx) error {
			ok, err := insert(tx)
			if ok {
				t.Errorf("%s: reported a row inserted", name)
			}
			return err
		})
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestAgentTurnsWalk: the turns of one agent are a function of its start and
// stop events, never of the order they were inserted in; each insert is
// followed by the rebuild, as the hook does.
func TestAgentTurnsWalk(t *testing.T) {
	ctx := context.Background()
	for name, scenario := range map[string]struct {
		edges []Event
		want  string
	}{
		"two whole turns": {
			[]Event{
				subagentEdge("e1", EventSubagentStart, 1), subagentEdge("e2", EventSubagentStop, 2),
				subagentEdge("e3", EventSubagentStart, 3), subagentEdge("e4", EventSubagentStop, 4),
			},
			"1(1,2),2(3,4)",
		},
		"orphan stop": {[]Event{subagentEdge("e2", EventSubagentStop, 2)}, "1(-,2)"},
		"double start": {
			[]Event{
				subagentEdge("e1", EventSubagentStart, 1), subagentEdge("e3", EventSubagentStart, 3),
				subagentEdge("e4", EventSubagentStop, 4),
			},
			"1(1,-),2(3,4)",
		},
		"open turn": {[]Event{subagentEdge("e1", EventSubagentStart, 1)}, "1(1,-)"},
	} {
		t.Run(name, func(t *testing.T) {
			for _, order := range permutations(scenario.edges) {
				store := openTestStore(t)
				for _, edge := range order {
					if err := store.Batch(ctx, func(tx *Tx) error {
						if _, err := tx.InsertEvent(ctx, edge); err != nil {
							return err
						}
						return tx.RebuildAgentTurns(ctx, "a1")
					}); err != nil {
						t.Fatal(err)
					}
				}
				if got := agentTurnRows(t, store, "a1"); got != scenario.want {
					ids := make([]string, len(order))
					for i, edge := range order {
						ids[i] = edge.EventID
					}
					t.Errorf("inserted in order %s: agent_turns = %s, want %s", strings.Join(ids, ","), got, scenario.want)
				}
			}
		})
	}
}

func TestAgentTurnsCarryEdgeIDsAndOwner(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	start, stop := subagentEdge("e1", EventSubagentStart, 1), subagentEdge("e2", EventSubagentStop, 2)
	start.PromptID = Ptr("prompt-1")
	stop.AgentType = nil // the stop edge names no type: the turn keeps the start's
	if err := store.Batch(ctx, func(tx *Tx) error {
		for _, edge := range []Event{start, stop} {
			if _, err := tx.InsertEvent(ctx, edge); err != nil {
				return err
			}
		}
		other := subagentEdge("e9", EventSubagentStart, 1)
		other.AgentID = Ptr("a2")
		if _, err := tx.InsertEvent(ctx, other); err != nil {
			return err
		}
		return tx.RebuildAgentTurns(ctx, "a1")
	}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "agent_turns", "agent_id = ? AND seq = 1", "a1")
	for column, want := range map[string]any{
		"session_id": "sess-1", "agent_type": "Explore", "prompt_id": "prompt-1", "start_event_id": "e1", "stop_event_id": "e2",
	} {
		if got[column] != want {
			t.Errorf("agent_turns.%s = %v, want %v", column, got[column], want)
		}
	}
	if n := count(t, store, "agent_turns"); n != 1 {
		t.Errorf("agent_turns holds %d rows, want 1 (another agent's events are not walked)", n)
	}
}

// TestSessionTouchAndRefresh: a session's row is a function of the runs that
// touched it and the events and requests stored, whichever order they landed.
func TestSessionTouchAndRefresh(t *testing.T) {
	ctx := context.Background()
	model := func(id string, ts int64, name string) Request {
		return Request{RequestID: id, SessionID: Ptr("sess-1"), TS: Ptr(ts), Model: Ptr(name)}
	}
	steps := map[string]func(tx *Tx) error{
		"resume": func(tx *Tx) error {
			_, err := tx.InsertEvent(ctx, Event{EventID: "s5", Event: EventSessionStart, TS: 5, SessionID: Ptr("sess-1"), Source: Ptr("resume"), Model: Ptr("start-model")})
			return err
		},
		"startup": func(tx *Tx) error {
			_, err := tx.InsertEvent(ctx, Event{EventID: "s1", Event: EventSessionStart, TS: 1, SessionID: Ptr("sess-1"), Source: Ptr("startup")})
			return err
		},
		"early end": func(tx *Tx) error {
			_, err := tx.InsertEvent(ctx, Event{EventID: "s4", Event: EventSessionEnd, TS: 4, SessionID: Ptr("sess-1"), Reason: Ptr("clear")})
			return err
		},
		"end": func(tx *Tx) error {
			_, err := tx.InsertEvent(ctx, Event{EventID: "s9", Event: EventSessionEnd, TS: 9, SessionID: Ptr("sess-1"), Reason: Ptr("other")})
			return err
		},
		"model A": func(tx *Tx) error { return tx.UpsertRequest(ctx, model("msg_a", 3, "A"), Overwrite) },
		"model B": func(tx *Tx) error { return tx.UpsertRequest(ctx, model("msg_b", 7, "B"), Overwrite) },
		"sub-agent request": func(tx *Tx) error {
			r := model("msg_sub", 8, "SUB")
			r.AgentID = Ptr("a1")
			return tx.UpsertRequest(ctx, r, Overwrite)
		},
	}
	names := []string{"resume", "startup", "early end", "end", "model A", "model B", "sub-agent request"}
	store := openTestStore(t)
	for i, order := range permutations(names) {
		if i%7 != 0 {
			continue // every seventh of the 5040 orders keeps the run short
		}
		for _, table := range []string{"sessions", "events", "requests"} {
			if _, err := store.DB().Exec("DELETE FROM " + table); err != nil {
				t.Fatal(err)
			}
		}
		for step, name := range order {
			if err := store.Batch(ctx, func(tx *Tx) error {
				if err := steps[name](tx); err != nil {
					return err
				}
				// Each run touches the session at its own clock reading.
				touch := Session{SessionID: "sess-1", TS: int64(10 * (step + 1)), Cwd: Ptr(fmt.Sprintf("/tmp/cwd-%d", step))}
				if err := tx.TouchSession(ctx, touch); err != nil {
					return err
				}
				return tx.RefreshSession(ctx, "sess-1")
			}); err != nil {
				t.Fatal(err)
			}
		}
		got := row(t, store, "sessions", "session_id = ?", "sess-1")
		for column, want := range map[string]any{
			"start_source": "startup", "end_reason": "other", "model": "B", "first_ts": int64(10), "last_ts": int64(70),
			"engine": "claude", "cwd": "/tmp/cwd-0", // the earliest run's cwd, whatever order the runs land in
		} {
			if got[column] != want {
				t.Fatalf("order %v: sessions.%s = %v, want %v", order, column, got[column], want)
			}
		}
	}
}

func TestSessionModelFallsBackToTheLatestSessionStart(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Batch(ctx, func(tx *Tx) error {
		for _, e := range []Event{
			{EventID: "s1", Event: EventSessionStart, TS: 1, SessionID: Ptr("sess-1"), Model: Ptr("old")},
			{EventID: "s2", Event: EventSessionStart, TS: 2, SessionID: Ptr("sess-1"), Model: Ptr("new")},
			{EventID: "s3", Event: EventSessionStart, TS: 3, SessionID: Ptr("sess-1")},
		} {
			if _, err := tx.InsertEvent(ctx, e); err != nil {
				return err
			}
		}
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: 3}); err != nil {
			return err
		}
		return tx.RefreshSession(ctx, "sess-1")
	}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "sessions", "session_id = ?", "sess-1")
	if got["model"] != "new" || got["start_source"] != nil || got["end_reason"] != nil {
		t.Errorf("session = model %v, start_source %v, end_reason %v; want new, NULL, NULL", got["model"], got["start_source"], got["end_reason"])
	}
}

func TestTouchSessionTakesTheEarliestRunAndRefreshNeedsARow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Batch(ctx, func(tx *Tx) error { return tx.RefreshSession(ctx, "sess-none") }); err == nil {
		t.Error("RefreshSession of a session TouchSession never wrote returned no error")
	}
	if err := store.Batch(ctx, func(tx *Tx) error {
		if err := tx.TouchSession(ctx, Session{SessionID: "sess-1", TS: 50, Cwd: Ptr("/tmp/a"), Host: Ptr("host-a"), TZOffsetMinutes: Ptr(int64(-300))}); err != nil {
			return err
		}
		return tx.TouchSession(ctx, Session{
			SessionID: "sess-1", TS: 20, Cwd: Ptr("/tmp/b"), TranscriptPath: Ptr("/tmp/t.jsonl"), TZName: Ptr("America/Toronto"),
		})
	}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "sessions", "session_id = ?", "sess-1")
	for column, want := range map[string]any{
		"first_ts": int64(20), "last_ts": int64(50), "cwd": "/tmp/b", "host": "host-a", "tz_offset_minutes": int64(-300),
		"transcript_path": "/tmp/t.jsonl", "tz_name": "America/Toronto", "engine": "claude",
	} {
		if got[column] != want {
			t.Errorf("sessions.%s = %v, want %v", column, got[column], want)
		}
	}
}

// clashDelivery is one delivery of a payload's bytes: its ts and the columns
// that differ between deliveries of the same bytes (effort is the hook
// process's, seat_dir its seat).
type clashDelivery struct {
	ts     int64
	effort string
	seat   *string
}

func TestInsertKeepsTheEarliestDelivery(t *testing.T) {
	ctx := context.Background()
	tables := map[string]func(tx *Tx, d clashDelivery) (bool, error){
		"events": func(tx *Tx, d clashDelivery) (bool, error) {
			return tx.InsertEvent(ctx, Event{EventID: "same-bytes", Event: "Notification", TS: d.ts, SessionID: Ptr("sess-1"), Effort: Ptr(d.effort), SeatDir: d.seat})
		},
		"turns": func(tx *Tx, d clashDelivery) (bool, error) {
			return tx.InsertTurn(ctx, Turn{EventID: "same-bytes", Event: "Stop", TS: d.ts, SessionID: Ptr("sess-1"), Effort: Ptr(d.effort), SeatDir: d.seat})
		},
	}
	for table, insert := range tables {
		t.Run(table, func(t *testing.T) {
			store := openTestStore(t)
			deliver := func(d clashDelivery) bool {
				t.Helper()
				var ok bool
				if err := store.Batch(ctx, func(tx *Tx) error {
					var err error
					ok, err = insert(tx, d)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return ok
			}
			check := func(when string, wantTS int64, wantEffort string, wantSeat any) {
				t.Helper()
				got := row(t, store, table, "event_id = ?", "same-bytes")
				if n := count(t, store, table); n != 1 || got["ts"] != wantTS || got["effort"] != wantEffort || got["seat_dir"] != wantSeat {
					t.Errorf("%s: %s holds %d rows, row ts %v effort %v seat_dir %v; want 1 row, ts %d effort %s seat_dir %v",
						when, table, n, got["ts"], got["effort"], got["seat_dir"], wantTS, wantEffort, wantSeat)
				}
			}
			if !deliver(clashDelivery{ts: 2000, effort: "high", seat: Ptr("/tmp/seat-late")}) {
				t.Error("the first delivery reported no row written")
			}
			if !deliver(clashDelivery{ts: 1000, effort: "low"}) {
				t.Error("an earlier delivery landing second reported no row written")
			}
			check("earlier delivery lands second", 1000, "low", nil)
			if deliver(clashDelivery{ts: 3000, effort: "max", seat: Ptr("/tmp/seat-later")}) {
				t.Error("a later delivery reported a row written")
			}
			if deliver(clashDelivery{ts: 1000, effort: "max"}) {
				t.Error("a delivery at the stored ts reported a row written")
			}
			check("later and tied deliveries", 1000, "low", nil)
		})
	}
}

func TestTouchSessionColumnsComeFromTheEarliestRun(t *testing.T) {
	ctx := context.Background()
	run := func(ts int64, tag string) Session {
		return Session{
			SessionID: "sess-1", TS: ts, Cwd: Ptr("/a" + tag), TranscriptPath: Ptr("/t" + tag + ".jsonl"), SeatDir: Ptr("/seat" + tag),
			ConfigDir: Ptr("/config" + tag), Host: Ptr("host" + tag), TZName: Ptr("zone" + tag), TZOffsetMinutes: Ptr(ts / 1000),
		}
	}
	early, late := run(1000, ""), run(2000, "/sub")
	// A run carrying no value leaves the column to the others, whichever is earlier.
	early.Host, late.TZName = nil, nil
	for _, order := range permutations([]Session{early, late}) {
		store := openTestStore(t)
		for _, s := range order {
			if err := store.Batch(ctx, func(tx *Tx) error { return tx.TouchSession(ctx, s) }); err != nil {
				t.Fatal(err)
			}
		}
		got := row(t, store, "sessions", "session_id = ?", "sess-1")
		for column, want := range map[string]any{
			"first_ts": int64(1000), "last_ts": int64(2000), "cwd": "/a", "transcript_path": "/t.jsonl", "seat_dir": "/seat",
			"config_dir": "/config", "host": "host/sub", "tz_name": "zone", "tz_offset_minutes": int64(1),
		} {
			if got[column] != want {
				t.Errorf("runs at ts %d then %d: sessions.%s = %v, want %v", order[0].TS, order[1].TS, column, got[column], want)
			}
		}
	}
}

// runValue is the value n of a session column as the sessions row holds it: a
// larger n is a larger value, so a test can tell the earliest run from the
// smallest value.
func runValue(column string, n int) any {
	if column == "tz_offset_minutes" {
		return int64(n)
	}
	return fmt.Sprintf("v%02d", n)
}

// sessionRunColumns are the seven columns a hook run carries into its session,
// each with how a run of the test sets its value n.
var sessionRunColumns = []struct {
	name string
	set  func(s *Session, n int)
}{
	{"cwd", func(s *Session, n int) { s.Cwd = Ptr(runValue("cwd", n).(string)) }},
	{"transcript_path", func(s *Session, n int) { s.TranscriptPath = Ptr(runValue("transcript_path", n).(string)) }},
	{"seat_dir", func(s *Session, n int) { s.SeatDir = Ptr(runValue("seat_dir", n).(string)) }},
	{"config_dir", func(s *Session, n int) { s.ConfigDir = Ptr(runValue("config_dir", n).(string)) }},
	{"host", func(s *Session, n int) { s.Host = Ptr(runValue("host", n).(string)) }},
	{"tz_name", func(s *Session, n int) { s.TZName = Ptr(runValue("tz_name", n).(string)) }},
	{"tz_offset_minutes", func(s *Session, n int) { s.TZOffsetMinutes = Ptr(runValue("tz_offset_minutes", n).(int64)) }},
}

// touchSessionRuns folds the runs into one session, in the order given, each in
// its own batch as the async hooks write it.
func touchSessionRuns(t *testing.T, store *Store, runs []Session) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.DB().Exec("DELETE FROM sessions"); err != nil {
		t.Fatal(err)
	}
	for _, s := range runs {
		if err := store.Batch(ctx, func(tx *Tx) error { return tx.TouchSession(ctx, s) }); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTouchSessionColumnIsTheEarliestRunsInEveryArrivalOrder: of three runs per
// column, the first without the column, the second carrying a value that sorts
// above the third's, the column and its ts are the second's whatever order the
// runs land in; the earliest run carrying it wins, never the smallest value.
func TestTouchSessionColumnIsTheEarliestRunsInEveryArrivalOrder(t *testing.T) {
	store := openTestStore(t)
	for _, column := range sessionRunColumns {
		blank := Session{SessionID: "sess-1", TS: 10}
		b, c := Session{SessionID: "sess-1", TS: 20}, Session{SessionID: "sess-1", TS: 30}
		column.set(&b, 90)
		column.set(&c, 10)
		for _, order := range permutations([]Session{blank, b, c}) {
			touchSessionRuns(t, store, order)
			got := row(t, store, "sessions", "session_id = ?", "sess-1")
			if got[column.name] != runValue(column.name, 90) || got[column.name+"_ts"] != int64(20) {
				t.Errorf("runs at ts %d, %d, %d: sessions.%s = %v with ts %v, want %v with ts 20",
					order[0].TS, order[1].TS, order[2].TS, column.name, got[column.name], got[column.name+"_ts"],
					runValue(column.name, 90))
			}
			if got["first_ts"] != int64(10) || got["last_ts"] != int64(30) {
				t.Errorf("runs at ts %d, %d, %d: first_ts %v last_ts %v, want 10 and 30",
					order[0].TS, order[1].TS, order[2].TS, got["first_ts"], got["last_ts"])
			}
			for _, other := range sessionRunColumns {
				if other.name != column.name && (got[other.name] != nil || got[other.name+"_ts"] != nil) {
					t.Errorf("sessions.%s = %v with ts %v, want NULL: no run carried it", other.name, got[other.name], got[other.name+"_ts"])
				}
			}
		}
	}
}

// TestTouchSessionColumnTakesTheSmallerValueAtOneTs: runs at one ts carrying
// different values leave the smaller, whatever order they land in, and a later
// run does not displace it.
func TestTouchSessionColumnTakesTheSmallerValueAtOneTs(t *testing.T) {
	store := openTestStore(t)
	for _, column := range sessionRunColumns {
		runs := map[string]Session{
			"high": {SessionID: "sess-1", TS: 20}, "low": {SessionID: "sess-1", TS: 20}, "later": {SessionID: "sess-1", TS: 30},
		}
		for name, n := range map[string]int{"high": 90, "low": 10, "later": 5} {
			run := runs[name]
			column.set(&run, n)
			runs[name] = run
		}
		for _, order := range permutations([]string{"high", "low", "later"}) {
			touchSessionRuns(t, store, []Session{runs[order[0]], runs[order[1]], runs[order[2]]})
			got := row(t, store, "sessions", "session_id = ?", "sess-1")
			if got[column.name] != runValue(column.name, 10) || got[column.name+"_ts"] != int64(20) {
				t.Errorf("runs landing %v: sessions.%s = %v with ts %v, want %v with ts 20",
					order, column.name, got[column.name], got[column.name+"_ts"], runValue(column.name, 10))
			}
		}
	}
}

// TestTouchSessionColumnNoRunCarriesStaysNull: a session whose runs carry none
// of the columns holds NULL in each column and in its ts.
func TestTouchSessionColumnNoRunCarriesStaysNull(t *testing.T) {
	store := openTestStore(t)
	runs := []Session{{SessionID: "sess-1", TS: 10}, {SessionID: "sess-1", TS: 20}, {SessionID: "sess-1", TS: 30}}
	for _, order := range permutations(runs) {
		touchSessionRuns(t, store, order)
		got := row(t, store, "sessions", "session_id = ?", "sess-1")
		for _, column := range sessionRunColumns {
			if got[column.name] != nil || got[column.name+"_ts"] != nil {
				t.Errorf("sessions.%s = %v with ts %v, want NULL and NULL: no run carried it", column.name, got[column.name], got[column.name+"_ts"])
			}
		}
		if got["first_ts"] != int64(10) || got["last_ts"] != int64(30) {
			t.Errorf("first_ts %v last_ts %v, want 10 and 30", got["first_ts"], got["last_ts"])
		}
	}
}
