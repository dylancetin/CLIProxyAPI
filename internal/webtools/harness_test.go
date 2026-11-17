package webtools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	webKey    = "tg-web-key"
	modelsKey = "tg-models-key"
	publicIP  = "93.184.216.34"
)

// fakeResolver answers from a map; unknown names fail like NXDOMAIN.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]string
	calls   map[string]int
	// next, when set, overrides the answer for a name per call (DNS rebinding).
	next func(host string, call int) []string
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[host]++
	answers, ok := r.answers[host]
	if r.next != nil {
		if override := r.next(host, r.calls[host]); override != nil {
			answers, ok = override, true
		}
	}
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, 0, len(answers))
	for _, a := range answers {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

type harness struct {
	t        *testing.T
	site     *httptest.Server
	handle   atomic.Value // http.HandlerFunc
	requests atomic.Int32
	dialed   []string
	dialMu   sync.Mutex
	resolver *fakeResolver
	svc      *Service
	router   *gin.Engine

	mu  sync.Mutex
	cfg config.WebToolsConfig
	now time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &harness{t: t, now: time.Unix(1_800_000_000, 0)}
	h.cfg = config.DefaultWebToolsConfig()
	h.handle.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	}))
	h.site = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		h.handle.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(h.site.Close)
	h.resolver = &fakeResolver{
		answers: map[string][]string{
			"example.com":       {publicIP},
			"www.example.com":   {publicIP},
			"news.example.com":  {publicIP},
			"other.org":         {"203.0.114.10"},
			"tr.wikipedia.org":  {publicIP},
			"loopback.test.com": {"127.0.0.1"},
			"private.test.com":  {"10.0.0.7"},
			"mixed.test.com":    {publicIP, "192.168.1.20"},
			"mapped.test.com":   {"::ffff:127.0.0.1"},
			"blocked.org":       {publicIP},
		},
		calls: map[string]int{},
	}
	nowFn := func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	h.svc = newService(func() config.WebToolsConfig {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cfg
	}, func(key string) []string {
		switch key {
		case webKey:
			return []string{"models", "web"}
		case modelsKey:
			return []string{"models"}
		}
		return []string{"models", "web"}
	}, nowFn)
	siteAddr := h.site.Listener.Addr().String()
	h.svc.Fetcher = &Fetcher{
		Resolver: h.resolver,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			h.dialMu.Lock()
			h.dialed = append(h.dialed, address)
			h.dialMu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, siteAddr)
		},
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server certificate
		Timeout:   2 * time.Second,
		UserAgent: UserAgent(),
	}
	h.router = gin.New()
	h.router.Use(func(c *gin.Context) {
		if key := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "); key != "" {
			c.Set("userApiKey", key)
		}
	})
	h.router.GET("/v1/tools", h.svc.Probe)
	h.router.POST("/v1/tools/web_search", h.svc.Search)
	h.router.POST("/v1/tools/web_fetch", h.svc.Fetch)
	return h
}

func (h *harness) setHandler(fn http.HandlerFunc) { h.handle.Store(fn) }

func (h *harness) setConfig(fn func(*config.WebToolsConfig)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(&h.cfg)
	cfg := config.Config{WebTools: h.cfg}
	cfg.NormalizeWebToolsConfig()
	h.cfg = cfg.WebTools
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *harness) call(method, path, key, body string) (int, map[string]any, http.Header) {
	h.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("%s %s: response is not JSON: %s", method, path, rec.Body.String())
	}
	return rec.Code, out, rec.Header()
}

func (h *harness) fetch(body string) (int, map[string]any) {
	h.t.Helper()
	code, out, _ := h.call(http.MethodPost, "/v1/tools/web_fetch", webKey, body)
	return code, out
}

func (h *harness) search(body string) (int, map[string]any) {
	h.t.Helper()
	code, out, _ := h.call(http.MethodPost, "/v1/tools/web_search", webKey, body)
	return code, out
}

// errCode returns error.code from an error envelope.
func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func errField(out map[string]any, field string) any {
	e, _ := out["error"].(map[string]any)
	return e[field]
}

const articleHTML = `<!DOCTYPE html>
<html lang="tr">
<head>
<meta charset="utf-8">
<title>Kabataş Erkek Lisesi - Vikipedi</title>
<meta property="og:site_name" content="Vikipedi">
<meta name="author" content="Ayşe Yılmaz">
<meta property="article:published_time" content="2024-11-02T10:00:00Z">
<script>window.secret = "do not leak";</script>
<style>.x { color: red }</style>
</head>
<body>
<nav><a href="/">Ana sayfa</a> <a href="/random">Rastgele</a></nav>
<article>
<h1>Kabataş Erkek Lisesi</h1>
<p>Kabataş Erkek Lisesi, 1908 yılında İstanbul'da kurulmuş köklü bir okuldur. Okul, <a href="/wiki/Be%C5%9Fikta%C5%9F">Beşiktaş</a> ilçesinde, Boğaz kıyısında yer alır ve uzun bir geçmişe sahiptir.</p>
<p>Okulun mezunları arasında birçok bilim insanı, sanatçı ve devlet adamı bulunur. Bu paragraf, okunabilirlik algoritmasının makaleyi ana içerik olarak seçmesi için yeterince uzun bir metin içerir.</p>
<p hidden>Gizli paragraf görünmemeli.</p>
<p aria-hidden="true">Ekran okuyucudan gizli metin.</p>
<p style="display: none">Stil ile gizlenmiş metin.</p>
<div style="visibility:hidden">Görünmez kutu.</div>
<p><img src="/logo.png" alt="Okul binası"> <img src="/spacer.gif" alt=""> <a href="/wiki/Dosya:Logo.svg"><img src="/logo.svg" alt=""></a></p>
<p>Daha fazla bilgi için <a href="javascript:alert(1)">buraya tıklayın</a>, <a href="mailto:info@example.com">e-posta gönderin</a> ya da <a href="https://example.com/tarih">tarih sayfasına</a> bakın.</p>
<ul><li>Birinci madde</li><li>İkinci madde</li></ul>
<table><tr><th>Yıl</th><th>Olay</th></tr><tr><td>1908</td><td>Kuruluş</td></tr></table>
<iframe src="https://ads.example.com/frame"></iframe>
<form><input type="text" value="form değeri"><button>Gönder</button></form>
<noscript>JavaScript gerekli</noscript>
<svg><text>vektör yazı</text></svg>
<template><p>Şablon içeriği</p></template>
<!-- yorum satırı -->
</article>
</body>
</html>`

func TestMain(m *testing.M) {
	log.SetLevel(log.WarnLevel)
	os.Exit(m.Run())
}
