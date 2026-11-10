//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import "syscall"

// Refuse leaf symlinks atomically when opening the hook for writing.
const questionHookOpenFlags = syscall.O_NOFOLLOW
