package rotation

import (
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
// semantic source the bind-time comparator (MoreConsumeFirst) and the
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

// MinRotationHeadroom is the least remaining-quota fraction (1 - utilization) a
// candidate must still have before rotation will prefer it for its perishable
// weekly quota rather than for being idle (BOS-830). It bounds consume-first:
// a soon-resetting account is only worth switching onto while it has room to do
// real work, so rotation never lands on an account at 95% that re-caps within
// minutes and burns another attempt from the per-run rotation budget.
//
// Note the deliberate window asymmetry: the band is measured against the
// WORST-case window (UsageUtil of the candidate's live snapshot) while the urgency it gates is the 7d reset. So an
// account whose weekly quota is perishable but whose 5h window is nearly spent
// stays out of the band. That is the conservative pairing and the intended one —
// it is exactly the "re-caps within minutes" case this floor exists to exclude.
//
// A package constant, deliberately not a config key: 0.25 is a starting point
// with no field evidence behind it, and it lives in exactly one place so it can
// be tuned — or promoted to ManagedAgentAccountsConfig — additively once there is.
// Because the floor is compared with >=, the "exactly at the floor is in band"
// guarantee is exact only while the value is binary-representable (0.25 is;
// 0.3 would not be) — worth re-checking the boundary test when tuning it.
const MinRotationHeadroom = 0.25

// HasRotationHeadroom reports whether util leaves at least MinRotationHeadroom
// of the window unspent, i.e. whether the account is inside the consume-first
// band. The comparison is >=, so headroom exactly at the floor is INSIDE the
// band. It is the single source of that predicate for engine.selectCandidate.
func HasRotationHeadroom(util float64) bool { return 1-util >= MinRotationHeadroom }

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
// explicit priority, fresh spendable headroom, weekly expiry, utilization, LRU,
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
		ab, bb := HasRotationHeadroom(au), HasRotationHeadroom(bu)
		if ab != bb {
			return ab
		}
		if ab {
			if order := compareWeeklyReset(a.Reset7d, b.Reset7d, now); order != 0 {
				return order < 0
			}
		}
		if au != bu {
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
