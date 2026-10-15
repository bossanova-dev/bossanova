package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Use a host-wide path: HOME and TMPDIR may differ between racing daemons.
var fixedPortLockDir = "/tmp"

const (
	// One critical section is a probe plus a bind. Eight probe budgets leave
	// headroom for scheduling and binding while bounding a frozen owner's delay.
	fixedPortLockWait = 8 * fixedPortProbeTimeout
	fixedPortLockPoll = 10 * time.Millisecond
)

// acquireFixedPortLock serializes the fixed-port probe and bind. The empty lock
// file persists: unlinking it would let another opener lock a different inode.
func acquireFixedPortLock(port int, wait time.Duration) (release func(), err error) {
	path := filepath.Join(fixedPortLockDir, fmt.Sprintf("bossd-failover-proxy-%d.lock", port))
	// #nosec G304 G302 -- fixed host-wide lock keyed only by port; no-follow refuses symlinks, read-only permits cross-user locking
	// owner=@recurser review-by=2027-01-13 issue=BOS-1617
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open fixed port lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("acquire fixed port lock: %w", err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for fixed port lock: %w", err)
		}
		time.Sleep(min(fixedPortLockPoll, remaining))
	}
}
