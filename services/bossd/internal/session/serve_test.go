package session

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type lifecycleServeRecorder struct {
	stopped []string
	err     error
}

func (r *lifecycleServeRecorder) Stop(_ context.Context, id string) error {
	r.stopped = append(r.stopped, id)
	return r.err
}

func TestChatCleanupContinuesAfterServeStopError(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	name := "boss-aaaaaaaa-bbbbbbbb"
	h.chats.chatsBySession = map[string][]*domain.AgentChat{"sess-1": {{AgentSessionID: "chat-1", TmuxSessionName: &name}}}
	stopErr := errors.New("remove service report: permission denied")
	h.lc.SetServeStopper(&lifecycleServeRecorder{err: stopErr})
	if err := h.lc.killAllChatTmuxSessions(t.Context(), "sess-1"); !errors.Is(err, stopErr) {
		t.Fatalf("cleanup error = %v, want serve failure", err)
	}
	if h.findCall("kill-session") == nil || len(h.chats.tmuxNameUpdates) != 1 || h.chats.tmuxNameUpdates[0].name != nil {
		t.Fatal("serve failure skipped chat kill and pointer cleanup")
	}
}

func TestArchiveSessionKeepsChatCleanupBestEffort(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	h.chats.listBySessionErr = errors.New("chat listing unavailable")
	serve := &lifecycleServeRecorder{}
	h.lc.SetServeStopper(serve)
	if err := h.lc.ArchiveSession(t.Context(), "sess-1"); err != nil {
		t.Fatalf("archive after chat cleanup failure: %v", err)
	}
	if len(h.sessions.archived()) != 1 || len(serve.stopped) != 1 {
		t.Fatalf("archive did not finish with one serve stop: stopped=%v", serve.stopped)
	}
}

func TestArchiveSessionKeepsWorktreeWhenServeStopFails(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	stopErr := errors.New("serve process is still running")
	h.lc.SetServeStopper(&lifecycleServeRecorder{err: stopErr})
	if err := h.lc.ArchiveSession(t.Context(), "sess-1"); !errors.Is(err, stopErr) {
		t.Fatalf("archive error=%v, want serve failure", err)
	}
	if len(h.sessions.archived()) != 0 || len(h.wt.archived) != 0 {
		t.Fatal("archive discarded a worktree with live services")
	}
}

func TestArchiveSessionStopsServe_Sessions_AC250701(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo"}
	sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", State: machine.ImplementingPlan}
	lc := newTestLifecycle(sessions, repos, nil, nil, &mockWorktreeManager{}, newMockAgentRunner(), nil, newMockVCSProvider(), zerolog.Nop())
	serve := &lifecycleServeRecorder{}
	lc.SetServeStopper(serve)
	if err := lc.ArchiveSession(context.Background(), "sess-1"); err != nil {
		t.Fatal(err)
	}
	if len(serve.stopped) != 1 || serve.stopped[0] != "sess-1" {
		t.Fatalf("stopped=%v", serve.stopped)
	}
}
