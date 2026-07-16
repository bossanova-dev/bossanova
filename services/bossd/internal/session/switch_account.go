package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
)

// AccountStatus is the switch-relevant state of a target account, mapped from
// the registry's models.Account view. Only Active is switchable; Disabled,
// Failed, and Cooling are refused with a typed error before any pane is touched.
type AccountStatus int

const (
	// AccountActive is a healthy, selectable account.
	AccountActive AccountStatus = iota
	// AccountDisabled is an operator-disabled account — never switchable.
	AccountDisabled
	// AccountCooling is an account inside its rotation cooldown window.
	AccountCooling
	// AccountFailed is an account whose last-known health check failed — the
	// rotation engine sidelines it and session creation refuses to bind it, so a
	// manual switch refuses it too (mirroring checkAccountEligible).
	AccountFailed
)

// switchAccount is the switch's internal, provider-agnostic view of a target
// account, projected from models.Account by the registry adapter (or supplied
// directly by a test stub). It carries just what SwitchAccount needs to
// validate the target and label the outcome.
type switchAccount struct {
	ID            string
	Provider      string // "claude" | "codex"; "" for the system-default account 0
	Label         string
	Status        AccountStatus
	CooldownUntil *time.Time // non-nil & future ⇒ cooling (also reflected in Status)
}

// accountBinding is the narrow BOS-170 session→account binding contract the
// switch consumes: read the session's current account and rebind it to a new
// one. The real implementation wraps the session store; tests inject a stub.
type accountBinding interface {
	SessionAccount(ctx context.Context, sessionID string) (string, error)
	BindSessionAccount(ctx context.Context, sessionID, accountID string) error
}

// accountRegistry resolves a target account id to the switch's internal view.
// The real implementation wraps db.AccountStore; tests inject a stub.
type accountRegistry interface {
	Account(ctx context.Context, accountID string) (switchAccount, error)
}

// chatStatusReader reports whether a chat's agent is mid-turn (WORKING). The
// real implementation adapts *status.Tracker (held by the server, not the
// Lifecycle) via ChatWorkingFunc; tests inject a stub. A nil reader is treated
// as "not working" so the switch never blocks on missing status wiring.
type chatStatusReader interface {
	IsWorking(agentSessionID string) bool
}

// transcriptProbe reports whether an on-disk transcript exists for a resume id,
// so the switch can fall back to a fresh chat rather than asking the agent CLI
// to resume against a transcript that isn't there. The real implementation
// forwards to the agent plugin's TranscriptExists RPC; tests inject a stub.
type transcriptProbe interface {
	TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool
}

// resumeFeasibleCrossAccount reports whether the given provider can resume a
// prior conversation under a DIFFERENT account. Spike-1.1 (BOS-158) PROVED
// cross-account resume works for BOTH CLIs via client-side replay, so the
// conservative default returns true. Kept as an overridable package var so the
// per-provider capability can later be driven by BOS-169's RotationCapability
// probe, and so tests can force the fresh-fallback branch.
var resumeFeasibleCrossAccount = func(provider string) bool {
	_ = provider
	return true // TODO(BOS-169 RotationCapability): drive per-provider from the plugin probe.
}

// SwitchAccountParams is the input to a manual account switch.
type SwitchAccountParams struct {
	SessionID       string
	AgentSessionID  string
	TargetAccountID string // "" = system-default account 0
	// Force interrupts a mid-turn (WORKING) chat instead of refusing. Set from
	// an explicit operator confirmation.
	Force bool
	// Auto marks the automatic rotation path (BOS-175): the switch was triggered
	// by the ChatRotator on a CHAT_STATUS_LIMITED transition, not an operator. It
	// renders the in-chat notice via AutoRotateNotice with PreviousResetAt. It has
	// no operator to confirm a mid-turn interrupt, so it can never set Force — and
	// it still honors the WORKING guard below: a chat that recovered to WORKING in
	// the window between the rotator's LIMITED re-check and this switch is left
	// untouched rather than having its live turn killed.
	Auto bool
	// PreviousResetAt is the parsed usage-limit reset time of the account being
	// rotated away from, used only on the Auto path for the notice wording. Zero
	// when the banner carried no parseable reset time.
	PreviousResetAt time.Time
}

// SwitchAccountResult reports the outcome of a manual account switch.
type SwitchAccountResult struct {
	// Resumed is true when the chat came back up resuming the prior
	// conversation, false when it started fresh (or on a no-op).
	Resumed bool
	// TargetLabel is the human-friendly label of the account switched to.
	TargetLabel string
	// NoticeText is the single-line, user-facing summary of what happened.
	NoticeText string
}

// Typed errors surfaced by SwitchAccount so callers (server/CLI/MCP/TUI/web)
// can map them to the right status and messaging.
var (
	// ErrAccountDisabled means the target account is operator-disabled.
	ErrAccountDisabled = errors.New("target account is disabled")
	// ErrAccountCooling means the target account is inside its rotation cooldown.
	ErrAccountCooling = errors.New("target account is cooling")
	// ErrAccountFailed means the target account's last-known health check failed.
	ErrAccountFailed = errors.New("target account failed its last health check")
	// ErrChatMidTurn means the chat is WORKING and Force was not set.
	ErrChatMidTurn = errors.New("chat is mid-turn")
)

// SetAccountSwitchDeps wires the production account-switch adapters onto the
// Lifecycle without changing NewLifecycle's signature. The session→account
// binding defaults lazily from the Lifecycle's own session store, so only the
// registry (account metadata), the mid-turn status reader (adapting the
// server-held status.Tracker), and the transcript probe (agent plugin clients)
// need injecting. Any of them may be left nil in older/test wiring:
//   - nil registry ⇒ SwitchAccount errors (it cannot validate the target).
//   - nil status reader ⇒ mid-turn is treated as not-working (no guard).
//   - nil transcript probe ⇒ a present resume id is assumed resumable.
func (l *Lifecycle) SetAccountSwitchDeps(accounts db.AccountStore, clients map[string]agent.AgentRunnerClient, working ChatWorkingFunc) {
	l.accountSwitchRegistry = accountRegistryAdapter{store: accounts}
	l.accountSwitchTranscripts = transcriptProbeAdapter{clients: clients}
	if working != nil {
		l.accountSwitchWorking = working
	}
}

// SwitchAccount stops the session's chat, rebinds the session to the chosen
// account, and brings the chat back up under it — resuming the prior
// conversation when cross-account resume is feasible and a transcript exists,
// otherwise starting fresh. It reuses existing primitives (KillChatTmuxSession,
// the BOS-170 binding, StartTmuxChat) and never resends the interrupted prompt
// (D12).
//
// NOTE: the respawn is always interactive (tmux-hosted). A switch initiated
// against a headless run therefore promotes it to a tmux-hosted chat resumed
// under the new account — acceptable because the manual switch is an operator
// action. A headless-preserving respawn is a later seam.
// TODO(BOS-174): headless-preserving respawn via the rotateAndRestart path.
func (l *Lifecycle) SwitchAccount(ctx context.Context, p SwitchAccountParams) (SwitchAccountResult, error) {
	if l.sessions == nil || l.agentChats == nil {
		return SwitchAccountResult{}, fmt.Errorf("account switch: session/chat store not configured")
	}

	// 1. Load the session and its chat.
	session, err := l.sessions.Get(ctx, p.SessionID)
	if err != nil {
		return SwitchAccountResult{}, fmt.Errorf("get session %s: %w", p.SessionID, err)
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, p.AgentSessionID)
	if err != nil {
		return SwitchAccountResult{}, fmt.Errorf("get agent chat %s: %w", p.AgentSessionID, err)
	}
	if chat == nil {
		return SwitchAccountResult{}, fmt.Errorf("agent chat not found for agent_session_id %q", p.AgentSessionID)
	}

	// The switch binds the CHAT's account, not the session's, so a cross-agent
	// chat (e.g. a Codex chat inside a Claude session) switches without corrupting
	// the parent session's own binding — each chat carries its own provider-scoped
	// account. The default binding reads/writes chat.AccountID via the chat store;
	// tests inject l.accountSwitchBinding.
	binding := l.accountSwitchBinding
	if binding == nil {
		binding = chatAccountBinding{chats: l.agentChats, agentSessionID: p.AgentSessionID}
	}
	if l.accountSwitchRegistry == nil {
		return SwitchAccountResult{}, fmt.Errorf("account switch: account registry not configured")
	}

	// 2. Validate the target, then short-circuit a no-op switch.
	target, err := l.accountSwitchRegistry.Account(ctx, p.TargetAccountID)
	if err != nil {
		return SwitchAccountResult{}, fmt.Errorf("load target account %q: %w", p.TargetAccountID, err)
	}
	if target.Status == AccountDisabled {
		return SwitchAccountResult{}, fmt.Errorf("%w: account %q is disabled", ErrAccountDisabled, target.Label)
	}
	if target.Status == AccountFailed {
		return SwitchAccountResult{}, fmt.Errorf("%w: account %q", ErrAccountFailed, target.Label)
	}
	if target.Status == AccountCooling || (target.CooldownUntil != nil && target.CooldownUntil.After(l.switchNow())) {
		return SwitchAccountResult{}, fmt.Errorf("%w: account %q is cooling until %s",
			ErrAccountCooling, target.Label, coolingUntilText(target.CooldownUntil))
	}
	current, err := binding.SessionAccount(ctx, p.SessionID)
	if err != nil {
		return SwitchAccountResult{}, fmt.Errorf("read current session account: %w", err)
	}
	if current == p.TargetAccountID {
		// Already on the target: no stop, no rebind, no respawn.
		return SwitchAccountResult{
			TargetLabel: target.Label,
			NoticeText:  "already on " + target.Label,
		}, nil
	}

	// 3. Mid-turn guard: never interrupt a WORKING chat's in-progress turn. A
	// manual switch refuses unless the operator confirms with Force. The automatic
	// rotation path (Auto) has no operator to confirm, so it can never Force — but
	// it must still honor this guard rather than skip it: the rotator's LIMITED
	// re-check runs BEFORE the engine Decide (a DB transaction), leaving a narrow
	// window in which the chat can recover to WORKING before we reach this point.
	// Interrupting it here would kill a live turn whose prompt is never resent
	// (D12), contradicting the "recovered chat is not rotated" invariant. So a chat
	// that recovered to WORKING aborts the switch with ErrChatMidTurn — the rotator
	// treats a failed switch as fail-safe and leaves the chat as-is. No pane is
	// killed on refusal.
	if !p.Force && l.accountSwitchWorking != nil && l.accountSwitchWorking.IsWorking(p.AgentSessionID) {
		return SwitchAccountResult{}, fmt.Errorf("%w; confirm/--force to interrupt", ErrChatMidTurn)
	}

	// 4. Resume-vs-fresh. Provider comes from the target account; for the
	// system-default account 0 (empty provider) fall back to the CHAT's own agent
	// name — a cross-agent chat may run a different provider than its parent
	// session, so switching to/from account 0 must probe the chat's plugin — then
	// to the session's agent for legacy chats with an empty AgentName.
	provider := target.Provider
	if provider == "" {
		provider = chat.AgentName
		if provider == "" {
			provider = session.AgentName
		}
	}
	resumeID, hasResume := switchResumeID(chat)
	resumable := resumeFeasibleCrossAccount(provider) && hasResume &&
		l.switchTranscriptExists(ctx, chat.AgentName, session.WorktreePath, resumeID)

	// 5. STOP the current chat pane. A headless chat (no tmux name) is a no-op.
	if err := l.KillChatTmuxSession(ctx, p.SessionID, p.AgentSessionID); err != nil {
		return SwitchAccountResult{}, fmt.Errorf("stop chat before switch: %w", err)
	}

	// 6. Build the outcome. Never resend the interrupted prompt. The audit
	// Detail carries ONLY the outcome suffix ("resumed" / "started fresh (…)"):
	// the rotation history renders "<from> switched to <to> — <detail>" from
	// the structured From/To fields, so repeating "switched to <label>" in
	// Detail would force every consumer to string-strip the duplication back
	// out. The full sentence lives only in the operator-facing pane notice.
	outcomeDetail := "resumed"
	if !resumable {
		outcomeDetail = "started fresh (could not resume this conversation cross-account)"
	}
	notice := "switched to " + target.Label + " — " + outcomeDetail
	// The automatic rotation path (BOS-175) uses the D4 notice wording, which
	// carries the previous account's reset time; the fresh-start caveat is still
	// appended when cross-account resume was not feasible.
	if p.Auto {
		notice = AutoRotateNotice(target.Label, p.PreviousResetAt)
		if !resumable {
			notice += " (started fresh)"
		}
	}

	// 7. REBIND the session to the target account. StartTmuxChat below resolves
	// the new account's env overlay from this freshly-persisted binding. The
	// operator-driven path binds AND audits in one step via the shared
	// RebindAndAudit helper (also called by the S7 failover proxy, BOS-320), so
	// the binding write and AuditEvent shape have a single source of truth. The
	// automatic path (Auto) binds only: the ChatRotator already recorded the
	// audit at its decision point, so auditing here would double-record. The
	// manual audit is recorded here (on the rebind) rather than after the
	// respawn; a respawn failure below still leaves the session bound to the
	// target, so auditing the rebind reflects what actually happened.
	if p.Auto {
		if err := binding.BindSessionAccount(ctx, p.SessionID, p.TargetAccountID); err != nil {
			l.stampSwitchStartError(ctx, p.AgentSessionID, "rebind account failed: "+err.Error())
			return SwitchAccountResult{}, fmt.Errorf("rebind session %s to account %q: %w", p.SessionID, p.TargetAccountID, err)
		}
	} else if err := l.RebindAndAudit(ctx, binding, RebindAndAuditParams{
		SessionID: p.SessionID,
		ChatID:    p.AgentSessionID,
		Provider:  provider,
		Trigger:   "ROTATION_TRIGGER_MANUAL",
		// Audit the from-side as its human label (like ToAccount), not the raw
		// account id, so the TUI rotation history reads "<from> switched to <to>".
		// Degrades per resolveAccountLabel: an unresolvable id falls back to its
		// short-id display form, an empty id to the unmanaged-credentials label.
		FromAccount:   l.resolveAccountLabel(ctx, current),
		BindAccountID: p.TargetAccountID,
		ToAccount:     target.Label,
		Outcome:       "ROTATION_OUTCOME_ROTATED",
		Detail:        outcomeDetail,
	}); err != nil {
		l.stampSwitchStartError(ctx, p.AgentSessionID, "rebind account failed: "+err.Error())
		return SwitchAccountResult{}, err
	}

	// A switch respawns the pane under the target account's fresh creds, so any
	// sticky failover bearer the proxy cached for a PRIOR auto-failover must be
	// dropped — otherwise the proxy would keep forwarding the old swapped bearer
	// and silently override this explicit switch (desyncing the forwarded account
	// from the just-persisted account_id). No-op when the proxy is disabled.
	l.forgetProxyBearer(p.SessionID)

	// 8. RESPAWN the chat under the new account. Resume reuses the prior id
	// (StartTmuxChat adds --resume); a fresh switch mints a new id. Never a
	// prompt/command — the interrupted prompt is not resent (D12), so the zero
	// DeliveryPrefillOnly intent is correct.
	respawn := ChatInput{}
	if resumable {
		respawn.ResumeAgentSessionID = resumeID
	}
	// AllowSiblingChat bypasses StartTmuxChat's session-wide live-chat idempotency
	// check. The switch already killed the targeted pane and rebound the session;
	// this respawn must bring THAT chat back up. Without the bypass, a session with
	// another live sibling chat would trip the guard — StartTmuxChat would find the
	// sibling's pane and return AlreadyExists instead of respawning the just-killed
	// target, leaving a multi-chat session on the new account with the selected
	// chat stopped/failed.
	if _, err := l.StartTmuxChat(ctx, p.SessionID, respawn, chat.Title, HookOpts{AllowSiblingChat: true}); err != nil {
		l.stampSwitchStartError(ctx, p.AgentSessionID, "respawn after switch failed: "+err.Error())
		return SwitchAccountResult{}, fmt.Errorf("respawn chat after switch: %w", err)
	}

	return SwitchAccountResult{
		Resumed:     resumable,
		TargetLabel: target.Label,
		NoticeText:  notice,
	}, nil
}

// switchNow returns the switch's time source: the injected test clock when set,
// otherwise wall-clock time.
func (l *Lifecycle) switchNow() time.Time {
	if l.clock != nil {
		return l.clock()
	}
	return time.Now()
}

// switchTranscriptExists reports whether a transcript exists for the resume id.
// With no probe wired it degrades to "a present resume id is resumable", so
// unit tests without a probe still exercise the resume path.
func (l *Lifecycle) switchTranscriptExists(ctx context.Context, agentName, workDir, resumeID string) bool {
	if l.accountSwitchTranscripts == nil {
		return resumeID != ""
	}
	return l.accountSwitchTranscripts.TranscriptExists(ctx, agentName, workDir, resumeID)
}

// stampSwitchStartError best-effort marks the chat row start-failed so a switch
// that dies after STOP leaves a visible "(failed to start)" entry rather than a
// silently vanished chat. Errors are logged, never returned — the caller has its
// own error to surface.
func (l *Lifecycle) stampSwitchStartError(ctx context.Context, agentSessionID, reason string) {
	if l.agentChats == nil {
		return
	}
	if err := l.agentChats.MarkStartFailed(ctx, agentSessionID, reason); err != nil {
		l.logger.Warn().Err(err).
			Str("agentSessionID", agentSessionID).
			Str("reason", reason).
			Msg("account switch: failed to stamp chat start-error")
	}
}

// switchResumeID returns the id the respawn resumes under: the chat's LOGICAL
// agent_session_id. The bool reports whether any resumable id is present.
//
// It deliberately does NOT prefer chat.ProviderSessionID. Unlike spawnChatTmux
// (which keeps the boss key = chat.AgentSessionID and passes the provider id to
// the plugin ONLY as the --resume arg), StartTmuxChat reuses ResumeAgentSessionID
// as the boss correlation key — the agent_chats row, tmux session name, and log
// path all key on it. Passing a Codex provider id (which differs from the
// agent_session_id) would re-key the respawn onto the provider id: after the STOP
// cleared the original pane, StartTmuxChat would create/delete rows under the
// provider id and leave the originally selected logical chat stopped, surfacing a
// duplicate live chat and breaking callers still holding the original id. Keying
// on the logical id keeps the respawn bound to the selected chat. For Claude the
// provider session id equals the agent_session_id, so resume is unaffected; for a
// Codex chat whose provider id differs, the transcript probe (keyed by the
// provider rollout id) won't match the logical id, so the switch degrades to its
// documented fresh-start fallback — correctly keyed. Preserving Codex
// rollout-resume across a switch needs StartTmuxChat to separate the boss key
// from the provider resume id (a later seam).
func switchResumeID(chat *models.AgentChat) (string, bool) {
	if chat == nil || chat.AgentSessionID == "" {
		return "", false
	}
	return chat.AgentSessionID, true
}

// coolingUntilText renders a cooldown time for the ErrAccountCooling message,
// or "an unknown time" when the registry didn't carry one.
func coolingUntilText(until *time.Time) string {
	if until == nil {
		return "an unknown time"
	}
	return until.Format(time.RFC3339)
}

// --- Production adapters over BOS-170 / status / plugin seams ---

// sessionAccountBinding adapts db.SessionStore to the accountBinding seam:
// SessionAccount reads Session.AccountID; BindSessionAccount persists the
// rebind via UpdateSessionParams.AccountID (nullable-string double pointer,
// present-empty ⇒ system-default account 0).
type sessionAccountBinding struct {
	sessions db.SessionStore
}

func (b sessionAccountBinding) SessionAccount(ctx context.Context, sessionID string) (string, error) {
	s, err := b.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if s.AccountID == nil {
		return "", nil
	}
	return *s.AccountID, nil
}

func (b sessionAccountBinding) BindSessionAccount(ctx context.Context, sessionID, accountID string) error {
	acctPtr := &accountID
	_, err := b.sessions.Update(ctx, sessionID, db.UpdateSessionParams{AccountID: &acctPtr})
	return err
}

// chatAccountBinding adapts db.AgentChatStore to the accountBinding seam so the
// manual switch binds the CHAT's account (agent_chats.account_id) rather than
// the parent session's — the authority moved to the chat in BOS-381. It is
// scoped to a single agentSessionID at construction: the sessionID argument the
// accountBinding interface passes (RebindAndAudit forwards p.SessionID) is
// ignored, since the write targets the chat row directly via
// UpdateAccountIDByAgentSessionID. A present-empty account id (system-default
// account 0) persists as a non-nil pointer to "", mirroring the session binding.
type chatAccountBinding struct {
	chats          db.AgentChatStore
	agentSessionID string
}

func (b chatAccountBinding) SessionAccount(ctx context.Context, _ string) (string, error) {
	chat, err := b.chats.GetByAgentSessionID(ctx, b.agentSessionID)
	if err != nil {
		return "", err
	}
	if chat == nil || chat.AccountID == nil {
		return "", nil
	}
	return *chat.AccountID, nil
}

func (b chatAccountBinding) BindSessionAccount(ctx context.Context, _, accountID string) error {
	acctPtr := &accountID
	return b.chats.UpdateAccountIDByAgentSessionID(ctx, b.agentSessionID, acctPtr)
}

// accountRegistryAdapter adapts db.AccountStore to the accountRegistry seam,
// projecting models.Account into the switch's internal view. The empty account
// id resolves to the system-default account 0 (always active, no env).
type accountRegistryAdapter struct {
	store db.AccountStore
}

func (a accountRegistryAdapter) Account(ctx context.Context, accountID string) (switchAccount, error) {
	if accountID == "" {
		return switchAccount{ID: "", Label: account.UnmanagedLocalCredentialsLabel, Status: AccountActive}, nil
	}
	if a.store == nil {
		return switchAccount{}, fmt.Errorf("account store not configured")
	}
	acct, err := a.store.Get(ctx, accountID)
	if err != nil {
		return switchAccount{}, fmt.Errorf("get account %s: %w", accountID, err)
	}
	sa := switchAccount{
		ID:            acct.ID,
		Provider:      string(acct.Provider),
		Label:         acct.Label,
		Status:        AccountActive,
		CooldownUntil: acct.CooldownUntil,
	}
	if sa.Label == "" {
		// Shared degraded-label policy (account.ShortID) so a label-less
		// account renders the same everywhere it surfaces.
		sa.Label = account.ShortID(acct.ID)
	}
	// Mirror checkAccountEligible's selectability predicate (status/health/cooldown)
	// so a manual switch refuses exactly what the rotation engine and session
	// creation sideline. Disabled (operator intent) and Failed (bad health) take
	// precedence over Cooling for the surfaced error; all three are non-switchable.
	switch {
	case acct.Status == models.AccountStatusDisabled:
		sa.Status = AccountDisabled
	case acct.Health == models.AccountHealthFailed:
		sa.Status = AccountFailed
	case acct.CooldownUntil != nil && acct.CooldownUntil.After(time.Now()):
		sa.Status = AccountCooling
	}
	return sa, nil
}

// resolveAccountLabel resolves an account id to its human label for rotation
// audit fields on EITHER side (manual-switch/failover/headless-rotation
// FromAccount AND ToAccount call sites), mirroring how the manual switch's
// to-side (target.Label) is already stored. An empty id (unbound session)
// resolves to the unmanaged-credentials label — mapped here, not in the
// registry adapter, so the contract holds for every registry state (including
// nil) — and an unresolvable id degrades to its account.ShortID display form,
// so the result is never blank. Side semantics: an empty FROM-side id means
// unmanaged credentials and maps to that label; an empty TO-side means "no
// target" (status-only outcomes) and stays empty — callers guard it (see
// recordRotation) so this helper is never asked to label an
// intentionally-empty to-side.
//
// The degraded fallback is account.ShortID, deliberately matching
// account.Resolver.Label (which feeds the chat rotator's audit from-side via
// ChatContext.FromLabel): both audit-feeding resolvers must degrade an
// unresolvable id to the same string. The full id stays observable in the
// Debug log below.
func (l *Lifecycle) resolveAccountLabel(ctx context.Context, accountID string) string {
	if accountID == "" {
		return account.UnmanagedLocalCredentialsLabel
	}
	if l.accountSwitchRegistry == nil {
		return account.ShortID(accountID)
	}
	sa, err := l.accountSwitchRegistry.Account(ctx, accountID)
	if err != nil {
		l.logger.Debug().Err(err).Str("account_id", accountID).
			Msg("rotation audit: account label lookup failed; falling back to short id")
		return account.ShortID(accountID)
	}
	return sa.Label
}

// transcriptProbeAdapter dispatches TranscriptExists to the agent plugin
// matching agentName, mirroring the server's liveTranscriptOracle: an unknown
// agent or a missing client returns false (spawn fresh rather than guess).
type transcriptProbeAdapter struct {
	clients map[string]agent.AgentRunnerClient
}

func (o transcriptProbeAdapter) TranscriptExists(ctx context.Context, agentName, workDir, agentSessionID string) bool {
	name := agentName
	if name == "" {
		name = "claude"
	}
	if o.clients == nil {
		return false
	}
	client, ok := o.clients[name]
	if !ok || client == nil {
		return false
	}
	resp, err := client.TranscriptExists(ctx, &bossanovav1.TranscriptExistsRequest{
		WorkDir:        workDir,
		AgentSessionId: agentSessionID,
	})
	if err != nil || resp == nil {
		return false
	}
	return resp.GetExists()
}

// ChatWorkingFunc adapts a plain mid-turn predicate to the chatStatusReader
// seam so the daemon can wire the server-held status.Tracker without the
// session package importing the status package.
type ChatWorkingFunc func(agentSessionID string) bool

// IsWorking reports whether the chat's agent is mid-turn.
func (f ChatWorkingFunc) IsWorking(agentSessionID string) bool {
	if f == nil {
		return false
	}
	return f(agentSessionID)
}
