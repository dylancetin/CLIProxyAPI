package usagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	// Registers the pure Go "sqlite" database/sql driver, keeping builds cgo-free.
	_ "modernc.org/sqlite"
)

const (
	defaultQueueCapacity = 4096
	defaultBatchSize     = 256
	defaultFlushInterval = time.Second
	sweepInterval        = time.Hour
	readConnections      = 4
)

// ErrClosed is returned by Flush once Close has started.
var ErrClosed = errors.New("usagestore: store is closed")

// writerConfig holds batch writer tunables; tests shrink them.
type writerConfig struct {
	queueCapacity int
	batchSize     int
	flushInterval time.Duration
	// hold delays the writer loop until it is closed, letting tests fill the queue.
	hold <-chan struct{}
}

func defaultWriterConfig() writerConfig {
	return writerConfig{
		queueCapacity: defaultQueueCapacity,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
	}
}

// Store is a SQLite-backed usage event store.
//
// It holds two handles on the same file: a single-connection write handle
// that serializes batches and sweeps, and a small read pool. In WAL mode
// readers see the last committed snapshot, so queries never wait on a
// batch commit.
type Store struct {
	now     func() time.Time
	writeDB *sql.DB
	readDB  *sql.DB

	insertEvent  *sql.Stmt
	upsertHourly *sql.Stmt

	rawRetention    atomic.Int64
	hourlyRetention atomic.Int64

	// mu guards closed so that no event is enqueued after Close starts the
	// final drain; HandleUsage only takes the read lock.
	mu       sync.RWMutex
	closed   bool
	events   chan Event
	flushReq chan chan error

	stopCtx    context.Context
	stop       context.CancelFunc
	writerDone chan struct{}
	writerErr  error
	wg         sync.WaitGroup

	dropped      atomic.Int64
	lastDropWarn atomic.Int64

	closeOnce sync.Once
	closeErr  error
}

// Open opens or creates the database at opts.Path, runs migrations, and
// configures WAL and busy_timeout.
func Open(ctx context.Context, opts Options) (*Store, error) {
	return open(ctx, opts, defaultWriterConfig())
}

func open(ctx context.Context, opts Options, wc writerConfig) (_ *Store, err error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("usagestore: empty database path")
	}
	path, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("usagestore: resolve path: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("usagestore: create directory: %w", err)
	}
	// Create the file ourselves so it is private; SQLite gives the WAL and
	// shm files the same mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("usagestore: create database file: %w", err)
	}
	if err = f.Close(); err != nil {
		return nil, fmt.Errorf("usagestore: create database file: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	stopCtx, stop := context.WithCancel(context.Background())
	s := &Store{
		now:        now,
		events:     make(chan Event, wc.queueCapacity),
		flushReq:   make(chan chan error),
		stopCtx:    stopCtx,
		stop:       stop,
		writerDone: make(chan struct{}),
	}
	defer func() {
		if err != nil {
			stop()
			err = errors.Join(err, s.closeHandles())
		}
	}()

	s.writeDB, err = sql.Open("sqlite", sqliteDSN(path, url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(ON)"},
		"_txlock": {"immediate"},
	}))
	if err != nil {
		return nil, fmt.Errorf("usagestore: open write handle: %w", err)
	}
	s.writeDB.SetMaxOpenConns(1)
	if err = migrate(ctx, s.writeDB); err != nil {
		return nil, err
	}
	s.readDB, err = sql.Open("sqlite", sqliteDSN(path, url.Values{
		"_pragma": {"busy_timeout(5000)", "foreign_keys(ON)", "query_only(1)"},
	}))
	if err != nil {
		return nil, fmt.Errorf("usagestore: open read handle: %w", err)
	}
	s.readDB.SetMaxOpenConns(readConnections)
	s.readDB.SetMaxIdleConns(readConnections)
	if s.insertEvent, err = s.writeDB.PrepareContext(ctx, insertEventSQL); err != nil {
		return nil, fmt.Errorf("usagestore: prepare event insert: %w", err)
	}
	if s.upsertHourly, err = s.writeDB.PrepareContext(ctx, upsertHourlySQL); err != nil {
		return nil, fmt.Errorf("usagestore: prepare hourly upsert: %w", err)
	}

	s.SetRetention(opts.RawRetention, opts.HourlyRetention)
	if err = s.Sweep(ctx); err != nil {
		return nil, err
	}

	s.wg.Add(2)
	go s.runWriter(wc)
	go s.runSweeper()
	return s, nil
}

// sqliteDSN builds a file: URI so paths containing '?' or '#' survive and
// the pragmas apply to every pooled connection.
func sqliteDSN(path string, params url.Values) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: params.Encode()}).String()
}

// Close flushes pending events, stops background work, and closes the database.
// It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.stop()
		s.wg.Wait()
		s.closeErr = errors.Join(s.writerErr, s.closeHandles())
	})
	return s.closeErr
}

func (s *Store) closeHandles() error {
	var errs []error
	for _, stmt := range []*sql.Stmt{s.insertEvent, s.upsertHourly} {
		if stmt != nil {
			errs = append(errs, stmt.Close())
		}
	}
	for _, db := range []*sql.DB{s.readDB, s.writeDB} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	return errors.Join(errs...)
}

// Flush writes every event queued before the call and waits for the commit.
func (s *Store) Flush(ctx context.Context) error {
	reply := make(chan error, 1)
	select {
	case s.flushReq <- reply:
	case <-s.writerDone:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped reports how many events were discarded because the queue was full.
func (s *Store) Dropped() int64 {
	return s.dropped.Load()
}

// SetRetention updates retention windows at runtime; 0 keeps rows forever.
func (s *Store) SetRetention(raw, hourly time.Duration) {
	s.rawRetention.Store(int64(raw))
	s.hourlyRetention.Store(int64(hourly))
}

// Sweep applies retention. It also runs periodically while the Store is open.
func (s *Store) Sweep(ctx context.Context) error {
	now := s.now()
	if raw := time.Duration(s.rawRetention.Load()); raw > 0 {
		cutoff := now.Add(-raw)
		// The hour_start bound is implied by the timestamp bound; it lets the
		// delete use the hour index instead of scanning the table.
		if _, err := s.writeDB.ExecContext(ctx,
			`DELETE FROM usage_events WHERE hour_start <= ? AND requested_at_ms < ?`,
			hourStart(cutoff), cutoff.UnixMilli()); err != nil {
			return fmt.Errorf("usagestore: sweep events: %w", err)
		}
	}
	if hourly := time.Duration(s.hourlyRetention.Load()); hourly > 0 {
		// Keep an hour bucket while any part of it is inside the window.
		cutoff := now.Add(-hourly).Unix() - int64(time.Hour/time.Second)
		if _, err := s.writeDB.ExecContext(ctx, `DELETE FROM usage_hourly WHERE hour_start <= ?`, cutoff); err != nil {
			return fmt.Errorf("usagestore: sweep hourly: %w", err)
		}
	}
	return nil
}

func (s *Store) runSweeper() {
	defer s.wg.Done()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-ticker.C:
			if err := s.Sweep(s.stopCtx); err != nil && s.stopCtx.Err() == nil {
				log.WithError(err).Error("usagestore: retention sweep failed")
			}
		}
	}
}

func hourStart(t time.Time) int64 {
	return t.Truncate(time.Hour).Unix()
}
