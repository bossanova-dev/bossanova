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
	return UsageSnapshotRateLimited(snap) || snap.Util5h >= 1 || snap.Util7d >= 1
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
	case UsageSnapshotRateLimited(snap):
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
