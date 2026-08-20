package tmux

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/plugin/hostclient"
)

// TestSessionStartReadinessFitsStartChatRunBudget is a relational guard, not a
// value test. It exists because the session-start readiness wait does not run
// at the top of the stack on the repair-driven path: it runs *inside* a
// plugin→host unary RPC (StartChatRun), which carries its own ceiling in
// lib/bossalib/plugin/hostclient/hostclient.go. If the readiness deadline ever
// outgrows that ceiling, the RPC is cut off first and the readiness budget
// silently stops taking effect on that path — the bug BOS-896 fixed, which
// nothing else in the tree would notice.
//
// The relationship cannot be expressed as a compile-time derivation. The
// dependency edge runs one way (services/bossd requires lib/bossalib via a
// local replace, with no edge back), so hostclient cannot see this package's
// readiness default. This test is the enforcement instead.
//
// It lives in package tmux because submitVerifyWait is unexported, and it reads
// that value from the production delivery-options builder rather than restating
// 2s, so the assertion tracks the real production value if it moves.
func TestSessionStartReadinessFitsStartChatRunBudget(t *testing.T) {
	t.Parallel()

	const (
		readinessConst = "tmux.DefaultSessionStartReadyDeadline (services/bossd/internal/tmux/tmux.go)"
		ceilingConst   = "hostclient.StartChatRunRPCTimeout (lib/bossalib/plugin/hostclient/hostclient.go)"
	)

	// Build the real session-start delivery options. A zero Client has no
	// operator-configured deadline, so this exercises the default fallback —
	// the same path production takes on a host that configured nothing. submit
	// is true because the session-start delivery does press Enter, which is
	// what arms the submit verifier. The detector is nil, as the plain
	// …WithReadyMarker wrappers pass (BOS-894): interstitial detection short-
	// circuits the wait early and so can only make the real elapsed time
	// shorter than the budget asserted here, never longer.
	opts := (&Client{}).startDeliveryOpts(sendPlanReadyMarker, true, nil)

	readiness := opts.deadline
	if readiness != DefaultSessionStartReadyDeadline {
		t.Fatalf("startDeliveryOpts deadline = %v, want the %s default of %v — "+
			"this test asserts against the production builder, so a mismatch means the builder changed",
			readiness, readinessConst, DefaultSessionStartReadyDeadline)
	}
	if opts.submitVerifyWait <= 0 {
		t.Fatalf("startDeliveryOpts submitVerifyWait = %v, want a positive submit-verifier budget; "+
			"this test reads it from the production builder so it cannot drift", opts.submitVerifyWait)
	}

	// 1. The hard invariant: the readiness wait plus the submit verifier that
	//    follows it both run inside one StartChatRun, so together they must fit
	//    under that RPC's ceiling.
	budget := readiness + opts.submitVerifyWait
	if budget >= hostclient.StartChatRunRPCTimeout {
		t.Fatalf("session-start readiness budget outgrew the host RPC ceiling: "+
			"%s (%v) + submitVerifyWait (%v) = %v, which is not under %s (%v).\n"+
			"Both run inside one StartChatRun on the repair-driven session-start path, so the RPC ceiling "+
			"fires first and the readiness deadline silently stops taking effect there. "+
			"Raise %s to match, or lower %s.",
			readinessConst, readiness, opts.submitVerifyWait, budget,
			ceilingConst, hostclient.StartChatRunRPCTimeout,
			ceilingConst, readinessConst)
	}

	// 2. Headroom, as a deliberate policy margin: the ceiling must be at least
	//    twice the readiness deadline.
	//
	//    Be honest about what this is. The tails that actually run inside the
	//    same RPC after the readiness wait are small — the submit verifier
	//    asserted above, plus freshProviderSessionIDResolveDeadline, which
	//    lives in package session and is invisible from here (this package must
	//    not reach across for it or clone its literal). A margin sized only to
	//    cover them would be a few seconds, not a further 45. The 2x rule is
	//    chosen instead so the daemon-side deadline stays *comfortably* the
	//    first of the two to fire under unmodelled overhead — process spawn, DB
	//    write, hook wiring, and on the replace branch a whole first
	//    StartTmuxChat and cleanup before the launch that this deadline times.
	//    That ordering is what preserves the diagnostic: waitForReadyMarker
	//    honours ctx.Done() and returns a bare context error instead of its
	//    pane capture whenever the RPC ceiling wins the race.
	//
	//    So a failure here is not proof of breakage — invariant 1 above is the
	//    hard one. It means the pair has drifted out of the ratio this ticket
	//    chose, and the two constants need re-deciding together rather than one
	//    being nudged.
	//
	//    The margin is measured from the readiness deadline rather than from
	//    the full budget above deliberately: at the shipped values (45s
	//    readiness, 90s ceiling) the from-budget form would demand 92s and
	//    contradict the pinned 90s constant. From-readiness is the form the
	//    shipped pair actually satisfies. It does so with exactly zero slack,
	//    which is intended — it means any upward move of readiness lands here
	//    first, before invariant 1 could ever fire.
	if hostclient.StartChatRunRPCTimeout-readiness < readiness {
		t.Fatalf("host RPC ceiling no longer holds the 2x policy margin over the readiness wait: "+
			"%s (%v) minus %s (%v) = %v, which is less than one further readiness deadline (%v).\n"+
			"This is a deliberate ratio, not a derivation from the in-RPC tails (those are only a few "+
			"seconds). It keeps the daemon-side deadline comfortably first to fire under unmodelled "+
			"session-start overhead, so a slow start surfaces as the readiness diagnostic with its pane "+
			"capture rather than a bare context error.\n"+
			"Re-decide the pair together: raise %s to at least %v — which also requires updating the "+
			"equality pin in TestStartChatRunRPCTimeoutDefault (lib/bossalib/plugin/hostclient/hostclient_test.go) "+
			"and the value quoted in the tmux.go doc comment — or lower %s.",
			ceilingConst, hostclient.StartChatRunRPCTimeout, readinessConst, readiness,
			hostclient.StartChatRunRPCTimeout-readiness, readiness,
			ceilingConst, 2*readiness, readinessConst)
	}
}

// TestStartChatRunCeilingExceedsSharedClamp is the cheap companion assertion:
// the exemption only means anything if it is looser than the shared unary
// clamp it exempts StartChatRun from.
func TestStartChatRunCeilingExceedsSharedClamp(t *testing.T) {
	t.Parallel()
	if hostclient.StartChatRunRPCTimeout <= hostclient.DefaultRPCTimeout {
		t.Fatalf("hostclient.StartChatRunRPCTimeout (%v) must exceed hostclient.DefaultRPCTimeout (%v)",
			hostclient.StartChatRunRPCTimeout, hostclient.DefaultRPCTimeout)
	}
	// Sanity floor: a shared clamp that already covered the readiness wait
	// would mean this whole guard is unnecessary. It does not.
	if hostclient.DefaultRPCTimeout >= DefaultSessionStartReadyDeadline+2*time.Second {
		t.Log("note: hostclient.DefaultRPCTimeout now covers the readiness budget on its own; " +
			"the StartChatRun exemption may no longer be load-bearing")
	}
}
