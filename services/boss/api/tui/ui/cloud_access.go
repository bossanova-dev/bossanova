package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/recurser/boss/internal/domain/rpcerr"
)

// CheckoutRefusedByAccessState reports whether the server refused to mint a
// checkout because this account's state moved underneath the client: it is
// already paying, or its entitlement is mid-flight. Both refusals are settled by
// re-reading the cloud access status, never by telling the user that checkout is
// unavailable. The account this distinction exists for — checkout completed, tab
// closed, never came back — now reaches the "already active" refusal, and it is a
// paying customer; "Cloud checkout unavailable" is the wrong thing to show it.
//
// The one CreateCheckoutSession refusal deliberately outside this set is
// "stripe customer is required": that is a provisioning failure, not a state the
// account can poll its way out of, so it still falls through to the error path.
func CheckoutRefusedByAccessState(err error) bool {
	return CheckoutActivationPending(err) || CheckoutAlreadyActive(err)
}

func CheckoutActivationPending(err error) bool {
	return CheckoutRefusalMentions(err, "being activated")
}

func CheckoutAlreadyActive(err error) bool {
	return CheckoutRefusalMentions(err, "already active")
}

func CheckoutRefusalMentions(err error, phrase string) bool {
	if err == nil || rpcerr.KindOf(err) != rpcerr.FailedPrecondition {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), phrase)
}

var cloudAccessSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b((?:access_token|refresh_token|id_token|token|api[_-]?key|client[_-]?secret|secret|key)\s*=\s*)[^&\s,;]+`),
	regexp.MustCompile(`(?i)\b(authorization\s*:\s*bearer\s+)[^\s,;]+`),
}

// CloudBillingUnavailableLine is the user-facing copy shown when Bossanova
// Cloud reports its billing system as unavailable. It is shared between the
// home gate line and the subscription flow so the two cannot drift.
const CloudBillingUnavailableLine = "Cloud billing unavailable. Local sessions are still available."

// CloudAccessUnavailableFallbackLine is shown when the subscription flow reaches
// an unexpected, unrecoverable state with no specific error to report.
const CloudAccessUnavailableFallbackLine = "Bossanova Cloud is unavailable. Local sessions are still available."

func CloudAccessUnavailableLine(err error) string {
	return fmt.Sprintf(
		"Cloud access status unavailable: %s. Local sessions are still available.",
		CloudAccessErrorSummary(err),
	)
}

func CloudAccessErrorSummary(err error) string {
	if err == nil {
		return "unknown error"
	}
	if code := rpcerr.KindOf(err); code != rpcerr.Unknown {
		message := RedactCloudAccessError(ConnectErrorMessage(err))
		return fmt.Sprintf("%s: %s", strings.ToLower(code.String()), message)
	}
	return RedactCloudAccessError(err.Error())
}

func ConnectErrorMessage(err error) string {
	if rpcerr.KindOf(err) != rpcerr.Unknown {
		if message := rpcerr.Message(err); message != "" {
			return message
		}
	}
	return "unknown error"
}

func RedactCloudAccessError(message string) string {
	for _, pattern := range cloudAccessSecretPatterns {
		message = pattern.ReplaceAllString(message, "$1[redacted]")
	}
	return message
}
