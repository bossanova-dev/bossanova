//go:build !darwin && !linux

package skillinstall

import "syscall"

func swapDirAtomic(_, _ string) error {
	return syscall.ENOSYS
}
