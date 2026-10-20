// Package serve hosts the bossanova MCP server over either stdio (the mode an
// MCP host spawns) or a persistent Streamable HTTP daemon.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/socketauth"
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

// HTTPOptions configures the Streamable HTTP daemon.
type HTTPOptions struct {
	// Token is the canonical hex bearer token every /mcp request must carry as
	// "Authorization: Bearer <token>" (socketauth.LoadOrCreateHTTPToken). An
	// empty or malformed token fails closed: every /mcp request is rejected.
	Token string
}

// HTTP runs the MCP server as a Streamable HTTP daemon, mounting the
// bearer-authenticated MCP endpoint at /mcp and the unauthenticated liveness
// and build-metadata probes at /healthz and /buildinfo. addr must be a loopback
// address.
func HTTP(ctx context.Context, addr string, backend bossmcp.Backend, opts bossmcp.Options, httpOpts HTTPOptions) error {
	return HTTPWithListenerHook(ctx, addr, backend, opts, httpOpts, nil)
}

// checkLoopbackAddr refuses any listen address whose host is not a loopback IP
// or "localhost". A non-loopback bind would expose the tool surface beyond the
// machine and turns off the SDK's DNS-rebinding Host check.
func checkLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("mcp: invalid --http address %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("mcp: refusing to serve HTTP on non-loopback address %q; use 127.0.0.1, [::1] or localhost", addr)
}

// requireBearer wraps next so a request without "Authorization: Bearer
// <token>" gets 401 with a Bearer challenge and never reaches the MCP handler.
func requireBearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !socketauth.ValidHTTPBearer(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bossanova-mcp"`)
			http.Error(w, "unauthorized: run `boss mcp token` and send it as Authorization: Bearer <token>", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HTTPWithListenerHook is HTTP with a callback invoked once the listener is
// bound, receiving the resolved address (useful when addr requests an ephemeral
// port). The hook is primarily a test seam.
func HTTPWithListenerHook(ctx context.Context, addr string, backend bossmcp.Backend, opts bossmcp.Options, httpOpts HTTPOptions, onListen func(addr string)) error {
	if err := checkLoopbackAddr(addr); err != nil {
		return err
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return newServer(backend, opts, nil)
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(httpOpts.Token, handler))
	// /healthz and /buildinfo stay unauthenticated: `boss mcp status` reads them
	// and they carry no capability.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// /buildinfo lets `boss mcp status` compare the running service's compiled-in
	// build metadata against the on-disk binary's, so a daemon left running on a
	// superseded binary can be named as drifted.
	mux.HandleFunc("/buildinfo", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(BuildInfo{
			Version: buildinfo.Version,
			Commit:  buildinfo.Commit,
			Date:    buildinfo.Date,
			Summary: buildinfo.String(),
		}); err != nil {
			log.Printf("mcp: encode build info response: %v", err)
		}
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
