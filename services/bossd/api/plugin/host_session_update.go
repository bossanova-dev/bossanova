package plugin

import (
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/domain"
)

func firedSessionUpdate(previous machine.State, sm *machine.Machine) domain.UpdateSessionParams {
	state := int(sm.State())
	count := sm.Context().AttemptCount
	update := domain.UpdateSessionParams{State: &state, AttemptCount: &count}
	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	} else if previous == machine.Blocked {
		var cleared *string
		update.BlockedReason = &cleared
		update.LastAttemptHeadSHA = &cleared
	}
	return update
}
