package credsync

import (
	"context"
	"errors"
)

// resumePendingRotation reuses the exact durable proposal after an interrupted
// upload. A lost response is resolved by reading the committed generation; a
// competing survivor's valid seal may replace the proposal at the same generation.
func (e *Engine) resumePendingRotation(ctx context.Context) error {
	proposal, err := e.trust.PendingRotation()
	if err != nil || proposal == nil {
		return err
	}
	err = e.remote.RollKey(ctx, proposal.ExpectedGeneration, proposal.KeyGeneration, proposal.Seals, proposal.Blobs)
	if err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	devices, err := e.remote.ListDevices(ctx)
	if err != nil {
		return err
	}
	if _, err = e.verifiedDevices(devices); err != nil {
		return err
	}
	generation, sealed, err := e.remote.GetKeySeal(ctx)
	if err != nil {
		return err
	}
	if generation < proposal.KeyGeneration {
		return ErrRollback
	}
	key, err := e.openSignedKeySeal(generation, sealed, devices)
	if err != nil {
		return err
	}
	if err = e.trust.ClearPendingRotation(); err != nil {
		return err
	}
	e.key = key
	e.generation = generation
	for _, d := range devices {
		if d.Status == "revoked" {
			e.observedRevocations[d.DaemonID] = true
		}
	}
	return nil
}
