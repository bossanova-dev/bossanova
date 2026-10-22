package sessionports

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// probeTimeout bounds a single target probe (both the plaintext and the TLS
	// attempt share this deadline).
	probeTimeout = 250 * time.Millisecond
	// batchProbeTimeout bounds a whole batch of concurrent probes.
	batchProbeTimeout = 1 * time.Second
	// maxProbeConcurrency caps how many targets are probed at once.
	maxProbeConcurrency = 8
	// maxHeaderBytes caps how much of a response we read while looking for the
	// status line, so a chatty or malicious socket cannot make us read forever.
	maxHeaderBytes = 16 << 10
	// bossUserAgent identifies these discovery probes in server logs.
	bossUserAgent = "Boss/1.0 (session-endpoint-probe)"
)

// statusLineRE matches a syntactically valid HTTP/1.x status line: any 3-digit
// status code is positive evidence, including 4xx and 5xx.
var statusLineRE = regexp.MustCompile(`^HTTP/1\.[01] [0-9]{3}(?: .*)?$`)

// prober verifies a single loopback target and reports the confirmed scheme.
type prober interface {
	// Probe returns ("http"|"https", true) when target speaks HTTP over
	// plaintext or TLS respectively, or ("", false) otherwise.
	Probe(ctx context.Context, target string) (scheme string, ok bool)
}

// dialFunc abstracts establishing a raw TCP connection so tests can inject a
// dialer. It matches (&net.Dialer{}).DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// socketProber is the production prober. It talks raw sockets — never
// net/http.Client — so environment proxies, redirects, cookies, and credentials
// can never leak into a discovery probe.
type socketProber struct {
	dial dialFunc
}

// newSocketProber builds a socketProber over the default system dialer.
func newSocketProber() *socketProber {
	return &socketProber{dial: (&net.Dialer{}).DialContext}
}

// Probe classifies target's scheme. It tries plaintext HTTP first, then
// confirms against TLS (verification disabled — safe only because the target is
// a discovered loopback socket).
//
// A successful TLS handshake is authoritative: a plaintext server cannot
// complete one, so if TLS succeeds the socket really is HTTPS even when the
// plaintext attempt also saw a valid status line. That last case is not
// hypothetical — Go's own http.Server answers a cleartext request to a TLS
// listener with a real "HTTP/1.0 400 Bad Request" status line, which would
// otherwise be misread as plaintext HTTP. Preferring the definitive TLS signal
// keeps that (and nginx-style resets) correctly classified as https.
func (p *socketProber) Probe(ctx context.Context, target string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	plain := p.tryPlaintext(ctx, target)
	if p.tryTLS(ctx, target) {
		return "https", true
	}
	if plain {
		return "http", true
	}
	return "", false
}

// tryPlaintext dials target and checks for a valid HTTP status line over cleartext.
func (p *socketProber) tryPlaintext(ctx context.Context, target string) bool {
	conn, err := p.dial(ctx, "tcp", target)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	applyDeadline(ctx, conn)
	return exchangeHEAD(conn, target)
}

// tryTLS dials target, performs a TLS handshake with http/1.1 ALPN and
// verification disabled, then checks for a valid HTTP status line.
func (p *socketProber) tryTLS(ctx context.Context, target string) bool {
	conn, err := p.dial(ctx, "tcp", target)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	applyDeadline(ctx, conn)
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	tconn := tls.Client(conn, &tls.Config{
		// #nosec G402 -- discovered loopback socket has no stable certificate identity; the handshake only confirms HTTPS.
		// owner=@recurser review-by=2027-01-18 issue=BOS-503
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"http/1.1"},
		ServerName:         host,
	})
	if err := tconn.HandshakeContext(ctx); err != nil {
		return false
	}
	return exchangeHEAD(tconn, target)
}

// applyDeadline mirrors ctx's deadline onto the connection so a stalled read or
// write cannot outlive the probe budget.
func applyDeadline(ctx context.Context, conn net.Conn) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
}

// exchangeHEAD writes the fixed HEAD request and reports whether the peer replies
// with a valid HTTP status line.
func exchangeHEAD(conn net.Conn, target string) bool {
	req := "HEAD / HTTP/1.1\r\n" +
		"Host: " + target + "\r\n" +
		"User-Agent: " + bossUserAgent + "\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return false
	}
	return hasValidStatusLine(conn)
}

// hasValidStatusLine reads at most maxHeaderBytes looking for the first line and
// validates it as an HTTP status line. The read is capped, so garbage without a
// newline can never make this block or allocate without bound.
func hasValidStatusLine(r io.Reader) bool {
	br := bufio.NewReader(io.LimitReader(r, maxHeaderBytes))
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	line = strings.TrimRight(line, "\r\n")
	return statusLineRE.MatchString(line)
}

// probeBatch probes every target concurrently (capped at maxProbeConcurrency)
// under an overall batchProbeTimeout, returning the confirmed scheme for each
// positive target and whether the batch completed. Every spawned goroutine is
// joined before returning.
//
// complete is false when the batch deadline (or parent cancellation) fired
// before every target could be attempted, so some absent targets were not
// probed at all rather than probed-and-rejected. Callers must treat an
// incomplete batch as "don't know", never as authoritative absence — otherwise
// a probe overload would wipe still-live endpoints.
func probeBatch(ctx context.Context, p prober, targets []string) (map[string]string, bool) {
	if len(targets) == 0 {
		return nil, true
	}
	bctx, cancel := context.WithTimeout(ctx, batchProbeTimeout)
	defer cancel()

	results := make(map[string]string, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxProbeConcurrency)

	for _, target := range targets {
		select {
		case sem <- struct{}{}:
		case <-bctx.Done():
			// Not every target was dispatched: report the batch incomplete.
			wg.Wait()
			return results, false
		}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			defer func() { <-sem }()
			if scheme, ok := p.Probe(bctx, target); ok {
				mu.Lock()
				results[target] = scheme
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()
	// Every target was dispatched, but if the batch deadline fired while late
	// probes were still running they may have been cut short and misclassified
	// negative; treat that as incomplete too.
	return results, bctx.Err() == nil
}
