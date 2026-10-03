// Package paths resolves every filesystem location callmeter reads or writes:
// its own state home and the files under it, and the Claude Code directories
// a hook reports. Each resolver takes the environment as a Getenv, never the
// process environment directly, so a test hands in a map.
package paths

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// EnvHome names the variable that overrides the state home.
const EnvHome = "CALLMETER_HOME"

// Getenv reads one environment variable: os.Getenv in production, a map
// lookup in a test.
type Getenv func(string) string

// Home is callmeter's state directory: $CALLMETER_HOME when set, else
// $XDG_STATE_HOME/callmeter when XDG_STATE_HOME is absolute, else
// $HOME/.local/state/callmeter. Inside go test with $CALLMETER_HOME empty it
// is an error, never the real home: a test that reaches here without its jail
// must fail loudly rather than write to a live store.
func Home(getenv Getenv) (string, error) {
	return resolveHome(getenv, testing.Testing())
}

func resolveHome(getenv Getenv, underTest bool) (string, error) {
	if home := getenv(EnvHome); home != "" {
		return home, nil
	}
	if underTest {
		return "", fmt.Errorf("paths: %s is empty under go test; refusing to resolve the real state home", EnvHome)
	}
	if state := getenv("XDG_STATE_HOME"); filepath.IsAbs(state) {
		return filepath.Join(state, "callmeter"), nil
	}
	user, err := userHome(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".local", "state", "callmeter"), nil
}

// Store is the database file under home.
func Store(home string) string { return filepath.Join(home, "callmeter.db") }

// Log is the JSON-lines log file under home.
func Log(home string) string { return filepath.Join(home, "callmeter.log") }

// Missed is the log the wrapper appends to when it could not run the binary.
func Missed(home string) string { return filepath.Join(home, "missed.log") }

// BinCache is the cached binary of one version under home.
func BinCache(home, version string) string {
	return filepath.Join(home, "bin", version, "callmeter")
}

// SeatDir is the seat a hook ran from: $CLAUDE_CONFIG_DIR made absolute and
// clean, symlinks unresolved; unset or empty, $HOME/.claude.
func SeatDir(getenv Getenv) (string, error) {
	seat := getenv("CLAUDE_CONFIG_DIR")
	if seat == "" {
		return ConfigDir(getenv)
	}
	abs, err := filepath.Abs(seat)
	if err != nil {
		return "", fmt.Errorf("paths: make CLAUDE_CONFIG_DIR %q absolute: %w", seat, err)
	}
	return abs, nil
}

// ConfigDir is the user's own Claude Code directory, $HOME/.claude.
func ConfigDir(getenv Getenv) (string, error) {
	user, err := userHome(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".claude"), nil
}

func userHome(getenv Getenv) (string, error) {
	user := getenv("HOME")
	if user == "" {
		return "", errors.New("paths: HOME is empty")
	}
	return user, nil
}
