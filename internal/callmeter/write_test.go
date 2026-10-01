package callmeter

import (
	"context"
	"reflect"
	"strings"
	"testing"
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
	want := map[string]any{"ts": int64(1000), "tool": "Bash", "source": SourceHook, "bytes_delivered": int64(42)}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
	if inserted := row(t, store, "calls", "tool_use_id = ?", "toolu_2"); inserted == nil || inserted["tool"] != "Grep" {
		t.Errorf("FillEmpty did not insert the missing row: %v", inserted)
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
