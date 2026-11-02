package management

import (
	"context"
	"errors"
	"maps"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

var errNoConfigFile = errors.New("management: no config file path configured")

// configUsagePrices reads and writes model prices through the handler's config,
// so edits land in the config file and survive restarts and reloads.
type configUsagePrices struct{ h *Handler }

func (p configUsagePrices) Get() usagestore.Prices {
	if p.h == nil {
		return nil
	}
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	if p.h.cfg == nil {
		return nil
	}
	return maps.Clone(p.h.cfg.UsageTracker.Prices)
}

func (p configUsagePrices) Put(prices usagestore.Prices) error {
	if p.h == nil {
		return errNoConfigFile
	}
	p.h.mu.Lock()
	if p.h.cfg == nil || p.h.configFilePath == "" {
		p.h.mu.Unlock()
		return errNoConfigFile
	}
	prev := p.h.cfg.UsageTracker.Prices
	p.h.cfg.UsageTracker.Prices = maps.Clone(prices)
	if errSave := config.SaveConfigPreserveComments(p.h.configFilePath, p.h.cfg, true); errSave != nil {
		p.h.cfg.UsageTracker.Prices = prev
		p.h.mu.Unlock()
		return errSave
	}
	snapshot := p.h.reloadSnapshotConfigLocked()
	p.h.mu.Unlock()
	p.h.reloadConfigAfterManagementSaveAsync(context.Background(), snapshot)
	return nil
}
