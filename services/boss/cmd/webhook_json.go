package main

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- JSON schemas ---------------------------------------------------------
//
// These are the stable `boss webhook … --json` contracts. Field names are part
// of the machine contract; renames are breaking. Timestamps are RFC3339 UTC,
// empty when unset; proto optional ints are nullable. No schema carries the
// signing secret except webhookSecretJSON, the add / rotate-secret envelope,
// which carries it in its own top-level "secret" key.

type webhookJSON struct {
	ID              string   `json:"id"`
	OrganizationID  string   `json:"organization_id"`
	URL             string   `json:"url"`
	Description     string   `json:"description"`
	EventTypes      []string `json:"event_types"`
	Enabled         bool     `json:"enabled"`
	CreatedByUserID string   `json:"created_by_user_id"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
	SecretRotatedAt string   `json:"secret_rotated_at"`
}

// webhookSecretJSON is the add / rotate-secret envelope: the only schema that
// carries the one-time signing secret.
type webhookSecretJSON struct {
	Webhook webhookJSON `json:"webhook"`
	Secret  string      `json:"secret"`
}

type webhookEventTypeJSON struct {
	Type              string `json:"type"`
	DisplayLabel      string `json:"display_label"`
	Description       string `json:"description"`
	SamplePayloadJSON string `json:"sample_payload_json"`
}

type webhookDeliveryJSON struct {
	ID                     string `json:"id"`
	WebhookID              string `json:"webhook_id"`
	EventID                string `json:"event_id"`
	EventType              string `json:"event_type"`
	IsTest                 bool   `json:"is_test"`
	Status                 string `json:"status"`
	AttemptCount           int32  `json:"attempt_count"`
	LastResponseStatusCode *int32 `json:"last_response_status_code"`
	LastError              string `json:"last_error"`
	NextAttemptAt          string `json:"next_attempt_at"`
	CreatedAt              string `json:"created_at"`
	CompletedAt            string `json:"completed_at"`
}

type webhookAttemptJSON struct {
	AttemptNumber       int32  `json:"attempt_number"`
	StartedAt           string `json:"started_at"`
	DurationMs          int64  `json:"duration_ms"`
	ResponseStatusCode  *int32 `json:"response_status_code"`
	ResponseBodyExcerpt string `json:"response_body_excerpt"`
	ErrorMessage        string `json:"error_message"`
	IsSuccess           bool   `json:"is_success"`
}

type webhookHeaderJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// webhookTestJSON is `boss webhook test --json`. attempt is null when the
// delivery was cancelled before an attempt was made.
type webhookTestJSON struct {
	Delivery webhookDeliveryJSON `json:"delivery"`
	Attempt  *webhookAttemptJSON `json:"attempt"`
}

type webhookDeliveriesJSON struct {
	Deliveries    []webhookDeliveryJSON `json:"deliveries"`
	NextPageToken string                `json:"next_page_token"`
}

type webhookDeliveryDetailJSON struct {
	Delivery       webhookDeliveryJSON  `json:"delivery"`
	Attempts       []webhookAttemptJSON `json:"attempts"`
	RequestBody    string               `json:"request_body"`
	RequestHeaders []webhookHeaderJSON  `json:"request_headers"`
}

// optionalInt32 copies a proto optional int so the JSON value is null, not 0,
// when the field is absent.
func optionalInt32(present bool, v int32) *int32 {
	if !present {
		return nil
	}
	return &v
}

// webhookToJSON maps a proto SessionWebhook field by field, so a new proto
// field (and above all a secret) cannot leak into the contract by default.
func webhookToJSON(w *pb.SessionWebhook) webhookJSON {
	return webhookJSON{
		ID:              w.GetId(),
		OrganizationID:  w.GetOrganizationId(),
		URL:             w.GetUrl(),
		Description:     w.GetDescription(),
		EventTypes:      nonNilStrings(w.GetEventTypes()),
		Enabled:         w.GetIsEnabled(),
		CreatedByUserID: w.GetCreatedByUserId(),
		CreatedAt:       rfc3339OrEmpty(w.GetCreatedAt()),
		UpdatedAt:       rfc3339OrEmpty(w.GetUpdatedAt()),
		SecretRotatedAt: rfc3339OrEmpty(w.GetSecretRotatedAt()),
	}
}

func webhookEventTypeToJSON(e *pb.SessionWebhookEventType) webhookEventTypeJSON {
	return webhookEventTypeJSON{
		Type:              e.GetType(),
		DisplayLabel:      e.GetDisplayLabel(),
		Description:       e.GetDescription(),
		SamplePayloadJSON: e.GetSamplePayloadJson(),
	}
}

func webhookDeliveryToJSON(d *pb.SessionWebhookDelivery) webhookDeliveryJSON {
	return webhookDeliveryJSON{
		ID:                     d.GetId(),
		WebhookID:              d.GetWebhookId(),
		EventID:                d.GetEventId(),
		EventType:              d.GetEventType(),
		IsTest:                 d.GetIsTest(),
		Status:                 d.GetStatus(),
		AttemptCount:           d.GetAttemptCount(),
		LastResponseStatusCode: optionalInt32(d.LastResponseStatusCode != nil, d.GetLastResponseStatusCode()),
		LastError:              d.GetLastError(),
		NextAttemptAt:          rfc3339OrEmpty(d.GetNextAttemptAt()),
		CreatedAt:              rfc3339OrEmpty(d.GetCreatedAt()),
		CompletedAt:            rfc3339OrEmpty(d.GetCompletedAt()),
	}
}

func webhookAttemptToJSON(a *pb.SessionWebhookDeliveryAttempt) webhookAttemptJSON {
	return webhookAttemptJSON{
		AttemptNumber:       a.GetAttemptNumber(),
		StartedAt:           rfc3339OrEmpty(a.GetStartedAt()),
		DurationMs:          a.GetDurationMs(),
		ResponseStatusCode:  optionalInt32(a.ResponseStatusCode != nil, a.GetResponseStatusCode()),
		ResponseBodyExcerpt: a.GetResponseBodyExcerpt(),
		ErrorMessage:        a.GetErrorMessage(),
		IsSuccess:           a.GetIsSuccess(),
	}
}

func webhookDeliveriesToJSON(resp *pb.ListSessionWebhookDeliveriesResponse) webhookDeliveriesJSON {
	out := webhookDeliveriesJSON{
		Deliveries:    make([]webhookDeliveryJSON, 0, len(resp.GetDeliveries())),
		NextPageToken: resp.GetNextPageToken(),
	}
	for _, d := range resp.GetDeliveries() {
		out.Deliveries = append(out.Deliveries, webhookDeliveryToJSON(d))
	}
	return out
}

func webhookDeliveryDetailToJSON(resp *pb.GetSessionWebhookDeliveryResponse) webhookDeliveryDetailJSON {
	out := webhookDeliveryDetailJSON{
		Delivery:       webhookDeliveryToJSON(resp.GetDelivery()),
		Attempts:       make([]webhookAttemptJSON, 0, len(resp.GetAttempts())),
		RequestBody:    resp.GetRequestBody(),
		RequestHeaders: make([]webhookHeaderJSON, 0, len(resp.GetRequestHeaders())),
	}
	for _, a := range resp.GetAttempts() {
		out.Attempts = append(out.Attempts, webhookAttemptToJSON(a))
	}
	for _, h := range resp.GetRequestHeaders() {
		out.RequestHeaders = append(out.RequestHeaders, webhookHeaderJSON{Name: h.GetName(), Value: h.GetValue()})
	}
	return out
}
