package apiversion

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// reviewOnlyParkMixes are the parks the operator saw read red "blocked" on
// green PRs, and two that are still blocked because a code means something is
// wrong.
var reviewOnlyParkMixes = []struct {
	name         string
	codes        []string
	blocked      bool
	sentence     string
	sentence0921 string
}{
	{"#3162 no-receipt + UI", []string{"no-receipt", "human-review:ui"}, false, "UI change", "Built by hand, no factory review"},
	{"#3184 UI + unproven", []string{"human-review:ui", "human-review:unproven"}, false, "UI change", "Some checks have no evidence"},
	{"#3189 unproven", []string{"human-review:unproven"}, false, "Some checks have no evidence", "Some checks have no evidence"},
	{"no-receipt", []string{"no-receipt"}, false, "No review of the latest commit", "Built by hand, no factory review"},
	{"no-receipt + open checklist", []string{"no-receipt", "ledger-open"}, true, "The PR checklist has open items", "Built by hand, no factory review"},
	{"unproven + repair exhausted", []string{"human-review:unproven", "repair-exhausted"}, true, "Verify is blocked", "Some checks have no evidence"},
}

// TestReviewOnlyPark_CurrentAndPreviousPin checks each mix at V20260922, where
// a park that only needs a person's look is a REVIEW offering MERGE that reads
// "needs sign-off" / WARNING and a park with a failure stays a blocked FIX
// reading "blocked" / DANGER, and at V20260921, where every one of them is a
// blocked FIX reading "blocked" with its old sentence, on ProxyGetSession and
// ProxyListSessions.
func TestReviewOnlyPark_CurrentAndPreviousPin(t *testing.T) {
	for _, mix := range reviewOnlyParkMixes {
		for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
			for _, v := range []Version{V20260922, V20260921} {
				t.Run(mix.name+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
					source := reviewOnlyParkSession("s", mix.codes...)
					pristine := proto.Clone(source)
					msg, get := sessionResponse(procedure, source)
					ProductionChanges().Apply(procedure, msg, v)
					if !proto.Equal(source, pristine) {
						t.Fatal("mutated cached source")
					}
					got := get(msg)
					a := got.GetAttention()
					blocked, sentence := mix.blocked, mix.sentence
					if v == V20260921 {
						blocked, sentence = true, mix.sentence0921
					}
					wantKind, wantLabel, wantIntent := pb.AttentionKind_ATTENTION_KIND_REVIEW, displaystatus.NeedsSignOffLabel, pb.DisplayIntent_DISPLAY_INTENT_WARNING
					if blocked {
						wantKind, wantLabel, wantIntent = pb.AttentionKind_ATTENTION_KIND_FIX, displaystatus.BlockedLabel, pb.DisplayIntent_DISPLAY_INTENT_DANGER
					}
					if a.GetKind() != wantKind || a.GetSentence() != sentence {
						t.Errorf("attention = %v %q, want %v %q", a.GetKind(), a.GetSentence(), wantKind, sentence)
					}
					if merges := slices.Contains(a.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE); merges == blocked {
						t.Errorf("actions = %v; a blocked park must not offer MERGE and a review must", a.GetActions())
					}
					if got.GetDisplayLabel() != wantLabel || got.GetDisplayIntent() != wantIntent || got.GetDisplaySpinner() {
						t.Errorf("display = %q %v spinner=%v, want %q %v", got.GetDisplayLabel(), got.GetDisplayIntent(), got.GetDisplaySpinner(), wantLabel, wantIntent)
					}
					if v == V20260922 && !proto.Equal(got, downconvertParkVerdictSentence(source)) {
						t.Error("the current pin must receive today's answer unchanged")
					}
					if !slices.Equal(a.GetParkCodes(), source.GetAttention().GetParkCodes()) {
						t.Errorf("park codes = %q, want %q kept", a.GetParkCodes(), source.GetAttention().GetParkCodes())
					}
				})
			}
		}
	}
}

// TestReviewOnlyPark_LeavesOtherRowsAlone checks that rows the change did not
// move pass through as the same pointer: a park without either code, a row
// with no attention, and a "blocked" row whose codes include one.
func TestReviewOnlyPark_LeavesOtherRowsAlone(t *testing.T) {
	blockedUnproven := reviewOnlyParkSession("blocked", "ledger-open")
	blockedUnproven.VerifyParkCodes = []string{"ledger-open", "human-review:unproven"}
	for name, source := range map[string]*pb.Session{
		"UI park":          reviewOnlyParkSession("ui", "human-review:ui"),
		"open checklist":   reviewOnlyParkSession("ledger", "ledger-open"),
		"no attention":     {DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, DisplayLabel: displaystatus.NeedsSignOffLabel},
		"blocked label":    blockedUnproven,
		"not a park label": {DisplayLabel: displaystatus.NeedsSignOffLabel, VerifyParkCodes: []string{"no-receipt"}},
	} {
		if got := downconvertReviewOnlyParkSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
