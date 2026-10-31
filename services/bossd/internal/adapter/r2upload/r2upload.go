// Package r2upload writes proof objects with credentials held by the daemon.
package r2upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/recurser/bossd/internal/domain"
)

// CredentialSource reads upload configuration fresh when a bucket is opened.
type CredentialSource interface {
	UploadTarget() (domain.EvidenceUploadTarget, bool)
}

// Uploader opens a credential-backed bucket for each evidence operation.
type Uploader struct {
	source  CredentialSource
	baseURL string
	client  *http.Client
}

// Option configures the HTTP transport used by an uploader.
type Option func(*Uploader)

// WithBaseURL overrides the Cloudflare API origin, primarily for hermetic tests.
func WithBaseURL(baseURL string) Option {
	return func(u *Uploader) { u.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient overrides the upload client.
func WithHTTPClient(client *http.Client) Option {
	return func(u *Uploader) {
		if client != nil {
			u.client = client
		}
	}
}

// New constructs a daemon uploader. Callers supply the operation deadline via Put.
func New(source CredentialSource, options ...Option) *Uploader {
	u := &Uploader{source: source, baseURL: "https://api.cloudflare.com/client/v4", client: http.DefaultClient}
	for _, option := range options {
		option(u)
	}
	return u
}

// OpenEvidenceBucket reads fresh credentials without exposing them to the door.
func (u *Uploader) OpenEvidenceBucket() (domain.EvidenceBucket, error) {
	if u.source == nil {
		return nil, domain.ErrEvidenceUploadNotConfigured
	}
	target, ok := u.source.UploadTarget()
	if !ok || target.Token == "" {
		return nil, domain.ErrEvidenceUploadNotConfigured
	}
	return &bucket{target: target, baseURL: u.baseURL, client: u.client}, nil
}

type bucket struct {
	target  domain.EvidenceUploadTarget
	baseURL string
	client  *http.Client
}

func (b *bucket) PublicBaseURL() string { return b.target.PublicBaseURL }

func (b *bucket) PagesEnabled() bool { return b.target.PagesEnabled }

func (b *bucket) Put(ctx context.Context, key, contentType string, body io.Reader, size int64) error {
	endpoint := b.baseURL + "/accounts/" + url.PathEscape(b.target.AccountID) + "/r2/buckets/" + url.PathEscape(b.target.Bucket) + "/objects/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return errors.New("R2 upload: invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+b.target.Token)
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	resp, err := b.client.Do(req)
	if err != nil {
		// Transport errors may include server-controlled URLs and reflected headers.
		// Keep them private; cancellation is still identifiable by errors.Is.
		if ctx.Err() != nil {
			return fmt.Errorf("R2 upload: %w", ctx.Err())
		}
		return errors.New("R2 upload: transport failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	const bodyLimit = 1024
	data, _ := io.ReadAll(io.LimitReader(resp.Body, int64(bodyLimit+len(b.target.Token))))
	message := strings.ReplaceAll(string(data), b.target.Token, strings.Repeat("*", len(b.target.Token)))
	// Read past the visible limit before redacting so a token crossing that limit
	// cannot leave its prefix in the diagnostic.
	if len(message) > bodyLimit {
		message = message[:bodyLimit]
	}
	return fmt.Errorf("R2 upload: HTTP %d: %s", resp.StatusCode, message)
}
