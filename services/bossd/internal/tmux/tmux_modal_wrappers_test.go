package tmux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// updateInterstitialPane is the shape codex draws when it boots onto its
// "Update available!" screen: row 1 carries codex's own composer glyph, so
// composerRowIndex resolves it as a live input row and the readiness gate is
// satisfied on the row-anchoring half alone.
//
// It is written out here rather than read from a fixture on purpose. This file
// proves an argv property — "given a modal verdict, nothing was typed" — and is
// deliberately incurious about which bytes produced the verdict. The real
// capture, and the grammar that calls it a modal, are proven where they belong:
// plugins/bossd-plugin-codex/testdata/panes/update_interstitial.txt with
// TestBootInterstitialRealPaneFixture, and the session-start path against a
// byte-identical copy of the same capture.
const updateInterstitialPane = "" +
	"  ✨ Update available! 0.147.0 -> 9.99.0\n" +
	"\n" +
	"› 1. Update now (runs `npm install -g @openai/codex`)\n" +
	"  2. Skip\n" +
	"  3. Skip until next version\n" +
	"\n" +
	"  Press enter to continue\n"

// modalWrapperDelivery names one of the four …WithModal wrappers and the
// …WithReadyMarker sibling it must be indistinguishable from when the detector
// is nil.
type modalWrapperDelivery struct {
	name       string
	withModal  func(c *Client, ctx context.Context, detector ModalDetector) error
	withMarker func(c *Client, ctx context.Context) error
}

func modalWrapperDeliveries() []modalWrapperDelivery {
	const sess = "boss-test-sess"
	const payload = "run the thing"
	const plan = "line one\nline two"
	return []modalWrapperDelivery{
		{
			name: "SendPlanWithModal",
			withModal: func(c *Client, ctx context.Context, d ModalDetector) error {
				return c.SendPlanWithModal(ctx, sess, plan, composerMarkerCodex, d)
			},
			withMarker: func(c *Client, ctx context.Context) error {
				return c.SendPlanWithReadyMarker(ctx, sess, plan, composerMarkerCodex)
			},
		},
		{
			name: "SendLineWithModal",
			withModal: func(c *Client, ctx context.Context, d ModalDetector) error {
				return c.SendLineWithModal(ctx, sess, payload, composerMarkerCodex, d)
			},
			withMarker: func(c *Client, ctx context.Context) error {
				return c.SendLineWithReadyMarker(ctx, sess, payload, composerMarkerCodex)
			},
		},
		{
			name: "PrefillPlanWithModal",
			withModal: func(c *Client, ctx context.Context, d ModalDetector) error {
				return c.PrefillPlanWithModal(ctx, sess, plan, composerMarkerCodex, d)
			},
			withMarker: func(c *Client, ctx context.Context) error {
				return c.PrefillPlanWithReadyMarker(ctx, sess, plan, composerMarkerCodex)
			},
		},
		{
			name: "PrefillLineWithModal",
			withModal: func(c *Client, ctx context.Context, d ModalDetector) error {
				return c.PrefillLineWithModal(ctx, sess, payload, composerMarkerCodex, d)
			},
			withMarker: func(c *Client, ctx context.Context) error {
				return c.PrefillLineWithReadyMarker(ctx, sess, payload, composerMarkerCodex)
			},
		},
	}
}

// TestModalWrappersRefuseBootInterstitial covers all four wrappers against the
// pane the session-start path meets: an agent that has drawn its update screen
// instead of a composer.
//
// The prefill arms matter as much as the submit ones. A prefill sends no Enter,
// which sounds harmless, but it still TYPES — and on this menu every keystroke
// is a selection shortcut, with "1" sitting on "Update now". Refusing only the
// submitting arms would leave half the session-start path delivering into it.
func TestModalWrappersRefuseBootInterstitial(t *testing.T) {
	t.Parallel()

	for _, delivery := range modalWrapperDeliveries() {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()

			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{updateInterstitialPane}}
			client := NewClient(WithCommandFactory(fake.factory))

			err := delivery.withModal(client, context.Background(), func(context.Context, []byte) (bool, error) {
				return true, nil
			})
			if err == nil {
				t.Fatal("delivered into a pane showing the update interstitial")
			}
			if !errors.Is(err, ErrBlockedByModal) {
				t.Fatalf("err = %v, want ErrBlockedByModal", err)
			}
			if got := OutcomeOf(err); got != OutcomeBlockedByModal {
				t.Fatalf("OutcomeOf = %v, want %v", got, OutcomeBlockedByModal)
			}
			// The refusal must name what it saw. An operator reading only
			// "blocked by modal" has to re-attach to a pane that has since moved
			// on; the embedded capture is what makes the verdict reviewable.
			if !strings.Contains(err.Error(), "Update available!") {
				t.Errorf("refusal does not name the modal it saw:\n%v", err)
			}
			assertNoDestructiveTmuxCalls(t, fake)
		})
	}
}

// TestModalWrappersNilDetectorMatchesReadyMarkerSibling pins the fallback the
// wiring depends on. Callers pass whatever the agent registry produced, and
// that is nil whenever the agent's plugin is not loaded — so a nil detector
// must leave the delivery byte-for-byte identical to the …WithReadyMarker
// spelling. An unloaded plugin has to degrade to the pre-BOS-600 behaviour,
// never to a failure or to a different delivery shape.
//
// Comparing the full recorded argv, rather than just the returned error, is
// what makes this a real equivalence: paste-vs-type routing, the Enter, and the
// verifier's re-reads all live in the argv and nowhere else.
func TestModalWrappersNilDetectorMatchesReadyMarkerSibling(t *testing.T) {
	t.Parallel()

	// Not ready on the first capture, ready on the second, so each run polls
	// exactly twice and then delivers. Ending the wait on pane content rather
	// than on the clock keeps the call sequence a deterministic property of the
	// code under test instead of a measurement of how many ticks fitted into a
	// wall-clock deadline on a loaded machine.
	panes := []string{
		"• Running the thing\n  no composer here\n",
		"• done\n\n› \n",
	}

	for _, delivery := range modalWrapperDeliveries() {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()

			run := func(viaModal bool) []sendPlanCall {
				fake := &sendPlanRecordingFactory{capturePaneOutputs: panes}
				client := NewClient(WithCommandFactory(fake.factory))
				var err error
				if viaModal {
					err = delivery.withModal(client, context.Background(), nil)
				} else {
					err = delivery.withMarker(client, context.Background())
				}
				if err != nil {
					t.Fatalf("viaModal=%v: %v", viaModal, err)
				}
				return fake.callsCopy()
			}

			modal, marker := run(true), run(false)
			if len(modal) != len(marker) {
				t.Fatalf("call counts differ: WithModal(nil)=%d, WithReadyMarker=%d\n%s\n%s",
					len(modal), len(marker), formatModalWrapperCalls(modal), formatModalWrapperCalls(marker))
			}
			for i := range modal {
				if modal[i].subcommand != marker[i].subcommand || !equalSlices(modal[i].args, marker[i].args) {
					t.Fatalf("call %d differs:\n WithModal(nil): %s %v\n WithReadyMarker: %s %v",
						i, modal[i].subcommand, modal[i].args, marker[i].subcommand, marker[i].args)
				}
			}
		})
	}
}

func formatModalWrapperCalls(calls []sendPlanCall) string {
	var b strings.Builder
	for i, call := range calls {
		fmt.Fprintf(&b, "  [%d] %s %v\n", i, call.subcommand, call.args)
	}
	return b.String()
}
