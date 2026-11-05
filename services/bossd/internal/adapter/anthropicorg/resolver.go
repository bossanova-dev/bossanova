// Package anthropicorg reads the Anthropic organization id a Claude credential
// belongs to. The id is identity input only: callers digest it and never log or
// persist it.
package anthropicorg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	// countTokensPath is free to call. The billed /v1/messages is never used.
	countTokensPath = "/v1/messages/count_tokens"
	defaultTimeout  = 5 * time.Second
	maxBodyBytes    = 1 << 20
	oauthBetaHeader = "oauth-2025-04-20"
	apiVersion      = "2023-06-01"
	userAgent       = "claude-code/bossanova-1"
	// organizationHeader is sent on every authenticated response, including the
	// 403 from an organization that refuses OAuth and a 400 for a retired model.
	organizationHeader = "Anthropic-Organization-Id"
	// probeModel duplicates claudeProbeModel in plugins/bossd-plugin-claude/probe.go.
	// Plugins and the host share no packages. A retired id still returns the
	// header on its 400, so drift degrades nothing while the header is sent.
	probeModel = "claude-haiku-4-5-20251001"
)

// ErrUnauthorized reports that Anthropic rejected the credential with a 401.
var ErrUnauthorized = errors.New("anthropic rejected the credential")

// ErrNoOrganization reports a response that carried no organization header.
var ErrNoOrganization = errors.New("anthropic response carried no organization id")

// Resolver asks Anthropic which organization a Claude credential belongs to.
type Resolver struct {
	baseURL string
	client  *http.Client
	logger  zerolog.Logger
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithBaseURL overrides the Anthropic API origin, for tests.
func WithBaseURL(url string) Option {
	return func(r *Resolver) { r.baseURL = strings.TrimRight(url, "/") }
}

// WithTimeout overrides the 5s per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(r *Resolver) { r.client.Timeout = d }
}

// WithLogger records lookup outcomes. Log lines carry the HTTP status only,
// never the credential, the organization id or the response body.
func WithLogger(logger zerolog.Logger) Option {
	return func(r *Resolver) { r.logger = logger }
}

// New returns a Resolver for the public Anthropic API.
func New(opts ...Option) *Resolver {
	r := &Resolver{baseURL: defaultBaseURL, client: &http.Client{Timeout: defaultTimeout}, logger: zerolog.Nop()}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// OrganizationID returns the organization the bearer token belongs to. It reads
// the organization header from any status except 401, so an organization that
// refuses OAuth still identifies itself. A 401, a missing header, a transport
// error or a timeout returns an error and no id.
func (r *Resolver) OrganizationID(ctx context.Context, token string) (string, error) {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, probeModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+countTokensPath, bytes.NewBufferString(body))
	if err != nil {
		return "", fmt.Errorf("build organization request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", oauthBetaHeader)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		r.logger.Debug().Msg("anthropic organization lookup: request failed")
		return "", fmt.Errorf("anthropic organization lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode == http.StatusUnauthorized {
		r.logger.Debug().Int("status", resp.StatusCode).Msg("anthropic organization lookup: unauthorized")
		return "", ErrUnauthorized
	}
	org := strings.TrimSpace(resp.Header.Get(organizationHeader))
	if org == "" {
		r.logger.Debug().Int("status", resp.StatusCode).Msg("anthropic organization lookup: no organization header")
		return "", ErrNoOrganization
	}
	r.logger.Debug().Int("status", resp.StatusCode).Msg("anthropic organization lookup: resolved")
	return org, nil
}
