package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

// fakeTmuxClient lets us assert which Command was passed without exec'ing tmux.
// Shared between spawn_chat_tmux_test.go and wake_chat_test.go. The mutex
// makes captured / createdN / hasSession reads/writes race-safe so the
// singleflight test in wake_chat_test.go can hit it from many goroutines.
type fakeTmuxClient struct {
	mu         sync.Mutex
	available  bool
	hasSession bool
	captured   []string
	createErr  error
	createdN   int
	// slowCreate, when true, sleeps briefly inside NewSessionWithCmd so
	// concurrent goroutines actually contend on singleflight.Do.
	slowCreate bool
	lastName   string
}

func (f *fakeTmuxClient) Available(_ context.Context) bool { return f.available }
func (f *fakeTmuxClient) HasSession(_ context.Context, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasSession
}
func (f *fakeTmuxClient) NewSessionWithCmd(_ context.Context, name, _ string, cmd []string) error {
	if f.slowCreate {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append([]string{}, cmd...)
	f.lastName = name
	if f.createErr != nil {
		return f.createErr
	}
	f.createdN++
	f.hasSession = true
	return nil
}

// fakeTranscriptOracle controls TranscriptExists for tests.
type fakeTranscriptOracle struct {
	exists    bool
	existsFor map[string]bool
	calls     []transcriptCall
}

type transcriptCall struct {
	agentName      string
	workDir        string
	agentSessionID string
}

func (f *fakeTranscriptOracle) TranscriptExists(_ context.Context, agentName, workDir, agentSessionID string) bool {
	f.calls = append(f.calls, transcriptCall{agentName: agentName, workDir: workDir, agentSessionID: agentSessionID})
	if f.existsFor != nil {
		return f.existsFor[agentSessionID]
	}
	return f.exists
}

type fakeInteractiveSessionResolver struct {
	sessionID         string
	legacySessionID   string
	ambiguous         bool
	reason            string
	legacyAmbiguous   bool
	legacyReason      string
	cancelOnFreshCall func()
	calls             []resolverCall
}

type resolverCall struct {
	agentName           string
	workDir             string
	requestedSessionID  string
	launchedAfter       time.Time
	chatCreatedAt       time.Time
	allowLegacyBackfill bool
}

func (f *fakeInteractiveSessionResolver) ResolveInteractiveSessionID(_ context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool) (interactiveSessionResolution, error) {
	f.calls = append(f.calls, resolverCall{
		agentName:           agentName,
		workDir:             workDir,
		requestedSessionID:  requestedSessionID,
		launchedAfter:       launchedAfter,
		chatCreatedAt:       chatCreatedAt,
		allowLegacyBackfill: allowLegacyBackfill,
	})
	sessionID := f.sessionID
	if allowLegacyBackfill && f.legacySessionID != "" {
		sessionID = f.legacySessionID
	}
	ambiguous := f.ambiguous
	reason := f.reason
	if allowLegacyBackfill && f.legacyAmbiguous {
		ambiguous = true
		reason = f.legacyReason
	}
	if !allowLegacyBackfill && f.cancelOnFreshCall != nil {
		f.cancelOnFreshCall()
	}
	return interactiveSessionResolution{
		SessionID: sessionID,
		Ambiguous: ambiguous,
		Reason:    reason,
	}, nil
}

// fakeArgvBuilder is a programmable argvBuilder. fresh/resume hold per-agent
// argv prefixes; BuildInteractive picks one based on the resume flag and
// appends agentSessionID, mirroring the shape both real plugins produce.
// calls records every invocation so tests can assert *which* agent the
// builder was asked to resolve — that's how we pin the bug fix.
type fakeArgvBuilder struct {
	mu     sync.Mutex
	fresh  map[string][]string
	resume map[string][]string
	calls  []argvCall
}

// argvCall captures one BuildInteractive invocation for assertions.
type argvCall struct {
	agentName      string
	agentSessionID string
	resume         bool
	logPath        string
}

func (f *fakeArgvBuilder) BuildInteractive(_ context.Context, agentName, agentSessionID string, resume bool, logPath string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argvCall{agentName: agentName, agentSessionID: agentSessionID, resume: resume, logPath: logPath})
	// Mirror liveArgvBuilder's legacy default so tests with chat.AgentName=""
	// (rows that predate the agent_name column) route to claude rather than
	// erroring out. liveArgvBuilder does the same at spawn_chat_tmux.go.
	name := agentName
	if name == "" {
		name = defaultLegacyAgent
	}
	bucket := f.fresh
	if resume {
		bucket = f.resume
	}
	if prefix, ok := bucket[name]; ok {
		out := append([]string{}, prefix...)
		out = append(out, agentSessionID)
		return out, nil
	}
	return nil, fmt.Errorf("fakeArgvBuilder: no argv configured for agent %q (resume=%v)", name, resume)
}

// claudeArgvBuilder is the default fake used by tests that don't care about
// agent-name routing. It mirrors today's hardcoded claude argv shape so
// existing --session-id / --resume assertions continue to pin the resume
// vs. fresh decision logic in spawnChatTmux.
func claudeArgvBuilder() *fakeArgvBuilder {
	return &fakeArgvBuilder{
		fresh:  map[string][]string{"claude": {"claude", "--session-id"}},
		resume: map[string][]string{"claude": {"claude", "--resume"}},
	}
}

func newTestChat(t *testing.T) *models.AgentChat {
	t.Helper()
	return &models.AgentChat{
		ID:             "chat-id",
		SessionID:      "sess-id",
		AgentSessionID: "agent-session-1",
		AgentName:      "claude",
	}
}

func TestSpawnChatTmux_AlreadyLive(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: true},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive", got.Outcome)
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("expected no NewSession call, got %d", tmuxer.createdN)
	}
}

func TestSpawnChatTmux_FreshStart_NoResumeFlag(t *testing.T) {
	wd := t.TempDir()
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	chat := newTestChat(t)
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         chat,
		WorktreePath: wd,
		TmuxName:     "boss-aaa-bbb",
		ForceFresh:   false,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if !contains(tmuxer.captured, "--session-id") || contains(tmuxer.captured, "--resume") {
		t.Fatalf("expected --session-id only, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_ResumeWhenTranscriptExists(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	wd := t.TempDir()
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: true},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: wd,
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeResumed {
		t.Fatalf("got %v, want OutcomeResumed", got.Outcome)
	}
	if !contains(tmuxer.captured, "--resume") || contains(tmuxer.captured, "--session-id") {
		t.Fatalf("expected --resume only, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_ForceFreshOverridesTranscript(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: true},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
		ForceFresh:   true,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if contains(tmuxer.captured, "--resume") {
		t.Fatalf("force_fresh should suppress --resume, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_WorktreeMissing(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_ = os.RemoveAll(missing)
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: missing,
		TmuxName:     "boss-aaa-bbb",
	})
	if err == nil {
		t.Fatalf("expected ErrWorktreeMissing, got nil")
	}
	if err != ErrWorktreeMissing {
		t.Fatalf("got %v, want ErrWorktreeMissing", err)
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("worktree-missing must not spawn tmux, got createdN=%d", tmuxer.createdN)
	}
}

func TestSpawnChatTmux_TmuxUnavailable(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: false}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unavailable tmux must not error, got %v", err)
	}
	if got.Outcome != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive (no-op)", got.Outcome)
	}
}

func TestSpawnChatTmux_UsesProviderSessionIDForResumeAndTmuxNameUsesAgentSessionID(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	transcripts := &fakeTranscriptOracle{exists: true}
	builder := claudeArgvBuilder()
	providerID := "provider-real-1"
	chat := newTestChat(t)
	chat.ProviderSessionID = &providerID

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: transcripts,
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeResumed {
		t.Fatalf("got %v, want OutcomeResumed", got.Outcome)
	}
	if len(transcripts.calls) != 1 || transcripts.calls[0].agentSessionID != providerID {
		t.Fatalf("TranscriptExists must use provider id %q, got calls=%+v", providerID, transcripts.calls)
	}
	if len(builder.calls) != 1 || builder.calls[0].agentSessionID != providerID {
		t.Fatalf("BuildInteractive must use provider id %q, got calls=%+v", providerID, builder.calls)
	}
	if tmuxer.lastName != "boss-agent-session-1" {
		t.Fatalf("tmux name must keep agent session id, got %q", tmuxer.lastName)
	}
}

func TestSpawnChatTmux_FreshFallbackReasonTranscriptMissingForProviderID(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	providerID := "provider-real-1"
	chat := newTestChat(t)
	chat.ProviderSessionID = &providerID

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{existsFor: map[string]bool{providerID: false}},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if got.FallbackReason != WakeFallbackReasonTranscriptMissing {
		t.Fatalf("fallback reason = %q, want %q", got.FallbackReason, WakeFallbackReasonTranscriptMissing)
	}
}

func TestSpawnChatTmux_FreshFallbackReasonProviderIDDiscoveryTimeout(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	got, err := spawnChatTmux(ctx, spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
		Resolver:    &fakeInteractiveSessionResolver{},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.FallbackReason != WakeFallbackReasonProviderIDDiscoveryTimeout {
		t.Fatalf("fallback reason = %q, want %q", got.FallbackReason, WakeFallbackReasonProviderIDDiscoveryTimeout)
	}
}

func TestSpawnChatTmux_ClaudeWithoutProviderSessionIDUsesAgentSessionID(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	transcripts := &fakeTranscriptOracle{exists: true}
	builder := claudeArgvBuilder()
	chat := newTestChat(t)

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: transcripts,
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeResumed {
		t.Fatalf("got %v, want OutcomeResumed", got.Outcome)
	}
	if len(transcripts.calls) != 1 || transcripts.calls[0].agentSessionID != chat.AgentSessionID {
		t.Fatalf("TranscriptExists must use agent session id %q, got calls=%+v", chat.AgentSessionID, transcripts.calls)
	}
	if len(builder.calls) != 1 || builder.calls[0].agentSessionID != chat.AgentSessionID {
		t.Fatalf("BuildInteractive must use agent session id %q, got calls=%+v", chat.AgentSessionID, builder.calls)
	}
}

func TestSpawnChatTmux_FreshLaunchResolverReturnsProviderSessionID(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	resolver := &fakeInteractiveSessionResolver{sessionID: "codex-real-1"}
	chat := newTestChat(t)
	chat.AgentName = "codex"

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv: &fakeArgvBuilder{
			fresh:  map[string][]string{"codex": {"codex"}},
			resume: map[string][]string{"codex": {"codex", "resume"}},
		},
		Resolver: resolver,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if got.ProviderSessionID != "codex-real-1" {
		t.Fatalf("got provider id %q, want codex-real-1", got.ProviderSessionID)
	}
	if got.LaunchedAt.IsZero() {
		t.Fatalf("LaunchedAt must be set")
	}
	if len(resolver.calls) != 1 || resolver.calls[0].requestedSessionID != chat.AgentSessionID {
		t.Fatalf("resolver must be called with agent session id, got calls=%+v", resolver.calls)
	}
}

func TestSpawnChatTmux_ResolverTimeoutDoesNotFailFreshLaunch(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	got, err := spawnChatTmux(ctx, spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
		Resolver:    &fakeInteractiveSessionResolver{},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if got.ProviderSessionID != "" {
		t.Fatalf("expected unresolved provider id, got %q", got.ProviderSessionID)
	}
}

func TestSpawnChatTmux_AmbiguousResolverStopsPollingWithoutProviderSessionID(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	resolver := &fakeInteractiveSessionResolver{
		ambiguous: true,
		reason:    "multiple codex-tui rollouts matched",
	}

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
		Resolver:    resolver,
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if got.ProviderSessionID != "" {
		t.Fatalf("expected no provider id for ambiguous discovery, got %q", got.ProviderSessionID)
	}
	if !got.DiscoveryAmbiguous || got.DiscoveryReason != "multiple codex-tui rollouts matched" {
		t.Fatalf("ambiguous discovery not preserved: %+v", got)
	}
	if len(resolver.calls) != 1 {
		t.Fatalf("ambiguous resolver should stop polling after one call, got %d", len(resolver.calls))
	}
}

// TestSpawnChatTmux_RoutesArgvByAgentName is the regression test for the
// codex/claude bug. A chat persisted with AgentName="codex" must drive a
// `codex …` tmux command, not the historical hardcoded `claude …`.
// Today (pre-fix) spawnChatTmux ignores chat.AgentName and always emits
// claude argv — this test fails until the argvBuilder dep is honoured.
func TestSpawnChatTmux_RoutesArgvByAgentName(t *testing.T) {
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
	chat := &models.AgentChat{
		ID:             "chat-id",
		SessionID:      "sess-id",
		AgentSessionID: "agent-session-1",
		AgentName:      "codex",
	}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got.Outcome)
	}
	if len(builder.calls) != 1 || builder.calls[0].agentName != "codex" {
		t.Fatalf("argv builder must be asked for agent %q exactly once, got calls=%+v", "codex", builder.calls)
	}
	if len(tmuxer.captured) == 0 || tmuxer.captured[0] != "codex" {
		t.Fatalf("tmux command for codex chat must start with %q, got %v", "codex", tmuxer.captured)
	}
}

// TestSpawnChatTmux_LegacyEmptyAgentNameDefaultsToClaude pins the migration
// guarantee: chats persisted before the agent_name column existed surface
// as "" on the model and must continue to launch claude. The argvBuilder
// receives the empty string and the live impl's "" → "claude" fallback
// keeps these legacy rows working without a data migration.
func TestSpawnChatTmux_LegacyEmptyAgentNameDefaultsToClaude(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	chat := &models.AgentChat{
		ID:             "chat-id",
		SessionID:      "sess-id",
		AgentSessionID: "agent-session-1",
		AgentName:      "", // legacy row
	}
	if _, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("expected 1 builder call, got %d", len(builder.calls))
	}
	// spawnChatTmux passes chat.AgentName through verbatim; the live impl
	// applies the "" → "claude" default. Tests cover that default at the
	// liveArgvBuilder level — here we only assert the dep was called and
	// the captured cmd reflects whatever the builder returned.
	if len(tmuxer.captured) == 0 || tmuxer.captured[0] != "claude" {
		t.Fatalf("legacy empty-AgentName chat must spawn claude, got cmd=%v", tmuxer.captured)
	}
}

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}
