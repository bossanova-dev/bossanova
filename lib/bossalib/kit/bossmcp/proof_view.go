package bossmcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// proofViewLinkResult is get_proof_view_link's JSON result.
type proofViewLinkResult struct {
	ViewURL       string `json:"view_url"`
	ExpiresAt     string `json:"expires_at"`
	HeadSHA       string `json:"head_sha"`
	IsCurrentHead bool   `json:"is_current_head"`
}

// getProofViewLink mints a session's hosted proof view link. The session read
// comes first and is the authorization rule: a caller who cannot read the
// session gets its error and no mint is attempted.
func getProofViewLink(ctx context.Context, backend Backend, id string) (*mcp.CallToolResult, any, error) {
	s, err := backend.GetSession(ctx, id)
	if err != nil {
		return errorResult(err), nil, nil
	}
	if s.GetPrNumber() <= 0 {
		return errorResult(errors.New("session has no pull request")), nil, nil
	}
	minter, ok := backend.(ProofViewBackend)
	if !ok {
		return errorResult(errors.New("review page links are not supported by this backend")), nil, nil
	}
	head := s.GetPrProofHeadSha()
	resp, err := minter.MintProofViewLink(ctx, &pb.MintProofViewLinkRequest{RepoOriginUrl: s.GetRepoOriginUrl(), PrNumber: s.GetPrNumber(), HeadSha: head})
	if err != nil {
		return errorResult(err), nil, nil
	}
	out := proofViewLinkResult{
		ViewURL:       resp.GetViewUrl(),
		HeadSHA:       head,
		IsCurrentHead: head != "" && strings.EqualFold(head, s.GetPrDisplayHeadSha()),
	}
	if resp.GetExpiresAt() != nil {
		out.ExpiresAt = resp.GetExpiresAt().AsTime().UTC().Format(time.RFC3339)
	}
	r, err := jsonResult(out)
	return r, nil, err
}
