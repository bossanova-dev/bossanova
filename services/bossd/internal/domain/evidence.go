package domain

import (
	"context"
	"errors"
	"io"
)

// ErrEvidenceUploadNotConfigured means no daemon upload credential is provisioned.
var ErrEvidenceUploadNotConfigured = errors.New("evidence upload not configured")

// EvidenceUploadTarget is daemon-only upload configuration. Token must never be logged.
type EvidenceUploadTarget struct {
	AccountID     string
	Bucket        string
	PublicBaseURL string
	Token         string
}

// EvidenceBucket provides object writes without exposing its credential to callers.
type EvidenceBucket interface {
	PublicBaseURL() string
	Put(ctx context.Context, key, contentType string, body io.Reader, size int64) error
}
