package apiversion

import (
	"slices"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// ProofAttentionChange restores video-only attention for clients predating
// confidence levels and head-pinned proof pages.
type ProofAttentionChange struct{}

// Version implements VersionChange.
func (ProofAttentionChange) Version() Version { return V20261101 }

// TransformResponse implements VersionChange for session-bearing unary responses.
func (ProofAttentionChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertProofAttentionSession)
}

func downconvertProofAttentionSession(s *pb.Session) *pb.Session {
	a := s.GetAttention()
	if a.GetConfidenceLevel() == pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED && len(a.GetAsks()) == 0 && a.GetProofUrl() == "" && len(a.GetLoweredBy()) == 0 && !slices.Contains(a.GetActions(), pb.AttentionAction_ATTENTION_ACTION_PROOF) {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	a = cloned.Attention
	a.ConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	a.Asks = nil
	a.ProofUrl = ""
	a.LoweredBy = nil
	actions := a.Actions[:0]
	hasVideo := false
	for _, action := range a.Actions {
		if action == pb.AttentionAction_ATTENTION_ACTION_PROOF {
			if a.EvidenceUrl == "" {
				continue
			}
			action = pb.AttentionAction_ATTENTION_ACTION_VIDEO
		}
		if action == pb.AttentionAction_ATTENTION_ACTION_VIDEO {
			if hasVideo {
				continue
			}
			hasVideo = true
		}
		actions = append(actions, action)
	}
	a.Actions = actions
	return cloned
}
