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
		('ev4', 'StopFailure', 9, '{"error":"rate_limit"}')`,
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
	want := []RedactCount{{"calls.input", 2}, {"calls.error", 2}, {"events.detail", 2}, {"faults.error", 2}, {"command_parts", 2}}
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
