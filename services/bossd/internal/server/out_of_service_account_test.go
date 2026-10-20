package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

type sessionAccountWrites struct {
	db.SessionStore
	accountIDs []string
}

func (s *sessionAccountWrites) Update(_ context.Context, id string, p db.UpdateSessionParams) (*models.Session, error) {
	if p.AccountID != nil && *p.AccountID != nil {
		s.accountIDs = append(s.accountIDs, **p.AccountID)
	}
	return &models.Session{ID: id}, nil
}

// Waking a chat that runs on its session's account, after that account was
// disabled, moves the session to the default account. Panes started from the
// session's account read their bearer from the session, so leaving it on the
// disabled account would keep every one of them failing.
func TestResolveChatAccountEnvForSpawn_DisabledSessionAccountMovesTheSession(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	old := mustAddClaude(t, s, "devops", []byte("old-blob"))
	good := mustAddClaude(t, s, "hello", []byte("good-blob"))
	disabled := string(models.AgentAccountStatusDisabled)
	if _, err := s.UpdateAgentAccount(ctx, connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: old.Id, Status: &disabled})); err != nil {
		t.Fatalf("UpdateAgentAccount: %v", err)
	}
	mat := &fakeMaterializer{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	resolver := account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	s.resolver = resolver
	sessions := &sessionAccountWrites{}
	lc := session.NewLifecycle(sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())
	lc.SetAgentAccountGetter(accts.Get)
	lc.SetDefaultAgentAccountResolver(resolver.DefaultAgentAccountID)
	s.lifecycle = lc
	s.agentChats = &accountEnvChatStoreFake{}

	seed := old.Id
	sess := &models.Session{ID: "sess-1", AgentName: "claude", AccountID: &seed}
	chat := &models.AgentChat{SessionID: sess.ID, AgentSessionID: "agent-1", AgentName: "claude"}

	if _, err := s.resolveChatAccountEnvForSpawn(ctx, sess, chat, "", recordAccountUse); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(sessions.accountIDs) != 1 || sessions.accountIDs[0] != good.Id {
		t.Fatalf("session account writes = %v, want one write to %s", sessions.accountIDs, good.Id)
	}
	if sess.AccountID == nil || *sess.AccountID != good.Id {
		t.Errorf("session account = %v, want %s", sess.AccountID, good.Id)
	}
}

// Waking or attaching a chat whose account the operator disabled (through the
// same UpdateAgentAccount call `boss account update --status disabled` makes) runs
// it on the provider's default account and moves the chat's binding there.
// The read-only probe derives the same account but writes nothing.
func TestResolveChatAccountEnvForSpawn_DisabledChatAccountMovesToDefault(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	old := mustAddClaude(t, s, "devops", []byte("old-blob"))
	good := mustAddClaude(t, s, "hello", []byte("good-blob"))
	disabled := string(models.AgentAccountStatusDisabled)
	if _, err := s.UpdateAgentAccount(ctx, connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: old.Id, Status: &disabled})); err != nil {
		t.Fatalf("UpdateAgentAccount: %v", err)
	}
	mat := &fakeMaterializer{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	resolver := account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	s.resolver = resolver
	lc := &session.Lifecycle{}
	lc.SetAgentAccountGetter(accts.Get)
	lc.SetDefaultAgentAccountResolver(resolver.DefaultAgentAccountID)
	s.lifecycle = lc
	chats := &accountEnvChatStoreFake{}
	s.agentChats = chats

	sess := &models.Session{ID: "sess-1", AgentName: "claude"}
	bound := old.Id
	chat := &models.AgentChat{SessionID: sess.ID, AgentSessionID: "agent-1", AgentName: "claude", AccountID: &bound}

	if _, err := s.resolveChatAccountEnvForSpawn(ctx, sess, chat, "", skipAccountUseRecord); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if chats.updateAccountCalls != 0 {
		t.Fatalf("probe wrote the chat binding %d times, want 0", chats.updateAccountCalls)
	}

	if _, err := s.resolveChatAccountEnvForSpawn(ctx, sess, chat, "", recordAccountUse); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if len(mat.materialize) == 0 {
		t.Fatal("no credentials were materialized")
	}
	for _, id := range mat.materialize {
		if id != good.Id {
			t.Fatalf("materialized %v, want only the enabled account %s", mat.materialize, good.Id)
		}
	}
	if chats.updateAccountCalls != 1 || chats.lastAccountID == nil || *chats.lastAccountID != good.Id {
		t.Fatalf("chat binding writes = %d (last %v), want one write to %s", chats.updateAccountCalls, chats.lastAccountID, good.Id)
	}
}

// When the chat cannot be moved off its disabled account, the launch fails
// with the target account's label and the cause, and no ids.
func TestResolveChatAccountEnvForSpawn_FailedChatMoveReadsPlainly(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	old := mustAddClaude(t, s, "devops", []byte("old-blob"))
	mustAddClaude(t, s, "hello", []byte("good-blob"))
	disabled := string(models.AgentAccountStatusDisabled)
	if _, err := s.UpdateAgentAccount(ctx, connect.NewRequest(&pb.UpdateAgentAccountRequest{Id: old.Id, Status: &disabled})); err != nil {
		t.Fatalf("UpdateAgentAccount: %v", err)
	}
	mat := &fakeMaterializer{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
	resolver := account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	s.resolver = resolver
	lc := &session.Lifecycle{}
	lc.SetAgentAccountGetter(accts.Get)
	lc.SetDefaultAgentAccountResolver(resolver.DefaultAgentAccountID)
	lc.SetAgentAccountSwitchDeps(accts, nil, nil)
	s.lifecycle = lc
	s.agentChats = &accountEnvChatStoreFake{updateAccountErr: errors.New("database is locked")}

	bound := old.Id
	chat := &models.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-1", AgentName: "claude", AccountID: &bound}
	_, err := s.resolveChatAccountEnvForSpawn(ctx, &models.Session{ID: "sess-1", AgentName: "claude"}, chat, "", recordAccountUse)
	const want = "could not move this chat to hello: database is locked"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
