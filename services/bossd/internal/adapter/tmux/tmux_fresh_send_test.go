package tmux

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestFreshPaneReadinessOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		pane string
		want SubmitOutcome
	}{
		{name: "modal", pane: sendPlanReadyMarker + " 1. Approve", want: OutcomeBlockedByModal},
		{name: "timeout", want: OutcomeNotSubmitted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &sendPlanRecordingFactory{}
				c := NewClient(WithCommandFactory(fake.factory))
				opts := c.freshPaneDeliveryOpts(sendPlanReadyMarker, true, func(context.Context, []byte) (bool, error) {
					return true, nil
				})
				opts.readinessCapture = func(context.Context, string) (string, error) { return tc.pane, nil }
				err := c.sendPlan(context.Background(), "boss-test-fresh", typedSlashPayload, opts)
				if got := OutcomeOf(err); got != tc.want {
					t.Fatalf("outcome %v (%v), want %v", got, err, tc.want)
				}
				if tc.want == OutcomeBlockedByModal && !errors.Is(err, ErrBlockedByModal) {
					t.Fatalf("modal refusal lost its sentinel: %v", err)
				}
				assertNoDestructiveTmuxCalls(t, fake)
			})
		})
	}
}

func TestFreshPaneSendSubmitsOnASlowStartingComposer_ChatPicker_AC261701(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux: run make -C services/bossd test-all GO_TEST_PACKAGES=./internal/adapter/tmux")
	}
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "established", true: "fresh"}[fresh], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 5*time.Second)
			err := c.SendMessageWithHooks(ctx, pane, typedSlashPayload, true, sendPlanReadyMarker, nil, SendHooks{FreshPane: fresh})
			if fresh {
				if err != nil {
					t.Fatalf("fresh send: %v", err)
				}
				if ran := ranLines(ctx, t, c, pane); !slices.Equal(ran, []string{typedSlashPayload}) {
					t.Fatalf("ran %q, want payload exactly once", ran)
				}
			} else if OutcomeOf(err) != OutcomeNotSubmitted {
				t.Fatalf("established send = %v, want not submitted", err)
			}
		})
	}
}

func TestFreshPaneSendThatNeverDrawsIsNotSubmittedWithinBudget_ChatPicker_AC261702(t *testing.T) {
	if testing.Short() {
		t.Skip("real tmux: run make -C services/bossd test-all GO_TEST_PACKAGES=./internal/adapter/tmux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposer(ctx, t, integrationTestTimeout)
	enters := 0
	onTmux(c, func(args []string, _ CommandFactory) {
		if slices.Contains(args, "send-keys") && slices.Contains(args, "Enter") {
			enters++
		}
	})
	started := time.Now()
	err := c.SendMessageWithHooks(ctx, pane, typedSlashPayload, true, sendPlanReadyMarker, nil, SendHooks{FreshPane: true})
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome %v (%v), want not submitted", OutcomeOf(err), err)
	}
	if elapsed := time.Since(started); elapsed > freshPaneSendBudget {
		t.Fatalf("send took %v, budget %v", elapsed, freshPaneSendBudget)
	}
	if enters != 0 {
		t.Fatalf("sent %d Enters to an undrawn composer", enters)
	}
	if ran := ranLines(ctx, t, c, pane); len(ran) != 0 {
		t.Fatalf("agent ran %q, want nothing", ran)
	}
}

func TestFreshPaneSendBudgetFitsRelayCeiling(t *testing.T) {
	const relayDeadline = 30 * time.Second
	if freshPaneSendBudget >= relayDeadline {
		t.Fatal("fresh send must leave room in relay deadline")
	}
	c := NewClient()
	opts := c.freshPaneDeliveryOpts(sendPlanReadyMarker, true, nil)
	if len(opts.typedDrawWaits) < 2 {
		t.Fatal("fresh send needs escalating draw windows")
	}
	codex := c.freshPaneDeliveryOpts(codexReadyMarker, true, nil)
	if len(codex.typedDrawWaits) != 0 || !codex.freshPaneDeadline.IsZero() {
		t.Fatal("Codex send timing must stay established")
	}
}

// A stuck capture may consume the send deadline before the safe no-Enter
// withdrawal runs. That cleanup must not create a new budget after it.
func TestFreshPaneCleanupSharesSendBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("bounded process integration: run make -C services/bossd test-all GO_TEST_PACKAGES=./internal/adapter/tmux")
	}
	undrawn, _, _ := typedSlashPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn}}
	typed := false
	c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if typed && (slices.Contains(args, "capture-pane") || slices.Contains(args, "C-u")) {
			return exec.CommandContext(ctx, "sleep", "60")
		}
		if slices.Contains(args, typedSlashPayload) {
			typed = true
		}
		return fake.factory(ctx, name, args...)
	}))
	started := time.Now()
	err := c.SendMessageWithHooks(context.Background(), "boss-test-fresh", typedSlashPayload, true, sendPlanReadyMarker, nil, SendHooks{FreshPane: true})
	const processExitSlack = time.Second
	if elapsed := time.Since(started); elapsed > freshPaneSendBudget+processExitSlack {
		t.Fatalf("send and cleanup took %v, beyond shared %v budget", elapsed, freshPaneSendBudget)
	}
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome %v (%v), want not submitted", OutcomeOf(err), err)
	}
}
