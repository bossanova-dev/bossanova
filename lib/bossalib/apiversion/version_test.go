package apiversion_test

import (
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

func TestParse_Valid(t *testing.T) {
	cases := []string{
		"2026-06-29",
		"2026-07-01",
		"2000-01-01",
		"2099-12-31",
	}
	for _, s := range cases {
		v, err := apiversion.Parse(s)
		if err != nil {
			t.Errorf("Parse(%q) error = %v, want nil", s, err)
		}
		if v.String() != s {
			t.Errorf("Parse(%q).String() = %q, want %q", s, v.String(), s)
		}
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := []string{
		"",
		"garbage",
		"2026-13-01", // invalid month
		"2026-00-01", // invalid month
		"2026-01-32", // invalid day
		"2026/06/29", // wrong separator
		"20260629",   // no separator
		"06-29-2026", // wrong order
	}
	for _, s := range cases {
		_, err := apiversion.Parse(s)
		if err == nil {
			t.Errorf("Parse(%q) = nil error, want error", s)
		}
	}
}

func TestNewRegistry_Valid(t *testing.T) {
	all := []apiversion.Version{"2026-06-29", "2026-07-01"}
	reg, err := apiversion.NewRegistry(all, "2026-07-01", "2026-06-29")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if reg.Current() != "2026-07-01" {
		t.Errorf("Current() = %q, want 2026-07-01", reg.Current())
	}
	if reg.Default() != "2026-06-29" {
		t.Errorf("Default() = %q, want 2026-06-29", reg.Default())
	}
}

func TestNewRegistry_Empty(t *testing.T) {
	_, err := apiversion.NewRegistry(nil, "", "")
	if err == nil {
		t.Fatal("NewRegistry(nil) = nil error, want error")
	}
}

func TestNewRegistry_InvalidVersion(t *testing.T) {
	_, err := apiversion.NewRegistry(
		[]apiversion.Version{"not-a-date"},
		"not-a-date",
		"not-a-date",
	)
	if err == nil {
		t.Fatal("NewRegistry with invalid version = nil error, want error")
	}
}

func TestNewRegistry_NotStrictlyIncreasing(t *testing.T) {
	_, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-07-01", "2026-06-29"}, // reversed
		"2026-06-29",
		"2026-06-29",
	)
	if err == nil {
		t.Fatal("NewRegistry with reversed versions = nil error, want error")
	}
}

func TestNewRegistry_Duplicate(t *testing.T) {
	_, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-06-29", "2026-06-29"},
		"2026-06-29",
		"2026-06-29",
	)
	if err == nil {
		t.Fatal("NewRegistry with duplicate versions = nil error, want error")
	}
}

func TestNewRegistry_CurrentNotMember(t *testing.T) {
	_, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-06-29"},
		"2026-07-01", // not in list
		"2026-06-29",
	)
	if err == nil {
		t.Fatal("NewRegistry with current not in registry = nil error, want error")
	}
}

func TestNewRegistry_DefaultNotMember(t *testing.T) {
	_, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-06-29"},
		"2026-06-29",
		"2026-07-01", // not in list
	)
	if err == nil {
		t.Fatal("NewRegistry with default not in registry = nil error, want error")
	}
}

func TestRegistry_All_ReturnsCopy(t *testing.T) {
	all := []apiversion.Version{"2026-06-29", "2026-07-01"}
	reg, _ := apiversion.NewRegistry(all, "2026-07-01", "2026-06-29")
	got := reg.All()
	if len(got) != 2 {
		t.Fatalf("All() len = %d, want 2", len(got))
	}
	// Mutating the returned slice must not affect the registry.
	got[0] = "9999-01-01"
	got2 := reg.All()
	if got2[0] != "2026-06-29" {
		t.Errorf("All() returned a reference; mutation affected registry")
	}
}

func TestRegistry_IsSupported(t *testing.T) {
	reg, _ := apiversion.NewRegistry(
		[]apiversion.Version{"2026-06-29", "2026-07-01"},
		"2026-07-01",
		"2026-06-29",
	)
	if !reg.IsSupported("2026-06-29") {
		t.Error("IsSupported(Baseline) = false, want true")
	}
	if !reg.IsSupported("2026-07-01") {
		t.Error("IsSupported(V20260701) = false, want true")
	}
	if reg.IsSupported("2025-01-01") {
		t.Error("IsSupported(unknown) = true, want false")
	}
}

func TestRegistry_Newer(t *testing.T) {
	reg, _ := apiversion.NewRegistry(
		[]apiversion.Version{"2026-06-29", "2026-07-01"},
		"2026-07-01",
		"2026-06-29",
	)
	// V20260701 is newer than Baseline.
	if !reg.Newer("2026-07-01", "2026-06-29") {
		t.Error("Newer(V20260701, Baseline) = false, want true")
	}
	// The reverse is false.
	if reg.Newer("2026-06-29", "2026-07-01") {
		t.Error("Newer(Baseline, V20260701) = true, want false")
	}
	// Equal versions are not strictly newer.
	if reg.Newer("2026-06-29", "2026-06-29") {
		t.Error("Newer(Baseline, Baseline) = true, want false")
	}
	// Unknown version is not newer than anything.
	if reg.Newer("9999-01-01", "2026-06-29") {
		t.Error("Newer(unknown, Baseline) = true, want false")
	}
	if reg.Newer("2026-06-29", "9999-01-01") {
		t.Error("Newer(Baseline, unknown) = true, want false")
	}
}

func TestDefaultRegistry(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	if reg == nil {
		t.Fatal("DefaultRegistry() = nil")
	}
	// Production registry has thirteen versions ordered oldest→newest:
	// Baseline, V20260704, V20260705, V20260706, V20260711, V20260718,
	// V20260723, V20260803, V20260804, V20260812, V20260816, V20260820 and
	// V20260821. Current is V20260821 (newest behavior) while Default stays Baseline
	// (header-less callers pin to the oldest version). V20260701 is NOT a member
	// (example/test use only).
	if reg.Current() != apiversion.V20260821 {
		t.Errorf("DefaultRegistry().Current() = %q, want %q", reg.Current(), apiversion.V20260821)
	}
	if reg.Default() != apiversion.Baseline {
		t.Errorf("DefaultRegistry().Default() = %q, want %q", reg.Default(), apiversion.Baseline)
	}
	all := reg.All()
	if len(all) != 13 {
		t.Errorf("DefaultRegistry().All() len = %d, want 13", len(all))
	}
	if len(all) > 0 && all[0] != apiversion.Baseline {
		t.Errorf("DefaultRegistry().All()[0] = %q, want %q", all[0], apiversion.Baseline)
	}
	if !reg.IsSupported(apiversion.V20260704) {
		t.Errorf("DefaultRegistry().IsSupported(V20260704) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260705) {
		t.Errorf("DefaultRegistry().IsSupported(V20260705) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260706) {
		t.Errorf("DefaultRegistry().IsSupported(V20260706) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260711) {
		t.Errorf("DefaultRegistry().IsSupported(V20260711) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260718) {
		t.Errorf("DefaultRegistry().IsSupported(V20260718) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260723) {
		t.Errorf("DefaultRegistry().IsSupported(V20260723) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260803) {
		t.Errorf("DefaultRegistry().IsSupported(V20260803) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260804) {
		t.Errorf("DefaultRegistry().IsSupported(V20260804) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260812) {
		t.Errorf("DefaultRegistry().IsSupported(V20260812) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260816) {
		t.Errorf("DefaultRegistry().IsSupported(V20260816) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260820) {
		t.Errorf("DefaultRegistry().IsSupported(V20260820) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260821) {
		t.Errorf("DefaultRegistry().IsSupported(V20260821) = false, want true")
	}
	// V20260701 is an exported example const but must not be in the production registry.
	if reg.IsSupported(apiversion.V20260701) {
		t.Errorf("DefaultRegistry().IsSupported(V20260701) = true, want false (example version must not ship in production registry)")
	}
}

// TestDefaultRegistry_TwoVersionInlineWorks verifies that a 2-version registry
// (Baseline + V20260701) can be constructed inline, to keep the validation
// tests below passing even after DefaultRegistry was shrunk to 1 member.
func TestDefaultRegistry_TwoVersionInlineWorks(t *testing.T) {
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260701},
		apiversion.V20260701,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("inline 2-version registry: %v", err)
	}
	if reg.Current() != apiversion.V20260701 {
		t.Errorf("current = %q, want V20260701", reg.Current())
	}
	if reg.Default() != apiversion.Baseline {
		t.Errorf("default = %q, want Baseline", reg.Default())
	}
	if len(reg.All()) != 2 {
		t.Errorf("All() len = %d, want 2", len(reg.All()))
	}
}

func TestConstants(t *testing.T) {
	if apiversion.Baseline.String() != "2026-06-29" {
		t.Errorf("Baseline = %q, want 2026-06-29", apiversion.Baseline)
	}
	if apiversion.V20260701.String() != "2026-07-01" {
		t.Errorf("V20260701 = %q, want 2026-07-01", apiversion.V20260701)
	}
	if apiversion.V20260704.String() != "2026-07-04" {
		t.Errorf("V20260704 = %q, want 2026-07-04", apiversion.V20260704)
	}
	if apiversion.V20260705.String() != "2026-07-05" {
		t.Errorf("V20260705 = %q, want 2026-07-05", apiversion.V20260705)
	}
	if apiversion.V20260706.String() != "2026-07-06" {
		t.Errorf("V20260706 = %q, want 2026-07-06", apiversion.V20260706)
	}
}

// TestDefaultRegistry_CurrentIsNewestReleasedLiteral pins Current to the RAW
// date literal of the newest shipped version, for the same reason
// ReleasedVersions stores raw literals (see released.go): referencing the
// V2026xxxx constant here would let the constant and the registry be re-pointed
// at a different date together and the guard would never notice. Bumping the API
// version is therefore a deliberate two-line edit — the registry and this
// literal — not an accident.
func TestDefaultRegistry_CurrentIsNewestReleasedLiteral(t *testing.T) {
	const wantCurrent = apiversion.Version("2026-08-21")
	if got := apiversion.DefaultRegistry().Current(); got != wantCurrent {
		t.Errorf("DefaultRegistry().Current() = %q, want %q", got, wantCurrent)
	}
	released := apiversion.ReleasedVersions
	if len(released) == 0 {
		t.Fatal("ReleasedVersions is empty")
	}
	if got := released[len(released)-1]; got != wantCurrent {
		t.Errorf("newest ReleasedVersions entry = %q, want %q — Current and the golden ledger must be appended in lockstep", got, wantCurrent)
	}
}
