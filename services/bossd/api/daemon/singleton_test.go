package server

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireSingletonLock_SecondAcquireFails(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), LockFileName)

	first, err := AcquireSingletonLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Close() }()

	// A second acquire while the first lock is held must be refused so a
	// duplicate bossd cannot start and steal the socket.
	second, err := AcquireSingletonLock(lockPath)
	if !errors.Is(err, ErrDaemonAlreadyRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second acquire: got err=%v, want ErrDaemonAlreadyRunning", err)
	}
}

func TestAcquireSingletonLock_ReacquireAfterRelease(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), LockFileName)

	first, err := AcquireSingletonLock(lockPath)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Releasing the lock (process exit / Close) must let a fresh daemon take it.
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := AcquireSingletonLock(lockPath)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = second.Close()
}
