package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicUnifiedRateLimitHeaderPrefix = "Anthropic-Ratelimit-Unified-"
	anthropicUnifiedStatusHeader          = "Anthropic-Ratelimit-Unified-Status"
	anthropicUnifiedResetHeader           = "Anthropic-Ratelimit-Unified-Reset"
)

// IsAnthropicUnifiedRateLimitHeader reports whether name is one of Anthropic's
// anthropic-ratelimit-unified-* response headers.
func IsAnthropicUnifiedRateLimitHeader(name string) bool {
	return strings.HasPrefix(http.CanonicalHeaderKey(strings.TrimSpace(name)), anthropicUnifiedRateLimitHeaderPrefix)
}

// claudeCooldownRateLimitSignals returns the stored upstream unified rate-limit
// snapshot when auth is a Claude credential held in an active quota cooldown for
// model by an Anthropic unified rate-limit rejection. It returns nil for other
// providers and for cooldowns caused by transient failures, so those keep the
// plain Retry-After response that clients retry automatically.
func claudeCooldownRateLimitSignals(auth *Auth, model string, now time.Time) map[string]string {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return nil
	}
	candidates := make([]*QuotaState, 0, 2)
	if auth.Quota.Reason == "credential_quota" {
		candidates = append(candidates, &auth.Quota)
	}
	if model != "" {
		modelKey := canonicalModelKey(model)
		for stateModel, state := range auth.ModelStates {
			if state != nil && canonicalModelKey(stateModel) == modelKey {
				candidates = append(candidates, &state.Quota)
			}
		}
	}
	var best *QuotaState
	for _, quota := range candidates {
		if !quota.Exceeded || !quota.NextRecoverAt.After(now) || !quotaSignalsShowClaudeRejection(quota.Signals) {
			continue
		}
		if best == nil || quota.ObservedAt.After(best.ObservedAt) {
			best = quota
		}
	}
	if best == nil {
		return nil
	}
	return best.Clone().Signals
}

// quotaSignalsShowClaudeRejection reports whether a stored snapshot came from an
// Anthropic response that rejected the request on a unified rate-limit window.
func quotaSignalsShowClaudeRejection(signals map[string]string) bool {
	for _, name := range []string{
		anthropicUnifiedStatusHeader,
		"Anthropic-Ratelimit-Unified-5h-Status",
		"Anthropic-Ratelimit-Unified-7d-Status",
	} {
		if strings.EqualFold(strings.TrimSpace(signals[name]), "rejected") {
			return true
		}
	}
	return false
}

// attachClaudeCooldownRateLimitHeaders replays the upstream unified rate-limit
// headers on a model cooldown error, so clients such as Claude Code can report
// the limit and its reset time. The status is forced to "rejected" and the reset
// is set to the proxy's own cooldown deadline, which is when the proxy will
// accept requests again. Cooldown errors are built per selection, so the error
// is updated in place.
func attachClaudeCooldownRateLimitHeaders(err error, signals map[string]string, now time.Time) {
	if len(signals) == 0 {
		return
	}
	var cooldownErr *modelCooldownError
	if !errors.As(err, &cooldownErr) || cooldownErr == nil {
		return
	}
	headers := make(http.Header)
	for name, value := range signals {
		if IsAnthropicUnifiedRateLimitHeader(name) {
			headers.Set(name, value)
		}
	}
	resetAt := now.Add(cooldownErr.resetIn)
	resetUnix := resetAt.Unix()
	if resetAt.Nanosecond() > 0 {
		resetUnix++
	}
	headers.Set(anthropicUnifiedStatusHeader, "rejected")
	headers.Set(anthropicUnifiedResetHeader, strconv.FormatInt(resetUnix, 10))
	cooldownErr.rateLimitHeaders = headers
}
