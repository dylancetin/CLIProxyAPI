package usagestore_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const million = 1_000_000

var testPrices = usagestore.Prices{
	"model-a": {Input: 1, Output: 2},
	"model-b": {Input: 10, Output: 20},
}

func seededStore(t *testing.T, attempts ...attempt) *usagestore.Store {
	t.Helper()
	s, _ := openStore(t, usagestore.Options{})
	publish(s, attempts...)
	flush(t, s)
	return s
}

func querySeries(t *testing.T, s *usagestore.Store, q usagestore.Query, prices usagestore.Prices) usagestore.SeriesResult {
	t.Helper()
	res, err := s.QuerySeries(context.Background(), q, prices)
	if err != nil {
		t.Fatalf("QuerySeries() error = %v", err)
	}
	return res
}

// seriesTokens renders each series as "key=total" in result order.
func seriesTokens(res usagestore.SeriesResult, dims ...usagestore.Dimension) []string {
	out := make([]string, 0, len(res.Series))
	for _, s := range res.Series {
		key := ""
		for _, d := range dims {
			key += "/" + s.Key[d]
		}
		out = append(out, fmt.Sprintf("%s=%d", key, s.Total.Tokens.Total))
	}
	return out
}

func assertStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func assertFloat(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestQuerySeriesZeroFillsHourBuckets(t *testing.T) {
	s := seededStore(t,
		attempt{at: t0.Add(10 * time.Minute), provider: "claude", model: "model-a", in: 10, latency: 100 * time.Millisecond, ttft: 40 * time.Millisecond},
		attempt{at: t0.Add(3*time.Hour + 30*time.Minute), provider: "claude", model: "model-a", in: 20, out: 5, latency: 300 * time.Millisecond, failed: true, status: 500},
	)
	for _, tt := range []struct {
		name     string
		from, to time.Time
	}{
		{"aligned", t0, t0.Add(6 * time.Hour)},
		{"unaligned range widens to whole hours", t0.Add(30 * time.Minute), t0.Add(5*time.Hour + time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := querySeries(t, s, usagestore.Query{From: tt.from, To: tt.to, Bucket: usagestore.BucketHour}, nil)
			if len(res.Buckets) != 6 || len(res.Series) != 1 {
				t.Fatalf("buckets, series = %d, %d; want 6, 1", len(res.Buckets), len(res.Series))
			}
			series := res.Series[0]
			if len(series.Key) != 0 {
				t.Fatalf("total series key = %v, want empty", series.Key)
			}
			wantTokens := []int64{10, 0, 0, 25, 0, 0}
			for i, p := range series.Points {
				bucket := t0.Add(time.Duration(i) * time.Hour)
				if !res.Buckets[i].Equal(bucket) || !p.BucketStart.Equal(bucket) || p.BucketStart.Location() != time.UTC {
					t.Fatalf("bucket %d = %v / %v, want %v UTC", i, res.Buckets[i], p.BucketStart, bucket)
				}
				if p.Tokens.Total != wantTokens[i] {
					t.Fatalf("point %d tokens = %d, want %d", i, p.Tokens.Total, wantTokens[i])
				}
			}
			want := usagestore.Point{
				Requests: 2, Failures: 1,
				Tokens:       usagestore.Tokens{InputUncached: 30, OutputOther: 5, Total: 35},
				LatencyMsSum: 400, LatencyMsMax: 300, TTFTMsSum: 40, TTFTMsMax: 40, TTFTCount: 1,
			}
			if series.Total != want || res.Total != want {
				t.Fatalf("totals = %+v / %+v, want %+v", series.Total, res.Total, want)
			}
		})
	}
}

func TestQuerySeriesDayBuckets(t *testing.T) {
	s := seededStore(t,
		attempt{at: t0.Add(time.Hour), provider: "claude", model: "model-a", in: 1},
		attempt{at: t0.Add(23 * time.Hour), provider: "claude", model: "model-a", in: 2},
		attempt{at: t0.Add(36 * time.Hour), provider: "claude", model: "model-a", in: 4},
	)
	res := querySeries(t, s, usagestore.Query{From: t0.Add(6 * time.Hour), To: t0.Add(49 * time.Hour), Bucket: usagestore.BucketDay}, nil)
	if len(res.Buckets) != 3 {
		t.Fatalf("buckets = %v, want 3 days", res.Buckets)
	}
	for i, p := range res.Series[0].Points {
		day := t0.Add(time.Duration(i) * 24 * time.Hour)
		if !p.BucketStart.Equal(day) || p.Tokens.Total != []int64{3, 4, 0}[i] {
			t.Fatalf("point %d = %v tokens %d, want %v tokens %d", i, p.BucketStart, p.Tokens.Total, day, []int64{3, 4, 0}[i])
		}
	}
}

func groupFixture() []attempt {
	return []attempt{
		{at: t0, provider: "claude", model: "model-a", in: 100},
		{at: t0.Add(time.Hour), provider: "claude", model: "model-b", in: 10},
		{at: t0, provider: "codex", model: "model-a", in: 50},
		{at: t0, provider: "gemini", model: "model-a", in: 7},
	}
}

func TestQuerySeriesGroupBy(t *testing.T) {
	s := seededStore(t, groupFixture()...)
	q := usagestore.Query{From: t0, To: t0.Add(2 * time.Hour)}

	q.GroupBy = []usagestore.Dimension{usagestore.DimProvider}
	assertStrings(t, "by provider", seriesTokens(querySeries(t, s, q, nil), q.GroupBy...),
		[]string{"/claude=110", "/codex=50", "/gemini=7"})

	q.GroupBy = []usagestore.Dimension{usagestore.DimProvider, usagestore.DimModel}
	res := querySeries(t, s, q, nil)
	assertStrings(t, "by provider and model", seriesTokens(res, q.GroupBy...),
		[]string{"/claude/model-a=100", "/codex/model-a=50", "/claude/model-b=10", "/gemini/model-a=7"})
	if res.Total.Tokens.Total != 167 || res.Total.Requests != 4 {
		t.Fatalf("result total = %+v, want 167 tokens over 4 requests", res.Total)
	}
	if got := res.Series[2].Points; got[0].Tokens.Total != 0 || got[1].Tokens.Total != 10 {
		t.Fatalf("claude/model-b points = %+v, want [0 10]", got)
	}
}

func TestQuerySeriesFilters(t *testing.T) {
	s := seededStore(t, groupFixture()...)
	q := usagestore.Query{From: t0, To: t0.Add(2 * time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimProvider}}

	q.Filters = map[usagestore.Dimension][]string{
		usagestore.DimProvider: {"claude", "codex"},
		usagestore.DimModel:    {"model-a"},
	}
	assertStrings(t, "or within, and across", seriesTokens(querySeries(t, s, q, nil), q.GroupBy...),
		[]string{"/claude=100", "/codex=50"})

	q.Filters = map[usagestore.Dimension][]string{usagestore.DimProvider: {"claude' OR 1=1 --"}, usagestore.DimModel: {}}
	if res := querySeries(t, s, q, nil); len(res.Series) != 0 || res.Total.Requests != 0 {
		t.Fatalf("injection-shaped filter matched %+v", res.Series)
	}
}

func TestQuerySeriesLimitFoldsRestIntoOther(t *testing.T) {
	s := seededStore(t,
		attempt{at: t0, provider: "p1", model: "m", in: 50},
		attempt{at: t0, provider: "p2", model: "m", in: 40},
		attempt{at: t0, provider: "p3", model: "m", in: 30},
		attempt{at: t0.Add(time.Hour), provider: "p4", model: "m", in: 20, latency: 900 * time.Millisecond},
		attempt{at: t0.Add(time.Hour), provider: "p5", model: "n", in: 10},
	)
	q := usagestore.Query{From: t0, To: t0.Add(2 * time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimProvider}, Limit: 2}
	res := querySeries(t, s, q, nil)
	// "other" holds more tokens than p1 and still sorts last.
	assertStrings(t, "series", seriesTokens(res, q.GroupBy...), []string{"/p1=50", "/p2=40", "/other=60"})
	other := res.Series[2]
	if other.Total.Requests != 3 || other.Total.LatencyMsMax != 900 || other.Hints != nil {
		t.Fatalf("other total = %+v hints %v", other.Total, other.Hints)
	}
	if other.Points[0].Tokens.Total != 30 || other.Points[1].Tokens.Total != 30 {
		t.Fatalf("other points = %+v, want [30 30]", other.Points)
	}
	if res.Total.Tokens.Total != 150 {
		t.Fatalf("result total tokens = %d, want 150", res.Total.Tokens.Total)
	}

	q.GroupBy, q.Limit = []usagestore.Dimension{usagestore.DimProvider, usagestore.DimModel}, 1
	res = querySeries(t, s, q, nil)
	assertStrings(t, "two-dimension fold", seriesTokens(res, q.GroupBy...), []string{"/p1/m=50", "/other/other=100"})
}

func TestQuerySeriesDefaultLimit(t *testing.T) {
	attempts := make([]attempt, 25)
	for i := range attempts {
		attempts[i] = attempt{at: t0, provider: fmt.Sprintf("p%02d", i), in: int64(100 - i)}
	}
	s := seededStore(t, attempts...)
	res := querySeries(t, s, usagestore.Query{From: t0, To: t0.Add(time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimProvider}}, nil)
	if len(res.Series) != 21 || res.Series[20].Key[usagestore.DimProvider] != "other" || res.Series[20].Total.Requests != 5 {
		t.Fatalf("default limit gave %d series, last %v", len(res.Series), res.Series[len(res.Series)-1].Key)
	}
}

func TestQuerySeriesPricesEachModelWhileGroupedByProvider(t *testing.T) {
	s := seededStore(t,
		attempt{at: t0, provider: "claude", model: "model-a", in: million, out: million},
		attempt{at: t0, provider: "claude", model: "model-b", in: million},
		attempt{at: t0.Add(time.Hour), provider: "claude", model: "unpriced", in: million},
		attempt{at: t0, provider: "codex", model: "model-a", reasoning: million},
	)
	q := usagestore.Query{From: t0, To: t0.Add(2 * time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimProvider}}

	res := querySeries(t, s, q, testPrices)
	claude := res.Series[0]
	assertFloat(t, "claude hour 0 cost", claude.Points[0].CostUSD, 1+2+10)
	assertFloat(t, "claude hour 1 cost", claude.Points[1].CostUSD, 0)
	assertFloat(t, "claude cost", claude.Total.CostUSD, 13)
	assertFloat(t, "codex cost", res.Series[1].Total.CostUSD, 2)
	assertFloat(t, "result cost", res.Total.CostUSD, 15)

	withDefault := maps.Clone(testPrices)
	withDefault["default"] = usagestore.ModelPrice{Input: 0.5}
	assertFloat(t, "cost with default", querySeries(t, s, q, withDefault).Total.CostUSD, 15.5)
	assertFloat(t, "cost without prices", querySeries(t, s, q, nil).Total.CostUSD, 0)
}

func TestQuerySeriesStreamDimension(t *testing.T) {
	s := seededStore(t,
		attempt{at: t0, provider: "claude", stream: true, in: 10},
		attempt{at: t0, provider: "claude", in: 20},
	)
	q := usagestore.Query{From: t0, To: t0.Add(time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimStream}}
	assertStrings(t, "by stream", seriesTokens(querySeries(t, s, q, nil), q.GroupBy...), []string{"/sync=20", "/stream=10"})

	q.GroupBy = nil
	q.Filters = map[usagestore.Dimension][]string{usagestore.DimStream: {"stream"}}
	if got := querySeries(t, s, q, nil).Total.Tokens.Total; got != 10 {
		t.Fatalf("stream-filtered tokens = %d, want 10", got)
	}
}

func TestQuerySeriesCarriesAPIKeyHint(t *testing.T) {
	const keyA, keyB = "sk-alpha-key-00000001", "sk-bravo-key-00000002"
	a := usagestore.EventFromRecord(usage.Record{APIKey: keyA})
	b := usagestore.EventFromRecord(usage.Record{APIKey: keyB})
	s := seededStore(t,
		attempt{at: t0, provider: "claude", apiKey: keyA, in: 30},
		attempt{at: t0, provider: "claude", apiKey: keyB, in: 20},
		attempt{at: t0, provider: "claude", in: 10},
	)
	q := usagestore.Query{From: t0, To: t0.Add(time.Hour), GroupBy: []usagestore.Dimension{usagestore.DimAPIKey}}
	res := querySeries(t, s, q, nil)
	want := []struct{ hash, hint string }{{a.APIKeyHash, a.APIKeyHint}, {b.APIKeyHash, b.APIKeyHint}, {"", ""}}
	if len(res.Series) != len(want) {
		t.Fatalf("series = %d, want %d", len(res.Series), len(want))
	}
	for i, w := range want {
		got := res.Series[i]
		if got.Key[usagestore.DimAPIKey] != w.hash || got.Hints[usagestore.DimAPIKey] != w.hint {
			t.Fatalf("series %d key %v hints %v, want %s %s", i, got.Key, got.Hints, w.hash, w.hint)
		}
	}

	q.Filters = map[usagestore.Dimension][]string{usagestore.DimAPIKey: {b.APIKeyHash}}
	if got := querySeries(t, s, q, nil).Total.Tokens.Total; got != 20 {
		t.Fatalf("api_key-filtered tokens = %d, want 20", got)
	}
}

func TestQueriesRejectInvalidInput(t *testing.T) {
	s, _ := openStore(t, usagestore.Options{})
	ctx := context.Background()
	valid := usagestore.Query{From: t0, To: t0.Add(time.Hour)}
	for _, tt := range []struct {
		name string
		edit func(*usagestore.Query)
	}{
		{"empty range", func(q *usagestore.Query) { q.To = q.From }},
		{"unknown bucket", func(q *usagestore.Query) { q.Bucket = "week" }},
		{"unknown group", func(q *usagestore.Query) { q.GroupBy = []usagestore.Dimension{"provider; DROP TABLE usage_hourly"} }},
		{"duplicate group", func(q *usagestore.Query) {
			q.GroupBy = []usagestore.Dimension{usagestore.DimModel, usagestore.DimModel}
		}},
		{"unknown filter", func(q *usagestore.Query) { q.Filters = map[usagestore.Dimension][]string{"nope": {"x"}} }},
		{"too many buckets", func(q *usagestore.Query) { q.To = q.From.Add(20 * 365 * 24 * time.Hour) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := valid
			tt.edit(&q)
			if _, err := s.QuerySeries(ctx, q, nil); !errors.Is(err, usagestore.ErrInvalidQuery) {
				t.Fatalf("QuerySeries() error = %v, want ErrInvalidQuery", err)
			}
		})
	}
	if _, err := s.QueryBreakdown(ctx, t0, t0.Add(time.Hour), "nope", nil, nil); !errors.Is(err, usagestore.ErrInvalidQuery) {
		t.Fatalf("QueryBreakdown(unknown dim) error = %v, want ErrInvalidQuery", err)
	}
	if _, err := s.QueryDimensionValues(ctx, t0, t0); !errors.Is(err, usagestore.ErrInvalidQuery) {
		t.Fatalf("QueryDimensionValues(empty range) error = %v, want ErrInvalidQuery", err)
	}
}

func TestQueriesRespectContextCancellation(t *testing.T) {
	s := seededStore(t, groupFixture()...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.QuerySeries(ctx, usagestore.Query{From: t0, To: t0.Add(time.Hour)}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("QuerySeries() error = %v, want context.Canceled", err)
	}
	if _, err := s.QueryBreakdown(ctx, t0, t0.Add(time.Hour), usagestore.DimModel, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("QueryBreakdown() error = %v, want context.Canceled", err)
	}
	if _, err := s.QueryDimensionValues(ctx, t0, t0.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("QueryDimensionValues() error = %v, want context.Canceled", err)
	}
}

func TestQueryBreakdown(t *testing.T) {
	const keyA, keyB = "sk-alpha-key-00000001", "sk-bravo-key-00000002"
	a := usagestore.EventFromRecord(usage.Record{APIKey: keyA})
	b := usagestore.EventFromRecord(usage.Record{APIKey: keyB})
	s := seededStore(t,
		attempt{at: t0, provider: "claude", model: "model-a", apiKey: keyA, in: 100, latency: 200 * time.Millisecond},
		attempt{at: t0.Add(time.Hour), provider: "claude", model: "model-b", apiKey: keyB, in: million},
		attempt{at: t0, provider: "codex", model: "model-a", apiKey: keyA, in: 50, latency: 700 * time.Millisecond},
		attempt{at: t0.Add(48 * time.Hour), provider: "codex", model: "model-a", in: 999},
	)
	ctx := context.Background()
	from, to := t0, t0.Add(24*time.Hour)

	rows, err := s.QueryBreakdown(ctx, from, to, usagestore.DimModel, map[usagestore.Dimension][]string{usagestore.DimProvider: {"claude"}}, testPrices)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Value != "model-b" || rows[1].Value != "model-a" || rows[0].Hint != "" {
		t.Fatalf("model breakdown = %+v", rows)
	}
	assertFloat(t, "model-b cost", rows[0].Point.CostUSD, 10)
	if rows[1].Point.Tokens.Total != 100 {
		t.Fatalf("model-a tokens = %d, want 100", rows[1].Point.Tokens.Total)
	}

	rows, err = s.QueryBreakdown(ctx, from, to, usagestore.DimAPIKey, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Value != b.APIKeyHash || rows[0].Hint != b.APIKeyHint ||
		rows[1].Value != a.APIKeyHash || rows[1].Hint != a.APIKeyHint {
		t.Fatalf("api_key breakdown = %+v", rows)
	}
	if p := rows[1].Point; p.Requests != 2 || p.Tokens.Total != 150 || p.LatencyMsMax != 700 || p.LatencyMsSum != 900 {
		t.Fatalf("key A point = %+v", p)
	}

	rows, err = s.QueryBreakdown(ctx, from, to, usagestore.DimProvider, nil, testPrices)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Value != "claude" || rows[1].Value != "codex" {
		t.Fatalf("provider breakdown = %+v", rows)
	}
	assertFloat(t, "claude cost", rows[0].Point.CostUSD, 10+100.0/million)

	rows, err = s.QueryBreakdown(ctx, t0.Add(-48*time.Hour), t0.Add(-24*time.Hour), usagestore.DimProvider, nil, nil)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty breakdown = %#v, %v; want empty non-nil slice", rows, err)
	}
}

func TestQueryDimensionValues(t *testing.T) {
	const key = "sk-alpha-key-00000001"
	hash := usagestore.EventFromRecord(usage.Record{APIKey: key}).APIKeyHash
	s := seededStore(t,
		attempt{at: t0, provider: "codex", model: "model-b", requested: "fast", account: "b.json", apiKey: key, effort: "high", tier: "priority", stream: true, in: 1},
		attempt{at: t0.Add(time.Hour), provider: "claude", model: "model-a", account: "a.json", tier: "default", in: 1},
		attempt{at: t0.Add(48 * time.Hour), provider: "late", model: "model-z", in: 1},
	)
	got, err := s.QueryDimensionValues(context.Background(), t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := map[usagestore.Dimension][]string{
		usagestore.DimProvider:        {"claude", "codex"},
		usagestore.DimModel:           {"model-a", "model-b"},
		usagestore.DimRequestedModel:  {"fast", "model-a"},
		usagestore.DimAccount:         {"a.json", "b.json"},
		usagestore.DimAPIKey:          {"", hash},
		usagestore.DimReasoningEffort: {"", "high"},
		usagestore.DimServiceTier:     {"default", "priority"},
		usagestore.DimStream:          {"stream", "sync"},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("QueryDimensionValues() =\n%q\nwant\n%q", got, want)
	}

	empty, err := s.QueryDimensionValues(context.Background(), t0.Add(-24*time.Hour), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != len(want) || empty[usagestore.DimProvider] == nil || len(empty[usagestore.DimProvider]) != 0 {
		t.Fatalf("empty range values = %#v, want every dimension with an empty list", empty)
	}
}
