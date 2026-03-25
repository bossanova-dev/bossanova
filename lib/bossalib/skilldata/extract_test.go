package skilldata

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"skills/boss-test/SKILL.md":      {Data: []byte("# Test Skill\nDo the thing.")},
		"skills/boss-other/SKILL.md":     {Data: []byte("# Other Skill\nDo other thing.")},
		"skills/boss-finalize/add-pr.sh": {Data: []byte("#!/bin/sh\necho ok")},
		"skills/boss-finalize/SKILL.md":  {Data: []byte("# Finalize\nLand it.")},
	}
}

func TestExtractSkills(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
	}

	// Files should exist in the namespace directory.
	tests := []struct {
		rel     string
		content string
	}{
		{"bossanova/boss-test/SKILL.md", "# Test Skill\nDo the thing."},
		{"bossanova/boss-other/SKILL.md", "# Other Skill\nDo other thing."},
		{"bossanova/boss-finalize/SKILL.md", "# Finalize\nLand it."},
		{"bossanova/boss-finalize/add-pr.sh", "#!/bin/sh\necho ok"},
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

func TestExtractSkillsCreatesSymlinks(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
	}

	// Each boss-* skill should have a symlink in the parent dir.
	for _, name := range []string{"boss-test", "boss-other", "boss-finalize"} {
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

func TestExtractSkillsSymlinksReadable(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
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

func TestExtractSkillsIdempotent(t *testing.T) {
	dest := t.TempDir()
	fs := testFS()

	if err := ExtractSkills(dest, fs); err != nil {
		t.Fatalf("first ExtractSkills: %v", err)
	}
	if err := ExtractSkills(dest, fs); err != nil {
		t.Fatalf("second ExtractSkills: %v", err)
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

func TestExtractSkillsCreatesDirectories(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "deep", "nested", "path")
	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
	}

	path := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %s, got error: %v", path, err)
	}
}

func TestExtractSkillsRemovesStaleSkills(t *testing.T) {
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

	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
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

func TestExtractSkillsShellScriptPermissions(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractSkills(dest, testFS()); err != nil {
		t.Fatalf("ExtractSkills: %v", err)
	}

	// Shell scripts should be executable.
	shPath := filepath.Join(dest, "bossanova", "boss-finalize", "add-pr.sh")
	info, err := os.Stat(shPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", shPath, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("expected .sh file to be executable, got mode %o", info.Mode().Perm())
	}

	// Markdown files should NOT be executable.
	mdPath := filepath.Join(dest, "bossanova", "boss-test", "SKILL.md")
	info, err = os.Stat(mdPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", mdPath, err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("expected .md file to not be executable, got mode %o", info.Mode().Perm())
	}
}

func TestDefaultSkillsDir(t *testing.T) {
	dir, err := DefaultSkillsDir()
	if err != nil {
		t.Fatalf("DefaultSkillsDir: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("expected absolute path, got %q", dir)
	}
	if filepath.Base(dir) != "skills" {
		t.Errorf("expected path ending in skills, got %q", dir)
	}
}

func TestBossSkillsInstalled(t *testing.T) {
	dir := t.TempDir()

	// Empty directory: not installed.
	if BossSkillsInstalled(dir) {
		t.Error("expected false for empty directory")
	}

	// Create the namespace dir without boss-* subdirs.
	nsDir := filepath.Join(dir, "bossanova")
	if err := os.MkdirAll(nsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if BossSkillsInstalled(dir) {
		t.Error("expected false for empty namespace directory")
	}

	// Create a boss-* directory inside the namespace.
	if err := os.MkdirAll(filepath.Join(nsDir, "boss-test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !BossSkillsInstalled(dir) {
		t.Error("expected true after creating boss-test dir in namespace")
	}
}

func TestBossSkillsInstalledNonexistentDir(t *testing.T) {
	if BossSkillsInstalled("/nonexistent/path/that/does/not/exist") {
		t.Error("expected false for nonexistent directory")
	}
}
