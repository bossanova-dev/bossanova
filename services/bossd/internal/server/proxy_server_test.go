package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/session"
)

// --- fakes ---

type fakeFailover struct {
	result            session.FailoverResult
	prepareErr        error
	commitErr         error
	prepareCalls      int
	commitCalls       int
	lastStatus        int
	currentBearer     string
	currentBearerErr  error
	currentBearerCall int
	currentBearerID   string
}

func (f *fakeFailover) PrepareFailover(_ context.Context, _ string, status int) (session.FailoverResult, error) {
	f.prepareCalls++
	f.lastStatus = status
	return f.result, f.prepareErr
}

func (f *fakeFailover) CurrentBearer(_ context.Context, sessionID string) (string, error) {
	f.currentBearerCall++
	f.currentBearerID = sessionID
	return f.currentBearer, f.currentBearerErr
}

func (f *fakeFailover) CommitFailover(context.Context, string, session.FailoverResult) error {
	f.commitCalls++
	return f.commitErr
}

// errOnAuthTransport fails the RoundTrip whose Authorization matches failAuth,
// so a test can simulate a replay-leg transport error while the initial leg
// still reaches the real upstream.
type errOnAuthTransport struct {
	base     http.RoundTripper
	failAuth string
}

func (t errOnAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == t.failAuth {
		return nil, fmt.Errorf("simulated replay transport failure")
	}
	return t.base.RoundTrip(req)
}

// recordedRequest captures what the upstream saw, for assertions.
type recordedRequest struct {
	method    string
	path      string
	auth      string
	apiKey    string
	beta      string
	version   string
	userAgent string
	body      string
}

// fakeUpstream is an httptest.Server standing in for api.anthropic.com. It
// returns 429 for the "first-token" bearer and 200 for the "second-token"
// bearer, so a 429→swap→replay flow succeeds. It records every request.
type fakeUpstream struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func newFakeUpstream(t *testing.T) (*fakeUpstream, *httptest.Server) {
	u := &fakeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{
			method:    r.Method,
			path:      r.URL.Path,
			auth:      auth,
			apiKey:    r.Header.Get("x-api-key"),
			beta:      r.Header.Get("anthropic-beta"),
			version:   r.Header.Get("anthropic-version"),
			userAgent: r.Header.Get("User-Agent"),
			body:      string(body),
		})
		u.mu.Unlock()
		switch auth {
		case "Bearer second-token":
			w.Header().Set("anthropic-ratelimit-unified-5h-status", "allowed")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "pong")
		case "Bearer first-token":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "rate limited")
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "unauthorized upstream")
		}
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

func (u *fakeUpstream) all() []recordedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]recordedRequest, len(u.requests))
	copy(out, u.requests)
	return out
}

func startProxy(t *testing.T, f Failover, upstreamURL string, logger zerolog.Logger) (*ProxyServer, string) {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{Failover: f, Logger: logger, Upstream: upstreamURL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- ps.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
		<-serveErr
	})
	return ps, fmt.Sprintf("http://127.0.0.1:%d", ps.Port())
}

// doMessages sends a POST /v1/messages through the proxy for the given token.
func doMessages(t *testing.T, base, token, bearer, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/s/"+token+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "claude-code/bossanova-1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// --- tests ---

func TestProxy_2xxPassThrough(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	// second-token yields 200 on the FIRST attempt, so no failover fires.
	resp := doMessages(t, base, tok, "second-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("PrepareFailover called %d times on a 2xx; want 0", f.prepareCalls)
	}
	reqs := up.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].path != "/v1/messages" || reqs[0].beta != "oauth-2025-04-20" ||
		reqs[0].version != "2023-06-01" || reqs[0].userAgent != "claude-code/bossanova-1" {
		t.Fatalf("headers/path not preserved on forward: %+v", reqs[0])
	}
}

// TestProxy_firstLegTranslatesSentinelToBoundBearer proves the interactive-mode
// fix (BOS-326): a Claude session that sends only a sentinel x-api-key (the REPL
// ignores CLAUDE_CODE_OAUTH_TOKEN and authenticates via ANTHROPIC_API_KEY) has
// its FIRST leg forwarded with the bound account's OAuth bearer resolved via
// CurrentBearer — and the sentinel key is stripped so it never reaches upstream.
func TestProxy_firstLegTranslatesSentinelToBoundBearer(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearer: "second-token"} // bound account's bearer
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	// Client sends ONLY x-api-key (the sentinel) and no Authorization header —
	// exactly what the interactive REPL emits under ANTHROPIC_API_KEY.
	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Fatalf("status=%d body=%q, want 200/pong (first leg served by bound bearer)", resp.StatusCode, body)
	}
	if f.currentBearerCall != 1 {
		t.Fatalf("CurrentBearer calls=%d, want 1", f.currentBearerCall)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("PrepareFailover calls=%d, want 0 (first leg already succeeded)", f.prepareCalls)
	}
	reqs := up.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].auth != "Bearer second-token" {
		t.Fatalf("upstream Authorization = %q, want bound bearer", reqs[0].auth)
	}
	if reqs[0].apiKey != "" {
		t.Fatalf("sentinel x-api-key leaked upstream: %q (must be stripped)", reqs[0].apiKey)
	}
}

func TestProxy_firstLegChatTokenUsesChatTarget(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearer: "second-token"}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForChat("sess-1", "chat-1", "acct-chat")

	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if f.currentBearerID != session.ProxyTargetForChat("chat-1", "acct-chat") {
		t.Fatalf("CurrentBearer target = %q, want chat target", f.currentBearerID)
	}
	reqs := up.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].auth != "Bearer second-token" || reqs[0].apiKey != "" {
		t.Fatalf("upstream auth/apiKey = %q/%q, want bearer only", reqs[0].auth, reqs[0].apiKey)
	}
}

// TestProxy_firstLegNoBearerForwardsClientHeader proves the fail-safe: when
// CurrentBearer returns "" (unbound / proxy disabled / unresolvable) the proxy
// forwards the client's own headers unchanged — today's behavior.
func TestProxy_firstLegNoBearerForwardsClientHeader(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearer: ""} // no bound bearer
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	// Client authenticates itself with a working bearer; proxy must not clobber it.
	resp := doMessages(t, base, tok, "second-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (client header forwarded unchanged)", resp.StatusCode)
	}
	reqs := up.all()
	if len(reqs) != 1 || reqs[0].auth != "Bearer second-token" {
		t.Fatalf("client Authorization not preserved: %+v", reqs)
	}
	if f.currentBearerCall != 0 {
		t.Fatalf("CurrentBearer calls=%d, want 0 for self-authenticated request", f.currentBearerCall)
	}
}

func TestProxy_firstLegNonSentinelAPIKeyDoesNotResolveBoundBearer(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearerErr: fmt.Errorf("materializer should not be called")}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", "sk-ant-client-owned")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if f.currentBearerCall != 0 {
		t.Fatalf("CurrentBearer calls=%d, want 0 for client x-api-key", f.currentBearerCall)
	}
	reqs := up.all()
	if len(reqs) != 1 || reqs[0].apiKey != "sk-ant-client-owned" {
		t.Fatalf("client x-api-key not preserved: %+v", reqs)
	}
}

// TestProxy_firstLegBearerUnavailableReturns503 proves the transient-dependency
// fix: when CurrentBearer ERRORS because the bound account's OAuth bearer cannot
// be resolved (e.g. the claude plugin subprocess is briefly restarting, so
// MaterializeAccount is unreachable), the proxy must NOT forward the managed
// sentinel — that is a guaranteed upstream 401 that kills the interactive
// session. It fails closed with a RETRYABLE 503 (+ Retry-After) so the CLI
// retries once the plugin recovers, never touching upstream and never burning a
// rotation on a self-inflicted 401.
func TestProxy_firstLegBearerUnavailableReturns503(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearerErr: fmt.Errorf("plugin MaterializeAccount: %w", session.ErrBearerUnavailable)}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 (retryable; a doomed sentinel request must not be forwarded)", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatalf("missing Retry-After header on 503")
	}
	if got := len(up.all()); got != 0 {
		t.Fatalf("upstream saw %d requests, want 0 (must not forward a guaranteed-401 sentinel request)", got)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("PrepareFailover calls=%d, want 0 (transient resolve failure must not cool/rotate the bound account)", f.prepareCalls)
	}
}

func TestProxy_firstLegNonBearerErrorPassesThrough(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearerErr: fmt.Errorf("session store unavailable")}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (non-materializer errors are not retryable bearer unavailability)", resp.StatusCode)
	}
	reqs := up.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].apiKey != "" {
		t.Fatalf("sentinel x-api-key leaked upstream: %q", reqs[0].apiKey)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("PrepareFailover calls=%d, want 0", f.prepareCalls)
	}
}

// TestProxy_firstLegUnboundSentinelPassesThrough preserves the credentialless
// contract: when CurrentBearer returns "" with NO error (the session is
// genuinely unbound — there is no managed account to translate to), the sentinel
// is stripped and the resulting upstream 401 passes through WITHOUT triggering
// failover. Only a transient resolve ERROR (above) yields a retryable 503.
func TestProxy_firstLegUnboundSentinelPassesThrough(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearer: ""} // unbound, no error
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (unbound sentinel passes through, no failover)", resp.StatusCode)
	}
	reqs := up.all()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].apiKey != "" {
		t.Fatalf("sentinel x-api-key leaked upstream: %q", reqs[0].apiKey)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("PrepareFailover calls=%d, want 0", f.prepareCalls)
	}
}

func TestProxy_429_swapsAndReplays(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	const sent = `{"prompt":"failover please"}`
	resp := doMessages(t, base, tok, "first-token", sent)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after replay", resp.StatusCode)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
	if f.prepareCalls != 1 || f.lastStatus != http.StatusTooManyRequests {
		t.Fatalf("PrepareFailover calls=%d lastStatus=%d, want 1/429", f.prepareCalls, f.lastStatus)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover calls=%d, want 1 (account_id persisted to second account)", f.commitCalls)
	}
	reqs := up.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (original + replay)", len(reqs))
	}
	if reqs[0].auth != "Bearer first-token" {
		t.Fatalf("original bearer = %q, want first-token", reqs[0].auth)
	}
	if reqs[1].auth != "Bearer second-token" {
		t.Fatalf("replay bearer = %q, want second-token (Authorization: Bearer swapped)", reqs[1].auth)
	}
	// Buffered body replayed byte-for-byte.
	if reqs[0].body != sent || reqs[1].body != sent {
		t.Fatalf("body not byte-identical across replay: original=%q replay=%q sent=%q", reqs[0].body, reqs[1].body, sent)
	}
	// Preserved headers on the replay too.
	if reqs[1].beta != "oauth-2025-04-20" || reqs[1].version != "2023-06-01" || reqs[1].userAgent != "claude-code/bossanova-1" {
		t.Fatalf("headers not preserved on replay: %+v", reqs[1])
	}
}

// TestProxy_swapIsSticky proves the swap sticks across requests: after one
// 429→swap→replay, a SECOND request whose (frozen-env) subprocess still sends
// the original capped bearer is forwarded with the swapped bearer on its FIRST
// leg, so it succeeds WITHOUT re-entering failover. Without stickiness the
// second request would re-trip the 429, re-decide, and cool the healthy account
// just rotated to — cascading through the pool and auditing once per request.
func TestProxy_swapIsSticky(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	// Request 1: subprocess sends the capped first-token → swap → replay 200.
	resp1 := doMessages(t, base, tok, "first-token", `{"prompt":"one"}`)
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("request 1 status = %d, want 200", resp1.StatusCode)
	}

	// Request 2: subprocess env is frozen, so it STILL sends first-token. With a
	// sticky swap the proxy forwards second-token on the first leg → 200 and no
	// second failover.
	resp2 := doMessages(t, base, tok, "first-token", `{"prompt":"two"}`)
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || string(body2) != "pong" {
		t.Fatalf("request 2 status=%d body=%q, want 200/pong", resp2.StatusCode, body2)
	}

	if f.prepareCalls != 1 {
		t.Fatalf("PrepareFailover called %d times, want 1 (no re-failover on the sticky second request)", f.prepareCalls)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover called %d times, want 1 (exactly one AuditEvent across both requests)", f.commitCalls)
	}
	reqs := up.all()
	// r0: first-token 429, r1: second-token replay 200 (request 1);
	// r2: second-token 200 on the FIRST leg (request 2, sticky).
	if len(reqs) != 3 {
		t.Fatalf("upstream saw %d requests, want 3 (429 + replay + sticky forward)", len(reqs))
	}
	if reqs[2].auth != "Bearer second-token" {
		t.Fatalf("sticky forward bearer = %q, want second-token (swap must persist)", reqs[2].auth)
	}

	// After Deregister the sticky bearer is dropped.
	ps.Deregister("sess-1")
	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("sticky bearer survived Deregister: %q", got)
	}
}

// TestProxy_forgetBearer proves ForgetBearer drops the sticky swapped bearer
// (so a subsequent manual SwitchAccount respawn is not silently overridden)
// while keeping the session's path token registered.
func TestProxy_forgetBearer(t *testing.T) {
	_, srv := newFakeUpstream(t)
	ps, _ := startProxy(t, &fakeFailover{}, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")
	ps.rememberBearer("sess-1", "swapped-token")

	ps.ForgetBearer("sess-1")

	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("sticky bearer not cleared by ForgetBearer: %q", got)
	}
	if sid, ok := ps.sessionForToken(tok); !ok || sid != "sess-1" {
		t.Fatalf("path token must survive ForgetBearer: sessionForToken=%q,%v", sid, ok)
	}
	ps.ForgetBearer("unknown-session") // no panic for an unknown session
}

func TestProxy_401_swapsAndReplays(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_AUTH_INVALIDATED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	// "third-token" is neither first nor second, so the upstream returns 401.
	resp := doMessages(t, base, tok, "third-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 401 replay", resp.StatusCode)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
	if f.lastStatus != http.StatusUnauthorized {
		t.Fatalf("PrepareFailover lastStatus=%d, want 401", f.lastStatus)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover calls=%d, want 1", f.commitCalls)
	}
	reqs := up.all()
	if len(reqs) != 2 || reqs[1].auth != "Bearer second-token" {
		t.Fatalf("expected replay with second-token, got %+v", reqs)
	}
}

// TestProxy_commitFailure_servesReplayButNotSticky proves the self-heal
// contract: when CommitFailover fails after a successful replay, the client
// still receives the 200 replay, but the swap is NOT made sticky (so the next
// request re-forwards the original bearer and re-tries the failover, keeping the
// forwarded account in sync with the un-rebound account_id).
func TestProxy_commitFailure_servesReplayButNotSticky(t *testing.T) {
	_, srv := newFakeUpstream(t)
	f := &fakeFailover{
		result:    session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2"},
		commitErr: fmt.Errorf("db down"),
	}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Fatalf("status=%d body=%q, want 200/pong (successful replay must still be served)", resp.StatusCode, body)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover calls=%d, want 1", f.commitCalls)
	}
	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("swap must NOT be sticky when commit failed, but cached bearer = %q", got)
	}
}

// TestProxy_replayStillFails_noCommit proves a replay that itself returns >=400
// (e.g. the next account also rejects) skips commit, doesn't stick the swap, and
// returns the failed replay to the client unchanged.
func TestProxy_replayStillFails_noCommit(t *testing.T) {
	_, srv := newFakeUpstream(t)
	// third-token is neither first nor second, so the fake upstream 401s the replay.
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "third-token", NextAccountID: "acct-2"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401 (failed replay returned unchanged)", resp.StatusCode)
	}
	if f.commitCalls != 0 {
		t.Fatalf("CommitFailover calls=%d, want 0 (no commit on a failed replay)", f.commitCalls)
	}
	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("swap must NOT be sticky on a failed replay, cached bearer = %q", got)
	}
}

// TestProxy_replayTransportError_502 proves a transport error on the replay leg
// returns 502 (no commit, not sticky).
func TestProxy_replayTransportError_502(t *testing.T) {
	_, srv := newFakeUpstream(t)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2"}}
	ps, err := NewProxyServer(ProxyServerConfig{
		Failover:  f,
		Logger:    zerolog.Nop(),
		Upstream:  srv.URL,
		Transport: errOnAuthTransport{base: http.DefaultTransport, failAuth: "Bearer second-token"},
	})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- ps.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
		<-serveErr
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", ps.Port())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 on replay transport error", resp.StatusCode)
	}
	if f.commitCalls != 0 {
		t.Fatalf("CommitFailover calls=%d, want 0", f.commitCalls)
	}
	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("swap must NOT be sticky on a replay transport error, cached bearer = %q", got)
	}
}

func TestProxy_noTarget_originalStatusUnchanged(t *testing.T) {
	up, srv := newFakeUpstream(t)
	// Rotate=false models both "no rotation target" and "materialize failure":
	// the proxy must return the ORIGINAL upstream response unchanged.
	f := &fakeFailover{result: session.FailoverResult{Rotate: false}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want original 429 unchanged", resp.StatusCode)
	}
	if string(body) != "rate limited" {
		t.Fatalf("body = %q, want original upstream body", body)
	}
	if f.prepareCalls != 1 {
		t.Fatalf("PrepareFailover calls=%d, want 1", f.prepareCalls)
	}
	if f.commitCalls != 0 {
		t.Fatalf("CommitFailover calls=%d, want 0 (no rotation)", f.commitCalls)
	}
	if len(up.all()) != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (no replay)", len(up.all()))
	}
}

func TestProxy_authRouting(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	good := ps.TokenForSession("sess-1")

	t.Run("wrong token rejected with 401, no upstream call", func(t *testing.T) {
		resp := doMessages(t, base, "wrong-"+good, "second-token", `{}`)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 for wrong token", resp.StatusCode)
		}
	})

	t.Run("absent token (bare path) 404, no upstream call", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodHead, base+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for tokenless probe", resp.StatusCode)
		}
	})

	if len(up.all()) != 0 {
		t.Fatalf("upstream must not be reached for auth failures, saw %d", len(up.all()))
	}
}

func TestProxy_neverLogsToken(t *testing.T) {
	var buf bytes.Buffer
	_, srv := newFakeUpstream(t)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	_ = resp.Body.Close()

	logs := buf.String()
	if strings.Contains(logs, "second-token") {
		t.Fatalf("bearer token leaked into logs:\n%s", logs)
	}
	if strings.Contains(logs, tok) {
		t.Fatalf("per-session proxy token leaked into logs:\n%s", logs)
	}
}

func TestProxy_tokenRegistryRoundTrip(t *testing.T) {
	_, srv := newFakeUpstream(t)
	ps, _ := startProxy(t, &fakeFailover{}, srv.URL, zerolog.Nop())

	tok := ps.TokenForSession("sess-1")
	if tok == "" || len(tok) != 64 {
		t.Fatalf("token = %q, want 64 hex chars", tok)
	}
	if again := ps.TokenForSession("sess-1"); again != tok {
		t.Fatalf("TokenForSession not stable: %q != %q", again, tok)
	}
	if sid, ok := ps.sessionForToken(tok); !ok || sid != "sess-1" {
		t.Fatalf("sessionForToken(%q) = %q,%v want sess-1,true", tok, sid, ok)
	}
	ps.Deregister("sess-1")
	if _, ok := ps.sessionForToken(tok); ok {
		t.Fatalf("token still resolves after Deregister")
	}
}

func TestParseProxyPath(t *testing.T) {
	cases := []struct {
		in      string
		wantTok string
		wantUp  string
		wantOK  bool
	}{
		{"/s/abc/v1/messages", "abc", "/v1/messages", true},
		{"/s/abc", "abc", "/", true},
		{"/s/abc/", "abc", "/", true},
		{"/v1/messages", "", "", false},
		{"/", "", "", false},
		{"/s/", "", "", false},
	}
	for _, tc := range cases {
		tok, up, ok := parseProxyPath(tc.in)
		if tok != tc.wantTok || up != tc.wantUp || ok != tc.wantOK {
			t.Errorf("parseProxyPath(%q) = %q,%q,%v want %q,%q,%v", tc.in, tok, up, ok, tc.wantTok, tc.wantUp, tc.wantOK)
		}
	}
}
