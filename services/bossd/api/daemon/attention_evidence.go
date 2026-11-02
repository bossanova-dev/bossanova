package server

import (
	"slices"
	"strings"

	"github.com/recurser/bossalib/attention"
)

// hydrateAttentionEvidence supplies only current-head proof and legacy media.
func (s *Server) hydrateAttentionEvidence(sessionID, head string, in *attention.Input) {
	if s.evidence == nil || head == "" {
		return
	}
	if proof, ok := s.evidence.Proof(sessionID, head); ok && strings.EqualFold(proof.Head, head) {
		in.Proof = &attention.Proof{URL: proof.Page, Level: proof.Level, Asks: slices.Clone(proof.Asks), LoweredBy: slices.Clone(proof.LoweredBy)}
		for _, media := range proof.Media {
			if media.Kind == "video" && media.URL != "" {
				in.Evidence = &attention.Evidence{URL: media.URL, VideoDuration: media.Duration}
				break
			}
		}
	}
	if entry, ok := s.evidence.Entry(sessionID); ok && strings.EqualFold(entry.Summary.Head, head) {
		summary := entry.Summary
		in.Evidence = &attention.Evidence{URL: summary.VideoURL, VideoDuration: summary.VideoDuration, Proven: summary.Proven, Total: summary.Total}
	}
}
