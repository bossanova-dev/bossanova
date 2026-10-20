package alertstate

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// failingFacts is a repair alert that is still current: same head as the failed
// repair, red CI, an open PR.
func failingFacts() RepairFacts {
	return RepairFacts{
		FailedHeadSHA:  "aaa111",
		CurrentHeadSHA: "aaa111",
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		HasFailures:    true,
	}
}

func TestRepairAlertResolution_EachEndConditionClears(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*RepairFacts)
		wantReason ClearReason
	}{
		{
			name:       "unchanged head with red CI stays current",
			mutate:     func(*RepairFacts) {},
			wantReason: "",
		},
		{
			name:       "head moved past the failed head",
			mutate:     func(f *RepairFacts) { f.CurrentHeadSHA = "bbb222" },
			wantReason: ClearHeadMoved,
		},
		{
			name: "CI went green",
			mutate: func(f *RepairFacts) {
				f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
				f.HasFailures = false
			},
			wantReason: ClearCIGreen,
		},
		{
			name: "clean checking run counts as green (moved repairFailureResolved rule)",
			mutate: func(f *RepairFacts) {
				f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CHECKING
				f.HasFailures = false
			},
			wantReason: ClearCIGreen,
		},
		{
			name:       "checking with failures stays current",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CHECKING },
			wantReason: "",
		},
		{
			name: "verifying with changes requested stays current",
			mutate: func(f *RepairFacts) {
				f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_VERIFYING
				f.HasFailures = false
				f.HasChangesRequested = true
			},
			wantReason: "",
		},
		{
			name:       "PR merged",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_MERGED },
			wantReason: ClearLeftRepairStates,
		},
		{
			name:       "PR closed",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CLOSED },
			wantReason: ClearLeftRepairStates,
		},
		{
			name:       "PR back to draft",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_DRAFT },
			wantReason: ClearLeftRepairStates,
		},
		{
			name:       "session has no PR",
			mutate:     func(f *RepairFacts) { f.NoPR = true },
			wantReason: ClearLeftRepairStates,
		},
		{
			name:       "unknown current head is not a head move",
			mutate:     func(f *RepairFacts) { f.CurrentHeadSHA = "" },
			wantReason: "",
		},
		{
			name:       "unknown failed head is not a head move",
			mutate:     func(f *RepairFacts) { f.FailedHeadSHA = "" },
			wantReason: "",
		},
		{
			name:       "unpolled display status stays current",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED },
			wantReason: "",
		},
		{
			name:       "conflict stays current",
			mutate:     func(f *RepairFacts) { f.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_CONFLICT },
			wantReason: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := failingFacts()
			tc.mutate(&f)
			if got := RepairAlertResolution(f); got != tc.wantReason {
				t.Fatalf("RepairAlertResolution = %q, want %q", got, tc.wantReason)
			}
			if got, want := RepairAlertCurrent(f), tc.wantReason == ""; got != want {
				t.Fatalf("RepairAlertCurrent = %v, want %v", got, want)
			}
		})
	}
}

func TestCIResolved_MatchesFormerTUIRule(t *testing.T) {
	tests := []struct {
		status   pb.DisplayStatus
		failures bool
		changes  bool
		want     bool
	}{
		{pb.DisplayStatus_DISPLAY_STATUS_PASSING, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_MERGED, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_CLOSED, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_CHECKING, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, false, false, true},
		{pb.DisplayStatus_DISPLAY_STATUS_CHECKING, true, false, false},
		{pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, false, true, false},
		{pb.DisplayStatus_DISPLAY_STATUS_FAILING, true, false, false},
		{pb.DisplayStatus_DISPLAY_STATUS_REJECTED, false, true, false},
		{pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED, false, false, false},
	}
	for _, tc := range tests {
		if got := CIResolved(tc.status, tc.failures, tc.changes); got != tc.want {
			t.Errorf("CIResolved(%v, failures=%v, changes=%v) = %v, want %v", tc.status, tc.failures, tc.changes, got, tc.want)
		}
	}
}

func TestAuthWedgeCurrent(t *testing.T) {
	wedgeAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	wedge := &pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED,
		CreatedAt: timestamppb.New(wedgeAt),
	}
	older := &pb.RotationEvent{
		Outcome:   pb.RotationOutcome_ROTATION_OUTCOME_ROTATED,
		CreatedAt: timestamppb.New(wedgeAt.Add(-time.Hour)),
	}
	untimed := &pb.RotationEvent{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED}

	tests := []struct {
		name   string
		events []*pb.RotationEvent
		rec    WedgeRecovery
		want   bool
	}{
		{name: "no events", events: nil, want: false},
		{name: "wedge with no output since", events: []*pb.RotationEvent{wedge, older}, want: true},
		{name: "wedge then later agent output", events: []*pb.RotationEvent{wedge, older}, rec: WedgeRecovery{LastAgentActivityAt: wedgeAt.Add(time.Minute)}, want: false},
		{name: "output before the wedge", events: []*pb.RotationEvent{wedge}, rec: WedgeRecovery{LastAgentActivityAt: wedgeAt.Add(-time.Minute)}, want: true},
		{name: "output at the wedge instant is not after it", events: []*pb.RotationEvent{wedge}, rec: WedgeRecovery{LastAgentActivityAt: wedgeAt}, want: true},
		{name: "PR done ends the wedge", events: []*pb.RotationEvent{wedge}, rec: WedgeRecovery{PRDone: true}, want: false},
		{
			name:   "newest event is not a wedge",
			events: []*pb.RotationEvent{older, wedge},
			want:   false,
		},
		{
			name:   "wedge without a timestamp stays current despite output",
			events: []*pb.RotationEvent{untimed},
			rec:    WedgeRecovery{LastAgentActivityAt: wedgeAt.Add(time.Minute)},
			want:   true,
		},
		{
			name:   "wedge without a timestamp ends when the PR is done",
			events: []*pb.RotationEvent{untimed},
			rec:    WedgeRecovery{PRDone: true},
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthWedgeCurrent(tc.events, tc.rec); got != tc.want {
				t.Fatalf("AuthWedgeCurrent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFailedHeadFor(t *testing.T) {
	older := time.Unix(100, 0)
	newer := time.Unix(200, 0)
	cases := []struct {
		name      string
		count     int32
		started   *time.Time
		blocked   *time.Time
		stalled   bool
		wantEmpty bool
	}{
		{name: "failed round, no refusal", count: 2, started: &older},
		{name: "refusal older than round", count: 1, started: &newer, blocked: &older},
		{name: "blocked only", count: 0, blocked: &newer, wantEmpty: true},
		{name: "blocked on current head, last real round on older head", count: 1, started: &older, blocked: &newer, wantEmpty: true},
		{name: "live stall", count: 1, started: &older, stalled: true, wantEmpty: true},
	}
	for _, tc := range cases {
		got := FailedHeadFor("sha-old", tc.count, tc.started, tc.blocked, tc.stalled)
		if tc.wantEmpty != (got == "") {
			t.Errorf("%s: FailedHeadFor = %q, wantEmpty %v", tc.name, got, tc.wantEmpty)
		}
	}
	// The alert is kept: blocked on the new head, old recorded head.
	f := RepairFacts{
		FailedHeadSHA:  FailedHeadFor("sha-old", 1, &older, &newer, false),
		CurrentHeadSHA: "sha-new",
		DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		HasFailures:    true,
	}
	if !RepairAlertCurrent(f) {
		t.Fatal("blocked alert on current head was cleared as head-moved")
	}
}

func TestBlockedResolution(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name  string
		facts BlockedFacts
		want  ClearReason
	}{
		{"head moved", BlockedFacts{AttemptHeadSHA: "abc", CurrentHeadSHA: "def"}, ClearHeadMoved},
		{"equal heads ignore case", BlockedFacts{AttemptHeadSHA: "ABC", CurrentHeadSHA: "abc"}, ""},
		{"unknown current head", BlockedFacts{AttemptHeadSHA: "abc"}, ""},
		{"unknown attempt head", BlockedFacts{CurrentHeadSHA: "abc"}, ""},
		{"unpolled", BlockedFacts{}, ""},
		{"unknown mergeability", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, ""},
		{"not mergeable", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, Mergeable: &no}, ""},
		{"failures", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, Mergeable: &yes, HasFailures: true}, ""},
		{"changes requested", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_APPROVED, Mergeable: &yes, HasChangesRequested: true}, ""},
		{"run working on a draft", BlockedFacts{RunWorking: true, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT}, ClearRunWorking},
		{"run working with CI failing", BlockedFacts{RunWorking: true, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING, HasFailures: true}, ClearRunWorking},
		{"checks running", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, Mergeable: &yes}, ClearCIPending},
		{"checks running, mergeability unknown", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING}, ClearCIPending},
		{"checks running, not mergeable", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, Mergeable: &no}, ClearCIPending},
		{"checks running with one failed", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, Mergeable: &yes, HasFailures: true}, ""},
		{"checks running with changes requested", BlockedFacts{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, Mergeable: &yes, HasChangesRequested: true}, ""},
	}
	for _, status := range []pb.DisplayStatus{pb.DisplayStatus_DISPLAY_STATUS_PASSING, pb.DisplayStatus_DISPLAY_STATUS_APPROVED, pb.DisplayStatus_DISPLAY_STATUS_REVIEW, pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN} {
		tests = append(tests, struct {
			name  string
			facts BlockedFacts
			want  ClearReason
		}{status.String(), BlockedFacts{DisplayStatus: status, Mergeable: &yes}, ClearCIGreen})
	}
	for _, status := range []pb.DisplayStatus{pb.DisplayStatus_DISPLAY_STATUS_DRAFT, pb.DisplayStatus_DISPLAY_STATUS_FAILING, pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, pb.DisplayStatus_DISPLAY_STATUS_REJECTED, pb.DisplayStatus_DISPLAY_STATUS_IDLE, pb.DisplayStatus_DISPLAY_STATUS_MERGED, pb.DisplayStatus_DISPLAY_STATUS_CLOSED, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED} {
		tests = append(tests, struct {
			name  string
			facts BlockedFacts
			want  ClearReason
		}{status.String(), BlockedFacts{DisplayStatus: status, Mergeable: &yes}, ""})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BlockedResolution(tc.facts); got != tc.want {
				t.Fatalf("BlockedResolution = %q, want %q", got, tc.want)
			}
			if got := BlockedAlertCurrent(tc.facts); got != (tc.want == "") {
				t.Fatalf("BlockedAlertCurrent = %v, want %v", got, tc.want == "")
			}
		})
	}
}
