package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

// switchChatStoreFake is a minimal db.AgentChatStore for exercising the
// SwitchSessionAccount handler's request validation and primary-live-chat
// resolution — the paths that return before delegating to the Lifecycle.
type switchChatStoreFake struct {
	chats   []*models.AgentChat
	listErr error
}

func (f *switchChatStoreFake) Create(context.Context, db.CreateAgentChatParams) (*models.AgentChat, error) {
	return nil, nil
}

func (f *switchChatStoreFake) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	for _, c := range f.chats {
		if c != nil && c.AgentSessionID == agentSessionID {
			return c, nil
		}
	}
	return nil, nil
}

func (f *switchChatStoreFake) ListBySession(context.Context, string) ([]*models.AgentChat, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.chats, nil
}

func (f *switchChatStoreFake) ListBySessions(context.Context, []string) (map[string][]*models.AgentChat, error) {
	return nil, nil
}

func (f *switchChatStoreFake) UpdateTitle(context.Context, string, string) error { return nil }

func (f *switchChatStoreFake) UpdateTitleByAgentSessionID(context.Context, string, string) error {
	return nil
}

func (f *switchChatStoreFake) UpdateTmuxSessionName(context.Context, string, *string) error {
	return nil
}

func (f *switchChatStoreFake) UpdateProviderSessionID(context.Context, string, *string) error {
	return nil
}

func (f *switchChatStoreFake) UpdateAccountIDByAgentSessionID(context.Context, string, *string) error {
	return nil
}

func (f *switchChatStoreFake) MarkStartFailed(context.Context, string, string) error { return nil }

func (f *switchChatStoreFake) RebindResumedChat(context.Context, string, db.RebindResumedChatParams) error {
	return nil
}

func (f *switchChatStoreFake) DeleteByAgentSessionID(context.Context, string) error { return nil }

func (f *switchChatStoreFake) ListWithTmuxSession(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func (f *switchChatStoreFake) ListRoutableChats(context.Context) ([]*models.AgentChat, error) {
	return nil, nil
}

func TestSwitchSessionAccount_RequiresSessionID(t *testing.T) {
	srv := &Server{}
	_, err := srv.SwitchSessionAccount(context.Background(), connect.NewRequest(&pb.SwitchSessionAccountRequest{}))
	if err == nil {
		t.Fatal("expected error for empty session_id, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", got)
	}
}

func TestSwitchSessionAccount_NoLiveChatIsFailedPrecondition(t *testing.T) {
	// agent_session_id omitted + no chat carries a tmux session ⇒ nothing live
	// to switch. The handler must refuse before touching the Lifecycle.
	srv := &Server{
		agentChats: &switchChatStoreFake{
			chats: []*models.AgentChat{
				{AgentSessionID: "chat-headless", TmuxSessionName: nil},
			},
		},
	}
	_, err := srv.SwitchSessionAccount(context.Background(), connect.NewRequest(&pb.SwitchSessionAccountRequest{
		SessionId: "session-1",
	}))
	if err == nil {
		t.Fatal("expected error when session has no live chat, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
}

// TestSwitchSessionAccount_AuthInvalidTargetIsFailedPrecondition covers the
// race between the handler's own eligibility precheck and the Lifecycle's
// reload of the same target. resolveSessionAccount reads the account row at
// request time; the Lifecycle reads it again before touching the pane. Credential
// maintenance can bench the target in between, so the Lifecycle's refusal is a
// routine statement about the target's state and must be reported as a failed
// precondition — not as CodeInternal, which claims a daemon fault for a request
// the daemon handled exactly as designed (and which the hosted classifier, which
// maps only NotFound and FailedPrecondition, degrades further into an unmapped
// code).
func TestSwitchSessionAccount_AuthInvalidTargetIsFailedPrecondition(t *testing.T) {
	live := "boss-repo-live"
	switcher := &fakeAccountSwitcher{
		err: fmt.Errorf("%w: account %q; re-authenticate it before switching",
			session.ErrAccountAuthInvalid, "work"),
	}
	srv := &Server{
		agentChats: &switchChatStoreFake{
			chats: []*models.AgentChat{
				{AgentSessionID: "chat-live", TmuxSessionName: &live, AgentName: "claude"},
			},
		},
		// Healthy on this read: that is what lets the precheck pass and puts the
		// refusal where the reviewer's race puts it — inside the Lifecycle.
		accounts: accountBindingStore{byProvider: map[models.AccountProvider][]*models.Account{
			models.AccountProviderClaude: {
				{ID: "acct-work", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
			},
		}},
		switchAccountFn: switcher.switchFn,
		logger:          zerolog.Nop(),
	}

	_, err := srv.SwitchSessionAccount(context.Background(), connect.NewRequest(&pb.SwitchSessionAccountRequest{
		SessionId: "session-1",
		AccountId: "work",
	}))
	if err == nil {
		t.Fatal("expected an error when the target account is auth-invalid, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (CodeInternal reports a server fault for a routine target-state refusal)", got)
	}
}

func TestResolvePrimaryLiveChat_PicksMostRecentLive(t *testing.T) {
	older := "boss-repo-old"
	newer := "boss-repo-new"
	srv := &Server{
		agentChats: &switchChatStoreFake{
			chats: []*models.AgentChat{
				{AgentSessionID: "chat-headless", TmuxSessionName: nil, CreatedAt: time.Unix(300, 0)},
				{AgentSessionID: "chat-old", TmuxSessionName: &older, CreatedAt: time.Unix(100, 0)},
				{AgentSessionID: "chat-new", TmuxSessionName: &newer, CreatedAt: time.Unix(200, 0)},
			},
		},
	}
	got, err := srv.resolvePrimaryLiveChat(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("resolvePrimaryLiveChat: %v", err)
	}
	if got != "chat-new" {
		t.Fatalf("resolved chat = %q, want chat-new (most recently created live chat)", got)
	}
}

func TestSwitchTargetAgentName_PrefersChatAgent(t *testing.T) {
	// The switch validates the target account against the CHAT's provider (the
	// picker is scoped to it), not the parent session's agent.
	srv := &Server{
		agentChats: &switchChatStoreFake{
			chats: []*models.AgentChat{
				{AgentSessionID: "chat-codex", AgentName: "codex"},
			},
		},
	}
	got, err := srv.switchTargetAgentName(context.Background(), "session-1", "chat-codex")
	if err != nil {
		t.Fatalf("switchTargetAgentName: %v", err)
	}
	if got != "codex" {
		t.Fatalf("agent = %q, want codex (chat's own agent)", got)
	}
}

func TestSwitchTargetAgentName_ChatNotFound(t *testing.T) {
	srv := &Server{agentChats: &switchChatStoreFake{chats: nil}}
	_, err := srv.switchTargetAgentName(context.Background(), "session-1", "missing")
	if err == nil {
		t.Fatal("expected NotFound when the chat is missing, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", got)
	}
}

func TestResolvePrimaryLiveChat_NoLiveChat(t *testing.T) {
	srv := &Server{agentChats: &switchChatStoreFake{chats: nil}}
	_, err := srv.resolvePrimaryLiveChat(context.Background(), "session-1")
	if err == nil {
		t.Fatal("expected error when no live chat, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", got)
	}
}
