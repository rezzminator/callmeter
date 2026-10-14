package applog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	for _, tc := range []struct{ level, file, event string }{{"error", "errors.jsonl", "store-failed"}, {"warning", "warnings.jsonl", "transcript-unreadable"}, {"info", "info.jsonl", "logger-ready"}} {
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
	levelFixture(&stderr, dir, "info", "logger-ready", "recovered")
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
		name, session          string
		cascade, extra, refuse bool
		sourceBytes            int
	}{
		{"short", "s1", false, false, false, 0},
		{"counterexample-4018", strings.Repeat("s", 3900), false, false, false, 4018},
		{"compact-marker-limit", strings.Repeat("s", 3911), false, false, false, 4029},
		{"optional-detail-limit", strings.Repeat("s", 3939), true, false, false, 4057},
		{"past-optional-detail-limit", strings.Repeat("s", 3940), false, false, true, 4058},
		{"cascading-counterexample", strings.Repeat("s", 3900), true, false, false, 4018},
		{"escaped-session", strings.Repeat("<", 650), true, false, false, 4018},
		{"quoted-session", strings.Repeat("\"\\", 975), true, false, false, 4018},
		{"unicode-session", strings.Repeat("界", 1300), true, false, false, 4018},
		{"source-msg-and-detail", strings.Repeat("s", 3900), true, true, false, 0},
		{"immutable-notice-too-large", strings.Repeat("s", 3978), false, false, true, 4096},
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
			if tc.refuse {
				if !strings.Contains(stderr.String(), "metadata exceeds 4096 bytes") || strings.Count(stderr.String(), "\n") != 1 {
					t.Fatalf("immutable metadata refusal = %q", stderr.String())
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Size() != pluginLogCapBytes+1 {
					t.Fatalf("impossible notice changed source file: size %d, want %d", info.Size(), pluginLogCapBytes+1)
				}
				if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
					t.Fatalf("impossible notice archive stat = %v, want absent", err)
				}
				return
			}
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
			if tc.name == "compact-marker-limit" || tc.name == "optional-detail-limit" {
				for _, line := range readLines(t, warnings) {
					if len(line)+1 != rowMaxBytes {
						t.Fatalf("maximum notice bytes = %d, want %d", len(line)+1, rowMaxBytes)
					}
				}
			}
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
			if stderr.Len() != 0 {
				t.Fatalf("successful cap stderr = %q", stderr.String())
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
	for _, tc := range []struct{ stage, level, event string }{{"store", "error", "store-failed"}, {"seat", "error", "store-failed"}, {"parse", "error", "payload-unparsable"}, {"payload", "error", "payload-unparsable"}, {"panic", "error", "hook-panic"}, {"terminated", "error", "hook-terminated"}, {"transcript", "warning", "transcript-unreadable"}} {
		t.Run(tc.stage, func(t *testing.T) {
			dir := t.TempDir()
			sink := Sink{Log: filepath.Join(dir, "callmeter.log"), Levels: filepath.Join(dir, "lv")}
			var stderr bytes.Buffer
			cause := errors.New("database is locked")
			if tc.stage == "panic" {
				cause = errors.New("PRIVATE_FILE_BODY")
			}
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
			raw, err := os.ReadFile(filepath.Join(sink.Levels, file))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("PRIVATE_FILE_BODY")) {
				t.Fatal("persisted private failure body")
			}
			row := levelRecords(t, filepath.Join(sink.Levels, file))[0]
			if row["event"] != tc.event || row["level"] != tc.level {
				t.Fatalf("stage row=%v", row)
			}
		})
	}
}

func TestLevelSecuresExistingObjects(t *testing.T) {
	for _, capped := range []bool{false, true} {
		t.Run(map[bool]string{false: "append", true: "cap"}[capped], func(t *testing.T) {
			parent := t.TempDir()
			if err := os.Chmod(parent, 0755); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(parent, "levels")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "errors.jsonl")
			legacy := filepath.Join(parent, "callmeter.log")
			for _, file := range []string{path, legacy, filepath.Join(dir, "errors.jsonl.1"), filepath.Join(dir, "warnings.jsonl.1"), filepath.Join(dir, "info.jsonl.1")} {
				if err := os.WriteFile(file, []byte("{\"msg\":\"old\"}\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if capped {
				if err := os.Truncate(path, pluginLogCapBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			Level(&stderr, Sink{Log: legacy, Levels: dir}, Row{Level: "error", Event: "store-failed", Msg: "new", Where: "test", Session: "s1"})
			for file, mode := range map[string]os.FileMode{parent: 0755, legacy: 0644, dir: 0700, path: 0600, filepath.Join(dir, "errors.jsonl.1"): 0600, filepath.Join(dir, "warnings.jsonl.1"): 0600, filepath.Join(dir, "info.jsonl.1"): 0600} {
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Errorf("%s mode = %o, want %o", file, info.Mode().Perm(), mode)
				}
			}
			rows := levelRecords(t, path)
			if got := rows[len(rows)-1]["msg"]; got != "new" {
				t.Fatalf("row = %v", got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestLevelChmodFailureRearmsAfterRecovery(t *testing.T) {
	for _, site := range []string{"directory", "file"} {
		t.Run(site, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "levels")
			oldDir, oldFile := beforeLevelDirChmod, beforeLevelFileChmod
			t.Cleanup(func() { beforeLevelDirChmod, beforeLevelFileChmod = oldDir, oldFile })
			if site == "directory" {
				beforeLevelDirChmod = func(path string) {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				beforeLevelFileChmod = func(file *os.File) {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			var stderr bytes.Buffer
			for i := 0; i < 2; i++ {
				levelFixture(&stderr, dir, "error", "store-failed", "failed")
			}
			if strings.Count(stderr.String(), "\n") != 1 || !strings.Contains(stderr.String(), "chmod level "+site) || !strings.Contains(stderr.String(), dir) {
				t.Fatalf("chmod failure stderr = %q, want one contextual notice", stderr.String())
			}
			beforeLevelDirChmod, beforeLevelFileChmod = nil, nil
			levelFixture(&stderr, dir, "error", "store-failed", "recovered")
			rows := levelRecords(t, filepath.Join(dir, "errors.jsonl"))
			if len(rows) != 1 || rows[0]["msg"] != "recovered" {
				t.Fatalf("recovery rows = %v", rows)
			}
			if site == "directory" {
				beforeLevelDirChmod = func(path string) {
					if err := os.RemoveAll(path); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				beforeLevelFileChmod = func(file *os.File) {
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			levelFixture(&stderr, dir, "error", "store-failed", "failed again")
			if strings.Count(stderr.String(), "\n") != 2 {
				t.Fatalf("rearmed stderr = %q", stderr.String())
			}
		})
	}
}

func TestLevelPreservesIntentionalDirectoryRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root writer required")
	}
	for _, capped := range []bool{false, true} {
		t.Run(map[bool]string{false: "creation", true: "cap"}[capped], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "errors.jsonl")
			if capped {
				if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, pluginLogCapBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(dir, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(dir, 0700); err != nil {
					t.Error(err)
				}
			})
			var stderr bytes.Buffer
			levelFixture(&stderr, dir, "error", "store-failed", "one")
			levelFixture(&stderr, dir, "error", "store-failed", "two")
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0500 || strings.Count(stderr.String(), "\n") != 1 {
				t.Fatalf("mode=%o stderr=%q", info.Mode().Perm(), stderr.String())
			}
			if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
				t.Fatalf("refused cap changed archive: %v", err)
			}
		})
	}
}
func TestLevelCompetingCapKeepsCurrentRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "errors.jsonl")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, pluginLogCapBytes+1); err != nil {
		t.Fatal(err)
	}
	old := beforeLevelFileChmod
	t.Cleanup(func() { beforeLevelFileChmod = old })
	beforeLevelFileChmod = func(*os.File) {
		beforeLevelFileChmod = nil
		if err := os.Rename(path, path+".1"); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	levelFixture(&stderr, dir, "error", "store-failed", "current")
	if stderr.Len() != 0 {
		t.Fatalf("competing cap dropped current row: %s", stderr.String())
	}
	if got := levelRecords(t, path)[0]["msg"]; got != "current" {
		t.Fatalf("row=%v", got)
	}
}

func TestLevelRefusesForeignAndSpecialEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "dangling", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			foreign := filepath.Join(t.TempDir(), "foreign")
			if err := os.WriteFile(foreign, []byte("private"), 0644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "errors.jsonl")
			switch kind {
			case "symlink":
				if err := os.Symlink(foreign, path); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := os.Symlink(foreign+"-missing", path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var stderr bytes.Buffer
			done := make(chan struct{})
			go func() { levelFixture(&stderr, dir, "error", "store-failed", "one"); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("logger blocked on nonregular entry")
			}
			if stderr.Len() == 0 {
				t.Fatal("unsafe endpoint accepted")
			}
			raw, err := os.ReadFile(foreign)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(foreign)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != "private" || info.Mode().Perm() != 0644 {
				t.Fatalf("foreign target changed: %q mode %o", raw, info.Mode().Perm())
			}
		})
	}
}

func TestLevelTrustedHomeAncestors(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "override"}[override], func(t *testing.T) {
			root := t.TempDir()
			realHome := filepath.Join(root, "real-home")
			home := filepath.Join(root, "home")
			if err := os.Mkdir(realHome, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(realHome, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(realHome, home); err != nil {
				t.Fatal(err)
			}
			getenv := func(key string) string {
				if key == "PLUGIN_LOG_DIR" && override {
					return filepath.Join(home, "plugin-logs")
				}
				return ""
			}
			sink := ResolveSink(getenv, filepath.Join(home, ".local", "state", "callmeter"))
			var stderr bytes.Buffer
			levelFixture(&stderr, sink.Levels, "error", "store-failed", "first")
			path := filepath.Join(sink.Levels, "errors.jsonl")
			if got := levelRecords(t, path)[0]["msg"]; got != "first" {
				t.Fatalf("trusted-home first row = %v", got)
			}
			if err := os.Truncate(path, pluginLogCapBytes+1); err != nil {
				t.Fatal(err)
			}
			levelFixture(&stderr, sink.Levels, "error", "store-failed", "after cap")
			if got := levelRecords(t, path)[0]["msg"]; got != "after cap" {
				t.Fatalf("trusted-home cap row = %v", got)
			}
			if row := levelRecords(t, filepath.Join(sink.Levels, "warnings.jsonl"))[0]; row["event"] != "log-capped" || row["session"] != "s1" {
				t.Fatalf("trusted-home cap notice = %v", row)
			}
			for file, mode := range map[string]os.FileMode{realHome: 0755, sink.Levels: 0700, path: 0600, path + ".1": 0600} {
				info, err := os.Stat(file)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != mode {
					t.Fatalf("trusted-home %s mode = %o, want %o", file, info.Mode().Perm(), mode)
				}
			}
			info, err := os.Lstat(home)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSymlink == 0 || stderr.Len() != 0 {
				t.Fatalf("trusted-home link mode = %v, stderr = %q", info.Mode(), stderr.String())
			}
		})
	}
}

func TestLevelTrustedAncestorsDoNotWeakenLeafProtections(t *testing.T) {
	for _, site := range []string{"directory", "active", "backup"} {
		for _, kind := range []string{"symlink", "dangling", "fifo", "directory"} {
			if site == "directory" && kind == "directory" {
				continue
			}
			t.Run(site+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				realHome, home := filepath.Join(root, "real-home"), filepath.Join(root, "home")
				if err := os.Mkdir(realHome, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(realHome, home); err != nil {
					t.Fatal(err)
				}
				foreign := filepath.Join(root, "foreign")
				if site == "directory" {
					if err := os.Mkdir(foreign, 0755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(foreign, []byte("private"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chmod(foreign, 0755); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(home, "levels")
				path := dir
				if site != "directory" {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(dir, "errors.jsonl")
					if site == "backup" {
						path += ".1"
					}
				}
				switch kind {
				case "symlink":
					if err := os.Symlink(foreign, path); err != nil {
						t.Fatal(err)
					}
				case "dangling":
					if err := os.Symlink(foreign+"-missing", path); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				var stderr bytes.Buffer
				done := make(chan struct{})
				go func() { levelFixture(&stderr, dir, "error", "store-failed", "one"); close(done) }()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("trusted ancestor logger blocked on unsafe leaf")
				}
				if strings.Count(stderr.String(), "\n") != 1 {
					t.Fatalf("unsafe %s %s fallback = %q", site, kind, stderr.String())
				}
				info, err := os.Stat(foreign)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0755 {
					t.Fatalf("foreign target mode changed to %o", info.Mode().Perm())
				}
				if site != "directory" {
					raw, err := os.ReadFile(foreign)
					if err != nil {
						t.Fatal(err)
					}
					if string(raw) != "private" {
						t.Fatalf("foreign target bytes = %q", raw)
					}
				}
			})
		}
	}
}
