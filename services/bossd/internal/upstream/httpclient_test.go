package upstream

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// TestBuildUpstreamHTTPClient_HTTPSPreservesProxy verifies the production
// (TLS) path preserves net/http's environment proxy behavior. Corporate and
// controlled production environments may depend on HTTPS_PROXY/NO_PROXY; using
// a raw *http2.Transport bypasses that proxy support.
func TestBuildUpstreamHTTPClient_HTTPSPreservesProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")

	c := BuildUpstreamHTTPClient("https://orchestrator.bossanova.dev")
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("https:// path: expected *http.Transport, got %T", c.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("https:// path: expected ProxyFromEnvironment-compatible proxy function")
	}
	req, err := http.NewRequest(http.MethodPost, "https://orchestrator.bossanova.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy.example:3128" {
		t.Fatalf("proxy URL = %v, want http://proxy.example:3128", proxyURL)
	}
	if c.Timeout != 0 {
		t.Errorf("Timeout = %s, want 0 (long-lived bidi streams must not have a client wall-clock timeout)", c.Timeout)
	}
}

// TestBuildUpstreamHTTPClient_HTTPSKeepalive verifies the HTTP/2 transport
// nested under the production *http.Transport carries the keepalive knobs.
func TestBuildUpstreamHTTPClient_HTTPSKeepalive(t *testing.T) {
	_, h2tr := buildHTTPSUpstreamTransport()

	if h2tr.ReadIdleTimeout != HTTP2ReadIdleTimeout {
		t.Errorf("ReadIdleTimeout = %s, want %s", h2tr.ReadIdleTimeout, HTTP2ReadIdleTimeout)
	}
	if h2tr.PingTimeout != HTTP2PingTimeout {
		t.Errorf("PingTimeout = %s, want %s", h2tr.PingTimeout, HTTP2PingTimeout)
	}
}

// TestWorkOSRefreshHTTPClientHasHalfOpenDetection guards the client used for
// the refresh exchange — the one whose failure signs the machine out once its
// replay budget is spent (BOS-941). It previously used
// http.DefaultTransport, which has no HTTP/2 keepalive: a pooled connection to
// api.workos.com that had gone half-open (laptop sleep, Wi-Fi or VPN path
// change) was handed straight back out, the request was written into a dead
// connection, and the resulting timeout was classified as a dispatched
// exchange with an unknown outcome — the state that forces a manual re-login.
func TestWorkOSRefreshHTTPClientHasHalfOpenDetection(t *testing.T) {
	c := newWorkOSRefreshHTTPClient()

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("refresh client transport = %T, want *http.Transport", c.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("refresh client uses http.DefaultTransport, which has no HTTP/2 keepalive")
	}
	// ConfigureTransports registers the h2 upgrade under "h2"; its absence
	// means the keepalive knobs were never wired in.
	if _, registered := tr.TLSNextProto["h2"]; !registered {
		t.Fatal("refresh client transport has no HTTP/2 upgrade registered")
	}
	if c.Timeout != workOSRefreshTimeout {
		t.Errorf("Timeout = %s, want %s", c.Timeout, workOSRefreshTimeout)
	}
	// This assertion used to be a FLOOR (">10s"), on the theory that an
	// overrun was unrecoverable so the budget had to cover a cold DNS lookup,
	// TCP connect, TLS handshake and round trip on a just-woken machine.
	// BOS-941 inverted it: WorkOS's replay grace window makes an overrun
	// recoverable, and makes a long timeout the thing that breaks recovery,
	// because a replay issued after the window closed lands on an answer
	// instead of on the documented idempotent repeat. The full budget
	// arithmetic is pinned by TestWorkOSReplayBudgetFitsGraceWindow; the
	// property that belongs here is the single exchange's own: it must not be
	// able to spend the whole window by itself.
	if workOSRefreshTimeout >= workOSReplayGraceWindow {
		t.Errorf("workOSRefreshTimeout = %s, want less than the %s WorkOS replay grace window",
			workOSRefreshTimeout, workOSReplayGraceWindow)
	}
	// Inverting the old assertion removed the only FLOOR in the package, and
	// the ceiling above cannot supply one: workOSRefreshTimeout = 100ms would
	// satisfy every other test here while timing out on healthy exchanges and
	// spending the whole replay budget before a cold DNS lookup finished. The
	// cold-wake reasoning that justified raising the budget from 10s did not
	// become wrong when BOS-941 capped it — it stopped being the ONLY
	// consideration. So keep a floor, well under the 8s the budget is sized
	// for, that still fails loudly if someone shrinks the exchange to
	// something no real network round trip can complete in.
	if workOSRefreshTimeout < 5*time.Second {
		t.Errorf("workOSRefreshTimeout = %s, want at least 5s so a cold DNS lookup, TCP connect, TLS handshake and round trip can finish",
			workOSRefreshTimeout)
	}
}

// TestBuildUpstreamHTTPClient_H2CKeepalive verifies the cleartext (h2c / local
// dev) path keeps AllowHTTP + a plain dialer AND carries the same keepalive
// knobs, so a dev daemon behaves like production.
func TestBuildUpstreamHTTPClient_H2CKeepalive(t *testing.T) {
	c := BuildUpstreamHTTPClient("http://localhost:8080")
	tr, ok := c.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("http:// path: expected *http2.Transport, got %T", c.Transport)
	}
	if !tr.AllowHTTP {
		t.Error("http:// path: expected AllowHTTP=true for h2c")
	}
	if tr.DialTLSContext == nil {
		t.Error("http:// path: expected a plain DialTLSContext for h2c")
	}
	if tr.ReadIdleTimeout != HTTP2ReadIdleTimeout {
		t.Errorf("ReadIdleTimeout = %s, want %s", tr.ReadIdleTimeout, HTTP2ReadIdleTimeout)
	}
	if tr.PingTimeout != HTTP2PingTimeout {
		t.Errorf("PingTimeout = %s, want %s", tr.PingTimeout, HTTP2PingTimeout)
	}
}

// TestH2CDialerIsBounded verifies the cleartext reconnect dialer has a bounded
// connect timeout, so a routed-but-unreachable orchestrator fails fast instead
// of wedging stream recovery on the OS default TCP connect timeout. The TLS
// path inherits net/http's DefaultTransport dialer; the h2c path builds its own
// and must not leave it unbounded.
func TestH2CDialerIsBounded(t *testing.T) {
	d := h2cDialer()
	if d.Timeout != h2cDialTimeout {
		t.Errorf("h2c dialer Timeout = %s, want %s", d.Timeout, h2cDialTimeout)
	}
	if d.Timeout == 0 {
		t.Error("h2c dialer Timeout must be non-zero (unbounded connect wedges reconnect)")
	}
	if d.KeepAlive != h2cDialKeepAlive {
		t.Errorf("h2c dialer KeepAlive = %s, want %s", d.KeepAlive, h2cDialKeepAlive)
	}
}
