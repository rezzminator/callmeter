// Package testjail holds the setup every test package needs before its first
// test runs: HOME, TMPDIR and CALLMETER_HOME moved into temp, the Claude Code
// session variables cleared. It is imported only by _test.go files, so it
// never reaches the binary, and it imports nothing of callmeter's own, so any
// package's tests can use it without an import cycle.
package testjail

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// clearedEnv are the variables a jail empties: none belongs to a test unless
// it sets one with t.Setenv.
var clearedEnv = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_SESSION_ID", "CLAUDE_EFFORT", "CALLMETER_BIN"}

// Run jails the process, runs the package's tests and removes the jail. It
// returns a non-zero code, running no test, when the jail cannot be built:
// an unjailed package fails loudly, it never writes to a live store.
//
// TMPDIR is /tmp with its symlinks resolved — short and canonical on both
// kernels (/private/tmp on macOS, /tmp on Linux) — so every t.TempDir() has
// one spelling and fits under a unix socket's path cap. HOME, CALLMETER_HOME
// and XDG_STATE_HOME are fresh directories under one jail root.
func Run(m *testing.M) int {
	base, err := canonicalBase()
	if err != nil {
		warnSetup("no canonical temp base: %v", err)
		return 1
	}
	return run(m.Run, base)
}

// canonicalBase is the symlink-resolved /tmp, or the symlink-resolved default
// temp dir where /tmp cannot be resolved.
func canonicalBase() (string, error) {
	base, err := filepath.EvalSymlinks("/tmp")
	if err == nil {
		return base, nil
	}
	fallback, fallbackErr := filepath.EvalSymlinks(os.TempDir())
	if fallbackErr != nil {
		return "", errors.Join(err, fallbackErr)
	}
	return fallback, nil
}

func run(tests func() int, base string) int {
	realHome, err := os.UserHomeDir()
	if err != nil {
		// With no real home in effect there is nothing a jail home could
		// collide with; say so rather than guess.
		warnSetup("real home unknown, jail cannot compare against it: %v", err)
		realHome = ""
	}
	// A `go` child (a test that builds the binary) derives its caches from
	// HOME when they are unset, and HOME moves below: pin them first.
	if code := pinGoDirs(); code != 0 {
		return code
	}
	root, err := os.MkdirTemp(base, "callmeter-jail-")
	if err != nil {
		warnSetup("no jail root under %s: %v", base, err)
		return 1
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
			warnSetup("remove jail root %s: %v", root, err)
		}
	}()
	home := filepath.Join(root, "home")
	jailed := map[string]string{
		"HOME":           home,
		"TMPDIR":         base,
		"CALLMETER_HOME": filepath.Join(root, "callmeter"),
		"XDG_STATE_HOME": filepath.Join(root, "state"),
	}
	for _, dir := range []string{home, jailed["CALLMETER_HOME"], jailed["XDG_STATE_HOME"]} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			warnSetup("create jail directory %s: %v", dir, err)
			return 1
		}
	}
	if err := refuseRealHome(home, realHome); err != nil {
		warnSetup("%v", err)
		return 1
	}
	for _, name := range clearedEnv {
		jailed[name] = ""
	}
	for name, value := range jailed {
		if err := os.Setenv(name, value); err != nil {
			warnSetup("set %s to %q: %v", name, value, err)
			return 1
		}
	}
	return tests()
}

// refuseRealHome is an error when the jail's HOME is the user's real home,
// compared through symlinks where both resolve.
func refuseRealHome(jailHome, realHome string) error {
	if realHome == "" {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(realHome); err == nil {
		realHome = resolved
	}
	if resolved, err := filepath.EvalSymlinks(jailHome); err == nil {
		jailHome = resolved
	}
	if filepath.Clean(jailHome) == filepath.Clean(realHome) {
		return fmt.Errorf("jail HOME %s is the user's real home; refusing to run", jailHome)
	}
	return nil
}

// warnSetup reports a testjail setup failure on stderr, the one door every
// message in this file writes through.
func warnSetup(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "testjail: "+format+"\n", args...)
}

// pinGoDirs fixes GOCACHE, GOPATH, GOMODCACHE and TEST_TELEMETRY_DIR at their
// values under the real home, before HOME moves, so a `go` child still
// writing at teardown never fills the jail (RemoveAll would fail with
// "directory not empty"). A missing user cache or home dir is reported and the
// variable left as it was; a value that cannot be set returns non-zero.
func pinGoDirs() int {
	set := func(name, value string) int {
		if err := os.Setenv(name, value); err != nil {
			warnSetup("set %s: %v", name, err)
			return 1
		}
		return 0
	}
	goCache := os.Getenv("GOCACHE")
	if goCache == "" {
		if cache, err := os.UserCacheDir(); err != nil {
			warnSetup("GOCACHE left unpinned — user cache dir: %v", err)
		} else {
			goCache = filepath.Join(cache, "go-build")
			if code := set("GOCACHE", goCache); code != 0 {
				return code
			}
		}
	}
	if os.Getenv("TEST_TELEMETRY_DIR") == "" {
		if goCache == "" {
			warnSetup("TEST_TELEMETRY_DIR left unpinned — GOCACHE is unavailable")
			return 1
		}
		telemetry, err := filepath.Abs(filepath.Join(goCache, "telemetry"))
		if err != nil {
			warnSetup("TEST_TELEMETRY_DIR left unpinned — resolve under GOCACHE: %v", err)
			return 1
		}
		if code := set("TEST_TELEMETRY_DIR", telemetry); code != 0 {
			return code
		}
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			warnSetup("GOPATH/GOMODCACHE left unpinned — user home dir: %v", err)
			return 0
		}
		gopath = filepath.Join(home, "go")
		if code := set("GOPATH", gopath); code != 0 {
			return code
		}
	}
	if os.Getenv("GOMODCACHE") == "" {
		if code := set("GOMODCACHE", filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")); code != 0 {
			return code
		}
	}
	return 0
}
