package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

func TestReadyLabelPinnedAndCurrent(t *testing.T) {
	for _, method := range []string{bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure} {
		for _, version := range []Version{V20260915, DefaultRegistry().Current()} {
			t.Run(method+string(version), func(t *testing.T) {
				s := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, DisplayLabel: displaystatus.ReadyLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, HasBuildReceipt: true}
				var msg any
				if method == bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure {
					msg = &pb.ProxyListSessionsResponse{Sessions: []*pb.Session{s}}
				} else {
					msg = &pb.ProxyGetSessionResponse{Session: s}
				}
				ProductionChanges().Apply(method, msg, version)
				var got *pb.Session
				switch m := msg.(type) {
				case *pb.ProxyListSessionsResponse:
					got = m.Sessions[0]
				case *pb.ProxyGetSessionResponse:
					got = m.Session
				}
				want := "✓ passing"
				if version == DefaultRegistry().Current() {
					want = displaystatus.ReadyLabel
				}
				if got.DisplayLabel != want {
					t.Fatalf("label=%q want=%q", got.DisplayLabel, want)
				}
				if s.DisplayLabel != displaystatus.ReadyLabel {
					t.Fatal("mutated shared session")
				}
				if !got.HasBuildReceipt {
					t.Fatal("receipt cleared")
				}
			})
		}
	}
}
func TestReadyComposesWithWaitingInverses(t *testing.T) {
	s := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING, DisplayLabel: displaystatus.ReadyLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, HasBuildReceipt: true, IsWaitingDemoted: true}
	got := applyToSession(t, s, Baseline)
	if got.DisplayLabel != "working" || !got.DisplaySpinner {
		t.Fatalf("chain output=%+v", got)
	}
	s.IsWaitingDemoted = false
	s.IsReadyOverWaiting = true
	got = applyToSession(t, s, V20260915)
	if got.DisplayLabel != displaystatus.WaitingLabel || !got.DisplaySpinner {
		t.Fatalf("waiting inverse=%+v", got)
	}
}
