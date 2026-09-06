// Package apiversion implements Stripe-style date-based API versioning for the
// OrchestratorService Connect/gRPC API served by bosso.
//
// Clients send a "Bossanova-Version" request header containing a YYYY-MM-DD
// identifier. The server validates it against a Registry, stores the resolved
// version in the request context via Interceptor, and applies an ordered chain
// of VersionChange transforms so older clients receive the response shape they
// expect. A request resolved to the current version runs zero transforms,
// keeping the hot path allocation-free.
//
// To add a new API version and behavior change, append a new Version constant,
// register it in DefaultRegistry, bump Current, and add the matching
// VersionChange to the Changes list wired into bosso. See docs/api-versioning.md
// for the full procedure.
package apiversion

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Version is a validated YYYY-MM-DD API version identifier.
type Version string

// Baseline is the launch baseline API version. It is the oldest supported
// version and the default assumed for header-less requests.
const Baseline Version = "2026-06-29"

// V20260701 is a TEST-ONLY API version. It was introduced alongside the
// reference transform (ReferenceChange) to exercise the transform framework
// end-to-end while keeping this package self-contained. It is intentionally NOT
// a member of the production registry (see DefaultRegistry).
const V20260701 Version = "2026-07-01"

// V20260704 shipped the first live down-convert transform (OrphanedStateChange):
// the OrchestratorService began serving SessionState SESSION_STATE_ORPHANED on
// Session.state, a new terminal state for headless runs killed by a daemon
// restart. Clients pinned to Baseline (which never saw ORPHANED) are
// down-converted to the prior observable behavior, SESSION_STATE_IMPLEMENTING_PLAN.
const V20260704 Version = "2026-07-04"

// V20260705 shipped the AgentAuthFailedChange transform: the OrchestratorService
// began serving the AttentionReason value ATTENTION_REASON_AGENT_AUTH_FAILED on
// Session.attention_status.reason, a new attention reason surfaced when an
// agent's pane shows the login-required terminal shape ("Not logged in" /
// "Please run /login"). Before this detection existed such a session showed no
// auth-specific attention (it "just went quiet"), so clients pinned to an older
// version (which never saw this reason) are down-converted back to no attention.
const V20260705 Version = "2026-07-05"

// V20260706 shipped two transforms: UnmanagedLabelChange, which restores the
// prior "System default" label for older clients when the OrchestratorService
// serves "Unmanaged local credentials" for the unbound rotation account; and
// LimitedChatStatusChange, which maps CHAT_STATUS_LIMITED plus the derived
// "usage-limited..." session display shape back to the prior idle-style behavior
// for older clients.
const V20260706 Version = "2026-07-06"

// V20260711 ships the NoEligibleAccountChange transform (it was current until
// V20260718 superseded it). The OrchestratorService began serving the
// RotationOutcome value ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT (6) on
// Session.rotation_events[].outcome, a new terminal state distinguishing "no
// active account to rotate to" from the prior "agent cannot rotate"
// (ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY). Clients pinned to an older
// version were built before this value existed, so they are down-converted back
// to ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY (the prior observable value).
const V20260711 Version = "2026-07-11"

// V20260718 ships the ErroredStatusChange transform (it was current until
// V20260723 superseded it). At V20260718 the OrchestratorService began
// serving the BOS-430 errored-recolor display shape for orphaned/blocked
// sessions on Session.display_label / display_intent / display_spinner. An
// errored session now keeps its real underlying status label and spinner but
// has its intent recolored to DANGER (an orphaned session no longer collapses to
// a static "orphaned"/no-spinner tuple, and a blocked session's green/neutral
// status is recolored red). Clients pinned to an older version were built
// against the prior shapes — a fixed "orphaned"/DANGER tuple for ORPHANED and an
// un-recolored base cascade for BLOCKED — so they are down-converted back to
// those (see displaystatus.PreErroredOutput).
const V20260718 Version = "2026-07-18"

// V20260723 ships the RespawnSameAccountOutcomeChange transform (it was current
// until V20260803 superseded it). At V20260723 the
// OrchestratorService began serving the RotationOutcome values
// ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT (7) and
// ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED (8) on
// Session.rotation_events[].outcome — new terminal states for the BOS-482
// respawn-in-place healer (a pane auth-failed while its bound account probes
// healthy is stopped and respawned under the SAME account; the cap-exhausted
// value marks the per-chat respawn budget spent for the window). Clients pinned
// to an older version were built before these values existed, so they are
// down-converted back to the prior observable value,
// ROTATION_OUTCOME_STATUS_ONLY_NO_CAPABILITY.
const V20260723 Version = "2026-07-23"

// V20260803 ships the AgentStalledChange transform (it was current until
// V20260804 superseded it). At V20260803 the OrchestratorService began
// serving the AttentionReason value ATTENTION_REASON_AGENT_STALLED (6) on
// Session.attention_status.reason — a new attention reason raised when a chat
// reports CHAT_STATUS_WORKING while its agent has made no semantic progress (no
// new transcript record) for longer than its phase's threshold, i.e. a silently
// dead turn behind a still-animating spinner. Like AGENT_AUTH_FAILED it only
// fires where the session previously had NO attention at all: before the
// detector existed such a session "just kept spinning". Clients pinned to an
// older version were built before this reason existed, so they are
// down-converted back to the prior observable behavior — no attention.
const V20260803 Version = "2026-08-03"

// V20260804 ships the WaitingChatStatusChange transform (it was current until
// V20260812 superseded it). At V20260804 the OrchestratorService began
// serving the ChatStatus value CHAT_STATUS_WAITING (6), plus the accompanying
// waiting_reason string on ChatStatusEntry, ChatStatusDelta and
// SessionStatusEntry. A chat is WAITING when it is blocked on an external event
// — a registered GitHub callback, a background poll tick — rather than
// computing; before the value existed such a chat reported CHAT_STATUS_WORKING
// with no reason, so "blocked on CI for 82 minutes" and "hung" looked the same.
// Clients pinned to an older version were built before WAITING existed and
// would render an unknown enum value blank, so they are down-converted back to
// the prior observable behavior: CHAT_STATUS_WORKING with an empty
// waiting_reason.
const V20260804 Version = "2026-08-04"

// V20260812 ships the DraftPRFailureLabelChange transform (it was current until
// V20260816 superseded it). At V20260812 the OrchestratorService
// stopped letting a session-level draft-PR-creation failure claim the session's
// primary display composite while a chat is live (BOS-855). The "? PR failed"
// cascade branch moved from directly below the QUESTION/LIMITED chat branches to
// immediately below WORKING, so a session whose draft PR failed but whose chat is
// working / waiting — or whose worktree is initializing, merging or archiving —
// now serves that live label on Session.display_label / display_intent /
// display_spinner instead of "? PR failed". A row can therefore no longer assert
// a past failure and a present activity at once. Clients pinned to an older
// version were built against the old precedence, so they are down-converted back
// to "? PR failed" (see displaystatus.PreDraftPRFailureOutput). The failure is
// not lost for a current client: it is carried by a warning hint that is
// deliberately exempt from the accompanying recessive treatment.
const V20260812 Version = "2026-08-12"

// V20260816 ships the GateFailedOutcomeChange transform (it was current until
// V20260820 superseded it): at V20260816 the OrchestratorService began
// distinguishing a cron gate that could NOT be evaluated from one that ran and
// decided there was no work (BOS-881). A gate that timed out, could not be
// launched, or was reported missing/unrunnable by the shell (exit 127 / 126)
// now records last_run_outcome "gate_failed" and derives
// CRON_JOB_STATUS_FAILED on CronJob.last_run_status, where it previously
// recorded "gated" and derived CRON_JOB_STATUS_GATED — a warning-styled
// "waiting, healthy" value that made a repo-wide broken PATH read as a quiet
// backlog sweep. RunCronJobNow likewise returns the new "gate_failed" skip
// reason on that path. No CronJobStatus enum member was added: the new outcome
// reuses the existing FAILED value via isCronFailureOutcome. Clients pinned to
// an older version were built against the prior values, so they are
// down-converted back to outcome "gated", CRON_JOB_STATUS_GATED, and the
// "gated" skip reason.
const V20260816 Version = "2026-08-16"

// V20260820 ships the SwitchDeadlineCodeChange transform (it was current until
// V20260821 superseded it): at V20260820 the OrchestratorService
// began serving connect.CodeDeadlineExceeded for a ProxySwitchSessionAccount
// whose switch was ended by its own daemon-side budget (BOS-947). Before this
// change the daemon had no wire value for "a deadline stopped this", so the
// relayed CommandResult carried ERROR_CODE_UNSPECIFIED and bosso's
// validateCommandResult fell through to connect.CodeAborted.
//
// CodeAborted was the wrong answer, not merely a vaguer one: it invites a
// retry, and BOS-747's rule is that a request killed by its own deadline must
// not be retried. The daemon now emits the new
// CommandResult.ERROR_CODE_DEADLINE_EXCEEDED for that case (see
// classifySwitchCommandError in services/bossd) and bosso renders it as
// CodeDeadlineExceeded.
//
// The change is in the VALUE served on an existing procedure, not the schema.
// Clients pinned to an older version were built when this case read as
// CodeAborted, so for any request resolved older than V20260820 the transform
// restores that code, message intact.
//
// It down-converts ONLY the relayed daemon deadline, matched by the typed
// relayed-daemon-deadline marker. bosso's own commandDeadline expiry on the same
// procedure already returned CodeDeadlineExceeded long before this version, and
// a procedure-scoped transform would have regressed that correct, pre-existing
// answer for old clients — a regression introduced by the compatibility layer
// itself. See SwitchDeadlineCodeChange in transform.go.
const V20260820 Version = "2026-08-20"

// V20260821 is the current production API version. It ships two
// ProxySwitchSessionAccount error-path transforms.
//
// SwitchResultCeilingMessageChange: ProxySwitchSessionAccount began using a
// self-describing message when bosso's own result ceiling stops waiting before a
// daemon verdict arrives. The code remains connect.CodeDeadlineExceeded, but
// current clients now see that the account switch may still be running and that
// the request did not cancel or tear it down. Clients pinned to an older version
// were built against the historical relay timeout text, so they are
// down-converted to "command timed out after 2m0s".
//
// The transform targets ONLY the typed handler-owned result-ceiling marker.
// Relayed daemon deadlines keep using SwitchDeadlineCodeChange, and bosso's
// generic dispatchOwnerCommand relay timeout remains unchanged for older
// clients.
//
// SwitchCanceledCodeChange: ProxySwitchSessionAccount began serving
// connect.CodeCanceled for a switch ended by caller cancellation relayed from the
// daemon (BOS-958). Before this change the daemon had no wire value for "the
// caller cancelled this", so the relayed CommandResult carried
// ERROR_CODE_UNSPECIFIED and bosso's validateCommandResult fell through to
// connect.CodeAborted. Clients pinned to an older version are down-converted
// back to CodeAborted, message intact.
//
// The cancellation transform targets ONLY the typed relayed-daemon-canceled
// marker. bosso's own context.Canceled mapping on the same procedure already
// returned CodeCanceled before this version, so a procedure-scoped transform
// would regress that correct, pre-existing answer for old clients.
const V20260821 Version = "2026-08-21"

// V20260825 ships StaleCheckStateChange: Session.last_check_state now serves
// only a verdict demonstrated at the current PR head SHA. Stale, missing, or
// non-demonstrated observations serve CHECKS_OVERALL_UNSPECIFIED, while the raw
// persisted latch moves to last_check_state_observed with observed-at
// provenance. Clients pinned to an older version are down-converted so
// last_check_state again equals last_check_state_observed.
const V20260825 Version = "2026-08-25"

// V20260902 ships the organization-scoped visibility cutover. The compatibility
// behavior is handler-gated rather than registered as a VersionChange because
// the transform seam sees only a procedure and payload: it cannot inspect the
// caller, convert success back to NotFound, or drop/reorder streaming frames.
const V20260902 Version = "2026-09-02"

// V20260903 ships SwitchActiveOrganizationRetiredMessageChange:
// SwitchActiveOrganization is retired in favor of AuthKit switchToOrganization.
// Current clients see the retirement guidance, while older clients are
// down-converted to the legacy organization-management-unimplemented message.
const V20260903 Version = "2026-09-03"

// V20260904 ships AbandonedCheckoutStatusChange: the OrchestratorService now
// distinguishes an abandoned Stripe Checkout from a genuinely activating
// subscription on CloudAccessStatus (BOS-1076).
//
// An account that reached the CheckoutStarted setup state — a Stripe Checkout
// session was created — used to be reported exactly like an account whose user
// had returned from Stripe: state CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
// message "Your subscription is being activated.", can_create_checkout false and
// checkout_started true. A user who opened Checkout and closed the tab therefore
// watched an activation spinner forever with no route back.
//
// From V20260904 only CloudAccountSetupEntitlementPending — written when the
// user returns from Stripe — reports the activating shape. A created-but-never-
// completed session keeps state CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION, keeps its
// checkout affordance (can_create_checkout true), and reports checkout_started
// true purely so the client can offer to RESUME rather than restart. No field or
// enum member was added: the change is in the VALUES served on
// GetCloudAccessStatus, CreateCheckoutSession and RefreshCloudEntitlements, and
// the previously-impossible combination
// NEEDS_SUBSCRIPTION && checkout_started && can_create_checkout is what carries
// it. Clients pinned to an older version were built when that combination could
// not occur, so they are down-converted back to the prior activating shape (see
// AbandonedCheckoutStatusChange in transform.go).
//
// One narrow sub-case is folded into that down-convert rather than restored
// exactly: a personal account in CheckoutStarted whose idempotency key was never
// recorded used to read NEEDS_SUBSCRIPTION with checkout_started false, and its
// CreateCheckoutSession call was then refused with "subscription is being
// activated" — the same dead end, reached one round trip later. Nothing on the
// wire distinguishes it from an ordinary abandoned checkout, so the transform
// serves the activating shape for both.
//
// The CreateCheckoutSession ERROR path also moved: that keyless claim now mints a
// session instead of being refused. A transform cannot turn a success back into
// an error (TransformResponse and TransformError never see the other path), so
// there is no seam for it and it is deliberately not gated in the handler either:
// down-converting a working checkout URL back into a permanent refusal would be a
// regression introduced by the compatibility layer itself.
const V20260904 Version = "2026-09-04"

// V20260905 makes the orchestrator's fleet-wide repository and daemon reads
// span every organization the caller belongs to, and gives each an explicit
// filter for callers that want one (BOS-1157, BOS-1159).
//
// ProxyListReposAggregated used to resolve ONE organization from the caller's
// claim and fan out only over that organization's daemons. A repository
// registered on a machine that authenticated into another of the caller's own
// organizations was therefore absent, and the response said nothing about the
// omission — the web repository settings page rendered "No repositories are
// registered" to a user whose machine was registered and live.
//
// From V20260905 an unset ProxyListReposAggregatedRequest.organization_id means
// the union across the caller's memberships, with any unreadable organization
// reported in the new failed_organizations rather than silently dropped. Setting
// the field asks for one organization, which is the pre-cutover behavior made
// explicit rather than inherited from the session claim.
//
// There is no transform for this and there cannot be one: down-converting a
// union back to "the organization this particular caller's claim named" needs
// the request, and TransformResponse never sees it. That is the same seam
// IsOrgScopedVisibility (V20260902) documents, so the compatibility branch lives
// in the handler — see IsCrossOrgRepoReads. A client pinned older keeps the
// single-organization read AND keeps failed_organizations empty, because the
// path that populates it does not run for them.
const V20260905 Version = "2026-09-05"

// V20260906 ships cloud access resolved across every organization the caller
// belongs to (BOS-1152). The compatibility behavior is handler-gated rather
// than registered as a VersionChange, for the same reason as V20260902: the
// verdict is caller-relative and TransformResponse never sees the request.
//
// Cloud access used to be decided from the single account the caller's ACTIVE
// organization claim resolved to. A user who belonged to a subscribed
// organization but was acting under an unsubscribed one was served
// CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION even though their membership already
// paid for access. From V20260906 a claim that fails to authorise folds over
// the caller's other member organizations, and any one of them that is
// entitled reports CLOUD_ACCESS_STATE_ACTIVE.
//
// No field or enum member was added: the change is in the VALUES served on
// GetCloudAccessStatus, CreateCheckoutSession and RefreshCloudEntitlements, and
// in whether the RPCs guarded by CloudAccessPolicy.Require admit the caller at
// all. CloudAccessStatus does carry account_id and workos_org_id, so a response
// names which account decided it — but a transform cannot compare that to the
// caller's OWN organization, because TransformResponse receives only
// (method, msg) and never the request. Down-converting therefore requires the
// caller, which puts it in the handler: at an older resolved version
// CloudAccessPolicy.Check skips the fan-out entirely and serves the
// claim-only answer byte-for-byte.
const V20260906 Version = "2026-09-06"

// V20260907 ships CloudAccessOrganizationChange: CloudAccessStatus.workos_org_id
// is now populated with the WorkOS organization the caller is acting as
// (BOS-1155).
//
// The field has existed on the wire since the original Stripe subscription
// gating, but no producer ever assigned it: billing.CloudAccessPolicy.Check and
// decorateCloudAccessStatus both left it at "", so every client that read it
// observed the empty string on every response. From V20260907
// decorateCloudAccessStatus -- the single chokepoint every CloudAccessStatus
// bosso serves passes through -- fills it from the caller's resolved
// organization scope, falling back to the raw org_id claim. A daemon caller,
// which has no organization of its own, still reports "".
//
// No field or enum member was added: the change is purely in the VALUE served on
// GetCloudAccessStatus, CreateCheckoutSession and RefreshCloudEntitlements. That
// is what makes it a behavioral change rather than an additive one -- a client
// built before this version was built when the field could only be empty, so it
// is down-converted back to "" (see CloudAccessOrganizationChange in
// transform.go).
//
// The consumer is the boss CLI's cloud login gate: it looks the id up in
// ListOrganizations to find the matching mirror row and opens the refused login
// at the organization-scoped `/:orgId/subscribe` instead of the unscoped page.
// Without a populated field that lookup was never reached and the scoped URL was
// unreachable in production.
const V20260907 Version = "2026-09-07"

// V20260908 lets session commands resolve an owning daemon across every
// organization the caller belongs to (BOS-1166). Older clients retain the
// active-organization-only lookup.
//
// There is no transform for this and there cannot be one: the change turns a
// NotFound into success, TransformError never runs on the success path, and no
// TransformResponse can turn the returned session back into an error. The
// compatibility branch therefore lives in the handler; see
// IsCrossOrgSessionCommands.
const V20260908 Version = "2026-09-08"

// V20260909 ships ProxyListSessionsOwnerResolutionChange: a daemon-filtered
// session list now distinguishes an unavailable ownership store from a
// legitimately empty result (BOS-1169).
//
// Before this version ProxyListSessions silently skipped every session whose
// owner could not be resolved, regardless of whether the session was outside
// the caller's scope or the ownership store was unavailable. From V20260909 a
// non-NotFound owner-resolution failure returns CodeUnavailable, while
// NotFound remains a privacy-preserving skip. Clients pinned to an older
// version are down-converted to the complete legacy short-list response (see
// ProxyListSessionsOwnerResolutionChange in transform.go).
const V20260909 Version = "2026-09-09"

// V20260910 makes the orchestrator's cron-job and remaining fleet reads span
// every organization the caller belongs to, with an optional organization
// filter where supported (BOS-1158, BOS-1161), and makes
// ProxyListReposAggregated fail when repository-holder enrichment is unavailable
// (BOS-1162). It also ships real WorkOS invitations from
// InviteOrganizationMember (BOS-1122) and surfaces pending invitations in
// ListOrganizationMembers (BOS-1123). Older clients retain their
// single-organization fleet behavior and the successful unstamped repository
// list; PendingInvitationResponseChange clears invitation-only fields on the
// invite response and removes pending rows from the member list.
//
// Compatibility is handler-gated because the response transform has neither
// the request nor the caller's membership set; see IsCrossOrgCronReads and
// IsCrossOrgFleetReads. Repository-list
// compatibility uses ProxyListReposHolderResolutionChange because its complete
// legacy response can be returned alongside a typed error for recovery.
//
// The invitation success response carries a pending OrganizationMember:
// user_id and workos_membership_id are empty, email and role identify the invite,
// and is_invite_pending is true. PendingInvitationResponseChange clears that new
// marker for clients pinned below this version.
//
// The larger error-to-success behavior change cannot be restored by a response
// transform: an unregistered address formerly returned NotFound, while a WorkOS
// invitation now succeeds, and TransformError never sees a success response.
// As with the V20260904 checkout success-path asymmetry, the working behavior is
// deliberately served to pinned clients rather than frozen behind a handler gate.
const V20260910 Version = "2026-09-10"

// V20260911 lets RemoveOrganizationMember revoke a pending invitation when
// invitation_id is set (BOS-1125). That success performs a side effect and has
// an empty response, so neither response nor error transforms can restore the
// prior InvalidArgument result. IsInvitationRevocation is the narrow handler
// gate that keeps older clients on the pre-field behavior.
//
// V20260911 surfaces an accepted WorkOS invitation in
// ListOrganizationMembers until its WorkOS user lands as a local membership.
// The additive is_invite_accepted field lets current clients render that
// short-lived state without treating it as an actionable active member. Older
// clients never observed accepted invitation-only rows, so
// AcceptedInvitationResponseChange removes them.
//
// Reconciliation also restores Stripe seat quantity whenever the local mirror's
// membership cardinality changes. That side effect is intentionally not gated:
// it repairs billing truth after WorkOS membership changes, and serving stale
// seat quantities to an older caller would perpetuate the defect. A response
// transform cannot undo an external billing write.
const V20260911 Version = "2026-09-11"

// V20260912 folds the cross-organization session union into
// ProxyListSessions (BOS-1165). An unset organization_id spans every
// organization the caller belongs to, while setting it narrows the read after
// a membership check. Older clients retain the claimed-organization read.
//
// This compatibility boundary is handler-gated because the result set depends
// on both the request and the caller's memberships, neither of which is
// available to TransformResponse. See IsCrossOrgSessionReads.
const V20260912 Version = "2026-09-12"

// V20260913 ships SupersededCredentialClassChange: AuthCheck.failure_class now
// carries "credential_superseded" ALONGSIDE a "healthy" outcome (BOS-1175).
//
// The daemon detects that an ambient `codex login` for the same provider account
// holds a different refresh token, so the refresh chain behind the stored
// credential is dead even though the provider still accepts the stored access
// token. That is a warning about the future, not a failure: the account stays
// eligible and its probe still reports outcome "healthy".
//
// No field or enum member was added: the change is purely in the VALUE served on
// AuthCheck.failure_class, which is embedded in Account and reaches clients on
// ProxyListAccounts, ProxyManageListAccounts, ProxyAddAccount and
// ProxyRefreshAccount. What makes it behavioral rather than additive is the
// invariant it breaks — before this version a "healthy" check ALWAYS carried an
// empty failure_class, so a client built against it may render an unhealthy row,
// a raw token, or nothing at all for a class it cannot interpret. Clients pinned
// to an older version are therefore down-converted back to the empty
// failure_class they were built against (see SupersededCredentialClassChange in
// transform.go). Only the healthy pairing is blanked: a
// "credential_superseded" class on a non-healthy outcome is not a shape this
// version introduced, and no such producer exists.

// V20260914 ships RefreshChainUnprovenOutcomeChange: Account.auth_check.outcome
// began serving "refresh_chain_unproven" (with failure_class
// "refresh_not_observed") for a credential check that COMPLETED CLEANLY on a
// credential whose own access token says a token refresh should already have
// happened and whose run observed no credential write (BOS-1174). Before this
// change that identical clean run served outcome "healthy" with an empty
// failure_class.
//
// No enum member was added — auth_check.outcome is a plain string — so the
// behavior change is in the VALUE served, exactly like GateFailedOutcomeChange
// (V20260816) and StaleCheckStateChange (V20260825). It matters because clients
// switch on that string: the web account list and the boss TUI both map a known
// outcome to a severity, and a deployed older build that has never seen
// "refresh_chain_unproven" would flip a green "healthy" pill to an
// undetermined/warning one for an account whose behavior did not change.
//
// For any request resolved older than V20260914 the transform restores the
// prior observable pair: outcome "healthy", failure_class "". It is applied to
// every OrchestratorService procedure that can carry an Account.
//
// NOTE (branch/main skew): this constant is deliberately NOT added to
// ReleasedVersions. It is the single trailing UNRELEASED Current contract that
// released.go's ledger and TestReleasedVersions_AreRegistryPrefix explicitly
// allow, which also keeps the immutable ledger free of a merge conflict with
// the 2026-09-12 entry main shipped in parallel.
const V20260913 Version = "2026-09-13"

const V20260914 Version = "2026-09-14"

// Parse validates and returns a Version from a strict YYYY-MM-DD calendar date
// string. It rejects strings that are not valid calendar dates (e.g. "2026-13-01")
// or that use any other format.
func Parse(s string) (Version, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", fmt.Errorf("apiversion: invalid version %q: must be a valid YYYY-MM-DD calendar date", s)
	}
	// Verify the round-trip to reject any non-canonical representation.
	if t.Format("2006-01-02") != s {
		return "", fmt.Errorf("apiversion: invalid version %q: must be a valid YYYY-MM-DD calendar date", s)
	}
	return Version(s), nil
}

// String implements fmt.Stringer.
func (v Version) String() string { return string(v) }

// Registry holds an ordered (oldest→newest) slice of registered API versions,
// the current (latest known) version, and the default version used when a
// request carries no Bossanova-Version header.
//
// By policy the default is the oldest supported version so that a header-less
// caller never silently shifts onto newer behavior — matching the Stripe intent
// of "pin to the version you started on."
type Registry struct {
	all     []Version
	current Version
	def     Version
}

// NewRegistry constructs a Registry, validating that:
//   - all is non-empty
//   - every entry is a valid YYYY-MM-DD calendar date
//   - entries are strictly increasing (no duplicates)
//   - current and def are both members of all
func NewRegistry(all []Version, current, def Version) (*Registry, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("apiversion: registry must contain at least one version")
	}
	members := make(map[Version]struct{}, len(all))
	var prev Version
	for i, v := range all {
		if _, err := Parse(string(v)); err != nil {
			return nil, err
		}
		if _, dup := members[v]; dup {
			return nil, fmt.Errorf("apiversion: duplicate version %q in registry", v)
		}
		// Compare against the previous element via a tracked variable rather
		// than re-indexing all[i-1] so gosec's G602 bounds analysis stays happy.
		if i > 0 && string(v) <= string(prev) {
			return nil, fmt.Errorf("apiversion: versions must be strictly increasing: %q is not newer than %q", v, prev)
		}
		members[v] = struct{}{}
		prev = v
	}
	if _, ok := members[current]; !ok {
		return nil, fmt.Errorf("apiversion: current version %q is not in the registry", current)
	}
	if _, ok := members[def]; !ok {
		return nil, fmt.Errorf("apiversion: default version %q is not in the registry", def)
	}
	cp := make([]Version, len(all))
	copy(cp, all)
	return &Registry{all: cp, current: current, def: def}, nil
}

// All returns a copy of all registered versions, ordered oldest→newest.
func (r *Registry) All() []Version {
	cp := make([]Version, len(r.all))
	copy(cp, r.all)
	return cp
}

// Current returns the latest known API version.
func (r *Registry) Current() Version { return r.current }

// Default returns the version assumed when a request carries no
// Bossanova-Version header. By policy this is the oldest supported version.
func (r *Registry) Default() Version { return r.def }

// IsSupported reports whether v is a member of the registry.
func (r *Registry) IsSupported(v Version) bool {
	return slices.Contains(r.all, v)
}

// Newer reports whether a is strictly newer than b. Both a and b must be
// members of the registry; if either is unknown Newer returns false and callers
// should not rely on the ordering of unknown versions.
func (r *Registry) Newer(a, b Version) bool {
	if !r.IsSupported(a) || !r.IsSupported(b) {
		return false
	}
	// YYYY-MM-DD strings compare correctly under lexicographic ordering because
	// they are zero-padded and most-significant field first.
	return string(a) > string(b)
}

// DefaultRegistry returns a Registry seeded with the known production API
// versions, ordered oldest→newest: Baseline, V20260704, V20260705, V20260706,
// V20260711, V20260718, V20260723, V20260803, V20260804, V20260812, V20260816,
// V20260820, V20260821, V20260825, V20260902, V20260903, V20260904,
// V20260905, V20260906, V20260907, V20260908, V20260909, V20260910,
// V20260911, V20260912, V20260913, and V20260914. Current is V20260914 (the
// newest behavior) while Default
// stays Baseline (the oldest supported version), so a
// header-less caller is pinned to Baseline and is down-converted by
// ProductionChanges, and a client that negotiates V20260913 runs zero transforms.
//
// V20260701 is intentionally NOT a member of the production registry — it
// exists as an exported const for example and test use only (it is exercised
// by the transform framework tests and the reference ReferenceChange).
//
// To add a new API version, append it here, set it as Current, and add the
// matching VersionChange to ProductionChanges. See docs/api-versioning.md for
// the full procedure.
func DefaultRegistry() *Registry {
	reg, err := NewRegistry(
		[]Version{Baseline, V20260704, V20260705, V20260706, V20260711, V20260718, V20260723, V20260803, V20260804, V20260812, V20260816, V20260820, V20260821, V20260825, V20260902, V20260903, V20260904, V20260905, V20260906, V20260907, V20260908, V20260909, V20260910, V20260911, V20260912, V20260913, V20260914},
		V20260914,
		Baseline,
	)
	if err != nil {
		panic("apiversion: DefaultRegistry is invalid: " + err.Error())
	}
	return reg
}

// IsOrgScopedVisibility reports whether the version resolved for ctx observes
// organization-scoped session/chat visibility (V20260902 and newer). Callers
// that resolve older must keep serving the pre-cutover, user-scoped view.
//
// This is the sanctioned handler-level gate for a change the transform seam
// cannot express: TransformResponse/TransformError never see the request, so a
// caller-relative result set cannot be down-converted, and a success cannot be
// turned back into a NotFound. Streaming frame-sequence changes are likewise
// outside the transform mechanism.
func IsOrgScopedVisibility(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260902, ResolvedVersion(ctx))
}

// IsCrossOrgRepoReads reports whether the version resolved for ctx observes the
// cross-organization repository read (V20260905 and newer). Callers that resolve
// older must keep serving the pre-cutover, single-organization view.
//
// Handler-level for the same reason as IsOrgScopedVisibility: the change is in
// WHICH organizations the response covers, which is relative to the requesting
// caller, and TransformResponse never sees the request.
func IsCrossOrgRepoReads(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260905, ResolvedVersion(ctx))
}

// IsCrossOrgSessionReads reports whether the resolved version observes the
// cross-organization default for ProxyListSessions. Older callers remain
// scoped to the organization in their active claim.
//
// This is handler-level because the result set depends on the request and the
// caller's membership set, which TransformResponse cannot inspect.
func IsCrossOrgSessionReads(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260912, ResolvedVersion(ctx))
}

// IsCrossOrgCronReads reports whether the version resolved for ctx observes the
// cross-organization cron-job read (V20260910 and newer). Older callers remain
// scoped to the organization in their active claim.
//
// This is a handler-level gate because the result set is caller-relative and
// TransformResponse does not receive the request context needed to reconstruct
// the previous claimed-organization result.
func IsCrossOrgCronReads(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260910, ResolvedVersion(ctx))
}

// IsCrossOrgFleetReads reports whether the version resolved for ctx observes
// the remaining fleet reads across every organization the caller belongs to.
// Older callers retain the former single-organization behavior.
func IsCrossOrgFleetReads(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260910, ResolvedVersion(ctx))
}

// IsInvitationRevocation reports whether the resolved version may interpret
// RemoveOrganizationMemberRequest.invitation_id as a pending invitation revoke.
// Older callers retain the prior invalid-request behavior because an empty
// success response cannot be down-converted into that error after the side
// effect has already happened.
func IsInvitationRevocation(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260911, ResolvedVersion(ctx))
}

// IsCrossOrgSessionCommands reports whether the version resolved for ctx may
// route a session command through any organization the caller belongs to
// (V20260908 and newer). Older callers remain scoped to the active organization.
//
// This is a handler-level gate because a member-organization lookup changes a
// NotFound into success. TransformError never sees the success path, and
// TransformResponse cannot recreate the prior error.
func IsCrossOrgSessionCommands(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260908, ResolvedVersion(ctx))
}

// IsCrossOrgDaemonReads reports whether the version resolved for ctx observes
// the cross-organization daemon inventory (V20260905 and newer). Callers that
// resolve older retain the pre-cutover, single-organization read.
//
// This is daemon-specific rather than reusing IsCrossOrgRepoReads: both share a
// release cutover, but they guard independent request-relative API behaviors.
func IsCrossOrgDaemonReads(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260905, ResolvedVersion(ctx))
}

// IsMemberOrgCloudAccess reports whether the version resolved for ctx observes
// cloud access folded across every organization the caller belongs to
// (V20260906 and newer). Callers that resolve older must keep being judged from
// their claimed organization alone.
//
// This is the sanctioned handler-level gate for a change the transform seam
// cannot express. The verdict is caller-relative: whether an ACTIVE answer came
// from the caller's own organization or from a sibling is only decidable
// against the request, and TransformResponse/TransformError never see it.
func IsMemberOrgCloudAccess(ctx context.Context) bool {
	return !DefaultRegistry().Newer(V20260906, ResolvedVersion(ctx))
}
