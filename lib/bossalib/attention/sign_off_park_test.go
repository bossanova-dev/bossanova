package attention_test

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// A sign-off park stays the head's park whatever outranks it in Compute: the
// review queue lists the PR by it while a chat asks or works.
func TestSignOffParkOutlivesChatActivity_Sessions_AC209001(t *testing.T) {
	parkSince := stateSince.Add(-2 * time.Hour)
	parked := withParks(readyPR(), "human-review:database")
	parked.ParkSince = parkSince
	parked.ParkLabel = "moderate: Migrations"
	parked.HasTrackerLink = true

	idle := attention.Compute(parked)
	if idle.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
		t.Fatalf("idle park kind = %v, want REVIEW", idle.GetKind())
	}
	if got := attention.SignOffPark(parked); !proto.Equal(got, idle) {
		t.Fatalf("idle SignOffPark = %v, want Compute's REVIEW %v", got, idle)
	}

	question := parked
	question.QuestionChatID, question.QuestionText = "chat-1", "Which base?"
	working := parked
	working.ChatStatus, working.ChatAtWork = pb.ChatStatus_CHAT_STATUS_WORKING, true
	fault := parked
	fault.Fix = &attention.FixFact{AlertKey: "conflict", Sentence: "The PR conflicts with its base", Since: stateSince}
	for name, in := range map[string]attention.Input{"question": question, "working chat": working, "fault": fault} {
		t.Run(name, func(t *testing.T) {
			if got := attention.Compute(in).GetKind(); got == pb.AttentionKind_ATTENTION_KIND_REVIEW {
				t.Fatalf("Compute kind = %v, want the activity to outrank the park", got)
			}
			if got := attention.SignOffPark(in); !proto.Equal(got, idle) {
				t.Fatalf("SignOffPark = %v, want the idle park %v", got, idle)
			}
		})
	}
}

func TestSignOffParkIsNilOffAnOpenSignOff_Sessions_AC209001(t *testing.T) {
	merged := withParks(readyPR(), "human-review:ui")
	merged.PRState = attention.PRStateMerged
	closed := withParks(readyPR(), "human-review:ui")
	closed.PRState = attention.PRStateClosed
	noPR := withParks(readyPR(), "human-review:ui")
	noPR.HasPR = false
	partial := withParks(unverifiedPR(), "human-review:ui")
	partial.IsPartial = true
	for name, in := range map[string]attention.Input{
		"unparked head":             readyPR(),
		"blocked park":              withParks(readyPR(), "repair-exhausted"),
		"sign-off beside a blocker": withParks(readyPR(), "human-review:ui", "cannot-assess"),
		"partial build":             partial,
		"merged":                    merged,
		"closed":                    closed,
		"no PR":                     noPR,
	} {
		t.Run(name, func(t *testing.T) {
			if got := attention.SignOffPark(in); got != nil {
				t.Fatalf("SignOffPark = %v, want nil", got)
			}
		})
	}
}
