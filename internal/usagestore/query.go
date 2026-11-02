package usagestore

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultSeriesLimit = 20
	// maxSeriesBuckets bounds dense series memory; a year of hours fits.
	maxSeriesBuckets = 10_000
	otherValue       = "other"
)

// ErrInvalidQuery marks a query rejected before it reaches SQLite.
var ErrInvalidQuery = errors.New("usagestore: invalid query")

// span is a bucket-aligned [start, end) range in unix seconds.
type span struct {
	start, end, step int64
}

func (sp span) buckets() int64 {
	return (sp.end - sp.start) / sp.step
}

// alignSpan widens [from, to) to whole buckets, because usage_hourly cannot
// answer sub-hour boundaries.
func alignSpan(from, to time.Time, step time.Duration) (span, error) {
	if !to.After(from) {
		return span{}, fmt.Errorf("%w: to must be after from", ErrInvalidQuery)
	}
	start := from.Truncate(step)
	end := to.Truncate(step)
	if end.Before(to) {
		end = end.Add(step)
	}
	return span{start: start.Unix(), end: end.Unix(), step: int64(step / time.Second)}, nil
}

func bucketStep(b Bucket) (time.Duration, error) {
	switch b {
	case BucketHour, "":
		return time.Hour, nil
	case BucketDay:
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%w: unknown bucket %q", ErrInvalidQuery, b)
	}
}

// QuerySeries returns q's series with one point per bucket overlapping
// [From, To), using BucketHour when q.Bucket is empty and pricing CostUSD from prices.
func (s *Store) QuerySeries(ctx context.Context, q Query, prices Prices) (SeriesResult, error) {
	step, err := bucketStep(q.Bucket)
	if err != nil {
		return SeriesResult{}, err
	}
	sp, err := alignSpan(q.From, q.To, step)
	if err != nil {
		return SeriesResult{}, err
	}
	if n := sp.buckets(); n > maxSeriesBuckets {
		return SeriesResult{}, fmt.Errorf("%w: %d buckets exceeds the limit of %d", ErrInvalidQuery, n, maxSeriesBuckets)
	}
	for i, dim := range q.GroupBy {
		if slices.Contains(q.GroupBy[:i], dim) {
			return SeriesResult{}, fmt.Errorf("%w: duplicate group dimension %q", ErrInvalidQuery, dim)
		}
	}

	buckets := make([]time.Time, sp.buckets())
	for i := range buckets {
		buckets[i] = time.Unix(sp.start+int64(i)*sp.step, 0).UTC()
	}
	newPoints := func() []Point {
		points := make([]Point, len(buckets))
		for i := range points {
			points[i].BucketStart = buckets[i]
		}
		return points
	}

	byID := map[string]*Series{}
	if len(q.GroupBy) == 0 {
		byID[""] = &Series{Key: SeriesKey{}, Points: newPoints()}
	}
	err = s.aggregate(ctx, aggQuery{span: sp, bucketed: true, dims: q.GroupBy, filters: q.Filters}, func(r aggRow) {
		id := strings.Join(r.values, "\x00")
		series, ok := byID[id]
		if !ok {
			key := make(SeriesKey, len(q.GroupBy))
			for i, dim := range q.GroupBy {
				key[dim] = r.values[i]
			}
			series = &Series{Key: key, Hints: r.hints, Points: newPoints()}
			byID[id] = series
		}
		r.point.CostUSD = prices.Cost(r.model, r.point.Tokens)
		series.Points[(r.bucket-sp.start)/sp.step].Add(r.point)
	})
	if err != nil {
		return SeriesResult{}, err
	}

	type entry struct {
		id     string
		series *Series
	}
	entries := make([]entry, 0, len(byID))
	for id, series := range byID {
		for _, p := range series.Points {
			series.Total.Add(p)
		}
		entries = append(entries, entry{id: id, series: series})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if c := comparePoints(a.series.Total, b.series.Total); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})

	limit := q.Limit
	if limit <= 0 {
		limit = defaultSeriesLimit
	}
	result := SeriesResult{Buckets: buckets, Series: make([]Series, 0, min(len(entries), limit+1))}
	for i, e := range entries {
		if i == limit {
			break
		}
		result.Series = append(result.Series, *e.series)
	}
	if len(entries) > limit {
		other := Series{Key: make(SeriesKey, len(q.GroupBy)), Points: newPoints()}
		for _, dim := range q.GroupBy {
			other.Key[dim] = otherValue
		}
		for _, e := range entries[limit:] {
			for i, p := range e.series.Points {
				other.Points[i].Add(p)
			}
			other.Total.Add(e.series.Total)
		}
		result.Series = append(result.Series, other)
	}
	for _, series := range result.Series {
		result.Total.Add(series.Total)
	}
	return result, nil
}

// QueryBreakdown returns totals per distinct value of dim within [from, to),
// sorted by total tokens descending.
func (s *Store) QueryBreakdown(ctx context.Context, from, to time.Time, dim Dimension, filters map[Dimension][]string, prices Prices) ([]BreakdownRow, error) {
	sp, err := alignSpan(from, to, time.Hour)
	if err != nil {
		return nil, err
	}
	byValue := map[string]*BreakdownRow{}
	err = s.aggregate(ctx, aggQuery{span: sp, dims: []Dimension{dim}, filters: filters}, func(r aggRow) {
		row, ok := byValue[r.values[0]]
		if !ok {
			row = &BreakdownRow{Value: r.values[0], Hint: r.hints[dim]}
			byValue[r.values[0]] = row
		}
		r.point.CostUSD = prices.Cost(r.model, r.point.Tokens)
		row.Point.Add(r.point)
	})
	if err != nil {
		return nil, err
	}
	rows := make([]BreakdownRow, 0, len(byValue))
	for _, row := range byValue {
		rows = append(rows, *row)
	}
	slices.SortFunc(rows, func(a, b BreakdownRow) int {
		if c := comparePoints(a.Point, b.Point); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	return rows, nil
}

// QueryDimensionValues returns each dimension's sorted distinct values within
// [from, to), with key hashes for DimAPIKey.
func (s *Store) QueryDimensionValues(ctx context.Context, from, to time.Time) (map[Dimension][]string, error) {
	sp, err := alignSpan(from, to, time.Hour)
	if err != nil {
		return nil, err
	}
	out := make(map[Dimension][]string, len(dimensionColumns))
	for dim, expr := range dimensionColumns {
		values, err := s.queryStrings(ctx,
			"SELECT DISTINCT "+expr+" FROM usage_hourly WHERE hour_start >= ? AND hour_start < ? ORDER BY 1",
			sp.start, sp.end)
		if err != nil {
			return nil, fmt.Errorf("usagestore: query %s values: %w", dim, err)
		}
		out[dim] = values
	}
	return out, nil
}

func (s *Store) queryStrings(ctx context.Context, query string, args ...any) (_ []string, err error) {
	if s.isClosed() {
		return nil, ErrClosed
	}
	defer func() {
		// A concurrent Close fails the read with database/sql's unexported error.
		if err != nil && s.isClosed() {
			err = ErrClosed
		}
	}()
	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	values := []string{}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

// aggQuery selects usage_hourly sums grouped by bucket (when bucketed), dims,
// and model. Model is always grouped so cost is priced per model.
type aggQuery struct {
	span     span
	bucketed bool
	dims     []Dimension
	filters  map[Dimension][]string
}

type aggRow struct {
	bucket int64
	values []string
	hints  map[Dimension]string
	model  string
	point  Point
}

const aggregateColumns = `SUM(requests), SUM(failures), SUM(input_uncached), SUM(input_cache_read),
    SUM(input_cache_write), SUM(output_reasoning), SUM(output_other), SUM(unclassified),
    SUM(total_tokens), SUM(latency_ms_sum), MAX(latency_ms_max), SUM(ttft_ms_sum),
    MAX(ttft_ms_max), SUM(ttft_count)`

func (s *Store) aggregate(ctx context.Context, q aggQuery, fn func(aggRow)) (err error) {
	if s.isClosed() {
		return ErrClosed
	}
	defer func() {
		// A concurrent Close fails the read with database/sql's unexported error.
		if err != nil && s.isClosed() {
			err = ErrClosed
		}
	}()
	var groupExprs, hintExprs []string
	var hintDims []Dimension
	if q.bucketed {
		groupExprs = append(groupExprs, fmt.Sprintf("hour_start / %d * %d", q.span.step, q.span.step))
	}
	for _, dim := range q.dims {
		expr, ok := dimensionColumns[dim]
		if !ok {
			return fmt.Errorf("%w: unknown dimension %q", ErrInvalidQuery, dim)
		}
		groupExprs = append(groupExprs, expr)
		if hint, ok := dimensionHintColumns[dim]; ok {
			hintDims = append(hintDims, dim)
			hintExprs = append(hintExprs, "MAX("+hint+")")
		}
	}
	groupExprs = append(groupExprs, "model")
	where, args, err := filterClause(q.filters)
	if err != nil {
		return err
	}

	query := "SELECT " + strings.Join(slices.Concat(groupExprs, hintExprs), ", ") + ", " + aggregateColumns +
		" FROM usage_hourly WHERE hour_start >= ? AND hour_start < ?" + where +
		" GROUP BY " + strings.Join(groupExprs, ", ")
	rows, err := s.readDB.QueryContext(ctx, query, append([]any{q.span.start, q.span.end}, args...)...)
	if err != nil {
		return fmt.Errorf("usagestore: query usage: %w", err)
	}
	defer closeRows(rows)

	for rows.Next() {
		r := aggRow{values: make([]string, len(q.dims))}
		hints := make([]string, len(hintDims))
		dest := make([]any, 0, len(groupExprs)+len(hintDims)+14)
		if q.bucketed {
			dest = append(dest, &r.bucket)
		}
		for i := range r.values {
			dest = append(dest, &r.values[i])
		}
		dest = append(dest, &r.model)
		for i := range hints {
			dest = append(dest, &hints[i])
		}
		p := &r.point
		dest = append(dest, &p.Requests, &p.Failures, &p.Tokens.InputUncached, &p.Tokens.InputCacheRead,
			&p.Tokens.InputCacheWrite, &p.Tokens.OutputReasoning, &p.Tokens.OutputOther, &p.Tokens.Unclassified,
			&p.Tokens.Total, &p.LatencyMsSum, &p.LatencyMsMax, &p.TTFTMsSum, &p.TTFTMsMax, &p.TTFTCount)
		if err = rows.Scan(dest...); err != nil {
			return fmt.Errorf("usagestore: scan usage: %w", err)
		}
		if len(hintDims) > 0 {
			r.hints = make(map[Dimension]string, len(hintDims))
			for i, dim := range hintDims {
				r.hints[dim] = hints[i]
			}
		}
		fn(r)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("usagestore: read usage: %w", err)
	}
	return nil
}

// filterClause renders filters as parameterized IN lists, OR within a
// dimension and AND across dimensions. Only dimensionColumns expressions reach the SQL.
func filterClause(filters map[Dimension][]string) (string, []any, error) {
	dims := make([]Dimension, 0, len(filters))
	for dim, values := range filters {
		if len(values) > 0 {
			dims = append(dims, dim)
		}
	}
	slices.Sort(dims)
	var b strings.Builder
	var args []any
	for _, dim := range dims {
		expr, ok := dimensionColumns[dim]
		if !ok {
			return "", nil, fmt.Errorf("%w: unknown filter dimension %q", ErrInvalidQuery, dim)
		}
		values := filters[dim]
		b.WriteString(" AND ")
		b.WriteString(expr)
		b.WriteString(" IN (?")
		b.WriteString(strings.Repeat(", ?", len(values)-1))
		b.WriteString(")")
		for _, v := range values {
			args = append(args, v)
		}
	}
	return b.String(), args, nil
}

// comparePoints orders by total tokens, then requests, both descending.
func comparePoints(a, b Point) int {
	if c := cmp.Compare(b.Tokens.Total, a.Tokens.Total); c != 0 {
		return c
	}
	return cmp.Compare(b.Requests, a.Requests)
}

// Add sums src into p and keeps the larger value of each max field.
func (p *Point) Add(src Point) {
	p.Requests += src.Requests
	p.Failures += src.Failures
	p.Tokens.InputUncached += src.Tokens.InputUncached
	p.Tokens.InputCacheRead += src.Tokens.InputCacheRead
	p.Tokens.InputCacheWrite += src.Tokens.InputCacheWrite
	p.Tokens.OutputReasoning += src.Tokens.OutputReasoning
	p.Tokens.OutputOther += src.Tokens.OutputOther
	p.Tokens.Unclassified += src.Tokens.Unclassified
	p.Tokens.Total += src.Tokens.Total
	p.LatencyMsSum += src.LatencyMsSum
	p.LatencyMsMax = max(p.LatencyMsMax, src.LatencyMsMax)
	p.TTFTMsSum += src.TTFTMsSum
	p.TTFTMsMax = max(p.TTFTMsMax, src.TTFTMsMax)
	p.TTFTCount += src.TTFTCount
	p.CostUSD += src.CostUSD
}

func closeRows(rows *sql.Rows) {
	if err := rows.Close(); err != nil {
		log.WithError(err).Error("usagestore: close rows")
	}
}
