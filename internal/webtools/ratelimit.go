package webtools

import (
	"math"
	"net/http"
	"sync"
	"time"
)

// Per-key limits.
const (
	SearchPerMinute   = 20
	FetchPerMinute    = 40
	MaxConcurrentKey  = 4
	FetchMaxChars     = 20000
	rateWindow        = time.Minute
	limiterIdleExpiry = 10 * time.Minute
)

type keyState struct {
	searches []time.Time
	fetches  []time.Time
	inFlight int
	lastSeen time.Time
}

// limiter enforces sliding one-minute windows per key and per tool, and a cap
// on concurrent tool calls per key.
type limiter struct {
	mu   sync.Mutex
	now  func() time.Time
	keys map[string]*keyState
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, keys: map[string]*keyState{}}
}

func prune(times []time.Time, now time.Time) []time.Time {
	cut := 0
	for cut < len(times) && now.Sub(times[cut]) >= rateWindow {
		cut++
	}
	return times[cut:]
}

func rateLimited(retryAfter time.Duration, what string) *toolError {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return &toolError{HTTPStatus: http.StatusTooManyRequests, Code: CodeRateLimited, Message: what, RetryAfter: seconds}
}

// acquire records one call of tool ("search" or "fetch") for key and returns
// a release function, or a rate_limited error.
func (l *limiter) acquire(key, tool string) (func(), *toolError) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.keys[cacheKey(key, "")]
	if state == nil {
		l.cleanupLocked(now)
		state = &keyState{}
		l.keys[cacheKey(key, "")] = state
	}
	state.lastSeen = now
	state.searches = prune(state.searches, now)
	state.fetches = prune(state.fetches, now)
	if state.inFlight >= MaxConcurrentKey {
		return nil, rateLimited(time.Second, "too many tool calls at once for this key")
	}
	window, limit := &state.fetches, FetchPerMinute
	if tool == "search" {
		window, limit = &state.searches, SearchPerMinute
	}
	if len(*window) >= limit {
		return nil, rateLimited((*window)[0].Add(rateWindow).Sub(now), "per-minute limit reached for this key")
	}
	*window = append(*window, now)
	state.inFlight++
	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !released {
			released = true
			state.inFlight--
		}
	}, nil
}

func (l *limiter) cleanupLocked(now time.Time) {
	for key, state := range l.keys {
		if state.inFlight == 0 && now.Sub(state.lastSeen) > limiterIdleExpiry {
			delete(l.keys, key)
		}
	}
}
