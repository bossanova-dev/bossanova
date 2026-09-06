package session

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/rs/zerolog"
)

// BOS-1135. A chat-scoped account switch kills and respawns the pane, but the
// respawn used to fold the failover-proxy overlay through the SESSION-scoped
// helper. The minted token's target resolved through sessions.account_id — the
// binding a chat-scoped switch deliberately never writes — while the overlay
// also strips the child's own CLAUDE_CODE_OAUTH_TOKEN, so the new pane had no
// route to the new account at all and kept billing the exhausted one.
//
// Every assertion below reads the environment bossd actually hands tmux, parsed
// back out of the recorded `tmux new-session -e K=V` argv. That is the layer the
// fix owns; which bearer a live upstream request carries can only be observed
// against a running daemon with two real accounts.

const switchProxyPort = 45671

// targetRecordingRegistrar is a proxyTokenRegistrar whose minted token ENCODES
// the resolution target it registered, so a test can tell a session-scoped token
// from a chat-scoped one by looking at the URL alone. The real registrar hides
// the target behind an opaque random token, which is exactly the distinction
// this ticket turns on.
type targetRecordingRegistrar struct {
	mu           sync.Mutex
	chatTargets  []string
	sessionCalls []string
}

func (r *targetRecordingRegistrar) TokenForSession(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionCalls = append(r.sessionCalls, sessionID)
	return "sessiontok-" + url.QueryEscape(sessionID)
}

func (r *targetRecordingRegistrar) TokenForChat(_, agentSessionID, fallbackAccountID string) string {
	target := ProxyTargetForChat(agentSessionID, fallbackAccountID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatTargets = append(r.chatTargets, target)
	return "chattok-" + url.QueryEscape(target)
}

func (r *targetRecordingRegistrar) ForgetBearer(string)                              {}
func (r *targetRecordingRegistrar) ForgetAllBearers()                                {}
func (r *targetRecordingRegistrar) AdoptToken(string, string)                        {}
func (r *targetRecordingRegistrar) AdoptTokenForChat(string, string, string, string) {}
func (r *targetRecordingRegistrar) RebuildTokenRegistry(context.Context) error       { return nil }

func (r *targetRecordingRegistrar) RetargetChatToken(_, _, _, _ string) {}

func (r *targetRecordingRegistrar) targets() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.chatTargets...)
}

// wantChatProxyURL is the ANTHROPIC_BASE_URL a CHAT-scoped spawn must carry.
func wantChatProxyURL(agentSessionID, accountID string) string {
	return "http://127.0.0.1:" + strconv.Itoa(switchProxyPort) + "/s/chattok-" +
		url.QueryEscape(ProxyTargetForChat(agentSessionID, accountID))
}

// wantSessionProxyURL is the ANTHROPIC_BASE_URL a SESSION-scoped spawn carries —
// the shape the ticket's site used to produce and the two session-authority
// sites must keep producing.
func wantSessionProxyURL(sessionID string) string {
	return "http://127.0.0.1:" + strconv.Itoa(switchProxyPort) + "/s/sessiontok-" + url.QueryEscape(sessionID)
}

// accountKeyedEnvResolver materializes a bearer NAMED for the account it was
// asked to resolve, so a test can prove the credentials came from the target
// account and not merely that some overlay was applied.
type accountKeyedEnvResolver struct{}

// A session with no bound account is not a failure: it is the unmanaged case,
// where there is nothing to inject and the spawn proceeds on the agent CLI's own
// login. Only a BOUND account that cannot be materialized refuses (BOS-1142),
// and no test here exercises that arm through this double, so the error is
// always nil.
func (accountKeyedEnvResolver) Resolve(_ context.Context, sess *models.Session) (map[string]string, error) {
	if sess == nil || sess.AccountID == nil || *sess.AccountID == "" {
		return nil, nil
	}
	return map[string]string{claudeOAuthTokenEnvKey: "bearer-for-" + *sess.AccountID}, nil
}

// idKeyedSwitchRegistry answers with the account that was actually requested, so
// a test can switch twice to two different targets.
type idKeyedSwitchRegistry struct{}

func (idKeyedSwitchRegistry) Account(_ context.Context, id string) (switchAccount, error) {
	return switchAccount{ID: id, Provider: "claude", Label: "Label " + id, Status: AccountActive}, nil
}

// switchStartAccount is the account both the session and its chat are bound to
// before every switch below — the realistic pre-switch state, in which the stale
// session binding is present and would silently win if the respawn's proxy
// target were still session-scoped.
const switchStartAccount = "acct-1"

// newProxySwitchHarness is newSwitchHarness with the failover proxy live and a
// target-recording registrar wired, plus both bindings seeded to
// switchStartAccount.
func newProxySwitchHarness(t *testing.T) *startTmuxChatHarness {
	t.Helper()
	h := newSwitchHarness(t)
	h.lc.accountSwitchRegistry = idKeyedSwitchRegistry{}
	h.lc.SetRotationRecorder(rotation.NewRecorder(&captureAuditStore{}, zerolog.Nop()))
	h.lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
	enableFailoverProxy(h.lc)
	h.lc.SetProxyPort(switchProxyPort)
	h.lc.SetProxyRegistrar(&targetRecordingRegistrar{})
	start := switchStartAccount
	h.sessions.sessions["sess-1"].AccountID = &start
	startForChat := switchStartAccount
	h.chats.chatsBySession["sess-1"][0].AccountID = &startForChat
	return h
}

// spawnEnv parses the environment bossd handed tmux out of the last recorded
// `new-session` argv. tmux takes each variable as `-e K=V`, so this is the
// environment the agent child really inherits — not a value re-derived by the
// test.
func spawnEnv(t *testing.T, h *startTmuxChatHarness) map[string]string {
	t.Helper()
	call := h.tmuxFake.lastCall("new-session")
	if call == nil {
		t.Fatal("no tmux new-session recorded; the respawn never happened")
	}
	env := map[string]string{}
	for i := 0; i+1 < len(call.args); i++ {
		if call.args[i] != "-e" {
			continue
		}
		k, v, ok := strings.Cut(call.args[i+1], "=")
		if ok {
			env[k] = v
		}
	}
	return env
}

// TestSwitchAccount_ResumableRespawnUsesChatScopedProxyTarget is the ticket's
// central assertion: after a cross-account switch the respawned pane's
// ANTHROPIC_BASE_URL resolves through the CHAT target, so the proxy reads the
// binding the switch wrote (agent_chats.account_id) rather than the session's
// untouched one.
func TestSwitchAccount_ResumableRespawnUsesChatScopedProxyTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}

	env := spawnEnv(t, h)
	want := wantChatProxyURL("agent-1", "acct-2")
	if got := env["ANTHROPIC_BASE_URL"]; got != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the chat-scoped target %q", got, want)
	}
	// The regression this replaces: the SESSION target, which resolves through
	// sessions.account_id and so still reads the account we switched away from.
	if got := env["ANTHROPIC_BASE_URL"]; got == wantSessionProxyURL("sess-1") {
		t.Errorf("ANTHROPIC_BASE_URL is still the session-scoped target %q", got)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != SentinelAPIKey {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the BOS-326 sentinel", got)
	}
	// Credential materialization was already correct and must stay correct: the
	// account resolver saw the TARGET account, even though the overlay strips its
	// bearer from the child env once the sentinel is planted.
	if _, ok := env[claudeOAuthTokenEnvKey]; ok {
		t.Errorf("%s must be stripped when the proxy overlay is applied: %v", claudeOAuthTokenEnvKey, env)
	}
}

// TestSwitchAccount_SecondSwitchRetargetsTheChatToken exercises TokenForChat's
// target-REWRITE branch: the chat already has a token from the first switch, so
// the second must re-register that same token against the new account rather
// than leaving it pinned to the first.
func TestSwitchAccount_SecondSwitchRetargetsTheChatToken(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	reg, ok := h.lc.proxyRegistrar.(*targetRecordingRegistrar)
	if !ok {
		t.Fatalf("registrar = %T, want *targetRecordingRegistrar", h.lc.proxyRegistrar)
	}

	for _, target := range []string{"acct-2", "acct-3"} {
		if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
			SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: target,
		}); err != nil {
			t.Fatalf("SwitchAccount to %s: %v", target, err)
		}
	}

	env := spawnEnv(t, h)
	want := wantChatProxyURL("agent-1", "acct-3")
	if got := env["ANTHROPIC_BASE_URL"]; got != want {
		t.Errorf("after two switches ANTHROPIC_BASE_URL = %q, want the SECOND target %q", got, want)
	}
	targets := reg.targets()
	if len(targets) < 2 {
		t.Fatalf("chat targets registered = %v, want one per switch", targets)
	}
	if last := targets[len(targets)-1]; last != ProxyTargetForChat("agent-1", "acct-3") {
		t.Errorf("last registered chat target = %q, want the second account's target", last)
	}
	if targets[0] == targets[len(targets)-1] {
		t.Errorf("both switches registered the same target %q; the rewrite branch never ran", targets[0])
	}
}

// TestSwitchAccount_NonResumableBindsNewChatToTarget covers root cause B: with
// no transcript to resume, StartTmuxChat mints a FRESH chat, and without the
// explicit ChatInput override it would seed that chat's account from
// sessions.account_id — the account the operator just left.
func TestSwitchAccount_NonResumableBindsNewChatToTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: false}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.Resumed {
		t.Fatal("precondition: this switch must start fresh, not resume")
	}

	h.chats.mu.Lock()
	creates := append([]db.CreateAgentChatParams(nil), h.chats.createCalls...)
	h.chats.mu.Unlock()
	if len(creates) != 1 {
		t.Fatalf("agent_chats Create calls = %d, want exactly 1 (the fresh chat)", len(creates))
	}
	fresh := creates[0]
	if fresh.AccountID == nil || *fresh.AccountID != "acct-2" {
		t.Errorf("fresh chat AccountID = %v, want acct-2 (the switch target), not the session seed acct-1", fresh.AccountID)
	}
	if fresh.AgentSessionID == "agent-1" {
		t.Fatal("precondition: a non-resumable switch mints a NEW agent session id")
	}

	env := spawnEnv(t, h)
	if got, want := env["ANTHROPIC_BASE_URL"], wantChatProxyURL(fresh.AgentSessionID, "acct-2"); got != want {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the fresh chat's chat-scoped target %q", got, want)
	}
}

// TestSwitchAccount_NonResumableResolvesTargetCredentialsWithoutProxy is the
// non-proxy regression guard: with the failover proxy off there is no overlay to
// re-target, so the only thing standing between the child and the wrong account
// is the account the env resolver was asked for. It must be the target's.
func TestSwitchAccount_NonResumableResolvesTargetCredentialsWithoutProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	h.lc.accountSwitchTranscripts = stubTranscriptProbe{exists: false}
	off := false
	h.lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &off})

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}

	env := spawnEnv(t, h)
	if got, want := env[claudeOAuthTokenEnvKey], "bearer-for-acct-2"; got != want {
		t.Errorf("%s = %q, want %q (the TARGET account's bearer)", claudeOAuthTokenEnvKey, got, want)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("no sentinel may be planted with the proxy disabled: %v", env["ANTHROPIC_API_KEY"])
	}
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("no proxy base URL may be injected with the proxy disabled: %v", env["ANTHROPIC_BASE_URL"])
	}
}

// TestSwitchAccount_ResumableRespawnKeepsTargetCredentialsWithoutProxy is the
// same guard on the RESUMABLE path, where the chat row (not the ChatInput
// override) is the account authority.
func TestSwitchAccount_ResumableRespawnKeepsTargetCredentialsWithoutProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	off := false
	h.lc.SetRotationConfig(config.ManagedAccountsConfig{FailoverProxy: &off})

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}

	env := spawnEnv(t, h)
	if got, want := env[claudeOAuthTokenEnvKey], "bearer-for-acct-2"; got != want {
		t.Errorf("%s = %q, want %q (the TARGET account's bearer)", claudeOAuthTokenEnvKey, got, want)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("no sentinel may be planted with the proxy disabled")
	}
}

// TestSwitchAccount_SubmitsSingleLineNotice pins the delivery contract: the
// respawn carries the switch notice as a single-line PROMPT and submits it, so
// the chat comes back visibly running instead of holding a pending composer
// line — the reporter's actual complaint.
func TestSwitchAccount_SubmitsSingleLineNotice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}

	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("no respawn BuildInteractiveCommand recorded")
	}
	// InitialPrompt is ChatInput.Prompt verbatim, so this reads the field the
	// switch set rather than a value re-derived by the test.
	notice := last.GetInitialPrompt()
	if notice != res.NoticeText {
		t.Errorf("respawn prompt = %q, want the notice returned to the caller %q", notice, res.NoticeText)
	}
	if !strings.Contains(notice, "Label acct-2") {
		t.Errorf("notice %q does not name the target account label", notice)
	}
	if strings.Contains(notice, "\n") {
		t.Errorf("notice %q is multi-line; multi-line payloads are the known prefilled-but-never-submitted shape", notice)
	}
	if last.GetInitialCommand() != "" {
		t.Errorf("InitialCommand = %q, want empty — the notice is prompt text", last.GetInitialCommand())
	}
	// Delivery == DeliverySubmit is observable as the Enter that
	// DeliveryPrefillOnly never sends.
	if got := h.tmuxFake.enterSendKeysCount(); got != 1 {
		t.Errorf("Enter presses = %d, want exactly 1 (DeliverySubmit, not DeliveryPrefillOnly)", got)
	}
}

// TestSwitchAccount_AutoRotationAlsoSubmitsNotice records the deliberate choice
// left open by the plan. The automatic rotator reaches this path from a chat it
// found usage-LIMITED — already stalled, its turn cut off and never resent — so
// a silent respawn would park an unattended run at an idle composer forever.
// Manual and automatic therefore behave identically.
func TestSwitchAccount_AutoRotationAlsoSubmitsNotice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2", Auto: true,
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("no respawn BuildInteractiveCommand recorded")
	}
	if got := last.GetInitialPrompt(); got != res.NoticeText {
		t.Errorf("auto respawn prompt = %q, want the auto-rotation notice %q", got, res.NoticeText)
	}
	if got := h.tmuxFake.enterSendKeysCount(); got != 1 {
		t.Errorf("auto Enter presses = %d, want exactly 1 (auto rotation submits too)", got)
	}
}

// TestSwitchAccount_RespawnInPlaceDeliversNothing is the other half of that
// choice. BOS-482's same-account respawn changes no account, can fire for many
// chats at once after a daemon restart, and has nothing to tell the operator —
// so it keeps delivering nothing rather than burning a turn per chat, or
// leaving the pending composer line this ticket exists to remove.
func TestSwitchAccount_RespawnInPlaceDeliversNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-1",
		Auto: true, RespawnSameAccount: true,
	})
	if err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if res.NoticeText == "" {
		t.Fatal("precondition: the respawn-in-place path still composes a caller-facing notice")
	}
	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("no respawn BuildInteractiveCommand recorded")
	}
	if got := last.GetInitialPrompt(); got != "" {
		t.Errorf("respawn-in-place delivered %q into the pane, want nothing", got)
	}
	if got := h.tmuxFake.enterSendKeysCount(); got != 0 {
		t.Errorf("respawn-in-place Enter presses = %d, want 0", got)
	}
}

// TestSwitchAccount_RespawnInPlaceDeliversNothingWhenChatRowIsUnbound is the
// same guarantee for the chat row the test above cannot express. The notice is
// suppressed on the caller's INTENT (RespawnSameAccount), not on
// isSameAccountRespawn, because that conjunct compares two account values
// derived by DIFFERENT rules: the rotator's target comes from
// ChatContext.AccountID, which falls back to sessions.account_id for a chat row
// whose account_id is NULL, while the `current` side reads
// chatAccountBinding.SessionAccount, which returns "" for that row and never
// consults the session. Keyed on the comparison, an auth-refresh respawn of a
// NULL-bound chat would submit a "switched to <label>" turn into a pane whose
// account never changed — unattended, and once per chat after a daemon restart.
func TestSwitchAccount_RespawnInPlaceDeliversNothingWhenChatRowIsUnbound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	// The row carries no account of its own; the session stays on acct-1, which
	// is how the rotator resolves the target it passes back in.
	h.chats.chatsBySession["sess-1"][0].AccountID = nil

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: switchStartAccount,
		Auto: true, RespawnSameAccount: true,
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}

	last := h.agentFake.LastBuildInteractiveCommand
	if last == nil {
		t.Fatal("no respawn BuildInteractiveCommand recorded")
	}
	if got := last.GetInitialPrompt(); got != "" {
		t.Errorf("respawn-in-place of a NULL-bound chat delivered %q into the pane, want nothing", got)
	}
	if got := h.tmuxFake.enterSendKeysCount(); got != 0 {
		t.Errorf("respawn-in-place Enter presses = %d, want 0 (no turn burned on an auth refresh)", got)
	}
}

// TestSwitchAccount_UndeliverableNoticeStillSucceeds pins the non-fatal
// contract. The account is rebound and the pane is up before the notice is
// delivered, so a refused delivery must degrade to "pane up, no notice" — not
// report a completed switch as failed, and above all not run the failure
// cleanup, which KILLS the healthy pane it just brought up.
func TestSwitchAccount_UndeliverableNoticeStillSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)
	// The readiness gate resolves a composer row and then asks the runner whether
	// the pane is really showing a modal. Answering yes refuses the delivery with
	// a CLASSIFIED outcome (blocked by modal) against a pane that is up.
	h.agentFake.HasQuestionPromptFunc = func(*bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
		return &bossanovav1.HasQuestionPromptResponse{BlocksInput: true}, nil
	}

	res, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	})
	if err != nil {
		t.Fatalf("SwitchAccount must not fail on an undeliverable notice: %v", err)
	}
	if res.TargetLabel != "Label acct-2" {
		t.Errorf("TargetLabel = %q, want the target's label", res.TargetLabel)
	}
	// The rebind is the part that must have survived.
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2 persisted despite the failed notice", got)
	}
	if got := h.tmuxFake.enterSendKeysCount(); got != 0 {
		t.Errorf("a refused delivery still sent %d Enter presses", got)
	}
	// Nothing may be stamped "(failed to start)": the chat is healthy.
	h.chats.mu.Lock()
	stamps := append([]markStartFailedCall(nil), h.chats.markStartFailedCalls...)
	h.chats.mu.Unlock()
	for _, s := range stamps {
		if strings.Contains(s.reason, "send plan failed") || strings.Contains(s.reason, "respawn after switch failed") {
			t.Errorf("healthy chat stamped start-failed over an undelivered notice: %q", s.reason)
		}
	}
}

// TestSwitchAccount_PreservesChatScopedBinding is the BOS-1386 guard the plan
// lists as verify-only, asserted rather than grepped: the manual switch must
// still write ONLY agent_chats.account_id. Mirroring it onto the session would
// "fix" the symptom by undoing the decision that a cross-agent chat cannot
// corrupt its parent session's binding.
func TestSwitchAccount_PreservesChatScopedBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newProxySwitchHarness(t)

	if _, err := h.lc.SwitchAccount(context.Background(), SwitchAccountParams{
		SessionID: "sess-1", AgentSessionID: "agent-1", TargetAccountID: "acct-2",
	}); err != nil {
		t.Fatalf("SwitchAccount: %v", err)
	}
	if got := h.sessions.sessions["sess-1"].AccountID; got == nil || *got != "acct-1" {
		t.Errorf("session AccountID = %v, want the untouched acct-1", got)
	}
	if got := h.chats.chatsBySession["sess-1"][0].AccountID; got == nil || *got != "acct-2" {
		t.Errorf("chat AccountID = %v, want acct-2", got)
	}
}

// TestResolveAccountEnvForChat_DiscriminatesAuthority isolates the resolver
// itself: a chat that owns its account keys the overlay on the CHAT target,
// while a session-seeded spawn (nil chat, or a chat with no account of its own)
// keeps the session target byte-for-byte. This is the discriminator the two
// session-authority spawn sites depend on staying unchanged.
func TestResolveAccountEnvForChat_DiscriminatesAuthority(t *testing.T) {
	newLC := func() *Lifecycle {
		lc := &Lifecycle{logger: zerolog.Nop()}
		lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
		enableFailoverProxy(lc)
		lc.SetProxyPort(switchProxyPort)
		lc.SetProxyRegistrar(&targetRecordingRegistrar{})
		return lc
	}
	acct := "acct-7"
	sess := &models.Session{ID: "s1", AgentName: "claude", AccountID: &acct}

	t.Run("chat-bound account keys the chat target", func(t *testing.T) {
		lc := newLC()
		chat := &models.AgentChat{SessionID: "s1", AgentSessionID: "agent-9", AgentName: "claude", AccountID: &acct}
		got, err := lc.resolveAccountEnvForChat(context.Background(), sess, chat)
		if err != nil {
			t.Fatalf("resolveAccountEnvForChat: %v", err)
		}
		if want := wantChatProxyURL("agent-9", acct); got["ANTHROPIC_BASE_URL"] != want {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want %q", got["ANTHROPIC_BASE_URL"], want)
		}
	})

	t.Run("nil chat degrades to the session target", func(t *testing.T) {
		lc := newLC()
		got, err := lc.resolveAccountEnvForChat(context.Background(), sess, nil)
		if err != nil {
			t.Fatalf("resolveAccountEnvForChat: %v", err)
		}
		if want := wantSessionProxyURL("s1"); got["ANTHROPIC_BASE_URL"] != want {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want the session target %q", got["ANTHROPIC_BASE_URL"], want)
		}
	})

	t.Run("chat with no account of its own degrades to the session target", func(t *testing.T) {
		lc := newLC()
		chat := &models.AgentChat{SessionID: "s1", AgentSessionID: "agent-9", AgentName: "claude"}
		got, err := lc.resolveAccountEnvForChat(context.Background(), sess, chat)
		if err != nil {
			t.Fatalf("resolveAccountEnvForChat: %v", err)
		}
		if want := wantSessionProxyURL("s1"); got["ANTHROPIC_BASE_URL"] != want {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want the session target %q", got["ANTHROPIC_BASE_URL"], want)
		}
	})

	t.Run("session-authority resolveAccountEnv is unchanged", func(t *testing.T) {
		lc := newLC()
		got, err := lc.resolveAccountEnv(context.Background(), sess)
		if err != nil {
			t.Fatalf("resolveAccountEnv: %v", err)
		}
		if want := wantSessionProxyURL("s1"); got["ANTHROPIC_BASE_URL"] != want {
			t.Errorf("ANTHROPIC_BASE_URL = %q, want the session target %q", got["ANTHROPIC_BASE_URL"], want)
		}
	})
}

// TestSessionAuthoritySpawnsStaySessionScoped is the over-reach guard. The two
// session-authority spawn sites — the session-start capability preflight and the
// initial headless start — run before any agent_chats row exists for the
// session, so no chat binding can have diverged from the session's and there is
// nothing for a chat-scoped target to resolve through. Both must therefore keep
// minting a SESSION-scoped proxy token, and this reads the environment each of
// them actually handed the runner rather than the helper they called.
func TestSessionAuthoritySpawnsStaySessionScoped(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	runner := newMockAgentRunner()
	repos.repos["repo-1"] = &models.Repo{
		ID: "repo-1", LocalPath: "/tmp/repo", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/worktrees",
	}
	acct := switchStartAccount
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Seeded", Plan: "plan body",
		BaseBranch: "main", AgentName: "claude", AccountID: &acct, State: machine.CreatingWorktree,
	}
	lc := newTestLifecycle(sessions, repos, nil, nil, &mockWorktreeManager{}, runner, nil, newMockVCSProvider(), zerolog.Nop())
	lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
	enableFailoverProxy(lc)
	lc.SetProxyPort(switchProxyPort)
	lc.SetProxyRegistrar(&targetRecordingRegistrar{})

	// A non-UNSPECIFIED profile is what makes the preflight run at all, so both
	// session-authority sites are exercised by one start.
	if err := lc.StartSession(ctx, "sess-1", StartSessionOpts{
		Detach:                    true,
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	awaitDraftPR(t, lc, "sess-1")

	want := wantSessionProxyURL("sess-1")
	if len(runner.preflights) != 1 {
		t.Fatalf("preflight records = %d, want 1", len(runner.preflights))
	}
	if got := runner.preflights[0].env["ANTHROPIC_BASE_URL"]; got != want {
		t.Errorf("preflight ANTHROPIC_BASE_URL = %q, want the session-scoped target %q", got, want)
	}
	if len(runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(runner.started))
	}
	if got := runner.started[0].env["ANTHROPIC_BASE_URL"]; got != want {
		t.Errorf("headless start ANTHROPIC_BASE_URL = %q, want the session-scoped target %q", got, want)
	}
}

// TestResumeOrphanedHeadlessRuns_UsesChatScopedProxyTarget covers the sibling
// the plan enumerated alongside the ticket's own site. An orphan resume builds
// its spawn view from the primary chat, so a chat switched before the daemon
// died must resume on the chat's account — not on the session's stale seed.
func TestResumeOrphanedHeadlessRuns_UsesChatScopedProxyTarget(t *testing.T) {
	f := newOrphanResumeFixture(t)
	yes := true
	f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &yes, FailoverProxy: &yes})
	f.lc.SetProxyPort(switchProxyPort)
	f.lc.SetProxyRegistrar(&targetRecordingRegistrar{})
	f.lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
	chatAccount := "acct-chat"
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "agent-old",
			AgentName:      "claude",
			AccountID:      &chatAccount,
		}},
	}}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(f.runner.started))
	}
	want := wantChatProxyURL("agent-old", chatAccount)
	if got := f.runner.started[0].env["ANTHROPIC_BASE_URL"]; got != want {
		t.Errorf("orphan-resume ANTHROPIC_BASE_URL = %q, want the chat-scoped target %q", got, want)
	}
}

// TestSwitchBudgetStillFundsTheSubmit records the budget decision. The notice is
// submitted INSIDE the switch's detach.Flight rather than after it, and that is
// safe because the respawn already ran a composer-readiness wait inside the same
// budget before this change — it delivered an empty prefill — so the notice adds
// the submit verifier's tail, not a second readiness wait. The budget's fixed
// allowances must keep covering that tail on top of the modal probe they already
// reserve.
func TestSwitchBudgetStillFundsTheSubmit(t *testing.T) {
	for _, readiness := range []int{0, 5, 45, 120} {
		d := time.Duration(readiness) * time.Second
		budget := config.SwitchRespawnBudgetFor(d)
		resolved := d
		if readiness == 0 {
			resolved = config.DefaultSessionStartReadyDeadline
		}
		allowance := budget - resolved
		want := config.SwitchModalProbeReserve + config.StartChatRunSubmitVerifierTail
		if allowance < want {
			t.Errorf("readiness=%v: budget allowance over the readiness wait = %v, want at least %v "+
				"(one modal probe plus the submit verifier the account-switch notice adds)",
				d, allowance, want)
		}
	}
}

// retargetableRegistrar models the ProxyServer's chat-token bookkeeping closely
// enough to answer the one question the rekey test asks: which proxy target does
// the token a RUNNING pane already holds resolve to now? targetRecordingRegistrar
// cannot answer it — it encodes the target in the token string itself, so a
// later retarget would be invisible, exactly as it is in the real registrar
// where the token is opaque and only the registry knows the target.
type retargetableRegistrar struct {
	mu        sync.Mutex
	minted    int
	targets   map[string]string // token → proxy target
	chatToken map[string]string // agentSessionID → token
	retargets int
}

func newRetargetableRegistrar() *retargetableRegistrar {
	return &retargetableRegistrar{targets: map[string]string{}, chatToken: map[string]string{}}
}

func (r *retargetableRegistrar) TokenForSession(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok := "sessiontok-" + url.QueryEscape(sessionID)
	r.targets[tok] = sessionID
	return tok
}

func (r *retargetableRegistrar) TokenForChat(_, agentSessionID, fallbackAccountID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, ok := r.chatToken[agentSessionID]
	if !ok {
		r.minted++
		tok = "chattok-" + strconv.Itoa(r.minted)
		r.chatToken[agentSessionID] = tok
	}
	r.targets[tok] = ProxyTargetForChat(agentSessionID, fallbackAccountID)
	return tok
}

func (r *retargetableRegistrar) RetargetChatToken(_, priorAgentSessionID, newAgentSessionID, accountID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tok, ok := r.chatToken[priorAgentSessionID]
	if !ok {
		return
	}
	r.retargets++
	delete(r.chatToken, priorAgentSessionID)
	r.chatToken[newAgentSessionID] = tok
	r.targets[tok] = ProxyTargetForChat(newAgentSessionID, accountID)
}

func (r *retargetableRegistrar) ForgetBearer(string)                              {}
func (r *retargetableRegistrar) ForgetAllBearers()                                {}
func (r *retargetableRegistrar) AdoptToken(string, string)                        {}
func (r *retargetableRegistrar) AdoptTokenForChat(string, string, string, string) {}
func (r *retargetableRegistrar) RebuildTokenRegistry(context.Context) error       { return nil }

// targetFor resolves a token exactly as the proxy's hashToTarget index does.
func (r *retargetableRegistrar) targetFor(token string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.targets[token]
}

// tokenFromProxyURL extracts the /s/<token> path segment of an injected
// ANTHROPIC_BASE_URL — the value a spawned process bakes in permanently.
func tokenFromProxyURL(t *testing.T, baseURL string) string {
	t.Helper()
	idx := strings.LastIndex(baseURL, "/s/")
	if idx == -1 {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want a /s/<token> path", baseURL)
	}
	return baseURL[idx+len("/s/"):]
}

// TestResumeOrphanedHeadlessRuns_ProxyTokenFollowsTheRekey is the rekey half of
// the chat-scoped target. The spawn env can only be keyed on the PRIOR agent
// session id — the replacement id does not exist until the runner returns it —
// and syncResumedPrimaryChat then moves the chat row onto that new id. Unless
// the token moves with it, the frozen /s/<token> the resumed process holds
// resolves to an id no row answers to: chatProxyBinding finds nothing,
// CurrentBearer returns "", and the proxy forwards the sentinel API key that
// upstream rejects.
//
// The assertion is deliberately taken at the proxy's own seam (CurrentBearer
// over the target the token resolves to) rather than at the registrar call, so
// it fails for the reason that matters — a resumed run with no working proxy
// authentication — and not merely because a method went uncalled.
func TestResumeOrphanedHeadlessRuns_ProxyTokenFollowsTheRekey(t *testing.T) {
	f := newOrphanResumeFixture(t)
	yes := true
	f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &yes, FailoverProxy: &yes})
	f.lc.SetProxyPort(switchProxyPort)
	registrar := newRetargetableRegistrar()
	f.lc.SetProxyRegistrar(registrar)
	f.lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
	chatAccount := "acct-chat"
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "agent-old",
			AgentName:      "claude",
			AccountID:      &chatAccount,
		}},
	}}
	f.materializer.env = map[string]string{claudeOAuthTokenEnvKey: "bearer-for-acct-chat"}
	f.lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
		return &models.Account{ID: id, Provider: models.AccountProviderClaude, Label: "chat"}, nil
	})

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if len(f.runner.started) != 1 {
		t.Fatalf("headless starts = %d, want 1", len(f.runner.started))
	}
	// The rekey is the precondition the whole test rests on: without it the old
	// target would keep resolving and every assertion below would pass vacuously.
	chats := f.lc.agentChats.(*mockAgentChatStore)
	if got := chats.chatsBySession[f.sessionID][0].AgentSessionID; got != "agent-new" {
		t.Fatalf("chat row agent_session_id = %q, want agent-new (the resume rekeys it)", got)
	}

	token := tokenFromProxyURL(t, f.runner.started[0].env["ANTHROPIC_BASE_URL"])
	target := registrar.targetFor(token)
	wantTarget := ProxyTargetForChat("agent-new", chatAccount)
	if target != wantTarget {
		t.Errorf("the resumed pane's token resolves to %q, want %q (the id the chat row now carries)",
			target, wantTarget)
	}
	bearer, err := f.lc.CurrentBearer(context.Background(), target)
	if err != nil {
		t.Fatalf("CurrentBearer: %v", err)
	}
	if bearer != "bearer-for-acct-chat" {
		t.Errorf("CurrentBearer over the resumed pane's own token = %q, want the chat account's bearer; "+
			"an empty bearer means the proxy forwards the sentinel API key and the run loses authentication",
			bearer)
	}
}

// TestResumeOrphanedHeadlessRuns_NoRetargetWithoutAChatBoundAccount pins the
// guard: a session-seeded spawn never minted a chat-scoped token, so the resume
// must not ask the registrar to move one. The registrar no-ops on an unknown
// chat either way, but calling it would mean the discriminator here had drifted
// from resolveAccountEnvForChat's.
func TestResumeOrphanedHeadlessRuns_NoRetargetWithoutAChatBoundAccount(t *testing.T) {
	f := newOrphanResumeFixture(t)
	yes := true
	f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeOrphans: &yes, FailoverProxy: &yes})
	f.lc.SetProxyPort(switchProxyPort)
	registrar := newRetargetableRegistrar()
	f.lc.SetProxyRegistrar(registrar)
	f.lc.SetAccountEnvResolver(accountKeyedEnvResolver{})
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		f.sessionID: {{
			SessionID:      f.sessionID,
			AgentSessionID: "agent-old",
			AgentName:      "claude",
			// No chat-scoped account: the session seed governs this spawn.
		}},
	}}

	if n := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); n != 1 {
		t.Fatalf("resumed = %d, want 1", n)
	}
	if registrar.retargets != 0 {
		t.Errorf("RetargetChatToken calls = %d, want 0 for a session-authority spawn", registrar.retargets)
	}
}
