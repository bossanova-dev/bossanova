package session

import (
	"context"

	"github.com/recurser/bossalib/machine"
)

// DependabotRepairInFlight reports a live repair or plan on a Dependabot branch.
// A stale display flag alone never holds the slot after its chat has died.
func (l *Lifecycle) DependabotRepairInFlight(ctx context.Context, repoID string) (sessionID string, busy bool) {
	if l.sessions == nil {
		return "", false
	}
	sessions, err := l.sessions.List(ctx, repoID)
	if err != nil {
		l.logger.Warn().Err(err).Str("repo_id", repoID).Msg("Dependabot busy check failed; deferring automated start")
		return "", true
	}
	for _, s := range sessions {
		if s == nil || s.ArchivedAt != nil || !IsDependabotBranch(s.BranchName) {
			continue
		}
		repairing := false
		if l.alertClear.display != nil {
			if d := l.alertClear.display.Get(s.ID); d != nil {
				repairing = d.IsRepairing
			}
		}
		if (repairing || s.State == machine.ImplementingPlan) && l.hasLiveChatPane(ctx, s.ID) {
			return s.ID, true
		}
	}
	return "", false
}
