package webtools

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestFetchArticle(t *testing.T) {
	h := newHarness(t)
	var got http.Header
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(articleHTML))
	})
	code, out := h.fetch(`{"url":"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi","extra":"ignored"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch = %d %v", code, out)
	}
	md, _ := out["markdown"].(string)
	for _, want := range []string{
		"Kabataş Erkek Lisesi, 1908 yılında",
		"[Beşiktaş](https://tr.wikipedia.org/wiki/Be%C5%9Fikta%C5%9F)",
		"[Görsel: Okul binası]",
		"[tarih sayfasına](https://example.com/tarih)",
		"buraya tıklayın", "e-posta gönderin",
		"- Birinci madde",
		"| Yıl", "| 1908 | Kuruluş |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{
		"do not leak", "color: red", "Gizli paragraf", "Ekran okuyucudan", "Stil ile gizlenmiş", "Görünmez kutu",
		"javascript:", "mailto:", "spacer", "form değeri", "Gönder", "JavaScript gerekli", "vektör yazı",
		"Şablon içeriği", "yorum satırı", "<", "\n\n\n", "[]", "Dosya:Logo",
	} {
		if strings.Contains(md, unwanted) {
			t.Errorf("markdown contains %q:\n%s", unwanted, md)
		}
	}
	if out["url"] != "https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi" || out["contentType"] != "text/html" ||
		out["siteName"] != "Vikipedi" || out["lang"] != "tr" || out["published"] != "2024-11-02T10:00:00Z" {
		t.Errorf("metadata = %v", out)
	}
	if title, _ := out["title"].(string); !strings.Contains(title, "Kabataş Erkek Lisesi") {
		t.Errorf("title = %v", out["title"])
	}
	if out["offset"] != float64(0) || out["truncated"] != false || out["nextOffset"] != nil || out["found"] != nil {
		t.Errorf("window fields = %v", out)
	}
	if int(out["totalChars"].(float64)) != len([]rune(md)) {
		t.Errorf("totalChars = %v, markdown has %d", out["totalChars"], len([]rune(md)))
	}

	// Request headers: honest UA, no cookies, no credentials, no referer.
	if got.Get("Accept") != acceptHeader || got.Get("Accept-Language") != defaultAcceptLang ||
		!strings.HasPrefix(got.Get("User-Agent"), "Mozilla/5.0 (compatible; CLI-Tollgate/") {
		t.Errorf("request headers = %v", got)
	}
	for _, header := range []string{"Cookie", "Authorization", "Referer"} {
		if got.Get(header) != "" {
			t.Errorf("%s was sent", header)
		}
	}
	// The dialer connected to the resolved public address on port 443.
	if len(h.dialed) == 0 || h.dialed[0] != publicIP+":443" {
		t.Errorf("dialed = %v", h.dialed)
	}
}

func TestFetchLangHintAndCharset(t *testing.T) {
	h := newHarness(t)
	var acceptLang string
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		acceptLang = r.Header.Get("Accept-Language")
		// "Şişli" in ISO-8859-9 (Turkish).
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-9")
		_, _ = w.Write([]byte{0xDE, 'i', 0xFE, 'l', 'i', '\r', '\n', '\r', '\n', '\r', '\n', '\r', '\n', 'x'})
	})
	code, out := h.fetch(`{"url":"https://example.com/a.txt","lang":"en"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch = %d %v", code, out)
	}
	if out["markdown"] != "Şişli\n\nx" || out["contentType"] != "text/plain" || out["title"] != "example.com" {
		t.Fatalf("plain text = %q (%v)", out["markdown"], out)
	}
	if acceptLang != "en,en;q=0.8" {
		t.Fatalf("accept-language = %q", acceptLang)
	}

	h.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta charset="windows-1254"><title>T</title></head><body><p>` + "\xDE" + `ehir</p></body></html>`))
	})
	_, out = h.fetch(`{"url":"https://example.com/meta-charset"}`)
	if md, _ := out["markdown"].(string); !strings.Contains(md, "Şehir") {
		t.Fatalf("meta charset not applied: %v", out)
	}
}

func TestFetchBodyFallbackAndNoContent(t *testing.T) {
	h := newHarness(t)
	h.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Kısa</title></head><body><span>Tek satır.</span></body></html>`))
	})
	code, out := h.fetch(`{"url":"https://example.com/short"}`)
	if code != http.StatusOK || !strings.Contains(out["markdown"].(string), "Tek satır.") || out["title"] != "Kısa" {
		t.Fatalf("short page = %d %v", code, out)
	}
	h.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><script>only()</script><div hidden>x</div></body></html>`))
	})
	code, out = h.fetch(`{"url":"https://example.com/empty"}`)
	if code != http.StatusUnprocessableEntity || errCode(out) != CodeNoContent {
		t.Fatalf("empty page = %d %v", code, out)
	}
}

func TestFetchRedirects(t *testing.T) {
	h := newHarness(t)
	var cookieSeen string
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if c := r.Header.Get("Cookie"); c != "" {
			cookieSeen = c
		}
		switch r.URL.Path {
		case "/r1":
			http.SetCookie(w, &http.Cookie{Name: "track", Value: "1"})
			http.Redirect(w, r, "https://www.example.com/r2", http.StatusFound)
		case "/r2":
			http.Redirect(w, r, "/r3", http.StatusMovedPermanently)
		case "/r3":
			http.Redirect(w, r, "https://news.example.com/final", http.StatusTemporaryRedirect)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/to-http":
			http.Redirect(w, r, "http://example.com/plain", http.StatusFound)
		case "/to-ip":
			http.Redirect(w, r, "https://127.0.0.1/admin", http.StatusFound)
		case "/to-private-name":
			http.Redirect(w, r, "https://private.test.com/", http.StatusFound)
		case "/to-blocked":
			http.Redirect(w, r, "https://blocked.org/x", http.StatusFound)
		case "/to-port":
			http.Redirect(w, r, "https://example.com:8443/x", http.StatusFound)
		case "/no-location":
			w.WriteHeader(http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = fmt.Fprintf(w, "landed on %s%s", r.Host, r.URL.Path)
		}
	})
	code, out := h.fetch(`{"url":"https://example.com/r1"}`)
	if code != http.StatusOK || out["url"] != "https://news.example.com/final" || out["markdown"] != "landed on news.example.com/final" {
		t.Fatalf("3 redirects = %d %v", code, out)
	}
	if cookieSeen != "" {
		t.Fatalf("cookie was sent back: %q", cookieSeen)
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Domains.List = []string{"blocked.org"} })
	cases := map[string]struct {
		status int
		code   string
	}{
		"/loop":            {422, CodeTooManyRedirects},
		"/to-http":         {400, CodeInsecureURL},
		"/to-ip":           {403, CodePrivateAddress},
		"/to-private-name": {403, CodePrivateAddress},
		"/to-blocked":      {403, CodeBlockedDomain},
		"/to-port":         {400, CodeInsecureURL},
		"/no-location":     {502, CodeFetchFailed},
	}
	for path, want := range cases {
		code, out := h.fetch(`{"url":"https://example.com` + path + `"}`)
		if code != want.status || errCode(out) != want.code {
			t.Errorf("%s = %d %v, want %d %s", path, code, out, want.status, want.code)
		}
	}
}

func TestFetchSSRFGuard(t *testing.T) {
	h := newHarness(t)
	cases := map[string]string{
		"https://loopback.test.com/":   CodePrivateAddress, // resolves to 127.0.0.1
		"https://private.test.com/":    CodePrivateAddress, // resolves to 10.0.0.7
		"https://mixed.test.com/":      CodePrivateAddress, // one public, one private answer
		"https://mapped.test.com/":     CodePrivateAddress, // ::ffff:127.0.0.1
		"https://127.0.0.1/":           CodePrivateAddress,
		"https://[::1]/":               CodePrivateAddress,
		"https://169.254.169.254/":     CodePrivateAddress,
		"https://[::ffff:10.1.1.1]/":   CodePrivateAddress,
		"https://192.168.0.1/":         CodePrivateAddress,
		"https://localhost/":           CodePrivateAddress,
		"https://nas.local/":           CodePrivateAddress,
		"https://intranet/":            CodePrivateAddress,
		"http://example.com/":          CodeInsecureURL,
		"https://example.com:444/":     CodeInsecureURL,
		"https://me:pw@example.com/":   CodeInsecureURL,
		"https://nxdomain.example.net": CodeFetchFailed,
	}
	for target, want := range cases {
		code, out := h.fetch(`{"url":"` + target + `"}`)
		if errCode(out) != want {
			t.Errorf("%s = %d %v, want %s", target, code, out, want)
		}
	}
	for _, addr := range h.dialed {
		if !strings.HasPrefix(addr, publicIP) {
			t.Fatalf("dialed a refused address: %v", h.dialed)
		}
	}
}

func TestFetchDNSRebindingIsCheckedAtDial(t *testing.T) {
	h := newHarness(t)
	// First lookup is public, every later one points at loopback.
	h.resolver.next = func(host string, call int) []string {
		if host == "rebind.example.com" {
			if call == 1 {
				return []string{publicIP}
			}
			return []string{"127.0.0.1"}
		}
		return nil
	}
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close") // force a new connection (and lookup) per request
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/second", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("should not be reached"))
	})
	code, out := h.fetch(`{"url":"https://rebind.example.com/first"}`)
	if code != http.StatusForbidden || errCode(out) != CodePrivateAddress {
		t.Fatalf("rebinding = %d %v", code, out)
	}
}

func TestFetchProductionDialerRefusesPrivateResolution(t *testing.T) {
	// No injected Dial: the real dialer path must refuse before connecting.
	f := &Fetcher{Resolver: &fakeResolver{answers: map[string][]string{"evil.example.com": {"127.0.0.1"}}, calls: map[string]int{}}, Timeout: time.Second}
	target, _ := parseFetchURL("https://evil.example.com/", config.WebDomainsConfig{Mode: "denylist"})
	_, terr := f.Fetch(t.Context(), target, config.WebDomainsConfig{Mode: "denylist"}, "")
	if terr == nil || terr.Code != CodePrivateAddress {
		t.Fatalf("fetch = %v", terr)
	}
}

func TestFetchLimits(t *testing.T) {
	h := newHarness(t)
	big := bytes.Repeat([]byte("a"), defaultMaxBodyBytes+10)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(big)
	_ = zw.Close()
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big-declared":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", fmt.Sprint(len(big)))
			_, _ = w.Write(big)
		case "/big-chunked":
			w.Header().Set("Content-Type", "text/plain")
			flusher := w.(http.Flusher)
			for i := 0; i < len(big); i += 1 << 20 {
				end := min(i+1<<20, len(big))
				_, _ = w.Write(big[i:end])
				flusher.Flush()
			}
		case "/big-gzip":
			// Small on the wire, over the limit once decompressed.
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(gz.Bytes())
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.7"))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("\x89PNG"))
		case "/sniff-pdf":
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte("%PDF-1.7 binary"))
		case "/markdown":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = w.Write([]byte("# Başlık\n\n\n\n\nMetin"))
		case "/xhtml":
			w.Header().Set("Content-Type", "application/xhtml+xml")
			_, _ = w.Write([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><p>XHTML içerik</p></body></html>`))
		case "/slow":
			time.Sleep(1500 * time.Millisecond)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("late"))
		case "/missing":
			http.NotFound(w, r)
		case "/error":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	cases := map[string]struct {
		status int
		code   string
	}{
		"/big-declared": {422, CodeTooLarge},
		"/big-chunked":  {422, CodeTooLarge},
		"/big-gzip":     {422, CodeTooLarge},
		"/pdf":          {422, CodeUnsupportedType},
		"/image":        {422, CodeUnsupportedType},
		"/sniff-pdf":    {422, CodeUnsupportedType},
		"/missing":      {502, CodeFetchFailed},
		"/error":        {502, CodeFetchFailed},
	}
	for path, want := range cases {
		code, out := h.fetch(`{"url":"https://example.com` + path + `"}`)
		if code != want.status || errCode(out) != want.code {
			t.Errorf("%s = %d %v, want %d %s", path, code, out, want.status, want.code)
		}
	}
	if _, out := h.fetch(`{"url":"https://example.com/missing"}`); errField(out, "status") != float64(404) {
		t.Errorf("fetch_failed must carry the site status: %v", out)
	}
	if code, out := h.fetch(`{"url":"https://example.com/markdown"}`); code != 200 || out["markdown"] != "# Başlık\n\nMetin" || out["contentType"] != "text/markdown" {
		t.Errorf("markdown = %d %v", code, out)
	}
	if code, out := h.fetch(`{"url":"https://example.com/xhtml"}`); code != 200 || !strings.Contains(out["markdown"].(string), "XHTML içerik") {
		t.Errorf("xhtml = %d %v", code, out)
	}

	h.svc.Fetcher.Timeout = 300 * time.Millisecond
	start := time.Now()
	code, out := h.fetch(`{"url":"https://example.com/slow"}`)
	if code != http.StatusGatewayTimeout || errCode(out) != CodeTimeout {
		t.Errorf("slow = %d %v", code, out)
	}
	if time.Since(start) > 1200*time.Millisecond {
		t.Errorf("timeout did not cut the call short")
	}
}

func TestFetchValidation(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{
		`not json`,
		`{}`,
		`{"url":""}`,
		`{"url":42}`,
		`{"url":"https://example.com/","maxChars":499}`,
		`{"url":"https://example.com/","maxChars":20001}`,
		`{"url":"https://example.com/","offset":-1}`,
		`{"url":"https://example.com/","find":"` + strings.Repeat("x", 201) + `"}`,
		`{"url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`,
	} {
		code, out := h.fetch(body)
		if code != http.StatusBadRequest || errCode(out) != CodeInvalidRequest {
			t.Errorf("%s = %d %v", body, code, out)
		}
	}
	if h.requests.Load() != 0 {
		t.Fatalf("invalid requests reached the site %d times", h.requests.Load())
	}
}

func TestFetchWindowingFindAndCache(t *testing.T) {
	h := newHarness(t)
	var paragraphs []string
	for i := 0; i < 60; i++ {
		paragraphs = append(paragraphs, fmt.Sprintf("<p>Paragraf %d: %s</p>", i, strings.Repeat("metin ", 20)))
	}
	paragraphs[45] = "<p>Burada özel bir İFADE geçiyor ve sonra devam ediliyor.</p>"
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/long", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>Uzun</title></head><body><article>" + strings.Join(paragraphs, "\n") + "</article></body></html>"))
	})
	code, first := h.fetch(`{"url":"https://example.com/start","maxChars":1000}`)
	if code != http.StatusOK || first["truncated"] != true || first["url"] != "https://example.com/long" {
		t.Fatalf("first page = %d %v", code, first)
	}
	md := first["markdown"].(string)
	next := int(first["nextOffset"].(float64))
	if next != len([]rune(md)) || next > 1000 || next < 800 || !strings.HasSuffix(md, "\n\n") {
		t.Fatalf("window end = %d, markdown ends %q", next, md[len(md)-10:])
	}
	requests := h.requests.Load()

	// Paging and find are served from the cache, by original or final URL.
	_, second := h.fetch(fmt.Sprintf(`{"url":"https://example.com/start","maxChars":1000,"offset":%d}`, next))
	if second["offset"] != float64(next) || !strings.HasPrefix(second["markdown"].(string), "Paragraf") {
		t.Fatalf("second page = %v", second)
	}
	_, found := h.fetch(`{"url":"https://example.com/long","maxChars":500,"find":"ifade"}`)
	if found["found"] != true || found["matchOffset"] == nil {
		t.Fatalf("find = %v", found)
	}
	window := found["markdown"].(string)
	if !strings.Contains(window, "İFADE") || !strings.HasPrefix(window, "Paragraf 4") {
		t.Fatalf("find window = %q", window)
	}
	total := int(found["totalChars"].(float64))
	_, missing := h.fetch(`{"url":"https://example.com/long","find":"bulunmayan"}`)
	if missing["found"] != false || missing["markdown"] != "" || missing["truncated"] != false || int(missing["totalChars"].(float64)) != total {
		t.Fatalf("not found = %v", missing)
	}
	if _, last := h.fetch(fmt.Sprintf(`{"url":"https://example.com/long","offset":%d}`, total-100)); last["truncated"] != false || last["nextOffset"] != nil {
		t.Fatalf("last page = %v", last)
	}
	if h.requests.Load() != requests {
		t.Fatalf("cache missed: %d extra requests", h.requests.Load()-requests)
	}

	// Another key does not share the cache.
	h.call(http.MethodPost, "/v1/tools/web_fetch", "tg-other-key", `{"url":"https://example.com/long"}`)
	if h.requests.Load() != requests+1 {
		t.Fatalf("cache shared across keys")
	}
	// Domain list changes apply to cached pages.
	h.setConfig(func(c *config.WebToolsConfig) { c.Domains.List = []string{"example.com"} })
	if code, out := h.fetch(`{"url":"https://example.com/long"}`); code != 403 || errCode(out) != CodeBlockedDomain {
		t.Fatalf("blocked after caching = %d %v", code, out)
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Domains.List = nil })
	// After ten minutes the page is downloaded again.
	h.advance(pageCacheTTL + time.Second)
	h.fetch(`{"url":"https://example.com/long"}`)
	if h.requests.Load() != requests+2 {
		t.Fatalf("expired cache entry was reused")
	}
}

func TestFetchConcurrencyCap(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(MaxConcurrentKey)
	h.setHandler(func(w http.ResponseWriter, r *http.Request) {
		started.Done()
		<-release
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok " + r.URL.Path))
	})
	var wg sync.WaitGroup
	for i := 0; i < MaxConcurrentKey; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.fetch(fmt.Sprintf(`{"url":"https://example.com/slow-%d"}`, i))
		}(i)
	}
	started.Wait()
	code, out, header := h.call(http.MethodPost, "/v1/tools/web_fetch", webKey, `{"url":"https://example.com/fifth"}`)
	close(release)
	wg.Wait()
	if code != http.StatusTooManyRequests || errCode(out) != CodeRateLimited || header.Get("Retry-After") != "1" || errField(out, "retry_after") != float64(1) {
		t.Fatalf("fifth concurrent fetch = %d %v %v", code, out, header)
	}
}

func TestFetchPerMinuteLimit(t *testing.T) {
	h := newHarness(t)
	h.setHandler(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	for i := 0; i < FetchPerMinute; i++ {
		if code, out := h.fetch(`{"url":"https://example.com/p"}`); code != 200 {
			t.Fatalf("fetch %d = %d %v", i, code, out)
		}
	}
	code, out, header := h.call(http.MethodPost, "/v1/tools/web_fetch", webKey, `{"url":"https://example.com/p"}`)
	if code != http.StatusTooManyRequests || errCode(out) != CodeRateLimited || header.Get("Retry-After") != "60" {
		t.Fatalf("41st fetch = %d %v %v", code, out, header)
	}
	h.advance(time.Minute)
	if code, _ := h.fetch(`{"url":"https://example.com/p"}`); code != 200 {
		t.Fatalf("after a minute = %d", code)
	}
}
