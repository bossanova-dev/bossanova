package models

import (
	"strings"
	"time"
)

// CronJobOutcome is the result recorded after a cron job's session is finalized.
type CronJobOutcome string

const (
	CronJobOutcomeDeletedNoChanges  CronJobOutcome = "deleted_no_changes"
	CronJobOutcomePRCreated         CronJobOutcome = "pr_created"
	CronJobOutcomePRSkippedNoGitHub CronJobOutcome = "pr_skipped_no_github"
	CronJobOutcomePRFailed          CronJobOutcome = "pr_failed"
	CronJobOutcomeChatSpawnFailed   CronJobOutcome = "chat_spawn_failed"
	CronJobOutcomeCleanupFailed     CronJobOutcome = "cleanup_failed"
	CronJobOutcomeFailedRecovered   CronJobOutcome = "failed_recovered"
	// CronJobOutcomeFireFailed records a fire that never reached a session —
	// e.g. the scheduler's CreateSession call returned an error. Distinct
	// from the later-pipeline failure outcomes (PRFailed, ChatSpawnFailed).
	CronJobOutcomeFireFailed CronJobOutcome = "fire_failed"
	// CronJobOutcomeGated records a fire that was skipped because the job's
	// gate_command RAN and exited non-zero, indicating the gate condition was
	// not met. This is a healthy skip: the gate was consulted and said no.
	CronJobOutcomeGated CronJobOutcome = "gated"
	// CronJobOutcomeGateFailed records a fire that was skipped because the
	// job's gate_command could NOT be evaluated at all — it timed out, could
	// not be launched, was not executable, was never configured, or the shell
	// reported it missing (exit 127) or unrunnable (exit 126). The fire is
	// still blocked (the gate contract fails closed), but the gate condition is
	// UNKNOWN rather than false, so this must never be reported as the healthy
	// `gated` skip. That conflation is what kept the BOS-880 PATH incident
	// invisible: eighteen PRs backed up behind a cron history that looked like
	// a quiet, healthy backlog sweep (BOS-881). It derives cron STATUS FAILED
	// via isCronFailureOutcome so a broken gate is red and escalatable.
	CronJobOutcomeGateFailed CronJobOutcome = "gate_failed"
	// CronJobOutcomePRNoChanges records a run that produced no real work — the
	// branch carries only the empty draft-PR bootstrap commit (or otherwise has
	// an empty diff against its base), so any attached PR is a no-op. Two paths
	// record it: the headless (detach) empty-run guard, and finalize's
	// mark-ready backstop, which refuses to advertise an empty-diff PR as
	// reviewable and applies to cron runs too (BOS-591). It is an attention
	// outcome (see needsAttention): the session is Blocked rather than surfaced
	// as a green ready-for-review PR, so a headless /boss-epic driver
	// fail-isolates the dead session instead of merging an empty PR. It is
	// deliberately distinct from pr_failed, which means something actually broke.
	CronJobOutcomePRNoChanges CronJobOutcome = "pr_no_changes"
	// CronJobOutcomeWorktreeGone records a finalize that ran against a session
	// whose worktree is already gone — e.g. the session was archived/removed
	// (ArchiveSession deletes the worktree but leaves the row in an
	// implementing state) before a late Stop hook or stranded-cron sweep tried
	// to finalize it. `git status` against the missing path fails, but this is a
	// benign no-op, NOT a PR/housekeeping failure: there is nothing left to
	// finalize. It is therefore a non-attention outcome (needsAttention ==
	// false, no scary blocked_reason) and a benign cron STATUS (not FAILED) —
	// distinct from pr_failed, which means the agent ran and only PR creation
	// failed against a live worktree.
	CronJobOutcomeWorktreeGone CronJobOutcome = "worktree_gone"
	// CronJobOutcomeZeroOutput records a successful no-worktree cron run.
	CronJobOutcomeZeroOutput CronJobOutcome = "zero_output"
)

// CronJobConcurrencyPolicy decides what a fire does while the same job's
// previous run is still in progress (BOS-1437). Wording follows GitHub Actions
// (`cancel-in-progress`) and Kestra (`allowConcurrent`). The stored values are
// the cron_jobs.concurrency_policy column's CHECK set.
type CronJobConcurrencyPolicy string

const (
	// CronJobConcurrencyPolicySkip skips the new fire while the previous run is
	// in progress. The default, and today's overlap suppression.
	CronJobConcurrencyPolicySkip CronJobConcurrencyPolicy = "skip"
	// CronJobConcurrencyPolicyCancelInProgress cancels the in-progress previous
	// run, then starts the new one.
	CronJobConcurrencyPolicyCancelInProgress CronJobConcurrencyPolicy = "cancel_in_progress"
	// CronJobConcurrencyPolicyAllowConcurrent starts the new run regardless of
	// the previous one.
	CronJobConcurrencyPolicyAllowConcurrent CronJobConcurrencyPolicy = "allow_concurrent"
)

// Valid reports whether p is one of the known stored concurrency policies.
func (p CronJobConcurrencyPolicy) Valid() bool {
	switch p {
	case CronJobConcurrencyPolicySkip, CronJobConcurrencyPolicyCancelInProgress, CronJobConcurrencyPolicyAllowConcurrent:
		return true
	}
	return false
}

// ParseCronJobConcurrencyPolicy maps a stored string to a policy. Any value
// outside the known set (including "") normalizes to the default,
// CronJobConcurrencyPolicySkip, so a read never yields an unknown policy.
func ParseCronJobConcurrencyPolicy(s string) CronJobConcurrencyPolicy {
	if p := CronJobConcurrencyPolicy(s); p.Valid() {
		return p
	}
	return CronJobConcurrencyPolicySkip
}

// ParseCronJobConcurrencyPolicyInput parses a policy a person or agent typed
// (CLI flag, MCP argument). Unlike ParseCronJobConcurrencyPolicy it never
// defaults: it trims, matches case-insensitively, and accepts '-' for '_', so
// "allow-concurrent", "ALLOW_CONCURRENT" and "allow_concurrent" are all the
// same policy, and reports ok=false for anything else (including "").
func ParseCronJobConcurrencyPolicyInput(s string) (CronJobConcurrencyPolicy, bool) {
	p := CronJobConcurrencyPolicy(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_"))
	if !p.Valid() {
		return "", false
	}
	return p, true
}

// CronJob represents a scheduled prompt that fires on a cron expression.
type CronJob struct {
	ID                    string
	RepoID                string
	Name                  string
	Prompt                string
	Schedule              string
	Timezone              *string // IANA name; nil = daemon-local
	AgentName             string
	Model                 string // opaque agent model id; "" = plugin default.
	IsEnabled             bool
	GateCommand           string // shell command run before firing; non-zero exit skips the run
	ShouldRunSetupCommand bool   // whether to run the repo setup script before the agent session
	// IsZeroOutput marks the job as intended to fire with no worktree, branch, or
	// PR, because the run is expected to produce no repo changes. Persisted and
	// echoed back only — nothing honours it yet, so a job with this set still
	// fires exactly as it does today (BOS-543).
	IsZeroOutput bool
	// ConcurrencyPolicy decides what a fire does while this job's previous run
	// is still in progress. Defaults to skip.
	ConcurrencyPolicy CronJobConcurrencyPolicy
	LastRunSessionID  *string
	LastRunAgentName  string
	LastRunAt         *time.Time
	LastRunOutcome    *CronJobOutcome
	NextRunAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
