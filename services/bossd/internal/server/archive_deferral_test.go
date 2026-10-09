package server

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// --- fakes ---

// deferralChatStore is a minimal db.AgentChatStore: the chats of each session,
// plus Create/GetByAgentSessionID for the RecordChat path. Any other method
// nil-panics loudly through the embedded interface.
type deferralChatStore struct {
	db.AgentChatStore
	mu    sync.Mutex
	chats map[string]*models.AgentChat // agent session id -> chat
}

func newDeferralChatStore() *deferralChatStore {
	return &deferralChatStore{chats: map[string]*models.AgentChat{}}
}

func (s *deferralChatStore) add(sessionID, agentSessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chats[agentSessionID] = &models.AgentChat{SessionID: sessionID, AgentSessionID: agentSessionID}
}

func (s *deferralChatStore) ListBySession(_ context.Context, sessionID string) ([]*models.AgentChat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*models.AgentChat
	for _, c := range s.chats {
		if c.SessionID == sessionID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *deferralChatStore) GetByAgentSessionID(_ context.Context, agentSessionID string) (*models.AgentChat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.chats[agentSessionID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return c, nil
}

func (s *deferralChatStore) Create(_ context.Context, p db.CreateAgentChatParams) (*models.AgentChat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &models.AgentChat{SessionID: p.SessionID, AgentSessionID: p.AgentSessionID, Title: p.Title}
	s.chats[p.AgentSessionID] = c
	return c, nil
}

// deferralSessionStore is a minimal db.SessionStore holding unarchived rows.
type deferralSessionStore struct {
	db.SessionStore
	mu   sync.Mutex
	rows map[string]*models.Session
}

func (s *deferralSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *r
	return &cp, nil
}

// recordingExecutor stands in for ArchiveSessionAndNotify.
type recordingExecutor struct {
	mu    sync.Mutex
	n     int
	calls chan string
}

func newRecordingExecutor() *recordingExecutor {
	return &recordingExecutor{calls: make(chan string, 16)}
}

func (e *recordingExecutor) archive(_ context.Context, id string) error {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	e.calls <- id
	return nil
}

func (e *recordingExecutor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

func (e *recordingExecutor) expectOne(t *testing.T, want string) {
	t.Helper()
	select {
	case id := <-e.calls:
		if id != want {
			t.Fatalf("archived %q, want %q", id, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("archive of %q never ran", want)
	}
}

func (e *recordingExecutor) expectNone(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case id := <-e.calls:
		t.Fatalf("unexpected archive of %q", id)
	case <-time.After(within):
	}
}

// fakeClock is the deferrer's injected clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// --- harness ---

const deferralSessionID = "sess-1"

type deferralHarness struct {
	srv      *Server
	d        *archiveDeferrer
	tracker  *status.Tracker
	display  *status.DisplayTracker
	chats    *deferralChatStore
	sessions *deferralSessionStore
	exec     *recordingExecutor
	clock    *fakeClock
}

// newDeferralHarness builds a Server whose deferrer reads a real status.Tracker
// and DisplayTracker, a fake chat store holding chatIDs for deferralSessionID,
// and archives through a recording executor. The fallback tick is an hour, so
// only an explicit nudge re-evaluates.
func newDeferralHarness(t *testing.T, chatIDs ...string) *deferralHarness {
	t.Helper()
	h := &deferralHarness{
		tracker:  status.NewTracker(),
		display:  status.NewDisplayTracker(),
		chats:    newDeferralChatStore(),
		sessions: &deferralSessionStore{rows: map[string]*models.Session{deferralSessionID: {ID: deferralSessionID, Title: "t"}}},
		exec:     newRecordingExecutor(),
		clock:    &fakeClock{t: time.Now()},
	}
	for _, id := range chatIDs {
		h.chats.add(deferralSessionID, id)
	}
	h.srv = &Server{
		sessions:       h.sessions,
		agentChats:     h.chats,
		chatStatus:     h.tracker,
		displayTracker: h.display,
		logger:         zerolog.Nop(),
	}
	h.d = newArchiveDeferrer(h.chats, h.tracker, h.display, h.sessions, h.exec.archive, zerolog.Nop())
	h.d.now = h.clock.Now
	h.d.interval = time.Hour
	h.srv.archiveDeferrerOnce.Do(func() { h.srv.archiveDeferrer = h.d })
	return h
}

// run starts the deferrer loop, stopped and joined at cleanup.
func (h *deferralHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := h.srv.RunArchiveDeferrer(ctx, nil)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("archive deferrer loop did not stop")
		}
	})
}

func (h *deferralHarness) archiving() bool {
	e := h.display.Get(deferralSessionID)
	return e != nil && e.Archiving
}

func (h *deferralHarness) rpc(t *testing.T, req *pb.ArchiveSessionRequest) *pb.ArchiveSessionResponse {
	t.Helper()
	req.Id = deferralSessionID
	resp, err := h.srv.ArchiveSession(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("ArchiveSession: %v", err)
	}
	return resp.Msg
}

// --- tests ---

func TestRequestArchive_AllChatsIdle_ArchivesImmediately(t *testing.T) {
	h := newDeferralHarness(t, "chat-a", "chat-b")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.tracker.Update("chat-b", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())

	resp := h.rpc(t, &pb.ArchiveSessionRequest{})

	// Synchronous: the executor already ran by the time the RPC returned.
	if got := h.exec.count(); got != 1 {
		t.Fatalf("executor calls = %d, want 1 (synchronous)", got)
	}
	if resp.GetIsDeferred() {
		t.Error("is_deferred = true, want false")
	}
	if resp.GetBlockingAgentSessionId() != "" {
		t.Errorf("blocking_agent_session_id = %q, want empty", resp.GetBlockingAgentSessionId())
	}
	if resp.GetSession().GetId() != deferralSessionID {
		t.Errorf("session id = %q, want %q", resp.GetSession().GetId(), deferralSessionID)
	}
	if h.d.has(deferralSessionID) {
		t.Error("an immediate archive must leave no pending entry")
	}
}

func TestRequestArchive_WorkingChat_DefersThenArchivesOnIdle(t *testing.T) {
	h := newDeferralHarness(t, "chat-a", "chat-b")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.tracker.Update("chat-b", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	h.run(t)

	resp := h.rpc(t, &pb.ArchiveSessionRequest{})
	if !resp.GetIsDeferred() {
		t.Fatal("is_deferred = false, want true while chat-b is working")
	}
	if got := resp.GetBlockingAgentSessionId(); got != "chat-b" {
		t.Errorf("blocking_agent_session_id = %q, want chat-b", got)
	}
	if !h.archiving() {
		t.Error("archive_pending (DisplayTracker archiving) = false, want true while deferred")
	}
	hydrated := &pb.Session{Id: deferralSessionID}
	HydrateDisplayEntry(hydrated, h.display.Get(deferralSessionID))
	if !hydrated.GetArchivePending() {
		t.Error("hydrated session archive_pending = false, want true while deferred")
	}
	h.exec.expectNone(t, 50*time.Millisecond)

	// A nudge while still working changes nothing.
	h.srv.NudgePendingArchive(deferralSessionID)
	h.exec.expectNone(t, 50*time.Millisecond)

	h.tracker.Update("chat-b", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.srv.NudgePendingArchive(deferralSessionID)
	h.exec.expectOne(t, deferralSessionID)

	// Exactly once: the entry was removed before firing.
	h.srv.NudgePendingArchive("")
	h.exec.expectNone(t, 100*time.Millisecond)
	if h.d.has(deferralSessionID) {
		t.Error("pending entry survived the fired archive")
	}
}

func TestRequestArchive_LimitedBlocks_StalledDoesNot(t *testing.T) {
	t.Run("limited blocks", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a")
		h.tracker.UpdateLimited("chat-a", time.Time{}, time.Now())
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Pending || out.BlockingAgentSessionID != "chat-a" {
			t.Fatalf("outcome = %+v, want pending on chat-a", out)
		}
		if h.exec.count() != 0 {
			t.Fatal("a LIMITED chat must defer the archive")
		}
	})
	t.Run("stalled working chat does not block", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a")
		h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		h.tracker.SetStalled("chat-a", true)
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Archived {
			t.Fatalf("outcome = %+v, want archived (stalled chat is exempt)", out)
		}
	})
	t.Run("stopped and untracked chats do not block", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a", "chat-untracked")
		h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_STOPPED, time.Now())
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Archived {
			t.Fatalf("outcome = %+v, want archived", out)
		}
	})
}

func TestRequestArchive_IdlePromotedWaitingDoesNotBlock(t *testing.T) {
	t.Run("waiting promoted from idle does not block", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a")
		h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
		h.tracker.SetWaiting("chat-a", "awaiting checks_passed_ready on acme/widget#1")
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Archived {
			t.Fatalf("outcome = %+v, want archived", out)
		}
	})
	t.Run("waiting promoted from working blocks", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a")
		h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		h.tracker.SetWaiting("chat-a", "awaiting checks_passed_ready on acme/widget#1")
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Pending {
			t.Fatalf("outcome = %+v, want pending (raw status is WORKING)", out)
		}
	})
}

// TestRequestArchive_RequesterGuard pins the self-archive guard. The tracker
// stamps ReceivedAt with the real clock while the deferrer's settle window
// runs on the injected one, so each case places the request's acceptance time
// relative to the real moment of the tracker observation.
func TestRequestArchive_RequesterGuard(t *testing.T) {
	t.Run("idle reading from before the request keeps the requester busy", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-self")
		// The poller last saw the requester IDLE, before its turn started.
		h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
		h.clock.set(time.Now())
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Pending || out.BlockingAgentSessionID != "chat-self" {
			t.Fatalf("outcome = %+v, want pending on the requester", out)
		}
	})

	t.Run("non-working observation before settle does not release", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-self")
		accepted := time.Now().Add(-5 * time.Second)
		h.clock.set(accepted)
		if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"}); !out.Pending {
			t.Fatalf("outcome = %+v, want pending", out)
		}
		// Observed IDLE ~5s after acceptance: inside the settle window.
		h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
		h.clock.set(accepted.Add(requesterSettle + time.Second))
		h.d.sweep(context.Background(), nil)
		h.exec.expectNone(t, 50*time.Millisecond)
		if !h.d.has(deferralSessionID) {
			t.Fatal("a pre-settle observation released the requester")
		}
	})

	t.Run("non-working observation after settle releases", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-self")
		accepted := time.Now().Add(-requesterSettle - time.Second)
		h.clock.set(accepted)
		if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"}); !out.Pending {
			t.Fatalf("outcome = %+v, want pending", out)
		}
		// A WORKING reading after settle still holds it.
		h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		h.clock.set(time.Now())
		h.d.sweep(context.Background(), nil)
		h.exec.expectNone(t, 50*time.Millisecond)

		h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
		h.d.sweep(context.Background(), nil)
		h.exec.expectOne(t, deferralSessionID)
	})

	t.Run("requester outside the session gets no guard", func(t *testing.T) {
		h := newDeferralHarness(t, "chat-a")
		h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
		out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-elsewhere"})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Archived {
			t.Fatalf("outcome = %+v, want archived", out)
		}
	})
}

func TestRequestArchive_ForceBypassesDeferral(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	// A pending entry from an earlier request is superseded by the force.
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	resp := h.rpc(t, &pb.ArchiveSessionRequest{ShouldForce: true})
	if resp.GetIsDeferred() {
		t.Fatal("is_deferred = true with should_force")
	}
	if got := h.exec.count(); got != 1 {
		t.Fatalf("executor calls = %d, want 1", got)
	}
	if h.d.has(deferralSessionID) {
		t.Error("force left the earlier pending entry behind")
	}
}

func TestRequestArchive_DuplicateRequestsSingleArchive(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	h.run(t)

	for i := 0; i < 3; i++ {
		if out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); err != nil || !out.Pending {
			t.Fatalf("request %d: outcome = %+v err = %v, want pending", i, out, err)
		}
	}
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.srv.NudgePendingArchive(deferralSessionID)
	h.exec.expectOne(t, deferralSessionID)
	h.srv.NudgePendingArchive("")
	h.exec.expectNone(t, 100*time.Millisecond)
}

func TestCancelPendingArchive_ViaResurrect(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}

	var frames []*pb.ResurrectSessionResponse
	err := h.srv.StreamResurrectSession(context.Background(), &pb.ResurrectSessionRequest{Id: deferralSessionID}, func(r *pb.ResurrectSessionResponse) error {
		frames = append(frames, r)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamResurrectSession: %v", err)
	}
	if len(frames) != 1 || frames[0].GetSessionResurrected() == nil {
		t.Fatalf("frames = %v, want one SessionResurrected", frames)
	}
	got := frames[0].GetSessionResurrected().GetSession()
	if got.GetId() != deferralSessionID || got.GetArchivedAt() != nil {
		t.Errorf("resurrected session = %v, want the unarchived %s", got, deferralSessionID)
	}
	if h.d.has(deferralSessionID) {
		t.Error("pending entry survived the resurrect")
	}
	if h.archiving() {
		t.Error("archiving flag survived the resurrect")
	}

	// The cancelled archive never fires.
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)
}

func TestCancelPendingArchive_ViaNewChat(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}

	// Re-recording an EXISTING chat (a resume) does not cancel.
	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "chat-a"})); err != nil {
		t.Fatalf("RecordChat(existing): %v", err)
	}
	if !h.d.has(deferralSessionID) {
		t.Fatal("resuming an existing chat cancelled the pending archive")
	}

	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "chat-new"})); err != nil {
		t.Fatalf("RecordChat(new): %v", err)
	}
	if h.d.has(deferralSessionID) {
		t.Error("a new chat did not cancel the pending archive")
	}
	if h.archiving() {
		t.Error("archiving flag survived the cancel")
	}
}

// TestArchiveDeferrer_RestartDropsPending pins the in-memory design: a fresh
// deferrer (a restarted daemon) holds nothing, and the merged-but-unarchived
// sweep re-derives the archive through the deferring archiver — deferring it
// again while the chat works, and archiving once it is idle.
func TestArchiveDeferrer_RestartDropsPending(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewAgentChatStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         "/tmp/r",
		OriginURL:         "https://github.com/acme/r",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if !repo.ShouldArchiveSessionsAfterMerge {
		t.Fatal("precondition: archive-after-merge defaults on")
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feature", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	merged := int(machine.Merged)
	pr := 7
	prPtr := &pr
	if _, err := sessions.Update(ctx, sess.ID, db.UpdateSessionParams{State: &merged, PRNumber: &prPtr}); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	if _, err := chats.Create(ctx, db.CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: "chat-a"}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	build := func(tracker *status.Tracker, exec *recordingExecutor) (*Server, *archiveDeferrer) {
		srv := &Server{sessions: sessions, repos: repos, agentChats: chats, chatStatus: tracker, displayTracker: status.NewDisplayTracker(), logger: zerolog.Nop()}
		d := newArchiveDeferrer(chats, tracker, srv.displayTracker, sessions, exec.archive, zerolog.Nop())
		d.interval = time.Hour
		srv.archiveDeferrerOnce.Do(func() { srv.archiveDeferrer = d })
		return srv, d
	}

	// Before the restart: the archive is pending on the working chat.
	trackerA := status.NewTracker()
	trackerA.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	srvA, dA := build(trackerA, newRecordingExecutor())
	if out, _ := srvA.RequestArchive(ctx, sess.ID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	if !dA.has(sess.ID) {
		t.Fatal("precondition: pending before restart")
	}

	// The restart: a fresh daemon has a fresh deferrer, and the poller sees
	// the chat still working.
	trackerB := status.NewTracker()
	trackerB.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	execB := newRecordingExecutor()
	srvB, dB := build(trackerB, execB)
	if dB.has(sess.ID) {
		t.Fatal("a fresh deferrer must hold no pending archives")
	}

	handles := make(chan (<-chan struct{}), 4)
	resolver := session.NewPRAssociationResolver(sessions, repos, nil, zerolog.Nop()).
		WithArchiver(session.SessionArchiverFunc(srvB.RequestArchiveAutomatic), func(_ string, d <-chan struct{}) { handles <- d })
	if _, err := resolver.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	select {
	case h := <-handles:
		<-h
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep did not request the archive")
	}
	if !dB.has(sess.ID) {
		t.Fatal("the sweep did not re-derive the pending archive")
	}
	execB.expectNone(t, 50*time.Millisecond)

	trackerB.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	dB.sweep(ctx, nil)
	execB.expectOne(t, sess.ID)
}

func TestArchiveDeferrer_RunJoinsOnCancel(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	handles := make(chan (<-chan struct{}), 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := h.srv.RunArchiveDeferrer(ctx, func(id string, d <-chan struct{}) {
		if id != deferralSessionID {
			t.Errorf("tracked %q, want %q", id, deferralSessionID)
		}
		handles <- d
	})

	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.srv.NudgePendingArchive(deferralSessionID)

	// The fired archive's handle reaches the tracker and closes when it ends.
	select {
	case fired := <-handles:
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatal("fired archive handle never closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fired archive was not handed to the tracker")
	}
	h.exec.expectOne(t, deferralSessionID)

	// Leave one pending at shutdown: cancelling drops it and joins the loop.
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunArchiveDeferrer done did not close after ctx cancel")
	}
	if h.d.has(deferralSessionID) {
		t.Error("pending entries must be dropped at shutdown")
	}
}

// A sweep drops entries whose session vanished or was archived by another path.
func TestArchiveDeferrer_SweepDropsArchivedOrMissing(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	now := time.Now()
	h.sessions.mu.Lock()
	h.sessions.rows[deferralSessionID].ArchivedAt = &now
	h.sessions.mu.Unlock()
	h.d.sweep(context.Background(), nil)
	if h.d.has(deferralSessionID) || h.archiving() {
		t.Error("an archived session's pending entry was not dropped")
	}
	h.exec.expectNone(t, 50*time.Millisecond)
}

// An executor failure on the immediate path surfaces as the RPC's error.
func TestRequestArchive_ImmediateExecutorErrorPropagates(t *testing.T) {
	h := newDeferralHarness(t)
	h.d.execute = func(context.Context, string) error { return errors.New("boom") }
	_, err := h.srv.ArchiveSession(context.Background(), connect.NewRequest(&pb.ArchiveSessionRequest{Id: deferralSessionID}))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("code = %v, want Internal (err=%v)", connect.CodeOf(err), err)
	}
}

// TestDispatcherPRMerged_RealDeferrer_DefersWhileChatWorking drives the real
// session.Dispatcher through the real deferral seam, wired exactly as main.go
// wires it (SessionArchiverFunc(RequestArchiveAutomatic)): a pr_merged on an
// archive-after-merge repo with a working chat leaves the archive pending, and
// the deferrer fires it once the chat is idle.
func TestDispatcherPRMerged_RealDeferrer_DefersWhileChatWorking(t *testing.T) {
	ctx := context.Background()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewAgentChatStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName: "r", LocalPath: "/tmp/r", OriginURL: "https://github.com/acme/r",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feature", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	awaiting := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, sess.ID, db.UpdateSessionParams{State: &awaiting}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if _, err := chats.Create(ctx, db.CreateAgentChatParams{SessionID: sess.ID, AgentSessionID: "chat-build"}); err != nil {
		t.Fatalf("create chat: %v", err)
	}

	tracker := status.NewTracker()
	tracker.Update("chat-build", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	display := status.NewDisplayTracker()
	exec := newRecordingExecutor()
	srv := &Server{sessions: sessions, repos: repos, agentChats: chats, chatStatus: tracker, displayTracker: display, logger: zerolog.Nop()}
	d := newArchiveDeferrer(chats, tracker, display, sessions, exec.archive, zerolog.Nop())
	d.interval = time.Hour
	srv.archiveDeferrerOnce.Do(func() { srv.archiveDeferrer = d })

	tracked := make(chan (<-chan struct{}), 4)
	track := func(_ string, done <-chan struct{}) { tracked <- done }
	loopCtx, cancel := context.WithCancel(ctx)
	loopDone := srv.RunArchiveDeferrer(loopCtx, track)
	t.Cleanup(func() { cancel(); <-loopDone })

	dispatcher := session.NewDispatcher(sessions, repos, nil, zerolog.Nop())
	dispatcher.SetArchiver(session.SessionArchiverFunc(srv.RequestArchiveAutomatic), track)
	events := make(chan session.SessionEvent, 1)
	events <- session.SessionEvent{SessionID: sess.ID, Event: vcs.PRMerged{PRID: 1}}
	close(events)
	dispatcher.Run(ctx, events)

	// The dispatcher's archive worker (the request) completes...
	select {
	case done := <-tracked:
		<-done
	case <-time.After(2 * time.Second):
		t.Fatal("archive-after-merge was not requested")
	}
	// ...leaving the archive pending, not executed.
	if !d.has(sess.ID) {
		t.Fatal("archive was not deferred while the chat works")
	}
	exec.expectNone(t, 50*time.Millisecond)
	if e := display.Get(sess.ID); e == nil || !e.Archiving {
		t.Error("archive_pending not raised for the deferred archive")
	}

	tracker.Update("chat-build", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	srv.NudgePendingArchive(sess.ID)
	exec.expectOne(t, sess.ID)
	select {
	case done := <-tracked:
		<-done
	case <-time.After(2 * time.Second):
		t.Fatal("the fired archive was not handed to the archive tracker")
	}
}

// hookedChatLister runs hook once, on the first ListBySession, before
// delegating — a seam for interleaving a request inside a sweep's evaluation.
type hookedChatLister struct {
	inner archiveChatLister
	fired atomic.Bool
	hook  func()
}

func (l *hookedChatLister) ListBySession(ctx context.Context, sessionID string) ([]*models.AgentChat, error) {
	if l.fired.CompareAndSwap(false, true) {
		l.hook()
	}
	return l.inner.ListBySession(ctx, sessionID)
}

func TestArchiveDeferrer_SweepHonoursRequesterAddedMidEvaluation(t *testing.T) {
	h := newDeferralHarness(t, "chat-a", "chat-self")
	base := time.Now()
	h.clock.set(base)
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, base)
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); !out.Pending {
		t.Fatalf("outcome = %+v, want pending", out)
	}
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, base)

	// While the sweep evaluates its snapshot, chat-self asks to archive its
	// own session and is told the archive is pending on it.
	var selfOut ArchiveOutcome
	h.d.chats = &hookedChatLister{inner: h.chats, hook: func() {
		selfOut, _ = h.d.request(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"})
	}}
	h.d.sweep(context.Background(), nil)

	if !selfOut.Pending || selfOut.BlockingAgentSessionID != "chat-self" {
		t.Fatalf("self request outcome = %+v, want pending on chat-self", selfOut)
	}
	h.exec.expectNone(t, 50*time.Millisecond)
	if !h.d.has(deferralSessionID) {
		t.Fatal("sweep claimed an entry whose requester guard it never evaluated")
	}
}

func TestRequestArchive_RepeatRequestRefreshesRequesterGuard(t *testing.T) {
	h := newDeferralHarness(t, "chat-a", "chat-self")
	base := time.Now().Add(-time.Hour)
	h.clock.set(base)
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, base)
	if out, _ := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"}); !out.Pending {
		t.Fatalf("first outcome = %+v, want pending", out)
	}

	// chat-self settled long after its first request, then started a new turn
	// the poller has not seen yet, and asks again. chat-a is now idle.
	h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, base.Add(time.Minute))
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, base.Add(time.Minute))
	h.clock.set(base.Add(2 * time.Minute))
	out, err := h.srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Pending || out.BlockingAgentSessionID != "chat-self" {
		t.Fatalf("repeat outcome = %+v, want pending on chat-self (fresh settle window)", out)
	}
	h.exec.expectNone(t, 50*time.Millisecond)
}

func TestRequestArchive_ConcurrentRequestJoinsInflightArchive(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	h.d.execute = func(context.Context, string) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(started)
			<-release
		}
		return nil
	}

	firstDone := make(chan ArchiveOutcome, 1)
	go func() {
		out, _ := h.d.request(context.Background(), deferralSessionID, ArchiveRequest{})
		firstDone <- out
	}()
	<-started
	secondDone := make(chan ArchiveOutcome, 1)
	go func() {
		out, _ := h.d.request(context.Background(), deferralSessionID, ArchiveRequest{})
		secondDone <- out
	}()
	select {
	case out := <-secondDone:
		t.Fatalf("second request returned %+v before the in-flight archive finished", out)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for _, ch := range []chan ArchiveOutcome{firstDone, secondDone} {
		select {
		case out := <-ch:
			if !out.Archived {
				t.Fatalf("outcome = %+v, want archived", out)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request never returned")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("executor calls = %d, want 1", calls)
	}
}
