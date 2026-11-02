package auth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// claudeRejectionSignals mirrors the quota snapshot stored from an Anthropic
// 429 that rejected the request on the shared 5-hour window.
func claudeRejectionSignals(upstreamReset time.Time) map[string]string {
	reset := strconv.FormatInt(upstreamReset.Unix(), 10)
	return map[string]string{
		"Anthropic-Ratelimit-Unified-Status":               "rejected",
		"Anthropic-Ratelimit-Unified-5h-Status":            "rejected",
		"Anthropic-Ratelimit-Unified-5h-Reset":             reset,
		"Anthropic-Ratelimit-Unified-Reset":                reset,
		"Anthropic-Ratelimit-Unified-Representative-Claim": "five_hour",
		"Anthropic-Ratelimit-Unified-Overage-Status":       "rejected",
		"Retry-After": "120",
	}
}

// claudeThrottleSignals mirrors a short-lived 429 where the unified limiter
// still allowed the request.
func claudeThrottleSignals() map[string]string {
	return map[string]string{
		"Anthropic-Ratelimit-Unified-Status":    "allowed",
		"Anthropic-Ratelimit-Unified-5h-Status": "allowed",
		"Retry-After":                           "30",
	}
}

func credentialQuotaAuth(id, provider string, next time.Time, signals map[string]string) *Auth {
	return &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "credential_quota",
			NextRecoverAt: next,
			ObservedAt:    time.Now().Add(-time.Minute),
			Signals:       signals,
		},
	}
}

func modelQuotaAuth(id, provider, model string, next time.Time, signals map[string]string) *Auth {
	auth := &Auth{
		ID:       id,
		Provider: provider,
		Status:   StatusActive,
		ModelStates: map[string]*ModelState{
			model: {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: next,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: next,
					ObservedAt:    time.Now().Add(-time.Minute),
					Signals:       signals,
				},
			},
		},
	}
	updateAggregatedAvailability(auth, time.Now())
	return auth
}

// executeAgainstCoolingAuths runs a request through Manager.Execute while every
// candidate is cooling and returns the resulting cooldown error.
func executeAgainstCoolingAuths(t *testing.T, provider, model string, auths ...*Auth) *modelCooldownError {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{id: provider}
	manager.RegisterExecutor(executor)
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		reg.RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
		authID := auth.ID
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		manager.RefreshSchedulerEntry(auth.ID)
	}

	_, errExecute := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if calls := executor.ExecuteCalls(); len(calls) != 0 {
		t.Fatalf("executor called for %v, want no upstream call while cooling", calls)
	}
	var cooldownErr *modelCooldownError
	if !errors.As(errExecute, &cooldownErr) || cooldownErr == nil {
		t.Fatalf("Execute() error = %T (%v), want *modelCooldownError", errExecute, errExecute)
	}
	return cooldownErr
}

func TestClaudeCooldownReplaysUnifiedRateLimitRejection(t *testing.T) {
	// The half-second offset keeps the rounded-up reset deterministic.
	next := time.Now().Add(3 * time.Hour).Truncate(time.Second).Add(500 * time.Millisecond)
	wantReset := strconv.FormatInt(next.Truncate(time.Second).Unix()+1, 10)
	upstreamReset := next.Add(-20 * time.Second)

	tests := []struct {
		name  string
		model string
		auth  *Auth
	}{
		{
			name:  "credential-wide 5h rejection",
			model: "claude-replay-credential-model",
			auth:  credentialQuotaAuth("claude-replay-credential", "claude", next, claudeRejectionSignals(upstreamReset)),
		},
		{
			name:  "model-scoped rejection",
			model: "claude-replay-model-model",
			auth:  modelQuotaAuth("claude-replay-model", "claude", "claude-replay-model-model", next, claudeRejectionSignals(upstreamReset)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cooldownErr := executeAgainstCoolingAuths(t, "claude", tt.model, tt.auth)
			headers := SafeResponseHeaders(cooldownErr)

			if got := headers.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
				t.Fatalf("unified status = %q, want rejected", got)
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Reset"); got != wantReset {
				t.Fatalf("unified reset = %q, want proxy cooldown deadline %s", got, wantReset)
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Representative-Claim"); got != "five_hour" {
				t.Fatalf("representative claim = %q, want five_hour", got)
			}
			if got := headers.Get("Anthropic-Ratelimit-Unified-Overage-Status"); got != "rejected" {
				t.Fatalf("overage status = %q, want replayed rejected", got)
			}
			wantUpstreamReset := strconv.FormatInt(upstreamReset.Unix(), 10)
			if got := headers.Get("Anthropic-Ratelimit-Unified-5h-Reset"); got != wantUpstreamReset {
				t.Fatalf("5h reset = %q, want upstream value %s", got, wantUpstreamReset)
			}
			if got := headers.Get("Retry-After"); got == "" || got == "120" {
				t.Fatalf("Retry-After = %q, want the proxy cooldown seconds, not the stored upstream value", got)
			}
		})
	}
}

func TestClaudeCooldownOmitsUnifiedHeadersWithoutRejection(t *testing.T) {
	next := time.Now().Add(time.Hour)
	tests := []struct {
		name     string
		provider string
		model    string
		auths    []*Auth
	}{
		{
			name:     "short throttle allowed by the unified limiter",
			provider: "claude",
			model:    "claude-omit-throttle-model",
			auths: []*Auth{
				modelQuotaAuth("claude-omit-throttle", "claude", "claude-omit-throttle-model", next, claudeThrottleSignals()),
			},
		},
		{
			name:     "non-claude provider",
			provider: "codex",
			model:    "claude-omit-codex-model",
			auths: []*Auth{
				credentialQuotaAuth("claude-omit-codex", "codex", next, claudeRejectionSignals(next)),
			},
		},
		{
			name:     "earliest-recovering candidate is only throttled",
			provider: "claude",
			model:    "claude-omit-mixed-model",
			auths: []*Auth{
				credentialQuotaAuth("claude-omit-mixed-rejected", "claude", next.Add(2*time.Hour), claudeRejectionSignals(next.Add(2*time.Hour))),
				modelQuotaAuth("claude-omit-mixed-throttled", "claude", "claude-omit-mixed-model", time.Now().Add(30*time.Second), claudeThrottleSignals()),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cooldownErr := executeAgainstCoolingAuths(t, tt.provider, tt.model, tt.auths...)
			headers := SafeResponseHeaders(cooldownErr)

			for name := range headers {
				if IsAnthropicUnifiedRateLimitHeader(name) {
					t.Fatalf("unexpected unified rate-limit header %s=%q", name, headers.Get(name))
				}
			}
			if headers.Get("Retry-After") == "" {
				t.Fatal("Retry-After missing, want the plain cooldown response")
			}
		})
	}
}

func TestRestoreModelCooldownErrorModelKeepsRateLimitHeaders(t *testing.T) {
	cooldownErr := newModelCooldownError("", "claude", time.Minute)
	attachClaudeCooldownRateLimitHeaders(cooldownErr, claudeRejectionSignals(time.Now()), time.Now())

	restored := restoreModelCooldownErrorModel(cooldownErr, "claude-opus-route")
	if got := SafeResponseHeaders(restored).Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("restored unified status = %q, want rejected", got)
	}
}
