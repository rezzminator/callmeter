package callmeter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// postToolUse and batch are the two events that write one call: the hook's
// PostToolUse owns the call's facts, PostToolBatch its delivered bytes and
// request. Async hooks land them in either order.
func postToolUse() Call {
	return Call{
		ToolUseID:  "toolu_1",
		SessionID:  Ptr("sess-1"),
		TS:         Ptr(int64(1000)),
		Tool:       Ptr("Bash"),
		Input:      Ptr(`{"command":"seq 1 5"}`),
		Cwd:        Ptr("/tmp/demo-proj"),
		DurationMS: Ptr(int64(12)),
		Failed:     Ptr(false),
		BytesReal:  Ptr(int64(10)),
		Source:     Ptr(SourceHook),
	}
}

func batch() Call {
	return Call{
		ToolUseID:      "toolu_1",
		SessionID:      Ptr("sess-1"),
		RequestID:      Ptr(ProvisionalKey("toolu_1")),
		BytesDelivered: Ptr(int64(10)),
		Source:         Ptr(SourceHook),
	}
}

func TestUpsertOrderIndependence(t *testing.T) {
	ctx := context.Background()
	orders := map[string][]Call{
		"post then batch": {postToolUse(), batch()},
		"batch then post": {batch(), postToolUse()},
	}
	rows := map[string]map[string]any{}
	for name, calls := range orders {
		store := openTestStore(t)
		for _, c := range calls {
			if err := store.UpsertCall(ctx, c, Overwrite); err != nil {
				t.Fatalf("%s: UpsertCall: %v", name, err)
			}
		}
		rows[name] = row(t, store, "calls", "tool_use_id = ?", "toolu_1")
	}
	a, b := rows["post then batch"], rows["batch then post"]
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("rows differ by arrival order:\npost first:  %v\nbatch first: %v", a, b)
	}
	if a["bytes_delivered"] != int64(10) || a["tool"] != "Bash" || a["request_id"] != "pending:toolu_1" {
		t.Fatalf("merged row lost a column: %v", a)
	}
	if a["file_bytes"] != nil {
		t.Fatalf("unprovided file_bytes = %v, want NULL", a["file_bytes"])
	}
}

func TestFillEmptyNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertCall(ctx, postToolUse(), Overwrite); err != nil {
		t.Fatal(err)
	}
	fill := Call{
		ToolUseID:      "toolu_1",
		TS:             Ptr(int64(999)),
		Tool:           Ptr("Read"),
		BytesDelivered: Ptr(int64(42)),
		Source:         Ptr(SourceHook),
	}
	if err := store.UpsertCall(ctx, fill, FillEmpty); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCall(ctx, Call{ToolUseID: "toolu_2", Tool: Ptr("Grep")}, FillEmpty); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "calls", "tool_use_id = ?", "toolu_1")
	// ts alone keeps the earlier of the two in every mode (TestCallTSKeepsTheEarliest).
	want := map[string]any{"ts": int64(999), "tool": "Bash", "source": SourceHook, "bytes_delivered": int64(42)}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
	if inserted := row(t, store, "calls", "tool_use_id = ?", "toolu_2"); inserted == nil || inserted["tool"] != "Grep" {
		t.Errorf("FillEmpty did not insert the missing row: %v", inserted)
	}
}

func TestUpsertCallChangingCwdDropsItsParts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial Call
		change  Call
		mode    Mode
		want    int
	}{
		{"changed cwd", postToolUse(), Call{Cwd: Ptr("/tmp/demo-proj/start")}, Overwrite, 0},
		{"changed input", postToolUse(), Call{Input: Ptr(`{"command":"true"}`)}, Overwrite, 0},
		{"same cwd and input", postToolUse(), Call{Cwd: postToolUse().Cwd, Input: postToolUse().Input}, Overwrite, 1},
		{"fill nothing", postToolUse(), Call{Cwd: Ptr("/tmp/demo-proj/start"), Input: Ptr(`{"command":"true"}`)}, FillEmpty, 1},
		{"fill cwd", Call{ToolUseID: "toolu_1"}, Call{Cwd: Ptr("/tmp/demo-proj/start")}, FillEmpty, 0},
		{"fill input", Call{ToolUseID: "toolu_1"}, Call{Input: Ptr(`{"command":"true"}`)}, FillEmpty, 0},
		{"unrelated update", postToolUse(), Call{BytesDelivered: Ptr(int64(42))}, Overwrite, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			if err := store.UpsertCall(ctx, tc.initial, Overwrite); err != nil {
				t.Fatal(err)
			}
			parts := []CommandPart{{Seq: 0, Lang: "sh", Program: "cat", ParseStatus: "ok"}}
			if err := store.ReplaceCommandParts(ctx, "toolu_1", parts); err != nil {
				t.Fatal(err)
			}
			if err := store.AddFault(ctx, Fault{ToolUseID: "toolu_1", Stage: StageParse, Error: "old parse"}); err != nil {
				t.Fatal(err)
			}
			tc.change.ToolUseID = "toolu_1"
			if err := store.Batch(ctx, func(tx *Tx) error {
				if err := tx.UpsertCall(ctx, tc.change, tc.mode); err != nil {
					return err
				}
				var got int
				if err := tx.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM command_parts WHERE tool_use_id = ?", "toolu_1").Scan(&got); err != nil {
					return err
				}
				if got != tc.want {
					t.Errorf("parts inside upsert transaction = %d, want %d", got, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := count(t, store, "command_parts"); got != tc.want {
				t.Errorf("parts after upsert = %d, want %d", got, tc.want)
			}
			if got := count(t, store, "faults"); got != 1 {
				t.Errorf("parse faults = %d, want the old fault kept until re-parse", got)
			}
		})
	}
}

func TestUpsertCallInvalidationErrorsRollBack(t *testing.T) {
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"read", "ALTER TABLE calls RENAME TO hidden_calls", "read parse inputs"},
		{"upsert", `CREATE TRIGGER refuse_call BEFORE UPDATE ON calls BEGIN SELECT RAISE(ABORT, 'test upsert failure'); END`, "test upsert failure"},
		{"delete", `CREATE TRIGGER refuse_parts BEFORE DELETE ON command_parts BEGIN SELECT RAISE(ABORT, 'test invalidation failure'); END`, "invalidate command parts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			original := postToolUse()
			if err := store.UpsertCall(ctx, original, Overwrite); err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceCommandParts(ctx, original.ToolUseID, []CommandPart{{Seq: 0, Lang: "sh", ParseStatus: "ok"}}); err != nil {
				t.Fatal(err)
			}
			err := store.Batch(ctx, func(tx *Tx) error {
				if _, err := tx.tx.ExecContext(ctx, tc.setup); err != nil {
					return err
				}
				return tx.UpsertCall(ctx, Call{ToolUseID: original.ToolUseID, Cwd: Ptr("/tmp/demo-proj/start")}, Overwrite)
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("upsert error = %v, want %s", err, tc.want)
			}
			if got := row(t, store, "calls", "tool_use_id = ?", original.ToolUseID)["cwd"]; got != *original.Cwd {
				t.Error("failed invalidation changed the stored cwd")
			}
			if got := count(t, store, "command_parts"); got != 1 {
				t.Errorf("parts after rollback = %d, want 1", got)
			}
		})
	}
}

func TestUpsertCutsError(t *testing.T) {
	store := openTestStore(t)
	if err := store.UpsertCall(
		context.Background(),
		Call{ToolUseID: "toolu_1", Error: Ptr(strings.Repeat("é", 600))},
		Overwrite,
	); err != nil {
		t.Fatal(err)
	}
	got, _ := row(t, store, "calls", "tool_use_id = ?", "toolu_1")["error"].(string)
	if n := len([]rune(got)); n != ErrorLimit {
		t.Fatalf("stored error holds %d characters, want %d", n, ErrorLimit)
	}
}

func TestResolveRequestRewritesCallsAndMerges(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	// Two batches of one message: the first already resolved to msg_1, the
	// second still provisional under its first call.
	if err := store.Batch(ctx, func(tx *Tx) error {
		if err := tx.UpsertRequest(
			ctx,
			Request{
				RequestID:     "msg_1",
				SessionID:     Ptr("sess-1"),
				TS:            Ptr(int64(2000)),
				ContextTokens: Ptr(int64(500)),
				Calls:         Ptr(int64(1)),
				Pending:       Ptr(false),
			},
			Overwrite,
		); err != nil {
			return err
		}
		if err := tx.UpsertCall(ctx, Call{ToolUseID: "toolu_a", RequestID: Ptr("msg_1")}, Overwrite); err != nil {
			return err
		}
		key := ProvisionalKey("toolu_b")
		if err := tx.UpsertRequest(
			ctx,
			Request{
				RequestID: key,
				SessionID: Ptr("sess-1"),
				TS:        Ptr(int64(1500)),
				Calls:     Ptr(int64(2)),
				Pending:   Ptr(true),
				ConfigDir: Ptr("/tmp/demo-config"),
			},
			Overwrite,
		); err != nil {
			return err
		}
		for _, id := range []string{"toolu_b", "toolu_c"} {
			if err := tx.UpsertCall(ctx, Call{ToolUseID: id, RequestID: Ptr(key)}, Overwrite); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.ResolveRequest(
		ctx,
		ProvisionalKey("toolu_b"),
		Request{RequestID: "msg_1", OutputTokens: Ptr(int64(30))},
	); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	if n := count(t, store, "requests"); n != 1 {
		t.Fatalf("requests holds %d rows, want the one merged msg_1", n)
	}
	got := row(t, store, "requests", "request_id = ?", "msg_1")
	want := map[string]any{
		"calls":          int64(3),
		"ts":             int64(1500),
		"context_tokens": int64(500),
		"output_tokens":  int64(30),
		"pending":        int64(0),
		"config_dir":     "/tmp/demo-config",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("merged %s = %v, want %v", name, got[name], value)
		}
	}
	for _, id := range []string{"toolu_a", "toolu_b", "toolu_c"} {
		if c := row(t, store, "calls", "tool_use_id = ?", id); c["request_id"] != "msg_1" {
			t.Errorf("call %s request_id = %v, want msg_1", id, c["request_id"])
		}
	}
}

func TestResolveRequestWithoutExistingMessageRow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := ProvisionalKey("toolu_x")
	if err := store.UpsertRequest(
		ctx,
		Request{RequestID: key, SessionID: Ptr("sess-1"), Calls: Ptr(int64(1)), Pending: Ptr(true)},
		Overwrite,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCall(ctx, Call{ToolUseID: "toolu_x", RequestID: Ptr(key)}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveRequest(ctx, key, Request{RequestID: "msg_9", ContextTokens: Ptr(int64(7))}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "requests", "request_id = ?", "msg_9")
	if got == nil || got["session_id"] != "sess-1" || got["calls"] != int64(1) || got["pending"] != int64(0) ||
		got["context_tokens"] != int64(7) {
		t.Fatalf("resolved row = %v", got)
	}
	if row(t, store, "requests", "request_id = ?", key) != nil {
		t.Fatal("the provisional row survived")
	}
}

func TestPendingRequests(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seed := []Request{
		{RequestID: ProvisionalKey("toolu_main"), SessionID: Ptr("sess-1"), TS: Ptr(int64(1)), Pending: Ptr(true)},
		{
			RequestID: ProvisionalKey("toolu_sub"),
			SessionID: Ptr("sess-1"),
			AgentID:   Ptr("agent-7"),
			TS:        Ptr(int64(2)),
			Pending:   Ptr(true),
		},
		{RequestID: "msg_done", SessionID: Ptr("sess-1"), Pending: Ptr(false)},
	}
	for _, r := range seed {
		if err := store.UpsertRequest(ctx, r, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []Call{
		{ToolUseID: "toolu_main", RequestID: Ptr(ProvisionalKey("toolu_main")), TS: Ptr(int64(1))},
		{ToolUseID: "toolu_main2", RequestID: Ptr(ProvisionalKey("toolu_main")), TS: Ptr(int64(2))},
	} {
		if err := store.UpsertCall(ctx, c, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	main, err := store.PendingRequests(ctx, "sess-1", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []PendingRequest{{RequestID: "pending:toolu_main", CallIDs: []string{"toolu_main", "toolu_main2"}}}
	if !reflect.DeepEqual(main, want) {
		t.Fatalf("main chat pending = %v, want %v", main, want)
	}
	sub, err := store.PendingRequests(ctx, "sess-1", "agent-7")
	if err != nil {
		t.Fatal(err)
	}
	// No call row yet: the key's own first call is still listed.
	want = []PendingRequest{{RequestID: "pending:toolu_sub", CallIDs: []string{"toolu_sub"}}}
	if !reflect.DeepEqual(sub, want) {
		t.Fatalf("agent pending = %v, want %v", sub, want)
	}
}

func TestReplaceCommandParts(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first := []CommandPart{
		{
			Seq:         0,
			Lang:        "sh",
			Program:     "cat",
			Args:        []string{"a.txt"},
			Files:       []string{"/tmp/demo-proj/a.txt"},
			ParseStatus: "ok",
			Conditional: true,
		},
		{Seq: 1, Lang: "sh", Program: "wc"},
	}
	if err := store.ReplaceCommandParts(ctx, "toolu_1", first); err != nil {
		t.Fatal(err)
	}
	if got := row(t, store, "command_parts", "tool_use_id = ? AND seq = 1", "toolu_1"); got["conditional"] != int64(0) {
		t.Fatalf("certain part stored conditional = %v, want 0", got["conditional"])
	}
	if err := store.ReplaceCommandParts(ctx, "toolu_1", first[:1]); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, "command_parts"); n != 1 {
		t.Fatalf("command_parts holds %d rows after replace, want 1", n)
	}
	got := row(t, store, "command_parts", "tool_use_id = ? AND seq = 0", "toolu_1")
	if got["args"] != `["a.txt"]` || got["files"] != `["/tmp/demo-proj/a.txt"]` || got["program"] != "cat" ||
		got["conditional"] != int64(1) {
		t.Fatalf("part = %v", got)
	}
}

func TestUpsertRejectsEmptyKey(t *testing.T) {
	store := openTestStore(t)
	err := store.UpsertCall(context.Background(), Call{Tool: Ptr("Bash")}, Overwrite)
	if err == nil || !strings.Contains(err.Error(), "tool_use_id") {
		t.Fatalf("UpsertCall without a key = %v, want an error naming tool_use_id", err)
	}
}

// TestSeatDirMergesLikeConfigDir: seat_dir, once set, is never overwritten by
// a write that does not carry it, and a provisional request's survives its
// resolution into a row that had none.
func TestSeatDirMergesLikeConfigDir(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seat := "/tmp/demo-seat"
	key := ProvisionalKey("toolu_a")
	if err := store.Batch(ctx, func(tx *Tx) error {
		if err := tx.UpsertRequest(ctx, Request{RequestID: "msg_1", Calls: Ptr(int64(1))}, Overwrite); err != nil {
			return err
		}
		if err := tx.UpsertRequest(ctx, Request{
			RequestID: key, Calls: Ptr(int64(1)), Pending: Ptr(true), SeatDir: Ptr(seat),
		}, Overwrite); err != nil {
			return err
		}
		if err := tx.UpsertCall(
			ctx,
			Call{ToolUseID: "toolu_a", SeatDir: Ptr(seat)},
			Overwrite,
		); err != nil {
			return err
		}
		if err := tx.UpsertCall(ctx, Call{ToolUseID: "toolu_a", Tool: Ptr("Bash")}, Overwrite); err != nil {
			return err
		}
		if err := tx.UpsertAgent(
			ctx,
			Agent{AgentID: "agent_a", SeatDir: Ptr(seat)},
			Overwrite,
		); err != nil {
			return err
		}
		return tx.UpsertAgent(ctx, Agent{AgentID: "agent_a", Model: Ptr("opus")}, Overwrite)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.ResolveRequest(ctx, key, Request{RequestID: "msg_1"}); err != nil {
		t.Fatalf("ResolveRequest: %v", err)
	}
	for _, got := range []map[string]any{
		row(t, store, "requests", "request_id = ?", "msg_1"),
		row(t, store, "calls", "tool_use_id = ?", "toolu_a"),
		row(t, store, "agents", "agent_id = ?", "agent_a"),
	} {
		if got["seat_dir"] != seat {
			t.Errorf("row seat_dir = %v, want %s", got["seat_dir"], seat)
		}
	}
}

// TestRequestRoundTripsTheTokenSplit: a requests row keeps the whole split,
// the model and the stop reason, and a usage with no cache_creation object
// leaves the 5m and 1h columns NULL, never 0.
func TestRequestRoundTripsTheTokenSplit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	split := Request{
		RequestID: "msg_split", SessionID: Ptr("sess-1"), PromptID: Ptr("prompt-1"), TS: Ptr(int64(5)),
		Model: Ptr("claude-sonnet-4-5"), StopReason: Ptr("tool_use"),
		InputTokens: Ptr(int64(2)), CacheReadTokens: Ptr(int64(12015)), CacheCreationTokens: Ptr(int64(5206)),
		CacheCreation5mTokens: Ptr(int64(0)), CacheCreation1hTokens: Ptr(int64(5206)),
		ContextTokens: Ptr(int64(2 + 12015 + 5206)), OutputTokens: Ptr(int64(374)),
	}
	unsplit := Request{
		RequestID: "msg_unsplit", InputTokens: Ptr(int64(3)), CacheReadTokens: Ptr(int64(4)),
		CacheCreationTokens: Ptr(int64(5)), ContextTokens: Ptr(int64(12)),
	}
	for _, r := range []Request{split, unsplit} {
		if err := store.UpsertRequest(ctx, r, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	got := row(t, store, "requests", "request_id = ?", "msg_split")
	for column, want := range map[string]any{
		"prompt_id": "prompt-1", "model": "claude-sonnet-4-5", "stop_reason": "tool_use", "input_tokens": int64(2),
		"cache_read_tokens": int64(12015), "cache_creation_tokens": int64(5206), "cache_creation_5m_tokens": int64(0),
		"cache_creation_1h_tokens": int64(5206), "context_tokens": int64(17223), "output_tokens": int64(374),
	} {
		if got[column] != want {
			t.Errorf("msg_split.%s = %v, want %v", column, got[column], want)
		}
	}
	got = row(t, store, "requests", "request_id = ?", "msg_unsplit")
	if got["cache_creation_5m_tokens"] != nil || got["cache_creation_1h_tokens"] != nil || got["input_tokens"] != int64(3) {
		t.Errorf("msg_unsplit split = %v / %v, input %v; want NULL / NULL, 3",
			got["cache_creation_5m_tokens"], got["cache_creation_1h_tokens"], got["input_tokens"])
	}
}

// TestResolveRequestCarriesTheTokenSplit: a provisional request resolved to
// its message id ends as one row holding the split, model and stop reason of
// the resolution, and the columns only the provisional row had.
func TestResolveRequestCarriesTheTokenSplit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := ProvisionalKey("toolu_p")
	if err := store.UpsertRequest(ctx, Request{
		RequestID: key, SessionID: Ptr("sess-1"), PromptID: Ptr("prompt-9"), TS: Ptr(int64(7)), Calls: Ptr(int64(2)),
		Pending: Ptr(true),
	}, Overwrite); err != nil {
		t.Fatal(err)
	}
	// calls counts the calls rows carrying the key, as the batch writes them.
	for _, id := range []string{"toolu_p", "toolu_q"} {
		if err := store.UpsertCall(ctx, Call{ToolUseID: id, RequestID: Ptr(key)}, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ResolveRequest(ctx, key, Request{
		RequestID: "msg_r", TS: Ptr(int64(6)), Model: Ptr("claude-haiku-4-5"), StopReason: Ptr("end_turn"),
		InputTokens: Ptr(int64(1)), CacheReadTokens: Ptr(int64(2)), CacheCreationTokens: Ptr(int64(3)),
		CacheCreation5mTokens: Ptr(int64(1)), CacheCreation1hTokens: Ptr(int64(2)),
		ContextTokens: Ptr(int64(6)), OutputTokens: Ptr(int64(9)),
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, store, "requests"); n != 1 {
		t.Fatalf("requests holds %d rows after the resolution, want 1", n)
	}
	got := row(t, store, "requests", "request_id = ?", "msg_r")
	for column, want := range map[string]any{
		"model": "claude-haiku-4-5", "stop_reason": "end_turn", "input_tokens": int64(1), "cache_read_tokens": int64(2),
		"cache_creation_tokens": int64(3), "cache_creation_5m_tokens": int64(1), "cache_creation_1h_tokens": int64(2),
		"context_tokens": int64(6), "output_tokens": int64(9), "prompt_id": "prompt-9", "calls": int64(2),
		"pending": int64(0),
	} {
		if got[column] != want {
			t.Errorf("msg_r.%s = %v, want %v", column, got[column], want)
		}
	}
}

// TestResolveRequestMergeKeepsTheStoredSplit: parallel calls of one message in
// two batches — the row already resolved keeps what it has, the provisional
// one fills every column the stored row lacks.
func TestResolveRequestMergeKeepsTheStoredSplit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	key := ProvisionalKey("toolu_b")
	for _, r := range []Request{
		{RequestID: "msg_m", Model: Ptr("stored-model"), InputTokens: Ptr(int64(4)), Calls: Ptr(int64(1))},
		{
			RequestID: key, PromptID: Ptr("prompt-2"), Model: Ptr("provisional-model"), StopReason: Ptr("tool_use"),
			InputTokens: Ptr(int64(99)), CacheReadTokens: Ptr(int64(5)), CacheCreationTokens: Ptr(int64(6)),
			CacheCreation5mTokens: Ptr(int64(1)), CacheCreation1hTokens: Ptr(int64(5)), Calls: Ptr(int64(1)),
		},
	} {
		if err := store.UpsertRequest(ctx, r, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	// One call of the message in each batch, as the batches write them.
	for id, request := range map[string]string{"toolu_a": "msg_m", "toolu_b": key} {
		if err := store.UpsertCall(ctx, Call{ToolUseID: id, RequestID: Ptr(request)}, Overwrite); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ResolveRequest(ctx, key, Request{RequestID: "msg_m"}); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "requests", "request_id = ?", "msg_m")
	for column, want := range map[string]any{
		"model": "stored-model", "input_tokens": int64(4), "calls": int64(2), "prompt_id": "prompt-2",
		"stop_reason": "tool_use", "cache_read_tokens": int64(5), "cache_creation_tokens": int64(6),
		"cache_creation_5m_tokens": int64(1), "cache_creation_1h_tokens": int64(5),
	} {
		if got[column] != want {
			t.Errorf("merged msg_m.%s = %v, want %v", column, got[column], want)
		}
	}
}

// TestCallRoundTripsTheOutcomeColumns: the new calls columns reach the store
// and read back as written.
func TestCallRoundTripsTheOutcomeColumns(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertCall(ctx, Call{
		ToolUseID: "toolu_o", PromptID: Ptr("prompt-1"), IsInterrupt: Ptr(true), Effort: Ptr("high"),
		PermissionMode: Ptr("plan"), LinesAdded: Ptr(int64(3)), LinesRemoved: Ptr(int64(1)),
		CommitSHA: Ptr("1a2b3c4"), CommitBranch: Ptr("develop"), TestRunner: Ptr("go"),
	}, Overwrite); err != nil {
		t.Fatal(err)
	}
	got := row(t, store, "calls", "tool_use_id = ?", "toolu_o")
	for column, want := range map[string]any{
		"prompt_id": "prompt-1", "is_interrupt": int64(1), "effort": "high", "permission_mode": "plan",
		"lines_added": int64(3), "lines_removed": int64(1), "commit_sha": "1a2b3c4", "commit_branch": "develop",
		"test_runner": "go",
	} {
		if got[column] != want {
			t.Errorf("toolu_o.%s = %v, want %v", column, got[column], want)
		}
	}
	if err := store.UpsertAgent(ctx, Agent{AgentID: "agent_o", PromptID: Ptr("prompt-1")}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if got := row(t, store, "agents", "agent_id = ?", "agent_o"); got["prompt_id"] != "prompt-1" {
		t.Errorf("agent prompt_id = %v, want prompt-1", got["prompt_id"])
	}
}

// TestKeepMinAndKeepMaxAreOrderIndependent: an agent's started is the earliest
// start seen and its stopped the latest stop, whichever lands first; a NULL
// stored side loses to a value.
func TestKeepMinAndKeepMaxAreOrderIndependent(t *testing.T) {
	ctx := context.Background()
	for name, order := range map[string]struct{ starts, stops []int64 }{
		"ascending":  {[]int64{3, 5}, []int64{7, 9}},
		"descending": {[]int64{5, 3}, []int64{9, 7}},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			for _, started := range order.starts {
				if err := store.UpsertAgent(ctx, Agent{AgentID: "a", Started: Ptr(started)}, KeepMin); err != nil {
					t.Fatal(err)
				}
			}
			for _, stopped := range order.stops {
				if err := store.UpsertAgent(ctx, Agent{AgentID: "a", Stopped: Ptr(stopped)}, KeepMax); err != nil {
					t.Fatal(err)
				}
			}
			got := row(t, store, "agents", "agent_id = ?", "a")
			if got["started"] != int64(3) || got["stopped"] != int64(9) {
				t.Errorf("started, stopped = %v, %v; want 3, 9", got["started"], got["stopped"])
			}
		})
	}
	store := openTestStore(t)
	if err := store.UpsertAgent(ctx, Agent{AgentID: "b", SessionID: Ptr("s")}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, Agent{AgentID: "b", Started: Ptr(int64(4))}, KeepMin); err != nil {
		t.Fatal(err)
	}
	for _, stopped := range []int64{8, 2} {
		if err := store.UpsertAgent(ctx, Agent{AgentID: "b", Stopped: Ptr(stopped)}, KeepMax); err != nil {
			t.Fatal(err)
		}
	}
	got := row(t, store, "agents", "agent_id = ?", "b")
	if got["started"] != int64(4) || got["stopped"] != int64(8) {
		t.Errorf("a NULL stored side must lose: started, stopped = %v, %v; want 4, 8", got["started"], got["stopped"])
	}
}

// TestCallTSKeepsTheEarliest: a call's ts is the earliest hook that saw it,
// whatever the upsert's mode and the landing order: a PostToolUse or a batch
// landing after its PreToolUse never moves ts later, an earlier one landing
// last still wins, and an upsert providing no ts leaves it.
func TestCallTSKeepsTheEarliest(t *testing.T) {
	ctx := context.Background()
	early := Call{ToolUseID: "toolu_1", TS: Ptr(int64(1000))}
	late := Call{ToolUseID: "toolu_1", TS: Ptr(int64(2000))}
	none := Call{ToolUseID: "toolu_1", Tool: Ptr("Bash")}
	for _, mode := range []Mode{Overwrite, FillEmpty} {
		for name, calls := range map[string][]Call{
			"early then late": {early, late, none},
			"late then early": {late, early, none},
			"none first":      {none, late, early},
		} {
			store := openTestStore(t)
			for _, c := range calls {
				if err := store.UpsertCall(ctx, c, mode); err != nil {
					t.Fatalf("mode %d, %s: UpsertCall: %v", mode, name, err)
				}
			}
			if got := row(t, store, "calls", "tool_use_id = ?", "toolu_1")["ts"]; got != int64(1000) {
				t.Errorf("mode %d, %s: ts = %v, want 1000", mode, name, got)
			}
		}
	}
}

func TestUnfinishedCallsSkipsALandedEdit(t *testing.T) {
	ctx := context.Background()
	for _, response := range noRealOutputResponses(t) {
		t.Run(response.name, func(t *testing.T) {
			store := openTestStore(t)
			real, err := RealBytes(response.tool, response.raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range []Call{
				{ToolUseID: "landed", SessionID: Ptr("sess-1"), Tool: Ptr(response.tool), RequestID: Ptr("msg_main"), Failed: Ptr(false), BytesReal: real, Source: Ptr(SourceHook)},
				{ToolUseID: "rebuilt", SessionID: Ptr("sess-1"), Tool: Ptr(response.tool), RequestID: Ptr("msg_main"), Failed: Ptr(false), BytesReal: real, BytesDelivered: Ptr(int64(7)), Source: Ptr(SourceTranscript)},
				{ToolUseID: "failed", SessionID: Ptr("sess-1"), Tool: Ptr(response.tool), RequestID: Ptr("msg_main"), Failed: Ptr(true)},
				{ToolUseID: "not-landed", SessionID: Ptr("sess-1"), Tool: Ptr(response.tool), RequestID: Ptr("msg_main")},
				{ToolUseID: "old", SessionID: Ptr("sess-1"), Tool: Ptr(response.tool), RequestID: Ptr("msg_main"), Failed: Ptr(false), BytesReal: Ptr(int64(42))},
			} {
				if err := store.UpsertCall(ctx, call, Overwrite); err != nil {
					t.Fatal(err)
				}
			}
			calls, err := store.UnfinishedCalls(ctx, "sess-1")
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, call := range calls {
				ids = append(ids, call.ToolUseID)
			}
			if !reflect.DeepEqual(ids, []string{"not-landed"}) {
				t.Errorf("UnfinishedCalls ids = %v, want only not-landed", ids)
			}
			var open int
			if err := store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM calls c WHERE "+callOpen("c")).Scan(&open); err != nil {
				t.Fatal(err)
			}
			if open != 1 {
				t.Errorf("callOpen count = %d, want only not-landed", open)
			}
			if got := row(t, store, "calls", "tool_use_id = 'old'")["bytes_real"]; got != int64(42) {
				t.Errorf("old bytes_real = %v, want 42", got)
			}
		})
	}
}

// TestSettleRequestLeavesAnotherOwnersRow: a forked session starts with a copy
// of its parent's history under the same message ids, one of them written with
// all-zero usage. The fork's sweep (SettleRequest, Overwrite) must leave the
// parent's row as it stands, whether the copy is older than the fork's first
// run (the UPDATE) or not (the upsert); so must a sub-agent's sweep over a
// main-chat row. The owner's own sweep still overwrites.
func TestSettleRequestLeavesAnotherOwnersRow(t *testing.T) {
	ctx := context.Background()
	parent := func() Request {
		return Request{
			RequestID: "msg_m", SessionID: Ptr("sess-a"), TS: Ptr(int64(100)), Model: Ptr("claude-opus-4-1"),
			StopReason: Ptr("tool_use"), InputTokens: Ptr(int64(2)), CacheReadTokens: Ptr(int64(331235)),
			CacheCreationTokens: Ptr(int64(344)), ContextTokens: Ptr(int64(331581)), OutputTokens: Ptr(int64(583)),
		}
	}
	zeroed := func(session string, agent *string) Request {
		return Request{
			RequestID: "msg_m", SessionID: Ptr(session), AgentID: agent, TS: Ptr(int64(100)),
			Model: Ptr("claude-opus-4-1"), StopReason: Ptr("tool_use"), Pending: Ptr(false), Source: Ptr(SourceHook),
			InputTokens: Ptr(int64(0)), CacheReadTokens: Ptr(int64(0)), CacheCreationTokens: Ptr(int64(0)),
			ContextTokens: Ptr(int64(0)), OutputTokens: Ptr(int64(0)),
		}
	}
	for _, tc := range []struct {
		name  string
		sweep Request
		since int64
		want  int64 // output_tokens after the sweep
	}{
		{"fork older than its first run", zeroed("sess-b", nil), 200, 583},
		{"fork at or after its first run", zeroed("sess-b", nil), 50, 583},
		{"sub-agent over a main-chat row", zeroed("sess-a", Ptr("agent-1")), 200, 583},
		{"owner older than its first run", zeroed("sess-a", nil), 200, 0},
		{"owner at or after its first run", zeroed("sess-a", nil), 50, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			if err := store.UpsertRequest(ctx, parent(), Overwrite); err != nil {
				t.Fatal(err)
			}
			if err := store.Batch(ctx, func(tx *Tx) error {
				return tx.SettleRequest(ctx, tc.sweep, nil, tc.since)
			}); err != nil {
				t.Fatal(err)
			}
			got := row(t, store, "requests", "request_id = ?", "msg_m")
			if got["output_tokens"] != tc.want || got["session_id"] != "sess-a" || got["agent_id"] != nil {
				t.Errorf("msg_m output %v session %v agent %v, want %d sess-a <nil>",
					got["output_tokens"], got["session_id"], got["agent_id"], tc.want)
			}
			if tc.want != 0 && (got["input_tokens"] != int64(2) || got["cache_read_tokens"] != int64(331235) ||
				got["cache_creation_tokens"] != int64(344)) {
				t.Errorf("msg_m tokens %v/%v/%v, want 2/331235/344 unchanged",
					got["input_tokens"], got["cache_read_tokens"], got["cache_creation_tokens"])
			}
		})
	}
}

// TestSettleSweepLeavesTheStoreToConcurrentHooks reproduces the Stop hook's
// request sweep over a store of many calls: one transaction settles every
// transcript request (SettleRequest, each recounting its calls), while async
// hooks of other chats write. Without an index on calls(request_id) each
// recount scans calls, the sweep holds the write lock past the writers' wait,
// and they drop their events busy.
func TestSettleSweepLeavesTheStoreToConcurrentHooks(t *testing.T) {
	const (
		requests      = 800
		callsPerReq   = 1
		storedCalls   = 30000 // calls rows of the session, most of them no request's
		writers       = 8
		writerWait    = time.Second
		session       = "sess-sweep"
		since         = int64(1000)
		sweepSettleTS = int64(2000)
	)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	store := openStoreAt(t, path)

	// Seed in one transaction: storedCalls calls of the session, none pointing
	// at a request yet; the first callsPerReq of each request's are its own.
	seed, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	insert, err := seed.PrepareContext(ctx, "INSERT INTO calls (tool_use_id, session_id, ts, tool, source) VALUES (?, ?, ?, 'Bash', ?)")
	if err != nil {
		t.Fatalf("prepare seed: %v", err)
	}
	callIDs := make([][]string, requests)
	for n := 0; n < storedCalls; n++ {
		id := fmt.Sprintf("toolu_%d", n)
		if r := n / callsPerReq; r < requests {
			callIDs[r] = append(callIDs[r], id)
		}
		if _, err := insert.ExecContext(ctx, id, session, sweepSettleTS, SourceHook); err != nil {
			t.Fatalf("seed call %s: %v", id, err)
		}
	}
	if err := insert.Close(); err != nil {
		t.Fatalf("close seed statement: %v", err)
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	handles := make([]*Store, writers)
	for i := range handles {
		handle, err := OpenDBWaiting(ctx, path, writerWait)
		if err != nil {
			t.Fatalf("open writer %d: %v", i, err)
		}
		handles[i] = handle
	}

	start := make(chan struct{})
	waits := make([]time.Duration, writers)
	errs := make([]error, writers)
	var group sync.WaitGroup
	for i := range handles {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			begun := time.Now()
			errs[i] = handles[i].UpsertCall(ctx, Call{
				ToolUseID: fmt.Sprintf("toolu_writer_%d", i),
				SessionID: Ptr("sess-other"),
				TS:        Ptr(sweepSettleTS),
				Tool:      Ptr("Read"),
				Source:    Ptr(SourceHook),
			}, Overwrite)
			waits[i] = time.Since(begun)
		}()
	}

	var heldAt time.Time
	sweepErr := store.BatchHeld(ctx, func() {
		heldAt = time.Now()
		close(start)
	}, func(tx *Tx) error {
		for r := 0; r < requests; r++ {
			err := tx.SettleRequest(ctx, Request{
				RequestID: fmt.Sprintf("msg_%d", r),
				SessionID: Ptr(session),
				Pending:   Ptr(false),
				Source:    Ptr(SourceHook),
				TS:        Ptr(sweepSettleTS),
			}, callIDs[r], since)
			if err != nil {
				return err
			}
		}
		return nil
	})
	hold := time.Since(heldAt)
	group.Wait()
	for i, handle := range handles {
		if err := handle.Close(); err != nil {
			t.Errorf("close writer %d: %v", i, err)
		}
	}

	t.Logf("the sweep held the write lock %v (writers wait %v)", hold, writerWait)
	if sweepErr != nil {
		t.Fatalf("the sweep: %v", sweepErr)
	}
	for i, err := range errs {
		t.Logf("writer %d waited %v", i, waits[i])
		switch {
		case err == nil:
		case IsBusy(err):
			t.Errorf("writer %d dropped its event, SQLITE_BUSY after %v: %v", i, waits[i], err)
		default:
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if got := row(t, store, "requests", "request_id = ?", "msg_0"); got == nil || got["calls"] != int64(callsPerReq) {
		t.Errorf("request msg_0 = %v, want %d calls", got, callsPerReq)
	}
}

// openIncompleteStore opens a store whose schema the open could not complete:
// a version-1 store opened while another connection holds the write lock.
func openIncompleteStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "callmeter.db")
	makeVersionOneStore(t, path)
	holderDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holderDB.Close(); err != nil {
			t.Errorf("close holder: %v", err)
		}
	})
	holder, err := holderDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holder.Close(); err != nil {
			t.Errorf("close holder connection: %v", err)
		}
	})
	if _, err := holder.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenDBWaiting(ctx, path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("OpenDBWaiting while the write lock is held: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := holder.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if store.SchemaComplete() {
		t.Fatal("SchemaComplete = true although the open could not write")
	}
	return store
}

func putMarks(t *testing.T, store *Store, agentID string, now int64, marks TranscriptMarks) {
	t.Helper()
	ctx := context.Background()
	if err := store.Batch(ctx, func(tx *Tx) error {
		return tx.PutMarks(ctx, "sess-demo", agentID, "/tmp/demo-home/.claude", now, marks)
	}); err != nil {
		t.Fatalf("PutMarks: %v", err)
	}
}

func TestPutMarksWritesTheRowsOnce(t *testing.T) {
	store := openTestStore(t)
	marks := readMarksFixture(t).Marks
	putMarks(t, store, "", 5000, marks)
	putMarks(t, store, "", 5000, marks)
	for table, want := range map[string]int{"compactions": 1, "stop_hooks": 1, "stop_hook_runs": 4, "turn_durations": 1, "session_costs": 1} {
		if got := count(t, store, table); got != want {
			t.Errorf("%s rows after a write and an identical write = %d, want %d", table, got, want)
		}
	}
	for table, want := range map[string]map[string]any{
		"compactions": {
			"entry_id": "4c8d0498-fbb2-45a0-b6ac-7279953c4fd7", "session_id": "sess-demo", "agent_id": nil, "ts": int64(1791113158553),
			"trigger": "auto", "pre_tokens": int64(333677), "post_tokens": int64(18677), "cumulative_dropped_tokens": int64(315000),
			"duration_ms": int64(72119), "seat_dir": "/tmp/demo-home/.claude",
		},
		"stop_hooks": {
			"entry_id": "8b7a2d0e-b849-4d10-a191-b1cd766d87f3", "session_id": "sess-demo", "agent_id": nil, "prompt_id": marksPrompt1,
			"ts": int64(1791065682790), "hook_count": int64(4), "hook_errors": int64(0), "seat_dir": "/tmp/demo-home/.claude",
		},
		"turn_durations": {
			"entry_id": "463cc8cc-6d27-4c84-b49d-06fa061a443b", "session_id": "sess-demo", "agent_id": nil, "prompt_id": marksPrompt1,
			"ts": int64(1791065682814), "duration_ms": int64(81557), "message_count": int64(60), "background_agents": int64(4),
			"seat_dir": "/tmp/demo-home/.claude",
		},
		"session_costs": {
			"session_id": "sess-demo", "ts": int64(5000), "started": int64(1791065248155), "cost_usd": 59.30682170000001,
			"api_ms": int64(9190688), "api_no_retry_ms": int64(9181361), "tool_ms": int64(3682088), "wall_ms": int64(53062358),
			"model_costs": `{"claude-opus-5-5":45.53485040000004,"claude-sonnet-5-5":13.771971300000002}`, "seat_dir": "/tmp/demo-home/.claude",
		},
	} {
		got := row(t, store, table, "1 = 1")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s row = %v\nwant %v", table, got, want)
		}
	}
	runs := row(t, store, "stop_hook_runs", "entry_id = ? AND seq = 4", "8b7a2d0e-b849-4d10-a191-b1cd766d87f3")
	if runs["name"] != "callmeter" || runs["duration_ms"] != nil || runs["ts"] != int64(1791065682790) ||
		runs["command_bytes"] != int64(len("${CLAUDE_PLUGIN_ROOT}/libexec/callmeter hook")) {
		t.Errorf("async hook run = %v, want callmeter, NULL duration, the summary's ts", runs)
	}
	// The cost-state is cumulative: a later one replaces the stored one, an
	// earlier one that lands after it (async hooks arrive in any order) does not.
	later := TranscriptMarks{Cost: &CostState{CostUSD: Ptr(60.5), WallMS: Ptr(int64(53062359))}}
	putMarks(t, store, "", 7000, later)
	if got := row(t, store, "session_costs", "session_id = ?", "sess-demo"); got["cost_usd"] != 60.5 || got["ts"] != int64(7000) ||
		got["wall_ms"] != int64(53062359) || got["started"] != nil || got["model_costs"] != nil {
		t.Errorf("session_costs after a later cost-state = %v", got)
	}
	putMarks(t, store, "", 9000, marks)
	if got := row(t, store, "session_costs", "session_id = ?", "sess-demo"); got["cost_usd"] != 60.5 || got["wall_ms"] != int64(53062359) || got["ts"] != int64(7000) {
		t.Errorf("session_costs after an earlier cost-state landed late = %v, want the later state kept, ts 7000", got)
	}
}

func TestPutMarksOfAnAgentKeepsItsIdAndNoCost(t *testing.T) {
	store := openTestStore(t)
	putMarks(t, store, "agent-1", 5000, readMarksFixture(t).Marks)
	if got := count(t, store, "session_costs"); got != 0 {
		t.Errorf("session_costs rows after an agent's marks = %d, want 0", got)
	}
	if got := row(t, store, "turn_durations", "1 = 1"); got["agent_id"] != "agent-1" {
		t.Errorf("turn_durations.agent_id = %v, want agent-1", got["agent_id"])
	}
	if got := row(t, store, "compactions", "1 = 1"); got["agent_id"] != "agent-1" {
		t.Errorf("compactions.agent_id = %v, want agent-1", got["agent_id"])
	}
}

func TestPutMarksIsANoOpOnAnIncompleteStore(t *testing.T) {
	ctx := context.Background()
	store := openIncompleteStore(t)
	putMarks(t, store, "", 5000, readMarksFixture(t).Marks)
	known, err := store.KnownMarks(ctx, "sess-demo")
	if err != nil || len(known) != 0 {
		t.Errorf("KnownMarks on an incomplete store = %v, %v; want empty, nil", known, err)
	}
	var tables int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('compactions','session_costs','stop_hooks','stop_hook_runs','turn_durations')").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("an incomplete store holds %d mark tables after PutMarks, want none", tables)
	}
	// The request still lands, without the column it cannot hold.
	if err := store.Batch(ctx, func(tx *Tx) error {
		return tx.SettleRequest(ctx, Request{
			RequestID: "msg_think", SessionID: Ptr("sess-demo"), TS: Ptr(int64(9)), OutputTokens: Ptr(int64(50)), ThinkingTokens: Ptr(int64(7)),
		}, nil, 0)
	}); err != nil {
		t.Fatalf("SettleRequest with thinking tokens on an incomplete store: %v", err)
	}
	if got := row(t, store, "requests", "request_id = ?", "msg_think"); got["output_tokens"] != int64(50) {
		t.Errorf("request row = %v, want output_tokens 50", got)
	}
}

func TestSettleRequestStoresThinkingTokens(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Batch(ctx, func(tx *Tx) error {
		for _, r := range []Request{
			{RequestID: "msg_think", SessionID: Ptr("sess-1"), TS: Ptr(int64(9)), OutputTokens: Ptr(int64(50)), ThinkingTokens: Ptr(int64(0))},
			{RequestID: "msg_plain", SessionID: Ptr("sess-1"), TS: Ptr(int64(9)), OutputTokens: Ptr(int64(50))},
			{RequestID: "msg_more", SessionID: Ptr("sess-1"), TS: Ptr(int64(9)), OutputTokens: Ptr(int64(50)), ThinkingTokens: Ptr(int64(31))},
		} {
			if err := tx.SettleRequest(ctx, r, nil, 0); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]any{"msg_think": int64(0), "msg_plain": nil, "msg_more": int64(31)} {
		if got := row(t, store, "requests", "request_id = ?", id)["thinking_tokens"]; got != want {
			t.Errorf("%s.thinking_tokens = %v, want %v", id, got, want)
		}
	}
}

// TestResolveRequestCarriesThinkingTokens: the merge of a provisional request
// into its message row keeps the stored thinking count and fills it where NULL.
func TestResolveRequestCarriesThinkingTokens(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.UpsertRequest(ctx, Request{RequestID: ProvisionalKey("toolu_1"), SessionID: Ptr("sess-1"), TS: Ptr(int64(5)),
		Pending: Ptr(true), ThinkingTokens: Ptr(int64(12))}, Overwrite); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveRequest(ctx, ProvisionalKey("toolu_1"), Request{RequestID: "msg_1", Model: Ptr("m")}); err != nil {
		t.Fatal(err)
	}
	if got := row(t, store, "requests", "request_id = ?", "msg_1")["thinking_tokens"]; got != int64(12) {
		t.Errorf("resolved thinking_tokens = %v, want 12", got)
	}
}

func TestKnownMarksAndSince(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	marks := readMarksFixture(t).Marks
	putMarks(t, store, "", 5000, marks)
	known, err := store.KnownMarks(ctx, "sess-demo")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"4c8d0498-fbb2-45a0-b6ac-7279953c4fd7": true, "8b7a2d0e-b849-4d10-a191-b1cd766d87f3": true, "463cc8cc-6d27-4c84-b49d-06fa061a443b": true,
	}
	if !reflect.DeepEqual(known, want) {
		t.Errorf("KnownMarks = %v, want %v", known, want)
	}
	if other, err := store.KnownMarks(ctx, "sess-other"); err != nil || len(other) != 0 {
		t.Errorf("KnownMarks of another session = %v, %v; want none", other, err)
	}
	left := marks.Since(0, known)
	if len(left.Compactions)+len(left.StopHooks)+len(left.TurnDurations) != 0 || left.Cost != marks.Cost {
		t.Errorf("Since with every id known = %+v, want only the cost", left)
	}
	// Older than since is not back-filled: the stop hook and turn duration
	// (1791065682xxx) go, the compaction (1791113158553) stays.
	young := marks.Since(1791100000000, nil)
	if len(young.Compactions) != 1 || len(young.StopHooks) != 0 || len(young.TurnDurations) != 0 || young.Cost == nil {
		t.Errorf("Since(1791100000000) = %+v", young)
	}
}

// TestPutMarksStoresNoCommandText: a hook's command and arguments never reach
// the store: only its derived name and its length.
func TestPutMarksStoresNoCommandText(t *testing.T) {
	store := openTestStore(t)
	read, err := readMarksVariant(t,
		"$CLAUDE_PROJECT_DIR/.claude/scripts/notify.sh stop", "/srv/PRIVATEPATH/hook.sh --token=PRIVATEARG",
		"Conversation compacted", "PRIVATECONTENT",
		"Summarize the notes.", "PRIVATEPROMPT",
		"Done.", "PRIVATEREPLY")
	if err != nil {
		t.Fatal(err)
	}
	putMarks(t, store, "", 5000, read.Marks)
	runs := row(t, store, "stop_hook_runs", "seq = 1")
	if runs["name"] != "hook.sh" || runs["command_bytes"] != int64(len("/srv/PRIVATEPATH/hook.sh --token=PRIVATEARG")) {
		t.Fatalf("hook run = %v, want name hook.sh and the command's length", runs)
	}
	tables, err := store.DB().Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := errors.Join(tables.Err(), tables.Close()); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		rows, err := store.DB().Query("SELECT * FROM " + name)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if text, ok := v.(string); ok && strings.Contains(text, "PRIVATE") {
					t.Errorf("%s.%s holds %q", name, columns[i], text)
				}
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
	}
}
