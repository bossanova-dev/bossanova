//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package credmaterialize

import (
	"io/fs"
	"syscall"
)

// authFileHasMultipleLinks reports whether a regular auth.json has aliases that
// may belong to another account. If the platform metadata is unexpected, refuse
// the read; replacing the leaf remains safe and restores account-local bytes.
func authFileHasMultipleLinks(_ string, info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink > 1
}
