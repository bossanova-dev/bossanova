package session

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

// The four best-effort cleanups BOS-952 routed through detach.Cleanup in this
// package (not every detached cleanup here — see idleReapPointerRestoreTimeout
// in tmux_chat.go) all run on a context DETACHED from the caller's and bounded
// by detach.CleanupBudget. Both halves matter and neither is visible from the
// call list alone:
//
//   - detached, because the commonest way to REACH any of them is the caller's
//     own budget expiring, and on a dead context the tmux kill never starts and
//     the store refuses the write — both swallowed by design, leaving a leaked
//     pane and a row that still reads live.
//   - bounded, because a detached cleanup with no deadline of its own can
//     outlive the failure it is recording.
//
// Every test below therefore drives the helper with an ALREADY-CANCELLED caller
// context and asserts on the context the collaborator actually received, not
// merely that it was called.

// deadlineTolerance absorbs the scheduling jitter between the helper deriving
// its context and the collaborator reading the deadline back.
const deadlineTolerance = time.Second

// deadlineWithin fails unless d sits within deadlineTolerance of the shared
// cleanup budget, measured from now.
func deadlineWithin(t *testing.T, what string, d time.Time) {
	t.Helper()
	if d.IsZero() {
		t.Fatalf("%s ran on a context with NO deadline; a detached cleanup must not be able to outlive the process", what)
	}
	remaining := time.Until(d)
	if remaining > detach.CleanupBudget+deadlineTolerance || remaining < detach.CleanupBudget-deadlineTolerance {
		t.Errorf("%s deadline is %s away, want %s ± %s — the cleanup is not on the shared cleanup budget",
			what, remaining, detach.CleanupBudget, deadlineTolerance)
	}
}

// deadCallerCtx returns a context that has already been cancelled, which is the
// state every one of these helpers is most likely to be called in.
func deadCallerCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestKillTmuxChatBestEffortRunsOnADetachedBudgetedContext(t *testing.T) {
	t.Parallel()
	tmuxFake := newFakeTmux()
	l := &Lifecycle{
		tmux:   tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory)),
		logger: zerolog.Nop(),
	}

	l.killTmuxChatBestEffort(deadCallerCtx(t), "sess-1", "chat-1", "boss-chat-1")

	kill := tmuxFake.lastCall("kill-session")
	if kill == nil {
		t.Fatal("no kill-session was issued; the leaked pane spawns with RemainOnExit and never self-reaps")
	}
	if kill.ctxErr != nil {
		t.Errorf("the cleanup kill was issued on a dead context (%v), so tmux never ran it and the pane leaks", kill.ctxErr)
	}
	deadlineWithin(t, "the cleanup kill", kill.deadline)
}

func TestFailStartBestEffortStampsOnADetachedBudgetedContext(t *testing.T) {
	t.Parallel()
	tmuxFake := newFakeTmux()
	chats := &mockAgentChatStore{}
	l := &Lifecycle{
		tmux:       tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory)),
		agentChats: chats,
		logger:     zerolog.Nop(),
	}

	l.failStartBestEffort(deadCallerCtx(t), "sess-1", "chat-1", "boss-chat-1", "spawn failed")

	// Both halves are detached independently; assert on each.
	kill := tmuxFake.lastCall("kill-session")
	if kill == nil {
		t.Fatal("failStartBestEffort issued no kill-session")
	}
	if kill.ctxErr != nil {
		t.Errorf("failStartBestEffort's kill was issued on a dead context (%v)", kill.ctxErr)
	}
	deadlineWithin(t, "failStartBestEffort's kill", kill.deadline)

	calls := chats.markStartFailedCalls
	if len(calls) != 1 {
		t.Fatalf("MarkStartFailed calls = %d, want 1 — the row keeps reading live with its pane gone", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("failStartBestEffort's stamp wrote on a dead context (%v); the store refuses it and the row stays live", calls[0].ctxErr)
	}
	deadlineWithin(t, "failStartBestEffort's stamp", calls[0].deadline)
}

func TestRecordFailedStartChatWritesOnADetachedBudgetedContext(t *testing.T) {
	t.Parallel()
	// One derived context covers the whole lookup/create/stamp trio, so the
	// stamp's recorded context IS the context the lookup and the create ran on.
	// Undetached, the lookup is the first thing to fail and the function returns
	// having written nothing at all — which this assertion catches, because the
	// stamp is then never recorded.
	chats := &mockAgentChatStore{}
	l := &Lifecycle{agentChats: chats, logger: zerolog.Nop()}

	l.recordFailedStartChat(deadCallerCtx(t), "sess-1", "chat-1", "claude", "Title", "tmux launch failed")

	calls := chats.markStartFailedCalls
	if len(calls) != 1 {
		t.Fatalf("MarkStartFailed calls = %d, want 1 — the tmux launch failure would be invisible", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("recordFailedStartChat wrote on a dead context (%v)", calls[0].ctxErr)
	}
	deadlineWithin(t, "recordFailedStartChat's stamp", calls[0].deadline)
}

func TestStampSwitchStartErrorWritesOnADetachedBudgetedContext(t *testing.T) {
	t.Parallel()
	chats := &mockAgentChatStore{}
	l := &Lifecycle{agentChats: chats, logger: zerolog.Nop()}

	l.stampSwitchStartError(deadCallerCtx(t), "chat-1", "respawn after switch failed")

	calls := chats.markStartFailedCalls
	if len(calls) != 1 {
		t.Fatalf("MarkStartFailed calls = %d, want 1 — a switch that died after STOP would vanish silently", len(calls))
	}
	if calls[0].ctxErr != nil {
		t.Errorf("stampSwitchStartError wrote on a dead context (%v); the row keeps reading live", calls[0].ctxErr)
	}
	deadlineWithin(t, "stampSwitchStartError's stamp", calls[0].deadline)
}
