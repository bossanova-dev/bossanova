package main

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
)

// sharedUsageStore is the account store a shared usage reading is written to and
// reconciled against.
type sharedUsageStore interface {
	usageSnapshotRecorder
	accountCooldownClearer
}

// sharedUsageCooldownRecorder records a usage reading another daemon probed and
// bosso relayed, then clears a local cooldown that reading contradicts.
//
// Without it, a non-poller daemon could never clear a cooldown: the shared
// reading keeps its usage fresh, so ShouldProbe skips the local probe, and the
// local probe was the only path into reconcileContradictedCooldown. A reset
// weekly limit then stayed benched until the old reset time.
//
// A relayed reading is not structurally fresh the way a local probe is, so it
// must be younger than the staleness window before it may clear a bench.
type sharedUsageCooldownRecorder struct {
	store     sharedUsageStore
	logger    zerolog.Logger
	staleness time.Duration
	now       func() time.Time
}

func (r sharedUsageCooldownRecorder) RecordUsageProbe(ctx context.Context, accountID string, snap domain.UsageSnapshot) error {
	if err := r.store.RecordUsageProbe(ctx, accountID, snap); err != nil {
		return err
	}
	now := r.now()
	if snap.FetchedAt == nil || now.Sub(*snap.FetchedAt) >= r.staleness {
		return nil
	}
	acct, err := r.store.Get(ctx, accountID)
	if err != nil || acct == nil || acct.CooldownUntil == nil || !acct.CooldownUntil.After(now) {
		return nil
	}
	reconcileContradictedCooldown(ctx, r.logger, r.store, acct, snap)
	return nil
}
