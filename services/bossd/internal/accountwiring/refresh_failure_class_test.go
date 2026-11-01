package accountwiring

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

// fakeRefreshFailures stands in for the refresh broker: it reports one class
// for one account, and records the since it was asked about.
type fakeRefreshFailures struct {
	accountID string
	class     credmaterialize.RefreshFailureClass
	asked     []time.Time
}

func (f *fakeRefreshFailures) RefreshFailureSince(accountID string, since time.Time) (credmaterialize.RefreshFailureClass, bool) {
	f.asked = append(f.asked, since)
	if accountID != f.accountID || f.class == "" {
		return "", false
	}
	return f.class, true
}

func brokerFailureSmokeRunner(t *testing.T, exitError string, failures refreshFailureSource) *SmokeRunner {
	t.Helper()
	blob := []byte(`{"access":"access-token","refresh":"refresh-token","id_token":"id-token"}`)
	runner, err := NewSmokeRunner(map[string]agent.RunnerClient{"codex": &smokeClient{exitError: exitError}}, &smokeCreds{
		blobs: map[string][]byte{"acct-1": blob},
	}, zerolog.Nop(), WithSmokeBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}
	runner.refreshFailures = failures
	return runner
}

func TestSmokeRecordsTheBrokerFailureClassOnTransientChecks_Accounts_AC223402(t *testing.T) {
	for _, tc := range []struct {
		name        string
		exitError   string
		wantOutcome domain.AuthCheckOutcome
		wantClass   string
	}{
		{"broker failure refines an unclassified transient", "stream disconnected before completion", domain.AuthCheckOutcomeTransient, string(credmaterialize.RefreshFailureDeviceNotEnrolled)},
		{"auth_invalid keeps its own class", codexAuthRequiredSentinel, domain.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated},
		{"rate_limited keeps its own class", "agent rate limit reached (429)", domain.AuthCheckOutcomeTransient, authFailureRateLimited},
		{"unavailable keeps its own class", "could not run the codex binary", domain.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable},
		{"a clean run reads no broker failure", "", domain.AuthCheckOutcomeHealthy, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := &fakeRefreshFailures{accountID: "acct-1", class: credmaterialize.RefreshFailureDeviceNotEnrolled}
			store := newAuthStore(codexAcct("acct-1", nil))
			m := newMaintainer(t, brokerFailureSmokeRunner(t, tc.exitError, failures), store, nil)
			before := time.Now()

			_ = m.Smoke(context.Background(), "acct-1", "codex", nil)

			writes := store.recorded()
			if len(writes) != 1 {
				t.Fatalf("RecordAuthCheck writes = %d; want 1", len(writes))
			}
			if writes[0].Outcome != tc.wantOutcome || writes[0].FailureClass != tc.wantClass {
				t.Fatalf("recorded %q/%q; want %q/%q", writes[0].Outcome, writes[0].FailureClass, tc.wantOutcome, tc.wantClass)
			}
			if tc.exitError == "" && len(failures.asked) != 0 {
				t.Fatal("a clean run consulted the broker")
			}
			for _, since := range failures.asked {
				if since.Before(before) {
					t.Fatalf("asked for failures since %v, before the run started at %v", since, before)
				}
			}
		})
	}
}

func TestWithRefreshFailureClassNeverChangesTheOutcomeOrAProviderClass(t *testing.T) {
	broker := credmaterialize.RefreshFailureOutcomeUnknown
	for _, tc := range []struct {
		outcome domain.AuthCheckOutcome
		class   string
		broker  credmaterialize.RefreshFailureClass
		want    string
	}{
		{domain.AuthCheckOutcomeTransient, authFailureUnclassified, broker, string(broker)},
		{domain.AuthCheckOutcomeTransient, authFailureTransientProvider, broker, string(broker)},
		{domain.AuthCheckOutcomeTransient, authFailureUnclassified, "", authFailureUnclassified},
		{domain.AuthCheckOutcomeTransient, authFailureRateLimited, broker, authFailureRateLimited},
		{domain.AuthCheckOutcomeTransient, authFailureUsageExhausted, broker, authFailureUsageExhausted},
		{domain.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated, broker, authFailureAuthInvalidated},
		{domain.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable, broker, authFailureRunnerUnavailable},
		{domain.AuthCheckOutcomeHealthy, "", broker, ""},
	} {
		if got := withRefreshFailureClass(tc.outcome, tc.class, tc.broker); got != tc.want {
			t.Errorf("withRefreshFailureClass(%q, %q, %q) = %q; want %q", tc.outcome, tc.class, tc.broker, got, tc.want)
		}
	}
}

func TestSmokeRunnerWithoutBrokerReportsNoRefreshFailure(t *testing.T) {
	runner := brokerFailureSmokeRunner(t, "stream disconnected before completion", nil)
	res, err := runner.Verify(context.Background(), "acct-1", "codex", nil)
	if err == nil || res.RefreshFailure != "" {
		t.Fatalf("Verify = %+v, %v; want a failure with no broker class", res, err)
	}
	var nilRunner *SmokeRunner
	if _, err := nilRunner.Verify(context.Background(), "acct-1", "codex", nil); err == nil {
		t.Fatal("a nil runner verified")
	}
}
