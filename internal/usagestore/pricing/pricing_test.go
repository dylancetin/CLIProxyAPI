package pricing_test

import (
	"math"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/pricing"
)

const million = 1_000_000

var prices = pricing.Prices{
	"claude-opus-5-5":  {Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75},
	pricing.DefaultKey: {Input: 1, CacheRead: 0.1, CacheWrite: 1.25, Output: 4},
}

func assertCost(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("Cost = %v, want %v", got, want)
	}
}

func TestCostUsesExactModelPrice(t *testing.T) {
	tokens := pricing.Tokens{InputUncached: million, InputCacheRead: million, InputCacheWrite: million, OutputOther: million}
	assertCost(t, prices.Cost("claude-opus-5-5", tokens), 15+1.5+18.75+75)
}

func TestCostFallsBackToDefault(t *testing.T) {
	tokens := pricing.Tokens{InputUncached: 2 * million, OutputOther: million / 2}
	assertCost(t, prices.Cost("unknown-model", tokens), 2*1+0.5*4)
}

func TestCostWithoutPriceIsZero(t *testing.T) {
	tokens := pricing.Tokens{InputUncached: million, OutputOther: million}
	noDefault := pricing.Prices{"claude-opus-5-5": prices["claude-opus-5-5"]}
	assertCost(t, noDefault.Cost("unknown-model", tokens), 0)
	assertCost(t, pricing.Prices(nil).Cost("claude-opus-5-5", tokens), 0)
}

func TestCostPricesReasoningAtOutputRate(t *testing.T) {
	reasoning := prices.Cost("claude-opus-5-5", pricing.Tokens{OutputReasoning: 2 * million})
	assertCost(t, reasoning, 2*75)
	other := prices.Cost("claude-opus-5-5", pricing.Tokens{OutputOther: 2 * million})
	assertCost(t, reasoning, other)
}

func TestCostIgnoresUnclassifiedTokens(t *testing.T) {
	tokens := pricing.Tokens{Unclassified: million, Total: million}
	assertCost(t, prices.Cost("claude-opus-5-5", tokens), 0)
}
