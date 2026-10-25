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
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/repotrust"
)

const evidenceUploadCeiling = 10 * time.Minute
const evidenceObjectSizeCap = 300 * 1024 * 1024

var evidenceScreenshotName = regexp.MustCompile(`^screenshots/[A-Za-z0-9._-]+\.(png|jpg|jpeg|webp)$`)

// UploadEvidence publishes only finished evidence belonging to a trusted registered workspace.
func (s *Server) UploadEvidence(ctx context.Context, req *connect.Request[pb.UploadEvidenceRequest]) (*connect.Response[pb.UploadEvidenceResponse], error) {
	ctx, cancel := context.WithTimeout(ctx, evidenceUploadCeiling)
	defer cancel()
	dir, err := s.evidenceDirectory(ctx, req.Msg.EvidenceDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	files, err := validateEvidenceObjects(dir, req.Msg.Objects)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer func() {
		for _, f := range files {
			_ = f.file.Close()
		}
	}()
	if s.evidenceBucketOpener == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("evidence upload is not configured"))
	}
	bucket, err := s.evidenceBucketOpener.OpenEvidenceBucket()
	if errors.Is(err, domain.ErrEvidenceUploadNotConfigured) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("evidence upload is not configured"))
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("open evidence upload destination"))
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("mint evidence prefix"))
	}
	prefix := "verify-evidence/" + base64.RawURLEncoding.EncodeToString(nonce) + "/"
	keys := make([]string, 0, len(files))
	for _, object := range files {
		key := prefix + object.name
		s.logger.Info().Str("key_prefix", prefix).Str("object", object.name).Int64("size_bytes", object.size).Msg("uploading verify evidence")
		if err := bucket.Put(ctx, key, object.contentType, object.file, object.size); err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("evidence object upload failed"))
		}
		keys = append(keys, key)
	}
	return connect.NewResponse(&pb.UploadEvidenceResponse{KeyPrefix: prefix, PublicBaseUrl: bucket.PublicBaseURL(), ObjectKeys: keys}), nil
}

func (s *Server) evidenceDirectory(ctx context.Context, path string) (*os.Root, error) {
	denied := func() (*os.Root, error) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("evidence directory is outside a trusted workspace"))
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
		return nil, err
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
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("evidence directory must be <workspace>/.verify-evidence/<run>"))
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
	return evidence, nil
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
	if len(objects) == 0 || len(objects) > 64 {
		return nil, errors.New("evidence must contain 1 to 64 objects")
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
	}
	if !evidenceScreenshotName.MatchString(name) {
		return ""
	}
	switch filepath.Ext(name) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return ""
}
