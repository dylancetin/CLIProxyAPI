// Package usagestore persists per-attempt usage events in SQLite and serves
// bucketed series and breakdowns over them.
package usagestore

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/pricing"
)

// Dimension is a groupable, filterable column of an Event.
type Dimension string

const (
	// DimProvider groups by executor provider identifier.
	DimProvider Dimension = "provider"
	// DimModel groups by the upstream model actually used.
	DimModel Dimension = "model"
	// DimRequestedModel groups by the model or alias the client asked for.
	DimRequestedModel Dimension = "requested_model"
	// DimAccount groups by upstream credential (AuthID).
	DimAccount Dimension = "account"
	// DimAPIKey groups by client API key, stored hashed and shown as a hint.
	DimAPIKey Dimension = "api_key"
	// DimReasoningEffort groups by translated upstream reasoning level.
	DimReasoningEffort Dimension = "reasoning_effort"
	// DimServiceTier groups by service tier.
	DimServiceTier Dimension = "service_tier"
	// DimStream groups by delivery mode: "stream" or "sync".
	DimStream Dimension = "stream"
)

// dimensionColumns is the single registry mapping Dimension to its SQL column.
// Add a dimension here and in the schema; nothing else branches on Dimension.
var dimensionColumns = map[Dimension]string{
	DimProvider:        "provider",
	DimModel:           "model",
	DimRequestedModel:  "requested_model",
	DimAccount:         "account",
	DimAPIKey:          "api_key_hash",
	DimReasoningEffort: "reasoning_effort",
	DimServiceTier:     "service_tier",
	DimStream:          "stream",
}

// Bucket is the time granularity of a series. Buckets are aligned in UTC.
type Bucket string

const (
	// BucketHour aligns points to UTC hours.
	BucketHour Bucket = "hour"
	// BucketDay aligns points to UTC days.
	BucketDay Bucket = "day"
)

// Tokens holds non-overlapping token counts for one attempt or an aggregate.
type Tokens = pricing.Tokens

// ModelPrice is the USD price per 1,000,000 tokens for one model.
type ModelPrice = pricing.ModelPrice

// Prices maps a model id to its price; exact match first, then key "default".
type Prices = pricing.Prices

// FailureClass buckets a failed attempt by cause. It is empty for success.
type FailureClass string

const (
	// FailureNone marks a successful attempt.
	FailureNone FailureClass = ""
	// FailureClient4xx is a 4xx other than 401, 403, and 429.
	FailureClient4xx FailureClass = "client_4xx"
	// FailureRateLimit429 is an upstream 429.
	FailureRateLimit429 FailureClass = "rate_limit_429"
	// FailureAuth401403 is an upstream 401 or 403.
	FailureAuth401403 FailureClass = "auth_401_403"
	// FailureServer5xx is an upstream 5xx.
	FailureServer5xx FailureClass = "server_5xx"
	// FailureNetwork is a failure without an HTTP status.
	FailureNetwork FailureClass = "network"
	// FailureOther is a failure with a non-error status code.
	FailureOther FailureClass = "other"
)

// Event is one upstream attempt, parsed once from a usage.Record.
type Event struct {
	// RequestedAt is in UTC, zero when the record had no timestamp.
	RequestedAt time.Time
	// TraceID is the inbound request id, shared across retries.
	TraceID string
	// RequestID is unique per attempt.
	RequestID    string
	Provider     string
	ExecutorType string
	// Model is the upstream-reported model when known, else the routed model.
	Model string
	// RequestedModel is the client alias when set, else the routed model.
	RequestedModel string
	// Account is the AuthID, or "" when the executor had no auth.
	Account  string
	AuthType string
	// APIKeyHash is the hex SHA-256 of the client key, "" when absent.
	APIKeyHash string
	// APIKeyHint is the masked client key, "" when absent.
	APIKeyHint      string
	AccessProvider  string
	ReasoningEffort string
	ServiceTier     string
	Stream          bool
	Failed          bool
	StatusCode      int
	FailureClass    FailureClass
	Latency         time.Duration
	// TTFT is zero when unknown.
	TTFT   time.Duration
	Tokens Tokens
	// Quality is the usage.TokenAccountingQuality of Tokens.
	Quality string
}

// Options configures Open.
type Options struct {
	// Path is the SQLite file path.
	Path string
	// RawRetention bounds usage_events age; 0 keeps rows forever.
	RawRetention time.Duration
	// HourlyRetention bounds usage_hourly age; 0 keeps rows forever.
	HourlyRetention time.Duration
	// Now overrides the clock; nil uses time.Now.
	Now func() time.Time
}

// Query selects a bucketed series over [From, To).
type Query struct {
	From, To time.Time
	Bucket   Bucket
	// GroupBy splits the result into one series per value combination; empty yields one "total" series.
	GroupBy []Dimension
	// Filters match any listed value within a dimension and every listed dimension.
	Filters map[Dimension][]string
	// Limit caps series by total tokens and folds the rest into key "other"; 0 means 20.
	Limit int
}

// SeriesKey holds one value per GroupBy dimension.
type SeriesKey map[Dimension]string

// Point aggregates attempts in one bucket, or across buckets for totals.
type Point struct {
	BucketStart time.Time
	// Requests counts attempts.
	Requests     int64
	Failures     int64
	Tokens       Tokens
	LatencyMsSum int64
	LatencyMsMax int64
	TTFTMsSum    int64
	TTFTMsMax    int64
	// TTFTCount counts attempts with a known TTFT.
	TTFTCount int64
	// CostUSD is computed from Prices at query time; 0 when no price applies.
	CostUSD float64
}

// Series is one grouped time series.
type Series struct {
	Key SeriesKey
	// Points has one entry per bucket in [From, To), zero-filled.
	Points []Point
	// Total sums Points; max fields hold maxima.
	Total Point
}

// SeriesResult is the response of QuerySeries.
type SeriesResult struct {
	Buckets []time.Time
	Series  []Series
	Total   Point
}

// BreakdownRow is the total for one value of a dimension.
type BreakdownRow struct {
	Value string
	// Hint is the masked key for DimAPIKey rows, "" otherwise.
	Hint  string
	Point Point
}
