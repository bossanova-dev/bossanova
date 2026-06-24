package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/boss/cmd/skillgen"
)

// TestSkillMatchesGenerated asserts that the committed command-reference region
// of the canonical SKILL.md byte-equals a fresh render of the real CLI tree.
// This makes `make test` fail on any drift — a command or flag added without
// running `make gen-skill` — and is a strictly stronger guard than the old
// substring-coverage check it replaced.
func TestSkillMatchesGenerated(t *testing.T) {
	skill := readSkillContent(t)
	body, err := skillgen.GenerateBody(rootCmd())
	if err != nil {
		t.Fatalf("GenerateBody: %v", err)
	}
	want, err := skillgen.Inject(skill, body)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if want != skill {
		t.Errorf("SKILL.md is stale — run `make gen-skill`.\n" +
			"The generated command reference no longer matches the CLI.")
	}
}

// readSkillContent reads the canonical boss SKILL.md from the embedded skill
// source tree, walking up from this test file's location to locate the repo
// root. This avoids depending on `make copy-skills` having populated mirror
// copies before `go test` compiles the test binary.
func readSkillContent(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		candidate := filepath.Join(dir, "services", "boss", "internal", "skillinstall", "skills", "boss", "SKILL.md")
		if _, err := os.Stat(candidate); err == nil {
			data, err := os.ReadFile(candidate)
			if err != nil {
				t.Fatalf("read %s: %v", candidate, err)
			}
			return string(data)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate services/boss/internal/skillinstall/skills/boss/SKILL.md walking up from %s", filepath.Dir(thisFile))
		}
		dir = parent
	}
}
