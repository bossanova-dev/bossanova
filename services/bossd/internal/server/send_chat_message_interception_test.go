package server

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/session"
)

// fakeAccountSwitcher is the switchAccountFn seam spy: it records the switch
// params (so tests can assert Auto:false, the resolved target, and Force) and
// returns a canned result/error, letting the "/boss switch" interception be
// exercised without a real session.Lifecycle.
type fakeAccountSwitcher struct {
	mu     sync.Mutex
	calls  int
	params session.SwitchAccountParams
	result session.SwitchAccountResult
	err    error
}

func (f *fakeAccountSwitcher) switchFn(_ context.Context, p session.SwitchAccountParams) (session.SwitchAccountResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.params = p
	return f.result, f.err
}

func (f *fakeAccountSwitcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func sentCount(tmuxer *fakeTmuxClient) int {
	tmuxer.mu.Lock()
	defer tmuxer.mu.Unlock()
	return len(tmuxer.sentMessages)
}

// TestSendChatMessage_Intercepts_NamedSwitch is the core credit-free-switch
// guard: a single-line "/boss switch <label>" runs the account switch and
// returns its notice WITHOUT ever calling the tmux spawner's SendMessage (zero
// LLM calls). The switch runs on the manual path (Auto:false) with the resolved
// target account.
func TestSendChatMessage_Intercepts_NamedSwitch(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{result: session.SwitchAccountResult{Resumed: true, TargetLabel: "work", NoticeText: "switched to work — resumed"}}

	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		accounts: accountBindingStore{byProvider: map[models.AccountProvider][]*models.Account{
			models.AccountProviderClaude: {
				{ID: "acct-work", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
			},
		}},
		wakeHook:        wakeHook{spawner: tmuxer},
		switchAccountFn: switcher.switchFn,
		logger:          zerolog.Nop(),
	}

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch work",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Msg.Delivered {
		t.Error("Delivered = true, want false for an intercepted switch")
	}
	if resp.Msg.NoticeText != "switched to work — resumed" {
		t.Errorf("NoticeText = %q, want the switch notice", resp.Msg.NoticeText)
	}
	if got := switcher.callCount(); got != 1 {
		t.Fatalf("SwitchAccount calls = %d, want 1", got)
	}
	if switcher.params.Auto {
		t.Error("switch Auto = true, want false (manual/operator path)")
	}
	if switcher.params.TargetAccountID != "acct-work" {
		t.Errorf("TargetAccountID = %q, want resolved id %q", switcher.params.TargetAccountID, "acct-work")
	}
	if switcher.params.Force {
		t.Error("Force = true, want false (no --force given)")
	}
	// The credit-free promise: the message never reached the tmux spawner.
	if n := sentCount(tmuxer); n != 0 {
		t.Errorf("tmux SendMessage calls = %d, want 0 (message must never be delivered)", n)
	}
}

// TestSendChatMessage_Intercepts_ForceFlag threads "--force" into the switch.
func TestSendChatMessage_Intercepts_ForceFlag(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{result: session.SwitchAccountResult{NoticeText: "switched"}}

	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		accounts: accountBindingStore{byProvider: map[models.AccountProvider][]*models.Account{
			models.AccountProviderClaude: {
				{ID: "acct-work", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
			},
		}},
		wakeHook:        wakeHook{spawner: tmuxer},
		switchAccountFn: switcher.switchFn,
		logger:          zerolog.Nop(),
	}

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch work --force",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !switcher.params.Force {
		t.Error("Force = false, want true for '--force'")
	}
	if sentCount(tmuxer) != 0 {
		t.Error("tmux SendMessage must not be called for an intercepted switch")
	}
}

// TestSendChatMessage_Intercepts_MidTurnWithoutForce maps a mid-turn refusal to
// FailedPrecondition naming --force, and still never delivers the message.
func TestSendChatMessage_Intercepts_MidTurnWithoutForce(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{err: session.ErrChatMidTurn}

	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		accounts: accountBindingStore{byProvider: map[models.AccountProvider][]*models.Account{
			models.AccountProviderClaude: {
				{ID: "acct-work", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
			},
		}},
		wakeHook:        wakeHook{spawner: tmuxer},
		switchAccountFn: switcher.switchFn,
		logger:          zerolog.Nop(),
	}

	_, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch work",
		Submit:         true,
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "force") {
		t.Errorf("error %q must name --force as the remedy", err.Error())
	}
	if sentCount(tmuxer) != 0 {
		t.Error("message must not be delivered when the switch is refused")
	}
}

// TestSendChatMessage_Intercepts_UnnamedExcludesCurrent picks the next eligible
// account excluding the current one via the S1 util-aware resolver.
func TestSendChatMessage_Intercepts_UnnamedExcludesCurrent(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	a1 := mustAddClaude(t, s, "one", []byte("blob-1"))
	a2 := mustAddClaude(t, s, "two", []byte("blob-2"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", AccountID: &a1.Id}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{result: session.SwitchAccountResult{NoticeText: "switched"}}
	s.agentChats = &chatStoreFake{chat: chat}
	s.sessions = &sessionStoreFake{sess: sess}
	s.wakeHook = wakeHook{spawner: tmuxer}
	s.switchAccountFn = switcher.switchFn

	if _, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch",
		Submit:         true,
	})); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if switcher.callCount() != 1 {
		t.Fatalf("SwitchAccount calls = %d, want 1", switcher.callCount())
	}
	if switcher.params.TargetAccountID != a2.Id {
		t.Errorf("unnamed target = %q, want the OTHER account %q (current %q excluded)", switcher.params.TargetAccountID, a2.Id, a1.Id)
	}
	if sentCount(tmuxer) != 0 {
		t.Error("tmux SendMessage must not be called for an intercepted switch")
	}
}

// TestSendChatMessage_Intercepts_UnnamedNoAlternative returns a friendly notice
// (not an error, not a delivery) when the current account is the only one.
func TestSendChatMessage_Intercepts_UnnamedNoAlternative(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	only := mustAddClaude(t, s, "only", []byte("blob"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", AccountID: &only.Id}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{}
	s.agentChats = &chatStoreFake{chat: chat}
	s.sessions = &sessionStoreFake{sess: sess}
	s.wakeHook = wakeHook{spawner: tmuxer}
	s.switchAccountFn = switcher.switchFn

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch",
		Submit:         true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Msg.Delivered {
		t.Error("Delivered = true, want false")
	}
	if !strings.Contains(resp.Msg.NoticeText, "no other account") {
		t.Errorf("NoticeText = %q, want the no-alternative notice", resp.Msg.NoticeText)
	}
	if switcher.callCount() != 0 {
		t.Error("SwitchAccount must not be called when there is no other account")
	}
	if sentCount(tmuxer) != 0 {
		t.Error("message must not be delivered")
	}
}

// TestSendChatMessage_PrefillSwitchNotIntercepted proves the submit gate: a
// "/boss switch" sent with submit=false ("only prefill the composer") is NOT
// executed — it is staged into the pane like any other prefill, honoring the
// caller's explicit non-submit intent. The switch fires only on a submitted
// command.
func TestSendChatMessage_PrefillSwitchNotIntercepted(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	switcher := &fakeAccountSwitcher{}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	s.switchAccountFn = switcher.switchFn

	resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
		AgentSessionId: "agent-1",
		Message:        "/boss switch work",
		Submit:         false,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.Msg.Delivered {
		t.Error("Delivered = false, want true (a prefilled /boss switch is staged, not intercepted)")
	}
	if switcher.callCount() != 0 {
		t.Errorf("SwitchAccount calls = %d, want 0 (prefill must not execute a switch)", switcher.callCount())
	}
	if n := sentCount(tmuxer); n != 1 {
		t.Fatalf("tmux SendMessage calls = %d, want 1 (prefill delivered)", n)
	}
}

// TestSendChatMessage_PassesThroughNonSwitch proves the interception is scoped:
// a normal message, an embedded "/boss switch" in prose, and a multi-line
// message whose first line is "/boss switch" are ALL delivered to the agent and
// never trigger a switch.
func TestSendChatMessage_PassesThroughNonSwitch(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"plain-prose", "do the thing"},
		{"embedded-switch", "please /boss switch now"},
		{"multiline-switch", "/boss switch\nand more"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
			sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
			tmuxer := &fakeTmuxClient{available: true, hasSession: true}
			switcher := &fakeAccountSwitcher{}
			s := newSendMessageTestServer(t, chat, sess, tmuxer)
			s.switchAccountFn = switcher.switchFn

			resp, err := s.SendChatMessage(context.Background(), connect.NewRequest(&pb.SendChatMessageRequest{
				AgentSessionId: "agent-1",
				Message:        tc.msg,
				Submit:         true,
			}))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !resp.Msg.Delivered {
				t.Error("Delivered = false, want true (message must reach the agent)")
			}
			if switcher.callCount() != 0 {
				t.Errorf("SwitchAccount calls = %d, want 0 (not a control command)", switcher.callCount())
			}
			if n := sentCount(tmuxer); n != 1 {
				t.Fatalf("tmux SendMessage calls = %d, want 1 (message delivered)", n)
			}
		})
	}
}
