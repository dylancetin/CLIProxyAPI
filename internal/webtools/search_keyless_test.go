package webtools

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// mcpRequest is one JSON-RPC message a fake MCP server received.
type mcpRequest struct {
	header http.Header
	query  map[string][]string
	body   struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
}

type mcpReply struct {
	status      int
	contentType string
	body        string
	header      map[string]string
}

// fakeMCP answers each message with reply(i, request) and records it.
type fakeMCP struct {
	mu       sync.Mutex
	requests []mcpRequest
	srv      *httptest.Server
}

func newFakeMCP(t *testing.T, reply func(i int, r mcpRequest) mcpReply) *fakeMCP {
	t.Helper()
	f := &fakeMCP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req mcpRequest
		req.header = r.Header.Clone()
		req.query = r.URL.Query()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req.body)
		f.mu.Lock()
		i := len(f.requests)
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		out := reply(i, req)
		if out.contentType == "" {
			out.contentType = "application/json"
		}
		for k, v := range out.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", out.contentType)
		if out.status == 0 {
			out.status = 200
		}
		w.WriteHeader(out.status)
		_, _ = w.Write([]byte(out.body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) last() mcpRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *fakeMCP) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fixtureReply serves a recorded live response (testdata/*), as SSE when it
// was recorded as SSE.
func fixtureReply(t *testing.T, name string) mcpReply {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	ct := "application/json"
	if strings.HasPrefix(string(raw), "event:") || strings.HasPrefix(string(raw), "data:") {
		ct = "text/event-stream"
	}
	return mcpReply{contentType: ct, body: string(raw)}
}

func resultField(out map[string]any, i int, field string) any {
	results, _ := out["results"].([]any)
	if i >= len(results) {
		return nil
	}
	return results[i].(map[string]any)[field]
}

func TestSearchExaKeylessMCP(t *testing.T) {
	h := newHarness(t)
	fixture := fixtureReply(t, "exa_mcp.txt")
	mcp := newFakeMCP(t, func(int, mcpRequest) mcpReply { return fixture })
	var restGot capturedRequest
	rest := fakeProvider(t, 200, `{"results":[]}`, &restGot)
	h.svc.Providers[config.WebSearchExa] = Exa{Endpoint: rest.URL, MCPEndpoint: mcp.srv.URL}

	code, out := h.search(`{"query":"Kabataş Erkek Lisesi","count":3,"provider":"exa","site":"meb.gov.tr","freshness":"year"}`)
	// The site filter keeps only the meb.gov.tr result.
	if code != 200 || out["provider"] != "exa" || !slices.Equal(resultURLs(out), []string{"https://ogm.meb.gov.tr/yuz-tarihi-lise/1908-ISTANBUL-BESIKTAS-KABATAS-ERKEK-LISESI.html"}) {
		t.Fatalf("exa keyless = %d %v", code, out)
	}
	req := mcp.last()
	if req.body.Method != "tools/call" || req.body.Params.Name != "web_search_exa" || req.body.Params.Arguments["numResults"] != float64(3) ||
		req.body.Params.Arguments["query"] != "Kabataş Erkek Lisesi" ||
		req.body.Params.Arguments["objective"] != "Kabataş Erkek Lisesi; only pages on meb.gov.tr; published in the past year" {
		t.Fatalf("exa mcp request = %+v", req.body)
	}
	if req.header.Get("Accept") != "application/json, text/event-stream" || len(req.query) != 0 {
		t.Fatalf("exa mcp headers = %v %v", req.header, req.query)
	}
	snippet, _ := resultField(out, 0, "snippet").(string)
	if !strings.HasPrefix(snippet, "Kabataş Erkek Lisesi, 1908 yılında") || resultField(out, 0, "published") != nil ||
		resultField(out, 0, "title") != "TARİHÎ 100 LİSE KİTABI İNTERAKTİF HARİTASI" {
		t.Fatalf("exa result = %v", out["results"])
	}

	_, out = h.search(`{"query":"Kabataş Erkek Lisesi","count":3,"provider":"exa"}`)
	if len(resultURLs(out)) != 3 || resultField(out, 2, "published") != "2026-10-31T14:51:36Z" || resultField(out, 2, "site") != "tr.wikipedia.org" {
		t.Fatalf("exa all = %v", out["results"])
	}
	if restGot.method != "" {
		t.Fatal("keyless exa called the REST API")
	}
	// With a key, the REST API is used instead.
	h.setConfig(withKeys("", "", "exa-key"))
	if _, out := h.search(`{"query":"q","provider":"exa"}`); out["provider"] != "exa" || restGot.header.Get("x-api-key") != "exa-key" {
		t.Fatalf("keyed exa = %v", out)
	}
}

func TestSearchParallelMCP(t *testing.T) {
	h := newHarness(t)
	fixture := fixtureReply(t, "parallel_mcp.json")
	mcp := newFakeMCP(t, func(int, mcpRequest) mcpReply { return fixture })
	h.svc.Providers[config.WebSearchParallel] = Parallel{Endpoint: mcp.srv.URL}

	code, out := h.search(`{"query":"kabataş tarihçe","provider":"parallel","site":"meb.k12.tr","lang":"tr"}`)
	if code != 200 || out["provider"] != "parallel" ||
		!slices.Equal(resultURLs(out), []string{"https://kabataserkeklisesi.meb.k12.tr/icerikler/tarihce_1784484.html"}) {
		t.Fatalf("parallel = %d %v", code, out)
	}
	req := mcp.last()
	if req.body.Params.Name != "web_search" || req.header.Get("Authorization") != "" ||
		!slices.Equal(anyToStrings(req.body.Params.Arguments["search_queries"]), []string{"kabataş tarihçe site:meb.k12.tr"}) ||
		req.body.Params.Arguments["objective"] != "kabataş tarihçe; only pages on meb.k12.tr; in language tr" {
		t.Fatalf("parallel request = %+v %v", req.body, req.header)
	}
	// Title whitespace collapsed, publish_date read.
	if resultField(out, 0, "title") != "Tarihçe - Kabataş Erkek Lisesi" || resultField(out, 0, "published") != "2015-03-30" {
		t.Fatalf("parallel result = %v", out["results"])
	}
	_, out = h.search(`{"query":"q","provider":"parallel"}`)
	if len(resultURLs(out)) != 3 || resultField(out, 0, "published") != nil || resultField(out, 0, "snippet") == "" {
		t.Fatalf("parallel all = %v", out["results"])
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Providers.Parallel.APIKey = "par-key" })
	h.search(`{"query":"q","provider":"parallel"}`)
	if mcp.last().header.Get("Authorization") != "Bearer par-key" {
		t.Fatalf("keyed parallel headers = %v", mcp.last().header)
	}
	// Without structuredContent the JSON text content is read.
	mcp2 := newFakeMCP(t, func(int, mcpRequest) mcpReply {
		return mcpReply{body: `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"results\":[{\"url\":\"https://example.com/a\",\"title\":null,\"excerpts\":[\"x\"]}]}"}]}}`}
	})
	h.svc.Providers[config.WebSearchParallel] = Parallel{Endpoint: mcp2.srv.URL}
	if _, out := h.search(`{"query":"q","provider":"parallel"}`); !slices.Equal(resultURLs(out), []string{"https://example.com/a"}) || resultField(out, 0, "title") != "example.com" {
		t.Fatalf("parallel text fallback = %v", out)
	}
}

func TestSearchFirecrawlMCP(t *testing.T) {
	h := newHarness(t)
	fixture := fixtureReply(t, "firecrawl_mcp.txt")
	mcp := newFakeMCP(t, func(int, mcpRequest) mcpReply { return fixture })
	h.svc.Providers[config.WebSearchFirecrawl] = Firecrawl{Endpoint: mcp.srv.URL}

	code, out := h.search(`{"query":"kabataş","provider":"firecrawl","count":2,"freshness":"week","site":"mapy.com"}`)
	if code != 200 || out["provider"] != "firecrawl" || !slices.Equal(resultURLs(out), []string{"https://mapy.com/en/?source=osm&id=11444063"}) {
		t.Fatalf("firecrawl = %d %v", code, out)
	}
	args := mcp.last().body.Params.Arguments
	if mcp.last().body.Params.Name != "firecrawl_search" || args["limit"] != float64(2) || args["tbs"] != "qdr:w" ||
		!slices.Equal(anyToStrings(args["includeDomains"]), []string{"mapy.com"}) || !slices.Equal(anyToStrings(args["sources"]), []string{"web"}) ||
		args["domainTools"] != nil || mcp.last().header.Get("Authorization") != "" {
		t.Fatalf("firecrawl request = %+v", mcp.last().body)
	}
	if resultField(out, 0, "title") != "Kabataş Boys High School" || !strings.HasPrefix(resultField(out, 0, "snippet").(string), "Founded in 1908") {
		t.Fatalf("firecrawl result = %v", out["results"])
	}
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Providers.Firecrawl.APIKey = "fc-key" })
	_, out = h.search(`{"query":"q","provider":"firecrawl"}`)
	if len(resultURLs(out)) != 3 || mcp.last().header.Get("Authorization") != "Bearer fc-key" || mcp.last().body.Params.Arguments["domainTools"] != false {
		t.Fatalf("keyed firecrawl = %v %v", out, mcp.last())
	}
	// Older servers return "data" as a plain list.
	results, err := parseFirecrawlText(`{"success":true,"data":[{"url":"https://example.com/x","title":"X","description":"d"}]}`)
	if err != nil || len(results) != 1 || results[0].Snippet != "d" {
		t.Fatalf("list data = %v %v", results, err)
	}
	if _, err := parseFirecrawlText(`Search failed`); err == nil {
		t.Fatal("non-JSON text must fail")
	}
}

func TestSearchTinyFishMCP(t *testing.T) {
	h := newHarness(t)
	fixture := fixtureReply(t, "tinyfish_mcp.json")
	mcp := newFakeMCP(t, func(_ int, r mcpRequest) mcpReply {
		switch r.body.Method {
		case "initialize":
			return mcpReply{body: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`, header: map[string]string{"Mcp-Session-Id": "sess-1"}}
		case "notifications/initialized":
			return mcpReply{status: 202}
		}
		if r.header.Get("X-API-Key") != "" && r.header.Get("Mcp-Session-Id") != "sess-1" {
			return mcpReply{status: 400, body: `{"error":"session required"}`}
		}
		return fixture
	})
	h.svc.Providers[config.WebSearchTinyFish] = TinyFish{Endpoint: mcp.srv.URL, Now: func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC) }}

	code, out := h.search(`{"query":"Kabataş Erkek Lisesi","provider":"tinyfish","lang":"tr","freshness":"month","site":"wikipedia.org"}`)
	if code != 200 || out["provider"] != "tinyfish" || !slices.Equal(resultURLs(out), []string{"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi"}) {
		t.Fatalf("tinyfish = %d %v", code, out)
	}
	req := mcp.last()
	args := req.body.Params.Arguments
	if mcp.count() != 1 || req.body.Params.Name != "search" || req.header.Get("X-TinyFish-Access-Mode") != "keyless" ||
		args["include_domains"] != "wikipedia.org" || args["language"] != "tr" || args["after_date"] != "2026-10-17" {
		t.Fatalf("tinyfish request = %+v %v", req.body, req.header)
	}
	// The http:// result is dropped; "by AB Çelik" is not a date.
	_, out = h.search(`{"query":"q","provider":"tinyfish"}`)
	if !slices.Equal(resultURLs(out), []string{
		"https://tr.wikipedia.org/wiki/Kabata%C5%9F_Erkek_Lisesi",
		"https://albainsaat.com/en/kabatas-erkek-lisesi-restorasyon",
		"https://repository.bilkent.edu.tr/items/1f7f631c-0b51-41ac-8785-97da97207d9f",
	}) || resultField(out, 2, "published") != nil {
		t.Fatalf("tinyfish all = %v", out["results"])
	}

	// With a key: initialize, notifications/initialized, then the call with the session.
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Providers.TinyFish.APIKey = "tf-key" })
	before := mcp.count()
	if code, out := h.search(`{"query":"q","provider":"tinyfish"}`); code != 200 || len(resultURLs(out)) != 3 {
		t.Fatalf("keyed tinyfish = %d %v", code, out)
	}
	mcp.mu.Lock()
	seq := mcp.requests[before:]
	mcp.mu.Unlock()
	if len(seq) != 3 || seq[0].body.Method != "initialize" || seq[1].body.Method != "notifications/initialized" ||
		seq[1].header.Get("Mcp-Session-Id") != "sess-1" || seq[2].header.Get("X-API-Key") != "tf-key" || seq[2].header.Get("X-TinyFish-Access-Mode") != "" {
		t.Fatalf("keyed sequence = %+v", seq)
	}
}

func TestParseMCPBody(t *testing.T) {
	cases := []struct {
		name, body string
		ok         bool
		isErr      bool
	}{
		{"json", `{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`, true, false},
		{"sse", "event: message\ndata: {\"result\":{\"content\":[]}}\n\n", true, false},
		{"sse no space crlf", "event: message\r\ndata:{\"result\":{\"content\":[]}}\r\n\r\n", true, false},
		{"sse multi-line data", "data: {\"result\":\ndata: {\"content\":[]}}\n\n", true, false},
		{"sse ping first", "event: ping\ndata: {}\n\nevent: message\ndata: {\"result\":{\"content\":[]}}\n", true, false},
		{"rpc error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`, true, true},
		{"garbage", `<html>`, false, false},
		{"empty", ``, false, false},
	}
	for _, tc := range cases {
		env, ok := parseMCPBody([]byte(tc.body))
		if ok != tc.ok || (ok && (env.Error != nil) != tc.isErr) {
			t.Errorf("%s: ok=%v env=%+v", tc.name, ok, env)
		}
	}
}

func TestMCPToolErrors(t *testing.T) {
	h := newHarness(t)
	var reply mcpReply
	mcp := newFakeMCP(t, func(int, mcpRequest) mcpReply { return reply })
	h.svc.Providers[config.WebSearchFirecrawl] = Firecrawl{Endpoint: mcp.srv.URL}
	cases := []struct {
		reply    mcpReply
		upstream any
	}{
		{mcpReply{body: `{"result":{"isError":true,"content":[{"type":"text","text":"Rate limit exceeded, retry later"}]}}`}, float64(429)},
		{mcpReply{body: `{"result":{"isError":true,"content":[{"type":"text","text":"Invalid API key"}]}}`}, float64(401)},
		{mcpReply{body: `{"error":{"code":-32602,"message":"bad arguments"}}`}, nil},
		{mcpReply{status: 503, body: `down`}, float64(503)},
		{mcpReply{body: `{"result":{"content":[{"type":"text","text":"not json"}]}}`}, nil},
	}
	for i, tc := range cases {
		reply = tc.reply
		code, out := h.search(`{"query":"q","provider":"firecrawl"}`)
		if code != 502 || errCode(out) != CodeSearchFailed || errField(out, "status") != tc.upstream {
			t.Errorf("case %d = %d %v", i, code, out)
		}
		h.advance(2 * time.Minute) // let any pause expire
	}
}

// --- router ---

// rateLimitedThenOK serves 429 (with retryAfter) for the first n calls.
func providerServer(t *testing.T, status func() (int, string), calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		*calls++
		mu.Unlock()
		code, retryAfter := status()
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == 200 {
			_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://example.com/r","content":"c"}],"web":{"results":[{"title":"T","url":"https://example.com/r","description":"c"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"limited"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRouterFailoverAndCooldown(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var tavilyCalls, braveCalls int
	tavilyStatus := func() (int, string) { return 429, "120" }
	tavily := providerServer(t, func() (int, string) { return tavilyStatus() }, &tavilyCalls, &mu)
	brave := providerServer(t, func() (int, string) { return 200, "" }, &braveCalls, &mu)
	h.svc.Providers[config.WebSearchTavily] = Tavily{Endpoint: tavily.URL}
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: brave.URL}
	// Only tavily (keyless) and brave (keyed) can search.
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Search.Keyless = false
		c.Search.Providers.Tavily.APIKey = "tv"
		c.Search.Providers.Brave.APIKey = "b"
		c.Search.Default = "tavily"
	})

	// Default answers 429: paused, brave answers, the response names it.
	code, out := h.search(`{"query":"q"}`)
	if code != 200 || out["provider"] != "brave" || tavilyCalls != 1 || braveCalls != 1 {
		t.Fatalf("failover = %d %v (tavily %d, brave %d)", code, out, tavilyCalls, braveCalls)
	}
	// While paused, the default is skipped without a call.
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "brave" || tavilyCalls != 1 {
		t.Fatalf("paused = %v (tavily %d)", out, tavilyCalls)
	}
	// The probe reports what would answer now.
	if _, probe, _ := h.call(http.MethodGet, "/v1/tools", webKey, ""); probe["tools"].(map[string]any)["web_search"].(map[string]any)["provider"] != "brave" {
		t.Fatalf("probe while paused = %v", probe)
	}
	// A search naming the paused provider is still tried (no failover) and reports the pause.
	code, out = h.search(`{"query":"q","provider":"tavily"}`)
	if code != 502 || errCode(out) != CodeSearchFailed || errField(out, "status") != float64(429) || errField(out, "retry_after") != float64(120) || tavilyCalls != 2 {
		t.Fatalf("named paused = %d %v", code, out)
	}
	// Retry-After elapsed: the default is used again.
	tavilyStatus = func() (int, string) { return 200, "" }
	h.advance(121 * time.Second)
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "tavily" || tavilyCalls != 3 {
		t.Fatalf("after cooldown = %v", out)
	}
	// A new key lifts the pause at once.
	tavilyStatus = func() (int, string) { return 429, "" }
	h.search(`{"query":"q"}`)
	tavilyStatus = func() (int, string) { return 200, "" }
	h.setConfig(func(c *config.WebToolsConfig) { c.Search.Providers.Tavily.APIKey = "tv-new" })
	if _, out := h.search(`{"query":"q"}`); out["provider"] != "tavily" {
		t.Fatalf("new key = %v", out)
	}
}

func TestRouterAllRateLimited(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var calls int
	limited := providerServer(t, func() (int, string) { return 429, "30" }, &calls, &mu)
	limitedNoHeader := providerServer(t, func() (int, string) { return 402, "" }, &calls, &mu)
	h.svc.Providers[config.WebSearchTavily] = Tavily{Endpoint: limited.URL}
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: limitedNoHeader.URL}
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Search.Keyless = false
		c.Search.Providers.Tavily.APIKey = "tv"
		c.Search.Providers.Brave.APIKey = "b"
	})
	// Both tried; the error is the last 429-like one, retry_after the soonest unpause.
	code, out := h.search(`{"query":"q"}`)
	if code != 502 || errCode(out) != CodeSearchFailed || calls != 2 || errField(out, "retry_after") != float64(30) {
		t.Fatalf("all limited = %d %v (calls %d)", code, out, calls)
	}
	// Everything paused: no provider is called.
	code, out, header := h.call(http.MethodPost, "/v1/tools/web_search", webKey, `{"query":"q"}`)
	if code != 502 || errCode(out) != CodeSearchFailed || errField(out, "status") != float64(429) || calls != 2 || header.Get("Retry-After") != "30" {
		t.Fatalf("all paused = %d %v (calls %d)", code, out, calls)
	}
	h.advance(31 * time.Second)
	h.search(`{"query":"q"}`)
	if calls != 3 {
		t.Fatalf("tavily unpaused after 30 s, brave after 60 s; calls = %d", calls)
	}
}

func TestRouterNoFailoverOnOtherErrors(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var badCalls, goodCalls int
	bad := providerServer(t, func() (int, string) { return 500, "" }, &badCalls, &mu)
	good := providerServer(t, func() (int, string) { return 200, "" }, &goodCalls, &mu)
	h.svc.Providers[config.WebSearchTavily] = Tavily{Endpoint: bad.URL}
	h.svc.Providers[config.WebSearchBrave] = Brave{Endpoint: good.URL}
	h.setConfig(func(c *config.WebToolsConfig) {
		c.Search.Keyless = false
		c.Search.Providers.Tavily.APIKey = "tv"
		c.Search.Providers.Brave.APIKey = "b"
		c.Search.Default = "tavily"
	})
	if code, out := h.search(`{"query":"q"}`); code != 502 || errField(out, "status") != float64(500) || goodCalls != 0 {
		t.Fatalf("500 = %d %v", code, out)
	}
	// Not paused: tried again next time.
	h.search(`{"query":"q"}`)
	if badCalls != 2 {
		t.Fatalf("bad calls = %d", badCalls)
	}
}

func TestRouterAutoSticky(t *testing.T) {
	r := newRouter(func() time.Time { return time.Unix(0, 0) })
	picks := 0
	r.pick = func(n int) int { picks++; return n - 1 }
	cfg := config.DefaultWebToolsConfig().Search
	// Keyless only: random among them, then sticky per app key.
	first := r.next(cfg, "app-1", nil)
	if first != "tinyfish" || r.next(cfg, "app-1", nil) != "tinyfish" || picks != 1 {
		t.Fatalf("sticky = %s picks %d", first, picks)
	}
	if r.current(cfg, "app-1") != "tinyfish" || r.current(cfg, "app-2") != "tavily" {
		t.Fatalf("current = %s / %s", r.current(cfg, "app-1"), r.current(cfg, "app-2"))
	}
	// Failover within the same search skips attempted ones and moves the affinity.
	if got := r.next(cfg, "app-1", []string{"tinyfish"}); got != "firecrawl" || r.affinity["app-1"] != "firecrawl" {
		t.Fatalf("failover pick = %s", got)
	}
	// A keyed provider wins over keyless ones once it exists, but affinity holds while it is open.
	cfg.Providers.Exa.APIKey = "e"
	if got := r.next(cfg, "app-1", nil); got != "firecrawl" {
		t.Fatalf("affinity kept = %s", got)
	}
	if got := r.next(cfg, "app-3", nil); got != "exa" {
		t.Fatalf("keyed preferred = %s", got)
	}
	// Paused sticky provider: another is picked.
	r.pause("firecrawl", "", "10")
	if got := r.next(cfg, "app-1", nil); got != "exa" {
		t.Fatalf("paused sticky = %s", got)
	}
	// Fixed default mode never touches affinity.
	cfg.Default = "parallel"
	delete(r.affinity, "app-9")
	if got := r.next(cfg, "app-9", nil); got != "parallel" || r.affinity["app-9"] != "" {
		t.Fatalf("fixed = %s", got)
	}
}

func TestCooldownFor(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              defaultCooldown,
		"120":                           120 * time.Second,
		"0":                             0,
		"-5":                            defaultCooldown,
		"soon":                          defaultCooldown,
		"Tue, 17 Nov 2026 12:01:30 GMT": 90 * time.Second,
		"Tue, 17 Nov 2026 11:00:00 GMT": 0,
		"999999":                        maxCooldown,
	}
	for in, want := range cases {
		if got := cooldownFor(in, now); got != want {
			t.Errorf("cooldownFor(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWebSearchConfigUsable(t *testing.T) {
	c := config.DefaultWebToolsConfig().Search
	if c.Default != "auto" || !c.Keyless || !slices.Equal(c.Usable(), []string{"tavily", "exa", "parallel", "firecrawl", "tinyfish"}) || c.CanSearch("brave") {
		t.Fatalf("defaults = %+v usable %v", c, c.Usable())
	}
	c.Providers.Brave.APIKey = " b "
	c.Keyless = false
	if !slices.Equal(c.Usable(), []string{"brave"}) || !c.IsAuto() {
		t.Fatalf("keyless off = %v", c.Usable())
	}
	c.Default = "brave"
	if c.IsAuto() || c.CanSearch("bing") {
		t.Fatal("brave with a key is a usable default")
	}
	cfg := config.Config{WebTools: config.WebToolsConfig{Search: config.WebSearchConfig{Default: " TinyFish "}}}
	cfg.NormalizeWebToolsConfig()
	if cfg.WebTools.Search.Default != "tinyfish" {
		t.Fatalf("normalized = %q", cfg.WebTools.Search.Default)
	}
	cfg.WebTools.Search.Default = "bing"
	cfg.NormalizeWebToolsConfig()
	if cfg.WebTools.Search.Default != "auto" {
		t.Fatalf("unknown default = %q", cfg.WebTools.Search.Default)
	}
}

func TestStripMarkdown(t *testing.T) {
	cases := map[string]string{
		"+ Get connected [](groups.google.com/g/golang-nuts) [](github.com/golang) news": "Get connected   news",
		"# Go 1.27 Release Notes\n## Introduction":                                       "Go 1.27 Release Notes\nIntroduction",
		"See [the docs](https://go.dev/doc) and ![logo](x.png).":                         "See the docs and logo.",
		"**Bold** and `code` and __under__":                                              "Bold and code and under",
		"Toggle navigation * * * T.C. Millî":                                             "Toggle navigation T.C. Millî",
		"> quoted\n- item\n* item":                                                       "quoted\nitem\nitem",
		"2 * 3 = 6":                                                                      "2 * 3 = 6",
	}
	for in, want := range cases {
		if got := stripMarkdown(in); got != want {
			t.Errorf("stripMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}
