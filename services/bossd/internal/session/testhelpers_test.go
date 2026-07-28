package session

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// backgroundDraftPRIsTracked reports whether a background draft-PR step is
// currently registered for sessionID. Tests use it to assert the NEGATIVE case
// (DeferPR starts no step at all), which WaitForBackgroundDraftPR cannot
// distinguish from "the step already finished".
func (l *Lifecycle) backgroundDraftPRIsTracked(sessionID string) bool {
	l.backgroundDraftPRMu.Lock()
	defer l.backgroundDraftPRMu.Unlock()
	_, ok := l.backgroundDraftPRs[sessionID]
	return ok
}

// awaitDraftPR blocks until the background draft-PR step StartSession spawned
// for sessionID has finished (BOS-540). Every StartSession assertion that reads
// the session row, the provider's create calls, or the worktree call log must go
// through this first: the create no longer completes before StartSession
// returns, so reading those without joining the goroutine both races it and
// asserts on a half-written state.
//
// It is a no-op when no create is in flight — a DeferPR session, a session that
// already had a PR, or a StartSession that failed before the spawn — so it is
// safe to call after every StartSession unconditionally.
func awaitDraftPR(t *testing.T, lc *Lifecycle, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lc.WaitForBackgroundDraftPR(ctx, sessionID); err != nil {
		t.Fatalf("background draft PR for %s did not finish: %v", sessionID, err)
	}
}

// Compile-time check that fakeAgentForLifecycle satisfies AgentRunnerClient.
var _ agent.AgentRunnerClient = (*fakeAgentForLifecycle)(nil)

// fakeAgentForLifecycle is a no-op fake for agent.AgentRunnerClient.
// Tests that exercise the HookToken path inject this via lc.SetAgent.
// The LastConfigureHookReq field lets callers verify what was passed.
type fakeAgentForLifecycle struct {
	LastConfigureHookReq        *bossanovav1.ConfigureFinalizeHookRequest
	ConfigureHookReqs           []*bossanovav1.ConfigureFinalizeHookRequest
	LastBuildInteractiveCommand *bossanovav1.BuildInteractiveCommandRequest
	IsSupported                 bool  // controls ConfigureFinalizeHook response
	ConfigureHookErr            error // when non-nil, ConfigureFinalizeHook returns it
	ReadyMarker                 string
	CommandPrefix               string
	ConsumesInitialInput        bool
	OnConfigureHook             func()
	SuggestPRTitleFunc          func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error)
	IgnoredDirtyFiles           []string
}

func newFakeAgent() *fakeAgentForLifecycle {
	return &fakeAgentForLifecycle{IsSupported: true}
}

func (f *fakeAgentForLifecycle) GetInfo(_ context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}

func (f *fakeAgentForLifecycle) StartRun(_ context.Context, _ *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{SessionId: "fake"}, nil
}

func (f *fakeAgentForLifecycle) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (f *fakeAgentForLifecycle) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}

func (f *fakeAgentForLifecycle) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: true}, nil
}

func (f *fakeAgentForLifecycle) ConfigureFinalizeHook(_ context.Context, req *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	f.LastConfigureHookReq = req
	f.ConfigureHookReqs = append(f.ConfigureHookReqs, req)
	if f.OnConfigureHook != nil {
		f.OnConfigureHook()
	}
	if f.ConfigureHookErr != nil {
		return nil, f.ConfigureHookErr
	}
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: f.IsSupported}, nil
}

func (f *fakeAgentForLifecycle) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}

func (f *fakeAgentForLifecycle) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	f.LastBuildInteractiveCommand = req
	// Mirror the shape of plugins/bossd-plugin-claude's real argv so cron
	// tests that assert on the substring "claude --session-id " still pass
	// against the extracted StartTmuxChat path. The production plugin
	// returns bare argv (no shell wrapper) so tmux can spawn claude
	// directly on a PTY; capture-to-disk is handled by `tmux pipe-pane`
	// after the spawn, not by piping stdout through tee.
	argv := []string{"claude", "--session-id", req.SessionId}
	if f.ConsumesInitialInput {
		if req.GetInitialCommand() != "" {
			prefix := f.CommandPrefix
			if prefix == "" {
				prefix = "/"
			}
			argv = append(argv, prefix+strings.TrimLeft(req.GetInitialCommand(), "/$"))
		} else if req.GetInitialPrompt() != "" {
			argv = append(argv, req.GetInitialPrompt())
		}
	}
	return &bossanovav1.BuildInteractiveCommandResponse{
		Argv:                 argv,
		ReadyMarker:          f.ReadyMarker,
		CommandPrefix:        f.CommandPrefix,
		ConsumesInitialInput: f.ConsumesInitialInput,
	}, nil
}

func (f *fakeAgentForLifecycle) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{Found: req.GetRequestedSessionId() != "", SessionId: req.GetRequestedSessionId()}, nil
}

func (f *fakeAgentForLifecycle) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: f.IgnoredDirtyFiles}, nil
}

func (f *fakeAgentForLifecycle) SuggestPRTitle(_ context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	if f.SuggestPRTitleFunc != nil {
		return f.SuggestPRTitleFunc(req)
	}
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *fakeAgentForLifecycle) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}

func (f *fakeAgentForLifecycle) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *fakeAgentForLifecycle) DetectUsageLimit(_ context.Context, _ *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (f *fakeAgentForLifecycle) ProbeRateLimit(_ context.Context, _ *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return &bossanovav1.ProbeRateLimitResponse{}, nil
}

func (f *fakeAgentForLifecycle) HasWorkingIndicator(_ context.Context, _ *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{}, nil
}

func (f *fakeAgentForLifecycle) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}

func (f *fakeAgentForLifecycle) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

func (f *fakeAgentForLifecycle) ReadTranscript(_ context.Context, _ *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}

func (f *fakeAgentForLifecycle) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{}, nil
}

func (f *fakeAgentForLifecycle) MaterializeAccount(_ context.Context, _ *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
}

// fakePollArmer records calls to Arm so tests can assert that the poll
// fallback was (or was not) wired for a given agent_session_id. armCount and
// armedSessions let a test verify the EXACT set of sessions armed (e.g. that a
// cron or hook-supported sibling was not also armed) rather than just the last.
type fakePollArmer struct {
	armCalled      bool
	armCount       int
	armedSessionID string
	armedID        string
	armedSessions  []string
	client         agent.AgentRunnerClient
}

func (f *fakePollArmer) Arm(_ context.Context, sessionID, agentSessionID string, client agent.AgentRunnerClient) {
	f.armCalled = true
	f.armCount++
	f.armedSessionID = sessionID
	f.armedID = agentSessionID
	f.armedSessions = append(f.armedSessions, sessionID)
	f.client = client
}

type recordingCronCompletionNotifier struct {
	mu    sync.Mutex
	calls []string
}

func (n *recordingCronCompletionNotifier) NotifyCronAgentStopped(sessionID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, sessionID)
}

func (n *recordingCronCompletionNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *recordingCronCompletionNotifier) callsCopy() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.calls))
	copy(out, n.calls)
	return out
}

type recordingPollCompleter struct {
	mu    sync.Mutex
	calls []pollCompletionCall
}

type pollCompletionCall struct {
	sessionID      string
	agentSessionID string
	exitError      string
}

func (c *recordingPollCompleter) SignalRunComplete(sessionID, agentSessionID, exitError string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, pollCompletionCall{
		sessionID:      sessionID,
		agentSessionID: agentSessionID,
		exitError:      exitError,
	})
}

func (c *recordingPollCompleter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *recordingPollCompleter) callsCopy() []pollCompletionCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]pollCompletionCall, len(c.calls))
	copy(out, c.calls)
	return out
}
