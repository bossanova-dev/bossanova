package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"database/sql"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// finalizeDispatchTimeout bounds the background FinalizeSession call spawned
// by the Stop hook. Five minutes comfortably covers the worst-case
// EnsurePR + push + chat-spawn path on a slow repo while still preventing
// a stuck finalize from leaking goroutines forever.
const finalizeDispatchTimeout = 5 * time.Minute

// HookFinalizer is the narrow subset of *session.Lifecycle the hook server
// depends on. Defined as an interface so tests can substitute a fake
// finalizer without pulling in a full Lifecycle.
type HookFinalizer interface {
	FinalizeSession(ctx context.Context, sessionID string) (*session.FinalizeResult, error)
}

// HookServer runs a loopback-only HTTP server that receives Claude Code
// Stop-hook notifications and dispatches FinalizeSession asynchronously.
//
// Security model:
//   - Binds 127.0.0.1 only. External traffic cannot reach the endpoint.
//   - Per-session bearer token auth; constant-time compare against
//     sessions.hook_token.
//   - FinalizeSession runs in a background goroutine; the HTTP response
//     returns as soon as auth succeeds. Claude's Stop-hook contract asks
//     for non-blocking handlers.
//
// The bound port is exposed via Port() so the daemon entrypoint can plumb
// it into Lifecycle.SetHookPort directly. The hook server runs in the
// same process as the lifecycle, so there is no port file on disk — a
// previous design wrote one to ~/Library/Application Support/bossanova/
// and the lifecycle read it back, but a missing or stale file produced
// silent cron failures and the file added nothing the in-process pointer
// didn't already provide.
type HookServer struct {
	sessions  db.SessionStore
	finalizer HookFinalizer
	logger    zerolog.Logger
	listener  net.Listener
	srv       *http.Server
}

// HookServerConfig gathers the HookServer dependencies.
type HookServerConfig struct {
	Sessions  db.SessionStore
	Finalizer HookFinalizer
	Logger    zerolog.Logger
}

// NewHookServer constructs a HookServer. Call Listen to bind, then Serve
// (typically in a goroutine). Shutdown is safe after either.
func NewHookServer(cfg HookServerConfig) *HookServer {
	return &HookServer{
		sessions:  cfg.Sessions,
		finalizer: cfg.Finalizer,
		logger:    cfg.Logger,
	}
}

// Listen binds a loopback-only TCP listener on an ephemeral port and wires
// the finalize route into an http.Server.
//
// Split from Serve so Shutdown can race safely with the serving goroutine
// (the http.Server field is populated synchronously here).
func (h *HookServer) Listen() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen tcp 127.0.0.1: %w", err)
	}
	h.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/finalize/{session_id}", h.handleFinalize)

	h.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return nil
}

// Serve blocks serving requests on the loopback listener. Returns
// http.ErrServerClosed on clean Shutdown.
func (h *HookServer) Serve() error {
	return h.srv.Serve(h.listener)
}

// Shutdown gracefully stops the server.
func (h *HookServer) Shutdown(ctx context.Context) error {
	if h.srv == nil {
		return nil
	}
	return h.srv.Shutdown(ctx)
}

// Port returns the bound port. Returns 0 before Listen has been called.
func (h *HookServer) Port() int {
	if h.listener == nil {
		return 0
	}
	addr, ok := h.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// handleFinalize implements POST /hooks/finalize/{session_id}.
//
// Response codes:
//   - 400 — session_id path parameter missing
//   - 401 — Authorization header missing/malformed or token mismatch
//   - 404 — session not found
//   - 500 — session lookup failed for non-NotFound reasons
//   - 200 — session found with nil hook_token (non-cron no-op) OR
//     auth succeeded and FinalizeSession was dispatched. The Stop hook
//     cannot distinguish these; it only needs to know the daemon received
//     the signal.
func (h *HookServer) handleFinalize(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	sess, err := h.sessions.Get(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		h.logger.Error().Err(err).Str("session", sessionID).Msg("hook: session lookup failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Non-cron sessions have no hook_token configured. Treat as a silent
	// no-op 200 so a misconfigured settings.local.json in a non-cron
	// worktree doesn't spam 401s in the daemon log.
	if sess.HookToken == nil || *sess.HookToken == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(*sess.HookToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Auth passed; dispatch FinalizeSession asynchronously and return 200
	// immediately. Claude's Stop-hook contract expects non-blocking
	// handlers. Errors are logged; the outcome column on cron_jobs is the
	// user-facing record of what happened.
	safego.Go(h.logger, func() {
		ctx, cancel := context.WithTimeout(context.Background(), finalizeDispatchTimeout)
		defer cancel()
		if _, err := h.finalizer.FinalizeSession(ctx, sessionID); err != nil {
			h.logger.Error().Err(err).
				Str("session", sessionID).
				Msg("hook: FinalizeSession failed")
		}
	})

	w.WriteHeader(http.StatusOK)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value. Returns ok=false for empty, non-Bearer, or empty-token
// inputs.
func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", false
	}
	return token, true
}
