package server

import (
	"context"
	"strings"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
)

// hydrateMergedConfidence shares the merged head's proof level across reads
// and the stream. Attention itself remains DONE for merged work.
func (s *Server) hydrateMergedConfidence(ctx context.Context, p *pb.Session, d attentionDeps) {
	p.MergedConfidenceLevel = pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED
	if d.row.State != machine.Merged || s.evidence == nil {
		return
	}
	head := ""
	if d.display != nil {
		head = d.display.HeadSHA
	} else if s.checkSnapshots != nil {
		if snaps, err := s.checkSnapshots.ListBySession(ctx, d.row.ID, 1); err == nil && len(snaps) > 0 {
			head = snaps[0].HeadSHA
		}
	}
	if head == "" {
		return
	}
	if proof, ok := s.evidence.Proof(d.row.ID, head); ok && strings.EqualFold(proof.Head, head) {
		p.MergedConfidenceLevel = attention.ProofConfidenceLevel(proof.Level)
	}
}
