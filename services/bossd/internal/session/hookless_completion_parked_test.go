package session

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/tmux"
)

// TestArmTmuxCompletionForHooklessTmux_ClearedPanePointer pins BOS-884: the
// hookless-completion poller infers "the pane is gone, therefore the agent
// exited" from a missing tmux session. That inference is only sound while the
// chat still carries its pane pointer. Every deliberate teardown
// (KillChatTmuxSession) kills the pane and then clears
// agent_chats.tmux_session_name, so a cleared pointer means the pane was reaped
// on purpose and the run must NOT be finalized. A pointer that is still set
// with the pane missing is a genuine death and must still finalize.
//
// The poller captures the pane name at arm time, so it cannot observe a later
// clear from that closure — it has to re-read the chat row. Both cases below
// therefore run the same arm call and differ only in the persisted row.
func TestArmTmuxCompletionForHooklessTmux_ClearedPanePointer(t *testing.T) {
	tmuxName := "boss-repo-1-run-1"

	tests := []struct {
		name string
		// paneName is the chat row's persisted tmux_session_name. nil models a
		// deliberate teardown that already cleared the pointer.
		paneName   *string
		wantSignal bool
	}{
		{
			name:       "cleared pointer is a deliberate teardown, not a completion",
			paneName:   nil,
			wantSignal: false,
		},
		{
			name:       "pointer still set is a genuine death and still finalizes",
			paneName:   &tmuxName,
			wantSignal: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			cronID := "cron-1"
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				Title:        "Reaped chat",
				WorktreePath: "/tmp/wt",
				BaseBranch:   "main",
				State:        machine.ImplementingPlan,
				AgentName:    "codex",
				CronJobID:    &cronID,
			}

			chats := &mockAgentChatStore{
				chatsBySession: map[string][]*models.AgentChat{
					"sess-1": {
						{
							ID:              "chat-1",
							SessionID:       "sess-1",
							AgentSessionID:  "run-1",
							AgentName:       "codex",
							TmuxSessionName: tt.paneName,
						},
					},
				},
			}

			// has-session fails the way real tmux does for a pane that is gone:
			// exit 1 with "can't find session". That is a clean "not alive"
			// answer, not a probe error.
			tmuxFake := newFakeTmux()
			tmuxFake.failSubcommand["has-session"] = true
			tmuxFake.failStderr["has-session"] = "can't find session"
			tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

			notifier := &recordingCronCompletionNotifier{}
			lc := newTestLifecycle(sessions, newMockRepoStore(), chats, &stubCronJobStore{},
				&mockWorktreeManager{}, newMockAgentRunner(), tx, newMockVCSProvider(), zerolog.Nop())
			lc.SetCronCompletionNotifier(notifier)
			lc.SetDaemonCtx(ctx)
			lc.tmuxCompletionPollInterval = time.Millisecond

			lc.armTmuxCompletionForHooklessTmux("sess-1", "run-1", tmuxName)

			if tt.wantSignal {
				waitForCount(t, "NotifyCronAgentStopped", notifier.count)
				if got := notifier.callsCopy()[0]; got != "sess-1" {
					t.Fatalf("NotifyCronAgentStopped called with %q, want sess-1", got)
				}
				return
			}

			// Negative case: give the poller far more than its 1ms cadence to
			// misbehave, then prove it actually probed tmux (so a silent
			// never-started poller cannot pass this test vacuously).
			waitForNoCount(t, "NotifyCronAgentStopped", notifier.count, 200*time.Millisecond)
			if !tmuxFake.hasSubcommand("has-session") {
				t.Fatal("poller never probed tmux has-session; the assertion above proved nothing")
			}
		})
	}
}

// waitForNoCount asserts count stays at zero for the whole window. It is the
// mirror of waitForCount: used where the contract is that a signal must NOT
// fire, so the test has to spend real time rather than sample once.
func waitForNoCount(t *testing.T, name string, count func() int, window time.Duration) {
	t.Helper()

	deadline := time.NewTimer(window)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if got := count(); got != 0 {
			t.Fatalf("%s count = %d, want 0", name, got)
		}
		select {
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

// TestLoglessTmuxCompletionEvidence_ClearedPanePointer pins the precedent this
// ticket generalizes: loglessTmuxCompletionEvidence already skips a chat whose
// tmux_session_name is NULL rather than counting its missing pane as completion
// evidence. It had no test, so a refactor could have quietly reintroduced the
// bad inference. A cleared-pointer-only session yields no candidate pane and so
// defers finalization; a session whose pointer is still set and whose pane is
// gone is genuine completion evidence.
func TestLoglessTmuxCompletionEvidence_ClearedPanePointer(t *testing.T) {
	tmuxName := "boss-repo-1-run-1"

	tests := []struct {
		name       string
		paneName   *string
		wantOver   bool
		wantReason string
	}{
		{
			name:       "cleared pointer is not completion evidence",
			paneName:   nil,
			wantOver:   false,
			wantReason: "tmux_pane_unknown",
		},
		{
			name:       "pointer set with a missing pane is completion evidence",
			paneName:   &tmuxName,
			wantOver:   true,
			wantReason: "tmux_pane_missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentID := "run-1"
			cronID := "cron-1"
			sess := &models.Session{
				ID:             "sess-1",
				RepoID:         "repo-1",
				State:          machine.ImplementingPlan,
				AgentName:      "codex",
				AgentSessionID: &agentID,
				CronJobID:      &cronID,
			}

			chats := &mockAgentChatStore{
				chatsBySession: map[string][]*models.AgentChat{
					"sess-1": {
						{
							ID:              "chat-1",
							SessionID:       "sess-1",
							AgentSessionID:  "run-1",
							AgentName:       "codex",
							TmuxSessionName: tt.paneName,
						},
					},
				},
			}

			tmuxFake := newFakeTmux()
			tmuxFake.failSubcommand["has-session"] = true
			tmuxFake.failStderr["has-session"] = "can't find session"
			tx := tmux.NewClient(tmux.WithCommandFactory(tmuxFake.factory))

			lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), chats, &stubCronJobStore{},
				&mockWorktreeManager{}, newMockAgentRunner(), tx, newMockVCSProvider(), zerolog.Nop())

			over, reason := lc.loglessTmuxCompletionEvidence(sess)
			if over != tt.wantOver || reason != tt.wantReason {
				t.Fatalf("loglessTmuxCompletionEvidence = (%v, %q), want (%v, %q)",
					over, reason, tt.wantOver, tt.wantReason)
			}
		})
	}
}
