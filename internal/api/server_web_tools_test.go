package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// keyListProvider authenticates bearer keys from a fixed list.
type keyListProvider struct{ keys map[string]bool }

func (p keyListProvider) Identifier() string { return "test-keys" }

func (p keyListProvider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}
	if !p.keys[key] {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	return &sdkaccess.Result{Provider: "test-keys", Principal: key}, nil
}

func TestWebToolsRoutesUseKeyScopes(t *testing.T) {
	const ownKey = "tg-own-key"
	const appKey = "tg-app-0123456789abcdef0123456789abcdef"
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{ownKey, appKey}}}
	cfg.WebTools = proxyconfig.DefaultWebToolsConfig()
	server := newTestServerWithConfig(t, cfg)
	server.accessManager.SetProviders([]sdkaccess.Provider{keyListProvider{keys: map[string]bool{ownKey: true, appKey: true}}})
	// The app key was approved through an access request with models only.
	grants := `{"grants":[{"id":"g1","name":"App","origin":"https://app.example","key_hash":"` + sha256Hex(appKey) + `","key_hint":"x","granted_at":"2026-11-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(server.configFilePath), "access-grants.json"), []byte(grants), 0o600); err != nil {
		t.Fatal(err)
	}

	probe := func(key string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		server.engine.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := probe("tg-unknown"); code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", code)
	}
	if code, _ := probe(""); code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", code)
	}
	code, out := probe(ownKey)
	if code != http.StatusOK || len(out["scopes"].([]any)) != 2 {
		t.Fatalf("own key = %d %v", code, out)
	}
	fetch := out["tools"].(map[string]any)["web_fetch"].(map[string]any)
	if fetch["available"] != true {
		t.Fatalf("own key fetch = %v", fetch)
	}
	code, out = probe(appKey)
	if code != http.StatusOK || len(out["scopes"].([]any)) != 1 {
		t.Fatalf("app key = %d %v", code, out)
	}
	if reason := out["tools"].(map[string]any)["web_search"].(map[string]any)["reason"]; reason != "scope_required" {
		t.Fatalf("app key reason = %v", reason)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/tools/web_fetch", strings.NewReader(`{"url":"https://example.com/"}`))
	req.Header.Set("Authorization", "Bearer "+appKey)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()
	server.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"scope_required"`) {
		t.Fatalf("app key fetch = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Fatalf("CORS headers = %v", rec.Header())
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
