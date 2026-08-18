package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/rotation"
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
	lastKind          rotation.SignalKind
	lastTrigger       string
}

func (f *fakeFailover) PrepareFailover(_ context.Context, _ string, status int) (session.FailoverResult, error) {
	f.prepareCalls++
	f.lastStatus = status
	return f.result, f.prepareErr
}

func (f *fakeFailover) PrepareFailoverKind(_ context.Context, _ string, kind rotation.SignalKind, trigger string) (session.FailoverResult, error) {
	f.prepareCalls++
	f.lastKind = kind
	f.lastTrigger = trigger
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
	return startProxyConfigured(t, f, upstreamURL, logger, nil)
}

// startProxyConfigured is startProxy with a hook to mutate the server BEFORE it
// begins serving (e.g. to shrink the SSE peek byte cap / deadline for a backstop
// test), so the field write happens-before the serve goroutine and stays
// -race-clean.
func startProxyConfigured(t *testing.T, f Failover, upstreamURL string, logger zerolog.Logger, configure func(*ProxyServer)) (*ProxyServer, string) {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{Failover: f, Logger: logger, Upstream: upstreamURL})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if configure != nil {
		configure(ps)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- ps.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = ps.Shutdown(ctx)
		<-serveErr
	})
	return ps, fmt.Sprintf("http://127.0.0.1:%d", ps.Port())
}

// newSSEUpstream stands in for api.anthropic.com on the mid-stream-SSE path. For
// the "second-token" bearer (the rotation target) it returns a PLAIN 200 whose
// body is "REPLAYED", so a replayed stream is trivially distinguishable from the
// initial one. For every other bearer it returns a 200 text/event-stream (well,
// the given contentType) carrying initialBody. It records every request.
func newSSEUpstream(t *testing.T, contentType, initialBody string) (*fakeUpstream, *httptest.Server) {
	u := &fakeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, auth: auth,
			apiKey: r.Header.Get("x-api-key"), body: string(body),
		})
		u.mu.Unlock()
		if auth == "Bearer second-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "REPLAYED")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, initialBody)
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

// Canonical Anthropic Messages SSE frames used by the mid-stream-SSE tests.
const (
	sseMessageStart      = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n"
	ssePing              = "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	sseContentBlockStart = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n"
	sseContentBlockDelta = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
	sseMessageStop       = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	sseRateLimitError    = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"rate limited\"}}\n\n"
	sseOverloadedError   = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n"
)

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

// newForbiddenUpstream returns 200 for the "second-token" bearer and a 403 with
// forbiddenBody for every other bearer, so a suspension 403→swap→replay flow can
// be exercised while a benign 403 can be shown to pass through unchanged.
func newForbiddenUpstream(t *testing.T, forbiddenBody string) (*fakeUpstream, *httptest.Server) {
	u := &fakeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, auth: auth,
			apiKey: r.Header.Get("x-api-key"), body: string(body),
		})
		u.mu.Unlock()
		if auth == "Bearer second-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "pong")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, forbiddenBody)
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

// TestProxy_403Suspension_swapsAndReplays proves an upstream 403 whose body
// confirms an account suspension rotates the session to a healthy account and
// replays — the in-flight analogue of the 401 auth-invalidation path.
func TestProxy_403Suspension_swapsAndReplays(t *testing.T) {
	up, srv := newForbiddenUpstream(t, `{"type":"error","error":{"type":"permission_error","message":"Your organization has disabled Claude subscription access for Claude Code"}}`)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_AUTH_INVALIDATED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "suspended-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after suspension replay", resp.StatusCode)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want pong", body)
	}
	if f.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d, want 1", f.prepareCalls)
	}
	if f.lastKind != rotation.AuthInvalidated {
		t.Fatalf("failover kind = %v, want AuthInvalidated", f.lastKind)
	}
	if f.lastTrigger != "ROTATION_TRIGGER_AUTH_INVALIDATED" {
		t.Fatalf("trigger = %q, want ROTATION_TRIGGER_AUTH_INVALIDATED", f.lastTrigger)
	}
	if f.commitCalls != 1 {
		t.Fatalf("commit calls = %d, want 1", f.commitCalls)
	}
	if reqs := up.all(); len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (original 403 + replay)", len(reqs))
	}
}

// TestProxy_403Benign_passesThrough proves a benign 403 (e.g. a setup-token
// scope refusal) is NOT treated as a suspension: no failover is attempted and
// the original response body is returned to the client byte-for-byte.
func TestProxy_403Benign_passesThrough(t *testing.T) {
	const benign = `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`
	up, srv := newForbiddenUpstream(t, benign)
	// Result would rotate if asked — the assertion is that it is never asked.
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "benign-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 passed through", resp.StatusCode)
	}
	if string(body) != benign {
		t.Fatalf("body = %q, want benign 403 passed through byte-identical", body)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (benign 403 must not trigger failover)", f.prepareCalls)
	}
	if reqs := up.all(); len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (no replay)", len(reqs))
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

// TestProxy_forgetAllBearers proves ForgetAllBearers drops every session's
// sticky swapped bearer (so a credential refresh cannot let a stale swapped
// bearer silently override the freshly-saved credential) while keeping every
// path token registered.
func TestProxy_forgetAllBearers(t *testing.T) {
	_, srv := newFakeUpstream(t)
	ps, _ := startProxy(t, &fakeFailover{}, srv.URL, zerolog.Nop())
	tok1 := ps.TokenForSession("sess-1")
	tok2 := ps.TokenForSession("sess-2")
	ps.rememberBearer("sess-1", "swapped-1")
	ps.rememberBearer("sess-2", "swapped-2")

	ps.ForgetAllBearers()

	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("sticky bearer for sess-1 not cleared: %q", got)
	}
	if got := ps.bearerForSession("sess-2"); got != "" {
		t.Fatalf("sticky bearer for sess-2 not cleared: %q", got)
	}
	if sid, ok := ps.sessionForToken(tok1); !ok || sid != "sess-1" {
		t.Fatalf("path token 1 must survive ForgetAllBearers: %q,%v", sid, ok)
	}
	if sid, ok := ps.sessionForToken(tok2); !ok || sid != "sess-2" {
		t.Fatalf("path token 2 must survive ForgetAllBearers: %q,%v", sid, ok)
	}
	ps.ForgetAllBearers() // idempotent; no panic when already empty
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
		_, _ = ps.Shutdown(ctx)
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

// --- mid-stream SSE rate-limit peek (BOS-408) ---

// TestProxy_SSEErrorFirstRateLimit_swapsAndReplays proves a 200 text/event-stream
// whose FIRST frame is an `error` with rate_limit_error (before any
// message_start) rotates and replays: the client receives ONLY the replayed
// stream, zero bytes of the rejected one.
func TestProxy_SSEErrorFirstRateLimit_swapsAndReplays(t *testing.T) {
	up, srv := newSSEUpstream(t, "text/event-stream", sseRateLimitError)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after replay", resp.StatusCode)
	}
	if string(body) != "REPLAYED" {
		t.Fatalf("body = %q, want REPLAYED (only the replayed stream)", body)
	}
	if strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("rejected stream leaked to client: %q", body)
	}
	if f.prepareCalls != 1 {
		t.Fatalf("PrepareFailover calls = %d, want 1", f.prepareCalls)
	}
	if f.lastKind != rotation.UsageLimited {
		t.Fatalf("failover kind = %v, want UsageLimited", f.lastKind)
	}
	if f.lastTrigger != "ROTATION_TRIGGER_USAGE_LIMITED" {
		t.Fatalf("trigger = %q, want ROTATION_TRIGGER_USAGE_LIMITED", f.lastTrigger)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover calls = %d, want 1", f.commitCalls)
	}
	reqs := up.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (original SSE + replay)", len(reqs))
	}
	if reqs[1].auth != "Bearer second-token" {
		t.Fatalf("replay bearer = %q, want second-token", reqs[1].auth)
	}
}

// TestProxy_SSEPreContentRateLimit_swapsAndReplays proves a rate_limit_error that
// arrives AFTER message_start/ping but BEFORE the first content_block_delta is
// still transparently recovered: the buffered opening frames are discarded, and
// the client sees only the replayed stream.
func TestProxy_SSEPreContentRateLimit_swapsAndReplays(t *testing.T) {
	preContent := sseMessageStart + ssePing + sseRateLimitError
	_, srv := newSSEUpstream(t, "text/event-stream", preContent)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != "REPLAYED" {
		t.Fatalf("status=%d body=%q, want 200/REPLAYED", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "message_start") || strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("buffered pre-content prefix leaked to client: %q", body)
	}
	if f.prepareCalls != 1 || f.lastKind != rotation.UsageLimited {
		t.Fatalf("prepareCalls=%d kind=%v, want 1/UsageLimited", f.prepareCalls, f.lastKind)
	}
	if f.commitCalls != 1 {
		t.Fatalf("CommitFailover calls = %d, want 1", f.commitCalls)
	}
}

// TestProxy_SSEReplayAlsoRateLimited_noCommit proves that when the FIRST leg
// opens with a pre-content SSE rate_limit_error (rotate) and the rotated
// account's REPLAY is ALSO an HTTP-200 SSE rate_limit_error, the swap is NOT
// committed: the rotated account must not be rebound/audited as if it served the
// request just because its status line was <400. The client still receives the
// (reconstructed) rate-limited stream, and the swap is left unstuck so the next
// request re-tries failover.
func TestProxy_SSEReplayAlsoRateLimited_noCommit(t *testing.T) {
	// Both bearers get a 200 text/event-stream that opens with a rate_limit_error
	// before any content — the first leg AND the replay are rate-limited.
	preContent := sseMessageStart + ssePing + sseRateLimitError
	u := &fakeUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			apiKey: r.Header.Get("x-api-key"), body: string(body),
		})
		u.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, preContent)
	}))
	t.Cleanup(srv.Close)

	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "acct-2", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (rate-limited replay streamed through)", resp.StatusCode)
	}
	// The client receives the reconstructed rate-limited stream (honest), not a
	// false success.
	if !strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("body = %q, want the rate_limit_error streamed through", body)
	}
	if f.prepareCalls != 1 {
		t.Fatalf("PrepareFailover calls = %d, want 1 (first leg rotated)", f.prepareCalls)
	}
	if f.commitCalls != 0 {
		t.Fatalf("CommitFailover calls = %d, want 0 (a rate-limited replay must not commit)", f.commitCalls)
	}
	if got := ps.bearerForSession("sess-1"); got != "" {
		t.Fatalf("swap must NOT be sticky when the replay is itself rate-limited, cached bearer = %q", got)
	}
	reqs := u.all()
	if len(reqs) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (original + replay)", len(reqs))
	}
	if reqs[1].auth != "Bearer second-token" {
		t.Fatalf("replay bearer = %q, want second-token", reqs[1].auth)
	}
}

// TestProxy_SSEPostContentRateLimit_passesThrough proves a rate_limit_error that
// arrives AFTER a content_block_delta already shipped is passed through
// byte-for-byte with no replay — the transparent-recovery boundary. (BOS-406/407
// cover this after the fact.)
func TestProxy_SSEPostContentRateLimit_passesThrough(t *testing.T) {
	postContent := sseMessageStart + sseContentBlockStart + sseContentBlockDelta + sseRateLimitError + sseMessageStop
	up, srv := newSSEUpstream(t, "text/event-stream", postContent)
	// Result WOULD rotate if asked — the assertion is that it is never asked.
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 passed through", resp.StatusCode)
	}
	if string(body) != postContent {
		t.Fatalf("body not byte-identical passthrough:\n got %q\nwant %q", body, postContent)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (post-content rate_limit must not rotate)", f.prepareCalls)
	}
	if reqs := up.all(); len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (no replay)", len(reqs))
	}
}

// TestProxy_SSEOverloadedError_passesThrough proves an overloaded_error is
// transient (the CLI retries it) and does NOT rotate — it passes through.
func TestProxy_SSEOverloadedError_passesThrough(t *testing.T) {
	overloaded := sseMessageStart + sseOverloadedError
	_, srv := newSSEUpstream(t, "text/event-stream", overloaded)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(body) != overloaded {
		t.Fatalf("status=%d body=%q, want overloaded stream passed through byte-identical", resp.StatusCode, body)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (overloaded_error must not rotate)", f.prepareCalls)
	}
}

// TestProxy_SSEHappyPath_byteIdentical proves a clean stream (no error) is
// delivered byte-for-byte and failover is never consulted.
func TestProxy_SSEHappyPath_byteIdentical(t *testing.T) {
	happy := sseMessageStart + sseContentBlockStart + ssePing + sseContentBlockDelta + sseMessageStop
	up, srv := newSSEUpstream(t, "text/event-stream", happy)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != happy {
		t.Fatalf("happy-path stream not byte-identical:\n got %q\nwant %q", body, happy)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (no error → no failover)", f.prepareCalls)
	}
	if reqs := up.all(); len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
}

// TestProxy_SSEByteCapBackstop_passesThrough proves the byte-cap backstop: when a
// decisive frame lies beyond the peek's byte budget the stream fails safe to
// pass-through (no rotation), even though a rate_limit_error is present.
func TestProxy_SSEByteCapBackstop_passesThrough(t *testing.T) {
	body := ssePing + sseRateLimitError // ping frame alone exceeds the tiny cap
	_, srv := newSSEUpstream(t, "text/event-stream", body)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxyConfigured(t, f, srv.URL, zerolog.Nop(), func(ps *ProxyServer) {
		ps.ssePeekByteCap = 8 // smaller than the first frame
	})
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(got) != body {
		t.Fatalf("status=%d body=%q, want the stream passed through byte-identical", resp.StatusCode, got)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (byte-cap backstop must not rotate)", f.prepareCalls)
	}
}

// TestProxy_SSEDeadlineBackstop_passesThrough proves the deadline backstop: with
// the peek deadline already elapsed the stream fails safe to pass-through even
// though its first frame is a rate_limit_error. This exercises the between-reads
// loop guard (the deadline is checked at the top of the peek loop), which is the
// bound the deadline actually provides — not a mid-read stall. Deterministic
// (negative deadline, no wall-clock sleep).
func TestProxy_SSEDeadlineBackstop_passesThrough(t *testing.T) {
	_, srv := newSSEUpstream(t, "text/event-stream", sseRateLimitError)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxyConfigured(t, f, srv.URL, zerolog.Nop(), func(ps *ProxyServer) {
		ps.ssePeekDeadline = -time.Second // already elapsed at first read
	})
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(got) != sseRateLimitError {
		t.Fatalf("status=%d body=%q, want the stream passed through byte-identical", resp.StatusCode, got)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (deadline backstop must not rotate)", f.prepareCalls)
	}
}

// TestProxy_NonSSE200_peekNotEngaged proves a non-SSE 200 (application/json) is
// streamed through untouched — the peek is gated on the SSE content type, so a
// rate_limit_error-looking JSON body is never inspected for rotation.
func TestProxy_NonSSE200_peekNotEngaged(t *testing.T) {
	jsonBody := `{"type":"error","error":{"type":"rate_limit_error"}}`
	up, srv := newSSEUpstream(t, "application/json", jsonBody)
	f := &fakeFailover{result: session.FailoverResult{Rotate: true, Token: "second-token"}}
	ps, base := startProxy(t, f, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"hi"}`)
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK || string(got) != jsonBody {
		t.Fatalf("status=%d body=%q, want json body passed through unchanged", resp.StatusCode, got)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare calls = %d, want 0 (non-SSE 200 must not engage the peek)", f.prepareCalls)
	}
	if reqs := up.all(); len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
}

func TestParseSSEFrame(t *testing.T) {
	cases := []struct {
		name      string
		frame     string
		wantEvent string
		wantData  string
	}{
		{"event and data", "event: error\ndata: {\"type\":\"error\"}", "error", `{"type":"error"}`},
		{"data only", "data: {\"type\":\"ping\"}", "", `{"type":"ping"}`},
		{"multi-line data joined with newline", "event: message_start\ndata: {\"type\":\ndata: \"message_start\"}", "message_start", "{\"type\":\n\"message_start\"}"},
		{"carriage returns trimmed", "event: ping\r\ndata: {}\r", "ping", "{}"},
		{"no recognized lines", "id: 42\nretry: 100", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, data := parseSSEFrame([]byte(tc.frame))
			if ev != tc.wantEvent || data != tc.wantData {
				t.Fatalf("parseSSEFrame(%q) = %q,%q want %q,%q", tc.frame, ev, data, tc.wantEvent, tc.wantData)
			}
		})
	}
}

func TestClassifySSEFrame(t *testing.T) {
	cases := []struct {
		name        string
		frame       string
		want        sseDecision
		wantErrType string // BOS-483: the classified SSE error type surfaced for observability
	}{
		{"rate_limit_error rotates", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\"}}", sseRotate, "rate_limit_error"},
		{"overloaded_error passes", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}", ssePassthrough, "overloaded_error"},
		{"content_block_delta passes", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}", ssePassthrough, ""},
		{"message_start undecided", "event: message_start\ndata: {\"type\":\"message_start\"}", sseUndecided, ""},
		{"ping undecided", "event: ping\ndata: {\"type\":\"ping\"}", sseUndecided, ""},
		{"malformed data on error frame does not rotate", "event: error\ndata: {not json", ssePassthrough, ""},
		{"malformed data no event is undecided", "data: {not json", sseUndecided, ""},
		{"event name only content_block_delta passes", "event: content_block_delta", ssePassthrough, ""},
		{"data-type wins over event name", "event: message_start\ndata: {\"type\":\"content_block_delta\"}", ssePassthrough, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errType := classifySSEFrame([]byte(tc.frame))
			if got != tc.want {
				t.Fatalf("classifySSEFrame(%q) = %v, want %v", tc.frame, got, tc.want)
			}
			if errType != tc.wantErrType {
				t.Fatalf("classifySSEFrame(%q) errType = %q, want %q", tc.frame, errType, tc.wantErrType)
			}
		})
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

// freeLoopbackPort grabs an OS-assigned loopback port, then releases it so the
// caller can re-bind it. Used to parametrize the fixed-port bind tests instead
// of hardcoding 44127 (which would flake if the host already holds it).
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release free port: %v", err)
	}
	return port
}

// TestProxyListen_FixedPortBindsExactly asserts that a configured Port > 0 binds
// that exact loopback port and Port() reports it (BOS-409 core fix).
func TestProxyListen_FixedPortBindsExactly(t *testing.T) {
	port := freeLoopbackPort(t)
	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Port: port})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _, _ = ps.Shutdown(context.Background()) }()
	if got := ps.Port(); got != port {
		t.Fatalf("Port() = %d, want the fixed configured port %d", got, port)
	}
}

// TestProxyListen_ImmediateRebindSamePort is the restart-wedge regression guard:
// bind the fixed port, shut down, then IMMEDIATELY re-bind the SAME port and
// assert success. Without SO_REUSEADDR a lingering socket could refuse the
// re-bind, which is exactly the daemon-restart failure BOS-409 fixes.
func TestProxyListen_ImmediateRebindSamePort(t *testing.T) {
	port := freeLoopbackPort(t)

	ps1, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Port: port})
	if err != nil {
		t.Fatalf("NewProxyServer 1: %v", err)
	}
	if err := ps1.Listen(); err != nil {
		t.Fatalf("Listen 1: %v", err)
	}
	if got := ps1.Port(); got != port {
		t.Fatalf("first bind Port() = %d, want %d", got, port)
	}
	if _, err := ps1.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 1: %v", err)
	}

	// Immediately re-bind the same port — simulates a daemon restart.
	ps2, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Port: port})
	if err != nil {
		t.Fatalf("NewProxyServer 2: %v", err)
	}
	if err := ps2.Listen(); err != nil {
		t.Fatalf("immediate re-bind of port %d failed (restart-wedge regression): %v", port, err)
	}
	defer func() { _, _ = ps2.Shutdown(context.Background()) }()
	if got := ps2.Port(); got != port {
		t.Fatalf("re-bind Port() = %d, want the SAME fixed port %d", got, port)
	}
}

// TestProxyListen_ZeroPortEphemeral asserts Port: 0 preserves today's ephemeral
// bind (the explicit opt-out): a non-zero OS-assigned port.
func TestProxyListen_ZeroPortEphemeral(t *testing.T) {
	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Port: 0})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _, _ = ps.Shutdown(context.Background()) }()
	if got := ps.Port(); got == 0 {
		t.Fatal("Port() = 0 after ephemeral bind, want a non-zero OS-assigned port")
	}
}

// TestProxyListen_InUseFallsBackToEphemeral asserts that when the fixed port is
// already held by a live listener, Listen does not error but falls back to an
// ephemeral bind so bossd never fails to start over a port collision.
func TestProxyListen_InUseFallsBackToEphemeral(t *testing.T) {
	port := freeLoopbackPort(t)

	// Hold the fixed port with a plain, live listener for the whole test.
	held, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// Extremely unlikely given freeLoopbackPort just released it, but if the
		// OS re-handed the port we can't run this case deterministically.
		t.Skipf("could not hold port %d for the collision case: %v", port, err)
	}
	defer func() { _ = held.Close() }()

	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Port: port})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := ps.Listen(); err != nil {
		t.Fatalf("Listen must fall back on EADDRINUSE, not error: %v", err)
	}
	defer func() { _, _ = ps.Shutdown(context.Background()) }()
	got := ps.Port()
	if got == 0 {
		t.Fatal("fallback Port() = 0, want a non-zero ephemeral port")
	}
	if got == port {
		t.Fatalf("fallback Port() = %d, want a DIFFERENT ephemeral port (the fixed one is held)", port)
	}
}

// --- BOS-483 pass-through observability ---

// ptClock is a monotonic, race-safe injectable clock for the pass-through
// observability tests (Warn rate-limiting and minute-bucket counters are both
// time-driven, so a real clock would make them flaky).
type ptClock struct {
	mu sync.Mutex
	t  time.Time
}

func newPTClock() *ptClock { return &ptClock{t: time.Unix(1_700_000_000, 0)} }
func (c *ptClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *ptClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newStatusUpstream answers every request with a fixed status + body regardless
// of bearer, so a test can drive an arbitrary un-rotated error response.
func newStatusUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ptEvents parses zerolog JSON lines from raw and returns those whose "event"
// field equals want.
func ptEvents(t *testing.T, raw, want string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["event"] == want {
			out = append(out, m)
		}
	}
	return out
}

// withClock returns a startProxyConfigured hook that pins BOTH the Warn
// rate-limiter clock and the counter clock to clk (they must agree).
func withClock(clk *ptClock) func(*ProxyServer) {
	return func(ps *ProxyServer) {
		ps.now = clk.now
		ps.ptStats.now = clk.now
	}
}

func TestProxy_passthrough_statusNotIntercepted(t *testing.T) {
	var buf bytes.Buffer
	srv := newStatusUpstream(t, http.StatusInternalServerError, "boom")
	f := &fakeFailover{} // Rotate=false
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "self-bearer", `{"prompt":"x"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passed through", resp.StatusCode)
	}
	// A 500 is never an intercept target, so failover is never consulted.
	if f.prepareCalls != 0 {
		t.Fatalf("prepareCalls = %d, want 0 for a 500", f.prepareCalls)
	}
	evs := ptEvents(t, buf.String(), "failover_proxy_passthrough")
	if len(evs) != 1 {
		t.Fatalf("passthrough events = %d, want exactly 1:\n%s", len(evs), buf.String())
	}
	e := evs[0]
	if e["reason"] != passthroughReasonStatusNotIntercepted {
		t.Fatalf("reason = %v, want %s", e["reason"], passthroughReasonStatusNotIntercepted)
	}
	if e["status"].(float64) != 500 || e["method"] != "POST" || e["path"] != "/v1/messages" || e["session"] != "sess-1" {
		t.Fatalf("fields mismatch: %+v", e)
	}
}

func TestProxy_passthrough_failoverDeclined_401(t *testing.T) {
	var buf bytes.Buffer
	// newFakeUpstream returns 401 for any bearer that is not first/second-token.
	_, srv := newFakeUpstream(t)
	f := &fakeFailover{} // PrepareFailover returns Rotate=false
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "unknown-bearer", `{"prompt":"x"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 passed through", resp.StatusCode)
	}
	// A 401 with client auth IS an intercept target, so failover is consulted…
	if f.prepareCalls != 1 {
		t.Fatalf("prepareCalls = %d, want 1 (401 attempts failover)", f.prepareCalls)
	}
	// …but declined (Rotate=false), so it passes through as failover_declined.
	evs := ptEvents(t, buf.String(), "failover_proxy_passthrough")
	if len(evs) != 1 || evs[0]["reason"] != passthroughReasonFailoverDeclined {
		t.Fatalf("want 1 failover_declined event, got:\n%s", buf.String())
	}
}

func TestProxy_passthrough_suspensionNotMatched(t *testing.T) {
	var buf bytes.Buffer
	srv := newStatusUpstream(t, http.StatusForbidden, "scope refused")
	f := &fakeFailover{}
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "self-bearer", `{"prompt":"x"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 passed through", resp.StatusCode)
	}
	// A benign (non-suspension) 403 never rotates.
	if f.prepareCalls != 0 {
		t.Fatalf("prepareCalls = %d, want 0 for a benign 403", f.prepareCalls)
	}
	evs := ptEvents(t, buf.String(), "failover_proxy_passthrough")
	if len(evs) != 1 || evs[0]["reason"] != passthroughReasonSuspensionNotMatched {
		t.Fatalf("want 1 suspension_not_matched event, got:\n%s", buf.String())
	}
}

func TestProxy_passthrough_sseErrorPassthrough(t *testing.T) {
	var buf bytes.Buffer
	overloaded := sseMessageStart + sseOverloadedError
	_, srv := newSSEUpstream(t, "text/event-stream", overloaded)
	f := &fakeFailover{} // Rotate=false even if consulted
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "first-token", `{"prompt":"x"}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != overloaded {
		t.Fatalf("status=%d body=%q, want overloaded stream passed through", resp.StatusCode, body)
	}
	evs := ptEvents(t, buf.String(), "failover_proxy_passthrough")
	if len(evs) != 1 {
		t.Fatalf("passthrough events = %d, want 1:\n%s", len(evs), buf.String())
	}
	e := evs[0]
	if e["reason"] != passthroughReasonSSEErrorPassthrough || e["sse_error"] != "overloaded_error" || e["status"].(float64) != 200 {
		t.Fatalf("sse passthrough fields mismatch: %+v", e)
	}
}

func TestProxy_passthrough_silentOnSuccess(t *testing.T) {
	var buf bytes.Buffer
	_, srv := newFakeUpstream(t)
	f := &fakeFailover{}
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	tok := ps.TokenForSession("sess-1")

	resp := doMessages(t, base, tok, "second-token", `{"prompt":"x"}`) // 200
	_ = resp.Body.Close()
	if evs := ptEvents(t, buf.String(), "failover_proxy_passthrough"); len(evs) != 0 {
		t.Fatalf("a 2xx must stay silent, got %d passthrough events:\n%s", len(evs), buf.String())
	}
}

func TestProxy_passthrough_warnRateLimited(t *testing.T) {
	var buf bytes.Buffer
	clk := newPTClock()
	srv := newStatusUpstream(t, http.StatusInternalServerError, "boom")
	f := &fakeFailover{}
	ps, base := startProxyConfigured(t, f, srv.URL, zerolog.New(&buf), withClock(clk))
	tok := ps.TokenForSession("sess-1")

	// Two 500s inside the same minute → one Warn, but BOTH counted.
	_ = doMessages(t, base, tok, "b", `{}`).Body.Close()
	_ = doMessages(t, base, tok, "b", `{}`).Body.Close()
	if evs := ptEvents(t, buf.String(), "failover_proxy_passthrough"); len(evs) != 1 {
		t.Fatalf("within-window repeats must collapse to 1 Warn, got %d", len(evs))
	}
	snap := ps.PassthroughStatsSnapshot()
	if len(snap) != 1 || snap[0].Total != 2 {
		t.Fatalf("counters must record BOTH suppressed + emitted events, got %+v", snap)
	}

	// Past the window → a second Warn emits.
	clk.advance(passthroughWarnWindow + time.Second)
	_ = doMessages(t, base, tok, "b", `{}`).Body.Close()
	if evs := ptEvents(t, buf.String(), "failover_proxy_passthrough"); len(evs) != 2 {
		t.Fatalf("a repeat past the window must emit a 2nd Warn, got %d", len(evs))
	}
	if snap := ps.PassthroughStatsSnapshot(); snap[0].Total != 3 {
		t.Fatalf("total after 3 events = %d, want 3", snap[0].Total)
	}
}

func TestProxy_passthrough_chatDisplayIDRedaction(t *testing.T) {
	var buf bytes.Buffer
	srv := newStatusUpstream(t, http.StatusInternalServerError, "boom")
	f := &fakeFailover{}
	ps, base := startProxy(t, f, srv.URL, zerolog.New(&buf))
	// A chat target is chat\x00<agentSessionID>\x00<fallbackAccountID>.
	tok := ps.TokenForChat("sess-1", "chat-77", "acct-secret")

	resp := doMessages(t, base, tok, "self", `{}`)
	_ = resp.Body.Close()

	logs := buf.String()
	evs := ptEvents(t, logs, "failover_proxy_passthrough")
	if len(evs) != 1 || evs[0]["session"] != "chat-77" {
		t.Fatalf("display id must be the agentSessionID only, got:\n%s", logs)
	}
	if strings.Contains(logs, "acct-secret") {
		t.Fatalf("fallback account id leaked into logs:\n%s", logs)
	}
	if strings.Contains(logs, "\x00") {
		t.Fatalf("raw NUL-delimited chat target leaked into logs:\n%s", logs)
	}
	if strings.Contains(logs, tok) {
		t.Fatalf("per-chat proxy token leaked into logs:\n%s", logs)
	}
}

func TestProxy_unknownToken_selfIdentifying(t *testing.T) {
	var buf bytes.Buffer
	_, srv := newFakeUpstream(t)
	ps, base := startProxy(t, &fakeFailover{}, srv.URL, zerolog.New(&buf))
	_ = ps // no token registered → any token is unknown

	const bogus = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	resp := doMessages(t, base, bogus, "b", `{}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if string(body) != unknownTokenBody+"\n" {
		t.Fatalf("body = %q, want the self-identifying constant + newline", body)
	}
	evs := ptEvents(t, buf.String(), "failover_proxy_unknown_token")
	if len(evs) != 1 {
		t.Fatalf("unknown_token events = %d, want 1:\n%s", len(evs), buf.String())
	}
	e := evs[0]
	if e["token_fingerprint"] != tokenFingerprint(bogus) {
		t.Fatalf("fingerprint = %v, want %s", e["token_fingerprint"], tokenFingerprint(bogus))
	}
	if e["path"] != "/v1/messages" {
		t.Fatalf("path = %v, want the upstream path only (never the token-bearing request path)", e["path"])
	}
	logs := buf.String()
	if strings.Contains(logs, bogus) {
		t.Fatalf("raw unknown token leaked into logs:\n%s", logs)
	}
	if strings.Contains(logs, "/s/"+bogus) {
		t.Fatalf("token-bearing request path leaked into logs:\n%s", logs)
	}
}

func TestProxy_unknownToken_warnRateLimited(t *testing.T) {
	var buf bytes.Buffer
	clk := newPTClock()
	_, srv := newFakeUpstream(t)
	ps, base := startProxyConfigured(t, &fakeFailover{}, srv.URL, zerolog.New(&buf), withClock(clk))
	_ = ps
	const bogus = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

	_ = doMessages(t, base, bogus, "b", `{}`).Body.Close()
	_ = doMessages(t, base, bogus, "b", `{}`).Body.Close()
	if evs := ptEvents(t, buf.String(), "failover_proxy_unknown_token"); len(evs) != 1 {
		t.Fatalf("repeated unknown-token rejections must collapse to 1 Warn, got %d", len(evs))
	}
}

func TestProxy_tokenlessProbe_noUnknownTokenWarn(t *testing.T) {
	var buf bytes.Buffer
	_, srv := newFakeUpstream(t)
	_, base := startProxy(t, &fakeFailover{}, srv.URL, zerolog.New(&buf))

	// A path with no /s/<token>/ prefix is a reachability probe → 404, not an
	// unknown-token rejection (which would over-count and mis-attribute).
	req, _ := http.NewRequest(http.MethodHead, base+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for tokenless probe", resp.StatusCode)
	}
	if evs := ptEvents(t, buf.String(), "failover_proxy_unknown_token"); len(evs) != 0 {
		t.Fatalf("tokenless probe must not log an unknown-token Warn, got %d", len(evs))
	}
}

func TestProxy_Deregister_forgetsStats(t *testing.T) {
	srv := newStatusUpstream(t, http.StatusInternalServerError, "boom")
	ps, base := startProxy(t, &fakeFailover{}, srv.URL, zerolog.Nop())
	tok := ps.TokenForSession("sess-1")

	_ = doMessages(t, base, tok, "b", `{}`).Body.Close()
	if len(ps.PassthroughStatsSnapshot()) != 1 {
		t.Fatalf("expected 1 tracked session before Deregister")
	}
	ps.Deregister("sess-1")
	if snap := ps.PassthroughStatsSnapshot(); len(snap) != 0 {
		t.Fatalf("Deregister must forget the session's pass-through counters, got %+v", snap)
	}
}

func TestProxy_passthroughWarn_mapHardBackstop(t *testing.T) {
	clk := newPTClock()
	srv := newStatusUpstream(t, http.StatusInternalServerError, "boom")
	ps, _ := startProxyConfigured(t, &fakeFailover{}, srv.URL, zerolog.Nop(), withClock(clk))
	// Flood the dedupe map with > cap DISTINCT keys inside a single window (clock
	// frozen), so the age-based sweep frees nothing and the wholesale-clear
	// backstop must fire to keep the map bounded.
	for i := range passthroughWarnMapCap + 50 {
		ps.allowPassthroughWarn(fmt.Sprintf("sess-%d", i), "500")
	}
	if got := len(ps.ptLogLast); got > passthroughWarnMapCap {
		t.Fatalf("dedupe map size = %d, want <= %d (hard backstop must bound it)", got, passthroughWarnMapCap)
	}
}

// --- passthroughStats unit tests (direct, package-internal) ---

func TestPassthroughStats_recordAndSnapshot(t *testing.T) {
	clk := newPTClock()
	s := newPassthroughStats(clk.now)
	s.record("sess-a", "500")
	s.record("sess-a", "500")
	s.record("sess-a", "429")
	s.record("sess-b", "500")

	snap := s.snapshot()
	if len(snap) != 2 {
		t.Fatalf("sessions = %d, want 2", len(snap))
	}
	// sess-a has the higher total (3) so it sorts first.
	if snap[0].DisplayID != "sess-a" || snap[0].Total != 3 {
		t.Fatalf("snap[0] = %+v, want sess-a total 3", snap[0])
	}
	// Classes sorted by count desc: 500(2) before 429(1).
	if len(snap[0].Classes) != 2 || snap[0].Classes[0].Class != "500" || snap[0].Classes[0].Count != 2 {
		t.Fatalf("classes = %+v, want 500=2 first", snap[0].Classes)
	}
	if snap[1].DisplayID != "sess-b" || snap[1].Total != 1 {
		t.Fatalf("snap[1] = %+v, want sess-b total 1", snap[1])
	}
}

func TestPassthroughStats_emptyDisplayIDIgnored(t *testing.T) {
	s := newPassthroughStats(newPTClock().now)
	s.record("", "500")
	if len(s.snapshot()) != 0 {
		t.Fatalf("empty display id must not be recorded")
	}
}

func TestPassthroughStats_classFoldsToOther(t *testing.T) {
	s := newPassthroughStats(newPTClock().now)
	for i := range passthroughStatsMaxClasses + 5 {
		s.record("sess-a", fmt.Sprintf("class-%d", i))
	}
	snap := s.snapshot()
	if len(snap) != 1 {
		t.Fatalf("sessions = %d, want 1", len(snap))
	}
	// The cap bounds distinct REAL classes to maxClasses; the "other" fold bucket
	// is one extra entry, so the hard ceiling is maxClasses+1.
	if len(snap[0].Classes) > passthroughStatsMaxClasses+1 {
		t.Fatalf("distinct classes = %d, want ≤ %d (excess folds to other)", len(snap[0].Classes), passthroughStatsMaxClasses+1)
	}
	var foundOther bool
	for _, c := range snap[0].Classes {
		if c.Class == passthroughStatsOtherClass {
			foundOther = true
		}
	}
	if !foundOther {
		t.Fatalf("excess classes must fold into %q: %+v", passthroughStatsOtherClass, snap[0].Classes)
	}
}

func TestPassthroughStats_sessionLRUEviction(t *testing.T) {
	clk := newPTClock()
	s := newPassthroughStats(clk.now)
	// Fill to the cap, each in its own minute so the LRU anchor is well-ordered.
	for i := range passthroughStatsMaxSessions {
		s.record(fmt.Sprintf("sess-%d", i), "500")
		clk.advance(time.Minute)
	}
	// One more distinct session evicts the least-recently-active (sess-0).
	s.record("sess-new", "500")
	for _, entry := range s.snapshot() {
		if entry.DisplayID == "sess-0" {
			t.Fatalf("least-recently-active session must have been LRU-evicted")
		}
	}
	if len(s.snapshot()) > passthroughStatsMaxSessions {
		t.Fatalf("session count exceeded the cap")
	}
}

func TestPassthroughStats_windowExpiry(t *testing.T) {
	clk := newPTClock()
	s := newPassthroughStats(clk.now)
	s.record("sess-a", "500")
	// Advance beyond the retention window; the old bucket must age out.
	clk.advance((passthroughStatsWindow + 1) * time.Minute)
	if snap := s.snapshot(); len(snap) != 0 {
		t.Fatalf("buckets older than the window must be omitted, got %+v", snap)
	}
}

// --- RepairDoctor informational check ---

type fakePTProvider struct{ snap []passthroughSessionSnapshot }

func (f fakePTProvider) PassthroughStatsSnapshot() []passthroughSessionSnapshot { return f.snap }

func TestPassthroughStatsCheck_alwaysOk(t *testing.T) {
	// nil provider (proxy unwired) → Ok, names the disabled state.
	c := passthroughStatsCheck(nil)
	if !c.Ok || !strings.Contains(c.Detail, "not wired") {
		t.Fatalf("nil provider check = %+v, want Ok + 'not wired'", c)
	}
	// empty snapshot → Ok, clean detail.
	c = passthroughStatsCheck(fakePTProvider{})
	if !c.Ok || !strings.Contains(c.Detail, "no upstream errors") {
		t.Fatalf("empty check = %+v, want Ok + clean detail", c)
	}
	// populated → STILL Ok (informational only), detail summarizes.
	c = passthroughStatsCheck(fakePTProvider{snap: []passthroughSessionSnapshot{
		{DisplayID: "sess-a", Total: 5, Classes: []passthroughClassCount{{Class: "500", Count: 5}}},
	}})
	if !c.Ok {
		t.Fatalf("a populated tally must NOT redden the doctor: %+v", c)
	}
	if !strings.Contains(c.Detail, "500=5") {
		t.Fatalf("detail must summarize the classes: %q", c.Detail)
	}
}
