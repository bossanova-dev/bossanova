package main

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/r2upload"
)

// hostedProofUpstream is the part of bosso's client the hosted proof host needs.
type hostedProofUpstream interface {
	StartProofUpload(context.Context, *connect.Request[pb.StartProofUploadRequest]) (*connect.Response[pb.StartProofUploadResponse], error)
	PutProofObject(context.Context, *connect.Request[pb.PutProofObjectRequest]) (*connect.Response[pb.PutProofObjectResponse], error)
	AbortProofUpload(context.Context, *connect.Request[pb.AbortProofUploadRequest]) (*connect.Response[pb.AbortProofUploadResponse], error)
}

// hostedProofClient publishes proof pages through bosso with the daemon's own
// session token. reviewOrigin is the review app that frames the page; empty
// means this Cloud environment has none, so hosted publishing stays off.
type hostedProofClient struct {
	rpc          hostedProofUpstream
	token        func() string
	reviewOrigin string
}

// hostedProofLimitKinds names each bosso limit as the door reports it.
var hostedProofLimitKinds = map[pb.ProofUploadLimit]string{
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_FILE_COUNT:           "file-count",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_FILE_BYTES:           "file-bytes",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_PAGE_BYTES:           "page-bytes",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_PAGES_PER_HOUR:   "org-pages-per-hour",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_PAGES_PER_DAY:    "org-pages-per-day",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_BYTES_PER_DAY:    "org-bytes-per-day",
	pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_GLOBAL_BYTES_PER_DAY: "global-bytes-per-day",
}

func (c *hostedProofClient) sessionToken() string {
	if c.token == nil {
		return ""
	}
	return c.token()
}

func (c *hostedProofClient) StartHostedProof(ctx context.Context, in server.HostedProofStart) (server.HostedProofUpload, error) {
	token := c.sessionToken()
	if token == "" || c.reviewOrigin == "" {
		return server.HostedProofUpload{}, server.ErrHostedProofUnavailable
	}
	manifest := make([]*pb.ProofManifestEntry, 0, len(in.Manifest))
	for _, entry := range in.Manifest {
		manifest = append(manifest, &pb.ProofManifestEntry{Name: entry.Name, SizeBytes: entry.Size})
	}
	req := connect.NewRequest(&pb.StartProofUploadRequest{RepoOriginUrl: in.RepoOriginURL, PrNumber: in.PR, HeadSha: in.HeadSHA, Manifest: manifest})
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := c.rpc.StartProofUpload(ctx, req)
	if err != nil {
		return server.HostedProofUpload{}, hostedProofLimit(err)
	}
	return server.HostedProofUpload{UploadID: resp.Msg.UploadId, ReviewOrigin: c.reviewOrigin}, nil
}

func (c *hostedProofClient) PutHostedProofObject(ctx context.Context, in server.HostedProofPut) error {
	token := c.sessionToken()
	if token == "" {
		return server.ErrHostedProofUnavailable
	}
	req := connect.NewRequest(&pb.PutProofObjectRequest{RepoOriginUrl: in.RepoOriginURL, UploadId: in.UploadID, Name: in.Name, Content: in.Body})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := c.rpc.PutProofObject(ctx, req)
	return err
}

func (c *hostedProofClient) AbortHostedProof(ctx context.Context, uploadID, repoOriginURL string) error {
	token := c.sessionToken()
	if token == "" {
		return server.ErrHostedProofUnavailable
	}
	req := connect.NewRequest(&pb.AbortProofUploadRequest{UploadId: uploadID, RepoOriginUrl: repoOriginURL})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := c.rpc.AbortProofUpload(ctx, req)
	return err
}

// hostedProofLimit turns a ResourceExhausted refusal into the door's limit
// error. A missing or unmapped detail is "unknown", which the door treats as
// a window limit. Any other error passes through unchanged.
func hostedProofLimit(err error) error {
	var rpcErr *connect.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code() != connect.CodeResourceExhausted {
		return err
	}
	limit := &server.HostedProofLimitError{Limit: "unknown"}
	for _, detail := range rpcErr.Details() {
		value, valueErr := detail.Value()
		exceeded, ok := value.(*pb.ProofUploadLimitExceeded)
		if valueErr != nil || !ok {
			continue
		}
		if kind, known := hostedProofLimitKinds[exceeded.Limit]; known {
			limit.Limit, limit.Max = kind, exceeded.MaxValue
		}
		break
	}
	return limit
}

// reviewAppOrigin maps the daemon's orchestrator to the review app that frames
// hosted proof pages. Local, isolated and custom orchestrators map to "", so
// hosted publishing stays off there. This duplicates readReviewBase in
// .claude/skills/verify/lib/pr-proof-page.mjs and the boss CLI's subscribe
// URL, across a module boundary.
func reviewAppOrigin(orchestratorURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(orchestratorURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	switch strings.ToLower(parsed.Scheme + "://" + parsed.Host) {
	case "https://orchestrator.bossanova.dev":
		return "https://app.bossanova.dev"
	case "https://orchestrator-staging.bossanova.dev":
		return "https://app-staging.bossanova.dev"
	}
	return ""
}

// evidenceDestinations is the daemon's evidence opener: the local proof_upload
// bucket, promoted from the embedded uploader, plus the hosted proof host.
type evidenceDestinations struct {
	*r2upload.Uploader
	hosted *hostedProofClient
}

func (d evidenceDestinations) StartHostedProof(ctx context.Context, in server.HostedProofStart) (server.HostedProofUpload, error) {
	if d.hosted == nil {
		return server.HostedProofUpload{}, server.ErrHostedProofUnavailable
	}
	return d.hosted.StartHostedProof(ctx, in)
}

func (d evidenceDestinations) PutHostedProofObject(ctx context.Context, in server.HostedProofPut) error {
	if d.hosted == nil {
		return server.ErrHostedProofUnavailable
	}
	return d.hosted.PutHostedProofObject(ctx, in)
}

func (d evidenceDestinations) AbortHostedProof(ctx context.Context, uploadID, repoOriginURL string) error {
	if d.hosted == nil {
		return server.ErrHostedProofUnavailable
	}
	return d.hosted.AbortHostedProof(ctx, uploadID, repoOriginURL)
}
