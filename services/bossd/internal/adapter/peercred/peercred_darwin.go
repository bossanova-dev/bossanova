//go:build darwin

package peercred

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func peerPID(fd uintptr) (int, error) {
	return unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}

// ParentPID returns the current parent of pid, or a process-gone error.
func ParentPID(pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("process %d: %w", pid, os.ErrNotExist)
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// Darwin returns EIO for the empty sysctl result when a process has exited.
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) {
			return 0, fmt.Errorf("process %d: %w", pid, os.ErrNotExist)
		}
		return 0, fmt.Errorf("read process %d: %w", pid, err)
	}
	return int(info.Eproc.Ppid), nil
}
