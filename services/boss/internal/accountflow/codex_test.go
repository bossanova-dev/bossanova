package accountflow

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/agentcred"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// scriptedProcHook returns a proc that replays lines, closes the channel, then
// runs hook when Wait is called (used to model codex writing auth.json on exit).
func scriptedProcHook(lines []string, waitErr error, hook func() error) *fakeProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return &fakeProc{lines: ch, waitErr: waitErr, waitHook: hook}
}

func codexIDToken(t *testing.T, email string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]string{"em" + "ail": email})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return "hdr." + payload + ".sig"
}

func codexAuthBytes(t *testing.T, tokens map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"tokens": tokens, "last_refresh": "2026-07-04T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tempCodexHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "codex-home-*")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunCodexAdd(t *testing.T) {
	t.Run("happy_path", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"id_token": codexIDToken(t, "codexuser@example.com"), "account_id": "acc",
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{
			"To sign in visit https://auth.openai.com/codex/device",
			"and enter code ABCD-EFGHI",
		}, nil, hook)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl, Priority: 3,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err != nil {
			t.Fatalf("RunCodexAdd: %v", err)
		}
		tr := pr.transcript()
		urlIdx := strings.Index(tr, "https://auth.openai.com/codex/device")
		codeIdx := strings.Index(tr, "ABCD-EFGHI")
		okIdx := strings.Index(tr, "registered and verified")
		if urlIdx < 0 || codeIdx < 0 {
			t.Fatalf("device URL/code not surfaced:\n%s", tr)
		}
		if okIdx >= 0 && urlIdx > okIdx {
			t.Fatalf("URL surfaced AFTER completion; want before:\n%s", tr)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
		req := cl.addReqs[0]
		if req.GetProvider() != "codex" {
			t.Fatalf("provider = %q", req.GetProvider())
		}
		// The stored blob must be the canonical account-store shape
		// ({access,refresh,id_token}) the daemon validates, NOT the raw auth.json
		// (nested tokens.*_token); otherwise TestAccount rejects every interactive
		// codex registration as a missing-field credential.
		var stored map[string]string
		if err := json.Unmarshal(req.GetCredential(), &stored); err != nil {
			t.Fatalf("stored credential is not a JSON object: %v", err)
		}
		if stored["access"] != "at" || stored["refresh"] != "rt" {
			t.Fatalf("stored credential = %v, want flat access/refresh from auth.json tokens", stored)
		}
		if stored["id_token"] == "" {
			t.Fatalf("stored credential dropped id_token: %v", stored)
		}
		// Unmarshaling into map[string]string above would fail on the raw
		// {"tokens":{...}} shape (object value, not string), so success already
		// proves the nested shape was flattened.
		if req.GetPriority() != 3 {
			t.Fatalf("priority = %d, want 3 (--priority must reach AddAccount)", req.GetPriority())
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("CODEX_HOME not removed: %v", err)
		}
	})

	t.Run("timeout_abandoned", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := newBlockingProc([]string{
			"https://auth.openai.com/codex/device",
			"code ABCD-EFGHI",
		})
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
			Timeout: 50 * time.Millisecond,
		})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timed out", err)
		}
		if !proc.wasKilled() {
			t.Fatalf("proc was not killed on timeout")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on timeout: %v", statErr)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("nonzero_exit", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{"connecting", "429 too many requests"}, errors.New("exit status 1"), nil)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "429 too many requests") {
			t.Fatalf("err = %v, want 429 line", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("disabled_setting", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{
			"device code login is not enabled for this Codex server. Use the browser login or verify the server URL.",
		}, errors.New("exit status 1"), nil)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil {
			t.Fatalf("RunCodexAdd returned nil, want disabled-device-auth error")
		}
		tr := pr.transcript()
		for _, want := range []string{
			"chatgpt.com/#settings",
			"device code authorization",
			"Security",
			"boss account add codex",
		} {
			if !strings.Contains(tr, want) {
				t.Fatalf("transcript missing %q:\n%s", want, tr)
			}
		}
		if strings.Contains(tr, "ABCD-EFGHI") {
			t.Fatalf("device code leaked into remediation transcript:\n%s", tr)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on disabled setting: %v", statErr)
		}
	})

	t.Run("disabled_setting_wins_over_timeout", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := newBlockingProc([]string{
			"device code login is not enabled for this Codex server. Use the browser login or verify the server URL.",
		})
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
			Timeout: 50 * time.Millisecond,
		})
		if err == nil || strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want disabled-device-auth error before timeout", err)
		}
		if !strings.Contains(pr.transcript(), "Enable device code authorization for Codex") {
			t.Fatalf("missing remediation transcript:\n%s", pr.transcript())
		}
		if !proc.wasKilled() {
			t.Fatalf("proc was not killed after disabled-device-auth signal")
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
		if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Fatalf("CODEX_HOME not removed on disabled setting: %v", statErr)
		}
	})

	t.Run("no_auth_json", func(t *testing.T) {
		dir := tempCodexHome(t)
		proc := scriptedProcHook([]string{"logging in"}, nil, nil)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if err == nil || !strings.Contains(err.Error(), "no auth.json") {
			t.Fatalf("err = %v, want 'no auth.json'", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("missing_id_token", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{"access_token": "at", "refresh_token": "rt"})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{"done"}, nil, hook)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		})
		if !errors.Is(err, agentcred.ErrCodexAuthMissingField) {
			t.Fatalf("err = %v, want ErrCodexAuthMissingField", err)
		}
		if !strings.Contains(err.Error(), "id_token") {
			t.Fatalf("err = %v, want mention of id_token", err)
		}
		if len(cl.addReqs) != 0 {
			t.Fatalf("AddAccount must not be called")
		}
	})

	t.Run("prompt_never_parsed", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "at", "refresh_token": "rt",
			"id_token": codexIDToken(t, "bg@example.com"),
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{"authenticated in background"}, nil, hook)
		ex := &fakeExec{proc: proc}
		cl := &fakeAccountClient{}
		if err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: &fakePrompter{}, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		}); err != nil {
			t.Fatalf("background-auth path should still succeed: %v", err)
		}
		if len(cl.addReqs) != 1 {
			t.Fatalf("want 1 AddAccount, got %d", len(cl.addReqs))
		}
	})

	t.Run("no_secret_leak", func(t *testing.T) {
		dir := tempCodexHome(t)
		auth := codexAuthBytes(t, map[string]any{
			"access_token": "super-secret-access", "refresh_token": "super-secret-refresh",
			"id_token": codexIDToken(t, "leak@example.com"),
		})
		hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
		proc := scriptedProcHook([]string{
			"https://auth.openai.com/codex/device", "code ABCD-EFGHI",
		}, nil, hook)
		ex := &fakeExec{proc: proc}
		pr := &fakePrompter{}
		cl := &fakeAccountClient{}
		if err := RunCodexAdd(context.Background(), CodexOptions{
			Exec: ex, Prompter: pr, Client: cl,
			HomeDir: func() (string, error) { return dir, nil },
		}); err != nil {
			t.Fatalf("RunCodexAdd: %v", err)
		}
		tr := pr.transcript()
		for _, secret := range []string{"super-secret-access", "super-secret-refresh", string(auth)} {
			if strings.Contains(tr, secret) {
				t.Fatalf("secret leaked into transcript:\n%s", tr)
			}
		}
	})
}

// --- reauthentication (BOS-1142) -------------------------------------------

// codexReauthFixture wires the shared happy-path device login: a temp CODEX_HOME
// the scripted proc writes a complete auth.json into on exit.
func codexReauthFixture(t *testing.T) (CodexOptions, *fakePrompter, *fakeAccountClient, string) {
	t.Helper()
	dir := tempCodexHome(t)
	auth := codexAuthBytes(t, map[string]any{
		"access_token": "at", "refresh_token": "rt",
		"id_token": codexIDToken(t, "codexuser@example.com"), "account_id": "acc",
	})
	hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0o600) }
	proc := scriptedProcHook([]string{
		"To sign in visit https://auth.openai.com/codex/device",
		"and enter code ABCD-EFGHI",
	}, nil, hook)
	pr := &fakePrompter{}
	cl := &fakeAccountClient{listResult: []*pb.Account{
		{Id: "acct-codex-1", Provider: "codex", Label: "codex-one"},
	}}
	return CodexOptions{
		Exec: &fakeExec{proc: proc}, Prompter: pr, Client: cl,
		HomeDir: func() (string, error) { return dir, nil },
	}, pr, cl, dir
}

func TestRunCodexReauthRefreshesInPlaceAndNeverAdds(t *testing.T) {
	o, pr, cl, dir := codexReauthFixture(t)
	cl.refreshResult = &pb.RefreshAccountResponse{
		Account: &pb.Account{Id: "acct-codex-1"}, LiveSmokeRan: true,
	}

	if err := RunCodexReauth(context.Background(), o, "acct-codex-1"); err != nil {
		t.Fatalf("RunCodexReauth: %v", err)
	}
	if len(cl.addReqs) != 0 {
		t.Fatalf("reauth called AddAccount %d times; it must refresh the existing row in place", len(cl.addReqs))
	}
	if len(cl.refreshReqs) != 1 {
		t.Fatalf("RefreshAccount calls = %d, want 1", len(cl.refreshReqs))
	}
	req := cl.refreshReqs[0]
	if req.GetId() != "acct-codex-1" {
		t.Fatalf("refreshed id = %q, want acct-codex-1", req.GetId())
	}
	if !req.GetTestAfterSave() {
		t.Fatal("reauth must request post-save verification; a save is not proof the credential is usable")
	}
	// The canonical account-store shape, not the raw auth.json envelope.
	var stored map[string]any
	if err := json.Unmarshal(req.GetCredential(), &stored); err != nil {
		t.Fatalf("credential is not JSON: %v", err)
	}
	if _, ok := stored["tokens"]; ok {
		t.Fatal("credential was sent as the raw auth.json envelope, not the flat account-store shape")
	}
	if stored["access"] != "at" {
		t.Fatalf("access = %v, want at", stored["access"])
	}
	if !strings.Contains(pr.transcript(), "Reauthenticated") {
		t.Fatalf("no success message; transcript: %s", pr.transcript())
	}
	// The isolated CODEX_HOME must not survive: it held a live credential.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp CODEX_HOME %s survived the flow (stat err = %v)", dir, err)
	}
}

func TestRunCodexReauthFailsWhenTheNewCredentialDoesNotVerify(t *testing.T) {
	o, pr, cl, _ := codexReauthFixture(t)
	cl.refreshResult = &pb.RefreshAccountResponse{
		Account:      &pb.Account{Id: "acct-codex-1", LastTestError: "401 unauthorized"},
		LiveSmokeRan: true,
	}

	err := RunCodexReauth(context.Background(), o, "acct-codex-1")
	if err == nil {
		t.Fatal("want an error when the replacement credential fails verification")
	}
	if !strings.Contains(err.Error(), "401 unauthorized") {
		t.Fatalf("error does not carry the provider reason: %v", err)
	}
	if strings.Contains(pr.transcript(), "Reauthenticated") {
		t.Fatalf("claimed success on a failed verification; transcript: %s", pr.transcript())
	}
	// A failed reauth must never remove the account: the row, its label and its
	// bindings are exactly what the operator is trying to recover.
	if len(cl.removedIDs) != 0 {
		t.Fatalf("reauth removed accounts %v; it must leave the row in place", cl.removedIDs)
	}
}

func TestRunCodexReauthKeepsGoingWhenVerificationCouldNotRun(t *testing.T) {
	o, pr, cl, _ := codexReauthFixture(t)
	cl.refreshResult = &pb.RefreshAccountResponse{
		Account: &pb.Account{Id: "acct-codex-1", LastTestError: liveSmokeUnavailableDetail},
		Detail:  liveSmokeUnavailableDetail,
	}

	if err := RunCodexReauth(context.Background(), o, "acct-codex-1"); err != nil {
		t.Fatalf("verification being unavailable is not a credential failure: %v", err)
	}
	if !strings.Contains(pr.transcript(), "Reauthenticated") {
		t.Fatalf("no reauthenticated message; transcript: %s", pr.transcript())
	}
	if !strings.Contains(pr.transcript(), "couldn't run") {
		t.Fatalf("did not disclose that verification was skipped; transcript: %s", pr.transcript())
	}
}

// TestRunCodexReauthDoesNotClaimAnIneligibleAccountIsEligible pins the
// eligibility half of the closing verdict to the account state the daemon
// actually returned.
//
// RefreshAccount replaces the credential and restores HEALTH; it does not
// enable a disabled account and does not clear a cooldown. The TUI offers [R]
// on every codex row, disabled and cooling ones included, so the flow used to
// close a perfectly successful reauthentication of a disabled account with "the
// account is eligible again" — an operator reading that walks away believing
// rotation can select the row, and it cannot.
func TestRunCodexReauthDoesNotClaimAnIneligibleAccountIsEligible(t *testing.T) {
	o, pr, cl, _ := codexReauthFixture(t)
	cl.refreshResult = &pb.RefreshAccountResponse{
		Account:      &pb.Account{Id: "acct-codex-1", Status: "disabled", Health: "ok"},
		LiveSmokeRan: true,
	}

	if err := RunCodexReauth(context.Background(), o, "acct-codex-1"); err != nil {
		t.Fatalf("RunCodexReauth: %v", err)
	}
	tr := pr.transcript()
	// The credential half of the verdict is still true and must survive: the
	// remedy for a disabled row is `boss account update --status active`, not
	// another reauthentication.
	if !strings.Contains(tr, "the new credential verified") {
		t.Fatalf("dropped the verification verdict; transcript: %s", tr)
	}
	if strings.Contains(tr, "eligible again") {
		t.Fatalf("claimed a disabled account is eligible; transcript: %s", tr)
	}
	if !strings.Contains(tr, "disabled") {
		t.Fatalf("did not say why the account still will not be selected; transcript: %s", tr)
	}
}

// TestReauthVerifiedLineDerivesEligibilityFromTheReturnedAccount walks the
// states RefreshAccount can hand back. The verification claim is constant; only
// the eligibility clause moves.
func TestReauthVerifiedLineDerivesEligibilityFromTheReturnedAccount(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	active := func() *pb.Account { return &pb.Account{Id: "a", Status: "active", Health: "ok"} }
	// A proto message carries a mutex, so each case gets a fresh row rather than
	// a copy of a shared one.
	withCooldown := func(at time.Time) *pb.Account {
		a := active()
		a.CooldownUntil = timestamppb.New(at)
		return a
	}
	tests := []struct {
		name         string
		acct         *pb.Account
		wantEligible bool
		wantContains string
	}{
		{
			name:         "an active healthy account is eligible",
			acct:         active(),
			wantEligible: true,
		},
		{
			// The row is fine; it is simply switched off. Reauthenticating it
			// changes nothing about that.
			name:         "a disabled account is not",
			acct:         &pb.Account{Id: "a", Status: "disabled", Health: "ok"},
			wantContains: "disabled",
		},
		{
			name:         "an account still marked failed is not",
			acct:         &pb.Account{Id: "a", Status: "active", Health: "failed"},
			wantContains: "marked failed",
		},
		{
			// RecordAuthCheck writes only the auth_check columns and leaves
			// Health alone, so this state is invisible to a status+health test.
			name:         "an auth-invalid verdict benches the account",
			acct:         &pb.Account{Id: "a", Status: "active", Health: "ok", AuthCheck: &pb.AuthCheck{Outcome: "auth_invalid"}},
			wantContains: "benched by its last credential check",
		},
		{
			name:         "a live cooldown is not cleared by a reauthentication",
			acct:         withCooldown(now.Add(30 * time.Minute)),
			wantContains: "cooling down",
		},
		{
			// An expired cooldown is not a cooldown.
			name:         "a cooldown that has already elapsed does not block",
			acct:         withCooldown(now.Add(-time.Minute)),
			wantEligible: true,
		},
		{
			// Unknown eligibility is not eligibility: an older CLI reading a
			// status token a newer daemon introduced must make no claim rather
			// than guess the permissive one.
			name: "an unrecognised status makes no claim",
			acct: &pb.Account{Id: "a", Status: "quarantined", Health: "ok"},
		},
		{
			// The same rule for the shape that carries no row at all.
			name: "a response with no account makes no claim",
			acct: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line := ReauthVerifiedLine("acct-1", tc.acct, now)
			if !strings.Contains(line, "Reauthenticated account acct-1") {
				t.Fatalf("line does not name the account: %q", line)
			}
			// Whatever the eligibility clause says, the line is still a VERIFIED
			// verdict — the TUI must not report it in the error tier.
			if !ReauthLineIsVerified(line) {
				t.Fatalf("verified line not classified as verified: %q", line)
			}
			switch {
			case tc.wantEligible:
				if !strings.Contains(line, "eligible again") {
					t.Fatalf("want an eligibility claim, got %q", line)
				}
			case tc.wantContains != "":
				if strings.Contains(line, "eligible again") {
					t.Fatalf("claimed eligibility for an ineligible account: %q", line)
				}
				if !strings.Contains(line, tc.wantContains) {
					t.Fatalf("line %q does not explain %q", line, tc.wantContains)
				}
			default:
				if strings.Contains(line, "eligible") || strings.Contains(line, "rotation will not select") {
					t.Fatalf("made an eligibility claim from state that supports none: %q", line)
				}
			}
		})
	}
}

// The unverified verdict must never be mistaken for the verified one: the TUI
// picks its report tier off exactly this predicate, and the fail-open direction
// is reporting an unverified reauthentication as a success.
func TestReauthLineIsVerifiedRejectsTheUnverifiedVerdict(t *testing.T) {
	for _, line := range []string{
		ReauthUnverifiedLine("acct-1", "no agent runner attached"),
		"",
		"Reauthenticated account acct-1.",
	} {
		if ReauthLineIsVerified(line) {
			t.Fatalf("classified %q as a verified verdict", line)
		}
	}
}

func TestRunCodexReauthRequiresAnAccountID(t *testing.T) {
	o, _, cl, _ := codexReauthFixture(t)
	if err := RunCodexReauth(context.Background(), o, "   "); err == nil {
		t.Fatal("want an error for a blank account id")
	}
	if len(cl.refreshReqs) != 0 {
		t.Fatal("a blank account id must be refused before any device login is driven")
	}
}

func TestClassifyRefresh(t *testing.T) {
	tests := []struct {
		name       string
		resp       *pb.RefreshAccountResponse
		err        error
		want       testOutcome
		wantReason string
	}{
		{
			name: "verified",
			resp: &pb.RefreshAccountResponse{Account: &pb.Account{Id: "a"}, LiveSmokeRan: true},
			want: testVerified,
		},
		{
			name:       "transport_error_is_a_failure",
			err:        errors.New("connect: unavailable"),
			want:       testFailed,
			wantReason: "connect: unavailable",
		},
		{
			name:       "provider_rejection_is_a_failure",
			resp:       &pb.RefreshAccountResponse{Account: &pb.Account{Id: "a", LastTestError: "401"}, LiveSmokeRan: true},
			want:       testFailed,
			wantReason: "401",
		},
		{
			name: "unavailable_is_not_a_credential_failure",
			resp: &pb.RefreshAccountResponse{
				Account: &pb.Account{Id: "a", LastTestError: liveSmokeUnavailableDetail},
				Detail:  liveSmokeUnavailableDetail,
			},
			want:       testUnavailable,
			wantReason: liveSmokeUnavailableDetail,
		},
		{
			name: "inconclusive_is_not_a_credential_failure",
			resp: &pb.RefreshAccountResponse{
				Account: &pb.Account{Id: "a", LastTestError: liveSmokeInconclusiveDetail},
				Detail:  liveSmokeInconclusiveDetail,
			},
			want:       testUnavailable,
			wantReason: liveSmokeInconclusiveDetail,
		},
		{
			// The zero value of testOutcome is testVerified, so an absent
			// response must be classified explicitly or silence reads as success.
			name: "no_response_and_no_error_is_not_success",
			want: testFailed,
		},
		{
			// The fail-open this classifier used to have: any live_smoke_ran=false
			// shape whose detail was not one of the two recognised sentinels fell
			// through to testVerified, so the flow announced "the new credential
			// verified" for a reauthentication nothing had verified. A detail a
			// newer daemon adds is the likely source, and the older CLI must
			// degrade to "not verified", never to "verified".
			name: "unrecognised_no_smoke_detail_is_not_verified",
			resp: &pb.RefreshAccountResponse{
				Account: &pb.Account{Id: "a"},
				Detail:  "verification deferred: no agent runner attached",
			},
			want:       testUnavailable,
			wantReason: "verification deferred: no agent runner attached",
		},
		{
			// The barest shape of the same fall-open: no smoke, no detail, no
			// recorded error. Nothing here is evidence of anything, least of all
			// of health.
			name: "no_smoke_and_no_detail_is_not_verified",
			resp: &pb.RefreshAccountResponse{Account: &pb.Account{Id: "a"}},
			want: testUnavailable,
		},
		{
			// A credential the daemon recorded as rejected still outranks "no
			// smoke ran": the row carries a real verdict that names a remedy, and
			// downgrading it to "unverified" would lose it.
			name: "recorded_rejection_without_smoke_stays_a_failure",
			resp: &pb.RefreshAccountResponse{
				Account: &pb.Account{Id: "a", LastTestError: "401 unauthorized"},
				Detail:  "some newer shape",
			},
			want:       testFailed,
			wantReason: "401 unauthorized",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyRefresh(tc.resp, tc.err)
			if got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// The inconclusive sentinel is duplicated across a module boundary; if bossd
// changes it, this flow silently starts reporting "no verdict" as a failure.
func TestLiveSmokeInconclusiveDetailMatchesBossd(t *testing.T) {
	want := "provider verification inconclusive: credential changed during verification"
	if liveSmokeInconclusiveDetail != want {
		t.Fatalf("liveSmokeInconclusiveDetail = %q, want %q (must match the bossd sentinel)", liveSmokeInconclusiveDetail, want)
	}
}

func TestRunCodexReauthRefusesBeforeTheLoginWhenTheAccountIsWrong(t *testing.T) {
	t.Run("unknown_id", func(t *testing.T) {
		o, _, cl, _ := codexReauthFixture(t)
		err := RunCodexReauth(context.Background(), o, "acct-nope")
		if err == nil || !strings.Contains(err.Error(), "no account with id acct-nope") {
			t.Fatalf("want an unknown-id refusal, got %v", err)
		}
		if len(cl.refreshReqs) != 0 {
			t.Fatal("refused reauth still called RefreshAccount")
		}
	})
	t.Run("wrong_provider_names_the_alternative", func(t *testing.T) {
		o, _, cl, _ := codexReauthFixture(t)
		cl.listResult = []*pb.Account{{Id: "acct-claude-1", Provider: "claude"}}
		err := RunCodexReauth(context.Background(), o, "acct-claude-1")
		if err == nil {
			t.Fatal("want a refusal for a non-codex account")
		}
		if !strings.Contains(err.Error(), "boss account refresh acct-claude-1") {
			t.Fatalf("refusal does not name the alternative: %v", err)
		}
		if len(cl.refreshReqs) != 0 {
			t.Fatal("refused reauth still called RefreshAccount")
		}
	})
}
