package usagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	log "github.com/sirupsen/logrus"
)

const schemaVersion = 1

const schemaSQL = `
CREATE TABLE IF NOT EXISTS usage_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS usage_events (
    id INTEGER PRIMARY KEY,
    requested_at_ms INTEGER NOT NULL,
    hour_start INTEGER NOT NULL,          -- unix seconds, floor to UTC hour
    trace_id TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    provider TEXT NOT NULL DEFAULT '',
    executor_type TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    requested_model TEXT NOT NULL DEFAULT '',
    account TEXT NOT NULL DEFAULT '',
    auth_type TEXT NOT NULL DEFAULT '',
    api_key_hash TEXT NOT NULL DEFAULT '',
    api_key_hint TEXT NOT NULL DEFAULT '',
    access_provider TEXT NOT NULL DEFAULT '',
    reasoning_effort TEXT NOT NULL DEFAULT '',
    service_tier TEXT NOT NULL DEFAULT '',
    stream INTEGER NOT NULL DEFAULT 0,
    failed INTEGER NOT NULL DEFAULT 0,
    status_code INTEGER NOT NULL DEFAULT 0,
    failure_class TEXT NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    ttft_ms INTEGER NOT NULL DEFAULT 0,   -- 0 = unknown
    input_uncached INTEGER NOT NULL DEFAULT 0,
    input_cache_read INTEGER NOT NULL DEFAULT 0,
    input_cache_write INTEGER NOT NULL DEFAULT 0,
    output_reasoning INTEGER NOT NULL DEFAULT 0,
    output_other INTEGER NOT NULL DEFAULT 0,
    unclassified INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    quality TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS usage_events_hour ON usage_events(hour_start);
CREATE INDEX IF NOT EXISTS usage_events_trace ON usage_events(trace_id);

CREATE TABLE IF NOT EXISTS usage_hourly (
    hour_start INTEGER NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    requested_model TEXT NOT NULL,
    account TEXT NOT NULL,
    api_key_hash TEXT NOT NULL,
    api_key_hint TEXT NOT NULL,
    reasoning_effort TEXT NOT NULL,
    service_tier TEXT NOT NULL,
    stream INTEGER NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    failures INTEGER NOT NULL DEFAULT 0,
    input_uncached INTEGER NOT NULL DEFAULT 0,
    input_cache_read INTEGER NOT NULL DEFAULT 0,
    input_cache_write INTEGER NOT NULL DEFAULT 0,
    output_reasoning INTEGER NOT NULL DEFAULT 0,
    output_other INTEGER NOT NULL DEFAULT 0,
    unclassified INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms_sum INTEGER NOT NULL DEFAULT 0,
    latency_ms_max INTEGER NOT NULL DEFAULT 0,
    ttft_ms_sum INTEGER NOT NULL DEFAULT 0,
    ttft_ms_max INTEGER NOT NULL DEFAULT 0,
    ttft_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hour_start, provider, model, requested_model, account, api_key_hash, reasoning_effort, service_tier, stream)
);
`

// migrate creates the schema and records its version. It refuses databases
// written by a newer schema so an older binary never misreads them.
func migrate(ctx context.Context, db *sql.DB) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usagestore: begin migration: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			log.WithError(errRollback).Error("usagestore: rollback migration")
		}
	}()

	if _, err = tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("usagestore: create schema: %w", err)
	}
	var version string
	err = tx.QueryRowContext(ctx, `SELECT value FROM usage_meta WHERE key = 'schema_version'`).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_meta (key, value) VALUES ('schema_version', ?)`, strconv.Itoa(schemaVersion)); err != nil {
			return fmt.Errorf("usagestore: record schema version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("usagestore: read schema version: %w", err)
	case version != strconv.Itoa(schemaVersion):
		err = fmt.Errorf("usagestore: unsupported schema version %q, want %d", version, schemaVersion)
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usagestore: commit migration: %w", err)
	}
	return nil
}
