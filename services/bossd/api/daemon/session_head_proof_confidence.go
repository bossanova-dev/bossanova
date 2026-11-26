package server

import (
	"context"
	"slices"
	"strings"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// hydrateHeadProofConfidence serves the proof record bossd holds for the PR
// head it currently tracks, in any session state, as the record wrote it: its
// level, lowered_by and asks, never a sign-off park's operator-label view.
// A record for another head or a legacy record (no level) leaves it unset.
func (s *Server) hydrateHeadProofConfidence(ctx context.Context, p *pb.Session, d attentionDeps) {
	p.PrHeadProofConfidence = nil
	if d.row == nil || s.evidence == nil {
		return
	}
	head := s.trackedPRHead(ctx, d)
	if head == "" {
		return
	}
	proof, ok := s.evidence.Proof(d.row.ID, head)
	if !ok || !strings.EqualFold(proof.Head, head) || proof.Level == "" {
		return
	}
	p.PrHeadProofConfidence = &pb.ProofConfidence{
		HeadSha:   proof.Head,
		Level:     attention.ProofConfidenceLevel(proof.Level),
		LoweredBy: slices.Clone(proof.LoweredBy),
		Asks:      slices.Clone(proof.Asks),
	}
}
