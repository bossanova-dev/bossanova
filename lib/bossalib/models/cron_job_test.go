package models

import "testing"

func TestCronJobConcurrencyPolicyValid(t *testing.T) {
	cases := []struct {
		in   CronJobConcurrencyPolicy
		want bool
	}{
		{CronJobConcurrencyPolicySkip, true},
		{CronJobConcurrencyPolicyCancelInProgress, true},
		{CronJobConcurrencyPolicyAllowConcurrent, true},
		{CronJobConcurrencyPolicy(""), false},
		{CronJobConcurrencyPolicy("bogus"), false},
		{CronJobConcurrencyPolicy("SKIP"), false},
		{CronJobConcurrencyPolicy("cancel-in-progress"), false},
	}
	for _, tc := range cases {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("CronJobConcurrencyPolicy(%q).Valid() = %v, want %v", string(tc.in), got, tc.want)
		}
	}
}

func TestParseCronJobConcurrencyPolicy(t *testing.T) {
	cases := []struct {
		in   string
		want CronJobConcurrencyPolicy
	}{
		{"skip", CronJobConcurrencyPolicySkip},
		{"cancel_in_progress", CronJobConcurrencyPolicyCancelInProgress},
		{"allow_concurrent", CronJobConcurrencyPolicyAllowConcurrent},
		{"", CronJobConcurrencyPolicySkip},
		{"bogus", CronJobConcurrencyPolicySkip},
		{"Allow_Concurrent", CronJobConcurrencyPolicySkip}, // unknown casing normalizes to the default
	}
	for _, tc := range cases {
		if got := ParseCronJobConcurrencyPolicy(tc.in); got != tc.want {
			t.Errorf("ParseCronJobConcurrencyPolicy(%q) = %q, want %q", tc.in, string(got), string(tc.want))
		}
	}
}

func TestParseCronJobConcurrencyPolicyInput(t *testing.T) {
	for _, tt := range []struct {
		in     string
		want   CronJobConcurrencyPolicy
		wantOK bool
	}{
		{"skip", CronJobConcurrencyPolicySkip, true},
		{"cancel-in-progress", CronJobConcurrencyPolicyCancelInProgress, true},
		{"cancel_in_progress", CronJobConcurrencyPolicyCancelInProgress, true},
		{"allow-concurrent", CronJobConcurrencyPolicyAllowConcurrent, true},
		{"allow_concurrent", CronJobConcurrencyPolicyAllowConcurrent, true},
		{"ALLOW-CONCURRENT", CronJobConcurrencyPolicyAllowConcurrent, true},
		{"  Skip ", CronJobConcurrencyPolicySkip, true},
		{"queue", "", false},
		{"", "", false},
		{"allow concurrent", "", false},
	} {
		got, ok := ParseCronJobConcurrencyPolicyInput(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("ParseCronJobConcurrencyPolicyInput(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}
