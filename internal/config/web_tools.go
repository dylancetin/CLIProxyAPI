package config

import "strings"

// Web search provider names, in the order clients list them.
const (
	WebSearchBrave     = "brave"
	WebSearchTavily    = "tavily"
	WebSearchExa       = "exa"
	WebSearchParallel  = "parallel"
	WebSearchFirecrawl = "firecrawl"
	WebSearchTinyFish  = "tinyfish"
)

// WebSearchAuto is the search default that lets Tollgate pick a provider per
// app and move on to another one when a provider is rate limited.
const WebSearchAuto = "auto"

// WebSearchProviderNames lists the supported web search providers in display
// order. The first three keep their v1 order.
var WebSearchProviderNames = []string{
	WebSearchBrave, WebSearchTavily, WebSearchExa,
	WebSearchParallel, WebSearchFirecrawl, WebSearchTinyFish,
}

// webSearchKeyless marks the providers that answer without an API key (with
// lower limits). Brave has no keyless mode.
var webSearchKeyless = map[string]bool{
	WebSearchTavily:    true,
	WebSearchExa:       true,
	WebSearchParallel:  true,
	WebSearchFirecrawl: true,
	WebSearchTinyFish:  true,
}

// WebSearchSupportsKeyless reports whether a provider can search without an API key.
func WebSearchSupportsKeyless(name string) bool { return webSearchKeyless[name] }

// IsWebSearchProvider reports whether name is a supported provider.
func IsWebSearchProvider(name string) bool {
	for _, p := range WebSearchProviderNames {
		if p == name {
			return true
		}
	}
	return false
}

// Web tools domain list modes.
const (
	WebDomainsDenylist  = "denylist"
	WebDomainsAllowlist = "allowlist"
)

// WebToolsConfig configures the /v1/tools web_search and web_fetch endpoints
// that let approved local apps search the web and read pages through this
// machine.
type WebToolsConfig struct {
	// Enabled switches both web tools on or off. Default true.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Search configures the search providers.
	Search WebSearchConfig `yaml:"search" json:"search"`
	// Domains restricts which hosts the tools may reach or return.
	Domains WebDomainsConfig `yaml:"domains" json:"domains"`
}

// WebSearchConfig holds the search provider keys and the default provider.
type WebSearchConfig struct {
	// Default names the provider used when a search names none (one of
	// WebSearchProviderNames), or "auto" to let Tollgate pick.
	Default string `yaml:"default" json:"default"`
	// Keyless lets providers that support it search without an API key.
	// Default true; when false only providers with a key can search.
	Keyless bool `yaml:"keyless" json:"keyless"`
	// Providers holds one API key per provider.
	Providers WebSearchProviders `yaml:"providers" json:"providers"`
}

// WebSearchProviders holds the per-provider settings.
type WebSearchProviders struct {
	Brave     WebSearchProviderConfig `yaml:"brave" json:"brave"`
	Tavily    WebSearchProviderConfig `yaml:"tavily" json:"tavily"`
	Exa       WebSearchProviderConfig `yaml:"exa" json:"exa"`
	Parallel  WebSearchProviderConfig `yaml:"parallel" json:"parallel"`
	Firecrawl WebSearchProviderConfig `yaml:"firecrawl" json:"firecrawl"`
	TinyFish  WebSearchProviderConfig `yaml:"tinyfish" json:"tinyfish"`
}

// WebSearchProviderConfig holds one provider's API key (optional for
// providers that support keyless use).
type WebSearchProviderConfig struct {
	APIKey string `yaml:"api-key" json:"api-key"`
}

// WebDomainsConfig is the domain deny/allow list. An entry matches the host
// and its subdomains.
type WebDomainsConfig struct {
	// Mode is denylist (default) or allowlist.
	Mode string `yaml:"mode" json:"mode"`
	// List holds hostnames.
	List []string `yaml:"list" json:"list"`
}

// DefaultWebToolsConfig returns the settings used when config.yaml has no web-tools section.
func DefaultWebToolsConfig() WebToolsConfig {
	return WebToolsConfig{
		Enabled: true,
		Search:  WebSearchConfig{Default: WebSearchAuto, Keyless: true},
		Domains: WebDomainsConfig{Mode: WebDomainsDenylist, List: []string{}},
	}
}

// APIKey returns the trimmed API key configured for a provider, or "".
func (p WebSearchProviders) APIKey(name string) string {
	switch name {
	case WebSearchBrave:
		return strings.TrimSpace(p.Brave.APIKey)
	case WebSearchTavily:
		return strings.TrimSpace(p.Tavily.APIKey)
	case WebSearchExa:
		return strings.TrimSpace(p.Exa.APIKey)
	case WebSearchParallel:
		return strings.TrimSpace(p.Parallel.APIKey)
	case WebSearchFirecrawl:
		return strings.TrimSpace(p.Firecrawl.APIKey)
	case WebSearchTinyFish:
		return strings.TrimSpace(p.TinyFish.APIKey)
	}
	return ""
}

// CanSearch reports whether a provider can answer: it has an API key, or it
// supports keyless use and keyless use is on.
func (s WebSearchConfig) CanSearch(name string) bool {
	if !IsWebSearchProvider(name) {
		return false
	}
	return s.Providers.APIKey(name) != "" || (s.Keyless && WebSearchSupportsKeyless(name))
}

// Usable lists the providers that can search, in display order.
func (s WebSearchConfig) Usable() []string {
	out := []string{}
	for _, name := range WebSearchProviderNames {
		if s.CanSearch(name) {
			out = append(out, name)
		}
	}
	return out
}

// IsAuto reports whether Tollgate picks the provider: the default is "auto",
// or the chosen provider can't search (no key and no keyless mode).
func (s WebSearchConfig) IsAuto() bool {
	return s.Default == WebSearchAuto || !s.CanSearch(s.Default)
}

// NormalizeWebToolsConfig cleans up values a user may have typed by hand:
// unknown provider or mode names fall back to the defaults, and domain
// entries are trimmed, lowercased and stripped of wildcards, schemes and paths.
func (cfg *Config) NormalizeWebToolsConfig() {
	if cfg == nil {
		return
	}
	w := &cfg.WebTools
	w.Search.Default = strings.ToLower(strings.TrimSpace(w.Search.Default))
	if w.Search.Default != WebSearchAuto && !IsWebSearchProvider(w.Search.Default) {
		w.Search.Default = WebSearchAuto
	}
	w.Domains.Mode = strings.ToLower(strings.TrimSpace(w.Domains.Mode))
	if w.Domains.Mode != WebDomainsAllowlist {
		w.Domains.Mode = WebDomainsDenylist
	}
	list := make([]string, 0, len(w.Domains.List))
	seen := map[string]bool{}
	for _, entry := range w.Domains.List {
		host := NormalizeWebDomain(entry)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		list = append(list, host)
	}
	w.Domains.List = list
}

// NormalizeWebDomain turns a domain list entry into a bare lowercase hostname:
// "https://*.Example.com/path" becomes "example.com".
func NormalizeWebDomain(entry string) string {
	host := strings.ToLower(strings.TrimSpace(entry))
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], "]") && strings.Count(host, ":") == 1 {
		host = host[:i]
	}
	host = strings.TrimPrefix(host, "*.")
	host = strings.Trim(host, ".")
	return host
}
