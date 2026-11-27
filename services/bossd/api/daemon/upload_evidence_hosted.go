package server

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/proofseal"
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
	var key []byte
	if s.sealHostedProof {
		if key, err = proofseal.NewPageKey(); err != nil {
			return nil, evidenceFailure(connect.CodeInternal, "seal-failed", "hosted proof page key could not be drawn", err)
		}
	}
	objects := hostedObjects(files, key)
	manifest := make([]HostedProofEntry, 0, len(objects))
	names := make([]string, 0, len(objects))
	for _, o := range objects {
		manifest = append(manifest, HostedProofEntry{Name: o.name, Size: o.size})
		names = append(names, o.name)
	}
	upload, err := publisher.StartHostedProof(ctx, HostedProofStart{RepoOriginURL: repo.OriginURL, HeadSHA: msg.HeadSha, PR: msg.PrNumber, Manifest: manifest})
	if err != nil {
		return nil, hostedStartError(err)
	}
	// A failed Put leaves an incomplete upload behind. No view link is ever
	// minted for an incomplete upload; the daemon aborts it so its reservation
	// is released, and bosso releases any it misses after a grace.
	for _, o := range hostedPutOrder(objects) {
		body, putErr := o.body()
		if putErr == nil {
			s.logger.Info().Str("upload_id", upload.UploadID).Str("object", o.name).Int64("size_bytes", o.size).Msg("uploading hosted verify evidence")
			if err := publisher.PutHostedProofObject(ctx, HostedProofPut{UploadID: upload.UploadID, RepoOriginURL: repo.OriginURL, Name: o.name, Body: body}); errors.Is(err, ErrHostedProofServerNotConfigured) {
				putErr = hostedServerNotConfigured(err)
			} else if err != nil {
				putErr = evidenceFailure(connect.CodeUnavailable, "object-upload-failed", "evidence object upload failed", err)
			}
		}
		if putErr != nil {
			s.abortHosted(ctx, publisher, upload.UploadID, repo.OriginURL)
			return nil, putErr
		}
	}
	slug := strings.SplitN(prefix, "/", 3)
	pageURL := fmt.Sprintf("%s/review/%s/%s/%d?head=%s", upload.ReviewOrigin, url.PathEscape(slug[0]), url.PathEscape(slug[1]), msg.PrNumber, msg.HeadSha)
	if key != nil {
		pageURL += "#k=" + proofseal.FragmentKey(key)
	}
	return connect.NewResponse(&pb.UploadEvidenceResponse{ObjectKeys: names, PageUrl: pageURL, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED, HostedLimits: hostedProofLimitsProto(upload.Limits)}), nil
}

// hostedAbortTimeout bounds the best-effort abort after a failed publish.
const hostedAbortTimeout = 10 * time.Second

// abortHosted releases an upload that will not complete. It runs even when
// the publish's own context has ended, and a failure is only logged: the
// caller returns the error that ended the publish, and bosso releases a
// missed upload after its grace.
func (s *Server) abortHosted(ctx context.Context, publisher hostedProofPublisher, uploadID, repoOriginURL string) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostedAbortTimeout)
	defer cancel()
	if err := publisher.AbortHostedProof(abortCtx, uploadID, repoOriginURL); err != nil {
		s.logger.Warn().Err(err).Str("upload_id", uploadID).Msg("abort hosted verify evidence upload failed; bosso releases it after its grace")
		return
	}
	s.logger.Info().Str("upload_id", uploadID).Msg("aborted hosted verify evidence upload")
}

// hostedPutOrder keeps request order but moves index.html last, so a served
// page never links a file that is not written yet.
func hostedPutOrder(objects []hostedObject) []hostedObject {
	ordered := slices.Clone(objects)
	slices.SortStableFunc(ordered, func(a, b hostedObject) int {
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
	case errors.Is(err, ErrHostedProofServerNotConfigured):
		return hostedServerNotConfigured(err)
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

// hostedServerNotConfigured is bosso's answer that it has no proof page
// storage. Unlike the daemon's own not-configured, an operator must act, so it
// is Unavailable with its own stage and a message the toolbox can explain.
func hostedServerNotConfigured(cause error) error {
	return evidenceFailure(connect.CodeUnavailable, "server-not-configured", ErrHostedProofServerNotConfigured.Error(), cause)
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
