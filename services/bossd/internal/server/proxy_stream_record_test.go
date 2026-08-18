package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/inflight"
	"github.com/recurser/bossd/internal/resume"
)

// recordedStreamIDs reads the durable in-flight stream record the way a freshly
// started daemon would — by parsing the file the previous process left on disk.
// A missing file means nothing was in flight.
func recordedStreamIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read stream record: %v", err)
	}
	var rec struct {
		AgentSessionIDs []string `json:"agent_session_ids"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse stream record: %v", err)
	}
	return rec.AgentSessionIDs
}

// TestHardKilledStreamProducesExactlyOneResumePromptAfterRestart is the BOS-890
// cross-component gate (acceptance criterion 7): a daemon that dies with a
// proxied stream in flight leads to EXACTLY ONE resume prompt after it comes
// back, and that prompt goes through the full unmodified gate ladder.
//
// It is deliberately in-process rather than a real `kill -9` of a daemon:
// spawning, killing and re-spawning a full bossd would be slow and flaky, and
// would prove nothing this does not. Every piece that matters is REAL — a real
// ProxyServer serving a real HTTP request against an httptest upstream, the
// real on-disk record written by the real Recorder, the real startup
// read-and-clear, and a real TransientResumer running its real gates. Only two
// things are simulated, and both in the direction that makes the test harder:
//
//   - The kill is simulated by simply never calling Shutdown, which is exactly
//     what a SIGKILL does — no drain, no seal, no cleanup, whatever the last
//     write left on disk is what the next process finds.
//   - The critical assertion is taken while the stream is genuinely still open,
//     which is the only instant a hard kill could actually land and so the only
//     instant at which the record's contents prove anything. That instant is
//     PINNED rather than raced: the upstream handler blocks on a channel this
//     test owns, and the client is held off closing its response body until the
//     test says so, so the proxy handler provably cannot reach its deferred
//     Leave while the assertion reads the file (see the comment at the
//     assertion for the full argument).
func TestHardKilledStreamProducesExactlyOneResumePromptAfterRestart(t *testing.T) {
	const (
		sessionID      = "sess-890"
		agentSessionID = "chat-890"
		accountID      = "acct-890"
		token          = "aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff00000000111111112"
	)
	recordPath := inflight.Path(t.TempDir())

	// --- daemon #1: serving a stream when it dies -------------------------

	inFlight := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	// The client's disconnect is a SECOND way the handler can end, and it is not
	// ordered against the upstream one: forward builds the upstream request with
	// http.NewRequestWithContext(r.Context(), …), so closing the response body
	// cancels the upstream leg, unwinds the handler and fires its deferred
	// streams.Leave — potentially before the assertion below has read the file.
	// Gating the close on a channel the test controls removes that second exit
	// entirely for the duration of the assertion.
	clientRelease := make(chan struct{})
	var clientOnce sync.Once
	releaseClient := func() { clientOnce.Do(func() { close(clientRelease) }) }
	t.Cleanup(releaseClient)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Headers are out and the body is open: from here until release the
		// agent is mid-response, which is the state a hard kill freezes.
		close(inFlight)
		<-release
	}))
	t.Cleanup(upstream.Close)

	recorder := inflight.NewRecorder(recordPath, zerolog.Nop())
	proxy, err := NewProxyServer(ProxyServerConfig{
		Failover: &fakeFailover{},
		Logger:   zerolog.Nop(),
		Upstream: upstream.URL,
		Streams:  recorder,
	})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := proxy.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- proxy.Serve() }()
	// No Shutdown in cleanup: this daemon is the one that gets hard-killed, so
	// nothing it would have done on the way out may run. The listener is closed
	// only to release the port once the test is over.
	t.Cleanup(func() {
		releaseUpstream()
		releaseClient()
		if proxy.listener != nil {
			_ = proxy.listener.Close()
		}
		<-serveErr
	})

	// Register the pane's chat token exactly as a spawn (or the BOS-481 startup
	// adoption sweep) would, so the proxy resolves the request to this chat.
	proxy.AdoptTokenForChat(sessionID, agentSessionID, accountID, token)

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req, rerr := http.NewRequest(http.MethodPost,
			fmt.Sprintf("http://127.0.0.1:%d/s/%s/v1/messages", proxy.Port(), token), nil)
		if rerr != nil {
			return
		}
		// A client-supplied bearer keeps the proxy off the failover-bearer path;
		// this test is about the record, not about auth resolution.
		req.Header.Set("Authorization", "Bearer client-token")
		resp, derr := http.DefaultClient.Do(req)
		// Hold the connection open until the test releases it. Closing here would
		// cancel the handler's request context and race the record assertion.
		<-clientRelease
		if derr == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never received the proxied request")
	}

	// (1) THE assertion for acceptance criterion 1: with the stream genuinely
	// open, the record on disk already names this chat. Everything downstream
	// depends on this being true at kill time rather than at shutdown time,
	// because a hard kill runs no shutdown code at all.
	//
	// This is ORDERED, not raced. The close of inFlight happens-before this read
	// and happens-after the handler's streams.Enter (Enter runs before forward,
	// and Enter persists synchronously before returning), so the write is
	// guaranteed visible. In the other direction the handler has exactly two
	// exits and both are blocked: the upstream leg is parked on <-release, and
	// the client leg is parked on <-clientRelease — neither channel is closed
	// until after the recovery below. So no goroutine can be in streams.Leave
	// while this line runs; a failure here is the recorder, never timing.
	if got := recordedStreamIDs(t, recordPath); len(got) != 1 || got[0] != agentSessionID {
		t.Fatalf("in-flight stream record = %v, want [%s] while the stream is open", got, agentSessionID)
	}

	// --- the kill --------------------------------------------------------
	//
	// Nothing happens here, and that is the point: no Shutdown, no drain, no
	// seal. The file above is byte-for-byte what the next daemon inherits.

	// --- daemon #2: startup recovery --------------------------------------

	severed, err := inflight.ReadAndClear(recordPath)
	if err != nil {
		t.Fatalf("startup ReadAndClear: %v", err)
	}
	if len(severed) != 1 || severed[0] != agentSessionID {
		t.Fatalf("startup recovered %v, want [%s]", severed, agentSessionID)
	}
	if _, statErr := os.Stat(recordPath); !os.IsNotExist(statErr) {
		t.Fatalf("startup did not clear the record (stat err = %v)", statErr)
	}
	// The dead process's handler can unwind now; the record is already claimed,
	// so nothing it does can affect the recovery below. Releasing the upstream
	// first and the client second means the ordinary completion path runs, with
	// the client disconnect landing on an already-finished stream.
	releaseUpstream()
	releaseClient()
	<-reqDone

	// The other half of the Enter/Leave pair, now that it is safe to observe:
	// the handler's deferred Leave must actually drop the chat. Without this a
	// recorder that only ever added would satisfy the assertion above forever
	// and turn every graceful restart into a resume prompt. <-reqDone only
	// proves the CLIENT is done, so the handler's unwind is awaited here rather
	// than assumed; a handler that never leaves fails on the deadline.
	leaveDeadline := time.Now().Add(5 * time.Second)
	for len(recorder.Snapshot()) != 0 {
		if time.Now().After(leaveDeadline) {
			t.Fatalf("finished stream still recorded in flight: %v", recorder.Snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}

	var mu sync.Mutex
	var delivered []string
	// The surviving pane as the new daemon's status poller seeds it on a cold
	// start: idle, with a last-output stamp from BEFORE the restart. No transient
	// banner is set, and that is the whole difficulty — the stream died without
	// rendering one, so the restart trigger is the only evidence that exists.
	//
	// seededLastOutput never moves. The poller's bootstrap value is a synthetic
	// placeholder, not an observation, and leaving it fixed means the idle gate
	// stays permanently satisfied — so the ONLY thing standing between this chat
	// and a second prompt is the liveness predicate below, which is exactly the
	// property this test is here to pin.
	seededLastOutput := time.Now().Add(-time.Hour)
	// The spinner-insensitive liveness reading for the same pane. It starts
	// SEEDED: the poller restored the pane but has observed no substantive output
	// from it yet, which is precisely the state a chat left parked by a severed
	// stream is in.
	substantiveAt := time.Time{}
	livenessSeeded := true
	resumer := resume.NewTransientResumer(resume.TransientResumerDeps{
		Logger:     zerolog.Nop(),
		MarkerSet:  func(string) bool { return false },
		AuthFailed: func(string) bool { return false },
		ChatState: func(id string) (bossanovav1.ChatStatus, time.Time, bool) {
			if id != agentSessionID {
				return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED, time.Time{}, false
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_IDLE, seededLastOutput, true
		},
		ChatLiveness: func(id string) (time.Time, bool, bool) {
			if id != agentSessionID {
				return time.Time{}, false, false
			}
			mu.Lock()
			defer mu.Unlock()
			return substantiveAt, livenessSeeded, true
		},
		SessionResumable: func(context.Context, string) bool { return true },
		Deliver: func(_ context.Context, _, message string) error {
			mu.Lock()
			delivered = append(delivered, message)
			// A prompt that lands IS substantive pane output: it is typed into the
			// pane, the agent answers it, and the poller's next tick records a real
			// (no longer seeded) liveness stamp after the severance. Modelling that
			// is not a convenience — it is the feedback that ends the cycle in
			// production, and it is what the "exactly one" assertion below actually
			// gates. A pane that stayed silent (a delivery that never landed) would
			// correctly be prompted again, which is what makes the assertion able to
			// fail rather than merely able to pass.
			substantiveAt = time.Now()
			livenessSeeded = false
			mu.Unlock()
			return nil
		},
		// BOTH scheduling seams are compressed, so the quiet period below spans the
		// whole remaining attempt ladder in milliseconds rather than the ~13 minutes
		// the production constants would need. Leaving BackoffBase at its 1m default
		// would make that assertion unfalsifiable for every bug except one that
		// re-armed inside the settle window: nothing else could have landed in the
		// observation window whether the code were correct or not.
		SettleWindow: 75 * time.Millisecond,
		BackoffBase:  20 * time.Millisecond,
	})

	if n := inflight.MarkSevered(zerolog.Nop(), severed, resumer.OnStreamSevered); n != 1 {
		t.Fatalf("MarkSevered marked %d chats, want 1", n)
	}
	// Marking is a trigger, not a resume: nothing may be delivered until the
	// settle window has actually elapsed.
	mu.Lock()
	immediate := len(delivered)
	mu.Unlock()
	if immediate != 0 {
		t.Fatal("delivered a resume prompt at marking time; the settle window must come first")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no resume prompt within 3s of recovering a severed stream")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Quiet period. With the compressed seams the remaining ladder is entirely
	// inside it: attempt 2 is armed at BackoffBase<<1 = 40ms after the first
	// delivery and attempt 3 at 80ms after that, and a cycle that re-armed off
	// the settle window would fire at 75ms — so 400ms covers every one of them
	// several times over. A second prompt from ANY of those paths lands here and
	// trips the count below; silence means the cycle genuinely ended, not that
	// the window was too short to see it.
	//
	// What this does NOT prove: it says nothing about behaviour beyond the
	// ladder (a chat re-armed hours later by a fresh trigger), and it is not a
	// statement about the production constants themselves — those are exercised
	// by the resume package's own tests.
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	got := append([]string(nil), delivered...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("resume prompts = %d, want exactly 1: %q", len(got), got)
	}
	if got[0] != resume.ResumeMessage {
		t.Fatalf("resume prompt = %q, want %q", got[0], resume.ResumeMessage)
	}
}

// TestSessionScopedStreamsAreNotRecorded pins the scope decision. A
// session-scoped proxy token names no chat, and the resume lane's gates and
// delivery seam both key on an agent session id — so recording one would arm a
// cycle after every restart that every gate below is guaranteed to abandon,
// buying nothing but log noise on a path that must stay quiet.
func TestSessionScopedStreamsAreNotRecorded(t *testing.T) {
	const token = "8888888899999999aaaaaaaabbbbbbbbccccccccddddddddeeeeeeeeffffffff"
	recordPath := inflight.Path(t.TempDir())

	seen := make(chan []string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Read the record from INSIDE the request, the only point at which
		// anything would have been written.
		seen <- recordedStreamIDs(t, recordPath)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy, err := NewProxyServer(ProxyServerConfig{
		Failover: &fakeFailover{},
		Logger:   zerolog.Nop(),
		Upstream: upstream.URL,
		Streams:  inflight.NewRecorder(recordPath, zerolog.Nop()),
	})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := proxy.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- proxy.Serve() }()

	proxy.AdoptToken(token, "sess-no-chat")

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/s/%s/v1/messages", proxy.Port(), token), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = proxy.Shutdown(ctx)
	<-serveErr

	if got := <-seen; len(got) != 0 {
		t.Fatalf("session-scoped stream recorded %v, want nothing", got)
	}
}

// TestGracefullyDrainedStreamsLeaveNothingToRecover is the other half of the
// contract, and the one that keeps the feature from nagging: a normal
// `boss daemon restart` whose streams finish must leave NO record, so the next
// startup recovers nothing and no chat is prompted. Without this, every
// graceful restart with traffic in flight would spray resume prompts into
// chats that completed their turns perfectly well.
func TestGracefullyDrainedStreamsLeaveNothingToRecover(t *testing.T) {
	const (
		sessionID      = "sess-drain"
		agentSessionID = "chat-drain"
		token          = "0000000011111111222222223333333344444444555555556666666677777777"
	)
	dir := t.TempDir()
	recordPath := inflight.Path(dir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	proxy, err := NewProxyServer(ProxyServerConfig{
		Failover: &fakeFailover{},
		Logger:   zerolog.Nop(),
		Upstream: upstream.URL,
		Streams:  inflight.NewRecorder(recordPath, zerolog.Nop()),
	})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	if err := proxy.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- proxy.Serve() }()

	proxy.AdoptTokenForChat(sessionID, agentSessionID, "acct-drain", token)

	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/s/%s/v1/messages", proxy.Port(), token), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer client-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome, _ := proxy.Shutdown(ctx)
	<-serveErr
	if !outcome.Drained {
		t.Fatalf("expected a clean drain, got %+v", outcome)
	}

	severed, err := inflight.ReadAndClear(recordPath)
	if err != nil {
		t.Fatalf("ReadAndClear: %v", err)
	}
	if len(severed) != 0 {
		t.Fatalf("a graceful restart left %v to recover, want nothing", severed)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("stream record left a temp file behind: %s", e.Name())
		}
	}
}
