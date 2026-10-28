// Package account resolves which registry account a session should run under
// and produces the per-account spawn environment for that account.
//
// Account SELECTION is degrade-safe: with no registry, no accounts, or no
// eligible account, DefaultAgentAccountID returns "account 0" (the system default)
// and the session runs on the agent CLI's own login. Nothing was promised, so
// nothing is broken.
//
// Credential INJECTION for a session that is already bound is not (BOS-1142).
// A bound session that cannot be given its account's credentials fails closed
// with a typed *InjectionError rather than silently running on the ambient
// login: the alternative produces work attributed to, billed to, and
// rate-limited against an identity the operator did not choose, with no signal
// anywhere that it happened. The error's Outcome separates "this credential is
// unusable" (InjectionOutcomeInvalid) from "I could not evaluate this
// credential" (InjectionOutcomeUndetermined) so no caller has to guess.
//
// The resolver still never panics on nil dependencies, so it can be constructed
// before the DB/plugin adapters are wired.
package account

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/rotation"
)

// SystemDefaultAgentAccountID is the "account 0" sentinel: the empty string. A
// session bound to it uses the agent CLI's ambient login with no per-account
// environment injection.
const SystemDefaultAgentAccountID = ""

// Meta is the subset of the account registry this resolver reads.
type Meta struct {
	ModelWindows []domain.UsageModelWindow
	ID           string
	Provider     string // "claude" | "codex" — matches Session.AgentName
	Label        string
	Status       string     // "active" | "disabled"
	Health       string     // "ok" | "failed" — only "ok" is selectable
	Priority     int        // sort order; lower = preferred (matches models.Account)
	CoolingUntil *time.Time // non-nil & future ⇒ cooling
	LastUsedAt   *time.Time // for stable LRU tie-break

	// AuthInvalid is durable credential-verification state (BOS-1141): the
	// provider CONFIRMED the stored credential is unusable. It is deliberately
	// separate from Health, which also absorbs transient injection failures —
	// only a confirmed auth failure sets this, so a provider outage can never
	// bench an account through it. An auth-invalid account is excluded from
	// BOTH selection tiers and refused before materialization; since BOS-1142
	// that refusal fails the spawn closed with an InjectionOutcomeInvalid error
	// instead of degrading to the system default. A later healthy verification
	// clears it.
	AuthInvalid bool

	// Suspended means the provider refused the account outright (for example
	// the organization disabled subscription access). Like a disabled account it
	// is never selected and never launched on.
	Suspended bool

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
	List(ctx context.Context) ([]Meta, error)
	Get(ctx context.Context, id string) (Meta, bool, error)
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
}

// injectionHealthRecorder is an OPTIONAL seam the resolver type-asserts off its
// Registry. When the registry implements it, a materialize failure on a real
// spawn is recorded durably on the account row and a later success withdraws
// it. A registry that does not implement it degrades to the pre-BOS-973
// behaviour (log only), matching the resolver's nil-dependency discipline.
//
// Why it lives here and not at the call sites: four separate paths consume a
// resolver error (server.resolveAccountEnv, server.resolveChatAccountEnvForSpawn,
// accountwiring.SpawnEnvResolver.Resolve, plugin host service). Since BOS-1142
// they all PROPAGATE it rather than degrading to the ambient CLI login, but they
// still all funnel through resolveSpawnEnv, so recording once here cannot drift
// the way four copies would.
//
// reason is the MaterializeAgentAccount error string and nothing else. It carries
// filesystem paths, never credential material, and no implementation may widen
// it.
type injectionHealthRecorder interface {
	RecordInjectionFailure(ctx context.Context, id string, reason string) error
	MarkInjectionFailureCleared(ctx context.Context, id string) error
}

// Materializer turns a bound account into concrete spawn environment via the
// agent-runner plugin. SupportsRotation reports whether the provider's plugin
// can materialize credentials at all; when false the resolver degrades to
// status-only binding (no env injected).
type Materializer interface {
	SupportsRotation(ctx context.Context, provider string) (bool, error)
	MaterializeAgentAccount(ctx context.Context, accountID string) (map[string]string, error)
}

// defaultUsageStaleness is the fallback window a cached usage snapshot may age
// before the selector ignores it (falling back to priority/LRU). It mirrors
// config.ManagedAgentAccountsConfig.UsageStalenessWindow's default so a resolver built
// without the option behaves sensibly.
const defaultUsageStaleness = 30 * time.Minute

// Resolver is the degrade-safe "brain" consumed by spawn paths and account
// pickers. It holds no state beyond its dependencies and the usage-staleness
// window used by utilization-aware default selection.
type Resolver struct {
	reg                  Registry
	mat                  Materializer
	log                  zerolog.Logger
	staleness            time.Duration
	proactive5hThreshold float64
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

// WithProactive5hThreshold sets the fresh 5-hour utilization exclusion threshold.
// Invalid fractions keep the default (90%).
func WithProactive5hThreshold(threshold float64) ResolverOption {
	return func(r *Resolver) {
		if threshold > 0 && threshold <= 1 {
			r.proactive5hThreshold = threshold
		}
	}
}

// NewResolver builds a Resolver. reg and mat may be nil; the resolver tolerates
// nil dependencies and degrades to account 0 in that case. The usage-staleness
// window defaults to 30m; override it with WithUsageStalenessWindow.
func NewResolver(reg Registry, mat Materializer, log zerolog.Logger, opts ...ResolverOption) *Resolver {
	r := &Resolver{reg: reg, mat: mat, log: log, staleness: defaultUsageStaleness, proactive5hThreshold: .90}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// DefaultAgentAccountID picks the default account for provider at time now using two
// tiers. Tier 1 is the account the rotation engine would pick — active, healthy,
// and not cooling — ranked by rotation.MoreConsumeFirst: explicit priority,
// fresh headroom, weekly expiry, utilization, LRU and ID. When no account is strictly eligible, tier 2 falls back to the least-bad
// *active* account of the provider (even one that is cooling or failed-health),
// so a session never lands on the CLI's unmanaged ambient login while the
// provider still has a managed account registered — the user's hard rule.
//
// Returns "" (SystemDefaultAgentAccountID / unmanaged) ONLY when the provider has no
// active account at all: disabled-only and zero-account providers both degrade
// to unmanaged, as does a nil registry. Disabled accounts are a deliberate
// sideline and are excluded from both tiers.
//
// Deliberate rule: tier 2 may bind a failed-health active account when it is the
// only active option. This intentionally diverges from the rotation engine's
// strict selectability — a known-bad managed credential is still preferred over
// the un-interrogable ambient login, and rotation/verification re-checks it on
// use. Do not "fix" this back to skipping failed-health accounts in the fallback.
func (r *Resolver) DefaultAgentAccountID(ctx context.Context, provider, model string, now time.Time) (string, error) {
	return r.selectDefault(ctx, provider, model, "", now)
}

// DefaultAgentAccountIDExcluding returns the best eligible account for provider at
// time now EXCLUDING exclude (the current account), applying the same two-tier
// utilization-aware selection as DefaultAgentAccountID. It is used by the mechanical
// `/boss switch` interception to pick the "next best other account".
//
// It returns SystemDefaultAgentAccountID ("") when no OTHER account is eligible; the
// caller interprets that (with a non-empty current account) as "no other account
// available to switch to". A blank exclude makes it identical to
// DefaultAgentAccountID.
func (r *Resolver) DefaultAgentAccountIDExcluding(ctx context.Context, provider, model, exclude string, now time.Time) (string, error) {
	return r.selectDefault(ctx, provider, model, exclude, now)
}

// selectDefault holds the shared two-tier selection loop behind DefaultAgentAccountID
// and DefaultAgentAccountIDExcluding. It skips any account whose ID == exclude (only
// when exclude != "") in BOTH tiers; with a blank exclude it is byte-identical to
// the pre-refactor DefaultAgentAccountID.
func (r *Resolver) selectDefault(ctx context.Context, provider, model, exclude string, now time.Time) (string, error) {
	if r == nil || r.reg == nil {
		return SystemDefaultAgentAccountID, nil
	}
	all, err := r.reg.List(ctx)
	if err != nil {
		return "", err
	}

	best, found := Meta{}, false
	fallback, fallbackFound := Meta{}, false
	for _, a := range all {
		if exclude != "" && a.ID == exclude {
			continue
		}
		if a.Provider != provider || a.Status != "active" {
			continue
		}
		// Durable auth-invalid state (BOS-1141) removes an account from BOTH
		// tiers, including the least-bad fallback: unlike a failed health flag
		// (which can be a transient injection blip worth retrying), this is a
		// confirmed provider rejection, so "least bad" must never land here.
		// With no other candidate the loop falls through to the system default.
		if a.AuthInvalid || a.Suspended {
			continue
		}
		if !fallbackFound || r.moreEligibleFallback(a, fallback, now, model) {
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
		if r.freshCapped(a, now, model) || rotation.Over5hThreshold(r.consumeCandidate(a, now, model), r.proactive5hThreshold, now) {
			continue
		}
		if !found || rotation.MoreConsumeFirst(r.consumeCandidate(a, now, model), r.consumeCandidate(best, now, model), now) {
			best, found = a, true
		}
	}
	if found {
		return best.ID, nil
	}
	if fallbackFound {
		return fallback.ID, nil
	}
	return SystemDefaultAgentAccountID, nil
}

// isFresh reports whether an account's cached usage snapshot is recent enough to
// influence selection: it was fetched (UsageFetchedAt != nil) no longer than the
// resolver's staleness window ago. A nil snapshot or an aged one is "unknown",
// which the selector degrades to today's priority/LRU ordering.
func (r *Resolver) isFresh(a Meta, now time.Time) bool {
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
func (r *Resolver) freshCapped(a Meta, now time.Time, model string) bool {
	if !r.isFresh(a, now) {
		return false
	}
	return rotation.UtilizationCapped(a.Util5h) || rotation.UtilizationCapped(a.Util7d) || rotation.UtilizationCapped(r.consumeCandidate(a, now, model).ModelUtil)
}

func (r *Resolver) consumeCandidate(a Meta, now time.Time, model string) rotation.ConsumeFirstCandidate {
	w, _ := rotation.ModelWindowFor(a.ModelWindows, model)
	return rotation.ConsumeFirstCandidate{ModelUtil: w.Util, ModelReset: w.ResetsAt, ID: a.ID, Priority: a.Priority, Fresh: r.isFresh(a, now), Util5h: a.Util5h, Util7d: a.Util7d, Reset5h: a.Reset5h, Reset7d: a.Reset7d, LastUsedAt: a.LastUsedAt}
}

// moreEligibleFallback ranks tier-2 candidates (active accounts tier-1 rejected):
// a healthy account beats a failed one; then the account whose recovery comes
// soonest; then the tier-1 priority/LRU/id order via moreEligible. Recovery time
// unifies a cooling account's CoolingUntil with a fresh-capped account's soonest
// reset (see recoveryTime), so "all fresh candidates capped → soonest reset".
func (r *Resolver) moreEligibleFallback(candidate, current Meta, now time.Time, model string) bool {
	if healthRank(candidate) != healthRank(current) {
		return healthRank(candidate) < healthRank(current)
	}
	candRec, candKnown := r.recoveryTime(candidate, now, model)
	curRec, curKnown := r.recoveryTime(current, now, model)
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
func (r *Resolver) recoveryTime(a Meta, now time.Time, model string) (time.Time, bool) {
	if a.CoolingUntil != nil && a.CoolingUntil.After(now) {
		return *a.CoolingUntil, true
	}
	if r.freshCapped(a, now, model) || rotation.Over5hThreshold(r.consumeCandidate(a, now, model), r.proactive5hThreshold, now) {
		if reset, ok := r.earliestReset(a, now, model); ok {
			return reset, true
		}
		return time.Time{}, false
	}
	return time.Time{}, true
}

// earliestReset returns the soonest reset instant over the account's exhausted
// (capped) windows, and whether any such reset time is known.
func (r *Resolver) earliestReset(a Meta, now time.Time, model string) (time.Time, bool) {
	var earliest time.Time
	found := false
	if (rotation.UtilizationCapped(a.Util5h) || rotation.Over5hThreshold(r.consumeCandidate(a, now, model), r.proactive5hThreshold, now)) && a.Reset5h != nil {
		earliest, found = *a.Reset5h, true
	}
	if rotation.UtilizationCapped(a.Util7d) && a.Reset7d != nil {
		if !found || a.Reset7d.Before(earliest) {
			earliest, found = *a.Reset7d, true
		}
	}
	w, ok := rotation.ModelWindowFor(a.ModelWindows, model)
	if ok && rotation.UtilizationCapped(w.Util) && w.ResetsAt != nil && (!found || w.ResetsAt.Before(earliest)) {
		earliest, found = *w.ResetsAt, true
	}
	return earliest, found
}

func healthRank(a Meta) int {
	if a.Health == "ok" {
		return 0
	}
	return 1
}

// moreEligible retains the fallback's priority, weekly expiry, LRU and ID order.
func moreEligible(candidate, current Meta, now time.Time) bool {
	return rotation.MoreConsumeFirst(rotation.ConsumeFirstCandidate{ID: candidate.ID, Priority: candidate.Priority, Reset7d: candidate.Reset7d, LastUsedAt: candidate.LastUsedAt}, rotation.ConsumeFirstCandidate{ID: current.ID, Priority: current.Priority, Reset7d: current.Reset7d, LastUsedAt: current.LastUsedAt}, now)
}

// ResolveSpawnEnv produces the per-account spawn environment for accountID and
// records the account as used. Call it from paths that are actually launching
// an agent; read-only diagnostics must call ResolveSpawnEnvForProbe instead.
//
// Rules (BOS-1142 — bound bindings fail closed):
//   - accountID == "" (account 0): return (nil, nil) immediately, with NO
//     registry or materializer calls. Nothing was bound, nothing can be broken.
//   - account not found, auth-invalid, provider plugin without rotation
//     support, or a materialize failure: return an *InjectionError with
//     InjectionOutcomeInvalid. The spawn must not proceed on the ambient login.
//   - registry or materializer not wired, or an infrastructure call failed:
//     return an *InjectionError with InjectionOutcomeUndetermined. It still
//     fails the spawn closed, but it is NOT evidence the credential is bad and
//     must never be reported to an operator as one.
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
// Why sharing the body matters: LastUsedAt is the LRU key DefaultAgentAccountID
// selects on, so a diagnostic that bumped it could change which account the
// NEXT session is handed. But a probe that derived a DIFFERENT environment than
// the live chat would report on a world the chat never sees, which is the exact
// misdiagnosis BOS-867 exists to end. Both requirements are met only by one
// implementation with the bookkeeping varied, never by a forked derivation.
//
// Side effects this path DOES perform: MaterializeAgentAccount. That is deliberate
// and not skippable — materializing IS what produces the env (the claude plugin
// turns the stored credential blob into the token overlay; the codex plugin
// emits its auth.json file spec), so skipping it would return an empty map and
// break the byte-identical guarantee above. Any credential refresh or rotation
// a provider performs inside MaterializeAgentAccount therefore still happens on the
// probe path, by design: the probe must run under the credentials a real spawn
// would get.
//
// Side effects this path SKIPS: TouchLastUsed, and the BOS-973 injection-health
// record/clear. Both are pure bookkeeping — nothing about the returned env
// depends on either — and both write a ROTATION-SELECTION KEY that a diagnostic
// must not skew: LastUsedAt is the LRU key, and health is what
// rotation.isSelectable gates on. A probe that failed an account's health would
// change which account the next real session is handed, which is precisely the
// class of side effect this entry point exists to avoid.
//
// now is accepted for signature symmetry with ResolveSpawnEnv and is unused
// beyond the shared body's degrade paths; it must not become a second way for
// the two to diverge.
func (r *Resolver) ResolveSpawnEnvForProbe(ctx context.Context, accountID, provider string, now time.Time) (map[string]string, error) {
	return r.resolveSpawnEnv(ctx, accountID, provider, now, false)
}

// resolveSpawnEnv is the one body both exported entry points share.
// recordLastUsed is the ONLY behavioural difference between them.
//
// BOS-1142 repealed the degrade-to-account-0 policy for BOUND accounts. A
// session that names an account and cannot be given that account's credentials
// must not silently run on the agent CLI's ambient login: it produces work
// attributed to, billed to, and rate-limited against the wrong identity, and
// nothing on screen says so. Every bound refusal below therefore returns a typed
// *InjectionError the callers propagate.
//
// Two shapes stay degrading, and are not defects:
//   - accountID == SystemDefaultAgentAccountID: there is no binding to honour.
//   - r == nil: the resolver itself was never constructed.
func (r *Resolver) resolveSpawnEnv(
	ctx context.Context,
	accountID, provider string,
	now time.Time,
	recordLastUsed bool,
) (map[string]string, error) {
	if accountID == SystemDefaultAgentAccountID {
		return nil, nil
	}
	if r == nil {
		return nil, nil
	}
	if r.reg == nil {
		// Could-not-evaluate, not invalid: with no registry the resolver has no
		// way to look the binding up, which says nothing about the credential.
		return nil, undeterminedInjection(accountID, provider,
			"account registry is not configured; cannot resolve the bound account", nil)
	}

	acct, ok, err := r.reg.Get(ctx, accountID)
	if err != nil {
		return nil, undeterminedInjection(accountID, provider, "could not read the bound account", err)
	}
	if !ok {
		return nil, invalidInjection(accountID, provider, "bound account not found in the registry", nil)
	}

	// BOS-1141 recorded the confirmed auth-invalid verdict; BOS-1142 acts on it.
	// Refused BEFORE any materialization, so a known-dead account never reaches
	// the keyring, the worktree, or a spawned agent.
	if acct.AuthInvalid {
		return nil, invalidInjection(accountID, provider,
			"credential verification reported the stored credential invalid; re-authenticate the account", nil)
	}
	// A disabled or suspended account is out of service. Callers replace such a
	// binding before they get here; this refusal is the backstop that keeps any
	// path that did not from launching on it.
	if acct.Status == "disabled" {
		return nil, invalidInjection(accountID, provider, "the account is disabled", nil)
	}
	if acct.Suspended {
		return nil, invalidInjection(accountID, provider, "the provider has suspended the account", nil)
	}

	if r.mat == nil {
		// Could-not-evaluate: a daemon wired without a materializer cannot
		// answer the credential question for ANY account.
		return nil, undeterminedInjection(accountID, provider,
			"no credential materializer is wired; cannot inject the bound account", nil)
	}
	supports, err := r.mat.SupportsRotation(ctx, provider)
	if err != nil {
		return nil, undeterminedInjection(accountID, provider,
			"could not determine whether the provider supports credential rotation", err)
	}
	if !supports {
		// Invalid, not undetermined: the answer IS known — this provider's
		// runner cannot serve a managed account, so the binding can never be
		// honoured until the operator rebinds or upgrades the plugin.
		return nil, invalidInjection(accountID, provider,
			"the agent runner for this provider does not support credential rotation", nil)
	}

	env, err := r.mat.MaterializeAgentAccount(ctx, acct.ID)
	if err != nil {
		// recordLastUsed also gates the health write, and for the same reason it
		// gates TouchLastUsed: health is a rotation-selection key
		// (rotation.isSelectable), so the read-only probe path must not skew it.
		// See ResolveSpawnEnvForProbe's contract.
		if recordLastUsed {
			r.recordInjectionFailure(ctx, acct.ID, provider, err)
		}
		// BOS-973 recorded this failure; BOS-1142 stops swallowing it. The
		// underlying error is preserved via Unwrap so existing errors.Is checks
		// (context cancellation in particular) still see through.
		//
		// The recordInjectionFailure above stays on BOTH arms: it tracks whether
		// injection WORKED, which is a different axis from whether the credential
		// is good, and it is withdrawn on the next successful spawn either way.
		// Only the operator-facing classification below splits.
		if isUndeterminedCause(err) {
			// The call never reached a verdict — the plugin was mid-restart, the
			// RPC timed out, the context was cancelled. Calling that "invalid"
			// tells the operator to re-authenticate a credential nobody looked
			// at, which is the BOS-881 collapse this branch exists to close.
			return nil, undeterminedInjection(acct.ID, provider,
				"could not evaluate the bound account's credential", err)
		}
		return nil, invalidInjection(acct.ID, provider, "credential injection failed", err)
	}
	if len(env) == 0 {
		// A nil-error materialization that produced no environment is not a
		// success. The spawn would proceed on the agent CLI's ambient login
		// while every surface still reports the account as bound and in force —
		// byte-identical to the BOS-973 silent degrade this package exists to
		// close, and reached without any error ever being returned. Refuse it
		// here rather than trusting each materializer to have refused it: this
		// is the one call site of the seam, so this check covers every
		// implementation of it, present and future.
		//
		// Undetermined, not invalid: an empty env means nothing in this path
		// ever looked at the credential, so telling the operator to
		// re-authenticate would be exactly the BOS-881 collapse the outcome
		// split exists to prevent.
		if recordLastUsed {
			r.recordInjectionFailure(ctx, acct.ID, provider, errEmptyMaterialization)
		}
		return nil, undeterminedInjection(acct.ID, provider,
			"the credential materializer returned no environment for the bound account", nil)
	}
	if recordLastUsed {
		r.clearInjectionFailure(ctx, acct.ID)
		if touchErr := r.reg.TouchLastUsed(ctx, acct.ID, now); touchErr != nil {
			r.logWarn().Err(touchErr).Str("account_id", acct.ID).
				Msg("account: failed to record last-used; continuing")
		}
	}
	return env, nil
}

// injectionHealthWriteTimeout bounds the detached injection-health writes. They
// run on context.WithoutCancel because the ctx they inherit is the SPAWN's, and
// the spawn is failing: when the materialize error IS a context error (a
// cancelled spawn, a plugin-RPC deadline, a hung agent plugin), reusing that ctx
// guarantees the health UPDATE fails too. The write would then be swallowed as a
// WARN and reproduce the exact BOS-973 silent degrade for the whole
// timeout/cancel class — the failure most likely to strand an operator with no
// signal. Detaching keeps request-scoped logging values while surviving the
// cancellation; the timeout keeps a detached write from outliving the daemon's
// interest in it.
const injectionHealthWriteTimeout = 5 * time.Second

// recordInjectionFailure durably marks the account unhealthy so the downgrade to
// the agent CLI's ambient login is visible on the surfaces an operator already
// reads. A registry without the optional seam, or a failing write, only logs:
// the spawn's outcome must never depend on the bookkeeping.
func (r *Resolver) recordInjectionFailure(ctx context.Context, accountID, provider string, cause error) {
	rec, ok := r.reg.(injectionHealthRecorder)
	if !ok {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), injectionHealthWriteTimeout)
	defer cancel()
	if err := rec.RecordInjectionFailure(writeCtx, accountID, cause.Error()); err != nil {
		r.logWarn().Err(err).Str("account_id", accountID).Str("provider", provider).
			Msg("account: failed to record credential-injection failure; continuing")
	}
}

// clearInjectionFailure withdraws a previously recorded injection failure so a
// transient failure self-heals on the next successful spawn. The store scopes
// the clear to its own prefix, so a genuine account-test failure survives.
func (r *Resolver) clearInjectionFailure(ctx context.Context, accountID string) {
	rec, ok := r.reg.(injectionHealthRecorder)
	if !ok {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), injectionHealthWriteTimeout)
	defer cancel()
	if err := rec.MarkInjectionFailureCleared(writeCtx, accountID); err != nil {
		r.logWarn().Err(err).Str("account_id", accountID).
			Msg("account: failed to clear credential-injection failure; continuing")
	}
}

// Label returns a human-friendly label for accountID. "" ⇒ "Unmanaged local credentials".
// A known account returns its Label. When the registry is unreachable or the
// account is unknown it falls back to a short ID prefix so it is never empty.
func (r *Resolver) Label(ctx context.Context, accountID string) (string, error) {
	if accountID == SystemDefaultAgentAccountID {
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

func (r *Resolver) logWarn() *zerolog.Event { return r.log.Warn() }
