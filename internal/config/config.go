// Package config reads callmeter's one optional settings file,
// {CALLMETER_HOME}/config.json (paths.Config). No file means the defaults:
// callmeter keeps every row it ever wrote.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/paths"
)

// MaxRetentionDays bounds retention_days at 100 years, far inside what a
// time.Duration holds.
const MaxRetentionDays = 36500

// Config is the settings file. The zero value of every field is its default.
type Config struct {
	// RetentionDays is how many days of rows the store keeps: each report run
	// first prunes older rows, archiving them to archive.db. 0, and a missing
	// file, keep every row forever.
	RetentionDays int `json:"retention_days"`
}

// Retention is RetentionDays as a duration; 0 keeps every row forever.
func (c Config) Retention() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}

// Load reads the settings file at path. A file that does not exist is the zero
// Config. A file that cannot be read, is empty, is not one JSON object, names a
// field this version does not know (a misspelled key would otherwise keep or
// drop rows by silence) or holds a value out of range is an error naming path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("callmeter config %s: read: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, fmt.Errorf(`callmeter config %s: the file is empty; want a JSON object such as {"retention_days": 90}`, path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("callmeter config %s: %w", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("callmeter config %s: more than one JSON value in the file", path)
	}
	if cfg.RetentionDays < 0 || cfg.RetentionDays > MaxRetentionDays {
		return Config{}, fmt.Errorf("callmeter config %s: retention_days %d is outside 0 (keep forever) to %d",
			path, cfg.RetentionDays, MaxRetentionDays)
	}
	return cfg, nil
}

// Resolve is the settings a report runs under: the file at path (Load), then
// CALLMETER_RETENTION_DAYS over its retention_days. The variable is absent when
// unset or blank; its grammar is the file's, one JSON integer from 0 to
// MaxRetentionDays, save that a JSON null, which the file's key takes as 0, is
// a bad value here. A bad value is envErr, naming the variable, and the default
// (keep every row) stands, neither the file's value nor an error that stops the
// report. err is Load's: a file the report cannot trust stays an error whatever
// the variable says.
func Resolve(path string, getenv paths.Getenv) (cfg Config, envErr error, err error) {
	cfg, err = Load(path)
	if err != nil {
		return Config{}, nil, err
	}
	raw := strings.TrimSpace(getenv(paths.EnvRetentionDays))
	if raw == "" {
		return cfg, nil, nil
	}
	days, err := parseRetentionDays(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%s %q: %w", paths.EnvRetentionDays, raw, err), nil
	}
	cfg.RetentionDays = days
	return cfg, nil, nil
}

// parseRetentionDays reads raw as the file reads the key: one JSON integer in
// range. Decoding through a pointer tells a JSON null, which an int would take
// as 0, from a number.
func parseRetentionDays(raw string) (int, error) {
	var days *int
	if err := json.Unmarshal([]byte(raw), &days); err != nil || days == nil {
		return 0, errors.New("want an integer from 0 (keep forever) to " + strconv.Itoa(MaxRetentionDays))
	}
	if *days < 0 || *days > MaxRetentionDays {
		return 0, fmt.Errorf("outside 0 (keep forever) to %d", MaxRetentionDays)
	}
	return *days, nil
}
