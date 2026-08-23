// Package detachgate is the syntactic scanner behind bossd's per-package
// ratchet tests. It exists so the four packages that declare a
// singleflight.Group can each fail their own build on a raw DoChan call
// without four copies of the AST walk — and, since BOS-951, so that a gate on
// a test-only STRUCT FIELD (ScanFieldAssignments) reuses that same walk rather
// than becoming a fifth copy of it.
//
// # Test-only
//
// Nothing in production imports this package; its only callers are
// dochan_ratchet_test.go in internal/server, internal/session,
// internal/rotation and internal/upstream. It lives beside detach rather than
// inside it so the ratchet's machinery stays off detach's production surface.
//
// # One walk, one "scanned nothing is not clean" invariant
//
// Every scanner here goes through the same unexported walk: the ReadDir, the
// _test.go filter, the rule that a parse failure is an error rather than a
// clean result, and the filesScanned count all live in one place. That is the
// point of the package. A gate that copied the walk would also copy the
// filesScanned invariant, and a copied invariant is one that can drift while
// both copies keep passing — which for a gate is indistinguishable from
// working.
//
// # Deliberately syntactic, and deliberately per-directory
//
// The scanners parse source; they do not type-check. Scan therefore reports a
// call whose selector is spelled DoChan regardless of what the receiver
// actually is, and misses a DoChan reached any other way — most notably
// through a METHOD VALUE (`join := g.DoChan; join(key, fn)`), which is a known
// blind spot pinned by TestScanDoesNotSeeADoChanReachedThroughAMethodValue
// rather than an oversight. That is the accepted trade: the ratchet raises the
// cost of the mistake, while detach.Group — which exposes neither Do nor
// DoChan — is what makes the converted call site structurally safe.
//
// ScanFieldAssignments inherits the same trade and has blind spots of its own,
// listed on the function and pinned by tests beside the method-value one.
//
// A scan reads one directory and does not recurse: a repo-walking Go test
// cannot see the tree it needs from inside Bazel's per-target source sandbox,
// so a fifth package that introduces singleflight needs its own ratchet test.
//
// # Finding the directory
//
// Callers resolve it with ResolvePackageDir rather than hard-coding ".", because
// the working directory is not the package directory under every runner. `go
// test` sets it to the package; a rules_go `go_test` sets it to the target's
// rundir, which several targets in this repo pin to the REPO ROOT so that
// runtime.Caller paths resolve against staged data. Both older AST gates here
// read "." unconditionally, and what that costs them differs — which is the
// point. services/bosso/internal/server/routing_architecture_test.go runs under
// a rundir pinned to the repo root with no Go sources staged, so under Bazel it
// scans an empty directory and passes by finding nothing.
// services/boss/internal/views/checkbox_test.go escapes, but only by accident:
// its target pins no rundir and carries a *.go data glob added for a DIFFERENT
// test, and it happens to fail on a zero count of its own. Neither escape is a
// property of reading "." — so Scan reports filesScanned and its callers assert
// that count is non-zero, making "scanned nothing" impossible to mistake for
// clean.
package detachgate

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// Scan reports every raw DoChan call in the non-test Go files directly inside
// dir. Findings are formatted "file.go:LINE:enclosingFunc".
//
// filesScanned is returned alongside so a caller can distinguish "clean" from
// "scanned nothing": a ratchet that silently pointed at the wrong directory
// would otherwise pass by finding no files at all, which is the failure mode
// most likely to make the whole gate ornamental.
//
// A file that cannot be parsed is an error, never a clean scan, for the same
// reason.
func Scan(dir string) (findings []string, filesScanned int, err error) {
	filesScanned, err = walkPackage(dir, func(name string, fset *token.FileSet, file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "DoChan" {
					return true
				}
				findings = append(findings, fmt.Sprintf("%s:%d:%s",
					name, fset.Position(call.Pos()).Line, fn.Name.Name))
				return true
			})
		}
	})
	if err != nil {
		// Findings gathered before the failure are discarded deliberately: a
		// partial list alongside an error invites a caller to treat it as the
		// whole list.
		return nil, filesScanned, err
	}
	return findings, filesScanned, nil
}

// ScanFieldAssignments reports every assignment to a struct field named field
// in the non-test Go files directly inside dir. Findings are formatted
// "file.go:LINE".
//
// It backs the ratchets over test-only seams — a field that is safe only
// because production never sets it, such as Server.switchFlightAttached
// (BOS-951). It returns filesScanned for exactly the reason Scan does, and
// through exactly the same walk, so the "scanned nothing is not clean" rule
// cannot hold in one gate and quietly lapse in the other.
//
// Two forms are matched: an assignment whose left-hand side is a selector
// ending in field (`s.field = fn`, and its := form), and a KEYED composite
// literal element (`T{field: fn}`).
//
// # Blind spots, all pinned by tests
//
// Syntactic, so it matches the NAME regardless of which type owns it — a
// same-named field on an unrelated struct is reported too. That direction is
// safe: it over-reports rather than passing something through.
//
// It misses two shapes, each pinned beside the method-value pin for Scan:
//
//   - a POSITIONAL composite literal (`T{a, b, fn}`), which carries no
//     KeyValueExpr and so names no field at all;
//   - an assignment made THROUGH A POINTER to the field
//     (`p := &s.field; *p = fn`), whose left-hand side is a StarExpr.
//
// Both are accepted for the same reason as Scan's: the gate raises the cost of
// the mistake, it does not make it unreachable. What actually keeps the seam
// safe is that the field is unexported and its only writers live in _test.go
// files of the package that declares it.
func ScanFieldAssignments(dir, field string) (findings []string, filesScanned int, err error) {
	if field == "" {
		return nil, 0, errors.New("detachgate: no field name given; scanning for the empty field " +
			"would match nothing and the gate would pass forever")
	}
	filesScanned, err = walkPackage(dir, func(name string, fset *token.FileSet, file *ast.File) {
		record := func(pos token.Pos) {
			findings = append(findings, fmt.Sprintf("%s:%d", name, fset.Position(pos).Line))
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if selector, ok := lhs.(*ast.SelectorExpr); ok && selector.Sel.Name == field {
						record(n.Pos())
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == field {
					record(n.Pos())
				}
			}
			return true
		})
	})
	if err != nil {
		return nil, filesScanned, err
	}
	return findings, filesScanned, nil
}

// walkPackage parses every non-test Go file directly inside dir and hands each
// to visit, reporting how many were parsed.
//
// This is the one walk behind every scanner in this package. A parse failure is
// an error and never a clean scan, and filesScanned is reported so a caller can
// tell "clean" from "scanned nothing" — both rules live here precisely so a
// second gate inherits them rather than reimplementing them.
func walkPackage(dir string, visit func(name string, fset *token.FileSet, file *ast.File)) (filesScanned int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("detachgate: read %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		parsed, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return filesScanned, fmt.Errorf("detachgate: parse %s: %w", path, parseErr)
		}
		filesScanned++
		visit(name, fset, parsed)
	}
	return filesScanned, nil
}

// ResolvePackageDir returns the directory holding the non-test sources of the
// package that owns callerFile, which a ratchet test obtains from
// runtime.Caller(0). It is an error for no candidate to hold any non-test Go
// file — a ratchet that cannot find its own package must fail, never pass.
//
// Two candidates are tried, in order, because the working directory differs by
// runner (see the package doc):
//
//  1. callerFile's own directory. Under `go test` that is absolute and always
//     right. Under rules_go it is recorded relative to the execroot, so it
//     resolves for a target whose rundir is the repo root.
//  2. ".", which is right under `go test` and under a rules_go target that keeps
//     the default per-package rundir.
//
// Under Bazel either candidate only resolves if the package's sources are
// staged into the test's runfiles, so the go_test needs them in `data`.
//
// # Holding a Go file is not being the right package
//
// A candidate must ALSO carry the caller's own package directory name. Holding
// some non-test .go file is the weaker test, and passing it is not evidence:
// under a rundir this repo pins to the REPO ROOT, "." would satisfy it the
// moment anyone adds a root-level Go file, and Scan would then report a
// confident non-zero count for a tree that is not the package under gate.
// That is precisely the failure the filesScanned count exists to catch, so it
// must not be reachable through the resolver instead — a wrong directory
// scanned cleanly is indistinguishable from a clean package, forever.
func ResolvePackageDir(callerFile string) (string, error) {
	if callerFile == "" {
		return "", errors.New("detachgate: no caller file; pass the path from runtime.Caller(0)")
	}
	wantPkg := filepath.Base(filepath.Dir(callerFile))
	candidates := []string{filepath.Dir(callerFile), "."}
	for _, dir := range candidates {
		// Resolve "." before naming it: its own Base is ".", which matches no
		// package, and the runner's working directory is the whole point of
		// this candidate.
		abs, err := filepath.Abs(dir)
		if err != nil || filepath.Base(abs) != wantPkg {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
				return dir, nil
			}
		}
	}
	return "", fmt.Errorf("detachgate: no directory named %q holding non-test Go sources under any of %v "+
		"(cwd differs by runner; under Bazel the package's sources must be staged into the test's runfiles)",
		wantPkg, candidates)
}
