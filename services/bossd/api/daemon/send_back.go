package server

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
)

var sendBackDigest = regexp.MustCompile(`^sha256:[a-fA-F0-9]{64}$`)

// SendBackToRepair records a quoted operator work item through verify's blocked
// verdict. Once confirmed, it finishes on a bounded context even if the caller leaves.
func (s *Server) SendBackToRepair(ctx context.Context, req *connect.Request[pb.SendBackToRepairRequest]) (*connect.Response[pb.SendBackToRepairResponse], error) {
	pin, ok := vcs.NormalizeHeadSHA(req.Msg.GetExpectedHeadSha())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("expected_head_sha must be a 40-character hex commit SHA"))
	}
	digest := req.Msg.GetExpectedProofDigest()
	if digest != "" && !sendBackDigest.MatchString(digest) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("expected_proof_digest must be sha256:<64 hex>"))
	}
	note := req.Msg.GetNote()
	if !utf8.ValidString(note) || strings.TrimSpace(note) == "" || utf8.RuneCountInString(note) > 2000 || strings.ContainsFunc(note, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("note must contain 1 to 2000 code points and no control characters except newline and tab"))
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), domain.VerifyVerbTimeout)
	defer cancel()
	target, env, err := s.verifyTarget(runCtx, req.Msg.GetId(), false)
	if err != nil {
		return nil, err
	}
	response := &pb.SendBackToRepairResponse{}
	var current string
	if s.displayTracker != nil {
		if entry := s.displayTracker.Get(req.Msg.GetId()); entry != nil {
			current = entry.HeadSHA
		}
	}
	switch {
	case !strings.EqualFold(current, pin):
		response.Reason = "head-moved"
	default:
		var stored string
		if s.evidence != nil {
			if proof, ok := s.evidence.Proof(req.Msg.GetId(), pin); ok {
				stored = proof.Digest
			}
		}
		switch {
		case !strings.EqualFold(stored, digest):
			response.Reason = "proof-moved"
		case s.repairLease != nil && s.repairLease.Active(req.Msg.GetId()):
			response.Reason = "repair-active"
		default:
			result, err := s.verifyVerbsFor(env).SendBack(runCtx, target, domain.VerifySendBack{Head: pin, Note: note})
			if err != nil {
				return nil, s.verifyBridgeError("send-back", target, err)
			}
			response.IsSentBack = result.SentBack
			response.Reason = result.Reason
			response.CommentUrl = result.CommentURL
			response.IsAutoRepairEnabled = result.AutoRepair
		}
	}
	s.logger.Info().Str("session", req.Msg.GetId()).Int("pr", target.PR).Bool("sent_back", response.IsSentBack).Str("reason", response.Reason).Bool("operator_left", ctx.Err() != nil).Msg("operator send-back finished")
	return connect.NewResponse(response), nil
}
