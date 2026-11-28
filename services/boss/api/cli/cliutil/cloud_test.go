package cliutil

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/contract/cloud/cloudorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const (
	testProdWorkOSIssuer    = "https://api.workos.com/user_management/client_01KP805YXXAMZSN2YB4NGXS9XB"
	testStagingWorkOSIssuer = "https://api.workos.com/user_management/client_01KP805YA85J1BJ4FZN39R7MRT"
)

func testUnsignedJWTWithIssuer(t *testing.T, iss string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(map[string]string{
		"iss": iss,
		"aud": "bosso",
		"sub": "user_test_123",
	})
	if err != nil {
		t.Fatalf("marshal test JWT claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("test-signature-material"))
	return header + "." + payload + "." + sig
}

type cliutilStaticTokenStore struct {
	tokens    *auth.Tokens
	loadCalls atomic.Int32
}

func (s *cliutilStaticTokenStore) Save(tokens *auth.Tokens) error {
	s.tokens = tokens
	return nil
}

func (s *cliutilStaticTokenStore) Load() (*auth.Tokens, error) {
	s.loadCalls.Add(1)
	if s.tokens == nil {
		return nil, errors.New("no stored tokens")
	}
	return s.tokens, nil
}

func (s *cliutilStaticTokenStore) Delete() error {
	s.tokens = nil
	return nil
}

type cliutilRecordingOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
}

func (*cliutilRecordingOrchestrator) ListOrganizations(context.Context, *connect.Request[pb.ListOrganizationsRequest]) (*connect.Response[pb.ListOrganizationsResponse], error) {
	return connect.NewResponse(&pb.ListOrganizationsResponse{}), nil
}

type cliutilRecordedRequests struct {
	count atomic.Int32
	mu    sync.Mutex
	auth  []string
}

func (r *cliutilRecordedRequests) authHeaders() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.auth))
	copy(out, r.auth)
	return out
}

func newRecordingOrchestratorServer(t *testing.T) (*httptest.Server, *cliutilRecordedRequests) {
	t.Helper()
	rec := &cliutilRecordedRequests{}
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(&cliutilRecordingOrchestrator{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.count.Add(1)
		rec.mu.Lock()
		rec.auth = append(rec.auth, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestCloudTokenRefusesUnknownOrchestrator_OnboardingAuth_AC268801(t *testing.T) {
	t.Setenv("BOSS_CLOUD_TOKEN", "")
	srv, rec := newRecordingOrchestratorServer(t)

	prodToken := testUnsignedJWTWithIssuer(t, testProdWorkOSIssuer)
	store := &cliutilStaticTokenStore{
		tokens: &auth.Tokens{
			AccessToken: prodToken,
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := auth.NewManager(store, auth.Config{})
	notLoggedIn := errors.New("feature requires login: run 'boss login' first")

	remote, err := DialCloudRemote(context.Background(), mgr, srv.URL, notLoggedIn)
	if err == nil {
		_, _ = remote.ListOrganizations(context.Background())
		t.Fatal("DialCloudRemote succeeded for unknown orchestrator, want refusal")
	}
	if remote != nil {
		t.Fatalf("DialCloudRemote returned non-nil client on error: %v", remote)
	}
	if !errors.Is(err, cloudorigin.ErrUnknownOrchestrator) {
		t.Fatalf("err = %v, want errors.Is(ErrUnknownOrchestrator)", err)
	}
	if errors.Is(err, notLoggedIn) || strings.Contains(err.Error(), "boss login") {
		t.Errorf("err = %q, must not wrap notLoggedIn or advise running boss login", err.Error())
	}
	wantOrigin := cloudorigin.OriginString(srv.URL)
	if !strings.Contains(err.Error(), wantOrigin) {
		t.Errorf("err = %q, want it to name origin %q", err.Error(), wantOrigin)
	}
	if !strings.Contains(err.Error(), "BOSS_CLOUD_TOKEN") {
		t.Errorf("err = %q, want it to name BOSS_CLOUD_TOKEN", err.Error())
	}
	if strings.Contains(err.Error(), prodToken) {
		t.Errorf("err leaked stored token material: %q", err.Error())
	}
	if n := rec.count.Load(); n != 0 {
		t.Errorf("unknown orchestrator received %d request(s), want 0", n)
	}
	if n := store.loadCalls.Load(); n != 0 {
		t.Errorf("token store Load() called %d time(s) for unknown orchestrator, want 0", n)
	}

	t.Run("staging JWT against production URL refuses with ErrEnvironmentMismatch", func(t *testing.T) {
		stagingToken := testUnsignedJWTWithIssuer(t, testStagingWorkOSIssuer)
		stagingStore := &cliutilStaticTokenStore{
			tokens: &auth.Tokens{
				AccessToken: stagingToken,
				ExpiresAt:   time.Now().Add(time.Hour),
			},
		}
		stagingMgr := auth.NewManager(stagingStore, auth.Config{})

		tok, err := CloudToken(context.Background(), stagingMgr, "https://orchestrator.bossanova.dev")
		if err == nil || tok != "" {
			t.Fatalf("CloudToken = (%q, %v), want empty token and ErrEnvironmentMismatch", tok, err)
		}
		if !errors.Is(err, cloudorigin.ErrEnvironmentMismatch) {
			t.Fatalf("err = %v, want errors.Is(ErrEnvironmentMismatch)", err)
		}
		if !strings.Contains(err.Error(), "BOSS_CLOUD_TOKEN") {
			t.Errorf("err = %q, want BOSS_CLOUD_TOKEN hint", err.Error())
		}
		if strings.Contains(err.Error(), stagingToken) {
			t.Errorf("err leaked stored token material: %q", err.Error())
		}

		if _, dialErr := DialCloudRemote(context.Background(), stagingMgr, "https://orchestrator.bossanova.dev", notLoggedIn); !errors.Is(dialErr, cloudorigin.ErrEnvironmentMismatch) || errors.Is(dialErr, notLoggedIn) {
			t.Fatalf("DialCloudRemote err = %v, want unwrapped ErrEnvironmentMismatch", dialErr)
		}
	})

	t.Run("empty token store against known production URL still wraps notLoggedIn", func(t *testing.T) {
		emptyMgr := auth.NewManager(&cliutilStaticTokenStore{}, auth.Config{})
		_, err := DialCloudRemote(context.Background(), emptyMgr, "https://orchestrator.bossanova.dev", notLoggedIn)
		if !errors.Is(err, notLoggedIn) {
			t.Fatalf("DialCloudRemote err = %v, want notLoggedIn", err)
		}
	})
}

func TestCloudTokenSendsExplicitToken_OnboardingAuth_AC268801(t *testing.T) {
	srv, rec := newRecordingOrchestratorServer(t)

	keychainToken := testUnsignedJWTWithIssuer(t, testProdWorkOSIssuer)
	store := &cliutilStaticTokenStore{
		tokens: &auth.Tokens{
			AccessToken: keychainToken,
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := auth.NewManager(store, auth.Config{})
	const explicitToken = "explicit-cli-cloud-token"
	t.Setenv("BOSS_CLOUD_TOKEN", explicitToken)

	notLoggedIn := errors.New("feature requires login: run 'boss login' first")
	remote, err := DialCloudRemote(context.Background(), mgr, srv.URL, notLoggedIn)
	if err != nil {
		t.Fatalf("DialCloudRemote with BOSS_CLOUD_TOKEN failed: %v", err)
	}
	if _, err := remote.ListOrganizations(context.Background()); err != nil {
		t.Fatalf("ListOrganizations failed: %v", err)
	}

	if n := rec.count.Load(); n != 1 {
		t.Fatalf("server saw %d request(s), want 1", n)
	}
	headers := rec.authHeaders()
	wantHeader := "Bearer " + explicitToken
	if len(headers) != 1 || headers[0] != wantHeader {
		t.Fatalf("Authorization headers = %v, want [%q]", headers, wantHeader)
	}
	if strings.Contains(headers[0], keychainToken) {
		t.Fatalf("Authorization header leaked keychain token: %q", headers[0])
	}
	if n := store.loadCalls.Load(); n != 0 {
		t.Errorf("token store Load() called %d time(s) when BOSS_CLOUD_TOKEN is set, want 0", n)
	}
}
