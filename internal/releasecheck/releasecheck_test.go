package releasecheck

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const version = "9.9.9"

// freshSums is what the stub build-release.sh writes: the sums a build of the
// tree at hand would produce.
const freshSums = "aaaa  callmeter_9.9.9_linux_amd64\n"

// staleSums is a committed SHA256SUMS that a later commit made differ from a
// fresh build, as on develop between two releases.
const staleSums = "bbbb  callmeter_9.9.9_linux_amd64\n"

// stubBuild stands in for build-release.sh: `--out DIR` gets a SHA256SUMS
// holding freshSums.
const stubBuild = `#!/usr/bin/env bash
set -eu
[ "$1" = --out ] || exit 2
printf '%s' "$FRESH_SUMS" > "$2/SHA256SUMS"
`

// tree writes a temp tree whose version places all say `version`, with the
// CHANGELOG heading carrying a date, and returns its root. committed is the
// plugin's SHA256SUMS.
func tree(t *testing.T, committed, changelogHeading string) string {
	t.Helper()
	root := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "release-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"scripts/release-check.sh":                     string(src),
		"scripts/build-release.sh":                     stubBuild,
		"plugins/callmeter/.claude-plugin/plugin.json": `{"name":"callmeter","version":"` + version + `"}`,
		"README.md":                            "[![Version](https://img.shields.io/badge/version-" + version + "-blue)](./CHANGELOG.md)\n",
		"CHANGELOG.md":                         "# Changelog\n\n" + changelogHeading + "\n",
		"plugins/callmeter/libexec/SHA256SUMS": committed,
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// run runs the tree's release-check.sh and returns its exit code and stderr
// and stdout together.
func run(t *testing.T, root string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(root, "scripts", "release-check.sh"), args...)
	cmd.Env = append(os.Environ(), "FRESH_SUMS="+freshSums)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), out.String()
	default:
		t.Fatalf("run release-check.sh: %v", err)
		return -1, ""
	}
}

// TestReleaseCheckComparesSumsOnlyForARelease: the committed SHA256SUMS is
// refreshed in the release commit alone ("release: callmeter X"), so between
// releases it differs from a fresh build and a plain check passes; --release,
// the tagged commit's check, fails on a stale file and passes on a fresh one.
func TestReleaseCheckComparesSumsOnlyForARelease(t *testing.T) {
	dated := "## [" + version + "] — 2026-01-02"
	undated := "## [" + version + "]"
	tests := []struct {
		name      string
		committed string
		heading   string
		args      []string
		wantCode  int
		wantOut   string // a substring of the output
	}{
		{"plain check on develop between releases passes with stale sums", staleSums, undated, nil, 0, "ok: " + version},
		{"plain check says the sums were not compared", staleSums, undated, nil, 0, "SHA256SUMS not compared"},
		{"release check fails on stale sums", staleSums, dated, []string{"--release"}, 1, "SHA256SUMS is stale"},
		{"release check passes on fresh sums", freshSums, dated, []string{"--release"}, 0, "ok: " + version + " (release)"},
		{"plain check still fails on a missing changelog heading", staleSums, "## [0.0.1]", nil, 1, "has no '## [" + version + "]' heading"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := tree(t, tc.committed, tc.heading)
			code, out := run(t, root, tc.args...)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d; output:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output lacks %q:\n%s", tc.wantOut, out)
			}
		})
	}
}

// TestReleaseCheckReadsAPrereleaseBadge: shields.io writes a literal dash in a
// badge as `--`, so a prerelease version such as 0.4.5-dev.1 sits in the
// README as `badge/version-0.4.5--dev.1-blue`; the check decodes it and agrees.
func TestReleaseCheckReadsAPrereleaseBadge(t *testing.T) {
	const pre = "9.9.10-dev.1"
	tests := []struct {
		name     string
		badge    string
		wantCode int
		wantOut  string
	}{
		{"an escaped prerelease badge agrees", "9.9.10--dev.1", 0, "ok: " + pre},
		{"a badge naming only the release disagrees", "9.9.10", 1, "README.md badge says '9.9.10'"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := tree(t, staleSums, "## ["+pre+"]")
			files := map[string]string{
				"plugins/callmeter/.claude-plugin/plugin.json": `{"name":"callmeter","version":"` + pre + `"}`,
				"README.md": "[![Version](https://img.shields.io/badge/version-" + tc.badge + "-blue)](./CHANGELOG.md)\n",
			}
			for rel, body := range files {
				if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			code, out := run(t, root)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d; output:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output lacks %q:\n%s", tc.wantOut, out)
			}
		})
	}
}
