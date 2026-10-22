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

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
)

// QuestionSignalRecorder records — and clears — a structured "a question is
// pending" signal for a chat, keyed by agent_session_id. Satisfied by
// *questionsignal.Store. Kept as a narrow interface so the hook server has no
// hard dependency on the store's concrete type and tests can substitute a
// fake.
//
// Clear is the inverse of SetPending and is reached only by an explicitly
// clearing payload (see clearingNotification); it is a no-op when no record
// exists.
type QuestionSignalRecorder interface {
	SetPending(agentSessionID, source string)
	Clear(agentSessionID string)
}

// RunTokenValidator authenticates a per-run bearer token WITHOUT any side
// effect (no completion signal, no state mutation). Satisfied by
// *plugin.HostServiceServer.ValidateRunToken. The question hook carries the
// same per-run token the run-scoped Stop/Notification hooks use, so it
// authenticates through this seam rather than the sessions.hook_token path.
type RunTokenValidator interface {
	ValidateRunToken(ctx context.Context, agentSessionID, bearerToken string) error
}

// PromptReceiptRecorder records that an agent submitted a prompt, keyed by
// agent_session_id, with the receipt's arrival time. Satisfied by
// *session.Lifecycle.RecordPromptSubmitted, which keeps a receipt only while a
// launch is confirming that chat's first input and reports whether one was.
//
// ValidPromptReceiptToken authenticates the per-chat receipt bearer token
// (session.Lifecycle derives it from the daemon socket token), with no side
// effect.
type PromptReceiptRecorder interface {
	RecordPromptSubmitted(agentSessionID string, at time.Time) bool
	ValidPromptReceiptToken(agentSessionID, token string) bool
}

// BackgroundAgentRecorder records how many background subagents a chat's last
// finished turn left running (*status.Tracker).
type BackgroundAgentRecorder interface {
	SetBackgroundAgents(agentSessionID string, count int)
}

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

// CronCompletionNotifier receives session-keyed agent completion signals.
// The hook server does not decide whether a cron session is truly complete;
// it only forwards the signal to the lifecycle gate.
type CronCompletionNotifier interface {
	NotifyCronAgentStopped(sessionID string)
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
	sessions               db.SessionStore
	finalizer              HookFinalizer
	cronCompletionNotifier CronCompletionNotifier
	completer              AgentRunCompleter
	questionSignals        QuestionSignalRecorder
	questionAuth           RunTokenValidator
	promptReceipts         PromptReceiptRecorder
	backgroundAgents       BackgroundAgentRecorder
	logger                 zerolog.Logger
	listener               net.Listener
	srv                    *http.Server
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
	// QuestionSignals and QuestionAuth back POST /hooks/question/{agent_session_id}
	// (BOS-485). Both are optional for legacy/test wiring that doesn't exercise
	// the structured question signal; when either is nil the endpoint returns
	// 500 rather than silently no-op'ing. Production (cmd/main.go) and the
	// shared test harness wire both to the questionsignal store and the
	// *plugin.HostServiceServer respectively.
	QuestionSignals QuestionSignalRecorder
	QuestionAuth    RunTokenValidator
	// PromptReceipts backs POST /hooks/prompt-submitted/{agent_session_id}, the
	// first-input delivery receipt. Optional for legacy/test wiring; when nil
	// the endpoint returns 500. Production wires the session.Lifecycle.
	PromptReceipts PromptReceiptRecorder
	// BackgroundAgents backs POST /hooks/turn-ended/{agent_session_id}, which
	// keeps a chat idle over a running background subagent WORKING. Optional:
	// when nil the endpoint acknowledges and records nothing, and the chat's
	// status falls back to the pane. Production wires the status tracker.
	BackgroundAgents BackgroundAgentRecorder
	Logger           zerolog.Logger
}

// NewHookServer constructs a HookServer. Call Listen to bind, then Serve
// (typically in a goroutine). Shutdown is safe after either.
func NewHookServer(cfg HookServerConfig) *HookServer {
	return &HookServer{
		sessions:         cfg.Sessions,
		finalizer:        cfg.Finalizer,
		completer:        cfg.Completer,
		questionSignals:  cfg.QuestionSignals,
		questionAuth:     cfg.QuestionAuth,
		promptReceipts:   cfg.PromptReceipts,
		backgroundAgents: cfg.BackgroundAgents,
		logger:           cfg.Logger,
	}
}

// SetCronCompletionNotifier wires the cron completion gate used by the
// session-keyed finalize hook. Nil preserves the legacy direct-finalize path
// for tests or partially wired daemons.
func (h *HookServer) SetCronCompletionNotifier(notifier CronCompletionNotifier) {
	h.cronCompletionNotifier = notifier
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
	mux.HandleFunc("POST /hooks/question/{agent_session_id}", h.handleQuestion)
	mux.HandleFunc("POST /hooks/prompt-submitted/{agent_session_id}", h.handlePromptSubmitted)
	mux.HandleFunc("POST /hooks/turn-ended/{agent_session_id}", h.handleTurnEnded)

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
//     auth succeeded and the cron completion gate was notified. If no gate is
//     wired, FinalizeSession was dispatched through the compatibility fallback.
//     The Stop hook cannot distinguish these; it only needs to know the daemon
//     received the signal.
func (h *HookServer) handleFinalize(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}

	sess, err := h.sessions.Get(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
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

	if h.cronCompletionNotifier != nil {
		h.cronCompletionNotifier.NotifyCronAgentStopped(sessionID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Compatibility fallback for tests and older wiring: if no notifier has
	// been installed, dispatch FinalizeSession as previous versions did.
	//
	// WARNING: this path BYPASSES the CronCompletionGate's run-completion check
	// (RunIsOver → cronRunIsOver), so it would finalize on any Stop hook — i.e.
	// mid-run. Production always wires the gate (cmd/main.go), so this is only
	// reached by tests and legacy/partial wiring. Do not rely on it for cron
	// sessions: finalize gating lives in the gate, which is the single chokepoint.
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

// maxAgentRunCompleteBody caps the agent-run-complete payload. It carries one
// short exit_error string, so 64 KiB is generous while bounding allocation.
const maxAgentRunCompleteBody = 64 << 10

// agentRunCompletePayload is the JSON body expected on
// POST /hooks/agent-run-complete/{agent_session_id}. Empty string on
// clean exit; populated when claude crashed or returned non-zero.
type agentRunCompletePayload struct {
	ExitError string `json:"exit_error"`
}

// handleAgentRunComplete implements POST /hooks/agent-run-complete/{agent_session_id}.
//
// Auth runs before the body is read: the Authorization header shape is checked
// first, then the token is validated with no side effect through the
// RunTokenValidator (when wired), and only then is the body read, capped at
// maxAgentRunCompleteBody.
//
// Response codes:
//   - 400 — agent_session_id path parameter missing, or malformed JSON body
//   - 401 — Authorization header missing/malformed or token mismatch
//   - 413 — body larger than maxAgentRunCompleteBody
//   - 500 — completer not configured (misconfiguration; should not happen in production)
//   - 200 — auth succeeded and waiter signalled, OR the run is unknown/already
//     completed (idempotent no-op; a duplicate or post-cleanup ping)
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

	// Authenticate before touching the body, so a caller without a run token
	// cannot make the daemon read (or allocate for) anything.
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		// Log auth failures as Warn so operators see token-rotation or
		// pointing-at-wrong-daemon issues immediately, instead of waiting
		// ~30 minutes for the WaitChatRun deadline to elapse.
		h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: agent-run-complete auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Side-effect-free pre-check through the same validator the question hook
	// uses. CompleteAgentRun repeats the authoritative constant-time check, so
	// valid callers see no change.
	if h.questionAuth != nil {
		switch err := h.questionAuth.ValidateRunToken(r.Context(), agentSessionID, token); {
		case err == nil:
		case errors.Is(err, plugin.ErrAgentRunNotFound):
			// Idempotent no-op for a run the daemon no longer tracks; see the
			// matching branch after CompleteAgentRun below.
			w.WriteHeader(http.StatusOK)
			return
		case errors.Is(err, plugin.ErrAuthMismatch):
			h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: agent-run-complete auth failed")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		default:
			h.logger.Error().Err(err).Str("agent_session", agentSessionID).Msg("hook: agent-run-complete auth error")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	// Body is optional; an empty body is treated as {"exit_error": ""}.
	var payload agentRunCompletePayload
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAgentRunCompleteBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
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

	sessionID, err := h.completer.CompleteAgentRun(r.Context(), agentSessionID, token, payload.ExitError)
	switch {
	case errors.Is(err, plugin.ErrAgentRunNotFound):
		// Idempotent no-op: a "this run is done" ping for a run the daemon no
		// longer tracks (already completed, or forgotten across a restart) is
		// success, not failure. Returning 200 here is what keeps a stale Stop
		// hook from surfacing as a "Stop hook error" line in the transcript.
		// A genuine auth failure still falls through to the 401 branch below,
		// so a rotated token is NOT silently swallowed.
		w.WriteHeader(http.StatusOK)
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

// handleQuestion implements POST /hooks/question/{agent_session_id} (BOS-485).
//
// It records a structured "a question is pending" signal that lets an explicit
// agent-emitted event (the Claude Code Notification hook) drive
// CHAT_STATUS_QUESTION instead of the regex pane scraper. The signal is a hint:
// the poller clears it when the user has answered, and the store ages it out
// via TTL, so nothing here needs to reconcile.
//
// The Notification hook fires on EVERY notification type (it is installed with
// an empty matcher — see bossdNotificationEntry — because Claude's Notification
// event filters its matcher by type). So this handler classifies by the
// notification_type in the forwarded payload and records a pending signal only
// for types that mean the human is needed (needsHumanNotification). Benign
// types (auth_success on login, idle_prompt, a normal turn-end) are ACKed with
// 200 but leave the store untouched — otherwise a login would flip the chat to
// "? question". An unrecognised type is logged at info (so a future Claude
// type, e.g. the AskUserQuestion selector's, self-reports for the allowlist)
// and, conservatively, not treated as a question.
//
// Auth mirrors the run-scoped hooks: the Notification hook carries the same
// per-run bearer token as the run's Stop hook, validated in constant time by
// the RunTokenValidator with no side effect.
//
// BOS-486 adds the inverse, additively: a payload that both names a recognised
// end-of-turn type and sets cleared:true (the opencode hook's
// {"notification_type":"session_idle","cleared":true} on session.idle) CLEARS
// the pending signal. No Claude notification type sets that flag, so every type
// the receiver already handled keeps its exact prior behaviour.
//
// Response codes:
//   - 400 — agent_session_id path parameter missing
//   - 401 — Authorization header missing/malformed, or the token mismatches a
//     registered run (can't authenticate ⇒ no store write)
//   - 500 — question deps not wired (misconfiguration; production always wires them)
//   - 200 — auth succeeded (the pending signal is recorded only for a
//     needs-human notification_type, and cleared only for an explicitly
//     clearing payload; benign/unknown types ACK without touching the store),
//     OR the run is unknown/torn-down (idempotent no-op, no store write) —
//     mirroring handleAgentRunComplete so a stale Notification hook's `curl -sf`
//     doesn't surface as a hook-error line in the Claude transcript
func (h *HookServer) handleQuestion(w http.ResponseWriter, r *http.Request) {
	agentSessionID := r.PathValue("agent_session_id")
	if agentSessionID == "" {
		http.Error(w, "agent_session_id required", http.StatusBadRequest)
		return
	}

	if h.questionSignals == nil || h.questionAuth == nil {
		// Defense-in-depth: the daemon entrypoint and test harness wire both.
		// Surface a clear 500 rather than silently dropping the signal so a
		// wiring regression is loud.
		h.logger.Error().Str("agent_session", agentSessionID).Msg("hook: question signal deps not configured")
		http.Error(w, "question signal not configured", http.StatusInternalServerError)
		return
	}

	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: question auth missing")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch err := h.questionAuth.ValidateRunToken(r.Context(), agentSessionID, token); {
	case err == nil:
		h.recordQuestionByType(agentSessionID, r)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, plugin.ErrAgentRunNotFound):
		// Idempotent no-op, mirroring handleAgentRunComplete: a Notification for
		// a run the daemon no longer tracks (already completed, or forgotten
		// across a restart) is not an error. The Notification hook carries the
		// same per-run token with the same lifetime as the Stop hook, so it
		// shares the identical post-teardown race window; returning 200 keeps a
		// stale Notification's `curl -sf` from surfacing as a hook-error line in
		// the Claude transcript. No positive auth ⇒ the store is left
		// un-written, so the "no write without auth" invariant still holds.
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, plugin.ErrAuthMismatch):
		// A registered run with the wrong token is a genuine auth failure (a
		// rotated token, or a request aimed at the wrong daemon); surface it as
		// 401 rather than swallow it, same as the sibling handler.
		h.logger.Warn().Str("agent_session", agentSessionID).Msg("hook: question auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	default:
		h.logger.Error().Err(err).Str("agent_session", agentSessionID).Msg("hook: question auth error")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// maxPromptSubmittedBody caps how much of the forwarded UserPromptSubmit
// payload is read. The payload carries the whole prompt, so the cap is
// generous; a larger body is read only up to it, fails to parse, and is then
// attributed by URL alone, exactly like a payload without a session_id.
const maxPromptSubmittedBody = 8 << 20

// promptSubmittedPayload is the part of Claude's UserPromptSubmit hook payload
// the receipt reads. The prompt it also carries is never logged.
type promptSubmittedPayload struct {
	SessionID string `json:"session_id"`
}

// handlePromptSubmitted implements POST /hooks/prompt-submitted/{agent_session_id},
// the first-input delivery receipt posted by Claude's UserPromptSubmit hook.
//
// It requires "Authorization: Bearer <token>", where token is the per-chat
// receipt token bossd derives (PromptReceiptRecorder.ValidPromptReceiptToken)
// and hands the runner in BuildInteractiveCommandRequest.hook_receipt_token, so
// a local process that can see a chat id cannot forge its delivery receipt. The
// token is checked before the body is read. A stale runner that sends no header
// gets 401 and its launch stays unconfirmed, the same degraded path a failed
// hook install takes. A receipt is kept only while a launch is confirming that
// very chat. Every Claude process in a worktree fires every hook entry in its
// settings, so a payload whose session_id names a different chat is
// acknowledged and dropped.
//
// Response codes:
//   - 400 — agent_session_id path parameter missing
//   - 401 — Authorization header missing/malformed or the token is wrong
//   - 500 — receipt recorder not wired (misconfiguration)
//   - 200 — everything else, recorded or not: the hook discards the response,
//     and a receipt nobody awaits is not an error
func (h *HookServer) handlePromptSubmitted(w http.ResponseWriter, r *http.Request) {
	at := time.Now()
	agentSessionID := r.PathValue("agent_session_id")
	if agentSessionID == "" {
		http.Error(w, "agent_session_id required", http.StatusBadRequest)
		return
	}
	if h.promptReceipts == nil {
		h.logger.Error().Str("agent_session", agentSessionID).Msg("hook: prompt receipt recorder not configured")
		http.Error(w, "prompt receipts not configured", http.StatusInternalServerError)
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || !h.promptReceipts.ValidPromptReceiptToken(agentSessionID, token) {
		h.logger.Debug().Str("agent_session", agentSessionID).Msg("hook: prompt-submitted auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var payload promptSubmittedPayload
	if body, err := io.ReadAll(io.LimitReader(r.Body, maxPromptSubmittedBody)); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &payload)
	}
	if payload.SessionID != "" && payload.SessionID != agentSessionID {
		h.logger.Debug().
			Str("agent_session", agentSessionID).
			Str("payload_session", payload.SessionID).
			Msg("hook: prompt-submitted payload names another chat; ignored")
		w.WriteHeader(http.StatusOK)
		return
	}
	// The main loop is running again, so the last turn's background-agent
	// report no longer describes the chat; the turn's own Stop reports afresh.
	// A background subagent reporting back arrives as a prompt too.
	if h.backgroundAgents != nil {
		h.backgroundAgents.SetBackgroundAgents(agentSessionID, 0)
	}
	recorded := h.promptReceipts.RecordPromptSubmitted(agentSessionID, at)
	h.logger.Debug().
		Str("agent_session", agentSessionID).
		Bool("awaited", recorded).
		Msg("hook: prompt-submitted receipt")
	w.WriteHeader(http.StatusOK)
}

// maxTurnEndedBody caps how much of the forwarded Stop payload is read. It
// carries the turn's last assistant message, so the cap is generous; a larger
// body fails to parse and records nothing.
const maxTurnEndedBody = 8 << 20

// turnEndedPayload is the part of Claude's Stop hook payload bossd reads.
type turnEndedPayload struct {
	SessionID       string `json:"session_id"`
	BackgroundTasks []struct {
		Type string `json:"type"`
	} `json:"background_tasks"`
}

// backgroundSubagents counts the payload's running background subagents.
// Shells and monitors are already read off the pane ("N shell still running");
// only a subagent leaves the pane looking idle.
func (p turnEndedPayload) backgroundSubagents() int {
	n := 0
	for _, task := range p.BackgroundTasks {
		if task.Type == "subagent" {
			n++
		}
	}
	return n
}

// handleTurnEnded implements POST /hooks/turn-ended/{agent_session_id}, posted
// by the Claude Stop hook bossd installs with the prompt receipt. It records how
// many background subagents the finished turn left running, so the tmux poller
// keeps a chat whose main loop sits idle at its prompt over a running subagent
// WORKING (and an armed callback cannot then turn it into a waiting "✓ ready").
//
// Auth is the prompt-receipt token. Every Claude process in a worktree fires
// every chat's entry, so a payload naming another chat is ignored, as the
// receipt does. An unreadable or unparsable body records nothing: the chat
// falls back to what its pane shows.
//
// Response codes:
//   - 400 — agent_session_id path parameter missing
//   - 401 — Authorization header missing/malformed or the token is wrong
//   - 200 — everything else; the hook discards the response
func (h *HookServer) handleTurnEnded(w http.ResponseWriter, r *http.Request) {
	agentSessionID := r.PathValue("agent_session_id")
	if agentSessionID == "" {
		http.Error(w, "agent_session_id required", http.StatusBadRequest)
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || h.promptReceipts == nil || !h.promptReceipts.ValidPromptReceiptToken(agentSessionID, token) {
		h.logger.Debug().Str("agent_session", agentSessionID).Msg("hook: turn-ended auth failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTurnEndedBody))
	var payload turnEndedPayload
	if err != nil || len(body) == 0 || json.Unmarshal(body, &payload) != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if payload.SessionID != "" && payload.SessionID != agentSessionID {
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.backgroundAgents != nil {
		h.backgroundAgents.SetBackgroundAgents(agentSessionID, payload.backgroundSubagents())
	}
	w.WriteHeader(http.StatusOK)
}

// maxNotificationBody caps how much of the forwarded Notification payload we
// read. Claude notification payloads are a few hundred bytes; 64 KiB is a
// generous ceiling that still bounds a malformed/hostile body.
const maxNotificationBody = 64 << 10

// recordQuestionByType reads the forwarded notification payload, and records a
// pending question signal only when its notification_type means the human is
// needed. It is called after the run token has authenticated, so a store write
// (or clear) here always follows positive auth.
//
// The body is best-effort: an empty/unreadable/unparsable payload yields an
// empty type, which is neither needs-human nor known-benign, so it takes the
// unclassified path (logged, no write) rather than guessing.
func (h *HookServer) recordQuestionByType(agentSessionID string, r *http.Request) {
	p := parseNotificationPayload(r)
	ntype, message := p.NotificationType, p.Message

	switch {
	case clearingNotification(ntype, p.Cleared):
		// The only path that clears. Checked FIRST so it can never be shadowed
		// by a future overlap with the classification arms below, and gated on
		// an explicit cleared:true so a bare notification of the same type
		// still takes the benign ACK-without-write path.
		h.questionSignals.Clear(agentSessionID)
		h.logger.Debug().
			Str("agent_session", agentSessionID).
			Str("notification_type", ntype).
			Msg("hook: question signal cleared")
	case needsHumanNotification(ntype):
		h.questionSignals.SetPending(agentSessionID, "claude-notification:"+ntype)
		h.logger.Debug().
			Str("agent_session", agentSessionID).
			Str("notification_type", ntype).
			Msg("hook: question signal recorded")
	case knownBenignNotification(ntype):
		// Expected non-question notification (login, idle, turn-end). ACK
		// without a write; Debug keeps the common case out of info logs.
		h.logger.Debug().
			Str("agent_session", agentSessionID).
			Str("notification_type", ntype).
			Msg("hook: notification ignored (not needs-human)")
	default:
		// Unrecognised (or missing) type. Don't treat it as a question — that
		// would risk false "? question" flips — but log loudly so a new Claude
		// notification type (e.g. the AskUserQuestion selector's) surfaces and
		// can be added to needsHumanNotification.
		h.logger.Info().
			Str("agent_session", agentSessionID).
			Str("notification_type", ntype).
			Str("message", message).
			Msg("hook: unclassified notification type; not treated as a question")
	}
}

// hookNotificationPayload is the forwarded hook body the classifier reads.
//
// notification_type + message are the Claude Code Notification hook's shape.
// cleared is the additive opt-in that the opencode event hook (BOS-486) sends
// on session.idle to retract a pending signal; Claude never sets it, so its
// absence keeps every existing type on exactly the path it took before.
type hookNotificationPayload struct {
	NotificationType string `json:"notification_type"`
	Message          string `json:"message"`
	Cleared          bool   `json:"cleared"`
}

// parseNotificationPayload extracts the forwarded notification body. Returns
// the zero value on any read/parse failure — the caller treats an empty type as
// unclassified, not an error.
func parseNotificationPayload(r *http.Request) hookNotificationPayload {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxNotificationBody))
	if err != nil || len(body) == 0 {
		return hookNotificationPayload{}
	}
	var p hookNotificationPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return hookNotificationPayload{}
	}
	return p
}

// clearingNotification reports whether this payload explicitly retracts a
// pending question signal.
//
// BOTH conditions are required: a recognised end-of-turn type AND cleared:true.
// The flag alone is not enough (an unrecognised type must never mutate the
// store), and the type alone is not enough (a bare session_idle takes the
// benign ACK-without-write path). No Claude notification type sets cleared, so
// this arm is unreachable for every type the receiver handled before BOS-486.
func clearingNotification(t string, cleared bool) bool {
	return cleared && t == "session_idle"
}

// needsHumanNotification reports whether a Claude Code Notification of this type
// means the agent is blocked waiting on the human (⇒ CHAT_STATUS_QUESTION).
//
// permission_prompt is empirically confirmed to fire on a tool-approval gate.
// agent_needs_input and elicitation_dialog are semantically "waiting on the
// human" and included so they're handled correctly if/when they fire; if they
// never do in bossd panes, no harm. Benign/informational types are excluded on
// purpose (see knownBenignNotification) so a login or turn-end can't raise a
// question.
func needsHumanNotification(t string) bool {
	switch t {
	case "permission_prompt", "agent_needs_input", "elicitation_dialog":
		return true
	default:
		return false
	}
}

// knownBenignNotification reports whether a Notification type is a recognised
// non-question event — one we deliberately ACK without recording a signal, and
// without the info-level "unclassified" log. Anything neither needs-human nor
// benign is genuinely unrecognised and worth surfacing.
//
// session_idle is the opencode end-of-turn event (BOS-486). It is benign here
// so a payload that names it WITHOUT cleared:true is a plain ACK rather than an
// "unclassified" log line; the clearing variant is handled earlier, by
// clearingNotification.
func knownBenignNotification(t string) bool {
	switch t {
	case "auth_success", "idle_prompt", "agent_completed",
		"elicitation_complete", "elicitation_response", "session_idle":
		return true
	default:
		return false
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
