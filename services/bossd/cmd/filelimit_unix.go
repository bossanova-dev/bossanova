//go:build unix

package main

import (
	"syscall"

	"github.com/rs/zerolog/log"
)

const (
	// fileLimitWant is the RLIMIT_NOFILE soft+hard target. 65536 is well under
	// macOS kern.maxfilesperproc (default 122880) and high enough for FD-heavy
	// setup scripts (prisma codegen).
	fileLimitWant = 65536
	// fileLimitSoftFloor is the minimum achieved soft limit below which bossd
	// warns loudly at startup: 4096 is empirically too low for prisma codegen,
	// so anything under this floor is very likely to fail with EMFILE.
	fileLimitSoftFloor = 8192
	// maxFilesPerProcFallback caps fileLimitWant when kern.maxfilesperproc is
	// unreadable (non-Darwin unix, sandbox). Never raises the historical target.
	maxFilesPerProcFallback = 65536
)

// fileLimitOutcome is the result of a raise attempt: the achieved soft limit,
// whether the hard cap was lifted, and whether the achieved soft is below the
// safe floor (i.e. the caller should warn).
type fileLimitOutcome struct {
	achievedSoft uint64
	hardRaised   bool
	warn         bool
}

// raiseFileLimit bumps RLIMIT_NOFILE so setup scripts bossd spawns don't inherit
// a low soft limit (256 on macOS) and die with EMFILE during FD-heavy steps like
// prisma codegen. Best-effort: it also lifts the hard cap when possible, and
// warns loudly when the achieved soft limit stays below the safe floor (a stale
// terminal that sourced `ulimit -n <low>` leaves a hard cap a non-root process
// cannot raise). Returns the achieved soft limit (0 if unreadable) so startup can
// record it in daemon state. Never fails the daemon.
func raiseFileLimit() uint64 {
	out := raiseFileLimitWith(syscall.Getrlimit, syscall.Setrlimit, maxFilesPerProc)
	switch {
	case out.warn:
		log.Warn().
			Uint64("soft", out.achievedSoft).
			Int("floor", fileLimitSoftFloor).
			Msg("bossd: RLIMIT_NOFILE soft limit is low; FD-heavy setup scripts (e.g. prisma codegen) may fail with EMFILE. Restart bossd from a shell without a lowered `ulimit -n` hard cap (avoid `ulimit -n`; prefer `ulimit -Sn`).")
	case out.achievedSoft > 0:
		log.Debug().Uint64("soft", out.achievedSoft).Bool("hard_raised", out.hardRaised).Msg("bossd: raised RLIMIT_NOFILE")
	}
	return out.achievedSoft
}

// raiseFileLimitWith is the testable core: get/set mirror syscall.Getrlimit/
// Setrlimit and maxPerProc returns kern.maxfilesperproc (and whether it was
// read). Injecting all three lets table tests drive every branch deterministically
// without mutating the live process limit.
func raiseFileLimitWith(
	get func(int, *syscall.Rlimit) error,
	set func(int, *syscall.Rlimit) error,
	maxPerProc func() (uint64, bool),
) fileLimitOutcome {
	var lim syscall.Rlimit
	if err := get(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return fileLimitOutcome{}
	}

	maxProc := uint64(maxFilesPerProcFallback)
	if m, ok := maxPerProc(); ok && m > 0 {
		maxProc = m
	}
	target := min(uint64(fileLimitWant), maxProc)

	hardRaised := false
	// Best-effort hard raise: only meaningful when the current hard cap is a
	// finite value below target. On a high/unlimited cap there's nothing to
	// lift; unprivileged under a lowered cap this fails EPERM and we fall
	// through to the soft-only raise (exactly the old behaviour).
	if isFiniteHardCap(lim.Max) && lim.Max < target {
		try := lim
		try.Max = target
		try.Cur = target
		if err := set(syscall.RLIMIT_NOFILE, &try); err == nil {
			hardRaised = true
			lim = try
		}
	}

	// Soft raise, clamped to the (possibly newly raised) hard cap.
	softTarget := target
	if isFiniteHardCap(lim.Max) && softTarget > lim.Max {
		softTarget = lim.Max
	}
	if lim.Cur < softTarget {
		try := lim
		try.Cur = softTarget
		if err := set(syscall.RLIMIT_NOFILE, &try); err == nil {
			lim = try
		}
	}

	// Re-read so achievedSoft reflects what the kernel actually granted.
	final := lim
	var reread syscall.Rlimit
	if err := get(syscall.RLIMIT_NOFILE, &reread); err == nil {
		final = reread
	}
	return fileLimitOutcome{
		achievedSoft: final.Cur,
		hardRaised:   hardRaised,
		warn:         final.Cur < fileLimitSoftFloor,
	}
}

// isFiniteHardCap reports whether a RLIMIT_NOFILE hard value is a finite ceiling
// we should clamp to. Darwin reports an unlimited cap as 0 or a huge
// RLIM_INFINITY value; both are treated as "no finite cap" (a huge value never
// clamps the 65536 target anyway, so the simple non-zero test is correct).
func isFiniteHardCap(max uint64) bool {
	return max != 0
}
