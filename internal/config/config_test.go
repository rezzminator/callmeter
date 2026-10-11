package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileKeepsEveryRow(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("Load of a missing file = %v, want the defaults", err)
	}
	if cfg.RetentionDays != 0 || cfg.Retention() != 0 {
		t.Errorf("default config = %+v (retention %v), want retention 0: keep every row forever", cfg, cfg.Retention())
	}
}

func TestLoadReadsRetentionDays(t *testing.T) {
	for _, c := range []struct {
		json string
		days int
	}{
		{`{}`, 0},
		{`{"retention_days": 0}`, 0},
		{`{"retention_days": 90}`, 90},
		{"  {\n  \"retention_days\": 1\n}\n", 1},
		{`{"retention_days": 36500}`, MaxRetentionDays},
	} {
		cfg, err := Load(writeConfig(t, c.json))
		if err != nil {
			t.Errorf("Load(%q) = %v, want retention_days %d", c.json, err, c.days)
			continue
		}
		if cfg.RetentionDays != c.days {
			t.Errorf("Load(%q).RetentionDays = %d, want %d", c.json, cfg.RetentionDays, c.days)
		}
		if want := time.Duration(c.days) * 24 * time.Hour; cfg.Retention() != want {
			t.Errorf("Load(%q).Retention() = %v, want %v", c.json, cfg.Retention(), want)
		}
	}
}

// TestLoadRefusesAFileItCannotTrust: every way the file can be wrong is an error
// naming the file, never the defaults: a typo must not read as "keep forever".
func TestLoadRefusesAFileItCannotTrust(t *testing.T) {
	for name, c := range map[string]struct{ json, want string }{
		"empty":            {"", "is empty"},
		"blank":            {" \n\t", "is empty"},
		"not json":         {"retention_days = 90", "invalid character"},
		"an array":         {"[90]", "cannot unmarshal array"},
		"a string":         {`{"retention_days": "90"}`, "cannot unmarshal string"},
		"a fraction":       {`{"retention_days": 30.5}`, "cannot unmarshal number 30.5"},
		"unknown key":      {`{"retention_day": 90}`, `unknown field "retention_day"`},
		"negative":         {`{"retention_days": -1}`, "outside 0 (keep forever) to 36500"},
		"too long":         {`{"retention_days": 36501}`, "outside 0 (keep forever) to 36500"},
		"trailing value":   {`{"retention_days": 90} {"retention_days": 1}`, "more than one JSON value"},
		"trailing garbage": {`{"retention_days": 90} x`, "more than one JSON value"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, c.json)
			cfg, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) = %+v, want an error", c.json, cfg)
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Load(%q) error = %q, want it to name %s and say %q", c.json, err, path, c.want)
			}
		})
	}
}

func TestLoadUnreadablePathIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("Load of a directory = %v, want an error naming %s", err, dir)
	}
}
