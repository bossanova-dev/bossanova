package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/tmux"
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
	slowCreate             bool
	lastName               string
	lastEnv                map[string]string
	sentMessages           []sentMessage
	sendMessageErr         error
	beforeSubmitSideEffect func()
	// panePID / panePIDByName / panePIDErr model tmux list-panes for the
	// codex provider-session fd resolver (BOS-290).
	panePID       int
	panePIDByName map[string]int
	panePIDErr    error
	// killed records every KillSession target in order, so a test can assert
	// that a spawn which failed after pane creation rolled the pane back
	// (BOS-845). killErr makes the rollback kill itself fail, exercising the
	// best-effort contract.
	killed  []string
	killErr error
}

type sentMessage struct {
	sessionName string
	text        string
	submit      bool
	readyMarker string
	// There is deliberately no working-probe field here. BOS-599's first pass
	// threaded one through this seam so the verifier could ask the agent whether
	// it was mid-turn; real pane captures showed that signal both absent from the
	// panes it would classify and pointed the wrong way, so delivery no longer
	// depends on it and the parameter is gone. The queued verdict is now read
	// from the pane, and is asserted where the panes are:
	// send_chat_message_queued_test.go at this layer, and the tmux package's
	// tmux_submit_verify_queued_test.go against the real captures.

	// modal is the per-agent modal detector the caller routed with this send. It
	// is recorded rather than called so a test can assert the send path resolved
	// a detector at all — nil here means the readiness gate would have run with
	// the modal check disabled (BOS-600).
	modal tmux.ModalDetector
	// beforeSubmitPresent records whether this delivery carried a turn-start
	// baseline hook.
	beforeSubmitPresent bool
}

func (f *fakeTmuxClient) Available(_ context.Context) bool { return f.available }
func (f *fakeTmuxClient) HasSession(_ context.Context, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasSession
}
func (f *fakeTmuxClient) NewSessionWithCmd(_ context.Context, name, _ string, cmd []string, env map[string]string) error {
	if f.slowCreate {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append([]string{}, cmd...)
	f.lastName = name
	f.lastEnv = env
	if f.createErr != nil {
		return f.createErr
	}
	f.createdN++
	f.hasSession = true
	return nil
}
func (f *fakeTmuxClient) SendMessage(_ context.Context, sessionName, text string, submit bool, readyMarker string, modal tmux.ModalDetector, beforeSubmit func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendMessageErr != nil {
		return f.sendMessageErr
	}
	if f.beforeSubmitSideEffect != nil {
		f.beforeSubmitSideEffect()
	}
	if beforeSubmit != nil {
		beforeSubmit()
	}
	f.sentMessages = append(f.sentMessages, sentMessage{
		sessionName:         sessionName,
		text:                text,
		submit:              submit,
		readyMarker:         readyMarker,
		modal:               modal,
		beforeSubmitPresent: beforeSubmit != nil,
	})
	return nil
}

// PanePID returns a configured per-session pane pid (panePIDByName), else the
// default panePID, so tests can model distinct sibling panes. panePIDErr, when
// set, simulates a tmux failure (caller falls back to pane pid 0).
func (f *fakeTmuxClient) PanePID(_ context.Context, sessionName string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panePIDErr != nil {
		return 0, f.panePIDErr
	}
	if pid, ok := f.panePIDByName[sessionName]; ok {
		return pid, nil
	}
	return f.panePID, nil
}

// KillSession records the target and marks the fake's single pane dead, so a
// rollback is observable both as a call and as a subsequent HasSession false.
func (f *fakeTmuxClient) KillSession(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, name)
	if f.killErr != nil {
		return f.killErr
	}
	f.hasSession = false
	return nil
}

// killedSessions returns a race-safe copy of the recorded kill targets.
func (f *fakeTmuxClient) killedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.killed...)
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
	// resolveErr fails every resolution, modeling the production
	// DeadlineExceeded that left a pane live but unnamed (BOS-845).
	resolveErr error
	calls      []resolverCall
	// byPanePID maps a pane pid to the session id fd resolution returns for it,
	// modeling each sibling chat's process holding its OWN rollout open
	// (BOS-290). When a call's panePID matches, it wins over sessionID.
	byPanePID map[int]string
	// legacyResolveErr fails only the legacy-backfill arm, so a test can drive
	// the attach-path backfill into DeadlineExceeded while leaving the fd arm
	// (and the pane-rollback cases that depend on it) untouched (BOS-844).
	legacyResolveErr error
	// cancelOnLegacyCall runs at the top of a legacy-backfill resolution and
	// legacyCtxErr records that call's ctx.Err() immediately afterwards. Together
	// they prove the backfill context survives the caller's cancellation.
	cancelOnLegacyCall func()
	legacyCtxErr       error
	legacyCtxSeen      bool
}

type resolverCall struct {
	agentName           string
	workDir             string
	requestedSessionID  string
	launchedAfter       time.Time
	chatCreatedAt       time.Time
	allowLegacyBackfill bool
	panePID             int
}

func (f *fakeInteractiveSessionResolver) ResolveInteractiveSessionID(ctx context.Context, agentName, workDir, requestedSessionID string, launchedAfter, chatCreatedAt time.Time, allowLegacyBackfill bool, panePID int) (interactiveSessionResolution, error) {
	f.calls = append(f.calls, resolverCall{
		agentName:           agentName,
		workDir:             workDir,
		requestedSessionID:  requestedSessionID,
		launchedAfter:       launchedAfter,
		chatCreatedAt:       chatCreatedAt,
		allowLegacyBackfill: allowLegacyBackfill,
		panePID:             panePID,
	})
	if allowLegacyBackfill {
		// Sample the backfill's own context: cancelling the caller's request
		// context here must leave this one live (BOS-844). CancelFunc propagates
		// synchronously, so the reading below is deterministic.
		if f.cancelOnLegacyCall != nil {
			f.cancelOnLegacyCall()
		}
		f.legacyCtxErr = ctx.Err()
		f.legacyCtxSeen = true
		if f.legacyResolveErr != nil {
			return interactiveSessionResolution{}, f.legacyResolveErr
		}
	}
	if f.resolveErr != nil {
		return interactiveSessionResolution{}, f.resolveErr
	}
	if !allowLegacyBackfill && panePID > 0 {
		if id, ok := f.byPanePID[panePID]; ok {
			if f.cancelOnFreshCall != nil {
				f.cancelOnFreshCall()
			}
			return interactiveSessionResolution{SessionID: id}, nil
		}
	}
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
// support is the append_system_prompt declaration every built response
// carries; the zero value is UNSPECIFIED, which is exactly what an old plugin
// binary sends.
type fakeArgvBuilder struct {
	mu      sync.Mutex
	fresh   map[string][]string
	resume  map[string][]string
	calls   []argvCall
	support bossanovav1.AppendSystemPromptSupport
}

// argvCall captures one BuildInteractive invocation for assertions.
type argvCall struct {
	agentName          string
	agentSessionID     string
	resume             bool
	worktreePath       string
	logPath            string
	appendSystemPrompt string
	model              string
	effort             string
	configHomeEnv      map[string]string
}

func (f *fakeArgvBuilder) BuildInteractive(_ context.Context, agentName, agentSessionID string, resume bool, worktreePath, logPath, appendSystemPrompt, model, effort string, configHomeEnv map[string]string) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argvCall{agentName: agentName, agentSessionID: agentSessionID, resume: resume, worktreePath: worktreePath, logPath: logPath, appendSystemPrompt: appendSystemPrompt, model: model, effort: effort, configHomeEnv: configHomeEnv})
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
		return &bossanovav1.BuildInteractiveCommandResponse{
			Argv:                      out,
			AppendSystemPromptSupport: f.support,
		}, nil
	}
	return nil, fmt.Errorf("fakeArgvBuilder: no argv configured for agent %q (resume=%v)", name, resume)
}

func TestSpawnChatTmux_ForwardsConfigHomeEnvBeforeArgvBuild(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
		SessionEnvFunc: func() (map[string]string, error) {
			return map[string]string{"CODEX_HOME": "/selected/codex", "HOME": "/selected/home", "API_KEY": "secret"}, nil
		},
	})
	if err != nil {
		t.Fatalf("spawnChatTmux: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	if got := builder.calls[0].configHomeEnv; !reflect.DeepEqual(got, map[string]string{"CODEX_HOME": "/selected/codex", "HOME": "/selected/home"}) {
		t.Fatalf("ConfigHomeEnv = %#v, want selected homes only", got)
	}
}

func TestSpawnChatTmux_ResolvesRelativeConfigHomesBeforeArgvBuild(t *testing.T) {
	worktree := t.TempDir()
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: worktree,
		TmuxName:     "boss-agent-session-1",
		SessionEnvFunc: func() (map[string]string, error) {
			return map[string]string{"CODEX_HOME": ".codex-account", "HOME": ".home-account"}, nil
		},
	})
	if err != nil {
		t.Fatalf("spawnChatTmux: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	want := map[string]string{
		"CODEX_HOME": filepath.Join(worktree, ".codex-account"),
		"HOME":       filepath.Join(worktree, ".home-account"),
	}
	if got := builder.calls[0].configHomeEnv; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfigHomeEnv = %#v, want worktree-resolved homes %#v", got, want)
	}
}

// claudeArgvBuilder is the default fake used by tests that don't care about
// agent-name routing. It mirrors today's hardcoded claude argv shape so
// existing --session-id / --resume assertions continue to pin the resume
// vs. fresh decision logic in spawnChatTmux.
func claudeArgvBuilder() *fakeArgvBuilder {
	return &fakeArgvBuilder{
		fresh:   map[string][]string{"claude": {"claude", "--session-id"}},
		resume:  map[string][]string{"claude": {"claude", "--resume"}},
		support: bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
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

// TestSpawnChatTmux_ThreadsModel pins the re-spawn/wake path (RecordChat,
// WakeChat): a session's model must reach BuildInteractive so a woken or
// re-ensured pane launches on the same model as the initial StartTmuxChat.
func TestSpawnChatTmux_ThreadsModel(t *testing.T) {
	wd := t.TempDir()
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	builder := claudeArgvBuilder()
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: wd,
		TmuxName:     "boss-aaa-bbb",
		Model:        "sonnet",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(builder.calls) != 1 {
		t.Fatalf("BuildInteractive calls = %d, want 1", len(builder.calls))
	}
	if got := builder.calls[0].model; got != "sonnet" {
		t.Fatalf("BuildInteractive model = %q, want sonnet", got)
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

	wd := t.TempDir()
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: transcripts,
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: wd,
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
	if builder.calls[0].worktreePath != wd {
		t.Fatalf("BuildInteractive worktree path = %q, want %q", builder.calls[0].worktreePath, wd)
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

// A resolver ERROR must degrade exactly like a resolver TIMEOUT: foreground
// provider-id discovery is best-effort, covered by background discovery and by
// attach-time backfill. Before this, an error was fatal to the spawn — so a
// transient plugin RPC failure destroyed a perfectly good pane and failed chat
// creation outright, which is the shape the reported DeadlineExceeded took.
func TestSpawnChatTmux_ResolverErrorDegradesToFreshFallback(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	resolveErr := errors.New(`agent "codex" ResolveInteractiveSessionID: rpc error: code = DeadlineExceeded`)

	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        claudeArgvBuilder(),
		Resolver:    &fakeInteractiveSessionResolver{resolveErr: resolveErr},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
	})
	if err != nil {
		t.Fatalf("resolver error propagated: %v — foreground discovery is best-effort", err)
	}
	if got.Outcome != OutcomeFreshFallback {
		t.Fatalf("outcome = %v, want OutcomeFreshFallback", got.Outcome)
	}
	if got.FallbackReason != WakeFallbackReasonProviderIDDiscoveryTimeout {
		t.Fatalf("fallback reason = %q, want %q", got.FallbackReason, WakeFallbackReasonProviderIDDiscoveryTimeout)
	}
	// Not ambiguous: the caller arms background discovery only for a
	// non-ambiguous miss, and a failed resolution is a miss, not a collision.
	if got.DiscoveryAmbiguous {
		t.Fatal("DiscoveryAmbiguous = true, want false — background discovery must stay armed after a resolver error")
	}
	if got.ProviderSessionID != "" {
		t.Fatalf("provider session id = %q, want empty", got.ProviderSessionID)
	}
	if got.LaunchedAt.IsZero() {
		t.Fatal("LaunchedAt is zero — the caller keys background discovery off it")
	}
	if killed := tmuxer.killedSessions(); len(killed) != 0 {
		t.Fatalf("KillSession targets = %v, want none — the pane is usable and must survive", killed)
	}
	if !tmuxer.hasSession {
		t.Fatal("tmux session was torn down after a best-effort discovery failure")
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

// TestSpawnChatTmux_PassesAppendSystemPrompt pins that the record/wake spawn
// path forwards the boss session-context suffix to BuildInteractive. Without
// this, continued/woken chats would launch without the boss identifiers that
// StartTmuxChat injects, so "every chat" would silently exclude these paths.
func TestSpawnChatTmux_PassesAppendSystemPrompt(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	transcripts := &fakeTranscriptOracle{exists: true}
	builder := claudeArgvBuilder()
	chat := newTestChat(t)
	const bossContext = "You are running inside a bossanova-managed chat. ..."

	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: transcripts,
		Argv:        builder,
	}, spawnInput{
		Chat:               chat,
		WorktreePath:       t.TempDir(),
		TmuxName:           "boss-agent-session-1",
		AppendSystemPrompt: bossContext,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(builder.calls) != 1 || builder.calls[0].appendSystemPrompt != bossContext {
		t.Fatalf("BuildInteractive must receive append-system-prompt %q, got calls=%+v", bossContext, builder.calls)
	}
}

// TestSpawnChatTmux_PassesSessionEnv pins that the canonical BOSS_* session
// environment is forwarded to the spawned tmux pane unchanged. This matters for
// cron sessions in particular: the autonomy directive carried via
// AppendSystemPrompt asserts BOSS_CRON=true, so the spawned pane must also
// receive that env — otherwise shell-mode detection takes the interactive path.
func TestSpawnChatTmux_PassesSessionEnv(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	transcripts := &fakeTranscriptOracle{exists: true}
	builder := claudeArgvBuilder()
	chat := newTestChat(t)
	cronEnv := map[string]string{"BOSS_CRON": "true", "BOSS_CRON_JOB_ID": "job-1"}

	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: transcripts,
		Argv:        builder,
	}, spawnInput{
		Chat:         chat,
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
		SessionEnv:   cronEnv,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tmuxer.lastEnv["BOSS_CRON"] != "true" || tmuxer.lastEnv["BOSS_CRON_JOB_ID"] != "job-1" {
		t.Fatalf("tmux spawn must receive session env, got %+v", tmuxer.lastEnv)
	}
}

func TestSpawnChatTmux_SessionEnvFuncDeferredUntilSpawn(t *testing.T) {
	t.Run("already live skips env func", func(t *testing.T) {
		calls := 0
		_, err := spawnChatTmux(context.Background(), spawnDeps{
			Tmux:        &fakeTmuxClient{available: true, hasSession: true},
			Transcripts: &fakeTranscriptOracle{exists: true},
			Argv:        claudeArgvBuilder(),
		}, spawnInput{
			Chat:         newTestChat(t),
			WorktreePath: t.TempDir(),
			TmuxName:     "boss-agent-session-1",
			SessionEnvFunc: func() (map[string]string, error) {
				calls++
				return map[string]string{"ANTHROPIC_API_KEY": "sk-default"}, nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if calls != 0 {
			t.Fatalf("SessionEnvFunc calls = %d, want 0 for already-live chat", calls)
		}
	})

	t.Run("fresh spawn uses env func", func(t *testing.T) {
		tmuxer := &fakeTmuxClient{available: true, hasSession: false}
		calls := 0
		_, err := spawnChatTmux(context.Background(), spawnDeps{
			Tmux:        tmuxer,
			Transcripts: &fakeTranscriptOracle{exists: true},
			Argv:        claudeArgvBuilder(),
		}, spawnInput{
			Chat:         newTestChat(t),
			WorktreePath: t.TempDir(),
			TmuxName:     "boss-agent-session-1",
			SessionEnvFunc: func() (map[string]string, error) {
				calls++
				return map[string]string{"ANTHROPIC_API_KEY": "sk-default"}, nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if calls != 1 {
			t.Fatalf("SessionEnvFunc calls = %d, want 1", calls)
		}
		if tmuxer.lastEnv["ANTHROPIC_API_KEY"] != "sk-default" {
			t.Fatalf("tmux spawn env = %+v, want materialized account env", tmuxer.lastEnv)
		}
	})
}

// TestSpawnChatTmux_NoSessionEnvLeak pins the non-cron case: a nil SessionEnv
// must reach the spawner unchanged (no BOSS_* vars leak into plain sessions).
func TestSpawnChatTmux_NoSessionEnvLeak(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: true},
		Argv:        claudeArgvBuilder(),
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-agent-session-1",
		SessionEnv:   nil,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tmuxer.lastEnv != nil {
		t.Fatalf("plain session must not receive cron env, got %+v", tmuxer.lastEnv)
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

func TestChatAgentConventions(t *testing.T) {
	tests := []struct {
		name          string
		readyMarker   string
		commandPrefix string
	}{
		{name: "claude", readyMarker: "❯", commandPrefix: "/"},
		{name: "codex", readyMarker: "›", commandPrefix: "$"},
		{name: "opencode", readyMarker: "┃", commandPrefix: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatReadyMarker(tt.name); got != tt.readyMarker {
				t.Errorf("chatReadyMarker(%q) = %q, want %q", tt.name, got, tt.readyMarker)
			}
			if got := chatCommandPrefix(tt.name); got != tt.commandPrefix {
				t.Errorf("chatCommandPrefix(%q) = %q, want %q", tt.name, got, tt.commandPrefix)
			}
		})
	}
}

// TestSpawnChatTmux_SiblingCodexChatsResolveDistinctProviderIDs is the BOS-290
// regression: two codex chats in the SAME worktree, launched back-to-back, must
// bind to DISTINCT provider ids. Each chat's own tmux pane has its own pid, and
// process-fd resolution maps each pane pid to the rollout that pane's process
// holds open. The fake resolver models this via byPanePID; the fake tmux client
// reports a distinct pane pid per session name. Before the pane-pid threading,
// both spawns fed the resolver no discriminator and collided on one id.
func TestSpawnChatTmux_SiblingCodexChatsResolveDistinctProviderIDs(t *testing.T) {
	wd := t.TempDir()
	builder := &fakeArgvBuilder{
		fresh:  map[string][]string{"codex": {"codex"}},
		resume: map[string][]string{"codex": {"codex", "resume"}},
	}
	resolver := &fakeInteractiveSessionResolver{
		byPanePID: map[int]string{
			97741: "rollout-aaa",
			99402: "rollout-bbb",
		},
	}

	spawnOne := func(agentSessionID, tmuxName string, panePID int) spawnResult {
		tmuxer := &fakeTmuxClient{
			available:     true,
			hasSession:    false,
			panePIDByName: map[string]int{tmuxName: panePID},
		}
		chat := &models.AgentChat{
			ID:             "chat-" + agentSessionID,
			SessionID:      "sess-id",
			AgentSessionID: agentSessionID,
			AgentName:      "codex",
		}
		got, err := spawnChatTmux(context.Background(), spawnDeps{
			Tmux:        tmuxer,
			Transcripts: &fakeTranscriptOracle{exists: false},
			Argv:        builder,
			Resolver:    resolver,
		}, spawnInput{
			Chat:         chat,
			WorktreePath: wd,
			TmuxName:     tmuxName,
		})
		if err != nil {
			t.Fatalf("spawn %s: %v", agentSessionID, err)
		}
		return got
	}

	a := spawnOne("chatA", "boss-repo-chatA", 97741)
	b := spawnOne("chatB", "boss-repo-chatB", 99402)

	if a.ProviderSessionID != "rollout-aaa" {
		t.Errorf("chat A ProviderSessionID = %q, want rollout-aaa", a.ProviderSessionID)
	}
	if b.ProviderSessionID != "rollout-bbb" {
		t.Errorf("chat B ProviderSessionID = %q, want rollout-bbb", b.ProviderSessionID)
	}
	if a.ProviderSessionID == b.ProviderSessionID {
		t.Fatalf("sibling codex chats collided on provider id %q (BOS-290 regression)", a.ProviderSessionID)
	}
	// The resolver must have received each chat's own pane pid.
	var sawA, sawB bool
	for _, c := range resolver.calls {
		if c.panePID == 97741 {
			sawA = true
		}
		if c.panePID == 99402 {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Errorf("resolver did not receive both pane pids: sawA=%v sawB=%v calls=%+v", sawA, sawB, resolver.calls)
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

// stubResolverAgentClient is a configurable AgentRunnerClient used to drive
// the liveInteractiveSessionResolver / liveTranscriptOracle dispatch paths.
// The embedded interface is nil — only the two RPCs these dispatchers call
// are overridden, so any other method would panic (and never does here).
type stubResolverAgentClient struct {
	agent.AgentRunnerClient
	resolveResp    *bossanovav1.ResolveInteractiveSessionIDResponse
	resolveErr     error
	transcriptResp *bossanovav1.TranscriptExistsResponse
	transcriptErr  error
}

func (s stubResolverAgentClient) ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return s.resolveResp, s.resolveErr
}

func (s stubResolverAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return s.transcriptResp, s.transcriptErr
}

func TestLiveInteractiveSessionResolver(t *testing.T) {
	t.Parallel()

	t.Run("empty agent name resolves through the claude default registry key", func(t *testing.T) {
		t.Parallel()
		r := liveInteractiveSessionResolver{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{
				resolveResp: &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: "sess-1"},
			},
		}}
		// agentName "" must fall through to defaultLegacyAgent ("claude"); a
		// non-nil registry and a non-nil response must each be carried past
		// their guard clauses so the resolved SessionID survives.
		got, err := r.ResolveInteractiveSessionID(context.Background(), "", "/work", "", time.Time{}, time.Time{}, false, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.SessionID != "sess-1" {
			t.Fatalf("SessionID = %q, want %q (empty name must map to claude, registry+response must pass their guards)", got.SessionID, "sess-1")
		}
		if got.Ambiguous {
			t.Errorf("Ambiguous = true, want false")
		}
	})

	t.Run("ambiguous response is surfaced", func(t *testing.T) {
		t.Parallel()
		r := liveInteractiveSessionResolver{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{
				resolveResp: &bossanovav1.ResolveInteractiveSessionIDResponse{Ambiguous: true, Reason: "two matches"},
			},
		}}
		got, err := r.ResolveInteractiveSessionID(context.Background(), "claude", "/work", "", time.Time{}, time.Time{}, false, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Ambiguous || got.Reason != "two matches" {
			t.Fatalf("got %+v, want Ambiguous with reason %q", got, "two matches")
		}
	})

	t.Run("client error is wrapped and returned", func(t *testing.T) {
		t.Parallel()
		r := liveInteractiveSessionResolver{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{resolveErr: errors.New("boom")},
		}}
		got, err := r.ResolveInteractiveSessionID(context.Background(), "claude", "/work", "", time.Time{}, time.Time{}, false, 0)
		if err == nil {
			t.Fatalf("expected error, got resolution %+v", got)
		}
		if got.SessionID != "" {
			t.Errorf("SessionID = %q, want empty on error", got.SessionID)
		}
	})

	t.Run("nil registry returns an empty resolution", func(t *testing.T) {
		t.Parallel()
		var r liveInteractiveSessionResolver
		got, err := r.ResolveInteractiveSessionID(context.Background(), "claude", "/work", "", time.Time{}, time.Time{}, false, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.SessionID != "" || got.Ambiguous {
			t.Fatalf("got %+v, want empty resolution for nil registry", got)
		}
	})

	t.Run("unknown agent returns an empty resolution", func(t *testing.T) {
		t.Parallel()
		r := liveInteractiveSessionResolver{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{
				resolveResp: &bossanovav1.ResolveInteractiveSessionIDResponse{Found: true, SessionId: "sess-1"},
			},
		}}
		got, err := r.ResolveInteractiveSessionID(context.Background(), "codex", "/work", "", time.Time{}, time.Time{}, false, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.SessionID != "" {
			t.Fatalf("SessionID = %q, want empty for unregistered agent", got.SessionID)
		}
	})
}

func TestLiveTranscriptOracle(t *testing.T) {
	t.Parallel()

	t.Run("empty agent name probes through the claude default registry key", func(t *testing.T) {
		t.Parallel()
		o := liveTranscriptOracle{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{
				transcriptResp: &bossanovav1.TranscriptExistsResponse{Exists: true},
			},
		}}
		// "" must map to claude; a non-nil registry and a (nil err, non-nil
		// resp) pair must each pass their guards so Exists survives as true.
		if !o.TranscriptExists(context.Background(), "", "/work", "sid") {
			t.Fatal("want true: empty name must map to claude and a present transcript must report exists")
		}
	})

	t.Run("nil registry reports not-exists", func(t *testing.T) {
		t.Parallel()
		var o liveTranscriptOracle
		if o.TranscriptExists(context.Background(), "claude", "/work", "sid") {
			t.Fatal("nil registry must report not-exists")
		}
	})

	t.Run("unknown agent reports not-exists", func(t *testing.T) {
		t.Parallel()
		o := liveTranscriptOracle{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{transcriptResp: &bossanovav1.TranscriptExistsResponse{Exists: true}},
		}}
		if o.TranscriptExists(context.Background(), "codex", "/work", "sid") {
			t.Fatal("unregistered agent must report not-exists")
		}
	})

	t.Run("client error reports not-exists", func(t *testing.T) {
		t.Parallel()
		o := liveTranscriptOracle{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{transcriptErr: errors.New("boom")},
		}}
		if o.TranscriptExists(context.Background(), "claude", "/work", "sid") {
			t.Fatal("client error must report not-exists")
		}
	})

	t.Run("present transcript reports exists", func(t *testing.T) {
		t.Parallel()
		o := liveTranscriptOracle{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{transcriptResp: &bossanovav1.TranscriptExistsResponse{Exists: true}},
		}}
		if !o.TranscriptExists(context.Background(), "claude", "/work", "sid") {
			t.Fatal("present transcript must report exists")
		}
	})

	t.Run("absent transcript reports not-exists", func(t *testing.T) {
		t.Parallel()
		o := liveTranscriptOracle{clients: map[string]agent.AgentRunnerClient{
			"claude": stubResolverAgentClient{transcriptResp: &bossanovav1.TranscriptExistsResponse{Exists: false}},
		}}
		if o.TranscriptExists(context.Background(), "claude", "/work", "sid") {
			t.Fatal("absent transcript must report not-exists")
		}
	})
}

// spawnAndCaptureLog runs one spawn with a capturing logger and returns
// whatever the undelivered-instruction reporter wrote.
func spawnAndCaptureLog(t *testing.T, support bossanovav1.AppendSystemPromptSupport, classes []string) string {
	t.Helper()
	var buf bytes.Buffer
	builder := claudeArgvBuilder()
	builder.support = support
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: &fakeTranscriptOracle{exists: false},
		Argv:        builder,
		Logger:      zerolog.New(&buf),
	}, spawnInput{
		Chat:                      newTestChat(t),
		WorktreePath:              t.TempDir(),
		TmuxName:                  "boss-aaa-bbb",
		AppendSystemPrompt:        "boss session context …",
		AppendSystemPromptClasses: classes,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// argv is spawned from the response regardless of the declaration: report,
	// never enforce.
	if tmuxer.createdN != 1 {
		t.Fatalf("spawn must proceed whatever the runner declared, createdN = %d", tmuxer.createdN)
	}
	return buf.String()
}

// TestSpawnChatTmux_ReportsUndeliveredInstructions proves the wake/re-ensure
// spawn path reports a runner that never carried the suffix into argv — and
// still spawns it.
func TestSpawnChatTmux_ReportsUndeliveredInstructions(t *testing.T) {
	line := spawnAndCaptureLog(t,
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE,
		[]string{session.InstructionClassSessionContext, session.InstructionClassAutonomyDirective})
	if line == "" {
		t.Fatal("a NONE declaration with built instructions must emit a record")
	}
	for _, want := range []string{
		session.InstructionClassAutonomyDirective,
		"APPEND_SYSTEM_PROMPT_SUPPORT_NONE",
		`"level":"error"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("record missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "boss session context") {
		t.Errorf("record leaked the prompt body: %s", line)
	}
}

func TestSpawnChatTmux_SilentWhenRunnerDeclaresInArgv(t *testing.T) {
	if line := spawnAndCaptureLog(t,
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_IN_ARGV,
		[]string{session.InstructionClassSessionContext}); line != "" {
		t.Fatalf("IN_ARGV must emit nothing, got %s", line)
	}
}

// A caller that builds no instruction classes reports nothing even from a
// runner that declares nothing — there is no drop to name.
func TestSpawnChatTmux_SilentWhenNoInstructionsBuilt(t *testing.T) {
	if line := spawnAndCaptureLog(t,
		bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_UNSPECIFIED, nil); line != "" {
		t.Fatalf("no classes built must emit nothing, got %s", line)
	}
}
