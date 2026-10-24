package models

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCronJobConcurrencyPolicyProtoRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		stored CronJobConcurrencyPolicy
		wire   pb.CronJobConcurrencyPolicy
	}{
		{CronJobConcurrencyPolicySkip, pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP},
		{CronJobConcurrencyPolicyCancelInProgress, pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS},
		{CronJobConcurrencyPolicyAllowConcurrent, pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT},
	} {
		if got := CronJobConcurrencyPolicyToProto(tt.stored); got != tt.wire {
			t.Errorf("ToProto(%q) = %v, want %v", tt.stored, got, tt.wire)
		}
		if got := CronJobConcurrencyPolicyFromProto(tt.wire); got != tt.stored {
			t.Errorf("FromProto(%v) = %q, want %q", tt.wire, got, tt.stored)
		}
	}
	if got := CronJobConcurrencyPolicyFromProto(pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_UNSPECIFIED); got != CronJobConcurrencyPolicySkip {
		t.Errorf("FromProto(UNSPECIFIED) = %q, want skip", got)
	}
	if got := CronJobConcurrencyPolicyFromProto(pb.CronJobConcurrencyPolicy(99)); got != CronJobConcurrencyPolicySkip {
		t.Errorf("FromProto(99) = %q, want skip", got)
	}
	if got := CronJobConcurrencyPolicyToProto("bogus"); got != pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP {
		t.Errorf("ToProto(bogus) = %v, want SKIP", got)
	}
}
