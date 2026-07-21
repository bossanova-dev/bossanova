package server

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// socketIsServing reports whether a daemon is actively accepting connections on
// the Unix socket at path. A successful dial means another bossd owns it.
func socketIsServing(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ErrDaemonAlreadyRunning is returned by AcquireSingletonLock when another
// bossd process already holds the singleton lock. Callers should treat this as
// a clean "nothing to do" exit rather than an error: a daemon is already
// serving, so this process must not start (and must NOT steal the socket).
var ErrDaemonAlreadyRunning = errors.New("another bossd is already running")

// LockFileName is the singleton lock file, kept alongside the socket and DB in
// the bossanova data directory.
const LockFileName = "bossd.lock"

// DefaultLockPath returns the singleton lock path in the same data directory as
// the socket and database (see DefaultSocketPath).
func DefaultLockPath() (string, error) {
	sock, err := DefaultSocketPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(sock), LockFileName), nil
}

// AcquireSingletonLock takes an exclusive, non-blocking advisory lock (flock) on
// lockPath so that only one bossd can own the socket and SQLite database at a
// time. Without this guard a second bossd would os.Remove the live socket and
// rebind it (Server.Listen), stealing it from the running instance — two
// daemons would then contend over the same DB, which manifests as the TUI
// flashing "Cannot connect to daemon".
//
// The returned *os.File MUST be kept open for the lifetime of the process; the
// flock is held until the descriptor is closed or the process exits (including
// on crash, which is why flock is preferred over a bare pidfile). Close it
// during graceful shutdown to release the lock promptly.
//
// Returns ErrDaemonAlreadyRunning if another process already holds the lock.
func AcquireSingletonLock(lockPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	// #nosec G304 -- daemon singleton-lock path derived from config/data dir; no operator input
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}

	return f, nil
}
