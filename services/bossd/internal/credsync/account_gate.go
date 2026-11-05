package credsync

import "github.com/recurser/bossd/internal/domain"

// AgentAccountBlockReason reports a known credential-sharing failure for a synced
// Codex account. Other providers refresh locally; unknown readiness cannot
// establish that a refresh will fail.
func (r Readiness) AgentAccountBlockReason(a *domain.AgentAccount) string {
	if a.Provider != domain.AgentAccountProviderCodex || a.SyncedAgentAccountID == "" {
		return ""
	}
	switch r.State {
	case ReadinessUnenrolled, ReadinessEnrollmentPending, ReadinessEnrollmentExpired, ReadinessRevoked, ReadinessBootstrapUnpinned:
		return string(r.State)
	default:
		return ""
	}
}
