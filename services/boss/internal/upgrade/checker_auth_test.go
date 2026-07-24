package upgrade

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const stableReleaseBody = `[{"tag_name":"v1.2.4","html_url":"https://example.test/stable"}]`

func okReleaseResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(stableReleaseBody)),
		Header:     make(http.Header),
		Request:    req,
	}
}

func TestCheckerCheckSendsAuthorizationWhenTokenSet(t *testing.T) {
	t.Parallel()

	var gotAuth string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotAuth = req.Header.Get("Authorization")
			return okReleaseResponse(req), nil
		}),
	}

	if _, err := (Checker{HTTPClient: client, Token: "secret-tok"}).Check(context.Background(), "v1.2.3"); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if want := "Bearer secret-tok"; gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestCheckerCheckOmitsAuthorizationWhenTokenEmpty(t *testing.T) {
	t.Parallel()

	authSeen := false
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "" {
				authSeen = true
			}
			return okReleaseResponse(req), nil
		}),
	}

	if _, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3"); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if authSeen {
		t.Fatal("Authorization header sent with an empty token, want none")
	}
}

func TestVerifyReleaseTagAuthorizationHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		token    string
		wantAuth string
	}{
		{name: "with token", token: "tok", wantAuth: "Bearer tok"},
		{name: "without token", token: "", wantAuth: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotAuth string
			client := &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					gotAuth = req.Header.Get("Authorization")
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader("")),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}),
			}
			if err := VerifyReleaseTag(context.Background(), client, "", "v1.2.3", tt.token); err != nil {
				t.Fatalf("VerifyReleaseTag() error = %v", err)
			}
			if gotAuth != tt.wantAuth {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tt.wantAuth)
			}
		})
	}
}

func TestCheckerCheckRateLimitError(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			h := make(http.Header)
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			h.Set("X-RateLimit-Resource", "core")
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     h,
				Request:    req,
			}, nil
		}),
	}

	_, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("Check() error = %v, want *RateLimitError", err)
	}
	if !rlErr.Resets.Equal(reset) {
		t.Fatalf("Resets = %v, want %v", rlErr.Resets, reset)
	}
	if rlErr.Resource != "core" {
		t.Fatalf("Resource = %q, want %q", rlErr.Resource, "core")
	}
	for _, want := range []string{"rate limit", "resets at", "gh auth login"} {
		if !strings.Contains(rlErr.Error(), want) {
			t.Fatalf("Error() = %q, want containing %q", rlErr.Error(), want)
		}
	}
}

func TestCheckerCheckNonRateLimit403IsGeneric(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			// A 403 without X-RateLimit-Remaining: 0 is a genuine forbidden,
			// not a rate limit.
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}

	_, err := (Checker{HTTPClient: client}).Check(context.Background(), "v1.2.3")
	if err == nil {
		t.Fatal("Check() error = nil, want a generic 403 error")
	}
	var rlErr *RateLimitError
	if errors.As(err, &rlErr) {
		t.Fatalf("Check() returned *RateLimitError for a non-rate-limit 403: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("Error() = %q, want containing %q", err.Error(), "HTTP 403")
	}
}

func TestCheckerCheckRetriesAnonymouslyOnUnauthorized(t *testing.T) {
	t.Parallel()

	var auths []string
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			auths = append(auths, req.Header.Get("Authorization"))
			if len(auths) == 1 {
				// A stale/expired token yields 401 on the first (authed) try.
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
					Request:    req,
				}, nil
			}
			return okReleaseResponse(req), nil
		}),
	}

	got, err := (Checker{HTTPClient: client, Token: "stale-tok"}).Check(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Check() error = %v, want anonymous-fallback success", err)
	}
	if got.LatestVersion != "v1.2.4" {
		t.Fatalf("Check() latest = %q, want v1.2.4", got.LatestVersion)
	}
	if len(auths) != 2 {
		t.Fatalf("request count = %d, want 2 (authed then anonymous retry)", len(auths))
	}
	if auths[0] != "Bearer stale-tok" {
		t.Fatalf("first request Authorization = %q, want Bearer stale-tok", auths[0])
	}
	if auths[1] != "" {
		t.Fatalf("retry Authorization = %q, want none (anonymous fallback)", auths[1])
	}
}

func TestVerifyReleaseTagRateLimitError(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			h := make(http.Header)
			h.Set("X-RateLimit-Remaining", "0")
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     h,
				Request:    req,
			}, nil
		}),
	}

	err := VerifyReleaseTag(context.Background(), client, "", "v1.2.4", "")
	var rlErr *RateLimitError
	if !errors.As(err, &rlErr) {
		t.Fatalf("VerifyReleaseTag() error = %v, want *RateLimitError", err)
	}
}
