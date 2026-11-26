package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/repotrust"
)

// EvidenceReasonHeader names the machine-readable upload precondition response header.
const EvidenceReasonHeader = "Bossd-Evidence-Reason"

// EvidenceLimitHeader names the hosted page-shape limit an over-limit refusal
// hit, and EvidenceLimitMaxHeader carries bosso's maximum for it.
const (
	EvidenceLimitHeader    = "Bossd-Evidence-Limit"
	EvidenceLimitMaxHeader = "Bossd-Evidence-Limit-Max"
)

const evidenceUploadCeiling = 10 * time.Minute
const evidenceObjectSizeCap = 300 * 1024 * 1024

// evidenceObjectsPerCall caps one UploadEvidence call, and so one hosted page.
const evidenceObjectsPerCall = 64

// evidencePageName is the proof page object and evidencePlayerName the static
// script it links; both are accepted only under the deterministic prefix and
// on a destination with pages enabled.
const (
	evidencePageName   = "index.html"
	evidencePlayerName = "player.js"
)

var (
	evidenceScreenshotName = regexp.MustCompile(`^screenshots/[A-Za-z0-9._-]+\.(png|jpg|jpeg|webp)$`)
	// evidenceMediaName is a proof page's media, published flat beside it.
	evidenceMediaName = regexp.MustCompile(`^media/[A-Za-z0-9._-]+\.(mp4|png|jpg|webp|pdf)$`)
	evidenceHeadSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	evidenceRepoSlug  = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// UploadEvidence publishes only finished evidence belonging to a trusted registered workspace.
// Every refusal is logged at warn level with the repo, PR, head and publisher it was for.
func (s *Server) UploadEvidence(ctx context.Context, req *connect.Request[pb.UploadEvidenceRequest]) (*connect.Response[pb.UploadEvidenceResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, evidenceUploadCeiling)
	defer cancel()
	resp, repo, err := s.uploadEvidence(ctx, req.Msg)
	if err != nil {
		s.logEvidenceFailure(req.Msg, repo, err)
	}
	return resp, err
}

// logEvidenceFailure leaves the daemon's trace of a refused upload: the RPC
// code and message the caller sees, plus any cause that message withholds.
func (s *Server) logEvidenceFailure(msg *pb.UploadEvidenceRequest, repo *domain.Repo, err error) {
	event := s.logger.Warn().
		Str("publisher", msg.Destination.String()).
		Int32("pr_number", msg.PrNumber).
		Str("head_sha", msg.HeadSha).
		Str("evidence_dir", msg.EvidenceDir).
		Str("code", connect.CodeOf(err).String())
	if repo != nil {
		event = event.Str("repo_id", repo.ID).Str("repo", vcs.RepoSlug(repo.OriginURL))
	}
	var withheld *evidenceCause
	if errors.As(err, &withheld) && withheld.cause != nil {
		event = event.AnErr("cause", withheld.cause)
	}
	event.Err(err).Msg("verify evidence upload failed")
}

// evidenceCause is an RPC error whose message is fixed text, because the
// toolbox may publish it in a PR comment, while its cause stays for the log.
type evidenceCause struct {
	message string
	cause   error
}

func (e *evidenceCause) Error() string { return e.message }
func (e *evidenceCause) Unwrap() error { return e.cause }

// evidenceFailure is a connect error carrying message and the stage reason
// header on the wire, and cause only in the daemon log.
func evidenceFailure(code connect.Code, reason, message string, cause error) *connect.Error {
	rpcErr := connect.NewError(code, &evidenceCause{message: message, cause: cause})
	rpcErr.Meta().Set(EvidenceReasonHeader, reason)
	return rpcErr
}

func (s *Server) uploadEvidence(ctx context.Context, msg *pb.UploadEvidenceRequest) (*connect.Response[pb.UploadEvidenceResponse], *domain.Repo, error) {
	destination := msg.Destination
	if _, known := pb.EvidenceDestination_name[int32(destination)]; !known {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown evidence destination"))
	}
	deterministic, err := evidenceKeyed(msg.PrNumber, msg.HeadSha)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	dir, repo, err := s.evidenceDirectory(ctx, msg.EvidenceDir)
	if err != nil {
		return nil, repo, err
	}
	defer func() { _ = dir.Close() }()
	resp, err := s.publishEvidence(ctx, dir, repo, msg, deterministic)
	if err == nil {
		s.recordPublishedPage(ctx, repo, msg, resp.Msg)
	}
	return resp, repo, err
}

// publishEvidence validates the request's objects and sends them to its destination.
func (s *Server) publishEvidence(ctx context.Context, dir *os.Root, repo *domain.Repo, msg *pb.UploadEvidenceRequest, deterministic bool) (*connect.Response[pb.UploadEvidenceResponse], error) {
	destination := msg.Destination
	files, err := validateEvidenceObjects(dir, msg.Objects)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer func() {
		for _, f := range files {
			_ = f.file.Close()
		}
	}()
	page := slices.ContainsFunc(files, func(f evidenceUploadFile) bool {
		return f.name == evidencePageName || f.name == evidencePlayerName
	})
	if page && !deterministic {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("index.html and player.js require pr_number and head_sha"))
	}
	if destination == pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED {
		return s.publishHosted(ctx, repo, msg, files, deterministic)
	}
	prefix, err := evidencePrefix(repo, msg.PrNumber, msg.HeadSha, deterministic)
	if err != nil {
		return nil, err
	}
	var bucket domain.EvidenceBucket
	err = domain.ErrEvidenceUploadNotConfigured
	if s.evidenceBucketOpener != nil {
		bucket, err = s.evidenceBucketOpener.OpenEvidenceBucket()
	}
	if err != nil {
		if destination == pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL_OR_HOSTED && evidenceDestinationMissing(err) {
			return s.publishHosted(ctx, repo, msg, files, deterministic)
		}
		return nil, evidenceOpenError(err)
	}
	return s.publishLocal(ctx, bucket, prefix, files, page)
}

// evidenceDestinationMissing reports an opener error meaning the daemon's own
// proof_upload destination is absent rather than broken.
func evidenceDestinationMissing(err error) bool {
	return errors.Is(err, domain.ErrEvidenceDestinationNotConfigured) || errors.Is(err, domain.ErrEvidenceUploadNotConfigured)
}

// publishLocal writes every object to the daemon's own bucket under prefix.
func (s *Server) publishLocal(ctx context.Context, bucket domain.EvidenceBucket, prefix string, files []evidenceUploadFile, page bool) (*connect.Response[pb.UploadEvidenceResponse], error) {
	if page && !bucket.PagesEnabled() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("proof pages are not enabled for this destination"))
	}
	keys := make([]string, 0, len(files))
	for _, object := range files {
		key := prefix + object.name
		s.logger.Info().Str("key_prefix", prefix).Str("object", object.name).Int64("size_bytes", object.size).Msg("uploading verify evidence")
		if err := bucket.Put(ctx, key, object.contentType, object.file, object.size); err != nil {
			return nil, evidenceFailure(connect.CodeUnavailable, "object-upload-failed", "evidence object upload failed", err)
		}
		keys = append(keys, key)
	}
	return connect.NewResponse(&pb.UploadEvidenceResponse{KeyPrefix: prefix, PublicBaseUrl: bucket.PublicBaseURL(), ObjectKeys: keys, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL}), nil
}

// evidenceOpenError maps a destination failure to its RPC error without the
// opener's detail, which may name the destination.
func evidenceOpenError(err error) error {
	reason := ""
	switch {
	case errors.Is(err, domain.ErrEvidenceDestinationNotConfigured):
		reason = "not-configured"
	case errors.Is(err, domain.ErrEvidenceUploadNotConfigured):
		reason = "no-token"
	}
	if reason != "" {
		rpcErr := connect.NewError(connect.CodeFailedPrecondition, errors.New("evidence upload is not configured"))
		rpcErr.Meta().Set(EvidenceReasonHeader, reason)
		return rpcErr
	}
	return evidenceFailure(connect.CodeUnavailable, "destination-open-failed", "open evidence upload destination", err)
}

// evidenceKeyed reports whether the request asks for the deterministic
// <owner>/<repo>/<pr>/<sha>/ prefix. Both fields or neither are accepted.
func evidenceKeyed(pr int32, headSHA string) (bool, error) {
	switch {
	case pr == 0 && headSHA == "":
		return false, nil
	case pr == 0 || headSHA == "":
		return false, errors.New("pr_number and head_sha must be sent together")
	case pr < 1:
		return false, errors.New("pr_number must be at least 1")
	case !evidenceHeadSHA.MatchString(headSHA):
		return false, errors.New("head_sha must be 40 lowercase hex characters")
	}
	return true, nil
}

// evidencePrefix derives the object key prefix. The slug always comes from the
// trusted workspace owner's origin remote, never from the caller, so one repo
// cannot write under another's prefix in a shared bucket.
func evidencePrefix(repo *domain.Repo, pr int32, headSHA string, deterministic bool) (string, error) {
	if !deterministic {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", connect.NewError(connect.CodeInternal, errors.New("mint evidence prefix"))
		}
		return "verify-evidence/" + base64.RawURLEncoding.EncodeToString(nonce) + "/", nil
	}
	slug := ""
	if repo != nil {
		slug = vcs.RepoSlug(repo.OriginURL)
		// RepoSlug keeps only the first two path segments; a nested origin
		// (group/sub/repo) would share its prefix with a sibling, so refuse it.
		canonical := strings.TrimPrefix(vcs.NormalizeRepoURL(repo.OriginURL), "https://")
		if strings.Count(canonical, "/") != 2 || !strings.HasSuffix(canonical, "/"+slug) {
			slug = ""
		}
	}
	if !evidenceRepoSlug.MatchString(slug) || slices.ContainsFunc(strings.Split(slug, "/"), func(part string) bool { return part == "." || part == ".." }) {
		return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("workspace origin remote has no owner/repo slug"))
	}
	return fmt.Sprintf("%s/%d/%s/", slug, pr, headSHA), nil
}

func (s *Server) evidenceDirectory(ctx context.Context, path string) (*os.Root, *domain.Repo, error) {
	denied := func() (*os.Root, *domain.Repo, error) {
		return nil, nil, connect.NewError(connect.CodePermissionDenied, errors.New("evidence directory is outside a trusted workspace"))
	}
	if !filepath.IsAbs(path) {
		return denied()
	}
	dir, err := filepath.EvalSymlinks(path)
	if err != nil {
		return denied()
	}
	repo, sess, err := s.workspaceOwner(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if !repotrust.Allowed(repo) {
		return denied()
	}
	root := repo.LocalPath
	if sess != nil {
		root = sess.WorktreePath
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return denied()
	}
	relative, err := filepath.Rel(root, dir)
	if err != nil {
		return denied()
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != ".verify-evidence" || parts[1] == "." || parts[1] == ".." || parts[1] == "" {
		return nil, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("evidence directory must be <workspace>/.verify-evidence/<run>"))
	}
	workspace, err := os.OpenRoot(root)
	if err != nil {
		return denied()
	}
	defer func() { _ = workspace.Close() }()
	evidence, err := workspace.OpenRoot(relative)
	if err != nil {
		return denied()
	}
	return evidence, repo, nil
}

type evidenceUploadFile struct {
	name, contentType string
	file              *os.File
	size              int64
}

func validateEvidenceObjects(root *os.Root, objects []*pb.EvidenceObject) (files []evidenceUploadFile, err error) {
	// Open and validate all files before performing any PUT, keeping those descriptors for upload.
	defer func() {
		if err != nil {
			for _, f := range files {
				_ = f.file.Close()
			}
		}
	}()
	if len(objects) == 0 || len(objects) > evidenceObjectsPerCall {
		return nil, fmt.Errorf("evidence must contain 1 to %d objects", evidenceObjectsPerCall)
	}
	seen := map[string]bool{}
	for _, object := range objects {
		if object == nil {
			return files, errors.New("missing evidence object")
		}
		contentType := evidenceContentType(object.Name)
		if contentType == "" || seen[object.Name] {
			return files, errors.New("invalid or duplicate evidence object name")
		}
		seen[object.Name] = true
		if !filepath.IsLocal(object.Path) || filepath.Clean(object.Path) != object.Path || strings.Contains(object.Path, "\\") {
			return files, errors.New("invalid relative evidence object path")
		}
		for _, part := range strings.Split(object.Path, "/") {
			if part == ".." {
				return files, errors.New("parent segments are forbidden")
			}
		}
		// Root.Open enforces containment through symlinks atomically, including concurrent renames.
		f, openErr := root.OpenFile(object.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if openErr != nil {
			return files, errors.New("evidence object escapes directory or cannot be opened")
		}
		stat, statErr := f.Stat()
		if statErr != nil || !stat.Mode().IsRegular() || stat.Size() > evidenceObjectSizeCap {
			_ = f.Close()
			return files, fmt.Errorf("evidence object must be a regular file of at most %d bytes", evidenceObjectSizeCap)
		}
		files = append(files, evidenceUploadFile{name: object.Name, contentType: contentType, file: f, size: stat.Size()})
	}
	return files, nil
}

func evidenceContentType(name string) string {
	switch name {
	case "video.mp4":
		return "video/mp4"
	case "poster.png":
		return "image/png"
	case "manifest.json":
		return "application/json"
	case "request-log.ndjson":
		return "application/x-ndjson"
	case evidencePageName:
		return "text/html; charset=utf-8"
	case evidencePlayerName:
		return "text/javascript; charset=utf-8"
	}
	if !evidenceScreenshotName.MatchString(name) && !evidenceMediaName.MatchString(name) {
		return ""
	}
	switch filepath.Ext(name) {
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return ""
}
