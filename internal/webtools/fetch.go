package webtools

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const (
	defaultTimeout      = 10 * time.Second
	defaultMaxBodyBytes = 5 << 20
	defaultMaxRedirects = 3
	acceptHeader        = "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1"
	defaultAcceptLang   = "tr,en;q=0.8"
)

// Resolver looks up the addresses of a host name.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DialFunc opens a connection to an IP:port address that already passed the
// address check.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Fetcher downloads single web pages with the SSRF safeguards: https on port
// 443 only, at most three re-checked redirects, a direct connection (no
// proxy) to an address checked in the dialer, no cookies and size, type and
// time limits.
type Fetcher struct {
	// Resolver resolves host names; net.DefaultResolver when nil.
	Resolver Resolver
	// Dial connects to a checked IP:port; a net.Dialer that re-checks the
	// connected address when nil. Tests inject one to reach httptest servers.
	Dial DialFunc
	// TLSConfig overrides the client TLS config (tests trust their own CA).
	TLSConfig *tls.Config
	// Timeout bounds the whole call (connect, redirects, body).
	Timeout time.Duration
	// MaxBodyBytes caps the decompressed body.
	MaxBodyBytes int64
	// UserAgent is sent with every request.
	UserAgent string

	once   sync.Once
	client *http.Client
}

// fetched is a downloaded page before extraction.
type fetched struct {
	FinalURL          *url.URL
	ContentType       string // media type, lowercase, without parameters
	ContentTypeHeader string
	Body              []byte
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaultTimeout
}

func (f *Fetcher) maxBody() int64 {
	if f.MaxBodyBytes > 0 {
		return f.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

// controlConn re-checks the address the socket is about to connect to, so no
// path through the dialer can reach a blocked address.
func controlConn(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || blockedAddr(addr) {
		return errPrivate(host)
	}
	return nil
}

// dialChecked resolves host, refuses blocked addresses and connects to the
// first allowed address that answers. Connecting to the checked IP literal
// (never re-resolving the name) is what defeats DNS rebinding.
func (f *Fetcher) dialChecked(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if port != "443" {
		return nil, errInsecure("only port 443 is allowed")
	}
	var addrs []netip.Addr
	if ip, errParse := netip.ParseAddr(host); errParse == nil {
		addrs = []netip.Addr{ip}
	} else {
		if localName(host) {
			return nil, errPrivate(host)
		}
		resolver := f.Resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		ips, errLookup := resolver.LookupIPAddr(ctx, host)
		if errLookup != nil {
			return nil, errLookup
		}
		for _, ip := range ips {
			if addr, ok := netip.AddrFromSlice(ip.IP); ok {
				addrs = append(addrs, addr.Unmap())
			}
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}
	for _, addr := range addrs {
		if blockedAddr(addr) {
			// One private answer is enough to refuse: a name that mixes public
			// and private addresses is a rebinding setup.
			return nil, errPrivate(host)
		}
	}
	dial := f.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: f.timeout(), Control: controlConn}
		dial = d.DialContext
	}
	var lastErr error
	for _, addr := range addrs {
		conn, errDial := dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if errDial == nil {
			return conn, nil
		}
		lastErr = errDial
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func (f *Fetcher) httpClient() *http.Client {
	f.once.Do(f.initClient)
	return f.client
}

func (f *Fetcher) initClient() {
	tlsConfig := f.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport := &http.Transport{
		Proxy:                  nil, // never a proxy: the address check must see the real peer
		DialContext:            f.dialChecked,
		TLSClientConfig:        tlsConfig,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    f.timeout(),
		ResponseHeaderTimeout:  f.timeout(),
		MaxResponseHeaderBytes: 256 << 10,
	}
	f.client = &http.Client{
		Transport: transport,
		// Redirects are followed by hand so every hop is re-checked.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// allowedType reports the media types web_fetch reads.
func allowedType(mediaType string) bool {
	switch mediaType {
	case "text/html", "application/xhtml+xml", "text/plain", "text/markdown", "text/x-markdown":
		return true
	}
	return false
}

// Fetch downloads target, following up to three redirects. Every hop is
// re-checked against the URL rules and the domain list.
func (f *Fetcher) Fetch(ctx context.Context, target *url.URL, domains config.WebDomainsConfig, acceptLanguage string) (*fetched, *toolError) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	if acceptLanguage == "" {
		acceptLanguage = defaultAcceptLang
	}
	current := target
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, errInvalid("url cannot be requested")
		}
		req.Header.Set("Accept", acceptHeader)
		req.Header.Set("Accept-Language", acceptLanguage)
		req.Header.Set("User-Agent", f.UserAgent)
		resp, err := f.httpClient().Do(req)
		if err != nil {
			return nil, classifyFetchError(ctx, err, current.Hostname())
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
			location := resp.Header.Get("Location")
			drain(resp)
			if location == "" {
				return nil, &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeFetchFailed, Message: "redirect without a Location", UpstreamStatus: resp.StatusCode}
			}
			if hop >= defaultMaxRedirects {
				return nil, newError(http.StatusUnprocessableEntity, CodeTooManyRedirects, "more than %d redirects", defaultMaxRedirects)
			}
			next, errLoc := current.Parse(location)
			if errLoc != nil {
				return nil, &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeFetchFailed, Message: "invalid redirect location", UpstreamStatus: resp.StatusCode}
			}
			checked, terr := checkFetchURL(next, domains)
			if terr != nil {
				return nil, terr
			}
			current = checked
			continue
		}
		defer drain(resp)
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeFetchFailed,
				Message: fmt.Sprintf("the site answered %d %s", resp.StatusCode, statusText(resp.StatusCode)), UpstreamStatus: resp.StatusCode}
		}
		limit := f.maxBody()
		if resp.ContentLength > limit {
			return nil, newError(http.StatusUnprocessableEntity, CodeTooLarge, "the page is larger than %d MB", limit>>20)
		}
		header := resp.Header.Get("Content-Type")
		mediaType := ""
		if header != "" {
			if mt, _, errMime := mime.ParseMediaType(header); errMime == nil {
				mediaType = strings.ToLower(mt)
			}
		}
		if mediaType != "" && !allowedType(mediaType) {
			return nil, newError(http.StatusUnprocessableEntity, CodeUnsupportedType, "content type %s is not supported", mediaType)
		}
		// Go's transport decompresses gzip itself, so this limit counts the
		// decompressed size.
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, classifyFetchError(ctx, err, current.Hostname())
		}
		if int64(len(body)) > limit {
			return nil, newError(http.StatusUnprocessableEntity, CodeTooLarge, "the page is larger than %d MB", limit>>20)
		}
		if mediaType == "" {
			sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(body))
			mediaType = strings.ToLower(sniffed)
			if !allowedType(mediaType) {
				return nil, newError(http.StatusUnprocessableEntity, CodeUnsupportedType, "content type %s is not supported", mediaType)
			}
		}
		return &fetched{FinalURL: current, ContentType: mediaType, ContentTypeHeader: header, Body: body}, nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// classifyFetchError maps transport errors to tool errors.
func classifyFetchError(ctx context.Context, err error, host string) *toolError {
	var terr *toolError
	if errors.As(err, &terr) {
		return terr
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return errTimeout("the site")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errTimeout("the site")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeFetchFailed, Message: host + " could not be resolved"}
	}
	return &toolError{HTTPStatus: http.StatusBadGateway, Code: CodeFetchFailed, Message: "could not fetch the page from " + host}
}
