package cloudrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const testReportProdWorkOSIssuer = "https://api.workos.com/user_management/client_01KP805YXXAMZSN2YB4NGXS9XB"

func testReportJWT(t *testing.T, iss string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(map[string]string{
		"iss": iss,
		"aud": "bosso",
		"sub": "user_report_test",
	})
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("test-signature"))
	return header + "." + payload + "." + sig
}

type reportStaticTokenStore struct {
	tokens    *auth.Tokens
	loadCalls atomic.Int32
}

func (s *reportStaticTokenStore) Save(tokens *auth.Tokens) error {
	s.tokens = tokens
	return nil
}

func (s *reportStaticTokenStore) Load() (*auth.Tokens, error) {
	s.loadCalls.Add(1)
	if s.tokens == nil {
		return nil, errors.New("no stored tokens")
	}
	return s.tokens, nil
}

func (s *reportStaticTokenStore) Delete() error {
	s.tokens = nil
	return nil
}

type reportRecordingOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
}

func (*reportRecordingOrchestrator) ReportBug(context.Context, *connect.Request[pb.ReportBugRequest]) (*connect.Response[pb.ReportBugResponse], error) {
	return connect.NewResponse(&pb.ReportBugResponse{
		ReportId: "BOS-REPORT-TEST",
	}), nil
}

func TestNewReportClientOmitsTokenForUnknownURL_OnboardingAuth_AC268803(t *testing.T) {
	var (
		requests   atomic.Int32
		mu         sync.Mutex
		authHeader string
	)
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(&reportRecordingOrchestrator{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		mu.Lock()
		authHeader = r.Header.Get("Authorization")
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	t.Setenv("BOSS_REPORT_URL", srv.URL)

	store := &reportStaticTokenStore{
		tokens: &auth.Tokens{
			AccessToken: testReportJWT(t, testReportProdWorkOSIssuer),
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := auth.NewManager(store, auth.Config{})

	client, err := NewReportClient(context.Background(), mgr)
	if err != nil {
		t.Fatalf("NewReportClient failed: %v", err)
	}
	resp, err := ReportBug(context.Background(), client, &pb.ReportBugRequest{
		Comment: "bug report to unknown URL",
	})
	if err != nil {
		t.Fatalf("ReportBug failed: %v", err)
	}
	if resp.GetReportId() != "BOS-REPORT-TEST" {
		t.Fatalf("ReportBug ReportId = %q, want BOS-REPORT-TEST", resp.GetReportId())
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("server received %d request(s), want 1", n)
	}
	mu.Lock()
	gotAuth := authHeader
	mu.Unlock()
	if gotAuth != "" {
		t.Fatalf("Authorization header = %q, want empty string for unknown BOSS_REPORT_URL", gotAuth)
	}
	if n := store.loadCalls.Load(); n != 0 {
		t.Errorf("token store Load() called %d time(s) for unknown BOSS_REPORT_URL, want 0", n)
	}
}
