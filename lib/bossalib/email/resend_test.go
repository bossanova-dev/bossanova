package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResendMailer_Send(t *testing.T) {
	var gotAuth, gotContentType string
	var gotBody resendRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resend-id-1"}`))
	}))
	t.Cleanup(srv.Close)

	m := NewResendMailer("test-api-key", "reports@example.com").WithEndpoint(srv.URL)
	err := m.Send(context.Background(), "triage@example.com", "subject line", "<p>hi</p>")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAuth != "Bearer test-api-key" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer test-api-key")
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}
	if gotBody.From != "reports@example.com" {
		t.Errorf("from = %q, want %q", gotBody.From, "reports@example.com")
	}
	if len(gotBody.To) != 1 || gotBody.To[0] != "triage@example.com" {
		t.Errorf("to = %v, want [triage@example.com]", gotBody.To)
	}
	if gotBody.Subject != "subject line" {
		t.Errorf("subject = %q", gotBody.Subject)
	}
	if gotBody.HTML != "<p>hi</p>" {
		t.Errorf("html = %q", gotBody.HTML)
	}
}

func TestResendMailer_Send_Non2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Invalid API key"}`))
	}))
	t.Cleanup(srv.Close)

	m := NewResendMailer("bad", "reports@example.com").WithEndpoint(srv.URL)
	err := m.Send(context.Background(), "triage@example.com", "s", "<p>x</p>")
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") || !strings.Contains(msg, "Invalid API key") {
		t.Errorf("error = %q, want 401 + body", msg)
	}
}

func TestResendMailer_Send_RespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	m := NewResendMailer("k", "reports@example.com").WithEndpoint(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if err := m.Send(ctx, "to@example.com", "s", "b"); err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

func TestNoopMailer_Send(t *testing.T) {
	m := NewNoopMailer()
	if err := m.Send(context.Background(), "to@example.com", "s", "b"); err != nil {
		t.Errorf("noop Send returned error: %v", err)
	}
}

// TestResendMailer_StatusCodeBoundaries verifies the 2xx success boundary at
// 200 and 300 (catches mutation: status < 200 or status >= 300 boundary).
func TestResendMailer_StatusCodeBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"200 lower 2xx boundary", 200, false},
		{"201 inside 2xx", 201, false},
		{"299 upper 2xx boundary", 299, false},
		{"300 just above 2xx", 300, true},
		{"404 client error", 404, true},
		{"500 server error", 500, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"status":"x"}`))
			}))
			t.Cleanup(srv.Close)

			m := NewResendMailer("k", "f@example.com").WithEndpoint(srv.URL)
			err := m.Send(context.Background(), "to@example.com", "s", "<p>b</p>")
			if tt.wantErr && err == nil {
				t.Errorf("status %d: expected error, got nil", tt.status)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("status %d: expected no error, got %v", tt.status, err)
			}
		})
	}
}

// TestResendMailer_DefaultTimeout pins the HTTP client's Timeout to 10s so
// arithmetic mutations (10*time.Second → 10/time.Second, 10+time.Second, etc.)
// are caught.
func TestResendMailer_DefaultTimeout(t *testing.T) {
	m := NewResendMailer("k", "f@example.com")
	if m.client.Timeout != 10*time.Second {
		t.Errorf("client.Timeout = %v, want %v", m.client.Timeout, 10*time.Second)
	}
}
