package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"

	"github.com/recurser/bossalib/clocktest"
)

// fakeSettled is the finish deferrer's injected settled predicate: it reports
// the chat with recent agent-log output, standing in for UnattendedRunQuiet.
type fakeSettled struct {
	mu       sync.Mutex
	blocking string
	calls    int
}

func (f *fakeSettled) set(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocking = id
}

func (f *fakeSettled) settled(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.blocking, nil
}

type finishHarness struct {
	srv      *Server
	d        *archiveDeferrer
	tracker  *status.Tracker
	chats    *deferralChatStore
	sessions *deferralSessionStore
	exec     *recordingExecutor
	clock    *clocktest.Clock
	quiet    *fakeSettled
}

// newFinishHarness builds a Server whose finish deferrer reads a real
// status.Tracker, holds deferralSessionID as an unattended cron session with
// chatIDs, finalizes through a recording executor, and consults a fake settled
// predicate. The fallback tick is an hour, so only an explicit sweep or nudge
// re-evaluates.
func newFinishHarness(t *testing.T, chatIDs ...string) *finishHarness {
	t.Helper()
	cronJobID := "cron-1"
	h := &finishHarness{
		tracker: status.NewTracker(),
		chats:   newDeferralChatStore(),
		sessions: &deferralSessionStore{rows: map[string]*models.Session{
			deferralSessionID: {ID: deferralSessionID, Title: "t", CronJobID: &cronJobID},
		}},
		exec:  newRecordingExecutor(),
		clock: clocktest.New(time.Now()),
		quiet: &fakeSettled{},
	}
	for _, id := range chatIDs {
		h.chats.add(deferralSessionID, id)
	}
	h.srv = &Server{
		sessions:       h.sessions,
		agentChats:     h.chats,
		chatStatus:     h.tracker,
		displayTracker: status.NewDisplayTracker(),
		logger:         zerolog.Nop(),
	}
	h.srv.initDeferrers()
	h.d = newArchiveDeferrer(h.chats, h.tracker, nil, h.sessions, h.exec.archive, zerolog.Nop())
	h.d.settled = h.quiet.settled
	h.d.fireBudget = finishRunBudget
	h.d.now = h.clock.Now
	h.d.interval = time.Hour
	h.srv.finishDeferrer = h.d
	return h
}

func (h *finishHarness) rpc(req *pb.FinishUnattendedRunRequest) (*pb.FinishUnattendedRunResponse, error) {
	resp, err := h.srv.FinishUnattendedRun(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestFinishUnattendedRun_RejectsBadRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(h *finishHarness)
		req    *pb.FinishUnattendedRunRequest
		want   connect.Code
	}{
		{
			name: "empty session id",
			req:  &pb.FinishUnattendedRunRequest{RequesterAgentSessionId: "chat-self"},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "empty requester",
			req:  &pb.FinishUnattendedRunRequest{SessionId: deferralSessionID},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "missing session",
			req:  &pb.FinishUnattendedRunRequest{SessionId: "sess-gone", RequesterAgentSessionId: "chat-self"},
			want: connect.CodeNotFound,
		},
		{
			name: "attended session",
			mutate: func(h *finishHarness) {
				h.sessions.rows[deferralSessionID].CronJobID = nil
			},
			req:  &pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-self"},
			want: connect.CodeFailedPrecondition,
		},
		{
			name: "archived session",
			mutate: func(h *finishHarness) {
				now := time.Now()
				h.sessions.rows[deferralSessionID].ArchivedAt = &now
			},
			req:  &pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-self"},
			want: connect.CodeFailedPrecondition,
		},
		{
			name: "requester is not a chat of the session",
			mutate: func(h *finishHarness) {
				h.chats.add("sess-other", "chat-elsewhere")
			},
			req:  &pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-elsewhere"},
			want: connect.CodePermissionDenied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFinishHarness(t, "chat-self")
			if tc.mutate != nil {
				tc.mutate(h)
			}
			_, err := h.rpc(tc.req)
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.want, err)
			}
			if h.d.has(deferralSessionID) {
				t.Error("a refused request left a pending finish")
			}
			h.exec.expectNone(t, 20*time.Millisecond)
		})
	}
}

// TestFinishUnattendedRun_RequesterDeferredUntilSettled: the caller's own
// request always comes back deferred, and the finalize fires only after
// requesterSettle once the tracker has seen the requester settle.
func TestFinishUnattendedRun_RequesterDeferredUntilSettled(t *testing.T) {
	h := newFinishHarness(t, "chat-self")
	// The poller last saw the requester IDLE, before its turn ended.
	h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	accepted := time.Now().Add(-requesterSettle - time.Second)
	h.clock.Set(accepted)

	resp, err := h.rpc(&pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-self"})
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if !resp.GetIsDeferred() || resp.GetBlockingAgentSessionId() != "chat-self" {
		t.Fatalf("response = %+v, want deferred on the requester", resp)
	}
	if got := h.exec.count(); got != 0 {
		t.Fatalf("executor calls = %d, the RPC finalized its own caller synchronously", got)
	}

	// Still inside the settle window: nothing fires.
	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)

	// Past the window but the tracker reads the requester WORKING: still held.
	h.clock.Set(time.Now())
	h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)

	// Settled after the window: the finish fires, exactly once.
	h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.d.sweep(context.Background(), nil)
	h.exec.expectOne(t, deferralSessionID)
	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)
	if h.d.has(deferralSessionID) {
		t.Error("pending finish survived the fired finalize")
	}
}

// TestFinishUnattendedRun_RecentLogBlocksIdleChat is the background-subagent
// case: every chat reads idle in the tracker, but a chat's log is still being
// written, so the finish waits until the settled predicate reports quiet.
func TestFinishUnattendedRun_RecentLogBlocksIdleChat(t *testing.T) {
	h := newFinishHarness(t, "chat-self", "chat-bg")
	h.clock.Set(time.Now().Add(-requesterSettle - time.Second))
	if out, err := h.srv.finishDeferral().request(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"}); err != nil || !out.Pending {
		t.Fatalf("outcome = %+v err = %v, want pending", out, err)
	}
	h.clock.Set(time.Now())
	h.tracker.Update("chat-self", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.tracker.Update("chat-bg", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())
	h.quiet.set("chat-bg")

	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)
	if !h.d.has(deferralSessionID) {
		t.Fatal("a chat with recent log output did not hold the finish")
	}

	h.quiet.set("")
	h.d.sweep(context.Background(), nil)
	h.exec.expectOne(t, deferralSessionID)
}

// TestFinishUnattendedRun_SettledConsultedOnlyWhenNoChatBusy: the predicate is
// an extra gate after the chat-busy check, not a replacement for it.
func TestFinishUnattendedRun_SettledConsultedOnlyWhenNoChatBusy(t *testing.T) {
	h := newFinishHarness(t, "chat-a")
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	out, err := h.srv.finishDeferral().request(context.Background(), deferralSessionID, ArchiveRequest{})
	if err != nil || !out.Pending || out.BlockingAgentSessionID != "chat-a" {
		t.Fatalf("outcome = %+v err = %v, want pending on chat-a", out, err)
	}
	h.quiet.mu.Lock()
	calls := h.quiet.calls
	h.quiet.mu.Unlock()
	if calls != 0 {
		t.Fatalf("settled predicate called %d times while a chat was busy", calls)
	}
}

// TestCancelPendingFinish_ViaNewChat: a new chat row means the run is not
// done, so it cancels a pending finish; re-recording an existing chat does not.
func TestCancelPendingFinish_ViaNewChat(t *testing.T) {
	h := newFinishHarness(t, "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e60")
	if resp, err := h.rpc(&pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e60"}); err != nil || !resp.GetIsDeferred() {
		t.Fatalf("response = %+v err = %v, want deferred", resp, err)
	}

	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e60"})); err != nil {
		t.Fatalf("RecordChat(existing): %v", err)
	}
	if !h.d.has(deferralSessionID) {
		t.Fatal("resuming an existing chat cancelled the pending finish")
	}

	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e61"})); err != nil {
		t.Fatalf("RecordChat(new): %v", err)
	}
	if h.d.has(deferralSessionID) {
		t.Error("a new chat did not cancel the pending finish")
	}

	h.clock.Set(time.Now().Add(time.Hour))
	h.d.sweep(context.Background(), nil)
	h.exec.expectNone(t, 50*time.Millisecond)
}

// TestFinishUnattendedRun_RowDeletedWhilePendingIsDropped: the sweep or Stop
// gate hard-deleted the run while its finish was pending. The pending entry is
// dropped and nothing fires.
func TestFinishUnattendedRun_RowDeletedWhilePendingIsDropped(t *testing.T) {
	h := newFinishHarness(t, "chat-self")
	if resp, err := h.rpc(&pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-self"}); err != nil || !resp.GetIsDeferred() {
		t.Fatalf("response = %+v err = %v, want deferred", resp, err)
	}
	h.sessions.mu.Lock()
	delete(h.sessions.rows, deferralSessionID)
	h.sessions.mu.Unlock()
	h.d.sweep(context.Background(), nil)
	if h.d.has(deferralSessionID) {
		t.Error("a deleted session's pending finish was not dropped")
	}
	h.exec.expectNone(t, 50*time.Millisecond)
}

// TestFinishRunNow_DeletedRowIsDone: a finish that fires after another path
// already hard-deleted the row is a clean no-op through the real lifecycle.
func TestFinishRunNow_DeletedRowIsDone(t *testing.T) {
	sessions := &deferralSessionStore{rows: map[string]*models.Session{}}
	srv := &Server{
		sessions:  sessions,
		lifecycle: session.NewLifecycle(sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop()),
		logger:    zerolog.Nop(),
	}
	if err := srv.finishRunNow(context.Background(), "sess-gone"); err != nil {
		t.Fatalf("finishRunNow on a deleted row = %v, want nil", err)
	}

	// A row that is no unattended run is a real error, not a silent success.
	sessions.rows["sess-attended"] = &models.Session{ID: "sess-attended"}
	if err := srv.finishRunNow(context.Background(), "sess-attended"); err == nil {
		t.Fatal("finishRunNow on an attended session returned nil")
	}
}

// TestFinishDeferral_Wiring pins the two deferrer instances apart: the archive
// deferrer keeps no settled predicate and the archive budget; the finish
// deferrer gets the lifecycle's quiet-log predicate and the finalize budget.
func TestFinishDeferral_Wiring(t *testing.T) {
	sessions := &deferralSessionStore{rows: map[string]*models.Session{}}
	srv := New(Config{
		Sessions:   sessions,
		AgentChats: newDeferralChatStore(),
		Lifecycle:  session.NewLifecycle(sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop()),
		Logger:     zerolog.Nop(),
	})
	a, f := srv.archiveDeferral(), srv.finishDeferral()
	if a == f {
		t.Fatal("finish and archive share one deferrer")
	}
	if a.settled != nil || a.fireBudget != archiveRPCBudget {
		t.Errorf("archive deferrer settled set = %v, budget = %v; want nil and %v", a.settled != nil, a.fireBudget, archiveRPCBudget)
	}
	if f.settled == nil || f.fireBudget != finishRunBudget {
		t.Errorf("finish deferrer settled set = %v, budget = %v; want set and %v", f.settled != nil, f.fireBudget, finishRunBudget)
	}
	if f.display != nil {
		t.Error("finish deferrer raises the archiving flag")
	}
}

// Update applies the run_blocked_reason write ReportRunBlocked makes.
func (s *deferralSessionStore) Update(_ context.Context, id string, params db.UpdateSessionParams) (*models.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if params.RunBlockedReason != nil {
		r.RunBlockedReason = *params.RunBlockedReason
	}
	cp := *r
	return &cp, nil
}

// TestFinishUnattendedRun_BlockedReportPersistsBeforeDeferring: a blocked
// finish writes the run's reason onto the session row at request time, so a
// finalize from any path sees it, and the finish itself is still deferred.
func TestFinishUnattendedRun_BlockedReportPersistsBeforeDeferring(t *testing.T) {
	h := newFinishHarness(t, "chat-self")
	h.sessions.rows[deferralSessionID].State = machine.ImplementingPlan
	h.srv.lifecycle = session.NewLifecycle(h.sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

	resp, err := h.rpc(&pb.FinishUnattendedRunRequest{
		SessionId:               deferralSessionID,
		RequesterAgentSessionId: "chat-self",
		IsBlocked:               true,
		BlockedReason:           "unresolved P1 in review",
	})
	if err != nil {
		t.Fatalf("FinishUnattendedRun: %v", err)
	}
	if !resp.GetIsDeferred() {
		t.Fatalf("response = %+v, want deferred", resp)
	}
	got, _ := h.sessions.Get(context.Background(), deferralSessionID)
	if got.RunBlockedReason == nil || *got.RunBlockedReason != "unresolved P1 in review" {
		t.Fatalf("run_blocked_reason = %v, want the run's report", got.RunBlockedReason)
	}
	if got.State != machine.ImplementingPlan {
		t.Fatalf("state = %v, the report must not finalize the run itself", got.State)
	}
}

// TestFinishUnattendedRun_LateBlockedReportRefused: once finalize has moved
// the run on, a blocked report can no longer keep its PR draft, so it is
// refused rather than silently dropped, and no finish is queued.
func TestFinishUnattendedRun_LateBlockedReportRefused(t *testing.T) {
	h := newFinishHarness(t, "chat-self")
	h.sessions.rows[deferralSessionID].State = machine.ReadyForReview
	h.srv.lifecycle = session.NewLifecycle(h.sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

	_, err := h.rpc(&pb.FinishUnattendedRunRequest{
		SessionId:               deferralSessionID,
		RequesterAgentSessionId: "chat-self",
		IsBlocked:               true,
		BlockedReason:           "too late",
	})
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", got, err)
	}
	if h.d.has(deferralSessionID) {
		t.Error("a refused blocked report left a pending finish")
	}
	if got, _ := h.sessions.Get(context.Background(), deferralSessionID); got.RunBlockedReason != nil {
		t.Errorf("run_blocked_reason = %q, want untouched", *got.RunBlockedReason)
	}
}

// TestFinishUnattendedRun_BlockedRunTakesOnlyItsOwnChat: a Blocked run accepts
// a finish or a finish --blocked from its own chat only. Another chat of the
// session is refused and leaves nothing pending and no report. An accepted
// finish --blocked writes nothing to the row before it fires, so a new chat
// that cancels it leaves the row's reason and block time as they were.
func TestFinishUnattendedRun_BlockedRunTakesOnlyItsOwnChat(t *testing.T) {
	for _, tc := range []struct {
		name, requester, reason string
		want                    connect.Code
	}{
		{name: "the run's chat finishes", requester: "chat-self"},
		{name: "the run's chat re-reports BLOCKED", requester: "chat-self", reason: "still red"},
		{name: "another chat finishes", requester: "chat-other", want: connect.CodePermissionDenied},
		{name: "another chat reports BLOCKED", requester: "chat-other", reason: "operator says so", want: connect.CodePermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFinishHarness(t, "chat-self", "chat-other")
			row := h.sessions.rows[deferralSessionID]
			runChat, blockedAt := "chat-self", time.Now().Add(-time.Hour)
			row.State, row.AgentSessionID, row.StateEnteredAt = machine.Blocked, &runChat, &blockedAt
			h.srv.lifecycle = session.NewLifecycle(h.sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

			resp, err := h.rpc(&pb.FinishUnattendedRunRequest{
				SessionId: deferralSessionID, RequesterAgentSessionId: tc.requester, IsBlocked: tc.reason != "", BlockedReason: tc.reason,
			})
			got, _ := h.sessions.Get(context.Background(), deferralSessionID)
			if tc.want != 0 {
				if connect.CodeOf(err) != tc.want || h.d.has(deferralSessionID) || got.RunBlockedReason != nil {
					t.Fatalf("err=%v pending=%v run_blocked_reason=%v, want %v with nothing pending or reported",
						err, h.d.has(deferralSessionID), got.RunBlockedReason, tc.want)
				}
				return
			}
			if err != nil || !resp.GetIsDeferred() {
				t.Fatalf("resp=%+v err=%v, want the finish deferred", resp, err)
			}
			if got.RunBlockedReason != nil || got.State != machine.Blocked {
				t.Fatalf("run_blocked_reason=%v state=%v, the request must not touch the row before the finish fires",
					got.RunBlockedReason, got.State)
			}
			if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e62"})); err != nil {
				t.Fatalf("RecordChat(new): %v", err)
			}
			got, _ = h.sessions.Get(context.Background(), deferralSessionID)
			if h.d.has(deferralSessionID) || got.RunBlockedReason != nil || got.State != machine.Blocked || !got.StateEnteredAt.Equal(blockedAt) {
				t.Fatalf("pending=%v run_blocked_reason=%v state=%v blocked at=%v, want the finish dropped and the row as it was",
					h.d.has(deferralSessionID), got.RunBlockedReason, got.State, got.StateEnteredAt)
			}
		})
	}
}

// TestFinishUnattendedRun_BlockedRunRefusesWhileAFinishRuns: a finish of a
// Blocked run that arrives while an earlier finish of it is executing is
// refused. Joining would report success for a claim the running finish never
// reads, so a later finish --blocked would be dropped and the PR readied.
func TestFinishUnattendedRun_BlockedRunRefusesWhileAFinishRuns(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{name: "finish --blocked", reason: "review found a P1"},
		{name: "finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFinishHarness(t, "chat-self")
			row := h.sessions.rows[deferralSessionID]
			runChat, blockedAt := "chat-self", time.Now().Add(-time.Hour)
			row.State, row.AgentSessionID, row.StateEnteredAt = machine.Blocked, &runChat, &blockedAt
			h.srv.lifecycle = session.NewLifecycle(h.sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())

			h.d.mu.Lock()
			running := h.d.claimLocked(deferralSessionID)
			h.d.mu.Unlock()
			answered := make(chan error, 1)
			go func() {
				_, err := h.rpc(&pb.FinishUnattendedRunRequest{
					SessionId: deferralSessionID, RequesterAgentSessionId: "chat-self", IsBlocked: tc.reason != "", BlockedReason: tc.reason,
				})
				answered <- err
			}()
			var err error
			select {
			case err = <-answered:
				h.d.finish(deferralSessionID, running, nil)
			case <-time.After(2 * time.Second):
				h.d.finish(deferralSessionID, running, nil)
				err = <-answered
			}

			if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
				t.Fatalf("code = %v (err=%v), want FailedPrecondition while a finish runs", got, err)
			}
			if h.d.has(deferralSessionID) {
				t.Fatal("a refused finish left a pending finish")
			}
			if got, _ := h.sessions.Get(context.Background(), deferralSessionID); got.RunBlockedReason != nil || got.State != machine.Blocked {
				t.Fatalf("run_blocked_reason=%v state=%v, want the row untouched", got.RunBlockedReason, got.State)
			}
		})
	}
}

// TestRunBlockedSessionDisplaysAsBlocked: once finalize has Blocked a run that
// reported BLOCKED, its row carries the run's reason as attention and its
// draft PR renders red, never a green ready label.
func TestRunBlockedSessionDisplaysAsBlocked(t *testing.T) {
	reason := "run blocked: review left an unresolved P1"
	sess := &models.Session{ID: "sess-1", RepoID: "repo-1", State: machine.Blocked, BlockedReason: &reason}
	p := SessionToProto(sess)
	HydrateBaseAttention(p, sess, &models.Repo{ID: "repo-1"}, nil)

	if a := p.GetAttentionStatus(); !a.GetNeedsAttention() || a.GetSummary() != reason {
		t.Fatalf("attention = %+v, want needs attention with %q", a, reason)
	}
	p.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_DRAFT
	out := displaystatus.Compute(displaystatus.Input{Session: p, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE})
	if out.Label != "draft" || out.Intent != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("display = %+v, want a red draft label", out)
	}
}

func TestNew_RegistersChatCreatedCancel(t *testing.T) {
	h := newDeferralHarness(t, "chat-a")
	lc := session.NewLifecycle(h.sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop())
	srv := New(Config{Lifecycle: lc, Sessions: h.sessions, AgentChats: h.chats, ChatStatus: h.tracker, DisplayTracker: h.display, Logger: zerolog.Nop()})
	srv.archiveDeferrer = h.d
	finish := newArchiveDeferrer(h.chats, h.tracker, nil, h.sessions, h.exec.archive, zerolog.Nop())
	srv.finishDeferrer = finish
	h.tracker.Update("chat-a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	if out, err := srv.RequestArchive(context.Background(), deferralSessionID, ArchiveRequest{}); err != nil || !out.Pending {
		t.Fatalf("archive = %+v, err = %v", out, err)
	}
	if out, err := finish.request(context.Background(), deferralSessionID, ArchiveRequest{}); err != nil || !out.Pending {
		t.Fatalf("finish = %+v, err = %v", out, err)
	}
	lc.NotifyChatCreated("another-session")
	if !h.d.has(deferralSessionID) || !finish.has(deferralSessionID) || !h.archiving() {
		t.Fatal("another session cancelled pending work")
	}
	lc.NotifyChatCreated(deferralSessionID)
	if h.d.has(deferralSessionID) || finish.has(deferralSessionID) || h.archiving() {
		t.Fatal("new chat did not clear both pending deferrals and archiving flag")
	}
}
