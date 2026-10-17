package attention_test

import (
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

var verifiedAt = time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)

// verifiedFactoryPR is readyPR in a repo with an enabled verify job, verified
// at verifiedAt and still open when the answer is computed at now.
func verifiedFactoryPR(now time.Time) attention.Input {
	in := readyPR()
	in.IsFactoryRepo = true
	in.VerifiedSince = verifiedAt
	in.Now = now
	in.ChatStatus = pb.ChatStatus_CHAT_STATUS_IDLE
	return in
}

// TestCompute_VerifiedNotMergedIsTheOperatorsOnceTheGraceEnds checks the
// boundary: verify gets VerifyMergeGrace to merge a head it passed; from the
// moment the grace ends the PR is a MERGE dated from the verification, reading
// VerifiedNotMergedSentence, and carrying the label it showed in progress.
func TestCompute_VerifiedNotMergedIsTheOperatorsOnceTheGraceEnds(t *testing.T) {
	before := attention.Compute(verifiedFactoryPR(verifiedAt.Add(attention.VerifyMergeGrace - time.Nanosecond)))
	if before.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || before.GetProgressLabel() != "idle" {
		t.Fatalf("inside the grace = %v %q, want in progress, idle", before.GetKind(), before.GetProgressLabel())
	}
	for _, after := range []time.Duration{0, time.Second, 6 * time.Hour} {
		got := attention.Compute(verifiedFactoryPR(verifiedAt.Add(attention.VerifyMergeGrace + after)))
		if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE || !attention.NeedsYou(got.GetKind()) {
			t.Fatalf("grace+%v: kind = %v, want MERGE under NEEDS YOU", after, got.GetKind())
		}
		if got.GetSentence() != attention.VerifiedNotMergedSentence || got.GetIsUnverified() {
			t.Fatalf("grace+%v: %q unverified=%v, want %q verified", after, got.GetSentence(), got.GetIsUnverified(), attention.VerifiedNotMergedSentence)
		}
		if !slices.Equal(got.GetActions(), mergeActions) {
			t.Fatalf("grace+%v: actions = %v, want %v", after, got.GetActions(), mergeActions)
		}
		if !got.GetSince().AsTime().Equal(verifiedAt) {
			t.Fatalf("grace+%v: since = %v, want the verification at %v", after, got.GetSince().AsTime(), verifiedAt)
		}
		if got.GetProgressLabel() != before.GetProgressLabel() {
			t.Fatalf("grace+%v: progress label = %q, want %q carried", after, got.GetProgressLabel(), before.GetProgressLabel())
		}
	}
}

// TestCompute_VerifiedNotMergedNeedsEveryFact checks that the factory MERGE
// needs a verified head with a known verification time, a clock, and a PR that
// is otherwise merge-ready and not parked or outranked.
func TestCompute_VerifiedNotMergedNeedsEveryFact(t *testing.T) {
	late := verifiedAt.Add(2 * attention.VerifyMergeGrace)
	for name, edit := range map[string]func(*attention.Input){
		"unverified":          func(in *attention.Input) { in.Verified = false },
		"verification unseen": func(in *attention.Input) { in.VerifiedSince = time.Time{} },
		"sent back":           func(in *attention.Input) { in.IsSentBack = true },
		"no clock":            func(in *attention.Input) { in.Now = time.Time{} },
		"draft":               func(in *attention.Input) { in.PRDraft = true },
		"check failing":       func(in *attention.Input) { in.ChecksFailing = 1 },
		"checks running":      func(in *attention.Input) { in.ChecksDone = 6 },
		"not mergeable":       func(in *attention.Input) { in.Mergeable = boolPtr(false) },
		"mergeable unknown":   func(in *attention.Input) { in.Mergeable = nil },
		"merged":              func(in *attention.Input) { in.PRState = attention.PRStateMerged },
		"waiting on main CI":  func(in *attention.Input) { in.ParkCodes = []string{"base-ci-red"} },
	} {
		in := verifiedFactoryPR(late)
		edit(&in)
		if got := attention.Compute(in); got.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE {
			t.Errorf("%s: answered MERGE %v", name, got)
		}
	}
	question := verifiedFactoryPR(late)
	question.QuestionChatID = "chat"
	if got := attention.Compute(question).GetKind(); got != pb.AttentionKind_ATTENTION_KIND_QUESTION {
		t.Errorf("a question must outrank the merge, got %v", got)
	}
}

// TestCompute_VerifiedNotMergedIsIdempotent checks the answer depends only on
// its inputs: computing it again at the same clock gives the same message.
func TestCompute_VerifiedNotMergedIsIdempotent(t *testing.T) {
	in := verifiedFactoryPR(verifiedAt.Add(time.Hour))
	if first, again := attention.Compute(in), attention.Compute(in); !proto.Equal(first, again) {
		t.Fatalf("recomputed %v, first %v", again, first)
	}
}

// TestCompute_ParkIsDatedFromItsStart checks that a verify park, a REVIEW or a
// blocked FIX, is dated from ParkSince, and from StateSince only when the
// park's start is unknown. A fault outranking the park keeps its own date.
func TestCompute_ParkIsDatedFromItsStart(t *testing.T) {
	parkedAt := stateSince.Add(7 * time.Hour)
	for _, codes := range [][]string{{"human-review:ui"}, {"no-receipt"}, {"ledger-open"}, {"repair-exhausted", "human-review:security"}} {
		in := withParks(readyPR(), codes...)
		in.ParkSince = parkedAt
		if got := attention.Compute(in); !got.GetSince().AsTime().Equal(parkedAt) {
			t.Errorf("%v: since = %v, want the park's start %v", codes, got.GetSince().AsTime(), parkedAt)
		}
		in.ParkSince = time.Time{}
		if got := attention.Compute(in); !got.GetSince().AsTime().Equal(stateSince) {
			t.Errorf("%v without a park start: since = %v, want the state's %v", codes, got.GetSince().AsTime(), stateSince)
		}
		in.ParkSince = parkedAt
		in.Fix = fixFact("k")
		if got := attention.Compute(in); !got.GetSince().AsTime().Equal(in.Fix.Since) {
			t.Errorf("%v under a fault: since = %v, want the fault's %v", codes, got.GetSince().AsTime(), in.Fix.Since)
		}
	}
}
