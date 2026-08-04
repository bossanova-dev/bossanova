package tccprobe

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
