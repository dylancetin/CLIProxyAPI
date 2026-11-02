package usagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// EventFromRecord parses a usage.Record at the boundary.
// Record.Source is dropped and Record.APIKey is kept only as a hash and a
// masked hint, because either can carry a raw secret.
func EventFromRecord(r usage.Record) Event {
	breakdown := usage.EnsureTokenBreakdownForProvider(r.Detail, r.Provider, r.ExecutorType).TokenBreakdown
	apiKey := strings.TrimSpace(r.APIKey)
	return Event{
		RequestedAt:     r.RequestedAt.UTC(),
		TraceID:         strings.TrimSpace(r.TraceID),
		RequestID:       strings.TrimSpace(r.RequestID),
		Provider:        strings.TrimSpace(r.Provider),
		ExecutorType:    strings.TrimSpace(r.ExecutorType),
		Model:           firstNonEmpty(r.ResponseModel, r.Model),
		RequestedModel:  firstNonEmpty(r.Alias, r.Model),
		Account:         strings.TrimSpace(r.AuthID),
		AuthType:        strings.TrimSpace(r.AuthType),
		APIKeyHash:      hashAPIKey(apiKey),
		APIKeyHint:      maskAPIKey(apiKey),
		AccessProvider:  strings.TrimSpace(r.AccessProvider),
		ReasoningEffort: strings.TrimSpace(r.ReasoningEffort),
		// The upstream-reported tier is what was served and billed; the
		// requested tier is only a fallback.
		ServiceTier:  firstNonEmpty(r.ResponseServiceTier, r.Detail.ResponseServiceTier, r.ServiceTier, r.RequestServiceTier),
		Stream:       r.Stream,
		Failed:       r.Failed,
		StatusCode:   r.Fail.StatusCode,
		FailureClass: classifyFailure(r.Failed, r.Fail.StatusCode),
		Latency:      max(r.Latency, 0),
		TTFT:         max(r.TTFT, 0),
		Tokens: Tokens{
			InputUncached:   breakdown.Input.UncachedTokens,
			InputCacheRead:  breakdown.Input.CacheReadTokens,
			InputCacheWrite: breakdown.Input.CacheWriteTokens,
			OutputReasoning: breakdown.Output.ReasoningTokens,
			OutputOther:     breakdown.Output.NonReasoningTokens,
			Unclassified:    breakdown.UnclassifiedTokens,
			Total:           breakdown.TotalTokens,
		},
		Quality: string(breakdown.Quality),
	}
}

func classifyFailure(failed bool, status int) FailureClass {
	switch {
	case !failed:
		return FailureNone
	case status <= 0:
		return FailureNetwork
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return FailureAuth401403
	case status == http.StatusTooManyRequests:
		return FailureRateLimit429
	case status >= 400 && status < 500:
		return FailureClient4xx
	case status >= 500 && status < 600:
		return FailureServer5xx
	default:
		return FailureOther
	}
}

func hashAPIKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	// Reveal at most a quarter of the key so short keys are not mostly disclosed.
	n := min(4, len(runes)/8)
	if n == 0 {
		return "…"
	}
	return string(runes[:n]) + "…" + string(runes[len(runes)-n:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
