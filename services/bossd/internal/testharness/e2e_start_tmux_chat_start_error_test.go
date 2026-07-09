package testharness_test

// e2e_start_tmux_chat_start_error_test.go — proves that when the daemon's tmux
// `new-session` fails (e.g. a missing-terminfo "missing or unsuitable terminal"
// failure under launchd), StartTmuxChat stamps the captured tmux stderr onto a
// "(failed to start)" agent_chats row instead of only returning/logging it. The
// StartError rides the existing snapshot/stream pipe to boss show, the TUI, and
// external clients, so a daemon-side launch failure is visible rather than
// invisible.

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/testharness"
)

func TestStartTmuxChatStampsStartErrorOnNewSessionFailure(t *testing.T) {
	fake := testharness.NewCronReadyTmuxFake()
	fake.FailNewSession = "missing or unsuitable terminal: xterm-ghostty"
	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: fake.Factory(),
	})
	ctx := context.Background()

	repoID := h.SeedRepo(t, "https://github.com/test/repo.git")
	sess, err := h.Sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repoID,
		Title:        "tmux-launch-fail",
		Plan:         "do the thing",
		WorktreePath: t.TempDir(),
		BranchName:   "start-error-branch",
		BaseBranch:   "main",
		AgentName:    "claude",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, err = h.Lifecycle.StartTmuxChat(ctx, sess.ID,
		session.ChatInput{Prompt: "do the thing"}, "Tmux Fail", session.HookOpts{})
	if err == nil {
		t.Fatal("expected StartTmuxChat to fail when new-session fails")
	}

	chats, err := h.AgentChats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected exactly 1 (failed-to-start) chat row after new-session failure, got %d", len(chats))
	}
	chat := chats[0]
	if chat.StartError == nil || !strings.Contains(*chat.StartError, "missing or unsuitable terminal") {
		t.Fatalf("StartError = %v, want the captured tmux stderr", chat.StartError)
	}
	// The failed-start row must not advertise a live tmux target.
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		t.Errorf("expected cleared tmux_session_name on failed-start row, got %q", *chat.TmuxSessionName)
	}
}
