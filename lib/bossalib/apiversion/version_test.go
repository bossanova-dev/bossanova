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
	// Production registry is a single version: Current == Default == Baseline.
	// V20260701 is NOT a member (example/test use only).
	if reg.Current() != apiversion.Baseline {
		t.Errorf("DefaultRegistry().Current() = %q, want %q", reg.Current(), apiversion.Baseline)
	}
	if reg.Default() != apiversion.Baseline {
		t.Errorf("DefaultRegistry().Default() = %q, want %q", reg.Default(), apiversion.Baseline)
	}
	all := reg.All()
	if len(all) != 1 {
		t.Errorf("DefaultRegistry().All() len = %d, want 1", len(all))
	}
	if len(all) > 0 && all[0] != apiversion.Baseline {
		t.Errorf("DefaultRegistry().All()[0] = %q, want %q", all[0], apiversion.Baseline)
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
}
