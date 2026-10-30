package credmaterialize

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
)

// persistBack re-hashes auth.json under the account lock and, only when it
// changed since the last materialize/persist, merges it over the CURRENT stored
// credential and saves it back exactly once. It reloads the store's blob under
// the lock as the merge baseline — rather than a materialize-time snapshot — so
// that a concurrent session's already-persisted refresh is what this merge
// overlays. That is what keeps the newest id_token from being rolled back to an
// older one when two sessions share an account (last-writer-wins under the lock,
// but never a stale-baseline regression). On success it advances *recorded so a
// subsequent no-change call is a no-op.
//
// The read, the merge/save and the durable hash record are one transaction under
// the shared credential lock, exactly as in MaterializeCodex. m.locks cannot
// stand in for that lock here: production builds a Materializer per account
// source (services/bossd/cmd/main.go), so two of them share no local lock map and
// only the store's lock orders them. Were the record to land after the lock was
// released, it could overwrite a peer materializer's record of its own newer
// auth.json, leaving the sidecar describing bytes that are no longer on disk —
// and the next reconcile would then read that peer's write as an agent-side
// rotation and fold stale tokens over the fresher stored credential.
func (m *Materializer) persistBack(ctx context.Context, accountID, authPath string, recorded *[32]byte) error {
	unlock := m.locks.Lock(lockKey(providerCodex, accountID))
	defer unlock()

	persist := func() error {
		// #nosec G304 -- reads codex `auth.json` under the account lock; internally-derived materialize path (secret-adjacent)
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		current, err := os.ReadFile(authPath)
		if err != nil {
			return fmt.Errorf("read codex auth.json at %q for persist-back: %w", authPath, err)
		}
		currentHash := sha256.Sum256(current)
		if currentHash == *recorded {
			// Unchanged gate: nothing to save.
			return nil
		}

		if _, err := m.mergeAndSaveRefreshedLocked(ctx, accountID, current); err != nil {
			return err
		}
		// Advance the recorded hash to the on-disk bytes we just persisted, so a
		// subsequent PersistBack with no further agent-side change re-hashes
		// auth.json to the same value and no-ops (idempotent). Record it durably too:
		// the store has now absorbed exactly these bytes, so the next materialization
		// must not fold them in again.
		*recorded = currentHash
		m.recordAuthWrite(authPath, currentHash)
		return nil
	}
	if locker, ok := m.store.(CredentialStoreLocker); ok {
		return locker.WithCredentialLock(accountID, persist)
	}
	return persist()
}
