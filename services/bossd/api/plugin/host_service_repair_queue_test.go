package plugin

import (
	"context"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

func strPtr(s string) *string { return &s }

func newListSessionsFixture(t *testing.T, chats []*domain.AgentChat) *HostServiceServer {
	t.Helper()
	const repoID = "repo-1"
	const sessID = "sess-1"
	srv := newTestHostServiceServer(&mockVCSProvider{})
	srv.repoStore = &fakeRepoStore{
		repos: []*models.Repo{{ID: repoID, DisplayName: "Test Repo"}},
	}
	srv.sessionStore = &fakeSessionStoreWithListActive{
		sessionsByRepo: map[string][]*models.Session{
			repoID: {{ID: sessID, RepoID: repoID, Title: "session under test"}},
		},
	}
	srv.displayTracker = status.NewDisplayTracker()
	srv.chatTracker = status.NewTracker()
	srv.repairLease = status.NewRepairLeaseManager()
	srv.agentChats = &fakeAgentChatStore{
		chatsBySession: map[string][]*domain.AgentChat{
			sessID: chats,
		},
	}
	return srv
}

func TestHostListSessionsLiveRepairChatIsRepairing(t *testing.T) {
	now := time.Now()
	reclaimed := now.Add(-time.Minute)

	cases := []struct {
		name              string
		chats             []*domain.AgentChat
		seedTracker       func(*status.Tracker)
		livePane          func(context.Context, string) bool
		wantHasActiveChat bool
		wantIsRepairing   bool
	}{
		{
			name: "warm_tracker_live_repair_chat_sets_display_is_repairing",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-1", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-1")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-1", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
			},
			wantHasActiveChat: true,
			wantIsRepairing:   true,
		},
		{
			name: "cold_tracker_live_repair_pane_survives_daemon_restart",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-cold", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-cold")},
			},
			livePane: func(_ context.Context, name string) bool {
				return name == "tmux-repair-cold"
			},
			wantHasActiveChat: true,
			wantIsRepairing:   true,
		},
		{
			name: "cold_tracker_dead_repair_pane_does_not_count",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-dead", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-dead")},
			},
			livePane: func(_ context.Context, _ string) bool {
				return false
			},
			wantHasActiveChat: false,
			wantIsRepairing:   false,
		},
		{
			name: "freshly_idle_finished_repair_chat_does_not_set_display_is_repairing",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-finished", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-finished")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-finished", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, now)
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: true,
			wantIsRepairing:   false,
		},
		{
			name: "fresh_question_repair_chat_does_not_set_display_is_repairing",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-question", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-question")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-question", bossanovav1.ChatStatus_CHAT_STATUS_QUESTION, now)
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: true,
			wantIsRepairing:   false,
		},
		{
			name: "displaceable_idle_repair_chat_does_not_count",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-idle", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-idle")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-idle", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, now.Add(-10*time.Minute))
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: false,
			wantIsRepairing:   false,
		},
		{
			name: "displaceable_stopped_repair_chat_does_not_count",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-stopped", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-stopped")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-stopped", bossanovav1.ChatStatus_CHAT_STATUS_STOPPED, now)
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: false,
			wantIsRepairing:   false,
		},
		{
			name: "retired_reclaimed_repair_chat_does_not_count",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-retired", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-retired"), ReclaimedAt: &reclaimed},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-retired", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: false,
			wantIsRepairing:   false,
		},
		{
			name: "retired_start_error_repair_chat_does_not_count",
			chats: []*domain.AgentChat{
				{AgentSessionID: "repair-failed", SessionID: "sess-1", Role: domain.AgentChatRoleRepair, TmuxSessionName: strPtr("tmux-repair-failed"), StartError: strPtr("boom")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("repair-failed", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
			},
			livePane: func(_ context.Context, _ string) bool {
				return true
			},
			wantHasActiveChat: false,
			wantIsRepairing:   false,
		},
		{
			name: "live_operator_chat_does_not_set_display_is_repairing",
			chats: []*domain.AgentChat{
				{AgentSessionID: "op-1", SessionID: "sess-1", Role: domain.AgentChatRoleOperator, TmuxSessionName: strPtr("tmux-op-1")},
			},
			seedTracker: func(tr *status.Tracker) {
				tr.Update("op-1", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
			},
			wantHasActiveChat: true,
			wantIsRepairing:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newListSessionsFixture(t, tc.chats)
			if tc.seedTracker != nil {
				tc.seedTracker(srv.chatTracker)
			}
			if tc.livePane != nil {
				srv.SetLivePaneChecker(tc.livePane)
			}

			resp, err := srv.ListSessions(t.Context(), &bossanovav1.HostServiceListSessionsRequest{})
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			if len(resp.GetSessions()) != 1 {
				t.Fatalf("got %d sessions, want 1", len(resp.GetSessions()))
			}
			got := resp.GetSessions()[0]
			if got.GetHasActiveChat() != tc.wantHasActiveChat {
				t.Errorf("HasActiveChat = %v, want %v", got.GetHasActiveChat(), tc.wantHasActiveChat)
			}
			if got.GetDisplayIsRepairing() != tc.wantIsRepairing {
				t.Errorf("DisplayIsRepairing = %v, want %v", got.GetDisplayIsRepairing(), tc.wantIsRepairing)
			}
			if got.GetRepairActive() {
				t.Errorf("RepairActive = true, want false (lease manager is empty)")
			}
		})
	}
}

func TestSetRepairStatusQueued(t *testing.T) {
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	cur := base
	srv := newListSessionsFixture(t, nil)
	srv.repairLease = status.NewRepairLeaseManager().WithClock(time.Hour, func() time.Time { return cur })

	const sessID = "sess-1"
	ctx := t.Context()

	if _, err := srv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessID,
		IsQueued:  true,
	}); err != nil {
		t.Fatalf("SetRepairStatus(is_queued=true): %v", err)
	}
	if srv.repairLease.Active(sessID) {
		t.Fatal("is_queued must not acquire a repair lease")
	}
	if e := srv.displayTracker.Get(sessID); e != nil && e.IsRepairing {
		t.Fatal("is_queued must not set display_is_repairing")
	}

	resp, err := srv.ListSessions(ctx, &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	row := resp.GetSessions()[0]
	if row.GetRepairActive() || row.GetDisplayIsRepairing() {
		t.Fatalf("queued session flags = (repair_active=%v, display_is_repairing=%v), want (false, false)", row.GetRepairActive(), row.GetDisplayIsRepairing())
	}
	if row.GetRepairQueuedAt() == nil || !row.GetRepairQueuedAt().AsTime().Equal(base) {
		t.Fatalf("repair_queued_at = %v, want %v", row.GetRepairQueuedAt(), base)
	}

	cur = base.Add(3 * time.Minute)
	if _, err := srv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessID,
		IsQueued:  true,
	}); err != nil {
		t.Fatalf("refresh SetRepairStatus(is_queued=true): %v", err)
	}
	cur = base.Add(7 * time.Minute)
	resp, err = srv.ListSessions(ctx, &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions after refresh: %v", err)
	}
	if got := resp.GetSessions()[0].GetRepairQueuedAt(); got == nil || !got.AsTime().Equal(base) {
		t.Fatalf("repair_queued_at after refresh = %v, want %v", got, base)
	}

	if _, err := srv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId:    sessID,
		IsRepairing:  true,
		DispatcherId: "plugin-A",
	}); err != nil {
		t.Fatalf("SetRepairStatus(is_repairing=true): %v", err)
	}
	if _, err := srv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessID,
		IsQueued:  true,
	}); err != nil {
		t.Fatalf("SetRepairStatus(is_queued=true) while leased: %v", err)
	}
	if !srv.repairLease.Active(sessID) {
		t.Fatal("is_queued must not release an existing lease")
	}
	if e := srv.displayTracker.Get(sessID); e == nil || !e.IsRepairing {
		t.Fatal("is_queued must not clear an existing display_is_repairing flag")
	}
	resp, err = srv.ListSessions(ctx, &bossanovav1.HostServiceListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions while leased: %v", err)
	}
	row = resp.GetSessions()[0]
	if !row.GetRepairActive() {
		t.Fatal("expected repair_active=true while leased")
	}
	if row.GetRepairQueuedAt() != nil {
		t.Fatalf("repair_queued_at while leased = %v, want nil", row.GetRepairQueuedAt())
	}
}
