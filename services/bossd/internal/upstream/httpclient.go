package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// HTTP/2 client keepalive values. These mirror the server-side keepalive in
// bosso (services/bosso/internal/server/keepalive.go): after 20s of inbound
// idleness the client sends a PING and waits up to 10s for the PONG, so a
// dead peer is detected in ~30s.
//
// Why the daemon needs its OWN keepalive: the DaemonStream and TerminalStream
// are long-lived bidi streams whose reader blocks in stream.Receive(). If the
// underlying TCP connection goes half-open — no FIN/RST, which is exactly what
// happens on laptop sleep/wake or a Wi-Fi/network-path change — Receive()
// blocks forever with no client-level read deadline. The stream's reconnect
// loop never runs, the daemon never re-registers, and it silently disappears
// from the web ("daemon not available" / "No repositories available") even
// though the process is otherwise healthy. bosso's server keepalive detects
// the dead stream and drops the daemon, but only client keepalive gets the
// daemon to notice and reconnect.
const (
	HTTP2ReadIdleTimeout = 20 * time.Second
	HTTP2PingTimeout     = 10 * time.Second
)

// h2c dial bounds for the cleartext (local-dev) path. The TLS path inherits
// these from net/http's DefaultTransport dialer (via http.DefaultTransport
// .Clone()); the h2c path builds its own dialer and would otherwise have NO
// connect timeout, so a routed-but-unreachable orchestrator could wedge a
// reconnect dial on the OS default TCP connect timeout (minutes) instead of
// failing fast and letting the stream's reconnect loop retry. Values mirror
// net/http's DefaultTransport dialer.
const (
	h2cDialTimeout   = 30 * time.Second
	h2cDialKeepAlive = 30 * time.Second
)

// h2cDialer builds the bounded dialer used by the cleartext h2c transport.
func h2cDialer() *net.Dialer {
	return &net.Dialer{Timeout: h2cDialTimeout, KeepAlive: h2cDialKeepAlive}
}

// BuildUpstreamHTTPClient constructs the *http.Client the daemon uses to talk
// to the orchestrator. It always speaks HTTP/2 (ConnectRPC bidi streams
// require it) and always enables HTTP/2 keepalive PINGs so half-open streams
// are detected and reconnected.
//
// No client-level Timeout is set: DaemonStream and TerminalStream are
// long-lived, and http.Client.Timeout is a hard wall on the whole request
// (including reading the response body), which would tear the stream every
// Timeout seconds. Unary callers (RegisterDaemon) bound themselves with
// context.WithTimeout instead. Half-open detection is handled by the HTTP/2
// keepalive above, not by a blunt client timeout.
func BuildUpstreamHTTPClient(orchestratorURL string) *http.Client {
	if strings.HasPrefix(orchestratorURL, "http://") {
		tr := &http2.Transport{
			ReadIdleTimeout: HTTP2ReadIdleTimeout,
			PingTimeout:     HTTP2PingTimeout,
		}
		// Cleartext h2c (local dev against http://localhost:8080). Over
		// plain HTTP, Go's default transport stays on HTTP/1.1 and bosso's
		// h2c handler rejects bidi with HTTP 505, so force HTTP/2 with a
		// plain (non-TLS) dialer. The dialer is bounded (h2cDialer) so a
		// reconnect to an unreachable peer fails fast instead of hanging.
		tr.AllowHTTP = true
		tr.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return h2cDialer().DialContext(ctx, network, addr)
		}
		return &http.Client{Transport: tr}
	}

	tr, _ := buildHTTPSUpstreamTransport()
	return &http.Client{Transport: tr}
}

func buildHTTPSUpstreamTransport() (*http.Transport, *http2.Transport) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	h2tr, err := http2.ConfigureTransports(tr)
	if err != nil {
		// Clone of http.DefaultTransport has no custom TLSNextProto state, so
		// this is an internal invariant failure rather than user input.
		panic(err)
	}
	h2tr.ReadIdleTimeout = HTTP2ReadIdleTimeout
	h2tr.PingTimeout = HTTP2PingTimeout
	return tr, h2tr
}
