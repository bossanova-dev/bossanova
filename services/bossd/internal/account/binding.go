// Package account resolves which registry account a session should run under
// and produces the per-account spawn environment for that account.
//
// The resolver is deliberately degrade-safe: an empty registry, a missing
// account, or an agent-runner plugin that does not support credential rotation
// all collapse to "account 0" (the system default), meaning no per-account
// environment is injected and the CLI's own login is used. It never panics on
// nil dependencies so it can be constructed before the DB/plugin adapters are
// wired.
package account

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/rotation"
)

// SystemDefaultAccountID is the "account 0" sentinel: the empty string. A
// session bound to it uses the agent CLI's ambient login with no per-account
// environment injection.
const SystemDefaultAccountID = ""

// AccountMeta is the subset of the account registry this resolver reads.
type AccountMeta struct {
	ID           string
	Provider     string // "claude" | "codex" — matches Session.AgentName
	Label        string
	Status       string     // "active" | "disabled"
	Health       string     // "ok" | "failed" — only "ok" is selectable
	Priority     int        // sort order; lower = preferred (matches models.Account)
	CoolingUntil *time.Time // non-nil & future ⇒ cooling
	LastUsedAt   *time.Time // for stable LRU tie-break

	// Cached usage projection (from models.Account.Usage, populated by the
	// probe/refresh paths). Read-only at bind time — the resolver never probes.
	// A nil UsageFetchedAt means "never probed / unknown", which the selector
	// degrades to today's priority/LRU ordering.
	Util5h         float64
	Util7d         float64
	UsageFetchedAt *time.Time // nil = never probed / unknown
	Reset5h        *time.Time // nullable
	Reset7d        *time.Time // nullable
}

// Registry is the account-metadata source the resolver reads. Implementations
// adapt the bossd account store. List returns an empty slice when no registry
// or no accounts exist.
type Registry interface {
	List(ctx context.Context) ([]AccountMeta, error)
	Get(ctx context.Context, id string) (AccountMeta, bool, error)
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// Materializer turns a bound account into concrete spawn environment via the
// agent-runner plugin. SupportsRotation reports whether the provider's plugin
// can materialize credentials at all; when false the resolver degrades to
// status-only binding (no env injected).
type Materializer interface {
	SupportsRotation(ctx context.Context, provider string) (bool, error)
	MaterializeAccount(ctx context.Context, accountID string) (map[string]string, error)
}

// defaultUsageStaleness is the fallback window a cached usage snapshot may age
// before the selector ignores it (falling back to priority/LRU). It mirrors
// config.ManagedAccountsConfig.UsageStalenessWindow's default so a resolver built
// without the option behaves sensibly.
const defaultUsageStaleness = 30 * time.Minute

// Resolver is the degrade-safe "brain" consumed by spawn paths and account
// pickers. It holds no state beyond its dependencies and the usage-staleness
// window used by utilization-aware default selection.
type Resolver struct {
	reg       Registry
	mat       Materializer
	log       zerolog.Logger
	staleness time.Duration
}

// ResolverOption configures a Resolver at construction.
type ResolverOption func(*Resolver)

// WithUsageStalenessWindow sets the max age a cached usage snapshot may have to
// influence default-account selection. A non-positive duration is ignored and
// the default (30m) is kept.
func WithUsageStalenessWindow(d time.Duration) ResolverOption {
	return func(r *Resolver) {
		if d > 0 {
			r.staleness = d
		}
	}
}

// NewResolver builds a Resolver. reg and mat may be nil; the resolver tolerates
// nil dependencies and degrades to account 0 in that case. The usage-staleness
// window defaults to 30m; override it with WithUsageStalenessWindow.
func NewResolver(reg Registry, mat Materializer, log zerolog.Logger, opts ...ResolverOption) *Resolver {
	r := &Resolver{reg: reg, mat: mat, log: log, staleness: defaultUsageStaleness}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// DefaultAccountID picks the default account for provider at time now using two
// tiers. Tier 1 is the account the rotation engine would pick — active, healthy,
// and not cooling — ordered by the same rules the engine uses: lowest Priority
// (lower = preferred), then least-recently-used (never-used first), then lexical
// ID. When no account is strictly eligible, tier 2 falls back to the least-bad
// *active* account of the provider (even one that is cooling or failed-health),
// so a session never lands on the CLI's unmanaged ambient login while the
// provider still has a managed account registered — the user's hard rule.
//
// Returns "" (SystemDefaultAccountID / unmanaged) ONLY when the provider has no
// active account at all: disabled-only and zero-account providers both degrade
// to unmanaged, as does a nil registry. Disabled accounts are a deliberate
// sideline and are excluded from both tiers.
//
// Deliberate rule: tier 2 may bind a failed-health active account when it is the
// only active option. This intentionally diverges from the rotation engine's
// strict selectability — a known-bad managed credential is still preferred over
// the un-interrogable ambient login, and rotation/verification re-checks it on
// use. Do not "fix" this back to skipping failed-health accounts in the fallback.
func (r *Resolver) DefaultAccountID(ctx context.Context, provider string, now time.Time) (string, error) {
	return r.selectDefault(ctx, provider, "", now)
}

// DefaultAccountIDExcluding returns the best eligible account for provider at
// time now EXCLUDING exclude (the current account), applying the same two-tier
// utilization-aware selection as DefaultAccountID. It is used by the mechanical
// `/boss switch` interception to pick the "next best other account".
//
// It returns SystemDefaultAccountID ("") when no OTHER account is eligible; the
// caller interprets that (with a non-empty current account) as "no other account
// available to switch to". A blank exclude makes it identical to
// DefaultAccountID.
func (r *Resolver) DefaultAccountIDExcluding(ctx context.Context, provider, exclude string, now time.Time) (string, error) {
	return r.selectDefault(ctx, provider, exclude, now)
}

// selectDefault holds the shared two-tier selection loop behind DefaultAccountID
// and DefaultAccountIDExcluding. It skips any account whose ID == exclude (only
// when exclude != "") in BOTH tiers; with a blank exclude it is byte-identical to
// the pre-refactor DefaultAccountID.
func (r *Resolver) selectDefault(ctx context.Context, provider, exclude string, now time.Time) (string, error) {
	if r == nil || r.reg == nil {
		return SystemDefaultAccountID, nil
	}
	all, err := r.reg.List(ctx)
	if err != nil {
		return "", err
	}

	best, found := AccountMeta{}, false
	fallback, fallbackFound := AccountMeta{}, false
	for _, a := range all {
		if exclude != "" && a.ID == exclude {
			continue
		}
		if a.Provider != provider || a.Status != "active" {
			continue
		}
		if !fallbackFound || r.moreEligibleFallback(a, fallback, now) {
			fallback, fallbackFound = a, true
		}
		if a.Health != "ok" {
			continue
		}
		if a.CoolingUntil != nil && a.CoolingUntil.After(now) {
			continue
		}
		// A fresh, fully-capped account is excluded from tier-1 so a new chat
		// never lands on an account already at 100% of a window. Stale/unknown
		// usage never sidelines an account (it degrades to priority/LRU below).
		if r.freshCapped(a, now) {
			continue
		}
		if !found || r.moreEligibleUtil(a, best, now) {
			best, found = a, true
		}
	}
	if found {
		return best.ID, nil
	}
	if fallbackFound {
		return fallback.ID, nil
	}
	return SystemDefaultAccountID, nil
}

// isFresh reports whether an account's cached usage snapshot is recent enough to
// influence selection: it was fetched (UsageFetchedAt != nil) no longer than the
// resolver's staleness window ago. A nil snapshot or an aged one is "unknown",
// which the selector degrades to today's priority/LRU ordering.
func (r *Resolver) isFresh(a AccountMeta, now time.Time) bool {
	if a.UsageFetchedAt == nil {
		return false
	}
	// A snapshot stamped in the future (clock skew or corrupt persisted data)
	// is not trustworthy — treat it as unknown rather than perpetually fresh,
	// which would otherwise sideline a capped account past the staleness window.
	if a.UsageFetchedAt.After(now) {
		return false
	}
	return now.Sub(*a.UsageFetchedAt) <= r.staleness
}

// freshCapped reports whether an account has a fresh usage snapshot showing at
// least one window at or above the cap threshold. Only fresh readings sideline
// an account — a stale "capped" reading must not permanently exclude it.
func (r *Resolver) freshCapped(a AccountMeta, now time.Time) bool {
	if !r.isFresh(a, now) {
		return false
	}
	return rotation.UtilizationCapped(a.Util5h) || rotation.UtilizationCapped(a.Util7d)
}

// moreEligibleUtil ranks tier-1 candidates (already active, healthy, not cooling,
// and not fresh-capped) with utilization awareness layered on top of moreEligible:
//   - a fresh, non-capped candidate outranks a stale/unknown one (a recent
//     low-util signal is more trustworthy for avoiding immediate re-cap);
//   - among two fresh candidates, the lower max(Util5h,Util7d) wins, with ties
//     falling through to moreEligible (priority → LRU → id);
//   - among two stale/unknown candidates, moreEligible verbatim — keeping the
//     both-stale path byte-identical to the pre-change ordering.
func (r *Resolver) moreEligibleUtil(candidate, current AccountMeta, now time.Time) bool {
	candFresh := r.isFresh(candidate, now)
	curFresh := r.isFresh(current, now)
	if candFresh != curFresh {
		return candFresh
	}
	if candFresh {
		candUtil := rotation.MaxUtilization(candidate.Util5h, candidate.Util7d)
		curUtil := rotation.MaxUtilization(current.Util5h, current.Util7d)
		if candUtil != curUtil {
			return rotation.LowerUtilization(candUtil, curUtil)
		}
	}
	return moreEligible(candidate, current, now)
}

// moreEligibleFallback ranks tier-2 candidates (active accounts tier-1 rejected):
// a healthy account beats a failed one; then the account whose recovery comes
// soonest; then the tier-1 priority/LRU/id order via moreEligible. Recovery time
// unifies a cooling account's CoolingUntil with a fresh-capped account's soonest
// reset (see recoveryTime), so "all fresh candidates capped → soonest reset".
func (r *Resolver) moreEligibleFallback(candidate, current AccountMeta, now time.Time) bool {
	if healthRank(candidate) != healthRank(current) {
		return healthRank(candidate) < healthRank(current)
	}
	candRec, candKnown := r.recoveryTime(candidate, now)
	curRec, curKnown := r.recoveryTime(current, now)
	if candKnown != curKnown {
		// An account with a definite recovery instant beats one whose recovery
		// time we cannot estimate, so a fresh-capped account with an unknown
		// reset never out-ranks one with a known soonest reset.
		return candKnown
	}
	if candKnown && !candRec.Equal(curRec) {
		return candRec.Before(curRec)
	}
	return moreEligible(candidate, current, now)
}

// recoveryTime is an account's effective "available again" instant for tier-2
// ordering, plus whether that instant is known. A cooling account recovers at
// its future CoolingUntil; a fresh-capped account with a known reset recovers at
// its soonest reset; any other account (not cooling, not fresh-capped) is
// available now (the zero time). All three are "known". Only a fresh-capped
// account whose reset time is unknown returns known=false, so it sorts after
// every account with a definite recovery instant instead of masquerading as
// available-now (which the zero time would otherwise imply).
func (r *Resolver) recoveryTime(a AccountMeta, now time.Time) (time.Time, bool) {
	if a.CoolingUntil != nil && a.CoolingUntil.After(now) {
		return *a.CoolingUntil, true
	}
	if r.freshCapped(a, now) {
		if reset, ok := earliestReset(a); ok {
			return reset, true
		}
		return time.Time{}, false
	}
	return time.Time{}, true
}

// earliestReset returns the soonest reset instant over the account's exhausted
// (capped) windows, and whether any such reset time is known.
func earliestReset(a AccountMeta) (time.Time, bool) {
	var earliest time.Time
	found := false
	if rotation.UtilizationCapped(a.Util5h) && a.Reset5h != nil {
		earliest, found = *a.Reset5h, true
	}
	if rotation.UtilizationCapped(a.Util7d) && a.Reset7d != nil {
		if !found || a.Reset7d.Before(earliest) {
			earliest, found = *a.Reset7d, true
		}
	}
	return earliest, found
}

func healthRank(a AccountMeta) int {
	if a.Health == "ok" {
		return 0
	}
	return 1
}

// moreEligible reports whether candidate should beat current, mirroring the
// rotation engine's ORDER BY (account_decide_tx.go ListByProvider): lower
// Priority wins, then the weekly-expiry tiebreak (BOS-429), then
// least-recently-used (a nil LastUsedAt sorts as the zero time, so never-used
// accounts win the tie), then lexically smaller ID.
//
// The weekly-expiry tiebreak — applied only within an equal-Priority band, so it
// never overrides the operator's explicit prioritization — prefers the account
// whose weekly quota resets soonest in the future, so its credits are spent
// before the window rolls over. It uses rotation.FutureWeeklyReset (the single
// shared predicate) so bind-time selection, the SQL ListByProvider order, and
// the rotation fake never drift: an account with a known future Reset7d beats one
// with a nil (never probed) or past (already-rolled) reset, and among two
// known-future resets the sooner one wins. nil/past resets tie here and fall
// through to the LRU -> id tiebreak.
func moreEligible(candidate, current AccountMeta, now time.Time) bool {
	if candidate.Priority != current.Priority {
		return candidate.Priority < current.Priority
	}
	candReset, candFuture := rotation.FutureWeeklyReset(candidate.Reset7d, now)
	curReset, curFuture := rotation.FutureWeeklyReset(current.Reset7d, now)
	if candFuture != curFuture {
		return candFuture
	}
	if candFuture && !candReset.Equal(curReset) {
		return candReset.Before(curReset)
	}
	if !lastUsed(candidate).Equal(lastUsed(current)) {
		return lastUsed(candidate).Before(lastUsed(current))
	}
	return candidate.ID < current.ID
}

func lastUsed(a AccountMeta) time.Time {
	if a.LastUsedAt == nil {
		return time.Time{}
	}
	return *a.LastUsedAt
}

// ResolveSpawnEnv produces the per-account spawn environment for accountID and
// records the account as used. Call it from paths that are actually launching
// an agent; read-only diagnostics must call ResolveSpawnEnvForProbe instead.
//
// Degrade-safe rules:
//   - accountID == "" (account 0): return (nil, nil) immediately, with NO
//     registry or materializer calls.
//   - account not found in the registry: log once and treat as account 0.
//   - provider plugin does not support rotation (or materializer is nil): log
//     the degrade once and return (nil, nil) — status-only binding.
//   - otherwise materialize the account's env, best-effort bump last-used
//     (log-and-continue on error), and return the env.
func (r *Resolver) ResolveSpawnEnv(ctx context.Context, accountID, provider string, now time.Time) (map[string]string, error) {
	return r.resolveSpawnEnv(ctx, accountID, provider, now, true)
}

// ResolveSpawnEnvForProbe produces the SAME environment ResolveSpawnEnv does,
// through the same body, but performs no last-used bookkeeping. It exists for
// read-only diagnostics — today boss session mcp / DescribeChatMCP — which must
// be able to describe a chat's environment without changing anything about it.
//
// Why sharing the body matters: LastUsedAt is the LRU key DefaultAccountID
// selects on, so a diagnostic that bumped it could change which account the
// NEXT session is handed. But a probe that derived a DIFFERENT environment than
// the live chat would report on a world the chat never sees, which is the exact
// misdiagnosis BOS-867 exists to end. Both requirements are met only by one
// implementation with the bookkeeping varied, never by a forked derivation.
//
// Side effects this path DOES perform: MaterializeAccount. That is deliberate
// and not skippable — materializing IS what produces the env (the claude plugin
// turns the stored credential blob into the token overlay; the codex plugin
// emits its auth.json file spec), so skipping it would return an empty map and
// break the byte-identical guarantee above. Any credential refresh or rotation
// a provider performs inside MaterializeAccount therefore still happens on the
// probe path, by design: the probe must run under the credentials a real spawn
// would get.
//
// Side effect this path SKIPS: TouchLastUsed. That is the only one, and it is
// pure bookkeeping — nothing about the returned env depends on it.
//
// now is accepted for signature symmetry with ResolveSpawnEnv and is unused
// beyond the shared body's degrade paths; it must not become a second way for
// the two to diverge.
func (r *Resolver) ResolveSpawnEnvForProbe(ctx context.Context, accountID, provider string, now time.Time) (map[string]string, error) {
	return r.resolveSpawnEnv(ctx, accountID, provider, now, false)
}

// resolveSpawnEnv is the one body both exported entry points share.
// recordLastUsed is the ONLY behavioural difference between them.
func (r *Resolver) resolveSpawnEnv(
	ctx context.Context,
	accountID, provider string,
	now time.Time,
	recordLastUsed bool,
) (map[string]string, error) {
	if accountID == SystemDefaultAccountID {
		return nil, nil
	}
	if r == nil {
		return nil, nil
	}
	if r.reg == nil {
		r.logDebug().Str("account_id", accountID).Msg("account: no registry; using system default")
		return nil, nil
	}

	acct, ok, err := r.reg.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		r.logWarn().Str("account_id", accountID).Msg("account: bound account not found; using system default")
		return nil, nil
	}

	if r.mat == nil {
		r.logDebug().Str("account_id", accountID).Str("provider", provider).
			Msg("account: no materializer; status-only binding")
		return nil, nil
	}
	supports, err := r.mat.SupportsRotation(ctx, provider)
	if err != nil {
		return nil, err
	}
	if !supports {
		r.logDebug().Str("account_id", accountID).Str("provider", provider).
			Msg("account: plugin lacks rotation; status-only binding")
		return nil, nil
	}

	env, err := r.mat.MaterializeAccount(ctx, acct.ID)
	if err != nil {
		return nil, err
	}
	if recordLastUsed {
		if touchErr := r.reg.TouchLastUsed(ctx, acct.ID, now); touchErr != nil {
			r.logWarn().Err(touchErr).Str("account_id", acct.ID).
				Msg("account: failed to record last-used; continuing")
		}
	}
	return env, nil
}

// Label returns a human-friendly label for accountID. "" ⇒ "Unmanaged local credentials".
// A known account returns its Label. When the registry is unreachable or the
// account is unknown it falls back to a short ID prefix so it is never empty.
func (r *Resolver) Label(ctx context.Context, accountID string) (string, error) {
	if accountID == SystemDefaultAccountID {
		return UnmanagedLocalCredentialsLabel, nil
	}
	if r == nil || r.reg == nil {
		return ShortID(accountID), nil
	}
	acct, ok, err := r.reg.Get(ctx, accountID)
	if err != nil || !ok || acct.Label == "" {
		return ShortID(accountID), nil
	}
	return acct.Label, nil
}

// ShortID returns the first 8 characters of id (or the whole id if shorter).
// It is the single degraded-DISPLAY-label policy for account ids that cannot
// be resolved to a human label: Resolver.Label (session AccountLabel, chat
// rotator audit from-side) and the session package's resolveAccountLabel /
// registry adapter (manual-switch, failover, headless-rotation audits) all
// fall back through it, so the same broken account degrades to the same
// string on every surface. Display-only — never feed a shortened id back
// into an RPC/CLI input, which requires the full id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (r *Resolver) logDebug() *zerolog.Event { return r.log.Debug() }
func (r *Resolver) logWarn() *zerolog.Event  { return r.log.Warn() }
