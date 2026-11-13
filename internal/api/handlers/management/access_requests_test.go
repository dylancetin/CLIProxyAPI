package management

import (
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

const testAccessKey = "tg-web-0123456789abcdef0123456789abcdef"

func newAccessTestRouter(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &Handler{
		cfg:            &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"tg-primary"}}},
		configFilePath: writeTestConfigFile(t),
	}
	r := gin.New()
	r.POST("/access/requests", h.CreateAccessRequest)
	r.GET("/access/requests/:id", h.GetAccessRequest)
	r.GET("/m/access/requests", h.ListAccessRequests)
	r.POST("/m/access/requests/:id/approve", h.ApproveAccessRequest)
	r.POST("/m/access/requests/:id/deny", h.DenyAccessRequest)
	r.GET("/m/access/grants", h.ListAccessGrants)
	r.DELETE("/m/access/grants/:id", h.RevokeAccessGrant)
	return h, r
}

func doAccess(t *testing.T, r *gin.Engine, method, path, body, origin string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAccessRequestNeedsWatcher(t *testing.T) {
	_, r := newAccessTestRouter(t)
	code, _ := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 without a watching management client", code)
	}
}

func TestAccessRequestRejectsRemotePeer(t *testing.T) {
	_, r := newAccessTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/access/requests", strings.NewReader(`{"name":"App","key":"`+testAccessKey+`"}`))
	req.RemoteAddr = "192.0.2.10:50000"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-loopback peer", rec.Code)
	}
}

func TestAccessRequestValidation(t *testing.T) {
	_, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	for _, body := range []string{
		`{"name":"","key":"` + testAccessKey + `"}`,
		`{"name":"App","key":"short"}`,
		`{"name":"App","key":"` + strings.Repeat("x", 31) + `!"}`,
		`not json`,
	} {
		if code, _ := doAccess(t, r, http.MethodPost, "/access/requests", body, "https://app.example"); code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, code)
		}
	}
	if code, _ := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"tg-primary-is-not-32-chars"}`, ""); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestAccessRequestApproveAndRevoke(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")

	code, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"  My\u0007 Web   App ","key":"`+testAccessKey+`"}`, "https://app.example")
	if code != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %v", code, created)
	}
	id, _ := created["id"].(string)

	// A second request from the same origin waits for the first.
	if code, _ := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"Again","key":"`+testAccessKey+`x"}`, "https://app.example"); code != http.StatusConflict {
		t.Fatalf("duplicate origin status = %d, want 409", code)
	}

	_, list := doAccess(t, r, http.MethodGet, "/m/access/requests?wait=1", "", "")
	reqs, _ := list["requests"].([]any)
	if len(reqs) != 1 {
		t.Fatalf("pending = %v, want one", list)
	}
	first := reqs[0].(map[string]any)
	if first["name"] != "My Web App" || first["origin"] != "https://app.example" {
		t.Fatalf("pending request = %v", first)
	}
	if _, leaked := first["key"]; leaked {
		t.Fatalf("pending list must not include the key: %v", first)
	}

	if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", ""); status["status"] != AccessPending {
		t.Fatalf("status = %v, want pending", status)
	}

	code, grant := doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/approve", "", "")
	if code != http.StatusOK {
		t.Fatalf("approve status = %d, body = %v", code, grant)
	}
	if !slices.Equal(h.cfg.APIKeys, []string{"tg-primary", testAccessKey}) {
		t.Fatalf("api keys = %v", h.cfg.APIKeys)
	}
	if code, _ := doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/approve", "", ""); code != http.StatusNotFound {
		t.Fatalf("second approve status = %d, want 404", code)
	}
	if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", ""); status["status"] != AccessApproved {
		t.Fatalf("status = %v, want approved", status)
	}

	saved, err := os.ReadFile(filepath.Join(filepath.Dir(h.configFilePath), accessGrantsFileName))
	if err != nil {
		t.Fatalf("grants file: %v", err)
	}
	if strings.Contains(string(saved), testAccessKey) {
		t.Fatal("grants file must not contain the key")
	}

	_, grants := doAccess(t, r, http.MethodGet, "/m/access/grants", "", "")
	if list, _ := grants["grants"].([]any); len(list) != 1 {
		t.Fatalf("grants = %v, want one", grants)
	}
	if code, _ := doAccess(t, r, http.MethodDelete, "/m/access/grants/"+grant["id"].(string), "", ""); code != http.StatusOK {
		t.Fatalf("revoke status = %d", code)
	}
	if !slices.Equal(h.cfg.APIKeys, []string{"tg-primary"}) {
		t.Fatalf("api keys after revoke = %v", h.cfg.APIKeys)
	}
	_, grants = doAccess(t, r, http.MethodGet, "/m/access/grants", "", "")
	if list, _ := grants["grants"].([]any); len(list) != 0 {
		t.Fatalf("grants after revoke = %v", grants)
	}
}

func TestAccessRequestDenyStartsCooldown(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	id := created["id"].(string)

	if code, _ := doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/deny", "", ""); code != http.StatusOK {
		t.Fatalf("deny status = %d", code)
	}
	if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", ""); status["status"] != AccessDenied {
		t.Fatalf("status = %v, want denied", status)
	}
	if code, _ := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example"); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 during cooldown", code)
	}
	if len(h.cfg.APIKeys) != 1 {
		t.Fatalf("api keys = %v, deny must not add a key", h.cfg.APIKeys)
	}
}

func TestAccessRequestExpires(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	id := created["id"].(string)

	b := h.access()
	b.mu.Lock()
	b.requests[id].ExpiresAt = time.Now().Add(-time.Second)
	b.mu.Unlock()

	if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", ""); status["status"] != AccessExpired {
		t.Fatalf("status = %v, want expired", status)
	}
	if code, _ := doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/approve", "", ""); code != http.StatusNotFound {
		t.Fatalf("approve after expiry status = %d, want 404", code)
	}
}

func TestAccessRequestWaitWakesOnDecision(t *testing.T) {
	_, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	id := created["id"].(string)

	done := make(chan map[string]any, 1)
	go func() {
		_, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id+"?wait=30", "", "")
		done <- status
	}()
	doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/approve", "", "")
	select {
	case status := <-done:
		if status["status"] != AccessApproved {
			t.Fatalf("status = %v, want approved", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long poll did not wake up after approval")
	}
}
