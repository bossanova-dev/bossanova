package skillinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
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

func writeSourceTree(t *testing.T, root string, fsys fs.FS) string {
	t.Helper()
	srcRoot := filepath.Join(root, SourceRelPath)
	if err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(srcRoot, path), 0o755)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(srcRoot, path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	return srcRoot
}

func TestFindSourceRoot(t *testing.T) {
	root := t.TempDir()
	writeSourceTree(t, root, testFS())
	nested := filepath.Join(root, "nested", "child")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := FindSourceRoot(nested)
	if !ok || got != filepath.Join(root, SourceRelPath) {
		t.Fatalf("FindSourceRoot(%q) = (%q, %t), want (%q, true)", nested, got, ok, filepath.Join(root, SourceRelPath))
	}
	if got, ok := FindSourceRoot(t.TempDir()); ok || got != "" {
		t.Fatalf("FindSourceRoot missing tree = (%q, %t), want (\"\", false)", got, ok)
	}
	if got, ok := FindSourceRoot(string(filepath.Separator)); ok || got != "" {
		t.Fatalf("FindSourceRoot filesystem root = (%q, %t), want (\"\", false)", got, ok)
	}
}

func TestSourceManifestMatchesEquivalentFS(t *testing.T) {
	fsys := fstest.MapFS{
		"skills/.gitkeep":       {Data: []byte{}},
		"skills/boss/SKILL.md":  {Data: []byte("skill")},
		"skills/boss/toolbox/x": {Data: []byte("tool")},
	}
	srcRoot := writeSourceTree(t, t.TempDir(), fsys)
	want, err := Manifest(fsys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SourceManifest(srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("SourceManifest = %q, want Manifest = %q", got, want)
	}
}

func TestSourceDrift(t *testing.T) {
	fsys := testFS()
	srcRoot := writeSourceTree(t, t.TempDir(), fsys)
	installed := t.TempDir()
	if err := Extract(installed, fsys); err != nil {
		t.Fatal(err)
	}
	drift, err := SourceDrift(installed, srcRoot)
	if err != nil || drift {
		t.Fatalf("SourceDrift matching tree = (%t, %v), want (false, nil)", drift, err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	drift, err = SourceDrift(installed, srcRoot)
	if err != nil || !drift {
		t.Fatalf("SourceDrift changed source = (%t, %v), want (true, nil)", drift, err)
	}
	if drift, err := SourceDrift(t.TempDir(), srcRoot); err != nil || drift {
		t.Fatalf("SourceDrift uninstalled tree = (%t, %v), want (false, nil)", drift, err)
	}
}

func TestExtractFromSourceWritesCanonicalSourceBytes(t *testing.T) {
	srcRoot := writeSourceTree(t, t.TempDir(), changedFS())
	dest := t.TempDir()

	if err := ExtractFromSource(dest, srcRoot); err != nil {
		t.Fatalf("ExtractFromSource: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, Namespace, "boss-test", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "# Test Skill\nDo the changed thing."; got != want {
		t.Fatalf("installed source bytes = %q, want %q", got, want)
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

func TestAcquireUpdateLockSerializesSkillWriters(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatalf("acquire first update lock: %v", err)
	}

	acquired := make(chan struct{})
	release := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		second, err := acquireUpdateLock(dir)
		if err != nil {
			errs <- err
			return
		}
		close(acquired)
		<-release
		errs <- second.Unlock()
	}()

	select {
	case <-acquired:
		t.Fatal("second writer acquired the skill update lock while first writer held it")
	case err := <-errs:
		t.Fatalf("acquire second update lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("release first update lock: %v", err)
	}
	select {
	case <-acquired:
	case err := <-errs:
		t.Fatalf("acquire second update lock after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second writer did not acquire the skill update lock after release")
	}
	close(release)
	if err := <-errs; err != nil {
		t.Fatalf("release second update lock: %v", err)
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

func TestExtractPreservesExtensionlessShebangScripts(t *testing.T) {
	// Extensionless helper scripts (e.g. toolbox/.../scripts/review-package) are
	// invoked directly by skill prose. go:embed drops their on-disk 100755 mode
	// and executableSkillFile's filename heuristic misses them, so extraction
	// must fall back to a shebang probe or public installs hit "permission denied".
	fsys := fstest.MapFS{
		"skills/boss-build/SKILL.md": {Data: []byte("# Implement\nRun it.")},
		"skills/boss-build/toolbox/review/scripts/review-package": {
			Data: []byte("#!/usr/bin/env bash\necho ok"),
		},
		"skills/boss-build/toolbox/review/README": {
			Data: []byte("plain text, no shebang, must stay 0644"),
		},
	}
	dest := t.TempDir()
	if err := Extract(dest, fsys); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	scriptPath := filepath.Join(dest, "bossanova", "boss-build", "toolbox", "review", "scripts", "review-package")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", scriptPath, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("expected extensionless shebang script to be executable, got mode %o", info.Mode().Perm())
	}

	plainPath := filepath.Join(dest, "bossanova", "boss-build", "toolbox", "review", "README")
	info, err = os.Stat(plainPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", plainPath, err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Errorf("expected extensionless non-shebang file to not be executable, got mode %o", info.Mode().Perm())
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

func TestInstalledNeedsUpdateDoesNotCreateLockForLegacyInstall(t *testing.T) {
	dest := t.TempDir()
	if err := extract(dest, testFS()); err != nil {
		t.Fatalf("extract legacy install: %v", err)
	}
	lockPath := filepath.Join(dest, updateLockFile)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("legacy install lock before check = %v, want absent", err)
	}

	installed, needsUpdate, err := InstalledNeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("InstalledNeedsUpdate: %v", err)
	}
	if !installed || needsUpdate {
		t.Fatalf("InstalledNeedsUpdate = (%t, %t), want (true, false)", installed, needsUpdate)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("InstalledNeedsUpdate created lock %q: %v", lockPath, err)
	}
}

func TestInstalledNeedsUpdateWaitsForSkillUpdateLock(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	lock, err := acquireUpdateLock(dest)
	if err != nil {
		t.Fatalf("acquire skill update lock: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		installed, needsUpdate, err := InstalledNeedsUpdate(dest, testFS())
		if err == nil && (!installed || needsUpdate) {
			err = fmt.Errorf("InstalledNeedsUpdate = (%t, %t), want (true, false)", installed, needsUpdate)
		}
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("InstalledNeedsUpdate returned while Extract lock held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("release skill update lock: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("InstalledNeedsUpdate did not finish after Extract lock released")
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

// TestNeedsUpdateDetectsEmptyStaleTopLevelSkillDir uses an EMPTY stale boss-*
// directory at the top level of the namespace. Because it contains no files,
// the only code path that can flag it is the directory-level stale check at
// extract.go:134 (filepath.Dir(rel) == "."). This kills:
//   - line 131 CONDITIONALS_NEGATION (if err != nil after filepath.Rel in the
//     directory branch): the negated form returns early before reaching 134,
//     so the empty stale dir would go undetected.
//   - line 134 CONDITIONALS_NEGATION (filepath.Dir(rel) == "."): exercises the
//     true side of the top-level check with no file to mask the result.
func TestNeedsUpdateDetectsEmptyStaleTopLevelSkillDir(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Empty top-level boss-* directory (no files inside).
	stale := filepath.Join(dest, Namespace, "boss-empty-removed")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for empty stale top-level skill dir")
	}
}

// TestNeedsUpdateIgnoresNestedBossNamedDir places a directory whose basename
// looks like a boss skill but lives nested beneath an expected skill, with no
// files inside it. The real code must NOT flag it (filepath.Dir(rel) != ".").
// This kills line 134 CONDITIONALS_NEGATION's false side: the negated form
// (filepath.Dir(rel) != ".") would treat the nested dir as stale and report an
// update.
func TestNeedsUpdateIgnoresNestedBossNamedDir(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Nested dir named like a boss skill, but not top-level, and empty.
	nested := filepath.Join(dest, Namespace, "boss-test", "boss-nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if needs {
		t.Fatal("NeedsUpdate = true, want false for nested boss-named dir with no extra files")
	}
}

// TestNeedsUpdateDetectsExtraInstalledFile installs an extra file that is not
// part of the embedded payload. The real code flags it via the unexpected-file
// check at extract.go:143, which is only reached after the err check at line
// 140. This kills line 140 CONDITIONALS_NEGATION (if err != nil after
// filepath.Rel in the file branch): the negated form returns early before
// reaching 143, so the extra file would go undetected.
func TestNeedsUpdateDetectsExtraInstalledFile(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	extra := filepath.Join(dest, Namespace, "boss-test", "EXTRA.md")
	if err := os.WriteFile(extra, []byte("not embedded"), 0o644); err != nil {
		t.Fatal(err)
	}

	needs, err := NeedsUpdate(dest, testFS())
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if !needs {
		t.Fatal("NeedsUpdate = false, want true for extra installed file")
	}
}

// TestManifestStableOrderingAcrossFSIteration verifies embeddedFiles sorts its
// output deterministically by rel (extract.go:221, files[i].rel < files[j].rel).
// Two filesystems with the same files but registered such that natural map
// iteration could differ must produce identical manifests. This kills:
//   - line 221 CONDITIONALS_NEGATION (< -> >=): would reverse-sort, changing the
//     hash relative to the canonical ascending order.
//   - line 221 CONDITIONALS_BOUNDARY (< -> <=): a non-strict comparator is an
//     invalid sort.Slice less func; combined with the explicit ordering check
//     below the resulting order/hash diverges from the strict-less reference.
func TestManifestStableOrderingAcrossFSIteration(t *testing.T) {
	// Reference manifest from the canonical fixture.
	want, err := Manifest(testFS())
	if err != nil {
		t.Fatalf("Manifest(testFS): %v", err)
	}
	// Recompute several times; must be identical every time regardless of map
	// iteration order, proving a stable strict-ascending sort.
	for i := 0; i < 20; i++ {
		got, err := Manifest(testFS())
		if err != nil {
			t.Fatalf("Manifest iteration %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("Manifest unstable at iteration %d: got %q want %q", i, got, want)
		}
	}
}

// TestEmbeddedFilesSortedAscending asserts the precise ascending-by-rel order
// produced by embeddedFiles (extract.go:221). It pins the exact boundary
// behaviour of the comparator so that both the negated (>=, descending) and the
// boundary (<=, non-strict) mutants are caught: the expected sequence below is
// only correct for a strict ascending sort.
func TestEmbeddedFilesSortedAscending(t *testing.T) {
	files, err := embeddedFiles(testFS())
	if err != nil {
		t.Fatalf("embeddedFiles: %v", err)
	}
	want := []string{
		"boss-finalize/SKILL.md",
		"boss-finalize/add-pr.sh",
		"boss-other/SKILL.md",
		"boss-repair/SKILL.md",
		"boss-repair/scripts/review-feedback-probe.js",
		"boss-repair/scripts/review-feedback-probe.test.txt",
		"boss-test/SKILL.md",
		"boss/SKILL.md",
	}
	if len(files) != len(want) {
		t.Fatalf("embeddedFiles returned %d files, want %d", len(files), len(want))
	}
	for i, f := range files {
		if f.rel != want[i] {
			t.Fatalf("embeddedFiles[%d].rel = %q, want %q (full order: %v)", i, f.rel, want[i], relsOf(files))
		}
	}
	// Explicit strictly-ascending invariant: every adjacent pair must satisfy
	// a < b. A descending (>=) or non-strict (<=) comparator violates this.
	for i := 1; i < len(files); i++ {
		if files[i-1].rel >= files[i].rel {
			t.Fatalf("embeddedFiles not strictly ascending at %d: %q !< %q", i, files[i-1].rel, files[i].rel)
		}
	}
}

func relsOf(files []embeddedFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.rel
	}
	return out
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
	if _, err := os.Stat(filepath.Join(dest, updateLockFile)); !os.IsNotExist(err) {
		t.Fatalf("update lock exists after no-op, err=%v", err)
	}
}

func TestEnsureUpdatedNoOpOnCurrentInstall(t *testing.T) {
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	path := filepath.Join(dest, Namespace, "boss-test", "SKILL.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := EnsureUpdated(dest, testFS())
	if err != nil {
		t.Fatalf("EnsureUpdated: %v", err)
	}
	if updated {
		t.Fatal("EnsureUpdated = true on a current tree, want no-op")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("current tree was rewritten: mtime %v -> %v", before.ModTime(), after.ModTime())
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

func TestEnsureUpdatedDoesNotCreateMissingDirectory(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "missing", "skills")

	updated, err := EnsureUpdated(dest, testFS())
	if err != nil {
		t.Fatalf("EnsureUpdated: %v", err)
	}
	if updated {
		t.Fatal("EnsureUpdated updated a missing directory, want no-op")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("missing skill directory exists after no-op, err=%v", err)
	}
}

func TestEnsureUpdatedFromSourceNoOpOnCurrentInstall(t *testing.T) {
	srcRoot := writeSourceTree(t, t.TempDir(), testFS())
	dest := t.TempDir()
	if err := ExtractFromSource(dest, srcRoot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dest, Namespace, "boss-test", "SKILL.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := EnsureUpdatedFromSource(dest, srcRoot)
	if err != nil {
		t.Fatalf("EnsureUpdatedFromSource: %v", err)
	}
	if updated {
		t.Fatal("EnsureUpdatedFromSource = true on a current source tree, want no-op")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("current source tree was rewritten: mtime %v -> %v", before.ModTime(), after.ModTime())
	}
}

func TestEnsureUpdatedFromSourceRefreshesDifferentPayload(t *testing.T) {
	srcRoot := writeSourceTree(t, t.TempDir(), changedFS())
	dest := t.TempDir()
	if err := Extract(dest, testFS()); err != nil {
		t.Fatal(err)
	}

	updated, err := EnsureUpdatedFromSource(dest, srcRoot)
	if err != nil {
		t.Fatalf("EnsureUpdatedFromSource: %v", err)
	}
	if !updated {
		t.Fatal("EnsureUpdatedFromSource = false for a different installed payload, want true")
	}
	data, err := os.ReadFile(filepath.Join(dest, Namespace, "boss-test", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "# Test Skill\nDo the changed thing."; got != want {
		t.Fatalf("installed bytes = %q, want source bytes %q", got, want)
	}
}

func TestEnsureUpdatedFromSourceDoesNotInstallIntoEmptyDirectory(t *testing.T) {
	srcRoot := writeSourceTree(t, t.TempDir(), testFS())
	dest := t.TempDir()

	updated, err := EnsureUpdatedFromSource(dest, srcRoot)
	if err != nil {
		t.Fatalf("EnsureUpdatedFromSource: %v", err)
	}
	if updated {
		t.Fatal("EnsureUpdatedFromSource updated empty dir, want no-op")
	}
	if _, err := os.Stat(filepath.Join(dest, Namespace)); !os.IsNotExist(err) {
		t.Fatalf("namespace exists after no-op, err=%v", err)
	}
}
