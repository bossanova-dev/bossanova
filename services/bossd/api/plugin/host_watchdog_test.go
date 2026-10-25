package plugin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

const watchdogMarker = "agent went idle without completing"

// useFastWatchdog shrinks the package-level watchdog poll interval so armed
// WaitChatRun tests fire in milliseconds, restoring it afterwards. These tests
// do not call t.Parallel(), so the shared global is mutated sequentially.
func useFastWatchdog(t *testing.T) {
	t.Helper()
	prev := watchdogPollInterval
	watchdogPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { watchdogPollInterval = prev })
}

// newWatchdogServer builds a run-complete server with a real tracker and a
// programmable fake lifecycle, and registers the run so runSessionByID resolves.
func newWatchdogServer(t *testing.T) (*HostServiceServer, *fakeChatLifecycle) {
	t.Helper()
	srv := newRunCompleteServer()
	srv.chatTracker = status.NewTracker()
	lc := &fakeChatLifecycle{}
	srv.lifecycle = lc
	return srv, lc
}

func reclaimCalls(lc *fakeChatLifecycle) int {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return len(lc.reclaimRepairChatReqs)
}

func TestWaitChatRunWatchdog_DisabledFieldZero(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(60 * time.Millisecond)
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 0,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if !strings.Contains(resp.GetExitError(), "did not signal completion") {
		t.Fatalf("ExitError = %q, want legacy deadline message", resp.GetExitError())
	}
	if strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError %q must not carry the watchdog marker when disabled", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 0 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 0", got)
	}
}

func TestWaitChatRunWatchdog_FiresOnContinuousIdle(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(5 * time.Second)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-10*time.Minute))
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if !strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError = %q, want watchdog marker", resp.GetExitError())
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if len(lc.reclaimRepairChatReqs) != 1 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 1", len(lc.reclaimRepairChatReqs))
	}
	call := lc.reclaimRepairChatReqs[0]
	if call.sessionID != "sess-1" || call.agentSessionID != "agent-x" {
		t.Fatalf("reclaim call = %+v, want sess-1/agent-x", call)
	}
	if !strings.Contains(call.reason, "watchdog") {
		t.Fatalf("reclaim reason = %q, want watchdog reason", call.reason)
	}
}

func TestWaitChatRunWatchdog_WorkingNeverFires(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(80 * time.Millisecond)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now().Add(-1*time.Hour))
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError %q must not fire the watchdog for a WORKING chat", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 0 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 0", got)
	}
}

func TestWaitChatRunWatchdog_QuestionFiresAfterWindow(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(5 * time.Second)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_QUESTION, time.Now().Add(-16*time.Minute))
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if !strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError = %q, want watchdog marker for aged QUESTION", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 1 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 1", got)
	}
}

func TestWaitChatRunWatchdog_QuestionYoungNeverFires(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(80 * time.Millisecond)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_QUESTION, time.Now().Add(-1*time.Minute))
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError %q must not fire for a young QUESTION", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 0 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 0", got)
	}
}

func TestWaitChatRunWatchdog_StoppedFiresFast(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	srv.setWaitChatRunDeadline(5 * time.Second)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_STOPPED, time.Now())
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if !strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError = %q, want watchdog marker for STOPPED", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 1 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 1", got)
	}
}

func TestWaitChatRunWatchdog_HookWinsRace(t *testing.T) {
	srv, lc := newWatchdogServer(t)
	// Deliberately large poll interval so the first select cannot see a tick;
	// the pre-loaded hook result must win.
	prev := watchdogPollInterval
	watchdogPollInterval = time.Second
	t.Cleanup(func() { watchdogPollInterval = prev })

	srv.setWaitChatRunDeadline(5 * time.Second)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-10*time.Minute))
	ch := srv.registerRun("sess-1", "agent-x", "tok")
	ch <- completionResult{exitError: "hook-result"}

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if resp.GetExitError() != "hook-result" {
		t.Fatalf("ExitError = %q, want hook-result (hook must win the race)", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 0 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 0 when hook wins", got)
	}
}

func TestWatchdogCompletionAvailable_DrainsBufferedHookResult(t *testing.T) {
	done := make(chan completionResult, 1)
	done <- completionResult{exitError: "hook-result"}
	run := activeChatRun{done: done, completionMode: chatRunCompletionHook}

	res, ok := watchdogCompletionAvailable(run)
	if !ok {
		t.Fatal("watchdogCompletionAvailable = false, want true")
	}
	if res.exitError != "hook-result" {
		t.Fatalf("exitError = %q, want hook-result", res.exitError)
	}

	select {
	case extra := <-done:
		t.Fatalf("completion channel still had buffered result: %+v", extra)
	default:
	}
}

func TestWaitChatRunWatchdog_ReclaimErrorStillFailsRun(t *testing.T) {
	useFastWatchdog(t)
	srv, lc := newWatchdogServer(t)
	lc.reclaimRepairChatFunc = func(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
		return session.ReclaimRepairChatResult{}, context.DeadlineExceeded
	}
	srv.setWaitChatRunDeadline(5 * time.Second)
	srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-10*time.Minute))
	srv.registerRun("sess-1", "agent-x", "tok")

	resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{
		AgentSessionId:       "agent-x",
		IdleFailAfterSeconds: 1,
	})
	if err != nil {
		t.Fatalf("WaitChatRun: %v", err)
	}
	if !strings.Contains(resp.GetExitError(), watchdogMarker) {
		t.Fatalf("ExitError = %q, want watchdog marker even when reclaim errors", resp.GetExitError())
	}
	if got := reclaimCalls(lc); got != 1 {
		t.Fatalf("ReclaimRepairChat calls = %d, want 1 (attempted despite error)", got)
	}
}

// watchdogTranscriptClient overrides only transcript reads on the existing runner fake.
type watchdogTranscriptClient struct {
	*fakeAgentClient
	read func(*bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error)
}

func (f *watchdogTranscriptClient) ReadTranscript(_ context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return f.read(req)
}

type watchdogStopStore struct {
	AgentRunStore
	reasons []string
}

func (f *watchdogStopStore) MarkStoppedByAgentSessionID(_ context.Context, _, reason string, _ time.Time) error {
	f.reasons = append(f.reasons, reason)
	return nil
}

func TestWaitChatRunWatchdog_TranscriptEvidence(t *testing.T) {
	startedAt := time.Date(2026, 10, 25, 12, 0, 0, 123456789, time.UTC)
	current := startedAt.Add(time.Second).Format(time.RFC3339Nano)
	tests := []struct {
		name, role, timestamp, text                                string
		missing, empty, readError, fallback, noProvider, completed bool
	}{
		{name: "completed", role: "assistant", timestamp: current, text: "Repair assessed; nothing to change", completed: true},
		{name: "millisecond boundary", role: "assistant", timestamp: startedAt.Truncate(time.Millisecond).Format(time.RFC3339Nano), completed: true},
		{name: "provider fallback", role: "assistant", timestamp: current, fallback: true, completed: true},
		{name: "agent id only", role: "assistant", timestamp: current, noProvider: true, completed: true},
		{name: "missing transcript", missing: true},
		{name: "empty transcript", empty: true},
		{name: "read error", readError: true},
		{name: "user last", role: "user", timestamp: current},
		{name: "stale assistant", role: "assistant", timestamp: startedAt.Add(-time.Second).Format(time.RFC3339Nano)},
		{name: "missing timestamp", role: "assistant"},
		{name: "invalid timestamp", role: "assistant", timestamp: "not-a-date"},
		{name: "usage exhausted", role: "assistant", timestamp: current, text: "You have hit the usage limit"},
		{name: "rate limited", role: "assistant", timestamp: current, text: "HTTP 429 Too Many Requests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFastWatchdog(t)
			srv, lc := newWatchdogServer(t)
			providerID := "provider-x"
			chat := &domain.AgentChat{SessionID: "sess-1", AgentSessionID: "agent-x", AgentName: "codex"}
			if !tt.noProvider {
				chat.ProviderSessionID = &providerID
			}
			srv.agentChats = &fakeAgentChatStore{chatsByAgent: map[string]*domain.AgentChat{"agent-x": chat}}
			srv.sessionStore = &fakeSessionStore{sessions: map[string]*models.Session{"sess-1": {ID: "sess-1", WorktreePath: "/tmp/watchdog-worktree", AgentName: "codex"}}}
			var ids []string
			client := &watchdogTranscriptClient{fakeAgentClient: newFakeAgentClient()}
			client.read = func(req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
				ids = append(ids, req.GetAgentSessionId())
				if req.GetWorkDir() != "/tmp/watchdog-worktree" || req.GetMaxMessages() != 1 {
					t.Errorf("unexpected transcript request: %v", req)
				}
				if tt.readError {
					return nil, errors.New("unreadable transcript")
				}
				if tt.missing || (tt.fallback && req.GetAgentSessionId() == providerID) {
					return &bossanovav1.ReadTranscriptResponse{}, nil
				}
				resp := &bossanovav1.ReadTranscriptResponse{Exists: true}
				if !tt.empty {
					resp.Messages = []*bossanovav1.ChatMessage{{Role: tt.role, Timestamp: tt.timestamp, Text: tt.text}}
				}
				return resp, nil
			}
			srv.agentClients = map[string]agent.RunnerClient{"codex": client}
			stops := &watchdogStopStore{}
			srv.agentRuns = stops
			srv.registerRun("sess-1", "agent-x", "tok")
			srv.runMu.Lock()
			run := srv.runCompletion["agent-x"]
			run.startedAt = startedAt
			srv.runCompletion["agent-x"] = run
			srv.runMu.Unlock()
			srv.chatTracker.Update("agent-x", bossanovav1.ChatStatus_CHAT_STATUS_IDLE, time.Now().Add(-10*time.Minute))
			resp, err := srv.WaitChatRun(t.Context(), &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x", IdleFailAfterSeconds: 1})
			if err != nil {
				t.Fatal(err)
			}
			wantReason, wantReclaims := domain.AgentRunStopUnknown, 1
			if tt.completed {
				wantReason, wantReclaims = domain.AgentRunStopClean, 0
				if resp.GetExitError() != "" {
					t.Errorf("ExitError = %q, want clean completion", resp.GetExitError())
				}
			} else if !strings.Contains(resp.GetExitError(), watchdogMarker) {
				t.Errorf("ExitError = %q, want watchdog failure", resp.GetExitError())
			}
			if got := reclaimCalls(lc); got != wantReclaims {
				t.Errorf("reclaims = %d, want %d", got, wantReclaims)
			}
			if !reflect.DeepEqual(stops.reasons, []string{wantReason}) {
				t.Errorf("stop reasons = %v, want %s", stops.reasons, wantReason)
			}
			wantProvider := providerID
			if tt.noProvider {
				wantProvider = ""
			}
			if resp.GetProviderSessionId() != wantProvider {
				t.Errorf("provider id = %q, want %q", resp.GetProviderSessionId(), wantProvider)
			}
			wantIDs := []string{providerID}
			if tt.noProvider {
				wantIDs = []string{"agent-x"}
			} else if tt.fallback || tt.missing || tt.readError {
				wantIDs = append(wantIDs, "agent-x")
			}
			if !reflect.DeepEqual(ids, wantIDs) {
				t.Errorf("transcript ids = %v, want %v", ids, wantIDs)
			}
			srv.runMu.Lock()
			_, active := srv.runCompletion["agent-x"]
			srv.runMu.Unlock()
			if active {
				t.Error("run still registered after completion")
			}
		})
	}
}
