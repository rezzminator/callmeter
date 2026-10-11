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
	"time"
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
