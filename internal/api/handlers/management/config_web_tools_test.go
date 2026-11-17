package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8WebToolsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\naccess:\n  api-keys: [client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.WebTools.Enabled || cfg.WebTools.Search.Default != "brave" || cfg.WebTools.Domains.Mode != "denylist" {
		t.Fatalf("defaults = %+v", cfg.WebTools)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	reloads := make(chan *config.Config, 8)
	h.SetConfigReloadHook(func(_ context.Context, cfg *config.Config) { reloads <- cfg })
	router := gin.New()
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	do := func(method, body string) (int, string) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, "/v8/management/config/web-tools", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}

	if code, _ := do(http.MethodGet, ""); code != http.StatusNotFound {
		t.Fatalf("GET before the section exists = %d, want 404", code)
	}
	body := `{"enabled":false,"search":{"default":"exa","providers":{"brave":{"api-key":""},"tavily":{"api-key":"tv-1"},"exa":{"api-key":"exa-1"}}},"domains":{"mode":"allowlist","list":["Example.com","*.wiki.org"]}}`
	if code, out := do(http.MethodPut, body); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, out)
	}
	select {
	case next := <-reloads:
		w := next.WebTools
		if w.Enabled || w.Search.Default != "exa" || w.Search.Providers.Exa.APIKey != "exa-1" || w.Domains.Mode != "allowlist" ||
			!slices.Equal(w.Domains.List, []string{"example.com", "wiki.org"}) {
			t.Fatalf("reloaded web tools = %+v", w)
		}
		if got := w.Search.Configured(); !slices.Equal(got, []string{"tavily", "exa"}) {
			t.Fatalf("configured = %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload after PUT")
	}
	code, out := do(http.MethodGet, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != false {
		t.Fatalf("GET = %v", got)
	}
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "web-tools:") || !strings.Contains(string(saved), "exa-1") {
		t.Fatalf("config file = %s", saved)
	}
	reparsed, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reparsed.WebTools.Search.Providers.Tavily.APIKey != "tv-1" {
		t.Fatalf("reparsed = %+v", reparsed.WebTools)
	}
}
