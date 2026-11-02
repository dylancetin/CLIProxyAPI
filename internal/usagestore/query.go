package usagestore

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented is returned by Store operations that are not built yet.
var ErrNotImplemented = errors.New("usagestore: not implemented")

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
