package management

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

func TestAccessScopesNewConnection(t *testing.T) {
	cases := []struct {
		name   string
		scopes string
		want   []string
	}{
		{"absent means models", ``, []string{"models"}},
		{"models and web", `,"scopes":["models","web"]`, []string{"models", "web"}},
		{"models is added", `,"scopes":["web"]`, []string{"models", "web"}},
		{"duplicates collapse", `,"scopes":["web","web","models"]`, []string{"models", "web"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, r := newAccessTestRouter(t)
			doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
			code, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"`+tc.scopes+`}`, "https://app.example")
			if code != http.StatusAccepted || !slices.Equal(anyStrings(created["scopes"]), tc.want) {
				t.Fatalf("create = %d %v", code, created)
			}
			id := created["id"].(string)
			_, list := doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
			first := list["requests"].([]any)[0].(map[string]any)
			if !slices.Equal(anyStrings(first["scopes"]), tc.want) || first["upgrade"] != nil || first["grant_id"] != nil || first["added_scopes"] != nil {
				t.Fatalf("listing = %v", first)
			}
			if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", ""); status["scopes"] != nil {
				t.Fatalf("pending poll must not report scopes: %v", status)
			}
			_, grant := doAccess(t, r, http.MethodPost, "/m/access/requests/"+id+"/approve", "", "")
			if !slices.Equal(anyStrings(grant["scopes"]), tc.want) {
				t.Fatalf("grant = %v", grant)
			}
			_, status := doAccess(t, r, http.MethodGet, "/access/requests/"+id, "", "")
			if status["status"] != AccessApproved || !slices.Equal(anyStrings(status["scopes"]), tc.want) {
				t.Fatalf("approved poll = %v", status)
			}
			if got := h.KeyScopes(testAccessKey); !slices.Equal(got, tc.want) {
				t.Fatalf("KeyScopes = %v", got)
			}
		})
	}
}

func TestAccessScopesRefusals(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")

	code, out := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`","scopes":["models","admin"]}`, "https://app.example")
	if code != http.StatusBadRequest || out["code"] != "unknown_scope" || out["error"] != "unknown scope: admin" {
		t.Fatalf("unknown scope = %d %v", code, out)
	}
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"short"}`, "https://app.example")
	if code != http.StatusBadRequest || out["code"] != "invalid_request" {
		t.Fatalf("bad key = %d %v", code, out)
	}

	// The user's own key has no grant.
	h.cfg.APIKeys = append(h.cfg.APIKeys, "tg-own-0123456789abcdef0123456789abcdef")
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"tg-own-0123456789abcdef0123456789abcdef","scopes":["models","web"]}`, "https://app.example")
	if code != http.StatusConflict || out["code"] != "key_already_allowed" || out["error"] != "this key is already allowed" {
		t.Fatalf("own key = %d %v", code, out)
	}
	if got := h.KeyScopes("tg-own-0123456789abcdef0123456789abcdef"); !slices.Equal(got, AllScopes) {
		t.Fatalf("own key scopes = %v", got)
	}

	// Approve a models-only app, then try the refusals that apply to its key.
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	doAccess(t, r, http.MethodPost, "/m/access/requests/"+created["id"].(string)+"/approve", "", "")

	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`","scopes":["models"]}`, "https://app.example")
	if code != http.StatusConflict || out["code"] != "scopes_already_granted" || out["error"] != "this key already has these scopes" {
		t.Fatalf("nothing new = %d %v", code, out)
	}
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`"}`, "https://app.example")
	if code != http.StatusConflict || out["code"] != "scopes_already_granted" {
		t.Fatalf("no scopes on a granted key = %d %v", code, out)
	}
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"Evil","key":"`+testAccessKey+`","scopes":["models","web"]}`, "https://evil.example")
	if code != http.StatusConflict || out["code"] != "key_owned_by_other_app" || out["error"] != "this key belongs to another app" {
		t.Fatalf("other origin = %d %v", code, out)
	}
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"Evil","key":"`+testAccessKey+`","scopes":["web"]}`, "")
	if code != http.StatusConflict || out["code"] != "key_owned_by_other_app" {
		t.Fatalf("local program for a web origin's key = %d %v", code, out)
	}

	// One pending request per origin still applies, with a code.
	doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`","scopes":["web"]}`, "https://app.example")
	code, out = doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"App","key":"`+testAccessKey+`x","scopes":["web"]}`, "https://app.example")
	if code != http.StatusConflict || out["code"] != "request_pending" || out["id"] == nil {
		t.Fatalf("second pending = %d %v", code, out)
	}
}

func TestAccessScopeUpgrade(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`"}`, "https://app.example")
	_, grant := doAccess(t, r, http.MethodPost, "/m/access/requests/"+created["id"].(string)+"/approve", "", "")
	grantID := grant["id"].(string)
	keysBefore := slices.Clone(h.cfg.APIKeys)

	code, up := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["models","web"]}`, "https://app.example")
	if code != http.StatusAccepted || !slices.Equal(anyStrings(up["scopes"]), []string{"models", "web"}) {
		t.Fatalf("upgrade create = %d %v", code, up)
	}
	_, list := doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	first := list["requests"].([]any)[0].(map[string]any)
	if first["upgrade"] != true || first["grant_id"] != grantID || !slices.Equal(anyStrings(first["added_scopes"]), []string{"web"}) ||
		!slices.Equal(anyStrings(first["scopes"]), []string{"models", "web"}) {
		t.Fatalf("upgrade listing = %v", first)
	}
	if got := h.KeyScopes(testAccessKey); !slices.Equal(got, []string{"models"}) {
		t.Fatalf("scopes before approval = %v", got)
	}

	code, updated := doAccess(t, r, http.MethodPost, "/m/access/requests/"+up["id"].(string)+"/approve", "", "")
	if code != http.StatusOK || updated["id"] != grantID || !slices.Equal(anyStrings(updated["scopes"]), []string{"models", "web"}) {
		t.Fatalf("upgrade approve = %d %v", code, updated)
	}
	if !slices.Equal(h.cfg.APIKeys, keysBefore) {
		t.Fatalf("upgrade must not touch api keys: %v", h.cfg.APIKeys)
	}
	_, grants := doAccess(t, r, http.MethodGet, "/m/access/grants", "", "")
	list2 := grants["grants"].([]any)
	if len(list2) != 1 || !slices.Equal(anyStrings(list2[0].(map[string]any)["scopes"]), []string{"models", "web"}) {
		t.Fatalf("grants after upgrade = %v", grants)
	}
	_, status := doAccess(t, r, http.MethodGet, "/access/requests/"+up["id"].(string), "", "")
	if status["status"] != AccessApproved || !slices.Equal(anyStrings(status["scopes"]), []string{"models", "web"}) {
		t.Fatalf("upgrade poll = %v", status)
	}
	if got := h.KeyScopes(testAccessKey); !slices.Equal(got, []string{"models", "web"}) {
		t.Fatalf("scopes after approval = %v", got)
	}
	saved, err := os.ReadFile(filepath.Join(filepath.Dir(h.configFilePath), accessGrantsFileName))
	if err != nil || !strings.Contains(string(saved), `"scopes"`) || !strings.Contains(string(saved), `"web"`) {
		t.Fatalf("grants file = %s (%v)", saved, err)
	}
	code, out := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["models","web"]}`, "https://app.example")
	if code != http.StatusConflict || out["code"] != "scopes_already_granted" {
		t.Fatalf("after upgrade = %d %v", code, out)
	}
}

func TestAccessScopeUpgradeDenyKeepsGrant(t *testing.T) {
	h, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`"}`, "https://app.example")
	doAccess(t, r, http.MethodPost, "/m/access/requests/"+created["id"].(string)+"/approve", "", "")
	_, up := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["web"]}`, "https://app.example")
	if code, _ := doAccess(t, r, http.MethodPost, "/m/access/requests/"+up["id"].(string)+"/deny", "", ""); code != http.StatusOK {
		t.Fatalf("deny = %d", code)
	}
	if got := h.KeyScopes(testAccessKey); !slices.Equal(got, []string{"models"}) {
		t.Fatalf("scopes after deny = %v", got)
	}
	if !slices.Contains(h.cfg.APIKeys, testAccessKey) {
		t.Fatal("deny of an upgrade must keep the key")
	}
	code, out := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["web"]}`, "https://app.example")
	if code != http.StatusTooManyRequests || out["code"] != "cooldown" {
		t.Fatalf("after deny = %d %v", code, out)
	}
}

func TestAccessScopeUpgradeAfterRevoke(t *testing.T) {
	_, r := newAccessTestRouter(t)
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	_, created := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`"}`, "https://app.example")
	_, grant := doAccess(t, r, http.MethodPost, "/m/access/requests/"+created["id"].(string)+"/approve", "", "")
	_, up := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["web"]}`, "https://app.example")
	doAccess(t, r, http.MethodDelete, "/m/access/grants/"+grant["id"].(string), "", "")
	code, out := doAccess(t, r, http.MethodPost, "/m/access/requests/"+up["id"].(string)+"/approve", "", "")
	if code != http.StatusConflict || out["code"] != "grant_revoked" {
		t.Fatalf("approve after revoke = %d %v", code, out)
	}
	if _, status := doAccess(t, r, http.MethodGet, "/access/requests/"+up["id"].(string), "", ""); status["status"] != AccessExpired {
		t.Fatalf("status = %v", status)
	}
	// The revoked key may pair again as a new connection.
	code, again := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"KEL","key":"`+testAccessKey+`","scopes":["web"]}`, "https://app.example")
	if code != http.StatusAccepted || again["upgrade"] != nil {
		t.Fatalf("re-pair = %d %v", code, again)
	}
}

func TestAccessGrantsWithoutScopesMeanModels(t *testing.T) {
	h, r := newAccessTestRouter(t)
	h.cfg.APIKeys = append(h.cfg.APIKeys, testAccessKey)
	old := `{"grants":[{"id":"g1","name":"Old","origin":"https://app.example","key_hash":"` + hashKey(testAccessKey) + `","key_hint":"tg-w…cdef","granted_at":"2026-11-01T00:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.configFilePath), accessGrantsFileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	_, grants := doAccess(t, r, http.MethodGet, "/m/access/grants", "", "")
	list := grants["grants"].([]any)
	if len(list) != 1 || !slices.Equal(anyStrings(list[0].(map[string]any)["scopes"]), []string{"models"}) {
		t.Fatalf("grants = %v", grants)
	}
	if got := h.KeyScopes(testAccessKey); !slices.Equal(got, []string{"models"}) {
		t.Fatalf("KeyScopes = %v", got)
	}
	// An upgrade request works for it.
	doAccess(t, r, http.MethodGet, "/m/access/requests", "", "")
	code, up := doAccess(t, r, http.MethodPost, "/access/requests", `{"name":"Old","key":"`+testAccessKey+`","scopes":["models","web"]}`, "https://app.example")
	if code != http.StatusAccepted || up["id"] == nil {
		t.Fatalf("upgrade = %d %v", code, up)
	}
}
