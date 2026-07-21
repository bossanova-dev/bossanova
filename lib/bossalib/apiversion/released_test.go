package apiversion_test

import (
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

// TestReleasedVersions_WellFormed pins the golden ledger's own invariants: every
// entry is a valid YYYY-MM-DD version, entries are strictly increasing
// (append-only, newest last), and there are no duplicates. A malformed edit to
// ReleasedVersions fails here before it can weaken the append-only guard.
func TestReleasedVersions_WellFormed(t *testing.T) {
	if len(apiversion.ReleasedVersions) == 0 {
		t.Fatal("ReleasedVersions is empty; it must list every shipped version")
	}
	seen := make(map[apiversion.Version]struct{}, len(apiversion.ReleasedVersions))
	var prev apiversion.Version
	for i, v := range apiversion.ReleasedVersions {
		if _, err := apiversion.Parse(string(v)); err != nil {
			t.Errorf("ReleasedVersions[%d] = %q is not a valid version: %v", i, v, err)
		}
		if _, dup := seen[v]; dup {
			t.Errorf("ReleasedVersions[%d] = %q is a duplicate", i, v)
		}
		seen[v] = struct{}{}
		if i > 0 && string(v) <= string(prev) {
			t.Errorf("ReleasedVersions must be strictly increasing (append newest last): %q is not newer than %q", v, prev)
		}
		prev = v
	}
}

// TestDefaultRegistry_IsAppendOnlySupersetOfReleased is the guard: the shipped
// registry must support every version ever released. Removing or renaming a
// shipped version from DefaultRegistry() (or from version.go's constants)
// leaves its golden literal unsupported and fails this test — that is the
// structural block against silently regressing backwards compatibility.
func TestDefaultRegistry_IsAppendOnlySupersetOfReleased(t *testing.T) {
	if missing := apiversion.MissingReleased(apiversion.DefaultRegistry().All()); len(missing) != 0 {
		t.Errorf("DefaultRegistry() dropped previously-released version(s) %v — the registry is APPEND-ONLY: a shipped version must never be removed or renamed (see ReleasedVersions in released.go and docs/api-versioning.md)", missing)
	}
}

// TestMissingReleased_DetectsSimulatedRemoval gives the guard teeth: the same
// helper the guard relies on must actually report a dropped version. This is
// the failing-on-removal half of the append-only proof — it fails if
// MissingReleased ever stops detecting a regression.
func TestMissingReleased_DetectsSimulatedRemoval(t *testing.T) {
	// Simulate a registry that shipped only Baseline (the state that produced
	// the BOS-241 outage: a client requesting a newer version against a server
	// that dropped back to Baseline-only). Every later shipped version must be
	// reported missing, in ReleasedVersions order.
	shrunk := []apiversion.Version{apiversion.Baseline}
	missing := apiversion.MissingReleased(shrunk)
	want := []apiversion.Version{apiversion.V20260704, apiversion.V20260705, apiversion.V20260706, apiversion.V20260711, apiversion.V20260718}
	if len(missing) != len(want) {
		t.Fatalf("MissingReleased(%v) = %v, want %v — the append-only guard must detect every dropped shipped version",
			shrunk, missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("MissingReleased(%v)[%d] = %q, want %q", shrunk, i, missing[i], want[i])
		}
	}
}

// TestReleasedVersions_CoversNonExampleRegistryMembers keeps the golden ledger
// and the shipped registry in lockstep in the other direction: every version
// the registry currently ships must already be recorded as released. Appending
// a new version to DefaultRegistry() without also appending it to
// ReleasedVersions (the required lockstep step) fails here, so the ledger can
// never silently fall behind the registry.
func TestReleasedVersions_CoversNonExampleRegistryMembers(t *testing.T) {
	released := make(map[apiversion.Version]struct{}, len(apiversion.ReleasedVersions))
	for _, v := range apiversion.ReleasedVersions {
		released[v] = struct{}{}
	}
	for _, v := range apiversion.DefaultRegistry().All() {
		if _, ok := released[v]; !ok {
			t.Errorf("DefaultRegistry() ships %q but it is not in ReleasedVersions — appending a version to the registry must append it to the golden ledger too (append-only lockstep)", v)
		}
	}
}
