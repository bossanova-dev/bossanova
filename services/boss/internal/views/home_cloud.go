// Home's Bossanova Cloud access, checkout, and gate-line rendering. Split out
// of home.go (BOS-526); the declarations are unchanged.

package views

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type CloudAccessClient interface {
	GetCloudAccessStatus(ctx context.Context) (*pb.CloudAccessStatus, error)
	CreateCheckoutSession(ctx context.Context, returnURL, cancelURL string) (string, error)
	CreateBillingPortalSession(ctx context.Context, returnURL string) (string, error)
	RefreshCloudEntitlements(ctx context.Context) (*pb.CloudAccessStatus, error)
}

func (h *HomeModel) SetCloudAccessClient(c CloudAccessClient) {
	h.SetCloudSubscription(c, h.checkoutReturnURL, h.checkoutCancelURL)
}

func (h *HomeModel) SetCloudSubscription(c CloudAccessClient, returnURL, cancelURL string) {
	h.cloudAccess = c
	h.checkoutReturnURL = returnURL
	h.checkoutCancelURL = cancelURL
}

func (h HomeModel) handleCloudAccess(msg cloudAccessMsg) (tea.Model, tea.Cmd) {
	h.cloudChecking = false
	h.cloudStatus = msg.status
	h.cloudErr = msg.err
	if subscriptionIsActive(msg.status) {
		h.cloudCheckoutPolling = false
		h.cloudCheckoutStatus = ""
	}
	if msg.status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		h.cloudCheckoutPolling = false
	}
	return h, nil
}

func (h HomeModel) handleCloudCheckout(msg homeCloudCheckoutMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		if checkoutActivationPending(msg.err) {
			h.cloudStatus = &pb.CloudAccessStatus{
				State:   pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
				Message: "Your subscription is being activated.",
			}
			h.cloudErr = nil
			h.cloudCheckoutStatus = ""
			h.cloudCheckoutPolling = true
			h.cloudChecking = true
			return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)
		}
		if msg.url != "" {
			h.cloudCheckoutStatus = "Open this billing URL: " + msg.url
			h.cloudCheckoutPolling = true
			h.cloudChecking = true
			return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)
		} else {
			h.cloudCheckoutStatus = "Cloud checkout unavailable. Local sessions are still available."
			h.cloudCheckoutPolling = false
		}
		return h, nil
	}
	h.cloudCheckoutStatus = "Opened Bossanova Cloud subscription checkout."
	h.cloudCheckoutPolling = true
	h.cloudChecking = true
	return h, refreshCloudAccessStatus(h.cloudAccess, h.ctx)
}

// cloudAction intentionally returns no action-bar entry. When the Cloud
// subscription is inactive, Home stays quiet: it shows a one-line reminder
// (see cloudGateLine) instead of a permanent [b]illing menu item. Re-entry to
// billing stays in the login/subscription flow.
func (h HomeModel) cloudAction() string {
	return ""
}

func (h HomeModel) canOpenCloudSubscription() bool {
	return h.loggedIn && h.cloudAccess != nil && (subscriptionNeedsCheckout(h.cloudStatus) || cloudAccessPending(h.cloudStatus))
}

func checkoutActivationPending(err error) bool {
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "being activated")
}

var cloudAccessSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b((?:access_token|refresh_token|id_token|token|api[_-]?key|client[_-]?secret|secret|key)\s*=\s*)[^&\s,;]+`),
	regexp.MustCompile(`(?i)\b(authorization\s*:\s*bearer\s+)[^\s,;]+`),
}

// cloudBillingUnavailableLine is the user-facing copy shown when Bossanova
// Cloud reports its billing system as unavailable. It is shared between the
// home gate line and the subscription flow so the two cannot drift.
const cloudBillingUnavailableLine = "Cloud billing unavailable. Local sessions are still available."

// cloudAccessUnavailableFallbackLine is shown when the subscription flow reaches
// an unexpected, unrecoverable state with no specific error to report.
const cloudAccessUnavailableFallbackLine = "Bossanova Cloud is unavailable. Local sessions are still available."

// Home's fixed status copy. These are named rather than inlined at their one
// call site each so TestHomeFixedStatusCopyStaysReadableAtFloor measures the
// strings the screen actually renders: the wrap floor is narrower than most of
// this copy (see minStatusWrapWidth), and the guard that bounds the resulting
// height only works if it cannot drift from the source. Edit the copy here and
// the guard follows; the longest of these, statusCloudNeedsSubscriptionUpgrade
// at 112 columns, is what sets that bound.
const (
	statusCloudChecking                 = "Checking Bossanova Cloud access..."
	statusCloudNeedsSubscriptionUpgrade = "Bossanova Cloud requires a subscription. Local sessions are still available. Upgrade boss first, then subscribe."
	statusCloudNeedsSubscription        = "Bossanova Cloud requires a subscription. Local sessions are still available. Type u to s[u]bscribe."
	statusCloudPendingUpgrade           = "Bossanova Cloud setup has not completed yet. Upgrade boss first, then subscribe."
	statusCloudPending                  = "Bossanova Cloud setup has not completed yet. Type u to s[u]bscribe."
	statusUpgrading                     = "Upgrading..."
	statusRestartingDaemon              = "Restarting daemon..."
	statusUpgradeRestartPrompt          = "Upgrade installed. Quit boss after restart to use the new binary. [r]estart [esc] later"
	statusUpgradeDone                   = "Upgrade installed. Quit boss (q) and re-launch to use the new binary."
	statusUpgradeAvailableFormat        = "Upgrade available: boss %s -> %s. [u]pgrade [d]ismiss"
)

func cloudAccessUnavailableLine(err error) string {
	return fmt.Sprintf(
		"Cloud access status unavailable: %s. Local sessions are still available.",
		cloudAccessErrorSummary(err),
	)
}

func cloudAccessErrorSummary(err error) string {
	if err == nil {
		return "unknown error"
	}
	if code := connect.CodeOf(err); code != connect.CodeUnknown {
		message := redactCloudAccessError(connectErrorMessage(err))
		return fmt.Sprintf("%s: %s", strings.ToLower(code.String()), message)
	}
	return redactCloudAccessError(err.Error())
}

func connectErrorMessage(err error) string {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		if message := connectErr.Message(); message != "" {
			return message
		}
	}
	return "unknown error"
}

func redactCloudAccessError(message string) string {
	for _, pattern := range cloudAccessSecretPatterns {
		message = pattern.ReplaceAllString(message, "$1[redacted]")
	}
	return message
}

func (h HomeModel) cloudGateLine() string {
	if !h.loggedIn || h.cloudAccess == nil {
		return ""
	}
	if h.cloudErr != nil {
		return h.statusLine(colorWarning, cloudAccessUnavailableLine(h.cloudErr))
	}
	if h.cloudStatus.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE {
		return h.statusLine(colorWarning, cloudBillingUnavailableLine)
	}
	if h.cloudChecking && h.cloudStatus == nil {
		return h.statusLine(colorMuted, statusCloudChecking)
	}
	switch h.cloudStatus.GetState() {
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_PAST_DUE,
		pb.CloudAccessState_CLOUD_ACCESS_STATE_CANCELED:
		if h.upgradeAvailable {
			return h.statusLine(colorWarning, statusCloudNeedsSubscriptionUpgrade)
		}
		return h.statusLine(colorWarning, statusCloudNeedsSubscription)
	case pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH:
		if h.upgradeAvailable {
			return h.statusLine(colorWarning, statusCloudPendingUpgrade)
		}
		return h.statusLine(colorWarning, statusCloudPending)
	default:
		return ""
	}
}

func (h HomeModel) cloudCheckoutStatusLine() string {
	if !h.loggedIn || h.cloudAccess == nil || h.cloudCheckoutStatus == "" {
		return ""
	}
	return h.statusLine(colorSuccess, h.cloudCheckoutStatus)
}

func fetchCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.GetCloudAccessStatus(ctx)
		return cloudAccessMsg{status: status, err: err}
	}
}

func refreshCloudAccessStatus(c CloudAccessClient, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		status, err := c.RefreshCloudEntitlements(ctx)
		return cloudAccessMsg{status: status, err: err}
	}
}

func cloudAccessPending(status *pb.CloudAccessStatus) bool {
	return status.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
}
