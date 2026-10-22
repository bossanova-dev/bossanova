package tccprobe

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProtectedRootsFor(t *testing.T) {
	t.Parallel()

	home := filepath.Join(string(filepath.Separator), "Users", "alice")
	documents := filepath.Join(home, "Documents")
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")

	tests := []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "path under Documents selects Documents",
			paths: []string{filepath.Join(documents, "projects", "bossanova")},
			want:  []string{documents},
		},
		{
			name:  "paths under Desktop and Downloads select both roots",
			paths: []string{filepath.Join(downloads, "repo"), filepath.Join(desktop, "worktrees")},
			want:  []string{desktop, downloads},
		},
		{
			name:  "unrelated path selects no roots",
			paths: []string{filepath.Join(home, "Code", "bossanova")},
		},
		{
			name:  "path equal to root selects it",
			paths: []string{documents},
			want:  []string{documents},
		},
		{
			name: "duplicate paths under one root collapse",
			paths: []string{
				filepath.Join(documents, "repo"),
				filepath.Join(documents, "repo", "worktrees"),
				documents,
			},
			want: []string{documents},
		},
		{
			name:  "similarly prefixed sibling is excluded",
			paths: []string{filepath.Join(home, "Documents-old", "repo")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ProtectedRootsFor(home, tt.paths); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ProtectedRootsFor(%q, %q) = %q, want %q", home, tt.paths, got, tt.want)
			}
		})
	}
}

func TestRemedyNamesRelocationOption(t *testing.T) {
	t.Parallel()

	got := strings.ToLower(Remedy("/Users/alice/Documents", "/opt/homebrew/bin/bossd", StatusBlocked))

	if !strings.Contains(got, "relocat") {
		t.Errorf("Remedy() = %q, want it to mention relocating the repository/worktree base", got)
	}
	for _, dir := range []string{"documents", "desktop", "downloads"} {
		if !strings.Contains(got, dir) {
			t.Errorf("Remedy() = %q, want it to name %q among the guarded directories", got, dir)
		}
	}
}

func TestRemedyStatesNoDisplayConstraint(t *testing.T) {
	t.Parallel()

	got := strings.ToLower(Remedy("/Users/alice/Desktop", "/opt/homebrew/bin/bossd", StatusBlocked))

	if !strings.Contains(got, "no display") && !strings.Contains(got, "no one") && !strings.Contains(got, "nobody") {
		t.Errorf("Remedy() = %q, want it to explicitly state the no-display/no-one-at-the-screen constraint", got)
	}
}

func TestRemedyNeverRecommendsReinstalling(t *testing.T) {
	t.Parallel()

	for _, status := range []Status{StatusBlocked, StatusDenied} {
		got := strings.ToLower(Remedy("/Users/alice/Downloads", "/opt/homebrew/bin/bossd", status))

		for _, banned := range []string{"reinstall", "re-install", "redownload", "re-download"} {
			if strings.Contains(got, banned) {
				t.Errorf("Remedy(%v) = %q, must not suggest %q", status, got, banned)
			}
		}
	}
}

// TestRemedyOffersTheDialogRouteOnlyWhenOneIsPending covers the plan's
// "Blocked and Denied have different remedies" requirement, which the labels
// alone do not satisfy. Denied means TCC already returned a decision: there is
// no dialog to answer, so offering that route would send the operator down a
// dead end. Both variants must still state the no-display constraint and the
// relocation route, which are what make the advice usable headless.
func TestRemedyOffersTheDialogRouteOnlyWhenOneIsPending(t *testing.T) {
	t.Parallel()

	blocked := strings.ToLower(Remedy("/Users/alice/Documents", "bossd", StatusBlocked))
	denied := strings.ToLower(Remedy("/Users/alice/Documents", "bossd", StatusDenied))

	if !strings.Contains(blocked, "dialog is pending") {
		t.Errorf("Remedy(Blocked) = %q, want it to offer answering the pending dialog", blocked)
	}
	if strings.Contains(denied, "dialog") {
		t.Errorf("Remedy(Denied) = %q, must not mention a dialog — a denial means TCC already decided", denied)
	}
	for _, want := range []string{"relocate", "no display"} {
		if !strings.Contains(denied, want) {
			t.Errorf("Remedy(Denied) = %q, want it to keep %q", denied, want)
		}
		if !strings.Contains(blocked, want) {
			t.Errorf("Remedy(Blocked) = %q, want it to keep %q", blocked, want)
		}
	}
}

func TestProbeReadableDirectoryReturnsOK(t *testing.T) {
	dir := t.TempDir()

	results := Probe(context.Background(), []string{dir}, time.Second)

	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].Path != dir {
		t.Errorf("path = %q, want %q", results[0].Path, dir)
	}
	if results[0].Status != StatusOK {
		t.Errorf("status = %v, want %v", results[0].Status, StatusOK)
	}
	if results[0].Err != nil {
		t.Errorf("error = %v, want nil", results[0].Err)
	}
}

func TestProbeUnexpectedReadErrorReturnsError(t *testing.T) {
	want := errors.New("too many open files")
	withReadDir(t, func(string) ([]fs.DirEntry, error) { return nil, want })

	results := Probe(context.Background(), []string{"/unexpected"}, time.Second)
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].Status != StatusError {
		t.Errorf("status = %v, want %v", results[0].Status, StatusError)
	}
	if !errors.Is(results[0].Err, want) {
		t.Errorf("error = %v, want %v", results[0].Err, want)
	}
}

func TestProbeMissingDirectoryReturnsAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	results := Probe(context.Background(), []string{missing}, time.Second)

	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].Status != StatusAbsent {
		t.Errorf("status = %v, want %v", results[0].Status, StatusAbsent)
	}
	if !os.IsNotExist(results[0].Err) {
		t.Errorf("error = %v, want not-exist error", results[0].Err)
	}
}

func TestProbeUnreadableDirectoryReturnsDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 directories")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	results := Probe(context.Background(), []string{dir}, time.Second)

	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].Status != StatusDenied {
		t.Errorf("status = %v, want %v", results[0].Status, StatusDenied)
	}
	if !os.IsPermission(results[0].Err) {
		t.Errorf("error = %v, want permission error", results[0].Err)
	}
}

func TestProbeBlockedReadReturnsBlockedWithinTimeout(t *testing.T) {
	blocked := make(chan struct{})
	var workerDone []<-chan struct{}
	withReadDir(t, func(string) ([]fs.DirEntry, error) {
		<-blocked
		return nil, nil
	})

	timeout := 20 * time.Millisecond
	started := time.Now()
	results := ProbeWithTracker(context.Background(), []string{"/blocked"}, timeout, func(done <-chan struct{}) {
		workerDone = append(workerDone, done)
	})
	elapsed := time.Since(started)

	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].Status != StatusBlocked {
		t.Errorf("status = %v, want %v", results[0].Status, StatusBlocked)
	}
	if results[0].Err == nil {
		t.Error("error = nil, want timeout error")
	}
	if elapsed > 2*timeout {
		t.Errorf("probe took %s, want at most %s", elapsed, 2*timeout)
	}
	if len(workerDone) != 1 {
		t.Fatalf("tracked workers = %d, want 1", len(workerDone))
	}

	close(blocked)
	select {
	case <-workerDone[0]:
	case <-time.After(time.Second):
		t.Fatal("timed-out probe worker completion was not tracked")
	}
}

func TestProbeWithTrackerRetainsCancelledWorker(t *testing.T) {
	blocked := make(chan struct{})
	var workerDone []<-chan struct{}
	withReadDir(t, func(string) ([]fs.DirEntry, error) {
		<-blocked
		return nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := ProbeWithTracker(ctx, []string{"/blocked"}, time.Second, func(done <-chan struct{}) {
		workerDone = append(workerDone, done)
	})
	if len(results) != 1 || results[0].Status != StatusBlocked || !errors.Is(results[0].Err, context.Canceled) {
		t.Fatalf("cancelled probe result = %#v, want blocked cancellation", results)
	}
	if len(workerDone) != 1 {
		t.Fatalf("tracked workers = %d, want 1", len(workerDone))
	}

	close(blocked)
	select {
	case <-workerDone[0]:
	case <-time.After(time.Second):
		t.Fatal("cancelled probe worker completion was not tracked")
	}
}

func withReadDir(t *testing.T, fn func(string) ([]fs.DirEntry, error)) {
	t.Helper()
	readDirMu.Lock()
	previous := readDir
	readDir = fn
	readDirMu.Unlock()
	t.Cleanup(func() {
		readDirMu.Lock()
		readDir = previous
		readDirMu.Unlock()
	})
}
