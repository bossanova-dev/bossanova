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
	head := s.trackedPRHead(ctx, d)
	if head == "" {
		return
	}
	if proof, ok := s.evidence.Proof(d.row.ID, head); ok && strings.EqualFold(proof.Head, head) {
		p.MergedConfidenceLevel = attention.ProofConfidenceLevel(proof.Level)
	}
}

// trackedPRHead is the head bossd currently tracks for the session's PR: the
// display entry's head, else the newest check snapshot's. Empty when neither
// is known.
func (s *Server) trackedPRHead(ctx context.Context, d attentionDeps) string {
	if d.display != nil {
		return d.display.HeadSHA
	}
	if s.checkSnapshots != nil {
		if snaps, err := s.checkSnapshots.ListBySession(ctx, d.row.ID, 1); err == nil && len(snaps) > 0 {
			return snaps[0].HeadSHA
		}
	}
	return ""
}
