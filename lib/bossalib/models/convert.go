package models

import pb "github.com/recurser/bossalib/gen/bossanova/v1"

// CronJobConcurrencyPolicyToProto maps a stored concurrency policy onto the
// wire enum. Anything outside the known set maps to SKIP, never UNSPECIFIED,
// matching ParseCronJobConcurrencyPolicy's read-side default.
func CronJobConcurrencyPolicyToProto(p CronJobConcurrencyPolicy) pb.CronJobConcurrencyPolicy {
	switch ParseCronJobConcurrencyPolicy(string(p)) {
	case CronJobConcurrencyPolicyCancelInProgress:
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS
	case CronJobConcurrencyPolicyAllowConcurrent:
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT
	default:
		return pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP
	}
}

// CronJobConcurrencyPolicyFromProto maps a wire enum onto the stored policy for
// display. UNSPECIFIED (a peer that never set it) and unknown numbers read as
// SKIP, the policy the daemon applies to them.
func CronJobConcurrencyPolicyFromProto(p pb.CronJobConcurrencyPolicy) CronJobConcurrencyPolicy {
	switch p {
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS:
		return CronJobConcurrencyPolicyCancelInProgress
	case pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT:
		return CronJobConcurrencyPolicyAllowConcurrent
	default:
		return CronJobConcurrencyPolicySkip
	}
}
