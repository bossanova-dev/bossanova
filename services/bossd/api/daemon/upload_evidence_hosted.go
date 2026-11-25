package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// publishHosted uploads a whole proof page to bosso's hosted proof host. The
// local bucket is never opened here, and bucket keys stay inside bosso.
func (s *Server) publishHosted(ctx context.Context, repo *domain.Repo, msg *pb.UploadEvidenceRequest, files []evidenceUploadFile, deterministic bool) (*connect.Response[pb.UploadEvidenceResponse], error) {
	if !deterministic {
		// A random-prefix evidence upload has no hosted home.
		return nil, evidenceReasonError(connect.CodeFailedPrecondition, "not-configured", "hosted proof needs pr_number and head_sha")
	}
	if !slices.ContainsFunc(files, func(f evidenceUploadFile) bool { return f.name == evidencePageName }) {
		// StartProofUpload needs the whole manifest before any byte, so the
		// page and everything it links must arrive in one call.
		return nil, hostedLimitError("file-count", evidenceObjectsPerCall)
	}
	prefix, err := evidencePrefix(repo, msg.PrNumber, msg.HeadSha, true)
	if err != nil {
		return nil, err
	}
	publisher, ok := s.evidenceBucketOpener.(hostedProofPublisher)
	if !ok {
		return nil, hostedNotConfigured()
	}
	manifest := make([]HostedProofEntry, 0, len(files))
	names := make([]string, 0, len(files))
	for _, f := range files {
		manifest = append(manifest, HostedProofEntry{Name: f.name, Size: f.size})
		names = append(names, f.name)
	}
	upload, err := publisher.StartHostedProof(ctx, HostedProofStart{RepoOriginURL: repo.OriginURL, HeadSHA: msg.HeadSha, PR: msg.PrNumber, Manifest: manifest})
	if err != nil {
		return nil, hostedStartError(err)
	}
	// A failed Put leaves an incomplete upload behind. No view link is ever
	// minted for an incomplete upload, and bosso's retention backstop removes it.
	for _, f := range hostedPutOrder(files) {
		body, readErr := io.ReadAll(io.LimitReader(f.file, f.size+1))
		if readErr != nil || int64(len(body)) != f.size {
			return nil, evidenceFailure(connect.CodeUnavailable, "object-changed", "evidence object changed while uploading", readErr)
		}
		s.logger.Info().Str("upload_id", upload.UploadID).Str("object", f.name).Int64("size_bytes", f.size).Msg("uploading hosted verify evidence")
		if err := publisher.PutHostedProofObject(ctx, HostedProofPut{UploadID: upload.UploadID, RepoOriginURL: repo.OriginURL, Name: f.name, Body: body}); err != nil {
			return nil, evidenceFailure(connect.CodeUnavailable, "object-upload-failed", "evidence object upload failed", err)
		}
	}
	slug := strings.SplitN(prefix, "/", 3)
	pageURL := fmt.Sprintf("%s/review/%s/%s/%d?head=%s", upload.ReviewOrigin, url.PathEscape(slug[0]), url.PathEscape(slug[1]), msg.PrNumber, msg.HeadSha)
	return connect.NewResponse(&pb.UploadEvidenceResponse{ObjectKeys: names, PageUrl: pageURL, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED}), nil
}

// hostedPutOrder keeps request order but moves index.html last, so a served
// page never links a file that is not written yet.
func hostedPutOrder(files []evidenceUploadFile) []evidenceUploadFile {
	ordered := slices.Clone(files)
	slices.SortStableFunc(ordered, func(a, b evidenceUploadFile) int {
		switch {
		case a.name == evidencePageName && b.name != evidencePageName:
			return 1
		case b.name == evidencePageName && a.name != evidencePageName:
			return -1
		}
		return 0
	})
	return ordered
}

// hostedStartError maps a StartHostedProof failure to the refusal the caller
// can act on, without bosso's detail.
func hostedStartError(err error) error {
	var limit *HostedProofLimitError
	switch {
	case errors.As(err, &limit):
		switch limit.Limit {
		case "file-count", "file-bytes", "page-bytes":
			return hostedLimitError(limit.Limit, limit.Max)
		}
		// Window limits hide their numbers.
		return evidenceReasonError(connect.CodeFailedPrecondition, "quota-exhausted", "hosted proof quota is exhausted")
	case connect.CodeOf(err) == connect.CodeInvalidArgument:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("hosted proof manifest was refused"))
	}
	// Not signed in, not paying, not opted in, offline or no review origin.
	return evidenceFailure(connect.CodeFailedPrecondition, "not-configured", "hosted proof is not available", err)
}

func hostedNotConfigured() error {
	return evidenceReasonError(connect.CodeFailedPrecondition, "not-configured", "hosted proof is not available")
}

func hostedLimitError(kind string, maxValue int64) error {
	rpcErr := evidenceReasonError(connect.CodeResourceExhausted, "over-limit", "hosted proof page is over a limit")
	rpcErr.Meta().Set(EvidenceLimitHeader, kind)
	rpcErr.Meta().Set(EvidenceLimitMaxHeader, strconv.FormatInt(maxValue, 10))
	return rpcErr
}

func evidenceReasonError(code connect.Code, reason, message string) *connect.Error {
	rpcErr := connect.NewError(code, errors.New(message))
	rpcErr.Meta().Set(EvidenceReasonHeader, reason)
	return rpcErr
}
