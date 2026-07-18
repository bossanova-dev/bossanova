package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

type gateSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
}

func newGateSessionStore() *gateSessionStore {
	return &gateSessionStore{sessions: map[string]*models.Session{}}
}

func (s *gateSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[id]
	if session == nil {
		return nil, fmt.Errorf("session %s not found", id)
	}
	copy := *session
	return &copy, nil
}

type recordingCronFinalizer struct {
	mu    sync.Mutex
	calls []string
}

func (f *recordingCronFinalizer) FinalizeSession(_ context.Context, sessionID string) (*FinalizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, sessionID)
	return &FinalizeResult{Outcome: models.CronJobOutcomeDeletedNoChanges}, nil
}

func (f *recordingCronFinalizer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func waitForCount(t *testing.T, name string, count func() int) {
	t.Helper()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		got := count()
		if got == 1 {
			return
		}

		select {
		case <-timer.C:
			t.Fatalf("%s count = %d, want 1", name, got)
		case <-ticker.C:
		}
	}
}

func TestCronCompletionGateFinalizesAfterHookSignalWithoutWaitingForTmuxExit(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

func TestCronCompletionGateFinalizesAfterPollCompletionSignal(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

// blockingCronFinalizer blocks inside FinalizeSession until the supplied
// context is cancelled, then records the context's deadline-ness and error.
// It lets a test prove the gate bounds finalization rather than letting a hung
// git/GitHub call pin the goroutine forever.
type blockingCronFinalizer struct {
	started     chan struct{}
	hadDeadline bool
	ctxErr      error
	done        chan struct{}
}

func newBlockingCronFinalizer() *blockingCronFinalizer {
	return &blockingCronFinalizer{started: make(chan struct{}, 1), done: make(chan struct{})}
}

func (f *blockingCronFinalizer) FinalizeSession(ctx context.Context, _ string) (*FinalizeResult, error) {
	_, f.hadDeadline = ctx.Deadline()
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	f.ctxErr = ctx.Err()
	close(f.done)
	return nil, ctx.Err()
}

// TestCronCompletionGateBoundsFinalizeWithTimeout verifies the gated finalize
// path applies a deadline (restoring the hook server's former
// finalizeDispatchTimeout) so a finalize that blocks on network/credentials
// can't run forever.
func TestCronCompletionGateBoundsFinalizeWithTimeout(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := newBlockingCronFinalizer()

	cronID := "cron-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:        "sess-1",
		CronJobID: &cronID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:        sessions,
		Finalizer:       finalizer,
		QuietDelay:      time.Millisecond,
		FinalizeTimeout: 20 * time.Millisecond,
	})

	gate.NotifyCronAgentStopped("sess-1")

	select {
	case <-finalizer.started:
	case <-time.After(time.Second):
		t.Fatal("FinalizeSession was never invoked")
	}

	select {
	case <-finalizer.done:
	case <-time.After(time.Second):
		t.Fatal("FinalizeSession did not return; the finalize timeout was not applied")
	}

	if !finalizer.hadDeadline {
		t.Error("FinalizeSession context had no deadline; gated finalize is unbounded")
	}
	if finalizer.ctxErr != context.DeadlineExceeded {
		t.Errorf("ctx error = %v, want %v", finalizer.ctxErr, context.DeadlineExceeded)
	}
}

// assertNoFinalize fails if any FinalizeSession call lands within window. It is
// the inverse of waitForCount: it proves the gate did NOT finalize.
func assertNoFinalize(t *testing.T, name string, count func() int, window time.Duration) {
	t.Helper()

	deadline := time.After(window)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if got := count(); got != 0 {
			t.Fatalf("%s count = %d, want 0", name, got)
		}
		select {
		case <-deadline:
			return
		case <-ticker.C:
		}
	}
}

// TestCronCompletionGateDefersWhenRunNotOver reproduces the #874 false positive:
// the Stop hook fires while the cron agent is still working (e.g. paused awaiting
// a background subagent). The gate must NOT finalize — RunIsOver returns false —
// so the live run is left for the periodic recovery sweep, not finalized mid-run.
func TestCronCompletionGateDefersWhenRunNotOver(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		RunIsOver:  func(*models.Session) bool { return false },
	})

	gate.NotifyCronAgentStopped("sess-1")
	assertNoFinalize(t, "FinalizeSession", finalizer.count, 50*time.Millisecond)
}

// TestCronCompletionGateFinalizesWhenRunOver verifies the fast path is preserved:
// when RunIsOver confirms the run is genuinely over, the gate finalizes promptly.
func TestCronCompletionGateFinalizesWhenRunOver(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		RunIsOver:  func(*models.Session) bool { return true },
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

// TestCronCompletionGateFinalizesWhenRunIsOverUnwired pins back-compat: a nil
// RunIsOver (partial/legacy wiring) preserves the prior behavior of finalizing
// any cron-linked Stop.
func TestCronCompletionGateFinalizesWhenRunIsOverUnwired(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		// RunIsOver left nil.
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

// TestCronCompletionGateFinalizesUnattendedTmuxSession proves the gate finalizes
// a tmux_unattended session (TmuxUnattended=true, CronJobID nil) once its run is
// over — the durable-tmux completion path for /boss-epic — mirroring the cron path.
func TestCronCompletionGateFinalizesUnattendedTmuxSession(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		TmuxUnattended: true,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		RunIsOver:  func(*models.Session) bool { return true },
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

// TestCronCompletionGateFinalizesDetachSession proves the gate finalizes a
// durable tmux-hosted detach session (Detach=true, CronJobID nil, TmuxUnattended
// false) once its run is over — BOS-428's completion-routing requirement. Without
// Detach joining the unattended class, a tmux-hosted detach run's finalize Stop
// hook would be admitted here then dropped, stranding it in ImplementingPlan.
func TestCronCompletionGateFinalizesDetachSession(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		Detach:         true,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		RunIsOver:  func(*models.Session) bool { return true },
	})

	gate.NotifyCronAgentStopped("sess-1")
	waitForCount(t, "FinalizeSession", finalizer.count)
}

// TestCronCompletionGateIgnoresInteractiveSession proves the gate never
// auto-finalizes a plain interactive session — one with a HookToken but neither
// CronJobID nor TmuxUnattended. HookToken presence alone is NOT the autonomy
// signal; the persisted TmuxUnattended flag is.
func TestCronCompletionGateIgnoresInteractiveSession(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	agentID := "agent-1"
	hookToken := "deadbeef"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		AgentSessionID: &agentID,
		HookToken:      &hookToken,
		// CronJobID nil, TmuxUnattended false.
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
		RunIsOver:  func(*models.Session) bool { return true },
	})

	gate.NotifyCronAgentStopped("sess-1")
	assertNoFinalize(t, "FinalizeSession", finalizer.count, 50*time.Millisecond)
}

func TestCronCompletionGateIgnoresStaleQuietPeriod(t *testing.T) {
	sessions := newGateSessionStore()
	finalizer := &recordingCronFinalizer{}

	cronID := "cron-1"
	agentID := "agent-1"
	sessions.sessions["sess-1"] = &models.Session{
		ID:             "sess-1",
		CronJobID:      &cronID,
		AgentSessionID: &agentID,
	}

	gate := NewCronCompletionGate(CronCompletionGateDeps{
		Sessions:   sessions,
		Finalizer:  finalizer,
		QuietDelay: time.Millisecond,
	})

	old := newCronCompletionGatePending(context.CancelFunc(func() {}))
	newer := newCronCompletionGatePending(context.CancelFunc(func() {}))
	gate.pending["sess-1"] = newer

	gate.afterQuietPeriod(context.Background(), "sess-1", old)

	if got := finalizer.count(); got != 0 {
		t.Fatalf("FinalizeSession calls = %d, want 0 for stale quiet period", got)
	}
	if got := gate.pending["sess-1"]; got != newer {
		t.Fatalf("pending entry = %p, want newer entry %p", got, newer)
	}
}
