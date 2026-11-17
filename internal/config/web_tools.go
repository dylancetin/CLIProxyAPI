package config

import "strings"

// Web search provider names, in the order clients list them.
const (
	WebSearchBrave  = "brave"
	WebSearchTavily = "tavily"
	WebSearchExa    = "exa"
)

// WebSearchProviderNames lists the supported web search providers in display order.
var WebSearchProviderNames = []string{WebSearchBrave, WebSearchTavily, WebSearchExa}

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
	// Default names the provider used when a search names none: brave, tavily or exa.
	Default string `yaml:"default" json:"default"`
	// Providers holds one API key per provider.
	Providers WebSearchProviders `yaml:"providers" json:"providers"`
}

// WebSearchProviders holds the per-provider settings.
type WebSearchProviders struct {
	Brave  WebSearchProviderConfig `yaml:"brave" json:"brave"`
	Tavily WebSearchProviderConfig `yaml:"tavily" json:"tavily"`
	Exa    WebSearchProviderConfig `yaml:"exa" json:"exa"`
}

// WebSearchProviderConfig holds one provider's API key.
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
		Search:  WebSearchConfig{Default: WebSearchBrave},
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
	}
	return ""
}

// Configured lists the providers that have an API key, in display order.
func (s WebSearchConfig) Configured() []string {
	out := []string{}
	for _, name := range WebSearchProviderNames {
		if s.Providers.APIKey(name) != "" {
			out = append(out, name)
		}
	}
	return out
}

// EffectiveDefault is the chosen default when it has a key, else the first
// configured provider, else "".
func (s WebSearchConfig) EffectiveDefault() string {
	configured := s.Configured()
	for _, name := range configured {
		if name == s.Default {
			return name
		}
	}
	if len(configured) > 0 {
		return configured[0]
	}
	return ""
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
	switch w.Search.Default {
	case WebSearchBrave, WebSearchTavily, WebSearchExa:
	default:
		w.Search.Default = WebSearchBrave
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
