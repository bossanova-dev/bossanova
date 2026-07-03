package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
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
