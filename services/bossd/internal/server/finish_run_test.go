package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
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
	clock    *fakeClock
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
		clock: &fakeClock{t: time.Now()},
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
	h.d = newArchiveDeferrer(h.chats, h.tracker, nil, h.sessions, h.exec.archive, zerolog.Nop())
	h.d.settled = h.quiet.settled
	h.d.fireBudget = finishRunBudget
	h.d.now = h.clock.Now
	h.d.interval = time.Hour
	h.srv.finishDeferrerOnce.Do(func() { h.srv.finishDeferrer = h.d })
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
	h.clock.set(accepted)

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
	h.clock.set(time.Now())
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
	h.clock.set(time.Now().Add(-requesterSettle - time.Second))
	if out, err := h.srv.finishDeferral().request(context.Background(), deferralSessionID, ArchiveRequest{RequesterAgentSessionID: "chat-self"}); err != nil || !out.Pending {
		t.Fatalf("outcome = %+v err = %v, want pending", out, err)
	}
	h.clock.set(time.Now())
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
	h := newFinishHarness(t, "chat-a")
	if resp, err := h.rpc(&pb.FinishUnattendedRunRequest{SessionId: deferralSessionID, RequesterAgentSessionId: "chat-a"}); err != nil || !resp.GetIsDeferred() {
		t.Fatalf("response = %+v err = %v, want deferred", resp, err)
	}

	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "chat-a"})); err != nil {
		t.Fatalf("RecordChat(existing): %v", err)
	}
	if !h.d.has(deferralSessionID) {
		t.Fatal("resuming an existing chat cancelled the pending finish")
	}

	if _, err := h.srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{SessionId: deferralSessionID, AgentSessionId: "chat-new"})); err != nil {
		t.Fatalf("RecordChat(new): %v", err)
	}
	if h.d.has(deferralSessionID) {
		t.Error("a new chat did not cancel the pending finish")
	}

	h.clock.set(time.Now().Add(time.Hour))
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
	srv := &Server{
		sessions:   sessions,
		agentChats: newDeferralChatStore(),
		lifecycle:  session.NewLifecycle(sessions, nil, nil, nil, nil, nil, nil, nil, zerolog.Nop()),
		logger:     zerolog.Nop(),
	}
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
