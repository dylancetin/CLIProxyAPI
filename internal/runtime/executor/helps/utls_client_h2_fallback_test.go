package helps

import (
	"bytes"
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

const utlsFallbackTestAddr = "example.com:443"

// echoProtoHandler replies with the request protocol and the echoed body.
func echoProtoHandler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		w.Header().Set("X-Proto", r.Proto)
		_, _ = w.Write(body)
	})
}

// newMiddleboxH2Server negotiates h2 like a TLS-intercepting proxy, then sends
// WINDOW_UPDATE before SETTINGS. HTTP/1.1 is served normally.
func newMiddleboxH2Server(t *testing.T, h2Attempts *atomic.Int32) *httptest.Server {
	server := httptest.NewUnstartedServer(echoProtoHandler(t))
	server.TLS = &stdtls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.Config.TLSNextProto = map[string]func(*http.Server, *stdtls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *stdtls.Conn, _ http.Handler) {
			h2Attempts.Add(1)
			preface := make([]byte, len(http2.ClientPreface))
			if _, errRead := io.ReadFull(conn, preface); errRead != nil {
				t.Errorf("read client preface: %v", errRead)
				return
			}
			if errWrite := http2.NewFramer(conn, conn).WriteWindowUpdate(0, 1<<20); errWrite != nil {
				t.Errorf("write WINDOW_UPDATE: %v", errWrite)
				return
			}
			// Drain until the client gives up so its error comes from the
			// frame order, not from a reset connection.
			_, _ = io.Copy(io.Discard, conn)
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func newTestUtlsRoundTripper(t *testing.T, server *httptest.Server) *utlsRoundTripper {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	serverAddr := server.Listener.Addr().String()
	return &utlsRoundTripper{
		roots: roots,
		dialer: contextDialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, serverAddr)
		}),
	}
}

func resetUtlsHTTP1OnlyHost(t *testing.T) {
	utlsHTTP1OnlyHosts.Delete(utlsFallbackTestAddr)
	t.Cleanup(func() { utlsHTTP1OnlyHosts.Delete(utlsFallbackTestAddr) })
}

func postThroughUtls(t *testing.T, roundTripper http.RoundTripper, body string) (string, string) {
	t.Helper()
	req, errRequest := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/backend-api/codex/responses", bytes.NewReader([]byte(body)))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	resp, errRoundTrip := roundTripper.RoundTrip(req)
	if errRoundTrip != nil {
		t.Fatalf("RoundTrip error = %v", errRoundTrip)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			t.Errorf("close response body: %v", errClose)
		}
	}()
	payload, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return resp.Header.Get("X-Proto"), string(payload)
}

func TestUtlsRoundTripperFallsBackToHTTP1WhenMiddleboxBreaksH2(t *testing.T) {
	resetUtlsHTTP1OnlyHost(t)
	var h2Attempts atomic.Int32
	server := newMiddleboxH2Server(t, &h2Attempts)
	roundTripper := newTestUtlsRoundTripper(t, server)

	body := `{"model":"gpt-5","input":"hello"}`
	proto, payload := postThroughUtls(t, roundTripper, body)
	if proto != "HTTP/1.1" {
		t.Fatalf("protocol = %q, want HTTP/1.1", proto)
	}
	if payload != body {
		t.Fatalf("echoed body = %q, want %q", payload, body)
	}
	if got := h2Attempts.Load(); got != 1 {
		t.Fatalf("h2 attempts = %d, want 1", got)
	}
	if _, ok := utlsHTTP1OnlyHosts.Load(utlsFallbackTestAddr); !ok {
		t.Fatal("HTTP/1.1 fallback was not remembered")
	}

	// A new round tripper, as built per client, skips the broken h2 attempt.
	proto, payload = postThroughUtls(t, newTestUtlsRoundTripper(t, server), "second")
	if proto != "HTTP/1.1" || payload != "second" {
		t.Fatalf("second request = (%q, %q), want (HTTP/1.1, second)", proto, payload)
	}
	if got := h2Attempts.Load(); got != 1 {
		t.Fatalf("h2 attempts after second request = %d, want 1", got)
	}
}

func TestUtlsRoundTripperDoesNotRetryUnreplayableBody(t *testing.T) {
	resetUtlsHTTP1OnlyHost(t)
	var h2Attempts atomic.Int32
	server := newMiddleboxH2Server(t, &h2Attempts)
	roundTripper := newTestUtlsRoundTripper(t, server)

	req, errRequest := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com/backend-api/codex/responses", io.NopCloser(strings.NewReader("once")))
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	if req.GetBody != nil {
		t.Fatal("test request unexpectedly has GetBody")
	}
	resp, errRoundTrip := roundTripper.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatal("RoundTrip returned a response, want error")
	}
	if !isHTTP2ProtocolError(errRoundTrip) {
		t.Fatalf("RoundTrip error = %v (%T), want HTTP/2 PROTOCOL_ERROR", errRoundTrip, errRoundTrip)
	}
	if got := h2Attempts.Load(); got != 1 {
		t.Fatalf("h2 attempts = %d, want 1", got)
	}
}

func TestUtlsRoundTripperKeepsHTTP2ForHealthyServer(t *testing.T) {
	resetUtlsHTTP1OnlyHost(t)
	server := httptest.NewUnstartedServer(echoProtoHandler(t))
	server.EnableHTTP2 = true
	server.TLS = &stdtls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	t.Cleanup(server.Close)

	proto, payload := postThroughUtls(t, newTestUtlsRoundTripper(t, server), "healthy")
	if proto != "HTTP/2.0" {
		t.Fatalf("protocol = %q, want HTTP/2.0", proto)
	}
	if payload != "healthy" {
		t.Fatalf("echoed body = %q, want healthy", payload)
	}
	if _, ok := utlsHTTP1OnlyHosts.Load(utlsFallbackTestAddr); ok {
		t.Fatal("healthy h2 server was marked HTTP/1.1-only")
	}
}

func TestChromeHTTP1OnlyClientHelloSpecKeepsFingerprint(t *testing.T) {
	chrome, errSpec := tls.UTLSIdToSpec(tls.HelloChrome_Auto)
	if errSpec != nil {
		t.Fatal(errSpec)
	}
	http1, errHTTP1 := chromeHTTP1OnlyClientHelloSpec()
	if errHTTP1 != nil {
		t.Fatal(errHTTP1)
	}

	var chromeHasALPS bool
	for _, extension := range chrome.Extensions {
		switch extension.(type) {
		case *tls.ApplicationSettingsExtension, *tls.ApplicationSettingsExtensionNew:
			chromeHasALPS = true
		}
	}
	wantExtensions := len(chrome.Extensions)
	if chromeHasALPS {
		wantExtensions--
	}
	if got := len(http1.Extensions); got != wantExtensions {
		t.Fatalf("extension count = %d, want %d", got, wantExtensions)
	}

	var alpn []string
	for _, extension := range http1.Extensions {
		switch ext := extension.(type) {
		case *tls.ALPNExtension:
			alpn = ext.AlpnProtocols
		case *tls.ApplicationSettingsExtension, *tls.ApplicationSettingsExtensionNew:
			t.Fatalf("HTTP/1.1 spec still carries ALPS extension %T", ext)
		}
	}
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Fatalf("ALPN = %v, want [http/1.1]", alpn)
	}
	if len(http1.CipherSuites) != len(chrome.CipherSuites) {
		t.Fatalf("cipher suite count = %d, want %d", len(http1.CipherSuites), len(chrome.CipherSuites))
	}
}

func TestIsHTTP2ProtocolError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"x/net protocol error", http2.ConnectionError(http2.ErrCodeProtocol), true},
		{"wrapped protocol error", fmt.Errorf("utls: %w", http2.ConnectionError(http2.ErrCodeProtocol)), true},
		{"joined protocol error", errors.Join(io.EOF, http2.ConnectionError(http2.ErrCodeProtocol)), true},
		{"other connection error", http2.ConnectionError(http2.ErrCodeFlowControl), false},
		{"stream protocol error", http2.StreamError{StreamID: 1, Code: http2.ErrCodeProtocol}, false},
		{"context canceled", context.Canceled, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isHTTP2ProtocolError(tc.err); got != tc.want {
			t.Errorf("%s: isHTTP2ProtocolError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
