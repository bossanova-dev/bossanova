package anthropicorg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const (
	fakeToken = "sk-ant-oat01-fake-token-value"
	fakeOrg   = "4b0f2c1e-fake-org-id"
)

type seenRequest struct {
	method, path, auth, beta, version, contentType, body string
}

// recorder holds requests the fake server saw; the handler runs on a server
// goroutine, so reads go through the mutex.
type recorder struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (r *recorder) all() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seenRequest(nil), r.seen...)
}

func newServer(t *testing.T, status int, org string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.seen = append(rec.seen, seenRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("anthropic-beta"), r.Header.Get("anthropic-version"), r.Header.Get("Content-Type"), string(body)})
		rec.mu.Unlock()
		if org != "" {
			w.Header().Set("Anthropic-Organization-Id", org)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + fakeToken + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestOrganizationID_Accounts_AC225501(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		header  string
		want    string
		wantErr error
	}{
		{"ok", http.StatusOK, fakeOrg, fakeOrg, nil},
		{"oauth refused", http.StatusForbidden, fakeOrg, fakeOrg, nil},
		{"retired model", http.StatusBadRequest, fakeOrg, fakeOrg, nil},
		{"trimmed", http.StatusOK, "  " + fakeOrg + " ", fakeOrg, nil},
		{"unauthorized", http.StatusUnauthorized, fakeOrg, "", ErrUnauthorized},
		{"no header", http.StatusOK, "", "", ErrNoOrganization},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newServer(t, tc.status, tc.header)
			var logs bytes.Buffer
			r := New(WithBaseURL(srv.URL), WithLogger(zerolog.New(&logs).Level(zerolog.TraceLevel)))
			got, err := r.OrganizationID(context.Background(), fakeToken)
			if got != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("OrganizationID = %q, %v; want %q, %v", got, err, tc.want, tc.wantErr)
			}
			seen := rec.all()
			if len(seen) != 1 {
				t.Fatalf("requests = %d, want 1", len(seen))
			}
			req := seen[0]
			if req.method != http.MethodPost || req.path != "/v1/messages/count_tokens" {
				t.Fatalf("request %s %s, want POST /v1/messages/count_tokens", req.method, req.path)
			}
			if req.auth != "Bearer "+fakeToken || req.beta != "oauth-2025-04-20" || req.version != "2023-06-01" || req.contentType != "application/json" {
				t.Fatalf("headers: %+v", req)
			}
			if !strings.Contains(req.body, `"model":"`+probeModel+`"`) || !strings.Contains(req.body, `"messages"`) {
				t.Fatalf("body: %s", req.body)
			}
			assertNoSecrets(t, logs.String())
			if err != nil {
				assertNoSecrets(t, err.Error())
			}
		})
	}
}

func TestOrganizationIDTimeout_Accounts_AC225502(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up.
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Anthropic-Organization-Id", fakeOrg)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	// Cleanups run last-in first-out, so the handler is released before Close
	// waits for it.
	t.Cleanup(func() { close(release) })
	var logs bytes.Buffer
	r := New(WithBaseURL(srv.URL), WithTimeout(50*time.Millisecond), WithLogger(zerolog.New(&logs).Level(zerolog.TraceLevel)))
	got, err := r.OrganizationID(context.Background(), fakeToken)
	if got != "" || err == nil {
		t.Fatalf("OrganizationID = %q, %v; want timeout error", got, err)
	}
	assertNoSecrets(t, logs.String())
	assertNoSecrets(t, err.Error())
}

func TestOrganizationIDTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	got, err := New(WithBaseURL(url)).OrganizationID(context.Background(), fakeToken)
	if got != "" || err == nil {
		t.Fatalf("OrganizationID = %q, %v; want transport error", got, err)
	}
}

func TestNewDefaults(t *testing.T) {
	r := New()
	if r.baseURL != "https://api.anthropic.com" || r.client.Timeout != 5*time.Second {
		t.Fatalf("defaults: %s %s", r.baseURL, r.client.Timeout)
	}
}

func assertNoSecrets(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, fakeToken) || strings.Contains(text, fakeOrg) {
		t.Fatalf("secret leaked: %s", text)
	}
}
