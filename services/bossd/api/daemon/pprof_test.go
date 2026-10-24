package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPprofServedOverDaemonSocket is the AC-4 proof: the goroutine state of a
// running bossd can be captured without restarting it.
//
// It goes through the real Listen path — the same Unix socket, the same mux, the
// same 0600 permissions — rather than a hand-built test mux, so it would catch a
// registration that exists in isolation but is never wired into the daemon.
//
// The human equivalent is (see registerPprofHandlers for how the socket path
// resolves):
//
//	curl --unix-socket "$HOME/Library/Application Support/bossanova/bossd.sock" \
//	  'http://localhost/debug/pprof/goroutine?debug=2'
func TestPprofServedOverDaemonSocket(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	// Keep the path short: macOS caps sun_path at ~104 bytes, and t.TempDir()
	// names are long enough to matter once a filename is appended.
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("b717-%d.sock", time.Now().UnixNano()))
	if err := h.server.Listen(socketPath); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = h.server.Serve() }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(shutdownCtx)
		_ = os.Remove(socketPath)
	})

	// The socket must be owner-only: that file permission, not a loopback bind,
	// is what makes exposing pprof here safe.
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o, want 600", perm)
	}

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get("http://localhost/debug/pprof/goroutine?debug=2")
	if err != nil {
		t.Fatalf("GET /debug/pprof/goroutine: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	dump := string(body)
	// debug=2 renders a real stack dump, not the binary pprof protobuf.
	if !strings.Contains(dump, "goroutine ") {
		t.Fatalf("goroutine dump missing stack frames; got %.200q", dump)
	}
	if !strings.Contains(dump, "runtime.gopark") && !strings.Contains(dump, "testing.tRunner") {
		t.Fatalf("goroutine dump has no recognisable frames; got %.400q", dump)
	}
}

// TestPprofIndexServedOverDaemonSocket covers the index route, which is what
// dispatches every other runtime profile (heap, allocs, block, mutex).
func TestPprofIndexServedOverDaemonSocket(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("b717i-%d.sock", time.Now().UnixNano()))
	if err := h.server.Listen(socketPath); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = h.server.Serve() }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(shutdownCtx)
		_ = os.Remove(socketPath)
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}
	resp, err := client.Get("http://localhost/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "goroutine") {
		t.Fatalf("pprof index does not list the goroutine profile; got %.200q", body)
	}
}
