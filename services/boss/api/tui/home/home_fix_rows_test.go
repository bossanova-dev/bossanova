package home

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestHomeFixRowsOfferNoMerge(t *testing.T) {
	fault := attentionSession(pb.AttentionAction_ATTENTION_ACTION_RESUME, pb.AttentionAction_ATTENTION_ACTION_DISMISS)
	fault.Attention.Kind, fault.Attention.ParkCodes = pb.AttentionKind_ATTENTION_KIND_FIX, nil
	rows := map[string]*pb.Session{"fault": fault}
	for _, code := range knownParkCodes {
		if s := parkSession(code); s.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
			rows[code] = s
		}
	}
	for name, s := range rows {
		t.Run(name, func(t *testing.T) {
			s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
			c := &attentionStub{}
			if _, cmd := attentionHome(c, proto.CloneOf(s)).Update(keyPress('m')); cmd != nil {
				t.Fatal("[m] dispatched on a Home fix row")
			}
		})
	}
}
