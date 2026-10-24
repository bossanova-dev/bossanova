package ui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// allViewConstants is the enum, written out. Go has no reflection over consts,
// so the list cannot derive itself at runtime — but
// TestAllViewConstantsAreListed parses the package source to prove it is
// complete, so "hand-written" does not mean "unverified".
var allViewConstants = []struct {
	view View
	name string
}{
	{ViewHome, "ViewHome"},
	{ViewNewSession, "ViewNewSession"},
	{ViewAttach, "ViewAttach"},
	{ViewChatPicker, "ViewChatPicker"},
	{ViewRepoAdd, "ViewRepoAdd"},
	{ViewRepoList, "ViewRepoList"},
	{ViewRepoSettings, "ViewRepoSettings"},
	{ViewTrash, "ViewTrash"},
	{ViewSettings, "ViewSettings"},
	{ViewSessionSettings, "ViewSessionSettings"},
	{ViewLogin, "ViewLogin"},
	{ViewBugReport, "ViewBugReport"},
	{ViewCron, "ViewCron"},
	{ViewCronForm, "ViewCronForm"},
	{ViewOnboarding, "ViewOnboarding"},
	{ViewAgentAccounts, "ViewAgentAccounts"},
	{ViewAgentAccountEdit, "ViewAgentAccountEdit"},
	{ViewAgentAccountRegister, "ViewAgentAccountRegister"},
	{ViewGeneralSettings, "ViewGeneralSettings"},
}

// TestAllViewConstantsAreListed makes allViewConstants a trustworthy stand-in
// for the enum, in two steps.
//
// First, every entry must sit at its own iota value, contiguously from zero —
// that is what lets the guards below iterate the list as if it were the enum,
// and it catches a constant inserted mid-block (which shifts every later value)
// without the list being updated.
//
// Second, and the reason this test earns its keep: it parses the package's own
// source for the `View` const block and asserts the list covers it. Without
// that, the whole file is blind to the exact scenario it exists for — a 20th
// constant appended to the enum and never listed here would leave every guard
// below green while covering nothing. The package already does source parsing
// for this class of drift (see TestEveryFormFileIsCovered), and BUILD.bazel
// already ships the non-test sources as `data`, so this costs nothing new.
//
// The const block is located by scanning rather than by filename so that moving
// or renaming the file holding the enum does not silently disable the check.
// guard-id: boss-view-constants-listed
func TestAllViewConstantsAreListed(t *testing.T) {
	for i, tc := range allViewConstants {
		if int(tc.view) != i {
			t.Fatalf("allViewConstants[%d] is %s (= %d); the list must stay in iota order "+
				"and contiguous, because the guards below iterate it as the enum",
				i, tc.name, int(tc.view))
		}
	}

	declared := viewConstantNamesFromSource(t, ".")
	if len(declared) == 0 {
		t.Fatal("found no `View` const block in the package source; this test can no " +
			"longer verify the list and must be repaired, not deleted")
	}

	for _, name := range viewConstantsMissingFromList(declared) {
		t.Errorf("View constant %s is declared in the enum but missing from "+
			"allViewConstants.\n"+
			"Every guard in this file iterates that list, so an unlisted constant is "+
			"covered by nothing: it would not be checked for a renderActiveView arm or "+
			"a handleSwitchView arm, and both of those switches are outside the "+
			"exhaustive linter's reach. Add it to allViewConstants, then to the "+
			"`constructed` or `notRoutable` table in the switch-view guard.", name)
	}
	if len(declared) != len(allViewConstants) {
		t.Errorf("enum declares %d View constants, allViewConstants lists %d",
			len(declared), len(allViewConstants))
	}
}

// TestViewConstantListGuardReportsASeededUnlistedConstant parses a seeded
// package whose View const block declares a constant allViewConstants does not
// list, and requires the guard to name exactly that constant.
func TestViewConstantListGuardReportsASeededUnlistedConstant(t *testing.T) {
	dir := t.TempDir()
	source := "package ui\n\ntype View int\n\nconst (\n\tViewHome View = iota\n\tViewNewSession\n\tViewSeededUnlisted\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "view.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	declared := viewConstantNamesFromSource(t, dir)
	if len(declared) != 3 {
		t.Fatalf("declared = %v, want the 3 seeded constants", declared)
	}
	if missing := viewConstantsMissingFromList(declared); !slices.Equal(missing, []string{"ViewSeededUnlisted"}) {
		t.Fatalf("missing = %v, want [ViewSeededUnlisted]", missing)
	}
}

// viewConstantsMissingFromList returns the declared names allViewConstants
// does not list, in declaration order.
func viewConstantsMissingFromList(declared []string) []string {
	listed := make(map[string]bool, len(allViewConstants))
	for _, tc := range allViewConstants {
		listed[tc.name] = true
	}
	var missing []string
	for _, name := range declared {
		if !listed[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// viewConstantNamesFromSource returns the names in the `View` const block of
// the package in dir, read from the source files themselves.
func viewConstantNamesFromSource(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST || len(gen.Specs) == 0 {
				continue
			}
			// Only the first spec in an iota block carries the type.
			first, ok := gen.Specs[0].(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := first.Type.(*ast.Ident)
			if !ok || ident.Name != "View" {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	return names
}
