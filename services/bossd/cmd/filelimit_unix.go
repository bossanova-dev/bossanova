//go:build unix

package main

import (
	"syscall"

	"github.com/rs/zerolog/log"
)

// raiseFileLimit bumps RLIMIT_NOFILE so setup scripts bossd spawns don't inherit
// the low default soft limit (256 on macOS) and die with EMFILE during FD-heavy
// steps like prisma codegen. Best-effort: log and continue on failure.
func raiseFileLimit() {
	const want = 65536 // well under macOS kern.maxfilesperproc (122880)

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return
	}
	target := uint64(want)
	if lim.Max != 0 && target > lim.Max { // respect a finite hard cap; Darwin may report RLIM_INFINITY
		target = lim.Max
	}
	if lim.Cur >= target {
		return
	}
	prev := lim.Cur
	lim.Cur = target
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		log.Warn().Err(err).Uint64("want", target).Msg("bossd: could not raise RLIMIT_NOFILE")
		return
	}
	log.Debug().Uint64("from", prev).Uint64("to", target).Msg("bossd: raised RLIMIT_NOFILE")
}
