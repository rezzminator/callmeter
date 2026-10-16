package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rezzminator/callmeter/internal/paths"
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

func envOf(value string, set bool) paths.Getenv {
	return func(name string) string {
		if set && name == paths.EnvRetentionDays {
			return value
		}
		return ""
	}
}

// TestResolveRetentionDaysPrecedence: CALLMETER_RETENTION_DAYS beats config.json
// beats the default (keep every row); unset or blank is absent; a bad value is
// reported by its variable name and the default, never the file's value, is used.
func TestResolveRetentionDaysPrecedence(t *testing.T) {
	for name, c := range map[string]struct {
		file    string // config.json content; "" writes no file
		env     string
		envSet  bool
		days    int
		badWant string // substring of the reported env error; "" means none
	}{
		"env overrides the file":           {`{"retention_days": 90}`, "30", true, 30, ""},
		"env 0 overrides the file":         {`{"retention_days": 90}`, "0", true, 0, ""},
		"env without a file":               {"", "7", true, 7, ""},
		"env padded with whitespace":       {"", " \t12\n", true, 12, ""},
		"env at the upper bound":           {"", "36500", true, MaxRetentionDays, ""},
		"unset falls to the file":          {`{"retention_days": 90}`, "", false, 90, ""},
		"empty falls to the file":          {`{"retention_days": 90}`, "", true, 90, ""},
		"blank falls to the file":          {`{"retention_days": 90}`, " \t ", true, 90, ""},
		"unset and no file is the default": {"", "", false, 0, ""},
		"a word":                           {`{"retention_days": 90}`, "forever", true, 0, `"forever"`},
		"a fraction":                       {"", "30.5", true, 0, `"30.5"`},
		"an exponent":                      {"", "1e2", true, 0, `"1e2"`},
		"a quoted number":                  {"", `"30"`, true, 0, `\"30\"`},
		"null":                             {`{"retention_days": 90}`, "null", true, 0, `"null"`},
		"a leading zero":                   {"", "030", true, 0, `"030"`},
		"negative":                         {`{"retention_days": 90}`, "-1", true, 0, "outside 0 (keep forever) to 36500"},
		"past the upper bound":             {`{"retention_days": 90}`, "36501", true, 0, "outside 0 (keep forever) to 36500"},
		"too large for any integer":        {"", "99999999999999999999", true, 0, `"99999999999999999999"`},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if c.file != "" {
				path = writeConfig(t, c.file)
			}
			cfg, envErr, err := Resolve(path, envOf(c.env, c.envSet))
			if err != nil {
				t.Fatalf("Resolve = %v, want no file error", err)
			}
			if cfg.RetentionDays != c.days {
				t.Errorf("Resolve(file %q, env %q).RetentionDays = %d, want %d", c.file, c.env, cfg.RetentionDays, c.days)
			}
			switch {
			case c.badWant == "" && envErr != nil:
				t.Errorf("Resolve reported %v for env %q, want none", envErr, c.env)
			case c.badWant != "" && (envErr == nil ||
				!strings.Contains(envErr.Error(), paths.EnvRetentionDays) || !strings.Contains(envErr.Error(), c.badWant)):
				t.Errorf("Resolve env error = %v, want it to name %s and say %q", envErr, paths.EnvRetentionDays, c.badWant)
			}
		})
	}
}

// TestResolveKeepsAnUntrustedFileAnError: the env layer never hides a config.json
// the report cannot trust, a valid variable included.
func TestResolveKeepsAnUntrustedFileAnError(t *testing.T) {
	path := writeConfig(t, `{"retention_day": 90}`)
	if _, _, err := Resolve(path, envOf("30", true)); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("Resolve over a misspelled file = %v, want an error naming %s", err, path)
	}
}
