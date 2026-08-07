package remotehost

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/socketauth"
)

// loopbackDaemon stands in for the remote bossd: it answers ListRepos (the RPC
// LocalClient.Ping issues) and records the Authorization header it was sent, so
// the test can assert what actually crossed the forwarded socket rather than
// trusting the client's own bookkeeping.
type loopbackDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	mu     sync.Mutex
	header string
	seen   bool
}

func (d *loopbackDaemon) ListRepos(_ context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	d.mu.Lock()
	d.header = req.Header().Get(socketauth.AuthHeader)
	d.seen = true
	d.mu.Unlock()
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

func (d *loopbackDaemon) snapshot() (header string, seen bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.header, d.seen
}

// startLoopbackDaemon serves loopbackDaemon on a unix socket behind the real
// socketauth server interceptor, and returns the token that daemon will accept —
// what FetchToken reads over ssh in production.
//
// This is a local equivalent of package client's startAuthDaemon/recordingAuthDaemon
// rather than a reuse of them: those helpers are unexported test helpers in
// another package, and importing them is not possible.
func startLoopbackDaemon(t *testing.T) (socketPath string, daemon *loopbackDaemon, token string) {
	t.Helper()
	// /tmp directly, not t.TempDir(): a temp dir path plus a socket name can
	// exceed the ~104-char sun_path ceiling on darwin.
	socketPath = filepath.Join("/tmp", fmt.Sprintf("boss-host-loopback-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	_ = os.Remove(socketPath)

	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		t.Fatalf("LoadOrCreateToken: %v", err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	daemon = &loopbackDaemon{}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(
		daemon,
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
	return socketPath, daemon, token
}

// startRelay binds from and pipes every connection to to, which is what
// `ssh -L <local>:<remote>` does to bytes. It runs in-process because the fake
// ssh child is a /bin/sh stub: the child only holds the process slot the
// supervisor watches, and this relay carries the traffic.
//
// Nothing here calls t.Fatalf/t.Errorf — these goroutines outlive the test body,
// and reporting from them races the test's own completion.
func startRelay(t *testing.T, from, to string) {
	t.Helper()
	ln, err := net.Listen("unix", from)
	if err != nil {
		t.Fatalf("relay listen on %s: %v", from, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return // the listener was closed by cleanup
			}
			go relayConn(conn, to)
		}
	}()
}

// relayConn copies bytes both ways until either side hangs up. One direction
// finishing is enough to tear the pair down: HTTP/2 over a unix socket closes
// the whole connection, not a half of it.
func relayConn(down net.Conn, to string) {
	defer func() { _ = down.Close() }()
	up, err := net.Dial("unix", to)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()

	finished := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, down); finished <- struct{}{} }()
	go func() { _, _ = io.Copy(down, up); finished <- struct{}{} }()
	<-finished
}

// TestTunnelLoopbackDrivesARealRPCWithTheFetchedToken composes the pieces the
// unit tests only exercise separately: a supervised Tunnel, its forwarded local
// socket, and client.NewLocalWithToken carrying a token that exists only in
// memory. It drives a real Connect RPC through that stack and checks the far end
// received the exact token.
//
// What it proves: the composition is wire-correct end to end — the local socket
// the supervisor manages is dialable by the ordinary boss client, the explicitly
// supplied token reaches a daemon that actually enforces it, the tunnel's
// private directory still holds nothing but the socket once real traffic has
// flowed through it, and Close tears the directory down.
//
// What it does NOT prove: that `ssh -L` itself forwards correctly. The forward
// is stubbed — a /bin/sh child stands in for ssh and an in-process relay carries
// the bytes — because a real ssh needs a second machine (or a sshd and
// credentials) that CI does not have. The argv that a real ssh would receive is
// asserted separately in TestTunnelForwardArgv, and re-checked here so the
// relay is demonstrably standing in for the same endpoints.
func TestTunnelLoopbackDrivesARealRPCWithTheFetchedToken(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback tunnel integration test needs a real forwarder subprocess; skipped under -short")
	}

	remoteSocket, daemon, token := startLoopbackDaemon(t)
	tun, fake := newTestTunnel(t, func(cfg *TunnelConfig) {
		cfg.RemoteSocket = remoteSocket
	})

	// runOnce clears the local socket path before spawning the child, so the
	// relay must bind after that — which is exactly when `before` fires. A Once
	// keeps a supervisor restart from trying to bind a second time.
	var bound sync.Once
	fake.setBefore(func(int) {
		bound.Do(func() { startRelay(t, tun.LocalSocket(), remoteSocket) })
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tun.Start(ctx)

	readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
	defer readyCancel()
	if err := tun.Ready(readyCtx); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// The stubbed forward must still be pointed at the same two endpoints a real
	// ssh would have been given, or the relay would be proving a different path.
	_, args := fake.argv(0)
	wantForward := tun.LocalSocket() + ":" + remoteSocket
	if args[len(args)-3] != "-L" || args[len(args)-2] != wantForward {
		t.Fatalf("forward spec = %q, want -L %q", args, wantForward)
	}

	rpcCtx, rpcCancel := context.WithTimeout(ctx, 5*time.Second)
	defer rpcCancel()
	c := client.NewLocalWithToken(tun.LocalSocket(), token)
	if err := c.Ping(rpcCtx); err != nil {
		t.Fatalf("Ping through the tunnel: %v", err)
	}

	header, seen := daemon.snapshot()
	if !seen {
		t.Fatal("the daemon never saw the RPC, so nothing crossed the forwarded socket")
	}
	if want := "Bearer " + token; header != want {
		t.Fatalf("Authorization header = %q, want %q", header, want)
	}

	// The acceptance criterion, now on the composed path rather than a unit
	// fake: the remote daemon's token is held in memory, so even after a real
	// authenticated RPC nothing but the socket exists beside it.
	dir := filepath.Dir(tun.LocalSocket())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(tun.LocalSocket()) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("tunnel dir contains %v after a real RPC, want only the socket", names)
	}

	if err := tun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("tunnel dir survived Close: stat err = %v", err)
	}
}
