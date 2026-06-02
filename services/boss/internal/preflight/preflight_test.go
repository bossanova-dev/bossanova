package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckShellTools_AllPresent verifies that on a normal dev/CI host
// (where bash and tee are both on PATH) the check returns nil — the
// blocking preflight screen would otherwise fire on every boss launch.
func TestCheckShellTools_AllPresent(t *testing.T) {
	if issue := CheckShellTools(); issue != nil {
		t.Fatalf("CheckShellTools returned issue on normal host: title=%q detail=%q",
			issue.Title, issue.Detail)
	}
}

// TestCheckShellTools_BothMissing simulates a system without bash or tee
// by emptying PATH for the duration of the test. The check must report
// both tools and recommend the matching install command.
func TestCheckShellTools_BothMissing(t *testing.T) {
	t.Setenv("PATH", "")
	issue := CheckShellTools()
	if issue == nil {
		t.Fatal("expected issue when PATH is empty; got nil")
	}
	if !strings.Contains(issue.Title, "bash") || !strings.Contains(issue.Title, "tee") {
		t.Errorf("title should mention both missing tools; got %q", issue.Title)
	}
	if !strings.Contains(issue.Detail, "tee") {
		t.Errorf("detail should reference tee; got %q", issue.Detail)
	}
}

// TestCheckShellTools_SingleMissing pins the exact boundary at the
// `len(missing) > 1` branch (preflight.go:51). With exactly one tool
// missing (len(missing) == 1) the title must be the single-tool form
// ("tee is not installed"), not the combined "bash and tee are not
// installed". A boundary mutant that flips `> 1` to `>= 1` would emit
// the combined title here, so this case fails against the mutant and
// passes against the real code.
func TestCheckShellTools_SingleMissing(t *testing.T) {
	// Build a PATH that contains bash but not tee so exactly one tool
	// (tee) is reported missing.
	dir := t.TempDir()
	bash := filepath.Join(dir, "bash")
	if err := os.WriteFile(bash, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing fake bash: %v", err)
	}
	t.Setenv("PATH", dir)

	issue := CheckShellTools()
	if issue == nil {
		t.Fatal("expected issue when tee is missing; got nil")
	}
	if issue.Title != "tee is not installed" {
		t.Errorf("title should be single-tool form %q; got %q",
			"tee is not installed", issue.Title)
	}
	if strings.Contains(issue.Title, "bash and tee") {
		t.Errorf("title must not use combined form when only one tool is missing; got %q", issue.Title)
	}
}
