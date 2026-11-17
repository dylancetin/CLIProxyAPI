package webtools

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Providers reached through their hosted MCP servers. Each works without an
// API key (lower limits); a key, when set, is sent and lifts them.

var freshnessWords = map[string]string{"day": "past day", "week": "past week", "month": "past month", "year": "past year"}

// objectiveFor builds the natural-language goal some MCP search tools take,
// carrying hints for filters they have no field for.
func objectiveFor(q SearchQuery) string {
	parts := []string{q.Query}
	if q.Site != "" {
		parts = append(parts, "only pages on "+q.Site)
	}
	if w := freshnessWords[q.Freshness]; w != "" {
		parts = append(parts, "published in the "+w)
	}
	if q.Lang != "" {
		parts = append(parts, "in language "+q.Lang)
	}
	return strings.Join(parts, "; ")
}

var (
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdLinePref = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}[ \t]+|[*+\-][ \t]+|>[ \t]?)`)
	mdEmphasis = regexp.MustCompile("\\*\\*|__|`+")
	mdBullets  = regexp.MustCompile(`(?:^|\s)(?:[*+]\s+){2,}`)
)

// stripMarkdown turns the markdown excerpts MCP search tools return into
// plain text: links and images keep their text, heading, list and quote
// markers and emphasis go. plainText then collapses whitespace.
func stripMarkdown(s string) string {
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdLinePref.ReplaceAllString(s, "")
	s = mdEmphasis.ReplaceAllString(s, "")
	s = mdBullets.ReplaceAllString(s, " ")
	return s
}

func nowOr(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

// --- Exa without a key: the hosted MCP server's web_search_exa tool ---

var (
	exaBlockSplit = regexp.MustCompile(`\n\s*---\s*\n`)
	exaField      = func(name string) *regexp.Regexp { return regexp.MustCompile(`(?m)^` + name + `:[ \t]*(.*)$`) }
	exaURL        = exaField("URL")
	exaTitle      = exaField("Title")
	exaPublished  = exaField("Published")
	exaBody       = regexp.MustCompile(`(?m)^(?:Highlights|Text|Summary):[ \t]*\n?`)
)

func (e Exa) searchKeyless(ctx context.Context, client *http.Client, q SearchQuery) ([]RawResult, error) {
	endpoint := e.MCPEndpoint
	if endpoint == "" {
		endpoint = ExaMCPEndpoint
	}
	result, err := mcpCallTool(ctx, client, endpoint, nil, "web_search_exa", map[string]any{
		"query":      q.Query,
		"objective":  objectiveFor(q),
		"numResults": q.Count,
	})
	if err != nil {
		return nil, err
	}
	return parseExaMCPText(result.text()), nil
}

// parseExaMCPText reads Exa's text reply: blocks of "Title:", "URL:",
// "Published:", "Author:" lines then "Highlights:" (or "Text:") and the
// excerpt, separated by "---" lines.
func parseExaMCPText(text string) []RawResult {
	var out []RawResult
	for _, block := range exaBlockSplit.Split(strings.ReplaceAll(text, "\r\n", "\n"), -1) {
		field := func(re *regexp.Regexp) string {
			m := re.FindStringSubmatch(block)
			if m == nil {
				return ""
			}
			v := strings.TrimSpace(m[1])
			if v == "N/A" {
				return ""
			}
			return v
		}
		link := field(exaURL)
		if link == "" {
			continue
		}
		snippet := ""
		if loc := exaBody.FindStringIndex(block); loc != nil {
			// Highlights are separated by "..." lines.
			var parts []string
			for _, line := range strings.Split(block[loc[1]:], "\n") {
				if line = strings.TrimSpace(line); line == "..." {
					line = "…"
				}
				parts = append(parts, line)
			}
			snippet = strings.Join(parts, "\n")
		}
		out = append(out, RawResult{Title: field(exaTitle), URL: link, Snippet: stripMarkdown(snippet), Published: parseDate(field(exaPublished))})
	}
	return out
}

// --- Parallel ---

// Parallel calls Parallel's search MCP server (web_search tool).
type Parallel struct{ Endpoint string }

func (Parallel) Name() string { return config.WebSearchParallel }

func (p Parallel) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	endpoint := p.Endpoint
	if endpoint == "" {
		endpoint = ParallelEndpoint
	}
	query := q.Query
	if q.Site != "" {
		query += " site:" + q.Site
	}
	var headers map[string]string
	if apiKey != "" {
		headers = map[string]string{"Authorization": "Bearer " + apiKey}
	}
	result, err := mcpCallTool(ctx, client, endpoint, headers, "web_search", map[string]any{
		"objective":      objectiveFor(q),
		"search_queries": []string{query},
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			URL         string   `json:"url"`
			Title       *string  `json:"title"`
			PublishDate *string  `json:"publish_date"`
			Excerpts    []string `json:"excerpts"`
		} `json:"results"`
	}
	payload := []byte(result.StructuredContent)
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(result.text())
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, &providerError{msg: "the provider returned results Tollgate could not read"}
	}
	results := make([]RawResult, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, RawResult{
			Title:     deref(r.Title),
			URL:       r.URL,
			Snippet:   stripMarkdown(strings.Join(r.Excerpts, "\n…\n")),
			Published: parseDate(deref(r.PublishDate)),
		})
	}
	return results, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- Firecrawl ---

// Firecrawl calls Firecrawl's MCP server (firecrawl_search tool).
type Firecrawl struct{ Endpoint string }

func (Firecrawl) Name() string { return config.WebSearchFirecrawl }

var firecrawlTBS = map[string]string{"day": "qdr:d", "week": "qdr:w", "month": "qdr:m", "year": "qdr:y"}

func (f Firecrawl) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	endpoint := f.Endpoint
	if endpoint == "" {
		endpoint = FirecrawlEndpoint
	}
	args := map[string]any{
		"query":   q.Query,
		"limit":   q.Count,
		"sources": []string{"web"},
	}
	if q.Site != "" {
		args["includeDomains"] = []string{q.Site}
	}
	if tbs := firecrawlTBS[q.Freshness]; tbs != "" {
		args["tbs"] = tbs
	}
	var headers map[string]string
	if apiKey != "" {
		headers = map[string]string{"Authorization": "Bearer " + apiKey}
		// Authenticated searches add tool matches unless told not to.
		args["domainTools"] = false
	}
	result, err := mcpCallTool(ctx, client, endpoint, headers, "firecrawl_search", args)
	if err != nil {
		return nil, err
	}
	return parseFirecrawlText(result.text())
}

type firecrawlItem struct {
	URL         string  `json:"url"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Snippet     *string `json:"snippet"`
	Date        *string `json:"date"`
}

// parseFirecrawlText reads the JSON text Firecrawl returns:
// {"success":true,"data":{"web":[...]}} (or "data" as a plain list).
func parseFirecrawlText(text string) ([]RawResult, error) {
	var out struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, &providerError{msg: "the provider returned results Tollgate could not read"}
	}
	var items []firecrawlItem
	var grouped struct {
		Web []firecrawlItem `json:"web"`
	}
	if err := json.Unmarshal(out.Data, &grouped); err == nil && grouped.Web != nil {
		items = grouped.Web
	} else if err := json.Unmarshal(out.Data, &items); err != nil {
		return nil, &providerError{msg: "the provider returned results Tollgate could not read"}
	}
	results := make([]RawResult, 0, len(items))
	for _, r := range items {
		snippet := deref(r.Description)
		if snippet == "" {
			snippet = deref(r.Snippet)
		}
		results = append(results, RawResult{Title: deref(r.Title), URL: r.URL, Snippet: stripMarkdown(snippet), Published: parseDate(deref(r.Date))})
	}
	return results, nil
}

// --- TinyFish ---

// TinyFish calls TinyFish's MCP server (search tool). Keyless calls need no
// session; keyed calls are refused without the Mcp-Session-Id that
// initialize returns.
type TinyFish struct {
	Endpoint string
	Now      func() time.Time
}

func (TinyFish) Name() string { return config.WebSearchTinyFish }

func (t TinyFish) Search(ctx context.Context, client *http.Client, apiKey string, q SearchQuery) ([]RawResult, error) {
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = TinyFishEndpoint
	}
	args := map[string]any{"query": q.Query}
	if q.Site != "" {
		args["include_domains"] = q.Site
	}
	if q.Lang != "" {
		args["language"] = q.Lang
	}
	if period, ok := exaPeriods[q.Freshness]; ok {
		args["after_date"] = nowOr(t.Now).Add(-period).UTC().Format("2006-01-02")
	}
	headers := map[string]string{"X-TinyFish-Access-Mode": "keyless"}
	if apiKey != "" {
		headers = map[string]string{"X-API-Key": apiKey}
		session, err := mcpSession(ctx, client, endpoint, headers)
		if err != nil {
			return nil, err
		}
		headers["Mcp-Session-Id"] = session
	}
	result, err := mcpCallTool(ctx, client, endpoint, headers, "search", args)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			URL     string `json:"url"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
			Date    string `json:"date"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(result.text()), &out); err != nil {
		return nil, &providerError{msg: "the provider returned results Tollgate could not read"}
	}
	results := make([]RawResult, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, RawResult{Title: r.Title, URL: r.URL, Snippet: stripMarkdown(r.Snippet), Published: parseDate(r.Date)})
	}
	return results, nil
}
