package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// fakeMaterializer implements account.Materializer. It returns env by account
// id from envByAccount when set (so tests can prove provider-appropriate env is
// injected), falling back to the fixed env for any account otherwise.
type fakeMaterializer struct {
	supports     bool
	env          map[string]string
	envByAccount map[string]map[string]string
	calls        int
	materialize  []string

	// BOS-1142 error injectors. The fake used to be success-only, which made it
	// structurally unable to exercise the fail-closed paths: every refusal this
	// change introduces is reached through one of these two calls failing.
	supportsErr    error
	materializeErr error
}

// resolveChatAccountEnv is a test-only convenience wrapper around
// resolveChatAccountEnvForSpawn for the no-precomputed-default case. Production
// callers (record/attach and wake paths) always pass the default resolved by
// defaultAccountIDForChat, so this shim lives with the tests rather than on the
// server.
func (s *Server) resolveChatAccountEnv(ctx context.Context, sess *models.Session, chat *models.AgentChat) (map[string]string, error) {
	return s.resolveChatAccountEnvForSpawn(ctx, sess, chat, "", recordAccountUse)
}

type accountEnvChatStoreFake struct {
	db.AgentChatStore

	updateAccountCalls int
	lastAgentSessionID string
	lastAccountID      *string
}

func (f *accountEnvChatStoreFake) UpdateAccountIDByAgentSessionID(_ context.Context, agentSessionID string, accountID *string) error {
	f.updateAccountCalls++
	f.lastAgentSessionID = agentSessionID
	f.lastAccountID = accountID
	return nil
}

func (m *fakeMaterializer) SupportsRotation(_ context.Context, _ string) (bool, error) {
	if m.supportsErr != nil {
		return false, m.supportsErr
	}
	return m.supports, nil
}

func (m *fakeMaterializer) MaterializeAccount(_ context.Context, accountID string) (map[string]string, error) {
	m.calls++
	m.materialize = append(m.materialize, accountID)
	if m.materializeErr != nil {
		return nil, m.materializeErr
	}
	if env, ok := m.envByAccount[accountID]; ok {
		return env, nil
	}
	return m.env, nil
}

type serverStubRegistrar struct {
	token           string
	chatCalls       int
	lastChatSession string
	lastChatAgent   string
	lastChatAccount string
}

func (s *serverStubRegistrar) TokenForSession(string) string { return s.token }
func (s *serverStubRegistrar) ForgetBearer(string)           {}
func (s *serverStubRegistrar) ForgetAllBearers()             {}
func (s *serverStubRegistrar) TokenForChat(sessionID, agentSessionID, fallbackAccountID string) string {
	s.chatCalls++
	s.lastChatSession = sessionID
	s.lastChatAgent = agentSessionID
	s.lastChatAccount = fallbackAccountID
	return s.token
}
func (s *serverStubRegistrar) AdoptToken(string, string)                        {}
func (s *serverStubRegistrar) AdoptTokenForChat(string, string, string, string) {}
func (s *serverStubRegistrar) RebuildTokenRegistry(context.Context) error       { return nil }
func (s *serverStubRegistrar) RetargetChatToken(_, _, _, _ string)              {}

// TestResolveAccountEnv_BoundVsAccountZero proves the Fix #1 attach-path account
// env resolution: a bound session materializes its account's spawn env, while an
// unbound session (account 0) and a nil resolver both yield nil (byte-identical
// to the ambient-login behavior).
func TestResolveAccountEnv_BoundVsAccountZero(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-bound"}},
		zerolog.Nop(),
	)

	// Bound session: the account's materialized env is returned.
	bound := &models.Session{AccountID: &acct.Id, AgentName: "claude"}
	got, err := s.resolveAccountEnv(context.Background(), bound)
	if err != nil {
		t.Fatalf("bound session resolve failed: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-bound" {
		t.Errorf("bound session env = %v, want ANTHROPIC_API_KEY=sk-bound", got)
	}

	// Account 0 (nil AccountID): no env, no materializer call.
	unbound := &models.Session{AgentName: "claude"}
	if got, _ := s.resolveAccountEnv(context.Background(), unbound); got != nil {
		t.Errorf("account-0 env = %v, want nil", got)
	}

	// Nil resolver degrades to nil.
	s.resolver = nil
	if got, _ := s.resolveAccountEnv(context.Background(), bound); got != nil {
		t.Errorf("nil-resolver env = %v, want nil", got)
	}

	// Nil session degrades to nil (never panics).
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())
	if got, _ := s.resolveAccountEnv(context.Background(), nil); got != nil {
		t.Errorf("nil-session env = %v, want nil", got)
	}
}

// TestResolveAccountEnv_FailsClosedOnInjection proves the BOS-1142 refusal
// reaches the attach path intact: a bound session whose credentials cannot be
// injected gets an error and NO env, never a nil-error nil-env that the caller
// would read as "account 0" and spawn on the agent CLI's ambient login.
//
// It also pins the classification boundary the operator-facing surfaces depend
// on. A failed materialization indicts the credential (invalid); a materializer
// that could not even say whether the provider supports rotation indicts
// nothing (undetermined) and must never be reported as a bad credential.
func TestResolveAccountEnv_FailsClosedOnInjection(t *testing.T) {
	newServer := func(t *testing.T, mat *fakeMaterializer) (*Server, *pb.Account) {
		t.Helper()
		s, accts := newAccountServer(t, newFakeCredStore(), nil)
		acct := mustAddClaude(t, s, "work", []byte("blob"))
		s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
		return s, acct
	}

	t.Run("a materialize failure refuses the spawn as an invalid credential", func(t *testing.T) {
		mat := &fakeMaterializer{supports: true, materializeErr: errors.New("keyring read failed")}
		s, acct := newServer(t, mat)

		env, err := s.resolveAccountEnv(context.Background(), &models.Session{AccountID: &acct.Id, AgentName: "claude"})
		if err == nil {
			t.Fatal("resolveAccountEnv returned nil error; a bound account that cannot be injected must fail closed")
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
		if got := account.InjectionOutcomeOf(err); got != account.InjectionOutcomeInvalid {
			t.Errorf("outcome = %q, want %q", got, account.InjectionOutcomeInvalid)
		}
		if mat.calls != 1 {
			t.Errorf("materialize calls = %d, want 1", mat.calls)
		}
	})

	t.Run("an unanswerable rotation probe refuses without indicting the credential", func(t *testing.T) {
		mat := &fakeMaterializer{supportsErr: errors.New("plugin unreachable")}
		s, acct := newServer(t, mat)

		env, err := s.resolveAccountEnv(context.Background(), &models.Session{AccountID: &acct.Id, AgentName: "claude"})
		if err == nil {
			t.Fatal("resolveAccountEnv returned nil error; an unevaluable binding must still fail closed")
		}
		if env != nil {
			t.Errorf("env = %v, want nil", env)
		}
		if got := account.InjectionOutcomeOf(err); got != account.InjectionOutcomeUndetermined {
			t.Errorf("outcome = %q, want %q", got, account.InjectionOutcomeUndetermined)
		}
		if mat.calls != 0 {
			t.Errorf("materialize calls = %d, want 0; the probe failed before materialization", mat.calls)
		}
	})

	t.Run("account 0 is unaffected by a materializer that cannot serve anyone", func(t *testing.T) {
		mat := &fakeMaterializer{supports: true, materializeErr: errors.New("keyring read failed")}
		s, _ := newServer(t, mat)

		env, err := s.resolveAccountEnv(context.Background(), &models.Session{AgentName: "claude"})
		if err != nil {
			t.Fatalf("account-0 resolve failed: %v; nothing was bound, so nothing can be refused", err)
		}
		if env != nil {
			t.Errorf("account-0 env = %v, want nil", env)
		}
		if mat.calls != 0 {
			t.Errorf("materialize calls = %d, want 0", mat.calls)
		}
	})
}

// TestResolveChatAccountEnv_CrossAgent proves the chat spawn resolves account
// env for the CHAT's agent, not the parent session's: a same-agent chat inherits
// the session's binding, a cross-agent chat with no binding of its own uses the
// chat provider's default account, and an explicit chat-level binding wins.
func TestResolveChatAccountEnv_CrossAgent(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	claudeAcct := mustAddClaude(t, s, "claude-work", []byte("blob"))
	codexAcct := mustAddCodex(t, s, "codex-work", []byte("blob"))
	// Provider-shaped payloads so each assertion proves the CHAT provider's own
	// account was materialized, not just that the fake echoed a fixed blob.
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{
		claudeAcct.Id: {"ANTHROPIC_API_KEY": "sk-claude"},
		codexAcct.Id:  {"CODEX_API_KEY": "sk-codex"},
	}}
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		mat,
		zerolog.Nop(),
	)
	claudeSession := &models.Session{AccountID: &claudeAcct.Id, AgentName: "claude"}

	// Same-agent chat: inherits the session's claude binding.
	sameAgent := &models.AgentChat{AgentName: "claude"}
	if got, _ := s.resolveChatAccountEnv(context.Background(), claudeSession, sameAgent); got["ANTHROPIC_API_KEY"] != "sk-claude" {
		t.Errorf("same-agent chat env = %v, want ANTHROPIC_API_KEY=sk-claude", got)
	}
	if got := mat.materialize[len(mat.materialize)-1]; got != claudeAcct.Id {
		t.Fatalf("same-agent materialized account = %q, want claude account %q", got, claudeAcct.Id)
	}

	// Cross-agent chat (codex chat in a claude-bound session), no chat binding:
	// uses the codex provider's default account, never the parent claude account
	// — the injected env is codex-shaped and carries no claude credentials.
	crossAgent := &models.AgentChat{AgentName: "codex"}
	got, err := s.resolveChatAccountEnvForSpawn(context.Background(), claudeSession, crossAgent, codexAcct.Id, recordAccountUse)
	if err != nil {
		t.Fatalf("cross-agent resolve failed: %v", err)
	}
	if got["CODEX_API_KEY"] != "sk-codex" {
		t.Errorf("cross-agent chat env = %v, want CODEX_API_KEY=sk-codex", got)
	}
	if _, leaked := got["ANTHROPIC_API_KEY"]; leaked {
		t.Errorf("cross-agent chat env leaked claude credentials: %v", got)
	}
	if got := mat.materialize[len(mat.materialize)-1]; got != codexAcct.Id {
		t.Fatalf("cross-agent materialized account = %q, want codex account %q", got, codexAcct.Id)
	}

	// Explicit chat-level binding wins even when the chat runs its own agent.
	boundChat := &models.AgentChat{AgentName: "claude", AccountID: &claudeAcct.Id}
	if got, _ := s.resolveChatAccountEnv(context.Background(), &models.Session{AgentName: "claude"}, boundChat); got["ANTHROPIC_API_KEY"] != "sk-claude" {
		t.Errorf("chat-bound env = %v, want ANTHROPIC_API_KEY=sk-claude", got)
	}
	if got := mat.materialize[len(mat.materialize)-1]; got != claudeAcct.Id {
		t.Fatalf("explicit chat materialized account = %q, want claude account %q", got, claudeAcct.Id)
	}

	// Nil chat / nil resolver degrade to nil (never panics).
	if got, _ := s.resolveChatAccountEnv(context.Background(), claudeSession, nil); got != nil {
		t.Errorf("nil-chat env = %v, want nil", got)
	}
	s.resolver = nil
	if got, _ := s.resolveChatAccountEnv(context.Background(), claudeSession, sameAgent); got != nil {
		t.Errorf("nil-resolver env = %v, want nil", got)
	}
}

func TestResolveChatAccountEnvForSpawn_AddsClaudeProxyOverlay(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	claudeAcct := mustAddClaude(t, s, "claude-work", []byte("blob"))
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{
		claudeAcct.Id: {"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
	}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	yes := true
	lc := &session.Lifecycle{}
	lc.SetRotationConfig(config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	lc.SetProxyPort(5555)
	lc.SetProxyRegistrar(&serverStubRegistrar{token: "path-tok"})
	s.lifecycle = lc

	claudeSession := &models.Session{ID: "sess-1", AccountID: &claudeAcct.Id, AgentName: "claude"}
	chat := &models.AgentChat{SessionID: claudeSession.ID, AgentSessionID: "agent-1", AgentName: "claude"}
	got, _ := s.resolveChatAccountEnvForSpawn(context.Background(), claudeSession, chat, "", recordAccountUse)
	// BOS-326: the proxy resolves the bearer server-side, so the subprocess gets
	// the sentinel + base URL but NOT the account's OAuth token (which would trip
	// Claude Code's "both set" warning).
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN must be stripped when proxied: %v", got)
	}
	if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/path-tok" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want proxy URL", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_API_KEY"] != session.SentinelAPIKey {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want sentinel", got["ANTHROPIC_API_KEY"])
	}
}

func TestResolveChatAccountEnvForSpawn_AddsClaudeProxyOverlayForDefaultAccount(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	claudeAcct := mustAddClaude(t, s, "claude-work", []byte("blob"))
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{
		claudeAcct.Id: {"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
	}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	yes := true
	lc := &session.Lifecycle{}
	lc.SetRotationConfig(config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	lc.SetProxyPort(5555)
	registrar := &serverStubRegistrar{token: "path-tok"}
	lc.SetProxyRegistrar(registrar)
	s.lifecycle = lc

	claudeSession := &models.Session{ID: "sess-1", AgentName: "claude"}
	chat := &models.AgentChat{SessionID: claudeSession.ID, AgentSessionID: "agent-1", AgentName: "claude"}
	got, _ := s.resolveChatAccountEnvForSpawn(context.Background(), claudeSession, chat, claudeAcct.Id, recordAccountUse)
	// BOS-326: proxied subprocess carries the sentinel, not the OAuth token.
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN must be stripped when proxied: %v", got)
	}
	if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/path-tok" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want proxy URL", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_API_KEY"] != session.SentinelAPIKey {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want sentinel", got["ANTHROPIC_API_KEY"])
	}
	if registrar.chatCalls != 1 || registrar.lastChatSession != claudeSession.ID ||
		registrar.lastChatAgent != chat.AgentSessionID || registrar.lastChatAccount != claudeAcct.Id {
		t.Fatalf("chat proxy token call = (%d,%q,%q,%q), want default account target", registrar.chatCalls, registrar.lastChatSession, registrar.lastChatAgent, registrar.lastChatAccount)
	}
}

func TestResolveChatAccountEnvForSpawn_ChatBoundSameAgentUsesChatProxyTarget(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	parentAcct := mustAddClaude(t, s, "parent", []byte("parent-blob"))
	chatAcct := mustAddClaude(t, s, "chat", []byte("chat-blob"))
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{
		parentAcct.Id: {"CLAUDE_CODE_OAUTH_TOKEN": "parent-token"},
		chatAcct.Id:   {"CLAUDE_CODE_OAUTH_TOKEN": "chat-token"},
	}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	yes := true
	lc := &session.Lifecycle{}
	lc.SetRotationConfig(config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	lc.SetProxyPort(5555)
	registrar := &serverStubRegistrar{token: "path-tok"}
	lc.SetProxyRegistrar(registrar)
	s.lifecycle = lc

	claudeSession := &models.Session{ID: "sess-1", AccountID: &parentAcct.Id, AgentName: "claude"}
	chat := &models.AgentChat{SessionID: claudeSession.ID, AgentSessionID: "agent-1", AgentName: "claude", AccountID: &chatAcct.Id}
	got, _ := s.resolveChatAccountEnvForSpawn(context.Background(), claudeSession, chat, "", recordAccountUse)
	// BOS-326: the OAuth token is stripped from the proxied subprocess; the chat's
	// account is instead threaded to the proxy via the path token (asserted on the
	// registrar below), which is what selects chat-token server-side.
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN must be stripped when proxied: %v", got)
	}
	if got["ANTHROPIC_API_KEY"] != session.SentinelAPIKey {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want sentinel", got["ANTHROPIC_API_KEY"])
	}
	if registrar.chatCalls != 1 || registrar.lastChatAccount != chatAcct.Id {
		t.Fatalf("chat proxy token call = (%d,%q), want explicit chat account", registrar.chatCalls, registrar.lastChatAccount)
	}
}

func TestResolveChatAccountEnvForSpawn_AddsClaudeProxyOverlayForCrossAgentClaude(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	claudeAcct := mustAddClaude(t, s, "claude-work", []byte("blob"))
	codexAcct := mustAddCodex(t, s, "codex-work", []byte("blob"))
	mat := &fakeMaterializer{supports: true, envByAccount: map[string]map[string]string{
		claudeAcct.Id: {"CLAUDE_CODE_OAUTH_TOKEN": "bound-token"},
		codexAcct.Id:  {"CODEX_API_KEY": "sk-codex"},
	}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	yes := true
	lc := &session.Lifecycle{}
	lc.SetRotationConfig(config.ManagedAccountsConfig{Enabled: &yes, FailoverProxy: &yes})
	lc.SetProxyPort(5555)
	lc.SetProxyRegistrar(&serverStubRegistrar{token: "path-tok"})
	s.lifecycle = lc

	codexSession := &models.Session{ID: "sess-1", AccountID: &codexAcct.Id, AgentName: "codex"}
	chat := &models.AgentChat{SessionID: codexSession.ID, AgentSessionID: "agent-1", AgentName: "claude"}
	got, _ := s.resolveChatAccountEnvForSpawn(context.Background(), codexSession, chat, claudeAcct.Id, recordAccountUse)
	// BOS-326: proxied subprocess carries the sentinel, not the OAuth token.
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN must be stripped when proxied: %v", got)
	}
	if got["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:5555/s/path-tok" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want proxy URL", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_API_KEY"] != session.SentinelAPIKey {
		t.Fatalf("ANTHROPIC_API_KEY = %q, want sentinel", got["ANTHROPIC_API_KEY"])
	}
}

func TestDefaultAccountForChat_PersistsNeverBoundSameAgentSessionBinding(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-default"}},
		zerolog.Nop(),
	)
	store := &lifecycleSessionStoreFake{session: &models.Session{
		ID:        "sess-1",
		AgentName: "claude",
		CreatedAt: time.Now(),
	}}
	s.sessions = store

	sess := store.session
	chat := &models.AgentChat{SessionID: sess.ID, AgentName: "claude"}
	accountID := s.defaultAccountIDForChat(context.Background(), sess, chat)
	if accountID != acct.Id {
		t.Fatalf("defaultAccountIDForChat = %q, want %q", accountID, acct.Id)
	}
	s.persistDefaultAccountForChat(context.Background(), sess, chat, accountID)

	if sess.AccountID == nil || *sess.AccountID != acct.Id {
		t.Fatalf("session AccountID = %v, want %s", sess.AccountID, acct.Id)
	}
	if store.updateCalls != 1 {
		t.Fatalf("Update calls = %d, want 1", store.updateCalls)
	}
	if store.lastUpdate.AccountID == nil || *store.lastUpdate.AccountID == nil || **store.lastUpdate.AccountID != acct.Id {
		t.Fatalf("Update AccountID = %#v, want %s", store.lastUpdate.AccountID, acct.Id)
	}
	if got, _ := s.resolveChatAccountEnv(context.Background(), sess, chat); got["ANTHROPIC_API_KEY"] != "sk-default" {
		t.Fatalf("chat env = %v, want default account env", got)
	}
}

func TestDefaultAccountForChat_PersistsCrossAgentChatBinding(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	claudeAcct := mustAddClaude(t, s, "claude-work", []byte("blob"))
	codexAcct := mustAddCodex(t, s, "codex-work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-default"}},
		zerolog.Nop(),
	)
	chats := &accountEnvChatStoreFake{}
	s.agentChats = chats

	sess := &models.Session{ID: "sess-1", AgentName: "codex", AccountID: &codexAcct.Id}
	chat := &models.AgentChat{SessionID: sess.ID, AgentSessionID: "chat-1", AgentName: "claude"}
	accountID := s.defaultAccountIDForChat(context.Background(), sess, chat)
	if accountID != claudeAcct.Id {
		t.Fatalf("defaultAccountIDForChat = %q, want claude default %q", accountID, claudeAcct.Id)
	}
	s.persistDefaultAccountForChat(context.Background(), sess, chat, accountID)

	if chats.updateAccountCalls != 1 {
		t.Fatalf("UpdateAccountIDByAgentSessionID calls = %d, want 1", chats.updateAccountCalls)
	}
	if chats.lastAgentSessionID != chat.AgentSessionID {
		t.Fatalf("updated agent_session_id = %q, want %q", chats.lastAgentSessionID, chat.AgentSessionID)
	}
	if chats.lastAccountID == nil || *chats.lastAccountID != claudeAcct.Id {
		t.Fatalf("updated chat account_id = %v, want %s", chats.lastAccountID, claudeAcct.Id)
	}
	if chat.AccountID == nil || *chat.AccountID != claudeAcct.Id {
		t.Fatalf("chat AccountID = %v, want %s", chat.AccountID, claudeAcct.Id)
	}
}

func TestDefaultAccountForChat_HonorsExplicitAccountZero(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-default"}},
		zerolog.Nop(),
	)
	accountZero := ""
	store := &lifecycleSessionStoreFake{session: &models.Session{
		ID:        "sess-1",
		AgentName: "claude",
		AccountID: &accountZero,
		CreatedAt: time.Now(),
	}}
	s.sessions = store

	sess := store.session
	chat := &models.AgentChat{SessionID: sess.ID, AgentName: "claude"}
	accountID := s.defaultAccountIDForChat(context.Background(), sess, chat)
	if accountID != "" {
		t.Fatalf("defaultAccountIDForChat = %q, want empty for explicit account 0", accountID)
	}
	s.persistDefaultAccountForChat(context.Background(), sess, chat, accountID)

	if store.updateCalls != 0 {
		t.Fatalf("Update calls = %d, want 0", store.updateCalls)
	}
	if sess.AccountID == nil || *sess.AccountID != "" {
		t.Fatalf("session AccountID = %v, want present-empty account 0", sess.AccountID)
	}
	if got, _ := s.resolveChatAccountEnv(context.Background(), sess, chat); got != nil {
		t.Fatalf("chat env = %v, want nil for explicit account 0", got)
	}
}

func TestDefaultAccountForChat_BackfillsNeverBoundSession(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-default"}},
		zerolog.Nop(),
	)
	store := &lifecycleSessionStoreFake{session: &models.Session{
		ID:        "sess-1",
		AgentName: "claude",
		CreatedAt: time.Now(),
	}}
	s.sessions = store

	sess := store.session
	chat := &models.AgentChat{SessionID: sess.ID, AgentName: "claude"}
	accountID := s.defaultAccountIDForChat(context.Background(), sess, chat)
	if accountID != acct.Id {
		t.Fatalf("defaultAccountIDForChat = %q, want %q for never-bound session", accountID, acct.Id)
	}
	s.persistDefaultAccountForChat(context.Background(), sess, chat, accountID)

	if store.updateCalls != 1 {
		t.Fatalf("Update calls = %d, want 1", store.updateCalls)
	}
	if sess.AccountID == nil || *sess.AccountID != acct.Id {
		t.Fatalf("session AccountID = %v, want %s", sess.AccountID, acct.Id)
	}
	if got, _ := s.resolveChatAccountEnv(context.Background(), sess, chat); got["ANTHROPIC_API_KEY"] != "sk-default" {
		t.Fatalf("chat env = %v, want default account env", got)
	}
}

// TestMergeManagedOverAccount_Precedence proves managed keys win over account
// keys, and that a nil/empty account map returns managed byte-identical (the
// account-0 / no-resolver no-op path the SessionEnv line relies on).
func TestMergeManagedOverAccount_Precedence(t *testing.T) {
	managed := map[string]string{"BOSS_SESSION_ID": "s1", "SHARED": "managed-wins"}
	acctEnv := map[string]string{"ANTHROPIC_API_KEY": "sk-x", "SHARED": "account-loses"}

	merged := mergeManagedOverAccount(managed, acctEnv)
	if merged["BOSS_SESSION_ID"] != "s1" {
		t.Errorf("managed key dropped: %v", merged)
	}
	if merged["ANTHROPIC_API_KEY"] != "sk-x" {
		t.Errorf("account key not merged: %v", merged)
	}
	if merged["SHARED"] != "managed-wins" {
		t.Errorf("SHARED = %q, want managed to win over account", merged["SHARED"])
	}

	// Nil account map returns managed unchanged (same reference — no copy).
	if got := mergeManagedOverAccount(managed, nil); !reflect.DeepEqual(got, managed) {
		t.Errorf("nil account merge = %v, want byte-identical %v", got, managed)
	}
	if got := mergeManagedOverAccount(managed, map[string]string{}); !reflect.DeepEqual(got, managed) {
		t.Errorf("empty account merge = %v, want byte-identical %v", got, managed)
	}
}
