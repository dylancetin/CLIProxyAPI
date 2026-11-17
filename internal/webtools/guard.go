package webtools

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const maxURLLength = 2048

// blockedPrefixes are address ranges a web fetch may never connect to.
// IPv6 is additionally limited to global unicast (2000::/3), with NAT64 and
// IPv4-mapped forms checked against the IPv4 list.
var blockedPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this" network, unspecified
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // CGNAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, includes broadcast
	// IPv6 ranges inside 2000::/3 that are not ordinary global unicast
	"2001::/23",     // IETF protocol assignments (Teredo, benchmarking, ORCHID, ...)
	"2001:db8::/32", // documentation
	"2002::/16",     // 6to4
	"3fff::/20",     // documentation
)

var (
	globalUnicast6 = netip.MustParsePrefix("2000::/3")
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
)

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// blockedAddr reports whether a web fetch must not connect to ip.
func blockedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		if nat64.Contains(ip) {
			raw := ip.As16()
			return blockedAddr(netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}))
		}
		if !globalUnicast6.Contains(ip) {
			return true
		}
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// localName reports names that only make sense on a local network and are
// refused before any DNS lookup.
func localName(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// normalizeHost lowercases a URL host and drops a trailing dot and IPv6 brackets.
func normalizeHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// checkHostName refuses local names, private IP literals and hosts the domain
// list rejects. It does not resolve DNS; the dialer checks resolved addresses.
func checkHostName(host string, domains config.WebDomainsConfig) *toolError {
	if host == "" {
		return errInvalid("the URL has no host")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if blockedAddr(addr) {
			return errPrivate(host)
		}
	} else if localName(host) {
		return errPrivate(host)
	}
	if !domainAllowed(host, domains) {
		return errBlocked(host)
	}
	return nil
}

// domainMatches reports whether host is entry or one of its subdomains.
func domainMatches(host, entry string) bool {
	return host == entry || strings.HasSuffix(host, "."+entry)
}

// domainAllowed applies the deny/allow list to a host.
func domainAllowed(host string, domains config.WebDomainsConfig) bool {
	matched := false
	for _, entry := range domains.List {
		if entry != "" && domainMatches(host, entry) {
			matched = true
			break
		}
	}
	if domains.Mode == config.WebDomainsAllowlist {
		return matched
	}
	return !matched
}

// parseFetchURL validates a web_fetch target or redirect location: absolute
// https, port 443, no userinfo, a public host name allowed by the domain list.
func parseFetchURL(raw string, domains config.WebDomainsConfig) (*url.URL, *toolError) {
	if utf8.RuneCountInString(raw) > maxURLLength {
		return nil, errInvalid("url is longer than %d characters", maxURLLength)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" && u.Opaque == "" {
		return nil, errInvalid("url must be an absolute https:// URL")
	}
	return checkFetchURL(u, domains)
}

func checkFetchURL(u *url.URL, domains config.WebDomainsConfig) (*url.URL, *toolError) {
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, errInsecure("only https URLs can be fetched")
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, errInvalid("url must be an absolute https:// URL")
	}
	if u.User != nil {
		return nil, errInsecure("URLs with a user name or password are refused")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errInsecure("only port 443 is allowed")
	}
	host := normalizeHost(u)
	if terr := checkHostName(host, domains); terr != nil {
		return nil, terr
	}
	out := *u
	out.Scheme = "https"
	out.Fragment = ""
	out.RawFragment = ""
	// Drop an explicit :443 so cache keys and reported URLs are canonical.
	if u.Port() == "443" {
		out.Host = strings.TrimSuffix(u.Host, ":443")
	}
	return &out, nil
}

// resultURL validates a search result URL: https, public-looking host, allowed
// by the domain list. Results that fail are dropped silently.
func resultURL(raw string, domains config.WebDomainsConfig) (*url.URL, bool) {
	if raw == "" || utf8.RuneCountInString(raw) > maxURLLength {
		return nil, false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() {
		return nil, false
	}
	checked, terr := checkFetchURL(u, domains)
	if terr != nil {
		return nil, false
	}
	return checked, true
}

// statusText names an HTTP status for messages.
func statusText(code int) string {
	if text := http.StatusText(code); text != "" {
		return text
	}
	return "unexpected status"
}
