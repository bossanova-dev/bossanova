package server

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/detach"
	"github.com/rs/zerolog"
)

// ctxRecordingSpawner captures the context killSpawnedChatPaneBestEffort
// actually hands tmux. The embedded interface is nil on purpose: only
// KillSession is reachable from the helper under test, and leaving the rest
// unimplemented keeps this fake from drifting as tmuxSpawner grows.
//
// It records the context's liveness AND its deadline because neither is
// recoverable from "KillSession was called": the real client builds an
// exec.CommandContext, which simply refuses to start on a dead context and
// whose error the helper deliberately swallows. Without these fields the
// obvious assertion passes whether or not the kill is detached (BOS-897).
type ctxRecordingSpawner struct {
	tmuxSpawner
	names    []string
	ctxErr   error
	deadline time.Time
}

func (s *ctxRecordingSpawner) KillSession(ctx context.Context, name string) error {
	s.names = append(s.names, name)
	s.ctxErr = ctx.Err()
	s.deadline, _ = ctx.Deadline()
	return nil
}

func TestKillSpawnedChatPaneBestEffortRunsOnADetachedBudgetedContext(t *testing.T) {
	t.Parallel()
	// The rollback is reached because the claim FAILED, and the commonest
	// failure is the caller's own context expiring — so a rollback issued on
	// that context never runs, and a pane spawned with RemainOnExit leaks for
	// the host's lifetime with no row naming it for any later sweep to find.
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()

	spawner := &ctxRecordingSpawner{}
	killSpawnedChatPaneBestEffort(callerCtx, spawner, zerolog.Nop(), "chat-1", "boss-chat-1")

	if len(spawner.names) != 1 {
		t.Fatalf("KillSession calls = %d, want 1 — the orphaned pane is invisible to every cleanup path", len(spawner.names))
	}
	if spawner.ctxErr != nil {
		t.Errorf("the rollback kill was issued on a dead context (%v), so tmux never ran it and the pane leaks", spawner.ctxErr)
	}
	if spawner.deadline.IsZero() {
		t.Fatal("the rollback kill ran on a context with NO deadline; a detached cleanup must not outlive the process")
	}
	if remaining := time.Until(spawner.deadline); remaining > detach.CleanupBudget+time.Second || remaining < detach.CleanupBudget-time.Second {
		t.Errorf("rollback deadline is %s away, want %s ± 1s — the kill is not on the shared cleanup budget",
			remaining, detach.CleanupBudget)
	}
}
