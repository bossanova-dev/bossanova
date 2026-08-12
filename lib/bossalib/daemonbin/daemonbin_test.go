package daemonbin_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestInspectAcrossRealHomebrewVersionBump drives the classifier through an
// actual Cellar 1.90.0 → 1.91.0 bump with real files and real Stage calls —
// the "verified against a real version bump, not asserted" criterion.
func TestInspectAcrossRealHomebrewVersionBump(t *testing.T) {
	root := t.TempDir()
	appDataDir := filepath.Join(root, "app-data")
	staged := daemonbin.StagedPath(appDataDir)
	cellar := filepath.Join(root, "opt", "homebrew", "Cellar", "bossanova")

	src190 := writeBossd(t, filepath.Join(cellar, "1.90.0", "bin", "bossd"), "bossd binary bytes v1.90.0")
	if err := daemonbin.Stage(src190, staged); err != nil {
		t.Fatalf("Stage(1.90.0): %v", err)
	}
	// Pin the staged mtime so the ordering assertions cannot flake on clock or
	// filesystem-timestamp granularity while still exercising the real Stage.
	base := time.Now().Truncate(time.Second).Add(-time.Hour)
	setModTime(t, staged, base)
	// The daemon started just after 1.90.0 was staged, so it is current.
	startedAt := base.Add(time.Minute)

	if !daemonbin.IsHomebrewCellarBinary(src190) {
		t.Fatalf("IsHomebrewCellarBinary(%q) = false, want true", src190)
	}

	// brew upgrade: 1.90.0 is removed, 1.91.0 appears with different bytes and
	// a different size. The staged file is untouched, so the daemon keeps
	// running it — the exact BOS-864 failure.
	if err := os.RemoveAll(filepath.Join(cellar, "1.90.0")); err != nil {
		t.Fatalf("remove old Cellar version: %v", err)
	}
	src191 := writeBossd(t, filepath.Join(cellar, "1.91.0", "bin", "bossd"), "bossd binary bytes v1.91.0 with more content")

	upgraded, err := daemonbin.Inspect(src191, staged, startedAt)
	if err != nil {
		t.Fatalf("Inspect() after upgrade: %v", err)
	}
	if !upgraded.StagedKnown || !upgraded.StagedBehindSource {
		t.Fatalf("Inspect() after upgrade = %+v, want a known StagedBehindSource verdict", upgraded)
	}
	if !upgraded.RunningKnown || upgraded.RunningBehindStaged {
		t.Fatalf("Inspect() after upgrade = %+v, want RunningBehindStaged false (staged file unchanged)", upgraded)
	}
	if upgraded.Reason != daemonbin.ReasonSizeMismatch {
		t.Fatalf("Inspect() after upgrade reason = %q, want %q", upgraded.Reason, daemonbin.ReasonSizeMismatch)
	}

	// `boss daemon restart` re-stages, but until the daemon is replaced the
	// live process is behind the file. The flags must invert.
	if err := daemonbin.Stage(src191, staged); err != nil {
		t.Fatalf("Stage(1.91.0): %v", err)
	}
	setModTime(t, staged, base.Add(2*time.Minute))

	restaged, err := daemonbin.Inspect(src191, staged, startedAt)
	if err != nil {
		t.Fatalf("Inspect() after re-stage: %v", err)
	}
	if !restaged.StagedKnown || restaged.StagedBehindSource {
		t.Fatalf("Inspect() after re-stage = %+v, want StagedBehindSource false", restaged)
	}
	if !restaged.RunningKnown || !restaged.RunningBehindStaged {
		t.Fatalf("Inspect() after re-stage = %+v, want RunningBehindStaged true", restaged)
	}
	if restaged.Reason != daemonbin.ReasonRunningBehindStaged {
		t.Fatalf("Inspect() after re-stage reason = %q, want %q", restaged.Reason, daemonbin.ReasonRunningBehindStaged)
	}

	// A daemon started after the re-stage is fully current on both axes.
	current, err := daemonbin.Inspect(src191, staged, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Inspect() after restart: %v", err)
	}
	if current.StagedBehindSource || current.RunningBehindStaged || current.Reason != "" {
		t.Fatalf("Inspect() after restart = %+v, want fully current with no reason", current)
	}
}

// TestInspectNeverWritesTheStagedBinary is the crash-loop / dev-build-downgrade
// impossibility proof: detection is pure comparison.
func TestInspectNeverWritesTheStagedBinary(t *testing.T) {
	root := t.TempDir()
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	source := writeBossd(t, filepath.Join(root, "Cellar", "bossanova", "1.90.0", "bin", "bossd"), "released bytes")
	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}
	// A locally-built dev binary that differs from the released source: the
	// branch where an auto-re-stage would silently downgrade it.
	devBuild := writeBossd(t, filepath.Join(root, "checkout", "bin", "bossd"), "locally built dev bytes, different length")

	wantInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged: %v", err)
	}
	wantSize, wantModTime, wantHash := wantInfo.Size(), wantInfo.ModTime(), sha256File(t, staged)

	startedAt := wantModTime.Add(time.Minute)
	for _, tc := range []struct {
		name       string
		sourcePath string
		startedAt  time.Time
	}{
		{"current source", source, startedAt},
		{"dev build source", devBuild, startedAt},
		{"missing source", filepath.Join(root, "absent", "bossd"), startedAt},
		{"zero start time", source, time.Time{}},
		{"daemon behind staged", source, wantModTime.Add(-time.Hour)},
	} {
		for range 3 {
			if _, err := daemonbin.Inspect(tc.sourcePath, staged, tc.startedAt); err != nil {
				t.Fatalf("Inspect(%s): %v", tc.name, err)
			}
		}
	}

	gotInfo, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("stat staged after Inspect: %v", err)
	}
	if gotInfo.Size() != wantSize {
		t.Errorf("staged size after Inspect = %d, want %d", gotInfo.Size(), wantSize)
	}
	if !gotInfo.ModTime().Equal(wantModTime) {
		t.Errorf("staged mtime after Inspect = %v, want %v", gotInfo.ModTime(), wantModTime)
	}
	if got := sha256File(t, staged); got != wantHash {
		t.Errorf("staged SHA-256 after Inspect = %s, want %s", got, wantHash)
	}
	if got := readFile(t, devBuild); got != "locally built dev bytes, different length" {
		t.Errorf("dev build contents changed: %q", got)
	}
}

func TestInspectEqualSizeSourcesHashToDecide(t *testing.T) {
	root := t.TempDir()
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "AAAAAAAAAA")
	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}
	stagedModTime := modTime(t, staged)
	startedAt := stagedModTime.Add(time.Minute)

	// Same version reinstalled: identical bytes, refreshed source mtime. No
	// false warning.
	setModTime(t, source, stagedModTime.Add(time.Hour))
	same, err := daemonbin.Inspect(source, staged, startedAt)
	if err != nil {
		t.Fatalf("Inspect() equal size, equal content: %v", err)
	}
	if !same.StagedKnown || same.StagedBehindSource || same.Reason != "" {
		t.Fatalf("Inspect() same-version reinstall = %+v, want current with no reason", same)
	}

	// Equal size, different content: must hash and report stale.
	writeFile(t, source, "BBBBBBBBBB")
	differing, err := daemonbin.Inspect(source, staged, startedAt)
	if err != nil {
		t.Fatalf("Inspect() equal size, different content: %v", err)
	}
	if !differing.StagedKnown || !differing.StagedBehindSource {
		t.Fatalf("Inspect() equal-size mismatch = %+v, want StagedBehindSource true", differing)
	}
	if differing.Reason != daemonbin.ReasonContentMismatch {
		t.Fatalf("Inspect() equal-size mismatch reason = %q, want %q", differing.Reason, daemonbin.ReasonContentMismatch)
	}
}

func TestIsHomebrewCellarBinaryResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	brewPrefix := filepath.Join(root, "opt", "homebrew")
	cellarBinary := writeBossd(t, filepath.Join(brewPrefix, "Cellar", "bossanova", "1.91.0", "bin", "bossd"), "released")

	// The released layout: <brewPrefix>/bin/bossd is a symlink into the Cellar,
	// and ResolveBossdPath hands the classifier the symlink, not the target.
	linkPath := filepath.Join(brewPrefix, "bin", "bossd")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatalf("mkdir brew bin: %v", err)
	}
	if err := os.Symlink(cellarBinary, linkPath); err != nil {
		t.Fatalf("symlink brew bin: %v", err)
	}

	devBuild := writeBossd(t, filepath.Join(root, "checkout", "bin", "bossd"), "dev")

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"real Cellar path", cellarBinary, true},
		{"symlink into the Cellar", linkPath, true},
		{"checkout dev build", devBuild, false},
		{"plain /usr/local install", "/usr/local/bin/bossd", false},
		{"empty path", "", false},
		{"cellar-shaped path for another formula", filepath.Join(root, "Cellar", "other", "1.0.0", "bin", "bossd"), false},
	} {
		if got := daemonbin.IsHomebrewCellarBinary(tc.path); got != tc.want {
			t.Errorf("IsHomebrewCellarBinary(%s = %q) = %t, want %t", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestHomebrewCellarPrefixes(t *testing.T) {
	for _, tc := range []struct {
		binDir      string
		wantFormula string
		wantBrew    string
		wantOK      bool
	}{
		{
			binDir:      filepath.Join("/opt", "homebrew", "Cellar", "bossanova", "2.1.197", "bin"),
			wantFormula: filepath.Join("/opt", "homebrew", "Cellar", "bossanova", "2.1.197"),
			wantBrew:    filepath.Join("/opt", "homebrew"),
			wantOK:      true,
		},
		{
			binDir:      filepath.Join("/usr", "local", "Cellar", "bossanova", "v1.2.3", "bin"),
			wantFormula: filepath.Join("/usr", "local", "Cellar", "bossanova", "v1.2.3"),
			wantBrew:    filepath.Join("/usr", "local"),
			wantOK:      true,
		},
		{binDir: filepath.Join("/usr", "local", "bin")},
		{binDir: filepath.Join("/opt", "homebrew", "Cellar", "other", "1.0.0", "bin")},
		{binDir: filepath.Join("/opt", "homebrew", "Cellar", "bossanova", "1.0.0", "libexec")},
	} {
		formula, brew, ok := daemonbin.HomebrewCellarPrefixes(tc.binDir)
		if ok != tc.wantOK || formula != tc.wantFormula || brew != tc.wantBrew {
			t.Errorf("HomebrewCellarPrefixes(%q) = (%q, %q, %t), want (%q, %q, %t)",
				tc.binDir, formula, brew, ok, tc.wantFormula, tc.wantBrew, tc.wantOK)
		}
	}
}

func TestInspectReportsUndeterminableInputsAsUnknown(t *testing.T) {
	root := t.TempDir()
	staged := daemonbin.StagedPath(filepath.Join(root, "app-data"))
	source := writeBossd(t, filepath.Join(root, "source", "bossd"), "released")

	missingStaged, err := daemonbin.Inspect(source, staged, time.Now())
	if err != nil {
		t.Fatalf("Inspect() missing staged: %v", err)
	}
	if missingStaged.RunningKnown {
		t.Fatalf("Inspect() missing staged = %+v, want RunningKnown false", missingStaged)
	}
	if !missingStaged.StagedBehindSource || missingStaged.Reason != daemonbin.ReasonStagedMissing {
		t.Fatalf("Inspect() missing staged = %+v, want a staged-missing stale verdict", missingStaged)
	}

	if err := daemonbin.Stage(source, staged); err != nil {
		t.Fatalf("Stage(): %v", err)
	}

	missingSource, err := daemonbin.Inspect(filepath.Join(root, "absent", "bossd"), staged, modTime(t, staged).Add(time.Minute))
	if err != nil {
		t.Fatalf("Inspect() missing source: %v", err)
	}
	if missingSource.StagedKnown || missingSource.StagedBehindSource {
		t.Fatalf("Inspect() missing source = %+v, want StagedKnown false", missingSource)
	}
	if missingSource.Reason != daemonbin.ReasonSourceUnavailable {
		t.Fatalf("Inspect() missing source reason = %q, want %q", missingSource.Reason, daemonbin.ReasonSourceUnavailable)
	}

	zeroStart, err := daemonbin.Inspect(source, staged, time.Time{})
	if err != nil {
		t.Fatalf("Inspect() zero start time: %v", err)
	}
	if zeroStart.RunningKnown || zeroStart.RunningBehindStaged {
		t.Fatalf("Inspect() zero start time = %+v, want RunningKnown false", zeroStart)
	}
	if zeroStart.Reason != daemonbin.ReasonDaemonStartUnknown {
		t.Fatalf("Inspect() zero start reason = %q, want %q", zeroStart.Reason, daemonbin.ReasonDaemonStartUnknown)
	}
	if zeroStart.StagedBehindSource {
		t.Fatalf("Inspect() zero start time = %+v, want the staged verdict to remain current", zeroStart)
	}
}

func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	return info.ModTime()
}

func setModTime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %q: %v", path, err)
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
