package detachgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes files into a fresh directory and returns its path. Fixtures
// are built at run time rather than committed under testdata so a deliberately
// unparseable source file cannot be picked up by gofmt, the pre-commit
// formatter, or a lint pass that would refuse to leave it broken.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}
	return dir
}

const violation = `package fixture

type g struct{}

func (g) DoChan(string, func() (any, error)) <-chan struct{} { return nil }

func joinTheGroup() {
	var group g
	_ = group.DoChan("k", func() (any, error) { return nil, nil })
}
`

func TestScanReportsARawDoChanCallWithFileLineAndFunction(t *testing.T) {
	t.Parallel()
	findings, scanned, err := Scan(fixture(t, map[string]string{"flight.go": violation}))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one", findings)
	}
	if got, want := findings[0], "flight.go:9:joinTheGroup"; got != want {
		t.Errorf("finding = %q, want %q — a finding must name file, line and enclosing function", got, want)
	}
}

func TestScanIgnoresADoOnlyCallSite(t *testing.T) {
	t.Parallel()
	const doOnly = `package fixture

type g struct{}

func (g) Do(string, func() (any, error)) (any, error, bool) { return nil, nil, false }

func joinTheGroup() {
	var group g
	_, _, _ = group.Do("k", func() (any, error) { return nil, nil })
}
`
	findings, scanned, err := Scan(fixture(t, map[string]string{"flight.go": doOnly}))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none — Do carries no panic obligation and must not trip the ratchet", findings)
	}
}

func TestScanIgnoresTestFiles(t *testing.T) {
	t.Parallel()
	// A ratchet test may legitimately reference DoChan in a comment or a
	// fixture of its own; scanning _test.go files would make the gate
	// self-tripping.
	findings, scanned, err := Scan(fixture(t, map[string]string{"flight_test.go": violation}))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 0 {
		t.Fatalf("filesScanned = %d, want 0 — _test.go files are excluded", scanned)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none from a _test.go file", findings)
	}
}

func TestScanReportsZeroFilesForAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	findings, scanned, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 0 {
		t.Fatalf("filesScanned = %d, want 0", scanned)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
	// The point of the count: without it this result is indistinguishable from
	// a genuinely clean package, so a mis-scoped ratchet would pass forever.
}

func TestScanSurfacesAnUnparseableFileAsAnError(t *testing.T) {
	t.Parallel()
	dir := fixture(t, map[string]string{"broken.go": "package fixture\n\nfunc oops( {\n"})
	findings, _, err := Scan(dir)
	if err == nil {
		t.Fatal("an unparseable file scanned clean; a parse failure must never read as an absence of findings")
	}
	if !strings.Contains(err.Error(), "broken.go") {
		t.Errorf("error = %v, want it to name the file that failed to parse", err)
	}
	if findings != nil {
		t.Errorf("findings = %v, want none alongside an error", findings)
	}
}

func TestScanErrorsOnAMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, _, err := Scan(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("scanning a missing directory succeeded; a mis-scoped ratchet must fail loudly")
	}
}

func TestScanDoesNotSeeADoChanReachedThroughAMethodValue(t *testing.T) {
	t.Parallel()
	// A PINNED LIMIT, not a bug. Scan matches a CallExpr whose Fun is a
	// selector named DoChan; a method value defeats it. Recorded here so the
	// blind spot is a documented property of the ratchet rather than something
	// a future reader discovers by getting away with it. What actually makes
	// the converted call site safe is detach.Group, which exposes no DoChan at
	// all, so this escape has no target inside the package the gate protects.
	const methodValue = `package fixture

type g struct{}

func (g) DoChan(string, func() (any, error)) <-chan struct{} { return nil }

func joinTheGroup() {
	var group g
	join := group.DoChan
	_ = join("k", func() (any, error) { return nil, nil })
}
`
	findings, scanned, err := Scan(fixture(t, map[string]string{"flight.go": methodValue}))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v — the method-value blind spot has been closed; update this test and "+
			"the package doc rather than deleting the pin", findings)
	}
}

func TestScanFindsEveryViolationAcrossFiles(t *testing.T) {
	t.Parallel()
	findings, scanned, err := Scan(fixture(t, map[string]string{
		"a.go": violation,
		"b.go": violation,
	}))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 2 {
		t.Fatalf("filesScanned = %d, want 2", scanned)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %v, want one per file — the scan must not stop at the first", findings)
	}
}

func TestResolvePackageDirPrefersTheCallerFilesOwnDirectory(t *testing.T) {
	// Not parallel: the fallback case below uses t.Chdir, and this suite shares
	// one process working directory.
	dir := fixture(t, map[string]string{"flight.go": violation})
	got, err := ResolvePackageDir(filepath.Join(dir, "dochan_ratchet_test.go"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != dir {
		t.Errorf("resolved %q, want the caller file's own directory %q", got, dir)
	}
}

func TestResolvePackageDirFallsBackToTheWorkingDirectory(t *testing.T) {
	// The Bazel shape: the caller file's recorded path does not resolve from the
	// runner's working directory, but the working directory IS the package. The
	// caller path is unreachable and yet names the same package directory, which
	// is what makes "." admissible.
	dir := fixture(t, map[string]string{"flight.go": violation})
	t.Chdir(dir)
	callerFile := filepath.Join("nowhere/that", filepath.Base(dir), "dochan_ratchet_test.go")
	got, err := ResolvePackageDir(callerFile)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "." {
		t.Errorf("resolved %q, want the working directory", got)
	}
}

// TestResolvePackageDirRejectsADirectoryThatIsNotTheCallersPackage is the guard
// the whole ratchet rests on. A candidate that merely HOLDS Go files is not the
// package under gate, and accepting one would hand Scan a confident non-zero
// file count for the wrong tree — which reads as a clean package forever and
// makes every ratchet ornamental. This is the exact shape reachable under a
// rundir pinned to the repo root the moment a root-level Go file appears.
func TestResolvePackageDirRejectsADirectoryThatIsNotTheCallersPackage(t *testing.T) {
	// The working directory holds real, parseable non-test Go sources...
	dir := fixture(t, map[string]string{"flight.go": violation})
	t.Chdir(dir)
	// ...but it is not the "server" package the caller file belongs to, and the
	// caller's own directory does not exist either.
	if got, err := ResolvePackageDir("nowhere/that/server/dochan_ratchet_test.go"); err == nil {
		t.Fatalf("resolved %q for a directory that is not the caller's package; "+
			"the ratchet would scan the wrong tree and pass on a non-zero count", got)
	}
}

func TestResolvePackageDirErrorsWhenNoCandidateHoldsSources(t *testing.T) {
	// The failure that must never be silent: a ratchet that cannot find its own
	// package has to fail, because "found no violations" would be a lie.
	t.Chdir(t.TempDir())
	if _, err := ResolvePackageDir("nowhere/that/exists/dochan_ratchet_test.go"); err == nil {
		t.Fatal("resolving with no sources anywhere succeeded; the ratchet would then scan nothing and pass")
	}
}

func TestResolvePackageDirRejectsAnEmptyCallerFile(t *testing.T) {
	if _, err := ResolvePackageDir(""); err == nil {
		t.Fatal("an empty caller file resolved successfully; runtime.Caller's ok flag must be honoured")
	}
}

const fieldAssignment = `package fixture

type S struct{ hook func(string) }

func wire(s *S, fn func(string)) {
	s.hook = fn
}
`

func TestScanFieldAssignmentsReportsASelectorAssignment(t *testing.T) {
	t.Parallel()
	findings, scanned, err := ScanFieldAssignments(fixture(t, map[string]string{"wire.go": fieldAssignment}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one", findings)
	}
	if got, want := findings[0], "wire.go:6"; got != want {
		t.Errorf("finding = %q, want %q — a finding must name file and line", got, want)
	}
}

func TestScanFieldAssignmentsReportsAKeyedCompositeLiteral(t *testing.T) {
	t.Parallel()
	const keyed = `package fixture

type S struct{ hook func(string) }

func build(fn func(string)) *S {
	return &S{hook: fn}
}
`
	findings, _, err := ScanFieldAssignments(fixture(t, map[string]string{"build.go": keyed}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly one — a keyed literal element is an assignment to the field", findings)
	}
}

func TestScanFieldAssignmentsIgnoresAReadOfTheField(t *testing.T) {
	t.Parallel()
	const readOnly = `package fixture

type S struct{ hook func(string) }

func call(s *S) {
	if s.hook != nil {
		s.hook("k")
	}
}
`
	findings, scanned, err := ScanFieldAssignments(fixture(t, map[string]string{"call.go": readOnly}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none — the gate is on ASSIGNMENT, and production legitimately "+
			"reads a test-only field inside a nil check", findings)
	}
}

func TestScanFieldAssignmentsIgnoresTestFilesAndReportsTheCount(t *testing.T) {
	t.Parallel()
	// The invariant this package exists to keep in one place: a scan that read
	// no files must be distinguishable from a clean one, in EVERY scanner here.
	findings, scanned, err := ScanFieldAssignments(fixture(t, map[string]string{"wire_test.go": fieldAssignment}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 0 {
		t.Fatalf("filesScanned = %d, want 0 — _test.go files are excluded, and the wiring lives there", scanned)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none from a _test.go file", findings)
	}
}

func TestScanFieldAssignmentsSurfacesAnUnparseableFileAsAnError(t *testing.T) {
	t.Parallel()
	dir := fixture(t, map[string]string{"broken.go": "package fixture\n\nfunc oops( {\n"})
	findings, _, err := ScanFieldAssignments(dir, "hook")
	if err == nil {
		t.Fatal("an unparseable file scanned clean; a parse failure must never read as an absence of findings")
	}
	if findings != nil {
		t.Errorf("findings = %v, want none alongside an error", findings)
	}
}

func TestScanFieldAssignmentsRejectsAnEmptyFieldName(t *testing.T) {
	t.Parallel()
	// A gate asked for "" would match nothing and pass forever — the same class
	// of silent-clean failure filesScanned exists to catch.
	if _, _, err := ScanFieldAssignments(fixture(t, map[string]string{"wire.go": fieldAssignment}), ""); err == nil {
		t.Fatal("an empty field name scanned successfully; a gate on no field is not a gate")
	}
}

func TestScanFieldAssignmentsDoesNotSeeAPositionalCompositeLiteral(t *testing.T) {
	t.Parallel()
	// A PINNED LIMIT, not a bug — the sibling of the method-value pin above. A
	// positional literal carries no KeyValueExpr, so it names no field for a
	// syntactic scan to match. What keeps the seam safe is that the field is
	// unexported and its only writers live in the declaring package's _test.go
	// files; the gate raises the cost of the mistake rather than removing it.
	const positional = `package fixture

type S struct{ hook func(string) }

func build(fn func(string)) *S {
	return &S{fn}
}
`
	findings, scanned, err := ScanFieldAssignments(fixture(t, map[string]string{"build.go": positional}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v — the positional-literal blind spot has been closed; update this test "+
			"and the ScanFieldAssignments doc rather than deleting the pin", findings)
	}
}

func TestScanFieldAssignmentsDoesNotSeeAnAssignmentThroughAPointerToTheField(t *testing.T) {
	t.Parallel()
	// The second pinned limit: the left-hand side is a StarExpr, not a selector
	// ending in the field name, so the scan sees nothing to match.
	const throughPointer = `package fixture

type S struct{ hook func(string) }

func wire(s *S, fn func(string)) {
	p := &s.hook
	*p = fn
}
`
	findings, scanned, err := ScanFieldAssignments(fixture(t, map[string]string{"wire.go": throughPointer}), "hook")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("filesScanned = %d, want 1", scanned)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v — the pointer-indirection blind spot has been closed; update this test "+
			"and the ScanFieldAssignments doc rather than deleting the pin", findings)
	}
}
