package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/statusdetect"
)

// The modal pane fixtures under testdata/panes are copies of canonical captures
// owned by the agent plugins, following the same "copy, don't depend across a
// module boundary" rule as the codex submit fixtures above:
//
//	plugins/bossd-plugin-codex/testdata/panes/question.txt              -> codex_approval_menu.txt
//	plugins/bossd-plugin-claude/testdata/panes/limit_decision_modal.txt -> claude_question_modal.txt
//
// PROVENANCE: both are REAL captures of real agent panes, not reconstructions.
// codex_approval_menu.txt is a codex-cli approval prompt ("Would you like to run
// the following command?"); claude_question_modal.txt is a Claude weekly-limit
// decision modal. Nothing in this file is a guessed fixture — see the PR body for
// the one modal (codex's update interstitial) that could NOT be captured and is
// therefore deliberately left uncovered rather than invented.
const (
	codexApprovalMenuFixture = "codex_approval_menu.txt"
	claudeQuestionFixture    = "claude_question_modal.txt"
)

// The SHA-256 of each copy, pinned so the copy cannot silently diverge from the
// plugin-owned original. Criterion 1 — a REAL agent modal is really refused — is
// only proven by composing two tests that live in different Go modules: the ones
// here prove "given a modal verdict, this pane is refused with no destructive
// argv", and the plugin-side ones prove "the real grammar calls this pane a
// modal". That composition is sound only while both sides read the same bytes,
// and without a pin an edit to either copy leaves both suites green while the
// branch quietly stops proving its headline claim.
//
// The module boundary forbids reading across it, so each side hashes its own
// file against its own copy of the literal below. Be precise about what that
// does and does not buy: nothing here compares the two FILES, or even the two
// literals, so a change that edits both copies and re-pins both literals stays
// green while the bytes silently diverge. What the pin guarantees is that
// divergence cannot happen QUIETLY — touching one copy reddens that module and
// names the other file in the failure, so the sync is a deliberate act rather
// than something a reader has to notice. Treat it as a tripwire, not a proof.
// The matching assertions are:
//
//	plugins/bossd-plugin-codex  TestQuestionPromptRealPaneFixture
//	plugins/bossd-plugin-claude TestHasQuestionPromptSplitsNotifyFromBlocking
const (
	codexApprovalMenuDigest = "82bc86a3bc9ff3425b94eee793731a34e70a4b5f8d5afc228ea7e7b5fe620c33"
	claudeQuestionDigest    = "121503714e4e93e248124b8542828eed52656f727af770592c445f8f34778d29"
)

// TestModalPaneFixturesMatchPluginOriginals guards the joint described above.
func TestModalPaneFixturesMatchPluginOriginals(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ fixture, digest, origin string }{
		{codexApprovalMenuFixture, codexApprovalMenuDigest, "plugins/bossd-plugin-codex/testdata/panes/question.txt"},
		{claudeQuestionFixture, claudeQuestionDigest, "plugins/bossd-plugin-claude/testdata/panes/limit_decision_modal.txt"},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			got := fmt.Sprintf("%x", sha256.Sum256([]byte(readPaneFixture(t, tt.fixture))))
			if got != tt.digest {
				t.Fatalf("%s digest = %s, want %s; this copy must stay byte-identical to %s, "+
					"which the plugin-side grammar test asserts against the same digest",
					tt.fixture, got, tt.digest, tt.origin)
			}
		})
	}
}

// The composer-ready markers the daemon polls for, one per agent. Both are
// chatReadyMarker() outputs — see services/bossd/internal/server/spawn_chat_tmux.go,
// where "codex" maps to "›" and every other agent to the default "❯".
const (
	composerMarkerCodex    = "›"
	composerMarkerClaude   = "❯"
	composerMarkerOpenCode = "┃"
)

// assertNoDestructiveTmuxCalls fails if the run emitted any tmux subcommand that
// would have typed into or submitted to the pane. Proving the Enter was *not*
// sent is the whole point of BOS-600, and the recorded argv is the only place
// that can be proven: a refusal that still fired send-keys would look identical
// from the returned error alone.
func assertNoDestructiveTmuxCalls(t *testing.T, fake *sendPlanRecordingFactory) {
	t.Helper()
	for _, call := range fake.callsCopy() {
		switch call.subcommand {
		case "send-keys", "load-buffer", "paste-buffer":
			t.Fatalf("pane was refused but tmux %s was still invoked with %v", call.subcommand, call.args)
		}
	}
}

// TestSendRefusesModalPane covers acceptance criteria 1, 2 and 5: a send into a
// pane showing an agent modal is refused with ErrBlockedByModal /
// OutcomeBlockedByModal, the error names the modal it saw, and no Enter reaches
// the pane.
//
// The two subtests differ in how the agent's grammar is supplied, because the
// two grammars live in different places:
//
//   - Claude's grammar is in lib/bossalib/statusdetect, which services/bossd
//     already depends on, so the Claude subtest wires the REAL predicate the
//     plugin puts on the wire as blocks_input — HasModalPrompt, the narrow modal
//     subset, not the wider notify predicate — and is end-to-end: real capture ->
//     real grammar -> refusal -> recorded argv.
//   - codex's grammar lives in the codex plugin, a separate Go module that this
//     package must not import. The codex subtest therefore injects the verdict
//     the plugin's HasQuestionPrompt RPC would return, and criterion 1 is proven
//     by composition with two tests that cover the other halves:
//     plugins/bossd-plugin-codex TestQuestionPromptRealPaneFixture proves the
//     real codex grammar returns true for this exact fixture, and
//     internal/server TestModalDetectorForRoutesToAgentPlugin proves the daemon
//     routes the pane to that agent's RPC. Stated in full in the PR body.
func TestSendRefusesModalPane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		readyMarker string
		detector    ModalDetector
		// wantErrContains is a line unique to the modal, asserted so the
		// refusal demonstrably names what it saw rather than just failing.
		wantErrContains string
	}{
		{
			name:        "codex approval menu",
			fixture:     codexApprovalMenuFixture,
			readyMarker: composerMarkerCodex,
			detector: func(context.Context, []byte) (bool, error) {
				return true, nil
			},
			wantErrContains: "1. Yes, proceed (y)",
		},
		{
			name:        "claude decision modal",
			fixture:     claudeQuestionFixture,
			readyMarker: composerMarkerClaude,
			detector: func(_ context.Context, pane []byte) (bool, error) {
				return statusdetect.HasModalPrompt(pane), nil
			},
			wantErrContains: "What do you want to do?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pane := readPaneFixture(t, tt.fixture)

			// The pane's marker leads a MENU row, so the readiness gate's
			// row anchoring alone still accepts it. Pinning that here keeps
			// the two halves honest: row anchoring (criterion 4) and the
			// modal check (this test) fix different holes, and neither
			// subsumes the other.
			if composerRowIndex(pane, tt.readyMarker) == -1 {
				t.Fatalf("fixture %s has no row starting with %q; it no longer exercises the modal check", tt.fixture, tt.readyMarker)
			}

			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
			c := NewClient(WithCommandFactory(fake.factory))

			err := c.sendLine(context.Background(), "boss-test-sess", "run the thing", sendPlanOpts{
				deadline:      200 * time.Millisecond,
				pollInterval:  2 * time.Millisecond,
				readyMarker:   tt.readyMarker,
				modalDetector: tt.detector,
			})
			if err == nil {
				t.Fatal("expected refusal, got nil error")
			}
			if !errors.Is(err, ErrBlockedByModal) {
				t.Fatalf("error is not ErrBlockedByModal: %v", err)
			}
			if got := OutcomeOf(err); got != OutcomeBlockedByModal {
				t.Fatalf("OutcomeOf = %v, want %v", got, OutcomeBlockedByModal)
			}
			if !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("error does not name the modal it saw (want a line containing %q): %v", tt.wantErrContains, err)
			}
			assertNoDestructiveTmuxCalls(t, fake)
		})
	}
}

// TestModalRefusalIsDistinctFromNotSubmitted covers acceptance criterion 5's
// second half. The distinction is load-bearing rather than cosmetic:
// OutcomeNotSubmitted means the payload IS in the composer and a bare Enter can
// finish it, while OutcomeBlockedByModal means nothing was typed at all and a
// retry would fire into the same menu. A caller that conflated them would
// re-send into a modal, which is the exact bug BOS-600 exists to prevent.
func TestModalRefusalIsDistinctFromNotSubmitted(t *testing.T) {
	t.Parallel()

	if OutcomeBlockedByModal == OutcomeNotSubmitted {
		t.Fatal("OutcomeBlockedByModal collides with OutcomeNotSubmitted")
	}
	if OutcomeBlockedByModal.String() == OutcomeNotSubmitted.String() {
		t.Fatalf("outcomes render identically as %q", OutcomeBlockedByModal)
	}

	modalErr := classifySubmit(OutcomeBlockedByModal, ErrBlockedByModal)
	if got := OutcomeOf(modalErr); got != OutcomeBlockedByModal {
		t.Fatalf("OutcomeOf(modal error) = %v, want %v", got, OutcomeBlockedByModal)
	}
	// A plain not-submitted error must not be mistaken for a modal refusal,
	// and errors.Is must not match across the two either.
	notSubmitted := classifySubmit(OutcomeNotSubmitted, errors.New("payload sat in the composer"))
	if got := OutcomeOf(notSubmitted); got != OutcomeNotSubmitted {
		t.Fatalf("OutcomeOf(not-submitted error) = %v, want %v", got, OutcomeNotSubmitted)
	}
	if errors.Is(notSubmitted, ErrBlockedByModal) {
		t.Fatal("a not-submitted error matches ErrBlockedByModal")
	}
}

// TestComposerRowIndexRequiresRowLeadingMarker covers acceptance criterion 4:
// readiness now means "a live input box is drawn", not "this glyph occurs
// somewhere in the pane". The mid-row cases are the regression guard — the
// previous strings.Contains gate accepted every one of them.
func TestComposerRowIndexRequiresRowLeadingMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pane    string
		marker  string
		wantRow int
	}{
		{
			name:    "marker only inside a menu row",
			pane:    "• Running the thing\n  1. Yes › proceed\n  2. No\n",
			marker:  composerMarkerCodex,
			wantRow: -1,
		},
		{
			name:    "marker only inside prose",
			pane:    "the agent printed › in its output\n",
			marker:  composerMarkerCodex,
			wantRow: -1,
		},
		{
			name:    "marker leads a row",
			pane:    "• done\n\n› \n",
			marker:  composerMarkerCodex,
			wantRow: 2,
		},
		{
			name:    "indented marker still leads its row",
			pane:    "• done\n  › \n",
			marker:  composerMarkerCodex,
			wantRow: 1,
		},
		{
			name:    "opencode rail is not stripped as a box border",
			pane:    "OpenCode ready\n┃ Type a message\n",
			marker:  composerMarkerOpenCode,
			wantRow: 1,
		},
		{
			name:    "bottom-most row wins",
			pane:    "› old echo\nsome output\n› \n",
			marker:  composerMarkerCodex,
			wantRow: 2,
		},
		{
			name:    "empty marker is never ready",
			pane:    "› \n",
			marker:  "",
			wantRow: -1,
		},
		// A composer drawn inside a box puts the marker after the border, so a
		// strict "row starts with the marker" rule would call a perfectly live
		// input box unready and time out EVERY send to that pane. Row-anchoring
		// is meant to reject rows that are not the composer, not rows whose
		// composer is framed.
		{
			name:    "marker leads the contents of a boxed composer",
			pane:    "• done\n╭────────╮\n│ ❯      │\n╰────────╯\n",
			marker:  composerMarkerClaude,
			wantRow: 2,
		},
		{
			name:    "box edges are not composer rows",
			pane:    "╭────────╮\n╰────────╯\n",
			marker:  composerMarkerClaude,
			wantRow: -1,
		},
		// The border tolerance is one rune deep on purpose: stripping greedily
		// would start eating content on rows that legitimately begin with it.
		{
			name:    "marker behind a doubled border is not a composer row",
			pane:    "││ ❯ \n",
			marker:  composerMarkerClaude,
			wantRow: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := composerRowIndex(tt.pane, tt.marker); got != tt.wantRow {
				t.Fatalf("composerRowIndex = %d, want %d", got, tt.wantRow)
			}
		})
	}
}

// TestSendNotReadyWhenMarkerOnlyMidRow is criterion 4 end-to-end: a pane whose
// only marker glyph sits inside a menu row never becomes ready, so the send
// times out rather than typing into a menu — with no detector installed at all,
// which is what makes this the readiness gate's own fix rather than the modal
// check's.
func TestSendNotReadyWhenMarkerOnlyMidRow(t *testing.T) {
	t.Parallel()

	pane := "• Running the thing\n  1. Yes › proceed (y)\n  2. No\n\n  Press enter to confirm or esc to cancel\n"
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendLine(context.Background(), "boss-test-sess", "run the thing", sendPlanOpts{
		deadline:     20 * time.Millisecond,
		pollInterval: 2 * time.Millisecond,
		readyMarker:  composerMarkerCodex,
	})
	if err == nil {
		t.Fatal("expected a not-ready error, got nil")
	}
	if !strings.Contains(err.Error(), "ready marker") {
		t.Fatalf("expected a ready-marker timeout, got: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestReadyMarkerWithNonModalDetector covers acceptance criterion 3 at the gate
// itself: with a detector installed, an ordinary composer pane is still ready.
// The two failure modes below are the ones that would silently break every
// delivery once the check ships, so both are pinned:
//
//   - a detector that says "not a modal" must not interfere; and
//   - a detector that FAILS must fail open, because an unloaded or crashed agent
//     plugin must never become a new reason chat delivery stops. This mirrors
//     the status poller's rule that a missing plugin can never lock a chat in
//     QUESTION (internal/status/tmux_poller.go).
func TestReadyMarkerWithNonModalDetector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		detector ModalDetector
	}{
		{
			name: "detector reports no modal",
			detector: func(context.Context, []byte) (bool, error) {
				return false, nil
			},
		},
		{
			name: "detector fails and the gate fails open",
			detector: func(context.Context, []byte) (bool, error) {
				return false, errors.New("plugin unreachable")
			},
		},
		{
			name: "detector reports a modal but errored anyway",
			detector: func(context.Context, []byte) (bool, error) {
				return true, errors.New("plugin unreachable")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"• done\n\n› \n"}}
			c := NewClient(WithCommandFactory(fake.factory))

			err := c.waitForReadyMarker(context.Background(), "boss-test-sess", sendPlanOpts{
				deadline:      50 * time.Millisecond,
				pollInterval:  2 * time.Millisecond,
				readyMarker:   composerMarkerCodex,
				modalDetector: tt.detector,
			})
			if err != nil {
				t.Fatalf("composer pane was not accepted as ready: %v", err)
			}
		})
	}
}

// TestModalProbeIsBounded pins the deadline the gate imposes on each detector
// call. The detector is an out-of-process plugin RPC that runs INSIDE the poll
// loop and ahead of the loop's own deadline test, so an unbounded probe against
// a wedged plugin would hold a send open long past opts.deadline — turning a
// safety check into a hang. Fail-open is only safe if it also fails fast.
//
// Asserting on the ctx the detector receives, rather than on elapsed wall time,
// keeps this deterministic: no sleeping, no flake.
func TestModalProbeIsBounded(t *testing.T) {
	t.Parallel()

	var gotDeadline bool
	var budget time.Duration
	detector := func(ctx context.Context, _ []byte) (bool, error) {
		dl, ok := ctx.Deadline()
		gotDeadline = ok
		if ok {
			budget = time.Until(dl)
		}
		return false, nil
	}

	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"• done\n\n› \n"}}
	c := NewClient(WithCommandFactory(fake.factory))

	// The caller's context carries NO deadline, which is the case that matters:
	// without modalProbeTimeout there would be nothing at all to stop the probe.
	if err := c.waitForReadyMarker(context.Background(), "boss-test-sess", sendPlanOpts{
		deadline:      50 * time.Millisecond,
		pollInterval:  2 * time.Millisecond,
		readyMarker:   composerMarkerCodex,
		modalDetector: detector,
	}); err != nil {
		t.Fatalf("composer pane was not accepted as ready: %v", err)
	}
	if !gotDeadline {
		t.Fatal("the detector ran with no deadline; a hung plugin would stall the send indefinitely")
	}
	if budget <= 0 || budget > modalProbeTimeout {
		t.Errorf("probe budget = %v, want (0, %v]", budget, modalProbeTimeout)
	}
}

// TestModalProbeTimeoutFailsOpen completes the pair above: a probe that blocks
// until its own deadline must not fail the send. The gate degrades to the
// pre-BOS-600 behaviour — deliver — rather than refusing on a plugin's silence,
// which is the same fail-open rule an outright detector error follows.
func TestModalProbeTimeoutFailsOpen(t *testing.T) {
	t.Parallel()

	detector := func(ctx context.Context, _ []byte) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}

	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{"• done\n\n› \n"}}
	c := NewClient(WithCommandFactory(fake.factory))

	// The caller's context expires well before modalProbeTimeout, so the blocked
	// probe unblocks via cancellation and the run stays fast; what is under test
	// is the verdict, which must be "not a modal" rather than a refusal.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := c.waitForReadyMarker(ctx, "boss-test-sess", sendPlanOpts{
		deadline:      time.Second,
		pollInterval:  2 * time.Millisecond,
		readyMarker:   composerMarkerCodex,
		modalDetector: detector,
	}); err != nil {
		t.Fatalf("a probe that never answered blocked delivery (outcome %v): %v", OutcomeOf(err), err)
	}
}

// TestModalProbeIsAtMostOncePerWait pins the cost of the gate. The detector is a
// gRPC round-trip into the agent's plugin, so a gate that asked per poll tick
// would make ~50 round-trips per send in the default 5s window — worst exactly
// on the pane that is already failing. waitForReadyMarker instead asks only
// about a capture that already carries a composer row (where the probe's answer
// ends the wait either way) plus at most one more on the deadline path, which
// bounds the whole wait at one probe without any memoizing machinery.
//
// The pane below never draws a composer row, so it is the worst case: ~30
// captures. Anything that probed per capture lands an order of magnitude above
// the expected count, so this is not timing-sensitive in the direction that
// matters.
func TestModalProbeIsAtMostOncePerWait(t *testing.T) {
	t.Parallel()

	const stuck = "Cloning into 'repo'...\nreceiving objects: 41%\n"
	const stuckLater = "Cloning into 'repo'...\nreceiving objects: 88%\n"

	for _, tt := range []struct {
		name     string
		captures []string
	}{
		// capturePaneOutputs repeats its last entry once exhausted, so one entry
		// means byte-identical captures for the whole run.
		{name: "static pane", captures: []string{stuck}},
		// A pane that keeps changing must not cost a probe per change either:
		// the loop's probe is gated on the composer row, not on the bytes.
		{name: "changing pane", captures: []string{stuck, stuckLater}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			calls := 0
			detector := func(context.Context, []byte) (bool, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				return false, nil
			}

			fake := &sendPlanRecordingFactory{capturePaneOutputs: tt.captures}
			err := NewClient(WithCommandFactory(fake.factory)).
				waitForReadyMarker(context.Background(), "boss-test-sess", sendPlanOpts{
					deadline:      60 * time.Millisecond,
					pollInterval:  2 * time.Millisecond,
					readyMarker:   composerMarkerCodex,
					modalDetector: detector,
				})
			if err == nil {
				t.Fatal("a pane with no composer row was accepted as ready")
			}
			if OutcomeOf(err) == OutcomeBlockedByModal {
				t.Fatalf("detector answered false but the pane was refused as a modal: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != 1 {
				t.Errorf("detector called %d times across ~30 captures, want 1", calls)
			}
		})
	}
}

// TestMarkerlessModalIsNamedNotTimedOut covers the half of the gate the
// composer-row ordering would otherwise lose. Refusing only panes that draw a
// marker-led row is what makes the probe at-most-once, but a modal that draws NO
// composer-shaped row at all would then reach the deadline and be reported as
// "the agent never got ready" — sending the operator to look for a slow start
// that is not happening. One probe on the deadline path buys the diagnosis back.
func TestMarkerlessModalIsNamedNotTimedOut(t *testing.T) {
	t.Parallel()

	const interstitial = "  A new version is available.\n  [ Update now ]   [ Later ]\n"

	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{interstitial}}
	err := NewClient(WithCommandFactory(fake.factory)).
		waitForReadyMarker(context.Background(), "boss-test-sess", sendPlanOpts{
			deadline:     60 * time.Millisecond,
			pollInterval: 2 * time.Millisecond,
			readyMarker:  composerMarkerCodex,
			modalDetector: func(context.Context, []byte) (bool, error) {
				return true, nil
			},
		})
	if err == nil {
		t.Fatal("a pane showing a modal was accepted as ready")
	}
	if !errors.Is(err, ErrBlockedByModal) {
		t.Fatalf("markerless modal reported as a plain timeout, not a refusal: %v", err)
	}
	if got := OutcomeOf(err); got != OutcomeBlockedByModal {
		t.Fatalf("OutcomeOf = %v, want %v", got, OutcomeBlockedByModal)
	}
	assertNoDestructiveTmuxCalls(t, fake)
}

// TestSendMessageWithModalNilDetectorMatchesSendMessage pins the fallback the
// wiring depends on: callers pass whatever modalDetectorFor returns, and that is
// nil whenever the agent's plugin is not loaded. A nil detector must leave the
// delivery path byte-for-byte identical to plain SendMessage, so an unloaded
// plugin degrades to the pre-BOS-600 behaviour rather than to a failure.
func TestSendMessageWithModalNilDetectorMatchesSendMessage(t *testing.T) {
	t.Parallel()

	// The pane is not ready on the first capture and ready on the second, so
	// both spellings poll exactly twice and then deliver. That matters for more
	// than speed: an earlier version of this test polled a pane that NEVER
	// becomes ready and compared the two call counts, which made the assertion a
	// measurement of how many 100ms ticks each run fitted into a 5s wall-clock
	// deadline (it failed on 48 vs 49 under a loaded sandbox). Ending the wait on
	// pane content instead of on the clock makes the whole call sequence — poll,
	// poll, then the delivery commands — a deterministic property of the code
	// under test, and covers the success path rather than only the give-up path.
	panes := []string{
		"• Running the thing\n  no composer here\n",
		"• done\n\n› \n",
	}

	run := func(withModal bool) []sendPlanCall {
		t.Helper()
		fake := &sendPlanRecordingFactory{capturePaneOutputs: panes}
		c := NewClient(WithCommandFactory(fake.factory))
		var err error
		if withModal {
			err = c.SendMessageWithModal(context.Background(), "boss-test-sess", "hi", true, composerMarkerCodex, nil)
		} else {
			err = c.SendMessage(context.Background(), "boss-test-sess", "hi", true, composerMarkerCodex)
		}
		if err != nil {
			t.Fatalf("withModal=%t: unexpected error: %v", withModal, err)
		}
		return fake.callsCopy()
	}

	withCalls, plainCalls := run(true), run(false)

	// Check each run against a spelled-out sequence rather than only against the
	// other run: two runs of the same regressed code agree with each other
	// perfectly. This is the pre-BOS-600 delivery shape — poll until the
	// composer row appears, type, press Enter, read back to verify the submit —
	// and a nil detector must not add, drop, or reorder any of it.
	capture := sendPlanCall{subcommand: "capture-pane", args: []string{"-p", "-S", "-1000", "-t", "boss-test-sess"}}
	want := []sendPlanCall{
		capture,
		capture,
		{subcommand: "send-keys", args: []string{"-t", "boss-test-sess", "-l", "--", "hi"}},
		{subcommand: "send-keys", args: []string{"-t", "boss-test-sess", "Enter"}},
		capture,
	}
	for _, tc := range []struct {
		name  string
		calls []sendPlanCall
	}{{"with modal", withCalls}, {"plain", plainCalls}} {
		if len(tc.calls) != len(want) {
			t.Fatalf("%s: issued %d tmux call(s), want %d:\n got:  %v\n want: %v", tc.name, len(tc.calls), len(want), tc.calls, want)
		}
		for i := range want {
			if tc.calls[i].subcommand != want[i].subcommand || !equalSlices(tc.calls[i].args, want[i].args) {
				t.Fatalf("%s: tmux call %d = %v, want %v", tc.name, i, tc.calls[i], want[i])
			}
		}
	}
}

// TestSendMessageWithModalBindsDetectorPerCall pins the reason
// SendMessageWithModal exists: a daemon holds ONE long-lived Client shared by
// every chat, but the modal grammar varies per agent. Binding through the client
// would leak one chat's grammar into the next chat's send.
func TestSendMessageWithModalBindsDetectorPerCall(t *testing.T) {
	t.Parallel()

	pane := readPaneFixture(t, claudeQuestionFixture)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{pane}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.SendMessageWithModal(context.Background(), "boss-test-sess", "hi", true, composerMarkerClaude,
		func(context.Context, []byte) (bool, error) { return true, nil })
	if !errors.Is(err, ErrBlockedByModal) {
		t.Fatalf("per-call detector was not consulted: %v", err)
	}
	assertNoDestructiveTmuxCalls(t, fake)

	// The shared client must be unchanged, asserted behaviourally rather than by
	// reading a field: the very next send on the SAME client, given no detector,
	// must run the pre-BOS-600 path and reach the composer. A grammar that had
	// leaked onto the client would refuse this identical pane a second time.
	if err := c.SendMessageWithModal(context.Background(), "boss-test-sess", "hi", false, composerMarkerClaude, nil); err != nil {
		t.Fatalf("a detector-free send was refused: %v", err)
	}
}
