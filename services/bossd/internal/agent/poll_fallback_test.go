package agent_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// fakePollAgentClient implements agent.AgentRunnerClient. PollFallback only
// exercises ExitStatus; the other methods are defined as no-ops so the
// fake satisfies the full interface.
type fakePollAgentClient struct {
	exitCalls int32
	complete  bool
	exitErr   string
}

func (f *fakePollAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}
func (f *fakePollAgentClient) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	atomic.AddInt32(&f.exitCalls, 1)
	return &bossanovav1.AgentExitStatusResponse{IsComplete: f.complete, ExitError: f.exitErr}, nil
}

func (f *fakePollAgentClient) StartRun(_ context.Context, _ *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	return &bossanovav1.StartAgentRunResponse{}, nil
}
func (f *fakePollAgentClient) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, nil
}
func (f *fakePollAgentClient) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{}, nil
}
func (f *fakePollAgentClient) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{}, nil
}
func (f *fakePollAgentClient) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}
func (f *fakePollAgentClient) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (f *fakePollAgentClient) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}
func (f *fakePollAgentClient) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}
func (f *fakePollAgentClient) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakePollAgentClient) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}

type fakeCompleter struct {
	mu       sync.Mutex
	signaled bool
	exitErr  string
	id       string
}

func (f *fakeCompleter) SignalRunComplete(id, exitErr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signaled = true
	f.exitErr = exitErr
	f.id = id
}

func (f *fakeCompleter) snapshot() (bool, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signaled, f.exitErr, f.id
}

func TestPollFallbackSignalsCompletionWhenExitStatusReady(t *testing.T) {
	t.Parallel()
	ac := &fakePollAgentClient{complete: true, exitErr: ""}
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(zerolog.Nop(), 10*time.Millisecond, 0, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	p.Arm(ctx, "agent-sess-1", ac)

	deadline := time.After(500 * time.Millisecond)
	for {
		signaled, _, id := cc.snapshot()
		if signaled {
			if id != "agent-sess-1" {
				t.Errorf("id = %q", id)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("never signaled")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestPollFallbackSurfacesExitErrorVerbatim(t *testing.T) {
	t.Parallel()
	ac := &fakePollAgentClient{complete: true, exitErr: "boom"}
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(zerolog.Nop(), 10*time.Millisecond, 0, cc)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	p.Arm(ctx, "x", ac)
	deadline := time.After(500 * time.Millisecond)
	for {
		signaled, exitErr, _ := cc.snapshot()
		if signaled {
			if exitErr != "boom" {
				t.Errorf("exitErr = %q", exitErr)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("never signaled")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestPollFallbackStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ac := &fakePollAgentClient{complete: false}
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(zerolog.Nop(), 10*time.Millisecond, 0, cc)
	ctx, cancel := context.WithCancel(context.Background())
	p.Arm(ctx, "x", ac)
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	before := atomic.LoadInt32(&ac.exitCalls)
	time.Sleep(50 * time.Millisecond)
	after := atomic.LoadInt32(&ac.exitCalls)
	if after != before {
		t.Errorf("polls continued after cancel: before=%d after=%d", before, after)
	}
}
