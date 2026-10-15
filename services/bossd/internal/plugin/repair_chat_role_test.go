package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// liveChats scripts a lifecycle whose session holds the given live chats,
// newest first, the way findLiveTmuxChat reports them: a launch without
// AllowSiblingChat is blocked by the first chat still alive, and a replaced
// chat stops being alive.
type liveChats struct {
	order    []string
	replaced map[string]bool
	launched []session.HookOpts
}

func (l *liveChats) start(_ context.Context, _ string, _ session.ChatInput, _ string, opts session.HookOpts) (string, error) {
	l.launched = append(l.launched, opts)
	if !opts.AllowSiblingChat {
		for _, id := range l.order {
			if !l.replaced[id] {
				return id, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active")
			}
		}
	}
	return "agent-repair-new", nil
}

func newRoleRepairServer(t *testing.T, chats map[string]*models.AgentChat, order []string) (*HostServiceServer, *fakeChatLifecycle, *liveChats) {
	t.Helper()
	live := &liveChats{order: order, replaced: map[string]bool{}}
	lc := &fakeChatLifecycle{startFunc: live.start}
	lc.replaceBlockingChatForRepairFunc = func(_ context.Context, _, agentSessionID, _ string) (session.ReclaimRepairChatResult, error) {
		live.replaced[agentSessionID] = true
		return session.ReclaimRepairChatResult{Reclaimed: true}, nil
	}
	srv := newChatRunTestServer(lc, &models.Session{ID: "sess-1", WorktreePath: "/tmp/wt"})
	srv.agentChats = &fakeAgentChatStore{chatsByAgent: chats}
	idleSince := time.Now().Add(-10 * time.Minute)
	for id := range chats {
		srv.chatTracker.Update(id, bossanovav1.ChatStatus_CHAT_STATUS_IDLE, idleSince)
	}
	return srv, lc, live
}

func repairReplaceRequest() *bossanovav1.StartChatRunHostRequest {
	return &bossanovav1.StartChatRunHostRequest{
		SessionId:             "sess-1",
		Command:               "boss-repair",
		Title:                 "Repair: demo",
		Role:                  bossanovav1.ChatRole_CHAT_ROLE_REPAIR,
		ReplaceExistingChat:   true,
		ReplaceExistingReason: "auto-repair replacing idle chat after 10m0s of silence; threshold 5m0s",
		ReplaceExistingObservedLastChatActivityAt: timestamppb.New(time.Now().Add(-10 * time.Minute)),
	}
}

func replacedIDs(lc *fakeChatLifecycle) []string {
	var ids []string
	for _, r := range lc.replaceBlockingChatForRepairReqs {
		ids = append(ids, r.agentSessionID)
	}
	return ids
}

func TestStartChatRun_RepairStartsBesideAnOperatorOrLegacyChat(t *testing.T) {
	for _, tc := range []struct {
		name string
		role models.AgentChatRole
	}{
		{"operator chat", models.AgentChatRoleOperator},
		{"legacy chat with no role", models.AgentChatRoleUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chats := map[string]*models.AgentChat{
				"agent-human": {AgentSessionID: "agent-human", SessionID: "sess-1", Title: "Build next planned ticket", Role: tc.role},
			}
			for _, req := range []*bossanovav1.StartChatRunHostRequest{
				repairReplaceRequest(),
				{SessionId: "sess-1", Command: "boss-repair", Title: "Repair: demo"},
			} {
				srv, lc, live := newRoleRepairServer(t, chats, []string{"agent-human"})
				resp, err := srv.StartChatRun(t.Context(), req)
				if err != nil {
					t.Fatalf("StartChatRun (replace=%v): %v; repair must not stall on a chat it may not kill", req.GetReplaceExistingChat(), err)
				}
				if resp.GetAgentSessionId() != "agent-repair-new" {
					t.Fatalf("AgentSessionId = %q, want the new repair chat", resp.GetAgentSessionId())
				}
				if got := replacedIDs(lc); len(got) != 0 {
					t.Fatalf("replaced %v; an operator or legacy chat is never killed by repair", got)
				}
				if n := len(live.launched); n != 2 || !live.launched[1].AllowSiblingChat {
					t.Fatalf("launches = %+v, want a refused launch then a sibling launch", live.launched)
				}
			}
		})
	}
}

func TestStartChatRun_RepairRefusesToStartBesideAChatTheTrackerCannotShowIdle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     models.AgentChatRole
		evidence func(*status.Tracker)
	}{
		{"cold tracker, operator chat", models.AgentChatRoleOperator, func(tr *status.Tracker) { tr.Remove("agent-human") }},
		{"cold tracker, legacy chat", models.AgentChatRoleUnknown, func(tr *status.Tracker) { tr.Remove("agent-human") }},
		{"operator still working", models.AgentChatRoleOperator, func(tr *status.Tracker) {
			tr.Update("agent-human", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		}},
		{"operator idle under the repair window", models.AgentChatRoleOperator, func(tr *status.Tracker) {
			tr.Update("agent-human", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-time.Minute))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chats := map[string]*models.AgentChat{
				"agent-human": {AgentSessionID: "agent-human", SessionID: "sess-1", Title: "Build next planned ticket", Role: tc.role},
			}
			for _, req := range []*bossanovav1.StartChatRunHostRequest{
				repairReplaceRequest(),
				{SessionId: "sess-1", Command: "boss-repair", Title: "Repair: demo"},
			} {
				srv, lc, live := newRoleRepairServer(t, chats, []string{"agent-human"})
				tc.evidence(srv.chatTracker)
				_, err := srv.StartChatRun(t.Context(), req)
				if grpcstatus.Code(err) != codes.FailedPrecondition {
					t.Fatalf("code = %v (%v), want FailedPrecondition so the next sweep retries", grpcstatus.Code(err), err)
				}
				for _, opts := range live.launched {
					if opts.AllowSiblingChat {
						t.Fatalf("launches = %+v; repair started beside a chat with no idle evidence", live.launched)
					}
				}
				if got := replacedIDs(lc); len(got) != 0 {
					t.Fatalf("replaced %v; an operator or legacy chat is never killed by repair", got)
				}
			}
		})
	}
}

func TestStartChatRun_RepairReplacesAgentOwnedChatsThenStartsBesideTheRest(t *testing.T) {
	// The delta session: an idle repair chat codex renamed, the original build
	// chat, and an operator chat. Repair clears the two agent-owned chats and
	// starts beside the operator's.
	chats := map[string]*models.AgentChat{
		"agent-repair-old": {AgentSessionID: "agent-repair-old", SessionID: "sess-1", Title: "Repair pull request", Role: models.AgentChatRoleRepair},
		"agent-build":      {AgentSessionID: "agent-build", SessionID: "sess-1", Title: "Build next planned ticket", Role: models.AgentChatRoleBuild},
		"agent-operator":   {AgentSessionID: "agent-operator", SessionID: "sess-1", Title: "New chat", Role: models.AgentChatRoleOperator},
	}
	srv, lc, live := newRoleRepairServer(t, chats, []string{"agent-repair-old", "agent-build", "agent-operator"})

	resp, err := srv.StartChatRun(t.Context(), repairReplaceRequest())
	if err != nil {
		t.Fatalf("StartChatRun: %v", err)
	}
	if resp.GetAgentSessionId() != "agent-repair-new" {
		t.Fatalf("AgentSessionId = %q, want agent-repair-new", resp.GetAgentSessionId())
	}
	got := replacedIDs(lc)
	if len(got) != 2 || got[0] != "agent-repair-old" || got[1] != "agent-build" {
		t.Fatalf("replaced %v, want [agent-repair-old agent-build]", got)
	}
	if last := live.launched[len(live.launched)-1]; !last.AllowSiblingChat {
		t.Fatalf("final launch %+v, want a sibling launch beside the operator chat", last)
	}
}

func TestFireSessionEvent_BlockRecordsTheGivenReason(t *testing.T) {
	const reason = "automatic repair failed 5 times in a row on 0123456789ab; last error: boom"
	for _, tc := range []struct {
		name, reason, want string
	}{
		{"given reason", reason, reason},
		{"no reason keeps the machine default", "", "max attempts reached"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sessions, id := newFireSessionEventServer(t, false)
			if _, err := srv.FireSessionEvent(t.Context(), &bossanovav1.FireSessionEventRequest{
				SessionId:     id,
				Event:         bossanovav1.SessionEvent_SESSION_EVENT_BLOCK,
				BlockedReason: tc.reason,
			}); err != nil {
				t.Fatalf("FireSessionEvent: %v", err)
			}
			stored, err := sessions.Get(t.Context(), id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if stored.BlockedReason == nil || !strings.HasPrefix(*stored.BlockedReason, tc.want) {
				t.Fatalf("blocked_reason = %v, want prefix %q", stored.BlockedReason, tc.want)
			}
		})
	}
}

func TestStartChatRun_UnauthorizedRepairLeavesAgentOwnedBlockerToReclaim(t *testing.T) {
	chats := map[string]*models.AgentChat{
		"agent-build": {AgentSessionID: "agent-build", SessionID: "sess-1", Title: "Build next planned ticket", Role: models.AgentChatRoleBuild},
	}
	srv, lc, _ := newRoleRepairServer(t, chats, []string{"agent-build"})

	_, err := srv.StartChatRun(t.Context(), &bossanovav1.StartChatRunHostRequest{
		SessionId: "sess-1", Command: "boss-repair", Title: "Repair: demo", Role: bossanovav1.ChatRole_CHAT_ROLE_REPAIR,
	})
	if grpcstatus.Code(err) != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists so the plugin reclaims through the guarded RPC", grpcstatus.Code(err))
	}
	if got := replacedIDs(lc); len(got) != 0 {
		t.Fatalf("replaced %v without the plugin's idle authorization", got)
	}
}
