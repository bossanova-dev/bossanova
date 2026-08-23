package drip

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type trackingReadCloser struct {
	io.Reader
	read bool
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	b.read = true
	return b.Reader.Read(p)
}

func (*trackingReadCloser) Close() error { return nil }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLoopsDripSend(t *testing.T) {
	var gotAuth, gotIdempotencyKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/events/send" {
			t.Errorf("path = %s, want /v1/events/send", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotIdempotencyKey = r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	d := NewLoopsDrip("test-key").WithEndpoint(srv.URL)
	err := d.Send(context.Background(), Event{
		Email:          "trial@example.com",
		UserID:         "user-123",
		EventName:      "trial_started",
		IdempotencyKey: "trial-123",
		ContactProperties: map[string]any{
			"plan":            "trial",
			"email":           "wrong@example.com",
			"userId":          "wrong-user",
			"eventName":       "wrong_event",
			"mailingLists":    "wrong_lists",
			"eventProperties": "wrong_properties",
		},
		MailingLists:    map[string]bool{"onboarding": true},
		EventProperties: map[string]any{"source": "signup"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
	if gotIdempotencyKey != "trial-123" {
		t.Errorf("Idempotency-Key = %q, want trial-123", gotIdempotencyKey)
	}
	if gotBody["email"] != "trial@example.com" || gotBody["userId"] != "user-123" || gotBody["eventName"] != "trial_started" || gotBody["plan"] != "trial" {
		t.Errorf("body = %#v, want contact identifiers, event name, and contact property", gotBody)
	}
	if mailingLists, ok := gotBody["mailingLists"].(map[string]any); !ok || mailingLists["onboarding"] != true {
		t.Errorf("mailingLists = %#v, want onboarding membership", gotBody["mailingLists"])
	}
	if eventProperties, ok := gotBody["eventProperties"].(map[string]any); !ok || eventProperties["source"] != "signup" {
		t.Errorf("eventProperties = %#v, want signup source", gotBody["eventProperties"])
	}
}

func TestLoopsDripSendStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"duplicate"}`))
	}))
	t.Cleanup(srv.Close)

	err := NewLoopsDrip("key").WithEndpoint(srv.URL).Send(context.Background(), Event{Email: "trial@example.com", EventName: "trial_started"})
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error = %v, want StatusError", err)
	}
	if statusErr.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", statusErr.StatusCode, http.StatusConflict)
	}
	if !strings.Contains(statusErr.Error(), "409") {
		t.Errorf("error = %q, want status 409", statusErr)
	}
}

func TestLoopsDripDefaultTimeout(t *testing.T) {
	if got, want := NewLoopsDrip("key").client.Timeout, 10*time.Second; got != want {
		t.Errorf("client timeout = %v, want %v", got, want)
	}
}

func TestLoopsDripSendDrainsSuccessfulResponse(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("ok")}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	})}

	err := NewLoopsDrip("key").WithHTTPClient(client).Send(context.Background(), Event{Email: "trial@example.com", EventName: "trial_started"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !body.read {
		t.Fatal("successful response body was not drained")
	}
}

func TestLoopsDripSendRespectsCancelledContext(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(func() {
		close(releaseHandler)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		errs <- NewLoopsDrip("key").WithEndpoint(srv.URL).Send(ctx, Event{Email: "trial@example.com", EventName: "trial_started"})
	}()

	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not reach server")
	}
	cancel()

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("Send succeeded after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not return after context cancellation")
	}
}

func TestNoopDripSend(t *testing.T) {
	if err := NewNoopDrip().Send(context.Background(), Event{Email: "trial@example.com", EventName: "trial_started"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
