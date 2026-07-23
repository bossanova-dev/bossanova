//go:build !unix

package main

// raiseFileLimit is a no-op on platforms without RLIMIT_NOFILE (e.g. Windows)
// and reports 0 (unknown) so the daemon records no FD-limit health signal.
func raiseFileLimit() uint64 { return 0 }
