package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// ScopeWeb is the access scope the web tools require.
const ScopeWeb = "web"

const (
	defaultMaxChars = 8000
	minMaxChars     = 500
	maxQueryChars   = 200
	maxFindChars    = 200
	maxCount        = 10
	defaultCount    = 5
	maxRequestBody  = 16 << 10
)

// Service serves GET /v1/tools and POST /v1/tools/{web_search,web_fetch}.
type Service struct {
	// Config returns the current web-tools settings (hot-reloaded).
	Config func() config.WebToolsConfig
	// Scopes returns the scopes of an authenticated API key.
	Scopes func(apiKey string) []string
	// Fetcher downloads pages.
	Fetcher *Fetcher
	// SearchClient calls the search providers.
	SearchClient *http.Client
	// Providers maps provider names to adapters.
	Providers map[string]Provider
	// SearchTimeout bounds one provider call.
	SearchTimeout time.Duration

	now     func() time.Time
	limiter *limiter
	cache   *pageCache
	router  *router
}

// UserAgent is the honest User-Agent sent to sites. CLI Tollgate stamps the
// engine as "tollgate-<app version>", so releases report the app version.
func UserAgent() string {
	return userAgentFor(buildinfo.Version)
}

func userAgentFor(version string) string {
	return "Mozilla/5.0 (compatible; CLI-Tollgate/" + cleanVersion(version) + "; +https://github.com/dylancetin/cli-tollgate)"
}

// appVersion is the app version reported to providers.
func appVersion() string { return cleanVersion(buildinfo.Version) }

func cleanVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "tollgate-"), "v")
	if version == "" {
		version = "dev"
	}
	return version
}

// NewService builds a Service with the production fetcher and providers.
func NewService(cfg func() config.WebToolsConfig, scopes func(string) []string) *Service {
	return newService(cfg, scopes, time.Now)
}

func newService(cfg func() config.WebToolsConfig, scopes func(string) []string, now func() time.Time) *Service {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &Service{
		Config:        cfg,
		Scopes:        scopes,
		Fetcher:       &Fetcher{UserAgent: UserAgent()},
		SearchClient:  &http.Client{Transport: transport},
		SearchTimeout: defaultTimeout,
		Providers: map[string]Provider{
			config.WebSearchBrave:     Brave{},
			config.WebSearchTavily:    Tavily{},
			config.WebSearchExa:       Exa{},
			config.WebSearchParallel:  Parallel{},
			config.WebSearchFirecrawl: Firecrawl{},
			config.WebSearchTinyFish:  TinyFish{},
		},
		now:     now,
		limiter: newLimiter(now),
		cache:   newPageCache(now),
		router:  newRouter(now),
	}
}

func apiKeyFrom(c *gin.Context) string {
	if v, ok := c.Get("userApiKey"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func (s *Service) settings() config.WebToolsConfig {
	if s.Config == nil {
		return config.DefaultWebToolsConfig()
	}
	return s.Config()
}

func (s *Service) keyScopes(key string) []string {
	if s.Scopes == nil {
		return []string{"models", ScopeWeb}
	}
	return s.Scopes(key)
}

// Probe handles GET /v1/tools. It always answers 200 for an authenticated key.
func (s *Service) Probe(c *gin.Context) {
	cfg := s.settings()
	key := apiKeyFrom(c)
	scopes := s.keyScopes(key)
	hasWeb := slices.Contains(scopes, ScopeWeb)

	search := gin.H{"available": false}
	fetch := gin.H{"available": false, "render": false}
	switch {
	case !hasWeb:
		search["reason"] = CodeScopeRequired
		fetch["reason"] = CodeScopeRequired
	case !cfg.Enabled:
		search["reason"] = CodeDisabled
		fetch["reason"] = CodeDisabled
	default:
		fetch["available"] = true
		if providers := cfg.Search.Usable(); len(providers) > 0 {
			keyless := []string{}
			for _, name := range providers {
				if cfg.Search.Providers.APIKey(name) == "" {
					keyless = append(keyless, name)
				}
			}
			mode := "default"
			if cfg.Search.IsAuto() {
				mode = config.WebSearchAuto
			}
			search["available"] = true
			search["provider"] = s.router.current(cfg.Search, key)
			search["providers"] = providers
			search["keyless"] = keyless
			search["mode"] = mode
		} else {
			search["reason"] = CodeNotConfigured
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"scopes": scopes,
		"tools":  gin.H{"web_search": search, "web_fetch": fetch},
		"limits": gin.H{
			"fetch_max_chars":   FetchMaxChars,
			"search_per_minute": SearchPerMinute,
			"fetch_per_minute":  FetchPerMinute,
		},
	})
}

// gate checks scope and the enable switch.
func (s *Service) gate(c *gin.Context) (config.WebToolsConfig, string, *toolError) {
	key := apiKeyFrom(c)
	if !slices.Contains(s.keyScopes(key), ScopeWeb) {
		return config.WebToolsConfig{}, key, newError(http.StatusForbidden, CodeScopeRequired, "this key has no web access; ask for the web scope")
	}
	cfg := s.settings()
	if !cfg.Enabled {
		return cfg, key, newError(http.StatusServiceUnavailable, CodeDisabled, "web tools are switched off in Tollgate")
	}
	return cfg, key, nil
}

// decodeBody reads a JSON object; unknown fields are ignored.
func decodeBody(c *gin.Context, out any) *toolError {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBody))
	if err != nil {
		return errInvalid("request body is too large or unreadable")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errInvalid("body must be a JSON object with valid field types")
	}
	return nil
}

var (
	hostnamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	langHint        = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]{2,8})?$`)
)

// searchRequest is the web_search body; unknown fields are ignored.
type searchRequest struct {
	Query     *string `json:"query"`
	Count     *int    `json:"count"`
	Freshness *string `json:"freshness"`
	Site      *string `json:"site"`
	Lang      *string `json:"lang"`
	Provider  *string `json:"provider"`
}

// validateSearch checks a web_search request.
func validateSearch(raw searchRequest) (SearchQuery, string, *toolError) {
	q := SearchQuery{Count: defaultCount}
	if raw.Query == nil {
		return q, "", errInvalid("query is required")
	}
	q.Query = strings.TrimSpace(*raw.Query)
	if n := utf8.RuneCountInString(q.Query); n < 1 || n > maxQueryChars {
		return q, "", errInvalid("query must be 1-%d characters", maxQueryChars)
	}
	if raw.Count != nil {
		if *raw.Count < 1 || *raw.Count > maxCount {
			return q, "", errInvalid("count must be 1-%d", maxCount)
		}
		q.Count = *raw.Count
	}
	if raw.Freshness != nil && *raw.Freshness != "" {
		switch *raw.Freshness {
		case "day", "week", "month", "year":
			q.Freshness = *raw.Freshness
		default:
			return q, "", errInvalid("freshness must be day, week, month or year")
		}
	}
	if raw.Site != nil && strings.TrimSpace(*raw.Site) != "" {
		site := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(*raw.Site)), ".")
		if len(site) > 253 || !hostnamePattern.MatchString(site) {
			return q, "", errInvalid("site must be a hostname such as example.com")
		}
		q.Site = site
	}
	if raw.Lang != nil && strings.TrimSpace(*raw.Lang) != "" {
		lang := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(*raw.Lang), "_", "-"))
		if !langHint.MatchString(lang) {
			return q, "", errInvalid("lang must be an ISO 639-1 code such as tr or en")
		}
		q.Lang = lang[:2]
	}
	provider := ""
	if raw.Provider != nil {
		provider = strings.ToLower(strings.TrimSpace(*raw.Provider))
	}
	return q, provider, nil
}

// Search handles POST /v1/tools/web_search.
func (s *Service) Search(c *gin.Context) {
	outcome := "ok"
	providerName := "-"
	defer func() { log.Infof("web tools: search via %s -> %s", providerName, outcome) }()
	fail := func(err *toolError) {
		outcome = err.Code
		writeError(c, err)
	}

	cfg, key, terr := s.gate(c)
	if terr != nil {
		fail(terr)
		return
	}
	if len(cfg.Search.Usable()) == 0 {
		fail(newError(http.StatusServiceUnavailable, CodeNotConfigured, "no search provider can search: add an API key or allow keyless search in Tollgate"))
		return
	}
	var raw searchRequest
	if terr := decodeBody(c, &raw); terr != nil {
		fail(terr)
		return
	}
	q, provider, terr := validateSearch(raw)
	if terr != nil {
		fail(terr)
		return
	}
	if provider != "" {
		if !cfg.Search.CanSearch(provider) || s.Providers[provider] == nil {
			fail(newError(http.StatusBadRequest, CodeProviderUnavailable, "search provider %q is not available in Tollgate", provider))
			return
		}
		providerName = provider
	}
	release, terr := s.limiter.acquire(key, "search")
	if terr != nil {
		fail(terr)
		return
	}
	defer release()

	// A named provider is tried alone; otherwise the router picks one and
	// moves on to the next when a provider is rate limited.
	var attempted []string
	var lastErr *toolError
	for {
		name := provider
		if name == "" {
			name = s.router.next(cfg.Search, key, attempted)
		} else if len(attempted) > 0 {
			name = ""
		}
		adapter := s.Providers[name]
		if name == "" || adapter == nil {
			break
		}
		attempted = append(attempted, name)
		providerName = strings.Join(attempted, ">")
		results, terr := s.searchOnce(c.Request.Context(), adapter, cfg, name, q)
		if terr == nil {
			c.JSON(http.StatusOK, gin.H{"provider": name, "results": normalizeResults(results, q, cfg.Domains)})
			return
		}
		lastErr = terr
		if terr.UpstreamStatus != http.StatusTooManyRequests && terr.UpstreamStatus != http.StatusPaymentRequired {
			break
		}
	}
	if lastErr == nil {
		// Every usable provider is paused after a rate limit.
		lastErr = &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeSearchFailed,
			Message: "every search provider is rate limiting searches; try again later", UpstreamStatus: http.StatusTooManyRequests}
	}
	if lastErr.UpstreamStatus == http.StatusTooManyRequests || lastErr.UpstreamStatus == http.StatusPaymentRequired {
		if provider != "" {
			lastErr.RetryAfter = s.router.pausedFor(provider, cfg.Search.Providers.APIKey(provider))
		} else {
			lastErr.RetryAfter = s.router.retryAfter(cfg.Search)
		}
	}
	fail(lastErr)
}

// searchOnce calls one provider with the per-call time limit and pauses it
// when it answers with a rate limit.
func (s *Service) searchOnce(parent context.Context, adapter Provider, cfg config.WebToolsConfig, name string, q SearchQuery) ([]RawResult, *toolError) {
	timeout := s.SearchTimeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	apiKey := cfg.Search.Providers.APIKey(name)
	results, err := adapter.Search(ctx, s.SearchClient, apiKey, q)
	if err == nil {
		return results, nil
	}
	var perr *providerError
	if errors.As(err, &perr) && perr.rateLimited() {
		s.router.pause(name, apiKey, perr.retryAfter)
	}
	return nil, classifySearchError(ctx, err)
}

// Fetch handles POST /v1/tools/web_fetch.
func (s *Service) Fetch(c *gin.Context) {
	outcome := "ok"
	host := "-"
	defer func() { log.Infof("web tools: fetch GET %s -> %s", host, outcome) }()
	fail := func(err *toolError) {
		outcome = err.Code
		writeError(c, err)
	}

	cfg, key, terr := s.gate(c)
	if terr != nil {
		fail(terr)
		return
	}
	var raw struct {
		URL      *string `json:"url"`
		MaxChars *int    `json:"maxChars"`
		Offset   *int    `json:"offset"`
		Find     *string `json:"find"`
		Lang     *string `json:"lang"`
	}
	if terr := decodeBody(c, &raw); terr != nil {
		fail(terr)
		return
	}
	if raw.URL == nil || strings.TrimSpace(*raw.URL) == "" {
		fail(errInvalid("url is required"))
		return
	}
	maxChars := defaultMaxChars
	if raw.MaxChars != nil {
		if *raw.MaxChars < minMaxChars || *raw.MaxChars > FetchMaxChars {
			fail(errInvalid("maxChars must be %d-%d", minMaxChars, FetchMaxChars))
			return
		}
		maxChars = *raw.MaxChars
	}
	offset := 0
	if raw.Offset != nil {
		if *raw.Offset < 0 {
			fail(errInvalid("offset must be 0 or more"))
			return
		}
		offset = *raw.Offset
	}
	var find []rune
	if raw.Find != nil && *raw.Find != "" {
		find = []rune(*raw.Find)
		if len(find) > maxFindChars {
			fail(errInvalid("find must be 1-%d characters", maxFindChars))
			return
		}
	}
	acceptLanguage := ""
	if raw.Lang != nil {
		if lang := strings.ToLower(strings.TrimSpace(*raw.Lang)); langHint.MatchString(lang) {
			acceptLanguage = lang + ",en;q=0.8"
		}
	}
	target, terr := parseFetchURL(*raw.URL, cfg.Domains)
	if terr != nil {
		if target == nil {
			if u, ok := hostOf(*raw.URL); ok {
				host = u
			}
		}
		fail(terr)
		return
	}
	host = normalizeHostString(target.Hostname())

	release, terr := s.limiter.acquire(key, "fetch")
	if terr != nil {
		fail(terr)
		return
	}
	defer release()

	p := s.cache.get(key, target.String())
	if p != nil {
		// Settings may have changed since the page was cached.
		if _, terr := parseFetchURL(p.URL, cfg.Domains); terr != nil {
			fail(terr)
			return
		}
	} else {
		got, terr := s.Fetcher.Fetch(c.Request.Context(), target, cfg.Domains, acceptLanguage)
		if terr != nil {
			fail(terr)
			return
		}
		p = extract(got)
		if len(p.Markdown) == 0 {
			fail(newError(http.StatusUnprocessableEntity, CodeNoContent, "nothing readable was found on the page"))
			return
		}
		s.cache.put(key, p, target.String(), p.URL)
	}

	w := sliceWindow(p.Markdown, offset, maxChars, find)
	resp := gin.H{
		"url":         p.URL,
		"title":       nullable(p.Title),
		"siteName":    nullable(p.SiteName),
		"byline":      nullable(p.Byline),
		"published":   nullable(p.Published),
		"lang":        nullable(p.Lang),
		"contentType": p.ContentType,
		"markdown":    string(p.Markdown[w.Start:w.End]),
		"offset":      w.Start,
		"totalChars":  len(p.Markdown),
		"truncated":   false,
	}
	if w.Found != nil {
		resp["found"] = *w.Found
		if *w.Found {
			resp["matchOffset"] = w.MatchAt
		}
	}
	if (w.Found == nil || *w.Found) && w.End < len(p.Markdown) {
		resp["truncated"] = true
		resp["nextOffset"] = w.End
	}
	c.JSON(http.StatusOK, resp)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func normalizeHostString(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// hostOf extracts a host for logging from a URL that failed validation.
func hostOf(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if raw == "" || len(raw) > 255 {
		return "", false
	}
	return strings.ToLower(raw), true
}
