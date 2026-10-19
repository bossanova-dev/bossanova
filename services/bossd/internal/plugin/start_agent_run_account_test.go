package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/session"
)

// accountMovingChatStore lets the lifecycle persist a chat's account move so the
// host's own read of the primary chat sees it.
type accountMovingChatStore struct {
	*fakeAgentChatStore
}

func (s accountMovingChatStore) UpdateAccountIDByAgentSessionID(_ context.Context, agentSessionID string, accountID *string) error {
	chat := s.chatsByAgent[agentSessionID]
	if chat == nil {
		return errors.New("chat not found")
	}
	chat.AccountID = accountID
	return nil
}

// bearerPerAccount materializes a credential named for the account it resolves.
type bearerPerAccount struct{}

func (bearerPerAccount) Resolve(_ context.Context, sess *models.Session) (map[string]string, error) {
	if sess.AccountID == nil {
		return nil, nil
	}
	return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "bearer-for-" + *sess.AccountID}, nil
}

// A repair run on a session whose primary chat is bound to a disabled account
// runs on the provider's default account, and the chat row moves there.
func TestStartAgentRun_DisabledChatAccountRunsOnDefault(t *testing.T) {
	client := newFakeAgentClient()
	client.startResp = "claude-repair"
	primary := "agent-1"
	srv := newRepairTestServer(client, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt", AgentSessionID: &primary})
	bound := "acct-devops"
	chats := accountMovingChatStore{&fakeAgentChatStore{chatsByAgent: map[string]*models.AgentChat{
		primary: {SessionID: "sess-1", AgentSessionID: primary, AgentName: "claude", AccountID: &bound},
	}}}
	srv.agentChats = chats
	srv.SetAccountEnvResolver(bearerPerAccount{})

	rows := map[string]*models.Account{
		"acct-devops": {ID: "acct-devops", Label: "devops", Provider: models.AccountProviderClaude, Status: models.AccountStatusDisabled, Health: models.AccountHealthOK},
		"acct-hello":  {ID: "acct-hello", Label: "hello", Provider: models.AccountProviderClaude, Status: models.AccountStatusActive, Health: models.AccountHealthOK},
	}
	lc := session.NewLifecycle(srv.sessionStore, nil, chats, nil, nil, nil, nil, nil, zerolog.Nop())
	lc.SetAccountGetter(func(_ context.Context, id string) (*models.Account, error) {
		if a, ok := rows[id]; ok {
			return a, nil
		}
		return nil, errors.New("account not found")
	})
	lc.SetDefaultAccountResolver(func(context.Context, string, time.Time) (string, error) { return "acct-hello", nil })
	srv.SetLifecycle(lc)

	if _, err := srv.StartAgentRun(t.Context(), &bossanovav1.StartAgentRunHostRequest{SessionId: "sess-1", Prompt: "fix this"}); err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	if got := client.startCall.extraEnv["CLAUDE_CODE_OAUTH_TOKEN"]; got != "bearer-for-acct-hello" {
		t.Errorf("repair run credential = %q, want bearer-for-acct-hello (the disabled account must not run)", got)
	}
	if got := chats.chatsByAgent[primary].AccountID; got == nil || *got != "acct-hello" {
		t.Errorf("chat account = %v, want acct-hello", got)
	}
}
