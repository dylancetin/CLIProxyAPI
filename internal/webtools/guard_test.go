package webtools

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.9.10", "::1", // loopback
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", // RFC 1918
		"169.254.169.254", "fe80::1", "fe80::1%eth0", // link-local
		"100.64.0.1", "100.127.255.254", // CGNAT
		"0.0.0.0", "0.1.2.3", "::", // unspecified / this network
		"192.0.0.8", "198.18.0.1", "198.19.255.255", // IETF, benchmarking
		"192.0.2.1", "198.51.100.7", "203.0.113.9", "2001:db8::1", "3fff::1", // documentation
		"224.0.0.1", "239.255.255.250", "ff02::1", // multicast
		"240.0.0.1", "255.255.255.255", // reserved, broadcast
		"fc00::1", "fd12:3456::1", // ULA
		"fec0::1",                                                       // site-local (outside 2000::/3)
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::7f00:1", "64:ff9b::a00:1", // NAT64 of loopback / RFC 1918
		"2002:7f00:1::1", "2001::1", // 6to4, Teredo
	}
	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"93.184.216.34", "1.1.1.1", "8.8.8.8", "2606:4700::1111", "2a00:1450:4001::200e", "::ffff:93.184.216.34", "64:ff9b::5db8:d822"}
	for _, s := range allowed {
		if blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestLocalNames(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST.", "app.localhost", "nas.local", "printer.lan", "db.internal", "router.home.arpa", "intranet", ""} {
		if !localName(host) {
			t.Errorf("%q should be a local name", host)
		}
	}
	for _, host := range []string{"example.com", "tr.wikipedia.org", "local.example.com", "internal.example.org"} {
		if localName(host) {
			t.Errorf("%q should not be a local name", host)
		}
	}
}

func TestDomainList(t *testing.T) {
	deny := config.WebDomainsConfig{Mode: "denylist", List: []string{"example.com"}}
	allow := config.WebDomainsConfig{Mode: "allowlist", List: []string{"wikipedia.org"}}
	cases := []struct {
		host    string
		domains config.WebDomainsConfig
		want    bool
	}{
		{"example.com", deny, false},
		{"a.example.com", deny, false},
		{"a.b.example.com", deny, false},
		{"badexample.com", deny, true},
		{"example.com.evil.org", deny, true},
		{"other.org", deny, true},
		{"wikipedia.org", allow, true},
		{"tr.wikipedia.org", allow, true},
		{"notwikipedia.org", allow, false},
		{"example.com", allow, false},
		{"anything.org", config.WebDomainsConfig{Mode: "denylist"}, true},
		{"anything.org", config.WebDomainsConfig{Mode: "allowlist"}, false},
	}
	for _, tc := range cases {
		if got := domainAllowed(tc.host, tc.domains); got != tc.want {
			t.Errorf("domainAllowed(%q, %v) = %v, want %v", tc.host, tc.domains, got, tc.want)
		}
	}
}

func TestParseFetchURL(t *testing.T) {
	none := config.WebDomainsConfig{Mode: "denylist"}
	cases := map[string]string{
		"http://example.com/":               CodeInsecureURL,
		"ftp://example.com/":                CodeInsecureURL,
		"javascript:alert(1)":               CodeInsecureURL,
		"https://example.com:8443/":         CodeInsecureURL,
		"https://user:pw@example.com/":      CodeInsecureURL,
		"https://user@example.com/":         CodeInsecureURL,
		"https://127.0.0.1/":                CodePrivateAddress,
		"https://[::1]/":                    CodePrivateAddress,
		"https://[::ffff:192.168.0.1]/":     CodePrivateAddress,
		"https://localhost/":                CodePrivateAddress,
		"https://router.local/":             CodePrivateAddress,
		"https://intranet/":                 CodePrivateAddress,
		"/relative/path":                    CodeInvalidRequest,
		"https://":                          CodeInvalidRequest,
		"not a url":                         CodeInvalidRequest,
		"https://example.com/" + long(2100): CodeInvalidRequest,
	}
	for raw, code := range cases {
		_, terr := parseFetchURL(raw, none)
		if terr == nil || terr.Code != code {
			t.Errorf("%q: got %v, want %s", raw, terr, code)
		}
	}
	u, terr := parseFetchURL("HTTPS://Example.com:443/a?b=c#frag", none)
	if terr != nil || u.String() != "https://Example.com/a?b=c" {
		t.Fatalf("canonical url = %v %v", u, terr)
	}
	if _, terr := parseFetchURL("https://a.example.com/", config.WebDomainsConfig{Mode: "denylist", List: []string{"example.com"}}); terr == nil || terr.Code != CodeBlockedDomain {
		t.Fatalf("blocked domain = %v", terr)
	}
}

func long(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

func TestControlConnChecksConnectedAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "10.0.0.5:443", "[::ffff:127.0.0.1]:443"} {
		err := controlConn("tcp", addr, nil)
		var terr *toolError
		if !errors.As(err, &terr) || terr.Code != CodePrivateAddress {
			t.Errorf("%s: err = %v", addr, err)
		}
	}
	if err := controlConn("tcp", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
}

func TestNormalizeWebDomain(t *testing.T) {
	cases := map[string]string{
		"Example.com":                "example.com",
		"  *.wiki.org ":              "wiki.org",
		"https://news.example.com/x": "news.example.com",
		"example.com:443":            "example.com",
		".example.com.":              "example.com",
	}
	for in, want := range cases {
		if got := config.NormalizeWebDomain(in); got != want {
			t.Errorf("NormalizeWebDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
