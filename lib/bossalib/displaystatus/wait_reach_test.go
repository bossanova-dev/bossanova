package displaystatus

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCallbackWaitReach(t *testing.T) {
	tests := []struct {
		name, trigger string
		transition    bool
		pr            WaitPR
		blocker       WaitBlocker
	}{
		{"3795 conflicting draft", "checks_passed_ready", false, WaitPR{Open: true, Draft: true, Conflicting: true}, "conflict"},
		{"3795 draft", "checks_passed_ready", false, WaitPR{Open: true, Draft: true}, "draft"},
		{"3797 conflict red", "checks_passed_ready", true, WaitPR{Open: true, Conflicting: true, Checks: WaitChecksFailing}, "conflict"},
		{"pending", "checks_passed_ready", true, WaitPR{Open: true, Checks: WaitChecksPending}, ""},
		{"green", "checks_passed_ready", true, WaitPR{Open: true, Checks: WaitChecksGreen}, ""},
		{"green draft transition", "checks_passed", true, WaitPR{Open: true, Draft: true, Checks: WaitChecksGreen}, "draft"},
		{"checks failed", "checks_passed", true, WaitPR{Open: true, Checks: WaitChecksFailing}, "failing"},
		{"conflict pending", "checks_passed", true, WaitPR{Open: true, Conflicting: true, Checks: WaitChecksPending}, ""},
		{"conflict settled", "checks_passed", true, WaitPR{Open: true, Conflicting: true}, "conflict"},
	}
	for _, trigger := range []string{"merged", "closed", "ready_for_review", "checks_failed", "unknown", ""} {
		tests = append(tests, struct {
			name, trigger string
			transition    bool
			pr            WaitPR
			blocker       WaitBlocker
		}{trigger, trigger, true, WaitPR{Open: true, Conflicting: true}, ""})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CallbackWaitReach(tt.trigger, tt.transition, tt.pr)
			if got.Blocker != tt.blocker || got.Reachable != (tt.blocker == "") {
				t.Fatalf("got %+v, blocker %q", got, tt.blocker)
			}
			owner := ""
			if tt.blocker == "draft" {
				owner = "person"
			} else if tt.blocker != "" {
				owner = "repair"
			}
			if got.Owner != owner {
				t.Fatalf("owner %q, want %q", got.Owner, owner)
			}
		})
	}
}

func TestWaitPRFromSession(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    *pb.Session
		want WaitPR
	}{
		{"absent", nil, WaitPR{}},
		{"unknown mergeability", &pb.Session{}, WaitPR{Open: true}},
		{"conflicting draft", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT, PrMergeable: boolPointer(false)}, WaitPR{Open: true, Draft: true, Conflicting: true}},
		{"conflict red", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, DisplayHasFailures: true}, WaitPR{Open: true, Conflicting: true, Checks: WaitChecksFailing}},
		{"pending", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING}, WaitPR{Open: true, Checks: WaitChecksPending}},
		{"green", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}, WaitPR{Open: true, Checks: WaitChecksGreen}},
		{"approved", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_APPROVED}, WaitPR{Open: true, Checks: WaitChecksGreen}},
		{"merged", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED}, WaitPR{}},
		{"closed", &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED}, WaitPR{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := WaitPRFromSession(tt.p); got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
		})
	}
}
func boolPointer(b bool) *bool { return &b }

func TestDraftCheckTransitionFromSession(t *testing.T) {
	for _, tt := range []struct {
		name       string
		checks     pb.ChecksOverall
		wantChecks WaitChecks
		blocker    WaitBlocker
	}{
		{"green", pb.ChecksOverall_CHECKS_OVERALL_PASSED, WaitChecksGreen, "draft"},
		{"pending", pb.ChecksOverall_CHECKS_OVERALL_PENDING, WaitChecksPending, ""},
		{"failed", pb.ChecksOverall_CHECKS_OVERALL_FAILED, WaitChecksFailing, "failing"},
		{"unknown", pb.ChecksOverall_CHECKS_OVERALL_UNSPECIFIED, WaitChecksUnknown, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pr := WaitPRFromSession(&pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT, LastCheckState: tt.checks})
			if pr.Checks != tt.wantChecks {
				t.Fatalf("checks = %v, want %v", pr.Checks, tt.wantChecks)
			}
			got := CallbackWaitReach("checks_passed", true, pr)
			if got.Blocker != tt.blocker || got.Reachable != (tt.blocker == "") {
				t.Fatalf("reach = %+v, want blocker %q", got, tt.blocker)
			}
		})
	}
}
