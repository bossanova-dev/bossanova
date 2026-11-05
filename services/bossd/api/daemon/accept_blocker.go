package server

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
)

func validOperatorText(text string) bool {
	return utf8.ValidString(text) && strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= 2000 && !strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' })
}

func (s *Server) displayedPRHead(id string) string {
	if s.displayTracker != nil {
		if entry := s.displayTracker.Get(id); entry != nil {
			return entry.HeadSHA
		}
	}
	return ""
}

// AcceptSessionPRBlocker records a head-pinned operator acceptance. A confirmed
// request finishes on a bounded detached context even when the operator leaves.
func (s *Server) AcceptSessionPRBlocker(ctx context.Context, req *connect.Request[pb.AcceptSessionPRBlockerRequest]) (*connect.Response[pb.AcceptSessionPRBlockerResponse], error) {
	pin, ok := vcs.NormalizeHeadSHA(req.Msg.GetExpectedHeadSha())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("expected_head_sha must be a 40-character hex commit SHA"))
	}
	blocker := req.Msg.GetBlockerId()
	if !utf8.ValidString(blocker) || strings.TrimSpace(blocker) == "" || utf8.RuneCountInString(blocker) > 256 || strings.ContainsFunc(blocker, unicode.IsControl) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("blocker_id must contain 1 to 256 code points and no control characters"))
	}
	if !validOperatorText(req.Msg.GetReason()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reason must contain 1 to 2000 code points and no control characters except newline and tab"))
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), domain.VerifyVerbTimeout)
	defer cancel()
	target, env, err := s.verifyTarget(runCtx, req.Msg.GetId(), req.Msg.GetIsDryRun())
	if err != nil {
		return nil, err
	}
	result := domain.VerifyAcceptResult{}
	switch {
	case !strings.EqualFold(s.displayedPRHead(req.Msg.GetId()), pin):
		result.Reason = "head-moved"
	case s.repairLease != nil && s.repairLease.Active(req.Msg.GetId()):
		result.Reason = "repair-active"
	default:
		result, err = s.verifyVerbsFor(env).Accept(runCtx, target, domain.VerifyAccept{Head: pin, Blocker: blocker, Reason: req.Msg.GetReason()})
		if err != nil {
			return nil, s.verifyBridgeError("accept", target, err)
		}
	}
	s.logger.Info().Str("session", req.Msg.GetId()).Int("pr", target.PR).Bool("accepted", result.Accepted).Str("reason", result.Reason).Bool("operator_left", ctx.Err() != nil).Msg("operator accept finished")
	return connect.NewResponse(acceptResultProto(result)), nil
}
func acceptResultProto(r domain.VerifyAcceptResult) *pb.AcceptSessionPRBlockerResponse {
	return &pb.AcceptSessionPRBlockerResponse{IsAccepted: r.Accepted, IsAlreadyAccepted: r.Already, IsDryRun: r.DryRun, Reason: r.Reason, Detail: r.Detail, HeadSha: r.Head, BlockerId: r.Blocker, TicketIdentifier: r.TicketIdentifier, TicketUrl: r.TicketURL, CommentUrl: r.CommentURL, IsExistingTicket: r.ExistingTicket, PlannedWrites: r.Writes}
}

// ListSessionPRBlockers reads the findings an operator may accept at the displayed head.
func (s *Server) ListSessionPRBlockers(ctx context.Context, req *connect.Request[pb.ListSessionPRBlockersRequest]) (*connect.Response[pb.ListSessionPRBlockersResponse], error) {
	runCtx, cancel := context.WithTimeout(ctx, domain.VerifyVerbTimeout)
	defer cancel()
	target, env, err := s.verifyTarget(runCtx, req.Msg.GetId(), false)
	if err != nil {
		return nil, err
	}
	head := s.displayedPRHead(req.Msg.GetId())
	response := &pb.ListSessionPRBlockersResponse{HeadSha: head}
	if head == "" {
		response.Reason = "head-unknown"
		return connect.NewResponse(response), nil
	}
	readAt := s.attentionCache.blockers.clock()
	result, err := s.verifyVerbsFor(env).Blockers(runCtx, target, head)
	if err != nil {
		return nil, s.verifyBridgeError("blockers", target, err)
	}
	// The same read the session list serves its blocker tallies from.
	s.recordBlockers(req.Msg.GetId(), head, readAt, result)
	switch result.Outcome {
	case "absent":
		response.Reason = "no-blockers"
	case "unreadable":
		response.Reason = "comments-unreadable"
	case "written":
		for _, b := range result.Blockers {
			if b.Kind == "review-finding" {
				response.Blockers = append(response.Blockers, &pb.VerifyBlocker{Id: b.ID, Severity: b.Severity, FilePath: b.File, LineNumber: int32(b.Line), Summary: b.Summary, Text: b.Text})
			}
		}
		if len(response.Blockers) == 0 {
			response.Reason = "no-acceptable-blockers"
		}
	default:
		return nil, s.verifyBridgeError("blockers", target, domain.ErrVerifyShape)
	}
	return connect.NewResponse(response), nil
}
