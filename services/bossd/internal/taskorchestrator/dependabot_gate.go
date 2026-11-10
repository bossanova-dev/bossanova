package taskorchestrator

import (
	"context"
	"slices"
	"strings"
)

// DependabotRepairGate reports repository-wide repair activity across start paths.
type DependabotRepairGate interface {
	DependabotRepairInFlight(ctx context.Context, repoID string) (sessionID string, busy bool)
}

// SetDependabotRepairGate supplies the optional restart-safe start gate.
func (o *Orchestrator) SetDependabotRepairGate(g DependabotRepairGate) { o.dependabotGate = g }

func (o *Orchestrator) deferDependabotTask(ctx context.Context, repo repoInfo, externalID string, labels []string) bool {
	if o.dependabotGate == nil || !strings.HasPrefix(externalID, "dependabot:") && !slices.Contains(labels, "dependabot") {
		return false
	}
	holder, busy := o.dependabotGate.DependabotRepairInFlight(ctx, repo.id)
	if busy {
		o.logger.Info().Str("repo_id", repo.id).Str("holder", holder).Str("external_id", externalID).Msg("Dependabot repo busy, deferring task")
	}
	return busy
}
