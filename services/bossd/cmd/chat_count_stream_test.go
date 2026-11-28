package main

import (
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	server "github.com/recurser/bossd/api/daemon"
	pluginapi "github.com/recurser/bossd/api/plugin"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/prstack"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

func requireChatCounts(t *testing.T, sess *bossanovav1.Session, chats, working int32) {
	t.Helper()
	if sess.ChatCount == nil || sess.WorkingChatCount == nil {
		t.Fatalf("chat_count = %v, working_chat_count = %v, want both set", sess.ChatCount, sess.WorkingChatCount)
	}
	if sess.GetChatCount() != chats || sess.GetWorkingChatCount() != working {
		t.Fatalf("counts = %d chats, %d working; want %d, %d", sess.GetChatCount(), sess.GetWorkingChatCount(), chats, working)
	}
}

// Every recompute after a chat changes reaches bosso with the session's
// current counts, and a recompute that leaves them unchanged sends nothing:
// the event after the no-op is the next real change.
func TestChatCountRecomputer_PublishesOnlyWhenCountsMove(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	sessionID := "sess-counts"
	now := time.Now()
	chats := &fakeDispatcherChatStore{byAgentSessionID: map[string]*domain.AgentChat{
		"agent-a": {ID: "chat-a", SessionID: sessionID, AgentSessionID: "agent-a"},
	}}
	tracker := status.NewTracker()
	hydrator := &streamSessionHydrator{
		agentChats: chats,
		rawSessions: &fakeSessionStore{byID: map[string]*models.Session{
			sessionID: {ID: sessionID, State: machine.ImplementingPlan, CreatedAt: now, UpdatedAt: now},
		}},
		repos:             &fakeRepoStore{byID: map[string]*models.Repo{}},
		chatStatusTracker: tracker,
		rotationEvents:    fakeRotationEventStore{bySession: map[string][]db.RotationEvent{}},
		logger:            zerolog.Nop(),
	}
	display := &sweepRecomputerFake{}
	recompute := chatCountRecomputer{display: display, hydrator: hydrator, streamBus: bus, logger: zerolog.Nop()}

	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 1, 0)

	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	tracker.Update("agent-a", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, now)
	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 1, 1)

	chats.byAgentSessionID["agent-b"] = &domain.AgentChat{ID: "chat-b", SessionID: sessionID, AgentSessionID: "agent-b"}
	tracker.Update("agent-b", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, now)
	if err := recompute.Recompute(ctx, sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	requireChatCounts(t, requireSessionDelta(t, nextStreamEvent(t, events)), 2, 1)

	if got := len(display.sessions); got != 4 {
		t.Fatalf("display recomputes = %d, want 4", got)
	}
}

// A session whose chats cannot be listed reports no counts, never zero.
func TestStreamSessionHydrator_LeavesCountsAbsentWithoutChats(t *testing.T) {
	t.Parallel()

	sess := &bossanovav1.Session{Id: "sess-no-chats"}
	(&streamSessionHydrator{logger: zerolog.Nop()}).Hydrate(t.Context(), sess)
	if sess.ChatCount != nil || sess.WorkingChatCount != nil {
		t.Fatalf("chat_count = %v, working_chat_count = %v, want both absent", sess.ChatCount, sess.WorkingChatCount)
	}
}

func TestRepairQueueStreamDelta(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	bus := upstream.NewStreamBus(zerolog.Nop())
	defer bus.Close()
	events := bus.Subscribe(ctx)

	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	cur := base
	const headSHA = "abcdef12"
	prNum := 42
	no := false

	database := dbtest.New(t)
	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	chats := upstream.NewNotifyingAgentChatStore(db.NewAgentChatStore(database))

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo display",
		LocalPath:         "/tmp/repo-queued-stream",
		OriginURL:         "git@github.com:acme/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "queued stream",
		WorktreePath: "/tmp/wt/queued-stream",
		BranchName:   "feat/queued-stream",
		BaseBranch:   "main",
		PRNumber:     &prNum,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	state := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, sess.ID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatalf("update session state: %v", err)
	}
	if err := sessions.UpdateRepairDiagnostics(ctx, db.UpdateRepairDiagnosticsParams{
		SessionID:     sess.ID,
		StartedAt:     base,
		HeadSHA:       headSHA,
		DisplayStatus: int32(bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT),
	}); err != nil {
		t.Fatalf("update repair diagnostics: %v", err)
	}
	sessionID := sess.ID

	chatTracker := status.NewTracker()
	displayTracker := status.NewDisplayTracker()
	displayTracker.Set(sessionID, vcs.DisplayInfo{
		Status:    vcs.DisplayStatusConflict,
		Mergeable: &no,
		HeadSHA:   headSHA,
	})
	repairLease := status.NewRepairLeaseManager().WithClock(time.Hour, func() time.Time { return cur })

	srv := server.New(server.Config{
		Sessions:             sessions,
		Repos:                repos,
		AgentChats:           chats,
		ChatStatus:           chatTracker,
		DisplayTracker:       displayTracker,
		RepairLease:          repairLease,
		RepairWorkflowLoaded: func() bool { return true },
		Logger:               zerolog.Nop(),
	})

	hydrator := &streamSessionHydrator{
		agentChats:        chats,
		rawSessions:       sessions,
		repos:             repos,
		chatStatusTracker: chatTracker,
		displayTracker:    displayTracker,
		rotationEvents:    fakeRotationEventStore{bySession: map[string][]db.RotationEvent{}},
		attention:         srv.HydrateAttentionForStream,
		logger:            zerolog.Nop(),
	}

	w := &daemonWiring{
		rawSessions:             sessions,
		repos:                   repos,
		agentChats:              chats,
		chatStatusTracker:       chatTracker,
		displayTracker:          displayTracker,
		repairLease:             repairLease,
		displayComputer:         status.NewDisplayStatusComputer(sessions, displayTracker, chatTracker, chats, nil, zerolog.Nop()),
		wedgeActivityCache:      server.NewWedgeActivityCache(),
		evidenceReader:          evidence.NewReader(nil, zerolog.Nop()),
		prStacks:                prstack.NewDetector(nil, nil, zerolog.Nop()),
		resumeAttempts:          db.NewResumeAttemptStore(database),
		lifecycle:               &session.Lifecycle{},
		streamBus:               bus,
		streamHydrator:          hydrator,
		hydrateSessionForStream: hydrator.Hydrate,
	}
	w.wireStreamObservers()

	hostSrv := pluginapi.NewHostServiceServer(pluginapi.Config{
		DisplayTracker: displayTracker,
		RepairLease:    repairLease,
	})

	// Entering the repair queue publishes an UPDATED delta that suppresses the
	// finished-attempt "Merge conflict" FIX attention.
	if _, err := hostSrv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessionID,
		IsQueued:  true,
	}); err != nil {
		t.Fatalf("SetRepairStatus(is_queued=true): %v", err)
	}
	queuedDelta := requireSessionDelta(t, nextStreamEvent(t, events))
	if got := queuedDelta.GetAttention().GetKind(); got != bossanovav1.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
		t.Fatalf("queued delta attention = %v, want IN_PROGRESS", queuedDelta.GetAttention())
	}

	// An in-TTL refresh publishes no extra delta; the very next event on the
	// bus is the post-expiry delta below.
	cur = base.Add(2 * time.Minute)
	if _, err := hostSrv.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessionID,
		IsQueued:  true,
	}); err != nil {
		t.Fatalf("refresh SetRepairStatus(is_queued=true): %v", err)
	}

	// Leaving the queue (TTL expiry) publishes an UPDATED delta that restores
	// the finished-attempt "Merge conflict" FIX attention.
	cur = cur.Add(status.MinRepairQueueTTL)
	if _, ok := repairLease.QueuedSince(sessionID); ok {
		t.Fatal("expected queued mark to expire")
	}
	expiredDelta := requireSessionDelta(t, nextStreamEvent(t, events))
	if got := expiredDelta.GetAttention(); got.GetKind() != bossanovav1.AttentionKind_ATTENTION_KIND_FIX || got.GetSentence() != "Merge conflict" {
		t.Fatalf("expired delta attention = %v, want FIX %q", got, "Merge conflict")
	}
}
