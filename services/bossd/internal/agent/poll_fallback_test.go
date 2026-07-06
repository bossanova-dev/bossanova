package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

// fakePollAgentClient implements agent.AgentRunnerClient. PollFallback only
// exercises ExitStatus; the other methods are defined as no-ops so the
// fake satisfies the full interface.
type fakePollAgentClient struct {
	exitCalls    int32
	complete     bool
	exitErr      string
	failureClass string
	resetAt      *timestamppb.Timestamp
}

func (f *fakePollAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}
func (f *fakePollAgentClient) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	atomic.AddInt32(&f.exitCalls, 1)
	resp := &bossanovav1.AgentExitStatusResponse{IsComplete: f.complete, ExitError: f.exitErr}
	if f.failureClass != "" {
		fc := f.failureClass
		resp.FailureClass = &fc
	}
	resp.ResetAt = f.resetAt
	return resp, nil
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
func (f *fakePollAgentClient) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}
func (f *fakePollAgentClient) BuildInteractiveCommand(_ context.Context, _ *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{}, nil
}
func (f *fakePollAgentClient) ResolveInteractiveSessionID(_ context.Context, _ *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}
func (f *fakePollAgentClient) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{}, nil
}
func (f *fakePollAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *fakePollAgentClient) GetChatTitle(_ context.Context, _ *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{}, nil
}
func (f *fakePollAgentClient) HasQuestionPrompt(_ context.Context, _ *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *fakePollAgentClient) DetectUsageLimit(_ context.Context, _ *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (f *fakePollAgentClient) HasWorkingIndicator(_ context.Context, _ *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{}, nil
}
func (f *fakePollAgentClient) LastTurnIsUser(_ context.Context, _ *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakePollAgentClient) TranscriptExists(_ context.Context, _ *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}
func (f *fakePollAgentClient) ReadTranscript(_ context.Context, _ *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}
func (f *fakePollAgentClient) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{}, nil
}
func (f *fakePollAgentClient) MaterializeAccount(_ context.Context, _ *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
}

type fakeCompleter struct {
	mu        sync.Mutex
	signaled  bool
	exitErr   string
	sessionID string
	id        string
}

func (f *fakeCompleter) SignalSessionRunComplete(sessionID, id, exitErr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signaled = true
	f.exitErr = exitErr
	f.sessionID = sessionID
	f.id = id
}

func (f *fakeCompleter) snapshot() (bool, string, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signaled, f.sessionID, f.exitErr, f.id
}

func TestPollFallbackSignalsCompletionWhenExitStatusReady(t *testing.T) {
	t.Parallel()
	ac := &fakePollAgentClient{complete: true, exitErr: ""}
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(zerolog.Nop(), 10*time.Millisecond, 0, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	p.Arm(ctx, "sess-1", "agent-sess-1", ac)

	deadline := time.After(500 * time.Millisecond)
	for {
		signaled, sessionID, _, id := cc.snapshot()
		if signaled {
			if sessionID != "sess-1" {
				t.Errorf("sessionID = %q", sessionID)
			}
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
	p.Arm(ctx, "sess-1", "x", ac)
	deadline := time.After(500 * time.Millisecond)
	for {
		signaled, _, exitErr, _ := cc.snapshot()
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

// TestPollFallbackRecordsUsageLimitedExit is the BOS-165 record-only harness
// test: a usage_exhausted ExitStatus response emits a structured log line
// (with reset_at) and still completes the run verbatim — no rotation or other
// action, and exit handling is unchanged.
func TestPollFallbackRecordsUsageLimitedExit(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	ac := &fakePollAgentClient{
		complete:     true,
		exitErr:      "agent usage limit reached (resets at 2026-07-08T15:00:00Z)",
		failureClass: "usage_exhausted",
		resetAt:      timestamppb.New(reset),
	}
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(logger, 10*time.Millisecond, 0, cc)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	p.Arm(ctx, "sess-cap", "agent-cap", ac)

	deadline := time.After(500 * time.Millisecond)
	for {
		signaled, _, exitErr, _ := cc.snapshot()
		if signaled {
			// Record-only: the run still completes with the verbatim exit
			// error (no rotation / no state change).
			if exitErr != "agent usage limit reached (resets at 2026-07-08T15:00:00Z)" {
				t.Errorf("exitErr = %q, want verbatim usage-limited message", exitErr)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("never signaled")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Assert the structured record-only log line was emitted.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["failure_class"] != "usage_exhausted" {
			continue
		}
		found = true
		if entry["agent_session"] != "agent-cap" {
			t.Errorf("agent_session = %v, want agent-cap", entry["agent_session"])
		}
		if entry["session"] != "sess-cap" {
			t.Errorf("session = %v, want sess-cap", entry["session"])
		}
		if entry["reset_at"] == nil {
			t.Error("reset_at missing from record-only log line")
		}
	}
	if !found {
		t.Fatalf("no usage_exhausted record-only log line emitted; logs=%q", buf.String())
	}
}

func TestPollFallbackStopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ac := &fakePollAgentClient{complete: false}
	cc := &fakeCompleter{}
	p := agent.NewPollFallback(zerolog.Nop(), 10*time.Millisecond, 0, cc)
	ctx, cancel := context.WithCancel(context.Background())
	p.Arm(ctx, "sess-1", "x", ac)
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
