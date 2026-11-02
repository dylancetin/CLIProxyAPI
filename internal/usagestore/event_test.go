package usagestore_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestEventFromRecordCopiesIdentityFields(t *testing.T) {
	ev := usagestore.EventFromRecord(usage.Record{
		RequestID:       "req-1",
		TraceID:         "trace-1",
		Provider:        "claude",
		ExecutorType:    "ClaudeExecutor",
		Model:           "claude-opus-5-5",
		AuthID:          "claude-user.json",
		AuthType:        "oauth",
		AccessProvider:  "config-inline",
		ReasoningEffort: "high",
		Latency:         1500 * time.Millisecond,
		TTFT:            200 * time.Millisecond,
	})

	want := usagestore.Event{
		RequestID:       "req-1",
		TraceID:         "trace-1",
		Provider:        "claude",
		ExecutorType:    "ClaudeExecutor",
		Model:           "claude-opus-5-5",
		RequestedModel:  "claude-opus-5-5",
		Account:         "claude-user.json",
		AuthType:        "oauth",
		AccessProvider:  "config-inline",
		ReasoningEffort: "high",
		Latency:         1500 * time.Millisecond,
		TTFT:            200 * time.Millisecond,
		Quality:         string(usage.TokenAccountingQualityComplete),
	}
	if ev != want {
		t.Fatalf("EventFromRecord() =\n%+v\nwant\n%+v", ev, want)
	}
}

func TestEventFromRecordResolvesModels(t *testing.T) {
	tests := []struct {
		name           string
		record         usage.Record
		model          string
		requestedModel string
	}{
		{
			name:           "no alias",
			record:         usage.Record{Model: "gpt-5"},
			model:          "gpt-5",
			requestedModel: "gpt-5",
		},
		{
			name:           "alias is the requested model",
			record:         usage.Record{Model: "gpt-5", Alias: "fast"},
			model:          "gpt-5",
			requestedModel: "fast",
		},
		{
			name:           "response model overrides upstream model",
			record:         usage.Record{Model: "gpt-5", Alias: "fast", ResponseModel: "gpt-5-2026-08-01"},
			model:          "gpt-5-2026-08-01",
			requestedModel: "fast",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := usagestore.EventFromRecord(tt.record)
			if ev.Model != tt.model || ev.RequestedModel != tt.requestedModel {
				t.Fatalf("Model, RequestedModel = %q, %q; want %q, %q", ev.Model, ev.RequestedModel, tt.model, tt.requestedModel)
			}
		})
	}
}

func TestEventFromRecordHashesClientKey(t *testing.T) {
	const key = "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	const source = "upstream-secret-abcdefghijklmnop"
	ev := usagestore.EventFromRecord(usage.Record{APIKey: key, Source: source})

	sum := sha256.Sum256([]byte(key))
	if want := hex.EncodeToString(sum[:]); ev.APIKeyHash != want {
		t.Fatalf("APIKeyHash = %q, want %q", ev.APIKeyHash, want)
	}
	if want := "sk-a…6789"; ev.APIKeyHint != want {
		t.Fatalf("APIKeyHint = %q, want %q", ev.APIKeyHint, want)
	}
	dump := fmt.Sprintf("%+v", ev)
	if strings.Contains(dump, key) || strings.Contains(dump, source) {
		t.Fatalf("event leaks a raw secret: %s", dump)
	}
}

func TestEventFromRecordHintRevealsAtMostAQuarter(t *testing.T) {
	tests := []struct {
		key  string
		hint string
	}{
		{key: "short", hint: "…"},
		{key: "my-key-123", hint: "m…3"},
		{key: "0123456789abcdef", hint: "01…ef"},
	}
	for _, tt := range tests {
		ev := usagestore.EventFromRecord(usage.Record{APIKey: tt.key})
		if ev.APIKeyHint != tt.hint {
			t.Errorf("hint for %d-char key = %q, want %q", len(tt.key), ev.APIKeyHint, tt.hint)
		}
		if ev.APIKeyHash == "" {
			t.Errorf("hash for %d-char key is empty", len(tt.key))
		}
	}
}

func TestEventFromRecordWithoutClientKey(t *testing.T) {
	ev := usagestore.EventFromRecord(usage.Record{APIKey: "  "})
	if ev.APIKeyHash != "" || ev.APIKeyHint != "" {
		t.Fatalf("APIKeyHash, APIKeyHint = %q, %q; want empty", ev.APIKeyHash, ev.APIKeyHint)
	}
}

func TestEventFromRecordClassifiesFailures(t *testing.T) {
	tests := []struct {
		name   string
		failed bool
		status int
		want   usagestore.FailureClass
	}{
		{name: "success", failed: false, status: 0, want: usagestore.FailureNone},
		{name: "401", failed: true, status: 401, want: usagestore.FailureAuth401403},
		{name: "403", failed: true, status: 403, want: usagestore.FailureAuth401403},
		{name: "429", failed: true, status: 429, want: usagestore.FailureRateLimit429},
		{name: "400", failed: true, status: 400, want: usagestore.FailureClient4xx},
		{name: "499 client closed", failed: true, status: 499, want: usagestore.FailureClient4xx},
		{name: "500", failed: true, status: 500, want: usagestore.FailureServer5xx},
		{name: "503", failed: true, status: 503, want: usagestore.FailureServer5xx},
		{name: "network without status", failed: true, status: 0, want: usagestore.FailureNetwork},
		{name: "failed with non-error status", failed: true, status: 200, want: usagestore.FailureOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := usagestore.EventFromRecord(usage.Record{Failed: tt.failed, Fail: usage.Failure{StatusCode: tt.status, Body: "boom"}})
			if ev.FailureClass != tt.want {
				t.Fatalf("FailureClass = %q, want %q", ev.FailureClass, tt.want)
			}
			if ev.Failed != tt.failed || ev.StatusCode != tt.status {
				t.Fatalf("Failed, StatusCode = %v, %d; want %v, %d", ev.Failed, ev.StatusCode, tt.failed, tt.status)
			}
		})
	}
}

func TestEventFromRecordStreamFlag(t *testing.T) {
	if !usagestore.EventFromRecord(usage.Record{Stream: true}).Stream {
		t.Fatal("Stream = false, want true")
	}
	if usagestore.EventFromRecord(usage.Record{}).Stream {
		t.Fatal("Stream = true, want false")
	}
}

func TestEventFromRecordNormalizesToUTC(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.FixedZone("UTC+5", 5*3600))
	ev := usagestore.EventFromRecord(usage.Record{RequestedAt: at})
	if ev.RequestedAt.Location() != time.UTC || !ev.RequestedAt.Equal(at) {
		t.Fatalf("RequestedAt = %v, want %v in UTC", ev.RequestedAt, at.UTC())
	}
}

func TestEventFromRecordServiceTier(t *testing.T) {
	tests := []struct {
		name   string
		record usage.Record
		want   string
	}{
		{name: "response tier wins", record: usage.Record{ServiceTier: "auto", ResponseServiceTier: "priority"}, want: "priority"},
		{name: "detail response tier", record: usage.Record{ServiceTier: "auto", Detail: usage.Detail{ResponseServiceTier: "flex"}}, want: "flex"},
		{name: "requested tier fallback", record: usage.Record{ServiceTier: "default"}, want: "default"},
		{name: "deprecated request tier", record: usage.Record{RequestServiceTier: "priority"}, want: "priority"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usagestore.EventFromRecord(tt.record).ServiceTier; got != tt.want {
				t.Fatalf("ServiceTier = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEventFromRecordMapsTokens(t *testing.T) {
	tests := []struct {
		name    string
		record  usage.Record
		tokens  usagestore.Tokens
		quality usage.TokenAccountingQuality
	}{
		{
			name: "canonical breakdown",
			record: usage.Record{Detail: usage.Detail{
				TokenBreakdown: usage.NewIndependentTokenBreakdown(100, 20, 10, 50, 5, 0),
			}},
			tokens: usagestore.Tokens{
				InputUncached: 100, InputCacheRead: 20, InputCacheWrite: 10,
				OutputOther: 50, OutputReasoning: 5, Total: 185,
			},
			quality: usage.TokenAccountingQualityComplete,
		},
		{
			name: "unclassified total",
			record: usage.Record{Detail: usage.Detail{
				TokenBreakdown: usage.NewUnclassifiedTokenBreakdown(70),
			}},
			tokens:  usagestore.Tokens{Unclassified: 70, Total: 70},
			quality: usage.TokenAccountingQualityUnclassified,
		},
		{
			name: "legacy detail is normalized for the provider",
			record: usage.Record{Provider: "codex", Detail: usage.Detail{
				InputTokens: 100, CacheReadTokens: 40, OutputTokens: 30, ReasoningTokens: 10,
			}},
			tokens: usagestore.Tokens{
				InputUncached: 60, InputCacheRead: 40,
				OutputOther: 20, OutputReasoning: 10, Total: 130,
			},
			quality: usage.TokenAccountingQualityComplete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := usagestore.EventFromRecord(tt.record)
			if ev.Tokens != tt.tokens {
				t.Fatalf("Tokens = %+v, want %+v", ev.Tokens, tt.tokens)
			}
			if ev.Quality != string(tt.quality) {
				t.Fatalf("Quality = %q, want %q", ev.Quality, tt.quality)
			}
		})
	}
}
