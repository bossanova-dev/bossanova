package server

import (
	"runtime"
	"testing"

	"github.com/recurser/bossd/internal/detach/detachgate"
)

// thisPackageDir resolves the directory holding this package's non-test
// sources, which is what every gate in this file scans.
//
// A helper rather than two copies because the resolution is subtle: the working
// directory is not the package directory under every runner, and a gate that
// silently resolved the wrong one would scan cleanly forever. detachgate's doc
// carries the full account.
func thisPackageDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed, so this ratchet cannot locate its own package")
	}
	dir, err := detachgate.ResolvePackageDir(thisFile)
	if err != nil {
		t.Fatalf("locate the server package sources: %v", err)
	}
	return dir
}

// requireScanned is the invariant every gate in this file shares: a scan that
// read no files is not a clean result. A ratchet pointed at the wrong directory
// finds nothing and passes forever, which is indistinguishable from a clean
// package and makes the whole gate ornamental. detachgate returns the count for
// this reason; asserting it once here is what stops the two gates below from
// drifting apart on it.
func requireScanned(t *testing.T, scanned int) {
	t.Helper()
	if scanned == 0 {
		t.Fatal("scanned 0 Go files, so a clean result proves nothing — this ratchet is no longer reading its own package")
	}
	t.Logf("scanned %d non-test Go files in this package", scanned)
}

// TestNoRawDoChanCallInThisPackage fails on any raw singleflight DoChan call in
// this package's non-test sources.
//
// A DoChan flight body that does not recover its own panic is process-fatal:
// DoChan always seeds the call's result channels, so singleflight re-raises the
// panic with "go panic(e)" on a bare goroutine that no recover() anywhere in
// the daemon can reach. Join through detach.Flight instead — it always
// recovers, and detach.Group exposes no DoChan at all.
//
// This package is scanned because it declares a singleflight.Group. The scan
// reads THIS PACKAGE'S DIRECTORY ONLY — a repo-walking Go test cannot see the
// tree it would need from inside Bazel's per-target source sandbox. So the gate
// does not generalise: a FIFTH package that introduces singleflight needs its
// own copy of this file, its own `data` entry in the go_test (below), and
// detachgate's package doc records the other blind spot (a DoChan reached
// through a method value).
//
// The go_test staging matters: rules_go compiles the library's sources without
// putting them in the test's runfiles, so this package's BUILD.bazel carries a
// kept `data` glob of its non-test sources. Without it the scan finds nothing
// and this test fails on its own zero-file guard rather than passing blind.
func TestNoRawDoChanCallInThisPackage(t *testing.T) {
	t.Parallel()

	findings, scanned, err := detachgate.Scan(thisPackageDir(t))
	if err != nil {
		t.Fatalf("scan the server package: %v", err)
	}
	requireScanned(t, scanned)
	if len(findings) > 0 {
		t.Errorf("raw singleflight DoChan call(s) found: %v\n"+
			"a DoChan flight body that omits recover() crashes the whole daemon; "+
			"join through detach.Flight (services/bossd/internal/detach) instead", findings)
	}
}

// TestSwitchFlightAttachedIsAssignedOnlyByTests fails if any non-test source in
// this package ASSIGNS Server.switchFlightAttached.
//
// That field is BOS-951's test seam: set, it makes "a caller has attached to
// this chat's switch flight" observable, replacing four time.Sleep calls that
// stood in for the signal. It is handed to detach.WithAttachHook, whose own doc
// records the hazard — the hook runs synchronously on the switching goroutine
// inside detach.Flight, so a hook that blocks hangs the switch with no
// diagnostic. Production is safe from that for exactly one reason: nothing ever
// sets the field, so executeAccountSwitch passes detach.Flight no option and
// the flight is the unmodified primitive.
//
// The plan states that as an acceptance criterion backed by a grep a human is
// expected to run. This is that grep, run by the build instead — which is the
// same move dochan_ratchet_test.go above makes for raw DoChan, and for the same
// reason: a comment saying "test-only" is not a mechanism.
//
// It gates the ASSIGNMENT, not every mention of detach.WithAttachHook.
// executeAccountSwitch legitimately names the option — inside a nil check on
// this very field — so a scan that banned the identifier outright would have to
// carve out the one site that matters and would then gate nothing. Whether the
// field is ever set is the property the guarantee actually rests on.
//
// Same trade-off as the scan above: syntactic, this directory only, and reading
// the package's own sources staged through the go_test's `data` glob. It shares
// that scan's walk and its non-zero-file invariant rather than re-implementing
// them — detachgate.ScanFieldAssignments lists the two shapes it does not see
// (a positional composite literal, and an assignment through a pointer to the
// field) and pins both.
func TestSwitchFlightAttachedIsAssignedOnlyByTests(t *testing.T) {
	t.Parallel()

	const field = "switchFlightAttached"

	findings, scanned, err := detachgate.ScanFieldAssignments(thisPackageDir(t), field)
	if err != nil {
		t.Fatalf("scan the server package for assignments to %s: %v", field, err)
	}
	requireScanned(t, scanned)
	if len(findings) > 0 {
		t.Errorf("production assignment(s) to Server.%s found: %v\n"+
			"%s is a test-only seam; setting it in production code installs a detach.WithAttachHook "+
			"that runs synchronously inside detach.Flight, where a hook that blocks hangs the switch "+
			"with no diagnostic", field, findings, field)
	}
}
