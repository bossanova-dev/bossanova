package session

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

// startCronTmuxChat boots a cron-spawned Claude run inside a tmux session
// instead of going through the headless --print path that StartSession uses
// for interactive sessions.
//
// Steps, all of which must succeed for the call to return cleanly:
//
//  1. Verify tmux is wired up and available on the host.
//  2. Mint a fresh Claude session UUID and resolve the per-chat tmux name.
//  3. Spawn a detached tmux session running `claude --session-id <uuid>`
//     (plus --dangerously-skip-permissions when configured) in the
//     worktree path.
//  4. Persist a claude_chats row titled `Run "<cron name>"`, then stamp the
//     resolved tmux session name onto it.
//  5. Inject the session.Plan into the tmux session as a bracketed paste
//     (see Client.SendPlan).
//
// On failure at any step after tmux NewSession succeeds, the helper kills
// the tmux session and (where applicable) deletes the chat row so a
// retried fire doesn't pile up orphaned tmux sessions or DB rows. The
// scheduler's outer markFireFailed path turns the returned error into the
// fire_failed cron outcome.
func (l *Lifecycle) startCronTmuxChat(
	ctx context.Context,
	sessionID string,
	_ StartSessionOpts,
	session *models.Session,
	result *gitpkg.CreateResult,
) (string, error) {
	// Step 1: tmux must be available; otherwise there's nowhere to host the run.
	if l.tmux == nil || !l.tmux.Available(ctx) {
		return "", fmt.Errorf("tmux unavailable: cannot host cron-spawned session %s", sessionID)
	}

	if l.claudeChats == nil {
		// A nil ClaudeChatStore would leave the tmux session orphaned with no
		// matching DB row, which the boss UI cannot recover. Reject early so
		// the cron run is reported as fire_failed rather than silently leaking.
		return "", fmt.Errorf("claudeChats store unavailable: cannot host cron-spawned session %s", sessionID)
	}

	// Step 2: mint a fresh Claude session UUID and derive the tmux name.
	claudeID := uuid.NewString()
	tmuxName := tmux.ChatSessionName(session.RepoID, claudeID)

	// Step 3: build the claude command line and spawn the tmux session.
	args := []string{"claude", "--session-id", claudeID}
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		// Best-effort: fall back to default config and log. Without this,
		// a load failure would silently strip --dangerously-skip-permissions
		// from cron runs even when the operator has enabled it.
		l.logger.Warn().Err(cfgErr).
			Str("session", sessionID).
			Msg("cron: failed to load config; defaulting (DangerouslySkipPermissions=false)")
	}
	if cfg.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	if err := l.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    tmuxName,
		WorkDir: result.WorktreePath,
		Command: args,
	}); err != nil {
		return "", fmt.Errorf("create tmux session %q: %w", tmuxName, err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeID", claudeID).
		Str("tmuxSession", tmuxName).
		Str("worktree", result.WorktreePath).
		Msg("cron: started claude inside tmux")

	// Step 4: persist the claude_chats row, then stamp the tmux name on it.
	// On failure tear down the tmux session so we don't leave a process
	// running with no DB row pointing to it.
	cronName := session.Title
	if _, err := l.claudeChats.Create(ctx, db.CreateClaudeChatParams{
		SessionID: sessionID,
		ClaudeID:  claudeID,
		Title:     `Run "` + cronName + `"`,
	}); err != nil {
		l.killCronTmuxBestEffort(ctx, sessionID, claudeID, tmuxName)
		return "", fmt.Errorf("create claude_chats row for cron session %s: %w", sessionID, err)
	}

	if err := l.claudeChats.UpdateTmuxSessionName(ctx, claudeID, &tmuxName); err != nil {
		l.killCronTmuxBestEffort(ctx, sessionID, claudeID, tmuxName)
		if delErr := l.claudeChats.DeleteByClaudeID(ctx, claudeID); delErr != nil {
			l.logger.Warn().Err(delErr).
				Str("claudeID", claudeID).
				Msg("cron: failed to delete chat row after tmux name persist failure")
		}
		return "", fmt.Errorf("persist tmux session name for cron session %s: %w", sessionID, err)
	}

	// Step 5: inject the plan into the tmux session as a bracketed paste.
	if err := l.tmux.SendPlan(ctx, tmuxName, session.Plan); err != nil {
		l.killCronTmuxBestEffort(ctx, sessionID, claudeID, tmuxName)
		if delErr := l.claudeChats.DeleteByClaudeID(ctx, claudeID); delErr != nil {
			l.logger.Warn().Err(delErr).
				Str("claudeID", claudeID).
				Msg("cron: failed to delete chat row after SendPlan failure")
		}
		return "", fmt.Errorf("send plan to tmux session %q: %w", tmuxName, err)
	}

	return claudeID, nil
}

// killCronTmuxBestEffort tears down the tmux session created during a failed
// startCronTmuxChat. Errors are logged but never returned — the caller is
// already on a failure path and the cron outcome will be fire_failed regardless.
func (l *Lifecycle) killCronTmuxBestEffort(ctx context.Context, sessionID, claudeID, tmuxName string) {
	if l.tmux == nil {
		return
	}
	if err := l.tmux.KillSession(ctx, tmuxName); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("claudeID", claudeID).
			Str("tmuxSession", tmuxName).
			Msg("cron: failed to kill tmux session during failure cleanup")
	}
}
