package applog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func levelFixture(stderr *bytes.Buffer, dir, level, event, msg string) {
	Level(stderr, Sink{Levels: dir}, Row{Level: level, Event: event, Msg: msg, Where: "internal/applog/level_test.go#levelFixture", Session: "s1"})
}

func levelRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	lines := readLines(t, path)
	records := make([]map[string]any, len(lines))
	for i, line := range lines {
		if len([]byte(line))+1 > 4096 {
			t.Fatalf("row has %d bytes, want <=4096", len([]byte(line))+1)
		}
		if err := json.Unmarshal([]byte(line), &records[i]); err != nil {
			t.Fatalf("invalid row: %v", err)
		}
	}
	return records
}

func TestLevelRowsLandInTheirFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "levels")
	var stderr bytes.Buffer
	for _, tc := range []struct{ level, file, event string }{{"error", "errors.jsonl", "store-failed"}, {"warning", "warnings.jsonl", "transcript-unreadable"}, {"info", "info.jsonl", "report-done"}} {
		levelFixture(&stderr, dir, tc.level, tc.event, "one")
		raw, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Fatalf("level row missing in %s: %v", tc.file, err)
		}
		rows := levelRecords(t, filepath.Join(dir, tc.file))
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		row := rows[0]
		for key, want := range map[string]string{"plugin": "callmeter", "level": tc.level, "event": tc.event, "msg": "one", "where": "internal/applog/level_test.go#levelFixture", "session": "s1"} {
			if row[key] != want {
				t.Errorf("%s = %v, want %s", key, row[key], want)
			}
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", row["ts"].(string)); err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte(`{"ts":`)) || raw[len(raw)-1] != '\n' {
			t.Fatalf("wrong row framing: %q", raw)
		}
		info, err := os.Stat(filepath.Join(dir, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("mode = %o, want 600", info.Mode().Perm())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("successful writes produced stderr: %q", stderr.String())
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestLevelFallsBackToHomeLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CALLMETER_HOME", home)
	t.Setenv("PLUGIN_LOG_DIR", "")
	var stderr bytes.Buffer
	Failure(&stderr, ResolveSink(os.Getenv, home), "store", "s1", "", errors.New("database is locked"))
	path := filepath.Join(home, "logs", "errors.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fallback level row missing: %v", err)
	}
	row := levelRecords(t, path)[0]
	if row["event"] != "store-failed" || row["where"] != "internal/hookentry/callmeter.go#store" {
		t.Fatalf("fallback row = %v", row)
	}
}

func TestLevelWriteFailureIsOneStderrLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	levelFixture(&stderr, dir, "error", "store-failed", "one")
	levelFixture(&stderr, dir, "error", "store-failed", "two")
	if strings.Count(stderr.String(), "\n") != 1 || !strings.HasPrefix(stderr.String(), "callmeter: plugin log "+dir+": ") {
		t.Fatalf("fallback stderr = %q, want one plugin-log notice", stderr.String())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	levelFixture(&stderr, dir, "info", "report-done", "recovered")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	levelFixture(&stderr, dir, "error", "store-failed", "again")
	if strings.Count(stderr.String(), "\n") != 2 {
		t.Fatalf("rearmed stderr = %q, want two notices", stderr.String())
	}
}

func TestLevelCapMovesTheFileAside(t *testing.T) {
	for _, tc := range []struct {
		name, session  string
		cascade, extra bool
		sourceBytes    int
	}{
		{"short", "s1", false, false, 0},
		{"counterexample-4018", strings.Repeat("s", 3900), false, false, 4018},
		{"compact-marker-limit", strings.Repeat("s", 3911), false, false, 4029},
		{"optional-detail-limit", strings.Repeat("s", 3939), true, false, 4057},
		{"cascading-counterexample", strings.Repeat("s", 3900), true, false, 4018},
		{"escaped-session", strings.Repeat("<", 650), true, false, 4018},
		{"quoted-session", strings.Repeat("\"\\", 975), true, false, 4018},
		{"unicode-session", strings.Repeat("界", 1300), true, false, 4018},
		{"source-msg-and-detail", strings.Repeat("s", 3900), true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "errors.jsonl")
			warnings := filepath.Join(dir, "warnings.jsonl")
			for _, file := range []string{path, warnings} {
				if file == warnings && !tc.cascade {
					continue
				}
				if err := os.WriteFile(file, []byte("old\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(file, pluginLogCapBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			source := Row{TS: "2026-10-14T15:20:31.123Z", Plugin: "callmeter", Level: "error", Event: "x", Msg: "x", Where: "x", Session: tc.session}
			if tc.name == "short" {
				source.Event, source.Msg, source.Where = "store-failed", "new", "internal/applog/level_test.go#levelFixture"
			}
			if tc.extra {
				source.Msg = strings.Repeat("界<&", 1000)
				source.Detail = map[string]string{"error": strings.Repeat("x", 5000)}
			}
			wantSource, err := levelRow(source)
			if err != nil {
				t.Fatal(err)
			}
			if tc.sourceBytes != 0 && len(wantSource) != tc.sourceBytes {
				t.Fatalf("source bytes = %d, want %d", len(wantSource), tc.sourceBytes)
			}
			var stderr bytes.Buffer
			Level(&stderr, Sink{Levels: dir}, source)
			info, err := os.Stat(path + ".1")
			if err != nil {
				t.Fatalf("capped file was not moved aside: %v", err)
			}
			if info.Size() != pluginLogCapBytes+1 {
				t.Fatalf("cap discarded old bytes: %d", info.Size())
			}
			gotSource, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotSource, wantSource) {
				t.Fatalf("landed source differs: got %d bytes, want %d", len(gotSource), len(wantSource))
			}
			if stderr.Len() != 0 {
				t.Fatalf("successful cap stderr = %q", stderr.String())
			}
			rows := levelRecords(t, warnings)
			wantRows := 1
			if tc.cascade {
				wantRows = 2
			}
			if len(rows) != wantRows {
				t.Fatalf("cap notices = %d, want %d", len(rows), wantRows)
			}
			for _, row := range rows {
				for key, want := range map[string]string{"ts": source.TS, "plugin": "callmeter", "event": "log-capped", "level": "warning", "where": "internal/applog/level.go#Level", "session": tc.session} {
					if row[key] != want {
						t.Errorf("cap %s = %v, want complete %s", key, row[key], key)
					}
				}
				if tc.name == "optional-detail-limit" {
					if row["detail"] != nil || row["msg"] != "" {
						t.Fatalf("maximum identity notice = %v, want empty message and optional detail", row)
					}
				} else if tc.name != "short" {
					detail, ok := row["detail"].(map[string]any)
					if !ok || detail["truncated"] != true {
						t.Fatalf("cap detail = %v, want truncation marker", row["detail"])
					}
				}
			}
		})
	}
}

func TestLevelBoundsAndShape(t *testing.T) {
	for _, tc := range []struct {
		name, msg string
		detail    any
		wantMsg   string
		truncated bool
	}{
		{"exact", "one", nil, "one", false},
		{"detail-only", "keep this message", map[string]string{"error": strings.Repeat("x", 5000)}, "keep this message", true},
		{"unicode", strings.Repeat("界<&", 3000), nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := Row{TS: "2026-10-14T15:20:31.123Z", Plugin: "callmeter", Level: "error", Event: "store-failed", Msg: tc.msg, Where: "internal/hookentry/callmeter.go#store", Detail: tc.detail}
			raw, err := levelRow(row)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > 4096 {
				t.Fatalf("row bytes=%d", len(raw))
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if tc.wantMsg != "" && got["msg"] != tc.wantMsg {
				t.Fatalf("bounded msg=%q, want %q", got["msg"], tc.wantMsg)
			}
			if _, ok := got["session"]; ok {
				t.Fatalf("unknown session was emitted: %v", got)
			}
			if tc.truncated {
				d, ok := got["detail"].(map[string]any)
				if !ok || d["truncated"] != true || d["bytes"].(float64) <= 4096 {
					t.Fatalf("truncation metadata=%v", got["detail"])
				}
			}
			if tc.name == "exact" {
				want := `{"ts":"2026-10-14T15:20:31.123Z","plugin":"callmeter","level":"error","event":"store-failed","msg":"one","where":"internal/hookentry/callmeter.go#store"}` + "\n"
				if string(raw) != want {
					t.Fatalf("row=%s, want %s", raw, want)
				}
			}
		})
	}
}

func TestLevelFailureKeepsLegacyAndMapsStages(t *testing.T) {
	for _, tc := range []struct{ name, stage, level, event, cause string }{
		{"store", "store", "error", "store-failed", "database is locked"},
		{"seat", "seat", "error", "store-failed", "database is locked"},
		{"parse", "parse", "error", "payload-unparsable", "invalid payload"},
		{"payload", "payload", "error", "payload-unparsable", "invalid payload"},
		{"panic", "panic", "error", "hook-panic", "hook panicked"},
		{"terminated", "terminated", "error", "hook-terminated", "terminated by signal"},
		{"transcript", "transcript", "warning", "transcript-unreadable", "transcript unreadable"},
		{"empty", "store", "error", "store-failed", ""},
		{"bounded-unicode", "store", "error", "store-failed", strings.Repeat("界<&\"\\", 3000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sink := Sink{Log: filepath.Join(dir, "callmeter.log"), Levels: filepath.Join(dir, "lv")}
			var stderr bytes.Buffer
			cause := errors.New(tc.cause)
			Failure(&stderr, sink, tc.stage, "s1", "t1", cause)
			want := "callmeter: " + tc.stage + ": session \"s1\" call \"t1\": " + cause.Error() + "\n"
			if stderr.String() != want {
				t.Fatalf("legacy stderr=%q, want %q", stderr.String(), want)
			}
			var legacy map[string]string
			if err := json.Unmarshal([]byte(readLines(t, sink.Log)[0]), &legacy); err != nil {
				t.Fatal(err)
			}
			if legacy["step"] != tc.stage || legacy["level"] != "error" || legacy["err"] != cause.Error() {
				t.Fatalf("legacy row=%v", legacy)
			}
			file := "errors.jsonl"
			if tc.level == "warning" {
				file = "warnings.jsonl"
			}
			row := levelRecords(t, filepath.Join(sink.Levels, file))[0]
			if row["event"] != tc.event || row["level"] != tc.level || row["where"] != "internal/hookentry/callmeter.go#"+tc.stage || row["session"] != "s1" {
				t.Fatalf("stage row=%v", row)
			}
			detail, ok := row["detail"].(map[string]any)
			if !ok {
				t.Fatalf("failure detail=%v, want cause", row["detail"])
			}
			got, ok := detail["error"].(string)
			if !ok {
				t.Fatalf("failure detail.error=%v, want cause", detail["error"])
			}
			if tc.name == "bounded-unicode" {
				if got == "" || !strings.HasPrefix(tc.cause, got) || len(got) >= len(tc.cause) || detail["truncated"] != true {
					t.Fatalf("bounded cause=%q detail=%v, want truncated nonempty prefix", got, detail)
				}
			} else if got != tc.cause {
				t.Fatalf("failure detail.error=%q, want %q", got, tc.cause)
			}
		})
	}
}
