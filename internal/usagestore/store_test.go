package usagestore_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

func TestOpenCreatesSchemaAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "usage.db")
	ctx := context.Background()

	s, err := usagestore.Open(ctx, usagestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	publish(s, attempt{at: t0, provider: "claude", model: "model-a", in: 10})
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file: %v", err)
	}

	s, _ = openStore(t, usagestore.Options{Path: path})
	db := openRaw(t, path)
	for _, name := range []string{"usage_meta", "usage_events", "usage_hourly", "usage_events_hour", "usage_events_trace"} {
		if n := queryInt(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE name = ?", name); n != 1 {
			t.Fatalf("schema object %q count = %d, want 1", name, n)
		}
	}
	var version, mode string
	if err := db.QueryRow(`SELECT value FROM usage_meta WHERE key = 'schema_version'`).Scan(&version); err != nil || version != "1" {
		t.Fatalf("schema_version = %q, %v; want 1", version, err)
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	if n := countRows(t, db, "usage_events"); n != 1 {
		t.Fatalf("usage_events after reopen = %d, want 1", n)
	}
	if n := countRows(t, db, "usage_meta"); n != 1 {
		t.Fatalf("usage_meta rows = %d, want 1", n)
	}
	publish(s, attempt{at: t0, provider: "claude", model: "model-a", in: 5})
	flush(t, s)
	if n := queryInt(t, db, "SELECT requests FROM usage_hourly"); n != 2 {
		t.Fatalf("hourly requests after reopen = %d, want 2", n)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := usagestore.Open(context.Background(), usagestore.Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db := openRaw(t, path)
	if _, err := db.Exec(`UPDATE usage_meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if s, err := usagestore.Open(context.Background(), usagestore.Options{Path: path}); err == nil {
		_ = s.Close()
		t.Fatal("Open() of a newer schema succeeded, want error")
	}
}

func TestPluginWritesOneRowPerAttemptAndMatchingRollups(t *testing.T) {
	s, path := openStore(t, usagestore.Options{})
	attempts := mixedWorkload(300)
	publish(s, attempts...)
	flush(t, s)

	db := openRaw(t, path)
	if n := countRows(t, db, "usage_events"); n != int64(len(attempts)) {
		t.Fatalf("usage_events rows = %d, want %d", n, len(attempts))
	}
	assertRollupsMatchEvents(t, db)
	// Fewer rollup rows than half the events means the upsert merge path ran.
	if n := countRows(t, db, "usage_hourly"); n < 2 || n*2 > int64(len(attempts)) {
		t.Fatalf("usage_hourly rows = %d, want several rows merging multiple attempts", n)
	}
}

func TestPluginStoresAttemptDimensions(t *testing.T) {
	s, path := openStore(t, usagestore.Options{})
	at := t0.Add(5*time.Hour + 17*time.Minute + 3*time.Second)
	publish(s, attempt{
		at: at, provider: "claude", model: "model-a", requested: "fast", account: "acct.json",
		apiKey: "sk-client-key-123456", effort: "high", tier: "priority", stream: true,
		failed: true, status: 429, latency: 1500 * time.Millisecond, ttft: 250 * time.Millisecond,
		in: 100, cacheRead: 20, cacheWrite: 10, out: 50, reasoning: 5,
	})
	flush(t, s)

	var (
		requestedAtMs, hour, stream, failed, status, latency, ttft int64
		in, cacheRead, cacheWrite, reasoning, out, unclassified    int64
		total                                                      int64
		requestID, traceID, provider, model, requested, account    string
		keyHash, keyHint, effort, tier, failureClass, quality      string
	)
	err := openRaw(t, path).QueryRow(`SELECT requested_at_ms, hour_start, request_id, trace_id, provider, model,
		requested_model, account, api_key_hash, api_key_hint, reasoning_effort, service_tier, stream, failed,
		status_code, failure_class, latency_ms, ttft_ms, input_uncached, input_cache_read, input_cache_write,
		output_reasoning, output_other, unclassified, total_tokens, quality FROM usage_events`).Scan(
		&requestedAtMs, &hour, &requestID, &traceID, &provider, &model, &requested, &account, &keyHash, &keyHint,
		&effort, &tier, &stream, &failed, &status, &failureClass, &latency, &ttft, &in, &cacheRead, &cacheWrite,
		&reasoning, &out, &unclassified, &total, &quality)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("sk-client-key-123456"))
	got := []any{requestedAtMs, hour, requestID, traceID, provider, model, requested, account, keyHash, keyHint,
		effort, tier, stream, failed, status, failureClass, latency, ttft, in, cacheRead, cacheWrite, reasoning, out,
		unclassified, total, quality}
	want := []any{at.UnixMilli(), t0.Add(5 * time.Hour).Unix(), "req-0", "trace-0", "claude", "model-a", "fast",
		"acct.json", hex.EncodeToString(sum[:]), "sk…56", "high", "priority", int64(1), int64(1), int64(429),
		"rate_limit_429", int64(1500), int64(250), int64(100), int64(20), int64(10), int64(5), int64(50), int64(0),
		int64(185), "complete"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("column %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestSubMillisecondTTFTCountsAsKnown(t *testing.T) {
	s, path := openStore(t, usagestore.Options{})
	at := t0.Add(time.Minute)
	publish(s,
		attempt{at: at, provider: "p", model: "m", latency: 900 * time.Microsecond, ttft: 150 * time.Microsecond},
		attempt{at: at, provider: "p", model: "m", latency: 1900 * time.Microsecond, ttft: 1200 * time.Microsecond},
		attempt{at: at, provider: "p", model: "m", latency: 5 * time.Millisecond},
	)
	flush(t, s)

	db := openRaw(t, path)
	if got := queryRows(t, db, `SELECT latency_ms || '/' || ttft_ms FROM usage_events ORDER BY id`); strings.Join(got, ",") != "0/1,1/2,5/0" {
		t.Fatalf("latency_ms/ttft_ms = %v, want [0/1 1/2 5/0]", got)
	}
	if got := queryRows(t, db, `SELECT ttft_ms_sum || '/' || ttft_ms_max || '/' || ttft_count FROM usage_hourly`); strings.Join(got, ",") != "3/2/2" {
		t.Fatalf("hourly ttft sum/max/count = %v, want [3/2/2]", got)
	}
}

func TestPluginFillsZeroRequestedAtFromNow(t *testing.T) {
	now := t0.Add(3*time.Hour + 42*time.Minute)
	s, path := openStore(t, usagestore.Options{Now: newClock(now).Now})
	publish(s, attempt{provider: "claude", model: "model-a", in: 1})
	flush(t, s)

	db := openRaw(t, path)
	if got := queryInt(t, db, "SELECT requested_at_ms FROM usage_events"); got != now.UnixMilli() {
		t.Fatalf("requested_at_ms = %d, want %d", got, now.UnixMilli())
	}
	if got := queryInt(t, db, "SELECT hour_start FROM usage_hourly"); got != t0.Add(3*time.Hour).Unix() {
		t.Fatalf("hour_start = %d, want %d", got, t0.Add(3*time.Hour).Unix())
	}
}

func TestPluginWritesThousandEventsAcrossBatches(t *testing.T) {
	s, path := openStore(t, usagestore.Options{})
	attempts := mixedWorkload(1000)
	publish(s, attempts...)
	flush(t, s)

	db := openRaw(t, path)
	if n := countRows(t, db, "usage_events"); n != 1000 {
		t.Fatalf("usage_events rows = %d, want 1000", n)
	}
	if n := queryInt(t, db, "SELECT SUM(requests) FROM usage_hourly"); n != 1000 {
		t.Fatalf("hourly requests = %d, want 1000", n)
	}
	assertRollupsMatchEvents(t, db)
	if s.Dropped() != 0 {
		t.Fatalf("Dropped() = %d, want 0", s.Dropped())
	}
}

func TestConcurrentPublishFlushAndQuery(t *testing.T) {
	s, path := openStore(t, usagestore.Options{})
	const workers, perWorker = 8, 200
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempts := mixedWorkload(perWorker)
			plugin := s.Plugin()
			for i, a := range attempts {
				plugin.HandleUsage(context.Background(), a.record(w*perWorker+i))
				if i%50 == 0 {
					if _, err := s.QuerySeries(context.Background(), usagestore.Query{From: t0, To: t0.Add(48 * time.Hour)}, nil); err != nil {
						t.Errorf("QuerySeries() error = %v", err)
					}
					if err := s.Flush(context.Background()); err != nil {
						t.Errorf("Flush() error = %v", err)
					}
				}
			}
		}()
	}
	wg.Wait()
	flush(t, s)

	db := openRaw(t, path)
	if n := countRows(t, db, "usage_events"); n != workers*perWorker {
		t.Fatalf("usage_events rows = %d, want %d", n, workers*perWorker)
	}
	assertRollupsMatchEvents(t, db)
}

func TestCloseFlushesPendingEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := usagestore.Open(context.Background(), usagestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	publish(s, mixedWorkload(10)...)
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := s.Flush(context.Background()); !errors.Is(err, usagestore.ErrClosed) {
		t.Fatalf("Flush() after Close = %v, want ErrClosed", err)
	}
	publish(s, mixedWorkload(1)...)

	db := openRaw(t, path)
	if n := countRows(t, db, "usage_events"); n != 10 {
		t.Fatalf("usage_events rows = %d, want 10", n)
	}
	assertRollupsMatchEvents(t, db)
}

func TestQueriesAfterCloseReturnErrClosed(t *testing.T) {
	s, err := usagestore.Open(context.Background(), usagestore.Options{Path: filepath.Join(t.TempDir(), "usage.db")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(time.Hour)
	if _, err := s.QuerySeries(context.Background(), usagestore.Query{From: from, To: to}, nil); !errors.Is(err, usagestore.ErrClosed) {
		t.Fatalf("QuerySeries() after Close = %v, want ErrClosed", err)
	}
	if _, err := s.QueryBreakdown(context.Background(), from, to, usagestore.DimProvider, nil, nil); !errors.Is(err, usagestore.ErrClosed) {
		t.Fatalf("QueryBreakdown() after Close = %v, want ErrClosed", err)
	}
	if _, err := s.QueryDimensionValues(context.Background(), from, to); !errors.Is(err, usagestore.ErrClosed) {
		t.Fatalf("QueryDimensionValues() after Close = %v, want ErrClosed", err)
	}
}

func TestSweepAppliesRetention(t *testing.T) {
	now := t0.Add(30 * 24 * time.Hour)
	clock := newClock(now)
	s, path := openStore(t, usagestore.Options{Now: clock.Now})
	publish(s,
		attempt{at: now.Add(-10 * 24 * time.Hour), provider: "old", in: 1},
		attempt{at: now.Add(-50 * time.Hour), provider: "mid", in: 1},
		attempt{at: now.Add(-2 * time.Hour), provider: "new", in: 1},
	)
	flush(t, s)
	db := openRaw(t, path)
	ctx := context.Background()
	remaining := func(table string) []string {
		return queryRows(t, db, "SELECT provider FROM "+table+" ORDER BY provider")
	}
	assertProviders := func(table string, want ...string) {
		t.Helper()
		got := remaining(table)
		if len(got) != len(want) {
			t.Fatalf("%s providers = %v, want %v", table, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s providers = %v, want %v", table, got, want)
			}
		}
	}
	sweep := func() {
		t.Helper()
		if err := s.Sweep(ctx); err != nil {
			t.Fatalf("Sweep() error = %v", err)
		}
	}

	sweep()
	assertProviders("usage_events", "mid", "new", "old")
	assertProviders("usage_hourly", "mid", "new", "old")

	s.SetRetention(48*time.Hour, 0)
	sweep()
	assertProviders("usage_events", "new")
	assertProviders("usage_hourly", "mid", "new", "old")

	s.SetRetention(0, 72*time.Hour)
	sweep()
	assertProviders("usage_events", "new")
	assertProviders("usage_hourly", "mid", "new")

	clock.Set(now.Add(48 * time.Hour))
	sweep()
	assertProviders("usage_hourly", "new")

	clock.Set(now.Add(365 * 24 * time.Hour))
	s.SetRetention(0, 0)
	sweep()
	assertProviders("usage_events", "new")
	assertProviders("usage_hourly", "new")
}

func TestSweepKeepsPartiallyRetainedHour(t *testing.T) {
	hour := t0.Add(10 * time.Hour)
	clock := newClock(hour.Add(90 * time.Minute))
	s, path := openStore(t, usagestore.Options{Now: clock.Now, HourlyRetention: time.Hour})
	publish(s,
		attempt{at: hour.Add(-time.Minute), provider: "previous", in: 1},
		attempt{at: hour.Add(time.Minute), provider: "straddling", in: 1},
	)
	flush(t, s)
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := queryRows(t, openRaw(t, path), "SELECT provider FROM usage_hourly")
	if len(got) != 1 || got[0] != "straddling" {
		t.Fatalf("usage_hourly providers = %v, want [straddling]", got)
	}
}

func TestOpenSweepsExpiredRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	clock := newClock(t0)
	s, err := usagestore.Open(context.Background(), usagestore.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	publish(s, attempt{at: t0.Add(-time.Hour), provider: "claude", in: 1})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	clock.Set(t0.Add(7 * 24 * time.Hour))
	openStore(t, usagestore.Options{Path: path, Now: clock.Now, RawRetention: 24 * time.Hour, HourlyRetention: 24 * time.Hour})
	db := openRaw(t, path)
	if n := countRows(t, db, "usage_events") + countRows(t, db, "usage_hourly"); n != 0 {
		t.Fatalf("rows after Open with retention = %d, want 0", n)
	}
}
