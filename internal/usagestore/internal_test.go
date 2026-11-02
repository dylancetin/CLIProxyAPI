package usagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func openWithWriter(tb testing.TB, path string, wc writerConfig) *Store {
	tb.Helper()
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := open(context.Background(), Options{Path: path, Now: func() time.Time { return now }}, wc)
	if err != nil {
		tb.Fatalf("open() error = %v", err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Errorf("Close() error = %v", err)
		}
	})
	return s
}

func TestOpenAppliesPragmasToEveryHandle(t *testing.T) {
	s := openWithWriter(t, filepath.Join(t.TempDir(), "usage.db"), defaultWriterConfig())
	for _, tt := range []struct {
		name   string
		db     *sql.DB
		pragma string
		want   string
	}{
		{"write", s.writeDB, "journal_mode", "wal"},
		{"write", s.writeDB, "synchronous", "1"},
		{"write", s.writeDB, "busy_timeout", "5000"},
		{"write", s.writeDB, "foreign_keys", "1"},
		{"read", s.readDB, "busy_timeout", "5000"},
		{"read", s.readDB, "foreign_keys", "1"},
		{"read", s.readDB, "query_only", "1"},
	} {
		var got string
		if err := tt.db.QueryRow("PRAGMA " + tt.pragma).Scan(&got); err != nil {
			t.Fatalf("%s PRAGMA %s: %v", tt.name, tt.pragma, err)
		}
		if got != tt.want {
			t.Fatalf("%s PRAGMA %s = %q, want %q", tt.name, tt.pragma, got, tt.want)
		}
	}
}

func TestHandleUsageDropsWhenQueueFull(t *testing.T) {
	hold := make(chan struct{})
	wc := defaultWriterConfig()
	wc.queueCapacity, wc.hold = 4, hold
	s := openWithWriter(t, filepath.Join(t.TempDir(), "usage.db"), wc)

	plugin := s.Plugin()
	for i := range 10 {
		plugin.HandleUsage(context.Background(), usage.Record{RequestID: fmt.Sprint(i), Provider: "claude"})
	}
	if got := s.Dropped(); got != 6 {
		t.Fatalf("Dropped() = %d, want 6", got)
	}

	close(hold)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("usage_events rows = %d, want the 4 queued events", n)
	}
}

func TestFlushReturnsWhenContextEnds(t *testing.T) {
	wc := defaultWriterConfig()
	wc.hold = make(chan struct{})
	s := openWithWriter(t, filepath.Join(t.TempDir(), "usage.db"), wc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush() with a paused writer = %v, want context.Canceled", err)
	}
}

func TestCloseDrainsQueueOfPausedWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	wc := defaultWriterConfig()
	wc.hold = make(chan struct{})
	s := openWithWriter(t, path, wc)
	for i := range 5 {
		s.Plugin().HandleUsage(context.Background(), usage.Record{RequestID: fmt.Sprint(i)})
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("usage_events rows after Close = %d, want 5", n)
	}
}

// BenchmarkWriteBatch measures one-transaction batch commits of the default size.
func BenchmarkWriteBatch(b *testing.B) {
	s := openWithWriter(b, filepath.Join(b.TempDir(), "usage.db"), defaultWriterConfig())
	batch := make([]Event, defaultBatchSize)
	start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for i := range batch {
		batch[i] = EventFromRecord(usage.Record{
			RequestedAt: start.Add(time.Duration(i) * time.Minute),
			RequestID:   fmt.Sprint(i),
			Provider:    []string{"claude", "codex", "gemini"}[i%3],
			Model:       []string{"model-a", "model-b"}[i%2],
			AuthID:      fmt.Sprintf("acct-%d.json", i%4),
			APIKey:      fmt.Sprintf("sk-client-key-%04d", i%5),
			Latency:     time.Duration(100+i) * time.Millisecond,
			TTFT:        time.Duration(i%3) * 10 * time.Millisecond,
			Detail:      usage.Detail{TokenBreakdown: usage.NewIndependentTokenBreakdown(1000, 200, 0, 300, 50, 0)},
		})
	}
	ctx := context.Background()
	for b.Loop() {
		if err := s.writeBatch(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*len(batch))/b.Elapsed().Seconds(), "events/s")
}
