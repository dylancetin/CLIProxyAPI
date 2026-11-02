package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestManagementPanelAppearance(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	path := filepath.Join(staticDir, "management.html")
	asset := []byte(`<html><head><script type="module">const key="cli-proxy-theme";</script></head><body>management app</body></html>`)
	if err := os.WriteFile(path, asset, 0o600); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rr := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rr)
			ctx.Request = httptest.NewRequest(method, "/management.html", nil)
			server.serveManagementControlPanel(ctx)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q", got)
			}
			if method == http.MethodHead {
				if rr.Body.Len() != 0 {
					t.Fatal("HEAD response must not include a body")
				}
				return
			}
			if !strings.Contains(rr.Body.String(), `id="cpa-appearance"`) || !strings.Contains(rr.Body.String(), "management app") {
				t.Fatal("response must include both appearance and original application")
			}
		})
	}
	stored, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if !bytes.Equal(stored, asset) {
		t.Fatal("serving the panel must not change its on-disk release digest")
	}
}
