package attention_test

import (
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestCompute_Sessions_AC101_PartialMarker(t *testing.T) {
	for _, repo := range []struct {
		name     string
		factory  bool
		excluded bool
	}{
		{name: "nonfactory"},
		{name: "factory", factory: true},
		{name: "excluded", factory: true, excluded: true},
	} {
		t.Run(repo.name, func(t *testing.T) {
			in := unverifiedPR()
			in.IsFactoryRepo = repo.factory
			if repo.excluded {
				in.VerifyAdmission = attention.Admission{State: attention.AdmissionExcluded}
			}
			ordinary := attention.Compute(in)
			in.IsPartial = true
			got := attention.Compute(in)
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetSentence() != "Not verified · blocked · only part of the ticket was finished, so the PR is held" {
				t.Fatalf("partial = %v, want FIX Partial build", got)
			}
			if slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE) {
				t.Errorf("partial offers MERGE: %v", got.GetActions())
			}
			if !slices.Equal(got.GetParkCodes(), []string{attention.ParkCodePartial}) {
				t.Errorf("park codes = %v", got.GetParkCodes())
			}
			in.IsPartial = false
			if restored := attention.Compute(in); !proto.Equal(restored, ordinary) {
				t.Errorf("nonpartial changed: got %v, want %v", restored, ordinary)
			}
		})
	}
}

func TestCompute_Sessions_AC102_PartialMarkerLeavesIneligiblePRUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*attention.Input)
	}{
		{"running checks", func(in *attention.Input) { in.ChecksDone-- }},
		{"failing checks", func(in *attention.Input) { in.ChecksFailing = 1 }},
		{"conflicting", func(in *attention.Input) { in.Mergeable = boolPtr(false) }},
		{"draft", func(in *attention.Input) { in.PRDraft = true }},
		{"merged", func(in *attention.Input) { in.PRState = attention.PRStateMerged }},
		{"closed", func(in *attention.Input) { in.PRState = attention.PRStateClosed }},
		{"verified", func(in *attention.Input) { in.Verified = true }},
		{"verified factory within grace", func(in *attention.Input) { *in = verifiedFactoryPR(verifiedAt.Add(time.Second)) }},
		{"verified factory after grace", func(in *attention.Input) { *in = verifiedFactoryPR(verifiedAt.Add(attention.VerifyMergeGrace)) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := unverifiedPR()
			tt.change(&in)
			want := attention.Compute(in)
			in.IsPartial = true
			if got := attention.Compute(in); !proto.Equal(got, want) {
				t.Errorf("partial = %v, want unchanged %v", got, want)
			}
		})
	}
}

func TestCompute_PartialMarkerCombinesParkCodesWithoutMutation(t *testing.T) {
	for _, codes := range [][]string{
		{"no-receipt"},
		{"no-receipt,partial-build", "partial-build"},
	} {
		in := unverifiedPR()
		in.IsPartial = true
		// Spare capacity makes an append into the caller's slice detectable.
		storage := make([]string, len(codes)+1)
		copy(storage, codes)
		storage[len(codes)] = "untouched"
		in.ParkCodes = storage[:len(codes)]
		before := slices.Clone(storage)
		got := attention.Compute(in)
		if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetSentence() != "Not verified · blocked · only part of the ticket was finished, so the PR is held" || slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE) {
			t.Errorf("combined partial = %v, want FIX without MERGE", got)
		}
		if !slices.Equal(got.GetParkCodes(), []string{"no-receipt", attention.ParkCodePartial}) {
			t.Errorf("park codes = %v, want no-receipt and one partial-build", got.GetParkCodes())
		}
		if !slices.Equal(storage, before) {
			t.Errorf("caller park codes mutated: %v, want %v", storage, before)
		}
	}
}
