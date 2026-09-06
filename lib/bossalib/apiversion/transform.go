package apiversion

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// Account labels involved in UnmanagedLabelChange (V20260706). They are the raw
// literals rather than an import of services/bossd's account package: lib packages
// must not depend on a service's internal packages, so the down-convert pins the
// exact wire strings both sides emit.
const (
	// unmanagedLocalCredentialsLabel is the CURRENT (V20260706+) label the
	// OrchestratorService serves for a session unbound from any rotation account.
	// It mirrors account.UnmanagedLocalCredentialsLabel in services/bossd.
	unmanagedLocalCredentialsLabel = "Unmanaged local credentials"
	// systemDefaultAccountLabel is the PRIOR label older clients were built to
	// render for the unbound case; the transform restores it for them.
	systemDefaultAccountLabel = "System default"
)

// VersionChange describes a behavioral change associated with a specific API
// version. It down-converts a current-shape response to the behavior of the
// previous version for the RPC methods it targets; it must be a no-op for all
// other methods and message types.
//
// Real transforms in future PRs will type-assert msg against generated
// bossanova.v1 protobuf messages. The ReferenceChange in this file uses a
// simple demo struct to keep this package self-contained.
type VersionChange interface {
	// Version returns the API version at which this behavior change was
	// introduced. The transform is applied to any request resolved to a version
	// strictly older than Version().
	Version() Version

	// TransformResponse mutates msg in place to reflect the response shape of
	// the version prior to this change. method is the Connect RPC procedure
	// path (e.g. "/bossanova.v1.OrchestratorService/ListSessions").
	// Implementations must be no-ops for methods and message types they do not
	// target.
	TransformResponse(method string, msg any)
}

// ErrorTransform is an OPTIONAL capability a VersionChange may implement when
// the behavior it versions lives on the ERROR path rather than in a response
// message. A change that does not implement it is simply skipped by ApplyError,
// exactly as a change that targets no method is a no-op in TransformResponse.
//
// NOTE THE DELIBERATE SHAPE ASYMMETRY with TransformResponse, which takes msg
// any, returns nothing, and mutates the response in place. An error is an
// immutable value: there is no in-place edit that turns a CodeDeadlineExceeded
// into a CodeAborted. So the error capability must RETURN the replacement and
// ApplyError must thread it through the fan-out. Contorting either side to
// match the other would be worse than documenting the difference — making
// TransformResponse return a message would break every existing in-place
// transform, and pretending an error can be mutated in place would not work at
// all.
//
// SCOPE — UNARY ONLY. ApplyError is called from WrapUnary and from nowhere
// else. WrapStreamingHandler wraps the conn for Send-side RESPONSE transforms
// and returns the handler's error completely raw, and
// transformingStreamingHandlerConn has no error hook at all. So an
// ErrorTransform written for a streaming procedure is a silent no-op: no
// compile error, no failing test, no warning. That is a deliberate boundary
// (BOS-947 versioned a unary procedure and did not widen the interceptor), not
// an oversight — but it is invisible from this interface, which is why it is
// stated here rather than only in the plan that chose it.
// TestApplyError_StreamingHandlerErrorIsNotTransformed pins it, so extending
// the seam to streaming means deleting that test on purpose.
//
// Implementations must return err UNCHANGED for methods and error shapes they
// do not target, must tolerate a nil err, and must not turn a non-nil error
// into nil. Restoring a legacy success requires ErrorRecoveryTransform so the
// recovery can validate the response it would serve.
type ErrorTransform interface {
	// TransformError returns the error to serve in place of err for a client
	// resolved to the version prior to this change, or err unchanged when this
	// change does not target it. method is the Connect RPC procedure path.
	TransformError(method string, err error) error
}

// ErrorRecoveryTransform is the explicit, response-aware capability for a
// version change that restores a legacy success from a current error. Unlike
// ErrorTransform, it receives the concrete response message that WrapUnary
// would serve, so an implementation can require its expected response type
// before returning nil. Implementations must return err unchanged unless both
// the error and response are complete, producer-owned shapes for the method.
type ErrorRecoveryTransform interface {
	RecoverError(method string, response any, err error) error
}

// Changes is an ordered list of VersionChanges (oldest→newest) that can be
// applied to a response message for a given resolved API version.
type Changes struct {
	reg     *Registry
	changes []VersionChange
}

// NewChanges constructs a Changes, validating that:
//   - every change's Version() is a member of reg
//   - changes are in non-decreasing version order (oldest→newest)
func NewChanges(reg *Registry, changes ...VersionChange) (*Changes, error) {
	// Track the previous element via a variable rather than re-indexing
	// changes[i-1] so gosec's G602 bounds analysis stays happy (mirrors the
	// same workaround in NewRegistry).
	var prev Version
	for i, ch := range changes {
		v := ch.Version()
		if !reg.IsSupported(v) {
			return nil, fmt.Errorf("apiversion: change version %q is not in the registry", v)
		}
		if i > 0 {
			if string(v) < string(prev) {
				return nil, fmt.Errorf("apiversion: changes must be in non-decreasing version order: %q < %q", v, prev)
			}
		}
		prev = v
	}
	return &Changes{reg: reg, changes: changes}, nil
}

// Apply runs, in newest→oldest order, every change whose Version() is strictly
// newer than resolved. A request resolved to the registry's Current version (or
// newer than all changes) runs zero transforms, keeping the hot path free of
// allocations.
func (c *Changes) Apply(method string, msg any, resolved Version) {
	for i := len(c.changes) - 1; i >= 0; i-- {
		ch := c.changes[i]
		if c.reg.Newer(ch.Version(), resolved) {
			ch.TransformResponse(method, msg)
		}
	}
}

// ApplyError is Apply for error-to-error transforms. It runs, in newest→oldest order,
// every change whose Version() is strictly newer than resolved AND that
// implements the optional ErrorTransform capability, threading each result into
// the next. ErrorRecoveryTransform changes are skipped because this entry point
// has no response to validate. Changes that only transform responses are also
// skipped, so the common case allocates nothing and returns err untouched.
//
// Unlike Apply it RETURNS the (possibly replaced) error, because an error is an
// immutable value — see ErrorTransform for why that asymmetry is deliberate. A
// nil err is returned as-is without consulting any change: the success path
// never reaches this.
//
// Applied by WrapUnary ONLY — streaming handler errors are not transformed. See
// the SCOPE note on ErrorTransform.
//
// A change that REPLACES the error rather than returning it unchanged ends the
// chain's access to whatever the original carried: the replacement is what the
// next (older) ErrorTransform is handed, so any typed marker the original held
// is gone from that point on. With one implementer that is unobservable; a
// second error-path change that discriminates on a marker must therefore either
// preserve it when replacing, or accept that a newer change can shadow it.
func (c *Changes) ApplyError(method string, err error, resolved Version) error {
	return c.applyError(method, nil, err, resolved)
}

// ApplyErrorWithResponse applies error transforms with the concrete response
// available. WrapUnary uses this entry point so an ErrorRecoveryTransform may
// restore success only after validating the response it would serve.
func (c *Changes) ApplyErrorWithResponse(method string, response any, err error, resolved Version) error {
	return c.applyError(method, response, err, resolved)
}

func (c *Changes) applyError(method string, response any, err error, resolved Version) error {
	if err == nil {
		return nil
	}
	for i := len(c.changes) - 1; i >= 0; i-- {
		ch := c.changes[i]
		if !c.reg.Newer(ch.Version(), resolved) {
			continue
		}
		if response != nil {
			if recovery, ok := ch.(ErrorRecoveryTransform); ok {
				err = recovery.RecoverError(method, response, err)
				if err == nil {
					break
				}
				continue
			}
		}
		if et, ok := ch.(ErrorTransform); ok {
			transformed := et.TransformError(method, err)
			if transformed == nil {
				return connect.NewError(connect.CodeInternal, errors.New("api version error transform returned success without response-aware recovery"))
			}
			err = transformed
		}
	}
	return err
}

// relayedDaemonDeadline marks a connect error as the RELAYED daemon deadline —
// a ProxySwitchSessionAccount whose CommandResult came back carrying
// CommandResult_ERROR_CODE_DEADLINE_EXCEEDED because the DAEMON's own switch
// budget expired. bosso's validateCommandResult wraps with it (through
// MarkRelayedDaemonDeadline); SwitchDeadlineCodeChange matches it with
// errors.As, and callers outside this package ask IsRelayedDaemonDeadline.
//
// The marker exists because procedure alone cannot discriminate this case.
// bosso's dispatchOwnerCommand already maps its OWN commandDeadline expiry on
// this very procedure to connect.CodeDeadlineExceeded, and it did so long
// before V20260820. A transform that rewrote every CodeDeadlineExceeded on
// ProxySwitchSessionAccount back to CodeAborted would therefore regress that
// older, already-correct answer for old clients — a regression manufactured by
// the compatibility layer itself. Only the relayed error is new at V20260820,
// so only the relayed error is down-converted.
//
// Matching on the error MESSAGE text instead was considered and rejected: see
// docs/solutions/design-patterns/ask-whether-the-context-ended-before-asking-what-the-error-text-says.md
// for this repo's own record of why a substring classifier over an error string
// is unsafe.
//
// The TYPE is deliberately unexported while its constructor and predicate are
// not. Exporting it would make the zero value — &RelayedDaemonDeadline{}, a
// composite literal naming no fields, which is legal from any package even
// though every field is unexported — constructible by callers who never went
// through MarkRelayedDaemonDeadline, and a marker whose whole job is to be
// walked by errors.As must not be able to nil-deref the interceptor walking it.
// Unexporting removes that shape from the language rather than guarding against
// it, so Error and Unwrap need no nil branches and the exported surface is two
// functions instead of a type plus a constructor.
//
// It is a transparent wrapper: Unwrap exposes the underlying *connect.Error, so
// connect.CodeOf and connect's own error serialization see CodeDeadlineExceeded
// exactly as they would without the marker. Wrapping changes what the transform
// can recognise, never what an unversioned caller receives.
type relayedDaemonDeadline struct{ err error }

// MarkRelayedDaemonDeadline wraps err in the relayed-daemon-deadline marker.
// Returns nil for a nil err so callers can wrap unconditionally. It is the only
// way to produce the marker, which is what makes the zero value unreachable.
func MarkRelayedDaemonDeadline(err error) error {
	if err == nil {
		return nil
	}
	return &relayedDaemonDeadline{err: err}
}

// IsRelayedDaemonDeadline reports whether err carries the relayed-daemon-
// deadline marker anywhere in its chain. It is the exported way to ask the
// question — the marker type itself stays unexported, so a caller asserts the
// contract rather than reaching for the concrete type.
func IsRelayedDaemonDeadline(err error) bool {
	var marker *relayedDaemonDeadline
	return errors.As(err, &marker)
}

// Error implements error, delegating to the wrapped error so the message a
// client sees is unchanged by the marking.
func (e *relayedDaemonDeadline) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped *connect.Error to errors.As / errors.Is and, via
// them, to connect.CodeOf and connect's error serialization.
func (e *relayedDaemonDeadline) Unwrap() error { return e.err }

// switchResultCeilingExceeded marks the HANDLER-owned result ceiling on
// ProxySwitchSessionAccount: bosso stopped waiting before a daemon verdict
// arrived, while the account switch may still be running. It is deliberately
// separate from relayedDaemonDeadline, where the daemon itself already ended
// the switch and sent a deadline verdict.
type switchResultCeilingExceeded struct{ err error }

// MarkSwitchResultCeilingExceeded wraps err in the handler-owned switch result
// ceiling marker. Returns nil for a nil err so callers can wrap
// unconditionally.
func MarkSwitchResultCeilingExceeded(err error) error {
	if err == nil {
		return nil
	}
	return &switchResultCeilingExceeded{err: err}
}

// IsSwitchResultCeilingExceeded reports whether err carries the handler-owned
// switch result ceiling marker anywhere in its chain.
func IsSwitchResultCeilingExceeded(err error) bool {
	var marker *switchResultCeilingExceeded
	return errors.As(err, &marker)
}

func (e *switchResultCeilingExceeded) Error() string { return e.err.Error() }

func (e *switchResultCeilingExceeded) Unwrap() error { return e.err }

// retiredProcedure marks an error from a procedure that still exists on the
// bossanova.v1 wire surface for compatibility, but whose current behavior is
// intentionally retired.
type retiredProcedure struct{ err error }

// MarkRetiredProcedure wraps err in the retired-procedure marker. Returns nil
// for a nil err so callers can wrap unconditionally.
func MarkRetiredProcedure(err error) error {
	if err == nil {
		return nil
	}
	return &retiredProcedure{err: err}
}

// IsRetiredProcedure reports whether err carries the retired-procedure marker
// anywhere in its chain.
func IsRetiredProcedure(err error) bool {
	var marker *retiredProcedure
	return errors.As(err, &marker)
}

func (e *retiredProcedure) Error() string { return e.err.Error() }

func (e *retiredProcedure) Unwrap() error { return e.err }

// proxyListSessionsOwnerResolutionFailed marks the new V20260909 failure from
// ProxyListSessions when daemon-filter owner resolution is unavailable. The
// handler returns its partially filtered response alongside this error, letting
// the compatibility interceptor restore the legacy successful short list only
// for older clients.
type proxyListSessionsOwnerResolutionFailed struct{ err error }

// MarkProxyListSessionsOwnerResolutionFailed wraps err in the typed producer
// marker used by ProxyListSessionsOwnerResolutionChange.
func MarkProxyListSessionsOwnerResolutionFailed(err error) error {
	if err == nil {
		return nil
	}
	return &proxyListSessionsOwnerResolutionFailed{err: err}
}

// IsProxyListSessionsOwnerResolutionFailed reports whether err carries the
// ProxyListSessions owner-resolution marker.
func IsProxyListSessionsOwnerResolutionFailed(err error) bool {
	var marker *proxyListSessionsOwnerResolutionFailed
	return errors.As(err, &marker)
}

func (e *proxyListSessionsOwnerResolutionFailed) Error() string { return e.err.Error() }

func (e *proxyListSessionsOwnerResolutionFailed) Unwrap() error { return e.err }

// proxyListReposHolderResolutionFailed marks the V20260910 failure from
// ProxyListReposAggregated when the repository-holder store is unavailable.
// The producer returns the complete unstamped repository response alongside
// this error so older clients can retain the successful list they observed
// before holder enrichment existed.
type proxyListReposHolderResolutionFailed struct{ err error }

// MarkProxyListReposHolderResolutionFailed wraps err in the typed producer
// marker used by ProxyListReposHolderResolutionChange.
func MarkProxyListReposHolderResolutionFailed(err error) error {
	if err == nil {
		return nil
	}
	return &proxyListReposHolderResolutionFailed{err: err}
}

// IsProxyListReposHolderResolutionFailed reports whether err carries the
// ProxyListReposAggregated holder-resolution marker.
func IsProxyListReposHolderResolutionFailed(err error) bool {
	var marker *proxyListReposHolderResolutionFailed
	return errors.As(err, &marker)
}

func (e *proxyListReposHolderResolutionFailed) Error() string { return e.err.Error() }

func (e *proxyListReposHolderResolutionFailed) Unwrap() error { return e.err }

// relayedDaemonCanceled marks a connect error as the RELAYED daemon
// cancellation — a ProxySwitchSessionAccount whose CommandResult came back
// carrying CommandResult_ERROR_CODE_CANCELED because the caller cancelled the
// switch request while the DAEMON was executing it. bosso's validateCommandResult
// wraps with it (through MarkRelayedDaemonCanceled); SwitchCanceledCodeChange
// matches it with errors.As, and callers outside this package ask
// IsRelayedDaemonCanceled.
//
// The marker exists for the same reason relayedDaemonDeadline does: procedure
// alone cannot discriminate this case. bosso's dispatchOwnerCommand already maps
// its own context.Canceled path on this procedure to connect.CodeCanceled, and
// it did so before V20260821. A transform that rewrote every CodeCanceled on
// ProxySwitchSessionAccount back to CodeAborted would therefore regress that
// older, already-correct answer for old clients. Only the relayed error is new
// at V20260821, so only the relayed error is down-converted.
//
// The type is deliberately unexported while its constructor and predicate are
// exported. See relayedDaemonDeadline above for the zero-value rationale.
type relayedDaemonCanceled struct{ err error }

// MarkRelayedDaemonCanceled wraps err in the relayed-daemon-canceled marker.
// Returns nil for a nil err so callers can wrap unconditionally. It is the only
// way to produce the marker, which is what makes the zero value unreachable.
func MarkRelayedDaemonCanceled(err error) error {
	if err == nil {
		return nil
	}
	return &relayedDaemonCanceled{err: err}
}

// IsRelayedDaemonCanceled reports whether err carries the relayed-daemon-
// canceled marker anywhere in its chain. It is the exported way to ask the
// question — the marker type itself stays unexported, so a caller asserts the
// contract rather than reaching for the concrete type.
func IsRelayedDaemonCanceled(err error) bool {
	var marker *relayedDaemonCanceled
	return errors.As(err, &marker)
}

// Error implements error, delegating to the wrapped error so the message a
// client sees is unchanged by the marking.
func (e *relayedDaemonCanceled) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped *connect.Error to errors.As / errors.Is and, via
// them, to connect.CodeOf and connect's error serialization.
func (e *relayedDaemonCanceled) Unwrap() error { return e.err }

// RefMsg is the demo message type used by ReferenceChange. Real transforms in
// future PRs will type-assert against generated bossanova.v1 protobuf messages;
// this demo struct keeps the apiversion package self-contained and is what the
// e2e test (a later task) drives.
type RefMsg struct {
	Greeting string
}

// ProductionChanges returns the Changes wired into bosso, built against
// DefaultRegistry. It ships live transforms in non-decreasing version
// order: OrphanedStateChange (introduced at V20260704), which down-converts
// SESSION_STATE_ORPHANED on Session.state; AgentAuthFailedChange (introduced at
// V20260705), which neutralizes the ATTENTION_REASON_AGENT_AUTH_FAILED attention
// reason; UnmanagedLabelChange (introduced at V20260706), which restores the
// "System default" account label for the unbound case; LimitedChatStatusChange
// (introduced at V20260706), which maps CHAT_STATUS_LIMITED and its session
// display shape back to the prior idle-style behavior; NoEligibleAccountChange
// (introduced at V20260711), which down-converts
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT on Session.rotation_events
// back to ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY; ErroredStatusChange
// (introduced at V20260718), which restores the pre-BOS-430 orphaned/blocked
// display shape on Session.display_label / display_intent / display_spinner; and
// RespawnSameAccountOutcomeChange (introduced at V20260723), which down-converts
// ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT and ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED
// on Session.rotation_events back to ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY;
// AgentStalledChange (introduced at V20260803), which neutralizes the
// ATTENTION_REASON_AGENT_STALLED attention reason; and WaitingChatStatusChange
// (introduced at V20260804), which maps CHAT_STATUS_WAITING back to
// CHAT_STATUS_WORKING, clears the accompanying waiting_reason, and restores the
// working display shape on Session.display_label / display_intent /
// display_spinner; and DraftPRFailureLabelChange (introduced at V20260812),
// which restores the pre-BOS-855 "? PR failed" composite on
// Session.display_label / display_intent / display_spinner for a session whose
// draft-PR creation failed while a chat is live; and GateFailedOutcomeChange
// (introduced at V20260816), which restores the pre-BOS-881 "gated" outcome,
// CRON_JOB_STATUS_GATED status, and "gated" RunCronJobNow skip reason for a
// cron job whose gate command could not be evaluated at all; and
// SwitchDeadlineCodeChange (introduced at V20260820), which restores the
// pre-BOS-947 connect.CodeAborted for a relayed ProxySwitchSessionAccount
// failure whose CommandResult carried ERROR_CODE_DEADLINE_EXCEEDED — the
// mechanism's first ERROR-path transform, applied via ApplyError rather than
// Apply; SwitchResultCeilingMessageChange (introduced at V20260821), which
// restores the legacy "command timed out after 2m0s" text for older clients
// when ProxySwitchSessionAccount's handler-owned result ceiling expires; and
// SwitchCanceledCodeChange (also introduced at V20260821), which restores the
// pre-BOS-958 connect.CodeAborted for a relayed ProxySwitchSessionAccount
// failure whose CommandResult carried ERROR_CODE_CANCELED.
// StaleCheckStateChange (introduced at V20260825), which restores
// Session.last_check_state from last_check_state_observed for clients pinned
// before the field began serving only head-current demonstrated verdicts.
// SwitchActiveOrganizationRetiredMessageChange (introduced at V20260903), which
// restores the legacy organization-management-unimplemented message for older
// clients after SwitchActiveOrganization was retired in favor of AuthKit
// switchToOrganization.
// AbandonedCheckoutStatusChange (introduced at V20260904), which restores the
// pre-BOS-1076 activating-subscription shape on CloudAccessStatus for a Stripe
// Checkout session that was created but never completed.
// CloudAccessOrganizationChange (introduced at V20260907), which restores the
// always-empty CloudAccessStatus.workos_org_id older clients were built against,
// before bosso began populating it with the caller's organization.
// ProxyListSessionsOwnerResolutionChange (introduced at V20260909), which
// restores the legacy successful short list when ownership resolution fails;
// and ProxyListReposHolderResolutionChange (introduced at V20260910), which
// restores the legacy successful unstamped repository list when holder
// resolution fails; and PendingInvitationResponseChange (introduced at
// V20260910), which removes pending rows from ListOrganizationMembers and
// clears invitation-only fields older clients did not observe on
// InviteOrganizationMember responses.
// SupersededCredentialClassChange (introduced at V20260913), which restores the
// empty AuthCheck.failure_class older clients were built against whenever the
// daemon pairs "credential_superseded" with a "healthy" outcome.
//
// RefreshChainUnprovenOutcomeChange (introduced at V20260914), which restores
// the pre-BOS-1174 "healthy" / "" pair on Account.auth_check for a credential
// check that ran cleanly but could not prove the credential's refresh chain.
// Each is applied to clients pinned to a version older than the change; a
// request resolved to V20260913 or newer runs zero registered transforms.
//
// V20260906's cloud-access change registers no entry here: that behavior is
// caller-relative and stays handler-gated via apiversion.IsMemberOrgCloudAccess.
//
// V20260908 likewise registers no entry: cross-organization session command
// routing is handler-gated via apiversion.IsCrossOrgSessionCommands because no
// response or error transform can turn a successful dispatch back into the
// former NotFound.
//
// V20260910's caller-relative cron-job result set registers no entry: it is
// handler-gated via apiversion.IsCrossOrgCronReads because a transform cannot
// reconstruct the organization named by the request's claim. The repository
// holder failure introduced in the same release window does register an entry
// because response-aware error recovery can restore its legacy success.
//
// Future API behavior changes should:
//  1. Append the new Version to DefaultRegistry (see version.go).
//  2. Set it as Current in DefaultRegistry.
//  3. Append a VersionChange entry here describing the down-convert transform.
//
// See docs/api-versioning.md for the full procedure.
func ProductionChanges() *Changes {
	c, err := NewChanges(DefaultRegistry(), OrphanedStateChange{}, AgentAuthFailedChange{}, UnmanagedLabelChange{}, LimitedChatStatusChange{}, NoEligibleAccountChange{}, ErroredStatusChange{}, RespawnSameAccountOutcomeChange{}, AgentStalledChange{}, WaitingChatStatusChange{}, DraftPRFailureLabelChange{}, GateFailedOutcomeChange{}, SwitchDeadlineCodeChange{}, SwitchResultCeilingMessageChange{}, SwitchCanceledCodeChange{}, StaleCheckStateChange{}, SwitchActiveOrganizationRetiredMessageChange{}, AbandonedCheckoutStatusChange{}, CloudAccessOrganizationChange{}, ProxyListSessionsOwnerResolutionChange{}, ProxyListReposHolderResolutionChange{}, PendingInvitationResponseChange{}, AcceptedInvitationResponseChange{}, SupersededCredentialClassChange{}, RefreshChainUnprovenOutcomeChange{})
	if err != nil {
		panic("apiversion: ProductionChanges is invalid: " + err.Error())
	}
	return c
}

// SupersededCredentialClassChange is the production VersionChange introduced at
// V20260913.
//
// At V20260913 the OrchestratorService began serving AuthCheck.failure_class
// "credential_superseded" ALONGSIDE outcome "healthy" (BOS-1175): an ambient
// `codex login` for the same provider account holds a different refresh token,
// so the refresh chain behind the stored credential is dead even though the
// provider still accepts the stored access token. The account remains eligible,
// which is why the outcome stays healthy.
//
// No field or enum member was added; the change is in the VALUE served. What
// makes it a behavioral change rather than an additive one is the invariant it
// breaks: before this version outcome "healthy" ALWAYS came with an empty
// failure_class, so a client built against that pairing has no branch for a
// class it cannot interpret and may render the row as failed, print the raw
// token, or drop it. Clients pinned to an older version are therefore
// down-converted back to the empty failure_class.
//
// It targets the OrchestratorService procedures that carry an Account (and so
// its embedded AuthCheck): ProxyListAccounts, ProxyManageListAccounts,
// ProxyAddAccount and ProxyRefreshAccount. All are unary. All other methods and
// message types are no-ops.
//
// ONLY the healthy pairing is blanked. A "credential_superseded" class on a
// non-healthy outcome is not a shape this version introduced — no producer
// emits it — and blanking it anyway would erase a classification older clients
// could always observe on a failing check.
type SupersededCredentialClassChange struct{}

// Version implements VersionChange. The change was introduced at V20260913, so
// it is applied to any request resolved to a strictly older version.
func (SupersededCredentialClassChange) Version() Version { return V20260913 }

// Wire values involved in SupersededCredentialClassChange (V20260913). They are
// raw literals rather than an import of services/bossd's accountwiring package:
// lib packages must not depend on a service's internal packages, and pinning the
// exact wire strings keeps a later producer-side rename from silently changing
// what older clients receive.
const (
	// authOutcomeHealthy is the AuthCheck.outcome the superseded class is
	// paired with. It mirrors accountwiring's healthy outcome.
	authOutcomeHealthy = "healthy"
	// authFailureClassCredentialSuperseded is the CURRENT (V20260913+) class
	// served alongside a healthy outcome. It mirrors accountwiring's
	// authFailureCredentialSuperseded.
	authFailureClassCredentialSuperseded = "credential_superseded"
)

// downconvertSupersededAccount returns the Account to place in the response for
// a pre-V20260913 client. An account whose check is not the new
// healthy+"credential_superseded" pairing is returned unchanged and clone-free,
// which is every account a pre-BOS-1175 server could produce. Cloning otherwise
// matters for the same reason it does on the transforms above: the response may
// hold a pointer that is also cached or shared, so the down-convert must never
// mutate in place.
func downconvertSupersededAccount(a *pb.Account) *pb.Account {
	check := a.GetAuthCheck()
	if check.GetOutcome() != authOutcomeHealthy || check.GetFailureClass() != authFailureClassCredentialSuperseded {
		return a
	}
	clone, ok := proto.Clone(a).(*pb.Account)
	if !ok {
		return a
	}
	if clone.AuthCheck != nil {
		clone.AuthCheck.FailureClass = ""
	}
	return clone
}

// downconvertSupersededAccounts rewrites every account in a repeated field,
// leaving the slice untouched when nothing changed.
func downconvertSupersededAccounts(accounts []*pb.Account) {
	for i := range accounts {
		accounts[i] = downconvertSupersededAccount(accounts[i])
	}
}

// TransformResponse implements VersionChange. It clears the
// "credential_superseded" class from a healthy AuthCheck on every
// OrchestratorService response that can carry an Account. It is a no-op for any
// other method or payload type.
//
// SIX procedures carry one, not four: two return a repeated Account (ProxyList
// Accounts, ProxyManageListAccounts) and four return a singular one (ProxyAdd
// Account, ProxyRefreshAccount, ProxyUpdateAccount, ProxyTestAccount). The last
// two are easy to miss because neither reads as an account-health call, but both
// are served and both build their Account from the same accountToProto
// (services/bossd/internal/server/convert.go) that carries the durable class —
// TestAccount in particular re-reads the row through recordAndRespond after a
// verification, which is exactly when a superseded class is most likely present.
// accountProbes in production_coverage_test.go derives this list from the
// generated descriptors so a seventh carrier cannot escape it silently.
func (SupersededCredentialClassChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure:
		if m, ok := msg.(*pb.ProxyListAccountsResponse); ok && m != nil {
			downconvertSupersededAccounts(m.Accounts)
		}
	case bossanovav1connect.OrchestratorServiceProxyManageListAccountsProcedure:
		if m, ok := msg.(*pb.ProxyManageListAccountsResponse); ok && m != nil {
			downconvertSupersededAccounts(m.Accounts)
		}
	case bossanovav1connect.OrchestratorServiceProxyAddAccountProcedure:
		if m, ok := msg.(*pb.ProxyAddAccountResponse); ok && m != nil {
			m.Account = downconvertSupersededAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyRefreshAccountProcedure:
		if m, ok := msg.(*pb.ProxyRefreshAccountResponse); ok && m != nil {
			m.Account = downconvertSupersededAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyUpdateAccountProcedure:
		if m, ok := msg.(*pb.ProxyUpdateAccountResponse); ok && m != nil {
			m.Account = downconvertSupersededAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyTestAccountProcedure:
		if m, ok := msg.(*pb.ProxyTestAccountResponse); ok && m != nil {
			m.Account = downconvertSupersededAccount(m.GetAccount())
		}
	}
}

// AcceptedInvitationResponseChange is the V20260911 member-directory response
// change. Older clients cannot distinguish an accepted invitation placeholder
// from an active member, so the down-convert removes those rows.
type AcceptedInvitationResponseChange struct{}

// Version implements VersionChange.
func (AcceptedInvitationResponseChange) Version() Version { return V20260911 }

// TransformResponse restores the member list served before V20260911.
func (AcceptedInvitationResponseChange) TransformResponse(method string, msg any) {
	if method != bossanovav1connect.OrchestratorServiceListOrganizationMembersProcedure {
		return
	}
	response, ok := msg.(*pb.ListOrganizationMembersResponse)
	if !ok || response == nil {
		return
	}
	members := make([]*pb.OrganizationMember, 0, len(response.GetMembers()))
	for _, member := range response.GetMembers() {
		if member != nil && member.GetIsInviteAccepted() {
			continue
		}
		members = append(members, member)
	}
	response.Members = members
}

// PendingInvitationResponseChange is the V20260910 response change for real
// WorkOS invitations. Older clients never observed pending rows in
// ListOrganizationMembers, so the down-convert removes them. On
// InviteOrganizationMember it clears invitation-only fields while preserving
// the email and role older clients can still render. It cannot restore the old
// NotFound for unregistered emails: response transforms cannot turn a
// successful RPC back into an error.
type PendingInvitationResponseChange struct{}

// Version implements VersionChange.
func (PendingInvitationResponseChange) Version() Version { return V20260910 }

// TransformResponse restores the member-list and invitation response shapes
// served before V20260910. Shared member messages are never mutated in place.
func (PendingInvitationResponseChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceInviteOrganizationMemberProcedure:
		response, ok := msg.(*pb.InviteOrganizationMemberResponse)
		if !ok || response == nil || response.GetMember() == nil || !response.GetMember().GetIsInvitePending() {
			return
		}
		member, ok := proto.Clone(response.GetMember()).(*pb.OrganizationMember)
		if !ok {
			return
		}
		member.IsInvitePending = false
		member.InvitationId = ""
		response.Member = member
	case bossanovav1connect.OrchestratorServiceListOrganizationMembersProcedure:
		response, ok := msg.(*pb.ListOrganizationMembersResponse)
		if !ok || response == nil {
			return
		}
		members := make([]*pb.OrganizationMember, 0, len(response.GetMembers()))
		for _, member := range response.GetMembers() {
			if member != nil && member.GetIsInvitePending() {
				continue
			}
			members = append(members, member)
		}
		response.Members = members
	}
}

// ProxyListReposHolderResolutionChange is the V20260910 error-path change that
// makes a repository-holder store outage fail ProxyListReposAggregated instead
// of silently serving blank organization stamps. Older clients retain the
// complete unstamped repository list returned alongside the marked error;
// Current receives CodeInternal.
type ProxyListReposHolderResolutionChange struct{}

// Version implements VersionChange.
func (ProxyListReposHolderResolutionChange) Version() Version { return V20260910 }

// TransformResponse implements VersionChange. This change lives only on the
// error path.
func (ProxyListReposHolderResolutionChange) TransformResponse(string, any) {}

// RecoverError converts only the marked ProxyListReposAggregated holder-store
// failure accompanied by the expected complete response to success. Unmarked
// errors, mismatched response types, and every other procedure remain unchanged.
func (ProxyListReposHolderResolutionChange) RecoverError(method string, response any, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceProxyListReposAggregatedProcedure {
		return err
	}
	if !IsProxyListReposHolderResolutionFailed(err) {
		return err
	}
	if typed, ok := response.(*pb.ProxyListReposAggregatedResponse); !ok || typed == nil {
		return err
	}
	return nil
}

// ProxyListSessionsOwnerResolutionChange is the V20260909 error-path change
// that distinguishes an owner-store outage from a genuinely empty daemon-
// filtered list. Older clients retain the legacy short-list success returned
// alongside the marked error by the producer; Current receives CodeUnavailable.
type ProxyListSessionsOwnerResolutionChange struct{}

// Version implements VersionChange.
func (ProxyListSessionsOwnerResolutionChange) Version() Version { return V20260909 }

// TransformResponse implements VersionChange. This change lives only on the
// error path.
func (ProxyListSessionsOwnerResolutionChange) TransformResponse(string, any) {}

// RecoverError converts only the marked ProxyListSessions outage accompanied
// by the expected response message to success. WrapUnary then serves that
// complete legacy short list. Unmarked errors, mismatched response types, and
// every other procedure remain unchanged.
func (ProxyListSessionsOwnerResolutionChange) RecoverError(method string, response any, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure {
		return err
	}
	if !IsProxyListSessionsOwnerResolutionFailed(err) {
		return err
	}
	if typed, ok := response.(*pb.ProxyListSessionsResponse); !ok || typed == nil {
		return err
	}
	return nil
}

// AbandonedCheckoutStatusChange is the production VersionChange introduced at
// V20260904.
//
// At V20260904 the OrchestratorService began distinguishing an ABANDONED Stripe
// Checkout from a genuinely activating subscription on CloudAccessStatus
// (BOS-1076). An account that merely reached the CheckoutStarted setup state — a
// Checkout session was created, nothing was paid — used to be decorated exactly
// like an account whose user had returned from Stripe: state
// CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH, message "Your subscription is
// being activated.", can_create_checkout false, checkout_started true. A user who
// opened Checkout and closed the tab watched that spinner forever.
//
// Such an account now keeps CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION and its
// checkout affordance, and reports checkout_started only so the client can offer
// to RESUME. No field or enum member was added: the behavior change is in the
// VALUES served, carried by the combination
//
//	NEEDS_SUBSCRIPTION && checkout_started && can_create_checkout
//
// which the server could not emit before this version (decorateCloudAccessStatus
// forced checkout_started false on every NEEDS_SUBSCRIPTION status). That makes
// it an exact, self-contained discriminator readable from the response alone,
// which is all TransformResponse is given. A client pinned to an older version
// was built when that combination was impossible, so this change restores the
// prior shape: PENDING_ENTITLEMENT_REFRESH, the activating message,
// can_create_checkout false, checkout_started true, and the matching
// "pending_entitlement_refresh" denial_reason.
//
// It targets the three unary procedures that carry a CloudAccessStatus —
// GetCloudAccessStatus, CreateCheckoutSession and RefreshCloudEntitlements. All
// other methods and message types are no-ops.
type AbandonedCheckoutStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260904, so
// it is applied to any request resolved to a strictly older version.
func (AbandonedCheckoutStatusChange) Version() Version { return V20260904 }

// Wire values involved in AbandonedCheckoutStatusChange (V20260904). They are
// raw literals rather than an import of the bosso billing package for the same
// reason the cron outcome strings above are: this package pins the exact strings
// older clients were built to receive, so a later reword on the producer side
// cannot silently change what they get.
const (
	// cloudActivatingMessage is the message a pre-V20260904 client saw for an
	// account in the CheckoutStarted setup state. It mirrors bosso's
	// cloudActivatingMessage.
	cloudActivatingMessage = "Your subscription is being activated."
	// cloudPendingEntitlementDenialReason is the denial_reason bosso derives from
	// CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH. It mirrors bosso's
	// cloudAccessDenialReason for that state.
	cloudPendingEntitlementDenialReason = "pending_entitlement_refresh"
)

// downconvertAbandonedCheckoutStatus returns the CloudAccessStatus to place in
// the response for a pre-V20260904 client. A status that does not carry the
// abandoned-checkout combination is returned unchanged, keeping every other
// cloud-access shape — ACTIVE, PAST_DUE, CANCELED, BILLING_UNAVAILABLE, a plain
// never-started NEEDS_SUBSCRIPTION, and a real PENDING_ENTITLEMENT_REFRESH —
// clone-free and untouched. Cloning matters for the same reason it does on the
// Session transforms: the response may hold a pointer that is also cached or
// shared, so the down-convert must never mutate in place.
func downconvertAbandonedCheckoutStatus(st *pb.CloudAccessStatus) *pb.CloudAccessStatus {
	if st == nil {
		return st
	}
	if st.GetState() != pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION {
		return st
	}
	// The value triple this keys on is the wire form of the abandoned-checkout
	// signal, and bosso's decorateCloudAccessStatus is its only producer. That
	// producer is pinned exhaustively by
	// TestDecorateCloudAccessStatus_ResumeAffordanceIsExhaustivelyPinned
	// (services/bosso/internal/server/billing_test.go): if a state ever starts
	// emitting the pair somewhere new, that test fails before this transform can
	// silently start rewriting it.
	if !st.GetCheckoutStarted() || !st.GetCanCreateCheckout() {
		return st
	}
	clone, ok := proto.Clone(st).(*pb.CloudAccessStatus)
	if !ok {
		return st
	}
	clone.State = pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH
	clone.Message = cloudActivatingMessage
	clone.CanCreateCheckout = false
	clone.CheckoutStarted = true
	clone.DenialReason = cloudPendingEntitlementDenialReason
	return clone
}

// TransformResponse implements VersionChange. It restores the pre-BOS-1076
// activating shape on every OrchestratorService response that can carry a
// CloudAccessStatus. It is a no-op for any other method or payload type.
func (AbandonedCheckoutStatusChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceGetCloudAccessStatusProcedure:
		if m, ok := msg.(*pb.GetCloudAccessStatusResponse); ok {
			m.Status = downconvertAbandonedCheckoutStatus(m.GetStatus())
		}
	case bossanovav1connect.OrchestratorServiceCreateCheckoutSessionProcedure:
		if m, ok := msg.(*pb.CreateCheckoutSessionResponse); ok {
			m.Status = downconvertAbandonedCheckoutStatus(m.GetStatus())
		}
	case bossanovav1connect.OrchestratorServiceRefreshCloudEntitlementsProcedure:
		if m, ok := msg.(*pb.RefreshCloudEntitlementsResponse); ok {
			m.Status = downconvertAbandonedCheckoutStatus(m.GetStatus())
		}
	}
}

// CloudAccessOrganizationChange is the production VersionChange introduced at
// V20260907. Current responses populate CloudAccessStatus.workos_org_id with the
// WorkOS organization the caller is acting as. The field shipped with the
// original subscription gating but no producer ever assigned it, so every client
// built before V20260907 observed the empty string on every response; this
// restores that value for them.
//
// Blanking rather than clone-free pass-through is the whole transform: there is
// no prior non-empty value to reconstruct, because there never was one.
type CloudAccessOrganizationChange struct{}

// Version implements VersionChange. The change was introduced at V20260907, so
// it is applied to any request resolved to a strictly older version.
func (CloudAccessOrganizationChange) Version() Version { return V20260907 }

// downconvertCloudAccessOrganization returns the CloudAccessStatus a
// pre-V20260907 client expects: the same status with workos_org_id empty. A
// status that already carries no organization is returned unchanged and
// clone-free, which is both the daemon-caller case and every response produced
// before bosso started filling the field. Cloning otherwise matters for the same
// reason it does on the transforms above: the response may hold a pointer that is
// also cached or shared, so the down-convert must never mutate in place.
func downconvertCloudAccessOrganization(st *pb.CloudAccessStatus) *pb.CloudAccessStatus {
	if st == nil || st.GetWorkosOrgId() == "" {
		return st
	}
	clone, ok := proto.Clone(st).(*pb.CloudAccessStatus)
	if !ok {
		return st
	}
	clone.WorkosOrgId = ""
	return clone
}

// TransformResponse implements VersionChange. It empties workos_org_id on every
// OrchestratorService response that can carry a CloudAccessStatus. It is a no-op
// for any other method or payload type.
func (CloudAccessOrganizationChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceGetCloudAccessStatusProcedure:
		if m, ok := msg.(*pb.GetCloudAccessStatusResponse); ok {
			m.Status = downconvertCloudAccessOrganization(m.GetStatus())
		}
	case bossanovav1connect.OrchestratorServiceCreateCheckoutSessionProcedure:
		if m, ok := msg.(*pb.CreateCheckoutSessionResponse); ok {
			m.Status = downconvertCloudAccessOrganization(m.GetStatus())
		}
	case bossanovav1connect.OrchestratorServiceRefreshCloudEntitlementsProcedure:
		if m, ok := msg.(*pb.RefreshCloudEntitlementsResponse); ok {
			m.Status = downconvertCloudAccessOrganization(m.GetStatus())
		}
	}
}

// StaleCheckStateChange is the production VersionChange introduced at
// V20260825. Current responses serve Session.last_check_state as an evaluated,
// head-current value and keep the persisted latch in last_check_state_observed.
// Older clients were built against last_check_state carrying that raw latch, so
// this restores the prior value on every Session-bearing unary response.
type StaleCheckStateChange struct{}

// Version implements VersionChange. The change was introduced at V20260825, so
// it is applied to any request resolved to a strictly older version.
func (StaleCheckStateChange) Version() Version { return V20260825 }

func downconvertStaleCheckStateSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetLastCheckState() == s.GetLastCheckStateObserved() {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.LastCheckState = clone.GetLastCheckStateObserved()
	return clone
}

// TransformResponse implements VersionChange. It is a no-op for methods and
// payloads that do not carry Session messages.
func (StaleCheckStateChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertStaleCheckStateSession)
}

// OrphanedStateChange is the production VersionChange introduced at V20260704.
//
// At V20260704 the OrchestratorService began serving the SessionState value
// SESSION_STATE_ORPHANED (14) on Session.state — a new terminal state for a
// headless run that a daemon restart killed. Before this change such a run
// stayed in SESSION_STATE_IMPLEMENTING_PLAN (5). A client pinned to Baseline was
// built before ORPHANED existed and would not know how to render it, so for any
// request resolved older than V20260704 this change rewrites ORPHANED back to
// IMPLEMENTING_PLAN, preserving the prior observable behavior.
//
// It targets every OrchestratorService response that embeds one or more
// *pb.Session messages, keyed by the Connect procedure path. Only the unary
// procedures are exercised in practice: the version Interceptor applies
// transforms on unary responses only (streaming procedures such as
// ProxyCreateSession evolve via additive proto fields, not this layer), but the
// streaming created-Session case is handled here too so the transform is
// complete and directly unit-testable. All other methods and message types are
// no-ops.
type OrphanedStateChange struct{}

// Version implements VersionChange. The change was introduced at V20260704, so
// it is applied to any request resolved to a strictly older version (Baseline).
func (OrphanedStateChange) Version() Version { return V20260704 }

// downconvertOrphanedSession returns the Session to place in the response for a
// pre-V20260704 client. If s is ORPHANED it returns a CLONE whose state is reset
// to SESSION_STATE_IMPLEMENTING_PLAN; otherwise it returns s unchanged. Cloning
// is essential: in bosso's single-instance registry path the response holds the
// same *pb.Session pointers cached in the in-memory registry, so mutating in
// place would permanently corrupt the cached ORPHANED session (and race other
// readers). Only orphaned sessions allocate, keeping the common path clone-free.
func downconvertOrphanedSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetState() != pb.SessionState_SESSION_STATE_ORPHANED {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.State = pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN
	return clone
}

func transformUnarySessionResponse(method string, msg any, transform func(*pb.Session) *pb.Session) bool {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure:
		if m, ok := msg.(*pb.ProxyListSessionsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = transform(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyListSessionsAcrossOrganizationsProcedure:
		//nolint:staticcheck // The deprecated RPC remains supported for pinned clients.
		if m, ok := msg.(*pb.ProxyListSessionsAcrossOrganizationsResponse); ok {
			for i := range m.Sessions {
				m.Sessions[i] = transform(m.Sessions[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyStopSessionProcedure:
		if m, ok := msg.(*pb.ProxyStopSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyPauseSessionProcedure:
		if m, ok := msg.(*pb.ProxyPauseSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResumeSessionProcedure:
		if m, ok := msg.(*pb.ProxyResumeSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyMergeSessionProcedure:
		if m, ok := msg.(*pb.ProxyMergeSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyArchiveSessionProcedure:
		if m, ok := msg.(*pb.ProxyArchiveSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceTransferSessionProcedure:
		if m, ok := msg.(*pb.TransferSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyRetrySessionProcedure:
		if m, ok := msg.(*pb.ProxyRetrySessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyUpdateSessionProcedure:
		if m, ok := msg.(*pb.ProxyUpdateSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyLinkSessionPRProcedure:
		if m, ok := msg.(*pb.ProxyLinkSessionPRResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure:
		if m, ok := msg.(*pb.ProxyRunCronJobNowResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyCloseSessionProcedure:
		if m, ok := msg.(*pb.ProxyCloseSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	case bossanovav1connect.OrchestratorServiceProxyResurrectSessionProcedure:
		if m, ok := msg.(*pb.ProxyResurrectSessionResponse); ok {
			m.Session = transform(m.GetSession())
		}
	default:
		return false
	}
	return true
}

// TransformResponse implements VersionChange. It down-converts Session.state on
// each OrchestratorService response type that carries one or more Sessions,
// matched by procedure path, rewriting only response-local (cloned) copies so a
// shared registry pointer is never mutated. It is a no-op for any other method
// or payload type.
func (OrphanedStateChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertOrphanedSession)
	// ProxyCreateSession (and other streaming Session-bearing responses) are
	// intentionally NOT handled: the version interceptor applies transforms to
	// unary responses only, and streaming envelope evolution is handled by
	// additive proto fields / wire compatibility, not per-message value
	// transforms (see interceptor.go and docs/plans/BOS-10).
}

// AgentAuthFailedChange is the production VersionChange introduced at V20260705.
//
// At V20260705 the OrchestratorService began serving the AttentionReason value
// ATTENTION_REASON_AGENT_AUTH_FAILED (5) on Session.attention_status.reason — a
// new attention reason raised when an agent's pane shows the login-required
// terminal shape ("Not logged in" / "Please run /login"). This attention only
// fires where the session previously had NO attention at all: before the
// detector existed a login-dead session "just went quiet". A client pinned to an
// older version was built before this reason existed and would not know how to
// render it, so for any request resolved older than V20260705 this change
// neutralizes the attention back to the prior observable behavior — no attention
// — by clearing attention_status and the auth-specific blocked_reason.
//
// Because the detector only overlays AGENT_AUTH_FAILED where ComputeAttentionStatus
// returned no attention (a session already Blocked/Orphaned keeps its own reason),
// a Session carrying reason==AGENT_AUTH_FAILED never had a real DB blocked_reason,
// so clearing blocked_reason here is faithful and cannot erase an unrelated one.
//
// It targets the same OrchestratorService unary procedures OrphanedStateChange
// handles, keyed by the Connect procedure path. All other methods and message
// types are no-ops.
type AgentAuthFailedChange struct{}

// Version implements VersionChange. The change was introduced at V20260705, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704).
func (AgentAuthFailedChange) Version() Version { return V20260705 }

// downconvertAuthFailedSession returns the Session to place in the response for a
// pre-V20260705 client. If s carries attention_status.reason ==
// ATTENTION_REASON_AGENT_AUTH_FAILED it returns a CLONE with attention_status and
// blocked_reason cleared (restoring the prior "went quiet, no attention"
// behavior); otherwise it returns s unchanged. Cloning is essential: bosso's
// single-instance registry path holds the same *pb.Session pointers it caches, so
// mutating in place would corrupt the cached session (and race other readers).
// Only auth-failed sessions allocate, keeping the common path clone-free.
func downconvertAuthFailedSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_AUTH_FAILED {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.AttentionStatus = nil
	clone.BlockedReason = nil
	return clone
}

// TransformResponse implements VersionChange. It neutralizes an
// AGENT_AUTH_FAILED attention on each OrchestratorService response type that
// carries one or more Sessions, matched by procedure path, rewriting only
// response-local (cloned) copies so a shared registry pointer is never mutated.
// It is a no-op for any other method or payload type.
func (AgentAuthFailedChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertAuthFailedSession)
}

// AgentStalledChange is the production VersionChange introduced at V20260803.
//
// At V20260803 the OrchestratorService began serving the AttentionReason value
// ATTENTION_REASON_AGENT_STALLED (6) on Session.attention_status.reason — a new
// attention reason raised when a chat reports CHAT_STATUS_WORKING while its
// agent has made no semantic progress (no new transcript record) for longer than
// its phase's threshold, i.e. a silently dead turn behind a still-animating
// spinner. Like AGENT_AUTH_FAILED this attention only fires where the session
// previously had NO attention at all: before the detector existed such a session
// just kept spinning. A client pinned to an older version was built before this
// reason existed and would not know how to render it, so for any request
// resolved older than V20260803 this change neutralizes the attention back to
// the prior observable behavior — no attention — by clearing attention_status
// and the stall-specific blocked_reason.
//
// Because the detector only overlays AGENT_STALLED where ComputeAttentionStatus
// returned no attention (a session already Blocked/Orphaned keeps its own
// reason), a Session carrying reason==AGENT_STALLED never had a real DB
// blocked_reason, so clearing blocked_reason here is faithful and cannot erase
// an unrelated one.
//
// It targets EVERY unary OrchestratorService response that embeds one or more
// *pb.Session messages, keyed by the Connect procedure path: the read/lifecycle
// set OrphanedStateChange handles (ListSessions, GetSession, Stop, Pause,
// Resume, Merge, Archive, TransferSession) plus the remaining Session-returning
// procedures (RetrySession, UpdateSession, LinkSessionPR, RunCronJobNow,
// CloseSession, ResurrectSession) — the same full set ErroredStatusChange
// covers. The mutating procedures are NOT theoretical: bossd's UpdateSession /
// LinkSessionPR handlers hand their response proto to the OnSessionUpdated hook
// (services/bossd/cmd/main.go), which runs HydrateAgentObservability on it IN
// PLACE before it is returned, so the very Session those RPCs serve can carry a
// freshly-overlaid ATTENTION_REASON_AGENT_STALLED. All other methods and
// message types are no-ops.
//
// ProxyCreateSession is deliberately excluded: it is a streaming procedure and
// the version Interceptor applies transforms on unary responses only (see
// interceptor.go), matching OrphanedStateChange and AgentAuthFailedChange.
type AgentStalledChange struct{}

// Version implements VersionChange. The change was introduced at V20260803, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705, V20260706, V20260711, V20260718, V20260723).
func (AgentStalledChange) Version() Version { return V20260803 }

// downconvertStalledSession returns the Session to place in the response for a
// pre-V20260803 client. If s carries attention_status.reason ==
// ATTENTION_REASON_AGENT_STALLED it returns a CLONE with attention_status and
// blocked_reason cleared (restoring the prior "kept spinning, no attention"
// behavior); otherwise it returns s unchanged. Cloning is essential: bosso's
// single-instance registry path holds the same *pb.Session pointers it caches, so
// mutating in place would corrupt the cached session (and race other readers).
// Only stalled sessions allocate, keeping the common path clone-free.
func downconvertStalledSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetAttentionStatus().GetReason() != pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.AttentionStatus = nil
	clone.BlockedReason = nil
	return clone
}

// TransformResponse implements VersionChange. It neutralizes an AGENT_STALLED
// attention on each OrchestratorService response type that carries one or more
// Sessions, matched by procedure path, rewriting only response-local (cloned)
// copies so a shared registry pointer is never mutated. It is a no-op for any
// other method or payload type.
func (AgentStalledChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertStalledSession)
}

// UnmanagedLabelChange is the production VersionChange introduced at V20260706.
//
// At V20260706 the OrchestratorService began serving the account label
// "Unmanaged local credentials" (in place of the prior "System default") for a
// session that is unbound from any rotation account — the system-default
// "account 0" whose account_id is empty. It surfaces on Session.account_label
// (the resolver-hydrated read-only label), in Session.rotation_events (the
// nested audit history, where a switch to the unbound account records the label
// into to_account and the free-text detail), and on the
// ProxySwitchSessionAccountResponse target_label / notice_text emitted when a
// switch targets the unbound account. A client pinned to an older version was
// built to render "System default" for the unbound case, so for any request
// resolved older than V20260706 this change rewrites the unmanaged label back to
// "System default" in all three places.
//
// For Session.account_label it keys the rewrite off the robust unbound predicate
// — account_id empty AND account_label == "Unmanaged local credentials" — so it
// never touches a real account that happens to be labeled similarly. For
// rotation_events (which a session BOUND to a real account can still carry from a
// prior switch to the unbound account) it rewrites the label-bearing to_account /
// from_account fields on exact equality and ReplaceAll's the exact phrase out of
// the free-text detail, independent of the session's own account_id. For the
// switch response (which carries no account_id) it keys off the target_label
// literal: the "Unmanaged local credentials" label is RESERVED at account
// create/update time (services/bossd's account handler rejects it case-insensitively),
// so the daemon only ever emits it for the unbound target, and the notice_text
// rewrite is gated on that same predicate.
//
// It targets the same OrchestratorService unary Session-bearing procedures
// OrphanedStateChange handles, plus ProxySwitchSessionAccount, keyed by the
// Connect procedure path. All other methods and message types are no-ops.
type UnmanagedLabelChange struct{}

// Version implements VersionChange. The change was introduced at V20260706, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705).
func (UnmanagedLabelChange) Version() Version { return V20260706 }

// downconvertUnmanagedLabelSession returns the Session to place in the response
// for a pre-V20260706 client. It restores "System default" in two places: the
// read-only account_label when s is unbound (empty account_id) and carries the
// "Unmanaged local credentials" label, AND any rotation_events whose label-bearing
// fields (to_account / from_account / the free-text detail) reference the unmanaged
// label — a session BOUND to a real account can still carry historical rotation
// events from a prior switch to the unbound account, so the rotation-event rewrite
// is independent of the top-level account_id predicate. If nothing needs rewriting
// s is returned unchanged. Cloning is essential: bosso's single-instance registry
// path holds the same *pb.Session pointers it caches, so mutating in place would
// corrupt the cached session (and race other readers); proto.Clone deep-copies the
// nested rotation_events too, so the clone's events are safe to mutate. Only
// sessions that actually carry the unmanaged label allocate, keeping the common
// path clone-free.
func downconvertUnmanagedLabelSession(s *pb.Session) *pb.Session {
	if s == nil {
		return s
	}
	labelNeedsRewrite := s.GetAccountId() == "" && s.GetAccountLabel() == unmanagedLocalCredentialsLabel
	if !labelNeedsRewrite && !rotationEventsHaveUnmanagedLabel(s.GetRotationEvents()) {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	if labelNeedsRewrite {
		lbl := systemDefaultAccountLabel
		clone.AccountLabel = &lbl
	}
	downconvertUnmanagedRotationEvents(clone.GetRotationEvents())
	return clone
}

// rotationEventsHaveUnmanagedLabel reports whether any event targets the
// "Unmanaged local credentials" label in a label-bearing field. Detail text is
// deliberately not a trigger because real labels may contain the same phrase.
func rotationEventsHaveUnmanagedLabel(evs []*pb.RotationEvent) bool {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if ev.GetToAccount() == unmanagedLocalCredentialsLabel ||
			ev.GetFromAccount() == unmanagedLocalCredentialsLabel {
			return true
		}
	}
	return false
}

// downconvertUnmanagedRotationEvents rewrites the unmanaged label back to "System
// default" in each rotation event's label-bearing fields, in place on the cloned
// events. to_account / from_account are exact account-label fields
// (switch_account.go records the target's label into to_account for a switch to
// the unbound account), so they are rewritten only on exact equality. Detail is a
// human-facing free-text line and may contain a real account label that includes
// the unmanaged phrase, so it is rewritten only when the event's exact account
// fields prove the event targeted the unbound account.
func downconvertUnmanagedRotationEvents(evs []*pb.RotationEvent) {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		targetsUnmanaged := ev.GetToAccount() == unmanagedLocalCredentialsLabel ||
			ev.GetFromAccount() == unmanagedLocalCredentialsLabel
		if ev.GetToAccount() == unmanagedLocalCredentialsLabel {
			ev.ToAccount = systemDefaultAccountLabel
		}
		if ev.GetFromAccount() == unmanagedLocalCredentialsLabel {
			ev.FromAccount = systemDefaultAccountLabel
		}
		if targetsUnmanaged && strings.Contains(ev.GetDetail(), unmanagedLocalCredentialsLabel) {
			ev.Detail = strings.ReplaceAll(ev.GetDetail(), unmanagedLocalCredentialsLabel, systemDefaultAccountLabel)
		}
	}
}

// downconvertUnmanagedSwitchResponse rewrites a ProxySwitchSessionAccountResponse
// in place for a pre-V20260706 client. It keys off the target_label literal, which
// the daemon emits ONLY for the unbound account — the "Unmanaged local credentials"
// label is reserved at account create/update time (see services/bossd's account
// handler), so no real account can carry it. When the switch targeted the unbound
// account the label is restored to "System default" and any occurrence of the
// unmanaged label in the human-facing notice_text is replaced with "System default";
// the notice_text rewrite is gated on the same target_label predicate so it only
// fires for a genuine unbound switch, never merely because the text contains the
// phrase. The response message is constructed fresh per request (a proxy
// passthrough, never a cached registry pointer), so in-place mutation is safe.
func downconvertUnmanagedSwitchResponse(m *pb.ProxySwitchSessionAccountResponse) {
	if m == nil || m.GetTargetLabel() != unmanagedLocalCredentialsLabel {
		return
	}
	m.TargetLabel = systemDefaultAccountLabel
	if strings.Contains(m.GetNoticeText(), unmanagedLocalCredentialsLabel) {
		m.NoticeText = strings.ReplaceAll(m.GetNoticeText(), unmanagedLocalCredentialsLabel, systemDefaultAccountLabel)
	}
}

// TransformResponse implements VersionChange. It restores the "System default"
// account label on each OrchestratorService response type that carries one or
// more Sessions (rewriting only response-local cloned copies so a shared registry
// pointer is never mutated), and on the ProxySwitchSessionAccount response. It is
// a no-op for any other method or payload type.
func (UnmanagedLabelChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertUnmanagedLabelSession) {
		return
	}
	switch method {
	case bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure:
		if m, ok := msg.(*pb.ProxySwitchSessionAccountResponse); ok {
			downconvertUnmanagedSwitchResponse(m)
		}
	}
}

// LimitedChatStatusChange is the production VersionChange introduced at V20260706.
//
// At V20260706 the OrchestratorService began serving ChatStatus
// CHAT_STATUS_LIMITED (5) and the derived Session display_label
// "usage-limited..." when an agent pane shows a usage-limit banner. Older
// clients were built before the enum/display branch existed. For those clients
// this transform maps LIMITED back to IDLE and rewrites the limited composite
// display back to the previous idle-style shape.
type LimitedChatStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260706, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705).
func (LimitedChatStatusChange) Version() Version { return V20260706 }

func downconvertLimitedSession(s *pb.Session) *pb.Session {
	if s == nil || !strings.HasPrefix(s.GetDisplayLabel(), "usage-limited") {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	// Recompute through a BASE cascade, NOT Compute: a client older than
	// V20260706 predates BOS-430's errored-recolor overlay (V20260718), so for a
	// BLOCKED (or ORPHANED) session the idle-style fallback must be the
	// un-recolored base cascade. Using Compute here would re-apply the DANGER
	// recolor and leak the new errored shape into a down-convert that is supposed
	// to hide it.
	//
	// ComputeBasePreDraftPRFailure rather than plain ComputeBase: recomputing
	// means this client inherits every LATER cascade change too, and BOS-855
	// (V20260812) moved "? PR failed" below the transient
	// setting-up/merging/archiving branches. Plain ComputeBase would hand a
	// pre-V20260706 client "initializing"/"merging"/"archiving" for a draft-PR
	// failure that used to read "? PR failed". DraftPRFailureLabelChange cannot
	// cover it: this session is usage-limited, which that transform exempts, so
	// it returns untouched long before this change runs last in the chain.
	out := displaystatus.ComputeBasePreDraftPRFailure(displaystatus.Input{
		Session:    clone,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	})
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

func downconvertLimitedChatStatusEntry(e *pb.ChatStatusEntry) *pb.ChatStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.ChatStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

func downconvertLimitedSessionStatusEntry(e *pb.SessionStatusEntry) *pb.SessionStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.SessionStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

func downconvertLimitedChatStatusDelta(d *pb.ChatStatusDelta) *pb.ChatStatusDelta {
	if d == nil || d.GetStatus() != pb.ChatStatus_CHAT_STATUS_LIMITED {
		return d
	}
	clone, ok := proto.Clone(d).(*pb.ChatStatusDelta)
	if !ok {
		return d
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_IDLE
	return clone
}

// TransformResponse implements VersionChange. It down-converts LIMITED chat
// status enum values and the corresponding Session display composite.
func (LimitedChatStatusChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertLimitedSession) {
		return
	}
	switch method {
	case bossanovav1connect.DaemonServiceGetChatStatusesProcedure:
		if m, ok := msg.(*pb.GetChatStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedChatStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetChatStatusesProcedure:
		if m, ok := msg.(*pb.ProxyGetChatStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedChatStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.DaemonServiceGetSessionStatusesProcedure:
		if m, ok := msg.(*pb.GetSessionStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedSessionStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertLimitedSessionStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure:
		if m, ok := msg.(*pb.ProxyChatListEvent); ok {
			if snapshot := m.GetSnapshot(); snapshot != nil {
				for i := range snapshot.Statuses {
					snapshot.Statuses[i] = downconvertLimitedChatStatusEntry(snapshot.Statuses[i])
				}
			}
			if statusDelta := downconvertLimitedChatStatusDelta(m.GetStatusDelta()); statusDelta != m.GetStatusDelta() {
				m.Event = &pb.ProxyChatListEvent_StatusDelta{StatusDelta: statusDelta}
			}
		}
	}
}

// WaitingChatStatusChange is the production VersionChange introduced at
// V20260804.
//
// At V20260804 the OrchestratorService began serving ChatStatus
// CHAT_STATUS_WAITING (6) — a chat blocked on an external event (a registered
// GitHub callback, a background poll tick) rather than computing — together
// with the accompanying waiting_reason string on ChatStatusEntry,
// ChatStatusDelta and SessionStatusEntry. Older clients were built before the
// enum value and the field existed and would render an unknown value blank, so
// for those clients this transform maps WAITING back to the prior observable
// status, CHAT_STATUS_WORKING, and clears waiting_reason.
//
// It has two legs. The status leg is DaemonService GetChatStatuses /
// GetSessionStatuses, the OrchestratorService ProxyGetSessionStatuses proxy of
// the latter, and the OrchestratorService ProxyStreamChats snapshot and status
// delta. The display leg is the FULL unary session-bearing OrchestratorService
// set — the fourteen procedures AgentStalledChange (the immediately preceding
// change) and ErroredStatusChange enumerate, streaming ProxyCreateSession
// excluded because the Interceptor only transforms unary responses. WAITING also
// reaches displaystatus.Compute, where it produces its own "waiting"/INFO/
// no-spinner composite on Session.display_label / display_intent /
// display_spinner, and that composite is PERSISTED on the sessions row, so every
// one of those procedures returns it and an old client would otherwise be handed
// a label it has never rendered. LimitedChatStatusChange covers only the first
// eight; that narrower set is a pre-existing gap in an already-released change,
// not a precedent to copy.
type WaitingChatStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260804, so
// it is applied to any request resolved to a strictly older version.
func (WaitingChatStatusChange) Version() Version { return V20260804 }

// downconvertWaitingChatStatusEntry returns the entry to place in the response
// for a pre-V20260804 client. Cloning is essential: in bosso's single-instance
// registry path the response holds the same pointers cached in the in-memory
// registry, so mutating in place would permanently erase the WAITING state
// every other (current) client should still see. Only waiting entries allocate.
func downconvertWaitingChatStatusEntry(e *pb.ChatStatusEntry) *pb.ChatStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.ChatStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_WORKING
	clone.WaitingReason = ""
	return clone
}

func downconvertWaitingSessionStatusEntry(e *pb.SessionStatusEntry) *pb.SessionStatusEntry {
	if e == nil || e.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.SessionStatusEntry)
	if !ok {
		return e
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_WORKING
	clone.WaitingReason = ""
	return clone
}

func downconvertWaitingChatStatusDelta(d *pb.ChatStatusDelta) *pb.ChatStatusDelta {
	if d == nil || d.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING {
		return d
	}
	clone, ok := proto.Clone(d).(*pb.ChatStatusDelta)
	if !ok {
		return d
	}
	clone.Status = pb.ChatStatus_CHAT_STATUS_WORKING
	clone.WaitingReason = ""
	return clone
}

// downconvertWaitingSession restores the display composite a pre-V20260804
// client used to see for a session whose most notable chat is parked on an
// external event: that chat reported CHAT_STATUS_WORKING, so the old cascade
// rendered plain "working". The recompute therefore feeds WORKING back in, which
// also re-derives the intent (BOS-236 recolors a working session DANGER when its
// PR needs a fix) rather than hard-coding SUCCESS.
//
// It recomputes through displaystatus.Compute, NOT ComputeBase — the opposite of
// downconvertLimitedSession, and for a positional reason. Changes.Apply runs
// newest-first, and this change sits near the newest end, so it runs BEFORE
// every older transform (V20260812's draft-PR change is newer still and runs
// first, but it only ever rewrites a draft-PR-failure session's label away from
// this one's exact-match guard, so the two never contend): emitting the full
// Current composite (BOS-430's errored recolor
// included) is correct, because a client newer than V20260718 must still see
// that recolor, and a Baseline client has ErroredStatusChange applied
// afterwards to strip it. The limited transform is last in the chain and so must
// avoid re-introducing what earlier transforms already removed.
//
// The match is on the exact waiting label rather than a prefix: the waiting
// composite is generated by a single displaystatus branch with no suffix, and a
// loose match would rewrite unrelated labels into "working".
func downconvertWaitingSession(s *pb.Session) *pb.Session {
	if s == nil || s.GetDisplayLabel() != displaystatus.WaitingLabel {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	out := displaystatus.Compute(displaystatus.Input{
		Session:    clone,
		ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
	})
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

// TransformResponse implements VersionChange. It down-converts WAITING chat
// status enum values, clears the accompanying waiting_reason, and restores the
// pre-waiting display composite on every session-bearing response.
func (WaitingChatStatusChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertWaitingSession) {
		return
	}
	switch method {
	case bossanovav1connect.DaemonServiceGetChatStatusesProcedure:
		if m, ok := msg.(*pb.GetChatStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertWaitingChatStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyGetChatStatusesProcedure:
		if m, ok := msg.(*pb.ProxyGetChatStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertWaitingChatStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.DaemonServiceGetSessionStatusesProcedure:
		if m, ok := msg.(*pb.GetSessionStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertWaitingSessionStatusEntry(m.Statuses[i])
			}
		}
	// The Orchestrator proxy of the same call. This is the leg that actually
	// runs in production: apiversion.Interceptor is only ever installed on the
	// OrchestratorService handler (services/bosso/cmd/main.go), so the two
	// DaemonService cases above are defensive cover for a future daemon-side
	// server interceptor while THIS case is the one a live pre-V20260804 client
	// — the boss TUI in cloud mode, the MCP gateway — reaches. Omitting it would
	// hand that client an unknown enum value (rendered blank) plus a
	// waiting_reason string it was never built to display.
	case bossanovav1connect.OrchestratorServiceProxyGetSessionStatusesProcedure:
		if m, ok := msg.(*pb.ProxyGetSessionStatusesResponse); ok {
			for i := range m.Statuses {
				m.Statuses[i] = downconvertWaitingSessionStatusEntry(m.Statuses[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure:
		if m, ok := msg.(*pb.ProxyChatListEvent); ok {
			if snapshot := m.GetSnapshot(); snapshot != nil {
				for i := range snapshot.Statuses {
					snapshot.Statuses[i] = downconvertWaitingChatStatusEntry(snapshot.Statuses[i])
				}
			}
			if statusDelta := downconvertWaitingChatStatusDelta(m.GetStatusDelta()); statusDelta != m.GetStatusDelta() {
				m.Event = &pb.ProxyChatListEvent_StatusDelta{StatusDelta: statusDelta}
			}
		}
	}
}

// DraftPRFailureLabelChange is the production VersionChange introduced at
// V20260812.
//
// At V20260812 the OrchestratorService stopped letting a session-level
// draft-PR-creation failure claim the row's primary display composite while a
// chat is live (BOS-855). The "? PR failed" cascade branch moved from directly
// below the QUESTION/LIMITED chat branches to immediately below WORKING, so a
// session whose draft PR failed now serves "working" / "waiting" /
// "initializing" / "merging" / "archiving" on Session.display_label /
// display_intent / display_spinner whenever one of those is true, instead of
// "? PR failed". A client pinned to an older version was built against the old
// precedence and expects the failure to own the label, so this change restores
// it (see displaystatus.PreDraftPRFailureOutput).
//
// It targets the same FULL unary session-bearing OrchestratorService set that
// WaitingChatStatusChange's display leg enumerates — the composite is PERSISTED
// on the sessions row, so every one of those procedures can return it — PLUS the
// created message in the streaming ProxyCreateSession, exactly as
// ErroredStatusChange does. Streaming is NOT exempt: the Interceptor's
// WrapStreamingHandler wraps the connection in a transformingStreamingHandlerConn
// whose Send calls Changes.Apply on every streamed message (interceptor.go). Omit
// it and a pre-V20260718 client gets ErroredStatusChange applied over a label
// this change never restored — a composite no server version ever emitted.
//
// Hints are deliberately NOT down-converted: an older client receives
// "? PR failed" as its label AND the new draft-PR warning hint, so the failure is
// duplicated rather than lost. Suppressing hints per-version would push
// client-version logic into the hint producers.
type DraftPRFailureLabelChange struct{}

// Version implements VersionChange. The change was introduced at V20260812, so
// it is applied to any request resolved to a strictly older version.
func (DraftPRFailureLabelChange) Version() Version { return V20260812 }

// downconvertDraftPRFailureSession returns the Session to place in the response
// for a pre-V20260812 client. Cloning is essential: in bosso's single-instance
// registry path the response holds the same pointers cached in the in-memory
// registry, so mutating in place would permanently overwrite the live composite
// every other (current) client should still see. Only sessions whose composite
// actually changes allocate, keeping the common path clone-free.
//
// The whole decision — including the "is this blocked reason a draft-PR-creation
// failure" test — lives in displaystatus.PreDraftPRFailureOutput. That is
// deliberate: keeping the sessionreason guard inside displaystatus is what stops
// this package acquiring a dependency on lib/bossalib/sessionreason.
func downconvertDraftPRFailureSession(s *pb.Session) *pb.Session {
	if s == nil {
		return s
	}
	out := displaystatus.PreDraftPRFailureOutput(s)
	if out.Label == s.GetDisplayLabel() &&
		out.Intent == s.GetDisplayIntent() &&
		out.Spinner == s.GetDisplaySpinner() {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

// TransformResponse implements VersionChange. It restores the pre-BOS-855
// "? PR failed" composite on every session-bearing response — the unary set and
// the streamed ProxyCreateSession created message alike.
func (DraftPRFailureLabelChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertDraftPRFailureSession) {
		return
	}
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure:
		if m, ok := msg.(*pb.ProxyCreateSessionResponse); ok {
			if created, ok := m.Body.(*pb.ProxyCreateSessionResponse_Created); ok {
				created.Created = downconvertDraftPRFailureSession(created.Created)
			}
		}
	}
}

// GateFailedOutcomeChange is the production VersionChange introduced at
// V20260816.
//
// At V20260816 the OrchestratorService began distinguishing a cron gate that
// could NOT be evaluated from one that ran and decided there was no work
// (BOS-881). A gate that timed out, could not be launched, or was reported
// missing/unrunnable by the shell (exit 127 / 126) now serves
// CronJob.last_run_outcome "gate_failed" and the derived
// CronJob.last_run_status CRON_JOB_STATUS_FAILED, and RunCronJobNow returns the
// matching "gate_failed" skip reason. Before this change all four of those
// blocked-fire causes were flattened into "gated" / CRON_JOB_STATUS_GATED — a
// warning-styled "waiting, healthy" value, which is why a repo-wide broken PATH
// read as a quiet backlog sweep for hours.
//
// No CronJobStatus enum member was added; "gate_failed" reuses the existing
// CRON_JOB_STATUS_FAILED value. The behavior change is therefore in the VALUE
// served, not the schema — exactly the case this versioning mechanism exists
// for. A client pinned to an older version was built against the prior values,
// so for any request resolved older than V20260816 this change restores them:
// outcome "gated", status CRON_JOB_STATUS_GATED, skip reason "gated".
//
// It targets every OrchestratorService procedure that can carry a CronJob
// (ProxyListCronJobs, ProxyCreateCronJob, ProxyUpdateCronJob, ProxyGetCronJob)
// plus ProxyRunCronJobNow for the skip reason. All are unary. All other methods
// and message types are no-ops.
type GateFailedOutcomeChange struct{}

// Version implements VersionChange. The change was introduced at V20260816, so
// it is applied to any request resolved to a strictly older version.
func (GateFailedOutcomeChange) Version() Version { return V20260816 }

// Wire values involved in GateFailedOutcomeChange (V20260816). They are raw
// literals rather than an import of lib/bossalib/models because this package
// pins the exact wire strings both sides emit; models.CronJobOutcome is the
// producer's vocabulary, and coupling the transform to it would let a later
// rename silently change what older clients receive.
const (
	// cronOutcomeGateFailed is the CURRENT (V20260816+) outcome for a gate that
	// could not be evaluated. It mirrors models.CronJobOutcomeGateFailed.
	cronOutcomeGateFailed = "gate_failed"
	// cronOutcomeGated is the PRIOR value older clients were built to see for
	// that case; the transform restores it. It mirrors models.CronJobOutcomeGated.
	cronOutcomeGated = "gated"
)

// downconvertGateFailedCronJob returns the CronJob to place in the response for
// a pre-V20260816 client. Jobs whose outcome is not "gate_failed" are returned
// unchanged, keeping the common path clone-free. Cloning matters for the same
// reason it does on the Session transforms: a response may hold pointers that
// are also cached or shared, so the down-convert must never mutate in place.
//
// last_run_status is rewritten only when it is FAILED. A gate_failed job whose
// PRIOR run's session is still live derives RUNNING from the liveness branch in
// cronJobStatus, and an older server would have derived RUNNING there too — so
// clamping every gate_failed job to GATED would invent a shape no server ever
// served.
func downconvertGateFailedCronJob(j *pb.CronJob) *pb.CronJob {
	if j == nil || j.GetLastRunOutcome() != cronOutcomeGateFailed {
		return j
	}
	clone, ok := proto.Clone(j).(*pb.CronJob)
	if !ok {
		return j
	}
	clone.LastRunOutcome = cronOutcomeGated
	if clone.GetLastRunStatus() == pb.CronJobStatus_CRON_JOB_STATUS_FAILED {
		clone.LastRunStatus = pb.CronJobStatus_CRON_JOB_STATUS_GATED
	}
	return clone
}

// downconvertGateFailedCronJobWithDaemon rewrites only the wrapped job, leaving
// the daemon routing fields untouched. It clones the wrapper because the nested
// job pointer is replaced.
func downconvertGateFailedCronJobWithDaemon(e *pb.CronJobWithDaemon) *pb.CronJobWithDaemon {
	if e == nil {
		return e
	}
	job := downconvertGateFailedCronJob(e.GetJob())
	if job == e.GetJob() {
		return e
	}
	clone, ok := proto.Clone(e).(*pb.CronJobWithDaemon)
	if !ok {
		return e
	}
	clone.Job = job
	return clone
}

// TransformResponse implements VersionChange. It restores the pre-BOS-881
// "gated" outcome, GATED status, and "gated" skip reason on every
// OrchestratorService response that can carry them. It is a no-op for any other
// method or payload type.
func (GateFailedOutcomeChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListCronJobsProcedure:
		if m, ok := msg.(*pb.ProxyListCronJobsResponse); ok {
			for i := range m.Jobs {
				m.Jobs[i] = downconvertGateFailedCronJobWithDaemon(m.Jobs[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyCreateCronJobProcedure:
		if m, ok := msg.(*pb.ProxyCreateCronJobResponse); ok {
			m.Job = downconvertGateFailedCronJob(m.GetJob())
		}
	case bossanovav1connect.OrchestratorServiceProxyUpdateCronJobProcedure:
		if m, ok := msg.(*pb.ProxyUpdateCronJobResponse); ok {
			m.Job = downconvertGateFailedCronJob(m.GetJob())
		}
	case bossanovav1connect.OrchestratorServiceProxyGetCronJobProcedure:
		if m, ok := msg.(*pb.ProxyGetCronJobResponse); ok {
			m.CronJob = downconvertGateFailedCronJob(m.GetCronJob())
		}
	case bossanovav1connect.OrchestratorServiceProxyRunCronJobNowProcedure:
		if m, ok := msg.(*pb.ProxyRunCronJobNowResponse); ok {
			if m.GetSkippedReason() == cronOutcomeGateFailed {
				m.SkippedReason = cronOutcomeGated
			}
		}
	}
}

// RefreshChainUnprovenOutcomeChange is the production VersionChange introduced
// at V20260914.
//
// At V20260914 the OrchestratorService began distinguishing a credential check
// that ran cleanly AND proved nothing about the credential's refresh chain from
// one that ran cleanly and is simply healthy (BOS-1174). A check that completed
// with no provider error, on a credential whose own access token says a token
// refresh should already have happened, and whose run observed no credential
// write, now serves Account.auth_check.outcome "refresh_chain_unproven" with
// failure_class "refresh_not_observed". Before this change that identical clean
// run served "healthy" with an empty failure_class.
//
// No enum member was added — auth_check.outcome is a plain string — so the
// behavior change is in the VALUE served, exactly the case GateFailedOutcomeChange
// (V20260816) and StaleCheckStateChange (V20260825) were versioned for. It is
// observable because clients switch on that string to pick a severity
// (services/web/src/lib/accountRows.ts checkSeverity, and the boss TUI's
// accountCheckSeverity): an older build that has never seen the new token maps
// it to its unknown-outcome default and flips a green "healthy" pill to
// undetermined for an account whose behavior did not change. A client pinned to
// an older version was built against the prior pair, so for any request
// resolved older than V20260914 this change restores it: outcome "healthy",
// failure_class "".
//
// It targets every OrchestratorService procedure that can carry an Account
// (ProxyListAccounts, ProxyManageListAccounts, ProxyAddAccount,
// ProxyRefreshAccount, ProxyUpdateAccount, ProxyTestAccount). All are unary.
// All other methods and message types are no-ops.
type RefreshChainUnprovenOutcomeChange struct{}

// Version implements VersionChange. The change was introduced at V20260913, so
// it is applied to any request resolved to a strictly older version.
func (RefreshChainUnprovenOutcomeChange) Version() Version { return V20260914 }

// Wire values involved in RefreshChainUnprovenOutcomeChange (V20260914). They
// are raw literals rather than an import of lib/bossalib/models for the same
// reason the cron outcomes above are: this package pins the exact wire strings
// both sides emit, and coupling the transform to the producer's vocabulary
// would let a later rename silently change what older clients receive.
const (
	// authOutcomeRefreshChainUnproven is the CURRENT (V20260914+) outcome for a
	// clean check that could not prove the refresh chain. It mirrors
	// models.AuthCheckOutcomeRefreshChainUnproven.
	authOutcomeRefreshChainUnproven = "refresh_chain_unproven"
)

// downconvertRefreshChainUnprovenAccount returns the Account to place in the
// response for a pre-V20260914 client. Accounts whose check outcome is not
// "refresh_chain_unproven" are returned unchanged, keeping the common path
// clone-free. Cloning matters for the same reason it does on the Session and
// CronJob transforms: a response may hold pointers that are also cached or
// shared, so the down-convert must never mutate in place.
//
// failure_class is cleared alongside the outcome because the prior clean run
// served an empty class; leaving "refresh_not_observed" behind on a "healthy"
// outcome would invent a pair no server ever served.
func downconvertRefreshChainUnprovenAccount(a *pb.Account) *pb.Account {
	if a == nil || a.GetAuthCheck().GetOutcome() != authOutcomeRefreshChainUnproven {
		return a
	}
	clone, ok := proto.Clone(a).(*pb.Account)
	if !ok {
		return a
	}
	clone.AuthCheck.Outcome = authOutcomeHealthy
	clone.AuthCheck.FailureClass = ""
	return clone
}

// TransformResponse implements VersionChange. It restores the pre-BOS-1174
// "healthy" outcome and empty failure class on every OrchestratorService
// response that can carry an Account. It is a no-op for any other method or
// payload type.
func (RefreshChainUnprovenOutcomeChange) TransformResponse(method string, msg any) {
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyListAccountsProcedure:
		if m, ok := msg.(*pb.ProxyListAccountsResponse); ok {
			for i := range m.Accounts {
				m.Accounts[i] = downconvertRefreshChainUnprovenAccount(m.Accounts[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyManageListAccountsProcedure:
		if m, ok := msg.(*pb.ProxyManageListAccountsResponse); ok {
			for i := range m.Accounts {
				m.Accounts[i] = downconvertRefreshChainUnprovenAccount(m.Accounts[i])
			}
		}
	case bossanovav1connect.OrchestratorServiceProxyAddAccountProcedure:
		if m, ok := msg.(*pb.ProxyAddAccountResponse); ok {
			m.Account = downconvertRefreshChainUnprovenAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyRefreshAccountProcedure:
		if m, ok := msg.(*pb.ProxyRefreshAccountResponse); ok {
			m.Account = downconvertRefreshChainUnprovenAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyUpdateAccountProcedure:
		if m, ok := msg.(*pb.ProxyUpdateAccountResponse); ok {
			m.Account = downconvertRefreshChainUnprovenAccount(m.GetAccount())
		}
	case bossanovav1connect.OrchestratorServiceProxyTestAccountProcedure:
		if m, ok := msg.(*pb.ProxyTestAccountResponse); ok {
			m.Account = downconvertRefreshChainUnprovenAccount(m.GetAccount())
		}
	}
}

// NoEligibleAccountChange is the production VersionChange introduced at V20260711.
//
// At V20260711 the OrchestratorService began serving the RotationOutcome value
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT (6) on
// Session.rotation_events[].outcome — a new terminal state raised when rotation
// is capable but no account is eligible to switch to (all disabled/failed). It
// splits the prior single "status only" outcome so operators can tell "no active
// account" apart from "agent cannot rotate". A client pinned to an older version
// was built before this value existed, so for any request resolved older than
// V20260711 this change rewrites the new outcome back to the prior observable
// value, ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.
//
// It targets the same OrchestratorService unary Session-bearing procedures
// OrphanedStateChange handles, keyed by the Connect procedure path. All other
// methods and message types are no-ops.
type NoEligibleAccountChange struct{}

// Version implements VersionChange. The change was introduced at V20260711, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705, V20260706).
func (NoEligibleAccountChange) Version() Version { return V20260711 }

// downconvertNoEligibleSession returns the Session to place in the response for a
// pre-V20260711 client. If no rotation event carries outcome
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT it returns s unchanged;
// otherwise it returns a CLONE whose every such event's outcome is reset to the
// prior observable value ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY. Cloning is
// essential: bosso's single-instance registry path holds the same *pb.Session
// pointers it caches, so mutating in place would corrupt the cached session (and
// race other readers); proto.Clone deep-copies the nested rotation_events so the
// clone's events are safe to mutate. Only sessions that actually carry the new
// outcome allocate, keeping the common path clone-free.
func downconvertNoEligibleSession(s *pb.Session) *pb.Session {
	if s == nil || !rotationEventsHaveNoEligible(s.GetRotationEvents()) {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	for _, ev := range clone.GetRotationEvents() {
		if ev.GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
			ev.Outcome = pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY
		}
	}
	return clone
}

// rotationEventsHaveNoEligible reports whether any event carries the new
// ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT outcome.
func rotationEventsHaveNoEligible(evs []*pb.RotationEvent) bool {
	for _, ev := range evs {
		if ev.GetOutcome() == pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT {
			return true
		}
	}
	return false
}

// TransformResponse implements VersionChange. It down-converts the new
// no-eligible rotation outcome on each OrchestratorService response type that
// carries one or more Sessions, matched by procedure path, rewriting only
// response-local (cloned) copies so a shared registry pointer is never mutated.
// It is a no-op for any other method or payload type.
func (NoEligibleAccountChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertNoEligibleSession)
}

// ErroredStatusChange is the production VersionChange introduced at V20260718.
//
// At V20260718 the OrchestratorService began serving the BOS-430 errored-recolor
// display shape for orphaned/blocked sessions on Session.display_label /
// display_intent / display_spinner. An errored session now keeps its REAL
// underlying status label and spinner (a live "working"/spinner, a pending
// "? question") but has its intent recolored to DANGER. Before this change an
// ORPHANED session collapsed to a fixed "orphaned" / DANGER / no-spinner tuple
// (which won over even a merged/closed PR), and a BLOCKED session had no overlay
// at all — it showed the plain base cascade with the base intent.
//
// A client pinned to an older version was built against those prior shapes, so
// for any request resolved older than V20260718 this change restores them via
// displaystatus.PreErroredOutput: the fixed "orphaned"/DANGER tuple for ORPHANED
// and the un-recolored base intent for BLOCKED (label and spinner are already
// the base values there).
//
// It targets every OrchestratorService response that embeds one or more
// *pb.Session messages, keyed by the Connect procedure path: the read/lifecycle
// set OrphanedStateChange handles (ListSessions, GetSession, Stop, Pause, Resume,
// Merge, Archive, TransferSession), the remaining Session-returning procedures
// (RetrySession, UpdateSession, LinkSessionPR, RunCronJobNow, CloseSession,
// ResurrectSession), and the created message in the ProxyCreateSession stream —
// so an errored session crossing the version boundary is down-converted no
// matter which RPC served it. Because Apply runs newest→oldest, this change runs
// BEFORE OrphanedStateChange for a Baseline client, so it observes the still-
// ORPHANED state before that older transform rewrites it to IMPLEMENTING_PLAN.
// All other methods and message types are no-ops.
type ErroredStatusChange struct{}

// Version implements VersionChange. The change was introduced at V20260718, so
// it is applied to any request resolved to a strictly older version.
func (ErroredStatusChange) Version() Version { return V20260718 }

// downconvertErroredSession returns the Session to place in the response for a
// pre-V20260718 client. For an orphaned/blocked session it returns a CLONE whose
// Display* fields are reset to the pre-BOS-430 shape (displaystatus.PreErroredOutput);
// otherwise, or when the shape is already the prior one (e.g. an un-computed
// empty label, or a blocked muted-terminal PR that was never recolored), it
// returns s unchanged. Cloning is essential and mirrors downconvertOrphanedSession:
// bosso's single-instance registry path holds the same *pb.Session pointers cached
// in memory, so mutating in place would corrupt the cached session and race other
// readers. Only sessions that actually change allocate, keeping the common path
// clone-free.
func downconvertErroredSession(s *pb.Session) *pb.Session {
	if s == nil {
		return s
	}
	state := s.GetState()
	if state != pb.SessionState_SESSION_STATE_ORPHANED &&
		state != pb.SessionState_SESSION_STATE_BLOCKED {
		return s
	}
	out := displaystatus.PreErroredOutput(s)
	if out.Label == s.GetDisplayLabel() &&
		out.Intent == s.GetDisplayIntent() &&
		out.Spinner == s.GetDisplaySpinner() {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	clone.DisplayLabel = out.Label
	clone.DisplayIntent = out.Intent
	clone.DisplaySpinner = out.Spinner
	return clone
}

// TransformResponse implements VersionChange. It down-converts the errored
// display shape on each OrchestratorService response type that carries one or
// more Sessions, matched by procedure path, rewriting only response-local
// (cloned) copies so a shared registry pointer is never mutated. It is a no-op
// for any other method or payload type.
func (ErroredStatusChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertErroredSession) {
		return
	}
	switch method {
	case bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure:
		if m, ok := msg.(*pb.ProxyCreateSessionResponse); ok {
			if created, ok := m.Body.(*pb.ProxyCreateSessionResponse_Created); ok {
				created.Created = downconvertErroredSession(created.Created)
			}
		}
	}
}

// RespawnSameAccountOutcomeChange is the production VersionChange introduced at
// V20260723.
//
// At V20260723 the OrchestratorService began serving two new RotationOutcome
// values on Session.rotation_events[].outcome:
// ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT (7), audited when the BOS-482 healer
// stops and respawns a pane in place under its SAME account (pane auth-failed
// but the bound account probes healthy), and
// ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED (8), audited when the per-chat
// respawn-in-place budget is spent for the window. A client pinned to an older
// version was built before these values existed, so for any request resolved
// older than V20260723 this change rewrites BOTH back to the prior observable
// value, ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.
//
// It targets the same OrchestratorService unary Session-bearing procedures
// OrphanedStateChange handles, keyed by the Connect procedure path. All other
// methods and message types are no-ops.
type RespawnSameAccountOutcomeChange struct{}

// Version implements VersionChange. The change was introduced at V20260723, so
// it is applied to any request resolved to a strictly older version (Baseline,
// V20260704, V20260705, V20260706, V20260711, V20260718).
func (RespawnSameAccountOutcomeChange) Version() Version { return V20260723 }

// downconvertRespawnSession returns the Session to place in the response for a
// pre-V20260723 client. If no rotation event carries outcome
// ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT or ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED
// it returns s unchanged; otherwise it returns a CLONE whose every such event's
// outcome is reset to the prior observable value
// ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY. Cloning is essential: bosso's
// single-instance registry path holds the same *pb.Session pointers it caches, so
// mutating in place would corrupt the cached session (and race other readers);
// proto.Clone deep-copies the nested rotation_events so the clone's events are
// safe to mutate. Only sessions that actually carry a new outcome allocate,
// keeping the common path clone-free.
func downconvertRespawnSession(s *pb.Session) *pb.Session {
	if s == nil || !rotationEventsHaveRespawn(s.GetRotationEvents()) {
		return s
	}
	clone, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	for _, ev := range clone.GetRotationEvents() {
		switch ev.GetOutcome() {
		case pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
			pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED:
			ev.Outcome = pb.RotationOutcome_ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY
		default:
			// Every other outcome predates V20260723 and is passed through
			// unchanged.
		}
	}
	return clone
}

// rotationEventsHaveRespawn reports whether any event carries one of the new
// BOS-482 respawn outcomes.
func rotationEventsHaveRespawn(evs []*pb.RotationEvent) bool {
	for _, ev := range evs {
		switch ev.GetOutcome() {
		case pb.RotationOutcome_ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT,
			pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED:
			return true
		default:
			// Not a respawn outcome — keep scanning.
		}
	}
	return false
}

// TransformResponse implements VersionChange. It down-converts the new
// respawn/cap-exhausted rotation outcomes on each OrchestratorService response
// type that carries one or more Sessions, matched by procedure path, rewriting
// only response-local (cloned) copies so a shared registry pointer is never
// mutated. It is a no-op for any other method or payload type.
func (RespawnSameAccountOutcomeChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertRespawnSession)
}

// ReferenceChange is an example VersionChange introduced at V20260701. It
// demonstrates the VersionChange contract end-to-end and serves as the
// reference implementation for contributors adding real transforms.
//
// It targets the synthetic method "demo.Greet" and rewrites RefMsg.Greeting to
// prefix "[v1] ", simulating a backward-compatible response rewrite for clients
// pinned to the Baseline version. Real version changes follow this same pattern,
// type-asserting msg against generated bossanova.v1 message types.
type ReferenceChange struct{}

// Version implements VersionChange. The reference change was introduced at
// V20260701, so it is applied to any request resolved to Baseline.
func (ReferenceChange) Version() Version { return V20260701 }

// TransformResponse implements VersionChange. It acts only on the "demo.Greet"
// method with a *RefMsg payload; all other calls are no-ops.
func (ReferenceChange) TransformResponse(method string, msg any) {
	if method != "demo.Greet" {
		return
	}
	m, ok := msg.(*RefMsg)
	if !ok {
		return
	}
	m.Greeting = "[v1] " + m.Greeting
}

// SwitchDeadlineCodeChange is the production VersionChange introduced at
// V20260820, and the mechanism's FIRST error-path transform.
//
// At V20260820 the OrchestratorService began serving
// connect.CodeDeadlineExceeded for a ProxySwitchSessionAccount that the
// DAEMON's own switch budget ended (BOS-947). Before this change the daemon had
// no wire value for "a deadline stopped this": the relayed CommandResult
// carried ERROR_CODE_UNSPECIFIED and bosso's validateCommandResult fell through
// to connect.CodeAborted. That was not merely vaguer — CodeAborted invites a
// retry, and BOS-747's rule is that a request killed by its own deadline must
// not be retried. The daemon now emits the new
// CommandResult_ERROR_CODE_DEADLINE_EXCEEDED for that case.
//
// The behavior change is in the CODE served on an existing procedure, not the
// schema — exactly the case this versioning mechanism exists for. A client
// pinned to an older version was built when this case read as CodeAborted, so
// for any request resolved older than V20260820 this change restores that code
// with the message preserved.
//
// SCOPE — this is the part to read before touching the match. The transform is
// discriminated by BOTH the procedure AND the relayed-daemon-deadline marker, and
// the marker is the load-bearing half. bosso's dispatchOwnerCommand maps its
// OWN commandDeadline/switchCommandDeadline expiry on this same procedure to
// CodeDeadlineExceeded, and has done so since long before V20260820. Matching
// on the procedure alone would down-convert that pre-existing, correct answer
// too and hand old clients a CodeAborted the server never used to send — a
// regression introduced by the compatibility layer itself. Broadening this
// match is a blocking review finding, not a simplification.
//
// It implements ErrorTransform only. TransformResponse is a no-op: there is no
// success-path shape to down-convert, and the change must not touch one.
type SwitchDeadlineCodeChange struct{}

// Version implements VersionChange. The change was introduced at V20260820, so
// it is applied to any request resolved to a strictly older version.
func (SwitchDeadlineCodeChange) Version() Version { return V20260820 }

// TransformResponse implements VersionChange. Deliberately a no-op: this change
// lives entirely on the error path (see TransformError). It exists only because
// VersionChange requires it for membership in ProductionChanges, and it is what
// the ApplyError/Apply split tests pin — a success response must never be
// touched by an error-path change.
func (SwitchDeadlineCodeChange) TransformResponse(string, any) {}

// TransformError implements ErrorTransform. It rewrites ONLY a
// ProxySwitchSessionAccount error carrying the relayed-daemon-deadline marker,
// restoring the pre-V20260820 connect.CodeAborted with the original message
// intact. Every other procedure, an unmarked CodeDeadlineExceeded on this same
// procedure (bosso's own relay timeout), a non-Connect error, and a nil error
// are all returned unchanged.
func (SwitchDeadlineCodeChange) TransformError(method string, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure {
		return err
	}
	if !IsRelayedDaemonDeadline(err) {
		return err
	}
	// Preserve the daemon's own message; only the code changes. connect.Error's
	// Message() is what a client renders, and the pre-V20260820 CodeAborted
	// carried exactly this string via validateCommandResult's default arm.
	//
	// Message-only reconstruction deliberately drops Meta() and Details(). The
	// error this targets is built by validateCommandResult from a relayed
	// CommandResult and carries neither, so there is nothing to lose here — but
	// a future ErrorTransform copied from this one may target an error that
	// does, and it must copy them across rather than inherit this shortcut.
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		// Marked but not a Connect error: nothing meaningful to down-convert to,
		// and inventing a code here would be worse than passing it through.
		return err
	}
	return connect.NewError(connect.CodeAborted, errors.New(connectErr.Message()))
}

const legacySwitchResultCeilingMessage = "command timed out after 2m0s"

// SwitchResultCeilingMessageChange is the production VersionChange introduced
// at V20260821.
//
// At V20260821 ProxySwitchSessionAccount began serving a self-describing
// message when BOSSO's own result ceiling stops waiting before a daemon verdict
// arrives: the account switch may still be running, and the request did not
// cancel or tear it down. Before that change this path looked like the generic
// relay timeout, "command timed out after 2m0s". The code remains
// connect.CodeDeadlineExceeded in both versions; only the observable message
// changed.
//
// The match is deliberately both procedure-scoped and marker-scoped. The same
// procedure can also return a relayed daemon deadline, and dispatchOwnerCommand
// can produce generic unmarked relay timeouts. Neither behavior changed at
// V20260821.
type SwitchResultCeilingMessageChange struct{}

// Version implements VersionChange. The change was introduced at V20260821, so
// it is applied to any request resolved to a strictly older version.
func (SwitchResultCeilingMessageChange) Version() Version { return V20260821 }

// TransformResponse implements VersionChange. Deliberately a no-op: this
// change lives entirely on the error path.
func (SwitchResultCeilingMessageChange) TransformResponse(string, any) {}

// TransformError implements ErrorTransform. It rewrites ONLY the handler-owned
// ProxySwitchSessionAccount result-ceiling error back to the legacy timeout
// text older clients saw before V20260821, preserving CodeDeadlineExceeded.
func (SwitchResultCeilingMessageChange) TransformError(method string, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure {
		return err
	}
	if !IsSwitchResultCeilingExceeded(err) {
		return err
	}
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		return err
	}
	return connect.NewError(connect.CodeDeadlineExceeded, errors.New(legacySwitchResultCeilingMessage))
}

// SwitchCanceledCodeChange is the production VersionChange introduced at
// V20260821.
//
// At V20260821 the OrchestratorService began serving connect.CodeCanceled for a
// ProxySwitchSessionAccount that the caller cancelled while the DAEMON was
// executing the switch (BOS-958). Before this change the daemon had no wire
// value for "the caller abandoned this": the relayed CommandResult carried
// ERROR_CODE_UNSPECIFIED and bosso's validateCommandResult fell through to
// connect.CodeAborted. That was not merely vaguer — CodeAborted invites a retry,
// and retrying a caller-cancelled switch can stack duplicate work on top of a
// partially completed first attempt.
//
// The behavior change is in the CODE served on an existing procedure, not the
// schema. A client pinned to an older version was built when this case read as
// CodeAborted, so for any request resolved older than V20260821 this change
// restores that code with the message preserved.
//
// SCOPE — match both the procedure and the relayed-daemon-canceled marker.
// bosso's dispatchOwnerCommand maps its OWN context.Canceled path on this same
// procedure to CodeCanceled, and it did so before V20260821. Matching on the
// procedure alone would down-convert that pre-existing, correct answer too and
// hand old clients a CodeAborted the server never used to send.
//
// It implements ErrorTransform only. TransformResponse is a no-op: there is no
// success-path shape to down-convert, and the change must not touch one.
type SwitchCanceledCodeChange struct{}

// Version implements VersionChange. The change was introduced at V20260821, so
// it is applied to any request resolved to a strictly older version.
func (SwitchCanceledCodeChange) Version() Version { return V20260821 }

// TransformResponse implements VersionChange. Deliberately a no-op: this change
// lives entirely on the error path (see TransformError).
func (SwitchCanceledCodeChange) TransformResponse(string, any) {}

// TransformError implements ErrorTransform. It rewrites ONLY a
// ProxySwitchSessionAccount error carrying the relayed-daemon-canceled marker,
// restoring the pre-V20260821 connect.CodeAborted with the original message
// intact. Every other procedure, an unmarked CodeCanceled on this same
// procedure (bosso's own cancellation), a non-Connect error, and a nil error are
// all returned unchanged.
func (SwitchCanceledCodeChange) TransformError(method string, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceProxySwitchSessionAccountProcedure {
		return err
	}
	if !IsRelayedDaemonCanceled(err) {
		return err
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return err
	}
	return connect.NewError(connect.CodeAborted, errors.New(connectErr.Message()))
}

const legacySwitchActiveOrganizationMessage = "organization management is not implemented"

// SwitchActiveOrganizationRetiredMessageChange is the production VersionChange
// introduced at V20260903.
//
// SwitchActiveOrganization still exists in bossanova.v1 for FILE-level breaking
// compatibility, but current clients should switch organizations with AuthKit
// switchToOrganization and receive an explicit retirement message. Older clients
// were built when the organization-management surface returned the generic
// unimplemented message, so this restores that message for them while preserving
// CodeUnimplemented.
//
// The match is procedure-scoped and marker-scoped, and it checks only the
// Connect code. It deliberately never inspects message text: the marker is the
// handler's declaration that this is the retired procedure path.
type SwitchActiveOrganizationRetiredMessageChange struct{}

// Version implements VersionChange. The change was introduced at V20260903, so
// it is applied to any request resolved to a strictly older version.
func (SwitchActiveOrganizationRetiredMessageChange) Version() Version { return V20260903 }

// TransformResponse implements VersionChange. Deliberately a no-op: this
// change lives entirely on the error path.
func (SwitchActiveOrganizationRetiredMessageChange) TransformResponse(string, any) {}

// TransformError implements ErrorTransform. It rewrites ONLY the marked retired
// SwitchActiveOrganization CodeUnimplemented error back to the legacy
// organization-management-unimplemented message older clients saw before
// V20260903.
func (SwitchActiveOrganizationRetiredMessageChange) TransformError(method string, err error) error {
	if err == nil || method != bossanovav1connect.OrchestratorServiceSwitchActiveOrganizationProcedure {
		return err
	}
	if !IsRetiredProcedure(err) {
		return err
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		return err
	}
	return connect.NewError(connect.CodeUnimplemented, errors.New(legacySwitchActiveOrganizationMessage))
}
