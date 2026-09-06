package apiversion

import "slices"

// ReleasedVersions is the append-only golden ledger of every API version that
// has ever shipped in DefaultRegistry(). It is the backwards-compatibility
// contract made structural: once a dated version appears here it MUST remain
// supported forever (within the support window) and MUST NEVER be removed or
// renamed. TestDefaultRegistry_IsAppendOnlySupersetOfReleased asserts that
// DefaultRegistry().All() is a superset of this list, so dropping or renaming a
// shipped version fails CI instead of silently regressing production clients
// that still request the removed version.
//
// The entries are intentionally RAW date literals, not the Baseline / V2026xxxx
// constants: pinning the literal string is what catches a *rename* (e.g.
// re-pointing a constant at a different date). Referencing the constants here
// would let the golden ledger and the registry drift together and defeat the
// guard.
//
// Adding a new API version is an APPEND here (newest last), in lockstep with
// appending it to DefaultRegistry() and setting it Current. Never edit or delete
// an existing entry. See docs/api-versioning.md for the full procedure.
//
// Note: the example/test-only version V20260701 ("2026-07-01") is deliberately
// absent — it never shipped in the production registry (see version.go).
var ReleasedVersions = []Version{
	"2026-06-29", // Baseline — launch baseline.
	"2026-07-04", // V20260704 — OrphanedStateChange (SESSION_STATE_ORPHANED).
	"2026-07-05", // V20260705 — AgentAuthFailedChange (ATTENTION_REASON_AGENT_AUTH_FAILED).
	"2026-07-06", // V20260706 — UnmanagedLabelChange + LimitedChatStatusChange.
	"2026-07-11", // V20260711 — NoEligibleAccountChange (ROTATION_OUTCOME_STATUS_ONLY_NO_ELIGIBLE_ACCOUNT).
	"2026-07-18", // V20260718 — ErroredStatusChange (BOS-430 orphaned/blocked display recolor).
	"2026-07-23", // V20260723 — RespawnSameAccountOutcomeChange (ROTATION_OUTCOME_RESPAWNED_SAME_ACCOUNT + RESPAWN_CAP_EXHAUSTED).
	"2026-08-03", // V20260803 — AgentStalledChange (ATTENTION_REASON_AGENT_STALLED).
	"2026-08-04", // V20260804 — WaitingChatStatusChange (CHAT_STATUS_WAITING + waiting_reason).
	"2026-08-12", // V20260812 — DraftPRFailureLabelChange (BOS-855 "? PR failed" ranks below live activity).
	"2026-08-16", // V20260816 — GateFailedOutcomeChange (BOS-881 gate_failed / CRON_JOB_STATUS_FAILED for a gate that could not run).
	"2026-08-20", // V20260820 — SwitchDeadlineCodeChange (BOS-947 relayed switch deadline surfaces DEADLINE_EXCEEDED, not ABORTED).
	"2026-08-21", // V20260821 — SwitchResultCeilingMessageChange + SwitchCanceledCodeChange.
	"2026-08-25", // V20260825 — StaleCheckStateChange (last_check_state serves only head-current demonstrated verdicts).
	"2026-09-02", // V20260902 — organization-scoped visibility handler gate.
	"2026-09-03", // V20260903 — SwitchActiveOrganization retired in favor of AuthKit switchToOrganization.
	"2026-09-04", // V20260904 — AbandonedCheckoutStatusChange (BOS-1076 abandoned checkout no longer reports an activating subscription).
	"2026-09-05", // V20260905 — cross-organization repository and daemon reads (BOS-1157, BOS-1159; each spans every membership and takes an organization_id filter).
	"2026-09-06", // V20260906 — cloud access resolves across every organization the caller belongs to (BOS-1152), handler-gated.
	"2026-09-07", // V20260907 — CloudAccessOrganizationChange (BOS-1155 CloudAccessStatus.workos_org_id is populated).
	"2026-09-08", // V20260908 — session commands route across every organization the caller belongs to (BOS-1166), handler-gated.
	"2026-09-09", // V20260909 — ProxyListSessionsOwnerResolutionChange (BOS-1169 owner-store outages no longer look like empty results).
	"2026-09-10", // V20260910 — cross-organization cron/fleet reads, ProxyListReposHolderResolutionChange, and PendingInvitationResponseChange (BOS-1158, BOS-1161, BOS-1162, BOS-1122, BOS-1123).
	"2026-09-11", // V20260911 — AcceptedInvitationResponseChange (BOS-1124 accepted invitation placeholders).
	"2026-09-12", // V20260912 — cross-organization session reads (BOS-1165, optional organization_id filter).
	"2026-09-13", // V20260913 — SupersededCredentialClassChange (BOS-1175 AuthCheck.failure_class "credential_superseded" alongside a healthy outcome).
	"2026-09-14", // V20260914 — RefreshChainUnprovenOutcomeChange (BOS-1174 clean check that could not prove the refresh chain).
}

// MissingReleased returns every ReleasedVersions entry that is NOT present in
// supported, preserving ReleasedVersions order. A non-empty result means the
// registry has dropped or renamed a previously-shipped version, violating the
// append-only backwards-compatibility contract. supported is typically
// DefaultRegistry().All().
//
// It is the shared core of the append-only guard: the guard test asserts the
// result is empty for the real registry, and the same helper demonstrably
// reports the dropped version for a simulated removal.
func MissingReleased(supported []Version) []Version {
	var missing []Version
	for _, rv := range ReleasedVersions {
		if !slices.Contains(supported, rv) {
			missing = append(missing, rv)
		}
	}
	return missing
}
