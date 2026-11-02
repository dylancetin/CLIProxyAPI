package usagestore

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	// Registers the pure Go "sqlite" database/sql driver, keeping builds cgo-free.
	_ "modernc.org/sqlite"
)

// ErrNotImplemented is returned by Store operations that are not built yet.
var ErrNotImplemented = errors.New("usagestore: not implemented")

// Store is a SQLite-backed usage event store.
type Store struct{}

// Open opens or creates the database at opts.Path, runs migrations, and
// configures WAL and busy_timeout.
func Open(ctx context.Context, opts Options) (*Store, error) {
	return nil, ErrNotImplemented
}

// Close flushes pending events and closes the database.
func (s *Store) Close() error {
	return nil
}

// Plugin returns the batched usage writer. Its HandleUsage only enqueues.
func (s *Store) Plugin() usage.Plugin {
	return nil
}

// Flush drains the batch queue.
func (s *Store) Flush(ctx context.Context) error {
	return ErrNotImplemented
}

// SetRetention updates retention windows at runtime; 0 keeps rows forever.
func (s *Store) SetRetention(raw, hourly time.Duration) {}

// Sweep applies retention. It also runs periodically while the Store is open.
func (s *Store) Sweep(ctx context.Context) error {
	return ErrNotImplemented
}

// QuerySeries returns bucketed series for q, with CostUSD computed from prices.
func (s *Store) QuerySeries(ctx context.Context, q Query, prices Prices) (SeriesResult, error) {
	return SeriesResult{}, ErrNotImplemented
}

// QueryBreakdown returns totals per distinct value of dim within [from, to),
// sorted by total tokens descending.
func (s *Store) QueryBreakdown(ctx context.Context, from, to time.Time, dim Dimension, filters map[Dimension][]string, prices Prices) ([]BreakdownRow, error) {
	return nil, ErrNotImplemented
}

// QueryDimensionValues returns distinct values per dimension within [from, to)
// for filter pickers.
func (s *Store) QueryDimensionValues(ctx context.Context, from, to time.Time) (map[Dimension][]string, error) {
	return nil, ErrNotImplemented
}
