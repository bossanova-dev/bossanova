package accountwiring

import (
	"time"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/domain"
)

// refreshFailureSource is the slice of the refresh broker the smoke runner
// reads: the last redacted failure class the broker answered for an account.
type refreshFailureSource interface {
	RefreshFailureSince(accountID string, since time.Time) (credmaterialize.RefreshFailureClass, bool)
}

// refreshFailureSince reads the broker failure a failed verification run hit,
// if any. A runner without a broker has nothing to report.
func (r *SmokeRunner) refreshFailureSince(accountID string, since time.Time) credmaterialize.RefreshFailureClass {
	if r == nil || r.refreshFailures == nil {
		return ""
	}
	class, _ := r.refreshFailures.RefreshFailureSince(accountID, since)
	return class
}

// withRefreshFailureClass replaces a vague transient class with the broker's
// own class when the verification's Codex run hit a refresh-broker failure
// (BOS-2234). The broker classes are credmaterialize.RefreshFailureClass
// tokens, a closed set with no error text.
//
// It refines only a transient outcome whose class says nothing more specific
// (unclassified or transient_provider). auth_invalid, unavailable,
// rate_limited and usage_exhausted are the provider's or runner's own answer
// and are never overridden. It NEVER changes the outcome: even the
// reauthenticate classes stay transient, so no account is benched on a broker
// inference.
//
// No dated apiversion bump
// (docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff):
//   - Client grounds: the boss CLI, TUI and web read AuthCheck through the
//     OrchestratorService account procedures and may be version-skewed from
//     this daemon. AuthCheck.outcome is unchanged for every broker failure, so
//     eligibility and the severity every client derives from the outcome are
//     unchanged.
//   - Only failure_class on a non-healthy outcome is refined, and every client
//     renders that class verbatim (boss account ls prints transient:<class>).
//   - No transform is owed: SupersededCredentialClassChange in
//     lib/bossalib/apiversion/transform.go already states that classes on
//     failing checks are open to older clients, and blanking one would erase a
//     classification they could always observe.
func withRefreshFailureClass(
	outcome domain.AuthCheckOutcome,
	failureClass string,
	broker credmaterialize.RefreshFailureClass,
) string {
	if outcome != domain.AuthCheckOutcomeTransient || broker == "" {
		return failureClass
	}
	if failureClass != authFailureUnclassified && failureClass != authFailureTransientProvider {
		return failureClass
	}
	return string(broker)
}
