package sessionports

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawServer starts a TCP server on loopback that runs handle for each accepted
// connection. It cleans up the listener and joins every handler goroutine on
// test end, so the goleak check in TestMain stays green.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				handle(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

func TestSocketProberPlainHTTP2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if !ok || scheme != "http" {
		t.Fatalf("got scheme=%q ok=%v, want http/true", scheme, ok)
	}
}

func TestSocketProberPlainHTTP4xxIsPositive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "http://")

	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if !ok || scheme != "http" {
		t.Fatalf("got scheme=%q ok=%v, want http/true (4xx is valid HTTP)", scheme, ok)
	}
}

func TestSocketProberSelfSignedHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	target := strings.TrimPrefix(srv.URL, "https://")

	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if !ok || scheme != "https" {
		t.Fatalf("got scheme=%q ok=%v, want https/true", scheme, ok)
	}
}

func TestSocketProberTLSUsesUnsplitTargetAsServerName(t *testing.T) {
	const target = "session.test"
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if hello.ServerName != target {
				return nil, fmt.Errorf("server name = %q, want %q", hello.ServerName, target)
			}
			return nil, nil
		},
	}
	srv.StartTLS()
	defer srv.Close()

	p := &socketProber{dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, srv.Listener.Addr().String())
	}}
	if !p.tryTLS(context.Background(), target) {
		t.Fatal("expected TLS probe to preserve a target without a port as the SNI server name")
	}
}

func TestSocketProberNonHTTPIsNegative(t *testing.T) {
	target := rawServer(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("GARBAGE BYTES\r\nnot http at all\r\n"))
	})
	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if ok {
		t.Fatalf("expected negative for non-HTTP server, got scheme=%q", scheme)
	}
}

func TestSocketProberTimeoutIsNegativeAndBounded(t *testing.T) {
	// Accept the connection but never respond; the prober must give up within
	// its per-probe deadline rather than hang.
	target := rawServer(t, func(conn net.Conn) {
		// Block until the client closes (drains until EOF), so the handler
		// goroutine exits cleanly for goleak.
		buf := make([]byte, 512)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	})
	start := time.Now()
	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if ok {
		t.Fatalf("expected negative for non-responsive server, got scheme=%q", scheme)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v, expected it to be bounded", elapsed)
	}
}

func TestSocketProberOversizedHeaderIsBounded(t *testing.T) {
	// Emit far more than the 16 KiB header cap with no valid status line: the
	// prober must stop reading and classify negative, not hang or OOM.
	blob := strings.Repeat("A", 64*1024)
	target := rawServer(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte(blob))
	})
	start := time.Now()
	scheme, ok := newSocketProber().Probe(context.Background(), target)
	if ok {
		t.Fatalf("expected negative for oversized garbage, got scheme=%q", scheme)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe took %v, expected it to be bounded", elapsed)
	}
}

func TestProbeBatchCollectsPositives(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer httpSrv.Close()
	httpsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer httpsSrv.Close()
	garbage := rawServer(t, func(conn net.Conn) { _, _ = conn.Write([]byte("nope\r\n")) })

	httpTarget := strings.TrimPrefix(httpSrv.URL, "http://")
	httpsTarget := strings.TrimPrefix(httpsSrv.URL, "https://")

	got, complete := probeBatch(context.Background(), newSocketProber(), []string{httpTarget, httpsTarget, garbage})
	if !complete {
		t.Fatal("expected a small batch under budget to report complete")
	}
	if got[httpTarget] != "http" {
		t.Fatalf("http target = %q, want http", got[httpTarget])
	}
	if got[httpsTarget] != "https" {
		t.Fatalf("https target = %q, want https", got[httpsTarget])
	}
	if _, ok := got[garbage]; ok {
		t.Fatalf("garbage target should not be present, got %q", got[garbage])
	}
}

// countingProber records peak concurrency and, when block is set, blocks each
// probe until the context is cancelled so the batch deadline truncates it.
type countingProber struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	block    bool
}

func (p *countingProber) Probe(ctx context.Context, _ string) (string, bool) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxSeen {
		p.maxSeen = p.inFlight
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	if p.block {
		<-ctx.Done()
		return "", false
	}
	return "http", true
}

func TestProbeBatchCapsConcurrency(t *testing.T) {
	p := &countingProber{}
	targets := make([]string, 0, maxProbeConcurrency*3)
	for i := 0; i < maxProbeConcurrency*3; i++ {
		targets = append(targets, fmt.Sprintf("127.0.0.1:%d", 9000+i))
	}
	got, complete := probeBatch(context.Background(), p, targets)
	if !complete {
		t.Fatal("expected fast non-blocking probes to complete the batch")
	}
	if len(got) != len(targets) {
		t.Fatalf("expected all %d targets positive, got %d", len(targets), len(got))
	}
	p.mu.Lock()
	maxSeen := p.maxSeen
	p.mu.Unlock()
	if maxSeen > maxProbeConcurrency {
		t.Fatalf("concurrency cap exceeded: peak in-flight %d, cap %d", maxSeen, maxProbeConcurrency)
	}
}

func TestProbeBatchReportsTruncationAndIsBounded(t *testing.T) {
	p := &countingProber{block: true}
	targets := make([]string, 0, maxProbeConcurrency*3)
	for i := 0; i < maxProbeConcurrency*3; i++ {
		targets = append(targets, fmt.Sprintf("127.0.0.1:%d", 9000+i))
	}
	// A parent deadline shorter than batchProbeTimeout forces the batch to give
	// up before every blocking target is dispatched.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	got, complete := probeBatch(ctx, p, targets)
	if complete {
		t.Fatal("expected complete=false when the deadline truncated probing")
	}
	if len(got) != 0 {
		t.Fatalf("expected no positives from truncated blocking probes, got %v", got)
	}
	if elapsed := time.Since(start); elapsed > batchProbeTimeout {
		t.Fatalf("probeBatch not bounded by ctx: took %v", elapsed)
	}
	p.mu.Lock()
	maxSeen := p.maxSeen
	p.mu.Unlock()
	if maxSeen == 0 {
		t.Fatal("expected some probes to run before truncation")
	}
	if maxSeen > maxProbeConcurrency {
		t.Fatalf("concurrency cap exceeded under load: peak %d, cap %d", maxSeen, maxProbeConcurrency)
	}
}
