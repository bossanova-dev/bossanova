package github

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
)

// Background floor tiers. A read marked vcs.WithReserveQuotaReads feeds merge
// decisions and may spend down to the reserve floor; any other background read
// (vcs.WithCachedReads) stops at the standard floor.
const (
	floorTierStandard = "standard"
	floorTierReserve  = "reserve"
)

// backgroundFloors holds the daemon's own background GitHub reads back while
// the quota they spend is below a floor, so the share of each quota the crons
// leave untouched stays available to verify and finalize. Foreground calls and
// every write are never held back.
type backgroundFloors struct {
	standard float64
	reserve  float64

	mu sync.Mutex
	// refusing records the quota and tier pairs whose background reads are
	// being refused, so the start and the end of a refusal log once each.
	refusing map[floorKey]bool
	// probeFailedAt is when the gate last failed to read the quotas. Inside
	// quotaReadingTTL after it the gate fails open without probing again.
	probeFailedAt time.Time
}

type floorKey struct{ quota, tier string }

// WithBackgroundQuotaFloors holds background reads back below the given share
// of the quota they spend: standard for vcs.WithCachedReads reads, reserve for
// vcs.WithReserveQuotaReads reads. A floor of zero or less disables its tier,
// so a Provider built without this option never holds a read back.
func WithBackgroundQuotaFloors(standard, reserve float64) ProviderOption {
	return func(p *Provider) { p.floors.standard, p.floors.reserve = standard, reserve }
}

// floorFor reports the floor and tier a call under ctx is held to. A zero
// floor means the call is not gated.
func (f *backgroundFloors) floorFor(ctx context.Context) (float64, string) {
	switch {
	case vcs.ReserveQuotaReads(ctx):
		return f.reserve, floorTierReserve
	case vcs.CachedReadsAllowed(ctx):
		return f.standard, floorTierStandard
	}
	return 0, ""
}

// quotaShortfall is a floor refusal's figures: the quota's reading and the
// floor it fell below.
type quotaShortfall struct {
	remaining int
	limit     int
	floor     float64
}

func (s *quotaShortfall) message(quota string, resetAt time.Time) string {
	return fmt.Sprintf("GitHub %s quota below the background floor (%d/%d remaining, floor %g%%); background read skipped until %s",
		quota, s.remaining, s.limit, math.Round(s.floor*1000)/10, resetAt.Format(time.RFC3339))
}

// backgroundFloorRefusal returns a rate-limited error, without contacting
// GitHub, for a background read whose quota is below its tier's floor. It
// returns nil for a foreground call, a write or an unknown gh shape, and fails
// open when the quota's figures are unknown, have no limit or are stale. A
// refusal never pauses the breaker, so foreground calls keep running.
func (p *Provider) backgroundFloorRefusal(ctx context.Context, quota string, args []string) error {
	floor, tier := p.floors.floorFor(ctx)
	if floor <= 0 || !isReadInvocation(args) {
		return nil
	}
	res, ok := p.floorQuota(ctx, quota)
	if !ok {
		return nil
	}
	key := floorKey{quota: quota, tier: tier}
	if float64(res.Remaining) >= floor*float64(res.Limit) {
		p.floors.transition(p.logger, key, false, func(e *zerolog.Event) *zerolog.Event { return e })
		return nil
	}
	resetAt := time.Unix(res.Reset, 0)
	p.floors.transition(p.logger, key, true, func(e *zerolog.Event) *zerolog.Event {
		return e.Int("remaining", res.Remaining).Int("limit", res.Limit).Float64("floor", floor).Time("reset_at", resetAt)
	})
	return &rateLimitedError{
		quota:      quota,
		resetAt:    resetAt,
		belowFloor: &quotaShortfall{remaining: res.Remaining, limit: res.Limit, floor: floor},
	}
}

// transition logs one Info line when key starts or stops being refused.
func (f *backgroundFloors) transition(logger zerolog.Logger, key floorKey, refusing bool, fields func(*zerolog.Event) *zerolog.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refusing[key] == refusing {
		return
	}
	if f.refusing == nil {
		f.refusing = make(map[floorKey]bool)
	}
	f.refusing[key] = refusing
	event := fields(logger.Info().Str("quota", key.quota).Str("tier", key.tier))
	if refusing {
		event.Msg("github quota below the background floor; refusing background reads until the quota resets")
		return
	}
	event.Msg("github quota back above the background floor; resuming background reads")
}

// floorQuota reads quota's figures for the floor check through the shared
// quota cache. ok is false, and the read proceeds, when no usable figures are
// available: a failed or unreadable probe (not retried for quotaReadingTTL),
// a missing limit, or a reset that has already passed even after one re-read.
func (p *Provider) floorQuota(ctx context.Context, quota string) (quotaStatus, bool) {
	reading, ok := p.floorReading(ctx)
	if !ok {
		return quotaStatus{}, false
	}
	res, ok := reading.status(quota)
	if ok && p.resetPassed(res) {
		// A reading taken before the reset describes the old window; read the
		// new one once. A reading taken after the reset that still reports it
		// passed cannot be improved by probing again.
		if !reading.readAt.Before(time.Unix(res.Reset, 0)) {
			return quotaStatus{}, false
		}
		p.quotas.expire(reading.readAt)
		if reading, ok = p.floorReading(ctx); !ok {
			return quotaStatus{}, false
		}
		res, ok = reading.status(quota)
	}
	if !ok || res.Limit <= 0 || p.resetPassed(res) {
		return quotaStatus{}, false
	}
	return res, true
}

// floorReading returns the cached quota reading, probing only when it has
// expired and no probe failed within quotaReadingTTL.
func (p *Provider) floorReading(ctx context.Context) (quotaReading, bool) {
	if reading, ok, _ := p.quotas.fresh(); ok {
		return reading, true
	}
	if p.floors.probeBackingOff(p.nowFn()) {
		return quotaReading{}, false
	}
	reading, ok := p.readQuotas(ctx)
	if !ok {
		p.floors.markProbeFailed(p.nowFn())
	}
	return reading, ok
}

func (p *Provider) resetPassed(res quotaStatus) bool {
	return !time.Unix(res.Reset, 0).After(p.nowFn())
}

func (f *backgroundFloors) probeBackingOff(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.probeFailedAt.IsZero() && now.Sub(f.probeFailedAt) < quotaReadingTTL
}

func (f *backgroundFloors) markProbeFailed(now time.Time) {
	f.mu.Lock()
	f.probeFailedAt = now
	f.mu.Unlock()
}

// expire drops the cached reading while it is still the one read at readAt, so
// the next readQuotas probes. A newer reading is kept.
func (c *quotaCache) expire(readAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reading.resources != nil && c.reading.readAt.Equal(readAt) {
		c.reading = quotaReading{}
	}
}

// isReadInvocation reports whether a gh invocation only reads. It is an
// allow-list: a write, and any shape it does not know, is not a read, so the
// floor never refuses it.
func isReadInvocation(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[0] {
	case "pr":
		switch args[1] {
		case "view", "checks", "list", "diff", "status":
			return true
		}
	case "run":
		return args[1] == "list" || args[1] == "view"
	case "api":
		if args[1] == "graphql" {
			return isGraphQLQuery(args[2:])
		}
		for _, arg := range args[1:] {
			if isAPIWriteFlag(arg) {
				return false
			}
		}
		return true
	}
	return false
}

// isAPIWriteFlag reports whether a `gh api` argument sets the method, sends
// fields (which makes gh POST) or sends a request body.
func isAPIWriteFlag(arg string) bool {
	for _, flag := range []string{"--method", "--field", "--raw-field", "--input"} {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return len(arg) >= 2 && arg[0] == '-' && strings.ContainsRune("XfF", rune(arg[1]))
}

// isGraphQLQuery reports whether `gh api graphql` arguments carry a query=
// field whose document is not a mutation.
func isGraphQLQuery(args []string) bool {
	for _, arg := range args {
		for _, prefix := range []string{"--raw-field=", "--field=", "-f", "-F"} {
			if strings.HasPrefix(arg, prefix+"query=") {
				arg = strings.TrimPrefix(arg, prefix)
				break
			}
		}
		if doc, ok := strings.CutPrefix(arg, "query="); ok {
			return !strings.HasPrefix(strings.TrimSpace(doc), "mutation")
		}
	}
	return false
}
