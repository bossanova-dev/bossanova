package auth

import (
	"errors"
	"strings"
	"testing"
)

// The zero value must not be mistakable for a real verdict: it is the "never
// ran" sentinel, so Verified() is false and String() names it as unset.
func TestLoginVerifyOutcome_ZeroValueIsInvalid(t *testing.T) {
	var zero LoginVerifyOutcome
	if zero == LoginVerified || zero == LoginVerifyRecordNotUpdated || zero == LoginVerifyInconclusive {
		t.Fatalf("zero value collides with a real outcome: %d", zero)
	}
	if got := zero.String(); got != "unset" {
		t.Fatalf("zero String() = %q, want %q", got, "unset")
	}
	var v LoginVerification
	if v.Verified() {
		t.Fatal("zero LoginVerification must not report Verified")
	}
}

func TestLoginVerifyOutcome_String(t *testing.T) {
	cases := map[LoginVerifyOutcome]string{
		LoginVerified:               "verified",
		LoginVerifyRecordNotUpdated: "record_not_updated",
		LoginVerifyInconclusive:     "inconclusive",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("String() for %d = %q, want %q", outcome, got, want)
		}
	}
}

func TestLoginVerification_Verified(t *testing.T) {
	if !(LoginVerification{Outcome: LoginVerified}).Verified() {
		t.Error("LoginVerified must report Verified")
	}
	for _, outcome := range []LoginVerifyOutcome{LoginVerifyRecordNotUpdated, LoginVerifyInconclusive} {
		if (LoginVerification{Outcome: outcome}).Verified() {
			t.Errorf("%s must not report Verified", outcome)
		}
	}
}

// Every note is operator-facing text. It must name `boss auth-status` for the
// two failure outcomes, and the inconclusive note must additionally say how to
// get back to a known-good state (re-run login or restart the daemon).
func TestLoginVerification_NoteNamesAuthStatus(t *testing.T) {
	notUpdated := LoginVerification{
		Outcome: LoginVerifyRecordNotUpdated,
		Reason:  LoginVerifyReasonRecordAbsent,
	}.Note()
	if !strings.Contains(notUpdated, "boss auth-status") {
		t.Errorf("not-updated note must name boss auth-status: %q", notUpdated)
	}
	if !strings.Contains(notUpdated, "no credential record was found") {
		t.Errorf("not-updated note must describe the reason: %q", notUpdated)
	}

	inconclusive := LoginVerification{
		Outcome: LoginVerifyInconclusive,
		Reason:  LoginVerifyReasonReadFailed,
	}.Note()
	if !strings.Contains(inconclusive, "boss auth-status") {
		t.Errorf("inconclusive note must name boss auth-status: %q", inconclusive)
	}
	if !strings.Contains(inconclusive, "boss login") {
		t.Errorf("inconclusive note must tell the operator to re-run boss login: %q", inconclusive)
	}
	if !strings.Contains(inconclusive, "restart the daemon") {
		t.Errorf("inconclusive note must offer the daemon restart remedy: %q", inconclusive)
	}

	var unset LoginVerification
	if !strings.Contains(unset.Note(), "boss auth-status") {
		t.Errorf("unset note must still point at boss auth-status: %q", unset.Note())
	}
}

// Every reason in the closed vocabulary must render distinct, non-empty prose
// so an operator can tell the failures apart without a token in sight.
func TestLoginVerification_NoteCoversEveryReason(t *testing.T) {
	reasons := []string{
		LoginVerifyReasonRecordAbsent,
		LoginVerifyReasonNoAccessToken,
		LoginVerifyReasonNoRefreshToken,
		LoginVerifyReasonAccessTokenMismatch,
		ReloginReasonRefreshOutcomeUnknown,
		ReloginReasonRefreshTokenRejected,
	}
	seen := make(map[string]string, len(reasons))
	for _, reason := range reasons {
		note := LoginVerification{Outcome: LoginVerifyRecordNotUpdated, Reason: reason}.Note()
		if note == "" {
			t.Fatalf("reason %q produced an empty note", reason)
		}
		if prev, dup := seen[note]; dup {
			t.Errorf("reason %q renders identically to %q", reason, prev)
		}
		seen[note] = reason
	}
}

// The note is assembled from the enumerated Reason only. Err may wrap a
// keyring error whose text embeds record bytes, so it must never reach a
// rendered string — it exists for errors.Is alone.
func TestLoginVerification_NoteNeverLeaksSecrets(t *testing.T) {
	secret := "sk-super-secret-token-material"
	v := LoginVerification{
		Outcome: LoginVerifyInconclusive,
		Reason:  LoginVerifyReasonReadFailed,
		Email:   "user@example.com",
		Err:     errors.New("keyring read failed: " + secret),
	}
	note := v.Note()
	if strings.Contains(note, secret) {
		t.Fatalf("note leaked Err text: %q", note)
	}
	if strings.Contains(note, "keyring read failed") {
		t.Fatalf("note rendered the raw error: %q", note)
	}
	if !errors.Is(v.Err, v.Err) {
		t.Fatal("Err must remain inspectable by callers")
	}
}

// A sentinel wrapped into Err stays unwrappable, which is the whole reason the
// field exists.
func TestLoginVerification_ErrRemainsUnwrappable(t *testing.T) {
	v := LoginVerification{
		Outcome: LoginVerifyInconclusive,
		Reason:  LoginVerifyReasonReadFailed,
		Err:     errors.New("wrapped: " + ErrCredentialsUnreadable.Error()),
	}
	v.Err = errors.Join(ErrCredentialsUnreadable, v.Err)
	if !errors.Is(v.Err, ErrCredentialsUnreadable) {
		t.Fatal("errors.Is must reach the wrapped sentinel")
	}
	if strings.Contains(v.Note(), ErrCredentialsUnreadable.Error()) {
		t.Fatal("Note must not render the sentinel text")
	}
}
