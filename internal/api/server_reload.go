package api

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/githubauth"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) bool {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return false
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return false
	}
	return true
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	s.UpdateClientsContext(context.Background(), cfg)
}

// UpdateClientsContext updates runtime clients while honoring cancellation between
// short configuration and filesystem operations.
func (s *Server) UpdateClientsContext(ctx context.Context, cfg *config.Config) bool {
	if s == nil || cfg == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	cfg = cfg.CloneForRuntime()
	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	s.cfgMu.RLock()
	oldConfigYaml := append([]byte(nil), s.oldConfigYaml...)
	s.cfgMu.RUnlock()
	if len(oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(oldConfigYaml, &oldCfg)
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != cfg.RequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(cfg.RequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(cfg.RequestLog)
		}
	}

	if oldCfg == nil || oldCfg.Home.Enabled != cfg.Home.Enabled {
		if setter, ok := s.requestLogger.(interface{ SetHomeEnabled(bool) }); ok {
			setter.SetHomeEnabled(cfg.Home.Enabled)
		}
	}

	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile || oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
		if errContext := ctx.Err(); errContext != nil {
			return false
		}
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}

	s.syncUsageTracker(ctx, cfg)

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}
	if oldCfg == nil || oldCfg.TransientErrorCooldownSeconds != cfg.TransientErrorCooldownSeconds {
		auth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
	}

	if oldCfg != nil && oldCfg.DisableImageGeneration != cfg.DisableImageGeneration {
		log.Infof("disable-image-generation updated: %v -> %v", oldCfg.DisableImageGeneration, cfg.DisableImageGeneration)
	}

	applySignatureCacheConfig(oldCfg, cfg)
	applyClaudeToolSearchConfig(cfg)

	if oldCfg != nil && !reflect.DeepEqual(oldCfg.Antigravity.ConnectionPool, cfg.Antigravity.ConnectionPool) {
		executor.ResetAntigravityTransports()
	}

	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials)
	}

	// Update log level dynamically when debug flag changes
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}
	redisqueue.SetEnabled(s.managementRoutesEnabled.Load() || (cfg != nil && cfg.Home.Enabled))

	exampleAPIKeySafeModeRequired := s.exampleAPIKeySafeModeRequired(cfg)
	if exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(true)
	}
	accessConfigApplied := s.applyAccessConfig(oldCfg, cfg)
	if accessConfigApplied || exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(exampleAPIKeySafeModeRequired)
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	if s.codexLiveHandler != nil {
		if errUpdate := s.codexLiveHandler.UpdateConfig(cfg); errUpdate != nil {
			log.WithError(errUpdate).Error("failed to update Codex Live media relay configuration")
		}
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	githubauth.SetToken(cfg.GitHubToken)
	managementasset.SetCurrentConfig(cfg)
	if errContext := ctx.Err(); errContext != nil {
		return false
	}
	// Save YAML snapshot for next comparison
	marshaledCfg, _ := yaml.Marshal(cfg)
	s.cfgMu.Lock()
	s.oldConfigYaml = marshaledCfg
	s.cfgMu.Unlock()

	s.handlers.UpdateClients(effectiveSDKConfig(cfg))
	s.handlers.SetPluginHost(s.pluginHost)
	if s.pluginHost != nil {
		s.pluginHost.SetModelExecutor(s.handlers)
		s.pluginHost.SetAuthManager(s.handlers.AuthManager)
	}

	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
		s.mgmt.SetPluginHost(s.pluginHost)
	}
	s.refreshPluginManagementRoutes()

	// Count client sources from configuration and auth store.
	authEntries := 0
	if cfg != nil && !cfg.Home.Enabled {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(cfg.AuthDir)
		}
		authEntries = util.CountAuthFiles(ctx, tokenStore)
		if errContext := ctx.Err(); errContext != nil {
			return false
		}
	}
	geminiAPIKeyCount := len(cfg.GeminiKey)
	interactionsAPIKeyCount := len(cfg.InteractionsKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	xaiAPIKeyCount := len(cfg.XAIKey)
	metaAPIKeyCount := len(cfg.MetaKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		if entry.Disabled {
			continue
		}
		openAICompatCount += len(entry.APIKeyEntries)
	}

	total := authEntries + geminiAPIKeyCount + interactionsAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + xaiAPIKeyCount + metaAPIKeyCount + vertexAICompatCount + openAICompatCount
	fmt.Printf("server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Interactions API keys + %d Claude API keys + %d Codex keys + %d xAI keys + %d Meta API keys + %d Vertex-compat + %d OpenAI-compat)\n",
		total,
		authEntries,
		geminiAPIKeyCount,
		interactionsAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		xaiAPIKeyCount,
		metaAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)
	return ctx.Err() == nil
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil {
		return
	}
	s.wsAuthChanged = fn
}

// usageTrackerPluginName is the usage manager registration name of the tracker.
const usageTrackerPluginName = "sqlite-usage"

// openUsageStore is replaced in tests.
var openUsageStore = usagestore.Open

// usageTrackerRuntime is the SQLite usage store a Server owns.
type usageTrackerRuntime struct {
	mu         sync.Mutex
	plugin     *switchableUsagePlugin
	store      *usagestore.Store
	path       string
	raw        time.Duration
	hourly     time.Duration
	warnedPath string
	// failedPath suppresses Open retries until the tracker is toggled or moved,
	// so a persistent failure logs once instead of on every reload.
	failedPath string
	closed     bool
}

// switchableUsagePlugin forwards records to the current store plugin. The usage
// manager cannot unregister a plugin, so disabling clears the target instead.
// HandleUsage holds the read lock, so clearing waits out in-flight calls and the
// store is never used after it closes.
type switchableUsagePlugin struct {
	mu     sync.RWMutex
	target usage.Plugin
}

func (p *switchableUsagePlugin) HandleUsage(ctx context.Context, record usage.Record) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.target != nil {
		p.target.HandleUsage(ctx, record)
	}
}

func (p *switchableUsagePlugin) setTarget(target usage.Plugin) {
	p.mu.Lock()
	p.target = target
	p.mu.Unlock()
}

// usageTrackerPath returns sqlite-path, or usage.sqlite in the resolved auth dir.
// cfg.AuthDir already points at the local mirror when a remote token store is used.
func usageTrackerPath(cfg *config.Config) (string, error) {
	if path := strings.TrimSpace(cfg.UsageTracker.SQLitePath); path != "" {
		// For a non-empty path ResolveAuthDir only expands a leading ~ and cleans it.
		return util.ResolveAuthDir(path)
	}
	authDir, errResolve := util.ResolveAuthDir(cfg.AuthDir)
	if errResolve != nil {
		return "", errResolve
	}
	return filepath.Join(authDir, config.DefaultUsageTrackerFileName), nil
}

func usageRetention(days int) time.Duration {
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// StartUsageTracker opens the usage tracker for the current config. Reloads go
// through UpdateClientsContext.
func (s *Server) StartUsageTracker(ctx context.Context) {
	if s == nil {
		return
	}
	s.syncUsageTracker(ctx, s.getConfig())
}

// CloseUsageTracker detaches and closes the usage tracker for good. Call it after
// the usage manager stops so the dispatcher no longer feeds the store.
func (s *Server) CloseUsageTracker() {
	if s == nil {
		return
	}
	s.usageTracker.mu.Lock()
	defer s.usageTracker.mu.Unlock()
	s.closeUsageStoreLocked()
	s.usageTracker.closed = true
}

// syncUsageTracker reconciles the open store with cfg. Home mode forces it off.
func (s *Server) syncUsageTracker(ctx context.Context, cfg *config.Config) {
	if s == nil {
		return
	}
	t := &s.usageTracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if cfg == nil || !cfg.UsageTracker.Enabled || cfg.Home.Enabled {
		s.closeUsageStoreLocked()
		t.failedPath = ""
		return
	}
	path, errPath := usageTrackerPath(cfg)
	if errPath != nil {
		log.WithError(errPath).Warn("usage tracker unavailable: cannot resolve database path")
		return
	}
	raw := usageRetention(cfg.UsageTracker.RawRetentionDays)
	hourly := usageRetention(cfg.UsageTracker.HourlyRetentionDays)
	if t.store != nil {
		if path != t.path && path != t.warnedPath {
			t.warnedPath = path
			log.WithFields(log.Fields{"current": t.path, "configured": path}).Warn("usage tracker sqlite-path changed; restart required to apply")
		}
		if raw != t.raw || hourly != t.hourly {
			t.store.SetRetention(raw, hourly)
			t.raw, t.hourly = raw, hourly
		}
		return
	}
	if path == t.failedPath {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	// A reload context may be request scoped; the store outlives it.
	store, errOpen := openUsageStore(context.WithoutCancel(ctx), usagestore.Options{Path: path, RawRetention: raw, HourlyRetention: hourly})
	if errOpen != nil {
		t.failedPath = path
		log.WithError(errOpen).WithField("path", path).Warn("usage tracker unavailable; continuing without it")
		return
	}
	if t.plugin == nil {
		t.plugin = &switchableUsagePlugin{}
		usage.RegisterNamedPlugin(usageTrackerPluginName, t.plugin)
	}
	t.plugin.setTarget(store.Plugin())
	t.store, t.path, t.raw, t.hourly, t.warnedPath, t.failedPath = store, path, raw, hourly, "", ""
	if s.mgmt != nil {
		s.mgmt.SetUsageStore(store)
	}
	log.WithField("path", path).Info("usage tracker enabled")
}

func (s *Server) closeUsageStoreLocked() {
	t := &s.usageTracker
	if t.store == nil {
		return
	}
	t.plugin.setTarget(nil)
	if s.mgmt != nil {
		s.mgmt.SetUsageStore(nil)
	}
	if errClose := t.store.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close usage tracker store")
	}
	log.WithField("path", t.path).Info("usage tracker disabled")
	t.store, t.path, t.warnedPath = nil, "", ""
}

// applyClaudeToolSearchConfig publishes upstream.claude.tool-search to the
// Claude Responses translator, which the Claude executor and the Codex model
// catalog also read so all three agree on it.
func applyClaudeToolSearchConfig(cfg *config.Config) {
	responsesclaude.SetClientToolSearchEnabled(cfg != nil && cfg.Claude.ToolSearch)
}

func configuredSignatureCacheEnabled(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureCacheEnabled != nil {
		return *cfg.AntigravitySignatureCacheEnabled
	}
	return true
}

func applySignatureCacheConfig(oldCfg, cfg *config.Config) {
	newVal := configuredSignatureCacheEnabled(cfg)
	newStrict := configuredSignatureBypassStrict(cfg)
	if oldCfg == nil {
		cache.SetSignatureCacheEnabled(newVal)
		cache.SetSignatureBypassStrictMode(newStrict)
		return
	}

	oldVal := configuredSignatureCacheEnabled(oldCfg)
	if oldVal != newVal {
		cache.SetSignatureCacheEnabled(newVal)
	}

	oldStrict := configuredSignatureBypassStrict(oldCfg)
	if oldStrict != newStrict {
		cache.SetSignatureBypassStrictMode(newStrict)
	}
}

func configuredSignatureBypassStrict(cfg *config.Config) bool {
	if cfg != nil && cfg.AntigravitySignatureBypassStrict != nil {
		return *cfg.AntigravitySignatureBypassStrict
	}
	return false
}
