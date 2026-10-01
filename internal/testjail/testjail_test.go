package testjail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// jailVars are every variable run() sets or clears; a test that runs a nested
// jail registers each with t.Setenv first so the outer jail is restored.
var jailVars = []string{
	"HOME", "TMPDIR", "CALLMETER_HOME", "XDG_STATE_HOME",
	"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SESSION_ID", "CLAUDE_EFFORT", "CALLMETER_BIN",
}

func preserveJailVars(t *testing.T) {
	t.Helper()
	for _, name := range jailVars {
		t.Setenv(name, os.Getenv(name))
	}
}

func canonicalTmp(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func under(path, dir string) bool {
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// TestJailPointsHomeTmpdirAndCallmeterHomeIntoTemp pins the jail this very
// package runs under: HOME, TMPDIR and CALLMETER_HOME inside the canonical
// temp base, the session variables empty.
func TestJailPointsHomeTmpdirAndCallmeterHomeIntoTemp(t *testing.T) {
	base := canonicalTmp(t)
	if got := os.Getenv("TMPDIR"); got != base {
		t.Errorf("TMPDIR = %q, want the canonical /tmp %q", got, base)
	}
	for _, name := range []string{"HOME", "CALLMETER_HOME", "XDG_STATE_HOME"} {
		value := os.Getenv(name)
		if !under(value, base) {
			t.Errorf("%s = %q, want a directory under %q", name, value, base)
			continue
		}
		if info, err := os.Stat(value); err != nil || !info.IsDir() {
			t.Errorf("%s = %q is not an existing directory: %v", name, value, err)
		}
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SESSION_ID", "CLAUDE_EFFORT", "CALLMETER_BIN"} {
		if value := os.Getenv(name); value != "" {
			t.Errorf("%s = %q, want empty", name, value)
		}
	}
	if os.Getenv("HOME") == os.Getenv("CALLMETER_HOME") {
		t.Error("HOME and CALLMETER_HOME are one directory, want separate ones")
	}
	if dir := t.TempDir(); !under(dir, base) {
		t.Errorf("t.TempDir() = %q, want it under %q", dir, base)
	}
}

// TestRunPinsGoDirsBeforeHomeMoves pins that a go child still finds the real
// toolchain caches: each is set, absolute and outside the jail HOME.
func TestRunPinsGoDirsBeforeHomeMoves(t *testing.T) {
	home := os.Getenv("HOME")
	for _, name := range []string{"GOCACHE", "GOPATH", "GOMODCACHE", "TEST_TELEMETRY_DIR"} {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is unset after Run — a go child would write under the jailed home %s", name, home)
		}
		if !filepath.IsAbs(value) {
			t.Fatalf("%s %q is not absolute", name, value)
		}
		if under(value, home) {
			t.Fatalf("%s %q sits under the jail home %s", name, value, home)
		}
	}
}

// TestRunPassesTheCodeThroughAndRemovesTheJail pins run's contract on the
// success path: the tests' own exit code returns, they saw a jail, and the
// jail root is gone afterwards.
func TestRunPassesTheCodeThroughAndRemovesTheJail(t *testing.T) {
	preserveJailVars(t)
	outerHome := os.Getenv("HOME")
	var seenHome string
	code := run(func() int {
		seenHome = os.Getenv("HOME")
		return 7
	}, canonicalTmp(t))
	if code != 7 {
		t.Fatalf("run returned %d, want the tests' 7", code)
	}
	if seenHome == "" || seenHome == outerHome {
		t.Fatalf("tests saw HOME %q, want a fresh jail home (outer %q)", seenHome, outerHome)
	}
	root := filepath.Dir(seenHome)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("jail root %s still exists after run (stat error %v)", root, err)
	}
}

// TestRunFailsWithoutRunningTestsWhenItCannotBuildTheJail pins the broken
// state: a base that cannot hold the jail is a non-zero exit, the tests never
// run.
func TestRunFailsWithoutRunningTestsWhenItCannotBuildTheJail(t *testing.T) {
	preserveJailVars(t)
	missing := filepath.Join(t.TempDir(), "no", "such", "base")
	ran := false
	code := run(func() int {
		ran = true
		return 0
	}, missing)
	if code == 0 {
		t.Fatal("run returned 0 for a base that cannot hold the jail")
	}
	if ran {
		t.Fatal("run executed the tests without a jail")
	}
}

func TestRefuseRealHome(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	tests := []struct {
		name      string
		jail, now string
		wantErr   bool
	}{
		{"same path", real, real, true},
		{"jail home is a symlink to the real one", link, real, true},
		{"unclean spelling of the real home", real + "/.", real, true},
		{"different directory", other, real, false},
		{"real home unknown", other, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseRealHome(tt.jail, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("refuseRealHome(%q, %q) error = %v, wantErr %v", tt.jail, tt.now, err, tt.wantErr)
			}
		})
	}
}
