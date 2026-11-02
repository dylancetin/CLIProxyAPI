# SQLite usage tracker: design contract

This file is the shared contract between parallel implementers. Change it only through the coordinator. Companion research: `research-go.md` (Go pipeline) and `research-panel.md` (CPAMC panel and overlay). Read AGENTS.md in the repo root for code conventions.

## Done predicate

A server with the tracker enabled and a fake upstream receives a scripted mix of requests across several providers, models, upstream accounts, client API keys, streaming and non-streaming, some failing. Then:

1. `usage_events` holds exactly one row per upstream attempt with the dimensions of that attempt.
2. `usage_hourly` rows, summed, equal the raw rows for requests, failures, every token bucket, latency sum and max.
3. `GET /v8/management/observability/usage/series?bucket=hour&group=provider` sums match the scripted totals, and `group=model,account,api_key` splits match.
4. After `PUT .../usage/prices`, series and summary carry `cost_usd` equal to tokens times the stored prices, and the price edit survives a config reload.
5. The served CPAMC v1.27.2 panel shows a Usage nav item in the observe group. The page renders charts from the series endpoint. Group-by, metric, range, bucket, filter, and chart-type controls each change the rendering. Screenshot evidence in `.audit/evidence/`.
6. `go build ./...` with `CGO_ENABLED=0` and with cgo both pass. `go test ./...` passes. `gofmt -l` is empty. `internal/redisqueue` and `internal/translator` have no diff.
7. With the tracker disabled, no database file is created and no plugin is registered. In Home mode the tracker is forced off.

## Domain model (Go, package `internal/usagestore`)

Types first, per type-system discipline. Parse `usage.Record` into `Event` once at the boundary. Everything downstream trusts `Event`.

```go
package usagestore

// Dimension is a groupable, filterable column of an Event.
type Dimension string

const (
    DimProvider        Dimension = "provider"
    DimModel           Dimension = "model"           // upstream model actually used
    DimRequestedModel  Dimension = "requested_model" // alias the client asked for
    DimAccount         Dimension = "account"         // upstream credential: AuthID
    DimAPIKey          Dimension = "api_key"         // client key, stored hashed, shown as hint
    DimReasoningEffort Dimension = "reasoning_effort"
    DimServiceTier     Dimension = "service_tier"
    DimStream          Dimension = "stream"          // "stream" | "sync"
)

// dimensionColumns is the single registry mapping Dimension to its SQL column(s).
// Add a dimension here and in the schema; nothing else branches on Dimension.
var dimensionColumns = map[Dimension]string{ ... }

type Bucket string
const (
    BucketHour Bucket = "hour"
    BucketDay  Bucket = "day"
)

type Tokens struct {
    InputUncached   int64
    InputCacheRead  int64
    InputCacheWrite int64
    OutputReasoning int64
    OutputOther     int64 // non-reasoning output
    Unclassified    int64
    Total           int64
}

type FailureClass string // "" for success; "client_4xx", "rate_limit_429", "auth_401_403", "server_5xx", "network", "other"

// Event is one upstream attempt.
type Event struct {
    RequestedAt     time.Time // UTC
    TraceID         string    // inbound request id; shared across retries
    RequestID       string    // one per attempt
    Provider        string
    ExecutorType    string
    Model           string    // ResponseModel if set, else Model
    RequestedModel  string    // Alias if set, else Model
    Account         string    // AuthID, or "" when the executor had no auth (plugin executors)
    AuthType        string
    APIKeyHash      string    // hex sha256 of the raw client key, "" when absent
    APIKeyHint      string    // masked: first 4 + "…" + last 4, or "" when absent
    AccessProvider  string
    ReasoningEffort string
    ServiceTier     string
    Stream          bool
    Failed          bool
    StatusCode      int
    FailureClass    FailureClass
    Latency         time.Duration
    TTFT            time.Duration // zero when unknown
    Tokens          Tokens
    Quality         string // usage.TokenAccountingQuality as string
}

// EventFromRecord parses a usage.Record at the boundary.
// Never store Record.Source (can be a raw upstream API key) or Record.APIKey raw.
func EventFromRecord(r usage.Record) Event

type Options struct {
    Path            string        // sqlite file path
    RawRetention    time.Duration // 0 = keep forever
    HourlyRetention time.Duration // 0 = keep forever
    Now             func() time.Time
}

type Store struct{ ... }

func Open(ctx context.Context, opts Options) (*Store, error) // opens or creates, runs migrations, sets WAL + busy_timeout
func (s *Store) Close() error                                 // flushes pending events, closes the db
func (s *Store) Plugin() usage.Plugin                         // batched writer; HandleUsage must return fast (enqueue only)
func (s *Store) Flush(ctx context.Context) error             // drains the batch queue; tests use this
func (s *Store) SetRetention(raw, hourly time.Duration)      // hot reload
func (s *Store) Sweep(ctx context.Context) error             // applies retention; also runs periodically

type Query struct {
    From, To time.Time
    Bucket   Bucket
    GroupBy  []Dimension          // empty = one series "total"
    Filters  map[Dimension][]string // OR within a dimension, AND across
    Limit    int                  // max series by total tokens, rest folded into key "other"; 0 = default 20
}

type SeriesKey map[Dimension]string // one value per GroupBy dimension

type Point struct {
    BucketStart time.Time
    Requests    int64 // attempts
    Failures    int64
    Tokens      Tokens
    LatencyMsSum, LatencyMsMax  int64
    TTFTMsSum, TTFTMsMax        int64
    TTFTCount                   int64 // attempts with a known TTFT
    CostUSD     float64 // computed in Go from Prices at query time; 0 when no price
}

type Series struct {
    Key    SeriesKey
    Points []Point // one per bucket in [From, To), zero-filled
    Total  Point   // sums across points (max fields are maxes)
}

type SeriesResult struct {
    Buckets []time.Time
    Series  []Series
    Total   Point
}

func (s *Store) QuerySeries(ctx context.Context, q Query, prices Prices) (SeriesResult, error)

type BreakdownRow struct {
    Value string
    Hint  string // for api_key: the hint; otherwise ""
    Point Point
}

// QueryBreakdown returns totals per distinct value of dim within [From, To), sorted by total tokens desc.
func (s *Store) QueryBreakdown(ctx context.Context, from, to time.Time, dim Dimension, filters map[Dimension][]string, prices Prices) ([]BreakdownRow, error)

// QueryDimensionValues returns distinct values per dimension within [From, To) for filter pickers.
func (s *Store) QueryDimensionValues(ctx context.Context, from, to time.Time) (map[Dimension][]string, error)

// Prices: USD per 1,000,000 tokens. Reasoning output is priced at Output.
type ModelPrice struct {
    Input      float64 `yaml:"input" json:"input"`
    CacheRead  float64 `yaml:"cache-read" json:"cache_read"`
    CacheWrite float64 `yaml:"cache-write" json:"cache_write"`
    Output     float64 `yaml:"output" json:"output"`
}
type Prices map[string]ModelPrice // key: model id; exact match first, then key "default" if present

func (p Prices) Cost(model string, t Tokens) float64
```

Hour bucketing is UTC. Day bucketing is UTC too. The UI converts for display.

## Schema (SQLite, pure Go driver `modernc.org/sqlite`)

Pragmas on open: `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`, `foreign_keys=ON`. One writer connection (`db.SetMaxOpenConns(1)` for the write handle is acceptable), reads may use a second `*sql.DB` or the same.

```sql
CREATE TABLE IF NOT EXISTS usage_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- schema_version = 1

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
```

Writes: one transaction per batch. For each event, `INSERT` into `usage_events` and `INSERT ... ON CONFLICT DO UPDATE` into `usage_hourly` (sums add, maxes `MAX()`, `ttft_count` adds 1 only when `ttft_ms > 0`). Queries read `usage_hourly` only. Day bucket groups `hour_start / 86400`.

Retention: `Sweep` deletes `usage_events` older than RawRetention and `usage_hourly` older than HourlyRetention, when each is non-zero. Run on open and then hourly from a goroutine stopped by `Close`.

Writer plugin: `HandleUsage(ctx, record)` converts to Event and sends on a buffered channel (capacity 4096; on a full channel drop and count a dropped metric with a rate-limited warning). A goroutine flushes when 256 events are queued or every second via a ticker, whichever first. The usage manager dispatches plugins serially from one goroutine, so `HandleUsage` must never block on SQLite.

## Config (package `internal/config`)

Flat field on `Config`:

```go
UsageTracker UsageTrackerConfig `yaml:"usage-tracker" json:"usage-tracker"`

type UsageTrackerConfig struct {
    Enabled             bool                           `yaml:"enabled" json:"enabled"`
    SQLitePath          string                         `yaml:"sqlite-path" json:"sqlite-path"`                   // "" = <auth-dir>/usage.sqlite
    RawRetentionDays    int                            `yaml:"raw-retention-days" json:"raw-retention-days"`       // default 30, 0 = forever
    HourlyRetentionDays int                            `yaml:"hourly-retention-days" json:"hourly-retention-days"` // default 400, 0 = forever
    Prices              map[string]usagestore.ModelPrice `yaml:"prices" json:"prices"`
}
```

Avoid an import cycle: if `internal/config` must not import `internal/usagestore`, define `ModelPrice` in `internal/config` and have `usagestore.Prices` be `map[string]config.ModelPrice`, or define a tiny leaf package `internal/usagestore/pricing`. The config implementer decides and records the choice in `.audit/sqlite-usage-tracker.tsv`.

v8 YAML layout: `observability.usage.tracker` (add a `buildV8Paths` entry `{"usage-tracker", "observability.usage.tracker"}`). Defaults applied in both `config_load.go` and `parse.go`: `Enabled: true`, `RawRetentionDays: 30`, `HourlyRetentionDays: 400`. Document in `config.example.yaml` inside the `observability.usage:` block with a short example price entry. `TestV8ExampleLoadsAndRoundTrips` must stay green.

Example:

```yaml
observability:
  usage:
    tracker:
      enabled: true
      sqlite-path: ""
      raw-retention-days: 30
      hourly-retention-days: 400
      prices:
        claude-opus-5-5:
          input: 15
          cache-read: 1.5
          cache-write: 18.75
          output: 75
```

Wiring: at startup (`cmd/server/main.go` near the usage toggle at `:631`, or the SDK service lifecycle, whichever the Go research shows owns plugin registration) open the store when `Enabled && !HomeMode`, register `store.Plugin()` under the name `"sqlite-usage"`, hand the store to the management handler via a setter (`SetUsageStore`), and close it on shutdown. Hot reload (`internal/api/server_reload.go` and the watcher diff) handles enable/disable, retention, and price changes without restart. A path change requires restart; log a warning.

## Management API (v8 only, package `internal/api/handlers/management`, file `usage_tracker.go`)

Register in `internal/api/server_management_v8.go` and add to `TestManagementV8IndependentContract`. All under `/v8/management/observability/usage/`. Existing `api-keys` and `queue` stay as they are. When the store is nil (tracker disabled) every endpoint returns `503 {"error":"usage tracker disabled"}`.

`GET series`
Query: `from`, `to` (RFC3339 or unix seconds; default to = now, from = to - 24h), `bucket` (`hour` default, `day`), `group` (comma list of Dimension; default none), `filter.<dimension>` repeated or comma list (e.g. `filter.provider=claude,gemini`), `limit` (default 20, max 100).
Response:

```json
{
  "from": "2026-11-01T00:00:00Z", "to": "2026-11-02T00:00:00Z", "bucket": "hour",
  "buckets": ["2026-11-01T00:00:00Z", "..."],
  "series": [
    {"key": {"provider": "claude"}, "points": [{"requests": 12, "failures": 1, "tokens": {"input_uncached": 0, "input_cache_read": 0, "input_cache_write": 0, "output_reasoning": 0, "output_other": 0, "unclassified": 0, "total": 0}, "latency_ms_sum": 0, "latency_ms_max": 0, "ttft_ms_sum": 0, "ttft_ms_max": 0, "ttft_count": 0, "cost_usd": 0.0}], "total": {"...same shape as a point without bucket"}}
  ],
  "total": {"...point shape"}
}
```
Points are dense (one per bucket, zeros when empty). `key` for the folded remainder is `{"<dim>": "other"}` for each grouped dimension.

`GET summary`
Query: `from`, `to`, `filter.*`. Response: `{"total": point, "by": {"provider": [{"value": "claude", "point": {...}}], "model": [...], "requested_model": [...], "account": [...], "api_key": [{"value": "<hash>", "hint": "sk-a…b2c3", "point": {...}}], "reasoning_effort": [...], "service_tier": [...], "stream": [...]}}`. Each list sorted by total tokens desc, capped at 100.

`GET dimensions`
Query: `from`, `to`. Response: `{"provider": ["claude", ...], "model": [...], ..., "api_key": [{"value": "<hash>", "hint": "..."}]}`.

`GET prices` -> `{"prices": {"claude-opus-5-5": {"input": 15, "cache_read": 1.5, "cache_write": 18.75, "output": 75}}}`
`PUT prices` body same shape, replaces the whole map, validates non-negative numbers, persists to the config file using the same save path the other v8 config PUT handlers use, and takes effect immediately for queries.

Never return raw client API keys or `Record.Source`.

## Overlay UI (package `internal/managementasset`, files `usage.js`, `usage.css`)

Follow `research-panel.md` section 5 exactly for injection, nav entry, routing, mount, API capture, and fail-closed behavior. The page is a single `section[data-cpa-owned][data-cpa-page-root]` built by plain DOM and inline SVG. No libraries, no CDN.

Layout, top to bottom:
1. Header row: title "Usage", range presets (1h, 6h, 24h, 7d, 30d, custom from/to inputs), bucket toggle (hour/day), refresh button, auto-refresh toggle (60s).
2. Stat tiles: requests, failures (with %), total tokens, input/output split, avg latency, avg TTFT, estimated cost. From `summary.total`.
3. Main chart card with "advanced controls" bar:
   - metric: requests, failures, tokens total, input tokens, output tokens, cache read, cache write, reasoning tokens, avg latency, avg TTFT, cost
   - group by: none, provider, model, requested model, account, API key, reasoning effort, service tier, stream (multi-select up to 2)
   - chart type: stacked bars, grouped bars, lines, stacked area
   - scale: linear / log
   - top N: 5, 10, 20
   - legend with per-series toggle and solo (double-click)
   - filters: chips per dimension from `dimensions`, applied to all queries
   - crosshair tooltip with per-series values and bucket total
   - "table" toggle that renders the same data as an accessible table
   - CSV export of the current series
4. Breakdown tables: tabs per dimension from `summary.by` with columns requests, failures, tokens (total, in, out, cached), avg latency, cost.
5. Prices editor: table of model -> four prices, add/remove rows, Save calls `PUT prices`; only visible when the server returns a price map.

Persist control state in `localStorage["cpa-usage-view"]` (JSON) and mirror the range in the hash query (`cpa-range=24h`), so reload keeps the view. Theme: use CPAMC variables from `research-panel.md` plus a `--cpa-series-1..8` palette per theme defined in `usage.css`, distinguishable in dark and light and for color-blind readers (use the dataviz skill at `~/.claude/skills/dataviz/SKILL.md` if present). Numbers formatted with compact units (12.3k, 4.5M). Times shown in the browser's local zone with the UTC offset in the axis title.

Error states: tracker disabled (503) shows an instruction to enable `observability.usage.tracker.enabled`; no key captured shows "sign in to the panel first"; network error shows retry.

Testing: pure helpers (hash parsing, storage decode, bucketing into dense series, scale math, SVG path builders, number formatting, CSV) live in a form the existing `node --test` vm harness can load, and get tests in `usage.test.mjs`. Go test in `appearance_test.go` for injection order and idempotency. DOM behavior verified with Playwright against a running server with `MANAGEMENT_STATIC_PATH` pointing at a copy of the v1.27.2 release and a seeded SQLite file; screenshots saved under `.audit/evidence/`.

## Workstreams and seams

| Stream | Owns (only these paths) | Depends on |
|---|---|---|
| S0 skeleton gate | `go.mod`, `go.sum`, `internal/usagestore/` (types, signatures, compiling stubs) | contract |
| S1 store | `internal/usagestore/**` | S0 |
| S2 config and wiring | `internal/config/**`, `config.example.yaml`, `cmd/server/main.go`, `sdk/cliproxy/service_*.go`, `internal/api/server_reload.go`, `internal/watcher/diff/**`, `internal/tui` if the toggle shows there, `AGENTS.md` fix for `internal/usage/` | S0 |
| S3 management API | `internal/api/handlers/management/usage_tracker*.go`, `internal/api/server_management_v8.go`, `server_management_v8_test.go`, handler setter in `handler.go` | S0 |
| S4 overlay UI | `internal/managementasset/usage.js`, `usage.css`, `usage.test.mjs`, `appearance.go` injection, `appearance_test.go` | contract only |
| S5 verification harness | `test/usage_tracker_integration_test.go`, `.audit/evidence/` | S1, S2, S3 |

Shared files are owned by exactly one stream. Each stream works on its own branch from the S0 commit and reports the branch name and head SHA. The coordinator merges.
