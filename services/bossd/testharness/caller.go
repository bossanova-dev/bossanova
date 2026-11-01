package testharness

import (
	"context"
	"net"
	"net/http"
	"time"

	server "github.com/recurser/bossd/api/daemon"

	"github.com/recurser/bossd/internal/domain"
)

// operatorCallerAuthority keeps tests independent of their hosting agent's process tree.
type operatorCallerAuthority struct{}

func (operatorCallerAuthority) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return ctx
}
func (operatorCallerAuthority) Principal(context.Context) domain.CallerPrincipal {
	return domain.CallerPrincipal{Kind: domain.CallerOperator}
}

func newHarnessHTTPServer(callers server.CallerAuthority, path string, handler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	return &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ConnContext: callers.ConnContext}
}

func newHarnessHTTPClient(socketPath string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		},
	}}
}
