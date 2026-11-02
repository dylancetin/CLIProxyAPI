package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

type fakeBreakdownCall struct {
	from, to time.Time
	dim      usagestore.Dimension
	filters  map[usagestore.Dimension][]string
	prices   usagestore.Prices
}

type fakeUsageStore struct {
	mu             sync.Mutex
	series         usagestore.SeriesResult
	breakdown      map[usagestore.Dimension][]usagestore.BreakdownRow
	values         map[usagestore.Dimension][]string
	err            error
	seriesCalls    []usagestore.Query
	seriesPrices   []usagestore.Prices
	breakdownCalls []fakeBreakdownCall
	valueCalls     [][2]time.Time
}

func (f *fakeUsageStore) QuerySeries(_ context.Context, q usagestore.Query, prices usagestore.Prices) (usagestore.SeriesResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seriesCalls = append(f.seriesCalls, q)
	f.seriesPrices = append(f.seriesPrices, prices)
	return f.series, f.err
}

func (f *fakeUsageStore) QueryBreakdown(_ context.Context, from, to time.Time, dim usagestore.Dimension, filters map[usagestore.Dimension][]string, prices usagestore.Prices) ([]usagestore.BreakdownRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakdownCalls = append(f.breakdownCalls, fakeBreakdownCall{from: from, to: to, dim: dim, filters: filters, prices: prices})
	return f.breakdown[dim], f.err
}

func (f *fakeUsageStore) QueryDimensionValues(_ context.Context, from, to time.Time) (map[usagestore.Dimension][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCalls = append(f.valueCalls, [2]time.Time{from, to})
	return f.values, f.err
}

func (f *fakeUsageStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seriesCalls) + len(f.breakdownCalls) + len(f.valueCalls)
}

// withUsageTracker installs store (nil means disabled) and an isolated in-memory
// price source, restoring the defaults after the test.
func withUsageTracker(t *testing.T, store usageSeriesStore) *memoryPricesStore {
	t.Helper()
	prices := newMemoryPricesStore()
	setUsageSeriesStore(store)
	SetUsageTrackerPrices(prices)
	t.Cleanup(func() {
		setUsageSeriesStore(nil)
		SetUsageTrackerPrices(nil)
	})
	return prices
}

func withUsageTrackerNow(t *testing.T, now time.Time) {
	t.Helper()
	previous := usageTrackerNow
	usageTrackerNow = func() time.Time { return now }
	t.Cleanup(func() { usageTrackerNow = previous })
}

func serveUsageTracker(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	router := gin.New()
	router.GET("/usage/series", h.GetUsageSeries)
	router.GET("/usage/summary", h.GetUsageSummary)
	router.GET("/usage/dimensions", h.GetUsageDimensions)
	router.GET("/usage/prices", h.GetUsagePrices)
	router.PUT("/usage/prices", h.PutUsagePrices)
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func decodeUsageJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &out); errDecode != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), errDecode)
	}
	return out
}

func mustJSONValue(t *testing.T, raw string) any {
	t.Helper()
	var out any
	if errDecode := json.Unmarshal([]byte(raw), &out); errDecode != nil {
		t.Fatalf("decode expected %q: %v", raw, errDecode)
	}
	return out
}

func assertJSONEqual(t *testing.T, label string, got any, want string) {
	t.Helper()
	if expected := mustJSONValue(t, want); !reflect.DeepEqual(got, expected) {
		gotJSON, _ := json.Marshal(got)
		t.Fatalf("%s = %s, want %s", label, gotJSON, want)
	}
}

func usageTestPoint(requests, tokens, latencyMax int64, cost float64) usagestore.Point {
	return usagestore.Point{
		Requests:     requests,
		Failures:     requests / 4,
		Tokens:       usagestore.Tokens{InputUncached: tokens / 2, OutputOther: tokens - tokens/2, Total: tokens},
		LatencyMsSum: latencyMax * requests,
		LatencyMsMax: latencyMax,
		TTFTMsSum:    10 * requests,
		TTFTMsMax:    10,
		TTFTCount:    requests,
		CostUSD:      cost,
	}
}

func TestUsageSeriesReturnsContractShape(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fullPoint := usagestore.Point{
		BucketStart: from,
		Requests:    12, Failures: 1,
		Tokens: usagestore.Tokens{
			InputUncached: 1, InputCacheRead: 2, InputCacheWrite: 3,
			OutputReasoning: 4, OutputOther: 5, Unclassified: 6, Total: 21,
		},
		LatencyMsSum: 700, LatencyMsMax: 90, TTFTMsSum: 300, TTFTMsMax: 40, TTFTCount: 11, CostUSD: 0.25,
	}
	store := &fakeUsageStore{series: usagestore.SeriesResult{
		Buckets: []time.Time{from, from.Add(time.Hour)},
		Series: []usagestore.Series{
			{
				Key:    usagestore.SeriesKey{usagestore.DimProvider: "claude", usagestore.DimModel: "claude-opus-5-5"},
				Points: []usagestore.Point{fullPoint, {BucketStart: from.Add(time.Hour)}},
				Total:  fullPoint,
			},
			{
				Key:    usagestore.SeriesKey{usagestore.DimProvider: "other", usagestore.DimModel: "other"},
				Points: []usagestore.Point{{BucketStart: from}, usageTestPoint(2, 10, 5, 0)},
				Total:  usageTestPoint(2, 10, 5, 0),
			},
		},
		Total: usageTestPoint(14, 31, 90, 0.25),
	}}
	prices := withUsageTracker(t, store)
	stored := usagestore.Prices{"claude-opus-5-5": {Input: 15, Output: 75}}
	if errPut := prices.Put(stored); errPut != nil {
		t.Fatal(errPut)
	}

	target := "/usage/series?from=2026-11-01T01:00:00%2B01:00&to=1793577600&bucket=hour&group=provider,%20model,provider" +
		"&filter.provider=claude,gemini&filter.provider=codex&filter.api_key=abc&limit=5&unknown=1"
	rec := serveUsageTracker(t, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeUsageJSON(t, rec)
	if len(body) != 6 {
		t.Fatalf("top-level keys = %v, want from,to,bucket,buckets,series,total", body)
	}
	assertJSONEqual(t, "from", body["from"], `"2026-11-01T00:00:00Z"`)
	assertJSONEqual(t, "to", body["to"], `"2026-11-02T00:00:00Z"`)
	assertJSONEqual(t, "bucket", body["bucket"], `"hour"`)
	assertJSONEqual(t, "buckets", body["buckets"], `["2026-11-01T00:00:00Z","2026-11-01T01:00:00Z"]`)
	series := body["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series = %v", series)
	}
	first := series[0].(map[string]any)
	assertJSONEqual(t, "series[0].key", first["key"], `{"provider":"claude","model":"claude-opus-5-5"}`)
	fullPointJSON := `{"requests":12,"failures":1,"tokens":{"input_uncached":1,"input_cache_read":2,"input_cache_write":3,` +
		`"output_reasoning":4,"output_other":5,"unclassified":6,"total":21},"latency_ms_sum":700,"latency_ms_max":90,` +
		`"ttft_ms_sum":300,"ttft_ms_max":40,"ttft_count":11,"cost_usd":0.25}`
	zeroPointJSON := `{"requests":0,"failures":0,"tokens":{"input_uncached":0,"input_cache_read":0,"input_cache_write":0,` +
		`"output_reasoning":0,"output_other":0,"unclassified":0,"total":0},"latency_ms_sum":0,"latency_ms_max":0,` +
		`"ttft_ms_sum":0,"ttft_ms_max":0,"ttft_count":0,"cost_usd":0}`
	assertJSONEqual(t, "series[0].points", first["points"], "["+fullPointJSON+","+zeroPointJSON+"]")
	assertJSONEqual(t, "series[0].total", first["total"], fullPointJSON)
	second := series[1].(map[string]any)
	assertJSONEqual(t, "series[1].key", second["key"], `{"provider":"other","model":"other"}`)
	if points := second["points"].([]any); len(points) != 2 {
		t.Fatalf("series[1] points = %d, want dense 2", len(points))
	}
	total := body["total"].(map[string]any)
	if total["requests"] != float64(14) || total["cost_usd"] != 0.25 {
		t.Fatalf("total = %v", total)
	}

	if len(store.seriesCalls) != 1 {
		t.Fatalf("series calls = %d", len(store.seriesCalls))
	}
	want := usagestore.Query{
		From:    from,
		To:      time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		Bucket:  usagestore.BucketHour,
		GroupBy: []usagestore.Dimension{usagestore.DimProvider, usagestore.DimModel},
		Filters: map[usagestore.Dimension][]string{
			usagestore.DimProvider: {"claude", "gemini", "codex"},
			usagestore.DimAPIKey:   {"abc"},
		},
		Limit: 5,
	}
	if got := store.seriesCalls[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("query = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(store.seriesPrices[0], stored) {
		t.Fatalf("prices = %v, want %v", store.seriesPrices[0], stored)
	}
}

func TestUsageSeriesDefaultsAndEmptyResult(t *testing.T) {
	now := time.Date(2026, 11, 2, 15, 4, 5, 999, time.UTC)
	withUsageTrackerNow(t, now)
	store := &fakeUsageStore{series: usagestore.SeriesResult{Series: []usagestore.Series{{}}}}
	withUsageTracker(t, store)

	rec := serveUsageTracker(t, http.MethodGet, "/usage/series", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeUsageJSON(t, rec)
	assertJSONEqual(t, "from", body["from"], `"2026-11-01T15:04:05Z"`)
	assertJSONEqual(t, "to", body["to"], `"2026-11-02T15:04:05Z"`)
	assertJSONEqual(t, "bucket", body["bucket"], `"hour"`)
	assertJSONEqual(t, "buckets", body["buckets"], `[]`)
	series := body["series"].([]any)
	assertJSONEqual(t, "series[0].key", series[0].(map[string]any)["key"], `{}`)
	assertJSONEqual(t, "series[0].points", series[0].(map[string]any)["points"], `[]`)

	q := store.seriesCalls[0]
	if q.Limit != usageTrackerDefaultLimit || len(q.GroupBy) != 0 || len(q.Filters) != 0 || q.Bucket != usagestore.BucketHour {
		t.Fatalf("default query = %+v", q)
	}
	if !q.To.Equal(now.Truncate(time.Second)) || q.To.Sub(q.From) != 24*time.Hour {
		t.Fatalf("default range = %s..%s", q.From, q.To)
	}

	rec = serveUsageTracker(t, http.MethodGet, "/usage/series?to=2026-11-01T00:00:00Z&bucket=DAY", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	q = store.seriesCalls[1]
	if q.Bucket != usagestore.BucketDay || !q.From.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("to-only query = %+v", q)
	}
}

func TestUsageSeriesClampsLimitAndAllowsMaxRange(t *testing.T) {
	store := &fakeUsageStore{}
	withUsageTracker(t, store)
	from := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(400 * 24 * time.Hour)
	target := fmt.Sprintf("/usage/series?from=%d&to=%d&limit=1000", from.Unix(), to.Unix())
	rec := serveUsageTracker(t, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := store.seriesCalls[0].Limit; got != usageTrackerMaxLimit {
		t.Fatalf("limit = %d, want %d", got, usageTrackerMaxLimit)
	}
}

func TestUsageQueriesRejectInvalidParameters(t *testing.T) {
	store := &fakeUsageStore{}
	withUsageTracker(t, store)
	for _, tc := range []struct{ name, target, message string }{
		{"bad from", "/usage/series?from=yesterday", "from must be RFC3339 or unix seconds"},
		{"bad to", "/usage/series?to=2026-13-01T00:00:00Z", "to must be RFC3339 or unix seconds"},
		{"year out of range", "/usage/series?to=999999999999", "to must be RFC3339 or unix seconds"},
		{"to before from", "/usage/series?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", "to must be after from"},
		{"to equals from", "/usage/series?from=1793577600&to=1793577600", "to must be after from"},
		{"range too long", "/usage/series?from=2025-01-01T00:00:00Z&to=2026-11-01T00:00:00Z", "range must not exceed 400 days"},
		{"bad bucket", "/usage/series?bucket=week", "bucket must be hour or day"},
		{"unknown group", "/usage/series?group=provider,colour", `unknown group dimension "colour"`},
		{"unknown filter", "/usage/series?filter.colour=red", `unknown filter dimension "colour"`},
		{"zero limit", "/usage/series?limit=0", "limit must be a positive integer"},
		{"bad limit", "/usage/series?limit=ten", "limit must be a positive integer"},
		{"summary bad range", "/usage/summary?from=2026-11-02T00:00:00Z&to=2026-11-01T00:00:00Z", "to must be after from"},
		{"summary unknown filter", "/usage/summary?filter.colour=red", `unknown filter dimension "colour"`},
		{"dimensions bad time", "/usage/dimensions?from=nope", "from must be RFC3339 or unix seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveUsageTracker(t, http.MethodGet, tc.target, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			assertJSONEqual(t, "body", decodeUsageJSON(t, rec), fmt.Sprintf(`{"error":%q}`, tc.message))
		})
	}
	if store.calls() != 0 {
		t.Fatalf("store queried %d times for invalid requests", store.calls())
	}
}

func TestUsageSummaryReturnsBreakdowns(t *testing.T) {
	models := make([]usagestore.BreakdownRow, 0, 120)
	for i := range 120 {
		models = append(models, usagestore.BreakdownRow{Value: fmt.Sprintf("model-%03d", i), Point: usageTestPoint(1, int64(200-i), 1, 0)})
	}
	store := &fakeUsageStore{breakdown: map[usagestore.Dimension][]usagestore.BreakdownRow{
		usagestore.DimProvider: {
			{Value: "claude", Point: usageTestPoint(8, 400, 120, 1.5)},
			{Value: "gemini", Point: usageTestPoint(4, 100, 300, 0.25)},
		},
		usagestore.DimModel:  models,
		usagestore.DimAPIKey: {{Value: "hash-1", Hint: "sk-a…b2c3", Point: usageTestPoint(12, 500, 300, 1.75)}},
	}}
	prices := withUsageTracker(t, store)
	if errPut := prices.Put(usagestore.Prices{"default": {Input: 1}}); errPut != nil {
		t.Fatal(errPut)
	}

	rec := serveUsageTracker(t, http.MethodGet, "/usage/summary?from=1793491200&to=1793577600&filter.provider=claude,gemini&group=ignored", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeUsageJSON(t, rec)
	if len(body) != 2 {
		t.Fatalf("top-level keys = %v, want total,by", body)
	}
	assertJSONEqual(t, "total", body["total"], `{"requests":12,"failures":3,"tokens":{"input_uncached":250,"input_cache_read":0,`+
		`"input_cache_write":0,"output_reasoning":0,"output_other":250,"unclassified":0,"total":500},"latency_ms_sum":2160,`+
		`"latency_ms_max":300,"ttft_ms_sum":120,"ttft_ms_max":10,"ttft_count":12,"cost_usd":1.75}`)
	by := body["by"].(map[string]any)
	if len(by) != len(usageTrackerDimensions) {
		t.Fatalf("by keys = %d, want %d", len(by), len(usageTrackerDimensions))
	}
	for _, dim := range usageTrackerDimensions {
		if _, ok := by[string(dim)].([]any); !ok {
			t.Fatalf("by[%s] = %v, want a list", dim, by[string(dim)])
		}
	}
	providers := by["provider"].([]any)
	if len(providers) != 2 || providers[0].(map[string]any)["value"] != "claude" {
		t.Fatalf("providers = %v", providers)
	}
	if _, hasHint := providers[0].(map[string]any)["hint"]; hasHint {
		t.Fatalf("provider row carries a hint: %v", providers[0])
	}
	if got := len(by["model"].([]any)); got != usageTrackerBreakdownLimit {
		t.Fatalf("model rows = %d, want cap %d", got, usageTrackerBreakdownLimit)
	}
	apiKey := by["api_key"].([]any)[0].(map[string]any)
	if apiKey["value"] != "hash-1" || apiKey["hint"] != "sk-a…b2c3" {
		t.Fatalf("api_key row = %v", apiKey)
	}
	assertJSONEqual(t, "by.stream", by["stream"], `[]`)

	if len(store.breakdownCalls) != len(usageTrackerDimensions) {
		t.Fatalf("breakdown calls = %d", len(store.breakdownCalls))
	}
	wantFilters := map[usagestore.Dimension][]string{usagestore.DimProvider: {"claude", "gemini"}}
	for _, call := range store.breakdownCalls {
		if !reflect.DeepEqual(call.filters, wantFilters) || call.prices["default"].Input != 1 ||
			!call.from.Equal(time.Unix(1793491200, 0)) || !call.to.Equal(time.Unix(1793577600, 0)) {
			t.Fatalf("breakdown call = %+v", call)
		}
	}
}

func TestUsageDimensionsReturnsValuesAndHints(t *testing.T) {
	store := &fakeUsageStore{
		values: map[usagestore.Dimension][]string{
			usagestore.DimProvider: {"claude", "gemini"},
			usagestore.DimStream:   {"stream", "sync"},
			usagestore.DimAPIKey:   {"hash-1", "hash-2"},
		},
		breakdown: map[usagestore.Dimension][]usagestore.BreakdownRow{
			usagestore.DimAPIKey: {{Value: "hash-1", Hint: "sk-a…b2c3"}},
		},
	}
	withUsageTracker(t, store)

	rec := serveUsageTracker(t, http.MethodGet, "/usage/dimensions?from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONEqual(t, "dimensions", decodeUsageJSON(t, rec), `{
		"provider": ["claude", "gemini"], "model": [], "requested_model": [], "account": [],
		"api_key": [{"value": "hash-1", "hint": "sk-a…b2c3"}, {"value": "hash-2", "hint": ""}],
		"reasoning_effort": [], "service_tier": [], "stream": ["stream", "sync"]
	}`)
	if len(store.valueCalls) != 1 || !store.valueCalls[0][0].Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("value calls = %v", store.valueCalls)
	}
}

func TestUsageEndpointsReturn503WhenDisabled(t *testing.T) {
	withUsageTracker(t, &fakeUsageStore{})
	SetUsageTrackerStore(nil)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/usage/series", ""},
		{http.MethodGet, "/usage/summary", ""},
		{http.MethodGet, "/usage/dimensions", ""},
		{http.MethodGet, "/usage/prices", ""},
		{http.MethodPut, "/usage/prices", `{"prices":{}}`},
	} {
		rec := serveUsageTracker(t, tc.method, tc.target, tc.body)
		if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"error":"usage tracker disabled"}` {
			t.Fatalf("%s %s: status=%d body=%s", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}

func TestUsageStoreErrorsReturnGeneric500(t *testing.T) {
	store := &fakeUsageStore{
		err:    errors.New("SELECT api_key_hash FROM usage_events: database is locked"),
		values: map[usagestore.Dimension][]string{usagestore.DimAPIKey: {"hash-1"}},
	}
	withUsageTracker(t, store)
	for _, target := range []string{"/usage/series", "/usage/summary", "/usage/dimensions"} {
		rec := serveUsageTracker(t, http.MethodGet, target, "")
		if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":"usage query failed"}` {
			t.Fatalf("%s: status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

type failingPricesStore struct{ memoryPricesStore }

func (*failingPricesStore) Put(usagestore.Prices) error { return errors.New("disk full") }

func TestUsagePricesDefaultSourceIsInMemory(t *testing.T) {
	store := &fakeUsageStore{}
	withUsageTracker(t, store)
	SetUsageTrackerPrices(nil)
	t.Cleanup(func() { _ = defaultUsageTrackerPrices.Put(nil) })

	if rec := serveUsageTracker(t, http.MethodGet, "/usage/prices", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"prices":{}}` {
		t.Fatalf("initial GET: status=%d body=%s", rec.Code, rec.Body.String())
	}

	put := `{"prices":{" claude-opus-5-5 ":{"input":15,"cache_read":1.5,"cache_write":18.75,"output":75},"default":{"input":1}}}`
	want := `{"prices":{"claude-opus-5-5":{"input":15,"cache_read":1.5,"cache_write":18.75,"output":75},` +
		`"default":{"input":1,"cache_read":0,"cache_write":0,"output":0}}}`
	rec := serveUsageTracker(t, http.MethodPut, "/usage/prices", put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONEqual(t, "PUT response", decodeUsageJSON(t, rec), want)
	assertJSONEqual(t, "GET after PUT", decodeUsageJSON(t, serveUsageTracker(t, http.MethodGet, "/usage/prices", "")), want)

	// The default source hands out copies, so callers cannot mutate stored prices.
	defaultUsageTrackerPrices.Get()["claude-opus-5-5"] = usagestore.ModelPrice{}
	assertJSONEqual(t, "GET after caller mutation", decodeUsageJSON(t, serveUsageTracker(t, http.MethodGet, "/usage/prices", "")), want)

	if rec := serveUsageTracker(t, http.MethodGet, "/usage/series", ""); rec.Code != http.StatusOK {
		t.Fatalf("series: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := store.seriesPrices[0]["claude-opus-5-5"]; got != (usagestore.ModelPrice{Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75}) {
		t.Fatalf("series prices = %+v", store.seriesPrices[0])
	}

	rec = serveUsageTracker(t, http.MethodPut, "/usage/prices", `{"prices":{"gpt-6":{"output":2}}}`)
	assertJSONEqual(t, "replacing PUT", decodeUsageJSON(t, rec), `{"prices":{"gpt-6":{"input":0,"cache_read":0,"cache_write":0,"output":2}}}`)
	rec = serveUsageTracker(t, http.MethodPut, "/usage/prices", `{"prices":{}}`)
	assertJSONEqual(t, "clearing PUT", decodeUsageJSON(t, rec), `{"prices":{}}`)
}

func TestPutUsagePricesRejectsInvalidBodies(t *testing.T) {
	prices := withUsageTracker(t, &fakeUsageStore{})
	seed := usagestore.Prices{"seed": {Input: 1}}
	if errPut := prices.Put(seed); errPut != nil {
		t.Fatal(errPut)
	}
	tooMany := make(map[string]usagestore.ModelPrice, usageTrackerMaxPrices+1)
	for i := range usageTrackerMaxPrices + 1 {
		tooMany[fmt.Sprintf("model-%d", i)] = usagestore.ModelPrice{Input: 1}
	}
	tooManyBody, errMarshal := json.Marshal(map[string]any{"prices": tooMany})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, tc := range []struct{ name, body, message string }{
		{"empty body", ``, "invalid body: "},
		{"malformed JSON", `{"prices":`, "invalid body: "},
		{"top-level array", `[]`, "invalid body: "},
		{"prices array", `{"prices":[]}`, "invalid body: "},
		{"string number", `{"prices":{"m":{"input":"1"}}}`, "invalid body: "},
		{"overflowing number", `{"prices":{"m":{"input":1e400}}}`, "invalid body: "},
		{"unknown price field", `{"prices":{"m":{"cache-read":1}}}`, "invalid body: "},
		{"unknown top-level field", `{"prices":{},"extra":true}`, "invalid body: "},
		{"trailing data", `{"prices":{}} {}`, "invalid body: unexpected data after JSON object"},
		{"missing prices", `{}`, "prices must be an object"},
		{"null prices", `{"prices":null}`, "prices must be an object"},
		{"blank model", `{"prices":{"  ":{"input":1}}}`, "model name must not be empty"},
		{"null entry", `{"prices":{"m":null}}`, `price for "m" must be an object`},
		{"negative output", `{"prices":{"m":{"output":-0.5}}}`, `output for "m" must be a non-negative finite number`},
		{"negative cache read", `{"prices":{"m":{"cache_read":-1}}}`, `cache_read for "m" must be a non-negative finite number`},
		{"duplicate after trim", `{"prices":{"m":{"input":1}," m ":{"input":2}}}`, `duplicate model "m" after trimming`},
		{"too many entries", string(tooManyBody), "prices must not exceed 500 entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveUsageTracker(t, http.MethodPut, "/usage/prices", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			message, _ := decodeUsageJSON(t, rec)["error"].(string)
			matches := message == tc.message
			// Decoder errors come from encoding/json, so only their prefix is ours.
			if strings.HasSuffix(tc.message, ": ") {
				matches = strings.HasPrefix(message, tc.message)
			}
			if !matches {
				t.Fatalf("error = %q, want %q", message, tc.message)
			}
		})
	}
	oversized := `{"prices":{"` + strings.Repeat("m", usageTrackerMaxPriceBody) + `":{}}}`
	if rec := serveUsageTracker(t, http.MethodPut, "/usage/prices", oversized); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status=%d body=%.200s", rec.Code, rec.Body.String())
	}
	if got := prices.Get(); !reflect.DeepEqual(got, seed) {
		t.Fatalf("rejected writes changed prices: %v", got)
	}

	delete(tooMany, "model-0")
	maxBody, errMarshal := json.Marshal(map[string]any{"prices": tooMany})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if rec := serveUsageTracker(t, http.MethodPut, "/usage/prices", string(maxBody)); rec.Code != http.StatusOK {
		t.Fatalf("%d entries: status=%d body=%.200s", usageTrackerMaxPrices, rec.Code, rec.Body.String())
	}
	if got := len(prices.Get()); got != usageTrackerMaxPrices {
		t.Fatalf("stored entries = %d, want %d", got, usageTrackerMaxPrices)
	}
}

func TestPutUsagePricesReportsSaveFailure(t *testing.T) {
	withUsageTracker(t, &fakeUsageStore{})
	SetUsageTrackerPrices(&failingPricesStore{})
	rec := serveUsageTracker(t, http.MethodPut, "/usage/prices", `{"prices":{"m":{"input":1}}}`)
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":"failed to save prices"}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
