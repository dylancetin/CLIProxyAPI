package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestForceHomeRuntimeConfigDisablesUsageTracker(t *testing.T) {
	cfg := &config.Config{}
	cfg.UsageTracker.Enabled = true

	forceHomeRuntimeConfig(cfg)

	if cfg.UsageTracker.Enabled {
		t.Fatal("expected home runtime config to force the usage tracker off")
	}
}
