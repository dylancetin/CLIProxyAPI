package webtools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// TestLiveKeylessSearch queries every keyless provider's real endpoint once,
// without a key, and checks the response still parses. It needs the network,
// so it only runs with TOLLGATE_LIVE_SEARCH=1:
//
//	TOLLGATE_LIVE_SEARCH=1 go test -run TestLiveKeylessSearch -v ./internal/webtools/
func TestLiveKeylessSearch(t *testing.T) {
	if os.Getenv("TOLLGATE_LIVE_SEARCH") != "1" {
		t.Skip("set TOLLGATE_LIVE_SEARCH=1 to query the real providers")
	}
	providers := []Provider{Tavily{}, Exa{}, Parallel{}, Firecrawl{}, TinyFish{}}
	queries := []SearchQuery{
		{Query: "Kabataş Erkek Lisesi tarihi", Count: 5},
		{Query: "golang release notes", Count: 5, Freshness: "year", Site: "go.dev", Lang: "en"},
	}
	client := &http.Client{}
	for _, p := range providers {
		if !config.WebSearchSupportsKeyless(p.Name()) {
			t.Errorf("%s is not marked keyless", p.Name())
		}
		for _, q := range queries {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			start := time.Now()
			raw, err := p.Search(ctx, client, "", q)
			cancel()
			var perr *providerError
			if errors.As(err, &perr) && perr.rateLimited() {
				t.Logf("%s %q: keyless cap reached (%d, Retry-After %q)", p.Name(), q.Query, perr.status, perr.retryAfter)
				continue
			}
			if err != nil {
				t.Errorf("%s %q: %v", p.Name(), q.Query, err)
				continue
			}
			results := normalizeResults(raw, q, config.WebDomainsConfig{Mode: config.WebDomainsDenylist})
			t.Logf("%s %q: %d raw, %d kept, %v", p.Name(), q.Query, len(raw), len(results), time.Since(start).Round(time.Millisecond))
			for _, r := range results {
				t.Logf("    %s | %s | %s | %.80s", r.Published, r.Site, r.Title, r.Snippet)
			}
			if len(results) == 0 {
				t.Errorf("%s %q: no usable results", p.Name(), q.Query)
			}
		}
	}
}
