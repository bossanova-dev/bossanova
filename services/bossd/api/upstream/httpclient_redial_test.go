package upstream

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
	"golang.org/x/net/http2"
)

type observedRedialTransport struct {
	*http.Transport
	closed atomic.Bool
}

func TestRedialTransportRetiresH2CConnectionsAfterActiveRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		release := make(chan struct{})
		released := false
		server := &http2.Server{}
		done := safego.Go(zerolog.Nop(), func() {
			//nolint:staticcheck // Serve the net.Pipe connection directly to test h2c retirement inside a synctest bubble.
			server.ServeConn(serverConn, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				<-release
			})})
		})
		defer func() {
			_ = clientConn.Close()
			_ = serverConn.Close()
			<-done
		}()
		defer func() {
			if !released {
				close(release)
			}
		}()

		client := BuildUpstreamHTTPClient("http://upstream.example")
		tr, ok := client.Transport.(*RedialTransport)
		if !ok {
			t.Fatalf("transport = %T, want *RedialTransport", client.Transport)
		}
		old, ok := tr.Current().(*http2.Transport)
		if !ok {
			t.Fatalf("inner transport = %T, want *http2.Transport", tr.Current())
		}
		old.DialTLSContext = func(context.Context, string, string, *tls.Config) (net.Conn, error) {
			return clientConn, nil
		}
		resp, err := client.Get("http://upstream.example")
		if err != nil {
			t.Fatal(err)
		}
		tr.Redial()
		time.Sleep(2 * time.Minute)
		select {
		case <-done:
			t.Fatal("re-dial interrupted an active request")
		default:
		}
		close(release)
		released = true
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("retired connection remained open after becoming idle")
		}
	})
}

func (t *observedRedialTransport) CloseIdleConnections() {
	t.closed.Store(true)
	t.Transport.CloseIdleConnections()
}

func TestRedialTransportDialsFreshConnection(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "second")
	}))
	defer second.Close()
	target := first.URL
	var built []*observedRedialTransport
	tr := newRedialTransport(func() http.RoundTripper {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		inner := &observedRedialTransport{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, u.Host)
			},
		}}
		built = append(built, inner)
		return inner
	})
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	read := func(want string) {
		t.Helper()
		resp, err := client.Get("http://upstream.example")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil || string(body) != want {
			t.Fatalf("response = %q, %v; want %q", body, err, want)
		}
	}
	read("first")
	target = second.URL
	read("first") // The old pool still owns its connection until re-dial.
	tr.Redial()
	read("second")
	if len(built) != 2 || !built[0].closed.Load() {
		t.Fatal("re-dial did not retire the old transport's idle connections")
	}
	tr.CloseIdleConnections()
	if !built[1].closed.Load() {
		t.Fatal("CloseIdleConnections did not reach the current transport")
	}
}
