package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"

	"github.com/recurser/bossd/internal/domain"
)

func TestBuildTimeline_ChatSequence_Sessions_AC212801(t *testing.T) {
	startError := "internal startup error must stay private"
	reclaimed := at(19)
	f := timelineFacts{SessionTitle: "session", Runs: []domain.AgentRun{{StartedAt: at(4)}}, Chats: []*domain.AgentChat{
		{ID: "v2", Role: domain.AgentChatRoleVerify, CreatedAt: at(20), Seq: 5, ReclaimedAt: &reclaimed},
		{ID: "r", Role: domain.AgentChatRoleRepair, Title: "repair: checks", CreatedAt: at(15), StartError: &startError, ReclaimedAt: &reclaimed},
		{ID: "v", Role: domain.AgentChatRoleVerify, CreatedAt: at(10)},
		{ID: "b", Role: domain.AgentChatRoleBuild, Title: "session", CreatedAt: at(4)},
		{ID: "operator", Role: domain.AgentChatRoleOperator, CreatedAt: at(25)},
		{ID: "legacy", CreatedAt: at(26)},
	}}
	assertSteps(t, buildTimeline(f), []wantStep{
		{at(4), "Build started", "", current},
		{at(4), "build", "", done},
		{at(10), "verify", "", done},
		{at(15), "repair: checks", "failed to start", failed},
		{at(20), "verify 2", "replaced", done},
	})
}

func TestBuildTimeline_ChatlessVerify_Sessions_AC212801(t *testing.T) {
	startError := "failed"
	f := timelineFacts{HasPR: true, Runs: []domain.AgentRun{{StartedAt: at(1)}}, HeadStarts: []domain.CheckSnapshotHeadStart{
		{HeadSHA: "one", FirstPolledAt: at(5), VerifyStartedAt: at(8)},
		{HeadSHA: "two", FirstPolledAt: at(10), VerifyStartedAt: at(13)},
		{HeadSHA: "three", FirstPolledAt: at(20)},
	}, Chats: []*domain.AgentChat{{ID: "v", Role: domain.AgentChatRoleVerify, CreatedAt: at(12), StartError: &startError}}}
	assertSteps(t, buildTimeline(f), []wantStep{
		{at(1), "Build started", "", current},
		{at(8), "verify", "no chat", done},
		{at(12), "verify 2", "failed to start", failed},
	})
}

func TestTimelineChatLabelsUseSequenceForTies(t *testing.T) {
	f := timelineFacts{Chats: []*domain.AgentChat{
		{ID: "later", Role: domain.AgentChatRoleVerify, CreatedAt: at(4), Seq: 2},
		{ID: "earlier", Role: domain.AgentChatRoleVerify, CreatedAt: at(4), Seq: 1},
	}}
	assertSteps(t, buildTimeline(f), []wantStep{{at(4), "verify", "", done}, {at(4), "verify 2", "", done}})
}

func TestBuildTimeline_ChatlessBeforeVerifying_Sessions_AC212801(t *testing.T) {
	f := timelineFacts{HasPR: true,
		HeadStarts: []domain.CheckSnapshotHeadStart{{HeadSHA: timelineHead, FirstPolledAt: at(1), VerifyStartedAt: at(5)}},
		Snapshots:  []domain.CheckSnapshot{{HeadSHA: timelineHead, PolledAt: at(5), ComputedStatus: int(vcs.DisplayStatusVerifying)}},
	}
	assertSteps(t, buildTimeline(f), []wantStep{{at(5), "verify", "no chat", done}, {at(5), "Verifying", "", current}})
}

func TestGetSessionTimeline_ChatSequence_Sessions_AC212801(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos, sessions := db.NewRepoStore(sqlDB), db.NewSessionStore(sqlDB)
	chats, snapshots := db.NewAgentChatStore(sqlDB), db.NewCheckSnapshotStore(sqlDB)
	repo, err := repos.Create(ctx, db.CreateRepoParams{DisplayName: "timeline", LocalPath: "/tmp/timeline", OriginURL: "https://github.com/acme/timeline.git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt"})
	if err != nil {
		t.Fatal(err)
	}
	pr := 1
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "timeline", BranchName: "feat/timeline", BaseBranch: "main", PRNumber: &pr})
	if err != nil {
		t.Fatal(err)
	}
	firstPoll := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	if err := snapshots.Create(ctx, db.CheckSnapshot{SessionID: sess.ID, HeadSHA: "old", PolledAt: firstPoll, ComputedStatus: int(vcs.DisplayStatusVerifying)}); err != nil {
		t.Fatal(err)
	}
	if err := snapshots.Create(ctx, db.CheckSnapshot{SessionID: sess.ID, HeadSHA: timelineHead, PolledAt: firstPoll.Add(time.Second), ComputedStatus: int(vcs.DisplayStatusChecking)}); err != nil {
		t.Fatal(err)
	}
	want := []wantStep{{firstPoll, "verify", "no chat", done}}
	for i, role := range []domain.AgentChatRole{domain.AgentChatRoleBuild, domain.AgentChatRoleVerify, domain.AgentChatRoleRepair, domain.AgentChatRoleVerify} {
		title := string(role)
		if role == domain.AgentChatRoleRepair {
			title = "repair: checks"
		}
		chat, err := chats.Create(ctx, db.CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: fmt.Sprintf("agent-%d", i), Role: role, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		label := []string{"build", "verify 2", "repair: checks", "verify 3"}[i]
		want = append(want, wantStep{chat.CreatedAt, label, "", done})
	}
	srv := New(Config{Repos: repos, Sessions: sessions, AgentChats: chats, CheckSnapshots: snapshots})
	response, err := srv.GetSessionTimeline(ctx, connect.NewRequest(&pb.GetSessionTimelineRequest{SessionId: sess.ID}))
	if err != nil {
		t.Fatal(err)
	}
	assertSteps(t, response.Msg.GetSteps(), want)
}
