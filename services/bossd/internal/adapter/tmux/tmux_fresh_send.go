package tmux

import (
	"context"
	"time"
)

// Below the 30 s relay ceiling and FRESH_CHAT_SEND_TIMEOUT_MS in
// skills-toolbox/verify/verify-route.mjs. All fresh-send waits share this budget.
const freshPaneSendBudget = 20 * time.Second

// Three verifier windows (initial read, clear, final read), paste settle and
// withdrawal must still fit after the readiness and typed-draw waits.
const freshPaneSubmitReserve = 8 * time.Second

func (c *Client) freshPaneDeliveryOpts(marker string, submit bool, detector ModalDetector) sendPlanOpts {
	opts := c.sendDeliveryOpts(marker, submit, detector)
	if !submit || len(opts.typedDrawWaits) == 0 {
		return opts
	}
	opts.freshPaneDeadline = time.Now().Add(freshPaneSendBudget)
	opts.deadline = freshPaneSendBudget - freshPaneSubmitReserve
	opts.typedDrawWaits = startTypedDrawWaits()
	return opts
}

func (c *Client) sendMessageWithDeliveryHooks(ctx context.Context, name, text string, submit bool, marker string, detector ModalDetector, hooks SendHooks) error {
	opts := c.sendDeliveryOpts(marker, WillSubmit(submit, text), detector)
	if hooks.FreshPane {
		opts = c.freshPaneDeliveryOpts(marker, WillSubmit(submit, text), detector)
	}
	if !opts.freshPaneDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx = context.WithValue(ctx, freshPaneDeadlineKey{}, opts.freshPaneDeadline)
		ctx, cancel = context.WithDeadline(ctx, opts.freshPaneDeadline)
		defer cancel()
	}
	opts.beforeSubmit = hooks.BeforeSubmit
	opts.submitted = hooks.Submitted
	opts.receiptRequired = hooks.ReceiptRequired && hooks.Submitted != nil
	opts.prose = hooks.Prose
	return c.sendPlan(ctx, name, text, opts)
}

func (opts sendPlanOpts) freshDrawWait(wait time.Duration) time.Duration {
	if opts.freshPaneDeadline.IsZero() {
		return wait
	}
	return max(0, min(wait, time.Until(opts.freshPaneDeadline)-freshPaneSubmitReserve))
}

func (opts sendPlanOpts) freshDrawExpired() bool {
	return !opts.freshPaneDeadline.IsZero() && time.Until(opts.freshPaneDeadline) <= freshPaneSubmitReserve
}

// WithoutCancel permits safe withdrawal after a caller gives up, but a fresh
// send still owns one deadline shared by delivery and that withdrawal.
type freshPaneDeadlineKey struct{}

func deliveryCleanupContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Value(freshPaneDeadlineKey{}).(time.Time); ok {
		budget = min(budget, max(0, time.Until(deadline)))
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

func (opts sendPlanOpts) classifyReadyFailure(err error) error {
	if !opts.freshPaneDeadline.IsZero() && OutcomeOf(err) == OutcomeUnclassified {
		return classifySubmit(OutcomeNotSubmitted, err)
	}
	return err
}
