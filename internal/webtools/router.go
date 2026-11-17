package webtools

import (
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// defaultCooldown pauses a provider that answered 429 without a usable
// Retry-After header.
const (
	defaultCooldown = 60 * time.Second
	maxCooldown     = 24 * time.Hour
	maxAffinities   = 1024
)

// router picks the provider for a search that names none, remembers which
// providers are rate limited, and supplies the next provider to try when one
// is. Provider choice:
//
//   - the user's default when it can search ("default" mode);
//   - in "auto" mode, the provider this app used last (sticky, so one
//     conversation keeps one provider's style), else a random one,
//     preferring providers with an API key over keyless ones.
//
// After a 429 the provider is paused for Retry-After (60 s when absent) and
// the next untried, unpaused provider is used, keyed ones first.
type router struct {
	now  func() time.Time
	pick func(n int) int

	mu        sync.Mutex
	cooldowns map[string]cooldown
	affinity  map[string]string
}

type cooldown struct {
	until time.Time
	// apiKey is the key the 429 was for; a new key (or a key added to a
	// keyless provider) is not paused.
	apiKey string
}

func newRouter(now func() time.Time) *router {
	return &router{
		now:       now,
		pick:      rand.IntN,
		cooldowns: map[string]cooldown{},
		affinity:  map[string]string{},
	}
}

// paused reports whether provider is cooling down for apiKey, and until when.
// Callers hold r.mu.
func (r *router) paused(provider, apiKey string) (time.Time, bool) {
	c, ok := r.cooldowns[provider]
	if !ok {
		return time.Time{}, false
	}
	if !r.now().Before(c.until) {
		delete(r.cooldowns, provider)
		return time.Time{}, false
	}
	return c.until, c.apiKey == apiKey
}

// next returns the provider to try for a search by appKey, skipping
// attempted and paused providers, or "" when none is left.
func (r *router) next(s config.WebSearchConfig, appKey string, attempted []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keyed, keyless []string
	open := func(name string) bool {
		if slices.Contains(attempted, name) || !s.CanSearch(name) {
			return false
		}
		_, paused := r.paused(name, s.Providers.APIKey(name))
		return !paused
	}
	for _, name := range s.Usable() {
		if !open(name) {
			continue
		}
		if s.Providers.APIKey(name) != "" {
			keyed = append(keyed, name)
		} else {
			keyless = append(keyless, name)
		}
	}
	auto := s.IsAuto()
	if !auto && open(s.Default) {
		return s.Default
	}
	if auto {
		if last := r.affinity[appKey]; last != "" && open(last) {
			return last
		}
	}
	group := keyed
	if len(group) == 0 {
		group = keyless
	}
	if len(group) == 0 {
		return ""
	}
	choice := group[r.pick(len(group))]
	if auto {
		if len(r.affinity) >= maxAffinities {
			clear(r.affinity)
		}
		r.affinity[appKey] = choice
	}
	return choice
}

// current is the provider a search by appKey would most likely use, for the
// probe. It has no side effects.
func (r *router) current(s config.WebSearchConfig, appKey string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	usable := s.Usable()
	if len(usable) == 0 {
		return ""
	}
	isOpen := func(name string) bool {
		_, paused := r.paused(name, s.Providers.APIKey(name))
		return slices.Contains(usable, name) && !paused
	}
	if !s.IsAuto() && isOpen(s.Default) {
		return s.Default
	}
	if s.IsAuto() {
		if last := r.affinity[appKey]; last != "" && isOpen(last) {
			return last
		}
	}
	// First open keyed provider, else first open keyless one, else the first usable.
	for _, wantKey := range []bool{true, false} {
		for _, name := range usable {
			if (s.Providers.APIKey(name) != "") == wantKey && isOpen(name) {
				return name
			}
		}
	}
	return usable[0]
}

// pause records a rate limit for provider.
func (r *router) pause(provider, apiKey, retryAfter string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.cooldowns[provider] = cooldown{until: now.Add(cooldownFor(retryAfter, now)), apiKey: apiKey}
}

// retryAfter is the shortest wait, in whole seconds (at least 1), until one
// of the usable providers is unpaused, or 0 when one is open now.
func (r *router) retryAfter(s config.WebSearchConfig) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var soonest time.Time
	for _, name := range s.Usable() {
		until, paused := r.paused(name, s.Providers.APIKey(name))
		if !paused {
			return 0
		}
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	return ceilSeconds(soonest.Sub(r.now()))
}

// pausedFor returns the remaining pause for one provider in seconds, or 0.
func (r *router) pausedFor(provider, apiKey string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, paused := r.paused(provider, apiKey)
	if !paused {
		return 0
	}
	return ceilSeconds(until.Sub(r.now()))
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int((d + time.Second - 1) / time.Second)
}

// cooldownFor parses Retry-After (seconds or an HTTP date).
func cooldownFor(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultCooldown
	}
	var d time.Duration
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return defaultCooldown
		}
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		d = at.Sub(now)
		if d < 0 {
			d = 0
		}
	} else {
		return defaultCooldown
	}
	return min(d, maxCooldown)
}
