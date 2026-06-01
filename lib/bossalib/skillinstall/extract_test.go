package skillinstall

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/boss/SKILL.md":                                      {Data: []byte("# Boss CLI Reference\nAll commands.")},
		"skills/boss-test/SKILL.md":                                 {Data: []byte("# Test Skill\nDo the thing.")},
		"skills/boss-other/SKILL.md":                                {Data: []byte("# Other Skill\nDo other thing.")},
		"skills/boss-finalize/add-pr.sh":                            {Data: []byte("#!/bin/sh\necho ok")},
		"skills/boss-finalize/SKILL.md":                             {Data: []byte("# Finalize\nLand it.")},
		"skills/boss-repair/SKILL.md":                               {Data: []byte("# Repair\nFix it.")},
		"skills/boss-repair/scripts/review-feedback-probe.js":       {Data: []byte("#!/usr/bin/env node\nconsole.log('pending')")},
		"skills/boss-repair/scripts/review-feedback-probe.test.txt": {Data: []byte("fixture")},
	}
}

func changedFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/boss/SKILL.md":                                {Data: []byte("# Boss CLI Reference\nAll commands.")},
		"skills/boss-test/SKILL.md":                           {Data: []byte("# Test Skill\nDo the changed thing.")},
		"skills/boss-other/SKILL.md":                          {Data: []byte("# Other Skill\nDo other thing.")},
		"skills/boss-new/SKILL.md":                            {Data: []byte("# New Skill\nDo new thing.")},
		"skills/boss-finalize/add-pr.sh":                      {Data: []byte("#!/bin/sh\necho ok")},
		"skills/boss-finalize/SKILL.md":                       {Data: []byte("# Finalize\nLand it.")},
		"skills/boss-repair/SKILL.md":                         {Data: []byte("# Repair\nFix it.")},
		"skills/boss-repair/scripts/review-feedback-probe.js": {Data: []byte("#!/usr/bin/env node\nconsole.log('changed')")},
	}
}

func TestExtract(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Files should exist in the namespace directory.
	tests := []struct {
		rel     string
		content string
	}{
		{"bossanova/boss/SKILL.md", "# Boss CLI Reference\nAll commands."},
		{"bossanova/boss-test/SKILL.md", "# Test Skill\nDo the thing."},
		{"bossanova/boss-other/SKILL.md", "# Other Skill\nDo other thing."},
		{"bossanova/boss-finalize/SKILL.md", "# Finalize\nLand it."},
		{"bossanova/boss-finalize/add-pr.sh", "#!/bin/sh\necho ok"},
		{"bossanova/boss-repair/SKILL.md", "# Repair\nFix it."},
		{"bossanova/boss-repair/scripts/review-feedback-probe.js", "#!/usr/bin/env node\nconsole.log('pending')"},
		{"bossanova/boss-repair/scripts/review-feedback-probe.test.txt", "fixture"},
	}

	for _, tt := range tests {
		path := filepath.Join(dest, tt.rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%s): %v", tt.rel, err)
			continue
		}
		if string(data) != tt.content {
			t.Errorf("%s: got %q, want %q", tt.rel, string(data), tt.content)
		}
	}
}

func TestExtractCreatesSymlinks(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Each boss skill should have a symlink in the parent dir.
	for _, name := range []string{"boss", "boss-test", "boss-other", "boss-finalize"} {
		link := filepath.Join(dest, name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("Readlink(%s): %v", name, err)
			continue
		}
		expected := filepath.Join("bossanova", name)
		if target != expected {
			t.Errorf("symlink %s: got target %q, want %q", name, target, expected)
		}

		// Verify the symlink resolves to a real directory.
		info, err := os.Stat(link)
		if err != nil {
			t.Errorf("Stat(%s): %v", name, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to resolve to a directory", name)
		}
	}
}

func TestExtractSymlinksReadable(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Reading through the symlink should work.
	data, err := os.ReadFile(filepath.Join(dest, "boss-test", "SKILL.md"))
	if err != nil {
		t.Fatalf("ReadFile via symlink: %v", err)
	}
	if string(data) != "# Test Skill\nDo the thing." {
		t.Errorf("unexpected content via symlink: %q", string(data))
	}
}

func TestExtractIdempotent(t *testing.T) {
	dest := t.TempDir()
	fsys := testFS()

	if err := Extract(dest, fsys); err != nil {
		t.Fatalf("first Extract: %v", err)
	}
	if err := Extract(dest, fsys); err != nil {
		t.Fatalf("second Extract: %v", err)
	}

	// Verify content is still correct after double extraction.
	path := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "# Test Skill\nDo the thing." {
		t.Errorf("content after idempotent extract: got %q", string(data))
	}

	// Symlinks should still resolve.
	data, err = os.ReadFile(filepath.Join(dest, "boss-test", "SKILL.md"))
	if err != nil {
		t.Fatalf("ReadFile via symlink after idempotent extract: %v", err)
	}
	if string(data) != "# Test Skill\nDo the thing." {
		t.Errorf("content via symlink after idempotent extract: got %q", string(data))
	}
}

func TestExtractCreatesDirectories(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "deep", "nested", "path")
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	path := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %s, got error: %v", path, err)
	}
}

func TestExtractRemovesStaleSkills(t *testing.T) {
	dest := t.TempDir()
	nsDir := filepath.Join(dest, "bossanova")

	// Pre-create a stale boss skill in the namespace dir.
	staleDir := filepath.Join(nsDir, "boss-old-removed")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleDir, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-create a stale symlink.
	if err := os.Symlink(filepath.Join("bossanova", "boss-old-removed"), filepath.Join(dest, "boss-old-removed")); err != nil {
		t.Fatal(err)
	}

	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Stale skill directory should be removed.
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("expected stale skill dir to be removed, but it still exists")
	}

	// Stale symlink should be removed.
	if _, err := os.Lstat(filepath.Join(dest, "boss-old-removed")); !os.IsNotExist(err) {
		t.Errorf("expected stale symlink to be removed, but it still exists")
	}

	// Current skills should still be present.
	path := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected current skill to exist: %v", err)
	}
}

func TestExtractScriptPermissions(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for _, rel := range []string{
		"bossanova/boss-finalize/add-pr.sh",
		"bossanova/boss-repair/scripts/review-feedback-probe.js",
	} {
		path := filepath.Join(dest, rel)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("expected %s to be executable, got mode %o", rel, info.Mode().Perm())
		}
	}

	mdPath := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	info, err := os.Stat(mdPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", mdPath, err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("expected .md file to not be executable, got mode %o", info.Mode().Perm())
	}

	fixturePath := filepath.Join(dest, "bossanova", "boss-repair", "scripts", "review-feedback-probe.test.txt")
	info, err = os.Stat(fixturePath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", fixturePath, err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("expected non-script fixture to not be executable, got mode %o", info.Mode().Perm())
	}
}

func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("expected absolute path, got %q", dir)
	}
	if filepath.Base(dir) != "skills" {
		t.Errorf("expected path ending in skills, got %q", dir)
	}
}

func TestDirForAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name  string
		agent Agent
		want  string
	}{
		{name: "claude", agent: AgentClaude, want: filepath.Join(home, ".claude", "skills")},
		{name: "codex", agent: AgentCodex, want: filepath.Join(home, ".codex", "skills")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DirForAgent(tt.agent)
			if err != nil {
				t.Fatalf("DirForAgent: %v", err)
			}
			if got != tt.want {
				t.Fatalf("DirForAgent(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}

	if _, err := DirForAgent(Agent("unknown")); err == nil {
		t.Fatal("DirForAgent unknown: got nil error, want error")
	}
}

func TestManifestChangesWhenEmbeddedSkillsChange(t *testing.T) {
	a, err := Manifest(testFS())
	if err != nil {
		t.Fatalf("Manifest(testFS): %v", err)
	}
	b, err := Manifest(changedFS())
	if err != nil {
		t.Fatalf("Manifest(changedFS): %v", err)
	}
	if a == b {
		t.Fatal("Manifest did not change after embedded skill content changed")
	}
}

func TestIsInstalled(t *testing.T) {
	dir := t.TempDir()

	// Empty directory: not installed.
	if IsInstalled(dir) {
		t.Error("expected false for empty directory")
	}

	// Create the namespace dir without boss-* subdirs.
	nsDir := filepath.Join(dir, "bossanova")
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if IsInstalled(dir) {
		t.Error("expected false for empty namespace directory")
	}

	// Create a boss-* directory inside the namespace.
	if err := os.MkdirAll(filepath.Join(nsDir, "boss-test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsInstalled(dir) {
		t.Error("expected true after creating boss-test dir in namespace")
	}
}

func TestIsInstalledNonexistentDir(t *testing.T) {
	if IsInstalled("/nonexistent/path/that/does/not/exist") {
		t.Error("expected false for nonexistent directory")
	}
}

func TestNeedsUpdateFalseAfterExtract(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if needs {
		t.Fatal("NeedsUpdate = true, want false after fresh extract")
	}
}

func TestNeedsUpdateDetectsInstalledContentDrift(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	path := filepath.Join(dest, Namespace, "boss-test", "SKILL.md")
	if err := os.WriteFile(path, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for installed content drift")
	}
}

func TestNeedsUpdateDetectsInstalledScriptDrift(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	path := filepath.Join(dest, Namespace, "boss-repair", "scripts", "review-feedback-probe.js")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env node\nconsole.log('local edit')"), 0o755); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for installed script drift")
	}
}

func TestNeedsUpdateDetectsEmbeddedContentChangeAndNewSkill(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	needs, err := NeedsUpdate(dest, changedFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for changed embedded skills")
	}
}

func TestNeedsUpdateDetectsStaleInstalledSkillDir(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stale := filepath.Join(dest, Namespace, "boss-removed")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "SKILL.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for stale installed skill")
	}
}

func TestNeedsUpdateDetectsMissingTopLevelSymlink(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := os.Remove(filepath.Join(dest, "boss-test")); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for missing top-level symlink")
	}
}

func TestNeedsUpdateDetectsStaleTopLevelSymlink(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := os.Symlink(filepath.Join(Namespace, "boss-removed"), filepath.Join(dest, "boss-removed")); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for stale top-level symlink")
	}
}

func TestEnsureUpdatedDoesNotInstallIntoEmptyDirectory(t *testing.T) {
	dest := t.TempDir()
	updated, err := EnsureUpdated(dest, testFS())
	if err != nil {
		t.Fatalf("EnsureUpdated: %v", err)
	}
	if updated {
		t.Fatal("EnsureUpdated updated empty dir, want no-op")
	}
	if _, err := os.Stat(filepath.Join(dest, Namespace)); !os.IsNotExist(err) {
		t.Fatalf("namespace exists after no-op, err=%v", err)
	}
}

func TestEnsureUpdatedRefreshesStaleInstall(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	path := filepath.Join(dest, Namespace, "boss-test", "SKILL.md")
	if err := os.WriteFile(path, []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, err := EnsureUpdated(dest, testFS())
	if err != nil {
		t.Fatalf("EnsureUpdated: %v", err)
	}
	if !updated {
		t.Fatal("EnsureUpdated = false, want true")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# Test Skill\nDo the thing." {
		t.Fatalf("stale content not refreshed: %q", string(data))
	}
}
