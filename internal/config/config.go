// Package config resolves runtime settings.
//
// Precedence is label > file > environment. This file covers the environment
// layer and the global defaults; per-container rules arrive with the selection
// engine.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Defaults chosen for a host with many containers and one dominant volume: disk
// throughput is the bottleneck, so concurrency stays low, and compression stays
// at the level where it costs little downtime.
const (
	DefaultBackupDir     = "/backups"
	DefaultZstdLevel     = 3
	DefaultConcurrency   = 2
	DefaultRetentionKeep = 3
	DefaultRetentionDays = 14
	DefaultInterval      = 6 * time.Hour
)

// Config holds the global settings.
type Config struct {
	// BackupDir is the destination URI. A bare path means a local directory.
	BackupDir string
	// HelperImage runs the short-lived helper containers. Empty means the
	// direct path must work, which it does when running on the engine host.
	HelperImage string
	ZstdLevel   int
	Concurrency int

	RetentionKeep int
	RetentionDays int

	Interval time.Duration

	// RulesFile holds selection rules and per-container overrides. Absent is
	// the normal case; labels alone are enough for most setups.
	RulesFile string

	Komodo KomodoSettings
}

// KomodoSettings configures Komodo as a source of registry credentials.
//
// It is not a selection source. Which containers backup-tower acts on is
// decided from labels and the rule file alone, both of which can be read
// without a network — an external service that has to be reachable before
// anything can be decided is an external service that can stop your backups.
type KomodoSettings struct {
	URL       string
	APIKey    string
	APISecret string
}

// Configured reports whether Komodo can be queried.
func (k KomodoSettings) Configured() bool {
	return k.URL != "" && k.APIKey != "" && k.APISecret != ""
}

// Partial reports whether Komodo was half-configured, which is almost always a
// mistake worth pointing out rather than silently ignoring.
func (k KomodoSettings) Partial() bool {
	any := k.URL != "" || k.APIKey != "" || k.APISecret != ""
	return any && !k.Configured()
}

// Missing names the Komodo settings that still need to be supplied.
func (k KomodoSettings) Missing() []string {
	var out []string
	for _, f := range []struct {
		env   string
		value string
	}{
		{"KOMODO_URL", k.URL},
		{"KOMODO_API_KEY", k.APIKey},
		{"KOMODO_API_SECRET", k.APISecret},
	} {
		if f.value == "" {
			out = append(out, f.env)
		}
	}
	return out
}

// FromEnv reads the environment, falling back to the defaults.
func FromEnv() (Config, error) {
	c := Config{
		BackupDir:     DefaultBackupDir,
		ZstdLevel:     DefaultZstdLevel,
		Concurrency:   DefaultConcurrency,
		RetentionKeep: DefaultRetentionKeep,
		RetentionDays: DefaultRetentionDays,
		Interval:      DefaultInterval,
	}

	if v := os.Getenv("TOWER_BACKUP_DIR"); v != "" {
		c.BackupDir = v
	}
	if v := os.Getenv("TOWER_HELPER_IMAGE"); v != "" {
		c.HelperImage = v
	}
	if v := os.Getenv("TOWER_RULES_FILE"); v != "" {
		c.RulesFile = v
	}

	c.Komodo = KomodoSettings{
		URL:       os.Getenv("KOMODO_URL"),
		APIKey:    os.Getenv("KOMODO_API_KEY"),
		APISecret: os.Getenv("KOMODO_API_SECRET"),
	}

	var err error
	if c.ZstdLevel, err = envInt("TOWER_ZSTD_LEVEL", c.ZstdLevel); err != nil {
		return c, err
	}
	if c.Concurrency, err = envInt("TOWER_CONCURRENCY", c.Concurrency); err != nil {
		return c, err
	}
	if c.RetentionKeep, err = envInt("TOWER_RETENTION_KEEP", c.RetentionKeep); err != nil {
		return c, err
	}
	if c.RetentionDays, err = envInt("TOWER_RETENTION_DAYS", c.RetentionDays); err != nil {
		return c, err
	}
	if c.Interval, err = envDuration("TOWER_INTERVAL", c.Interval); err != nil {
		return c, err
	}

	return c, c.Validate()
}

// Validate rejects settings that would misbehave later rather than at the point
// of configuration.
func (c Config) Validate() error {
	if c.BackupDir == "" {
		return fmt.Errorf("TOWER_BACKUP_DIR must not be empty")
	}
	if c.ZstdLevel < 1 || c.ZstdLevel > 19 {
		return fmt.Errorf("TOWER_ZSTD_LEVEL must be between 1 and 19, got %d", c.ZstdLevel)
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("TOWER_CONCURRENCY must be at least 1, got %d", c.Concurrency)
	}
	if c.RetentionKeep < 0 {
		return fmt.Errorf("TOWER_RETENTION_KEEP must not be negative, got %d", c.RetentionKeep)
	}
	if c.RetentionDays < 0 {
		return fmt.Errorf("TOWER_RETENTION_DAYS must not be negative, got %d", c.RetentionDays)
	}
	if c.RetentionKeep == 0 && c.RetentionDays == 0 {
		return fmt.Errorf("retention would delete every snapshot: set TOWER_RETENTION_KEEP or TOWER_RETENTION_DAYS")
	}
	if c.Interval <= 0 {
		return fmt.Errorf("TOWER_INTERVAL must be positive, got %s", c.Interval)
	}
	return nil
}

func envInt(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback, fmt.Errorf("%s must be a number, got %q", key, v)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback, fmt.Errorf("%s must be a duration such as 6h or 30m, got %q", key, v)
	}
	return d, nil
}
