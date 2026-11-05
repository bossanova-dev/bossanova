package credsync

import (
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestAgentAccountBlockReason_Accounts_AC224701(t *testing.T) {
	for _, state := range []ReadinessState{"", ReadinessUnavailable, ReadinessUnknown, ReadinessReady, ReadinessUnenrolled, ReadinessEnrollmentPending, ReadinessEnrollmentExpired, ReadinessRevoked, ReadinessBootstrapUnpinned} {
		for _, provider := range []domain.AgentAccountProvider{domain.AgentAccountProviderCodex, domain.AgentAccountProviderClaude} {
			for _, syncedID := range []string{"", "synced"} {
				t.Run(string(state)+"/"+string(provider)+"/"+syncedID, func(t *testing.T) {
					acct := &domain.AgentAccount{Provider: provider, SyncedAgentAccountID: syncedID}
					want := ""
					if provider == domain.AgentAccountProviderCodex && syncedID != "" {
						switch state {
						case ReadinessUnenrolled, ReadinessEnrollmentPending, ReadinessEnrollmentExpired, ReadinessRevoked, ReadinessBootstrapUnpinned:
							want = string(state)
						case ReadinessUnavailable, ReadinessUnknown, ReadinessReady:
							want = ""
						}
					}
					if got := (Readiness{State: state}).AgentAccountBlockReason(acct); got != want {
						t.Fatalf("block reason = %q, want %q", got, want)
					}
				})
			}
		}
	}
}
