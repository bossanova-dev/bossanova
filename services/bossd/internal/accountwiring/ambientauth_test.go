package accountwiring

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
)

// Synthetic credential material for the ambient-comparison tests. The FAKE-
// prefix is the sentinel the redaction assertions search for; nothing here is
// or resembles a real credential.
const (
	ambientTestAccountID = "FAKE-PROVIDER-ACCOUNT"
	ambientTestOtherID   = "FAKE-OTHER-PROVIDER-ACCOUNT"
	ambientTestRefresh   = "FAKE-STORED-REFRESH"
	ambientTestRotated   = "FAKE-ROTATED-REFRESH"
	ambientTestAccess    = "FAKE-ACCESS"
)

// ambientTestSecrets is every synthetic value that must never reach durable
// state. It is the single place the sentinel set is written.
var ambientTestSecrets = []string{
	ambientTestAccountID, ambientTestOtherID,
	ambientTestRefresh, ambientTestRotated, ambientTestAccess,
}

func codexAuthBlob(t *testing.T, accountID, refresh string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  ambientTestAccess,
			"refresh_token": refresh,
			"account_id":    accountID,
		},
	})
	if err != nil {
		t.Fatalf("marshal codex blob: %v", err)
	}
	return raw
}

// --- classification -------------------------------------------------------

// TestWithSupersededClass is the whole mapping from an ambient comparison onto
// the durable failure class, including the two rules that keep the signal
// honest: the provider's own verdict is never overwritten, and every ambient
// state but "superseded" contributes nothing at all.
func TestWithSupersededClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome models.AuthCheckOutcome
		class   string
		ambient credmaterialize.AmbientAuthState
		wantCls string
	}{
		{
			name:    "healthy plus superseded is reported",
			outcome: models.AuthCheckOutcomeHealthy, class: "",
			ambient: credmaterialize.AmbientAuthSuperseded,
			wantCls: authFailureCredentialSuperseded,
		},
		{
			name:    "healthy plus in sync says nothing",
			outcome: models.AuthCheckOutcomeHealthy, class: "",
			ambient: credmaterialize.AmbientAuthInSync,
			wantCls: "",
		},
		{
			// Also the two-account case: an ambient login for a DIFFERENT
			// provider account resolves to not-evaluable, so it lands here and
			// produces no class rather than a benign-looking one.
			name:    "healthy plus not evaluable says nothing",
			outcome: models.AuthCheckOutcomeHealthy, class: "",
			ambient: credmaterialize.AmbientAuthNotEvaluable,
			wantCls: "",
		},
		{
			name:    "a confirmed auth failure keeps the provider class",
			outcome: models.AuthCheckOutcomeAuthInvalid, class: authFailureAuthInvalidated,
			ambient: credmaterialize.AmbientAuthSuperseded,
			wantCls: authFailureAuthInvalidated,
		},
		{
			name:    "a transient failure keeps its own class",
			outcome: models.AuthCheckOutcomeTransient, class: authFailureRateLimited,
			ambient: credmaterialize.AmbientAuthSuperseded,
			wantCls: authFailureRateLimited,
		},
		{
			name:    "an unavailable run keeps its own class",
			outcome: models.AuthCheckOutcomeUnavailable, class: authFailureRunnerUnavailable,
			ambient: credmaterialize.AmbientAuthSuperseded,
			wantCls: authFailureRunnerUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withSupersededClass(tc.outcome, tc.class, tc.ambient); got != tc.wantCls {
				t.Fatalf("withSupersededClass = %q; want %q", got, tc.wantCls)
			}
		})
	}
}

// --- durable recording ----------------------------------------------------

// TestMaintainerRecordsSupersededWithoutChangingEligibility is the acceptance
// criterion in one test: a clean verification whose ambient login has rotated
// the refresh chain records the superseded class, keeps the HEALTHY outcome,
// and leaves every input the selection predicates read untouched.
//
// The outcome half is the load-bearing half. auth_invalid is the single outcome
// that removes an account from both selection tiers; recording anything but
// healthy here would bench an account the provider just accepted.
func TestMaintainerRecordsSupersededWithoutChangingEligibility(t *testing.T) {
	acct := codexAcct("acct-1", nil)
	store := newAuthStore(acct)
	verifier := &fakeVerifier{ambient: credmaterialize.AmbientAuthSuperseded}
	m := newMaintainer(t, verifier, store, nil)

	if err := m.Smoke(context.Background(), "acct-1", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}

	writes := store.recorded()
	if len(writes) != 1 {
		t.Fatalf("RecordAuthCheck writes = %d; want 1", len(writes))
	}
	got := writes[0]
	if got.Outcome != models.AuthCheckOutcomeHealthy {
		t.Fatalf("outcome = %q; want %q: a superseded refresh chain is not a present rejection",
			got.Outcome, models.AuthCheckOutcomeHealthy)
	}
	if got.FailureClass != authFailureCredentialSuperseded {
		t.Fatalf("failure class = %q; want %q", got.FailureClass, authFailureCredentialSuperseded)
	}

	// Eligibility: isSelectable (rotation/engine.go) reads exactly Status,
	// Health, and IsAuthInvalid, and the resolver reads the same AuthInvalid
	// flag through toMeta. None of them may move.
	recorded := &models.Account{
		ID:        acct.ID,
		Provider:  acct.Provider,
		Status:    acct.Status,
		Health:    acct.Health,
		AuthCheck: got,
	}
	if recorded.IsAuthInvalid() {
		t.Fatal("a superseded stored credential was recorded as auth-invalid; eligibility must be unaffected")
	}
	if toMeta(recorded).AuthInvalid {
		t.Fatal("a superseded stored credential projects AuthInvalid onto the resolver's account meta")
	}
	if recorded.Status != models.AccountStatusActive || recorded.Health != models.AccountHealthOK {
		t.Fatalf("status/health moved: %q/%q", recorded.Status, recorded.Health)
	}
}

// TestMaintainerRecordsNoClassForNonSupersededAmbientStates pins that the
// no-op and not-evaluable cases record NO failure class at all, rather than a
// benign-looking one that an operator would have to learn to ignore.
func TestMaintainerRecordsNoClassForNonSupersededAmbientStates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ambient credmaterialize.AmbientAuthState
	}{
		{"in sync", credmaterialize.AmbientAuthInSync},
		{"not evaluable", credmaterialize.AmbientAuthNotEvaluable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newAuthStore(codexAcct("acct-1", nil))
			m := newMaintainer(t, &fakeVerifier{ambient: tc.ambient}, store, nil)

			if err := m.Smoke(context.Background(), "acct-1", "codex", nil); err != nil {
				t.Fatalf("Smoke: %v", err)
			}
			writes := store.recorded()
			if len(writes) != 1 {
				t.Fatalf("RecordAuthCheck writes = %d; want 1", len(writes))
			}
			if writes[0].FailureClass != "" {
				t.Fatalf("failure class = %q; want empty: this ambient state is not a signal", writes[0].FailureClass)
			}
			if writes[0].Outcome != models.AuthCheckOutcomeHealthy {
				t.Fatalf("outcome = %q; want %q", writes[0].Outcome, models.AuthCheckOutcomeHealthy)
			}
		})
	}
}

// TestMaintainerKeepsProviderFailureClassOverSuperseded pins that the provider
// remains the only authority on a present rejection. A run that failed already
// carries a strictly more actionable class — "reauthenticate now" beats
// "reauthenticate eventually" — and overwriting it would lose the remedy.
func TestMaintainerKeepsProviderFailureClassOverSuperseded(t *testing.T) {
	store := newAuthStore(codexAcct("acct-1", nil))
	verifier := &fakeVerifier{
		err:     errors.New("credential verification failed: " + codexAuthRequiredSentinel),
		ambient: credmaterialize.AmbientAuthSuperseded,
	}
	m := newMaintainer(t, verifier, store, nil)

	if err := m.Smoke(context.Background(), "acct-1", "codex", nil); err == nil {
		t.Fatal("Smoke reported a clean run for a refused credential")
	}
	writes := store.recorded()
	if len(writes) != 1 {
		t.Fatalf("RecordAuthCheck writes = %d; want 1", len(writes))
	}
	if writes[0].Outcome != models.AuthCheckOutcomeAuthInvalid {
		t.Fatalf("outcome = %q; want %q", writes[0].Outcome, models.AuthCheckOutcomeAuthInvalid)
	}
	if writes[0].FailureClass != authFailureAuthInvalidated {
		t.Fatalf("failure class = %q; want %q: the provider's verdict must win",
			writes[0].FailureClass, authFailureAuthInvalidated)
	}
}

// TestSupersededRecordCarriesNoCredentialMaterial pins that nothing which
// reaches durable state derives from a token or an account_id. The recorded
// row is metadata by construction: a closed-set outcome and a closed-set class.
func TestSupersededRecordCarriesNoCredentialMaterial(t *testing.T) {
	store := newAuthStore(codexAcct("acct-1", nil))
	m := newMaintainer(t, &fakeVerifier{ambient: credmaterialize.AmbientAuthSuperseded}, store, nil)

	if err := m.Smoke(context.Background(), "acct-1", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	writes := store.recorded()
	if len(writes) != 1 {
		t.Fatalf("RecordAuthCheck writes = %d; want 1", len(writes))
	}
	durable := string(writes[0].Outcome) + "|" + writes[0].FailureClass
	for i, secret := range ambientTestSecrets {
		if strings.Contains(durable, secret) {
			// Name only WHICH sentinel leaked, never the value, so this
			// assertion cannot itself become the leak it guards against.
			t.Fatalf("durable auth-check state carries synthetic credential material (sentinel index %d)", i)
		}
	}
}

// --- SmokeRunner integration ---------------------------------------------

// newAmbientSmokeRunner wires a SmokeRunner whose ambient CODEX_HOME is a temp
// dir the test controls.
func newAmbientSmokeRunner(t *testing.T, provider string, blobs map[string][]byte) (*SmokeRunner, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	runner, err := NewSmokeRunner(
		map[string]agent.AgentRunnerClient{provider: &smokeClient{}},
		&smokeCreds{blobs: blobs},
		zerolog.Nop(),
		WithSmokeBaseDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}
	return runner, home
}

func writeAmbientAuth(t *testing.T, home string, blob []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), blob, 0o600); err != nil {
		t.Fatalf("write ambient auth.json: %v", err)
	}
}

// TestSmokeRunnerReportsAmbientComparison is the end-to-end wiring: a real
// SmokeRunner over a real Materializer, reporting the three states the
// maintainer classifies. It is the only test that proves the comparison
// actually runs during a verification rather than only in isolation.
func TestSmokeRunnerReportsAmbientComparison(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ambient func(t *testing.T) []byte
		want    credmaterialize.AmbientAuthState
	}{
		{
			name:    "rotated ambient login supersedes the stored credential",
			ambient: func(t *testing.T) []byte { return codexAuthBlob(t, ambientTestAccountID, ambientTestRotated) },
			want:    credmaterialize.AmbientAuthSuperseded,
		},
		{
			name:    "matching ambient login is in sync",
			ambient: func(t *testing.T) []byte { return codexAuthBlob(t, ambientTestAccountID, ambientTestRefresh) },
			want:    credmaterialize.AmbientAuthInSync,
		},
		{
			name:    "an ambient login for another account is silent",
			ambient: func(t *testing.T) []byte { return codexAuthBlob(t, ambientTestOtherID, ambientTestRotated) },
			want:    credmaterialize.AmbientAuthNotEvaluable,
		},
		{
			name:    "no ambient login at all",
			ambient: nil,
			want:    credmaterialize.AmbientAuthNotEvaluable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := codexAuthBlob(t, ambientTestAccountID, ambientTestRefresh)
			runner, home := newAmbientSmokeRunner(t, "codex", map[string][]byte{"acct-1": stored})
			if tc.ambient != nil {
				writeAmbientAuth(t, home, tc.ambient(t))
			}

			res, err := runner.Verify(context.Background(), "acct-1", "codex", nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if res.AmbientAuth != tc.want {
				t.Fatalf("SmokeResult.AmbientAuth = %v; want %v", res.AmbientAuth, tc.want)
			}
		})
	}
}

// TestSmokeRunnerAmbientComparisonSkipsNonCodexProviders pins that only codex
// has an ambient CLI login to compare against. A claude account must report
// not-evaluable even when a rotated codex auth.json is sitting right there.
func TestSmokeRunnerAmbientComparisonSkipsNonCodexProviders(t *testing.T) {
	runner, home := newAmbientSmokeRunner(t, "claude", map[string][]byte{"acct-claude": []byte("FAKE-CLAUDE-TOKEN")})
	writeAmbientAuth(t, home, codexAuthBlob(t, ambientTestAccountID, ambientTestRotated))

	res, err := runner.Verify(context.Background(), "acct-claude", "claude", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.AmbientAuth != credmaterialize.AmbientAuthNotEvaluable {
		t.Fatalf("SmokeResult.AmbientAuth = %v; want %v for a non-codex provider",
			res.AmbientAuth, credmaterialize.AmbientAuthNotEvaluable)
	}
}

// TestSmokeRunnerAmbientComparisonSurvivesAFailedRun pins that the comparison
// still happens on a failure path. A run that failed is exactly when an
// operator most needs to know the stored refresh chain was superseded, and the
// comparison is evaluated on every return rather than only the clean one.
func TestSmokeRunnerAmbientComparisonSurvivesAFailedRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	client := &smokeClient{exitError: "boom"}
	runner, err := NewSmokeRunner(
		map[string]agent.AgentRunnerClient{"codex": client},
		&smokeCreds{blobs: map[string][]byte{"acct-1": codexAuthBlob(t, ambientTestAccountID, ambientTestRefresh)}},
		zerolog.Nop(),
		WithSmokeBaseDir(t.TempDir()),
	)
	if err != nil {
		t.Fatalf("NewSmokeRunner: %v", err)
	}
	writeAmbientAuth(t, home, codexAuthBlob(t, ambientTestAccountID, ambientTestRotated))

	res, verifyErr := runner.Verify(context.Background(), "acct-1", "codex", nil)
	if verifyErr == nil {
		t.Fatal("Verify reported a clean run for a failing agent")
	}
	if res.AmbientAuth != credmaterialize.AmbientAuthSuperseded {
		t.Fatalf("SmokeResult.AmbientAuth = %v; want %v on a failure path",
			res.AmbientAuth, credmaterialize.AmbientAuthSuperseded)
	}
}

// TestMain points CODEX_HOME at an empty directory for the whole package.
//
// Credential verification now reads the AMBIENT codex auth.json on every return
// path (BOS-1175). Without this, a test that does not set CODEX_HOME would read
// the developer's real ~/.codex/auth.json — a live credential file — and the
// suite would behave differently on a machine with a codex login than in CI.
// Individual tests still override it with t.Setenv, which restores to this value
// rather than to the operator's real home.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "accountwiring-codex-home-*")
	if err != nil {
		panic("create hermetic CODEX_HOME: " + err.Error())
	}
	if err := os.Setenv("CODEX_HOME", dir); err != nil {
		panic("set hermetic CODEX_HOME: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
