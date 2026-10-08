package callmeter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// oldRows are rows as the store held them before the heredoc and free-text
// rules: invented content only.
var oldRows = []string{
	`INSERT INTO calls (tool_use_id, tool, ts, cwd, input, error) VALUES
		('toolu_heredoc', 'Bash', 1, '/tmp/demo-proj',
		 '{"command":"cat > /tmp/demo-proj/a.txt <<''EOF''\nprivate-body-line\nEOF\nwc -l /tmp/demo-proj/a.txt","description":"Write private-description"}',
		 'Exit code 1' || char(10) || 'private-output-line'),
		('toolu_plain', 'Bash', 2, '/tmp/demo-proj', '{"command":"ls /tmp/demo-proj"}', NULL),
		('toolu_message', 'SendMessage', 3, '/tmp/demo-proj', '{"message":"private-message","to":"helper"}', NULL),
		('toolu_denied', 'Bash', 4, '/tmp/demo-proj', '{"command":"ls"}', 'denied by permission'),
		('toolu_read', 'Read', 5, '/tmp/demo-proj', '{"file_path":"/tmp/demo-proj/a.txt"}', 'private-tool-output')`,
	`INSERT INTO command_parts (tool_use_id, seq, lang, program, args, files, parse_status, parser) VALUES
		('toolu_heredoc', 0, 'sh', 'cat', '[]', '[]', 'ok', 1),
		('toolu_heredoc', 1, 'sh', 'wc', '["-l"]', '[]', 'ok', 1),
		('toolu_plain', 0, 'sh', 'ls', '[]', '[]', 'ok', 1)`,
	`INSERT INTO events (event_id, event, ts, detail) VALUES
		('ev1', 'Notification', 6, '{"notification_type":"idle","message":"private-note"}'),
		('ev2', 'Notification', 7, '{"notification_type":"idle","message_bytes":4}'),
		('ev3', 'PermissionDenied', 8, '{"reason":"private-reason"}'),
		('ev4', 'StopFailure', 9, '{"error":"rate_limit"}'),
		('ev5', 'PermissionDenied', 10, '{"tool_name":"Bash","tool_use_id_bytes":12}'),
		('ev6', 'PermissionDenied', 11, '{"tool_use_id":"toolu_denied"}'),
		('ev8', 'PermissionDenied', 13, '{"tool_name":"mcp__mail__send","tool_input":{"destination":"private-to@x","timeout":3}}')`,
	`INSERT INTO events (event_id, event, ts, tool_name, detail) VALUES
		('ev7', 'PermissionRequest', 12, 'mcp__ide__run', '{"tool_input":{"source":"SECRETCODE","timeout":3}}')`,
	`INSERT INTO faults (ts, tool_use_id, stage, error) VALUES
		(10, 'toolu_plain', 'parse', 'part 0 (): 1:9: private-token must be followed by )'),
		(11, 'toolu_plain', 'parse', 'part 2 (grep): 1:4: reached EOF near private-quote'),
		(12, NULL, 'binary', 'PreToolUse: download failed')`,
}

func TestRedactRewritesStoredRows(t *testing.T) {
	ctx := context.Background()
	store, err := OpenDB(ctx, filepath.Join(t.TempDir(), "callmeter.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	for _, stmt := range oldRows {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	counts, err := store.Redact(ctx)
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	want := []RedactCount{{"calls.input", 2}, {"calls.error", 2}, {"events.detail", 4}, {"faults.error", 2}, {"stop_hook_runs.name", 0}, {"command_parts", 2}}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("Redact = %v, want %v", counts, want)
	}
	var dump string
	if err := store.DB().QueryRow(`SELECT group_concat(COALESCE(input, '') || '|' || COALESCE(error, ''), char(10)) FROM calls`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	var detail string
	if err := store.DB().QueryRow(`SELECT group_concat(detail, char(10)) FROM events`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, `{"reason_bytes":14}`) || !strings.Contains(detail, `{"error":"rate_limit"}`) {
		t.Fatalf("details = %s, want the unknown reason sized and the StopFailure label kept", detail)
	}
	// A PermissionDenied detail stored before its tool_use_id was kept as an id,
	// and one stored after, are both their own sanitized form.
	if !strings.Contains(detail, `{"tool_name":"Bash","tool_use_id_bytes":12}`) || !strings.Contains(detail, `{"tool_use_id":"toolu_denied"}`) {
		t.Fatalf("details = %s, want both PermissionDenied details unchanged", detail)
	}
	// An mcp__ tool's input in a permission event keeps no string, its tool
	// read from the row's tool_name column, or from the detail of a
	// PermissionDenied stored before that event filled the column.
	if !strings.Contains(detail, `{"tool_input":{"source_bytes":10,"timeout":3}}`) || strings.Contains(detail, "SECRETCODE") {
		t.Fatalf("details = %s, want the mcp__ tool's input source sized", detail)
	}
	if !strings.Contains(detail, `{"tool_name":"mcp__mail__send","tool_input":{"destination_bytes":12,"timeout":3}}`) {
		t.Fatalf("details = %s, want the mcp__ tool named in its detail to have its input sized", detail)
	}
	var faults string
	if err := store.DB().QueryRow(`SELECT group_concat(error, '|') FROM (SELECT error FROM faults ORDER BY ts)`).Scan(&faults); err != nil {
		t.Fatal(err)
	}
	wantFaults := "part 0 (no program parsed): " + ErrorNotStored + " (40 bytes)|part 2 (grep): " + ErrorNotStored + " (35 bytes)|PreToolUse: download failed"
	if faults != wantFaults {
		t.Fatalf("faults = %q\nwant     %q", faults, wantFaults)
	}
	detail += faults
	if strings.Contains(dump+detail, "private") {
		t.Fatalf("free text left in the store:\n%s\n%s", dump, detail)
	}
	var input string
	if err := store.DB().QueryRow(`SELECT input FROM calls WHERE tool_use_id = 'toolu_heredoc'`).Scan(&input); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		t.Fatalf("decode %s: %v", input, err)
	}
	if fields["command"] != "cat > /tmp/demo-proj/a.txt <<'EOF'\nEOF\nwc -l /tmp/demo-proj/a.txt" ||
		fields["heredoc_bytes"] != float64(len("private-body-line\n")) ||
		fields["description_bytes"] != float64(len("Write private-description")) {
		t.Fatalf("redacted input = %s", input)
	}
	var errorsKept string
	if err := store.DB().QueryRow(`SELECT group_concat(error, '|') FROM (SELECT error FROM calls WHERE error IS NOT NULL ORDER BY ts)`).Scan(&errorsKept); err != nil {
		t.Fatal(err)
	}
	if errorsKept != "Exit code 1|denied by permission|"+ErrorNotStored {
		t.Fatalf("errors = %q", errorsKept)
	}
	var parts int
	if err := store.DB().QueryRow(`SELECT count(*) FROM command_parts WHERE tool_use_id = 'toolu_plain'`).Scan(&parts); err != nil || parts != 1 {
		t.Fatalf("the unchanged call's parts = %d (%v), want 1 kept", parts, err)
	}
	again, err := store.Redact(ctx)
	if err != nil {
		t.Fatalf("second Redact: %v", err)
	}
	for _, c := range again {
		if c.Rows != 0 {
			t.Fatalf("second Redact = %v, want nothing changed", again)
		}
	}
}

// A row Redact cannot rewrite fails the pass and leaves every row as it was.
func TestRedactFailureChangesNothing(t *testing.T) {
	ctx := context.Background()
	store, err := OpenDB(ctx, filepath.Join(t.TempDir(), "callmeter.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := store.DB().Exec(oldRows[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO events (event_id, event, ts, detail) VALUES ('bad', 'Notification', 9, 'not json')`); err != nil {
		t.Fatal(err)
	}
	if counts, err := store.Redact(ctx); err == nil || !strings.Contains(err.Error(), "events.detail of row bad") {
		t.Fatalf("Redact = (%v, %v), want the bad detail named", counts, err)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT count(*) FROM calls WHERE instr(input, 'private-body-line') > 0`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows still holding the body = %d (%v), want 1: the pass must roll back", n, err)
	}
}

func TestSanitizeError(t *testing.T) {
	cases := map[string]string{
		"Exit code 2\nprivate-output-line":  "Exit code 2",
		"Exit code 1":                       "Exit code 1",
		OutcomeDeniedByHook:                 OutcomeDeniedByHook,
		OutcomeDeniedByPermission:           OutcomeDeniedByPermission,
		OutcomeRejectedByUser:               OutcomeRejectedByUser,
		OutcomeRefused:                      OutcomeRefused,
		"File does not exist: private-path": ErrorNotStored,
		"Exit code 1 private-tail":          ErrorNotStored,
	}
	for text, want := range cases {
		if got := SanitizeError(text); got != want {
			t.Errorf("SanitizeError(%q) = %q, want %q", text, got, want)
		}
	}
}

// A Stop-hook name an older namer took from a prompt hook's prose becomes
// prompt# and a hash salted with its row, once; every name the current namer
// can emit stays. Invented names only.
func TestRedactRewritesProseHookNames(t *testing.T) {
	ctx := context.Background()
	store, err := OpenDB(ctx, filepath.Join(t.TempDir(), "callmeter.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	kept := []string{"callmeter", "notify.sh", "guard-stamp.sh", "codex-sync.sh", "#0123abcd", "prompt#89abcdef"}
	for i, name := range append([]string{"I"}, kept...) {
		if _, err := store.DB().Exec(`INSERT INTO stop_hook_runs (entry_id, seq, name, command_bytes) VALUES ('e1', ?, ?, 327)`, i, name); err != nil {
			t.Fatal(err)
		}
	}
	names := func() []string {
		rows, err := store.DB().Query(`SELECT name FROM stop_hook_runs ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		return got
	}
	for pass, want := range []int64{1, 0} {
		counts, err := store.Redact(ctx)
		if err != nil {
			t.Fatalf("Redact pass %d: %v", pass+1, err)
		}
		var got int64 = -1
		for _, c := range counts {
			if c.Column == "stop_hook_runs.name" {
				got = c.Rows
			}
		}
		if got != want {
			t.Errorf("pass %d: stop_hook_runs.name rewritten = %d, want %d (%v)", pass+1, got, want, counts)
		}
		after := names()
		if len(after) != 7 || !strings.HasPrefix(after[0], "prompt#") || len(after[0]) != len("prompt#")+8 || after[0] == "prompt#a83dd0cc" {
			t.Errorf("pass %d: the prose name became %q, want prompt# and a salted 8-hex hash", pass+1, after[0])
		}
		if !reflect.DeepEqual(after[1:], kept) {
			t.Errorf("pass %d: kept names = %v, want %v", pass+1, after[1:], kept)
		}
	}
}
