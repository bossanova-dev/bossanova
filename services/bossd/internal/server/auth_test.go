package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
	"github.com/recurser/bossd/internal/db"
)

// clientFor builds a DaemonServiceClient over the daemon's Unix socket. It
// attaches only the interceptors passed in, so a caller can construct an
// explicitly UNAUTHENTICATED client (no token interceptor) for negative tests.
func clientFor(t *testing.T, socketPath string, opts ...connect.ClientOption) bossanovav1connect.DaemonServiceClient {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	return bossanovav1connect.NewDaemonServiceClient(hc, "http://localhost", opts...)
}

// startAuthedTestDaemon stands up a real DaemonService via Listen on a Unix
// socket (so the server interceptor wired into Listen is exercised) and returns
// the socket path, the generated token (read from the co-located token file),
// and a stop func. Pass an empty socketPath to allocate a fresh one, or reuse a
// path to simulate a daemon restart against the same on-disk token.
func startAuthedTestDaemon(t *testing.T, socketPath string) (string, string, func()) {
	t.Helper()
	if socketPath == "" {
		// Use /tmp directly — t.TempDir() paths can exceed the 104-char macOS
		// Unix socket limit.
		socketPath = filepath.Join("/tmp", fmt.Sprintf("bossd-auth-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	}
	s := New(Config{Repos: db.NewRepoStore(setupServerTestDB(t))})
	if err := s.Listen(socketPath); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	go func() { _ = s.Serve() }()

	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		_ = os.Remove(socketPath)
	}

	token, err := socketauth.ReadToken(socketPath)
	if err != nil {
		stop()
		t.Fatalf("ReadToken after Listen: %v", err)
	}
	return socketPath, token, stop
}

func TestDaemon_RejectsUnauthenticatedClient(t *testing.T) {
	socketPath, _, stop := startAuthedTestDaemon(t, "")
	defer stop()

	c := clientFor(t, socketPath) // NO token interceptor — explicitly unauthenticated
	_, err := c.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestDaemon_AcceptsAuthenticatedClient(t *testing.T) {
	socketPath, token, stop := startAuthedTestDaemon(t, "")
	defer stop()

	// G302: the listener socket must be owner read/write only (0o600) — no
	// group/world bits and no unused execute bit — while still accepting an
	// authenticated dial (below). Assert the mode here, on the same listener a
	// real client connects over.
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 0o600", perm)
	}

	c := clientFor(t, socketPath, connect.WithInterceptors(socketauth.NewClientInterceptor(token)))
	if _, err := c.ListRepos(context.Background(), connect.NewRequest(&pb.ListReposRequest{})); err != nil {
		t.Fatalf("authenticated ListRepos failed: %v", err)
	}
}

// TestDaemon_RejectsUnauthenticatedStream proves the streaming handler is
// gated on stream-open, not just unary RPCs.
func TestDaemon_RejectsUnauthenticatedStream(t *testing.T) {
	socketPath, _, stop := startAuthedTestDaemon(t, "")
	defer stop()

	c := clientFor(t, socketPath) // no token
	stream, err := c.AttachSession(context.Background(), connect.NewRequest(&pb.AttachSessionRequest{Id: "does-not-exist"}))
	if err == nil {
		// Server streams surface the open-time error on the first Receive.
		stream.Receive()
		err = stream.Err()
		_ = stream.Close()
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestDaemon_TokenStableAcrossRestart(t *testing.T) {
	socketPath, tok1, stop1 := startAuthedTestDaemon(t, "")
	stop1() // stop the first daemon, leaving the token file in place

	_, tok2, stop2 := startAuthedTestDaemon(t, socketPath) // restart on same path
	defer stop2()
	if tok1 != tok2 {
		t.Fatalf("token changed across restart: %q vs %q", tok1, tok2)
	}
}
