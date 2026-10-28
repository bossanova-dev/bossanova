package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestStateEndingErroredIntentEveryPin(t *testing.T) {
	reg := DefaultRegistry()
	for _, state := range []pb.SessionState{pb.SessionState_SESSION_STATE_BLOCKED, pb.SessionState_SESSION_STATE_ORPHANED} {
		for _, tc := range []struct {
			label  string
			intent pb.DisplayIntent
		}{
			{"merging", pb.DisplayIntent_DISPLAY_INTENT_INFO}, {"archiving", pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		} {
			for _, procedure := range []string{bossanovav1connect.OrchestratorServiceProxyGetSessionProcedure, bossanovav1connect.OrchestratorServiceProxyListSessionsProcedure, bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure} {
				for _, v := range reg.All() {
					t.Run(state.String()+"/"+tc.label+"/"+procedureMethod(procedure)+"/"+v.String(), func(t *testing.T) {
						source := &pb.Session{State: state, DisplayLabel: tc.label, DisplayIntent: tc.intent, DisplaySpinner: true}
						pristine := proto.Clone(source)
						var msg any
						var get func(any) *pb.Session
						if procedure == bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure {
							msg = &pb.ProxyCreateSessionResponse{Body: &pb.ProxyCreateSessionResponse_Created{Created: source}}
							get = func(m any) *pb.Session { return m.(*pb.ProxyCreateSessionResponse).GetCreated() }
						} else {
							msg, get = sessionResponse(procedure, source)
						}
						ProductionChanges().Apply(procedure, msg, v)
						if !proto.Equal(source, pristine) {
							t.Fatal("mutated cached source")
						}
						wantLabel, wantIntent, wantSpinner := tc.label, tc.intent, true
						if reg.Newer(V20260929, v) {
							wantIntent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
						}
						if reg.Newer(V20260718, v) {
							wantIntent = tc.intent
							if state == pb.SessionState_SESSION_STATE_ORPHANED {
								wantLabel, wantIntent, wantSpinner = "orphaned", pb.DisplayIntent_DISPLAY_INTENT_DANGER, false
							}
						}
						got := get(msg)
						if got.GetDisplayLabel() != wantLabel || got.GetDisplayIntent() != wantIntent || got.GetDisplaySpinner() != wantSpinner {
							t.Fatalf("got %q/%s/%v, want %q/%s/%v", got.GetDisplayLabel(), got.GetDisplayIntent(), got.GetDisplaySpinner(), wantLabel, wantIntent, wantSpinner)
						}
					})
				}
			}
		}
	}
}

func TestStateEndingErroredIntentUnchangedPointers(t *testing.T) {
	for _, s := range []*pb.Session{nil, {}, {DisplayLabel: "merging", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO}, {State: pb.SessionState_SESSION_STATE_BLOCKED, DisplayLabel: "✓ passing", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}, {State: pb.SessionState_SESSION_STATE_BLOCKED, DisplayLabel: "merging", DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}} {
		if got := downconvertStateEndingErroredSession(s); got != s {
			t.Fatalf("unchanged row cloned: %v", s)
		}
	}
}
