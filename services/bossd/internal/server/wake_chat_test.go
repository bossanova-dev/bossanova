package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
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
	// updateProviderCtxErr records ctx.Err() as the persist saw it, so a test can
	// prove the write does not inherit an already-spent discovery budget.
	updateProviderCtxErr error
	updateAccount        *string
	updateAccountCall    int
	// updateNameErr / updateProviderErr fail the row writes that run while a
	// freshly spawned pane is still unnamed, so a test can prove the pane is
	// rolled back rather than leaked (BOS-845).
	updateNameErr     error
	updateProviderErr error
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
	f.updateNameCall++
	if f.updateNameErr != nil {
		return f.updateNameErr
	}
	f.updateName = name
	if f.chat != nil {
		f.chat.TmuxSessionName = name
	}
	return nil
}

func (f *chatStoreFake) UpdateProviderSessionID(ctx context.Context, _ string, providerSessionID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateProviderCall++
	f.updateProviderCtxErr = ctx.Err()
	if f.updateProviderErr != nil {
		return f.updateProviderErr
	}
	f.updateProvider = providerSessionID
	if f.chat != nil {
		f.chat.ProviderSessionID = providerSessionID
	}
	return nil
}

func (f *chatStoreFake) UpdateAccountIDByAgentSessionID(_ context.Context, _ string, accountID *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateAccount = accountID
	f.updateAccountCall++
	if f.chat != nil {
		f.chat.AccountID = accountID
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
		agentLogsDir: filepath.Join(t.TempDir(), "agent-logs"),
		agentChats:   &chatStoreFake{chat: chat},
		sessions:     &sessionStoreFake{sess: sess},
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

// TestWakeChat_NeverOverwritesNonNilProviderSessionID pins the BOS-290 guard:
// once a codex chat has a provider_session_id, a wake-time re-resolution that
// returns a DIFFERENT id must NOT overwrite the stored value. Overwriting on
// difference is exactly what stamped a sibling chat's id over the correct one.
func TestWakeChat_NeverOverwritesNonNilProviderSessionID(t *testing.T) {
	correct := "correct-rollout-id"
	chat := &models.AgentChat{
		ID:                "c1",
		AgentSessionID:    "agent-1",
		SessionID:         "s1",
		AgentName:         "codex",
		ProviderSessionID: &correct,
		CreatedAt:         time.Now(),
	}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	store := &chatStoreFake{chat: chat}
	s := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        &fakeArgvBuilder{fresh: map[string][]string{"codex": {"codex"}}},
			// The resolver would (wrongly) return a different id; the guard must
			// prevent it from ever being written.
			resolver: &fakeInteractiveSessionResolver{sessionID: "different-sibling-id"},
		},
	}

	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.updateProviderCall != 0 {
		t.Errorf("UpdateProviderSessionID called %d time(s), want 0 (must never overwrite a non-nil id)", store.updateProviderCall)
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID != correct {
		t.Errorf("ProviderSessionID = %v, want unchanged %q", chat.ProviderSessionID, correct)
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

// TestWakeChatStream_HeadlessRunActive_FailedPrecondition locks in that the
// reverse-stream path classifies an active headless run as FAILED_PRECONDITION
// (matching the direct connect RPC), so bosso maps it to a retryable
// FailedPrecondition rather than leaking ERROR_CODE_UNSPECIFIED → CodeAborted.
func TestWakeChatStream_HeadlessRunActive_FailedPrecondition(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newWakeTestServer(t, chat, sess, tmuxer)
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	_, _, _, code, err := s.WakeChatStream(context.Background(), "agent-1", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrHeadlessRunActive) {
		t.Fatalf("expected ErrHeadlessRunActive, got %v", err)
	}
	if code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("expected ERROR_CODE_FAILED_PRECONDITION, got %v", code)
	}
	// The refusal must be actionable across every attach surface (they render the
	// error string verbatim): explain the duplicate-agent risk and point at the
	// live agent log to tail instead.
	if msg := err.Error(); !strings.Contains(msg, "duplicate") ||
		!strings.Contains(msg, "agent-logs/agent-1.log") {
		t.Fatalf("expected actionable headless-active message with log path, got %q", msg)
	}

	tmuxer.mu.Lock()
	spawnCount := tmuxer.createdN
	tmuxer.mu.Unlock()
	if spawnCount != 0 {
		t.Errorf("expected 0 spawns while headless run active, got %d", spawnCount)
	}
}

func TestHeadlessRunActiveMessageUsesConfiguredAgentLogsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-logs")
	err := headlessRunActiveMessage(dir, "agent-1")
	if !strings.Contains(err.Error(), filepath.Join(dir, "agent-1.log")) {
		t.Fatalf("want configured log path, got %q", err)
	}
	if strings.Contains(err.Error(), "~/.bossanova/agent-logs") {
		t.Fatalf("must not use hardcoded path: %q", err)
	}
}

// TestWakeChatInternal_TmuxUnattended_LivePane_NotHeadlessActive proves a
// tmux_unattended session with a LIVE tmux pane is NOT misclassified as an
// active headless run: the ErrHeadlessRunActive gate keys on the absence of a
// tmux session, and a durable tmux-hosted unattended run always has one, so
// waking it returns AlreadyLive rather than the headless-active error.
func TestWakeChatInternal_TmuxUnattended_LivePane_NotHeadlessActive(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), IsTmuxUnattended: true}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newWakeTestServer(t, chat, sess, tmuxer)
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update("agent-1", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())

	outcome, _, _, err := s.WakeChatInternal(context.Background(), "agent-1", false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// The meaningful assertion: a live-pane tmux_unattended session wakes
	// into AlreadyLive, not the headless-active refuse path. A nil err here
	// already rules out ErrHeadlessRunActive (which WakeChatInternal always
	// returns alongside a non-nil error), so the outcome check below is the
	// real proof this session wasn't misclassified as an active headless run.
	if outcome != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive", outcome)
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

// TestWakeChat_CrossAgentBindsAndPersistsDefaultAccount is the wake-side mirror
// of the attach-path account binding: waking a cross-agent chat (a codex chat
// living in a claude-bound session) with no chat-level account binding must
// materialize the CHAT provider's default account into the spawn env and
// persist that binding on the chat row, so restarts and subsequent wakes keep
// using the same provider account rather than falling back to the ambient CLI
// login.
func TestWakeChat_CrossAgentBindsAndPersistsDefaultAccount(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, s, "claude-work", []byte("blob"))
	codexAcct := mustAddCodex(t, s, "codex-work", []byte("blob"))
	mat := &fakeMaterializer{supports: true, env: map[string]string{"CODEX_TOKEN": "sk-codex-default"}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())

	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	chats := &chatStoreFake{chat: chat}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s.agentChats = chats
	s.sessions = &sessionStoreFake{sess: sess}
	s.wakeHook = wakeHook{
		spawner:     tmuxer,
		transcripts: &fakeTranscriptOracle{exists: false},
		argv: &fakeArgvBuilder{
			fresh:  map[string][]string{"claude": {"claude"}, "codex": {"codex"}},
			resume: map[string][]string{"claude": {"claude", "--resume"}, "codex": {"codex", "resume"}},
		},
	}

	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	// The codex provider's default account env is injected into the spawn.
	if got := tmuxer.lastEnv["CODEX_TOKEN"]; got != "sk-codex-default" {
		t.Fatalf("wake spawn env CODEX_TOKEN = %q, want sk-codex-default (env=%v)", got, tmuxer.lastEnv)
	}
	// It's the codex account that was materialized, never the parent claude one.
	if len(mat.materialize) == 0 || mat.materialize[len(mat.materialize)-1] != codexAcct.Id {
		t.Fatalf("materialized account = %v, want codex account %q", mat.materialize, codexAcct.Id)
	}
	// The binding is persisted on the chat row for future wakes/restarts.
	if chats.updateAccountCall != 1 {
		t.Fatalf("UpdateAccountIDByAgentSessionID calls = %d, want 1", chats.updateAccountCall)
	}
	if chats.updateAccount == nil || *chats.updateAccount != codexAcct.Id {
		t.Fatalf("persisted chat account_id = %v, want %s", chats.updateAccount, codexAcct.Id)
	}
}

// TestWakeChat_CrossAgentDropsSessionModel is the BOS-255 regression, now
// enforced structurally by chat-scoped model authority (BOS-381): a codex chat
// living inside a claude session forwards its OWN (here empty) chat.Model, never
// the session's claude "opus" — which Codex rejects as an unknown model. The
// session model is no longer consulted at all, so the empty forward is proof the
// session's opus cannot leak into the codex runner.
func TestWakeChat_CrossAgentDropsSessionModel(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "codex"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", Model: "opus"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := &fakeArgvBuilder{
		fresh: map[string][]string{
			"claude": {"claude", "--session-id"},
			"codex":  {"codex"},
		},
		resume: map[string][]string{
			"claude": {"claude", "--resume"},
			"codex":  {"codex", "resume"},
		},
	}
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        builder,
		},
	}

	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	if got := builder.calls[0].agentName; got != "codex" {
		t.Fatalf("cross-agent spawn agent = %q, want codex", got)
	}
	if got := builder.calls[0].model; got != "" {
		t.Fatalf("cross-agent spawn model = %q, want empty (codex must not receive a claude model)", got)
	}
}

// TestWakeChat_SameAgentForwardsModel guards the common path under chat-scoped
// model authority (BOS-381): the spawn forwards the CHAT's model ("opus"), not
// the session's. The session model is deliberately left empty to prove the
// forwarded value comes from chat.Model, not from the session.
func TestWakeChat_SameAgentForwardsModel(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude", Model: "opus"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude", Model: ""}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	s := &Server{
		agentChats: &chatStoreFake{chat: chat},
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv:        builder,
		},
	}

	if _, err := s.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	if got := builder.calls[0].model; got != "opus" {
		t.Fatalf("same-agent spawn model = %q, want opus", got)
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

// newWakeLegacyBackfillFixture builds the chat shape the wake-path legacy
// backfill acts on: a codex chat with no provider_session_id and a non-zero
// CreatedAt. existsFor programs the transcript oracle, which decides whether the
// spawn resumes (no fresh discovery) or falls back fresh.
func newWakeLegacyBackfillFixture(t *testing.T, resolver *fakeInteractiveSessionResolver, existsFor map[string]bool) (*Server, *chatStoreFake, *fakeTmuxClient) {
	t.Helper()
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
	srv := &Server{
		agentChats: store,
		sessions:   &sessionStoreFake{sess: sess},
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{existsFor: existsFor},
			argv: &fakeArgvBuilder{
				fresh:  map[string][]string{"codex": {"codex"}},
				resume: map[string][]string{"codex": {"codex", "resume"}},
			},
			resolver: resolver,
		},
	}
	return srv, store, tmuxer
}

// A failed legacy backfill must cost a resume id, never the whole wake. This is
// the wake-path sibling of the record path's warn-and-continue: before the fix
// the scan's error was returned straight out of WakeChatInternal, surfacing as
// `ResolveInteractiveSessionID: ... DeadlineExceeded` on a host whose codex
// corpus is large enough to blow the budget.
func TestWakeChat_LegacyBackfillFailureDoesNotFailTheWake(t *testing.T) {
	legacyErr := errors.New(`agent "codex" ResolveInteractiveSessionID: rpc error: code = DeadlineExceeded`)
	resolver := &fakeInteractiveSessionResolver{
		legacyResolveErr: legacyErr,
		sessionID:        "codex-fresh-1",
	}
	srv, store, tmuxer := newWakeLegacyBackfillFixture(t, resolver, nil)

	resp, err := srv.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	}))
	if err != nil {
		t.Fatalf("wake failed on a legacy backfill error: %v — the scan is best-effort", err)
	}
	if !resolver.legacyCtxSeen {
		t.Fatal("legacy backfill never ran — the assertion would be vacuous")
	}
	if resp.Msg.Outcome != pb.WakeChatResponse_OUTCOME_FRESH_FALLBACK {
		t.Fatalf("outcome = %v, want OUTCOME_FRESH_FALLBACK", resp.Msg.Outcome)
	}
	if got := tmuxer.killedSessions(); len(got) != 0 {
		t.Fatalf("KillSession targets = %v, want none — a best-effort backfill failure must not tear down the pane", got)
	}
	if store.updateNameCall != 1 {
		t.Fatalf("tmux_session_name write calls = %d, want 1 — the wake must run to completion", store.updateNameCall)
	}
}

// The backfill context must survive the wake request's cancellation, exactly as
// it does on the record path (BOS-844).
func TestWakeChat_LegacyBackfillDoesNotInheritRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resolver := &fakeInteractiveSessionResolver{
		legacySessionID: "codex-legacy-1",
		// Cancelling the request mid-backfill is what makes this assertion
		// non-vacuous: an inherited context would read Canceled here.
		cancelOnLegacyCall: cancel,
	}
	srv, store, _ := newWakeLegacyBackfillFixture(t, resolver, map[string]bool{"codex-legacy-1": true})

	if _, err := srv.WakeChat(ctx, connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !resolver.legacyCtxSeen {
		t.Fatal("legacy backfill never ran — the cancellation assertion would be vacuous")
	}
	if resolver.legacyCtxErr != nil {
		t.Fatalf("backfill context error = %v, want nil — it must not inherit the request's cancellation", resolver.legacyCtxErr)
	}
	if store.updateProviderCall != 1 {
		t.Fatalf("UpdateProviderSessionID calls = %d, want 1", store.updateProviderCall)
	}
}

// The wake backfill is bounded by the same package-level budget the record path
// uses, not by the caller's deadline.
func TestWakeChat_LegacyBackfillUsesItsOwnBudget(t *testing.T) {
	old := providerSessionIDLegacyBackfillTimeout
	providerSessionIDLegacyBackfillTimeout = time.Nanosecond
	defer func() { providerSessionIDLegacyBackfillTimeout = old }()

	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-1"}
	srv, _, _ := newWakeLegacyBackfillFixture(t, resolver, map[string]bool{"codex-legacy-1": true})

	if _, err := srv.WakeChat(context.Background(), connect.NewRequest(&pb.WakeChatRequest{
		AgentSessionId: "agent-1",
	})); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !resolver.legacyCtxSeen {
		t.Fatal("legacy backfill never ran")
	}
	if !errors.Is(resolver.legacyCtxErr, context.DeadlineExceeded) {
		t.Fatalf("backfill context error = %v, want DeadlineExceeded from the shrunk budget", resolver.legacyCtxErr)
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

// TestWakeChat_ChatRowWriteFailureKillsFreshPane is the wake-path half of the
// BOS-845 postcondition: a pane this wake created must not outlive a failure to
// record its name on the chat row, because every cleanup path keys off that name.
func TestWakeChat_ChatRowWriteFailureKillsFreshPane(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	s := newWakeTestServer(t, chat, sess, tmuxer)
	s.agentChats = &chatStoreFake{chat: chat, updateNameErr: errors.New("boom")}

	_, _, _, err := s.WakeChatInternal(context.Background(), "agent-1", false)
	if err == nil {
		t.Fatalf("expected the tmux name write failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "persist tmux name") {
		t.Fatalf("error = %v, want it to wrap %q", err, "persist tmux name")
	}
	wantName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if got := tmuxer.killedSessions(); len(got) != 1 || got[0] != wantName {
		t.Fatalf("KillSession targets = %v, want exactly [%q]", got, wantName)
	}
	if tmuxer.hasSession {
		t.Fatalf("tmux session still live after a failed row write — the pane leaked")
	}
}

// TestWakeChat_AlreadyLivePaneNotKilledOnWriteFailure is the wake-path twin of
// the anti-regression guard: waking a chat whose pane is already live must not
// destroy that pane just because the row write failed.
func TestWakeChat_AlreadyLivePaneNotKilledOnWriteFailure(t *testing.T) {
	chat := &models.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir()}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newWakeTestServer(t, chat, sess, tmuxer)
	s.agentChats = &chatStoreFake{chat: chat, updateNameErr: errors.New("boom")}

	_, _, _, err := s.WakeChatInternal(context.Background(), "agent-1", false)
	if err == nil {
		t.Fatalf("expected the tmux name write failure to propagate, got nil")
	}
	if got := tmuxer.killedSessions(); len(got) != 0 {
		t.Fatalf("KillSession targets = %v, want none for an already-live pane", got)
	}
	if !tmuxer.hasSession {
		t.Fatalf("already-live tmux session was killed by a failed row write")
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
