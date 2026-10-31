package attention_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestVerifyHoldLabels(t *testing.T) {
	labels := attention.VerifyHoldLabels()
	if !slices.IsSorted(labels) || len(slices.Compact(slices.Clone(labels))) != len(labels) {
		t.Fatalf("labels not sorted and distinct: %v", labels)
	}
	for _, reason := range []string{"ticket-not-in-review", "draft", "attached", "ambiguous-pr", "claimed", "no-session", "session-has-ticket", "brand-new-code", ""} {
		in := unverifiedPR()
		in.IsFactoryRepo = true
		in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
		label := attention.Compute(in).GetProgressLabel()
		if !attention.IsVerifyHoldLabel(label) || !slices.Contains(labels, label) || strings.Contains(label, "outside verify") {
			t.Fatalf("reason %q: unexpected hold label %q", reason, label)
		}
	}
	for _, label := range labels {
		if attention.IsWaitLabel(label) || label == attention.VerifyUnknownLabel {
			t.Errorf("hold label collides with another progress label: %q", label)
		}
	}
	for _, label := range []string{"outside verify", "outside verify: draft", "outside verify: ticket not in review"} {
		if !attention.IsVerifyHoldLabel(label) {
			t.Errorf("legacy label %q not recognised", label)
		}
	}
	for _, label := range []string{"queued", "verify unknown", "working", "idle", "main red", "tests busy", "stopped", "waiting", "initializing", "checking 1/2", "outside verify unrelated", "outside verify:draft"} {
		if attention.IsVerifyHoldLabel(label) {
			t.Errorf("other label %q recognised as hold", label)
		}
	}
}

func TestCompute_VerifyAdmissionLabels_Sessions_AC105(t *testing.T) {
	for _, reason := range []string{"", "brand-new-code"} {
		in := unverifiedPR()
		in.IsFactoryRepo = true
		in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
		if got := attention.Compute(in).GetProgressLabel(); got != attention.VerifyHoldFallbackLabel {
			t.Fatalf("reason %q: got %q", reason, got)
		}
	}
}

func TestCompute_TicketPersonHold_Sessions_AC214401(t *testing.T) {
	testAdmissionHold(t, "ticket-needs-person", "Ticket waits on you · answer it or label it for build", "ticket waits on a person")
}

func TestCompute_TicketSelectionHold_Sessions_AC214402(t *testing.T) {
	testAdmissionHold(t, "ticket-not-selected", "Ticket not selected for verify · relabel it or merge by hand", "ticket not selected")
}

func testAdmissionHold(t *testing.T, reason, sentence, category string) {
	t.Helper()
	for _, checks := range []string{"green", "red", "running"} {
		for _, working := range []bool{false, true} {
			for _, linked := range []bool{false, true} {
				t.Run(checks+"/"+fmt.Sprint(working, linked), func(t *testing.T) {
					in := unverifiedPR()
					in.IsFactoryRepo = true
					in.HasTrackerLink = linked
					in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
					if checks == "red" {
						in.ChecksFailing = 1
					}
					if checks == "running" {
						in.ChecksDone = 1
					}
					if working {
						in.ChatStatus = pb.ChatStatus_CHAT_STATUS_WORKING
					}
					got := attention.Compute(in)
					wantActions := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB}
					if linked {
						wantActions = append(wantActions, pb.AttentionAction_ATTENTION_ACTION_TRACKER)
					}
					if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetSentence() != sentence || !slices.Equal(got.GetParkCodes(), []string{reason}) || !slices.Equal(got.GetActions(), wantActions) {
						t.Fatalf("attention = %v, want FIX %q with code %q and actions %v", got, sentence, reason, wantActions)
					}
					if got := attention.ParkCodeCategory(reason); got != category {
						t.Errorf("category = %q, want %q", got, category)
					}
					if attention.IsReportedParkCode(reason) {
						t.Error("admission hold must not be waived by park approval")
					}
				})
			}
		}
	}
}

func TestCompute_AdmissionHoldBoundaries_Sessions_AC214402(t *testing.T) {
	for _, reason := range []string{"ticket-needs-person", "ticket-not-selected"} {
		for name, mutate := range map[string]func(*attention.Input){
			"no PR":         func(in *attention.Input) { in.HasPR = false },
			"closed PR":     func(in *attention.Input) { in.PRState = attention.PRStateClosed },
			"merged PR":     func(in *attention.Input) { in.PRState = attention.PRStateMerged },
			"draft":         func(in *attention.Input) { in.PRDraft = true },
			"verified":      func(in *attention.Input) { in.Verified = true },
			"non-factory":   func(in *attention.Input) { in.IsFactoryRepo = false },
			"admitted":      func(in *attention.Input) { in.VerifyAdmission.State = attention.AdmissionAdmitted },
			"excluded":      func(in *attention.Input) { in.VerifyAdmission.State = attention.AdmissionExcluded },
			"unknown":       func(in *attention.Input) { in.VerifyAdmission.State = attention.AdmissionUnknown },
			"not in review": func(in *attention.Input) { in.VerifyAdmission.Reason = "ticket-not-in-review" },
		} {
			t.Run(reason+"/"+name, func(t *testing.T) {
				in := unverifiedPR()
				in.IsFactoryRepo = true
				in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: reason}
				mutate(&in)
				baseline := in
				baseline.VerifyAdmission.Reason = "ticket-not-in-review"
				if got, want := attention.Compute(in), attention.Compute(baseline); !proto.Equal(got, want) {
					t.Fatalf("attention = %v, want unchanged row %v", got, want)
				}
			})
		}
	}
}

func TestAdmissionHoldCodes(t *testing.T) {
	for _, code := range []string{attention.ParkCodeTicketNeedsPerson, attention.ParkCodeTicketNotSelected} {
		if !attention.IsAdmissionHoldCode(code) || attention.IsReportedParkCode(code) || attention.IsSignOffPark([]string{code}) {
			t.Errorf("%q must be an admission hold, never an approvable sign-off", code)
		}
		in := unverifiedPR()
		in.IsFactoryRepo = true
		in.VerifyAdmission = attention.Admission{State: attention.AdmissionPending, Reason: code}
		backing := []string{"no-receipt", "untouched"}
		in.ParkCodes = backing[:1]
		got := attention.Compute(in)
		if !slices.Equal(got.GetParkCodes(), []string{"no-receipt", code}) || backing[1] != "untouched" {
			t.Errorf("hold must follow gate codes without mutating input: attention=%v backing=%v", got, backing)
		}
	}
	for _, code := range []string{"ticket-not-in-review", "human-merge", "", "unknown"} {
		if attention.IsAdmissionHoldCode(code) {
			t.Errorf("%q is not an admission hold", code)
		}
	}
}
