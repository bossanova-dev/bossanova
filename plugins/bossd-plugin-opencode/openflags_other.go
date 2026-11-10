//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package main

// Lstat rejects static symlinks; os.Root confines concurrent replacements.
const questionHookOpenFlags = 0
