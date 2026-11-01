package credmaterialize

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RefreshRemedy is the closed set of responses an operator (or Codex) owes a
// failed brokered refresh. One fixed 503 used to cover all three, which hid
// opposite remedies behind one sentence (BOS-2234).
type RefreshRemedy string

// The three remedy classes.
const (
	RefreshRemedyTransient      RefreshRemedy = "transient"
	RefreshRemedyOperatorAction RefreshRemedy = "operator_action"
	RefreshRemedyReauthenticate RefreshRemedy = "reauthenticate"
)

// RefreshFailureClass is a closed, redacted token naming why a brokered refresh
// failed. It carries no error text, so it is safe in a response body, a log
// field and the durable auth-check row.
//
// These tokens reach AuthCheck.failure_class on transient checks without a
// dated apiversion bump; the skipped-bump justification
// (docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff) sits on
// withRefreshFailureClass in services/bossd/internal/accountwiring/refresh_failure_class.go.
type RefreshFailureClass string

// Transient classes: a retry is the right response.
const (
	RefreshFailureOwnedByPeer       RefreshFailureClass = "refresh_owned_by_peer"
	RefreshFailureSyncUnreachable   RefreshFailureClass = "refresh_sync_unreachable"
	RefreshFailureCredentialChanged RefreshFailureClass = "refresh_credential_changed"
	// RefreshFailureUnclassified is the default. Unrecognised errors are
	// transient on purpose: a wrong "transient" costs a retry, while a wrong
	// condemnation costs an operator action.
	RefreshFailureUnclassified  RefreshFailureClass = "refresh_unclassified"
	RefreshFailureBrokerStopped RefreshFailureClass = "refresh_broker_stopped"
)

// Operator-action classes: nothing clears without someone fixing the device.
const (
	RefreshFailureIdentityNotPinned RefreshFailureClass = "credential_identity_not_pinned"
	RefreshFailureDeviceNotEnrolled RefreshFailureClass = "credential_device_not_enrolled"
	RefreshFailureSyncUnavailable   RefreshFailureClass = "refresh_sync_unavailable"
	RefreshFailureJournalUnreadable RefreshFailureClass = "refresh_journal_unreadable"
	RefreshFailureRequestInvalid    RefreshFailureClass = "refresh_request_invalid"
	RefreshFailureCapabilityInvalid RefreshFailureClass = "refresh_capability_invalid"
)

// Reauthenticate classes: the stored refresh chain cannot be spent again.
const (
	RefreshFailureOutcomeUnknown   RefreshFailureClass = "refresh_outcome_unknown"
	RefreshFailureSecretConsumed   RefreshFailureClass = "refresh_secret_consumed"
	RefreshFailureRejected         RefreshFailureClass = "refresh_rejected"
	RefreshFailureTokenUnavailable RefreshFailureClass = "refresh_token_unavailable"
)

var refreshFailureRemedies = map[RefreshFailureClass]RefreshRemedy{
	RefreshFailureOwnedByPeer:       RefreshRemedyTransient,
	RefreshFailureSyncUnreachable:   RefreshRemedyTransient,
	RefreshFailureCredentialChanged: RefreshRemedyTransient,
	RefreshFailureUnclassified:      RefreshRemedyTransient,
	RefreshFailureBrokerStopped:     RefreshRemedyTransient,
	RefreshFailureIdentityNotPinned: RefreshRemedyOperatorAction,
	RefreshFailureDeviceNotEnrolled: RefreshRemedyOperatorAction,
	RefreshFailureSyncUnavailable:   RefreshRemedyOperatorAction,
	RefreshFailureJournalUnreadable: RefreshRemedyOperatorAction,
	RefreshFailureRequestInvalid:    RefreshRemedyOperatorAction,
	RefreshFailureCapabilityInvalid: RefreshRemedyOperatorAction,
	RefreshFailureOutcomeUnknown:    RefreshRemedyReauthenticate,
	RefreshFailureSecretConsumed:    RefreshRemedyReauthenticate,
	RefreshFailureRejected:          RefreshRemedyReauthenticate,
	RefreshFailureTokenUnavailable:  RefreshRemedyReauthenticate,
}

// Remedy maps the class onto its remedy; an unknown class is transient.
func (c RefreshFailureClass) Remedy() RefreshRemedy {
	if remedy, ok := refreshFailureRemedies[c]; ok {
		return remedy
	}
	return RefreshRemedyTransient
}

// Broker failures, kept as sentinels so ClassifyRefreshError can use errors.Is.
// Their texts are unchanged from the inline errors they replace.
var (
	errRefreshJournalUnreadable        = errors.New("credential refresh journal unreadable")
	errRefreshTokenUnavailable         = errors.New("credential refresh token unavailable")
	errRefreshOutcomeUnknown           = errors.New("credential refresh outcome unknown; reauthenticate")
	errExchangeOutcomeUnknown          = errors.New("credential exchange outcome unknown")
	errRefreshSecretConsumed           = errors.New("credential refresh secret already consumed; reauthenticate")
	errExchangeRejected                = errors.New("credential exchange unsuccessful; reauthenticate")
	errCredentialChangedWhileAcquiring = errors.New("credential changed while acquiring refresh ownership")
	errCredentialReplacedWhilePending  = errors.New("credential replaced while refresh was pending")
)

var refreshSentinelClasses = []struct {
	err   error
	class RefreshFailureClass
}{
	{ErrRefreshOwned, RefreshFailureOwnedByPeer},
	{errRefreshJournalUnreadable, RefreshFailureJournalUnreadable},
	{errRefreshTokenUnavailable, RefreshFailureTokenUnavailable},
	{errRefreshOutcomeUnknown, RefreshFailureOutcomeUnknown},
	{errExchangeOutcomeUnknown, RefreshFailureOutcomeUnknown},
	{errRefreshSecretConsumed, RefreshFailureSecretConsumed},
	{errExchangeRejected, RefreshFailureRejected},
	{errCredentialChangedWhileAcquiring, RefreshFailureCredentialChanged},
	{errCredentialReplacedWhilePending, RefreshFailureCredentialChanged},
	{context.DeadlineExceeded, RefreshFailureSyncUnreachable},
}

// RefreshFailure attaches a class that only the gate can see (for example a
// credsync sentinel) to an error the broker would otherwise call unclassified.
type RefreshFailure struct {
	Class RefreshFailureClass
	err   error
}

// NewRefreshFailure classifies err as class. A nil err stays nil.
func NewRefreshFailure(class RefreshFailureClass, err error) error {
	if err == nil {
		return nil
	}
	return &RefreshFailure{Class: class, err: err}
}

func (f *RefreshFailure) Error() string { return f.err.Error() }

// Unwrap exposes the classified error to errors.Is and errors.As.
func (f *RefreshFailure) Unwrap() error { return f.err }

// ClassifyRefreshError maps err onto the closed class set: a gate-supplied
// class first, then the broker's own sentinels, then unclassified.
func ClassifyRefreshError(err error) RefreshFailureClass {
	var failure *RefreshFailure
	if errors.As(err, &failure) {
		return failure.Class
	}
	for _, entry := range refreshSentinelClasses {
		if errors.Is(err, entry.err) {
			return entry.class
		}
	}
	return RefreshFailureUnclassified
}

// refreshFailureBody names the remedy and class Codex prints. It is built only
// from closed-set tokens and the account id, never from error text.
func refreshFailureBody(class RefreshFailureClass, accountID string) string {
	return fmt.Sprintf("credential refresh unavailable [%s: %s]; %s", class.Remedy(), class, refreshFailureHint(class, accountID))
}

func refreshFailureHint(class RefreshFailureClass, accountID string) string {
	switch {
	case class == RefreshFailureDeviceNotEnrolled:
		return "run `boss account devices approve` on an enrolled machine"
	case class == RefreshFailureIdentityNotPinned:
		return "confirm the bootstrap fingerprint with `boss account devices approve`"
	case class.Remedy() == RefreshRemedyOperatorAction:
		return "check `boss account devices ls`"
	case class.Remedy() == RefreshRemedyReauthenticate:
		return "run `boss account reauth " + accountID + "`"
	default:
		return "retry shortly"
	}
}

type refreshFailureRecord struct {
	class RefreshFailureClass
	at    time.Time
}

func (b *RefreshBroker) recordFailure(accountID string, class RefreshFailureClass) {
	b.failuresMu.Lock()
	defer b.failuresMu.Unlock()
	b.failures[accountID] = refreshFailureRecord{class: class, at: b.now()}
}

func (b *RefreshBroker) clearFailure(accountID string) {
	b.failuresMu.Lock()
	defer b.failuresMu.Unlock()
	delete(b.failures, accountID)
}

// RefreshFailureSince reports the last refresh failure class recorded for
// accountID at or after since. A later successful refresh clears it.
func (b *RefreshBroker) RefreshFailureSince(accountID string, since time.Time) (RefreshFailureClass, bool) {
	if b == nil {
		return "", false
	}
	b.failuresMu.Lock()
	defer b.failuresMu.Unlock()
	record, ok := b.failures[accountID]
	if !ok || record.at.Before(since) {
		return "", false
	}
	return record.class, true
}

// refreshRejection is one non-2xx answer. accountID is set only once the
// capability authenticated; claimedID only when it passed validateAccountID.
type refreshRejection struct {
	status    int
	class     RefreshFailureClass
	accountID string
	claimedID string
	body      string
	err       error
}

// respondFailure is the single path every non-2xx response takes. It logs the
// failure exactly once at WARN. It never logs the capability, the request body
// or credential bytes: err is a fixed broker/credsync string or a connect status.
func (b *RefreshBroker) respondFailure(w http.ResponseWriter, f refreshRejection) {
	event := b.logger.Warn().
		Int("status", f.status).
		Str("refresh_failure_class", string(f.class)).
		Str("refresh_remedy", string(f.class.Remedy()))
	switch {
	case f.accountID != "":
		event = event.Str("account_id", f.accountID)
	case f.claimedID != "":
		event = event.Str("claimed_account_id", f.claimedID)
	}
	if f.err != nil {
		event = event.Err(f.err)
	}
	event.Msg("credential refresh failed")
	http.Error(w, f.body, f.status)
}

// refreshFailed answers a refresh failure with its remedy and records the class
// for RefreshFailureSince. Every refresh failure stays a 503: changing the
// status would change how Codex treats its own auth state.
func (b *RefreshBroker) refreshFailed(w http.ResponseWriter, accountID string, err error) {
	class := ClassifyRefreshError(err)
	b.recordFailure(accountID, class)
	b.respondFailure(w, refreshRejection{status: http.StatusServiceUnavailable, class: class, accountID: accountID, body: refreshFailureBody(class, accountID), err: err})
}

// storedJournal reads the journal that authenticates a capability. It never
// creates state for an attacker-chosen account, so an absent journal is an
// invalid capability. A store error or a corrupt journal is daemon-side state
// no caller controls, so it stays classifiable as journal-unreadable.
func (b *RefreshBroker) storedJournal(ctx context.Context, accountID string) (refreshJournal, error) {
	var j refreshJournal
	raw, err := b.store.LoadRefreshState(ctx, accountID)
	switch {
	case err != nil:
		return j, fmt.Errorf("%w: %w", errRefreshJournalUnreadable, err)
	case len(raw) == 0:
		return j, errors.New("invalid capability")
	case json.Unmarshal(raw, &j) != nil || len(j.Seed) != 32:
		return refreshJournal{}, errRefreshJournalUnreadable
	}
	return j, nil
}

// authenticationFailed answers a capability that did not authenticate. A
// journal that cannot be read fails every refresh for its account, so it is
// answered and recorded like a refresh failure for the account the capability
// names. Every other reason stays a 401 that records nothing.
func (b *RefreshBroker) authenticationFailed(w http.ResponseWriter, capability string, err error) {
	claimed := claimedAccountID(capability)
	if claimed != "" && errors.Is(err, errRefreshJournalUnreadable) {
		class := ClassifyRefreshError(err)
		b.recordFailure(claimed, class)
		b.respondFailure(w, refreshRejection{status: http.StatusServiceUnavailable, class: class, claimedID: claimed, body: refreshFailureBody(class, claimed), err: err})
		return
	}
	b.respondFailure(w, refreshRejection{status: http.StatusUnauthorized, class: RefreshFailureCapabilityInvalid, claimedID: claimed, body: "invalid refresh capability"})
}

// claimedAccountID decodes the account a rejected capability names, without
// trusting it, so the WARN line can say which account a forged or stale
// capability claimed. Only an id that passes validateAccountID is returned.
func claimedAccountID(capability string) string {
	payload, _, ok := strings.Cut(strings.TrimPrefix(capability, brokerCapabilityPrefix), ".")
	if !ok || !strings.HasPrefix(capability, brokerCapabilityPrefix) {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var c refreshCapability
	if err != nil || json.Unmarshal(raw, &c) != nil || validateAccountID(c.AccountID) != nil {
		return ""
	}
	return c.AccountID
}
