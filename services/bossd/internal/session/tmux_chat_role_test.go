package session

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"errors"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossd/internal/adapter/tmux"
)

func TestReclaimRepairChat_ReclaimsAgentOwnedChatWhateverItsTitle(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		title string
		role  domain.AgentChatRole
	}{
		{"Repair pull request", domain.AgentChatRoleRepair},
		{"Build next planned ticket", domain.AgentChatRoleBuild},
		{"再確認する PR #3100 とセッション状態", domain.AgentChatRoleVerify},
	} {
		for name, reclaim := range map[string]func(*Lifecycle, context.Context, string, string, string) (ReclaimRepairChatResult, error){
			"reclaim": (*Lifecycle).ReclaimRepairChat,
			"replace": (*Lifecycle).ReplaceBlockingChatForRepair,
		} {
			t.Run(name+"/"+string(tc.role), func(t *testing.T) {
				h := newStartTmuxChatHarness(t)
				tmuxName := "boss-test-agent-owned"
				h.chats.chatsBySession = map[string][]*domain.AgentChat{
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
	for _, role := range []domain.AgentChatRole{domain.AgentChatRoleOperator, domain.AgentChatRoleUnknown} {
		for name, reclaim := range map[string]func(*Lifecycle, context.Context, string, string, string) (ReclaimRepairChatResult, error){
			"reclaim": (*Lifecycle).ReclaimRepairChat,
			"replace": (*Lifecycle).ReplaceBlockingChatForRepair,
		} {
			t.Run(name+"/"+roleLabel(role), func(t *testing.T) {
				h := newStartTmuxChatHarness(t)
				tmuxName := "boss-test-human"
				h.chats.chatsBySession = map[string][]*domain.AgentChat{
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
	h.chats.chatsBySession = map[string][]*domain.AgentChat{
		"sess-1": {
			{ID: "c-op", SessionID: "sess-1", AgentSessionID: "agent-operator", Role: domain.AgentChatRoleOperator, TmuxSessionName: &operatorTmux},
			{ID: "c-prior", SessionID: "sess-1", AgentSessionID: prior, Role: domain.AgentChatRoleRepair, TmuxSessionName: &priorTmux},
		},
	}

	id, err := h.lc.StartTmuxChat(ctx, "sess-1",
		ChatInput{Command: "boss-repair", ResumeAgentSessionID: prior, Delivery: DeliverySubmit, Role: domain.AgentChatRoleRepair},
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

func TestStartTmuxChat_Sessions_AC103_AC104_RepairInstructionDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	for _, parent := range []string{"attended", "unattended"} {
		for _, support := range []bossanovav1.AppendSystemPromptSupport{
			bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
			bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
		} {
			t.Run(parent+"/"+support.String(), func(t *testing.T) {
				h := newStartTmuxChatHarness(t)
				h.lc.SetHookPort(54321)
				sess := h.sessions.sessions["sess-1"]
				sess.AgentName = "codex"
				sess.IsTmuxUnattended = parent == "unattended"
				h.lc.SetAgents(map[string]agent.RunnerClient{"codex": h.agentFake})
				h.agentFake.ConsumesInitialInput = true
				h.agentFake.AppendSystemPromptSupport = support
				var logs bytes.Buffer
				h.lc.logger = zerolog.New(&logs)
				_, err := h.lc.StartTmuxChat(t.Context(), "sess-1",
					ChatInput{Command: "boss-repair", Delivery: DeliverySubmit, Role: domain.AgentChatRoleRepair},
					"Repair", HookOpts{Token: "tok", AutonomousRun: true})
				if err != nil {
					t.Fatal(err)
				}
				prompt := h.agentFake.LastBuildInteractiveCommand.GetAppendSystemPrompt()
				for _, want := range []string{"BOSS_SESSION_ID", sess.ID, cronAutonomyDirective, unattendedSubagentDirective} {
					if !strings.Contains(prompt, want) {
						t.Errorf("runner prompt missing %q", want)
					}
				}
				call := h.findCall("new-session")
				if call == nil || !slices.Contains(call.args, "BOSS_UNATTENDED=true") || !slices.Contains(call.args, "BOSS_CRON=true") {
					t.Errorf("autonomous launch missing unattended env: %+v", call)
				}
				if sess.IsTmuxUnattended != (parent == "unattended") {
					t.Error("launch changed parent lifecycle class")
				}
				count := 0
				for _, line := range bytes.Split(logs.Bytes(), []byte("\n")) {
					if len(line) == 0 {
						continue
					}
					var event struct {
						Message string   `json:"message"`
						Level   string   `json:"level"`
						Classes []string `json:"undeliveredClasses"`
					}
					if err := json.Unmarshal(line, &event); err != nil {
						t.Fatal(err)
					}
					if event.Message != "agent runner did not confirm bossd instructions reached argv" {
						continue
					}
					count++
					if event.Level != "error" || !slices.Equal(event.Classes, []string{InstructionClassSessionContext, InstructionClassAutonomyDirective}) {
						t.Errorf("unexpected delivery record: %+v", event)
					}
				}
				wantCount := 0
				if support == bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE {
					wantCount = 1
				}
				if count != wantCount {
					t.Errorf("undelivered records = %d, want %d", count, wantCount)
				}
			})
		}
	}
}

func TestStartTmuxChat_Sessions_AC104_AttendedOperatorKeepsAttendedInstructions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.agentFake.ConsumesInitialInput = true
	_, err := h.lc.StartTmuxChat(t.Context(), "sess-1",
		ChatInput{Command: "boss-repair", Delivery: DeliverySubmit, Role: domain.AgentChatRoleOperator},
		"Operator", HookOpts{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := h.agentFake.LastBuildInteractiveCommand.GetAppendSystemPrompt()
	if strings.Contains(prompt, cronAutonomyDirective) || strings.Contains(prompt, unattendedSubagentDirective) {
		t.Error("attended operator received autonomy directive")
	}
	call := h.findCall("new-session")
	if call == nil {
		t.Fatal("no new-session")
	}
	for _, value := range []string{"BOSS_UNATTENDED=true", "BOSS_CRON=true"} {
		if slices.Contains(call.args, value) {
			t.Errorf("attended operator received %s", value)
		}
	}
}
