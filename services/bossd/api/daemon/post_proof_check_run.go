package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// ProofCheckReasonHeader identifies a proof check refusal for local callers.
const ProofCheckReasonHeader = "Bossd-Proof-Check-Reason"

// ProofCheckRun is a proof check destined for a registered repository.
type ProofCheckRun struct {
	RepoOriginURL, HeadSHA, Title, Summary, DetailsURL string
}

// ProofCheckRunResult identifies the written check run.
type ProofCheckRunResult struct {
	HTMLURL   string
	IsCreated bool
}

// ProofCheckRuns publishes checks through the daemon's upstream credentials.
type ProofCheckRuns interface {
	UpsertProofCheckRun(context.Context, string, ProofCheckRun) (ProofCheckRunResult, error)
}

// PostProofCheckRun mirrors local proof using only daemon-held credentials.
func (s *Server) PostProofCheckRun(ctx context.Context, req *connect.Request[pb.PostProofCheckRunRequest]) (*connect.Response[pb.PostProofCheckRunResponse], error) {
	r := req.Msg
	if !evidenceRepoSlug.MatchString(r.Repo) || !evidenceHeadSHA.MatchString(r.HeadSha) {
		return nil, proofCheckError(connect.CodeInvalidArgument, "invalid-argument")
	}
	origin := "https://github.com/" + r.Repo
	if err := s.proofCheckRegistered(ctx, origin); err != nil {
		return nil, err
	}
	token := ""
	if s.proofCheckRunSessionToken != nil {
		token = s.proofCheckRunSessionToken()
	}
	if token == "" || s.authStateReporter == nil || s.authStateReporter.AuthState(ctx).NeedsLogin {
		return nil, proofCheckError(connect.CodeFailedPrecondition, "not-signed-in")
	}
	if s.proofCheckRuns == nil {
		return nil, proofCheckError(connect.CodeUnavailable, "upload-failed")
	}
	out, err := s.proofCheckRuns.UpsertProofCheckRun(ctx, token, ProofCheckRun{RepoOriginURL: origin, HeadSHA: r.HeadSha, Title: r.Title, Summary: r.Summary, DetailsURL: r.DetailsUrl})
	if err != nil {
		var rpcErr *connect.Error
		switch {
		case connect.CodeOf(err) == connect.CodePermissionDenied:
			return nil, proofCheckError(connect.CodePermissionDenied, "no-installation")
		case errors.As(err, &rpcErr) && rpcErr.Code() == connect.CodeFailedPrecondition && rpcErr.Message() == "checks write not granted":
			return nil, proofCheckError(connect.CodeFailedPrecondition, "checks-not-granted")
		default:
			return nil, proofCheckError(connect.CodeUnavailable, "upload-failed")
		}
	}
	return connect.NewResponse(&pb.PostProofCheckRunResponse{HtmlUrl: out.HTMLURL, IsCreated: out.IsCreated}), nil
}

func (s *Server) proofCheckRegistered(ctx context.Context, origin string) error {
	if s.repos == nil {
		return proofCheckError(connect.CodeFailedPrecondition, "repo-not-registered")
	}
	repos, err := s.repos.List(ctx)
	if err != nil {
		return proofCheckError(connect.CodeUnavailable, "upload-failed")
	}
	for _, repo := range repos {
		if repo != nil && vcs.NormalizeRepoURL(repo.OriginURL) == origin {
			return nil
		}
	}
	return proofCheckError(connect.CodeFailedPrecondition, "repo-not-registered")
}

func proofCheckError(code connect.Code, reason string) error {
	err := connect.NewError(code, errors.New("proof check run refused: "+reason))
	err.Meta().Set(ProofCheckReasonHeader, reason)
	return err
}
