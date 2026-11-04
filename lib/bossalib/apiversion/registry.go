package apiversion

import (
	"fmt"
	"slices"
	"time"
)

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
// the current (latest known) version, and the default version used when a
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

// Current returns the latest known API version.
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
