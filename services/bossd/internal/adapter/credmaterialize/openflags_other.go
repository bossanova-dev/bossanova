//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package credmaterialize

import "os"

// safeLeafOpenFlags is zero on platforms without O_NOFOLLOW/O_NONBLOCK.
const safeLeafOpenFlags = 0

// symlinkedLeafRefused reports whether the leaf must be refused because it is a
// symlink, for platforms whose open(2) cannot refuse one for us.
//
// This exists because the unix path's guarantee does NOT degrade gracefully.
// Without O_NOFOLLOW the open follows a symlink and the post-open f.Stat()
// describes the TARGET — reporting it as a regular, single-linked file — so
// every descriptor-side check passes on a file the caller never named. Windows
// is the live case: hardlinks_windows.go returns a real link count, so nothing
// else on that path fails closed either.
//
// Lstat-then-open is a weaker guarantee than O_NOFOLLOW, not an equal one: it
// can be raced between the two calls. It is nonetheless the guarantee this code
// had before the leaf was moved to a single open, and losing it silently on one
// platform is worse than holding it imperfectly there. An error is treated as a
// refusal: a leaf that cannot be proven non-symlink is not read.
func symlinkedLeafRefused(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return true
	}
	return info.Mode()&os.ModeSymlink != 0
}
