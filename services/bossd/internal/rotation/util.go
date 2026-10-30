package rotation

import (
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/recurser/bossd/internal/domain"
)

// FutureWeeklyReset reports the account's weekly-quota reset instant when it is
// known AND still strictly in the future; ok=false means "no urgent expiry"
// (a nil reset — never probed — or a reset already at/before now, since an
// already-rolled window is a fresh full week, not urgent). It is the single
// semantic source the stale branch of MoreConsumeFirst and the
// rotation fake share, so both stay in lockstep with the SQL
// "usage_reset_7d IS NOT NULL AND usage_reset_7d > now" rank in
// account_decide_tx.go's ListByProvider. Keeping the three ordering surfaces on
// one predicate is the "never drift" contract BOS-429 depends on.
func FutureWeeklyReset(reset7d *time.Time, now time.Time) (time.Time, bool) {
	if reset7d == nil {
		return time.Time{}, false
	}
	if !reset7d.After(now) {
		return time.Time{}, false
	}
	return *reset7d, true
}

// UtilizationCapThreshold is the utilization fraction at or above which a window
// is considered exhausted (fully capped). 1.0 == 100% of the window consumed.
const UtilizationCapThreshold = 1.0

// UtilizationCapped reports whether a single utilization fraction is at or above
// the cap threshold. It is the single source of the "this window is exhausted"
// predicate shared by engine.selectCandidate and the binding-time resolver.
func UtilizationCapped(util float64) bool { return util >= UtilizationCapThreshold }

// LowerUtilization reports whether a is strictly less utilized than b. It is the
// single source of the "prefer the less-utilized account" comparator shared by
// engine.selectCandidate and the binding-time resolver.
func LowerUtilization(a, b float64) bool { return a < b }

// MinRotationHeadroom is the effective 5h quota required to prefer an account's
// perishable weekly quota. The binary-representable floor includes equality.
const MinRotationHeadroom = 0.25

// MinWeeklyHeadroom keeps weekly and matched model windows below exhaustion
// while allowing soon-resetting accounts to spend their remaining weekly quota.
const MinWeeklyHeadroom = 0.05

// Fixed geometric boundaries preserve transitivity while widening the balancing
// interval further from reset. Sub-hour resets share the first bucket.
const (
	weeklyResetBucketBase  = time.Hour
	weeklyResetBucketWidth = 0.15
	weeklyPaceStep         = 0.05
	fiveHourHeadroomStep   = 0.10
)

// HasRotationHeadroom is the single fresh consume-first band predicate: enough
// effective 5h headroom, plus weekly and matched model-window reserves.
func HasRotationHeadroom(c ConsumeFirstCandidate, now time.Time) bool {
	return 1-effectiveUtil5h(c, now) >= MinRotationHeadroom &&
		c.Util7d < 1-MinWeeklyHeadroom && c.ModelUtil < 1-MinWeeklyHeadroom
}

func effectiveUtil5h(c ConsumeFirstCandidate, now time.Time) float64 {
	if c.Reset5h != nil && !c.Reset5h.After(now) {
		return 0
	}
	return c.Util5h
}

func weeklyResetBucket(reset, now time.Time) int {
	return max(0, int(math.Floor(math.Log(float64(reset.Sub(now))/float64(weeklyResetBucketBase))/math.Log1p(weeklyResetBucketWidth))))
}

func weeklyPace(c ConsumeFirstCandidate, reset, now time.Time) float64 {
	elapsed := min(1., max(0., 1-float64(reset.Sub(now))/float64(7*24*time.Hour)))
	return math.Floor((max(c.Util7d, c.ModelUtil) - elapsed) / weeklyPaceStep)
}

func compareFreshReset(a, b ConsumeFirstCandidate, now time.Time) (int, bool) {
	ar, ak := FutureWeeklyReset(a.Reset7d, now)
	br, bk := FutureWeeklyReset(b.Reset7d, now)
	if ak != bk {
		if ak {
			return -1, true
		}
		return 1, true
	}
	if !ak {
		return 0, false
	}
	ab, bb := weeklyResetBucket(ar, now), weeklyResetBucket(br, now)
	if ab < bb {
		return -1, true
	}
	if ab > bb {
		return 1, true
	}
	ap, bp := weeklyPace(a, ar, now), weeklyPace(b, br, now)
	if ap < bp {
		return -1, true
	}
	if ap > bp {
		return 1, true
	}
	ah, bh := math.Floor((1-effectiveUtil5h(a, now))/fiveHourHeadroomStep), math.Floor((1-effectiveUtil5h(b, now))/fiveHourHeadroomStep)
	if ah > bh {
		return -1, true
	}
	if ah < bh {
		return 1, true
	}
	return 0, true
}

// MaxUtilization reduces a 5h/7d utilization pair to the single worst-case
// fraction. It carries no rate-limit semantics; callers that need the
// rate-limited→1 bump layer it on top (see UsageUtil).
func MaxUtilization(u5h, u7d float64) float64 {
	if u7d > u5h {
		return u7d
	}
	return u5h
}

// Selectable reports whether an account may be chosen to run: active, healthy,
// and not benched by durable credential-verification state. It is the exported
// form of the engine's own predicate, so binding-validation surfaces outside
// this package share it instead of re-deriving eligibility and drifting.
func Selectable(a *domain.AgentAccount) bool { return isSelectable(a) }

// BindableNow reports whether an account may be bound to a session right now:
// Selectable, and not inside a cooldown window at the given instant.
//
// Explicit binding paths (CreateSession with an account id, manual account
// switch) need this stricter form because they must refuse BEFORE mutating a
// session — a spawn-time refusal arrives after the chat has already been
// stopped and rebound.
func BindableNow(a *domain.AgentAccount, now time.Time) bool {
	if !Selectable(a) {
		return false
	}
	return a.CooldownUntil == nil || !a.CooldownUntil.After(now)
}

// ConsumeFirstCandidate is the shared ranking view for bind-time and rotation
// selection. Callers remove ineligible accounts before comparing candidates.
type ConsumeFirstCandidate struct {
	ID         string
	Priority   int
	Fresh      bool
	Util5h     float64
	Util7d     float64
	ModelUtil  float64
	ModelReset *time.Time
	Reset5h    *time.Time
	Reset7d    *time.Time
	LastUsedAt *time.Time
}

// Over5hThreshold reports a fresh 5-hour window at or above threshold that
// has not reset. An unknown reset cannot establish that the window rolled over.
func Over5hThreshold(c ConsumeFirstCandidate, threshold float64, now time.Time) bool {
	return c.Fresh && c.Util5h >= threshold && (c.Reset5h == nil || c.Reset5h.After(now))
}

// MoreConsumeFirst is the single ranking contract for binding and rotation:
// explicit priority, fresh spendable headroom, weekly reset bucket, coarse pace,
// coarse 5h headroom, LRU,
// then ID. Its stale branch matches ListByProvider for selectable accounts.
func MoreConsumeFirst(a, b ConsumeFirstCandidate, now time.Time) bool {
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	if a.Fresh != b.Fresh {
		return a.Fresh
	}
	if a.Fresh {
		au, bu := max(a.Util5h, a.Util7d, a.ModelUtil), max(b.Util5h, b.Util7d, b.ModelUtil)
		ab, bb := HasRotationHeadroom(a, now), HasRotationHeadroom(b, now)
		if ab != bb {
			return ab
		}
		ordered := false
		if ab {
			order, known := compareFreshReset(a, b, now)
			if order != 0 {
				return order < 0
			}
			ordered = known
		}
		if !ordered && au != bu {
			return LowerUtilization(au, bu)
		}
	} else if order := compareWeeklyReset(a.Reset7d, b.Reset7d, now); order != 0 {
		return order < 0
	}
	var al, bl time.Time
	if a.LastUsedAt != nil {
		al = *a.LastUsedAt
	}
	if b.LastUsedAt != nil {
		bl = *b.LastUsedAt
	}
	if !al.Equal(bl) {
		return al.Before(bl)
	}
	return a.ID < b.ID
}

func compareWeeklyReset(a, b *time.Time, now time.Time) int {
	ar, ak := FutureWeeklyReset(a, now)
	br, bk := FutureWeeklyReset(b, now)
	if ak != bk {
		if ak {
			return -1
		}
		return 1
	}
	if ak {
		if ar.Before(br) {
			return -1
		}
		if br.Before(ar) {
			return 1
		}
	}
	return 0
}

// ModelWindowFor returns the per-model window that governs model, if any.
func ModelWindowFor(windows []domain.UsageModelWindow, model string) (domain.UsageModelWindow, bool) {
	tokens := modelTokens(model)
	for _, w := range windows {
		name := modelTokens(w.ModelName)
		if len(name) == 0 || len(name) > len(tokens) {
			continue
		}
		for i := 0; i <= len(tokens)-len(name); i++ {
			if slices.Equal(tokens[i:i+len(name)], name) {
				return w, true
			}
		}
	}
	return domain.UsageModelWindow{}, false
}
func modelTokens(model string) []string {
	return strings.FieldsFunc(strings.ToLower(model), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// UsageUtilForModel includes the matched model window in the account utilization.
func UsageUtilForModel(snap domain.UsageSnapshot, model string) float64 {
	util := UsageUtil(snap)
	if w, ok := ModelWindowFor(snap.ModelWindows, model); ok {
		util = max(util, w.Util)
	}
	return util
}

// UsageUtil reduces a usage snapshot to the single worst-case utilization
// fraction used to compare account load on one scale: max of Util5h/Util7d,
// bumped to 1 when a status-only snapshot is explicitly rate-limited. A
// measured sub-cap window wins over a contradictory coarse plan status, matching
// UsageSnapshotConfirmsLimited. It is the canonical reduction shared by the
// rotation package and the cmd wiring (candidate utilization probing), so the
// two cannot drift. (BOS-318)
func UsageUtil(snap domain.UsageSnapshot) float64 {
	util := MaxUtilization(snap.Util5h, snap.Util7d)
	if UsageSnapshotRateLimited(snap) && util < 1 && !hasMeasuredSubcapUsage(snap) {
		return 1
	}
	return util
}

// freshModelCapped ignores unknown, stale and future-dated cached usage.
func freshModelCapped(snap *domain.UsageSnapshot, model string, now time.Time, staleness time.Duration) bool {
	if snap == nil || snap.FetchedAt == nil || snap.FetchedAt.After(now) || now.Sub(*snap.FetchedAt) > staleness {
		return false
	}
	window, ok := ModelWindowFor(snap.ModelWindows, model)
	return ok && UtilizationCapped(window.Util)
}
