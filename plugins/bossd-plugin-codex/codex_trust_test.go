package main

import (
	"strings"
	"testing"
)

func TestSetCodexProjectTrustUpdatesExistingProjectSection(t *testing.T) {
	const config = `[projects."/tmp/repo"]
  trust_level = "untrusted"
notes = "kept"

[projects."/tmp/other"]
trust_level = "trusted"
`

	got := setCodexProjectTrust(config, "/tmp/repo")

	if strings.Count(got, `[projects."/tmp/repo"]`) != 1 {
		t.Fatalf("project section count = %d, want 1:\n%s", strings.Count(got, `[projects."/tmp/repo"]`), got)
	}
	if !strings.Contains(got, "  trust_level = \"trusted\"\nnotes = \"kept\"") {
		t.Fatalf("existing trust line was not updated in place:\n%s", got)
	}
	if strings.Contains(got, "untrusted") {
		t.Fatalf("stale trust level still present:\n%s", got)
	}
	if !strings.Contains(got, `[projects."/tmp/other"]`+"\ntrust_level = \"trusted\"") {
		t.Fatalf("neighbor project section was not preserved:\n%s", got)
	}
}

func TestSetCodexProjectTrustEscapesProjectPathAsTOMLBasicString(t *testing.T) {
	worktree := "C:\\Users\\Dave\\repo \"two\"\nline\t" + string(rune(0x01))

	got := setCodexProjectTrust("", worktree)

	wantHeader := `[projects."C:\\Users\\Dave\\repo \"two\"\nline\t\u0001"]`
	if !strings.Contains(got, wantHeader+"\ntrust_level = \"trusted\"") {
		t.Fatalf("escaped project header missing:\n%s", got)
	}
}
