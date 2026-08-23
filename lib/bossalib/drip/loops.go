package drip

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultLoopsEndpoint is the Loops API base URL.
const DefaultLoopsEndpoint = "https://app.loops.so/api"

// LoopsDrip sends lifecycle events through the Loops API.
type LoopsDrip struct {
	apiKey   string
	endpoint string
	client   *http.Client
}

// NewLoopsDrip returns a Drip that sends events through Loops.
func NewLoopsDrip(apiKey string) *LoopsDrip {
	return &LoopsDrip{
		apiKey:   apiKey,
		endpoint: DefaultLoopsEndpoint,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// WithEndpoint overrides the Loops API base URL. It is intended for tests and
// must be called before the client is used concurrently.
func (d *LoopsDrip) WithEndpoint(endpoint string) *LoopsDrip {
	d.endpoint = endpoint
	return d
}

// WithHTTPClient overrides the HTTP client. It is intended for tests and must
// be called before the client is used concurrently.
func (d *LoopsDrip) WithHTTPClient(client *http.Client) *LoopsDrip {
	d.client = client
	return d
}

// StatusError reports a non-successful Loops API response.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("loops: status %d", e.StatusCode)
	}
	return fmt.Sprintf("loops: status %d: %s", e.StatusCode, e.Body)
}

// Send POSTs an event to Loops. A supplied idempotency key is forwarded as the
// Idempotency-Key header.
func (d *LoopsDrip) Send(ctx context.Context, event Event) error {
	payload := make(map[string]any, len(event.ContactProperties)+5)
	for key, value := range event.ContactProperties {
		payload[key] = value
	}
	payload["eventName"] = event.EventName
	if event.Email != "" {
		payload["email"] = event.Email
	}
	if event.UserID != "" {
		payload["userId"] = event.UserID
	}
	if len(event.MailingLists) > 0 {
		payload["mailingLists"] = event.MailingLists
	}
	if len(event.EventProperties) > 0 {
		payload["eventProperties"] = event.EventProperties
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal loops payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.endpoint, "/")+"/v1/events/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build loops request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	if event.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", event.IdempotencyKey)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("loops request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("read loops error response (status %d): %w", resp.StatusCode, err)
	}
	return &StatusError{StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(responseBody))}
}
