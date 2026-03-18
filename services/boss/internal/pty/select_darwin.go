package pty

import "syscall"

// sysSelect wraps syscall.Select for Darwin where it returns only an error.
func sysSelect(nfd int, r *syscall.FdSet, w *syscall.FdSet, e *syscall.FdSet, timeout *syscall.Timeval) (int, error) {
	err := syscall.Select(nfd, r, w, e, timeout)
	if err != nil {
		return 0, err
	}
	return 1, nil
}
