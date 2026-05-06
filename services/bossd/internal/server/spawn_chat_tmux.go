package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
)

// Outcome describes the result of a spawn attempt.
type Outcome int

const (
	// OutcomeAlreadyLive means the tmux session was already alive (or tmux is
	// unavailable, treated as a no-op). No spawn was attempted.
	OutcomeAlreadyLive Outcome = iota
	// OutcomeResumed means a new tmux session was spawned with `claude --resume`
	// because a transcript for this AgentSessionID was found on disk.
	OutcomeResumed
	// OutcomeFreshFallback means a new tmux session was spawned with
	// `claude --session-id` because either ForceFresh was set or no transcript
	// was found on disk.
	OutcomeFreshFallback
)

// ErrWorktreeMissing means the chat's session worktree directory does not
// exist on disk, so we refuse to spawn (would create a tmux in a deleted
// path). Surfaced to the WakeChat handler as FAILED_PRECONDITION.
var ErrWorktreeMissing = errors.New("worktree directory missing")

// transcriptOracle abstracts TranscriptExists for testability.
type transcriptOracle interface {
	TranscriptExists(workDir, agentSessionID string) bool
}

// tmuxSpawner is the narrow surface of *tmux.Client used by spawnChatTmux.
type tmuxSpawner interface {
	Available(ctx context.Context) bool
	HasSession(ctx context.Context, name string) bool
	NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string) error
}

// spawnDeps groups the abstractions spawnChatTmux needs.
type spawnDeps struct {
	Tmux        tmuxSpawner
	Transcripts transcriptOracle
}

// spawnInput captures the per-chat parameters for a spawn attempt.
type spawnInput struct {
	Chat         *models.AgentChat
	WorktreePath string
	TmuxName     string
	ForceFresh   bool
}

// spawnChatTmux is the single source of truth for "ensure a tmux+claude
// exists for this chat". Used by ensureChatTmuxSession (start path) and
// (eventually) WakeChat (revive path). Idempotent: returns OutcomeAlreadyLive
// without spawning when the named tmux is already alive or tmux is
// unavailable.
//
// When a spawn is required, resume vs. fresh is decided by a transcript
// pre-flight: if ForceFresh is set, always fresh; otherwise consult the
// transcript oracle. This avoids invoking `claude --resume` against a
// session whose JSONL transcript is missing (which would fail).
func spawnChatTmux(ctx context.Context, deps spawnDeps, in spawnInput) (Outcome, error) {
	if deps.Tmux == nil || !deps.Tmux.Available(ctx) {
		return OutcomeAlreadyLive, nil
	}

	if deps.Tmux.HasSession(ctx, in.TmuxName) {
		return OutcomeAlreadyLive, nil
	}

	if _, err := os.Stat(in.WorktreePath); err != nil {
		if os.IsNotExist(err) {
			return 0, ErrWorktreeMissing
		}
		return 0, fmt.Errorf("stat worktree: %w", err)
	}

	resume := !in.ForceFresh && deps.Transcripts.TranscriptExists(in.WorktreePath, in.Chat.AgentSessionID)

	args := []string{"claude"}
	if resume {
		args = append(args, "--resume", in.Chat.AgentSessionID)
	} else {
		args = append(args, "--session-id", in.Chat.AgentSessionID)
	}
	cfg, _ := config.Load()
	if config.PluginConfigBool(&cfg, "claude", "dangerously_skip_permissions") {
		args = append(args, "--dangerously-skip-permissions")
	}

	if err := deps.Tmux.NewSessionWithCmd(ctx, in.TmuxName, in.WorktreePath, args); err != nil {
		return 0, fmt.Errorf("new tmux session: %w", err)
	}

	if resume {
		return OutcomeResumed, nil
	}
	return OutcomeFreshFallback, nil
}

// liveTmuxSpawner adapts *tmux.Client to the tmuxSpawner interface.
type liveTmuxSpawner struct{ c *tmux.Client }

// Available reports whether tmux is installed on the host.
func (l liveTmuxSpawner) Available(ctx context.Context) bool { return l.c.Available(ctx) }

// HasSession reports whether the named tmux session is currently alive.
func (l liveTmuxSpawner) HasSession(ctx context.Context, name string) bool {
	return l.c.HasSession(ctx, name)
}

// NewSessionWithCmd creates a detached tmux session running the given command.
func (l liveTmuxSpawner) NewSessionWithCmd(ctx context.Context, name, workDir string, cmd []string) error {
	return l.c.NewSession(ctx, tmux.NewSessionOpts{Name: name, WorkDir: workDir, Command: cmd})
}

// liveTranscriptOracle delegates to status.TranscriptExists.
type liveTranscriptOracle struct{}

// TranscriptExists reports whether the Claude Code JSONL transcript for the
// given (worktree, agentSessionID) is present on disk.
func (liveTranscriptOracle) TranscriptExists(workDir, agentSessionID string) bool {
	return status.TranscriptExists(workDir, agentSessionID)
}
