package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

type usageStoreOpenCall struct {
	opts usagestore.Options
}

// stubUsageStoreOpener records Open calls. Until usagestore.Open is implemented
// it stands in a zero Store so the reconciliation logic is still exercised.
func stubUsageStoreOpener(t *testing.T, failWith error) *[]usageStoreOpenCall {
	t.Helper()
	var calls []usageStoreOpenCall
	previous := openUsageStore
	openUsageStore = func(ctx context.Context, opts usagestore.Options) (*usagestore.Store, error) {
		calls = append(calls, usageStoreOpenCall{opts: opts})
		if failWith != nil {
			return nil, failWith
		}
		store, errOpen := usagestore.Open(ctx, opts)
		if errors.Is(errOpen, usagestore.ErrNotImplemented) {
			return &usagestore.Store{}, nil
		}
		return store, errOpen
	}
	t.Cleanup(func() { openUsageStore = previous })
	return &calls
}

func trackerConfig(server *Server) proxyconfig.Config {
	cfg := *server.getConfig()
	cfg.UsageTracker = proxyconfig.DefaultUsageTrackerConfig()
	return cfg
}

// captureLogs swaps the standard logger hooks for a test hook.
func captureLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	hook := logtest.NewLocal(logger)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks) })
	return hook
}

func hasLogEntry(hook *logtest.Hook, level log.Level, fragment string) bool {
	for _, entry := range hook.AllEntries() {
		if entry.Level == level && strings.Contains(entry.Message, fragment) {
			return true
		}
	}
	return false
}

func TestUsageTrackerDisabledOpensNothing(t *testing.T) {
	calls := stubUsageStoreOpener(t, nil)
	server := newTestServer(t)

	server.StartUsageTracker(context.Background())
	cfg := *server.getConfig()
	cfg.UsageTracker = proxyconfig.UsageTrackerConfig{Enabled: false, RawRetentionDays: 30}
	server.UpdateClients(&cfg)

	if len(*calls) != 0 {
		t.Fatalf("Open called %d times with the tracker disabled", len(*calls))
	}
	if server.usageTracker.plugin != nil {
		t.Fatal("tracker plugin registered while disabled")
	}
	if _, errStat := os.Stat(filepath.Join(cfg.AuthDir, proxyconfig.DefaultUsageTrackerFileName)); !os.IsNotExist(errStat) {
		t.Fatalf("database file exists while disabled: %v", errStat)
	}
}

func TestUsageTrackerHomeModeForcesOff(t *testing.T) {
	calls := stubUsageStoreOpener(t, nil)
	server := newTestServer(t)
	cfg := trackerConfig(server)
	cfg.Home.Enabled = true

	server.UpdateClients(&cfg)

	if len(*calls) != 0 || server.mgmt.UsageStore() != nil {
		t.Fatalf("tracker opened in Home mode (calls=%d)", len(*calls))
	}
}

func TestUsageTrackerOpenFailureContinues(t *testing.T) {
	hook := captureLogs(t)
	calls := stubUsageStoreOpener(t, errors.New("disk full"))
	server := newTestServer(t)
	cfg := trackerConfig(server)

	if ok := server.UpdateClientsContext(context.Background(), &cfg); !ok {
		t.Fatal("UpdateClientsContext failed because the tracker could not open")
	}
	if server.mgmt.UsageStore() != nil || server.usageTracker.plugin != nil {
		t.Fatal("failed tracker left a store or plugin behind")
	}
	if !hasLogEntry(hook, log.WarnLevel, "usage tracker unavailable") {
		t.Fatal("missing tracker unavailable warning")
	}

	// Unchanged settings do not retry; a toggle or a new path does.
	server.UpdateClients(&cfg)
	if len(*calls) != 1 {
		t.Fatalf("Open calls after an unchanged reload = %d, want 1", len(*calls))
	}
	cfg.UsageTracker.SQLitePath = filepath.Join(t.TempDir(), "other.sqlite")
	server.UpdateClients(&cfg)
	cfg.UsageTracker.Enabled = false
	server.UpdateClients(&cfg)
	cfg.UsageTracker.Enabled = true
	server.UpdateClients(&cfg)
	if len(*calls) != 3 {
		t.Fatalf("Open calls after path change and toggle = %d, want 3", len(*calls))
	}
}

func TestUsageTrackerReloadLifecycle(t *testing.T) {
	hook := captureLogs(t)
	calls := stubUsageStoreOpener(t, nil)
	server := newTestServer(t)
	cfg := trackerConfig(server)

	server.UpdateClients(&cfg)
	if len(*calls) != 1 {
		t.Fatalf("Open calls = %d, want 1", len(*calls))
	}
	wantPath := filepath.Join(cfg.AuthDir, proxyconfig.DefaultUsageTrackerFileName)
	if got := (*calls)[0].opts; got.Path != wantPath || got.RawRetention != 30*24*time.Hour || got.HourlyRetention != 400*24*time.Hour {
		t.Fatalf("Open options = %+v, want path %s and default retention", got, wantPath)
	}
	first := server.mgmt.UsageStore()
	plugin := server.usageTracker.plugin
	if first == nil || plugin == nil {
		t.Fatal("enabled tracker did not reach the handler or register a plugin")
	}

	// Retention, price, and path edits keep the open store.
	cfg.UsageTracker.RawRetentionDays = 7
	cfg.UsageTracker.HourlyRetentionDays = 0
	cfg.UsageTracker.Prices = map[string]usagestore.ModelPrice{"m": {Input: 1}}
	cfg.UsageTracker.SQLitePath = filepath.Join(t.TempDir(), "moved.sqlite")
	server.UpdateClients(&cfg)
	server.UpdateClients(&cfg)
	if len(*calls) != 1 || server.mgmt.UsageStore() != first {
		t.Fatal("setting edits reopened the store")
	}
	if server.usageTracker.raw != 7*24*time.Hour || server.usageTracker.hourly != 0 {
		t.Fatalf("retention = %v/%v, want 168h/0", server.usageTracker.raw, server.usageTracker.hourly)
	}
	restartWarnings := 0
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "restart required") {
			restartWarnings++
		}
	}
	if restartWarnings != 1 {
		t.Fatalf("restart warnings = %d, want 1", restartWarnings)
	}

	cfg.UsageTracker.Enabled = false
	server.UpdateClients(&cfg)
	if server.mgmt.UsageStore() != nil || server.usageTracker.store != nil {
		t.Fatal("disable left the store attached")
	}

	cfg.UsageTracker.Enabled = true
	server.UpdateClients(&cfg)
	if len(*calls) != 2 || (*calls)[1].opts.Path != cfg.UsageTracker.SQLitePath {
		t.Fatalf("re-enable did not reopen at the configured path: %+v", *calls)
	}
	if server.usageTracker.plugin != plugin {
		t.Fatal("re-enable registered a second plugin")
	}

	server.CloseUsageTracker()
	if server.mgmt.UsageStore() != nil {
		t.Fatal("close left the store attached")
	}
	server.UpdateClients(&cfg)
	if len(*calls) != 2 {
		t.Fatal("reload reopened the tracker after close")
	}
}

func TestUsageTrackerPath(t *testing.T) {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		t.Skip(errHome)
	}
	for _, tc := range []struct {
		name, authDir, sqlitePath, want string
	}{
		{"auth dir default", "/srv/auths", "", filepath.Join("/srv/auths", "usage.sqlite")},
		{"tilde auth dir", "~/auths", "", filepath.Join(home, "auths", "usage.sqlite")},
		{"explicit path", "/srv/auths", " /data/u.sqlite ", "/data/u.sqlite"},
		{"tilde path", "/srv/auths", "~/u.sqlite", filepath.Join(home, "u.sqlite")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &proxyconfig.Config{UsageTracker: proxyconfig.UsageTrackerConfig{SQLitePath: tc.sqlitePath}}
			cfg.AuthDir = tc.authDir
			got, errPath := usageTrackerPath(cfg)
			if errPath != nil || got != filepath.Clean(tc.want) {
				t.Fatalf("usageTrackerPath() = %q, %v; want %q", got, errPath, tc.want)
			}
		})
	}
}

type countingUsagePlugin struct {
	mu      sync.Mutex
	count   int
	entered chan struct{}
	release chan struct{}
}

func (p *countingUsagePlugin) HandleUsage(context.Context, usage.Record) {
	if p.entered != nil {
		p.entered <- struct{}{}
		<-p.release
	}
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
}

func TestSwitchableUsagePlugin(t *testing.T) {
	plugin := &switchableUsagePlugin{}
	plugin.HandleUsage(context.Background(), usage.Record{})

	first := &countingUsagePlugin{}
	plugin.setTarget(first)
	plugin.HandleUsage(context.Background(), usage.Record{})
	plugin.setTarget(nil)
	plugin.HandleUsage(context.Background(), usage.Record{})
	if first.count != 1 {
		t.Fatalf("forwarded %d records, want 1", first.count)
	}

	// HandleUsage holds the lock across the call, so clearing the target waits
	// for in-flight records and a closed store is never used.
	blocking := &countingUsagePlugin{entered: make(chan struct{}), release: make(chan struct{})}
	plugin.setTarget(blocking)
	done := make(chan struct{})
	go func() {
		plugin.HandleUsage(context.Background(), usage.Record{})
		close(done)
	}()
	<-blocking.entered
	if plugin.mu.TryLock() {
		plugin.mu.Unlock()
		t.Fatal("setTarget could proceed while a call was in flight")
	}
	close(blocking.release)
	<-done
	plugin.setTarget(nil)
	if blocking.count != 1 {
		t.Fatalf("in-flight record count = %d, want 1", blocking.count)
	}
}
