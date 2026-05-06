package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

// fakeTmuxClient lets us assert which Command was passed without exec'ing tmux.
// Shared between spawn_chat_tmux_test.go and wake_chat_test.go. The mutex
// makes captured / createdN / hasSession reads/writes race-safe so the
// singleflight test in wake_chat_test.go can hit it from many goroutines.
type fakeTmuxClient struct {
	mu         sync.Mutex
	available  bool
	hasSession bool
	captured   []string
	createErr  error
	createdN   int
	// slowCreate, when true, sleeps briefly inside NewSessionWithCmd so
	// concurrent goroutines actually contend on singleflight.Do.
	slowCreate bool
}

func (f *fakeTmuxClient) Available(_ context.Context) bool { return f.available }
func (f *fakeTmuxClient) HasSession(_ context.Context, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasSession
}
func (f *fakeTmuxClient) NewSessionWithCmd(_ context.Context, _, _ string, cmd []string) error {
	if f.slowCreate {
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append([]string{}, cmd...)
	if f.createErr != nil {
		return f.createErr
	}
	f.createdN++
	f.hasSession = true
	return nil
}

// fakeTranscriptOracle controls TranscriptExists for tests.
type fakeTranscriptOracle struct{ exists bool }

func (f fakeTranscriptOracle) TranscriptExists(_, _ string) bool { return f.exists }

func newTestChat(t *testing.T) *models.AgentChat {
	t.Helper()
	return &models.AgentChat{
		ID:             "chat-id",
		SessionID:      "sess-id",
		AgentSessionID: "agent-session-1",
		AgentName:      "claude",
	}
}

func TestSpawnChatTmux_AlreadyLive(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: true},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive", got)
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("expected no NewSession call, got %d", tmuxer.createdN)
	}
}

func TestSpawnChatTmux_FreshStart_NoResumeFlag(t *testing.T) {
	wd := t.TempDir()
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	chat := newTestChat(t)
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: false},
	}, spawnInput{
		Chat:         chat,
		WorktreePath: wd,
		TmuxName:     "boss-aaa-bbb",
		ForceFresh:   false,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got)
	}
	if !contains(tmuxer.captured, "--session-id") || contains(tmuxer.captured, "--resume") {
		t.Fatalf("expected --session-id only, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_ResumeWhenTranscriptExists(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	wd := t.TempDir()
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: true},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: wd,
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != OutcomeResumed {
		t.Fatalf("got %v, want OutcomeResumed", got)
	}
	if !contains(tmuxer.captured, "--resume") || contains(tmuxer.captured, "--session-id") {
		t.Fatalf("expected --resume only, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_ForceFreshOverridesTranscript(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: true},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
		ForceFresh:   true,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != OutcomeFreshFallback {
		t.Fatalf("got %v, want OutcomeFreshFallback", got)
	}
	if contains(tmuxer.captured, "--resume") {
		t.Fatalf("force_fresh should suppress --resume, got cmd=%v", tmuxer.captured)
	}
}

func TestSpawnChatTmux_WorktreeMissing(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: true, hasSession: false}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_ = os.RemoveAll(missing)
	_, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: false},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: missing,
		TmuxName:     "boss-aaa-bbb",
	})
	if err == nil {
		t.Fatalf("expected ErrWorktreeMissing, got nil")
	}
	if err != ErrWorktreeMissing {
		t.Fatalf("got %v, want ErrWorktreeMissing", err)
	}
	if tmuxer.createdN != 0 {
		t.Fatalf("worktree-missing must not spawn tmux, got createdN=%d", tmuxer.createdN)
	}
}

func TestSpawnChatTmux_TmuxUnavailable(t *testing.T) {
	tmuxer := &fakeTmuxClient{available: false}
	got, err := spawnChatTmux(context.Background(), spawnDeps{
		Tmux:        tmuxer,
		Transcripts: fakeTranscriptOracle{exists: false},
	}, spawnInput{
		Chat:         newTestChat(t),
		WorktreePath: t.TempDir(),
		TmuxName:     "boss-aaa-bbb",
	})
	if err != nil {
		t.Fatalf("unavailable tmux must not error, got %v", err)
	}
	if got != OutcomeAlreadyLive {
		t.Fatalf("got %v, want OutcomeAlreadyLive (no-op)", got)
	}
}

func contains(s []string, want string) bool {
	for _, x := range s {
		if x == want {
			return true
		}
	}
	return false
}
