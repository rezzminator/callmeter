package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(values map[string]string) Getenv {
	return func(name string) string { return values[name] }
}

func TestHomeReturnsCallmeterHomeWhenSet(t *testing.T) {
	got, err := Home(env(map[string]string{"CALLMETER_HOME": "/x/cm", "XDG_STATE_HOME": "/x/state", "HOME": "/h"}))
	if err != nil || got != "/x/cm" {
		t.Fatalf("Home() = %q, %v, want /x/cm", got, err)
	}
}

func TestResolveHomeFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"override wins", map[string]string{"CALLMETER_HOME": "/x/cm", "XDG_STATE_HOME": "/x/state"}, "/x/cm", ""},
		{"xdg state", map[string]string{"XDG_STATE_HOME": "/x/state", "HOME": "/h"}, "/x/state/callmeter", ""},
		{"relative xdg is ignored", map[string]string{"XDG_STATE_HOME": "state", "HOME": "/h"}, "/h/.local/state/callmeter", ""},
		{"home default", map[string]string{"HOME": "/h"}, "/h/.local/state/callmeter", ""},
		{"no home at all", map[string]string{}, "", "HOME is empty"},
		{"xdg alone needs no home", map[string]string{"XDG_STATE_HOME": "/x/state"}, "/x/state/callmeter", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveHome(env(tt.env), false)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveHome() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveHome() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

// TestHomeRefusesTheRealHomeUnderGoTest pins the jail's last line: with
// CALLMETER_HOME empty inside go test, Home is an error even when HOME and
// XDG_STATE_HOME would name a real directory.
func TestHomeRefusesTheRealHomeUnderGoTest(t *testing.T) {
	got, err := Home(env(map[string]string{"XDG_STATE_HOME": "/x/state", "HOME": "/h"}))
	if err == nil {
		t.Fatalf("Home() = %q, nil, want an error under go test", got)
	}
	if !strings.Contains(err.Error(), "CALLMETER_HOME") {
		t.Fatalf("error = %v, want it to name CALLMETER_HOME", err)
	}
	if got != "" {
		t.Fatalf("Home() = %q alongside the refusal, want empty", got)
	}
}

// TestHomeUnderTheJailIsTheJailsOwnHome pins that the test jail's variable
// reaches Home through the process environment.
func TestHomeUnderTheJailIsTheJailsOwnHome(t *testing.T) {
	jailed := os.Getenv("CALLMETER_HOME")
	if jailed == "" {
		t.Fatal("CALLMETER_HOME is empty: the package's TestMain did not run the jail")
	}
	got, err := Home(os.Getenv)
	if err != nil || got != jailed {
		t.Fatalf("Home(os.Getenv) = %q, %v, want %q", got, err, jailed)
	}
}

func TestFilesUnderHome(t *testing.T) {
	home := "/x/cm"
	for name, tt := range map[string]struct{ got, want string }{
		"store":    {Store(home), "/x/cm/callmeter.db"},
		"config":   {Config(home), "/x/cm/config.json"},
		"bincache": {BinCache(home, "0.1.0"), "/x/cm/bin/0.1.0/callmeter"},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", name, tt.got, tt.want)
		}
	}
}

// TestLogsLiveInThePluginLogDirectory: callmeter.log and missed.log sit in
// $PLUGIN_LOG_DIR/callmeter, else $HOME/.local/state/callmeter/logs, never
// under CALLMETER_HOME; with neither set there is no log file.
func TestLogsLiveInThePluginLogDirectory(t *testing.T) {
	for name, tt := range map[string]struct {
		env  map[string]string
		want string
	}{
		"shared":  {map[string]string{"PLUGIN_LOG_DIR": "/l", "HOME": "/h", "CALLMETER_HOME": "/x/cm"}, "/l/callmeter"},
		"home":    {map[string]string{"HOME": "/h", "CALLMETER_HOME": "/x/cm"}, "/h/.local/state/callmeter/logs"},
		"neither": {map[string]string{"CALLMETER_HOME": "/x/cm"}, ""},
	} {
		dir := LogDir(func(key string) string { return tt.env[key] })
		if dir != tt.want {
			t.Errorf("%s: LogDir = %q, want %q", name, dir, tt.want)
		}
		wantLog, wantMissed := "", ""
		if tt.want != "" {
			wantLog, wantMissed = tt.want+"/callmeter.log", tt.want+"/missed.log"
		}
		if Log(dir) != wantLog || Missed(dir) != wantMissed {
			t.Errorf("%s: Log, Missed = %q, %q, want %q, %q", name, Log(dir), Missed(dir), wantLog, wantMissed)
		}
	}
}

func TestSeatDir(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"unset falls back to home claude", map[string]string{"HOME": "/h"}, "/h/.claude", ""},
		{"absolute is cleaned", map[string]string{"CLAUDE_CONFIG_DIR": "/s/a/../b/", "HOME": "/h"}, "/s/b", ""},
		{"relative becomes absolute", map[string]string{"CLAUDE_CONFIG_DIR": "seat/dir"}, filepath.Join(cwd, "seat", "dir"), ""},
		{"unset without home is an error", map[string]string{}, "", "HOME is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SeatDir(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("SeatDir() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("SeatDir() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestSeatDirKeepsSymlinksUnresolved(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := SeatDir(env(map[string]string{"CLAUDE_CONFIG_DIR": link}))
	if err != nil || got != link {
		t.Fatalf("SeatDir() = %q, %v, want the link %q unresolved", got, err, link)
	}
}

func TestConfigDir(t *testing.T) {
	got, err := ConfigDir(env(map[string]string{"HOME": "/h", "CLAUDE_CONFIG_DIR": "/elsewhere"}))
	if err != nil || got != "/h/.claude" {
		t.Fatalf("ConfigDir() = %q, %v, want /h/.claude", got, err)
	}
	if _, err := ConfigDir(env(nil)); err == nil || !strings.Contains(err.Error(), "HOME is empty") {
		t.Fatalf("ConfigDir() without HOME error = %v, want it to say HOME is empty", err)
	}
}

func TestCostLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"named", map[string]string{"PFM_COST_LEDGER_DIR": "/ledger", "HOME": "/h"}, "/ledger"},
		{"default under HOME", map[string]string{"HOME": "/h"}, "/h/.local/state/pfm/cost-ledger"},
		{"default under PFM_HOME", map[string]string{"HOME": "/h", "PFM_HOME": "/p"}, "/p/.local/state/pfm/cost-ledger"},
	} {
		got, err := CostLedger(env(tc.values))
		if err != nil || got != tc.want {
			t.Errorf("%s: CostLedger = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if got, err := CostLedger(env(nil)); err == nil || got != "" {
		t.Errorf("CostLedger without HOME = %q, %v; want an error: no ledger directory can be named", got, err)
	}
}
