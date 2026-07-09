package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
)

// claudeOAuthTokenEnvKey is the env var the credential materializer returns for
// a Claude account (mirrors the claude plugin's CLAUDE_CODE_OAUTH_TOKEN). The
// failover proxy reads the next account's bearer from this key to rewrite the
// Authorization header. The value is a secret and is NEVER logged.
const claudeOAuthTokenEnvKey = "CLAUDE_CODE_OAUTH_TOKEN"

// SentinelAPIKey is a FIXED, non-credential marker injected as ANTHROPIC_API_KEY
// for proxied Claude sessions (BOS-326). The interactive Claude REPL ignores
// CLAUDE_CODE_OAUTH_TOKEN and authenticates only from the keychain unless an
// ANTHROPIC_API_KEY is present; setting this key overrides the keychain and
// forces every request through ANTHROPIC_BASE_URL (the loopback failover proxy)
// as an x-api-key request. The proxy then strips it and substitutes the bound
// account's OAuth bearer (CurrentBearer), so the sentinel never reaches upstream
// and billing stays on the subscription. Its last 20 characters must match the
// pre-approved entry seeded into ~/.claude.json (see apikey_approval.go). It is
// NOT a secret — it authorizes nothing upstream — but treat it as opaque config.
const SentinelAPIKey = "sk-ant-boss-managed-rotation-sentinel-00000000000000"

// SentinelApprovalSuffix is the last-20-chars claude stores in ~/.claude.json's
// customApiKeyResponses.approved as the approval key for SentinelAPIKey.
func SentinelApprovalSuffix() string { return SentinelAPIKey[len(SentinelAPIKey)-20:] }

const proxyChatTargetPrefix = "chat\x00"

// ProxyTargetForChat returns the opaque failover-proxy target id for a tmux chat
// path token. fallbackAccountID is the account resolved at spawn time, used only
// before the chat binding has been durably persisted.
func ProxyTargetForChat(agentSessionID, fallbackAccountID string) string {
	return proxyChatTargetPrefix + agentSessionID + "\x00" + fallbackAccountID
}

func parseProxyChatTarget(targetID string) (agentSessionID, fallbackAccountID string, ok bool) {
	if !strings.HasPrefix(targetID, proxyChatTargetPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(targetID, proxyChatTargetPrefix)
	agentSessionID, fallbackAccountID, ok = strings.Cut(rest, "\x00")
	return agentSessionID, fallbackAccountID, ok && agentSessionID != ""
}

// proxyTokenRegistrar mints and registers the per-session path token used in
// the ANTHROPIC_BASE_URL the failover proxy injects. The concrete implementer
// is the loopback proxy server (server package); the session package depends
// only on this narrow interface so it never imports the server. The token is a
// secret and is never logged.
type proxyTokenRegistrar interface {
	// TokenForSession returns a stable, unguessable token bound to sessionID,
	// minting + registering one on first call and returning the same token on
	// subsequent calls. The token routes the Claude subprocess's loopback
	// requests back to the owning session.
	TokenForSession(sessionID string) string
	// TokenForChat returns a stable token for a tmux chat whose account binding
	// lives on agent_chats rather than sessions. fallbackAccountID is captured
	// for the small pre-persist window between spawn and UpdateAccountID.
	TokenForChat(sessionID, agentSessionID, fallbackAccountID string) string
	// ForgetBearer drops any sticky failover bearer cached for sessionID, so the
	// next request forwards the (freshly-respawned) subprocess's own bearer
	// instead of a stale swapped one. Called whenever a rebind respawns the
	// pane (manual or automatic switch), keeping the forwarded account in sync
	// with the just-persisted account_id. Safe to call for an unknown session.
	ForgetBearer(sessionID string)
}

// forgetProxyBearer clears the failover proxy's sticky bearer for a session
// after a rebind that respawns the pane, so the respawned subprocess's own
// (correct) bearer is used on its next request rather than a stale swapped one.
// No-op when no registrar is wired (proxy disabled).
func (l *Lifecycle) forgetProxyBearer(sessionID string) {
	if l.proxyRegistrar == nil {
		return
	}
	l.proxyRegistrar.ForgetBearer(sessionID)
}

// SetProxyPort records the S7 failover proxy's bound loopback port (BOS-320),
// stamped from the daemon entrypoint after the proxy server's Listen()
// succeeds. Zero (the default) is the liveness gate: no ANTHROPIC_BASE_URL
// overlay is injected while the port is unset, so sessions talk to
// api.anthropic.com directly.
func (l *Lifecycle) SetProxyPort(port int) { l.proxyPort = port }

// SetProxyRegistrar wires the per-session proxy-token registrar. Leaving it
// unset (nil) disables ANTHROPIC_BASE_URL injection entirely — the proxy is
// strictly additive: unwiring it just falls sessions back to the direct
// api.anthropic.com path.
func (l *Lifecycle) SetProxyRegistrar(r proxyTokenRegistrar) { l.proxyRegistrar = r }

// failoverProxyEnabled reports whether the failover proxy should inject for this
// daemon: account management must be on AND the proxy sub-flag on (default both
// on). Re-reads settings live per call so a flip takes effect without a daemon
// restart. Fails safe: a settings load error disables the proxy.
func (l *Lifecycle) failoverProxyEnabled() bool {
	cfg, ok := l.currentRotationConfig()
	return ok && cfg.ManagedAccountsEnabled() && cfg.FailoverProxyEnabled()
}

// proxyBaseURL returns the ANTHROPIC_BASE_URL the Claude subprocess should be
// pointed at, or "" when the proxy must not be used for this session. It is the
// single gate for injection and enforces every precondition:
//   - managed_accounts.enabled AND failover_proxy_enabled are both on (default
//     both true; see failoverProxyEnabled);
//   - the session is bound to a managed account;
//   - the proxy has bound (liveness gate: proxyPort != 0 and a registrar wired);
//   - the session runs the Claude agent — only Claude honors ANTHROPIC_BASE_URL
//     under an OAuth bearer (proven by the BOS-319 spike); injecting it for a
//     Codex session would break that session.
//
// SCOPE BOUNDARY (S7): this gate is reached from Lifecycle.resolveAccountEnv
// and server-driven attach/wake spawns that already resolved a materialized
// Claude bearer. Headless repair-run plugin spawns do not fold in this overlay.
//
// Never logs the token.
func (l *Lifecycle) proxyBaseURL(sess *models.Session) string {
	if sess == nil || l.proxyRegistrar == nil || l.proxyPort == 0 {
		return ""
	}
	if sess.AccountID == nil || *sess.AccountID == "" {
		return ""
	}
	if sess.AgentName != string(models.AccountProviderClaude) {
		return ""
	}
	if !l.failoverProxyEnabled() {
		return ""
	}
	token := l.proxyRegistrar.TokenForSession(sess.ID)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/s/%s", l.proxyPort, token)
}

func (l *Lifecycle) proxyBaseURLForChat(sess *models.Session, chat *models.AgentChat, accountID string) string {
	if sess == nil || chat == nil || l.proxyRegistrar == nil || l.proxyPort == 0 {
		return ""
	}
	if accountID == "" || chat.AgentSessionID == "" {
		return ""
	}
	agentName := chat.AgentName
	if agentName == "" {
		agentName = string(models.AccountProviderClaude)
	}
	if agentName != string(models.AccountProviderClaude) {
		return ""
	}
	if !l.failoverProxyEnabled() {
		return ""
	}
	token := l.proxyRegistrar.TokenForChat(sess.ID, chat.AgentSessionID, accountID)
	if token == "" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/s/%s", l.proxyPort, token)
}

// CurrentBearer resolves the OAuth bearer of the session's CURRENTLY bound
// account for first-leg proxy translation (BOS-326). The interactive Claude REPL
// ignores CLAUDE_CODE_OAUTH_TOKEN and authenticates only from the sentinel
// ANTHROPIC_API_KEY, so the proxy must substitute the bound account's
// subscription bearer itself on the first leg. Fail-safe: it returns "" (no
// error) whenever the session is unbound or any seam is missing, so the proxy
// forwards the client's own header — today's behavior. It intentionally does
// not re-check live failover-proxy settings: resolveAccountEnv gates injection
// at process spawn, but an already-spawned Claude process keeps its frozen
// ANTHROPIC_BASE_URL + sentinel and still needs translation. It never logs the
// token.
func (l *Lifecycle) CurrentBearer(ctx context.Context, sessionID string) (string, error) {
	if agentSessionID, fallbackAccountID, ok := parseProxyChatTarget(sessionID); ok {
		return l.currentBearerForChat(ctx, agentSessionID, fallbackAccountID)
	}
	if l.sessions == nil || l.rotationBinding == nil ||
		l.accountMaterializer == nil || l.accountGetter == nil {
		return "", nil
	}
	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("current bearer: get session %s: %w", sessionID, err)
	}
	if sess == nil {
		return "", nil
	}
	b, bound, err := l.rotationBinding.CurrentBinding(ctx, sess)
	if err != nil {
		return "", fmt.Errorf("current bearer: current binding: %w", err)
	}
	if !bound || b.CappedAccountID == "" {
		return "", nil
	}
	return l.currentBearerForAccount(ctx, sessionID, b.CappedAccountID)
}

func (l *Lifecycle) currentBearerForChat(ctx context.Context, agentSessionID, fallbackAccountID string) (string, error) {
	if l.accountMaterializer == nil || l.accountGetter == nil {
		return "", nil
	}
	b, ok, err := l.chatProxyBinding(ctx, agentSessionID, fallbackAccountID)
	if err != nil {
		return "", fmt.Errorf("current bearer: %w", err)
	}
	if !ok {
		return "", nil
	}
	return l.currentBearerForAccount(ctx, agentSessionID, b.AccountID)
}

func (l *Lifecycle) currentBearerForAccount(ctx context.Context, logID, accountID string) (string, error) {
	acct, err := l.accountGetter(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("current bearer: get account %s: %w", accountID, err)
	}
	if acct == nil {
		return "", nil
	}
	envOverlay, err := l.accountMaterializer.Materialize(ctx, acct)
	if err != nil {
		// Materialize failure ⇒ fail-safe: return "" so the proxy forwards the
		// client's own header. For an interactive REPL that header is the sentinel
		// x-api-key, which upstream rejects (401) → the proxy's 429/401 failover
		// path then rotates to a healthy account. Self-healing, no mis-billing
		// (the sentinel authorizes nothing upstream); never blocks on this account.
		l.logger.Warn().Err(err).Str("session", logID).Str("bound_account_id", accountID).
			Msg("failover proxy: materialize bound account failed; forwarding client header")
		return "", nil
	}
	return envOverlay[claudeOAuthTokenEnvKey], nil
}

// RebindAndAuditParams gathers the inputs for RebindAndAudit.
type RebindAndAuditParams struct {
	SessionID     string
	ChatID        string // agent_session_id; "" for the proxy path (no single chat)
	Provider      string
	Trigger       string // rotation trigger enum name, e.g. ROTATION_TRIGGER_MANUAL
	FromAccount   string
	BindAccountID string // account id persisted as the session→account binding
	ToAccount     string // audit ToAccount value (Label for manual, ID for auto/proxy)
	Outcome       string // rotation outcome enum name, e.g. ROTATION_OUTCOME_ROTATED
	Detail        string
}

// RebindAndAudit persists the session→account rebind and records exactly one
// rotation AuditEvent — the shared "session N is now bound to account M,
// audited as a rotation" primitive, with NO pane stop/respawn. Both the manual
// SwitchAccount path and the failover proxy call it, so the binding write and
// the AuditEvent shape have a single source of truth. A nil binding lazily
// defaults to the db-backed one. Never logs credential values.
func (l *Lifecycle) RebindAndAudit(ctx context.Context, binding accountBinding, p RebindAndAuditParams) error {
	if binding == nil {
		binding = sessionAccountBinding{sessions: l.sessions}
	}
	if err := binding.BindSessionAccount(ctx, p.SessionID, p.BindAccountID); err != nil {
		return fmt.Errorf("rebind session %s to account %q: %w", p.SessionID, p.BindAccountID, err)
	}
	// Recorder.Record is nil-receiver safe and swallows insert errors, so an
	// unwired recorder or a failed audit never fails the rebind.
	l.rotationRecorder.Record(ctx, rotation.AuditEvent{
		SessionID:   p.SessionID,
		ChatID:      p.ChatID,
		Provider:    p.Provider,
		Trigger:     p.Trigger,
		FromAccount: p.FromAccount,
		ToAccount:   p.ToAccount,
		Outcome:     p.Outcome,
		Detail:      p.Detail,
	})
	return nil
}

// FailoverResult carries a proxy failover decision from PrepareFailover to the
// replay + CommitFailover. Token is the next account's bearer — a SECRET that
// must never be logged; it lives only long enough for the proxy to rewrite the
// Authorization header.
type FailoverResult struct {
	Rotate        bool   // false ⇒ pass the original upstream response through unchanged
	Token         string // next account's CLAUDE_CODE_OAUTH_TOKEN bearer (secret)
	NextAccountID string
	FromAccountID string
	Provider      string
	Trigger       string // ROTATION_TRIGGER_USAGE_LIMITED (429) | ROTATION_TRIGGER_AUTH_INVALIDATED (401)
}

type chatProxyBinding struct {
	SessionID      string
	AgentSessionID string
	AccountID      string
	Provider       string
	BindSession    bool
}

func proxyAgentName(agentName string) string {
	if agentName == "" {
		return string(models.AccountProviderClaude)
	}
	return agentName
}

func (l *Lifecycle) chatProxyBinding(ctx context.Context, agentSessionID, fallbackAccountID string) (chatProxyBinding, bool, error) {
	if l.agentChats == nil {
		return chatProxyBinding{}, false, nil
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		if errors.Is(err, db.ErrAgentChatNotFound) {
			return chatProxyBinding{}, false, nil
		}
		return chatProxyBinding{}, false, fmt.Errorf("get chat %s: %w", agentSessionID, err)
	}
	if chat == nil {
		return chatProxyBinding{}, false, nil
	}

	var sess *models.Session
	if l.sessions != nil && chat.SessionID != "" {
		sess, err = l.sessions.Get(ctx, chat.SessionID)
		if err != nil {
			return chatProxyBinding{}, false, fmt.Errorf("get session %s for chat %s: %w", chat.SessionID, agentSessionID, err)
		}
	}

	provider := proxyAgentName(chat.AgentName)
	accountID := fallbackAccountID
	bindSession := false
	switch {
	case chat.AccountID != nil:
		accountID = *chat.AccountID
	case sess != nil && proxyAgentName(sess.AgentName) == provider:
		bindSession = true
		if sess.AccountID != nil {
			accountID = *sess.AccountID
		}
	}
	if accountID == "" {
		return chatProxyBinding{}, false, nil
	}
	return chatProxyBinding{
		SessionID:      chat.SessionID,
		AgentSessionID: agentSessionID,
		AccountID:      accountID,
		Provider:       provider,
		BindSession:    bindSession,
	}, true, nil
}

// PrepareFailover consults the rotation engine for a session whose upstream
// request returned status (429 or 401), materializes the next account's bearer
// on an OutcomeRotate, and returns it in a FailoverResult. It does NOT persist
// the rebind — that happens in CommitFailover only after a successful replay,
// so sessions.account_id reflects the account that actually served the request.
//
// It is fail-safe throughout: a non-429/401 status, the flag off, a missing
// rotation seam, an unbound session, no rotation target, or a materialize
// failure all return Rotate=false so the proxy passes the ORIGINAL response
// through unchanged (never fabricates one). The returned error is informational
// (the proxy logs it, redaction-safe, and still passes through).
func (l *Lifecycle) PrepareFailover(ctx context.Context, sessionID string, status int) (FailoverResult, error) {
	var kind rotation.SignalKind
	var trigger string
	switch status {
	case http.StatusTooManyRequests:
		kind, trigger = rotation.UsageLimited, "ROTATION_TRIGGER_USAGE_LIMITED"
	case http.StatusUnauthorized:
		kind, trigger = rotation.AuthInvalidated, "ROTATION_TRIGGER_AUTH_INVALIDATED"
	default:
		return FailoverResult{}, nil
	}
	if !l.failoverProxyEnabled() {
		return FailoverResult{}, nil
	}
	if agentSessionID, fallbackAccountID, ok := parseProxyChatTarget(sessionID); ok {
		return l.prepareChatFailover(ctx, agentSessionID, fallbackAccountID, kind, trigger)
	}
	// Any missing seam degrades to today's direct behavior (fail-safe). The
	// upstream 429/401 is itself the authoritative signal, so — unlike the
	// headless usage-limit intercept — no confirm-before-cool probe is needed.
	if l.sessions == nil || l.rotationBinding == nil || l.rotationDecider == nil || l.accountMaterializer == nil {
		return FailoverResult{}, nil
	}
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: get session %s: %w", sessionID, err)
	}
	if session == nil {
		return FailoverResult{}, nil
	}
	b, bound, err := l.rotationBinding.CurrentBinding(ctx, session)
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: current binding: %w", err)
	}
	if !bound {
		return FailoverResult{}, nil
	}
	outcome, err := l.rotationDecider(ctx, rotation.Signal{
		Provider:        b.Provider,
		CappedAccountID: b.CappedAccountID,
		Kind:            kind,
		RotationCapable: b.RotationCapable,
	})
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: decide: %w", err)
	}
	if outcome.Kind != rotation.OutcomeRotate || outcome.NextAccount == nil {
		// No rotation target (all cooling / not rotation-capable): original
		// upstream response returned unchanged.
		return FailoverResult{}, nil
	}
	envOverlay, err := l.accountMaterializer.Materialize(ctx, outcome.NextAccount)
	if err != nil {
		// Materialize failure ⇒ original 429/401 returned unchanged.
		l.logger.Warn().Err(err).Str("session", sessionID).Str("next_account_id", outcome.NextAccount.ID).
			Msg("failover proxy: materialize next account failed; passing original response through")
		return FailoverResult{}, nil
	}
	token := envOverlay[claudeOAuthTokenEnvKey]
	if token == "" {
		return FailoverResult{}, nil
	}
	return FailoverResult{
		Rotate:        true,
		Token:         token,
		NextAccountID: outcome.NextAccount.ID,
		FromAccountID: b.CappedAccountID,
		Provider:      b.Provider,
		Trigger:       trigger,
	}, nil
}

func (l *Lifecycle) prepareChatFailover(ctx context.Context, agentSessionID, fallbackAccountID string, kind rotation.SignalKind, trigger string) (FailoverResult, error) {
	if l.rotationBinding == nil || l.rotationDecider == nil || l.accountMaterializer == nil {
		return FailoverResult{}, nil
	}
	cb, ok, err := l.chatProxyBinding(ctx, agentSessionID, fallbackAccountID)
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: chat binding: %w", err)
	}
	if !ok {
		return FailoverResult{}, nil
	}
	accountID := cb.AccountID
	sess := &models.Session{ID: cb.SessionID, AgentName: cb.Provider, AccountID: &accountID}
	b, bound, err := l.rotationBinding.CurrentBinding(ctx, sess)
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: chat current binding: %w", err)
	}
	if !bound {
		return FailoverResult{}, nil
	}
	outcome, err := l.rotationDecider(ctx, rotation.Signal{
		Provider:        b.Provider,
		CappedAccountID: b.CappedAccountID,
		Kind:            kind,
		RotationCapable: b.RotationCapable,
	})
	if err != nil {
		return FailoverResult{}, fmt.Errorf("failover: decide chat: %w", err)
	}
	if outcome.Kind != rotation.OutcomeRotate || outcome.NextAccount == nil {
		return FailoverResult{}, nil
	}
	envOverlay, err := l.accountMaterializer.Materialize(ctx, outcome.NextAccount)
	if err != nil {
		l.logger.Warn().Err(err).Str("agent_session_id", agentSessionID).Str("next_account_id", outcome.NextAccount.ID).
			Msg("failover proxy: materialize next chat account failed; passing original response through")
		return FailoverResult{}, nil
	}
	token := envOverlay[claudeOAuthTokenEnvKey]
	if token == "" {
		return FailoverResult{}, nil
	}
	return FailoverResult{
		Rotate:        true,
		Token:         token,
		NextAccountID: outcome.NextAccount.ID,
		FromAccountID: b.CappedAccountID,
		Provider:      b.Provider,
		Trigger:       trigger,
	}, nil
}

// CommitFailover persists the session→account rebind and records the rotation
// audit AFTER the proxy has successfully replayed with the next account's
// bearer, so sessions.account_id reflects the account that served the request
// and exactly one AuditEvent is recorded. A non-rotating result is a no-op.
func (l *Lifecycle) CommitFailover(ctx context.Context, sessionID string, r FailoverResult) error {
	if !r.Rotate {
		return nil
	}
	if agentSessionID, fallbackAccountID, ok := parseProxyChatTarget(sessionID); ok {
		return l.commitChatFailover(ctx, agentSessionID, fallbackAccountID, r)
	}
	return l.RebindAndAudit(ctx, l.accountSwitchBinding, RebindAndAuditParams{
		SessionID:     sessionID,
		Provider:      r.Provider,
		Trigger:       r.Trigger,
		FromAccount:   r.FromAccountID,
		BindAccountID: r.NextAccountID,
		ToAccount:     r.NextAccountID,
		Outcome:       "ROTATION_OUTCOME_ROTATED",
		Detail:        "failover proxy: replayed with next account, no pane respawn",
	})
}

func (l *Lifecycle) commitChatFailover(ctx context.Context, agentSessionID, fallbackAccountID string, r FailoverResult) error {
	cb, ok, err := l.chatProxyBinding(ctx, agentSessionID, fallbackAccountID)
	if err != nil {
		return fmt.Errorf("commit chat failover: %w", err)
	}
	if !ok {
		return nil
	}
	if cb.BindSession {
		return l.RebindAndAudit(ctx, l.accountSwitchBinding, RebindAndAuditParams{
			SessionID:     cb.SessionID,
			ChatID:        agentSessionID,
			Provider:      r.Provider,
			Trigger:       r.Trigger,
			FromAccount:   r.FromAccountID,
			BindAccountID: r.NextAccountID,
			ToAccount:     r.NextAccountID,
			Outcome:       "ROTATION_OUTCOME_ROTATED",
			Detail:        "failover proxy: replayed with next account, no pane respawn",
		})
	}
	if l.agentChats == nil {
		return nil
	}
	accountID := r.NextAccountID
	if err := l.agentChats.UpdateAccountIDByAgentSessionID(ctx, agentSessionID, &accountID); err != nil {
		return fmt.Errorf("commit chat failover: bind chat %s to account %q: %w", agentSessionID, accountID, err)
	}
	l.rotationRecorder.Record(ctx, rotation.AuditEvent{
		SessionID:   cb.SessionID,
		ChatID:      agentSessionID,
		Provider:    r.Provider,
		Trigger:     r.Trigger,
		FromAccount: r.FromAccountID,
		ToAccount:   r.NextAccountID,
		Outcome:     "ROTATION_OUTCOME_ROTATED",
		Detail:      "failover proxy: replayed with next account, no pane respawn",
	})
	return nil
}
