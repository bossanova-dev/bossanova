package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
)

// authTestDaemon is a minimal DaemonService that answers ListRepos (the RPC
// LocalClient.Ping uses) with an empty success response. Everything else is
// Unimplemented.
type authTestDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
}

func (authTestDaemon) ListRepos(context.Context, *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

// startAuthDaemon serves authTestDaemon on a Unix socket, gated by the
// socketauth server interceptor for the token co-located with socketPath.
func startAuthDaemon(t *testing.T) string {
	t.Helper()
	// Use /tmp directly — t.TempDir() can exceed the 104-char macOS Unix socket limit.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-client-auth-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(
		authTestDaemon{},
		connect.WithInterceptors(socketauth.NewServerInterceptor(token)),
	)
	mux.Handle(path, handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
	})
	return socketPath
}

func TestNewLocal_AttachesTokenWhenPresent(t *testing.T) {
	socketPath := startAuthDaemon(t)

	c := NewLocal(socketPath)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("authenticated Ping failed: %v", err)
	}
}

func TestNewLocal_MissingTokenGivesClearError(t *testing.T) {
	socketPath := startAuthDaemon(t)

	// Remove the co-located token so the client attaches no credential and the
	// daemon rejects it — the missing-token path a stale/old boss would hit.
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded without a token, want a clear error")
	}
	if !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("err = %v, want a clear daemon-rejected error mentioning 'daemon'", err)
	}
}

// TestNewLocal_MissingTokenClearErrorOnNonPingRPC proves the friendly mapping is
// applied to every unary RPC (via errMapInterceptor), not only Ping — the TUI/CLI
// first contact the daemon through ListRepos/ListSessions, not Ping.
func TestNewLocal_MissingTokenClearErrorOnNonPingRPC(t *testing.T) {
	socketPath := startAuthDaemon(t)
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	_, err := c.ListRepos(context.Background())
	if err == nil {
		t.Fatal("ListRepos succeeded without a token, want a clear error")
	}
	if !strings.Contains(err.Error(), "daemon rejected this client") {
		t.Fatalf("err = %v, want the mapped daemon-rejected error", err)
	}
}

// TestNewLocal_MissingTokenClearErrorOnStream proves the friendly mapping also
// covers streaming RPCs (surfaced via the stream's Err()), not just unary — a
// stale-token client opening a stream must get the actionable message too.
func TestNewLocal_MissingTokenClearErrorOnStream(t *testing.T) {
	socketPath := startAuthDaemon(t)
	if err := os.Remove(socketauth.TokenPath(socketPath)); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	c := NewLocal(socketPath)
	stream, err := c.AttachSession(context.Background(), "does-not-exist")
	if err == nil {
		// Stream-open auth surfaces on the first Receive/Err.
		stream.Receive()
		err = stream.Err()
		_ = stream.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "daemon rejected this client") {
		t.Fatalf("err = %v, want the mapped daemon-rejected error on the stream path", err)
	}
}
