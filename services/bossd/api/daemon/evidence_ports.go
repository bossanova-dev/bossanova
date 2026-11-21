package server

import (
	"context"
	"errors"

	"github.com/recurser/bossd/internal/domain"
)

// evidenceBucketOpener reads the upload credential fresh for each publish operation.
type evidenceBucketOpener interface {
	OpenEvidenceBucket() (domain.EvidenceBucket, error)
}

// hostedProofPublisher is implemented by an evidence opener that can also reach
// bosso's hosted proof host. A plain local opener (r2upload) does not, so hosted
// publishing is simply unavailable.
type hostedProofPublisher interface {
	StartHostedProof(context.Context, HostedProofStart) (HostedProofUpload, error)
	PutHostedProofObject(context.Context, HostedProofPut) error
}

// HostedProofEntry declares one page object and its exact size.
type HostedProofEntry struct {
	Name string
	Size int64
}

// HostedProofStart opens a hosted page for one pull request head.
type HostedProofStart struct {
	RepoOriginURL, HeadSHA string
	PR                     int32
	Manifest               []HostedProofEntry
}

// HostedProofUpload identifies a started upload and the review app that frames it.
type HostedProofUpload struct {
	UploadID, ReviewOrigin string
}

// HostedProofPut carries one object of a started upload. The repo origin rides
// on every Put because bosso re-resolves the organization each time.
type HostedProofPut struct {
	UploadID, RepoOriginURL, Name string
	Body                          []byte
}

// ErrHostedProofUnavailable means this daemon cannot reach the hosted proof
// host: it is not signed in, or its Cloud environment has no review app.
var ErrHostedProofUnavailable = errors.New("hosted proof host is unavailable")

// HostedProofLimitError is bosso's page-shape or quota refusal. Limit is the
// kind (file-count, file-bytes, page-bytes, a window kind, or unknown) and Max
// is bosso's maximum for it.
type HostedProofLimitError struct {
	Limit string
	Max   int64
}

func (e *HostedProofLimitError) Error() string {
	return "hosted proof limit exceeded: " + e.Limit
}
