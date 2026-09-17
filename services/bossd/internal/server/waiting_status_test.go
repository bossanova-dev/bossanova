package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

// waitingChatListFake serves a fixed, ordered chat list for one session. The
// order matters: the waiting-reason winner must be picked from an ordered slice,
// never from Go map iteration.
type waitingChatListFake struct {
	db.AgentChatStore
	chats []*models.AgentChat
}

func (f *waitingChatListFake) ListBySession(_ context.Context, _ string) ([]*models.AgentChat, error) {
	return f.chats, nil
}

const waitingTestReason = "awaiting checks_passed_ready on acme/widget#123"

// newWaitingStatusServer wires the minimum Server surface the chat/session
// status RPCs need, with the given chats all reported WORKING.
func newWaitingStatusServer(t *testing.T, agentSessionIDs ...string) (*Server, *status.Tracker) {
	t.Helper()
	chats := make([]*models.AgentChat, 0, len(agentSessionIDs))
	tracker := status.NewTracker()
	for _, id := range agentSessionIDs {
		chats = append(chats, &models.AgentChat{AgentSessionID: id, SessionID: "sess-1"})
		tracker.Update(id, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	}
	return &Server{
		chatStatus: tracker,
		agentChats: &waitingChatListFake{chats: chats},
	}, tracker
}

func chatEntryByID(statuses []*pb.ChatStatusEntry, id string) *pb.ChatStatusEntry {
	for _, e := range statuses {
		if e.GetAgentSessionId() == id {
			return e
		}
	}
	return nil
}

// A chat the derivation layer marked as parked must be SERVED as waiting, with
// the canonical reason attached — the marker is useless if the RPC keeps
// reporting the raw WORKING heartbeat.
func TestGetChatStatuses_ArmedCallbackChatReportsWaiting(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if got := entry.GetWaitingReason(); got != waitingTestReason {
		t.Fatalf("waiting_reason = %q, want %q", got, waitingTestReason)
	}
}

// Over-reporting waiting is the failure mode to guard against: with no marker,
// a working chat stays working and carries no reason.
func TestGetChatStatuses_NoMarkerStaysWorking(t *testing.T) {
	s, _ := newWaitingStatusServer(t, "agent-a")

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("status = %v, want WORKING", got)
	}
	if got := entry.GetWaitingReason(); got != "" {
		t.Fatalf("waiting_reason = %q, want empty", got)
	}
}

// A stale marker on a chat that is no longer working must not resurrect the
// waiting state: the reported status is the gate.
func TestGetChatStatuses_MarkerOnNonWorkingChatIsIgnored(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)
	tracker.Update("agent-a", pb.ChatStatus_CHAT_STATUS_QUESTION, time.Now())

	resp, err := s.GetChatStatuses(context.Background(), connect.NewRequest(&pb.GetChatStatusesRequest{
		SessionId: "sess-1",
	}))
	if err != nil {
		t.Fatalf("GetChatStatuses: %v", err)
	}
	entry := chatEntryByID(resp.Msg.GetStatuses(), "agent-a")
	if entry == nil {
		t.Fatal("no status entry for agent-a")
	}
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_QUESTION {
		t.Fatalf("status = %v, want QUESTION", got)
	}
	if got := entry.GetWaitingReason(); got != "" {
		t.Fatalf("waiting_reason = %q, want empty", got)
	}
}

func TestGetSessionStatuses_SurfacesWaitingReason(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-a")
	tracker.SetWaiting("agent-a", waitingTestReason)

	resp, err := s.GetSessionStatuses(context.Background(), connect.NewRequest(&pb.GetSessionStatusesRequest{
		SessionIds: []string{"sess-1"},
	}))
	if err != nil {
		t.Fatalf("GetSessionStatuses: %v", err)
	}
	if len(resp.Msg.GetStatuses()) != 1 {
		t.Fatalf("got %d session statuses, want 1", len(resp.Msg.GetStatuses()))
	}
	entry := resp.Msg.GetStatuses()[0]
	if got := entry.GetStatus(); got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if got := entry.GetWaitingReason(); got != waitingTestReason {
		t.Fatalf("waiting_reason = %q, want %q", got, waitingTestReason)
	}
}

// The session aggregate keeps a session with real work in it honest: one parked
// chat alongside one genuinely working chat is a working session, and it must
// not inherit the parked chat's reason.
func TestChatStatusFromSessionChats_WorkingSiblingBeatsWaiting(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-busy")
	tracker.SetWaiting("agent-parked", waitingTestReason)

	got, reason, ok, _ := s.chatStatusAndWaitingAggregate(context.Background(),
		s.agentChats.(*waitingChatListFake).chats, true)
	if !ok {
		t.Fatal("chatStatusAndWaitingAggregate reported not-loaded")
	}
	if got != pb.ChatStatus_CHAT_STATUS_WORKING {
		t.Fatalf("status = %v, want WORKING", got)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty (the session is working, not waiting)", reason)
	}
}

// Waiting outranks idle: a parked chat is more informative than a sibling that
// is merely sitting at a prompt.
func TestChatStatusFromSessionChats_WaitingBeatsIdle(t *testing.T) {
	s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-idle")
	tracker.SetWaiting("agent-parked", waitingTestReason)
	tracker.Update("agent-idle", pb.ChatStatus_CHAT_STATUS_IDLE, time.Now())

	got, reason, ok, _ := s.chatStatusAndWaitingAggregate(context.Background(),
		s.agentChats.(*waitingChatListFake).chats, true)
	if !ok {
		t.Fatal("chatStatusAndWaitingAggregate reported not-loaded")
	}
	if got != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("status = %v, want WAITING", got)
	}
	if reason != waitingTestReason {
		t.Fatalf("reason = %q, want %q", reason, waitingTestReason)
	}
}

// With two equally-parked chats the surfaced reason must be a function of the
// data, not of map iteration order: run it repeatedly and require one answer.
func TestChatStatusFromSessionChats_DeterministicReasonAcrossParkedChats(t *testing.T) {
	const otherReason = "awaiting merged on acme/widget#7"
	s, tracker := newWaitingStatusServer(t, "agent-b", "agent-a")
	tracker.SetWaiting("agent-b", otherReason)
	tracker.SetWaiting("agent-a", waitingTestReason)

	chats := s.agentChats.(*waitingChatListFake).chats
	for i := 0; i < 50; i++ {
		got, reason, ok, _ := s.chatStatusAndWaitingAggregate(context.Background(), chats, true)
		if !ok {
			t.Fatal("chatStatusAndWaitingAggregate reported not-loaded")
		}
		if got != pb.ChatStatus_CHAT_STATUS_WAITING {
			t.Fatalf("status = %v, want WAITING", got)
		}
		// agent-a sorts first, so its reason is the deterministic winner even
		// though agent-b comes first in the chat list.
		if reason != waitingTestReason {
			t.Fatalf("iteration %d: reason = %q, want %q", i, reason, waitingTestReason)
		}
	}
}

// The pre-existing ladder is unchanged by the new rung.
func TestChatStatusFromSessionChats_QuestionAndLimitedStillBeatWaiting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		other pb.ChatStatus
	}{
		{"question", pb.ChatStatus_CHAT_STATUS_QUESTION},
		{"limited", pb.ChatStatus_CHAT_STATUS_LIMITED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tracker := newWaitingStatusServer(t, "agent-parked", "agent-other")
			tracker.SetWaiting("agent-parked", waitingTestReason)
			tracker.Update("agent-other", tc.other, time.Now())

			got, reason, ok, _ := s.chatStatusAndWaitingAggregate(context.Background(),
				s.agentChats.(*waitingChatListFake).chats, true)
			if !ok {
				t.Fatal("chatStatusAndWaitingAggregate reported not-loaded")
			}
			if got != tc.other {
				t.Fatalf("status = %v, want %v", got, tc.other)
			}
			if reason != "" {
				t.Fatalf("reason = %q, want empty", reason)
			}
		})
	}
}

// --- BOS-1269: the idle-derived waiting demotion, at the ListSessions producer ---

// demotionStores wires a real in-memory database plus the two live trackers, so
// BOTH producers can be driven over ONE fixture. The fakes used elsewhere in
// this package cannot serve the persisting producer, which writes through a
// real SessionStore.
type demotionStores struct {
	sessions  db.SessionStore
	workflows db.WorkflowStore
	chats     db.AgentChatStore
	repos     db.RepoStore
	display   *status.DisplayTracker
	chatState *status.Tracker
	sessionID string
}

func newDemotionStores(t *testing.T) *demotionStores {
	t.Helper()
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_, thisFile, _, _ := runtime.Caller(0)
	if err := migrate.Run(database, os.DirFS(filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations"))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repos := db.NewRepoStore(database)
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "my-app",
		LocalPath:         "/tmp/bos1269-" + t.Name(),
		OriginURL:         "https://github.com/acme/my-app",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sessions := db.NewSessionStore(database)
	sess, err := sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     repo.ID,
		Title:      "Ship the release checklist",
		BranchName: "boss/release-checklist",
		BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &demotionStores{
		sessions:  sessions,
		workflows: db.NewWorkflowStore(database),
		chats:     db.NewAgentChatStore(database),
		repos:     repos,
		display:   status.NewDisplayTracker(),
		chatState: status.NewTracker(),
		sessionID: sess.ID,
	}
}

// addChat registers a chat reporting the given status, optionally parked on an
// armed callback. The session is addressed by id everywhere these tests need
// it, so the chat's own agent-session id is not returned.
func (d *demotionStores) addChat(t *testing.T, name string, reported pb.ChatStatus, parked bool) {
	t.Helper()
	id := "agent-" + name
	if _, err := d.chats.Create(context.Background(), db.CreateAgentChatParams{
		SessionID:      d.sessionID,
		AgentSessionID: id,
		Title:          name,
	}); err != nil {
		t.Fatalf("create chat %s: %v", name, err)
	}
	d.chatState.Update(id, reported, time.Now())
	if parked {
		d.chatState.SetWaiting(id, waitingTestReason)
	}
}

// persisted runs the persisting producer and returns the label it wrote.
func (d *demotionStores) persisted(t *testing.T) string {
	t.Helper()
	computer := status.NewDisplayStatusComputer(
		d.sessions, d.display, d.chatState, d.chats, d.workflows, zerolog.Nop(),
	)
	// The persisting producer derives the waiting reason through its own lookup
	// rather than reading the tracker, so mirror what the tracker already holds.
	computer.SetWaitingLookup(status.WaitingLookupFunc(func(_ context.Context, agentSessionID string) (string, error) {
		return d.chatState.Waiting(agentSessionID), nil
	}))
	if err := computer.Recompute(context.Background(), d.sessionID); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	sess, err := d.sessions.Get(context.Background(), d.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.DisplayLabel
}

// served runs the ListSessions producer and returns the Session it served.
func (d *demotionStores) served(t *testing.T) *pb.Session {
	t.Helper()
	s := &Server{
		repos:          d.repos,
		sessions:       d.sessions,
		agentChats:     d.chats,
		displayTracker: d.display,
		chatStatus:     d.chatState,
	}
	resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, p := range resp.Msg.GetSessions() {
		if p.GetId() == d.sessionID {
			return p
		}
	}
	t.Fatalf("session %s absent from ListSessions", d.sessionID)
	return nil
}

// TestListSessions_IdleDerivedWaitingOverPassingPRServesThePRLabelAndTheMark is
// the ListSessions producer's half of the reported symptom, plus the transport
// mark the down-convert keys on.
func TestListSessions_IdleDerivedWaitingOverPassingPRServesThePRLabelAndTheMark(t *testing.T) {
	d := newDemotionStores(t)
	d.addChat(t, "idle", pb.ChatStatus_CHAT_STATUS_IDLE, true)
	d.display.Set(d.sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	got := d.served(t)
	if got.GetDisplayLabel() != "✓ passing" {
		t.Fatalf("display_label = %q, want %q", got.GetDisplayLabel(), "✓ passing")
	}
	if got.GetDisplayIntent() != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Fatalf("display_intent = %v, want SUCCESS", got.GetDisplayIntent())
	}
	if got.GetDisplaySpinner() {
		t.Fatal("display_spinner = true, want false")
	}
	if !got.GetIsWaitingDemoted() {
		t.Fatal("is_waiting_demoted = false, want true — the inverse has nothing to key on without it")
	}
}

// TestListSessions_WorkingDerivedWaitingKeepsWaitingAndCarriesNoMark is R2 at
// the serving producer. The absent mark matters as much as the label: a mark on
// a row that never demoted would make the down-convert manufacture a wait.
func TestListSessions_WorkingDerivedWaitingKeepsWaitingAndCarriesNoMark(t *testing.T) {
	d := newDemotionStores(t)
	d.addChat(t, "working", pb.ChatStatus_CHAT_STATUS_WORKING, true)
	d.display.Set(d.sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	got := d.served(t)
	if got.GetDisplayLabel() != displaystatus.WaitingLabel {
		t.Fatalf("display_label = %q, want %q", got.GetDisplayLabel(), displaystatus.WaitingLabel)
	}
	if got.GetIsWaitingDemoted() {
		t.Fatal("is_waiting_demoted = true on an un-demoted row, want false")
	}
}

// TestListSessions_OrdinaryGreenRowCarriesNoMark pins the most dangerous
// negative: an ordinary passing session with no wait at all must never be
// marked, or the down-convert would rewrite it into "waiting".
func TestListSessions_OrdinaryGreenRowCarriesNoMark(t *testing.T) {
	d := newDemotionStores(t)
	d.addChat(t, "idle", pb.ChatStatus_CHAT_STATUS_IDLE, false)
	d.display.Set(d.sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	got := d.served(t)
	if got.GetDisplayLabel() != "✓ passing" {
		t.Fatalf("precondition: display_label = %q, want ✓ passing", got.GetDisplayLabel())
	}
	if got.GetIsWaitingDemoted() {
		t.Fatal("is_waiting_demoted = true on a row that was never waiting, want false")
	}
}

// TestDisplayComposite_BothProducersAgree is the cross-producer guard the
// Problem Frame calls for: the composite is written by two entirely different
// code paths — the persisting recompute and the ListSessions in-flight
// overwrite — which fold chat status through different functions. Nothing
// guarded their agreement before this test, so teaching one and not the other
// produced a session that read "✓ passing" from a stream delta and "waiting"
// from a list refresh.
func TestDisplayComposite_BothProducersAgree(t *testing.T) {
	cases := []struct {
		name     string
		reported pb.ChatStatus
		parked   bool
		pr       vcs.DisplayStatus
	}{
		{"idle-derived wait over a passing PR", pb.ChatStatus_CHAT_STATUS_IDLE, true, vcs.DisplayStatusPassing},
		{"idle-derived wait over an approved PR", pb.ChatStatus_CHAT_STATUS_IDLE, true, vcs.DisplayStatusApproved},
		{"working-derived wait over a passing PR", pb.ChatStatus_CHAT_STATUS_WORKING, true, vcs.DisplayStatusPassing},
		{"idle-derived wait over a failing PR", pb.ChatStatus_CHAT_STATUS_IDLE, true, vcs.DisplayStatusFailing},
		{"idle-derived wait over a PR in review", pb.ChatStatus_CHAT_STATUS_IDLE, true, vcs.DisplayStatusReview},
		{"no wait at all over a passing PR", pb.ChatStatus_CHAT_STATUS_IDLE, false, vcs.DisplayStatusPassing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDemotionStores(t)
			d.addChat(t, "a", tc.reported, tc.parked)
			d.display.Set(d.sessionID, vcs.DisplayInfo{Status: tc.pr})

			persisted := d.persisted(t)
			served := d.served(t).GetDisplayLabel()
			if persisted != served {
				t.Fatalf("producers disagree: persisted %q, ListSessions served %q", persisted, served)
			}
		})
	}
}

// TestListSessions_SessionWithNoChatsIsUnaffected keeps the fail-safe honest at
// the boundary: no chats means no waiting-resolved chat, so the aggregate is
// false and nothing demotes.
func TestListSessions_SessionWithNoChatsIsUnaffected(t *testing.T) {
	d := newDemotionStores(t)
	d.display.Set(d.sessionID, vcs.DisplayInfo{Status: vcs.DisplayStatusPassing})

	got := d.served(t)
	if got.GetIsWaitingDemoted() {
		t.Fatal("is_waiting_demoted = true for a session with no chats, want false")
	}
}

// TestChatStatusAndWaitingAggregate_IsOrderIndependent pins R6 at the fold that
// actually breaks ties on agent-session id: swapping the two chats' ids must not
// change the aggregate, and therefore cannot change the label.
func TestChatStatusAndWaitingAggregate_IsOrderIndependent(t *testing.T) {
	build := func(firstID, secondID string, firstReported, secondReported pb.ChatStatus) (pb.ChatStatus, bool) {
		tracker := status.NewTracker()
		tracker.Update(firstID, firstReported, time.Now())
		tracker.Update(secondID, secondReported, time.Now())
		tracker.SetWaiting(firstID, waitingTestReason)
		tracker.SetWaiting(secondID, waitingTestReason)
		chats := []*models.AgentChat{
			{AgentSessionID: firstID, SessionID: "sess-1"},
			{AgentSessionID: secondID, SessionID: "sess-1"},
		}
		s := &Server{chatStatus: tracker, agentChats: &waitingChatListFake{chats: chats}}
		st, _, _, allIdle := s.chatStatusAndWaitingAggregate(context.Background(), chats, true)
		return st, allIdle
	}

	// One idle-derived, one working-derived parked chat. The aggregate must be
	// false either way round, so the tie-break winner cannot decide the label.
	stA, idleA := build("agent-aaa", "agent-zzz", pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_WORKING)
	stB, idleB := build("agent-aaa", "agent-zzz", pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_IDLE)
	if stA != pb.ChatStatus_CHAT_STATUS_WAITING || stB != pb.ChatStatus_CHAT_STATUS_WAITING {
		t.Fatalf("precondition: folded statuses = %v/%v, want both WAITING", stA, stB)
	}
	if idleA || idleB {
		t.Fatalf("aggregate = %v/%v after swapping which id is idle-derived, want both false", idleA, idleB)
	}

	// Both idle-derived: true either way round.
	_, bothA := build("agent-aaa", "agent-zzz", pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_IDLE)
	_, bothB := build("agent-zzz", "agent-aaa", pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_IDLE)
	if !bothA || !bothB {
		t.Fatalf("aggregate = %v/%v for two idle-derived chats, want both true", bothA, bothB)
	}
}
