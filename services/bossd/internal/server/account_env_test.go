package server

import (
	"context"
	"reflect"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
)

// fakeMaterializer implements account.Materializer, returning a fixed env for
// any account so the server's resolveAccountEnv can be exercised end-to-end.
type fakeMaterializer struct {
	supports bool
	env      map[string]string
}

func (m *fakeMaterializer) SupportsRotation(_ context.Context, _ string) (bool, error) {
	return m.supports, nil
}

func (m *fakeMaterializer) MaterializeAccount(_ context.Context, _ string) (map[string]string, error) {
	return m.env, nil
}

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
	got := s.resolveAccountEnv(context.Background(), bound)
	if got["ANTHROPIC_API_KEY"] != "sk-bound" {
		t.Errorf("bound session env = %v, want ANTHROPIC_API_KEY=sk-bound", got)
	}

	// Account 0 (nil AccountID): no env, no materializer call.
	unbound := &models.Session{AgentName: "claude"}
	if got := s.resolveAccountEnv(context.Background(), unbound); got != nil {
		t.Errorf("account-0 env = %v, want nil", got)
	}

	// Nil resolver degrades to nil.
	s.resolver = nil
	if got := s.resolveAccountEnv(context.Background(), bound); got != nil {
		t.Errorf("nil-resolver env = %v, want nil", got)
	}

	// Nil session degrades to nil (never panics).
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())
	if got := s.resolveAccountEnv(context.Background(), nil); got != nil {
		t.Errorf("nil-session env = %v, want nil", got)
	}
}

// TestResolveChatAccountEnv_CrossAgent proves the chat spawn resolves account
// env for the CHAT's agent, not the parent session's: a same-agent chat inherits
// the session's binding, a cross-agent chat with no binding of its own falls back
// to account 0 (never another provider's credentials), and an explicit
// chat-level binding wins.
func TestResolveChatAccountEnv_CrossAgent(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		&fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-bound"}},
		zerolog.Nop(),
	)
	claudeSession := &models.Session{AccountID: &acct.Id, AgentName: "claude"}

	// Same-agent chat: inherits the session's claude binding.
	sameAgent := &models.AgentChat{AgentName: "claude"}
	if got := s.resolveChatAccountEnv(context.Background(), claudeSession, sameAgent); got["ANTHROPIC_API_KEY"] != "sk-bound" {
		t.Errorf("same-agent chat env = %v, want ANTHROPIC_API_KEY=sk-bound", got)
	}

	// Cross-agent chat (codex chat in a claude-bound session), no chat binding:
	// falls back to account 0 so the claude account env is never injected.
	crossAgent := &models.AgentChat{AgentName: "codex"}
	if got := s.resolveChatAccountEnv(context.Background(), claudeSession, crossAgent); got != nil {
		t.Errorf("cross-agent chat env = %v, want nil (account 0)", got)
	}

	// Explicit chat-level binding wins even when the chat runs its own agent.
	boundChat := &models.AgentChat{AgentName: "claude", AccountID: &acct.Id}
	if got := s.resolveChatAccountEnv(context.Background(), &models.Session{AgentName: "claude"}, boundChat); got["ANTHROPIC_API_KEY"] != "sk-bound" {
		t.Errorf("chat-bound env = %v, want ANTHROPIC_API_KEY=sk-bound", got)
	}

	// Nil chat / nil resolver degrade to nil (never panics).
	if got := s.resolveChatAccountEnv(context.Background(), claudeSession, nil); got != nil {
		t.Errorf("nil-chat env = %v, want nil", got)
	}
	s.resolver = nil
	if got := s.resolveChatAccountEnv(context.Background(), claudeSession, sameAgent); got != nil {
		t.Errorf("nil-resolver env = %v, want nil", got)
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
