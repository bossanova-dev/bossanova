package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"database/sql"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin"
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

// AgentRunCompleter is the narrow subset of *plugin.HostServiceServer the
// hook server depends on for /hooks/agent-run-complete dispatch. Defined
// as an interface so tests can substitute a fake.
//
// CompleteAgentRun looks up the run by agentSessionID, validates the
// bearer token in constant time, signals the run's completion channel
// (idempotent under duplicate POSTs), and clears tracker state.
//
// Returns sessionID for the boss session that owned the run on success.
// Returns an error wrapping plugin.ErrAgentRunNotFound when the
// agent_session_id was never registered (HTTP 404). Returns an error
// wrapping plugin.ErrAuthMismatch when the bearer token doesn't match
// the recorded run token (HTTP 401). exitError is the message from the
// hook payload (empty on clean exit).
type AgentRunCompleter interface {
	CompleteAgentRun(ctx context.Context, agentSessionID, bearerToken, exitError string) (sessionID string, err error)
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
	completer AgentRunCompleter
	logger    zerolog.Logger
	listener  net.Listener
	srv       *http.Server
}

// HookServerConfig gathers the HookServer dependencies.
//
// AgentRunCompleter is optional only for legacy test sites that don't
// exercise the agent-run path; production wiring (cmd/main.go) and the
// shared test harness pass the *plugin.HostServiceServer here.
type HookServerConfig struct {
	Sessions  db.SessionStore
	Finalizer HookFinalizer
	Completer AgentRunCompleter
	Logger    zerolog.Logger
}

// NewHookServer constructs a HookServer. Call Listen to bind, then Serve
// (typically in a goroutine). Shutdown is safe after either.
func NewHookServer(cfg HookServerConfig) *HookServer {
	return &HookServer{
		sessions:  cfg.Sessions,
		finalizer: cfg.Finalizer,
		completer: cfg.Completer,
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
	mux.HandleFunc("POST /hooks/agent-run-complete/{agent_session_id}", h.handleAgentRunComplete)

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

// agentRunCompletePayload is the JSON body expected on
// POST /hooks/agent-run-complete/{agent_session_id}. Empty string on
// clean exit; populated when claude crashed or returned non-zero.
type agentRunCompletePayload struct {
	ExitError string `json:"exit_error"`
}

// handleAgentRunComplete implements POST /hooks/agent-run-complete/{agent_session_id}.
//
// Response codes:
//   - 400 — agent_session_id path parameter missing, or malformed JSON body
//   - 401 — Authorization header missing/malformed or token mismatch
//   - 404 — agent_session_id not registered (already completed or never started)
//   - 500 — completer not configured (misconfiguration; should not happen in production)
//   - 200 — auth succeeded and waiter signalled
//
// The signal is synchronous (a buffered channel send + map cleanup) so
// there's no need for the safego.Go pattern that handleFinalize uses for
// FinalizeSession dispatch — the 30-minute upper bound on a run lives on
// the WaitChatRun side, not here.
func (h *HookServer) handleAgentRunComplete(w http.ResponseWriter, r *http.Request) {
	agentSessionID := r.PathValue("agent_session_id")
	if agentSessionID == "" {
		http.Error(w, "agent_session_id required", http.StatusBadRequest)
		return
	}

	if h.completer == nil {
		// Defense-in-depth: the daemon entrypoint and test harness wire a
		// non-nil completer in. Surface a clear 500 if a future caller
		// constructs a HookServer without one.
		h.logger.Error().Str("agent_session", agentSessionID).Msg("hook: agent run completer not configured")
		http.Error(w, "agent run completer not configured", http.StatusInternalServerError)
		return
	}

	// Body is optional; an empty body is treated as {"exit_error": ""}.
	var payload agentRunCompletePayload
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Error().Err(err).Str("agent_session", agentSessionID).Msg("hook: read body failed")
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "malformed JSON body", http.StatusBadRequest)
			return
		}
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		// Log auth failures as Warn so operators see token-rotation or
		// pointing-at-wrong-daemon issues immediately, instead of waiting
		// ~30 minutes for the WaitChatRun deadline to elapse.
		h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: agent-run-complete auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	sessionID, err := h.completer.CompleteAgentRun(r.Context(), agentSessionID, token, payload.ExitError)
	switch {
	case errors.Is(err, plugin.ErrAgentRunNotFound):
		http.Error(w, "agent run not found", http.StatusNotFound)
	case errors.Is(err, plugin.ErrAuthMismatch):
		h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: agent-run-complete auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case err != nil:
		h.logger.Error().Err(err).Str("agent_session", agentSessionID).Msg("hook: complete agent run failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
	default:
		_ = sessionID // observability hook for future use
		w.WriteHeader(http.StatusOK)
	}
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
