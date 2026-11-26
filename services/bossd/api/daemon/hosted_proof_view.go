package server

import (
	"context"
	"errors"
	"regexp"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrHostedProofViewSignedOut is what a HostedProofViews adapter returns when
// it holds no signed-in user's Bossanova Cloud token to mint with.
var ErrHostedProofViewSignedOut = errors.New("hosted proof view: not signed in")

// hostedProofViewSignIn is the refusal a signed-out or local-only daemon gives.
const hostedProofViewSignIn = "minting a hosted proof link needs Bossanova Cloud: sign in with boss login"

var hostedProofViewHead = regexp.MustCompile(`^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)

// HostedProofViewTarget names one pull request's hosted proof page.
type HostedProofViewTarget struct {
	RepoOriginURL string
	PRNumber      int32
	HeadSHA       string
}

// HostedProofViewLink is a short-lived signed link to a hosted proof page.
type HostedProofViewLink struct {
	ViewURL   string
	ExpiresAt time.Time
}

// HostedProofViews mints hosted proof view links as the signed-in user. The
// adapter owns the user-token lookup and returns ErrHostedProofViewSignedOut
// when it has none.
type HostedProofViews interface {
	MintProofViewLink(ctx context.Context, in HostedProofViewTarget) (HostedProofViewLink, error)
}

// MintHostedProofViewLink relays MintProofViewLink to the orchestrator with
// the signed-in user's token, so a local MCP caller can open a hosted proof
// page. Orchestrator errors pass through with their code and message.
func (s *Server) MintHostedProofViewLink(ctx context.Context, req *connect.Request[pb.MintHostedProofViewLinkRequest]) (*connect.Response[pb.MintHostedProofViewLinkResponse], error) {
	r := req.Msg
	origin := vcs.CanonicalRepoURL(r.GetRepoOriginUrl())
	switch {
	case origin == "":
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("repo_origin_url must identify a repository"))
	case r.GetPrNumber() <= 0:
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("pr_number must be positive"))
	case r.GetHeadSha() != "" && !hostedProofViewHead.MatchString(r.GetHeadSha()):
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("head_sha must be empty or 40 or 64 hex characters"))
	}
	if s.hostedProofViews == nil || (s.authStateReporter != nil && s.authStateReporter.AuthState(ctx).NeedsLogin) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(hostedProofViewSignIn))
	}
	link, err := s.hostedProofViews.MintProofViewLink(ctx, HostedProofViewTarget{RepoOriginURL: origin, PRNumber: r.GetPrNumber(), HeadSHA: r.GetHeadSha()})
	if errors.Is(err, ErrHostedProofViewSignedOut) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(hostedProofViewSignIn))
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.MintHostedProofViewLinkResponse{ViewUrl: link.ViewURL, ExpiresAt: timestamppb.New(link.ExpiresAt)}), nil
}
