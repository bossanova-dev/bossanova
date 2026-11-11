package taskorchestrator

import (
	"context"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

// GitHub comment writes have no idempotency key. Persist uncertainty before the
// write so recovery never treats a lost completion receipt as a safe retry.
const rebaseDeliveryUnconfirmed = "bot rebase delivery unconfirmed; automatic repeat suppressed"

func (o *Orchestrator) retainUnconfirmedRebase(ctx context.Context, mapping *domain.TaskMapping, isRebase bool) bool {
	if !isRebase || mapping == nil || mapping.Status != domain.TaskMappingStatusOrphaned || mapping.LastError == nil || *mapping.LastError != rebaseDeliveryUnconfirmed {
		return false
	}
	o.logger.Warn().Str("external_id", mapping.ExternalID).Msg(rebaseDeliveryUnconfirmed)
	status := domain.TaskMappingStatusSkipped
	// Preserve LastError: skipped means delivery is unknown, never completed.
	if _, err := o.taskMappings.Update(ctx, mapping.ID, db.UpdateTaskMappingParams{Status: &status}); err != nil {
		o.logger.Error().Err(err).Str("mapping", mapping.ID).Msg("retain unconfirmed bot rebase failed")
	}
	return true
}
