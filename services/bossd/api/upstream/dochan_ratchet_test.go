package upstream

import (
	"runtime"
	"testing"

	"github.com/recurser/bossd/internal/detach/detachgate"
)

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

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed, so this ratchet cannot locate its own package")
	}
	dir, err := detachgate.ResolvePackageDir(thisFile)
	if err != nil {
		t.Fatalf("locate the upstream package sources: %v", err)
	}
	findings, scanned, err := detachgate.Scan(dir)
	if err != nil {
		t.Fatalf("scan the upstream package: %v", err)
	}
	// Fail on an empty scan: a ratchet pointed at the wrong directory finds
	// nothing and passes forever, which is indistinguishable from a clean
	// package and makes the whole gate ornamental.
	if scanned == 0 {
		t.Fatal("scanned 0 Go files, so a clean result proves nothing — this ratchet is no longer reading its own package")
	}
	t.Logf("scanned %d non-test Go files in this package", scanned)
	if len(findings) > 0 {
		t.Errorf("raw singleflight DoChan call(s) found: %v\n"+
			"a DoChan flight body that omits recover() crashes the whole daemon; "+
			"join through detach.Flight (services/bossd/internal/detach) instead", findings)
	}
}
