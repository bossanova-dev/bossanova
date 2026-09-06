package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/recurser/bossalib/agenterr"
)

// injection.go carries the typed outcome BOS-1142 introduced when the resolver
// stopped degrading a bound account's failed credential injection to "account 0"
// (the agent CLI's ambient login).
//
// The classification is produced HERE, by the layer that actually knows why the
// injection failed, and read by callers through IsInjectionInvalid /
// IsInjectionUndetermined. No caller may re-derive it by matching on error text:
// a fail-closed guard that cannot tell "this credential is unusable" from "I
// could not evaluate this credential" reports the second as the first, which is
// precisely the failure BOS-881 documented.

// InjectionOutcome classifies a refusal to produce a bound account's spawn
// environment.
type InjectionOutcome string

const (
	// InjectionOutcomeInvalid means the account cannot serve this spawn: the
	// binding names an account that does not exist, the provider confirmed the
	// stored credential is unusable, the provider's plugin cannot materialize
	// credentials at all, or materialization failed for a credential-shaped
	// reason. The operator remedy is to re-authenticate or re-bind — retrying
	// unchanged will not help.
	//
	// A materialization failure is NOT automatically credential-shaped. When the
	// call never reached a verdict — the plugin was mid-restart, the RPC timed
	// out, the context was cancelled — the credential was never evaluated, and
	// that is InjectionOutcomeUndetermined below. See isUndeterminedCause.
	InjectionOutcomeInvalid InjectionOutcome = "invalid"

	// InjectionOutcomeUndetermined means the resolver could not evaluate the
	// binding: a dependency it needs was not wired, or an infrastructure call it
	// depends on failed. It is NOT evidence the credential is bad. It still
	// fails the spawn closed — a spawn that cannot prove it is running under the
	// account it was bound to must not run — but it must never be reported to an
	// operator as a credential problem, and it must never be recorded as durable
	// auth-invalid state.
	InjectionOutcomeUndetermined InjectionOutcome = "undetermined"
)

// ErrInjectionUndetermined marks a cause as infrastructure-shaped: the call that
// failed never reached a verdict about the credential, so a refusal built from
// it is undetermined rather than invalid.
//
// It exists because the layer that can READ that distinction is not this one.
// Deciding that a materialization failure was a transport failure means reading
// a gRPC status code, and this package is a pure library with no grpc
// dependency (see the package doc). So the wiring layer, which already holds the
// transport, wraps such causes with this sentinel on the way out, and
// isUndeterminedCause reads it back here — the classification still being
// produced by the layer that knows why, exactly as this file requires.
var ErrInjectionUndetermined = errors.New("credential could not be evaluated")

// errEmptyMaterialization is the cause recorded when the materializer returns a
// nil error and no environment. It exists because recordInjectionFailure stores
// cause.Error() and there is no underlying error to store: the defect is
// precisely that nothing failed loudly. Unexported — no caller outside this
// package should branch on it, and the operator-facing classification is
// carried by the InjectionError's Outcome, not by this text.
var errEmptyMaterialization = errors.New("materializer returned no environment")

// isUndeterminedCause reports whether err describes a call that never reached a
// verdict, rather than a verdict of "unusable".
//
// Context cancellation and deadline expiry are listed alongside the sentinel
// because they can be raised by the caller's own context without ever crossing
// the wiring layer that would have wrapped them.
func isUndeterminedCause(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrInjectionUndetermined) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// InjectionError is the error every fail-closed site in resolveSpawnEnv
// returns. AccountID/Provider name the binding that was refused; Outcome is the
// typed classification; Err is the underlying cause when one exists.
type InjectionError struct {
	AccountID string
	Provider  string
	Outcome   InjectionOutcome
	Reason    string
	Err       error
}

func (e *InjectionError) Error() string {
	msg := e.Reason
	if msg == "" {
		msg = "credential injection failed"
	}
	if e.AccountID != "" {
		msg = fmt.Sprintf("account %q: %s", e.AccountID, msg)
	}
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

func (e *InjectionError) Unwrap() error { return e.Err }

// Redacted is the operator-facing form of the refusal: the same message with
// known secret shapes masked through agenterr.Redact, the single masking path
// in this codebase.
//
// Error() stays raw on purpose — it feeds the daemon log and errors.Is
// unwrapping, and the materialize error it wraps can embed provider response
// bodies. Anything that becomes an RPC message or reaches a screen uses this
// instead. Note the two are not interchangeable: never compare against
// Redacted() and never parse it, because a redaction sentinel is not stable
// text to branch on. Branch on Outcome.
func (e *InjectionError) Redacted() string {
	return string(agenterr.Redact([]byte(e.Error())))
}

// RedactedMessage returns the masked operator-facing message for err when it is
// a typed injection refusal, and the masked plain error text otherwise. Callers
// at an RPC or render boundary use it so a provider's raw failure body never
// leaves the daemon verbatim.
func RedactedMessage(err error) string {
	if err == nil {
		return ""
	}
	if ie, ok := AsInjectionError(err); ok {
		return ie.Redacted()
	}
	return string(agenterr.Redact([]byte(err.Error())))
}

// invalidInjection builds an InjectionOutcomeInvalid refusal.
func invalidInjection(accountID, provider, reason string, cause error) *InjectionError {
	return &InjectionError{
		AccountID: accountID,
		Provider:  provider,
		Outcome:   InjectionOutcomeInvalid,
		Reason:    reason,
		Err:       cause,
	}
}

// undeterminedInjection builds an InjectionOutcomeUndetermined refusal.
func undeterminedInjection(accountID, provider, reason string, cause error) *InjectionError {
	return &InjectionError{
		AccountID: accountID,
		Provider:  provider,
		Outcome:   InjectionOutcomeUndetermined,
		Reason:    reason,
		Err:       cause,
	}
}

// AsInjectionError extracts the typed refusal from err, if any.
func AsInjectionError(err error) (*InjectionError, bool) {
	var ie *InjectionError
	if errors.As(err, &ie) {
		return ie, true
	}
	return nil, false
}

// IsInjectionInvalid reports whether err is a refusal that names the account's
// credential as the problem.
func IsInjectionInvalid(err error) bool {
	ie, ok := AsInjectionError(err)
	return ok && ie.Outcome == InjectionOutcomeInvalid
}

// IsInjectionUndetermined reports whether err is a refusal that declines to
// answer the credential question at all.
func IsInjectionUndetermined(err error) bool {
	ie, ok := AsInjectionError(err)
	return ok && ie.Outcome == InjectionOutcomeUndetermined
}

// InjectionOutcomeOf returns the classification carried by err, or the empty
// outcome when err is not a typed injection refusal.
func InjectionOutcomeOf(err error) InjectionOutcome {
	if ie, ok := AsInjectionError(err); ok {
		return ie.Outcome
	}
	return ""
}
