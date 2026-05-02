package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// hookMockSessionStore is a narrow stub satisfying db.SessionStore for the
// hook server tests. Only Get is exercised; the rest panic so a drift in
// HookServer's dependency surface is caught immediately.
type hookMockSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
	getErr   error
}

var _ db.SessionStore = (*hookMockSessionStore)(nil)

func newHookMockSessionStore() *hookMockSessionStore {
	return &hookMockSessionStore{sessions: make(map[string]*models.Session)}
}

func (m *hookMockSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	sess, ok := m.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return sess, nil
}

func (m *hookMockSessionStore) Create(context.Context, db.CreateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) List(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) ListByState(context.Context, int) ([]*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) ListActive(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) ListActiveWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (m *hookMockSessionStore) ListWithRepo(context.Context, string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (m *hookMockSessionStore) ListArchived(context.Context, string) ([]*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) Update(context.Context, string, db.UpdateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (m *hookMockSessionStore) UpdateStateConditional(context.Context, string, int, int) (bool, error) {
	panic("not used")
}
func (m *hookMockSessionStore) Archive(context.Context, string) error   { panic("not used") }
func (m *hookMockSessionStore) Resurrect(context.Context, string) error { panic("not used") }
func (m *hookMockSessionStore) Delete(context.Context, string) error    { panic("not used") }
func (m *hookMockSessionStore) AdvanceOrphanedSessions(context.Context) (int64, error) {
	panic("not used")
}

// fakeFinalizer records FinalizeSession invocations so tests can assert
// the dispatch happened (and only for the expected session).
type fakeFinalizer struct {
	mu    sync.Mutex
	calls []string
	done  chan struct{}
	err   error
}

func newFakeFinalizer() *fakeFinalizer {
	return &fakeFinalizer{done: make(chan struct{}, 1)}
}

func (f *fakeFinalizer) FinalizeSession(_ context.Context, sessionID string) (*session.FinalizeResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, sessionID)
	f.mu.Unlock()
	// Non-blocking signal so multiple calls don't deadlock on full channel.
	select {
	case f.done <- struct{}{}:
	default:
	}
	if f.err != nil {
		return nil, f.err
	}
	return &session.FinalizeResult{Outcome: models.CronJobOutcomePRCreated}, nil
}

func (f *fakeFinalizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// startHookServer boots a HookServer wired to the given deps and returns
// it plus the handler-dispatched base URL.
func startHookServer(t *testing.T, store db.SessionStore, fin HookFinalizer) (*HookServer, string) {
	t.Helper()
	hs := NewHookServer(HookServerConfig{
		Sessions:  store,
		Finalizer: fin,
		Logger:    zerolog.Nop(),
	})
	if err := hs.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- hs.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
		// Drain serve error; http.ErrServerClosed is expected.
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	})
	return hs, fmt.Sprintf("http://127.0.0.1:%d", hs.Port())
}

// waitForDispatch blocks up to timeout waiting for the fake finalizer to
// record a call. Used because the handler dispatches asynchronously.
func waitForDispatch(t *testing.T, f *fakeFinalizer, timeout time.Duration) {
	t.Helper()
	select {
	case <-f.done:
	case <-time.After(timeout):
		t.Fatalf("FinalizeSession was not dispatched within %s", timeout)
	}
}

func strPtr(s string) *string { return &s }

// TestHookServer_HappyPath valid token → 200 + FinalizeSession dispatched.
func TestHookServer_HappyPath(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-1"] = &models.Session{ID: "sess-1", HookToken: strPtr("secret-token")}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-1", "Bearer secret-token")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}

	waitForDispatch(t, fin, 1*time.Second)
	if got := fin.callCount(); got != 1 {
		t.Errorf("FinalizeSession calls = %d, want 1", got)
	}
	fin.mu.Lock()
	if fin.calls[0] != "sess-1" {
		t.Errorf("dispatched session = %q, want sess-1", fin.calls[0])
	}
	fin.mu.Unlock()
}

// TestHookServer_WrongToken wrong bearer token → 401, no dispatch.
func TestHookServer_WrongToken(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-1"] = &models.Session{ID: "sess-1", HookToken: strPtr("right-token")}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-1", "Bearer wrong-token")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	// Give any spurious dispatch a chance to race in; 100ms is enough.
	time.Sleep(100 * time.Millisecond)
	if got := fin.callCount(); got != 0 {
		t.Errorf("FinalizeSession calls = %d, want 0 on 401", got)
	}
}

// TestHookServer_MissingAuthHeader no Authorization header → 401.
func TestHookServer_MissingAuthHeader(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-1"] = &models.Session{ID: "sess-1", HookToken: strPtr("secret")}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-1", "")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

// TestHookServer_UnknownSession session not in DB → 404.
func TestHookServer_UnknownSession(t *testing.T) {
	store := newHookMockSessionStore()
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "does-not-exist", "Bearer anything")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestHookServer_NilHookTokenNoop a non-cron session (nil HookToken) → 200 no-op.
// This keeps legacy sessions from noisy-failing if settings.local.json is
// ever attached to a non-cron worktree.
func TestHookServer_NilHookTokenNoop(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-legacy"] = &models.Session{ID: "sess-legacy"} // HookToken nil
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-legacy", "Bearer whatever")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (no-op)", status)
	}
	time.Sleep(100 * time.Millisecond)
	if got := fin.callCount(); got != 0 {
		t.Errorf("FinalizeSession calls = %d, want 0 on nil-token no-op", got)
	}
}

// TestHookServer_EmptyHookTokenNoop empty-string HookToken treated same as nil.
func TestHookServer_EmptyHookTokenNoop(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-empty"] = &models.Session{ID: "sess-empty", HookToken: strPtr("")}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-empty", "Bearer whatever")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (empty-token no-op)", status)
	}
	if got := fin.callCount(); got != 0 {
		t.Errorf("FinalizeSession calls = %d, want 0", got)
	}
}

// TestBearerToken exercises the header parser's branches directly.
func TestBearerToken(t *testing.T) {
	cases := []struct {
		in      string
		wantTok string
		wantOK  bool
	}{
		{"Bearer abc", "abc", true},
		{"Bearer   abc  ", "abc", true},
		{"bearer abc", "", false}, // case sensitive per RFC 6750
		{"Basic abc", "", false},
		{"", "", false},
		{"Bearer ", "", false},
		{"Bearer", "", false},
	}
	for _, c := range cases {
		got, ok := bearerToken(c.in)
		if got != c.wantTok || ok != c.wantOK {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.wantTok, c.wantOK)
		}
	}
}

// TestHookServer_ListenerIsLoopback verifies the HookServer never binds
// a non-loopback address. An external attacker on the same network
// (e.g. a shared dev machine) shouldn't be able to reach the finalize
// endpoint even if they guess the port.
func TestHookServer_ListenerIsLoopback(t *testing.T) {
	store := newHookMockSessionStore()
	fin := newFakeFinalizer()
	hs, _ := startHookServer(t, store, fin)

	addr, ok := hs.listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is not *net.TCPAddr: %T", hs.listener.Addr())
	}
	if !addr.IP.IsLoopback() {
		t.Errorf("listener bound to %s, want a loopback IP", addr.IP)
	}
	if addr.IP.String() != "127.0.0.1" {
		// Defence-in-depth: .IsLoopback() also accepts ::1, but the plan
		// calls for IPv4 127.0.0.1 so WriteHookConfig's curl URL matches.
		t.Errorf("listener IP = %s, want 127.0.0.1", addr.IP)
	}
}

// Note: the plan originally called for a "attempt to bind 0.0.0.0 on
// the hook port → assert refuse" test, but macOS/BSD allow a wildcard
// bind to coexist with a specific-address bind on the same port, so
// that assertion doesn't hold cross-platform. The real security claim
// — that we never bind anywhere but 127.0.0.1 — is enforced by
// TestHookServer_ListenerIsLoopback above, which reads the actual
// listener address rather than relying on kernel rebind semantics.

// TestHookServer_MethodNotAllowed GET on the finalize path is rejected
// by the mux (only POST is registered). Belt-and-braces: restricts
// the attack surface to POSTs only.
func TestHookServer_MethodNotAllowed(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-1"] = &models.Session{ID: "sess-1", HookToken: strPtr("secret")}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/hooks/finalize/sess-1", base), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
	if n := fin.callCount(); n != 0 {
		t.Errorf("GET dispatched finalizer %d times; want 0", n)
	}
}

// TestHookServer_UnknownRoute404 requests outside the /hooks/finalize/
// prefix get a stock 404 from the mux. No leaking of internals.
func TestHookServer_UnknownRoute404(t *testing.T) {
	_, base := startHookServer(t, newHookMockSessionStore(), newFakeFinalizer())

	resp, err := http.Post(base+"/admin", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /admin status = %d, want 404", resp.StatusCode)
	}
}

// TestHookServer_MissingSessionID POST /hooks/finalize/ (empty path
// param) — the mux should 404 since the pattern requires a non-empty
// segment. Guards against empty session_id slipping through to the
// DB layer.
func TestHookServer_MissingSessionID(t *testing.T) {
	_, base := startHookServer(t, newHookMockSessionStore(), newFakeFinalizer())

	// Trailing slash with empty segment. Go's ServeMux pattern
	// `POST /hooks/finalize/{session_id}` does not match this.
	resp, err := http.Post(base+"/hooks/finalize/", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for empty session_id", resp.StatusCode)
	}
}

// TestHookServer_ConcurrentPOSTs verifies the HTTP layer is safe under
// concurrent Stop hooks for the same session — all requests authenticate
// and dispatch without deadlock, data race, or partial responses.
// FinalizeSession's own idempotency (the UpdateStateConditional gate
// tested in the session package) is what turns N dispatches into one
// observable side-effect; the HTTP server intentionally does not
// short-circuit above that layer so legitimate retries aren't dropped
// before they reach the gate.
func TestHookServer_ConcurrentPOSTs(t *testing.T) {
	store := newHookMockSessionStore()
	store.sessions["sess-concur"] = &models.Session{
		ID:        "sess-concur",
		HookToken: strPtr("rt"),
	}
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	statuses := make([]int, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			statuses[idx] = postFinalize(t, base, "sess-concur", "Bearer rt")
		}(i)
	}
	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("POST[%d] status = %d, want 200", i, s)
		}
	}
	// Give every dispatched goroutine a chance to record its call.
	deadline := time.Now().Add(2 * time.Second)
	for fin.callCount() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := fin.callCount(); got != n {
		t.Errorf("FinalizeSession calls = %d, want %d (handler does not dedupe — idempotency is enforced inside FinalizeSession)", got, n)
	}
}

// TestHookServer_InternalErrorOnStoreFailure surfaces non-NotFound DB
// errors as 500 rather than 404, so an operator paging on anomalous
// 500s sees real infrastructure failures distinctly from legitimate
// "unknown session" 404s.
func TestHookServer_InternalErrorOnStoreFailure(t *testing.T) {
	store := newHookMockSessionStore()
	store.getErr = fmt.Errorf("simulated db outage")
	fin := newFakeFinalizer()
	_, base := startHookServer(t, store, fin)

	status := postFinalize(t, base, "sess-1", "Bearer anything")
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

// postFinalize issues a POST /hooks/finalize/{id} with the given
// Authorization header value (empty string omits the header) and returns
// the HTTP status code. The body is drained + closed immediately so the
// caller needn't track the response struct.
func postFinalize(t *testing.T, base, sessionID, auth string) int {
	t.Helper()
	url := fmt.Sprintf("%s/hooks/finalize/%s", base, sessionID)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
