package usagestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// t0 is a UTC midnight used as the base of test timelines.
var t0 = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// openStore opens a store in a temp dir unless opts.Path is set, and closes it on cleanup.
func openStore(t *testing.T, opts usagestore.Options) (*usagestore.Store, string) {
	t.Helper()
	if opts.Path == "" {
		opts.Path = filepath.Join(t.TempDir(), "usage.db")
	}
	if opts.Now == nil {
		opts.Now = newClock(t0).Now
	}
	s, err := usagestore.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s, opts.Path
}

// openRaw opens a second handle on the database file for assertions.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("raw db Close() error = %v", err)
		}
	})
	return db
}

func queryInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func countRows(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	return queryInt(t, db, "SELECT COUNT(*) FROM "+table)
}

func flush(t *testing.T, s *usagestore.Store) {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
}

// attempt describes one upstream attempt in test terms.
type attempt struct {
	at                                        time.Time
	provider, model, requested                string
	account, apiKey, effort, tier             string
	stream, failed                            bool
	status                                    int
	latency, ttft                             time.Duration
	in, cacheRead, cacheWrite, out, reasoning int64
	unclassified                              int64
}

func (a attempt) record(id int) usage.Record {
	breakdown := usage.NewIndependentTokenBreakdown(a.in, a.cacheRead, a.cacheWrite, a.out, a.reasoning, 0)
	if a.unclassified > 0 {
		breakdown = usage.NewUnclassifiedTokenBreakdown(a.unclassified)
	}
	return usage.Record{
		RequestedAt:     a.at,
		RequestID:       fmt.Sprintf("req-%d", id),
		TraceID:         fmt.Sprintf("trace-%d", id/2),
		Provider:        a.provider,
		Model:           a.model,
		Alias:           a.requested,
		AuthID:          a.account,
		APIKey:          a.apiKey,
		ReasoningEffort: a.effort,
		ServiceTier:     a.tier,
		Stream:          a.stream,
		Failed:          a.failed,
		Fail:            usage.Failure{StatusCode: a.status},
		Latency:         a.latency,
		TTFT:            a.ttft,
		Detail:          usage.Detail{TokenBreakdown: breakdown},
	}
}

func publish(s *usagestore.Store, attempts ...attempt) {
	plugin := s.Plugin()
	for i, a := range attempts {
		plugin.HandleUsage(context.Background(), a.record(i))
	}
}

// mixedWorkload spreads n attempts over six dimension profiles, failures, and
// about n/20 hours, so each hourly key receives several attempts.
func mixedWorkload(n int) []attempt {
	providers := []string{"claude", "codex", "gemini"}
	models := []string{"model-a", "model-b", "model-c", "model-d"}
	accounts := []string{"acct-1.json", "acct-2.json", ""}
	keys := []string{"sk-client-one-0001", "sk-client-two-0002", ""}
	efforts := []string{"", "low", "high"}
	tiers := []string{"default", "priority"}
	out := make([]attempt, n)
	for i := range out {
		profile := i % 6
		a := attempt{
			at:        t0.Add(time.Duration(i) * 3 * time.Minute).Add(time.Duration(i%60) * time.Second),
			provider:  providers[profile%len(providers)],
			model:     models[profile%len(models)],
			requested: models[(profile/2)%len(models)],
			account:   accounts[profile%len(accounts)],
			apiKey:    keys[profile%len(keys)],
			effort:    efforts[profile%len(efforts)],
			tier:      tiers[profile%len(tiers)],
			stream:    profile%2 == 0,
			// Non-monotonic so a later attempt on the same key can be smaller.
			latency:   time.Duration(100+i*137%900) * time.Millisecond,
			in:        int64(100 + i%37),
			cacheRead: int64(i % 11 * 10),
			out:       int64(20 + i%23),
		}
		if i%3 != 0 {
			a.ttft = time.Duration(20+i*31%200) * time.Millisecond
		}
		if i%5 == 0 {
			a.failed, a.status = true, []int{429, 500, 401, 0}[i/5%4]
		}
		if i%7 == 0 {
			a.cacheWrite, a.reasoning = 30, 15
		}
		if i%13 == 0 {
			a.in, a.cacheRead, a.cacheWrite, a.out, a.reasoning, a.unclassified = 0, 0, 0, 0, 0, 77
		}
		out[i] = a
	}
	return out
}

const hourlyFromEventsSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    MAX(api_key_hint), reasoning_effort, service_tier, stream,
    COUNT(*), SUM(failed), SUM(input_uncached), SUM(input_cache_read), SUM(input_cache_write),
    SUM(output_reasoning), SUM(output_other), SUM(unclassified), SUM(total_tokens),
    SUM(latency_ms), MAX(latency_ms), SUM(ttft_ms), MAX(ttft_ms), SUM(ttft_ms > 0)
FROM usage_events
GROUP BY hour_start, provider, model, requested_model, account, api_key_hash, reasoning_effort, service_tier, stream`

const hourlyStoredSQL = `SELECT hour_start, provider, model, requested_model, account, api_key_hash,
    api_key_hint, reasoning_effort, service_tier, stream,
    requests, failures, input_uncached, input_cache_read, input_cache_write,
    output_reasoning, output_other, unclassified, total_tokens,
    latency_ms_sum, latency_ms_max, ttft_ms_sum, ttft_ms_max, ttft_count
FROM usage_hourly`

// assertRollupsMatchEvents checks that usage_hourly equals usage_events
// grouped by the hourly key, for every counter, sum, and max column.
func assertRollupsMatchEvents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, diff := range []struct{ name, query string }{
		{"events not matched by hourly", hourlyFromEventsSQL + " EXCEPT " + hourlyStoredSQL},
		{"hourly not matched by events", hourlyStoredSQL + " EXCEPT " + hourlyFromEventsSQL},
	} {
		if rows := queryRows(t, db, diff.query); len(rows) > 0 {
			t.Fatalf("%s:\n%s", diff.name, strings.Join(rows, "\n"))
		}
	}
	grouped := queryInt(t, db, "SELECT COUNT(*) FROM ("+hourlyFromEventsSQL+")")
	if stored := countRows(t, db, "usage_hourly"); stored != grouped {
		t.Fatalf("usage_hourly has %d rows, events group into %d", stored, grouped)
	}
}

func queryRows(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("rows.Close() error = %v", err)
		}
	}()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
