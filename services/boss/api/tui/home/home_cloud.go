// Home's Bossanova Cloud access, checkout, and gate-line rendering. Split out
// of home.go (BOS-526); the declarations are unchanged.

package home

import (
	"context"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type CloudAccessClient interface {
	GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error)
	CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error)
	CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error)
	RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error)
}

func (h *Model) SetCloudAccessClient(c CloudAccessClient) {
	h.SetCloudSubscription(c, h.checkoutReturnURL, h.checkoutCancelURL)
}

func (h *Model) SetCloudSubscription(c CloudAccessClient, returnURL, cancelURL string) {
	h.CloudAccess = c
	h.checkoutReturnURL = returnURL
	h.checkoutCancelURL = cancelURL
}

func (h Model) handleCloudAccess(msg cloudAccessMsg) (tea.Model, tea.Cmd) {
	h.cloudChecking = false
	h.CloudStatus = msg.Status
	h.cloudErr = msg.Err
	if SubscriptionIsActive(msg.Status) {
		h.cloudCheckoutPolling = false
		h.cloudCheckoutStatus = ""
	}
	if msg.Status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		h.cloudCheckoutPolling = false
	}
	return h, nil
}

func (h Model) handleCloudCheckout(msg homeCloudCheckoutMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		if CheckoutRefusedByAccessState(msg.Err) {
			if CheckoutAlreadyActive(msg.Err) {
				// The server consulted Stripe, found a live subscription and
				// settled the local account before refusing, so this caller is
				// a paying customer. Home renders none of CloudAccessStatus's
				// Message field, so standing in a PENDING_ENTITLEMENT_REFRESH
				// here would put "setup has not completed yet" in front of them
				// on the one line Home does render. Drop to the checking state
				// instead — the refresh below is what establishes the truth —
				// and say the reassuring thing through cloudCheckoutStatus,
				// which handleCloudAccess clears the moment access reads active.
				h.CloudStatus = nil
				h.cloudCheckoutStatus = statusCloudAlreadyActive
			} else {
				h.CloudStatus = &pb.CloudAccessStatus{
					State:   pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
					Message: "Your subscription is being activated.",
				}
				h.cloudCheckoutStatus = ""
			}
			h.cloudErr = nil
			h.cloudCheckoutPolling = true
			h.cloudChecking = true
			return h, refreshCloudAccessStatus(h.CloudAccess, h.Ctx)
		}
		if msg.URL != "" {
			h.cloudCheckoutStatus = "Open this billing URL: " + msg.URL
			h.cloudCheckoutPolling = true
			h.cloudChecking = true
			return h, refreshCloudAccessStatus(h.CloudAccess, h.Ctx)
		} else {
			h.cloudCheckoutStatus = "Cloud checkout unavailable. Local sessions are still available."
			h.cloudCheckoutPolling = false
		}
		return h, nil
	}
	h.cloudCheckoutStatus = "Opened Bossanova Cloud subscription checkout."
	h.cloudCheckoutPolling = true
	h.cloudChecking = true
	return h, refreshCloudAccessStatus(h.CloudAccess, h.Ctx)
}

// cloudAction intentionally returns no action-bar entry. When the Cloud
// subscription is inactive, Home stays quiet: it shows a one-line reminder
// (see cloudGateLine) instead of a permanent [b]illing menu item. Re-entry to
// billing stays in the login/subscription flow.
func (h Model) cloudAction() string {
	return ""
}

func (h Model) canOpenCloudSubscription() bool {
	return h.LoggedIn && h.CloudAccess != nil && (SubscriptionNeedsCheckout(h.CloudStatus) || cloudAccessPending(h.CloudStatus))
}

// Home's fixed status copy. These are named rather than inlined at their one
// call site each so TestHomeFixedStatusCopyStaysReadableAtFloor measures the
// strings the screen actually renders: the wrap floor is narrower than most of
// this copy (see minStatusWrapWidth), and the guard that bounds the resulting
// height only works if it cannot drift from the source. Edit the copy here and
// the guard follows; the longest of these, statusCloudNeedsSubscriptionUpgrade
// at 112 columns, is what sets that bound.
const (
	statusCloudChecking                 = "Checking Bossanova Cloud access…"
	statusCloudNeedsSubscriptionUpgrade = "Bossanova Cloud requires a subscription. Local sessions are still available. Upgrade boss first, then subscribe."
	statusCloudNeedsSubscription        = "Bossanova Cloud requires a subscription. Local sessions are still available. Type u to s[u]bscribe."
	statusCloudResumeCheckoutUpgrade    = "Checkout was not completed. Local sessions are still available. Upgrade boss first, then resume it."
	statusCloudResumeCheckout           = "Checkout was not completed. Local sessions are still available. Type u to res[u]me it."
	statusCloudAlreadyActive            = "Your subscription is already active."
	statusCloudPendingUpgrade           = "Bossanova Cloud setup has not completed yet. Upgrade boss first, then subscribe."
	statusCloudPending                  = "Bossanova Cloud setup has not completed yet. Type u to s[u]bscribe."
	statusUpgrading                     = "Upgrading…"
	statusRestartingDaemon              = "Restarting daemon…"
	statusUpgradeRestartPrompt          = "Upgrade installed. Quit boss after restart to use the new binary. [r]estart [esc] later"
	statusUpgradeDone                   = "Upgrade installed. Quit boss (q) and re-launch to use the new binary."
	statusUpgradeAvailableFormat        = "Upgrade available: boss %s -> %s. [u]pgrade [d]ismiss"
)

func (h Model) cloudGateLine() string {
	if !h.LoggedIn || h.CloudAccess == nil {
		return ""
	}
	if h.cloudErr != nil {
		return h.statusLine(ui.ColorWarning, CloudAccessUnavailableLine(h.cloudErr))
	}
	if h.CloudStatus.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return h.statusLine(ui.ColorWarning, CloudBillingUnavailableLine)
	}
	if h.cloudChecking && h.CloudStatus == nil {
		return h.statusLine(ui.ColorMuted, statusCloudChecking)
	}
	switch h.CloudStatus.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		if checkoutAbandoned(h.CloudStatus) {
			if h.upgradeAvailable {
				return h.statusLine(ui.ColorWarning, statusCloudResumeCheckoutUpgrade)
			}
			return h.statusLine(ui.ColorWarning, statusCloudResumeCheckout)
		}
		if h.upgradeAvailable {
			return h.statusLine(ui.ColorWarning, statusCloudNeedsSubscriptionUpgrade)
		}
		return h.statusLine(ui.ColorWarning, statusCloudNeedsSubscription)
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH:
		if h.upgradeAvailable {
			return h.statusLine(ui.ColorWarning, statusCloudPendingUpgrade)
		}
		return h.statusLine(ui.ColorWarning, statusCloudPending)
	default:
		return ""
	}
}

func (h Model) cloudCheckoutStatusLine() string {
	if !h.LoggedIn || h.CloudAccess == nil || h.cloudCheckoutStatus == "" {
		return ""
	}
	return h.statusLine(ui.ColorSuccess, h.cloudCheckoutStatus)
}

func fetchCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.GetCloudAccessStatus(ctx)
		return cloudAccessMsg{Status: status, Err: err}
	}
}

func refreshCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.RefreshCloudEntitlements(ctx)
		return cloudAccessMsg{Status: status, Err: err}
	}
}

func cloudAccessPending(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
}

// checkoutAbandoned reads the boolean pair the server publishes to separate an
// account that opened Stripe Checkout and walked away from one whose payment is
// landing. Both carry checkout_started; only the abandoned one still carries
// can_create_checkout, because only it can be sent back through checkout. The
// distinction is the whole point of BOS-1076, and Home is the surface the
// reported account was looking at — it must not describe an unfinished checkout
// as a subscription that was never started.
//
// checkout_started && !can_create_checkout is the other half of the pair: the
// user returned from Stripe and the entitlement is landing, which reaches Home
// as PENDING_ENTITLEMENT_REFRESH and keeps its own copy.
//
// Reachable on Home's cold read (fetchCloudAccessStatus -> GetCloudAccessStatus),
// which is the reported case: the checkout was abandoned in an earlier session
// or in the browser. It is NOT reachable for a checkout opened from this boss
// session, because that turns on cloudCheckoutPolling and every tick then calls
// RefreshCloudEntitlements, which promotes the account to EntitlementPending
// within seconds and keeps it there. See the KNOWN LIMITATION note on the
// promotion site in bosso's RefreshCloudEntitlements; the server needs an
// explicit returning-from-checkout signal before this pair survives a poll.
func checkoutAbandoned(status *pb.CloudAccessStatus) bool {
	return status.GetCheckoutStarted() && status.GetCanCreateCheckout()
}
