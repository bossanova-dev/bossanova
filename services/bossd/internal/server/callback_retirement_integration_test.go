package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

type retirementIntegration struct {
	sessions    db.SessionStore
	raw         *db.SQLiteSessionStore
	repos       *db.SQLiteRepoStore
	watches     *db.SQLiteGithubCallbackStore
	chats       *db.SQLiteAgentChatStore
	tracker     *status.Tracker
	display     *status.DisplayTracker
	computer    *status.DisplayStatusComputer
	retirer     *callback.Retirer
	child       *models.Session
	coordinator *models.Session
	repo        *models.Repo
}

func newRetirementIntegration(t *testing.T) *retirementIntegration {
	t.Helper()
	ctx := context.Background()
	database := setupServerTestDB(t)
	h := &retirementIntegration{raw: db.NewSessionStore(database), repos: db.NewRepoStore(database), watches: db.NewGithubCallbackStore(database), chats: db.NewAgentChatStore(database), tracker: status.NewTracker(), display: status.NewDisplayTracker()}
	var err error
	h.repo, err = h.repos.Create(ctx, db.CreateRepoParams{DisplayName: "retirement", LocalPath: "/tmp/retirement", OriginURL: "https://github.com/acme/widgets.git", DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/retirement-wt"})
	if err != nil {
		t.Fatal(err)
	}
	h.child, err = h.raw.Create(ctx, db.CreateSessionParams{RepoID: h.repo.ID, Title: "child", BranchName: "child", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	h.coordinator, err = h.raw.Create(ctx, db.CreateSessionParams{RepoID: h.repo.ID, Title: "coordinator", BranchName: "coordinator", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	pr := 7
	ppr := &pr
	state := int(machine.AwaitingChecks)
	if _, err := h.raw.Update(ctx, h.child.ID, db.UpdateSessionParams{State: &state, PRNumber: &ppr}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.raw.Update(ctx, h.coordinator.ID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []struct{ session, chat string }{{h.child.ID, "chat-build"}, {h.child.ID, "chat-verify"}, {h.coordinator.ID, "chat-coord"}} {
		if _, err := h.chats.Create(ctx, db.CreateAgentChatParams{SessionID: chat.session, AgentSessionID: chat.chat}); err != nil {
			t.Fatal(err)
		}
		h.tracker.Update(chat.chat, pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	}
	h.computer = status.NewDisplayStatusComputer(h.raw, h.display, h.tracker, h.chats, db.NewWorkflowStore(database), zerolog.Nop())
	h.computer.SetWaitingLookup(status.WaitingLookupFunc(func(ctx context.Context, id string) (string, error) {
		return callback.WaitingReasonForChat(ctx, h.watches, id, time.Now())
	}))
	h.display.SetRecomputer(h.computer)
	h.retirer = callback.NewRetirer(h.watches, h.chats, h.raw, h.repos, h.computer, time.Now, zerolog.Nop())
	h.sessions = db.NewRecomputingSessionStore(h.raw, h.computer).WithTransitionObserver(db.TransitionObservers{h.retirer})
	return h
}
func (h *retirementIntegration) arm(t *testing.T, chat string, pr int, trigger models.GithubCallbackTrigger) *models.GithubCallback {
	t.Helper()
	cb, _, err := h.watches.Create(context.Background(), db.CreateGithubCallbackParams{TargetChatID: chat, RepoOwner: "acme", RepoName: "widgets", PRNumber: pr, Trigger: trigger, Message: "watch " + string(trigger)})
	if err != nil {
		t.Fatal(err)
	}
	return cb
}
func (h *retirementIntegration) expectState(t *testing.T, cb *models.GithubCallback, want models.GithubCallbackState) {
	t.Helper()
	got, err := h.watches.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != want {
		t.Fatalf("%s callback %s state = %s, want %s", cb.TargetChatID, cb.Trigger, got.State, want)
	}
}
func (h *retirementIntegration) expectWaiting(t *testing.T, chat string, want bool) {
	t.Helper()
	reason, err := callback.WaitingReasonForChat(context.Background(), h.watches, chat, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if (reason != "") != want {
		t.Fatalf("chat %s waiting reason = %q, want waiting %v", chat, reason, want)
	}
}

// The production worker scans immediately, then waits. Join it after the stored
// terminal outcome, rather than exposing a scan API solely for this package.
func (h *retirementIntegration) deliverUntil(t *testing.T, cb *models.GithubCallback, want models.GithubCallbackState, deliver callback.ChatDeliverer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	worker := callback.NewDeliveryWorker(callback.WorkerConfig{Store: h.watches, Deliverer: deliver, Gate: h.retirer, PollInterval: time.Hour, Logger: zerolog.Nop()})
	done := safego.Go(zerolog.Nop(), func() { worker.Run(ctx) })
	defer func() { cancel(); <-done }()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		got, err := h.watches.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == want && (want != models.GithubCallbackStateCanceled || h.tracker.Waiting(cb.TargetChatID) == "") {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("delivery state = %s, want %s", got.State, want)
		case <-tick.C:
		}
	}
}
func TestCallbackRetirement_HumanMergeSettlesAndArchives(t *testing.T) {
	ctx := context.Background()
	h := newRetirementIntegration(t)
	merge := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerMerged)
	failed := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerChecksFailed)
	verify := h.arm(t, "chat-verify", 7, models.GithubCallbackTriggerChecksPassedReady)
	coordMerge := h.arm(t, "chat-coord", 7, models.GithubCallbackTriggerMerged)
	coordFailed := h.arm(t, "chat-coord", 7, models.GithubCallbackTriggerChecksFailed)
	for _, chat := range []string{"chat-build", "chat-verify", "chat-coord"} {
		h.expectWaiting(t, chat, true)
	}
	ready := int(machine.ReadyForReview)
	if _, err := h.sessions.Update(ctx, h.child.ID, db.UpdateSessionParams{State: &ready}); err != nil {
		t.Fatal(err)
	}
	for _, cb := range []*models.GithubCallback{merge, failed, verify, coordMerge, coordFailed} {
		h.expectState(t, cb, models.GithubCallbackStateActive)
	}
	// Keep the request pending during the legitimate merged wake. Idle WAITING
	// itself does not defer archives; the deferrer consults the raw chat tracker.
	h.tracker.Update("chat-build", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	exec := newRecordingExecutor()
	srv := &Server{sessions: h.sessions, repos: h.repos, agentChats: h.chats, chatStatus: h.tracker, displayTracker: h.display, logger: zerolog.Nop()}
	d := newArchiveDeferrer(h.chats, h.tracker, h.display, h.sessions, exec.archive, zerolog.Nop())
	d.interval = time.Hour
	srv.archiveDeferrerOnce.Do(func() { srv.archiveDeferrer = d })
	handles := make(chan (<-chan struct{}), 4)
	track := func(_ string, done <-chan struct{}) { handles <- done }
	dispatcher := session.NewDispatcher(h.sessions, h.repos, nil, zerolog.Nop())
	dispatcher.SetArchiver(session.SessionArchiverFunc(srv.RequestArchiveAutomatic), track)
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{SessionID: h.child.ID, Event: vcs.PRMerged{PRID: 7}}
	close(events)
	dispatcher.Run(ctx, events)
	select {
	case done := <-handles:
		<-done
	case <-time.After(3 * time.Second):
		t.Fatal("merge archive request missing")
	}
	if !d.has(h.child.ID) || exec.count() != 0 {
		t.Fatal("archive did not defer while chat working")
	}
	h.expectState(t, merge, models.GithubCallbackStateTriggered)
	h.expectState(t, failed, models.GithubCallbackStateCanceled)
	h.expectState(t, verify, models.GithubCallbackStateCanceled)
	h.expectState(t, coordMerge, models.GithubCallbackStateActive)
	h.expectState(t, coordFailed, models.GithubCallbackStateActive)
	// Reading the delivered row also synchronizes with the recording deliverer.
	var deliveredChat, deliveredPrompt string
	h.deliverUntil(t, merge, models.GithubCallbackStateDelivered, callback.DelivererFunc(func(_ context.Context, chat, message string) error {
		deliveredChat, deliveredPrompt = chat, message
		h.tracker.Update(chat, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		return nil
	}))
	if deliveredChat != "chat-build" || !strings.Contains(deliveredPrompt, merge.ID) || !strings.Contains(deliveredPrompt, "watch merged") {
		t.Fatalf("merge delivery = %s/%s", deliveredChat, deliveredPrompt)
	}
	h.tracker.Update("chat-build", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	if err := h.computer.Recompute(ctx, h.child.ID); err != nil {
		t.Fatal(err)
	}
	h.expectWaiting(t, "chat-build", false)
	h.expectWaiting(t, "chat-verify", false)
	d.sweep(ctx, track)
	exec.expectOne(t, h.child.ID)
	select {
	case done := <-handles:
		<-done
	case <-time.After(3 * time.Second):
		t.Fatal("archive worker missing")
	}
	if exec.count() != 1 || d.has(h.child.ID) {
		t.Fatal("archive did not complete exactly once")
	}
	stale := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerChecksFailed)
	if err := h.computer.Recompute(ctx, h.child.ID); err != nil {
		t.Fatal(err)
	}
	if h.tracker.Waiting("chat-build") == "" {
		t.Fatal("late watch did not cache waiting")
	}
	if _, err := h.watches.TriggerGroup(ctx, stale.ID, "checks_failed", time.Now()); err != nil {
		t.Fatal(err)
	}
	h.deliverUntil(t, stale, models.GithubCallbackStateCanceled, callback.DelivererFunc(func(context.Context, string, string) error { t.Error("late firing woke merged chat"); return nil }))
	h.expectWaiting(t, "chat-build", false)
	if h.tracker.Waiting("chat-build") != "" {
		t.Fatal("late drop left cached waiting reason")
	}
	if entry := h.tracker.Get("chat-build"); entry == nil || entry.Status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatal("late firing changed idle chat")
	}
}

type retirementMergedProvider struct{ vcs.Provider }

func (retirementMergedProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return &vcs.PRStatus{State: vcs.PRStateMerged}, nil
}
func TestCallbackRetirement_DisplayPollerManualMerge(t *testing.T) {
	h := newRetirementIntegration(t)
	ctx := context.Background()
	merged := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerMerged)
	failed := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerChecksFailed)
	verify := h.arm(t, "chat-verify", 7, models.GithubCallbackTriggerChecksPassedReady)
	coord := h.arm(t, "chat-coord", 7, models.GithubCallbackTriggerChecksFailed)
	poller := session.NewDisplayPoller(h.sessions, h.repos, retirementMergedProvider{}, h.display, time.Hour, zerolog.Nop())
	if err := poller.RefreshPR(ctx, h.repo.OriginURL, 7); err != nil {
		t.Fatal(err)
	}
	row, err := h.raw.Get(ctx, h.child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != machine.Merged {
		t.Fatalf("state=%s", row.State)
	}
	h.expectState(t, failed, models.GithubCallbackStateCanceled)
	h.expectState(t, verify, models.GithubCallbackStateCanceled)
	h.expectState(t, coord, models.GithubCallbackStateActive)
	h.expectState(t, merged, models.GithubCallbackStateTriggered)
	h.deliverUntil(t, merged, models.GithubCallbackStateDelivered, callback.DelivererFunc(func(context.Context, string, string) error { return nil }))
	h.expectWaiting(t, "chat-build", false)
	h.expectWaiting(t, "chat-verify", false)
	if h.tracker.Waiting("chat-verify") != "" {
		t.Fatal("poller left waiting reason cached")
	}
}
func TestCallbackRetirement_StopAndClose(t *testing.T) {
	for _, mode := range []string{"stop transition", "close RPC", "human PR close"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h := newRetirementIntegration(t)
			active := h.arm(t, "chat-build", 7, models.GithubCallbackTriggerChecksFailed)
			triggered := h.arm(t, "chat-verify", 8, models.GithubCallbackTriggerMerged)
			coord := h.arm(t, "chat-coord", 7, models.GithubCallbackTriggerChecksFailed)
			if _, err := h.watches.TriggerGroup(ctx, triggered.ID, "merged", time.Now()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "close RPC":
				srv := &Server{sessions: h.sessions, logger: zerolog.Nop()}
				if _, err := srv.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: h.child.ID})); err != nil {
					t.Fatal(err)
				}
			case "human PR close":
				dispatcher := session.NewDispatcher(h.sessions, h.repos, nil, zerolog.Nop())
				events := make(chan session.SessionEvent, 1)
				events <- session.SessionEvent{SessionID: h.child.ID, Event: vcs.PRClosed{PRID: 7}}
				close(events)
				dispatcher.Run(ctx, events)
			default:
				closed := int(machine.Closed)
				if _, err := h.sessions.Update(ctx, h.child.ID, db.UpdateSessionParams{State: &closed}); err != nil {
					t.Fatal(err)
				}
			}
			h.expectState(t, active, models.GithubCallbackStateCanceled)
			h.expectState(t, triggered, models.GithubCallbackStateCanceled)
			h.expectState(t, coord, models.GithubCallbackStateActive)
			stale := h.arm(t, "chat-build", 9, models.GithubCallbackTriggerMerged)
			if _, err := h.watches.TriggerGroup(ctx, stale.ID, "merged", time.Now()); err != nil {
				t.Fatal(err)
			}
			h.deliverUntil(t, stale, models.GithubCallbackStateCanceled, callback.DelivererFunc(func(context.Context, string, string) error { t.Error("late firing woke closed chat"); return nil }))
		})
	}
}
