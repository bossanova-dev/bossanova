package upstream

import (
	"bytes"
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
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/cloud/cloudorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

const (
	testProdOrchestratorOrigin    = "https://orchestrator.bossanova.dev"
	testStagingOrchestratorOrigin = "https://orchestrator-staging.bossanova.dev"
	testProdWorkOSIssuer          = "https://api.workos.com/user_management/client_01KP805YXXAMZSN2YB4NGXS9XB"
	testStagingWorkOSIssuer       = "https://api.workos.com/user_management/client_01KP805YA85J1BJ4FZN39R7MRT"
)

func makeScopeTestJWT(t *testing.T, iss string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadBytes, err := json.Marshal(map[string]any{
		"iss": iss,
		"sub": "user_test",
		"aud": "bosso",
	})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".signature"
}

type scopeRecordingOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler
}

func (scopeRecordingOrchestrator) RegisterDaemon(
	_ context.Context,
	_ *connect.Request[pb.RegisterDaemonRequest],
) (*connect.Response[pb.RegisterDaemonResponse], error) {
	return connect.NewResponse(&pb.RegisterDaemonResponse{SessionToken: "sess-recorded"}), nil
}

func newScopeRecordingServer(t *testing.T) (*httptest.Server, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var authHeaders [][]string
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(scopeRecordingOrchestrator{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		vals := append([]string(nil), r.Header.Values("Authorization")...)
		authHeaders = append(authHeaders, vals)
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]string, len(authHeaders))
		copy(out, authHeaders)
		return out
	}
}

func TestUnknownOrchestratorReceivesNoStoredToken_OnboardingAuth_AC268802(t *testing.T) {
	srv, recordedAuth := newScopeRecordingServer(t)
	prodJWT := makeScopeTestJWT(t, testProdWorkOSIssuer)
	stagingJWT := makeScopeTestJWT(t, testStagingWorkOSIssuer)

	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)
	var refreshCalls atomic.Int32
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		refreshCalls.Add(1)
		return nil, errors.New("unexpected WorkOS refresh")
	}
	kc := &fakeKeychain{record: &keychainTokens{
		AccessToken:  prodJWT,
		RefreshToken: "rt-prod",
		ExpiresAt:    time.Now().Add(time.Hour),
	}}
	kc.install(t)

	var logBuf bytes.Buffer
	p := testProvider(kc.snapshot())
	p.SetLogger(zerolog.New(&logBuf))
	p.SetOrchestratorScope(srv.URL)

	if got := p.Token(); got != "" {
		t.Fatalf("Token() for unknown orchestrator = %q, want empty", got)
	}
	if exp := p.ExpiresAt(); !exp.IsZero() {
		t.Fatalf("ExpiresAt() for unknown orchestrator = %v, want zero", exp)
	}
	if usable, reason := p.CredentialVerdict(); usable || reason != "" {
		t.Fatalf("CredentialVerdict() = (%v, %q), want (false, \"\")", usable, reason)
	}

	client := bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)
	jwt := RegisterJWT(p, &Config{OrchestratorURL: srv.URL})
	if jwt != "" {
		t.Fatalf("RegisterJWT() for unknown orchestrator without UserJWT = %q, want empty", jwt)
	}
	if _, err := Register(context.Background(), client, "daemon-1", "host-1", jwt, nil, nil); err != nil {
		t.Fatalf("Register() unexpected error: %v", err)
	}

	headers := recordedAuth()
	if len(headers) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(headers))
	}
	if len(headers[0]) != 0 {
		t.Fatalf("unknown orchestrator received Authorization headers %v, want none", headers[0])
	}

	_ = p.Token()
	_ = p.ExpiresAt()
	p.Reload()
	logOutput := logBuf.String()
	if strings.Contains(logOutput, prodJWT) {
		t.Fatalf("refusal log leaked token material: %s", logOutput)
	}
	if gotLines := strings.Count(strings.TrimSpace(logOutput), "\n") + 1; strings.TrimSpace(logOutput) == "" || gotLines != 1 {
		t.Fatalf("expected exactly 1 refusal warning log line, got %d:\n%s", gotLines, logOutput)
	}

	p.SetOrchestratorScope(testProdOrchestratorOrigin)
	if got := p.Token(); got != prodJWT {
		t.Fatalf("Token() on production origin = %q, want production JWT", got)
	}
	if p.ExpiresAt().IsZero() {
		t.Fatal("ExpiresAt() on production origin = zero, want non-zero")
	}
	if usable, _ := p.CredentialVerdict(); !usable {
		t.Fatal("CredentialVerdict() on production origin = false, want true")
	}

	var mismatchLogs bytes.Buffer
	pMismatch := testProvider(&keychainTokens{
		AccessToken:  stagingJWT,
		RefreshToken: "rt-staging",
		ExpiresAt:    time.Now().Add(30 * time.Second),
	})
	pMismatch.SetOrchestratorScope(testProdOrchestratorOrigin)
	pMismatch.SetLogger(zerolog.New(&mismatchLogs))
	if got := pMismatch.Token(); got != "" {
		t.Fatalf("Token() for staging JWT on production = %q, want empty", got)
	}
	if !pMismatch.ExpiresAt().IsZero() {
		t.Fatalf("ExpiresAt() for staging JWT on production = %v, want zero", pMismatch.ExpiresAt())
	}
	if tok, err := pMismatch.Refresh(context.Background()); tok != "" || err != nil {
		t.Fatalf("Refresh() for staging JWT on production = (%q, %v), want (\"\", nil)", tok, err)
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("WorkOS refresh called %d times, want 0", refreshCalls.Load())
	}
	if reason := pMismatch.ReloginReason(); reason != "" {
		t.Fatalf("ReloginReason() = %q, want empty", reason)
	}
	if !strings.Contains(mismatchLogs.String(), "staging") || !strings.Contains(mismatchLogs.String(), "production") {
		t.Fatalf("mismatch warning log missing environment names: %s", mismatchLogs.String())
	}
	if strings.Contains(mismatchLogs.String(), stagingJWT) {
		t.Fatalf("mismatch warning log leaked token material: %s", mismatchLogs.String())
	}
}

func TestExplicitUserJWTWinsOverKeychainOffCloud_OnboardingAuth_AC268802(t *testing.T) {
	srv, recordedAuth := newScopeRecordingServer(t)
	prodJWT := makeScopeTestJWT(t, testProdWorkOSIssuer)
	const explicitJWT = "explicit-off-cloud-jwt"

	p := testProvider(&keychainTokens{
		AccessToken:  prodJWT,
		RefreshToken: "rt-prod",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	p.SetOrchestratorScope(srv.URL)

	cfgWithJWT := &Config{
		OrchestratorURL: srv.URL,
		UserJWT:         explicitJWT,
	}
	if err := cfgWithJWT.CredentialRefusal(); err != nil {
		t.Fatalf("CredentialRefusal() with UserJWT = %v, want nil", err)
	}

	gotJWT := RegisterJWT(p, cfgWithJWT)
	if gotJWT != explicitJWT {
		t.Fatalf("RegisterJWT() = %q, want explicit JWT %q", gotJWT, explicitJWT)
	}

	client := bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)
	if _, err := Register(context.Background(), client, "daemon-1", "host-1", gotJWT, nil, nil); err != nil {
		t.Fatalf("Register() unexpected error: %v", err)
	}
	headers := recordedAuth()
	if len(headers) != 1 || len(headers[0]) != 1 || headers[0][0] != "Bearer "+explicitJWT {
		t.Fatalf("recorded Authorization headers = %v, want [[Bearer %s]]", headers, explicitJWT)
	}

	cfgNoJWT := &Config{OrchestratorURL: "https://untrusted.example/custom?secret=1"}
	refusal := cfgNoJWT.CredentialRefusal()
	if refusal == nil {
		t.Fatal("CredentialRefusal() without UserJWT = nil, want error")
	}
	if !errors.Is(refusal, cloudorigin.ErrUnknownOrchestrator) {
		t.Fatalf("CredentialRefusal() error = %v, want ErrUnknownOrchestrator", refusal)
	}
	msg := refusal.Error()
	for _, want := range []string{"https://untrusted.example", "BOSSD_USER_JWT", "BOSSD_ORCHESTRATOR_URL"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("CredentialRefusal() message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret=1") {
		t.Fatalf("CredentialRefusal() message leaked query string: %q", msg)
	}

	for _, knownURL := range []string{testProdOrchestratorOrigin, testStagingOrchestratorOrigin} {
		if err := (&Config{OrchestratorURL: knownURL}).CredentialRefusal(); err != nil {
			t.Fatalf("CredentialRefusal(%q) = %v, want nil", knownURL, err)
		}
	}
	if err := (*Config)(nil).CredentialRefusal(); err != nil {
		t.Fatalf("(*Config)(nil).CredentialRefusal() = %v, want nil", err)
	}
}

func TestRegisterJWTPrefersExplicitUserJWTOnCloudOrigins(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		issuer string
	}{
		{name: "production", origin: testProdOrchestratorOrigin, issuer: testProdWorkOSIssuer},
		{name: "staging", origin: testStagingOrchestratorOrigin, issuer: testStagingWorkOSIssuer},
	}
	const explicitJWT = "explicit-cloud-user-jwt"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keychainJWT := makeScopeTestJWT(t, tc.issuer)
			p := testProvider(&keychainTokens{
				AccessToken:  keychainJWT,
				RefreshToken: "rt-keychain",
				ExpiresAt:    time.Now().Add(-time.Minute),
			})
			p.SetOrchestratorScope(tc.origin)

			if got := RegisterJWT(p, &Config{OrchestratorURL: tc.origin, UserJWT: explicitJWT}); got != explicitJWT {
				t.Fatalf("RegisterJWT() with explicit UserJWT = %q, want %q", got, explicitJWT)
			}
			if got := RegisterJWT(p, &Config{OrchestratorURL: tc.origin}); got != keychainJWT {
				t.Fatalf("RegisterJWT() without UserJWT = %q, want keychain JWT", got)
			}
		})
	}
}

func TestScopedRefreshDoesNotPauseExplicitJWTDaemon_OnboardingAuth_AC268802(t *testing.T) {
	srv, _ := newScopeRecordingServer(t)
	prodJWT := makeScopeTestJWT(t, testProdWorkOSIssuer)
	const explicitJWT = "explicit-dev-jwt"

	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)
	var workOSCalls atomic.Int32
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		workOSCalls.Add(1)
		t.Errorf("WorkOS refresh must not be called for a scoped-out orchestrator")
		return nil, ErrRefreshTokenRejected
	}
	kc := &fakeKeychain{record: &keychainTokens{
		AccessToken:  prodJWT,
		RefreshToken: "rt-prod",
		ExpiresAt:    time.Now().Add(30 * time.Second),
	}}
	kc.install(t)

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	p := testProvider(kc.snapshot())
	p.SetLogger(logger)
	p.SetOrchestratorScope(srv.URL)

	if exp := p.ExpiresAt(); !exp.IsZero() {
		t.Fatalf("ExpiresAt() while scoped out = %v, want zero", exp)
	}

	tok, err := p.Refresh(context.Background())
	if tok != "" || err != nil {
		t.Fatalf("Refresh() while scoped out = (%q, %v), want (\"\", nil)", tok, err)
	}
	if workOSCalls.Load() != 0 {
		t.Fatalf("WorkOS refresh calls = %d, want 0", workOSCalls.Load())
	}

	authState := NewAuthState()
	client := bossanovav1connect.NewOrchestratorServiceClient(srv.Client(), srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemonOpener := &connectOpener{
		client:    client,
		authToken: explicitJWT,
		tokens:    p,
		authState: authState,
		logger:    logger,
	}
	rawDaemon := daemonOpener.DaemonStream(ctx).(connectBidiAdapter)
	if got := rawDaemon.stream.RequestHeader().Get("Authorization"); got != "Bearer "+explicitJWT {
		t.Fatalf("DaemonStream Authorization = %q, want %q", got, "Bearer "+explicitJWT)
	}
	_ = rawDaemon.CloseRequest()

	termOpener := &terminalConnectOpener{
		client:            client,
		authToken:         explicitJWT,
		tokens:            p,
		authState:         authState,
		logger:            logger,
		skipAbortOnCancel: true,
	}
	rawTerm := termOpener.TerminalStream(ctx).(terminalConnectBidiAdapter)
	if got := rawTerm.stream.RequestHeader().Get("Authorization"); got != "Bearer "+explicitJWT {
		t.Fatalf("TerminalStream Authorization = %q, want %q", got, "Bearer "+explicitJWT)
	}
	_ = rawTerm.CloseRequest()

	if authState.NeedsLogin() {
		t.Fatal("AuthState.NeedsLogin() = true, want false for explicit-JWT daemon")
	}
	if workOSCalls.Load() != 0 {
		t.Fatalf("WorkOS refresh calls after stream opens = %d, want 0", workOSCalls.Load())
	}
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("ReloginReason() = %q, want empty", reason)
	}
}
