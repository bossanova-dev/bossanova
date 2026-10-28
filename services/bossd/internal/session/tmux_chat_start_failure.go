package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/contract/plugin/statusdetect"
	"github.com/recurser/bossd/internal/detach"
)

// ClaudeFolderTrustPendingError names a boot dialog while preserving the delivery
// refusal for callers that classify the original sentinel or outcome.
type ClaudeFolderTrustPendingError struct {
	Path  string
	Cause error
}

func (e *ClaudeFolderTrustPendingError) Error() string {
	return fmt.Sprintf("Claude folder-trust prompt pending for %s; boss accepts Claude folder trust only for trusted repos (boss repo update <repo-id> --trusted); otherwise run claude in that path once and choose \"Yes, I trust this folder\": %v", e.Path, e.Cause)
}

func (e *ClaudeFolderTrustPendingError) Unwrap() error { return e.Cause }

type failedStartPane struct {
	text string
	err  error
}

func (l *Lifecycle) captureFailedStartPane(ctx context.Context, tmuxName string) *failedStartPane {
	pane := &failedStartPane{}
	if l.tmux == nil {
		pane.err = fmt.Errorf("tmux client unavailable for failed-start capture")
		return pane
	}
	detach.Cleanup(ctx, detach.CleanupBudget, func(captureCtx context.Context) {
		pane.text, pane.err = l.tmux.CapturePane(captureCtx, tmuxName)
	})
	return pane
}

func (l *Lifecycle) logFailedStartPane(sessionID, agentSessionID, tmuxName, reason string, pane *failedStartPane) {
	l.logger.Warn().Err(pane.err).
		Str("session", sessionID).Str("agentSessionID", agentSessionID).
		Str("tmuxSession", tmuxName).Str("reason", reason).Str("pane", failedStartPaneTail(pane.text)).
		Msg("agent start failed; pane evidence before teardown")
}

func failedStartPaneTail(pane string) string {
	const maxRows = 40
	const maxBytes = 16 * 1024
	rows := strings.Split(pane, "\n")
	tail := make([]string, 0, maxRows)
	for i := len(rows) - 1; i >= 0 && len(tail) < maxRows; i-- {
		if strings.TrimSpace(rows[i]) != "" {
			tail = append(tail, rows[i])
		}
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	text := strings.Join(tail, "\n")
	if len(text) > maxBytes {
		text = text[len(text)-maxBytes:]
	}
	return text
}

func (l *Lifecycle) classifyClaudeStartFailure(ctx context.Context, agentName, workDir, tmuxName string, cause error) (*failedStartPane, error) {
	if agentName != "claude" {
		return nil, cause
	}
	pane := l.captureFailedStartPane(ctx, tmuxName)
	if pane.err == nil && statusdetect.IsFolderTrustPrompt([]byte(pane.text)) {
		return pane, &ClaudeFolderTrustPendingError{Path: workDir, Cause: cause}
	}
	return pane, cause
}
