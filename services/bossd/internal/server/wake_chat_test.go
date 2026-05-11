package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// chatStoreFake satisfies db.AgentChatStore for WakeChat's narrow needs:
// GetByAgentSessionID and UpdateTmuxSessionName. The remaining methods of
// the interface are inherited from the embedded nil interface; calling them
// in a test would panic, signalling the handler reached for surface area
// it shouldn't.
type chatStoreFake struct {
	db.AgentChatStore
	mu                 sync.Mutex
	chat               *models.AgentChat
	updateName         *string
	updateNameCall     int
	updateProvider     *string
	updateProviderCall int
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

func (f *chatStoreFake) UpdateProviderSessionID(_ context.Context, _ string, providerSessionID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateProvider = providerSessionID
	f.updateProviderCall++
	if f.chat != nil {
		f.chat.ProviderSessionID = providerSessionID
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
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        claudeArgvBuilder(),
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

func TestWakeChatInternal_NilTmuxNoops(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        claudeArgvBuilder(),
		},
	}

	outcome, tmuxName, reason, err := s.WakeChatInternal(context.Background(), "agent-1", false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if outcome != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive", outcome)
	}
	wantTmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if tmuxName != wantTmuxName {
		t.Fatalf("got tmux name %q, want %q", tmuxName, wantTmuxName)
	}
	if reason != "" {
		t.Fatalf("got reason %q, want empty", reason)
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

// TestWakeChat_RoutesArgvByAgentName is the wake-side mirror of
// TestSpawnChatTmux_RoutesArgvByAgentName: a chat row persisted as
// AgentName="codex" must wake into a `codex …` tmux command, not the
// historical hardcoded `claude …`. This pins the second of the two
// broken spawn paths the codex routing fix addresses.
func TestWakeChat_RoutesArgvByAgentName(t *testing.T) {
	// Wake routing reads AgentName off the chat row (the per-chat override),
	// not the parent session — that's what RecordChat persists when the
	// user picks "codex" in the chat picker. Keep the session minimal.
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh: map[string][]string{
					"claude": {"claude", "--session-id"},
					"codex":  {"codex"},
				},
				resume: map[string][]string{
					"claude": {"claude", "--resume"},
					"codex":  {"codex", "resume"},
				},
			},
		},
	}

	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(tmuxer.captured) == 0 || tmuxer.captured[0] != "codex" {
		t.Fatalf("WakeChat for codex-typed chat must spawn codex, got cmd=%v", tmuxer.captured)
	}
}

func TestWakeChat_UsesProviderSessionIDForResume(t *testing.T) {
	providerID := "codex-real-1"
	chat := &models.AgentChat{
		ID:                "c1",
		AgentSessionID:    "agent-1",
		ProviderSessionID: &providerID,
		SessionID:         "s1",
		AgentName:         "codex",
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	transcripts := &fakeTranscriptOracle{exists: true}
	builder := &fakeArgvBuilder{
		fresh:  map[string][]string{"codex": {"codex"}},
		resume: map[string][]string{"codex": {"codex", "resume"}},
	}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: transcripts,
			argv:        builder,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("got %v, want OUTCOME_RESUMED", resp.Msg.Outcome)
	}
	if len(transcripts.calls) != 1 || transcripts.calls[0].agentSessionID != providerID {
		t.Fatalf("TranscriptExists must use provider id %q, got calls=%+v", providerID, transcripts.calls)
	}
	if len(builder.calls) != 1 || builder.calls[0].agentSessionID != providerID {
		t.Fatalf("BuildInteractive must use provider id %q, got calls=%+v", providerID, builder.calls)
	}
	if tmuxer.lastName != tmux.ChatSessionName("r1", "agent-1") {
		t.Fatalf("tmux name must use agent session id, got %q", tmuxer.lastName)
	}
}

func TestWakeChat_LegacyCodexBackfillsProviderSessionIDAndResumes(t *testing.T) {
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "codex",
		CreatedAt:      createdAt,
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	transcripts := &fakeTranscriptOracle{existsFor: map[string]bool{"codex-real-1": true}}
	resolver := &fakeInteractiveSessionResolver{sessionID: "codex-real-1"}
	builder := &fakeArgvBuilder{
		fresh:  map[string][]string{"codex": {"codex"}},
		resume: map[string][]string{"codex": {"codex", "resume"}},
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: transcripts,
			argv:        builder,
			resolver:    resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("got %v, want OUTCOME_RESUMED", resp.Msg.Outcome)
	}
	if store.updateProviderCall != 1 || store.updateProvider == nil || *store.updateProvider != "codex-real-1" {
		t.Fatalf("provider id not backfilled, calls=%d value=%v", store.updateProviderCall, store.updateProvider)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("legacy backfill should resolve once before spawn, got %d calls", len(resolver.calls))
	}
	if !resolver.calls[0].allowLegacyBackfill || !resolver.calls[0].chatCreatedAt.Equal(createdAt) ||
		!resolver.calls[0].launchedAfter.Equal(createdAt.Add(-5*time.Minute)) {
		t.Fatalf("legacy resolver call = %+v, want allow backfill with chat created_at and bounded launched_after fallback", resolver.calls[0])
	}
	if len(transcripts.calls) != 1 || transcripts.calls[0].agentSessionID != "codex-real-1" {
		t.Fatalf("TranscriptExists must use backfilled provider id, got calls=%+v", transcripts.calls)
	}
	if len(builder.calls) != 1 || builder.calls[0].agentSessionID != "codex-real-1" || !builder.calls[0].resume {
		t.Fatalf("BuildInteractive must resume provider id, got calls=%+v", builder.calls)
	}
}

func TestWakeChat_ForceFreshSkipsLegacyCodexBackfill(t *testing.T) {
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "codex",
		CreatedAt:      time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	resolver := &fakeInteractiveSessionResolver{
		legacySessionID: "stale-codex-real-1",
		ambiguous:       true,
		reason:          "fresh discovery not relevant",
	}
	builder := &fakeArgvBuilder{
		fresh:  map[string][]string{"codex": {"codex"}},
		resume: map[string][]string{"codex": {"codex", "resume"}},
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{existsFor: map[string]bool{"stale-codex-real-1": true}},
			argv:        builder,
			resolver:    resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
		ForceFresh:     true,
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if store.updateProviderCall != 0 {
		t.Fatalf("forced fresh wake should not persist legacy provider id, got %d calls", store.updateProviderCall)
	}
	for _, call := range resolver.calls {
		if call.allowLegacyBackfill {
			t.Fatalf("forced fresh wake should not run legacy backfill, got calls=%+v", resolver.calls)
		}
	}
	if len(builder.calls) != 1 || builder.calls[0].agentSessionID != "agent-1" || builder.calls[0].resume {
		t.Fatalf("forced fresh build should use agent id without resume, got calls=%+v", builder.calls)
	}
}

func TestWakeChat_LegacyCodexAmbiguousBackfillDoesNotGuess(t *testing.T) {
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "codex",
		CreatedAt:      time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	resolver := &fakeInteractiveSessionResolver{
		ambiguous: true,
		reason:    "multiple matching codex-tui rollouts found",
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{existsFor: map[string]bool{"codex-real-1": true}},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"codex": {"codex"}},
				resume: map[string][]string{"codex": {"codex", "resume"}},
			},
			resolver: resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if store.updateProviderCall != 0 {
		t.Fatalf("ambiguous legacy backfill should not persist provider id, got %d calls", store.updateProviderCall)
	}
	if len(resolver.calls) != 2 || !resolver.calls[0].allowLegacyBackfill || resolver.calls[1].allowLegacyBackfill {
		t.Fatalf("resolver calls = %+v, want legacy attempt then normal fresh discovery", resolver.calls)
	}
}

func TestWakeChat_ClaudeDoesNotRunLegacyCodexBackfill(t *testing.T) {
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "claude",
		CreatedAt:      time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	resolver := &fakeInteractiveSessionResolver{sessionID: "codex-real-1"}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: true},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"claude": {"claude"}},
				resume: map[string][]string{"claude": {"claude", "--resume"}},
			},
			resolver: resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_RESUMED {
		t.Fatalf("got %v, want OUTCOME_RESUMED", resp.Msg.Outcome)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("claude wake should not run codex legacy resolver, got calls=%+v", resolver.calls)
	}
}

func TestWakeChat_PersistsDiscoveredProviderSessionID(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"codex": {"codex"}},
				resume: map[string][]string{"codex": {"codex", "resume"}},
			},
			resolver: &fakeInteractiveSessionResolver{sessionID: "codex-real-1"},
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if store.updateProviderCall != 1 || store.updateProvider == nil || *store.updateProvider != "codex-real-1" {
		t.Fatalf("provider id not persisted, calls=%d value=%v", store.updateProviderCall, store.updateProvider)
	}
}

func TestWakeChat_ResolverAmbiguousLogsAndDoesNotPersistProviderID(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	var logs bytes.Buffer
	resolver := &fakeInteractiveSessionResolver{
		ambiguous: true,
		reason:    "multiple codex-tui rollouts matched",
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		logger:     zerolog.New(&logs),
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"codex": {"codex"}},
				resume: map[string][]string{"codex": {"codex", "resume"}},
			},
			resolver: resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if resp.Msg.Reason != WakeFallbackReasonProviderIDDiscoveryAmbiguous {
		t.Fatalf("reason = %q, want %q", resp.Msg.Reason, WakeFallbackReasonProviderIDDiscoveryAmbiguous)
	}
	if store.updateProviderCall != 0 {
		t.Fatalf("provider id should not be persisted, got %d calls", store.updateProviderCall)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("ambiguous resolver should stop polling after one call, got %d", len(resolver.calls))
	}
	if !bytes.Contains(logs.Bytes(), []byte("wake chat fresh fallback")) ||
		!bytes.Contains(logs.Bytes(), []byte(`"reason":"provider_id_discovery_ambiguous"`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"ambiguous":true`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"agent_session_id":"agent-1"`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"provider_session_id":""`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"agent_name":"codex"`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"worktree":"`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"tmux_session":"boss-r1-agent-1"`)) ||
		!bytes.Contains(logs.Bytes(), []byte("multiple codex-tui rollouts matched")) {
		t.Fatalf("expected ambiguous provider id warning, got logs=%s", logs.String())
	}
}

func TestWakeChat_FreshAmbiguousReasonBeatsLegacyAmbiguousBackfill(t *testing.T) {
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "codex",
		CreatedAt:      time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	var logs bytes.Buffer
	resolver := &fakeInteractiveSessionResolver{
		ambiguous:       true,
		reason:          "fresh discovery matched multiple codex-tui rollouts",
		legacyAmbiguous: true,
		legacyReason:    "legacy discovery matched multiple codex-tui rollouts",
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		logger:     zerolog.New(&logs),
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"codex": {"codex"}},
				resume: map[string][]string{"codex": {"codex", "resume"}},
			},
			resolver: resolver,
		},
	}

	resp, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if resp.Msg.Reason != WakeFallbackReasonProviderIDDiscoveryAmbiguous {
		t.Fatalf("reason = %q, want %q", resp.Msg.Reason, WakeFallbackReasonProviderIDDiscoveryAmbiguous)
	}
	if store.updateProviderCall != 0 {
		t.Fatalf("provider id should not be persisted, got %d calls", store.updateProviderCall)
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"reason":"provider_id_discovery_ambiguous"`)) ||
		!bytes.Contains(logs.Bytes(), []byte(`"discovery_reason":"fresh discovery matched multiple codex-tui rollouts"`)) {
		t.Fatalf("expected fresh ambiguous warning, got logs=%s", logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte(WakeFallbackReasonLegacyProviderIDDiscoveryAmbiguous)) ||
		bytes.Contains(logs.Bytes(), []byte("legacy discovery matched multiple codex-tui rollouts")) {
		t.Fatalf("legacy ambiguous reason leaked into fresh discovery warning, got logs=%s", logs.String())
	}
}

func TestWakeChat_FreshDiscoveryTimeoutReasonBeatsLegacyAmbiguousBackfill(t *testing.T) {
	chat := &models.AgentChat{
		ID:             "c1",
		AgentSessionID: "agent-1",
		SessionID:      "s1",
		AgentName:      "codex",
		CreatedAt:      time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	resolver := &fakeInteractiveSessionResolver{
		legacyAmbiguous: true,
		legacyReason:    "legacy discovery matched multiple codex-tui rollouts",
		cancelOnFreshCall: func() {
			cancel()
		},
	}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		logger:     zerolog.New(&logs),
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh: map[string][]string{"codex": {"codex"}},
			},
			resolver: resolver,
		},
	}

	resp, err := s.WakeChat(ctx, connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("got %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if resp.Msg.Reason != WakeFallbackReasonProviderIDDiscoveryTimeout {
		t.Fatalf("reason = %q, want %q", resp.Msg.Reason, WakeFallbackReasonProviderIDDiscoveryTimeout)
	}
	if store.updateProviderCall != 0 {
		t.Fatalf("provider id should not be persisted, got %d calls", store.updateProviderCall)
	}
	if len(resolver.calls) != 2 {
		t.Fatalf("resolver should run legacy and fresh discovery, got %d calls", len(resolver.calls))
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"reason":"provider_id_discovery_timeout"`)) {
		t.Fatalf("expected fresh timeout warning, got logs=%s", logs.String())
	}
	if bytes.Contains(logs.Bytes(), []byte(WakeFallbackReasonLegacyProviderIDDiscoveryAmbiguous)) ||
		bytes.Contains(logs.Bytes(), []byte("legacy discovery matched multiple codex-tui rollouts")) ||
		bytes.Contains(logs.Bytes(), []byte(`"ambiguous":true`)) ||
		bytes.Contains(logs.Bytes(), []byte(`"discovery_reason"`)) {
		t.Fatalf("legacy ambiguous discovery leaked into timeout warning, got logs=%s", logs.String())
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
