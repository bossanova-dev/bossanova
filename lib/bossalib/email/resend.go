package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultResendEndpoint is the Resend API endpoint for sending emails.
const DefaultResendEndpoint = "https://api.resend.com/emails"

// ResendMailer sends email via the Resend HTTP API.
type ResendMailer struct {
	apiKey   string
	from     string
	endpoint string
	client   *http.Client
}

// NewResendMailer returns a Mailer that POSTs to the Resend API.
// from must be a verified sender address for the configured Resend account.
func NewResendMailer(apiKey, from string) *ResendMailer {
	return &ResendMailer{
		apiKey:   apiKey,
		from:     from,
		endpoint: DefaultResendEndpoint,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// WithEndpoint overrides the Resend API endpoint (used in tests).
func (m *ResendMailer) WithEndpoint(endpoint string) *ResendMailer {
	m.endpoint = endpoint
	return m
}

// WithHTTPClient overrides the HTTP client (used in tests).
func (m *ResendMailer) WithHTTPClient(c *http.Client) *ResendMailer {
	m.client = c
	return m
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html"`
}

// Send POSTs the email to Resend. Returns an error if the API responds
// with a non-2xx status; the body of the error response is included.
func (m *ResendMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	payload := resendRequest{
		From:    m.from,
		To:      []string{to},
		Subject: subject,
		HTML:    htmlBody,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal resend payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return nil
}
