//go:build linux

package skillinstall

import "golang.org/x/sys/unix"

// swapDirAtomic atomically exchanges two existing directory paths on the same
// filesystem in a single kernel operation.
func swapDirAtomic(stage, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, target, unix.RENAME_EXCHANGE)
}
