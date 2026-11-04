package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// heldSession is the session a green, mergeable factory PR serves today for
// the admission answer state/reason.
func heldSession(state attention.AdmissionState, reason, display string) *pb.Session {
	return &pb.Session{Id: "held", DisplayLabel: display, Attention: attention.Compute(attention.Input{
		HasPR: true, PRState: attention.PRStateOpen,
		ChecksTotal: 7, ChecksDone: 7, Mergeable: proto.Bool(true),
		IsFactoryRepo:   true,
		VerifyAdmission: attention.Admission{State: state, Reason: reason},
	})}
}

// TestCandidateHoldLabel_EveryPin requires every pin older than V20261102 to
// receive, for each new hold, exactly the bytes it was served for the same head
// before: an admitted head's queued row for a candidate-step hold, and the
// ticket-not-in-review row for not-scanned.
func TestCandidateHoldLabel_EveryPin(t *testing.T) {
	reg := DefaultRegistry()
	type before struct {
		state  attention.AdmissionState
		reason string
		label  string
	}
	for reason, was := range map[string]before{
		"ci-red":           {attention.AdmissionAdmitted, "ticket", attention.WaitingForVerifyLabel},
		"ci-unsettled":     {attention.AdmissionAdmitted, "ticket", attention.WaitingForVerifyLabel},
		"candidate-unread": {attention.AdmissionAdmitted, "ticket", attention.WaitingForVerifyLabel},
		"not-scanned":      {attention.AdmissionPending, "ticket-not-in-review", "needs In Review"},
	} {
		for _, display := range []string{"✓ passing", "idle", "waiting", "usage-limited (resets soon)"} {
			for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure} {
				for _, v := range reg.All() {
					t.Run(reason+"/"+display+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
						source := heldSession(attention.AdmissionPending, reason, display)
						if label := source.GetAttention().GetProgressLabel(); label == was.label || !attention.IsVerifyHoldLabel(label) {
							t.Fatalf("today's label %q is not a new hold", label)
						}
						pristine := proto.Clone(source)
						msg, get := sessionResponse(procedure, source)
						ProductionChanges().Apply(procedure, msg, v)
						if !proto.Equal(source, pristine) {
							t.Fatal("mutated cached source")
						}
						got := get(msg)
						if !reg.Newer(V20261102, v) {
							if got != source {
								t.Fatal("current pin cloned source")
							}
							return
						}
						previous := heldSession(was.state, was.reason, display)
						prevMsg, prevGet := sessionResponse(procedure, previous)
						ProductionChanges().Apply(procedure, prevMsg, v)
						if want := prevGet(prevMsg); !proto.Equal(got, want) {
							t.Fatalf("pin %s got %v, want its previous answer %v", v, got, want)
						}
						if v == V20261101 && (got.GetAttention().GetProgressLabel() != was.label || got.GetAttention().GetStatusPhrase() != was.label) {
							t.Fatalf("V20261101 attention = %v, want %q label and phrase", got.GetAttention(), was.label)
						}
					})
				}
			}
		}
	}
}

func TestCandidateHoldLabel_LeavesOtherLabelsAlone(t *testing.T) {
	for _, label := range []string{"", "queued", "needs In Review", "verify unknown", "not verifiable", "working", "CI red"} {
		source := &pb.Session{Attention: &pb.SessionAttention{ProgressLabel: label, StatusPhrase: label}}
		if got := downconvertCandidateHoldLabelSession(source); got != source {
			t.Fatalf("non-hold %q cloned", label)
		}
	}
	for _, source := range []*pb.Session{nil, {}, {Attention: &pb.SessionAttention{}}} {
		if downconvertCandidateHoldLabelSession(source) != source {
			t.Fatal("empty session cloned")
		}
	}
	// A phrase that is not the label, such as a repair tally, is kept.
	source := &pb.Session{Attention: &pb.SessionAttention{ProgressLabel: "CI unsettled", StatusPhrase: "other"}}
	got := downconvertCandidateHoldLabelSession(source)
	if got.GetAttention().GetProgressLabel() != attention.WaitingForVerifyLabel || got.GetAttention().GetStatusPhrase() != "other" || source.GetAttention().GetProgressLabel() != "CI unsettled" {
		t.Fatalf("conversion = %v from %v", got, source)
	}
}

func TestCandidateHoldLabel_MethodAndPayload(t *testing.T) {
	source := heldSession(attention.AdmissionPending, "ci-red", "✓ passing")
	pristine := proto.Clone(source)
	msg, get := sessionResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, source)
	CandidateHoldLabelChange{}.TransformResponse("/other/method", msg)
	if get(msg) != source || !proto.Equal(source, pristine) {
		t.Fatal("wrong method changed source")
	}
	CandidateHoldLabelChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, &pb.ProxyListSessionsResponse{})
	CandidateHoldLabelChange{}.TransformResponse(bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, msg)
	if got := get(msg).GetAttention(); got.GetProgressLabel() != attention.WaitingForVerifyLabel || got.GetStatusPhrase() != attention.WaitingForVerifyLabel || !proto.Equal(source, pristine) {
		t.Fatalf("conversion = %v, want queued without touching the source", got)
	}
}
