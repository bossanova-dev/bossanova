package apiversion_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

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
	// Production registry has twenty-eight versions ordered oldest→newest:
	// Baseline, V20260704, V20260705, V20260706, V20260711, V20260718,
	// V20260723, V20260803, V20260804, V20260812, V20260816, V20260820,
	// V20260821, V20260825, V20260902, V20260903, V20260904, V20260905,
	// V20260906, V20260907, V20260908, V20260909, V20260910, V20260911,
	// V20260912, V20260913, V20260914 and V20260915. Current is V20260915
	// (newest behavior) while Default stays Baseline (header-less callers pin
	// to the oldest version).
	// V20260701 is NOT a member (example/test use only).
	if reg.Current() != apiversion.V20260915 {
		t.Errorf("DefaultRegistry().Current() = %q, want %q", reg.Current(), apiversion.V20260915)
	}
	if reg.Default() != apiversion.Baseline {
		t.Errorf("DefaultRegistry().Default() = %q, want %q", reg.Default(), apiversion.Baseline)
	}
	all := reg.All()
	if len(all) != 28 {
		t.Errorf("DefaultRegistry().All() len = %d, want 28", len(all))
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
	if !reg.IsSupported(apiversion.V20260825) {
		t.Errorf("DefaultRegistry().IsSupported(V20260825) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260902) {
		t.Errorf("DefaultRegistry().IsSupported(V20260902) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260903) {
		t.Errorf("DefaultRegistry().IsSupported(V20260903) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260907) {
		t.Errorf("DefaultRegistry().IsSupported(V20260907) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260908) {
		t.Errorf("DefaultRegistry().IsSupported(V20260908) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260909) {
		t.Errorf("DefaultRegistry().IsSupported(V20260909) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260910) {
		t.Errorf("DefaultRegistry().IsSupported(V20260910) = false, want true")
	}
	if !reg.IsSupported(apiversion.V20260911) {
		t.Errorf("DefaultRegistry().IsSupported(V20260911) = false, want true")
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
	if apiversion.V20260902.String() != "2026-09-02" {
		t.Errorf("V20260902 = %q, want 2026-09-02", apiversion.V20260902)
	}
	if apiversion.V20260903.String() != "2026-09-03" {
		t.Errorf("V20260903 = %q, want 2026-09-03", apiversion.V20260903)
	}
	if apiversion.V20260905.String() != "2026-09-05" {
		t.Errorf("V20260905 = %q, want 2026-09-05", apiversion.V20260905)
	}
	if apiversion.V20260906.String() != "2026-09-06" {
		t.Errorf("V20260906 = %q, want 2026-09-06", apiversion.V20260906)
	}
	if apiversion.V20260907.String() != "2026-09-07" {
		t.Errorf("V20260907 = %q, want 2026-09-07", apiversion.V20260907)
	}
	if apiversion.V20260908.String() != "2026-09-08" {
		t.Errorf("V20260908 = %q, want 2026-09-08", apiversion.V20260908)
	}
	if apiversion.V20260909.String() != "2026-09-09" {
		t.Errorf("V20260909 = %q, want 2026-09-09", apiversion.V20260909)
	}
	if apiversion.V20260910.String() != "2026-09-10" {
		t.Errorf("V20260910 = %q, want 2026-09-10", apiversion.V20260910)
	}
	if apiversion.V20260911.String() != "2026-09-11" {
		t.Errorf("V20260911 = %q, want 2026-09-11", apiversion.V20260911)
	}
	if apiversion.V20260912.String() != "2026-09-12" {
		t.Errorf("V20260912 = %q, want 2026-09-12", apiversion.V20260912)
	}
	if apiversion.V20260913.String() != "2026-09-13" {
		t.Errorf("V20260913 = %q, want 2026-09-13", apiversion.V20260913)
	}
}

// TestDefaultRegistry_CurrentIsRawLiteral pins Current independently of its
// named constant. Current may be one trailing unreleased contract; released.go
// remains the immutable ledger of versions that have actually shipped.
func TestDefaultRegistry_CurrentIsRawLiteral(t *testing.T) {
	const wantCurrent = apiversion.Version("2026-09-15")
	if got := apiversion.DefaultRegistry().Current(); got != wantCurrent {
		t.Errorf("DefaultRegistry().Current() = %q, want %q", got, wantCurrent)
	}
	released := apiversion.ReleasedVersions
	if len(released) == 0 {
		t.Fatal("ReleasedVersions is empty")
	}
	const wantNewestReleased = apiversion.Version("2026-09-14")
	if got := released[len(released)-1]; got != wantNewestReleased {
		t.Errorf("newest ReleasedVersions entry = %q, want %q", got, wantNewestReleased)
	}
}

// TestIsCrossOrgCronReads pins the BOS-1158 handler boundary: V20260909 and
// older keep the claimed-organization result, while V20260910 opens the union.
func TestIsCrossOrgCronReads(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	assertResolved := func(t *testing.T, version apiversion.Version) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		req.Header().Set(apiversion.HeaderName, version.String())
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsCrossOrgCronReads(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", version, err)
		}
		return got
	}

	for _, version := range reg.All() {
		want := !reg.Newer(apiversion.V20260910, version)
		if got := assertResolved(t, version); got != want {
			t.Errorf("IsCrossOrgCronReads(%s) = %v, want %v", version, got, want)
		}
	}
	if got := assertResolved(t, apiversion.V20260909); got {
		t.Error("IsCrossOrgCronReads(V20260909) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260910); !got {
		t.Error("IsCrossOrgCronReads(V20260910) = false, want true")
	}
}

func TestIsCrossOrgSessionReads(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)
	assertResolved := func(t *testing.T, version apiversion.Version) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		req.Header().Set(apiversion.HeaderName, version.String())
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsCrossOrgSessionReads(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", version, err)
		}
		return got
	}
	if got := assertResolved(t, apiversion.V20260911); got {
		t.Fatal("IsCrossOrgSessionReads(V20260911) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260912); !got {
		t.Fatal("IsCrossOrgSessionReads(V20260912) = false, want true")
	}
}

func TestIsInvitationRevocation(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	assertResolved := func(t *testing.T, version apiversion.Version) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		req.Header().Set(apiversion.HeaderName, version.String())
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsInvitationRevocation(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", version, err)
		}
		return got
	}

	if got := apiversion.IsInvitationRevocation(context.Background()); got {
		t.Fatal("IsInvitationRevocation(background) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260909); got {
		t.Fatal("IsInvitationRevocation(V20260909) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260910); got {
		t.Fatal("IsInvitationRevocation(V20260910) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260911); !got {
		t.Fatal("IsInvitationRevocation(V20260911) = false, want true")
	}
}

// TestIsCrossOrgSessionCommands pins the handler-gate boundary: only clients
// at or after V20260908 may route session commands through another member org.
func TestIsCrossOrgSessionCommands(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	assertResolved := func(t *testing.T, header string) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		if header != "" {
			req.Header().Set(apiversion.HeaderName, header)
		}
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsCrossOrgSessionCommands(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", header, err)
		}
		return got
	}

	if got := apiversion.IsCrossOrgSessionCommands(context.Background()); got {
		t.Errorf("IsCrossOrgSessionCommands(background) = true, want false")
	}
	for _, version := range reg.All() {
		want := !reg.Newer(apiversion.V20260908, version)
		if got := assertResolved(t, version.String()); got != want {
			t.Errorf("IsCrossOrgSessionCommands(%s) = %v, want %v", version, got, want)
		}
	}
	if got := assertResolved(t, ""); got {
		t.Errorf("IsCrossOrgSessionCommands(no header) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260906.String()); got {
		t.Errorf("IsCrossOrgSessionCommands(V20260906) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260907.String()); got {
		t.Errorf("IsCrossOrgSessionCommands(V20260907) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260908.String()); !got {
		t.Errorf("IsCrossOrgSessionCommands(V20260908) = false, want true")
	}
}

func TestIsOrgScopedVisibility(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	assertResolved := func(t *testing.T, header string) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		if header != "" {
			req.Header().Set(apiversion.HeaderName, header)
		}
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsOrgScopedVisibility(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", header, err)
		}
		return got
	}

	if got := apiversion.IsOrgScopedVisibility(context.Background()); got {
		t.Errorf("IsOrgScopedVisibility(background) = true, want false")
	}
	for _, version := range reg.All() {
		want := !reg.Newer(apiversion.V20260902, version)
		if got := assertResolved(t, version.String()); got != want {
			t.Errorf("IsOrgScopedVisibility(%s) = %v, want %v", version, got, want)
		}
	}
	if got := assertResolved(t, ""); got {
		t.Errorf("IsOrgScopedVisibility(no header) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260825.String()); got {
		t.Errorf("IsOrgScopedVisibility(V20260825) = true, want false")
	}
}

func TestIsCrossOrgDaemonReads(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)
	for _, version := range reg.All() {
		version := version
		t.Run(version.String(), func(t *testing.T) {
			req := connect.NewRequest(&struct{}{})
			req.Header().Set(apiversion.HeaderName, version.String())
			var got bool
			next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
				got = apiversion.IsCrossOrgDaemonReads(ctx)
				return connect.NewResponse(&struct{}{}), nil
			}
			if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			want := !reg.Newer(apiversion.V20260905, version)
			if got != want {
				t.Fatalf("IsCrossOrgDaemonReads(%s) = %v, want %v", version, got, want)
			}
		})
	}
	if apiversion.IsCrossOrgDaemonReads(context.Background()) {
		t.Fatal("IsCrossOrgDaemonReads(background) = true, want false")
	}
}

// TestIsMemberOrgCloudAccess is the V20260906 half of the same handler-gate
// proof: the gate must open for exactly the versions at or after V20260906 and
// for no others, so a client that negotiated an older version keeps being judged
// from its claimed organization alone.
func TestIsMemberOrgCloudAccess(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	assertResolved := func(t *testing.T, header string) bool {
		t.Helper()
		req := connect.NewRequest(&struct{}{})
		if header != "" {
			req.Header().Set(apiversion.HeaderName, header)
		}
		var got bool
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			got = apiversion.IsMemberOrgCloudAccess(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", header, err)
		}
		return got
	}

	if got := apiversion.IsMemberOrgCloudAccess(context.Background()); got {
		t.Errorf("IsMemberOrgCloudAccess(background) = true, want false")
	}
	for _, version := range reg.All() {
		want := !reg.Newer(apiversion.V20260906, version)
		if got := assertResolved(t, version.String()); got != want {
			t.Errorf("IsMemberOrgCloudAccess(%s) = %v, want %v", version, got, want)
		}
	}
	if got := assertResolved(t, ""); got {
		t.Errorf("IsMemberOrgCloudAccess(no header) = true, want false")
	}
	// One version back must still be closed — the boundary the gate exists for.
	if got := assertResolved(t, apiversion.V20260905.String()); got {
		t.Errorf("IsMemberOrgCloudAccess(V20260905) = true, want false")
	}
	if got := assertResolved(t, apiversion.V20260906.String()); !got {
		t.Errorf("IsMemberOrgCloudAccess(V20260906) = false, want true")
	}
}

// gateWarnRecord is one captured unresolved-gate-read report.
type gateWarnRecord struct {
	level     slog.Level
	gate      string
	caller    string
	answering string
}

// gateWarnRecorder is a slog.Handler that captures only the records carrying a
// "gate" attribute, so unrelated logging from elsewhere in the process cannot
// make either arm of TestUnresolvedGateRead pass or fail by accident.
type gateWarnRecorder struct {
	mu      sync.Mutex
	records []gateWarnRecord
}

func (r *gateWarnRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *gateWarnRecorder) Handle(_ context.Context, record slog.Record) error {
	captured := gateWarnRecord{level: record.Level}
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "gate":
			captured.gate = attr.Value.String()
		case "caller":
			captured.caller = attr.Value.String()
		case "answering":
			captured.answering = attr.Value.String()
		}
		return true
	})
	if captured.gate == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, captured)
	return nil
}

func (r *gateWarnRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *gateWarnRecorder) WithGroup(string) slog.Handler { return r }

func (r *gateWarnRecorder) snapshot() []gateWarnRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gateWarnRecord(nil), r.records...)
}

// installGateWarnRecorder swaps in a capturing default logger for the duration
// of the test. slog.SetDefault is process-global, so the prior default is
// restored on cleanup and this test must not run in parallel with anything that
// asserts on logs.
func installGateWarnRecorder(t *testing.T) *gateWarnRecorder {
	t.Helper()
	prior := slog.Default()
	recorder := &gateWarnRecorder{}
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(prior) })
	return recorder
}

// TestUnresolvedGateRead pins the BOS-1235 discriminator end to end: a version
// gate evaluated off a context the interceptor never touched reports itself
// exactly once per call site, and the same gate evaluated behind the
// interceptor reports nothing — including for a header-less client, whose
// resolved version is Baseline and therefore byte-identical to the fallback.
// Both arms run in one test so the silent arm is demonstrably able to fire.
func TestUnresolvedGateRead(t *testing.T) {
	apiversion.ResetUnresolvedGateReadsForTest()
	recorder := installGateWarnRecorder(t)

	// Fire arm: a bare context, the shape an HTTP middleware or raw route has.
	// Called twice from this one source line to pin the per-site dedupe.
	for range 2 {
		if got := apiversion.IsCrossOrgSessionCommands(context.Background()); got {
			t.Fatalf("IsCrossOrgSessionCommands(bare ctx) = true, want false")
		}
	}

	fired := recorder.snapshot()
	if len(fired) != 1 {
		t.Fatalf("unresolved gate read emitted %d records, want exactly 1: %+v", len(fired), fired)
	}
	if fired[0].level != slog.LevelWarn {
		t.Errorf("unresolved gate read level = %v, want %v", fired[0].level, slog.LevelWarn)
	}
	if fired[0].gate != "IsCrossOrgSessionCommands" {
		t.Errorf("unresolved gate read gate = %q, want %q", fired[0].gate, "IsCrossOrgSessionCommands")
	}
	if !strings.Contains(fired[0].caller, "version_test.go:") {
		t.Errorf("unresolved gate read caller = %q, want a version_test.go file:line", fired[0].caller)
	}
	if want := apiversion.DefaultRegistry().Default().String(); fired[0].answering != want {
		t.Errorf("unresolved gate read answering = %q, want %q", fired[0].answering, want)
	}

	// Quiet arm: the same predicate behind the interceptor. The header-less row
	// is the load-bearing one — it resolves to Baseline, the same value the
	// fallback returns, so a report here would prove the discriminator was the
	// version rather than the presence of the context key.
	interceptor := apiversion.Interceptor(apiversion.DefaultRegistry(), nil)
	for _, header := range []string{"", apiversion.V20260908.String(), apiversion.Baseline.String()} {
		req := connect.NewRequest(&struct{}{})
		if header != "" {
			req.Header().Set(apiversion.HeaderName, header)
		}
		next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			apiversion.IsCrossOrgSessionCommands(ctx)
			return connect.NewResponse(&struct{}{}), nil
		}
		if _, err := interceptor.WrapUnary(next)(context.Background(), req); err != nil {
			t.Fatalf("WrapUnary(%q): %v", header, err)
		}
	}

	if after := recorder.snapshot(); len(after) != 1 {
		t.Fatalf("interceptor-resolved gate reads emitted %d extra records, want 0: %+v", len(after)-1, after[1:])
	}
}

// TestUnresolvedGateReadCoversEveryPredicate pins that the report is wired into
// all nine handler-level gates, not only the one TestUnresolvedGateRead
// exercises, and that each names itself.
func TestUnresolvedGateReadCoversEveryPredicate(t *testing.T) {
	apiversion.ResetUnresolvedGateReadsForTest()
	recorder := installGateWarnRecorder(t)

	predicates := map[string]func(context.Context) bool{
		"IsOrgScopedVisibility":     apiversion.IsOrgScopedVisibility,
		"IsCrossOrgRepoReads":       apiversion.IsCrossOrgRepoReads,
		"IsCrossOrgSessionReads":    apiversion.IsCrossOrgSessionReads,
		"IsCrossOrgCronReads":       apiversion.IsCrossOrgCronReads,
		"IsCrossOrgFleetReads":      apiversion.IsCrossOrgFleetReads,
		"IsInvitationRevocation":    apiversion.IsInvitationRevocation,
		"IsCrossOrgSessionCommands": apiversion.IsCrossOrgSessionCommands,
		"IsCrossOrgDaemonReads":     apiversion.IsCrossOrgDaemonReads,
		"IsMemberOrgCloudAccess":    apiversion.IsMemberOrgCloudAccess,
	}
	for _, predicate := range predicates {
		predicate(context.Background())
	}

	seen := make(map[string]struct{})
	for _, record := range recorder.snapshot() {
		seen[record.gate] = struct{}{}
	}
	for name := range predicates {
		if _, ok := seen[name]; !ok {
			t.Errorf("%s did not report an unresolved gate read", name)
		}
	}
}
