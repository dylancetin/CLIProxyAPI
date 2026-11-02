package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	log "github.com/sirupsen/logrus"
)

const (
	usageTrackerDefaultRange   = 24 * time.Hour
	usageTrackerMaxRange       = 400 * 24 * time.Hour
	usageTrackerDefaultLimit   = 20
	usageTrackerMaxLimit       = 100
	usageTrackerBreakdownLimit = 100
	usageTrackerFilterPrefix   = "filter."
)

// usageTrackerDimensions lists every Dimension in response order.
var usageTrackerDimensions = []usagestore.Dimension{
	usagestore.DimProvider,
	usagestore.DimModel,
	usagestore.DimRequestedModel,
	usagestore.DimAccount,
	usagestore.DimAPIKey,
	usagestore.DimReasoningEffort,
	usagestore.DimServiceTier,
	usagestore.DimStream,
}

// usageTrackerNow is the clock for default query ranges.
var usageTrackerNow = time.Now

// usageSeriesStore is the query surface of *usagestore.Store.
type usageSeriesStore interface {
	QuerySeries(ctx context.Context, q usagestore.Query, prices usagestore.Prices) (usagestore.SeriesResult, error)
	QueryBreakdown(ctx context.Context, from, to time.Time, dim usagestore.Dimension, filters map[usagestore.Dimension][]string, prices usagestore.Prices) ([]usagestore.BreakdownRow, error)
	QueryDimensionValues(ctx context.Context, from, to time.Time) (map[usagestore.Dimension][]string, error)
}

// pricesStore is the source of model prices for usage queries and the prices endpoints.
type pricesStore interface {
	Get() usagestore.Prices
	Put(usagestore.Prices) error
}

type usageTokensJSON struct {
	InputUncached   int64 `json:"input_uncached"`
	InputCacheRead  int64 `json:"input_cache_read"`
	InputCacheWrite int64 `json:"input_cache_write"`
	OutputReasoning int64 `json:"output_reasoning"`
	OutputOther     int64 `json:"output_other"`
	Unclassified    int64 `json:"unclassified"`
	Total           int64 `json:"total"`
}

type usagePointJSON struct {
	Requests     int64           `json:"requests"`
	Failures     int64           `json:"failures"`
	Tokens       usageTokensJSON `json:"tokens"`
	LatencyMsSum int64           `json:"latency_ms_sum"`
	LatencyMsMax int64           `json:"latency_ms_max"`
	TTFTMsSum    int64           `json:"ttft_ms_sum"`
	TTFTMsMax    int64           `json:"ttft_ms_max"`
	TTFTCount    int64           `json:"ttft_count"`
	CostUSD      float64         `json:"cost_usd"`
}

type usageSeriesJSON struct {
	Key    map[usagestore.Dimension]string `json:"key"`
	Points []usagePointJSON                `json:"points"`
	Total  usagePointJSON                  `json:"total"`
}

type usageSeriesResponse struct {
	From    string            `json:"from"`
	To      string            `json:"to"`
	Bucket  usagestore.Bucket `json:"bucket"`
	Buckets []string          `json:"buckets"`
	Series  []usageSeriesJSON `json:"series"`
	Total   usagePointJSON    `json:"total"`
}

type usageBreakdownRowJSON struct {
	Value string `json:"value"`
	// Hint is set only for api_key rows.
	Hint  *string        `json:"hint,omitempty"`
	Point usagePointJSON `json:"point"`
}

type usageSummaryResponse struct {
	Total usagePointJSON                                   `json:"total"`
	By    map[usagestore.Dimension][]usageBreakdownRowJSON `json:"by"`
}

type usageAPIKeyValueJSON struct {
	Value string `json:"value"`
	Hint  string `json:"hint"`
}

// GetUsageSeries returns bucketed usage series.
func (h *Handler) GetUsageSeries(c *gin.Context) {
	store, prices, ok := h.requireUsageTracker(c)
	if !ok {
		return
	}
	q, errQuery := parseUsageSeriesQuery(c)
	if errQuery != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errQuery.Error()})
		return
	}
	result, errSeries := store.QuerySeries(c.Request.Context(), q, prices.Get())
	if errSeries != nil {
		usageQueryFailed(c, "series", errSeries)
		return
	}
	c.JSON(http.StatusOK, buildUsageSeriesResponse(q, result))
}

// GetUsageSummary returns the range total and per-dimension breakdowns.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	store, source, ok := h.requireUsageTracker(c)
	if !ok {
		return
	}
	from, to, errRange := parseUsageRange(c)
	if errRange != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRange.Error()})
		return
	}
	filters, errFilters := parseUsageFilters(c.Request.URL.Query())
	if errFilters != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errFilters.Error()})
		return
	}
	prices := source.Get()
	var total usagestore.Point
	by := make(map[usagestore.Dimension][]usageBreakdownRowJSON, len(usageTrackerDimensions))
	for _, dim := range usageTrackerDimensions {
		rows, errRows := store.QueryBreakdown(c.Request.Context(), from, to, dim, filters, prices)
		if errRows != nil {
			usageQueryFailed(c, "summary", errRows)
			return
		}
		// Every event has exactly one provider value, so provider rows partition
		// the range and their uncapped sum is the total.
		if dim == usagestore.DimProvider {
			for _, row := range rows {
				total = addUsagePoint(total, row.Point)
			}
		}
		if len(rows) > usageTrackerBreakdownLimit {
			rows = rows[:usageTrackerBreakdownLimit]
		}
		out := make([]usageBreakdownRowJSON, 0, len(rows))
		for _, row := range rows {
			item := usageBreakdownRowJSON{Value: row.Value, Point: usagePointToJSON(row.Point)}
			if dim == usagestore.DimAPIKey {
				hint := row.Hint
				item.Hint = &hint
			}
			out = append(out, item)
		}
		by[dim] = out
	}
	c.JSON(http.StatusOK, usageSummaryResponse{Total: usagePointToJSON(total), By: by})
}

// GetUsageDimensions returns distinct values per dimension for filter pickers.
func (h *Handler) GetUsageDimensions(c *gin.Context) {
	store, _, ok := h.requireUsageTracker(c)
	if !ok {
		return
	}
	from, to, errRange := parseUsageRange(c)
	if errRange != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errRange.Error()})
		return
	}
	ctx := c.Request.Context()
	values, errValues := store.QueryDimensionValues(ctx, from, to)
	if errValues != nil {
		usageQueryFailed(c, "dimensions", errValues)
		return
	}
	out := make(map[usagestore.Dimension]any, len(usageTrackerDimensions))
	for _, dim := range usageTrackerDimensions {
		if dim != usagestore.DimAPIKey {
			list := values[dim]
			if list == nil {
				list = []string{}
			}
			out[dim] = list
		}
	}
	keys := values[usagestore.DimAPIKey]
	hints := make(map[string]string, len(keys))
	if len(keys) > 0 {
		rows, errRows := store.QueryBreakdown(ctx, from, to, usagestore.DimAPIKey, nil, nil)
		if errRows != nil {
			usageQueryFailed(c, "dimensions", errRows)
			return
		}
		for _, row := range rows {
			hints[row.Value] = row.Hint
		}
	}
	apiKeys := make([]usageAPIKeyValueJSON, 0, len(keys))
	for _, key := range keys {
		apiKeys = append(apiKeys, usageAPIKeyValueJSON{Value: key, Hint: hints[key]})
	}
	out[usagestore.DimAPIKey] = apiKeys
	c.JSON(http.StatusOK, out)
}

// requireUsageTracker returns the store and price source, or answers 503 when
// the tracker is disabled.
func (h *Handler) requireUsageTracker(c *gin.Context) (usageSeriesStore, pricesStore, bool) {
	store, prices := h.usageTrackerState()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage tracker disabled"})
		return nil, nil, false
	}
	return store, prices, true
}

func usageQueryFailed(c *gin.Context, query string, err error) {
	log.WithError(err).WithField("query", query).Error("management: usage tracker query failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "usage query failed"})
}

func parseUsageSeriesQuery(c *gin.Context) (usagestore.Query, error) {
	from, to, errRange := parseUsageRange(c)
	if errRange != nil {
		return usagestore.Query{}, errRange
	}
	bucket, errBucket := parseUsageBucket(c.Query("bucket"))
	if errBucket != nil {
		return usagestore.Query{}, errBucket
	}
	group, errGroup := parseUsageGroup(c.Query("group"))
	if errGroup != nil {
		return usagestore.Query{}, errGroup
	}
	filters, errFilters := parseUsageFilters(c.Request.URL.Query())
	if errFilters != nil {
		return usagestore.Query{}, errFilters
	}
	limit, errLimit := parseUsageLimit(c.Query("limit"))
	if errLimit != nil {
		return usagestore.Query{}, errLimit
	}
	return usagestore.Query{From: from, To: to, Bucket: bucket, GroupBy: group, Filters: filters, Limit: limit}, nil
}

// parseUsageRange defaults to = now and from = to - 24h.
func parseUsageRange(c *gin.Context) (time.Time, time.Time, error) {
	to := usageTrackerNow().UTC().Truncate(time.Second)
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		parsed, errParse := parseUsageTime(raw)
		if errParse != nil {
			return time.Time{}, time.Time{}, errors.New("to must be RFC3339 or unix seconds")
		}
		to = parsed
	}
	from := to.Add(-usageTrackerDefaultRange)
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		parsed, errParse := parseUsageTime(raw)
		if errParse != nil {
			return time.Time{}, time.Time{}, errors.New("from must be RFC3339 or unix seconds")
		}
		from = parsed
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	if to.Sub(from) > usageTrackerMaxRange {
		return time.Time{}, time.Time{}, errors.New("range must not exceed 400 days")
	}
	return from, to, nil
}

func parseUsageTime(raw string) (time.Time, error) {
	var parsed time.Time
	if seconds, errInt := strconv.ParseInt(raw, 10, 64); errInt == nil {
		parsed = time.Unix(seconds, 0)
	} else {
		var errParse error
		if parsed, errParse = time.Parse(time.RFC3339, raw); errParse != nil {
			return time.Time{}, errParse
		}
	}
	parsed = parsed.UTC()
	// Responses echo times as RFC3339, which cannot represent other years.
	if year := parsed.Year(); year < 1 || year > 9999 {
		return time.Time{}, errors.New("year out of range")
	}
	return parsed, nil
}

func parseUsageBucket(raw string) (usagestore.Bucket, error) {
	switch bucket := usagestore.Bucket(strings.ToLower(strings.TrimSpace(raw))); bucket {
	case "":
		return usagestore.BucketHour, nil
	case usagestore.BucketHour, usagestore.BucketDay:
		return bucket, nil
	default:
		return "", errors.New("bucket must be hour or day")
	}
}

func parseUsageDimension(raw string) (usagestore.Dimension, bool) {
	dim := usagestore.Dimension(strings.ToLower(strings.TrimSpace(raw)))
	return dim, slices.Contains(usageTrackerDimensions, dim)
}

func parseUsageGroup(raw string) ([]usagestore.Dimension, error) {
	var group []usagestore.Dimension
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		dim, ok := parseUsageDimension(part)
		if !ok {
			return nil, fmt.Errorf("unknown group dimension %q", strings.TrimSpace(part))
		}
		if !slices.Contains(group, dim) {
			group = append(group, dim)
		}
	}
	return group, nil
}

// parseUsageFilters reads filter.<dimension> keys, each repeated or a comma list.
func parseUsageFilters(query url.Values) (map[usagestore.Dimension][]string, error) {
	filters := make(map[usagestore.Dimension][]string)
	for key, values := range query {
		name, ok := strings.CutPrefix(key, usageTrackerFilterPrefix)
		if !ok {
			continue
		}
		dim, ok := parseUsageDimension(name)
		if !ok {
			return nil, fmt.Errorf("unknown filter dimension %q", name)
		}
		for _, value := range values {
			for _, part := range strings.Split(value, ",") {
				part = strings.TrimSpace(part)
				if part != "" && !slices.Contains(filters[dim], part) {
					filters[dim] = append(filters[dim], part)
				}
			}
		}
	}
	return filters, nil
}

func parseUsageLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return usageTrackerDefaultLimit, nil
	}
	limit, errLimit := strconv.Atoi(raw)
	if errLimit != nil || limit <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	return min(limit, usageTrackerMaxLimit), nil
}

func buildUsageSeriesResponse(q usagestore.Query, result usagestore.SeriesResult) usageSeriesResponse {
	resp := usageSeriesResponse{
		From:    formatUsageTime(q.From),
		To:      formatUsageTime(q.To),
		Bucket:  q.Bucket,
		Buckets: make([]string, 0, len(result.Buckets)),
		Series:  make([]usageSeriesJSON, 0, len(result.Series)),
		Total:   usagePointToJSON(result.Total),
	}
	for _, bucket := range result.Buckets {
		resp.Buckets = append(resp.Buckets, formatUsageTime(bucket))
	}
	for _, series := range result.Series {
		key := make(map[usagestore.Dimension]string, len(series.Key))
		maps.Copy(key, series.Key)
		points := make([]usagePointJSON, 0, len(series.Points))
		for _, point := range series.Points {
			points = append(points, usagePointToJSON(point))
		}
		resp.Series = append(resp.Series, usageSeriesJSON{Key: key, Points: points, Total: usagePointToJSON(series.Total)})
	}
	return resp
}

func formatUsageTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func usagePointToJSON(p usagestore.Point) usagePointJSON {
	return usagePointJSON{
		Requests: p.Requests,
		Failures: p.Failures,
		Tokens: usageTokensJSON{
			InputUncached:   p.Tokens.InputUncached,
			InputCacheRead:  p.Tokens.InputCacheRead,
			InputCacheWrite: p.Tokens.InputCacheWrite,
			OutputReasoning: p.Tokens.OutputReasoning,
			OutputOther:     p.Tokens.OutputOther,
			Unclassified:    p.Tokens.Unclassified,
			Total:           p.Tokens.Total,
		},
		LatencyMsSum: p.LatencyMsSum,
		LatencyMsMax: p.LatencyMsMax,
		TTFTMsSum:    p.TTFTMsSum,
		TTFTMsMax:    p.TTFTMsMax,
		TTFTCount:    p.TTFTCount,
		CostUSD:      p.CostUSD,
	}
}

func addUsagePoint(a, b usagestore.Point) usagestore.Point {
	a.Requests += b.Requests
	a.Failures += b.Failures
	a.Tokens.InputUncached += b.Tokens.InputUncached
	a.Tokens.InputCacheRead += b.Tokens.InputCacheRead
	a.Tokens.InputCacheWrite += b.Tokens.InputCacheWrite
	a.Tokens.OutputReasoning += b.Tokens.OutputReasoning
	a.Tokens.OutputOther += b.Tokens.OutputOther
	a.Tokens.Unclassified += b.Tokens.Unclassified
	a.Tokens.Total += b.Tokens.Total
	a.LatencyMsSum += b.LatencyMsSum
	a.LatencyMsMax = max(a.LatencyMsMax, b.LatencyMsMax)
	a.TTFTMsSum += b.TTFTMsSum
	a.TTFTMsMax = max(a.TTFTMsMax, b.TTFTMsMax)
	a.TTFTCount += b.TTFTCount
	a.CostUSD += b.CostUSD
	return a
}

const (
	usageTrackerMaxPrices    = 500
	usageTrackerMaxPriceBody = 1 << 20
)

type usagePricesResponse struct {
	Prices usagestore.Prices `json:"prices"`
}

type usagePricesRequest struct {
	Prices map[string]*usagestore.ModelPrice `json:"prices"`
}

// GetUsagePrices returns the model prices used for cost_usd.
func (h *Handler) GetUsagePrices(c *gin.Context) {
	_, source, ok := h.requireUsageTracker(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, usagePricesResponse{Prices: nonNilUsagePrices(source.Get())})
}

// PutUsagePrices replaces the whole price map and returns the stored map.
func (h *Handler) PutUsagePrices(c *gin.Context) {
	_, source, ok := h.requireUsageTracker(c)
	if !ok {
		return
	}
	prices, errDecode := decodeUsagePrices(c)
	if errDecode != nil {
		status := http.StatusBadRequest
		if _, tooLarge := errors.AsType[*http.MaxBytesError](errDecode); tooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		c.JSON(status, gin.H{"error": errDecode.Error()})
		return
	}
	if errPut := source.Put(prices); errPut != nil {
		log.WithError(errPut).Error("management: failed to save usage tracker prices")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save prices"})
		return
	}
	c.JSON(http.StatusOK, usagePricesResponse{Prices: nonNilUsagePrices(source.Get())})
}

func decodeUsagePrices(c *gin.Context) (usagestore.Prices, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, usageTrackerMaxPriceBody))
	// Rejects misspelled price fields such as the YAML-style "cache-read",
	// which would otherwise silently price at zero.
	decoder.DisallowUnknownFields()
	var body usagePricesRequest
	if errDecode := decoder.Decode(&body); errDecode != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](errDecode); tooLarge {
			return nil, errDecode
		}
		return nil, fmt.Errorf("invalid body: %v", errDecode)
	}
	if errTrailing := decoder.Decode(&json.RawMessage{}); errTrailing != io.EOF {
		return nil, errors.New("invalid body: unexpected data after JSON object")
	}
	if body.Prices == nil {
		return nil, errors.New("prices must be an object")
	}
	if len(body.Prices) > usageTrackerMaxPrices {
		return nil, fmt.Errorf("prices must not exceed %d entries", usageTrackerMaxPrices)
	}
	prices := make(usagestore.Prices, len(body.Prices))
	for rawModel, price := range body.Prices {
		model := strings.TrimSpace(rawModel)
		if model == "" {
			return nil, errors.New("model name must not be empty")
		}
		if price == nil {
			return nil, fmt.Errorf("price for %q must be an object", model)
		}
		if _, duplicate := prices[model]; duplicate {
			return nil, fmt.Errorf("duplicate model %q after trimming", model)
		}
		for _, field := range []struct {
			name  string
			value float64
		}{
			{"input", price.Input},
			{"cache_read", price.CacheRead},
			{"cache_write", price.CacheWrite},
			{"output", price.Output},
		} {
			if field.value < 0 || math.IsNaN(field.value) || math.IsInf(field.value, 0) {
				return nil, fmt.Errorf("%s for %q must be a non-negative finite number", field.name, model)
			}
		}
		prices[model] = *price
	}
	return prices, nil
}

func nonNilUsagePrices(prices usagestore.Prices) usagestore.Prices {
	if prices == nil {
		return usagestore.Prices{}
	}
	return prices
}
