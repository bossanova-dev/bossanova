//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package credmaterialize

import "syscall"

// safeLeafOpenFlags are the extra open(2) flags that make a single open of an
// untrusted leaf safe to validate on the descriptor it returns:
//
//   - O_NOFOLLOW refuses a symlinked leaf outright, so the descriptor can only
//     ever be the entry that was named.
//   - O_NONBLOCK makes a writerless FIFO open return immediately instead of
//     blocking forever, so a diagnostic can never hang the caller.
const safeLeafOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// symlinkedLeafRefused is a no-op here and always reports false: O_NOFOLLOW
// above already refuses a symlinked leaf ATOMICALLY at open, which is strictly
// stronger than any check that could be written around it.
func symlinkedLeafRefused(string) bool { return false }
