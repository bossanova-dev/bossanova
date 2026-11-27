//go:build darwin

package skillinstall

import "golang.org/x/sys/unix"

// swapDirAtomic atomically exchanges two existing directory paths on the same
// filesystem in a single kernel operation.
func swapDirAtomic(stage, target string) error {
	return unix.RenamexNp(stage, target, unix.RENAME_SWAP)
}
