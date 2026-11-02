// Package pricing holds token counts and per-model prices for the usage tracker.
// It has no internal imports so that internal/config can depend on it without
// creating a cycle through sdk/cliproxy/usage.
package pricing

// DefaultKey is the Prices key used when a model has no exact entry.
const DefaultKey = "default"

const tokensPerPriceUnit = 1_000_000

// Tokens holds non-overlapping token counts for one attempt or an aggregate.
type Tokens struct {
	InputUncached   int64
	InputCacheRead  int64
	InputCacheWrite int64
	OutputReasoning int64
	// OutputOther is non-reasoning output.
	OutputOther  int64
	Unclassified int64
	Total        int64
}

// ModelPrice is the USD price per 1,000,000 tokens for one model.
type ModelPrice struct {
	Input      float64 `yaml:"input" json:"input"`
	CacheRead  float64 `yaml:"cache-read" json:"cache_read"`
	CacheWrite float64 `yaml:"cache-write" json:"cache_write"`
	Output     float64 `yaml:"output" json:"output"`
}

// Prices maps a model id to its price. Lookup tries the exact model id first,
// then DefaultKey.
type Prices map[string]ModelPrice

// Cost returns the estimated USD cost of t for model, or 0 when no price applies.
// Reasoning output is priced at the Output rate. Unclassified tokens are not
// priced because their input/output split is unknown.
func (p Prices) Cost(model string, t Tokens) float64 {
	price, ok := p[model]
	if !ok {
		price, ok = p[DefaultKey]
	}
	if !ok {
		return 0
	}
	sum := float64(t.InputUncached)*price.Input +
		float64(t.InputCacheRead)*price.CacheRead +
		float64(t.InputCacheWrite)*price.CacheWrite +
		float64(t.OutputReasoning+t.OutputOther)*price.Output
	return sum / tokensPerPriceUnit
}
