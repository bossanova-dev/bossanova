package auth

// Login verification (BOS-942/BOS-943).
//
// `boss login` used to report success on the strength of a WorkOS exchange
// alone: the keychain write was fire-and-forget, so a save that silently did
// nothing still printed "Logged in as ..." and still told the daemon to reload
// credentials that were never there. This file declares the verdict the save
// path now produces by reading the record back inside the same credential lock
// it just wrote under.
//
// Nothing in this file may carry token material. `Reason` is drawn from a
// closed set of enumerated, non-secret strings, and `Err` exists so callers can
// `errors.Is` it — never so a surface can print it.

// LoginVerifyOutcome is the three-way verdict of the post-save read-back.
//
// The constants start at 1 so the zero value is an invalid "verification never
// ran" sentinel, distinct from all three real outcomes. A caller that forgets
// to set an outcome therefore cannot be mistaken for a verified login.
type LoginVerifyOutcome int

const (
	// LoginVerified means the credential record was read back after the save
	// and is present, unflagged, and usable.
	LoginVerified LoginVerifyOutcome = iota + 1
	// LoginVerifyRecordNotUpdated means the read-back succeeded and proved the
	// record does not reflect the login that just happened: absent, still
	// flagged for re-login, missing a token, or holding a different access
	// token than the one just saved. The login did not take effect.
	LoginVerifyRecordNotUpdated
	// LoginVerifyInconclusive means the read-back itself could not be
	// completed — an unreadable record, a lock that could not be acquired in
	// time, or a cancelled context. The login may well have persisted; we
	// simply cannot say.
	LoginVerifyInconclusive
)

// String renders the outcome as the enumerated, non-secret token used in logs
// and in structured instrumentation.
func (o LoginVerifyOutcome) String() string {
	switch o {
	case LoginVerified:
		return "verified"
	case LoginVerifyRecordNotUpdated:
		return "record_not_updated"
	case LoginVerifyInconclusive:
		return "inconclusive"
	default:
		return "unset"
	}
}

// Enumerated, non-secret reasons for a LoginVerifyRecordNotUpdated verdict.
// A record that came back still flagged for re-login reports the persisted
// ReloginReason* value instead, so the full reason vocabulary is these four
// plus the ReloginReason* constants in tokenstore.go.
const (
	// LoginVerifyReasonRecordAbsent means the read-back found no record at all
	// where one had just been written.
	LoginVerifyReasonRecordAbsent = "record_absent"
	// LoginVerifyReasonNoAccessToken means the persisted record carries no
	// access token.
	LoginVerifyReasonNoAccessToken = "no_access_token"
	// LoginVerifyReasonNoRefreshToken means the persisted record carries no
	// refresh token, so the daemon could never refresh it.
	LoginVerifyReasonNoRefreshToken = "no_refresh_token"
	// LoginVerifyReasonAccessTokenMismatch means the persisted record holds an
	// access token other than the one this login just saved — a stale record
	// survived the write.
	LoginVerifyReasonAccessTokenMismatch = "access_token_mismatch"
)

// Enumerated, non-secret reasons for a LoginVerifyInconclusive verdict.
const (
	// LoginVerifyReasonReadFailed means the read-back returned an error that
	// was not "no record stored" — typically an unreadable/undecryptable
	// record.
	LoginVerifyReasonReadFailed = "read_failed"
	// LoginVerifyReasonLockTimeout means the credential lock could not be held
	// long enough to complete the read-back.
	LoginVerifyReasonLockTimeout = "lock_timeout"
)

// LoginVerification is the verdict a login save path returns.
//
// Outcome is always one of the three constants above. Reason is an enumerated,
// non-secret string, empty only for LoginVerified. Email is the address on the
// record that was read back, carried out so callers need not re-read the store
// to greet the user. Err is non-nil only for LoginVerifyInconclusive and is
// there to be unwrapped with errors.Is, never to be printed: the underlying
// keyring error may embed record bytes.
type LoginVerification struct {
	Outcome LoginVerifyOutcome
	Reason  string
	Email   string
	Err     error
}

// Verified reports whether the login is confirmed to have persisted. It is the
// single gate every surface uses before announcing success or telling the
// daemon to reload credentials.
func (v LoginVerification) Verified() bool {
	return v.Outcome == LoginVerified
}

// reasonDescription renders an enumerated reason as short, non-secret prose.
func reasonDescription(reason string) string {
	switch reason {
	case LoginVerifyReasonRecordAbsent:
		return "no credential record was found after saving"
	case LoginVerifyReasonNoAccessToken:
		return "the saved record has no access token"
	case LoginVerifyReasonNoRefreshToken:
		return "the saved record has no refresh token"
	case LoginVerifyReasonAccessTokenMismatch:
		return "the saved record does not match the new sign-in"
	case ReloginReasonRefreshOutcomeUnknown, ReloginReasonRefreshTokenRejected:
		return "the saved record is still flagged for re-login because " +
			ReloginReasonDescription(reason)
	case LoginVerifyReasonReadFailed:
		return "the credential record could not be read back"
	case LoginVerifyReasonLockTimeout:
		return "the credential lock could not be held long enough to check"
	case "":
		return "the credential record could not be confirmed"
	default:
		return "the credential record could not be confirmed"
	}
}

// Note is the short operator-facing text each surface renders for a verdict.
// It never contains token material: it is built only from the enumerated
// Reason, never from Err.
//
// Both the not-updated and the inconclusive notes name `boss auth-status`, the
// command that shows what is actually stored. The inconclusive note also tells
// the operator how to get back to a known-good state, because a stale daemon
// cache is the failure that outlives the command.
func (v LoginVerification) Note() string {
	switch v.Outcome {
	case LoginVerified:
		return "Credentials saved and verified."
	case LoginVerifyRecordNotUpdated:
		return "credentials were not saved — " + reasonDescription(v.Reason) +
			". Run boss auth-status to see what is stored, then run boss login again."
	case LoginVerifyInconclusive:
		return "credentials were saved but could not be confirmed — " +
			reasonDescription(v.Reason) +
			". Run boss auth-status to check, and if it still reports a sign-in is " +
			"needed, run boss login again or restart the daemon."
	default:
		return "credentials were not checked after saving. Run boss auth-status to see what is stored."
	}
}
