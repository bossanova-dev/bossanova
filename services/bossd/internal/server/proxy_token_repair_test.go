package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/rotation"
)

// dropFromMemory deletes a token's in-memory resolution while leaving its
// durable row untouched. That is not an artificial state: it is exactly what a
// daemon restart produces (the map is process-local, the table is not), and
// what a Deregister racing a still-live pane produces. It is the state BOS-982
// exists to recover from, so the tests below have to be able to create it.
func dropFromMemory(ps *ProxyServer, token string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.hashToTarget, proxyTokenHash(token))
}

// newRepairProxy starts a serving proxy wired to both a durable token store and
// a failover seam, with a stub upstream that must never be reached by an
// unknown-token request.
func newRepairProxy(t *testing.T, f Failover, store db.ProxyTokenStore, buf *bytes.Buffer) (*ProxyServer, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unknown-token request must never reach the upstream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	return startProxyConfigured(t, f, upstream.URL, zerolog.New(buf), func(ps *ProxyServer) {
		ps.proxyTokens = store
	})
}

// settleRepairs joins every unknown-token pane repair the proxy has dispatched.
//
// Since BOS-982's async dispatch the repair runs OFF the handler — the 401 is
// written first — so an assertion made the instant the response lands races the
// repair goroutine. That would make a "want 1" assertion flaky and, worse, make
// every "want none" assertion vacuous: it would pass simply because the repair
// had not started yet. The job is registered before the response is written, so
// once the client has the 401 this join is exact rather than a poll.
func settleRepairs(t *testing.T, ps *ProxyServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ps.waitRepairJobs(ctx)
	if ctx.Err() != nil {
		t.Fatal("unknown-token repairs did not finish within 5s")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestUnknownToken_DurableRowRoutesToPaneRepair is the primary BOS-982
// attribution test: a chat token that is gone from the in-memory map but still
// present in the durable store is attributed back to its chat and routed to
// repair — while the client still receives the unchanged self-identifying 401.
func TestUnknownToken_DurableRowRoutesToPaneRepair(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairHandled: true}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	if tok == "" {
		t.Fatal("TokenForChat returned no token")
	}
	dropFromMemory(ps, tok)

	resp, err := httpDo(newSentinelReq(t, base, tok))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, unknownTokenBody) {
		t.Fatalf("401 body changed: %q", body)
	}

	settleRepairs(t, ps)
	repairs := f.repairs()
	if len(repairs) != 1 {
		t.Fatalf("repair dispatches = %d, want 1", len(repairs))
	}
	got := repairs[0]
	if got.sessionID != "sess-1" || got.agentSessionID != "agent-01" {
		t.Fatalf("repair attributed to %+v, want sess-1/agent-01", got)
	}
	if got.token != tok {
		t.Fatal("repair must receive the PRESENTED token so the pane's baked URL can be compared against it")
	}
	// The presented token and the durable row are linked by exactly one thing: the
	// digest attribution looks up under. Recomputing sha256 here rather than calling
	// proxyTokenHash keeps that link pinned to the hash the ROWS were written with —
	// a change to the helper moves the write and the read together, so nothing else
	// in this file would notice.
	wantDigest := hex.EncodeToString(func() []byte { sum := sha256.Sum256([]byte(tok)); return sum[:] }())
	gets := store.getsSnapshot()
	if len(gets) != 1 || gets[0] != wantDigest {
		t.Fatalf("durable lookups = %v, want exactly [sha256(presented token)] = [%s]", gets, wantDigest)
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare (account rotation) calls = %d, want 0 — a self-inflicted 401 must not rotate accounts", f.prepareCalls)
	}
}

// TestUnknownToken_RepairDoesNotHoldThe401 pins that the rejection is written
// without waiting for the repair (BOS-982).
//
// The repair is not a cheap in-memory hop. It reads the durable token row, makes
// two tmux subprocess calls against a named pane, and then dispatches into the
// rotator, whose shared reservation loads config from disk through an API that
// takes no context — so proxyTokenRepairTimeout never bounded the whole attempt
// and could not have kept a wedged repair off the response path. The pane that
// produced this 401 is mid-request and retrying; making it wait on the machinery
// that repairs it is exactly backwards.
//
// The seam below is wedged open for the whole assertion, so if the dispatch ever
// moves back onto the handler goroutine this test blocks until its client
// deadline instead of passing.
func TestUnknownToken_RepairDoesNotHoldThe401(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	entered := make(chan struct{}, 1)
	block := make(chan struct{})
	f := &fakeFailover{repairHandled: true, repairEntered: entered, repairBlock: block}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	dropFromMemory(ps, tok)

	req := newSentinelReq(t, base, tok)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the 401 did not come back while a repair was in flight: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, unknownTokenBody) {
		_ = resp.Body.Close()
		t.Fatalf("401 body changed: %q", body)
	}
	_ = resp.Body.Close()

	// The response is in hand; only now is the repair allowed to complete. Reaching
	// this at all is the assertion — a synchronous dispatch could not have got here.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the repair never reached the failover seam")
	}
	close(block)
	settleRepairs(t, ps)
	if got := f.repairs(); len(got) != 1 {
		t.Fatalf("repair dispatches = %d, want 1", len(got))
	}
}

// TestUnknownToken_UnattributableIsUnchanged pins the fail-closed cases. Each
// row must leave the 401 exactly as it was and dispatch nothing: this branch is
// reachable by any local process with an arbitrary /s/<token>, so "cannot
// attribute" has to mean "do nothing", never "guess".
func TestUnknownToken_UnattributableIsUnchanged(t *testing.T) {
	const liveTok = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	cases := []struct {
		name  string
		token string
		seed  func(store *fakeProxyTokenStore)
	}{
		{
			name:  "no durable row at all",
			token: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		},
		{
			name:  "token is not 64-hex (never one of ours)",
			token: "not-a-real-token",
		},
		{
			name:  "uppercase hex is not a minted token shape",
			token: strings.ToUpper(liveTok),
		},
		{
			name:  "durable row is session-shaped (no chat to respawn)",
			token: liveTok,
			seed: func(store *fakeProxyTokenStore) {
				_ = store.Upsert(context.Background(), db.ProxyTokenRecord{
					TokenSHA256: proxyTokenHash(liveTok), SessionID: "sess-1",
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			store := newFakeProxyTokenStore()
			if tc.seed != nil {
				tc.seed(store)
			}
			f := &fakeFailover{repairHandled: true}
			ps, base := newRepairProxy(t, f, store, &buf)

			resp, err := httpDo(newSentinelReq(t, base, tc.token))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if body := readBody(t, resp); !strings.Contains(body, unknownTokenBody) {
				t.Fatalf("401 body changed: %q", body)
			}
			settleRepairs(t, ps)
			if got := f.repairs(); len(got) != 0 {
				t.Fatalf("repair dispatches = %+v, want none", got)
			}
			if f.prepareCalls != 0 {
				t.Fatalf("prepare calls = %d, want 0", f.prepareCalls)
			}
		})
	}
}

// TestUnknownToken_RepairSharesTheWarnRateLimit pins the rate-limit contract. A
// wedged pane retries every 15s and an attacker can replay one token freely, so
// the repair attempt must be charged by the SAME per-fingerprint limiter that
// already collapses the warn — one attempt per fingerprint per minute, not one
// per request.
func TestUnknownToken_RepairSharesTheWarnRateLimit(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairHandled: true}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	dropFromMemory(ps, tok)

	for i := 0; i < 5; i++ {
		resp, err := httpDo(newSentinelReq(t, base, tok))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			_ = resp.Body.Close()
			t.Fatalf("request %d status = %d, want 401", i, resp.StatusCode)
		}
		_ = readBody(t, resp)
		// Closed per iteration rather than deferred: a deferred close inside the
		// loop would hold all five connections open until the test returns.
		_ = resp.Body.Close()
	}

	settleRepairs(t, ps)
	if got := f.repairs(); len(got) != 1 {
		t.Fatalf("repair dispatches = %d across 5 identical requests, want 1", len(got))
	}
	if n := strings.Count(buf.String(), "failover_proxy_unknown_token"); n != 1 {
		t.Fatalf("unknown-token warns = %d, want 1", n)
	}
}

// TestUnknownToken_StoreErrorIsFailSafe pins that a durable-read failure leaves
// the 401 unchanged and dispatches nothing, rather than surfacing as a 5xx or a
// guessed attribution.
func TestUnknownToken_StoreErrorIsFailSafe(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	store.getErr = fmt.Errorf("database is locked")
	f := &fakeFailover{repairHandled: true}
	ps, base := newRepairProxy(t, f, store, &buf)

	const tok = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	resp, err := httpDo(newSentinelReq(t, base, tok))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, unknownTokenBody) {
		t.Fatalf("401 body changed: %q", body)
	}
	settleRepairs(t, ps)
	if got := f.repairs(); len(got) != 0 {
		t.Fatalf("repair dispatches = %+v, want none", got)
	}
}

// TestUnknownToken_RepairErrorIsFailSafe pins the OTHER failure mode of an
// attributable token: the row resolves and the repair is dispatched, but the
// Lifecycle seam itself errors (a sessions.Get or agentChats lookup failure that
// is not ErrAgentChatNotFound). That is the only path on which a real error from
// RepairProxyPane surfaces to the proxy, and it must stay fail-safe in both
// directions — the 401 body is unchanged, and a failed repair must NOT fall back
// to account rotation. The 401 was minted by this proxy, so the account behind it
// is not implicated no matter how the repair ends. (BOS-982)
func TestUnknownToken_RepairErrorIsFailSafe(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairErr: errors.New("tmux: no such pane")}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	if tok == "" {
		t.Fatal("TokenForChat returned no token")
	}
	dropFromMemory(ps, tok)

	resp, err := httpDo(newSentinelReq(t, base, tok))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, unknownTokenBody) {
		t.Fatalf("401 body changed: %q", body)
	}

	settleRepairs(t, ps)
	// Exactly one dispatch. The log assertions below already prove the seam RAN —
	// proxy_server.go emits "pane repair for an unknown path token failed" only when
	// RepairProxyPane returned a non-nil error — but not that it ran ONCE; a retry
	// loop behind the rate limiter would log the same line. A zero here means the
	// opposite: the token never reached the seam at all.
	if got := f.repairs(); len(got) != 1 {
		t.Fatalf("repair dispatches = %d, want exactly 1 — 0 means the seam was never reached, >1 that the failed repair was retried", len(got))
	}
	if f.prepareCalls != 0 {
		t.Fatalf("prepare (account rotation) calls = %d, want 0 — a failed pane repair must not fall back to rotating accounts", f.prepareCalls)
	}
	// The failure has to be greppable and distinguishable from the durable-read
	// failure above, which is the other branch that gives up on the same request.
	logged := buf.String()
	if !strings.Contains(logged, "pane repair for an unknown path token failed") {
		t.Fatalf("failed repair was not logged: %q", logged)
	}
	if !strings.Contains(logged, "tmux: no such pane") {
		t.Fatalf("the seam's error was dropped from the log: %q", logged)
	}
	// Same secret discipline as every other unknown-token line: fingerprint only.
	if strings.Contains(logged, tok) {
		t.Fatal("the log leaked the token bytes")
	}
}

// TestUpstream401StillRotatesAccounts is the bypass guard. The probe-skipping
// repair path must be reachable ONLY for a 401 the proxy minted itself; a 401
// that came back from the UPSTREAM is a real credential failure and must keep
// taking the account-rotation path.
func TestUpstream401StillRotatesAccounts(t *testing.T) {
	var buf bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid bearer"}`))
	}))
	t.Cleanup(upstream.Close)

	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairHandled: true, currentBearer: "first-token"}
	ps, err := NewProxyServer(ProxyServerConfig{Failover: f, Logger: zerolog.New(&buf), Upstream: upstream.URL, ProxyTokens: store})
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

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	resp, err := httpDo(newSentinelReq(t, base, tok))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_ = readBody(t, resp)

	if f.prepareCalls == 0 {
		t.Fatal("an UPSTREAM 401 must still consult the account-rotation path")
	}
	settleRepairs(t, ps)
	if got := f.repairs(); len(got) != 0 {
		t.Fatalf("an upstream 401 must never reach pane repair, got %+v", got)
	}
}

// rotatorRepairSeam is the production wiring in miniature: the proxy's Failover
// seam forwarding an attributed unknown-token 401 into a REAL ChatRotator. It
// stands in for session.Lifecycle, whose own attribution checks are covered in
// the session package; what this seam exists to prove is that the two halves
// meet — the proxy's 401 reaches the rotator, and the rotator respawns.
type rotatorRepairSeam struct {
	fakeFailover
	rotator *rotation.ChatRotator
}

func (s *rotatorRepairSeam) RepairProxyPane(ctx context.Context, sessionID, agentSessionID, token string) (bool, error) {
	if _, err := s.fakeFailover.RepairProxyPane(ctx, sessionID, agentSessionID, token); err != nil {
		return false, err
	}
	s.rotator.OnProxyTokenUnresolved(agentSessionID)
	return true, nil
}

// TestUnknownToken_EndToEndRespawnsWithoutProbe is the BOS-982 required proof.
//
// It registers a chat token, drops it from the in-memory map while leaving the
// durable row intact — the exact shape a daemon restart leaves behind — issues a
// real HTTP request on that token, and asserts the pane is respawned in place on
// its own account with ZERO account probes.
//
// On main this fails at the first assertion: the request 401s and nothing at all
// reaches the rotator.
func TestUnknownToken_EndToEndRespawnsWithoutProbe(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	seam := &rotatorRepairSeam{}

	switched := make(chan rotation.SwitchRequest, 4)
	probes := make(chan struct{}, 4)
	seam.rotator = rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(context.Context, string) (rotation.ChatContext, error) {
			return rotation.ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-1"}, nil
		},
		AuthProbe: func(context.Context, string) rotation.AuthProbeResult {
			probes <- struct{}{}
			return rotation.AuthProbeHealthy
		},
		Switch: func(_ context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			switched <- req
			return rotation.SwitchResult{}, nil
		},
		Now: time.Now,
	})

	ps, base := newRepairProxy(t, seam, store, &buf)
	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	dropFromMemory(ps, tok)

	resp, err := httpDo(newSentinelReq(t, base, tok))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	_ = readBody(t, resp)

	select {
	case req := <-switched:
		if !req.RespawnSameAccount {
			t.Fatal("repair must respawn in place (RespawnSameAccount=true), not rotate accounts")
		}
		if req.AccountID != "acct-1" {
			t.Fatalf("respawn bound account = %q, want the pane's currently bound account", req.AccountID)
		}
		if req.AgentSessionID != "agent-01" {
			t.Fatalf("respawn addressed %q, want agent-01", req.AgentSessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy-minted 401 never reached respawn-in-place")
	}

	select {
	case <-probes:
		t.Fatal("a proxy-minted 401 must never probe the account — the account was never consulted for it")
	default:
	}
}

// panicOnWriteRecorder is a ResponseWriter whose body write panics with
// http.ErrAbortHandler — the real shape a wrapping ResponseWriter or a
// middleware takes when it aborts a response mid-write. It exists to prove the
// unknown-token repair's WaitGroup Done is not conditional on control reaching a
// statement below the 401 write.
type panicOnWriteRecorder struct{ hdr http.Header }

func (p *panicOnWriteRecorder) Header() http.Header {
	if p.hdr == nil {
		p.hdr = http.Header{}
	}
	return p.hdr
}
func (p *panicOnWriteRecorder) WriteHeader(int) {}
func (p *panicOnWriteRecorder) Write([]byte) (int, error) {
	panic(http.ErrAbortHandler)
}

// TestUnknownTokenRepairReleasesItsJobWhenTheWritePanics pins the Add/Done
// pairing that Shutdown's drain depends on.
//
// beginUnknownTokenRepair takes repairJobs.Add(1) BEFORE the 401 is written so
// an observer of the response can never race the registration. That split makes
// the matching Done non-local: it lives in a closure the handler must still run
// after the write. If the write path panics — http.ErrAbortHandler from a
// wrapping ResponseWriter is the ordinary case — and the closure is merely
// called rather than deferred, the counter is stranded for the life of the
// process and EVERY later Shutdown burns its whole drain budget in
// waitRepairJobs waiting on a repair that will never run.
func TestUnknownTokenRepairReleasesItsJobWhenTheWritePanics(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairHandled: true}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	if tok == "" {
		t.Fatal("TokenForChat returned no token")
	}
	dropFromMemory(ps, tok)

	req := newSentinelReq(t, base, tok)
	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatal("the wired ResponseWriter did not panic; this test no longer exercises the abort path")
			}
		}()
		ps.handleProxy(&panicOnWriteRecorder{}, req)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ps.waitRepairJobs(ctx)
	if ctx.Err() != nil {
		t.Fatal("repairJobs never drained after a panicking 401 write: the Add is stranded, so every later Shutdown burns its full drain budget")
	}
}

// TestUnknownTokenRepairNotRegisteredOnceShutdownBegan pins the gate that keeps
// repairJobs.Add away from a live repairJobs.Wait.
//
// http.Server.Shutdown does NOT stop in-flight handlers when its ctx expires: it
// returns ctx.Err() and leaves them running, while waitRepairJobs' inner
// goroutine stays blocked in Wait() after its own ctx arm has fired. A surviving
// handler reaching the unknown-token branch would then Add from a possibly-zero
// counter concurrently with that live Wait — the documented sync.WaitGroup
// misuse. Registering nothing once Shutdown has begun is what removes the race;
// the pane's next unresolved-token 401 reaches the NEXT daemon, which is the one
// that can finish a repair anyway.
func TestUnknownTokenRepairNotRegisteredOnceShutdownBegan(t *testing.T) {
	var buf bytes.Buffer
	store := newFakeProxyTokenStore()
	f := &fakeFailover{repairHandled: true}
	ps, base := newRepairProxy(t, f, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
	if tok == "" {
		t.Fatal("TokenForChat returned no token")
	}
	dropFromMemory(ps, tok)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ps.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// A handler that outlived the drain still runs to completion; it just must not
	// register a repair. The 401 is written exactly as before.
	rec := httptest.NewRecorder()
	ps.handleProxy(rec, newSentinelReq(t, base, tok))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — the rejection is unchanged by the shutdown gate", rec.Code)
	}
	settleRepairs(t, ps)
	if calls := f.repairs(); len(calls) != 0 {
		t.Fatalf("repair calls = %d, want 0 — no repair may be registered once Shutdown has begun", len(calls))
	}
}

// TestUnknownTokenRepairRegistrationIsAtomicWithShutdown pins that the
// registration gate and the WaitGroup Add are ONE step against Shutdown.
//
// Reading the closing flag and then calling repairJobs.Add(1) is not atomic
// however the flag is stored: a registration that observed "still open" can be
// preempted between the two, and in that gap Shutdown can set the flag AND its
// repairJobs.Wait can return. The Add then lands on a WaitGroup whose Wait has
// already returned — the documented sync.WaitGroup misuse — and, worse than any
// diagnostic, it lets a repair goroutine outlive the shutdown that was supposed
// to have joined it.
//
// The assertion is that invariant stated directly: the set of repairs that have
// run when Shutdown RETURNS must be the final set. A repair appearing after that
// point is one Shutdown failed to join. Probabilistic in triggering, exact in
// meaning — and it is the interleaving the race detector reports on too, so this
// test earns its keep under -race.
func TestUnknownTokenRepairRegistrationIsAtomicWithShutdown(t *testing.T) {
	const iterations = 40
	const registrars = 4
	for i := 0; i < iterations; i++ {
		store := newFakeProxyTokenStore()
		f := &fakeFailover{repairHandled: true}
		ps, err := NewProxyServer(ProxyServerConfig{Failover: f, Logger: zerolog.Nop(), ProxyTokens: store})
		if err != nil {
			t.Fatalf("NewProxyServer: %v", err)
		}
		tok := ps.TokenForChat("sess-1", "agent-01", "acct-1")
		if tok == "" {
			t.Fatal("TokenForChat returned no token")
		}
		dropFromMemory(ps, tok)

		var wg sync.WaitGroup
		atShutdown := make(chan int, 1)
		wg.Add(registrars + 1)
		for r := 0; r < registrars; r++ {
			go func() {
				defer wg.Done()
				// Exactly the handler's shape: register, then run the (always deferred)
				// closure that dispatches the repair.
				run := ps.beginUnknownTokenRepair(tok)
				run()
			}()
		}
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := ps.Shutdown(ctx); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			atShutdown <- len(f.repairs())
		}()
		wg.Wait()

		// Join anything still outstanding, so a repair that escaped the shutdown is
		// counted rather than raced.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ps.waitRepairJobs(ctx)
		cancel()

		joined := <-atShutdown
		if final := len(f.repairs()); final != joined {
			t.Fatalf("iteration %d: %d repairs had run when Shutdown returned but %d ran in total — a repair was registered after Wait had already returned, so Shutdown did not join it", i, joined, final)
		}
	}
}
