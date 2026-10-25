package apiversion

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	sinceStateEntered = time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	sinceUpdated      = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	sinceVerified     = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	sinceParked       = time.Date(2026, 10, 16, 10, 5, 0, 0, time.UTC)
)

// sinceShape is one session shape bossd hydrates: in is the attention input
// as V20260922 computes it, base the one that drops what V20260922 added (a
// zero clock, no park start), which is exactly what V20260921 computed.
type sinceShape struct {
	name    string
	in      attention.Input
	base    func(attention.Input) attention.Input
	noState bool
	moved   pb.AttentionKind
}

func verifiedNotMergedInput(tracker bool, chat pb.ChatStatus, phase string) attention.Input {
	mergeable := true
	return attention.Input{
		HasPR: true, PRState: attention.PRStateOpen,
		ChecksTotal: 4, ChecksDone: 4, Mergeable: &mergeable,
		IsFactoryRepo: true, Verified: true,
		VerifiedSince: sinceVerified, Now: sinceVerified.Add(attention.VerifyMergeGrace),
		StateSince: sinceStateEntered, HasTrackerLink: tracker,
		ChatStatus: chat, ProgressPhase: phase,
	}
}

func parkInput(tracker bool, codes ...string) attention.Input {
	return attention.Input{
		HasPR: true, PRState: attention.PRStateOpen, ParkCodes: codes,
		Evidence:   &attention.Evidence{URL: "https://example.test/v.mp4"},
		StateSince: sinceStateEntered, ParkSince: sinceParked, HasTrackerLink: tracker,
	}
}

func withoutClock(in attention.Input) attention.Input     { in.Now = time.Time{}; return in }
func withoutParkStart(in attention.Input) attention.Input { in.ParkSince = time.Time{}; return in }

func sinceShapes() []sinceShape {
	var shapes []sinceShape
	for _, tracker := range []bool{false, true} {
		suffix := map[bool]string{false: "", true: " +tracker"}[tracker]
		shapes = append(shapes,
			sinceShape{name: "verified idle" + suffix, in: verifiedNotMergedInput(tracker, pb.ChatStatus_CHAT_STATUS_IDLE, ""), base: withoutClock, moved: pb.AttentionKind_ATTENTION_KIND_MERGE},
			sinceShape{name: "verified stopped no state time" + suffix, in: verifiedNotMergedInput(tracker, pb.ChatStatus_CHAT_STATUS_STOPPED, ""), base: withoutClock, noState: true, moved: pb.AttentionKind_ATTENTION_KIND_MERGE},
			sinceShape{name: "verified verifying" + suffix, in: verifiedNotMergedInput(tracker, pb.ChatStatus_CHAT_STATUS_WORKING, "verifying"), base: withoutClock, moved: pb.AttentionKind_ATTENTION_KIND_MERGE},
			sinceShape{name: "UI park" + suffix, in: parkInput(tracker, "human-review:ui"), base: withoutParkStart, moved: pb.AttentionKind_ATTENTION_KIND_REVIEW},
			sinceShape{name: "no-receipt park" + suffix, in: parkInput(tracker, "no-receipt"), base: withoutParkStart, moved: pb.AttentionKind_ATTENTION_KIND_REVIEW},
			sinceShape{name: "blocked park" + suffix, in: parkInput(tracker, "ledger-open", "human-review:unproven"), base: withoutParkStart, moved: pb.AttentionKind_ATTENTION_KIND_FIX},
			sinceShape{name: "blocked park no state time" + suffix, in: parkInput(tracker, "repair-exhausted"), base: withoutParkStart, noState: true, moved: pb.AttentionKind_ATTENTION_KIND_FIX},
		)
	}
	return shapes
}

// sinceSession is the row bossd serves for in: the attention, plus the state
// and update times the hydrator read StateSince from.
func sinceSession(shape sinceShape, in attention.Input) *pb.Session {
	s := &pb.Session{Id: "sess-since", UpdatedAt: timestamppb.New(sinceUpdated), StateEnteredAt: timestamppb.New(sinceStateEntered)}
	if shape.noState {
		s.StateEnteredAt = nil
		in.StateSince = sinceUpdated
	}
	s.Attention = attention.Compute(in)
	return s
}

// TestAttentionSince_EveryPinGetsItsOldAnswer is a differential check: for
// every shape and every registered version, the row V20260922 computes must
// reach a pin older than V20260922 byte-identical to the row V20260921
// computed, on ProxyGetSession and ProxyListSessions. From V20260922 the row
// retains its kind and since (later label changes still apply): a verified head
// verify left
// unmerged is a MERGE dated from its verification, and a park is dated from
// when it began.
func TestAttentionSince_EveryPinGetsItsOldAnswer(t *testing.T) {
	reg := DefaultRegistry()
	procedures := []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure}
	for _, shape := range sinceShapes() {
		today := sinceSession(shape, shape.in)
		before := sinceSession(shape, shape.base(shape.in))
		a := today.GetAttention()
		if a.GetKind() != shape.moved {
			t.Fatalf("%s: today's kind = %v, want %v", shape.name, a.GetKind(), shape.moved)
		}
		wantSince := sinceParked
		if shape.moved == pb.AttentionKind_ATTENTION_KIND_MERGE {
			wantSince = sinceVerified
			if a.GetSentence() != attention.VerifiedNotMergedSentence || a.GetIsUnverified() {
				t.Fatalf("%s: today = %v, want a verified MERGE reading %q", shape.name, a, attention.VerifiedNotMergedSentence)
			}
			if before.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
				t.Fatalf("%s: before = %v, want in progress", shape.name, before.GetAttention())
			}
		}
		if !a.GetSince().AsTime().Equal(wantSince) {
			t.Fatalf("%s: today's since = %v, want %v", shape.name, a.GetSince().AsTime(), wantSince)
		}
		if proto.Equal(today, before) {
			t.Fatalf("%s: today and before are the same row; the shape proves nothing", shape.name)
		}
		for _, procedure := range procedures {
			for _, v := range reg.All() {
				t.Run(shape.name+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
					source := proto.Clone(today).(*pb.Session)
					msg, get := sessionResponse(procedure, source)
					ProductionChanges().Apply(procedure, msg, v)
					if !proto.Equal(source, today) {
						t.Fatal("mutated cached source")
					}
					got := get(msg)
					if !reg.Newer(V20260922, v) {
						want := today
						if reg.Newer(V20260928, v) {
							want = downconvertShortWaitLabelSession(want)
						}
						if reg.Newer(V20260926, v) {
							want = downconvertWaitingForVerifyLabelSession(want)
						}
						if !proto.Equal(got, want) {
							t.Fatalf("got %v, want today's row with this pin's label: %v", got, want)
						}
						return
					}
					oldMsg, oldGet := sessionResponse(procedure, proto.Clone(before).(*pb.Session))
					ProductionChanges().Apply(procedure, oldMsg, v)
					if want := oldGet(oldMsg); !proto.Equal(got, want) {
						t.Fatalf("got  %v\nwant %v (what this pin was served before V20260922)", got, want)
					}
				})
			}
		}
	}
}

// TestAttentionSince_LeavesOtherRowsAlone checks that rows neither change
// moved pass through as the same pointer: a merge outside a factory repo, an
// in-progress row, a question, and a park already dated from its state.
func TestAttentionSince_LeavesOtherRowsAlone(t *testing.T) {
	mergeable := true
	stateDatedPark := parkInput(false, "human-review:ui")
	stateDatedPark.ParkSince = time.Time{}
	for name, in := range map[string]attention.Input{
		"ready merge":      {HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable, Verified: true, StateSince: sinceStateEntered},
		"in progress":      withoutClock(verifiedNotMergedInput(true, pb.ChatStatus_CHAT_STATUS_IDLE, "")),
		"question":         {QuestionChatID: "chat", StateSince: sinceUpdated},
		"state-dated park": stateDatedPark,
		"not yet overdue": func() attention.Input {
			in := verifiedNotMergedInput(false, pb.ChatStatus_CHAT_STATUS_IDLE, "")
			in.Now = in.Now.Add(-time.Second)
			return in
		}(),
	} {
		source := sinceSession(sinceShape{}, in)
		if got := downconvertVerifiedNotMergedSession(source); got != source {
			t.Errorf("%s rewritten by VerifiedNotMergedChange to %v", name, got)
		}
		if got := downconvertParkSinceSession(source); got != source {
			t.Errorf("%s rewritten by ParkSinceChange to %v", name, got)
		}
	}
}
