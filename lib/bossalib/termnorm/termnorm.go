// Package termnorm decides the effective TERM for a tmux launch/attach so a
// host-specific terminal (e.g. xterm-ghostty) whose terminfo entry is missing
// on the target box falls back to the broadly available xterm-256color instead
// of making tmux exit with "missing or unsuitable terminal".
package termnorm

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// FallbackTERM is the broadly-available terminal type used whenever the
// incoming TERM has no terminfo entry on this host. Exported so the preflight
// check, the repair-doctor terminal check, and the attach error diagnostic all
// name the same fallback rather than repeating the literal.
const FallbackTERM = "xterm-256color"

// InstallHint is the canonical one-line remediation for a missing terminfo
// entry: copy the local terminfo entry to a remote host. Shared by the same
// three call sites so the advice has a single source of truth.
const InstallHint = "infocmp -x $TERM | ssh <host> tic -x -"

// probeTimeout bounds the infocmp subprocess so a wedged probe can never hang a
// caller (the attach path runs it inside the Bubble Tea Update loop).
const probeTimeout = 2 * time.Second

// probe reports whether a terminfo entry for term exists on this host. It is a
// package var so tests can substitute a deterministic implementation.
var probe = terminfoPresent

// terminfoPresent shells out to `infocmp <term>`. If infocmp is not installed
// we fail open (return true) — refusing to launch because the probe tool is
// missing would be worse than attempting the user's real TERM. The exec is
// bounded by probeTimeout so a hung infocmp cannot stall the caller.
func terminfoPresent(term string) bool {
	if term == "" {
		return false
	}
	if _, err := exec.LookPath("infocmp"); err != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "infocmp", term).Run() == nil
}

// Prober reports whether a terminfo entry for term exists on the host that
// matters for the caller.
type Prober func(term string) bool

// Resolvable reports whether term's terminfo entry is present on this host.
func Resolvable(term string) bool { return probe(term) }

// Effective returns term when its terminfo entry resolves, else FallbackTERM.
func Effective(term string) string {
	return EffectiveWith(term, probe)
}

// EffectiveWith returns term when p resolves it, else FallbackTERM.
func EffectiveWith(term string, p Prober) string {
	if p(term) {
		return term
	}
	return FallbackTERM
}

// Normalize returns a copy of env with TERM set to Effective(currentTERM). When
// env has no TERM it appends TERM=FallbackTERM. The caller's slice is never
// mutated.
func Normalize(env []string) []string {
	return NormalizeWith(env, probe)
}

// NormalizeWith returns a copy of env with TERM set to EffectiveWith(currentTERM, p).
// When env has no TERM it appends TERM=FallbackTERM. The caller's slice is never
// mutated.
func NormalizeWith(env []string, p Prober) []string {
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	for i, e := range out {
		if !strings.HasPrefix(e, "TERM=") {
			continue
		}
		out[i] = "TERM=" + EffectiveWith(strings.TrimPrefix(e, "TERM="), p)
		return out
	}
	return append(out, "TERM="+FallbackTERM)
}
