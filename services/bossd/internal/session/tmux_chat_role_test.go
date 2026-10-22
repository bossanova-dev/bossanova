package session

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
)

func TestReclaimRepairChat_ReclaimsAgentOwnedChatWhateverItsTitle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		title string
		role  models.AgentChatRole
	}{
		{"Repair pull request", models.AgentChatRoleRepair},
		{"Build next planned ticket", models.AgentChatRoleBuild},
		{"再確認する PR #3100 とセッション状態", models.AgentChatRoleVerify},
	} {
		for name, reclaim := range map[string]func(*Lifecycle, context.Context, string, string, string) (ReclaimRepairChatResult, error){
			"reclaim": (*Lifecycle).ReclaimRepairChat,
			"replace": (*Lifecycle).ReplaceBlockingChatForRepair,
		} {
			t.Run(name+"/"+string(tc.role), func(t *testing.T) {
				h := newStartTmuxChatHarness(t)
				tmuxName := "boss-test-agent-owned"
				h.chats.chatsBySession = map[string][]*models.AgentChat{
					"sess-1": {{ID: "c1", SessionID: "sess-1", AgentSessionID: "agent-1", Title: tc.title, Role: tc.role, TmuxSessionName: &tmuxName}},
				}
				res, err := reclaim(h.lc, ctx, "sess-1", "agent-1", "auto-repair replacing idle chat")
				if err != nil {
					t.Fatalf("%s of an idle %s chat titled %q: %v", name, tc.role, tc.title, err)
				}
				if !res.Reclaimed || h.findCall("kill-session") == nil {
					t.Fatalf("result %+v, kill-session called %v; want the pane killed", res, h.findCall("kill-session") != nil)
				}
			})
		}
	}
}

func TestReclaimRepairChat_NeverKillsOperatorOrLegacyChat(t *testing.T) {
	ctx := context.Background()
	for _, role := range []models.AgentChatRole{models.AgentChatRoleOperator, models.AgentChatRoleUnknown} {
		for name, reclaim := range map[string]func(*Lifecycle, context.Context, string, string, string) (ReclaimRepairChatResult, error){
			"reclaim": (*Lifecycle).ReclaimRepairChat,
			"replace": (*Lifecycle).ReplaceBlockingChatForRepair,
		} {
			t.Run(name+"/"+roleLabel(role), func(t *testing.T) {
				h := newStartTmuxChatHarness(t)
				tmuxName := "boss-test-human"
				h.chats.chatsBySession = map[string][]*models.AgentChat{
					// The title a repair chat used to carry must not buy a kill.
					"sess-1": {{ID: "c1", SessionID: "sess-1", AgentSessionID: "agent-1", Title: "Repair: looks like mine", Role: role, TmuxSessionName: &tmuxName}},
				}
				_, err := reclaim(h.lc, ctx, "sess-1", "agent-1", "auto-repair replacing idle chat")
				if !errors.Is(err, ErrRepairChatNotReclaimable) {
					t.Fatalf("err = %v, want ErrRepairChatNotReclaimable", err)
				}
				if h.findCall("kill-session") != nil {
					t.Fatal("kill-session called on a chat repair may not reclaim")
				}
				chat, getErr := h.chats.GetByAgentSessionID(ctx, "agent-1")
				if getErr != nil {
					t.Fatalf("get: %v", getErr)
				}
				if chat.StartError != nil || chat.TmuxSessionName == nil {
					t.Fatalf("row changed (start_error %v, tmux %v); a refused reclaim leaves it alone", chat.StartError, chat.TmuxSessionName)
				}
			})
		}
	}
}

// A resume must land in its own live pane even when a newer chat is live too.
// Reporting the newer chat as the blocker would send a repair run that then
// starts beside it into relaunching a pane that is still running.
func TestStartTmuxChat_ResumePrefersItsOwnLivePaneOverANewerSibling(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	ctx := context.Background()
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.CommandPrefix = "$"
	h.agentFake.ConsumesInitialInput = true

	const prior = "agent-repair-prior"
	priorTmux := tmux.ChatSessionName("repo-abcdef12", prior)
	operatorTmux := tmux.ChatSessionName("repo-abcdef12", "agent-operator")
	h.chats.chatsBySession = map[string][]*models.AgentChat{
		"sess-1": {
			{ID: "c-op", SessionID: "sess-1", AgentSessionID: "agent-operator", Role: models.AgentChatRoleOperator, TmuxSessionName: &operatorTmux},
			{ID: "c-prior", SessionID: "sess-1", AgentSessionID: prior, Role: models.AgentChatRoleRepair, TmuxSessionName: &priorTmux},
		},
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: prior, Delivery: DeliverySubmit, Role: models.AgentChatRoleRepair},
		"Repair: demo", HookOpts{Token: "tok"})
	if err != nil {
		t.Fatalf("StartTmuxChat: %v; the resume target is live and must be resumed, not reported as blocked", err)
	}
	if id != prior {
		t.Fatalf("id = %q, want the resumed chat %q", id, prior)
	}
	if h.tmuxFake.hasSubcommand("new-session") {
		t.Fatal("a live resume target must not get a replacement pane")
	}
}
