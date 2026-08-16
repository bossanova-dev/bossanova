package main

import (
	"bytes"
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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
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

func TestProbeRateLimitUsageSuspensionReturnsPermissionDenied(t *testing.T) {
	// A suspended account (org/billing block) returns a 403 whose body carries
	// the suspension signature — distinct from the benign scope-refusal 403.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"oauth_org_not_allowed","request_id":"req_x"}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "suspended-token"},
	})
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want suspension error")
	}
	if resp != nil {
		t.Fatalf("ProbeRateLimit resp = %#v, want nil on suspension", resp)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status.Code = %v, want PermissionDenied", status.Code(err))
	}
	if !errors.Is(err, errClaudeAccountSuspended) {
		t.Fatalf("err does not wrap suspended sentinel: %v", err)
	}
}

func TestProbeRateLimitMessagesSuspensionReturnsPermissionDenied(t *testing.T) {
	// Usage endpoint 403s benignly (no suspension signature); the messages
	// fallback then surfaces the real /v1/messages 403 suspension body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
		case "/v1/messages":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"Your organization has disabled Claude subscription access for Claude Code"}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "suspended-token"},
	})
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want suspension error from messages fallback")
	}
	if resp != nil {
		t.Fatalf("ProbeRateLimit resp = %#v, want nil on suspension", resp)
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status.Code = %v, want PermissionDenied", status.Code(err))
	}
	if !errors.Is(err, errClaudeAccountSuspended) {
		t.Fatalf("err does not wrap suspended sentinel: %v", err)
	}
}

func TestProbeRateLimitBenign403BothEndpointsIsNotSuspension(t *testing.T) {
	// Neither endpoint carries the suspension signature (a plain setup-token
	// scope refusal on both) — must degrade to UNSUPPORTED, never suspension.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "setup-token"},
	})
	if err != nil {
		t.Fatalf("ProbeRateLimit err = %v, want benign fall-through", err)
	}
	if got := resp.GetStatus().GetStatus(); got != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Fatalf("status = %v, want UNSUPPORTED for benign 403", got)
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

// TestProbeRateLimitHTTP429DoesNotEscalateToMessages is the load-bearing BOS-828
// regression: a throttled usage endpoint must NOT fall through to
// probeMessagesRateLimit, which issues a real, billable POST /v1/messages. That
// call returns unified rate-limit headers, so before this fix the probe
// "succeeded" and the throttle was invisible — silently converting every
// subsequent refresh into a live API call. The test server fails the test if
// /v1/messages is touched at all.
func TestProbeRateLimitHTTP429DoesNotEscalateToMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"too many requests"}`))
		case "/v1/messages":
			t.Error("billable /v1/messages was called for a throttled usage probe")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL + "/api/oauth/usage", messagesURL: srv.URL + "/v1/messages"}
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "throttled-token"},
	})
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want throttle error")
	}
	if resp != nil {
		t.Fatalf("ProbeRateLimit resp = %#v, want nil on throttle", resp)
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("status.Code = %v, want ResourceExhausted", status.Code(err))
	}
	if !errors.Is(err, errClaudeUsageThrottled) {
		t.Fatalf("err does not wrap throttled sentinel: %v", err)
	}
	if got := retryInfoDelay(t, err); got != 120*time.Second {
		t.Fatalf("RetryInfo delay = %v, want 120s", got)
	}
	// The message must never re-derive as an agent rate limit: agenterr
	// deliberately classifies "429"-bearing text as KindRateLimited, which would
	// route a polling throttle into the usage-limit rotation intercept and bench
	// a perfectly healthy account (the BOS-584 bug class, from the other side).
	if agenterr.Classify(err.Error(), time.Now()).Kind == agenterr.KindRateLimited {
		t.Fatalf("throttle message re-derives as KindRateLimited: %q", err.Error())
	}
}

// TestProbeRateLimitHTTP429RetryAfterParsing pins the Retry-After contract:
// the seconds form is honoured, and an absent or malformed value yields a zero
// duration (and therefore no RetryInfo detail) without ever becoming an error.
// The HTTP-date form is deliberately unsupported — it is rare in practice, and
// the caller-side floor covers it.
func TestProbeRateLimitHTTP429RetryAfterParsing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "seconds", header: "120", want: 120 * time.Second},
		{name: "absent", header: "", want: 0},
		{name: "malformed word", header: "soon", want: 0},
		{name: "negative", header: "-30", want: 0},
		{name: "zero", header: "0", want: 0},
		{name: "http date form", header: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{name: "float", header: "12.5", want: 0},
		{name: "padded seconds", header: "  90  ", want: 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
			_, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
				CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "throttled-token"},
			})
			if err == nil {
				t.Fatal("ProbeRateLimit err = nil, want throttle error")
			}
			if !errors.Is(err, errClaudeUsageThrottled) {
				t.Fatalf("err does not wrap throttled sentinel: %v", err)
			}
			if status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("status.Code = %v, want ResourceExhausted", status.Code(err))
			}
			if got := retryInfoDelay(t, err); got != tc.want {
				t.Fatalf("RetryInfo delay = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProbeRateLimitHTTP429WithSuspensionBodyStaysSuspension pins the ordering
// inside the 429 arm: a real org/billing block outranks a polling throttle. A
// suspension is permanent and must still fail the account's health, so it must
// not be masked by the (transient, recoverable) throttle classification.
func TestProbeRateLimitHTTP429WithSuspensionBodyStaysSuspension(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"oauth_org_not_allowed","request_id":"req_x"}`))
	}))
	t.Cleanup(srv.Close)

	s := &Server{logger: zerolog.Nop(), httpClient: srv.Client(), usageURL: srv.URL}
	_, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "suspended-token"},
	})
	if err == nil {
		t.Fatal("ProbeRateLimit err = nil, want suspension error")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("status.Code = %v, want PermissionDenied (suspension outranks throttle)", status.Code(err))
	}
	if !errors.Is(err, errClaudeAccountSuspended) {
		t.Fatalf("err does not wrap suspended sentinel: %v", err)
	}
	if errors.Is(err, errClaudeUsageThrottled) {
		t.Fatal("suspension was misclassified as a throttle")
	}
}

// TestProbeRateLimit403ScopeRefusalStillReachesMessages pins that the new 429
// arm did not widen. A 403 scope refusal is the EXPECTED path for `claude
// setup-token` credentials that lack the user:profile scope, and it must still
// escalate to the messages probe — that is the only way such a token yields any
// usage signal at all.
func TestProbeRateLimit403ScopeRefusalStillReachesMessages(t *testing.T) {
	var messagesProbed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/usage":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
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
		t.Fatalf("ProbeRateLimit err = %v, want the setup-token messages fallback", err)
	}
	if !messagesProbed {
		t.Fatal("403 scope refusal no longer reaches probeMessagesRateLimit; the setup-token path is broken")
	}
	if got := resp.GetStatus().GetUtil_5H(); got != 0.10 {
		t.Fatalf("util_5h = %v, want 0.10 from the messages headers", got)
	}
}

// retryInfoDelay extracts the RetryInfo retry delay attached to err's gRPC
// status, or 0 when no such detail is present.
func retryInfoDelay(t *testing.T, err error) time.Duration {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("err is not a gRPC status error: %v", err)
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.RetryInfo); ok {
			return info.GetRetryDelay().AsDuration()
		}
	}
	return 0
}

// TestProbeRateLimitHTTP429LogsWarnWithoutCredentials pins the operator-facing
// half of the fix. The throttle must be legible in the log — before this it was
// completely silent, indistinguishable from a healthy probe — and the line must
// carry no token or credential material, matching the package contract that
// nothing here ever logs a credential blob.
func TestProbeRateLimitHTTP429LogsWarnWithoutCredentials(t *testing.T) {
	const token = "super-secret-oauth-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	s := &Server{logger: zerolog.New(&buf), httpClient: srv.Client(), usageURL: srv.URL}
	if _, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{
		CredentialEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": token},
	}); err == nil {
		t.Fatal("ProbeRateLimit err = nil, want throttle error")
	}

	logged := buf.String()
	if !strings.Contains(logged, `"level":"warn"`) {
		t.Fatalf("throttle did not log at WARN: %s", logged)
	}
	if !strings.Contains(logged, "throttled the poll") {
		t.Fatalf("throttle log is not distinguishable from a healthy probe: %s", logged)
	}
	if strings.Contains(logged, token) {
		t.Fatalf("throttle log leaked the credential: %s", logged)
	}
	if !strings.Contains(logged, "retry_after") {
		t.Fatalf("throttle log does not carry the parsed retry-after: %s", logged)
	}
}
