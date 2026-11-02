package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"gopkg.in/yaml.v3"
)

// s5Script mixes two providers, four models, two client keys, streaming and
// non-streaming, and upstream 429 and 500 failures. Claude has two
// credentials, so a failing claude request fails over across both.
var s5Script = []s5Request{
	{Provider: s5ClaudeProvider, Model: "s5-claude-opus", Key: 0, Stream: false, Status: http.StatusOK},
	{Provider: s5ClaudeProvider, Model: "s5-claude-opus", Key: 1, Stream: true, Status: http.StatusOK},
	{Provider: s5ClaudeProvider, Model: "s5-claude-haiku", Key: 0, Stream: true, Status: http.StatusOK},
	{Provider: s5ClaudeProvider, Model: "s5-claude-haiku", Key: 1, Stream: false, Status: http.StatusOK},
	{Provider: s5ClaudeProvider, Model: "s5-claude-opus", Key: 0, Stream: false, Status: http.StatusTooManyRequests},
	{Provider: s5ClaudeProvider, Model: "s5-claude-opus", Key: 0, Stream: true, Status: http.StatusOK},
	{Provider: s5ClaudeProvider, Model: "s5-claude-haiku", Key: 1, Stream: true, Status: http.StatusInternalServerError},
	{Provider: s5ClaudeProvider, Model: "s5-claude-opus", Key: 1, Stream: false, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-large", Key: 0, Stream: false, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-large", Key: 1, Stream: true, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-small", Key: 1, Stream: false, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-small", Key: 0, Stream: true, Status: http.StatusTooManyRequests},
	{Provider: s5CompatProvider, Model: "s5-compat-large", Key: 0, Stream: true, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-large", Key: 1, Stream: false, Status: http.StatusInternalServerError},
	{Provider: s5ClaudeProvider, Model: "s5-claude-haiku", Key: 0, Stream: false, Status: http.StatusOK},
	{Provider: s5CompatProvider, Model: "s5-compat-small", Key: 0, Stream: false, Status: http.StatusOK},
}

// s5ClaudeOpusPrice is the price PUT for one model, USD per 1M tokens.
var s5ClaudeOpusPrice = map[string]float64{"input": 3, "cache_read": 0.3, "cache_write": 3.75, "output": 15}

type s5PointJSON struct {
	Requests     int64    `json:"requests"`
	Failures     int64    `json:"failures"`
	Tokens       s5Tokens `json:"tokens"`
	LatencyMsSum int64    `json:"latency_ms_sum"`
	LatencyMsMax int64    `json:"latency_ms_max"`
	TTFTMsSum    int64    `json:"ttft_ms_sum"`
	TTFTMsMax    int64    `json:"ttft_ms_max"`
	TTFTCount    int64    `json:"ttft_count"`
	CostUSD      float64  `json:"cost_usd"`
}

func (p s5PointJSON) totals() s5Totals {
	return s5Totals{Requests: p.Requests, Failures: p.Failures, Tokens: p.Tokens}
}

type s5SeriesJSON struct {
	Key    map[string]string `json:"key"`
	Points []s5PointJSON     `json:"points"`
	Total  s5PointJSON       `json:"total"`
}

type s5SeriesResponse struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	Bucket  string         `json:"bucket"`
	Buckets []string       `json:"buckets"`
	Series  []s5SeriesJSON `json:"series"`
	Total   s5PointJSON    `json:"total"`
}

type s5BreakdownRowJSON struct {
	Value string      `json:"value"`
	Hint  *string     `json:"hint"`
	Point s5PointJSON `json:"point"`
}

type s5SummaryResponse struct {
	Total s5PointJSON                     `json:"total"`
	By    map[string][]s5BreakdownRowJSON `json:"by"`
}

var s5Dimensions = []string{"provider", "model", "requested_model", "account", "api_key", "reasoning_effort", "service_tier", "stream"}

func TestUsageTrackerEndToEndTwoProvidersOverHTTP(t *testing.T) {
	prevQueueEnabled := redisqueue.Enabled()
	prevUsageEnabled := redisqueue.UsageStatisticsEnabled()
	h := newS5Harness(t, s5HarnessOptions{trackerEnabled: true, script: s5Script})
	// Same queue reset the existing queue test uses, so earlier items do not leak in.
	redisqueue.SetEnabled(false)
	redisqueue.SetEnabled(true)
	redisqueue.SetUsageStatisticsEnabled(true)
	t.Cleanup(func() {
		redisqueue.SetEnabled(false)
		redisqueue.SetEnabled(prevQueueEnabled)
		redisqueue.SetUsageStatisticsEnabled(prevUsageEnabled)
	})

	var prices struct {
		Prices map[string]map[string]float64 `json:"prices"`
	}
	h.getJSON("prices", &prices)
	if len(prices.Prices) != 0 {
		t.Fatalf("step 0: GET prices before any PUT = %v, want an empty map", prices.Prices)
	}

	start := time.Now().UTC()
	for i, req := range s5Script {
		status, body := h.chat(i, req)
		if req.Status == http.StatusOK && status != http.StatusOK {
			t.Fatalf("step 2: request %d (%+v) status %d, want 200; body %s", i, req, status, body)
		}
		if req.Status != http.StatusOK && status == http.StatusOK {
			t.Fatalf("step 2: request %d (%+v) succeeded, want the scripted %d to surface; body %s", i, req, req.Status, body)
		}
	}
	ledger := buildS5Ledger(t, s5Script, h.upstream.snapshot())
	attempts := len(ledger.Rows)

	waitFor(t, "the usage dispatcher to deliver every attempt", 10*time.Second, func() bool {
		return len(h.probe.snapshot()) >= attempts
	})
	waitFor(t, "the tracker to flush every attempt", 10*time.Second, func() bool {
		var series s5SeriesResponse
		h.getJSON("series", &series)
		return series.Total.Requests >= int64(attempts)
	})
	if published := len(h.probe.snapshot()); published != attempts {
		t.Fatalf("step 3: usage records published = %d, upstream attempts = %d", published, attempts)
	}

	from := start.Truncate(time.Hour).Add(-time.Hour)
	to := time.Now().UTC().Truncate(time.Hour).Add(2 * time.Hour)
	var buckets []string
	for b := from; b.Before(to); b = b.Add(time.Hour) {
		buckets = append(buckets, b.Format(time.RFC3339))
	}
	seriesPath := func(params url.Values) string {
		params.Set("from", from.Format(time.RFC3339))
		params.Set("to", to.Format(time.RFC3339))
		params.Set("bucket", "hour")
		return "series?" + params.Encode()
	}
	evidenceSeries := map[string]json.RawMessage{}
	getSeries := func(name string, params url.Values) s5SeriesResponse {
		t.Helper()
		var resp s5SeriesResponse
		evidenceSeries[name] = h.getJSON(seriesPath(params), &resp)
		return resp
	}

	t.Run("sqlite rows match the ledger", func(t *testing.T) {
		assertS5SQLite(t, h, ledger)
	})

	t.Run("series group=provider", func(t *testing.T) {
		resp := getSeries("group=provider", url.Values{"group": {"provider"}})
		want := ledger.by(func(r s5LedgerRow) string { return r.Provider })
		assertS5Series(t, resp, buckets, want, ledger.total(), func(k map[string]string) string { return k["provider"] })
	})
	t.Run("series group=model,account", func(t *testing.T) {
		resp := getSeries("group=model,account", url.Values{"group": {"model,account"}})
		want := ledger.by(func(r s5LedgerRow) string { return r.Model + "|" + r.Account })
		assertS5Series(t, resp, buckets, want, ledger.total(), func(k map[string]string) string { return k["model"] + "|" + k["account"] })
	})
	t.Run("series group=api_key", func(t *testing.T) {
		resp := getSeries("group=api_key", url.Values{"group": {"api_key"}})
		want := ledger.by(func(r s5LedgerRow) string { return r.APIKeyHash })
		assertS5Series(t, resp, buckets, want, ledger.total(), func(k map[string]string) string { return k["api_key"] })
	})
	t.Run("series group=model,account,api_key", func(t *testing.T) {
		resp := getSeries("group=model,account,api_key", url.Values{"group": {"model,account,api_key"}})
		want := ledger.by(func(r s5LedgerRow) string { return r.Model + "|" + r.Account + "|" + r.APIKeyHash })
		assertS5Series(t, resp, buckets, want, ledger.total(), func(k map[string]string) string {
			return k["model"] + "|" + k["account"] + "|" + k["api_key"]
		})
	})
	t.Run("series filter.provider=claude", func(t *testing.T) {
		resp := getSeries("filter.provider=claude", url.Values{"filter.provider": {s5ClaudeProvider}})
		claude := ledger.by(func(r s5LedgerRow) string { return r.Provider })[s5ClaudeProvider]
		assertS5Series(t, resp, buckets, map[string]s5Totals{"": claude}, claude, func(map[string]string) string { return "" })
	})
	t.Run("series limit=1 folds the rest into other", func(t *testing.T) {
		resp := getSeries("group=provider&limit=1", url.Values{"group": {"provider"}, "limit": {"1"}})
		byProvider := ledger.by(func(r s5LedgerRow) string { return r.Provider })
		top, rest := s5ClaudeProvider, s5CompatProvider
		if byProvider[rest].Tokens.Total > byProvider[top].Tokens.Total {
			top, rest = rest, top
		}
		if byProvider[top].Tokens.Total == byProvider[rest].Tokens.Total {
			t.Fatal("ledger providers tie on total tokens; the script must break the tie")
		}
		want := map[string]s5Totals{top: byProvider[top], "other": byProvider[rest]}
		assertS5Series(t, resp, buckets, want, ledger.total(), func(k map[string]string) string { return k["provider"] })
		if len(resp.Series) != 2 || resp.Series[0].Key["provider"] != top || resp.Series[1].Key["provider"] != "other" {
			t.Fatalf("limit=1 series keys = %v, want [%s other]", s5SeriesKeys(resp), top)
		}
	})

	var summary s5SummaryResponse
	summaryPath := "summary?" + url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	summaryBody := h.getJSON(summaryPath, &summary)
	t.Run("summary", func(t *testing.T) {
		assertS5Shape(t, summaryBody, []string{"total", "by"})
		if got := summary.Total.totals(); got != ledger.total() {
			t.Fatalf("summary total = %+v, want %+v", got, ledger.total())
		}
		if got := slices.Sorted(maps.Keys(summary.By)); !slices.Equal(got, slices.Sorted(slices.Values(s5Dimensions))) {
			t.Fatalf("summary.by dimensions = %v, want %v", got, s5Dimensions)
		}
		for dim, key := range map[string]func(s5LedgerRow) string{
			"provider":        func(r s5LedgerRow) string { return r.Provider },
			"model":           func(r s5LedgerRow) string { return r.Model },
			"requested_model": func(r s5LedgerRow) string { return r.Model },
			"account":         func(r s5LedgerRow) string { return r.Account },
			"api_key":         func(r s5LedgerRow) string { return r.APIKeyHash },
			"stream":          func(r s5LedgerRow) string { return map[bool]string{true: "stream", false: "sync"}[r.Stream] },
		} {
			assertS5Breakdown(t, dim, summary.By[dim], ledger.by(key))
		}
		for _, row := range summary.By["api_key"] {
			wantHint := ""
			for _, key := range s5ClientKeys {
				if s5HashKey(key) == row.Value {
					wantHint = s5KeyHint(key)
				}
			}
			if row.Hint == nil || *row.Hint != wantHint {
				t.Fatalf("summary api_key %s hint = %v, want %q", row.Value, row.Hint, wantHint)
			}
		}
		for _, dim := range []string{"reasoning_effort", "service_tier"} {
			var sum s5Totals
			for _, row := range summary.By[dim] {
				sum.Requests += row.Point.Requests
				sum.Failures += row.Point.Failures
				sum.Tokens = sum.Tokens.add(row.Point.Tokens)
			}
			if sum != ledger.total() {
				t.Fatalf("summary.by.%s rows sum to %+v, want %+v", dim, sum, ledger.total())
			}
		}
	})

	var dimensions map[string]json.RawMessage
	dimensionsPath := "dimensions?" + url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
	dimensionsBody := h.getJSON(dimensionsPath, &dimensions)
	t.Run("dimensions", func(t *testing.T) {
		assertS5Shape(t, dimensionsBody, s5Dimensions)
		wantValues := map[string][]string{
			"provider":        {s5ClaudeProvider, s5CompatProvider},
			"model":           {"s5-claude-haiku", "s5-claude-opus", "s5-compat-large", "s5-compat-small"},
			"requested_model": {"s5-claude-haiku", "s5-claude-opus", "s5-compat-large", "s5-compat-small"},
			"account":         {"claude-alpha.json", "claude-bravo.json", "s5-compat-main.json"},
			"stream":          {"stream", "sync"},
		}
		for dim, want := range wantValues {
			var got []string
			if errDecode := json.Unmarshal(dimensions[dim], &got); errDecode != nil {
				t.Fatalf("dimensions.%s: %v", dim, errDecode)
			}
			if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
				t.Fatalf("dimensions.%s = %v, want %v", dim, got, want)
			}
		}
		var apiKeys []struct {
			Value string `json:"value"`
			Hint  string `json:"hint"`
		}
		if errDecode := json.Unmarshal(dimensions["api_key"], &apiKeys); errDecode != nil {
			t.Fatalf("dimensions.api_key: %v", errDecode)
		}
		want := map[string]string{s5HashKey(s5ClientKeys[0]): s5KeyHint(s5ClientKeys[0]), s5HashKey(s5ClientKeys[1]): s5KeyHint(s5ClientKeys[1])}
		got := map[string]string{}
		for _, item := range apiKeys {
			got[item.Value] = item.Hint
		}
		if !maps.Equal(got, want) {
			t.Fatalf("dimensions.api_key = %v, want hash->hint %v", got, want)
		}
	})

	t.Run("redisqueue still receives every record", func(t *testing.T) {
		assertS5Queue(t, ledger)
	})

	t.Run("prices drive cost_usd and survive a reload", func(t *testing.T) {
		status, body := h.management(http.MethodPut, "prices", map[string]any{"prices": map[string]any{"s5-claude-opus": s5ClaudeOpusPrice}})
		if status != http.StatusOK {
			t.Fatalf("PUT prices: status %d, body %s", status, body)
		}
		var putResp struct {
			Prices map[string]map[string]float64 `json:"prices"`
		}
		if errDecode := json.Unmarshal(body, &putResp); errDecode != nil || !maps.Equal(putResp.Prices["s5-claude-opus"], s5ClaudeOpusPrice) {
			t.Fatalf("PUT prices response = %s (%v), want the stored price", body, errDecode)
		}
		select {
		case errReload := <-h.reloads:
			if errReload != nil {
				t.Fatalf("reload after PUT prices: %v", errReload)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("PUT prices did not trigger the management save reload")
		}

		byModel := ledger.by(func(r s5LedgerRow) string { return r.Model })
		opus := byModel["s5-claude-opus"].Tokens
		wantCost := (float64(opus.InputUncached)*s5ClaudeOpusPrice["input"] +
			float64(opus.InputCacheRead)*s5ClaudeOpusPrice["cache_read"] +
			float64(opus.InputCacheWrite)*s5ClaudeOpusPrice["cache_write"] +
			float64(opus.OutputOther+opus.OutputReasoning)*s5ClaudeOpusPrice["output"]) / 1e6
		if wantCost == 0 {
			t.Fatal("ledger has no s5-claude-opus tokens to price")
		}
		assertCost := func(stage string) {
			t.Helper()
			resp := getSeries("group=model priced "+stage, url.Values{"group": {"model"}})
			for _, series := range resp.Series {
				want := 0.0
				if series.Key["model"] == "s5-claude-opus" {
					want = wantCost
				}
				if math.Abs(series.Total.CostUSD-want) > 1e-9 {
					t.Fatalf("%s: series %v cost_usd = %.12f, want %.12f", stage, series.Key, series.Total.CostUSD, want)
				}
				var pointSum float64
				for _, p := range series.Points {
					pointSum += p.CostUSD
				}
				if math.Abs(pointSum-want) > 1e-9 {
					t.Fatalf("%s: series %v point cost sum = %.12f, want %.12f", stage, series.Key, pointSum, want)
				}
			}
			if math.Abs(resp.Total.CostUSD-wantCost) > 1e-9 {
				t.Fatalf("%s: series total cost_usd = %.12f, want %.12f", stage, resp.Total.CostUSD, wantCost)
			}
			var priced s5SummaryResponse
			h.getJSON(summaryPath, &priced)
			if math.Abs(priced.Total.CostUSD-wantCost) > 1e-9 {
				t.Fatalf("%s: summary total cost_usd = %.12f, want %.12f", stage, priced.Total.CostUSD, wantCost)
			}
		}
		assertCost("after PUT")

		raw, errRead := os.ReadFile(h.configPath)
		if errRead != nil {
			t.Fatal(errRead)
		}
		var onDisk struct {
			Observability struct {
				Usage struct {
					Tracker struct {
						Prices map[string]map[string]float64 `yaml:"prices"`
					} `yaml:"tracker"`
				} `yaml:"usage"`
			} `yaml:"observability"`
		}
		if errYAML := yaml.Unmarshal(raw, &onDisk); errYAML != nil {
			t.Fatalf("parse saved config: %v", errYAML)
		}
		wantDisk := map[string]float64{"input": 3, "cache-read": 0.3, "cache-write": 3.75, "output": 15}
		if got := onDisk.Observability.Usage.Tracker.Prices["s5-claude-opus"]; !maps.Equal(got, wantDisk) {
			t.Fatalf("observability.usage.tracker.prices on disk = %v, want s5-claude-opus %v\n%s", onDisk.Observability.Usage.Tracker.Prices, wantDisk, raw)
		}

		reloaded, errLoad := config.LoadConfig(h.configPath)
		if errLoad != nil {
			t.Fatalf("reload config from disk: %v", errLoad)
		}
		h.server.UpdateClients(reloaded)
		var afterReload struct {
			Prices map[string]map[string]float64 `json:"prices"`
		}
		h.getJSON("prices", &afterReload)
		if len(afterReload.Prices) != 1 || !maps.Equal(afterReload.Prices["s5-claude-opus"], s5ClaudeOpusPrice) {
			t.Fatalf("GET prices after reload = %v, want only s5-claude-opus %v", afterReload.Prices, s5ClaudeOpusPrice)
		}
		assertCost("after reload")
	})

	t.Run("no raw secret in any response body", func(t *testing.T) {
		secrets := append(slices.Clone(s5ClientKeys), s5ManagementSecret)
		for _, cred := range s5Credentials {
			secrets = append(secrets, cred.UpstreamKey)
		}
		h.bodiesMu.Lock()
		defer h.bodiesMu.Unlock()
		if len(h.bodies) < 8 {
			t.Fatalf("only %d management responses captured", len(h.bodies))
		}
		for name, body := range h.bodies {
			for _, secret := range secrets {
				if strings.Contains(string(body), secret) {
					t.Fatalf("%s response contains raw secret %q", name, secret)
				}
			}
		}
	})

	evidence, _ := json.MarshalIndent(map[string]any{
		"ledger":            ledger,
		"upstream_attempts": h.upstream.snapshot(),
		"range":             map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
		"series":            evidenceSeries,
	}, "", "  ")
	writeS5Evidence(t, "s5-series.json", evidence)
	h.bodiesMu.Lock()
	summaryEvidence, _ := json.MarshalIndent(map[string]any{
		"summary":                     json.RawMessage(summaryBody),
		"dimensions":                  json.RawMessage(dimensionsBody),
		"put_prices":                  json.RawMessage(h.bodies["PUT prices"]),
		"prices_after_reload":         json.RawMessage(h.bodies["GET prices"]),
		"summary_priced_after_reload": json.RawMessage(h.bodies["GET "+summaryPath]),
	}, "", "  ")
	h.bodiesMu.Unlock()
	writeS5Evidence(t, "s5-summary.json", summaryEvidence)
}

func TestUsageTrackerDisabledCreatesNoDatabase(t *testing.T) {
	script := []s5Request{{Provider: s5CompatProvider, Model: "s5-compat-small", Key: 0, Stream: false, Status: http.StatusOK}}
	h := newS5Harness(t, s5HarnessOptions{trackerEnabled: false, script: script})

	if status, body := h.chat(0, script[0]); status != http.StatusOK {
		t.Fatalf("request with the tracker disabled: status %d, body %s", status, body)
	}
	waitFor(t, "the usage record to be dispatched", 10*time.Second, func() bool { return len(h.probe.snapshot()) >= 1 })

	if _, errStat := os.Stat(h.dbPath()); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("tracker disabled: %s exists (stat err %v)", h.dbPath(), errStat)
	}
	assertS5NoSQLiteFiles(t, h.authDir)
	for _, path := range []string{"series", "summary", "dimensions", "prices"} {
		status, body := h.management(http.MethodGet, path, nil)
		if status != http.StatusServiceUnavailable || strings.TrimSpace(string(body)) != `{"error":"usage tracker disabled"}` {
			t.Fatalf("GET %s with the tracker disabled = %d %s, want 503 usage tracker disabled", path, status, body)
		}
	}
}

// Home mode cannot proxy without a control plane, so these tests feed the
// usage dispatcher synthetic records through usage.PublishRecord.
func TestUsageTrackerHomeModeForcesTrackerOff(t *testing.T) {
	t.Run("reload into Home mode closes the tracker", func(t *testing.T) {
		script := []s5Request{{Provider: s5CompatProvider, Model: "s5-compat-small", Key: 0, Stream: false, Status: http.StatusOK}}
		h := newS5Harness(t, s5HarnessOptions{trackerEnabled: true, script: script})
		if status, body := h.chat(0, script[0]); status != http.StatusOK {
			t.Fatalf("request before Home mode: status %d, body %s", status, body)
		}
		waitFor(t, "the pre-Home attempt to be flushed", 10*time.Second, func() bool {
			var series s5SeriesResponse
			h.getJSON("series", &series)
			return series.Total.Requests >= 1
		})

		home, errLoad := config.LoadConfig(h.configPath)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		home.Home.Enabled = true
		h.server.UpdateClients(home)
		publishS5Synthetic(t, h, "s5-home-dropped")
		// Close flushes whatever store is still open, so a tracker that stayed
		// on in Home mode would persist the synthetic record here.
		h.server.CloseUsageTracker()

		db := openS5ReadOnly(t, h.dbPath())
		if n := s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_events WHERE model != 's5-compat-small'"); n != 0 {
			t.Fatalf("tracker stored %d records published after the reload into Home mode", n)
		}
		if n := s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_events"); n != 1 {
			t.Fatalf("usage_events rows = %d, want only the pre-Home attempt", n)
		}
	})

	t.Run("startup in Home mode opens nothing", func(t *testing.T) {
		h := newS5Harness(t, s5HarnessOptions{trackerEnabled: true, homeEnabled: true})
		publishS5Synthetic(t, h, "s5-home-startup")

		if _, errStat := os.Stat(h.dbPath()); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("Home mode with tracker.enabled=true: %s exists (stat err %v)", h.dbPath(), errStat)
		}
		assertS5NoSQLiteFiles(t, h.authDir)
		// Home mode serves no management routes at all, so 404 is the gate, not the tracker.
		if status, body := h.management(http.MethodGet, "series", nil); status != http.StatusNotFound {
			t.Fatalf("GET series in Home mode = %d %s, want 404", status, body)
		}
	})
}

// publishS5Synthetic publishes one record for model, then a barrier record, and
// waits until the probe sees the barrier. The dispatcher runs every plugin for
// one record before the next, so the first record has reached every plugin.
func publishS5Synthetic(t *testing.T, h *s5Harness, model string) {
	t.Helper()
	for _, m := range []string{model, model + "-barrier"} {
		usage.PublishRecord(context.Background(), usage.Record{
			Provider: s5ClaudeProvider, Model: m, AuthID: "claude-alpha.json",
			APIKey: s5ClientKeys[0], RequestedAt: time.Now(),
		})
	}
	waitFor(t, "the synthetic records to be dispatched", 10*time.Second, func() bool {
		for _, record := range h.probe.snapshot() {
			if record.Model == model+"-barrier" {
				return true
			}
		}
		return false
	})
}

// buildS5Ledger turns upstream observations into the expected tracker rows and
// checks them against the script: one attempt per success, and a failure
// fails over across every credential of its provider.
func buildS5Ledger(t *testing.T, script []s5Request, attempts []s5Attempt) s5Ledger {
	t.Helper()
	credentialsPerProvider := map[string]int{}
	for _, cred := range s5Credentials {
		credentialsPerProvider[cred.Provider]++
	}
	ledger := s5Ledger{Script: script, Seen: map[string]int{}}
	perRequest := map[int][]string{}
	for _, a := range attempts {
		req := script[a.Request]
		row := s5LedgerRow{
			Request:    a.Request,
			Provider:   a.Provider,
			Model:      req.Model,
			Account:    a.Account,
			APIKeyHash: s5HashKey(s5ClientKeys[req.Key]),
			APIKeyHint: s5KeyHint(s5ClientKeys[req.Key]),
			Stream:     req.Stream,
			Failed:     a.Status != http.StatusOK,
		}
		if row.Failed {
			row.StatusCode = a.Status
		} else {
			row.Tokens = s5TokensFor(a.Request, a.Provider)
		}
		ledger.Rows = append(ledger.Rows, row)
		perRequest[a.Request] = append(perRequest[a.Request], a.Account)
	}
	for i, req := range script {
		accounts := perRequest[i]
		ledger.Seen[fmt.Sprint(i)] = len(accounts)
		want := 1
		if req.Status != http.StatusOK {
			want = credentialsPerProvider[req.Provider]
		}
		if len(accounts) != want {
			t.Fatalf("step 2: request %d (%+v) reached the upstream %d times (%v), want %d", i, req, len(accounts), accounts, want)
		}
		if len(slices.Compact(slices.Sorted(slices.Values(accounts)))) != len(accounts) {
			t.Fatalf("step 2: request %d retried the same credential: %v", i, accounts)
		}
	}
	return ledger
}

func assertS5SQLite(t *testing.T, h *s5Harness, ledger s5Ledger) {
	t.Helper()
	db := openS5ReadOnly(t, h.dbPath())
	if got := s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_events"); got != int64(len(ledger.Rows)) {
		t.Fatalf("usage_events rows = %d, upstream attempts = %d", got, len(ledger.Rows))
	}

	rows, errQuery := db.Query(`SELECT provider, model, requested_model, account, api_key_hash, api_key_hint,
    stream, failed, status_code, failure_class, input_uncached, input_cache_read, input_cache_write,
    output_reasoning, output_other, unclassified, total_tokens, request_id, trace_id FROM usage_events`)
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			t.Errorf("rows.Close() error = %v", errClose)
		}
	}()
	var got []string
	requestIDs := map[string]bool{}
	rowsPerTrace := map[string]int{}
	for rows.Next() {
		var r s5LedgerRow
		var requestedModel, failureClass, requestID, traceID string
		if errScan := rows.Scan(&r.Provider, &r.Model, &requestedModel, &r.Account, &r.APIKeyHash, &r.APIKeyHint,
			&r.Stream, &r.Failed, &r.StatusCode, &failureClass, &r.Tokens.InputUncached, &r.Tokens.InputCacheRead,
			&r.Tokens.InputCacheWrite, &r.Tokens.OutputReasoning, &r.Tokens.OutputOther, &r.Tokens.Unclassified,
			&r.Tokens.Total, &requestID, &traceID); errScan != nil {
			t.Fatal(errScan)
		}
		if requestedModel != r.Model {
			t.Fatalf("row %s: requested_model %q != model %q with no alias configured", requestID, requestedModel, r.Model)
		}
		if want := s5FailureClass(r.Failed, r.StatusCode); failureClass != want {
			t.Fatalf("row %s: failure_class %q, want %q for failed=%v status=%d", requestID, failureClass, want, r.Failed, r.StatusCode)
		}
		if requestID == "" || requestIDs[requestID] {
			t.Fatalf("request_id %q is empty or repeated; want one row per attempt", requestID)
		}
		requestIDs[requestID] = true
		if traceID == "" {
			t.Fatalf("row %s has no trace_id", requestID)
		}
		rowsPerTrace[traceID]++
		got = append(got, s5RowKey(r))
	}
	if errRows := rows.Err(); errRows != nil {
		t.Fatal(errRows)
	}
	var want []string
	for _, r := range ledger.Rows {
		want = append(want, s5RowKey(r))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("usage_events dimensions differ from the ledger\n got: %s\nwant: %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}

	var perTrace, perRequest []int
	for _, n := range rowsPerTrace {
		perTrace = append(perTrace, n)
	}
	for _, n := range ledger.Seen {
		perRequest = append(perRequest, n)
	}
	sort.Ints(perTrace)
	sort.Ints(perRequest)
	if !slices.Equal(perTrace, perRequest) {
		t.Fatalf("rows per trace_id = %v, attempts per client request = %v; retries must share a trace", perTrace, perRequest)
	}

	assertS5RollupsMatchEvents(t, db)

	for _, name := range []string{h.dbPath(), h.dbPath() + "-wal"} {
		data, errRead := os.ReadFile(name)
		if errors.Is(errRead, os.ErrNotExist) {
			continue
		}
		if errRead != nil {
			t.Fatal(errRead)
		}
		for _, secret := range s5ClientKeys {
			if strings.Contains(string(data), secret) {
				t.Fatalf("%s stores the raw client key %q", filepath.Base(name), secret)
			}
		}
		for _, cred := range s5Credentials {
			if strings.Contains(string(data), cred.UpstreamKey) {
				t.Fatalf("%s stores the raw upstream key of %s", filepath.Base(name), cred.ID)
			}
		}
	}

	counts := []string{
		fmt.Sprintf("usage_events rows: %d", s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_events")),
		fmt.Sprintf("usage_hourly rows: %d", s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_hourly")),
		fmt.Sprintf("upstream attempts (ledger rows): %d", len(ledger.Rows)),
		fmt.Sprintf("client requests (script): %d", len(ledger.Script)),
		fmt.Sprintf("distinct request_id: %d", s5QueryInt(t, db, "SELECT COUNT(DISTINCT request_id) FROM usage_events")),
		fmt.Sprintf("distinct trace_id: %d", s5QueryInt(t, db, "SELECT COUNT(DISTINCT trace_id) FROM usage_events")),
		fmt.Sprintf("failed rows: %d", s5QueryInt(t, db, "SELECT COUNT(*) FROM usage_events WHERE failed = 1")),
		fmt.Sprintf("events not matched by hourly: %d", s5QueryInt(t, db, "SELECT COUNT(*) FROM ("+s5HourlyFromEventsSQL+" EXCEPT "+s5HourlyStoredSQL+")")),
		fmt.Sprintf("hourly not matched by events: %d", s5QueryInt(t, db, "SELECT COUNT(*) FROM ("+s5HourlyStoredSQL+" EXCEPT "+s5HourlyFromEventsSQL+")")),
		"",
		"per provider/model/account/api_key_hint/stream/failed: rows tokens",
	}
	counts = append(counts, s5QueryRows(t, db, `SELECT provider, model, account, api_key_hint, stream, failed, COUNT(*), SUM(total_tokens)
FROM usage_events GROUP BY provider, model, account, api_key_hint, stream, failed ORDER BY 1, 2, 3, 4, 5, 6`)...)
	writeS5Evidence(t, "s5-sqlite-counts.txt", []byte(strings.Join(counts, "\n")+"\n"))
}

func s5RowKey(r s5LedgerRow) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|stream=%v|failed=%v|status=%d|%+v", r.Provider, r.Model, r.Account, r.APIKeyHash[:12], r.APIKeyHint, r.Stream, r.Failed, r.StatusCode, r.Tokens)
}

func s5FailureClass(failed bool, status int) string {
	switch {
	case !failed:
		return ""
	case status == http.StatusTooManyRequests:
		return "rate_limit_429"
	case status >= 500:
		return "server_5xx"
	default:
		return "other"
	}
}

// assertS5Series checks series totals against the ledger and that every
// series is dense over the requested hourly range.
func assertS5Series(t *testing.T, resp s5SeriesResponse, buckets []string, want map[string]s5Totals, total s5Totals, keyOf func(map[string]string) string) {
	t.Helper()
	if resp.Bucket != "hour" || !slices.Equal(resp.Buckets, buckets) {
		t.Fatalf("buckets = %s %v, want hour %v", resp.Bucket, resp.Buckets, buckets)
	}
	got := map[string]s5Totals{}
	for _, series := range resp.Series {
		key := keyOf(series.Key)
		if _, dup := got[key]; dup {
			t.Fatalf("duplicate series key %v", series.Key)
		}
		got[key] = series.Total.totals()
		if len(series.Points) != len(buckets) {
			t.Fatalf("series %v has %d points for %d buckets; points must be dense", series.Key, len(series.Points), len(buckets))
		}
		var sum s5Totals
		for _, p := range series.Points {
			sum.Requests += p.Requests
			sum.Failures += p.Failures
			sum.Tokens = sum.Tokens.add(p.Tokens)
		}
		if sum != series.Total.totals() {
			t.Fatalf("series %v points sum to %+v, total says %+v", series.Key, sum, series.Total.totals())
		}
		for _, edge := range []int{0, len(series.Points) - 1} {
			if p := series.Points[edge]; p.totals() != (s5Totals{}) {
				t.Fatalf("series %v bucket %s outside the traffic window is %+v, want zero", series.Key, buckets[edge], p)
			}
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("series totals differ from the ledger\n got: %+v\nwant: %+v", got, want)
	}
	if resp.Total.totals() != total {
		t.Fatalf("response total = %+v, want %+v", resp.Total.totals(), total)
	}
}

func assertS5Breakdown(t *testing.T, dim string, rows []s5BreakdownRowJSON, want map[string]s5Totals) {
	t.Helper()
	got := map[string]s5Totals{}
	for i, row := range rows {
		got[row.Value] = row.Point.totals()
		if i > 0 && rows[i-1].Point.Tokens.Total < row.Point.Tokens.Total {
			t.Fatalf("summary.by.%s is not sorted by total tokens desc at %d", dim, i)
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("summary.by.%s differs from the ledger\n got: %+v\nwant: %+v", dim, got, want)
	}
}

func assertS5Shape(t *testing.T, body []byte, keys []string) {
	t.Helper()
	var top map[string]json.RawMessage
	if errDecode := json.Unmarshal(body, &top); errDecode != nil {
		t.Fatal(errDecode)
	}
	if got := slices.Sorted(maps.Keys(top)); !slices.Equal(got, slices.Sorted(slices.Values(keys))) {
		t.Fatalf("response keys = %v, want %v", got, keys)
	}
}

func s5SeriesKeys(resp s5SeriesResponse) []map[string]string {
	keys := make([]map[string]string, 0, len(resp.Series))
	for _, series := range resp.Series {
		keys = append(keys, series.Key)
	}
	return keys
}

// assertS5Queue drains the redis usage queue and checks it carries the same
// attempts as the tracker, using the existing queue test's payload parser.
func assertS5Queue(t *testing.T, ledger s5Ledger) {
	t.Helper()
	type queueKey struct {
		provider, model string
		failed          bool
	}
	want := map[queueKey]int64{}
	wantTokens := map[queueKey]int64{}
	for _, row := range ledger.Rows {
		k := queueKey{row.Provider, row.Model, row.Failed}
		want[k]++
		wantTokens[k] += row.Tokens.Total
	}
	got := map[queueKey]int64{}
	gotTokens := map[queueKey]int64{}
	for {
		items := redisqueue.PopOldest(100)
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			payload, ok := parseQueuedUsagePayload(t, item)
			if !ok || !strings.HasPrefix(payload.Model, "s5-") {
				continue
			}
			k := queueKey{payload.Provider, payload.Model, payload.Failed}
			got[k]++
			gotTokens[k] += payload.Tokens.TotalTokens
		}
	}
	if !maps.Equal(got, want) || !maps.Equal(gotTokens, wantTokens) {
		t.Fatalf("redis usage queue differs from the ledger\n got: %v tokens %v\nwant: %v tokens %v", got, gotTokens, want, wantTokens)
	}
}

func assertS5NoSQLiteFiles(t *testing.T, dir string) {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".sqlite") {
			t.Fatalf("auth dir contains %s with the tracker off", entry.Name())
		}
	}
}
