package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProbeRateLimitCallsOAuthUsageEndpoint(t *testing.T) {
	var gotAuth, gotBeta, gotUserAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %q, want /api/oauth/usage", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotUserAgent = r.Header.Get("User-Agent")
		http.ServeFile(w, r, "testdata/usage/healthy.json")
	}))
	t.Cleanup(srv.Close)

	s := &Server{
		logger:     zerolog.Nop(),
		httpClient: srv.Client(),
		usageURL:   srv.URL + "/api/oauth/usage",
	}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "secret-token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer token", gotAuth)
	}
	if gotBeta != claudeOAuthBetaHeader {
		t.Errorf("anthropic-beta = %q, want %q", gotBeta, claudeOAuthBetaHeader)
	}
	if gotUserAgent != claudeUsageUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUserAgent, claudeUsageUserAgent)
	}
	if resp.GetStatus().GetLimited() {
		t.Fatal("limited = true, want false for healthy fixture")
	}
	if got := resp.GetStatus().GetStatus(); got != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE {
		t.Fatalf("status = %v, want ACTIVE", got)
	}
	if got := resp.GetStatus().GetUtil_5H(); got != 0.33 {
		t.Errorf("util_5h = %v, want 0.33", got)
	}
	if got := resp.GetStatus().GetUtil_7D(); got != 0.13 {
		t.Errorf("util_7d = %v, want 0.13", got)
	}
	if got := resp.GetStatus().GetPlanTier(); got != "max_5x" {
		t.Errorf("plan_tier = %q, want max_5x", got)
	}
	if got := resp.GetStatus().GetReset_5H().AsTime(); !got.Equal(time.Date(2026, 4, 11, 7, 0, 0, 528743000, time.UTC)) {
		t.Errorf("reset_5h = %v, want fixture timestamp", got)
	}
}

func TestParseUsageDerivesRateLimited(t *testing.T) {
	data, err := os.ReadFile("testdata/usage/exhausted.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := parseUsage(data)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if !got.GetLimited() {
		t.Fatal("limited = false, want true")
	}
	if got.GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED {
		t.Fatalf("status = %v, want RATE_LIMITED", got.GetStatus())
	}
	if got.GetUtil_5H() != 1 {
		t.Errorf("util_5h = %v, want 1", got.GetUtil_5H())
	}
}

func TestParseUsageUsesMostConstrainedWeeklyModelWindow(t *testing.T) {
	data, err := os.ReadFile("testdata/usage/split_weekly_exhausted.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := parseUsage(data)
	if err != nil {
		t.Fatalf("parseUsage: %v", err)
	}
	if !got.GetLimited() {
		t.Fatal("limited = false, want true from split weekly window")
	}
	if got.GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED {
		t.Fatalf("status = %v, want RATE_LIMITED", got.GetStatus())
	}
	if got.GetUtil_7D() != 1 {
		t.Errorf("util_7d = %v, want 1 from seven_day_sonnet", got.GetUtil_7D())
	}
	if got.GetReset_7D() == nil {
		t.Fatal("reset_7d = nil, want split window reset")
	}
}

func TestParseUsageRejectsUnrecognizedSchema(t *testing.T) {
	if _, err := parseUsage([]byte(`{"usage":{"fiveHour":{"utilization":100}}}`)); err == nil {
		t.Fatal("parseUsage err = nil, want error for unrecognized schema")
	}
}

func TestParseUsageRejectsRecognizedWindowWithUnrecognizedFields(t *testing.T) {
	if _, err := parseUsage([]byte(`{"five_hour":{"usage":100}}`)); err == nil {
		t.Fatal("parseUsage err = nil, want error for unrecognized window fields")
	}
}

func TestProbeRateLimitHTTP401ReturnsAuthInvalidated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid oauth token"}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "dead-token"},
	})
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want auth error")
	}
	if resp != nil {
		t.Fatalf("ProbeRateLimit resp = %#v, want nil on auth error", resp)
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("status.Code = %v, want Unauthenticated", status.Code(err))
	}
	if !errors.Is(err, errClaudeUsageAuthInvalidated) {
		t.Fatalf("err does not wrap auth invalidated sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), agenterr.KindAuthInvalidated.String()) {
		t.Fatalf("err does not expose %q: %v", agenterr.KindAuthInvalidated.String(), err)
	}
}

func TestProbeRateLimitUsageTransportFailureReturnsError(t *testing.T) {
	client := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("temporary usage outage")
		}),
	}

	s := &Server{logger: zerolog.Nop(), httpClient: client, usageURL: "https://example.invalid/api/oauth/usage"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
	})
	if resp != nil {
		t.Fatalf("resp = %v, want nil on transient usage failure", resp)
	}
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want transient usage failure")
	}
	if !strings.Contains(err.Error(), "temporary usage outage") {
		t.Fatalf("err = %v, want temporary usage outage", err)
	}
}

func TestProbeRateLimitFallsBackToHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", "1764554400")
		w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.41")
		w.Header().Set("anthropic-ratelimit-unified-7d-reset", "2026-04-17T00:59:59.951713Z")
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "rate_limited")
		w.Header().Set("anthropic-ratelimit-tier", "max_20x")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	got := resp.GetStatus()
	if !got.GetLimited() {
		t.Fatal("limited = false, want true from per-window status fallback")
	}
	if got.GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED {
		t.Fatalf("status = %v, want RATE_LIMITED", got.GetStatus())
	}
	if got.GetUtil_5H() != 0.42 {
		t.Errorf("util_5h = %v, want 0.42", got.GetUtil_5H())
	}
	if got.GetReset_5H().AsTime() != time.Unix(1764554400, 0).UTC() {
		t.Errorf("reset_5h = %v, want epoch reset", got.GetReset_5H().AsTime())
	}
	if got.GetPlanTier() != "max_20x" {
		t.Errorf("plan_tier = %q, want max_20x", got.GetPlanTier())
	}
}

func TestProbeRateLimitHTTP403WithoutUsageHeadersFallsBackToMessagesHeaders(t *testing.T) {
	var gotMessagesMethod, gotMessagesAuth, gotMessagesVersion string
	var gotMessagesBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
		case "/v1/messages":
			gotMessagesMethod = r.Method
			gotMessagesAuth = r.Header.Get("Authorization")
			gotMessagesVersion = r.Header.Get("anthropic-version")
			body, _ := io.ReadAll(r.Body)
			gotMessagesBody = string(body)
			w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.51")
			w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.92")
			w.Header().Set("anthropic-ratelimit-unified-status", "allowed_warning")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if gotMessagesMethod != http.MethodPost {
		t.Fatalf("messages method = %q, want POST", gotMessagesMethod)
	}
	if gotMessagesAuth != "Bearer setup-token" {
		t.Fatalf("messages auth = %q, want Bearer setup-token", gotMessagesAuth)
	}
	if gotMessagesVersion != claudeAPIVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotMessagesVersion, claudeAPIVersion)
	}
	if !strings.Contains(gotMessagesBody, `"max_tokens":1`) || !strings.Contains(gotMessagesBody, claudeProbeModel) {
		t.Fatalf("messages body = %s, want minimal probe request", gotMessagesBody)
	}
	got := resp.GetStatus()
	if got.GetLimited() {
		t.Fatal("limited = true, want false for warning probe")
	}
	if got.GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING {
		t.Fatalf("status = %v, want WARNING", got.GetStatus())
	}
	if got.GetUtil_5H() != 0.51 {
		t.Fatalf("util_5h = %v, want 0.51", got.GetUtil_5H())
	}
	if got.GetUtil_7D() != 0.92 {
		t.Fatalf("util_7d = %v, want 0.92", got.GetUtil_7D())
	}
}

func TestProbeRateLimitMessagesFallbackUnauthorizedReturnsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
		case "/v1/messages":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid bearer token"}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-token"},
	})
	if resp != nil {
		t.Fatalf("resp = %v, want nil on auth invalidation", resp)
	}
	if !errors.Is(err, errClaudeUsageAuthInvalidated) {
		t.Fatalf("err = %v, want auth invalidated", err)
	}
}

func TestProbeRateLimitMessagesFallbackTransportFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
		case "/v1/messages":
			t.Fatal("messages request should fail in transport before reaching handler")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	client := srv.Client()
	baseTransport := http.DefaultTransport
	client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1/messages" {
			return nil, errors.New("temporary messages outage")
		}
		return baseTransport.RoundTrip(req)
	})

	s := &Server{logger: zerolog.Nop(), httpClient: client, usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-token"},
	})
	if resp != nil {
		t.Fatalf("resp = %v, want nil on transient messages failure", resp)
	}
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want transient messages failure")
	}
	if !strings.Contains(err.Error(), "temporary messages outage") {
		t.Fatalf("err = %v, want temporary messages outage", err)
	}
}

func TestProbeRateLimitUsageParseFailureFallsBackToMessagesHeaders(t *testing.T) {
	var messagesProbed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			// 200 OK but a body with no recognized usage fields and no
			// rate-limit headers, so parseUsage fails and the header
			// fallback finds nothing — forcing the messages probe.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"unrelated":"payload"}`))
		case "/v1/messages":
			messagesProbed = true
			w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.10")
			w.Header().Set("anthropic-ratelimit-unified-status", "allowed")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if !messagesProbed {
		t.Fatal("messages endpoint was not probed after usage parse failure")
	}
	got := resp.GetStatus()
	if got.GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE {
		t.Fatalf("status = %v, want ACTIVE from messages headers", got.GetStatus())
	}
	if got.GetUtil_5H() != 0.10 {
		t.Fatalf("util_5h = %v, want 0.10", got.GetUtil_5H())
	}
}

func TestParseUsageHeadersMapsClaudeStatusValues(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    bossanovav1.RateLimitPlanStatus
		limited bool
	}{
		{
			name: "allowed",
			raw:  "allowed",
			want: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
		},
		{
			name: "allowed warning",
			raw:  "allowed_warning",
			want: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING,
		},
		{
			name:    "rejected",
			raw:     "rejected",
			want:    bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
			limited: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("anthropic-ratelimit-unified-status", tt.raw)

			got, ok := parseUsageHeaders(headers)
			if !ok {
				t.Fatal("parseUsageHeaders ok = false, want true")
			}
			if got.GetStatus() != tt.want {
				t.Fatalf("status = %v, want %v", got.GetStatus(), tt.want)
			}
			if got.GetLimited() != tt.limited {
				t.Fatalf("limited = %v, want %v", got.GetLimited(), tt.limited)
			}
		})
	}
}

func TestParseUsageHeadersMapsGenericResetByRepresentativeClaim(t *testing.T) {
	tests := []struct {
		name       string
		claim      string
		want5H     bool
		want7D     bool
		wantStatus bossanovav1.RateLimitPlanStatus
	}{
		{
			name:       "five hour",
			claim:      "five_hour",
			want5H:     true,
			wantStatus: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED,
		},
		{
			name:       "weekly opus",
			claim:      "seven_day_opus",
			want7D:     true,
			wantStatus: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING,
		},
		{
			name:       "model shorthand",
			claim:      "sonnet",
			want7D:     true,
			wantStatus: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("anthropic-ratelimit-unified-status", "allowed_warning")
			if tt.wantStatus == bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED {
				headers.Set("anthropic-ratelimit-unified-status", "rejected")
			}
			headers.Set("anthropic-ratelimit-unified-reset", "1764554400")
			headers.Set("anthropic-ratelimit-unified-representative-claim", tt.claim)

			got, ok := parseUsageHeaders(headers)
			if !ok {
				t.Fatal("parseUsageHeaders ok = false, want true")
			}
			if got.GetStatus() != tt.wantStatus {
				t.Fatalf("status = %v, want %v", got.GetStatus(), tt.wantStatus)
			}
			if (got.GetReset_5H() != nil) != tt.want5H {
				t.Fatalf("reset_5h present = %v, want %v", got.GetReset_5H() != nil, tt.want5H)
			}
			if (got.GetReset_7D() != nil) != tt.want7D {
				t.Fatalf("reset_7d present = %v, want %v", got.GetReset_7D() != nil, tt.want7D)
			}
		})
	}
}

func TestParseHeaderUtilAcceptsFractionAndPercentShapes(t *testing.T) {
	tests := []struct {
		raw  string
		want float64
	}{
		{raw: "0.82", want: 0.82},
		{raw: "1.2", want: 0.012},
		{raw: "5", want: 0.05},
		{raw: "82", want: 0.82},
		{raw: "120", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseHeaderUtil(tt.raw)
			if !ok {
				t.Fatal("parseHeaderUtil ok = false, want true")
			}
			if got != tt.want {
				t.Fatalf("parseHeaderUtil(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestProbeRateLimitFallsBackToHeadersWhenBodySchemaDrifts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "rate_limited")
		_, _ = w.Write([]byte(`{"usage":{"fiveHour":{"utilization":100}}}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if !resp.GetStatus().GetLimited() {
		t.Fatal("limited = false, want true from fallback headers")
	}
	if resp.GetStatus().GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED {
		t.Fatalf("status = %v, want RATE_LIMITED", resp.GetStatus().GetStatus())
	}
}

func TestProbeRateLimitUsesStatusOnlyFallbackHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-status", "rate_limited")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if !resp.GetStatus().GetLimited() {
		t.Fatal("limited = false, want true from status-only fallback")
	}
}

func TestProbeRateLimitReportsUnsupportedWhenEndpointAndHeadersUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage", "/v1/messages":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if resp.GetStatus().GetLimited() {
		t.Fatal("limited = true, want false on ambiguous failure")
	}
	if resp.GetStatus().GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Fatalf("status = %v, want UNSUPPORTED fail-safe", resp.GetStatus().GetStatus())
	}
}
