package pty

import "syscall"

// sysSelect wraps syscall.Select for Linux where it returns (n, error).
func sysSelect(nfd int, r *syscall.FdSet, w *syscall.FdSet, e *syscall.FdSet, timeout *syscall.Timeval) (int, error) {
	return syscall.Select(nfd, r, w, e, timeout)
}
