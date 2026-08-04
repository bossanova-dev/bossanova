package daemonbin_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/daemonbin"
)

func TestStagedPathIsStableAcrossVersionBump(t *testing.T) {
	root := t.TempDir()
	appDataDir := filepath.Join(root, "app-data")
	src190 := writeBossd(t, filepath.Join(root, "Cellar", "bossanova", "1.90.0", "bin", "bossd"), "v1.90.0")
	staged := daemonbin.StagedPath(appDataDir)

	if err := daemonbin.Stage(src190, staged); err != nil {
		t.Fatalf("Stage(1.90.0): %v", err)
	}
	firstPath := daemonbin.StagedPath(appDataDir)

	if err := os.RemoveAll(filepath.Join(root, "Cellar", "bossanova", "1.90.0")); err != nil {
		t.Fatalf("remove old Cellar version: %v", err)
	}
	src191 := writeBossd(t, filepath.Join(root, "Cellar", "bossanova", "1.91.0", "bin", "bossd"), "v1.91.0")
	if err := daemonbin.Stage(src191, staged); err != nil {
		t.Fatalf("Stage(1.91.0): %v", err)
	}

	if got := daemonbin.StagedPath(appDataDir); got != firstPath {
		t.Fatalf("StagedPath() after version bump = %q, want stable path %q", got, firstPath)
	}
	if got := readFile(t, staged); got != "v1.91.0" {
		t.Fatalf("staged contents = %q, want %q", got, "v1.91.0")
	}
	if got := fileMode(t, staged); got != 0o755 {
		t.Fatalf("staged mode = %o, want 0755", got)
	}
}

func TestStagedPathIsUnderAppDataBinDir(t *testing.T) {
	if got, want := daemonbin.StagedPath("/a/b"), filepath.Join("/a/b", "bin", "bossd"); got != want {
		t.Fatalf("StagedPath() = %q, want %q", got, want)
	}
}

func TestNeedsStageDetectsChangedContents(t *testing.T) {
	root := t.TempDir()
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "first")
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}

	needs, reason, err := daemonbin.NeedsStage(source, staged)
	if err != nil {
		t.Fatalf("NeedsStage() unchanged: %v", err)
	}
	if needs || reason != "" {
		t.Fatalf("NeedsStage() unchanged = (%t, %q), want (false, empty)", needs, reason)
	}

	writeFile(t, source, "second")
	needs, reason, err = daemonbin.NeedsStage(source, staged)
	if err != nil {
		t.Fatalf("NeedsStage() changed: %v", err)
	}
	if !needs {
		t.Fatal("NeedsStage() after source content change = false, want true")
	}
	if !strings.Contains(reason, "content") || !strings.Contains(reason, "mismatch") {
		t.Fatalf("NeedsStage() changed reason = %q, want content-mismatch reason", reason)
	}
}

func TestNeedsStageWhenTargetMissing(t *testing.T) {
	root := t.TempDir()
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "source")
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))

	needs, reason, err := daemonbin.NeedsStage(source, staged)
	if err != nil {
		t.Fatalf("NeedsStage(): %v", err)
	}
	if !needs {
		t.Fatal("NeedsStage() with missing target = false, want true")
	}
	if !strings.Contains(reason, "staged binary missing") {
		t.Fatalf("NeedsStage() missing reason = %q, want missing staged binary", reason)
	}
}

func TestNeedsStageWhenTargetNotExecutable(t *testing.T) {
	root := t.TempDir()
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "source")
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}
	if err := os.Chmod(staged, 0o644); err != nil {
		t.Fatalf("chmod staged binary: %v", err)
	}

	needs, reason, err := daemonbin.NeedsStage(source, staged)
	if err != nil {
		t.Fatalf("NeedsStage(): %v", err)
	}
	if !needs {
		t.Fatal("NeedsStage() with non-executable target = false, want true")
	}
	if !strings.Contains(reason, "executable") {
		t.Fatalf("NeedsStage() non-executable reason = %q, want executable reason", reason)
	}
}

func TestStageIsAtomicOverExistingTarget(t *testing.T) {
	root := t.TempDir()
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "fresh")
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	writeFile(t, staged, "old")

	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}
	if got := readFile(t, staged); got != "fresh" {
		t.Fatalf("staged contents = %q, want %q", got, "fresh")
	}
	if got := fileMode(t, staged); got != 0o755 {
		t.Fatalf("staged mode = %o, want 0755", got)
	}
	entries, err := os.ReadDir(filepath.Dir(staged))
	if err != nil {
		t.Fatalf("read staged directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(staged) {
		t.Fatalf("staged directory entries = %v, want only %q", entryNames(entries), filepath.Base(staged))
	}
}

func TestStageRejectsMissingSource(t *testing.T) {
	root := t.TempDir()
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))

	if err := daemonbin.Stage(filepath.Join(root, "does-not-exist"), staged); err == nil {
		t.Fatal("Stage() missing source returned nil error")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged target after missing source stat error = %v, want not exist", err)
	}

	writeFile(t, staged, "existing target")
	if err := daemonbin.Stage(filepath.Join(root, "still-does-not-exist"), staged); err == nil {
		t.Fatal("Stage() missing source with existing target returned nil error")
	}
	if got := readFile(t, staged); got != "existing target" {
		t.Fatalf("existing staged target after missing source = %q, want unchanged contents", got)
	}
}

func TestStageIsIdempotent(t *testing.T) {
	root := t.TempDir()
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "unchanged")
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))

	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("first Stage(): %v", err)
	}
	first := sha256File(t, staged)
	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("second Stage(): %v", err)
	}
	if got := sha256File(t, staged); got != first {
		t.Fatalf("staged SHA-256 after repeated Stage() = %s, want %s", got, first)
	}
}

func writeBossd(t *testing.T, path, contents string) string {
	t.Helper()
	writeFile(t, path, contents)
	return path
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("make parent directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(contents)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.Mode().Perm()
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(contents))
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
