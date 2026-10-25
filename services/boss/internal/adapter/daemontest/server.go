// Package daemontest serves consumer-owned RPC fakes on authenticated Unix sockets.
package daemontest

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

// Serve starts a socket server and waits for its shutdown during test cleanup.
func Serve(t *testing.T, handler bossanovav1connect.DaemonServiceHandler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "boss-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	token, err := socketauth.LoadOrCreateToken(socket)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	path, rpc := bossanovav1connect.NewDaemonServiceHandler(handler, connect.WithInterceptors(socketauth.NewServerInterceptor(token)))
	mux := http.NewServeMux()
	mux.Handle(path, rpc)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := safego.Go(zerolog.Nop(), func() { _ = server.Serve(listener) })
	t.Cleanup(func() { _ = server.Close(); <-done })
	return socket
}
