// Package serve hosts the bossanova MCP server over either stdio (the mode an
// MCP host spawns) or a persistent Streamable HTTP daemon.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/buildinfo"
)

// keepAlive is the protocol-level ping interval. The SDK exposes keepalive via
// ServerOptions.KeepAlive (NOT StreamableHTTPOptions, which only has Stateless
// and JSONResponse in v1.0.0). 25s keeps tunnelled SSE streams alive well under
// Cloudflare's ~100s idle timeout.
const keepAlive = 25 * time.Second

// BuildInfo is the JSON body served at /buildinfo: the running service's
// compiled-in build metadata. Summary is buildinfo.String() so a consumer can
// compare running vs on-disk with a single field.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Summary string `json:"summary"`
}

func version() string {
	if v := buildinfo.Version; v != "" {
		return v
	}
	return "dev"
}

// newServer builds an MCP server with every bossanova tool registered and the
// keepalive ping interval configured. When detector is non-nil, a receiving
// middleware surfaces a staleness warning once the on-disk binary is replaced
// (the stdio per-chat drift case); a nil detector leaves responses untouched.
func newServer(backend bossmcp.Backend, opts bossmcp.Options, detector *driftDetector) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "bossanova", Version: version()},
		&mcp.ServerOptions{KeepAlive: keepAlive},
	)
	bossmcp.RegisterTools(s, backend, opts)
	if detector != nil {
		s.AddReceivingMiddleware(driftMiddleware(detector, func(msg string) {
			// stdout is the stdio transport — the warning must go to stderr.
			log.Println("mcp:", msg)
		}))
	}
	return s
}

// Stdio runs the MCP server over stdin/stdout. Each MCP host spawns one such
// server per chat and keeps it for the chat's lifetime, so it self-detects when
// its own binary is rebuilt underneath it.
func Stdio(ctx context.Context, backend bossmcp.Backend, opts bossmcp.Options) error {
	return newServer(backend, opts, detectorForSelf()).Run(ctx, &mcp.StdioTransport{})
}

// HTTP runs the MCP server as a Streamable HTTP daemon, mounting the MCP
// endpoint at /mcp and a liveness probe at /healthz.
func HTTP(ctx context.Context, addr string, backend bossmcp.Backend, opts bossmcp.Options) error {
	return HTTPWithListenerHook(ctx, addr, backend, opts, nil)
}

// HTTPWithListenerHook is HTTP with a callback invoked once the listener is
// bound, receiving the resolved address (useful when addr requests an ephemeral
// port). The hook is primarily a test seam.
func HTTPWithListenerHook(ctx context.Context, addr string, backend bossmcp.Backend, opts bossmcp.Options, onListen func(addr string)) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return newServer(backend, opts, nil)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// /buildinfo lets `boss mcp status` compare the running service's compiled-in
	// build metadata against the on-disk binary's, so a daemon left running on a
	// superseded binary can be named as drifted.
	mux.HandleFunc("/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BuildInfo{
			Version: buildinfo.Version,
			Commit:  buildinfo.Commit,
			Date:    buildinfo.Date,
			Summary: buildinfo.String(),
		})
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if onListen != nil {
		onListen(ln.Addr().String())
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
