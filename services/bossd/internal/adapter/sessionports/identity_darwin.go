//go:build darwin

package sessionports

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"
)

// readProcessIdentity reads a process's birth identity on macOS via
// sysctl KERN_PROC_PID, whose kinfo_proc carries the process start time
// (p_starttime). A missing process makes the sysctl return a zero-length
// buffer, which golang.org/x/sys/unix surfaces as unix.EIO (older kernels may
// use ESRCH); both mean "no such process" and are reported as an authoritative
// absence (Present=false, nil error). Any other syscall error is transient
// (the caller marks only the owning sessions incomplete).
//
// This lives in a darwin-only file because unix.SysctlKinfoProc and the
// kinfo_proc layout are BSD/Darwin symbols absent from the Linux
// golang.org/x/sys/unix build; a runtime GOOS guard would not gate
// compile-time symbol resolution.
func readProcessIdentity(_ context.Context, pid int) (Identity, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return Identity{Present: false}, nil
		}
		return Identity{}, err
	}
	// Fold the start-time timeval into a single comparable token
	// (microseconds since the epoch). Only equality across reads matters.
	start := kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)
	return Identity{StartTime: start, Present: true}, nil
}
