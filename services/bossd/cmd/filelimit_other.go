//go:build !unix

package main

// raiseFileLimit is a no-op on platforms without RLIMIT_NOFILE (e.g. Windows).
// The unix build carries the real implementation; this stub keeps main.go
// platform-agnostic so every cross-build compiles.
func raiseFileLimit() {}
