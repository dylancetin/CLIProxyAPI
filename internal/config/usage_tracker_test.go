package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore/pricing"
	"gopkg.in/yaml.v3"
)

func loadUsageTrackerBothWays(t *testing.T, raw string) (loaded, parsed UsageTrackerConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfgLoaded, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatalf("LoadConfig() error = %v", errLoad)
	}
	cfgParsed, errParse := ParseConfigBytes([]byte(raw))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	return cfgLoaded.UsageTracker, cfgParsed.UsageTracker
}

func TestUsageTrackerDefaults(t *testing.T) {
	for name, raw := range map[string]string{
		"absent":      "config-version: 8\nserver:\n  port: 8317\n",
		"empty block": "config-version: 8\nobservability:\n  usage:\n    tracker: {}\n",
		"empty path":  "config-version: 8\nobservability:\n  usage:\n    tracker:\n      sqlite-path: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			loaded, parsed := loadUsageTrackerBothWays(t, raw)
			want := DefaultUsageTrackerConfig()
			if !reflect.DeepEqual(loaded, want) {
				t.Fatalf("LoadConfig tracker = %+v, want %+v", loaded, want)
			}
			if !reflect.DeepEqual(parsed, want) {
				t.Fatalf("ParseConfigBytes tracker = %+v, want %+v", parsed, want)
			}
		})
	}
}

func TestUsageTrackerExplicitValuesAndClamp(t *testing.T) {
	raw := `config-version: 8
observability:
  usage:
    tracker:
      enabled: false
      sqlite-path: /data/usage.sqlite
      raw-retention-days: -5
      hourly-retention-days: -1
      prices:
        claude-opus-5-5:
          input: 15
          cache-read: 1.5
          cache-write: 18.75
          output: 75
`
	want := UsageTrackerConfig{
		SQLitePath: "/data/usage.sqlite",
		Prices: map[string]pricing.ModelPrice{
			"claude-opus-5-5": {Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75},
		},
	}
	loaded, parsed := loadUsageTrackerBothWays(t, raw)
	if !reflect.DeepEqual(loaded, want) {
		t.Fatalf("LoadConfig tracker = %+v, want %+v", loaded, want)
	}
	if !reflect.DeepEqual(parsed, want) {
		t.Fatalf("ParseConfigBytes tracker = %+v, want %+v", parsed, want)
	}
}

func TestUsageTrackerLegacyFlatKey(t *testing.T) {
	loaded, parsed := loadUsageTrackerBothWays(t, "port: 8317\nusage-tracker:\n  enabled: false\n  raw-retention-days: 7\n")
	for _, got := range []UsageTrackerConfig{loaded, parsed} {
		if got.Enabled || got.RawRetentionDays != 7 || got.HourlyRetentionDays != DefaultUsageTrackerHourlyRetentionDays {
			t.Fatalf("legacy usage-tracker = %+v", got)
		}
	}
}

func TestUsageTrackerV8RoundTrip(t *testing.T) {
	raw := `config-version: 8
server:
  port: 8317
observability:
  usage:
    tracker:
      enabled: false
      sqlite-path: /data/usage.sqlite
      raw-retention-days: 7
      hourly-retention-days: 90
      prices:
        default:
          input: 1
          output: 2
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	cfg, errLoad := LoadConfig(path)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	cfg.UsageTracker.Prices["claude-opus-5-5"] = pricing.ModelPrice{Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75}
	cfg.UsageTracker.HourlyRetentionDays = 120
	if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
		t.Fatal(errSave)
	}
	saved, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errValidate := ValidateV8Config(saved); errValidate != nil {
		t.Fatalf("saved config is not v8: %v\n%s", errValidate, saved)
	}
	var node yaml.Node
	if errUnmarshal := yaml.Unmarshal(saved, &node); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if yamlPath(node.Content[0], "usage-tracker") != nil {
		t.Fatalf("saved config kept the flat usage-tracker key:\n%s", saved)
	}
	if price := yamlPath(node.Content[0], "observability.usage.tracker.prices.claude-opus-5-5.cache-write"); price == nil || price.Value != "18.75" {
		t.Fatalf("saved config lost the tracker price under observability.usage.tracker:\n%s", saved)
	}
	reloaded, errReload := LoadConfig(path)
	if errReload != nil {
		t.Fatal(errReload)
	}
	if !reflect.DeepEqual(reloaded.UsageTracker, cfg.UsageTracker) {
		t.Fatalf("round trip tracker = %+v, want %+v", reloaded.UsageTracker, cfg.UsageTracker)
	}
}

func TestUsageTrackerExampleTemplate(t *testing.T) {
	example, errRead := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	cfg, errParse := ParseConfigBytes(example)
	if errParse != nil {
		t.Fatal(errParse)
	}
	if !reflect.DeepEqual(cfg.UsageTracker, DefaultUsageTrackerConfig()) {
		t.Fatalf("template tracker = %+v, want defaults", cfg.UsageTracker)
	}
	// The commented price example must parse once an operator uncomments it.
	text := strings.ReplaceAll(string(example), "\r\n", "\n")
	uncommented := strings.Replace(text, "      # prices:\n      #   claude-opus-5-5:\n      #     input: 15\n      #     cache-read: 1.5\n      #     cache-write: 18.75\n      #     output: 75\n",
		"      prices:\n        claude-opus-5-5:\n          input: 15\n          cache-read: 1.5\n          cache-write: 18.75\n          output: 75\n", 1)
	if uncommented == text {
		t.Fatal("config.example.yaml is missing the tracker price example")
	}
	cfg, errParse = ParseConfigBytes([]byte(uncommented))
	if errParse != nil {
		t.Fatal(errParse)
	}
	want := pricing.ModelPrice{Input: 15, CacheRead: 1.5, CacheWrite: 18.75, Output: 75}
	if got := cfg.UsageTracker.Prices["claude-opus-5-5"]; got != want {
		t.Fatalf("uncommented price = %+v, want %+v", got, want)
	}
}

func TestCloneForRuntimeCopiesUsageTrackerPrices(t *testing.T) {
	cfg := &Config{UsageTracker: UsageTrackerConfig{
		Enabled: true,
		Prices:  map[string]pricing.ModelPrice{"m": {Input: 1, Output: 2}},
	}}
	clone := cfg.CloneForRuntime()
	cfg.UsageTracker.Prices["m"] = pricing.ModelPrice{Input: 9}
	cfg.UsageTracker.Prices["added"] = pricing.ModelPrice{Input: 3}
	if got := clone.UsageTracker.Prices; len(got) != 1 || got["m"] != (pricing.ModelPrice{Input: 1, Output: 2}) {
		t.Fatalf("clone prices shared state with original: %+v", got)
	}
	if !clone.UsageTracker.Enabled {
		t.Fatal("clone lost tracker enabled flag")
	}
}

func TestUsageTrackerSaveRemovesPrices(t *testing.T) {
	raw := `config-version: 8
server:
  port: 8317
observability:
  usage:
    tracker:
      prices:
        a:
          input: 1
          output: 2
        b:
          input: 3
          output: 4
`
	for name, tc := range map[string]struct {
		prices map[string]pricing.ModelPrice
		want   map[string]pricing.ModelPrice
	}{
		"remove one model": {
			prices: map[string]pricing.ModelPrice{"a": {Input: 1, Output: 2}},
			want:   map[string]pricing.ModelPrice{"a": {Input: 1, Output: 2}},
		},
		"empty map": {prices: map[string]pricing.ModelPrice{}},
		"nil map":   {prices: nil},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, []byte(raw), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg, errLoad := LoadConfig(path)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if len(cfg.UsageTracker.Prices) != 2 {
				t.Fatalf("seed prices = %+v", cfg.UsageTracker.Prices)
			}
			cfg.UsageTracker.Prices = tc.prices
			if errSave := SaveConfigPreserveComments(path, cfg, true); errSave != nil {
				t.Fatal(errSave)
			}
			saved, errRead := os.ReadFile(path)
			if errRead != nil {
				t.Fatal(errRead)
			}
			reloaded, errReload := LoadConfig(path)
			if errReload != nil {
				t.Fatal(errReload)
			}
			if len(reloaded.UsageTracker.Prices) != len(tc.want) || (len(tc.want) > 0 && !reflect.DeepEqual(reloaded.UsageTracker.Prices, tc.want)) {
				t.Fatalf("reloaded prices = %+v, want %+v\n%s", reloaded.UsageTracker.Prices, tc.want, saved)
			}
			if errValidate := ValidateV8Config(saved); errValidate != nil {
				t.Fatalf("saved config is not v8: %v\n%s", errValidate, saved)
			}
		})
	}
}
