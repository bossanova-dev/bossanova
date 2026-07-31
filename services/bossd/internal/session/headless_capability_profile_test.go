package session

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestHeadlessCapabilityProfileFor locks the launch policy: only an unattended
// codex launch requires the tracker-plan-attachment surface. Every other
// combination must stay UNSPECIFIED so its launch path is byte-for-byte
// unchanged.
func TestHeadlessCapabilityProfileFor(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		opts  StartSessionOpts
		want  pb.HeadlessCapabilityProfile
	}{
		{
			name:  "codex detach requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex tmux-unattended requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{IsTmuxUnattended: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex cron requires the tracker-plan-attachment surface",
			agent: "codex",
			opts:  StartSessionOpts{CronJobID: "cron-1"},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex zero-output cron still requires the surface",
			agent: "codex",
			opts:  StartSessionOpts{CronJobID: "cron-1", ZeroOutput: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		},
		{
			name:  "codex interactive requires nothing",
			agent: "codex",
			opts:  StartSessionOpts{},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "claude detach requires nothing",
			agent: "claude",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "claude interactive requires nothing",
			agent: "claude",
			opts:  StartSessionOpts{},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
		{
			name:  "unresolved empty agent name never opts into a new gate",
			agent: "",
			opts:  StartSessionOpts{Detach: true},
			want:  pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := headlessCapabilityProfileFor(tc.agent, tc.opts); got != tc.want {
				t.Fatalf("headlessCapabilityProfileFor(%q, %+v) = %v, want %v", tc.agent, tc.opts, got, tc.want)
			}
		})
	}
}

// TestHeadlessCapabilityProfileForIsSideEffectFree pins that the policy does
// not mutate the opts it is handed — it is read-only over the launch signals.
func TestHeadlessCapabilityProfileForDoesNotMutateOpts(t *testing.T) {
	opts := StartSessionOpts{Detach: true, CronJobID: "cron-9", BranchName: "b"}
	before := opts
	_ = headlessCapabilityProfileFor(agentNameCodex, opts)
	if opts != before {
		t.Fatalf("opts mutated: %+v, want %+v", opts, before)
	}
}
