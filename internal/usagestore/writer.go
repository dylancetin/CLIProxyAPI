package usagestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const dropWarnInterval = time.Minute

const insertEventSQL = `INSERT INTO usage_events (
    requested_at_ms, hour_start, trace_id, request_id, provider, executor_type,
    model, requested_model, account, auth_type, api_key_hash, api_key_hint,
    access_provider, reasoning_effort, service_tier, stream, failed, status_code,
    failure_class, latency_ms, ttft_ms, input_uncached, input_cache_read,
    input_cache_write, output_reasoning, output_other, unclassified, total_tokens, quality
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const upsertHourlySQL = `INSERT INTO usage_hourly (
    hour_start, provider, model, requested_model, account, api_key_hash, api_key_hint,
    reasoning_effort, service_tier, stream, requests, failures, input_uncached,
    input_cache_read, input_cache_write, output_reasoning, output_other, unclassified,
    total_tokens, latency_ms_sum, latency_ms_max, ttft_ms_sum, ttft_ms_max, ttft_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (hour_start, provider, model, requested_model, account, api_key_hash, reasoning_effort, service_tier, stream)
DO UPDATE SET
    requests = requests + 1,
    failures = failures + excluded.failures,
    input_uncached = input_uncached + excluded.input_uncached,
    input_cache_read = input_cache_read + excluded.input_cache_read,
    input_cache_write = input_cache_write + excluded.input_cache_write,
    output_reasoning = output_reasoning + excluded.output_reasoning,
    output_other = output_other + excluded.output_other,
    unclassified = unclassified + excluded.unclassified,
    total_tokens = total_tokens + excluded.total_tokens,
    latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
    latency_ms_max = MAX(latency_ms_max, excluded.latency_ms_max),
    ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum,
    ttft_ms_max = MAX(ttft_ms_max, excluded.ttft_ms_max),
    ttft_count = ttft_count + excluded.ttft_count`

// Plugin returns a usage.Plugin whose HandleUsage enqueues events for the
// batch writer.
func (s *Store) Plugin() usage.Plugin {
	return writerPlugin{store: s}
}

type writerPlugin struct {
	store *Store
}

// HandleUsage never blocks. The usage manager calls plugins serially from one
// goroutine, so a full queue drops the event instead of waiting on SQLite.
func (p writerPlugin) HandleUsage(_ context.Context, record usage.Record) {
	p.store.enqueue(EventFromRecord(record))
}

func (s *Store) enqueue(ev Event) {
	if ev.RequestedAt.IsZero() {
		ev.RequestedAt = s.now().UTC()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	default:
		s.recordDrop()
	}
}

func (s *Store) recordDrop() {
	dropped := s.dropped.Add(1)
	now := s.now().UnixNano()
	last := s.lastDropWarn.Load()
	if (last == 0 || now-last >= int64(dropWarnInterval)) && s.lastDropWarn.CompareAndSwap(last, now) {
		log.WithField("dropped_total", dropped).Warn("usagestore: event queue full, dropping usage events")
	}
}

func (s *Store) runWriter(wc writerConfig) {
	defer s.wg.Done()
	defer close(s.writerDone)
	if wc.hold != nil {
		select {
		case <-wc.hold:
		case <-s.stopCtx.Done():
		}
	}
	ticker := time.NewTicker(wc.flushInterval)
	defer ticker.Stop()
	batch := make([]Event, 0, wc.batchSize)
	for {
		select {
		case ev := <-s.events:
			batch = append(batch, ev)
			if len(batch) >= wc.batchSize {
				batch, _ = s.commit(batch)
			}
		case <-ticker.C:
			batch, _ = s.commit(batch)
		case reply := <-s.flushReq:
			var err error
			batch, err = s.commit(s.drain(batch))
			reply <- err
		case <-s.stopCtx.Done():
			// Close set closed before stopping us, so the queue can only shrink.
			_, s.writerErr = s.commit(s.drain(batch))
			return
		}
	}
}

// drain appends exactly the events queued at call time, so it terminates
// even while producers keep sending.
func (s *Store) drain(batch []Event) []Event {
	for range len(s.events) {
		batch = append(batch, <-s.events)
	}
	return batch
}

// commit writes batch and returns it emptied. A failed batch is logged and
// discarded; retrying would stall the queue behind a persistent error.
func (s *Store) commit(batch []Event) ([]Event, error) {
	if len(batch) == 0 {
		return batch, nil
	}
	err := s.writeBatch(context.Background(), batch)
	if err != nil {
		log.WithError(err).WithField("events", len(batch)).Error("usagestore: failed to write usage batch")
	}
	clear(batch)
	return batch[:0], err
}

func (s *Store) writeBatch(ctx context.Context, batch []Event) (err error) {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("usagestore: begin batch: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
			log.WithError(errRollback).Error("usagestore: rollback batch")
		}
	}()
	insertEvent := tx.StmtContext(ctx, s.insertEvent)
	upsertHourly := tx.StmtContext(ctx, s.upsertHourly)
	for i := range batch {
		ev := &batch[i]
		hour := hourStart(ev.RequestedAt)
		latencyMs := ev.Latency.Milliseconds()
		ttftMs := ceilMilliseconds(ev.TTFT)
		stream, failed := boolInt(ev.Stream), boolInt(ev.Failed)
		t := ev.Tokens
		if _, err = insertEvent.ExecContext(ctx,
			ev.RequestedAt.UnixMilli(), hour, ev.TraceID, ev.RequestID, ev.Provider, ev.ExecutorType,
			ev.Model, ev.RequestedModel, ev.Account, ev.AuthType, ev.APIKeyHash, ev.APIKeyHint,
			ev.AccessProvider, ev.ReasoningEffort, ev.ServiceTier, stream, failed, ev.StatusCode,
			string(ev.FailureClass), latencyMs, ttftMs, t.InputUncached, t.InputCacheRead,
			t.InputCacheWrite, t.OutputReasoning, t.OutputOther, t.Unclassified, t.Total, ev.Quality,
		); err != nil {
			return fmt.Errorf("usagestore: insert event: %w", err)
		}
		if _, err = upsertHourly.ExecContext(ctx,
			hour, ev.Provider, ev.Model, ev.RequestedModel, ev.Account, ev.APIKeyHash, ev.APIKeyHint,
			ev.ReasoningEffort, ev.ServiceTier, stream, failed, t.InputUncached,
			t.InputCacheRead, t.InputCacheWrite, t.OutputReasoning, t.OutputOther, t.Unclassified,
			t.Total, latencyMs, latencyMs, ttftMs, ttftMs, boolInt(ttftMs > 0),
		); err != nil {
			return fmt.Errorf("usagestore: upsert hourly: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usagestore: commit batch: %w", err)
	}
	return nil
}

// ceilMilliseconds rounds up so a measured sub-millisecond TTFT is stored as
// 1 ms instead of 0, which the schema reads as unknown.
func ceilMilliseconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
