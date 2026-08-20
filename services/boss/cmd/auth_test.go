package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// loginVerdictStreams captures a command's output three ways. stdout and
// stderr are kept apart because WHICH stream a line lands on is itself part of
// the contract — the login result is stdout, the daemon's complaint is stderr,
// so a shell redirecting one still sees the other. combined keeps them
// interleaved because the ORDER of two lines on different streams is a
// separate contract, and separated buffers cannot show it.
type loginVerdictStreams struct {
	combined bytes.Buffer
	stdout   bytes.Buffer
	stderr   bytes.Buffer
}

// String reports the interleaved view, so assertions that only care that
// something was printed read naturally.
func (s *loginVerdictStreams) String() string { return s.combined.String() }

// loginVerdictCmd returns a command whose output is captured, plus the streams.
func loginVerdictCmd(t *testing.T) (*cobra.Command, *loginVerdictStreams) {
	t.Helper()
	s := &loginVerdictStreams{}
	cmd := &cobra.Command{}
	cmd.SetOut(io.MultiWriter(&s.combined, &s.stdout))
	cmd.SetErr(io.MultiWriter(&s.combined, &s.stderr))
	return cmd, s
}

// countingNotify records how many times the daemon notification fired and with
// which action, standing in for notifyDaemonAuthChange. It reports no daemon
// verdict, which is what a CLI with no daemon listening sees.
func countingNotify() (func(string) *pb.NotifyAuthChangeResponse, *int, *[]string) {
	return countingNotifyWith(nil)
}

// countingNotifyWith is countingNotify with a canned daemon verdict, for the
// cases that assert on what the CLI prints about it.
func countingNotifyWith(resp *pb.NotifyAuthChangeResponse) (func(string) *pb.NotifyAuthChangeResponse, *int, *[]string) {
	calls := 0
	actions := []string{}
	return func(action string) *pb.NotifyAuthChangeResponse {
		calls++
		actions = append(actions, action)
		return resp
	}, &calls, &actions
}

func TestRenderLoginVerdict_VerifiedNotifiesAndPrints(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, actions := countingNotify()

	verdict := auth.LoginVerification{Outcome: auth.LoginVerified, Email: "dave@example.com"}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notify); err != nil {
		t.Fatalf("renderLoginVerdict: %v", err)
	}

	if *calls != 1 {
		t.Fatalf("notify called %d times, want exactly 1", *calls)
	}
	if (*actions)[0] != "login" {
		t.Errorf("notify action = %q, want %q", (*actions)[0], "login")
	}
	if !strings.Contains(out.String(), "Logged in as dave@example.com") {
		t.Errorf("output missing the success line:\n%s", out.String())
	}
}

// With no email on the verdict the command still reports success, using the
// generic line rather than printing an empty address.
func TestRenderLoginVerdict_VerifiedWithoutEmail(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, _ := countingNotify()

	verdict := auth.LoginVerification{Outcome: auth.LoginVerified}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notify); err != nil {
		t.Fatalf("renderLoginVerdict: %v", err)
	}

	if *calls != 1 {
		t.Fatalf("notify called %d times, want exactly 1", *calls)
	}
	if !strings.Contains(out.String(), "Login successful!") {
		t.Errorf("output missing the generic success line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Logged in as") {
		t.Errorf("output printed an empty address:\n%s", out.String())
	}
}

// Nothing was stored, so the command must fail and must not claim success or
// wake the daemon into connecting with a credential that does not exist.
func TestRenderLoginVerdict_NotUpdatedFailsWithoutNotifying(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, _ := countingNotify()

	verdict := auth.LoginVerification{
		Outcome: auth.LoginVerifyRecordNotUpdated,
		Reason:  auth.LoginVerifyReasonRecordAbsent,
		Email:   "dave@example.com",
	}
	err := renderLoginVerdict(cmd, verdict, verdict.Email, notify)
	if err == nil {
		t.Fatal("renderLoginVerdict returned nil for a record_not_updated verdict")
	}
	if *calls != 0 {
		t.Errorf("notify called %d times, want 0", *calls)
	}
	if !strings.Contains(err.Error(), "boss auth-status") {
		t.Errorf("error must point at boss auth-status: %v", err)
	}
	for _, unwanted := range []string{"Logged in as", "Login successful!"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("output claimed success (%q):\n%s", unwanted, out.String())
		}
	}
}

// The write may have landed but could not be confirmed. That is not a failure
// exit, but it must not print a success line or notify the daemon, and the
// operator must be told how to get back to a known-good state.
func TestRenderLoginVerdict_InconclusiveExitsZeroWithRemediation(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, _ := countingNotify()

	verdict := auth.LoginVerification{
		Outcome: auth.LoginVerifyInconclusive,
		Reason:  auth.LoginVerifyReasonReadFailed,
		Email:   "dave@example.com",
	}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notify); err != nil {
		t.Fatalf("inconclusive verdict must exit zero, got: %v", err)
	}
	if *calls != 0 {
		t.Errorf("notify called %d times, want 0", *calls)
	}

	got := out.String()
	for _, want := range []string{"boss auth-status", "boss login", "restart the daemon"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Logged in as", "Login successful!"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output claimed success (%q):\n%s", unwanted, got)
		}
	}
}

// A zero verdict means verification never ran. Reporting success on it would
// reintroduce the bug this whole change exists to close.
func TestRenderLoginVerdict_UnsetVerdictIsNotSuccess(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, _ := countingNotify()

	err := renderLoginVerdict(cmd, auth.LoginVerification{}, "", notify)
	if err == nil {
		t.Fatal("an unset verdict must not be reported as a successful login")
	}
	if *calls != 0 {
		t.Errorf("notify called %d times, want 0", *calls)
	}
	if strings.Contains(out.String(), "Logged in as") || strings.Contains(out.String(), "Login successful!") {
		t.Errorf("output claimed success:\n%s", out.String())
	}
}

// Neither rendered surface may carry token material. Err is carried for
// errors.Is only; it must never reach the operator's terminal.
func TestRenderLoginVerdict_FailureMessagesCarryNoTokenMaterial(t *testing.T) {
	const secret = "sk-live-super-secret-token"

	for _, tc := range []struct {
		name    string
		verdict auth.LoginVerification
	}{
		{
			name: "not updated",
			verdict: auth.LoginVerification{
				Outcome: auth.LoginVerifyRecordNotUpdated,
				Reason:  auth.LoginVerifyReasonAccessTokenMismatch,
				Err:     errors.New("keyring: " + secret),
			},
		},
		{
			name: "inconclusive",
			verdict: auth.LoginVerification{
				Outcome: auth.LoginVerifyInconclusive,
				Reason:  auth.LoginVerifyReasonLockTimeout,
				Err:     errors.New("keyring: " + secret),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out := loginVerdictCmd(t)
			notify, _, _ := countingNotify()

			err := renderLoginVerdict(cmd, tc.verdict, "", notify)
			rendered := out.String()
			if err != nil {
				rendered += "\n" + err.Error()
			}
			if strings.Contains(rendered, secret) {
				t.Fatalf("rendered output leaked token material:\n%s", rendered)
			}
			if strings.Contains(rendered, "keyring: ") {
				t.Fatalf("rendered output printed the raw error:\n%s", rendered)
			}
		})
	}
}

// --- Daemon post-login verdict rendering (BOS-945) ---

// TestRenderDaemonLoginVerdict_Silence pins the cases that must produce NO
// output. Silence is the correct answer whenever the daemon did not actually
// deliver a verdict — an unknown outcome rendered as reassurance is exactly the
// failure BOS-945 exists to remove, only inverted.
func TestRenderDaemonLoginVerdict_Silence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp *pb.NotifyAuthChangeResponse
	}{
		{name: "no daemon answered", resp: nil},
		{name: "older daemon or no orchestrator", resp: &pb.NotifyAuthChangeResponse{}},
		{
			name: "explicit unspecified",
			resp: &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED},
		},
		{
			name: "everything worked",
			resp: &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_OUTCOME_OK},
		},
		{
			name: "an outcome this build does not know",
			resp: &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_Outcome(9999)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderDaemonLoginVerdict(tc.resp); got != "" {
				t.Errorf("renderDaemonLoginVerdict = %q, want empty", got)
			}
		})
	}
}

// TestRenderDaemonLoginVerdict_Warnings covers the outcomes that must say
// something, and asserts the three are distinguishable: the operator's next
// move differs for each (log in again / check the keyring backend / wait).
func TestRenderDaemonLoginVerdict_Warnings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		resp        *pb.NotifyAuthChangeResponse
		wantSubstrs []string
	}{
		{
			name: "flagged names the reason in operator language",
			resp: &pb.NotifyAuthChangeResponse{
				Outcome:       pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
				ReloginReason: auth.ReloginReasonRefreshTokenRejected,
			},
			wantSubstrs: []string{
				"Warning",
				auth.ReloginReasonDescription(auth.ReloginReasonRefreshTokenRejected),
				"boss auth-status",
			},
		},
		{
			name: "flagged with the other reason renders that reason",
			resp: &pb.NotifyAuthChangeResponse{
				Outcome:       pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
				ReloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
			},
			wantSubstrs: []string{auth.ReloginReasonDescription(auth.ReloginReasonRefreshOutcomeUnknown)},
		},
		{
			name: "missing points at the keyring backend",
			resp: &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING},
			wantSubstrs: []string{
				"Warning",
				"keyring backend",
			},
		},
		{
			name:        "register failure is informational, not a warning",
			resp:        &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED},
			wantSubstrs: []string{"retry in the background"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderDaemonLoginVerdict(tc.resp)
			if got == "" {
				t.Fatal("renderDaemonLoginVerdict returned nothing for an outcome that needs reporting")
			}
			for _, want := range tc.wantSubstrs {
				if !strings.Contains(got, want) {
					t.Errorf("output %q missing %q", got, want)
				}
			}
		})
	}
}

// The three reportable outcomes must not collapse into the same sentence: a
// user who cannot tell "log in again" from "the daemon reads a different
// keyring" from "it will retry" has learned nothing.
func TestRenderDaemonLoginVerdict_OutcomesAreDistinguishable(t *testing.T) {
	t.Parallel()

	flagged := renderDaemonLoginVerdict(&pb.NotifyAuthChangeResponse{
		Outcome:       pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
		ReloginReason: auth.ReloginReasonRefreshTokenRejected,
	})
	missing := renderDaemonLoginVerdict(&pb.NotifyAuthChangeResponse{
		Outcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING,
	})
	registerFailed := renderDaemonLoginVerdict(&pb.NotifyAuthChangeResponse{
		Outcome: pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED,
	})

	if flagged == missing || flagged == registerFailed || missing == registerFailed {
		t.Fatalf("outcomes render identically:\nflagged=%q\nmissing=%q\nregisterFailed=%q", flagged, missing, registerFailed)
	}
}

// TestRenderDaemonLoginVerdict_NeverLeaksCredentialMaterial is the negative
// guard the whole feature hangs on: this string is printed to a terminal and
// may be pasted into a bug report. Only the enumerated marker may influence it,
// and only through its human-readable description.
func TestRenderDaemonLoginVerdict_NeverLeaksCredentialMaterial(t *testing.T) {
	t.Parallel()

	const (
		accessToken  = "eyJhbGciOi-access-token-fixture"
		refreshToken = "refresh-token-fixture-9f8e7d"
	)

	// A hostile/buggy daemon putting token material where the reason belongs
	// must not get it echoed verbatim to the terminal.
	for _, outcome := range []pb.NotifyAuthChangeResponse_Outcome{
		pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED,
		pb.NotifyAuthChangeResponse_OUTCOME_OK,
		pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
		pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING,
		pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED,
	} {
		got := renderDaemonLoginVerdict(&pb.NotifyAuthChangeResponse{
			Outcome:       outcome,
			ReloginReason: accessToken,
		})
		for _, secret := range []string{accessToken, refreshToken, "access_token", "refresh_token"} {
			if strings.Contains(got, secret) {
				t.Fatalf("outcome %v leaked %q into the rendered output: %s", outcome, secret, got)
			}
		}
	}
}

// TestRenderLoginVerdict_PrintsTheDaemonWarningAfterSuccess is the end-to-end
// shape of the fix at the CLI seam: the login DID succeed, so the success line
// still prints and the command still exits zero — but the operator is told the
// daemon cannot use the credentials, instead of discovering it later from a
// daemon that silently never syncs.
func TestRenderLoginVerdict_PrintsTheDaemonWarningAfterSuccess(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, calls, _ := countingNotifyWith(&pb.NotifyAuthChangeResponse{
		Outcome:       pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
		ReloginReason: auth.ReloginReasonRefreshTokenRejected,
	})

	verdict := auth.LoginVerification{Outcome: auth.LoginVerified, Email: "dave@example.com"}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notify); err != nil {
		t.Fatalf("renderLoginVerdict: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("notify called %d times, want exactly 1", *calls)
	}

	got := out.String()
	successAt := strings.Index(got, "Logged in as dave@example.com")
	warningAt := strings.Index(got, "Warning")
	if successAt < 0 {
		t.Fatalf("output missing the success line:\n%s", got)
	}
	if warningAt < 0 {
		t.Fatalf("output missing the daemon warning:\n%s", got)
	}
	if warningAt < successAt {
		t.Errorf("daemon warning printed before the success line; it reads as a failed login:\n%s", got)
	}

	// The split is the point: `boss login >/dev/null` must still show the
	// warning, and a caller capturing stdout must not find it polluted.
	if !strings.Contains(out.stdout.String(), "Logged in as dave@example.com") {
		t.Errorf("success line did not go to stdout:\n%s", out.stdout.String())
	}
	if strings.Contains(out.stdout.String(), "Warning") {
		t.Errorf("daemon warning leaked into stdout:\n%s", out.stdout.String())
	}
	if !strings.Contains(out.stderr.String(), "Warning") {
		t.Errorf("daemon warning did not go to stderr:\n%s", out.stderr.String())
	}
	if strings.Contains(out.stderr.String(), "Logged in as") {
		t.Errorf("success line leaked into stderr:\n%s", out.stderr.String())
	}
	// The enumerated marker must arrive as prose, never as the raw token.
	if !strings.Contains(out.stderr.String(), auth.ReloginReasonDescription(auth.ReloginReasonRefreshTokenRejected)) {
		t.Errorf("warning missing the ReloginReasonDescription text:\n%s", out.stderr.String())
	}
	if !strings.Contains(out.stderr.String(), "boss auth-status") {
		t.Errorf("warning must point at boss auth-status:\n%s", out.stderr.String())
	}
}

// A daemon that reports OK adds nothing to the successful login output.
func TestRenderLoginVerdict_CleanDaemonVerdictAddsNoNoise(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	notify, _, _ := countingNotifyWith(&pb.NotifyAuthChangeResponse{
		Outcome: pb.NotifyAuthChangeResponse_OUTCOME_OK,
	})

	verdict := auth.LoginVerification{Outcome: auth.LoginVerified, Email: "dave@example.com"}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notify); err != nil {
		t.Fatalf("renderLoginVerdict: %v", err)
	}
	if got := out.String(); strings.Contains(got, "Warning") || strings.Contains(got, "Note:") {
		t.Errorf("a clean daemon verdict produced commentary:\n%s", got)
	}
}

// announceLoginSuccess must tolerate a nil notify hook — the seam tests inject
// one, and a nil hook is how callers say "no daemon to ask".
func TestAnnounceLoginSuccess_NilNotifyRendersNothing(t *testing.T) {
	cmd, _ := loginVerdictCmd(t)
	if got := announceLoginSuccess(cmd, "dave@example.com", nil); got != "" {
		t.Errorf("announceLoginSuccess with nil notify = %q, want empty", got)
	}
}

// stubDaemonAuthNotifier stands in for the local daemon client so the
// notifyDaemonAuthChange failure paths can be driven without a daemon.
type stubDaemonAuthNotifier struct {
	resp    *pb.NotifyAuthChangeResponse
	err     error
	actions []string
}

func (s *stubDaemonAuthNotifier) NotifyAuthChange(_ context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
	s.actions = append(s.actions, action)
	return s.resp, s.err
}

// withDaemonAuthNotifier swaps the socket-path resolver and client factory for
// the duration of one test, restoring both afterwards.
func withDaemonAuthNotifier(t *testing.T, socketPath string, socketErr error, stub *stubDaemonAuthNotifier) {
	t.Helper()
	origPath, origNew := daemonAuthSocketPath, newDaemonAuthNotifier
	t.Cleanup(func() {
		daemonAuthSocketPath, newDaemonAuthNotifier = origPath, origNew
	})
	daemonAuthSocketPath = func() (string, error) { return socketPath, socketErr }
	newDaemonAuthNotifier = func(string) daemonAuthNotifier { return stub }
}

// An unresolvable socket path means there is no daemon to ask. That is not a
// login failure, and it must not be dressed up as a verdict — nil is what
// renderDaemonLoginVerdict turns into silence.
func TestNotifyDaemonAuthChange_UnresolvableSocketReportsNoVerdict(t *testing.T) {
	stub := &stubDaemonAuthNotifier{resp: &pb.NotifyAuthChangeResponse{
		Outcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
	}}
	withDaemonAuthNotifier(t, "", errors.New("no socket path"), stub)

	if got := notifyDaemonAuthChange("login"); got != nil {
		t.Fatalf("notifyDaemonAuthChange = %v, want nil when the socket path cannot be resolved", got)
	}
	if len(stub.actions) != 0 {
		t.Fatalf("client was dialled anyway: %v", stub.actions)
	}
	if renderDaemonLoginVerdict(notifyDaemonAuthChange("login")) != "" {
		t.Fatal("a daemon that could not be reached must render nothing")
	}
}

// The daemon-not-running path: the RPC fails, and the CLI stays quiet rather
// than reporting a connection error the user did not ask about.
func TestNotifyDaemonAuthChange_RPCErrorReportsNoVerdict(t *testing.T) {
	stub := &stubDaemonAuthNotifier{
		resp: &pb.NotifyAuthChangeResponse{Outcome: pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING},
		err:  errors.New("dial unix: connect: connection refused"),
	}
	withDaemonAuthNotifier(t, "/tmp/bossd.sock", nil, stub)

	if got := notifyDaemonAuthChange("login"); got != nil {
		t.Fatalf("notifyDaemonAuthChange = %v, want nil when the RPC fails", got)
	}
	if len(stub.actions) != 1 || stub.actions[0] != "login" {
		t.Fatalf("actions = %v, want exactly [login]", stub.actions)
	}
	if renderDaemonLoginVerdict(notifyDaemonAuthChange("login")) != "" {
		t.Fatal("a failed NotifyAuthChange must render nothing")
	}
}

// And a reachable daemon's verdict is passed through untouched.
func TestNotifyDaemonAuthChange_PassesTheDaemonVerdictThrough(t *testing.T) {
	stub := &stubDaemonAuthNotifier{resp: &pb.NotifyAuthChangeResponse{
		Outcome:       pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED,
		ReloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	}}
	withDaemonAuthNotifier(t, "/tmp/bossd.sock", nil, stub)

	got := notifyDaemonAuthChange("login")
	if got == nil {
		t.Fatal("notifyDaemonAuthChange dropped a verdict the daemon gave")
	}
	if got.GetOutcome() != pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED {
		t.Fatalf("outcome = %v, want OUTCOME_CREDENTIALS_FLAGGED", got.GetOutcome())
	}
	if got.GetReloginReason() != auth.ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("relogin reason = %q, want %q", got.GetReloginReason(), auth.ReloginReasonRefreshOutcomeUnknown)
	}
}

// The whole point of the nil paths: a login against a machine with no daemon
// running still succeeds, still prints its success line, and adds no noise.
func TestRenderLoginVerdict_UnreachableDaemonStillSucceedsQuietly(t *testing.T) {
	cmd, out := loginVerdictCmd(t)
	withDaemonAuthNotifier(t, "/tmp/bossd.sock", nil, &stubDaemonAuthNotifier{
		err: errors.New("dial unix: connect: connection refused"),
	})

	verdict := auth.LoginVerification{Outcome: auth.LoginVerified, Email: "dave@example.com"}
	if err := renderLoginVerdict(cmd, verdict, verdict.Email, notifyDaemonAuthChange); err != nil {
		t.Fatalf("login failed because the daemon was not running: %v", err)
	}
	if !strings.Contains(out.stdout.String(), "Logged in as dave@example.com") {
		t.Errorf("output missing the success line:\n%s", out.stdout.String())
	}
	if out.stderr.String() != "" {
		t.Errorf("an unreachable daemon printed to stderr:\n%s", out.stderr.String())
	}
}

// The REGISTER_FAILED note is also what a daemon returns when it could not
// re-read the record and then failed to register with the stale cache, so the
// copy must not claim the credentials were accepted.
func TestRenderDaemonLoginVerdict_RegisterFailedClaimsNothingAboutCredentials(t *testing.T) {
	t.Parallel()

	got := renderDaemonLoginVerdict(&pb.NotifyAuthChangeResponse{
		Outcome: pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED,
	})
	if got == "" {
		t.Fatal("REGISTER_FAILED must render a note")
	}
	for _, claim := range []string{"accepted your credentials", "your credentials are", "credentials are valid"} {
		if strings.Contains(got, claim) {
			t.Errorf("register-failure note asserts %q, which nothing verified:\n%s", claim, got)
		}
	}
	if !strings.Contains(got, "re-register") {
		t.Errorf("register-failure note does not say what failed:\n%s", got)
	}
}
