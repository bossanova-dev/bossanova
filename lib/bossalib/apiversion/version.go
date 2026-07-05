// Package apiversion implements Stripe-style date-based API versioning for the
// OrchestratorService Connect/gRPC API served by bosso.
//
// Clients send a "Bossanova-Version" request header containing a YYYY-MM-DD
// identifier. The server validates it against a Registry, stores the resolved
// version in the request context via Interceptor, and applies an ordered chain
// of VersionChange transforms so older clients receive the response shape they
// expect. A request resolved to the current version runs zero transforms,
// keeping the hot path allocation-free.
//
// To add a new API version and behavior change, append a new Version constant,
// register it in DefaultRegistry, bump Current, and add the matching
// VersionChange to the Changes list wired into bosso. See docs/api-versioning.md
// for the full procedure.
package apiversion

import (
	"fmt"
	"slices"
	"time"
)

// Version is a validated YYYY-MM-DD API version identifier.
type Version string

// Baseline is the launch baseline API version. It is the oldest supported
// version and the default assumed for header-less requests.
const Baseline Version = "2026-06-29"

// V20260701 is a TEST-ONLY API version. It was introduced alongside the
// reference transform (ReferenceChange) to exercise the transform framework
// end-to-end while keeping this package self-contained. It is intentionally NOT
// a member of the production registry (see DefaultRegistry).
const V20260701 Version = "2026-07-01"

// V20260704 shipped the first live down-convert transform (OrphanedStateChange):
// the OrchestratorService began serving SessionState SESSION_STATE_ORPHANED on
// Session.state, a new terminal state for headless runs killed by a daemon
// restart. Clients pinned to Baseline (which never saw ORPHANED) are
// down-converted to the prior observable behavior, SESSION_STATE_IMPLEMENTING_PLAN.
const V20260704 Version = "2026-07-04"

// V20260705 is the current production API version. It ships the
// AgentAuthFailedChange transform: the OrchestratorService began serving the
// AttentionReason value ATTENTION_REASON_AGENT_AUTH_FAILED on
// Session.attention_status.reason, a new attention reason surfaced when an
// agent's pane shows the login-required terminal shape ("Not logged in" /
// "Please run /login"). Before this detection existed such a session showed no
// auth-specific attention (it "just went quiet"), so clients pinned to an older
// version (which never saw this reason) are down-converted back to no attention.
const V20260705 Version = "2026-07-05"

// Parse validates and returns a Version from a strict YYYY-MM-DD calendar date
// string. It rejects strings that are not valid calendar dates (e.g. "2026-13-01")
// or that use any other format.
func Parse(s string) (Version, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("apiversion: invalid version %q: must be a valid YYYY-MM-DD calendar date", s)
	}
	// Verify the round-trip to reject any non-canonical representation.
	if t.Format("2006-01-02") != s {
		return "", fmt.Errorf("apiversion: invalid version %q: must be a valid YYYY-MM-DD calendar date", s)
	}
	return Version(s), nil
}

// String implements fmt.Stringer.
func (v Version) String() string { return string(v) }

// Registry holds an ordered (oldest→newest) slice of registered API versions,
// the current (latest released) version, and the default version used when a
// request carries no Bossanova-Version header.
//
// By policy the default is the oldest supported version so that a header-less
// caller never silently shifts onto newer behavior — matching the Stripe intent
// of "pin to the version you started on."
type Registry struct {
	all     []Version
	current Version
	def     Version
}

// NewRegistry constructs a Registry, validating that:
//   - all is non-empty
//   - every entry is a valid YYYY-MM-DD calendar date
//   - entries are strictly increasing (no duplicates)
//   - current and def are both members of all
func NewRegistry(all []Version, current, def Version) (*Registry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("apiversion: registry must contain at least one version")
	}
	members := make(map[Version]struct{}, len(all))
	var prev Version
	for i, v := range all {
		if _, err := Parse(string(v)); err != nil {
			return nil, err
		}
		if _, dup := members[v]; dup {
			return nil, fmt.Errorf("apiversion: duplicate version %q in registry", v)
		}
		// Compare against the previous element via a tracked variable rather
		// than re-indexing all[i-1] so gosec's G602 bounds analysis stays happy.
		if i > 0 && string(v) <= string(prev) {
			return nil, fmt.Errorf("apiversion: versions must be strictly increasing: %q is not newer than %q", v, prev)
		}
		members[v] = struct{}{}
		prev = v
	}
	if _, ok := members[current]; !ok {
		return nil, fmt.Errorf("apiversion: current version %q is not in the registry", current)
	}
	if _, ok := members[def]; !ok {
		return nil, fmt.Errorf("apiversion: default version %q is not in the registry", def)
	}
	cp := make([]Version, len(all))
	copy(cp, all)
	return &Registry{all: cp, current: current, def: def}, nil
}

// All returns a copy of all registered versions, ordered oldest→newest.
func (r *Registry) All() []Version {
	cp := make([]Version, len(r.all))
	copy(cp, r.all)
	return cp
}

// Current returns the latest released API version.
func (r *Registry) Current() Version { return r.current }

// Default returns the version assumed when a request carries no
// Bossanova-Version header. By policy this is the oldest supported version.
func (r *Registry) Default() Version { return r.def }

// IsSupported reports whether v is a member of the registry.
func (r *Registry) IsSupported(v Version) bool {
	return slices.Contains(r.all, v)
}

// Newer reports whether a is strictly newer than b. Both a and b must be
// members of the registry; if either is unknown Newer returns false and callers
// should not rely on the ordering of unknown versions.
func (r *Registry) Newer(a, b Version) bool {
	if !r.IsSupported(a) || !r.IsSupported(b) {
		return false
	}
	// YYYY-MM-DD strings compare correctly under lexicographic ordering because
	// they are zero-padded and most-significant field first.
	return string(a) > string(b)
}

// DefaultRegistry returns a Registry seeded with the known production API
// versions, ordered oldest→newest: Baseline, V20260704 and V20260705. Current
// is V20260705 (the newest released behavior) while Default stays Baseline (the
// oldest supported version), so a header-less caller is pinned to Baseline and
// is down-converted by ProductionChanges, and a client that negotiates
// V20260705 runs zero transforms.
//
// V20260701 is intentionally NOT a member of the production registry — it
// exists as an exported const for example and test use only (it is exercised
// by the transform framework tests and the reference ReferenceChange).
//
// To add a new API version, append it here, set it as Current, and add the
// matching VersionChange to ProductionChanges. See docs/api-versioning.md for
// the full procedure.
func DefaultRegistry() *Registry {
	reg, err := NewRegistry(
		[]Version{Baseline, V20260704, V20260705},
		V20260705,
		Baseline,
	)
	if err != nil {
		panic("apiversion: DefaultRegistry is invalid: " + err.Error())
	}
	return reg
}
