package models

import (
	"strings"
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
