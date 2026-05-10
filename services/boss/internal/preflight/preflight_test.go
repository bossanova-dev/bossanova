package preflight

import (
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
