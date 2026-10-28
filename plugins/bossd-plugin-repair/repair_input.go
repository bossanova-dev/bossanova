package main

import (
	"context"
	"fmt"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// repairInput is what one repair attempt acted on. A second attempt on an
// input already attempted would redo the same work, so the duplicate guards
// skip it. Review feedback is part of a rejected PR's input, and the base
// branch tip part of a conflicted or failing PR's: both conflicts and checks
// can change when the base moves while the head sits still.
type repairInput struct {
	headSHA       string
	displayStatus bossanovav1.DisplayStatus
	reviewFP      string
	reviewFPOk    bool
	baseSHA       string
	baseOk        bool
}

func (cur repairInput) sameHeadAndStatus(prev repairInput) bool {
	return cur.headSHA != "" && prev.headSHA == cur.headSHA && prev.displayStatus == cur.displayStatus
}

// repeats reports whether cur is an input prev already attempted. A field cur
// could not read cannot show that the input moved, so it never lets a repeat
// through; a prior attempt that recorded no base (made before bases were
// recorded, or whose read failed) cannot show the base stayed put.
func (cur repairInput) repeats(prev repairInput) bool {
	if !cur.sameHeadAndStatus(prev) {
		return false
	}
	switch cur.displayStatus {
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED:
		return !cur.reviewFPOk || prev.reviewFP == cur.reviewFP
	case bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING:
		return !cur.baseOk || prev.baseSHA == cur.baseSHA
	default:
		return true
	}
}

// buildRepairInput reads the base only when it can unlock a persisted or in-memory
// attempt. A concurrent completion after the snapshot is conservatively treated
// as a repeat by the locked recheck; the next sweep reads the fresh attempt.
func (m *repairMonitor) buildRepairInput(ctx context.Context, sessionID string, info sessionInfo, headSHA string, displayStatus bossanovav1.DisplayStatus) (repairInput, repairInput) {
	reviewFP, reviewFPOk := m.currentReviewFingerprint(ctx, info)
	input := repairInput{headSHA: headSHA, displayStatus: displayStatus, reviewFP: reviewFP, reviewFPOk: reviewFPOk}
	persisted := repairInput{headSHA: info.LastRepairHeadSHA, displayStatus: info.LastRepairDisplayStatus, reviewFP: info.LastRepairReviewFingerprint, baseSHA: info.LastRepairBaseSHA}
	m.mu.Lock()
	inMemory := m.lastAttempt[sessionID]
	m.mu.Unlock()
	if input.sameHeadAndStatus(persisted) || input.sameHeadAndStatus(inMemory) {
		input.baseSHA, input.baseOk = m.currentBaseSHA(ctx, info, displayStatus)
	}
	return input, persisted
}

// noteBaseMoveRetryLocked counts retries differing from an attempt on this head
// and status only by the base tip. Callers hold mu and have ruled out repeats.
func (m *repairMonitor) noteBaseMoveRetryLocked(sessionID string, input repairInput, persistedMove bool) (baseMoveStreak, bool) {
	inMemoryMove := input.sameHeadAndStatus(m.lastAttempt[sessionID]) && attemptBaseSHA(input.displayStatus, input.baseSHA) != ""
	if !persistedMove && !inMemoryMove {
		return baseMoveStreak{}, false
	}
	return m.noteBaseMoveLocked(sessionID, input.headSHA, input.baseSHA)
}

// baseSHAForAttempt records the initial base after the sweep has chosen to run.
func (m *repairMonitor) baseSHAForAttempt(ctx context.Context, info sessionInfo, input repairInput) string {
	if input.baseOk {
		return input.baseSHA
	}
	baseSHA, _ := m.currentBaseSHA(ctx, info, input.displayStatus)
	return baseSHA
}

func baseMoveExhaustedReason(displayStatus bossanovav1.DisplayStatus, headSHA string, count int) string {
	if displayStatus == bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT {
		return fmt.Sprintf("automatic repair left the PR conflicted on %s through %d base branch moves; resolve the conflict by hand", shortSHA(headSHA), count)
	}
	return fmt.Sprintf("automatic repair left the PR failing on %s through %d base branch moves; resolve the failing checks by hand", shortSHA(headSHA), count)
}
