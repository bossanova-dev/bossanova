package server

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// chatStoreFake satisfies db.AgentChatStore for WakeChat's narrow needs:
// GetByAgentSessionID and UpdateTmuxSessionName. The remaining methods of
// the interface are inherited from the embedded nil interface; calling them
// in a test would panic, signalling the handler reached for surface area
// it shouldn't.
type chatStoreFake struct {
	db.AgentChatStore
	mu             sync.Mutex
	chat           *models.AgentChat
	updateName     *string
	updateNameCall int
}

func (f *chatStoreFake) GetByAgentSessionID(_ context.Context, _ string) (*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chat == nil {
		return nil, sql.ErrNoRows
	}
	return f.chat, nil
}

func (f *chatStoreFake) UpdateTmuxSessionName(_ context.Context, _ string, name *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateName = name
	f.updateNameCall++
	if f.chat != nil {
		f.chat.TmuxSessionName = name
	}
	return nil
}

// sessionStoreFake satisfies db.SessionStore narrowly: only Get is wired.
type sessionStoreFake struct {
	db.SessionStore
	sess *models.Session
}

func (f *sessionStoreFake) Get(_ context.Context, _ string) (*models.Session, error) {
	if f.sess == nil {
		return nil, sql.ErrNoRows
	}
	return f.sess, nil
}

// newWakeTestServer wires a Server with the minimum surface WakeChat needs
// and installs the spawn-deps test hook on the server instance itself —
// no package-level state, so adding t.Parallel() is safe.
func newWakeTestServer(t *testing.T, chat *models.AgentChat, sess *models.Session, tmuxer *fakeTmuxClient) *Server {
	t.Helper()
	return &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: fakeTranscriptOracle{exists: false},
		},
	}
}

func TestWakeChat_NotFound(t *testing.T) {
	s := newWakeTestServer(t, nil, nil, &fakeTmuxClient{available: true})
	_, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "missing",
	}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", connect.CodeOf(err))
	}
}

func TestWakeChat_AlreadyLive(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newWakeTestServer(t, chat, sess, tmuxer)

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_ALREADY_LIVE {
		t.Fatalf("got %v, want OUTCOME_ALREADY_LIVE", resp.Msg.Outcome)
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("expected no spawn, got %d", tmuxer.createdN)
	}
}

func TestWakeChat_FreshFallback_NoTranscript(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newWakeTestServer(t, chat, sess, tmuxer)

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if !contains(tmuxer.captured, "--session-id") {
		t.Fatalf("expected --session-id, got %v", tmuxer.captured)
	}
}

func TestWakeChat_WorktreeMissing_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: "/nonexistent/path"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newWakeTestServer(t, chat, sess, tmuxer)

	_, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", connect.CodeOf(err))
	}
}

func TestWakeChat_ConcurrentCallsCollapseToOneSpawn(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false, slowCreate: true}
	s := newWakeTestServer(t, chat, sess, tmuxer)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
				AgentSessionId: "agent-1",
			}))
		}()
	}
	wg.Wait()

	tmuxer.mu.Lock()
	defer tmuxer.mu.Unlock()
	if tmuxer.createdN != 1 {
		t.Fatalf("singleflight should collapse to 1 spawn, got %d", tmuxer.createdN)
	}
}

func TestWakeChat_TmuxSpawnFailure_Internal(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false, createErr: errors.New("tmux: command not found")}
	s := newWakeTestServer(t, chat, sess, tmuxer)

	_, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected Internal, got %v", connect.CodeOf(err))
	}
}

// TestOutcomeAs_WireValuesMatch pins the invariant that the two proto enum
// types (WakeChatResponse_Outcome on the connect RPC, WakeChatResult_Outcome
// on the reverse stream) share the same numeric values for each Outcome.
// outcomeAs relies on this — if a future proto edit reorders either enum,
// the generic mapper would silently misroute outcomes to the wrong wire
// values. This test fails loudly the moment the assumption breaks.
func TestOutcomeAs_WireValuesMatch(t *testing.T) {
	cases := []struct {
		in          Outcome
		wantConnect pb.WakeChatResponse_Outcome
		wantStream  pb.WakeChatResult_Outcome
	}{
		{OutcomeAlreadyLive, pb.WakeChatResponse_OUTCOME_ALREADY_LIVE, pb.WakeChatResult_OUTCOME_ALREADY_LIVE},
		{OutcomeResumed, pb.WakeChatResponse_OUTCOME_RESUMED, pb.WakeChatResult_OUTCOME_RESUMED},
		{OutcomeFreshFallback, pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK, pb.WakeChatResult_OUTCOME_FRESH_FALLBACK},
		{Outcome(99), pb.WakeChatResponse_OUTCOME_UNSPECIFIED, pb.WakeChatResult_OUTCOME_UNSPECIFIED},
	}
	for _, tc := range cases {
		if got := outcomeAs[pb.WakeChatResponse_Outcome](tc.in); got != tc.wantConnect {
			t.Errorf("connect: outcomeAs(%v) = %v, want %v", tc.in, got, tc.wantConnect)
		}
		if got := outcomeAs[pb.WakeChatResult_Outcome](tc.in); got != tc.wantStream {
			t.Errorf("stream: outcomeAs(%v) = %v, want %v", tc.in, got, tc.wantStream)
		}
	}
}
