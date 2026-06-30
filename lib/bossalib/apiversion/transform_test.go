package apiversion_test

import (
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

// fakeChange is a test-only VersionChange used to verify ordering and
// boundary behaviour without touching the reference transform logic.
type fakeChange struct {
	version apiversion.Version
	// applied counts the number of times TransformResponse was called.
	applied int
	// callOrder records the global call sequence shared across all fakeChanges
	// in a test. Each call appends this change's version to the slice.
	callOrder *[]apiversion.Version
}

func (f *fakeChange) Version() apiversion.Version { return f.version }

func (f *fakeChange) TransformResponse(_ string, _ any) {
	f.applied++
	if f.callOrder != nil {
		*f.callOrder = append(*f.callOrder, f.version)
	}
}

// testRegistry builds a Registry with three synthetic versions for ordering tests.
func testRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{"2026-01-01", "2026-06-29", "2026-07-01"},
		"2026-07-01",
		"2026-01-01",
	)
	if err != nil {
		t.Fatalf("testRegistry: %v", err)
	}
	return reg
}

// exampleRegistry builds a 2-version registry (Baseline + V20260701) for
// exercising the ReferenceChange, which targets V20260701. DefaultRegistry no
// longer includes V20260701 (production safety), so tests that need the
// reference transform must construct this inline registry.
func exampleRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260701},
		apiversion.V20260701,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("exampleRegistry: %v", err)
	}
	return reg
}

func TestChanges_EmptyChain_NoOp(t *testing.T) {
	reg := testRegistry(t)
	changes, err := apiversion.NewChanges(reg)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	msg := &apiversion.RefMsg{Greeting: "hello"}
	changes.Apply("any.Method", msg, reg.Default())
	if msg.Greeting != "hello" {
		t.Errorf("Apply on empty chain mutated msg: %q", msg.Greeting)
	}
}

func TestChanges_PinnedToCurrent_ZeroTransforms(t *testing.T) {
	reg := testRegistry(t)
	fc := &fakeChange{version: "2026-06-29"}
	changes, err := apiversion.NewChanges(reg, fc)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	// Resolved to the most-recent version; the change at 2026-06-29 is NOT
	// newer than "2026-07-01", so zero transforms should run.
	changes.Apply("any.Method", nil, "2026-07-01")
	if fc.applied != 0 {
		t.Errorf("Apply pinned to Current: applied = %d, want 0", fc.applied)
	}
}

func TestChanges_ResolvedOneVersionBack_RunsExactlyReferenceTransform(t *testing.T) {
	// DefaultRegistry has only Baseline, so we use an inline 2-version registry
	// to exercise ReferenceChange, which targets V20260701.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "world"}
	// Resolved to Baseline → ReferenceChange (at V20260701) is newer → applied.
	changes.Apply("demo.Greet", msg, apiversion.Baseline)
	if msg.Greeting != "[v1] world" {
		t.Errorf("Apply(Baseline): Greeting = %q, want %q", msg.Greeting, "[v1] world")
	}
}

func TestChanges_PinnedToCurrent_ReferenceTransformNotApplied(t *testing.T) {
	// Use inline 2-version registry so ReferenceChange (at V20260701) is valid.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "world"}
	// Resolved to Current (V20260701) → ReferenceChange is NOT newer → not applied.
	changes.Apply("demo.Greet", msg, reg.Current())
	if msg.Greeting != "world" {
		t.Errorf("Apply(Current): Greeting = %q, want unmodified %q", msg.Greeting, "world")
	}
}

func TestChanges_PerMethodTargeting_UntargetedMethodUntouched(t *testing.T) {
	// Use inline 2-version registry so ReferenceChange (at V20260701) is valid.
	reg := exampleRegistry(t)
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	msg := &apiversion.RefMsg{Greeting: "original"}
	// Different method → ReferenceChange is a no-op even when version is old.
	changes.Apply("other.Method", msg, apiversion.Baseline)
	if msg.Greeting != "original" {
		t.Errorf("Apply(wrong method): Greeting = %q, want %q", msg.Greeting, "original")
	}
}

func TestChanges_OrderingNewestToOldest(t *testing.T) {
	reg := testRegistry(t)
	order := []apiversion.Version{}
	fc1 := &fakeChange{version: "2026-01-01", callOrder: &order}
	fc2 := &fakeChange{version: "2026-06-29", callOrder: &order}
	fc3 := &fakeChange{version: "2026-07-01", callOrder: &order}

	changes, err := apiversion.NewChanges(reg, fc1, fc2, fc3)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	// Resolved to "2026-01-01": only transforms at versions STRICTLY newer than
	// "2026-01-01" run. fc1 is at exactly "2026-01-01" (not strictly newer) so
	// it is skipped; fc2 and fc3 run in newest→oldest order: fc3, fc2.
	changes.Apply("any.Method", nil, "2026-01-01")

	if len(order) != 2 {
		t.Fatalf("expected 2 transforms, got %d: %v", len(order), order)
	}
	if order[0] != "2026-07-01" || order[1] != "2026-06-29" {
		t.Errorf("wrong order: %v; want [2026-07-01, 2026-06-29]", order)
	}
}

func TestChanges_BoundaryStrictlyNewer(t *testing.T) {
	reg := testRegistry(t)
	order := []apiversion.Version{}
	fc1 := &fakeChange{version: "2026-01-01", callOrder: &order}
	fc2 := &fakeChange{version: "2026-06-29", callOrder: &order}

	changes, err := apiversion.NewChanges(reg, fc1, fc2)
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}

	// Resolved to 2026-06-29: fc2 is NOT strictly newer than itself (equal),
	// so only fc1 (at 2026-01-01, which IS newer than... wait no, 2026-01-01
	// is OLDER than resolved 2026-06-29).
	// fc2.Version()="2026-06-29" is NOT strictly newer than resolved="2026-06-29" → not applied.
	// fc1.Version()="2026-01-01" is NOT strictly newer than resolved="2026-06-29" → not applied.
	changes.Apply("any.Method", nil, "2026-06-29")
	if len(order) != 0 {
		t.Errorf("resolved to 2026-06-29: expected 0 transforms, got %d: %v", len(order), order)
	}
}

func TestNewChanges_VersionNotInRegistry(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	fc := &fakeChange{version: "2025-01-01"} // not in registry
	_, err := apiversion.NewChanges(reg, fc)
	if err == nil {
		t.Fatal("NewChanges with unregistered version = nil error, want error")
	}
}

func TestNewChanges_OutOfOrder(t *testing.T) {
	reg := testRegistry(t)
	fc1 := &fakeChange{version: "2026-06-29"}
	fc2 := &fakeChange{version: "2026-01-01"} // older than fc1
	_, err := apiversion.NewChanges(reg, fc1, fc2)
	if err == nil {
		t.Fatal("NewChanges out-of-order = nil error, want error")
	}
}

func TestReferenceChange_Version(t *testing.T) {
	rc := apiversion.ReferenceChange{}
	if rc.Version() != apiversion.V20260701 {
		t.Errorf("ReferenceChange.Version() = %q, want %q", rc.Version(), apiversion.V20260701)
	}
}

func TestReferenceChange_WrongType_NoOp(t *testing.T) {
	rc := apiversion.ReferenceChange{}
	// Passing a non-*RefMsg should be a no-op (no panic).
	rc.TransformResponse("demo.Greet", "not a RefMsg")
}
