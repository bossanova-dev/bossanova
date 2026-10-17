package apiversion

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var endedBlockEntered = time.Date(2026, 10, 17, 14, 3, 52, 0, time.UTC)

// endedBlockSession is a Blocked run whose agent works again, as V20260923
// serves it: in progress, with no legacy hint.
func endedBlockSession() *pb.Session {
	reason := "run blocked: flows CI remains red after 5 repair passes"
	return &pb.Session{
		Id:             "sess-ended-block",
		State:          pb.SessionState_SESSION_STATE_BLOCKED,
		BlockedReason:  &reason,
		StateEnteredAt: timestamppb.New(endedBlockEntered),
		UpdatedAt:      timestamppb.New(endedBlockEntered.Add(time.Hour)),
		Attention:      &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, ProgressLabel: "working"},
	}
}

// TestEndedBlockHint_EveryPinKeepsItsHint checks every version: from V20260923
// the ended block carries no attention_status; older pins get the hint every
// Blocked session carried, built from the reason and the state time.
func TestEndedBlockHint_EveryPinKeepsItsHint(t *testing.T) {
	reg := DefaultRegistry()
	for _, v := range reg.All() {
		t.Run(v.String(), func(t *testing.T) {
			source := endedBlockSession()
			pristine := proto.Clone(source)
			resp := &pb.ProxyGetSessionResponse{Session: source}
			ProductionChanges().Apply(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, resp, v)
			if !proto.Equal(source, pristine) {
				t.Fatal("mutated cached source")
			}
			got := resp.GetSession().GetAttentionStatus()
			if !reg.Newer(V20260923, v) {
				if got != nil {
					t.Fatalf("attention_status = %v, want none at %s", got, v)
				}
				return
			}
			want := &pb.AttentionStatus{
				NeedsAttention: true,
				Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
				Summary:        source.GetBlockedReason(),
				Since:          timestamppb.New(endedBlockEntered),
			}
			if !proto.Equal(got, want) {
				t.Fatalf("attention_status = %v, want %v", got, want)
			}
		})
	}
}

// TestEndedBlockHint_RebuildsTheDefaultSummary checks a Blocked row with no
// reason and no state time: the default summary, dated from updated_at.
func TestEndedBlockHint_RebuildsTheDefaultSummary(t *testing.T) {
	updated := endedBlockEntered.Add(time.Hour)
	got := downconvertEndedBlockHintSession(&pb.Session{State: pb.SessionState_SESSION_STATE_BLOCKED, UpdatedAt: timestamppb.New(updated)}).GetAttentionStatus()
	if got.GetSummary() != "blocked — needs human intervention" || !got.GetSince().AsTime().Equal(updated) {
		t.Fatalf("attention_status = %v, want the default summary since updated_at", got)
	}
}

// TestEndedBlockHint_LeavesOtherRowsAlone checks that a Blocked row that still
// carries a hint, and a row in any other state, pass through untouched.
func TestEndedBlockHint_LeavesOtherRowsAlone(t *testing.T) {
	stalled := &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AGENT_STALLED}
	for name, source := range map[string]*pb.Session{
		"blocked with a hint": {State: pb.SessionState_SESSION_STATE_BLOCKED, AttentionStatus: stalled},
		"implementing":        {State: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN},
		"orphaned":            {State: pb.SessionState_SESSION_STATE_ORPHANED},
	} {
		if got := downconvertEndedBlockHintSession(source); got != source {
			t.Errorf("%s rewritten to %v", name, got)
		}
	}
}
