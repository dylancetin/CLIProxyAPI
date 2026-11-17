package webtools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type capturedRequest struct {
	method string
	query  map[string][]string
	header http.Header
	body   map[string]any
}

// fakeProvider serves a canned JSON response and records the request.
func fakeProvider(t *testing.T, status int, response string, got *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.query = r.URL.Query()
		got.header = r.Header.Clone()
		got.body = nil
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &got.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withKeys(brave, tavily, exa string) func(*config.WebToolsConfig) {
	return func(c *config.WebToolsConfig) {
		c.Search.Providers.Brave.APIKey = brave
		c.Search.Providers.Tavily.APIKey = tavily
		c.Search.Providers.Exa.APIKey = exa
	}
}

func resultURLs(out map[string]any) []string {
	var urls []string
	results, _ := out["results"].([]any)
	for _, r := range results {
		urls = append(urls, r.(map[string]any)["url"].(string))
	}
	return urls
}

const braveResponse = `{"type":"search","web":{"results":[
 {"title":"Kabataş Erkek Lisesi - <strong>Vikipedi</strong>","url":"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi","description":"<strong>Kabataş</strong> Erkek Lisesi, 1908 yılında &amp; &quot;köklü&quot;   okul.","page_age":"2025-03-01T08:30:00","age":"March 1, 2025"},
 {"title":"Plain http","url":"http://insecure.example.com/","description":"dropped"},
 {"title":"Private","url":"https://192.168.1.1/admin","description":"dropped"},
 {"title":"Local","url":"https://nas.local/","description":"dropped"},
 {"title":"Blocked","url":"https://ads.blocked.org/x","description":"dropped"},
 {"title":"Duplicate","url":"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi#history","description":"dup"},
 {"title":"","url":"https://news.example.com/a","description":"` + "x" + `","age":"2 days ago"}
]}}`

func TestSearchBrave(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	srv := fakeProvider(t, 200, braveResponse, &got)
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: srv.URL + "/res/v1/web/search"}
	h.setConfig(withKeys("brave-key", "", ""))
	h.setConfig(func(c *config.WebToolsConfig) { c.Domains.List = []string{"blocked.org"} })

	code, out := h.search(`{"query":"  kabataş erkek lisesi 1908 ","count":5,"freshness":"year","lang":"tr","unknown":1}`)
	if code != 200 || out["provider"] != "brave" {
		t.Fatalf("search = %d %v", code, out)
	}
	if got.method != http.MethodGet || got.header.Get("X-Subscription-Token") != "brave-key" || got.header.Get("Accept") != "application/json" {
		t.Fatalf("brave request = %+v", got)
	}
	if got.query["q"][0] != "kabataş erkek lisesi 1908" || got.query["count"][0] != "5" || got.query["freshness"][0] != "py" || got.query["search_lang"][0] != "tr" {
		t.Fatalf("brave params = %v", got.query)
	}
	urls := resultURLs(out)
	if !slices.Equal(urls, []string{"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi", "https://news.example.com/a"}) {
		t.Fatalf("urls = %v", urls)
	}
	first := out["results"].([]any)[0].(map[string]any)
	if first["title"] != "Kabataş Erkek Lisesi - Vikipedi" || first["snippet"] != `Kabataş Erkek Lisesi, 1908 yılında & "köklü" okul.` ||
		first["published"] != "2025-03-01T08:30:00Z" || first["site"] != "tr.wikipedia.org" {
		t.Fatalf("first result = %v", first)
	}
	second := out["results"].([]any)[1].(map[string]any)
	if second["title"] != "news.example.com" || second["published"] != nil {
		t.Fatalf("second result = %v", second)
	}

	// site is appended to q; ja maps to Brave's jp; unknown languages are omitted.
	h.search(`{"query":"okul","site":"Wikipedia.org","lang":"ja"}`)
	if got.query["q"][0] != "okul site:wikipedia.org" || got.query["search_lang"][0] != "jp" || got.query["count"][0] != "5" || got.query["freshness"] != nil {
		t.Fatalf("site/lang params = %v", got.query)
	}
	h.search(`{"query":"okul","lang":"xx"}`)
	if got.query["search_lang"] != nil {
		t.Fatalf("unsupported lang was sent: %v", got.query)
	}
	// site restricts results to the host and its subdomains.
	_, out = h.search(`{"query":"okul","site":"wikipedia.org"}`)
	if urls := resultURLs(out); !slices.Equal(urls, []string{"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi"}) {
		t.Fatalf("site filter = %v", urls)
	}
	// count caps the results.
	_, out = h.search(`{"query":"okul","count":1}`)
	if len(resultURLs(out)) != 1 {
		t.Fatalf("count = %v", out)
	}
}

func TestSearchTavily(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	srv := fakeProvider(t, 200, `{"query":"q","results":[
		{"title":"Answer page","url":"https://example.com/answer","content":"A long clean excerpt `+strings.Repeat("kelime ", 80)+`","score":0.9,"published_date":"2025-06-01"},
		{"title":"No date","url":"https://other.org/x","content":"<b>bold</b> text","published_date":null}
	]}`, &got)
	h.svc.Providers[config.WebSearchTavily] = Tavily{Endpoint: srv.URL + "/search"}
	h.setConfig(withKeys("", "tvly-key", ""))

	code, out := h.search(`{"query":"what year","count":3,"freshness":"week","site":"example.com","lang":"tr"}`)
	if code != 200 || out["provider"] != "tavily" {
		t.Fatalf("search = %d %v", code, out)
	}
	if got.method != http.MethodPost || got.header.Get("Authorization") != "Bearer tvly-key" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("tavily request = %+v", got)
	}
	b := got.body
	if b["query"] != "what year" || b["max_results"] != float64(3) || b["time_range"] != "week" || b["search_depth"] != "basic" ||
		b["include_published_date"] != true || b["include_answer"] != false {
		t.Fatalf("tavily body = %v", b)
	}
	if domains, _ := b["include_domains"].([]any); len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("include_domains = %v", b["include_domains"])
	}
	if _, ok := b["search_lang"]; ok {
		t.Fatal("lang must be ignored for tavily")
	}
	results := out["results"].([]any)
	if len(results) != 1 { // other.org is outside site
		t.Fatalf("results = %v", results)
	}
	first := results[0].(map[string]any)
	snippet := first["snippet"].(string)
	if first["published"] != "2025-06-01" || len([]rune(snippet)) > maxSnippetChars || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("first = %v (%d chars)", first, len([]rune(snippet)))
	}

	h.search(`{"query":"q"}`)
	if _, ok := got.body["time_range"]; ok {
		t.Fatalf("time_range sent without freshness: %v", got.body)
	}
	if _, ok := got.body["include_domains"]; ok {
		t.Fatalf("include_domains sent without site: %v", got.body)
	}
	_, out = h.search(`{"query":"q"}`)
	if r := out["results"].([]any)[1].(map[string]any); r["snippet"] != "bold text" || r["published"] != nil {
		t.Fatalf("second = %v", r)
	}
}

func TestSearchExa(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	srv := fakeProvider(t, 200, `{"requestId":"r","results":[
		{"title":"Similar page","url":"https://example.com/similar","publishedDate":"2024-12-24T00:00:00.000Z","highlights":["first <em>highlight</em>","second"]},
		{"title":"Summary only","url":"https://other.org/s","summary":"A summary."},
		{"title":"Text only","url":"https://other.org/t","text":"Body text\n\nmore"}
	]}`, &got)
	h.svc.Providers[config.WebSearchExa] = Exa{Endpoint: srv.URL + "/search", Now: func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }}
	h.setConfig(withKeys("", "", "exa-key"))

	code, out := h.search(`{"query":"pages like this","count":10,"freshness":"month","site":"example.com"}`)
	if code != 200 || out["provider"] != "exa" {
		t.Fatalf("search = %d %v", code, out)
	}
	b := got.body
	if got.header.Get("x-api-key") != "exa-key" || b["query"] != "pages like this" || b["type"] != "auto" || b["numResults"] != float64(10) ||
		b["startPublishedDate"] != "2026-10-17T12:00:00.000Z" {
		t.Fatalf("exa request = %v %v", got.header, b)
	}
	if domains, _ := b["includeDomains"].([]any); len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("includeDomains = %v", b["includeDomains"])
	}
	contents, _ := b["contents"].(map[string]any)
	highlights, _ := contents["highlights"].(map[string]any)
	if highlights["maxCharacters"] != float64(maxSnippetChars) {
		t.Fatalf("contents = %v", b["contents"])
	}
	results := out["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["snippet"] != "first highlight … second" || results[0].(map[string]any)["published"] != "2024-12-24T00:00:00Z" {
		t.Fatalf("results = %v", results)
	}
	_, out = h.search(`{"query":"x","freshness":"day"}`)
	if got.body["startPublishedDate"] != "2026-11-16T12:00:00.000Z" {
		t.Fatalf("day = %v", got.body["startPublishedDate"])
	}
	results = out["results"].([]any)
	if results[1].(map[string]any)["snippet"] != "A summary." || results[2].(map[string]any)["snippet"] != "Body text more" {
		t.Fatalf("fallback snippets = %v", results)
	}
}

func TestSearchProviderSelection(t *testing.T) {
	h := newHarness(t)
	var braveGot, exaGot, tavilyGot capturedRequest
	brave := fakeProvider(t, 200, `{"web":{"results":[]}}`, &braveGot)
	exa := fakeProvider(t, 200, `{"results":[]}`, &exaGot)
	tavily := fakeProvider(t, 200, `{"results":[]}`, &tavilyGot)
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: brave.URL}
	h.svc.Providers[config.WebSearchExa] = Exa{Endpoint: exa.URL}
	h.svc.Providers[config.WebSearchTavily] = Tavily{Endpoint: tavily.URL}

	// Keyless search switched off and no keys: nothing can search.
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Keyless = false })
	code, out := h.search(`{"query":"q"}`)
	if code != 503 || errCode(out) != CodeNotConfigured {
		t.Fatalf("no keys = %d %v", code, out)
	}
	h.setConfig(withKeys("b", "", "e"))
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Default = "exa" })
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "exa" {
		t.Fatalf("default = %v", out)
	}
	if code, out := h.search(`{"query":"q","provider":"brave"}`); code != 200 || out["provider"] != "brave" || braveGot.header.Get("X-Subscription-Token") != "b" {
		t.Fatalf("explicit = %d %v", code, out)
	}
	// Keyless is off, so tavily (no key) can't search; unknown names never can.
	for _, p := range []string{"tavily", "bing"} {
		code, out := h.search(`{"query":"q","provider":"` + p + `"}`)
		if code != 400 || errCode(out) != CodeProviderUnavailable {
			t.Fatalf("provider %s = %d %v", p, code, out)
		}
	}
	// Chosen default that can't search: Tollgate picks (keyed providers first).
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Default = "tavily" })
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "brave" {
		t.Fatalf("fallback default = %v", out)
	}
	// Empty results are a success.
	if code, out := h.search(`{"query":"q"}`); code != 200 || len(out["results"].([]any)) != 0 {
		t.Fatalf("empty = %d %v", code, out)
	}

	// Keyless on (the default): tavily answers without a key, Brave still needs one.
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Search.Keyless = true
		c.Search.Providers.Brave.APIKey = ""
	})
	if code, out := h.search(`{"query":"q","provider":"tavily"}`); code != 200 || out["provider"] != "tavily" ||
		tavilyGot.header.Get("X-Tavily-Access-Mode") != "keyless" || tavilyGot.header.Get("Authorization") != "" {
		t.Fatalf("keyless tavily = %d %v %v", code, out, tavilyGot.header)
	}
	if code, out := h.search(`{"query":"q","provider":"brave"}`); code != 400 || errCode(out) != CodeProviderUnavailable {
		t.Fatalf("brave without key = %d %v", code, out)
	}
	// "auto" with exa keyed and others keyless: the keyed provider is preferred.
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Default = "auto" })
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "exa" || exaGot.header.Get("x-api-key") != "e" {
		t.Fatalf("auto = %v", out)
	}
}

func TestSearchFailures(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	h.setConfig(withKeys("b", "", ""))
	cases := []struct {
		status   int
		response string
		want     int
		code     string
		upstream any
	}{
		{500, `{"error":"boom"}`, 502, CodeSearchFailed, float64(500)},
		{401, `{"error":"bad key"}`, 502, CodeSearchFailed, float64(401)},
		{200, `not json`, 502, CodeSearchFailed, nil},
	}
	for _, tc := range cases {
		srv := fakeProvider(t, tc.status, tc.response, &got)
		h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: srv.URL}
		code, out := h.search(`{"query":"q"}`)
		if code != tc.want || errCode(out) != tc.code || errField(out, "status") != tc.upstream {
			t.Errorf("provider %d %s = %d %v", tc.status, tc.response, code, out)
		}
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: slow.URL}
	h.svc.SearchTimeout = 200 * time.Millisecond
	if code, out := h.search(`{"query":"q"}`); code != 504 || errCode(out) != CodeTimeout {
		t.Fatalf("timeout = %d %v", code, out)
	}
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: "http://127.0.0.1:1"}
	if code, out := h.search(`{"query":"q"}`); code != 502 || errCode(out) != CodeSearchFailed {
		t.Fatalf("unreachable = %d %v", code, out)
	}
}

func TestSearchValidation(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	srv := fakeProvider(t, 200, `{"web":{"results":[]}}`, &got)
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: srv.URL}
	h.setConfig(withKeys("b", "", ""))
	for _, body := range []string{
		`not json`, `[]`, `{}`, `{"query":"   "}`, `{"query":5}`,
		`{"query":"` + strings.Repeat("ş", 201) + `"}`,
		`{"query":"q","count":0}`, `{"query":"q","count":11}`, `{"query":"q","count":"5"}`,
		`{"query":"q","freshness":"hour"}`,
		`{"query":"q","site":"https://example.com"}`, `{"query":"q","site":"example.com/path"}`, `{"query":"q","site":"localhost"}`,
		`{"query":"q","lang":"turkish"}`,
	} {
		code, out := h.search(body)
		if code != 400 || errCode(out) != CodeInvalidRequest {
			t.Errorf("%s = %d %v", body, code, out)
		}
	}
	if code, _ := h.search(`{"query":"` + strings.Repeat("ş", 200) + `"}`); code != 200 {
		t.Fatalf("200-character query = %d", code)
	}
}

func TestSearchPerMinuteLimit(t *testing.T) {
	h := newHarness(t)
	var got capturedRequest
	srv := fakeProvider(t, 200, `{"web":{"results":[]}}`, &got)
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: srv.URL}
	h.setConfig(withKeys("b", "", ""))
	for i := 0; i < SearchPerMinute; i++ {
		if code, out := h.search(`{"query":"q"}`); code != 200 {
			t.Fatalf("search %d = %d %v", i, code, out)
		}
	}
	code, out, header := h.call(http.MethodPost, "/v1/tools/web_search", webKey, `{"query":"q"}`)
	if code != 429 || errCode(out) != CodeRateLimited || header.Get("Retry-After") == "" || errField(out, "retry_after") == nil {
		t.Fatalf("21st search = %d %v %v", code, out, header)
	}
	// Another key is unaffected.
	if code, _, _ := h.call(http.MethodPost, "/v1/tools/web_search", "tg-another", `{"query":"q"}`); code != 200 {
		t.Fatalf("other key = %d", code)
	}
}

func TestProbeStates(t *testing.T) {
	h := newHarness(t)
	probe := func(key string) map[string]any {
		t.Helper()
		code, out, _ := h.call(http.MethodGet, "/v1/tools", key, "")
		if code != 200 {
			t.Fatalf("probe = %d %v", code, out)
		}
		return out
	}
	tool := func(out map[string]any, name string) map[string]any {
		return out["tools"].(map[string]any)[name].(map[string]any)
	}

	out := probe(modelsKey)
	if s := out["scopes"].([]any); len(s) != 1 || s[0] != "models" {
		t.Fatalf("scopes = %v", out["scopes"])
	}
	for _, name := range []string{"web_search", "web_fetch"} {
		if tl := tool(out, name); tl["available"] != false || tl["reason"] != CodeScopeRequired || tl["provider"] != nil {
			t.Fatalf("%s without web = %v", name, tl)
		}
	}
	limits := out["limits"].(map[string]any)
	if limits["fetch_max_chars"] != float64(20000) || limits["search_per_minute"] != float64(20) || limits["fetch_per_minute"] != float64(40) {
		t.Fatalf("limits = %v", limits)
	}
	if tool(out, "web_fetch")["render"] != false {
		t.Fatal("render must be false")
	}

	// Fresh install: search works without keys through the keyless providers.
	out = probe(webKey)
	keyless := []string{"tavily", "exa", "parallel", "firecrawl", "tinyfish"}
	if tl := tool(out, "web_search"); tl["available"] != true || tl["mode"] != "auto" || tl["provider"] != "tavily" ||
		!slices.Equal(anyToStrings(tl["providers"]), keyless) || !slices.Equal(anyToStrings(tl["keyless"]), keyless) {
		t.Fatalf("search without keys = %v", tl)
	}
	if tl := tool(out, "web_fetch"); tl["available"] != true || tl["reason"] != nil {
		t.Fatalf("fetch without keys = %v", tl)
	}

	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Keyless = false })
	if tl := tool(probe(webKey), "web_search"); tl["available"] != false || tl["reason"] != CodeNotConfigured || tl["providers"] != nil || tl["mode"] != nil {
		t.Fatalf("keyless off without keys = %v", tl)
	}

	h.setConfig(withKeys("", "t", "e"))
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Default = "brave" })
	tl := tool(probe(webKey), "web_search")
	if tl["available"] != true || tl["provider"] != "tavily" || tl["mode"] != "auto" || !slices.Equal(anyToStrings(tl["providers"]), []string{"tavily", "exa"}) ||
		len(anyToStrings(tl["keyless"])) != 0 {
		t.Fatalf("search with keys = %v", tl)
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Default = "exa" })
	if tl := tool(probe(webKey), "web_search"); tl["provider"] != "exa" || tl["mode"] != "default" {
		t.Fatalf("chosen default = %v", tl)
	}
	// Keys and keyless together: keyed providers first in preference, all listed in display order.
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Search.Keyless = true
		c.Search.Default = "auto"
		c.Search.Providers.Tavily.APIKey = ""
		c.Search.Providers.Brave.APIKey = "b"
	})
	tl = tool(probe(webKey), "web_search")
	if tl["provider"] != "brave" || !slices.Equal(anyToStrings(tl["providers"]), []string{"brave", "tavily", "exa", "parallel", "firecrawl", "tinyfish"}) ||
		!slices.Equal(anyToStrings(tl["keyless"]), []string{"tavily", "parallel", "firecrawl", "tinyfish"}) {
		t.Fatalf("mixed = %v", tl)
	}

	h.setConfig(func(c *config.WebToolsConfig) { c.Enabled = false })
	out = probe(webKey)
	for _, name := range []string{"web_search", "web_fetch"} {
		if tl := tool(out, name); tl["available"] != false || tl["reason"] != CodeDisabled || tl["provider"] != nil {
			t.Fatalf("%s disabled = %v", name, tl)
		}
	}
}

func anyToStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}

func TestToolGates(t *testing.T) {
	h := newHarness(t)
	h.setConfig(withKeys("b", "", ""))
	for _, path := range []string{"/v1/tools/web_search", "/v1/tools/web_fetch"} {
		code, out, _ := h.call(http.MethodPost, path, modelsKey, `{"query":"q","url":"https://example.com/"}`)
		if code != 403 || errCode(out) != CodeScopeRequired {
			t.Fatalf("%s without web = %d %v", path, code, out)
		}
		msg, _ := errField(out, "message").(string)
		if msg == "" {
			t.Fatalf("error envelope lacks a message: %v", out)
		}
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Enabled = false })
	for _, path := range []string{"/v1/tools/web_search", "/v1/tools/web_fetch"} {
		code, out, _ := h.call(http.MethodPost, path, webKey, `{"query":"q","url":"https://example.com/"}`)
		if code != 503 || errCode(out) != CodeDisabled {
			t.Fatalf("%s disabled = %d %v", path, code, out)
		}
	}
	if h.requests.Load() != 0 {
		t.Fatal("a refused call reached the site")
	}
}

func TestBlockedDomainFetch(t *testing.T) {
	h := newHarness(t)
	h.setConfig(func(c *config.WebToolsConfig) { c.Domains.List = []string{"example.com"} })
	for _, target := range []string{"https://example.com/", "https://news.example.com/x"} {
		code, out := h.fetch(`{"url":"` + target + `"}`)
		if code != 403 || errCode(out) != CodeBlockedDomain || !strings.Contains(errField(out, "message").(string), "blocked in Tollgate's settings") {
			t.Fatalf("%s = %d %v", target, code, out)
		}
	}
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Domains.Mode = "allowlist"
		c.Domains.List = []string{"wikipedia.org"}
	})
	if code, out := h.fetch(`{"url":"https://example.com/"}`); code != 403 || errCode(out) != CodeBlockedDomain {
		t.Fatalf("allowlist miss = %d %v", code, out)
	}
	if code, out := h.fetch(`{"url":"https://tr.wikipedia.org/wiki/X"}`); code != 200 {
		t.Fatalf("allowlist hit = %d %v", code, out)
	}
}

func TestUserAgent(t *testing.T) {
	cases := map[string]string{
		"tollgate-0.2.0":        "0.2.0",
		"tollgate-v0.2.0":       "0.2.0",
		"tollgate-a41d6e5b12cd": "a41d6e5b12cd",
		"v8.1.0":                "8.1.0",
		"":                      "dev",
	}
	for in, want := range cases {
		if got := userAgentFor(in); got != "Mozilla/5.0 (compatible; CLI-Tollgate/"+want+"; +https://github.com/dylancetin/cli-tollgate)" {
			t.Errorf("userAgentFor(%q) = %q", in, got)
		}
	}
}

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"<strong>Bold</strong> &amp; <em>it</em>": "Bold & it",
		"a\n\n\tb   c":                   "a b c",
		"&amp;quot;double&amp;quot;":     `"double"`,
		"&lt;script&gt;x&lt;/script&gt;": "x",
	}
	for in, want := range cases {
		if got := plainText(in, 300); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("kelime ", 100)
	if got := plainText(long, 300); len([]rune(got)) > 300 || !strings.HasSuffix(got, "…") || strings.HasSuffix(got, " …") {
		t.Errorf("truncated = %q", got)
	}
}

func TestParseDate(t *testing.T) {
	cases := map[string]string{
		"2025-03-01":                "2025-03-01",
		"2025-03-01T08:30:00":       "2025-03-01T08:30:00Z",
		"2025-03-01T08:30:00+03:00": "2025-03-01T05:30:00Z",
		"2024-12-24T00:00:00.000Z":  "2024-12-24T00:00:00Z",
		"March 1, 2025":             "2025-03-01",
		"2 days ago":                "",
		"":                          "",
		"2025-13-01":                "",
	}
	for in, want := range cases {
		if got := parseDate(in); got != want {
			t.Errorf("parseDate(%q) = %q, want %q", in, got, want)
		}
	}
}
