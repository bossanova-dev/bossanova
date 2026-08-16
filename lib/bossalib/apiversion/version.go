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

// V20260816 is the current production API version. It ships the
// GateFailedOutcomeChange transform: at V20260816 the OrchestratorService began
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
// the current (latest released) version, and the default version used when a
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

// Current returns the latest released API version.
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
// V20260711, V20260718, V20260723, V20260803, V20260804, V20260812 and
// V20260816. Current is V20260816 (the newest released behavior) while Default
// stays Baseline (the oldest supported version), so a header-less caller is
// pinned to Baseline and is down-converted by ProductionChanges, and a client
// that negotiates V20260816 runs zero transforms.
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
		[]Version{Baseline, V20260704, V20260705, V20260706, V20260711, V20260718, V20260723, V20260803, V20260804, V20260812, V20260816},
		V20260816,
		Baseline,
	)
	if err != nil {
		panic("apiversion: DefaultRegistry is invalid: " + err.Error())
	}
	return reg
}
