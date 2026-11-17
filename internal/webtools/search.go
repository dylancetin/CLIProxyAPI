package webtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const (
	maxTitleChars   = 200
	maxSnippetChars = 300
	maxProviderBody = 2 << 20
)

// SearchQuery is a validated web_search request.
type SearchQuery struct {
	Query     string
	Count     int
	Freshness string // "", day, week, month, year
	Site      string
	Lang      string // ISO 639-1, lowercase
}

// RawResult is one provider result before normalization.
type RawResult struct {
	Title     string
	URL       string
	Snippet   string
	Published string
}

// SearchResult is one normalized result.
type SearchResult struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
	Site      string `json:"site"`
}

// Provider is one search API.
type Provider interface {
	Name() string
	Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error)
}

// providerError is a provider failure with the upstream status, if any.
type providerError struct {
	status int
	msg    string
	// retryAfter is the provider's Retry-After header, raw, if any.
	retryAfter string
}

// rateLimited reports whether the provider refused for quota reasons (429, or
// 402 from keyless modes that ask for payment once a free cap is used up).
func (e *providerError) rateLimited() bool {
	return e.status == http.StatusTooManyRequests || e.status == http.StatusPaymentRequired
}

func statusError(resp *http.Response) *providerError {
	return &providerError{
		status:     resp.StatusCode,
		msg:        fmt.Sprintf("the provider answered %d %s", resp.StatusCode, statusText(resp.StatusCode)),
		retryAfter: resp.Header.Get("Retry-After"),
	}
}

func (e *providerError) Error() string { return e.msg }

// Default provider API endpoints.
const (
	BraveEndpoint     = "https://api.search.brave.com/res/v1/web/search"
	TavilyEndpoint    = "https://api.tavily.com/search"
	ExaEndpoint       = "https://api.exa.ai/search"
	ExaMCPEndpoint    = "https://mcp.exa.ai/mcp"
	ParallelEndpoint  = "https://search.parallel.ai/mcp"
	FirecrawlEndpoint = "https://mcp.firecrawl.dev/v2/mcp"
	TinyFishEndpoint  = "https://agent.tinyfish.ai/mcp"
)

// clientName identifies Tollgate to providers that ask for it.
const clientName = "cli-tollgate"

// doJSON sends req and decodes a JSON response into out.
func doJSON(client *http.Client, req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &providerError{msg: "the provider returned invalid JSON"}
	}
	return nil
}

func postJSON(ctx context.Context, endpoint string, payload any) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// --- Brave Search API ---

// Brave calls the Brave Search API web search endpoint.
type Brave struct{ Endpoint string }

func (Brave) Name() string { return config.WebSearchBrave }

var braveFreshness = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}

// braveLangs maps ISO 639-1 codes to Brave's search_lang values (most are the
// same code; Brave only accepts its own list).
var braveLangs = map[string]string{
	"ar": "ar", "eu": "eu", "bn": "bn", "bg": "bg", "ca": "ca", "zh": "zh-hans", "hr": "hr", "cs": "cs",
	"da": "da", "nl": "nl", "en": "en", "et": "et", "fi": "fi", "fr": "fr", "gl": "gl", "de": "de",
	"gu": "gu", "he": "he", "hi": "hi", "hu": "hu", "is": "is", "it": "it", "ja": "jp", "kn": "kn",
	"ko": "ko", "lv": "lv", "lt": "lt", "ms": "ms", "ml": "ml", "mr": "mr", "nb": "nb", "no": "nb",
	"pl": "pl", "pt": "pt-br", "pa": "pa", "ro": "ro", "ru": "ru", "sr": "sr", "sk": "sk", "sl": "sl",
	"es": "es", "sv": "sv", "ta": "ta", "te": "te", "th": "th", "tr": "tr", "uk": "uk", "vi": "vi",
}

func (b Brave) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	endpoint := b.Endpoint
	if endpoint == "" {
		endpoint = BraveEndpoint
	}
	query := q.Query
	if q.Site != "" {
		query += " site:" + q.Site
	}
	params := url.Values{"q": {query}, "count": {strconv.Itoa(q.Count)}}
	if f := braveFreshness[q.Freshness]; f != "" {
		params.Set("freshness", f)
	}
	if lang := braveLangs[q.Lang]; lang != "" {
		params.Set("search_lang", lang)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", apiKey)
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				PageAge     string `json:"page_age"`
				Age         string `json:"age"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	results := make([]RawResult, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		published := parseDate(r.PageAge)
		if published == "" {
			published = parseDate(r.Age)
		}
		results = append(results, RawResult{Title: r.Title, URL: r.URL, Snippet: r.Description, Published: published})
	}
	return results, nil
}

// --- Tavily ---

// Tavily calls the Tavily search API. Without an API key it uses Tavily's
// keyless access mode (a small daily cap per IP address).
type Tavily struct{ Endpoint string }

func (Tavily) Name() string { return config.WebSearchTavily }

func (t Tavily) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = TavilyEndpoint
	}
	payload := map[string]any{
		"query":                  q.Query,
		"max_results":            q.Count,
		"search_depth":           "basic",
		"topic":                  "general",
		"include_answer":         false,
		"include_raw_content":    false,
		"include_images":         false,
		"include_published_date": true,
	}
	if q.Freshness != "" {
		payload["time_range"] = q.Freshness
	}
	if q.Site != "" {
		payload["include_domains"] = []string{q.Site}
	}
	req, err := postJSON(ctx, endpoint, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Client-Name", clientName)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Set("X-Tavily-Access-Mode", "keyless")
	}
	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	results := make([]RawResult, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, RawResult{Title: r.Title, URL: r.URL, Snippet: r.Content, Published: parseDate(r.PublishedDate)})
	}
	return results, nil
}

// --- Exa ---

// Exa calls the Exa search API with an API key, and Exa's hosted MCP server
// (which needs none) without one.
type Exa struct {
	Endpoint    string
	MCPEndpoint string
	Now         func() time.Time
}

func (Exa) Name() string { return config.WebSearchExa }

var exaPeriods = map[string]time.Duration{
	"day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 31 * 24 * time.Hour, "year": 365 * 24 * time.Hour,
}

func (e Exa) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	if apiKey == "" {
		return e.searchKeyless(ctx, client, q)
	}
	endpoint := e.Endpoint
	if endpoint == "" {
		endpoint = ExaEndpoint
	}
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	payload := map[string]any{
		"query":      q.Query,
		"type":       "auto",
		"numResults": q.Count,
		"contents": map[string]any{
			"highlights": map[string]any{"maxCharacters": maxSnippetChars},
		},
	}
	if period, ok := exaPeriods[q.Freshness]; ok {
		payload["startPublishedDate"] = now().Add(-period).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if q.Site != "" {
		payload["includeDomains"] = []string{q.Site}
	}
	req, err := postJSON(ctx, endpoint, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", apiKey)
	var out struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			PublishedDate string   `json:"publishedDate"`
			Highlights    []string `json:"highlights"`
			Summary       string   `json:"summary"`
			Text          string   `json:"text"`
		} `json:"results"`
	}
	if err := doJSON(client, req, &out); err != nil {
		return nil, err
	}
	results := make([]RawResult, 0, len(out.Results))
	for _, r := range out.Results {
		snippet := strings.Join(r.Highlights, " … ")
		if strings.TrimSpace(snippet) == "" {
			snippet = r.Summary
		}
		if strings.TrimSpace(snippet) == "" {
			snippet = r.Text
		}
		results = append(results, RawResult{Title: r.Title, URL: r.URL, Snippet: snippet, Published: parseDate(r.PublishedDate)})
	}
	return results, nil
}

// --- normalization ---

var (
	tagPattern = regexp.MustCompile(`<[^>]*>`)
	dateOnly   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// plainText strips tags, decodes entities and collapses whitespace.
func plainText(s string, max int) string {
	s = tagPattern.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// Some providers double-encode (&amp;quot;).
	s = html.UnescapeString(s)
	s = tagPattern.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, max)
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)[:max-1]
	cut := string(runes)
	// Prefer cutting at a word boundary in the last fifth.
	if i := strings.LastIndex(cut, " "); i > 0 && utf8.RuneCountInString(cut[:i]) > max*4/5 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// parseDate returns an ISO 8601 date or date-time for a provider date, or "".
func parseDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if dateOnly.MatchString(s) {
		if _, err := time.Parse("2006-01-02", s); err == nil {
			return s
		}
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04:05.000", "2006-01-02 15:04:05", time.RFC1123, time.RFC1123Z} {
		// Layouts without a zone parse as UTC.
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	for _, layout := range []string{"January 2, 2006", "Jan 2, 2006", "2 January 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// normalizeResults turns raw results into the contract shape: https only,
// public hosts, allowed by the domain list and the site filter, unique URLs,
// at most count.
func normalizeResults(raw []RawResult, q SearchQuery, domains config.WebDomainsConfig) []SearchResult {
	out := make([]SearchResult, 0, len(raw))
	seen := map[string]bool{}
	for _, r := range raw {
		u, ok := resultURL(r.URL, domains)
		if !ok {
			continue
		}
		host := normalizeHost(u)
		if q.Site != "" && !domainMatches(host, q.Site) {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		title := plainText(r.Title, maxTitleChars)
		if title == "" {
			title = host
		}
		out = append(out, SearchResult{
			Title:     title,
			URL:       u.String(),
			Snippet:   plainText(r.Snippet, maxSnippetChars),
			Published: r.Published,
			Site:      host,
		})
		if len(out) >= q.Count {
			break
		}
	}
	return out
}

// classifySearchError maps provider failures to tool errors.
func classifySearchError(ctx context.Context, err error) *toolError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return errTimeout("the search provider")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errTimeout("the search provider")
	}
	var perr *providerError
	if errors.As(err, &perr) {
		return &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeSearchFailed, Message: perr.msg, UpstreamStatus: perr.status}
	}
	return &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeSearchFailed, Message: "the search provider could not be reached"}
}
