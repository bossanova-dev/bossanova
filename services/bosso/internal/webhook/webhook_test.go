package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// --- HMAC-SHA256 Verification Tests ---

func TestGitHubVerifySignature_Valid(t *testing.T) {
	parser := &GitHubParser{}
	body := []byte(`{"action":"completed"}`)
	secret := "test-secret-123"

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-Hub-Signature-256", sig)

	if err := parser.VerifySignature(r, body, secret); err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
}

func TestGitHubVerifySignature_InvalidSecret(t *testing.T) {
	parser := &GitHubParser{}
	body := []byte(`{"action":"completed"}`)

	mac := hmac.New(sha256.New, []byte("correct-secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-Hub-Signature-256", sig)

	err := parser.VerifySignature(r, body, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("error = %q, want 'mismatch'", err.Error())
	}
}

func TestGitHubVerifySignature_MissingHeader(t *testing.T) {
	parser := &GitHubParser{}
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	// No signature header.

	err := parser.VerifySignature(r, []byte("body"), "secret")
	if err == nil {
		t.Fatal("expected error for missing header, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error = %q, want 'missing'", err.Error())
	}
}

func TestGitHubVerifySignature_InvalidFormat(t *testing.T) {
	parser := &GitHubParser{}
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-Hub-Signature-256", "md5=abc123")

	err := parser.VerifySignature(r, []byte("body"), "secret")
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
}

// --- Event Parsing Tests ---

func TestGitHubParse_CheckSuiteSuccess(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "completed",
		"check_suite": map[string]any{
			"id":         1,
			"conclusion": "success",
			"pull_requests": []any{
				map[string]any{"number": 42},
			},
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "check_suite")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.RepoOriginURL != "https://github.com/owner/repo" {
		t.Errorf("repo = %q, want %q", event.RepoOriginURL, "https://github.com/owner/repo")
	}

	passed := event.Event.GetChecksPassed()
	if passed == nil {
		t.Fatal("expected ChecksPassed event")
	}
	if passed.PrId != 42 {
		t.Errorf("pr_id = %d, want 42", passed.PrId)
	}
}

func TestGitHubParse_CheckSuiteFailure(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "completed",
		"check_suite": map[string]any{
			"id":         1,
			"conclusion": "failure",
			"pull_requests": []any{
				map[string]any{"number": 10},
			},
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "check_suite")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	failed := event.Event.GetChecksFailed()
	if failed == nil {
		t.Fatal("expected ChecksFailed event")
	}
	if failed.PrId != 10 {
		t.Errorf("pr_id = %d, want 10", failed.PrId)
	}
}

func TestGitHubParse_CheckSuiteNoPR(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "completed",
		"check_suite": map[string]any{
			"id":            1,
			"conclusion":    "success",
			"pull_requests": []any{},
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "check_suite")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event != nil {
		t.Error("expected nil event for check_suite with no PRs")
	}
}

func TestGitHubParse_PullRequestMerged(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 7,
			"state":  "closed",
			"merged": true,
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "pull_request")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	merged := event.Event.GetPrMerged()
	if merged == nil {
		t.Fatal("expected PRMerged event")
	}
	if merged.PrId != 7 {
		t.Errorf("pr_id = %d, want 7", merged.PrId)
	}
}

func TestGitHubParse_PullRequestClosed(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number": 5,
			"state":  "closed",
			"merged": false,
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "pull_request")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	closed := event.Event.GetPrClosed()
	if closed == nil {
		t.Fatal("expected PRClosed event")
	}
	if closed.PrId != 5 {
		t.Errorf("pr_id = %d, want 5", closed.PrId)
	}
}

func TestGitHubParse_PullRequestReview(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "submitted",
		"review": map[string]any{
			"state": "changes_requested",
			"body":  "Please fix the tests",
			"user": map[string]any{
				"login": "reviewer",
			},
		},
		"pull_request": map[string]any{
			"number": 15,
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "pull_request_review")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	review := event.Event.GetReviewSubmitted()
	if review == nil {
		t.Fatal("expected ReviewSubmitted event")
	}
	if review.PrId != 15 {
		t.Errorf("pr_id = %d, want 15", review.PrId)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(review.Comments))
	}
	if review.Comments[0].Author != "reviewer" {
		t.Errorf("author = %q, want %q", review.Comments[0].Author, "reviewer")
	}
	if review.Comments[0].State != pb.ReviewState_REVIEW_STATE_CHANGES_REQUESTED {
		t.Errorf("state = %v, want CHANGES_REQUESTED", review.Comments[0].State)
	}
}

func TestGitHubParse_CheckRunFailure(t *testing.T) {
	parser := &GitHubParser{}
	payload := map[string]any{
		"action": "completed",
		"check_run": map[string]any{
			"id":         999,
			"name":       "test-ci",
			"status":     "completed",
			"conclusion": "failure",
			"pull_requests": []any{
				map[string]any{"number": 20},
			},
		},
		"repository": map[string]any{
			"html_url": "https://github.com/owner/repo",
		},
	}
	body, _ := json.Marshal(payload)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "check_run")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}

	failed := event.Event.GetChecksFailed()
	if failed == nil {
		t.Fatal("expected ChecksFailed event")
	}
	if failed.PrId != 20 {
		t.Errorf("pr_id = %d, want 20", failed.PrId)
	}
	if len(failed.FailedChecks) != 1 {
		t.Fatalf("failed_checks = %d, want 1", len(failed.FailedChecks))
	}
	if failed.FailedChecks[0].Name != "test-ci" {
		t.Errorf("name = %q, want %q", failed.FailedChecks[0].Name, "test-ci")
	}
}

func TestGitHubParse_IgnoredEvent(t *testing.T) {
	parser := &GitHubParser{}
	body := []byte(`{"action":"starred"}`)

	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	r.Header.Set("X-GitHub-Event", "star")

	event, err := parser.Parse(r, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if event != nil {
		t.Error("expected nil event for ignored event type")
	}
}

func TestGitHubParse_MissingEventHeader(t *testing.T) {
	parser := &GitHubParser{}
	r := httptest.NewRequest("POST", "/webhooks/github", nil)
	// No X-GitHub-Event header.

	_, err := parser.Parse(r, []byte("{}"))
	if err == nil {
		t.Fatal("expected error for missing event header")
	}
}

// --- Registry Tests ---

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&GitHubParser{})

	p, err := reg.Get("github")
	if err != nil {
		t.Fatalf("Get github: %v", err)
	}
	if p.Provider() != "github" {
		t.Errorf("provider = %q, want %q", p.Provider(), "github")
	}

	_, err = reg.Get("gitlab")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// --- Handler Integration Tests ---

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := &Handler{}
	r := httptest.NewRequest("GET", "/webhooks/github", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestHandler_UnsupportedProvider(t *testing.T) {
	reg := NewRegistry()
	// No parsers registered.
	h := NewHandler(nil, nil, nil, reg, nopLogger())

	r := httptest.NewRequest("POST", "/webhooks/gitlab", nil)
	r.SetPathValue("provider", "gitlab")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_MissingProvider(t *testing.T) {
	reg := NewRegistry()
	h := NewHandler(nil, nil, nil, reg, nopLogger())

	r := httptest.NewRequest("POST", "/webhooks/", nil)
	// No path value set.
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func nopLogger() zerolog.Logger {
	return zerolog.Nop()
}
