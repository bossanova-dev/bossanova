//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows)

package credmaterialize

import "io/fs"

// authFileHasMultipleLinks conservatively refuses reconciliation on platforms
// without portable link-count metadata. The atomic replacement remains safe.
func authFileHasMultipleLinks(_ string, _ fs.FileInfo) bool {
	return true
}
