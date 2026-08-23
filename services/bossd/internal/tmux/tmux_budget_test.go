package tmux

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
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

	tests := []struct {
		name string
		opts []Option
	}{
		{name: "zero client uses default readiness"},
		{name: "1s configured readiness", opts: []Option{WithSessionStartReadyDeadline(time.Second)}},
		{name: "4s configured readiness", opts: []Option{WithSessionStartReadyDeadline(4 * time.Second)}},
		{name: "30s configured readiness", opts: []Option{WithSessionStartReadyDeadline(30 * time.Second)}},
		{name: "45s configured readiness", opts: []Option{WithSessionStartReadyDeadline(45 * time.Second)}},
		{name: "120s configured readiness", opts: []Option{WithSessionStartReadyDeadline(120 * time.Second)}},
		{name: "300s configured readiness", opts: []Option{WithSessionStartReadyDeadline(300 * time.Second)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := NewClient(tt.opts...).startDeliveryOpts(sendPlanReadyMarker, true, nil)

			readiness := opts.deadline
			if opts.submitVerifyWait <= 0 {
				t.Fatalf("startDeliveryOpts submitVerifyWait = %v, want a positive submit-verifier budget; "+
					"this test reads it from the production builder so it cannot drift", opts.submitVerifyWait)
			}

			// 1. The hard invariant: the readiness wait plus the submit verifier that
			//    follows it both run inside one StartChatRun, so together they must fit
			//    under that RPC's ceiling.
			rpcCeiling := config.StartChatRunBudgetFor(readiness)
			needed := readiness + opts.submitVerifyWait
			if needed >= rpcCeiling {
				t.Fatalf("session-start readiness budget outgrew the host RPC ceiling for configured readiness %v: "+
					"readiness (%v) + submitVerifyWait (%v) = %v, which is not under config.StartChatRunBudgetFor(readiness) (%v).\n"+
					"Both run inside one StartChatRun on the repair-driven session-start path, so the RPC ceiling "+
					"fires first and the readiness deadline silently stops taking effect there. "+
					"Derive the StartChatRun ceiling from the configured readiness rather than pinning it to the default.",
					readiness, readiness, opts.submitVerifyWait, needed, rpcCeiling)
			}

			// 2. Headroom, as a deliberate policy margin: the ceiling must be at least
			//    twice the readiness deadline once the fixed in-RPC tail is not the larger term.
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
			//    That ordering is what preserves the CONFIGURED wait. The diagnostic
			//    itself survives either way on this multi-attempt path —
			//    waitForReadyMarkerWithAttempts clamps each attempt to the remaining
			//    context, so the failure still carries its pane capture — but an
			//    attempt trimmed by the RPC ceiling ran for less than the readiness
			//    deadline asked for, and nothing announces that except the clamp
			//    clause folded into the timeout message.
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
			if readiness >= config.StartChatRunInRPCTail && rpcCeiling-readiness < readiness {
				t.Fatalf("host RPC ceiling no longer holds the 2x policy margin over the configured readiness wait: "+
					"config.StartChatRunBudgetFor(%v) (%v) leaves %v after readiness, less than one further readiness deadline.\n"+
					"This keeps the daemon-side deadline comfortably first to fire under unmodelled session-start overhead, "+
					"so a slow start spends the readiness deadline it was configured with rather than a stub of it trimmed to fit the RPC ceiling.",
					readiness, rpcCeiling, rpcCeiling-readiness)
			}
		})
	}
}

func TestStartChatRunSubmitVerifierTailMatchesStartDeliveryOpts(t *testing.T) {
	t.Parallel()

	opts := (&Client{}).startDeliveryOpts(sendPlanReadyMarker, true, nil)
	if config.StartChatRunSubmitVerifierTail != opts.submitVerifyWait {
		t.Fatalf("config.StartChatRunSubmitVerifierTail = %v, but startDeliveryOpts submitVerifyWait = %v.\n"+
			"StartChatRunBudgetFor sizes the plugin→host RPC around this tail from bossalib, "+
			"which cannot import services/bossd/internal/tmux; update the shared term and this guard together.",
			config.StartChatRunSubmitVerifierTail, opts.submitVerifyWait)
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
