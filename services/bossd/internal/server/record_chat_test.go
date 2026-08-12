package server

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

// recordChatStoreFake satisfies db.AgentChatStore for RecordChat's needs.
// GetByAgentSessionID always returns sql.ErrNoRows so the handler takes
// the Create branch (the path under test). Create stashes the params and
// synthesizes an AgentChat row that mirrors what SQLite would persist —
// crucially preserving the AgentName the handler passed through, since
// that's exactly what we're asserting on.
type recordChatStoreFake struct {
	db.AgentChatStore
	mu      sync.Mutex
	created *db.CreateAgentChatParams
}

func (f *recordChatStoreFake) GetByAgentSessionID(_ context.Context, _ string) (*models.AgentChat, error) {
	return nil, sql.ErrNoRows
}

func (f *recordChatStoreFake) Create(_ context.Context, params db.CreateAgentChatParams) (*models.AgentChat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := params
	f.created = &p
	return &models.AgentChat{
		ID:             "chat-1",
		SessionID:      params.SessionID,
		AgentSessionID: params.AgentSessionID,
		AgentName:      params.AgentName,
		Title:          params.Title,
	}, nil
}

func newRecordChatTestServer(sess *models.Session) (*Server, *recordChatStoreFake) {
	chats := &recordChatStoreFake{}
	return &Server{
		agentChats: chats,
		sessions:   &sessionStoreFake{sess: sess},
		// tmux left nil → ensureChatTmuxSession is a no-op, keeping the
		// test focused on the agent-name resolution rules.
	}, chats
}

type recordChatAgentClient struct {
	*listAgentsFakeClient
}

func (c *recordChatAgentClient) BuildInteractiveCommand(context.Context, *pb.BuildInteractiveCommandRequest) (*pb.BuildInteractiveCommandResponse, error) {
	return &pb.BuildInteractiveCommandResponse{Argv: []string{"sh", "-c", "true"}}, nil
}

// TestRecordChat_AgentNameOverridePreferred verifies that an explicit
// agent_name on RecordChatRequest takes precedence over the parent
// session's AgentName. Locks in the per-chat agent picker introduced
// for the codex routing work.
func TestRecordChat_AgentNameOverridePreferred(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "claude"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-1",
		Title:          "hello",
		AgentName:      strPtr("codex"),
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "codex" {
		t.Fatalf("response chat AgentName = %q, want %q", got, "codex")
	}
	if chats.created == nil {
		t.Fatalf("expected Create to be called")
	}
	if chats.created.AgentName != "codex" {
		t.Fatalf("Create params AgentName = %q, want %q (override should win over session)", chats.created.AgentName, "codex")
	}
}

// TestRecordChat_InheritsSessionAgentWhenUnset preserves the existing
// behavior: when the request omits AgentName, the chat row inherits
// the parent session's AgentName.
func TestRecordChat_InheritsSessionAgentWhenUnset(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "claude"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-2",
		Title:          "hello",
		// AgentName intentionally unset.
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "claude" {
		t.Fatalf("response chat AgentName = %q, want %q", got, "claude")
	}
	if chats.created.AgentName != "claude" {
		t.Fatalf("Create params AgentName = %q, want %q (should inherit from session)", chats.created.AgentName, "claude")
	}
}

// TestRecordChat_OverrideWinsEvenWhenSessionDiffers exercises the case
// where both the session and the override are non-empty but distinct.
// The override must win — this is the whole point of the field.
func TestRecordChat_OverrideWinsEvenWhenSessionDiffers(t *testing.T) {
	sess := &models.Session{ID: "s1", AgentName: "codex"}
	srv, chats := newRecordChatTestServer(sess)

	resp, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-3",
		Title:          "hello",
		AgentName:      strPtr("claude"),
	}))
	if err != nil {
		t.Fatalf("RecordChat: %v", err)
	}
	if got := resp.Msg.GetChat().GetAgentName(); got != "claude" {
		t.Fatalf("response chat AgentName = %q, want %q (override should win)", got, "claude")
	}
	if chats.created.AgentName != "claude" {
		t.Fatalf("Create params AgentName = %q, want %q", chats.created.AgentName, "claude")
	}
}

func TestRecordChat_TmuxSpawnFailureReturnsInternal(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	srv, _ := newRecordChatTestServer(sess)
	srv.agentClients = map[string]agent.AgentRunnerClient{
		"claude": &recordChatAgentClient{listAgentsFakeClient: &listAgentsFakeClient{}},
	}
	srv.tmux = tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if len(args) > 0 && args[0] == "-V" {
			return exec.CommandContext(ctx, "true")
		}
		return exec.CommandContext(ctx, "false")
	}))

	_, err := srv.RecordChat(context.Background(), connect.NewRequest(&pb.RecordChatRequest{
		SessionId:      "s1",
		AgentSessionId: "agent-4",
		Title:          "hello",
	}))
	if err == nil {
		t.Fatalf("expected tmux spawn error, got nil")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("expected CodeInternal, got %v", got)
	}
}

// newEnsureRollbackTestServer wires a *Server for the ensureChatTmuxSession
// pane-rollback tests (BOS-845). The observable tmux double is installed through
// the wakeHook seam, which (*Server).chatSpawnDeps now applies on the record path
// too; s.tmux still has to be a real client because ensureChatTmuxSession gates
// on s.tmux.Available before any deps exist, so the factory answers `tmux -V`
// and nothing else. resolver is left off the hook when nil — assigning a typed
// nil would make deps.Resolver non-nil and panic on first use. The argv double
// answers for both agents because the resolver-bearing cases run as codex (only
// non-claude agents reach ResolveInteractiveSessionID).
func newEnsureRollbackTestServer(t *testing.T, chats db.AgentChatStore, sess *models.Session, tmuxer *fakeTmuxClient, resolver *fakeInteractiveSessionResolver) *Server {
	t.Helper()
	srv := &Server{
		agentChats: chats,
		logger:     zerolog.Nop(),
		sessions:   &sessionStoreFake{sess: sess},
		tmux: tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			if len(args) > 0 && args[0] == "-V" {
				return exec.CommandContext(ctx, "true")
			}
			return exec.CommandContext(ctx, "false")
		})),
		wakeHook: wakeHook{
			spawner:     tmuxer,
			transcripts: &fakeTranscriptOracle{exists: false},
			argv: &fakeArgvBuilder{
				fresh:   map[string][]string{"claude": {"claude", "--session-id"}, "codex": {"codex"}},
				resume:  map[string][]string{"claude": {"claude", "--resume"}, "codex": {"codex", "resume"}},
				support: pb.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
			},
		},
	}
	if resolver != nil {
		srv.wakeHook.resolver = resolver
	}
	return srv
}

// TestEnsureChatTmuxSession_ChatRowWriteFailureKillsPane is BOS-845's mandated
// case: the pane is live but the write that would let any cleanup path find it
// failed, so the pane must not survive the call.
func TestEnsureChatTmuxSession_ChatRowWriteFailureKillsPane(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-write-fail", AgentName: "claude"}
	chats := &chatStoreFake{chat: chat, updateNameErr: errors.New("boom")}
	tmuxer := &fakeTmuxClient{available: true}
	srv := newEnsureRollbackTestServer(t, chats, sess, tmuxer, nil)

	err := srv.ensureChatTmuxSession(context.Background(), chat, false)
	if err == nil {
		t.Fatalf("expected the tmux_session_name write failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "persist tmux session name") {
		t.Fatalf("error = %v, want it to wrap %q", err, "persist tmux session name")
	}
	wantName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if got := tmuxer.killedSessions(); len(got) != 1 || got[0] != wantName {
		t.Fatalf("KillSession targets = %v, want exactly [%q]", got, wantName)
	}
	if tmuxer.hasSession {
		t.Fatalf("tmux session still live after a failed row write — the pane leaked")
	}
}

// TestEnsureChatTmuxSession_ResolverErrorKillsPane covers the path that produced
// the reported leak: the pane is created, then the interactive-session resolver
// fails (in production, a DeadlineExceeded from the codex runner).
func TestEnsureChatTmuxSession_ResolverErrorKillsPane(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-resolver-fail", AgentName: "codex"}
	chats := &chatStoreFake{chat: chat}
	tmuxer := &fakeTmuxClient{available: true}
	resolveErr := errors.New(`agent "codex" ResolveInteractiveSessionID: rpc error: code = DeadlineExceeded`)
	srv := newEnsureRollbackTestServer(t, chats, sess, tmuxer, &fakeInteractiveSessionResolver{resolveErr: resolveErr})

	err := srv.ensureChatTmuxSession(context.Background(), chat, false)
	if !errors.Is(err, resolveErr) {
		t.Fatalf("error = %v, want the resolver error to propagate unchanged", err)
	}
	wantName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if got := tmuxer.killedSessions(); len(got) != 1 || got[0] != wantName {
		t.Fatalf("KillSession targets = %v, want exactly [%q]", got, wantName)
	}
	if tmuxer.hasSession {
		t.Fatalf("tmux session still live after a resolver failure — the pane leaked")
	}
	if chats.updateNameCall != 0 {
		t.Fatalf("tmux_session_name write calls = %d, want 0 — the spawn never succeeded", chats.updateNameCall)
	}
}

// TestEnsureChatTmuxSession_ProviderSessionIDWriteFailureKillsPane covers the
// row write a "just fix the tmux_session_name write" patch would miss: it runs
// before the name write, so its failure strands the pane just as thoroughly.
func TestEnsureChatTmuxSession_ProviderSessionIDWriteFailureKillsPane(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-provider-fail", AgentName: "codex"}
	chats := &chatStoreFake{chat: chat, updateProviderErr: errors.New("boom")}
	tmuxer := &fakeTmuxClient{available: true}
	srv := newEnsureRollbackTestServer(t, chats, sess, tmuxer, &fakeInteractiveSessionResolver{sessionID: "rollout-1"})

	err := srv.ensureChatTmuxSession(context.Background(), chat, false)
	if err == nil {
		t.Fatalf("expected the provider_session_id write failure to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "persist provider session id") {
		t.Fatalf("error = %v, want it to wrap %q", err, "persist provider session id")
	}
	wantName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if got := tmuxer.killedSessions(); len(got) != 1 || got[0] != wantName {
		t.Fatalf("KillSession targets = %v, want exactly [%q]", got, wantName)
	}
	if tmuxer.hasSession {
		t.Fatalf("tmux session still live after a failed provider id write — the pane leaked")
	}
}

// TestEnsureChatTmuxSession_AlreadyLivePaneNotKilledOnWriteFailure is the
// anti-regression guard for the dominant risk: a pane this call did not create
// belongs to an earlier successful claim, so a transient row-write failure must
// never tear it down.
func TestEnsureChatTmuxSession_AlreadyLivePaneNotKilledOnWriteFailure(t *testing.T) {
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	chat := &models.AgentChat{ID: "c1", SessionID: sess.ID, AgentSessionID: "agent-already-live", AgentName: "claude"}
	chats := &chatStoreFake{chat: chat, updateNameErr: errors.New("boom")}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	srv := newEnsureRollbackTestServer(t, chats, sess, tmuxer, nil)

	err := srv.ensureChatTmuxSession(context.Background(), chat, false)
	if err == nil {
		t.Fatalf("expected the tmux_session_name write failure to propagate, got nil")
	}
	if got := tmuxer.killedSessions(); len(got) != 0 {
		t.Fatalf("KillSession targets = %v, want none for an already-live pane", got)
	}
	if !tmuxer.hasSession {
		t.Fatalf("already-live tmux session was killed by a failed row write")
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("spawns = %d, want 0 for an already-live pane", tmuxer.createdN)
	}
}

func TestEnsureChatTmuxSession_DoesNotBackfillDefaultAccountWhenAlreadyLive(t *testing.T) {
	srv, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, srv, "work", []byte("blob"))
	materializer := &fakeMaterializer{supports: true, env: map[string]string{"ANTHROPIC_API_KEY": "sk-default"}}
	srv.resolver = account.NewResolver(
		accountwiring.NewRegistry(accts),
		materializer,
		zerolog.Nop(),
	)
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	sessionStore := &lifecycleSessionStoreFake{session: sess}
	chat := &models.AgentChat{SessionID: sess.ID, AgentSessionID: "agent-live", AgentName: "claude"}
	srv.sessions = sessionStore
	srv.agentChats = &chatStoreFake{chat: chat}
	var tmuxCalls []string
	srv.tmux = tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		tmuxCalls = append(tmuxCalls, strings.Join(args, " "))
		switch {
		case len(args) > 0 && args[0] == "-V":
			return exec.CommandContext(ctx, "true")
		case len(args) > 0 && args[0] == "has-session":
			return exec.CommandContext(ctx, "true")
		default:
			return exec.CommandContext(ctx, "false")
		}
	}))

	if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if sessionStore.updateCalls != 0 {
		t.Fatalf("session Update calls = %d, want 0 for already-live chat", sessionStore.updateCalls)
	}
	if materializer.calls != 0 {
		t.Fatalf("materializer calls = %d, want 0 for already-live chat", materializer.calls)
	}
	if sess.AccountID != nil {
		t.Fatalf("session AccountID = %q, want nil legacy binding unchanged", *sess.AccountID)
	}
	for _, call := range tmuxCalls {
		if strings.HasPrefix(call, "new-session ") {
			t.Fatalf("tmux calls = %v, want no new-session for already-live chat", tmuxCalls)
		}
	}
}

// newResumeBackfillFixture wires the attach-path legacy-backfill cases: a codex
// chat with no provider session id yet, and an already-live pane so
// spawnChatTmux returns OutcomeAlreadyLive without ever reaching the fd-path
// resolver — only the backfill under test calls it. The resolver is a test
// double, so nothing here scans a real codex sessions root.
func newResumeBackfillFixture(t *testing.T, resolver *fakeInteractiveSessionResolver) (*Server, *models.AgentChat, *chatStoreFake, *fakeTmuxClient) {
	t.Helper()
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "codex"}
	chat := &models.AgentChat{
		ID:             "c1",
		SessionID:      sess.ID,
		AgentSessionID: "agent-resume-backfill",
		AgentName:      "codex",
		CreatedAt:      time.Now().Add(-time.Minute),
	}
	chats := &chatStoreFake{chat: chat}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	return newEnsureRollbackTestServer(t, chats, sess, tmuxer, resolver), chat, chats, tmuxer
}

// legacyCalls counts the backfill arm of the resolver (allowLegacyBackfill), as
// opposed to the fd-path resolution spawnChatTmux performs.
func legacyCalls(resolver *fakeInteractiveSessionResolver) int {
	n := 0
	for _, call := range resolver.calls {
		if call.allowLegacyBackfill {
			n++
		}
	}
	return n
}

// The reported failure: the attach-path legacy backfill timed out and its error
// took the whole chat attach down with it. It must now warn and continue.
func TestEnsureChatTmuxSession_ResumeBackfillErrorDoesNotFailAttach(t *testing.T) {
	resolver := &fakeInteractiveSessionResolver{legacyResolveErr: context.DeadlineExceeded}
	srv, chat, chats, tmuxer := newResumeBackfillFixture(t, resolver)

	if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession = %v, want nil — a failed backfill must not fail the attach", err)
	}
	if got := legacyCalls(resolver); got != 1 {
		t.Fatalf("legacy backfill resolutions = %d, want 1", got)
	}
	if got := tmuxer.killedSessions(); len(got) != 0 {
		t.Fatalf("KillSession targets = %v, want none — the already-live pane must survive", got)
	}
	if chat.ProviderSessionID != nil {
		t.Fatalf("provider session id = %q, want nil after a failed backfill", *chat.ProviderSessionID)
	}
	if chats.updateProviderCall != 0 {
		t.Fatalf("UpdateProviderSessionID calls = %d, want 0", chats.updateProviderCall)
	}
	if chats.updateNameCall != 1 {
		t.Fatalf("tmux_session_name write calls = %d, want 1 — the attach must still complete", chats.updateNameCall)
	}
}

// Warn-and-continue must not cost the happy path: a backfill that resolves still
// persists the id it found.
func TestEnsureChatTmuxSession_ResumeBackfillPersistsProviderSessionID(t *testing.T) {
	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-1"}
	srv, chat, chats, _ := newResumeBackfillFixture(t, resolver)

	if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if chat.ProviderSessionID == nil || *chat.ProviderSessionID != "codex-legacy-1" {
		t.Fatalf("provider session id = %v, want codex-legacy-1", chat.ProviderSessionID)
	}
	if chats.updateProviderCall != 1 {
		t.Fatalf("UpdateProviderSessionID calls = %d, want 1", chats.updateProviderCall)
	}
}

// The backfill runs on a detached context, so cancelling the caller's request
// context mid-resolution must leave the backfill's own context live.
func TestEnsureChatTmuxSession_ResumeBackfillSurvivesRequestCancellation(t *testing.T) {
	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-2"}
	srv, chat, chats, _ := newResumeBackfillFixture(t, resolver)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel from inside the resolution rather than before the call: an
	// already-dead ctx would short-circuit s.tmux.Available and never reach the
	// backfill at all, making the assertion vacuous.
	resolver.cancelOnLegacyCall = cancel

	if err := srv.ensureChatTmuxSession(ctx, chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if !resolver.legacyCtxSeen {
		t.Fatal("legacy backfill never ran — the cancellation assertion would be vacuous")
	}
	if resolver.legacyCtxErr != nil {
		t.Fatalf("backfill context error = %v, want nil — it must not inherit the request's cancellation", resolver.legacyCtxErr)
	}
	if chats.updateProviderCall != 1 {
		t.Fatalf("UpdateProviderSessionID calls = %d, want 1", chats.updateProviderCall)
	}
}

// The backfill context is bounded by its own package-level budget, not by the
// caller's deadline and not by the 2s foreground fd budget.
func TestEnsureChatTmuxSession_ResumeBackfillUsesItsOwnBudget(t *testing.T) {
	old := providerSessionIDLegacyBackfillTimeout
	providerSessionIDLegacyBackfillTimeout = time.Nanosecond
	defer func() { providerSessionIDLegacyBackfillTimeout = old }()

	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-3"}
	srv, chat, _, _ := newResumeBackfillFixture(t, resolver)

	if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if !resolver.legacyCtxSeen {
		t.Fatal("legacy backfill never ran")
	}
	if !errors.Is(resolver.legacyCtxErr, context.DeadlineExceeded) {
		t.Fatalf("backfill context error = %v, want DeadlineExceeded from the shrunk budget", resolver.legacyCtxErr)
	}
}

// A scan that finds an id at the very end of its budget must still persist it.
// The scan context is spent by construction here (the budget is shrunk to 1ns
// and the resolver still answers), so the persist can only succeed if it runs on
// its own detached context — reverting that split makes updateProviderCtxErr
// read DeadlineExceeded and the write get dropped, which the warn-and-continue
// branch above would otherwise swallow in silence.
func TestEnsureChatTmuxSession_ResumeBackfillPersistsWhenScanConsumesItsBudget(t *testing.T) {
	old := providerSessionIDLegacyBackfillTimeout
	providerSessionIDLegacyBackfillTimeout = time.Nanosecond
	defer func() { providerSessionIDLegacyBackfillTimeout = old }()

	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-late"}
	srv, chat, chats, _ := newResumeBackfillFixture(t, resolver)

	if err := srv.ensureChatTmuxSession(context.Background(), chat, true); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if !errors.Is(resolver.legacyCtxErr, context.DeadlineExceeded) {
		t.Fatalf("scan context error = %v, want DeadlineExceeded (the budget must be spent for this test to mean anything)", resolver.legacyCtxErr)
	}
	if chats.updateProviderCall != 1 {
		t.Fatalf("UpdateProviderSessionID calls = %d, want 1 (a late-but-successful scan must still persist)", chats.updateProviderCall)
	}
	if chats.updateProviderCtxErr != nil {
		t.Fatalf("persist context error = %v, want nil (the write must not inherit the spent scan budget)", chats.updateProviderCtxErr)
	}
	if chats.updateProvider == nil || *chats.updateProvider != "codex-legacy-late" {
		t.Fatalf("persisted provider id = %v, want codex-legacy-late", chats.updateProvider)
	}
}

// The legacy backfill belongs to the resume path only; a fresh chat must not pay
// for a whole-corpus scan.
func TestEnsureChatTmuxSession_NonResumeSkipsLegacyBackfill(t *testing.T) {
	resolver := &fakeInteractiveSessionResolver{legacySessionID: "codex-legacy-4"}
	srv, chat, _, _ := newResumeBackfillFixture(t, resolver)

	if err := srv.ensureChatTmuxSession(context.Background(), chat, false); err != nil {
		t.Fatalf("ensureChatTmuxSession: %v", err)
	}
	if got := legacyCalls(resolver); got != 0 {
		t.Fatalf("legacy backfill resolutions = %d, want 0 on the non-resume path", got)
	}
	if chat.ProviderSessionID != nil {
		t.Fatalf("provider session id = %q, want nil", *chat.ProviderSessionID)
	}
}
