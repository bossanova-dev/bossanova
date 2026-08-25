package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/session"
)

// newSentinelReq builds the interactive REPL's managed request shape (only the
// BOS-326 sentinel x-api-key, no Authorization) against the proxy at base for
// token tok.
func newSentinelReq(t *testing.T, base, tok string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/s/"+tok+"/v1/messages", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("x-api-key", session.SentinelAPIKey)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func httpDo(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}

// newAdoptProxy builds an un-listened ProxyServer whose logs land in buf, for
// the token-registry unit tests (AdoptToken / AdoptTokenForChat need no live
// socket — they only mutate the in-memory maps).
func newAdoptProxy(t *testing.T, buf *bytes.Buffer) *ProxyServer {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.New(buf)})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	return ps
}

// TestAdoptToken_registersExistingSessionToken proves the core BOS-481 move: a
// token minted by an old daemon (here just a fixed 64-hex string) is re-adopted
// onto a fresh ProxyServer WITHOUT minting, and immediately resolves.
func TestAdoptToken_registersExistingSessionToken(t *testing.T) {
	var buf bytes.Buffer
	ps := newAdoptProxy(t, &buf)
	const tok = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	ps.AdoptToken(tok, "sess-1")

	sid, ok := ps.sessionForToken(tok)
	if !ok || sid != "sess-1" {
		t.Fatalf("sessionForToken = %q,%v want sess-1,true", sid, ok)
	}
	if buf.Len() != 0 {
		t.Fatalf("clean adoption logged unexpectedly: %s", buf.String())
	}
}

func TestAdoptToken_idempotentAndConflictSafe(t *testing.T) {
	const tokA = "1111111111111111111111111111111111111111111111111111111111111111"
	const tokB = "2222222222222222222222222222222222222222222222222222222222222222"

	t.Run("same token same session is a no-op", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		ps.AdoptToken(tokA, "sess-1")
		ps.AdoptToken(tokA, "sess-1") // sibling / re-sweep
		if sid, ok := ps.sessionForToken(tokA); !ok || sid != "sess-1" {
			t.Fatalf("sessionForToken = %q,%v want sess-1,true", sid, ok)
		}
		if buf.Len() != 0 {
			t.Fatalf("no-op re-adopt logged: %s", buf.String())
		}
	})

	t.Run("session already holds a fresh-spawn token: spawn wins silently", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		fresh := ps.TokenForSession("sess-1") // a live spawn minted this
		ps.AdoptToken(tokA, "sess-1")         // stale pane's old token
		// The fresh token still routes; the old one was NOT registered.
		if sid, ok := ps.sessionForToken(fresh); !ok || sid != "sess-1" {
			t.Fatalf("fresh token lost: %q,%v", sid, ok)
		}
		if _, ok := ps.sessionForToken(tokA); ok {
			t.Fatalf("stale token registered over a live spawn; spawn must win")
		}
		if buf.Len() != 0 {
			t.Fatalf("spawn-wins path must be silent, logged: %s", buf.String())
		}
	})

	t.Run("token already registered to a different target: keep first + one Warn", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		ps.AdoptToken(tokA, "sess-1")
		ps.AdoptToken(tokA, "sess-2") // same token, different session
		if sid, ok := ps.sessionForToken(tokA); !ok || sid != "sess-1" {
			t.Fatalf("first registration not kept: %q,%v", sid, ok)
		}
		logged := buf.String()
		if !strings.Contains(logged, "keeping first registration") {
			t.Fatalf("expected a conflict Warn, got: %s", logged)
		}
		if strings.Contains(logged, tokA) {
			t.Fatalf("FULL token leaked into logs: %s", logged)
		}
		if !strings.Contains(logged, tokenPrefix(tokA)) {
			t.Fatalf("expected an <=8-hex token prefix in the log: %s", logged)
		}
	})

	t.Run("empty inputs are ignored", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		ps.AdoptToken("", "sess-1")
		ps.AdoptToken(tokB, "")
		if len(ps.hashToTarget) != 0 {
			t.Fatalf("empty inputs registered something: %v", ps.hashToTarget)
		}
	})
}

func TestAdoptTokenForChat_registersAndCleansUp(t *testing.T) {
	var buf bytes.Buffer
	ps := newAdoptProxy(t, &buf)
	const tok = "3333333333333333333333333333333333333333333333333333333333333333"

	ps.AdoptTokenForChat("sess-1", "chat-1", "acct-chat", tok)

	// Resolves to the CHAT target shape (not the bare session id).
	sid, ok := ps.sessionForToken(tok)
	want := session.ProxyTargetForChat("chat-1", "acct-chat")
	if !ok || sid != want {
		t.Fatalf("sessionForToken = %q,%v want %q,true", sid, ok, want)
	}
	// Bookkeeping is filled so Deregister tears the chat token down.
	ps.Deregister("sess-1")
	if _, ok := ps.sessionForToken(tok); ok {
		t.Fatalf("Deregister did not drop an adopted chat token; sessionHashes bookkeeping missing")
	}
	if buf.Len() != 0 {
		t.Fatalf("clean chat adoption logged: %s", buf.String())
	}
}

func TestAdoptTokenForChat_conflictSafe(t *testing.T) {
	const tok = "4444444444444444444444444444444444444444444444444444444444444444"

	t.Run("chat already holds a fresh token: spawn wins silently", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		fresh := ps.TokenForChat("sess-1", "chat-1", "acct-chat")
		ps.AdoptTokenForChat("sess-1", "chat-1", "acct-chat", tok)
		if _, ok := ps.sessionForToken(tok); ok {
			t.Fatalf("stale chat token registered over a live spawn")
		}
		if sid, ok := ps.sessionForToken(fresh); !ok || sid != session.ProxyTargetForChat("chat-1", "acct-chat") {
			t.Fatalf("fresh chat token lost: %q,%v", sid, ok)
		}
		if buf.Len() != 0 {
			t.Fatalf("spawn-wins must be silent: %s", buf.String())
		}
	})

	t.Run("same token different chat target: keep first + one Warn", func(t *testing.T) {
		var buf bytes.Buffer
		ps := newAdoptProxy(t, &buf)
		ps.AdoptTokenForChat("sess-1", "chat-1", "acct-a", tok)
		ps.AdoptTokenForChat("sess-1", "chat-2", "acct-b", tok)
		if sid, _ := ps.sessionForToken(tok); sid != session.ProxyTargetForChat("chat-1", "acct-a") {
			t.Fatalf("first chat registration not kept: %q", sid)
		}
		logged := buf.String()
		if !strings.Contains(logged, "keeping first registration") {
			t.Fatalf("expected a conflict Warn, got: %s", logged)
		}
		if strings.Contains(logged, tok) {
			t.Fatalf("FULL token leaked into logs: %s", logged)
		}
	})
}

// --- T3: restart integration from a REAL spawn shape ---
//
// Each case mints a token on proxy A exactly as a live spawn would, then adopts
// that SAME token onto a fresh proxy B (the post-restart daemon) and drives a
// real sentinel request through B. On main (no Adopt* methods) B would 401 the
// unknown token; here the pane reconnects with its own frozen token.

func TestProxy_adoptedSessionTokenSurvivesRestart(t *testing.T) {
	up, srv := newFakeUpstream(t)

	// Proxy A: a live spawn mints the session token.
	psA, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Upstream: srv.URL})
	if err != nil {
		t.Fatalf("NewProxyServer A: %v", err)
	}
	tok := psA.TokenForSession("sess-1")

	// Proxy B: fresh daemon after restart re-adopts the surviving pane's token.
	fB := &fakeFailover{currentBearer: "second-token"}
	psB, base := startProxy(t, fB, srv.URL, zerolog.Nop())
	psB.AdoptToken(tok, "sess-1")

	req := newSentinelReq(t, base, tok)
	resp, err := httpDo(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "pong" {
		t.Fatalf("adopted pane did not reconnect: status=%d body=%q", resp.StatusCode, body)
	}
	if fB.currentBearerID != "sess-1" {
		t.Fatalf("session-shape target not resolved: CurrentBearer id=%q want sess-1", fB.currentBearerID)
	}
	if len(up.all()) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(up.all()))
	}
}

func TestProxy_adoptedChatTokenSurvivesRestart(t *testing.T) {
	up, srv := newFakeUpstream(t)

	psA, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Upstream: srv.URL})
	if err != nil {
		t.Fatalf("NewProxyServer A: %v", err)
	}
	tok := psA.TokenForChat("sess-1", "chat-1", "acct-chat")

	fB := &fakeFailover{currentBearer: "second-token"}
	psB, base := startProxy(t, fB, srv.URL, zerolog.Nop())
	psB.AdoptTokenForChat("sess-1", "chat-1", "acct-chat", tok)

	req := newSentinelReq(t, base, tok)
	resp, err := httpDo(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "pong" {
		t.Fatalf("adopted chat pane did not reconnect: status=%d body=%q", resp.StatusCode, body)
	}
	if want := session.ProxyTargetForChat("chat-1", "acct-chat"); fB.currentBearerID != want {
		t.Fatalf("chat-shape target not resolved: CurrentBearer id=%q want %q", fB.currentBearerID, want)
	}
	_ = up
}

// TestProxy_adoptedSharedSessionTokenServesSiblings proves the shared-token
// spawn shape: two sibling same-agent chats bake ONE session token, so a single
// AdoptToken re-arms both panes.
func TestProxy_adoptedSharedSessionTokenServesSiblings(t *testing.T) {
	_, srv := newFakeUpstream(t)

	psA, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.Nop(), Upstream: srv.URL})
	if err != nil {
		t.Fatalf("NewProxyServer A: %v", err)
	}
	tok := psA.TokenForSession("sess-1") // both siblings share this

	fB := &fakeFailover{currentBearer: "second-token"}
	psB, base := startProxy(t, fB, srv.URL, zerolog.Nop())
	psB.AdoptToken(tok, "sess-1")
	psB.AdoptToken(tok, "sess-1") // second sibling's sweep row: no-op

	for i := 0; i < 2; i++ {
		req := newSentinelReq(t, base, tok)
		resp, err := httpDo(req)
		if err != nil {
			t.Fatalf("do %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || string(body) != "pong" {
			t.Fatalf("sibling %d did not reconnect: status=%d body=%q", i, resp.StatusCode, body)
		}
	}
	// Exactly one registry entry backs both siblings.
	if got := len(psB.hashToTarget); got != 1 {
		t.Fatalf("shared session token produced %d registry entries, want 1", got)
	}
}
