package rotation

import (
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
)

// UsageSnapshotConfirmsLimited reports whether an authoritative probe confirmed
// the account is actually exhausted. Missing/unfetched/unsupported snapshots
// fail safe: they do not authorize cooling the account.
func UsageSnapshotConfirmsLimited(snap models.UsageSnapshot) bool {
	if snap.FetchedAt == nil {
		return false
	}
	if hasCappedUsage(snap) {
		return true
	}
	// Claude can report the coarse plan status as rate_limited while its
	// simultaneously returned quota windows still show available capacity. The
	// measured windows are the account-limit authority in that contradictory
	// shape; trusting the coarse status would bench the account until an unrelated
	// later reset. Preserve the conservative status-only fallback for providers
	// that do not return a usable utilization measurement.
	return UsageSnapshotRateLimited(snap) && !hasMeasuredSubcapUsage(snap)
}

func hasCappedUsage(snap models.UsageSnapshot) bool {
	return snap.Util5h >= 1 || snap.Util7d >= 1
}

func hasMeasuredSubcapUsage(snap models.UsageSnapshot) bool {
	// Zero-valued protobuf scalars do not preserve field presence, so only treat
	// the persisted shape as contradictory when both windows carry a non-zero
	// measurement. The Claude probe resolves explicit zeroes while it still has
	// the provider payload's presence information.
	return snap.Util5h > 0 && snap.Util5h < 1 && snap.Util7d > 0 && snap.Util7d < 1
}

// UsageSnapshotRateLimited reports whether the normalized status is explicitly
// rate-limited.
func UsageSnapshotRateLimited(snap models.UsageSnapshot) bool {
	switch strings.ToUpper(strings.TrimSpace(snap.Status)) {
	case "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED", "RATE_LIMITED":
		return true
	default:
		return false
	}
}

// UsageSnapshotProbeUnavailable reports whether a probe result explicitly says
// the provider could not authoritatively check usage.
func UsageSnapshotProbeUnavailable(snap models.UsageSnapshot) bool {
	switch strings.ToUpper(strings.TrimSpace(snap.Status)) {
	case "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED", "RATE_LIMIT_PLAN_STATUS_UNSPECIFIED":
		return true
	default:
		return false
	}
}

// UsageSnapshotResetAt returns the reset epoch from the exhausted window. When
// both windows are exhausted or the status is explicitly limited, the later
// reset is the conservative cooldown authority.
func UsageSnapshotResetAt(snap models.UsageSnapshot) *time.Time {
	switch {
	case snap.Util5h >= 1 && snap.Util7d >= 1:
		return laterUsageReset(snap.Reset5h, snap.Reset7d)
	case snap.Util7d >= 1 && snap.Reset7d != nil:
		r := *snap.Reset7d
		return &r
	case snap.Util5h >= 1 && snap.Reset5h != nil:
		r := *snap.Reset5h
		return &r
	case UsageSnapshotRateLimited(snap) && (hasCappedUsage(snap) || !hasMeasuredSubcapUsage(snap)):
		return laterUsageReset(snap.Reset5h, snap.Reset7d)
	default:
		return nil
	}
}

func laterUsageReset(a, b *time.Time) *time.Time {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		r := *b
		return &r
	case b == nil:
		r := *a
		return &r
	case b.After(*a):
		r := *b
		return &r
	default:
		r := *a
		return &r
	}
}
