package config

import "github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/pricing"

const (
	// DefaultUsageTrackerRawRetentionDays bounds per-attempt rows by default.
	DefaultUsageTrackerRawRetentionDays = 30
	// DefaultUsageTrackerHourlyRetentionDays bounds hourly rollups by default.
	DefaultUsageTrackerHourlyRetentionDays = 400
	// DefaultUsageTrackerFileName is the database file name used when SQLitePath is empty.
	DefaultUsageTrackerFileName = "usage.sqlite"
)

// UsageTrackerConfig controls the SQLite usage tracker.
type UsageTrackerConfig struct {
	// Enabled records every upstream attempt into the SQLite store.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// SQLitePath is the database file; empty uses <auth-dir>/usage.sqlite, or
	// usage/usage.sqlite beside the local mirror of a remote token store.
	SQLitePath string `yaml:"sqlite-path" json:"sqlite-path"`
	// RawRetentionDays bounds per-attempt rows; 0 keeps them forever.
	RawRetentionDays int `yaml:"raw-retention-days" json:"raw-retention-days"`
	// HourlyRetentionDays bounds hourly rollups; 0 keeps them forever.
	HourlyRetentionDays int `yaml:"hourly-retention-days" json:"hourly-retention-days"`
	// Prices maps a model id or "default" to USD per 1,000,000 tokens.
	Prices map[string]pricing.ModelPrice `yaml:"prices" json:"prices"`
}

// DefaultUsageTrackerConfig returns the tracker defaults applied before unmarshal.
func DefaultUsageTrackerConfig() UsageTrackerConfig {
	return UsageTrackerConfig{
		Enabled:             true,
		RawRetentionDays:    DefaultUsageTrackerRawRetentionDays,
		HourlyRetentionDays: DefaultUsageTrackerHourlyRetentionDays,
	}
}

// normalizeUsageTracker clamps negative retention to 0 (keep forever).
func (cfg *Config) normalizeUsageTracker() {
	if cfg.UsageTracker.RawRetentionDays < 0 {
		cfg.UsageTracker.RawRetentionDays = 0
	}
	if cfg.UsageTracker.HourlyRetentionDays < 0 {
		cfg.UsageTracker.HourlyRetentionDays = 0
	}
}
