package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type brokerMemory struct {
	fakeStore
	stateMu         sync.Mutex
	state           []byte
	stateErr        error
	endpoint        []byte
	endpointErr     error
	endpointSaveErr error
	endpointTried   []byte
}

func (s *brokerMemory) LoadRefreshState(context.Context, string) ([]byte, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.stateErr != nil {
		return nil, s.stateErr
	}
	return append([]byte(nil), s.state...), nil
}
func (s *brokerMemory) SaveRefreshState(_ context.Context, _ string, b []byte) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state = append([]byte(nil), b...)
	return nil
}

func (s *brokerMemory) LoadRefreshBrokerEndpoint(context.Context) ([]byte, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.endpointErr != nil {
		return nil, s.endpointErr
	}
	return append([]byte(nil), s.endpoint...), nil
}
func (s *brokerMemory) SaveRefreshBrokerEndpoint(_ context.Context, b []byte) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.endpointTried = append([]byte(nil), b...)
	if s.endpointSaveErr != nil {
		return s.endpointSaveErr
	}
	s.endpoint = append([]byte(nil), b...)
	return nil
}

type brokerLease struct {
	owned     bool
	offline   bool
	publishes int
}

func (g *brokerLease) Receive(context.Context, string) error {
	if g.offline {
		return errors.New("offline")
	}
	return nil
}
func (g *brokerLease) Acquire(context.Context, string, []byte, string) (bool, error) {
	return g.owned, nil
}
func (g *brokerLease) PublishRefresh(context.Context, string, []byte, []byte) error {
	if g.offline {
		return errors.New("offline")
	}
	g.publishes++
	return nil
}
func brokerFixture(t *testing.T) (*RefreshBroker, *brokerMemory, *brokerLease, *atomic.Int64) {
	t.Helper()
	s := &brokerMemory{}
	s.blob = []byte(`{"refresh":"provider-secret","tokens":{"access_token":"old-access","refresh_token":"provider-secret","id_token":"old-id","account_id":"account"}}`)
	g := &brokerLease{owned: true}
	b, err := NewRefreshBroker(s, g, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	calls := &atomic.Int64{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var q map[string]string
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q["refresh_token"] != "provider-secret" {
			t.Error("provider did not receive real secret")
		}
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-secret","id_token":"new-id"}`)
	}))
	t.Cleanup(up.Close)
	b.providerURL = up.URL
	return b, s, g, calls
}
func brokerPost(t *testing.T, url, capability string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"client_id": codexOAuthClientID, "grant_type": "refresh_token", "refresh_token": capability})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A restarted broker reclaims the same address, so a pooled keep-alive
	// connection to its closed predecessor must never carry the next request.
	req.Close = true
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	raw, _ := io.ReadAll(r.Body)
	return r.StatusCode, raw
}
func TestRuntimeBrokerLongLivedAndConcurrentRefresh(t *testing.T) {
	b, s, g, calls := brokerFixture(t)
	m, err := New(s, zerolog.Nop(), WithBaseDir(t.TempDir()), WithRefreshBroker(b))
	if err != nil {
		t.Fatal(err)
	}
	mat, persist, err := m.MaterializeCodex(context.Background(), "shared")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(mat.HomeDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("provider-secret")) {
		t.Fatal("materialized provider refresh secret")
	}
	var a struct {
		Tokens tokenFields `json:"tokens"`
	}
	_ = json.Unmarshal(raw, &a)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			status, out := brokerPost(t, mat.Env["CODEX_REFRESH_TOKEN_URL_OVERRIDE"], a.Tokens.RefreshToken)
			if status != 200 || bytes.Contains(out, []byte("new-secret")) {
				t.Error("unsafe broker response", status)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || g.publishes != 1 {
		t.Fatalf("exchanges=%d publishes=%d", calls.Load(), g.publishes)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.currentBlob(), []byte("boss-refresh-v1")) || !bytes.Contains(s.currentBlob(), []byte("new-secret")) {
		t.Fatal("capability overwrote real keyring secret")
	}
}
func TestRuntimeBrokerRestartRecoversPublicationAndUnknownFailsClosed(t *testing.T) {
	b, s, g, calls := brokerFixture(t)
	cap, err := b.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the connection dying after provider accepted the refresh.
	b.client = &http.Client{Transport: brokerErrorTransport{}}
	status, _ := brokerPost(t, b.URL(), cap)
	if status == 200 {
		t.Fatal("unknown exchange succeeded")
	}
	b.Close()
	next, err := NewRefreshBroker(s, g, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.providerURL = b.providerURL
	cap, err = next.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	status, _ = brokerPost(t, next.URL(), cap)
	if status == 200 || calls.Load() != 0 {
		t.Fatal("retried ambiguous provider exchange")
	}
}

type brokerErrorTransport struct{ calls *atomic.Int64 }

func (x brokerErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if x.calls != nil {
		x.calls.Add(1)
	}
	return nil, errors.New("unknown outcome")
}
func TestRuntimeBrokerPendingPublicationRetriesWithoutExchange(t *testing.T) {
	b, s, g, calls := brokerFixture(t)
	cap, err := b.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	// Provider can complete while cloud coordination becomes unavailable.
	upstream := b.client
	b.client = &http.Client{Transport: brokerAfterTransport{base: upstream.Transport, after: func() { g.offline = true }}}
	status, _ := brokerPost(t, b.URL(), cap)
	if status == 200 {
		t.Fatal("returned unpublished refresh")
	}
	b.Close()
	g.offline = false
	next, err := NewRefreshBroker(s, g, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.providerURL = b.providerURL
	cap, err = next.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	status, _ = brokerPost(t, next.URL(), cap)
	if status != 200 || calls.Load() != 1 {
		t.Fatalf("status=%d exchanges=%d", status, calls.Load())
	}
}

type brokerAfterTransport struct {
	base  http.RoundTripper
	after func()
}

func (x brokerAfterTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := x.base
	if base == nil {
		base = http.DefaultTransport
	}
	out, err := base.RoundTrip(r)
	x.after()
	return out, err
}
func TestRuntimeBrokerRejectsUnauthorizedAndLeaseDenial(t *testing.T) {
	b, s, g, calls := brokerFixture(t)
	cap, err := b.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	status, _ := brokerPost(t, b.URL(), "forged")
	if status == 200 {
		t.Fatal("accepted forged capability")
	}
	g.owned = false
	b.peerWait = time.Millisecond
	status, _ = brokerPost(t, b.URL(), cap)
	if status == 200 || calls.Load() != 0 {
		t.Fatal("nonowner exchanged refresh")
	}
}

func TestRuntimeBrokerUnknownRemainsBlockedWhenAccessTokenChanges(t *testing.T) {
	b, s, g, _ := brokerFixture(t)
	cap, err := b.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	b.client = &http.Client{Transport: brokerErrorTransport{}}
	status, _ := brokerPost(t, b.URL(), cap)
	if status == 200 {
		t.Fatal("ambiguous exchange succeeded")
	}
	s.setBlob(bytes.ReplaceAll(s.currentBlob(), []byte("old-access"), []byte("different-access")))
	status, _ = brokerPost(t, b.URL(), cap)
	if status == 200 {
		t.Fatal("access-token mutation cleared unknown refresh outcome")
	}
	b.Close()
	next, err := NewRefreshBroker(s, g, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	cap, err = next.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	status, _ = brokerPost(t, next.URL(), cap)
	if status == 200 {
		t.Fatal("restart cleared unknown outcome")
	}
}
func TestRuntimeBrokerPreservesConcurrentReauthentication(t *testing.T) {
	b, s, _, calls := brokerFixture(t)
	cap, err := b.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	replacement := bytes.ReplaceAll(s.currentBlob(), []byte("provider-secret"), []byte("operator-new-secret"))
	b.client = &http.Client{Transport: brokerAfterTransport{after: func() { s.setBlob(replacement) }}}
	status, _ := brokerPost(t, b.URL(), cap)
	if status == 200 {
		t.Fatal("returned stale refresh after operator replaced credential")
	}
	if !bytes.Equal(s.currentBlob(), replacement) || calls.Load() != 1 {
		t.Fatal("pending result overwrote operator reauthentication")
	}
	status, _ = brokerPost(t, b.URL(), cap)
	if status != 200 || calls.Load() != 1 {
		t.Fatal("reauthentication did not recover without spending old token")
	}
}
func TestRuntimeBrokerRejectsOversizedRequestAndStops(t *testing.T) {
	b, store, _, calls := brokerFixture(t)
	capability, err := b.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"client_id": codexOAuthClientID, "grant_type": "refresh_token", "refresh_token": capability})
	response, err := http.Post(b.URL(), "application/json", strings.NewReader(string(body)+strings.Repeat(" ", 9000)))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode == 200 || calls.Load() != 0 {
		t.Fatal("oversized request reached provider")
	}
	url := b.URL()
	b.Close()
	response, err = http.Post(url, "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("stopped broker accepted request")
	}
}

type runtimeRefreshCloud struct {
	mu             sync.Mutex
	blob           []byte
	owner, attempt string
	fingerprint    string
}
type runtimeRefreshPeer struct {
	cloud  *runtimeRefreshCloud
	store  *brokerMemory
	id     string
	denied chan struct{}
}

func (p runtimeRefreshPeer) Receive(context.Context, string) error {
	p.cloud.mu.Lock()
	defer p.cloud.mu.Unlock()
	if p.cloud.blob != nil {
		p.store.setBlob(p.cloud.blob)
	}
	return nil
}
func (p runtimeRefreshPeer) Acquire(_ context.Context, _ string, fp []byte, attempt string) (bool, error) {
	p.cloud.mu.Lock()
	defer p.cloud.mu.Unlock()
	fingerprint := string(fp)
	if p.cloud.fingerprint == fingerprint && (p.cloud.owner != p.id || p.cloud.attempt != attempt) {
		if p.denied != nil {
			select {
			case p.denied <- struct{}{}:
			default:
			}
		}
		return false, nil
	}
	p.cloud.fingerprint = fingerprint
	p.cloud.owner = p.id
	p.cloud.attempt = attempt
	return true, nil
}
func (p runtimeRefreshPeer) PublishRefresh(_ context.Context, _ string, blob []byte, _ []byte) error {
	p.cloud.mu.Lock()
	defer p.cloud.mu.Unlock()
	p.cloud.blob = append([]byte(nil), blob...)
	return nil
}
func TestRuntimeBrokerTwoDaemonsOneExchange_Accounts_AC25(t *testing.T) {
	first, s, _, calls := brokerFixture(t)
	cloud := &runtimeRefreshCloud{}
	first.gate = runtimeRefreshPeer{cloud: cloud, store: s, id: "a"}
	otherStore := &brokerMemory{}
	otherStore.setBlob(s.currentBlob())
	second, err := NewRefreshBroker(otherStore, runtimeRefreshPeer{cloud: cloud, store: otherStore, id: "b"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.providerURL = first.providerURL
	capA, err := first.Capability(context.Background(), "shared", s.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	capB, err := second.Capability(context.Background(), "shared", otherStore.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, entry := range []struct {
		b   *RefreshBroker
		cap string
	}{{first, capA}, {second, capB}} {
		wg.Go(func() {
			status, _ := brokerPost(t, entry.b.URL(), entry.cap)
			if status != 200 && status != 503 {
				t.Errorf("unexpected status %d", status)
			}
		})
	}
	wg.Wait()
	for _, entry := range []struct {
		b   *RefreshBroker
		cap string
	}{{first, capA}, {second, capB}} {
		status, _ := brokerPost(t, entry.b.URL(), entry.cap)
		if status != 200 {
			t.Fatal("winning publication not received by peer")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("two daemons spent credential %d times", calls.Load())
	}
	// Erasing the local journal must not let even the same daemon identity
	// re-use an old credential with a newly generated broker seed/attempt ID.
	cloud.mu.Lock()
	owner := cloud.owner
	cloud.blob = nil
	cloud.mu.Unlock()
	lostStore, original := s, first
	if owner == "b" {
		lostStore, original = otherStore, second
	}
	original.Close()
	lostStore.setBlob([]byte(`{"tokens":{"access_token":"old-access","refresh_token":"provider-secret","id_token":"old-id"}}`))
	lostStore.stateMu.Lock()
	lostStore.state = nil
	lostStore.stateMu.Unlock()
	restarted, err := NewRefreshBroker(lostStore, runtimeRefreshPeer{cloud: cloud, store: lostStore, id: owner}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.providerURL = first.providerURL
	restarted.peerWait = time.Millisecond
	newcap, err := restarted.Capability(context.Background(), "shared", lostStore.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	status, _ := brokerPost(t, restarted.URL(), newcap)
	if status == 200 || calls.Load() != 1 {
		t.Fatal("lost journal retried spent token")
	}
}

func TestRuntimeBrokerNeverReusesEarlierUnknownSecret(t *testing.T) {
	b, s, _, _ := brokerFixture(t)
	original := s.currentBlob()
	cap, err := b.Capability(context.Background(), "shared", original)
	if err != nil {
		t.Fatal(err)
	}
	attempts := &atomic.Int64{}
	b.client = &http.Client{Transport: brokerErrorTransport{calls: attempts}}
	if status, _ := brokerPost(t, b.URL(), cap); status == 200 {
		t.Fatal("unknown exchange succeeded")
	}
	s.setBlob(bytes.ReplaceAll(original, []byte("provider-secret"), []byte("new-reauth-secret")))
	if status, _ := brokerPost(t, b.URL(), cap); status != 200 {
		t.Fatal("new credential did not recover unknown state")
	}
	s.setBlob(original)
	if status, _ := brokerPost(t, b.URL(), cap); status == 200 || attempts.Load() != 1 {
		t.Fatal("older unknown refresh secret was spent again after reauthentication")
	}
}

func TestRuntimeBrokerWaitsForActiveOwnerPublication(t *testing.T) {
	first, store, _, calls := brokerFixture(t)
	cloud := &runtimeRefreshCloud{}
	first.gate = runtimeRefreshPeer{cloud: cloud, store: store, id: "a"}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		_, _ = io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-secret"}`)
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	first.providerURL = upstream.URL
	other := &brokerMemory{}
	other.setBlob(store.currentBlob())
	denied := make(chan struct{}, 1)
	second, err := NewRefreshBroker(other, runtimeRefreshPeer{cloud: cloud, store: other, id: "b", denied: denied}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.providerURL = upstream.URL
	capA, err := first.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	capB, err := second.Capability(context.Background(), "shared", other.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	wg.Go(func() { status, _ := brokerPost(t, first.URL(), capA); statuses <- status })
	<-started
	wg.Go(func() { status, _ := brokerPost(t, second.URL(), capB); statuses <- status })
	<-denied
	once.Do(func() { close(release) })
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != 200 {
			t.Fatalf("active-owner request prematurely returned %d", status)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("peer did not reuse owner's published credential")
	}
}

// restartBroker closes first and starts its successor on the same store, as a
// daemon restart does.
func restartBroker(t *testing.T, first *RefreshBroker, store *brokerMemory, gate RefreshBrokerGate, rebind refreshBrokerRebind) *RefreshBroker {
	t.Helper()
	first.Close()
	next, err := newRefreshBroker(store, gate, zerolog.Nop(), rebind)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	next.providerURL = first.providerURL
	return next
}

func persistedEndpoint(t *testing.T, store *brokerMemory) refreshBrokerEndpoint {
	t.Helper()
	raw, _ := store.LoadRefreshBrokerEndpoint(context.Background())
	e, ok := decodeRefreshBrokerEndpoint(raw)
	if !ok {
		t.Fatalf("endpoint state not persisted: %q", raw)
	}
	return e
}

func brokerPort(b *RefreshBroker) int { return b.listener.Addr().(*net.TCPAddr).Port }

func TestRuntimeBrokerCapabilitySurvivesRestart_Accounts_AC223601(t *testing.T) {
	first, store, gate, calls := brokerFixture(t)
	oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	next := restartBroker(t, first, store, gate, defaultRefreshBrokerRebind)
	if next.URL() != first.URL() {
		t.Fatalf("restart moved the broker from %s to %s", first.URL(), next.URL())
	}
	status, raw := brokerPost(t, next.URL(), oldCap)
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("previous-instance capability: status=%d exchanges=%d", status, calls.Load())
	}
	var issued tokenFields
	if err := json.Unmarshal(raw, &issued); err != nil || !strings.HasPrefix(issued.RefreshToken, brokerCapabilityPrefix) {
		t.Fatal("broker did not return a capability")
	}
	second := &atomic.Int64{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"third-access","refresh_token":"third-secret","id_token":"third-id"}`)
	}))
	t.Cleanup(provider.Close)
	next.providerURL = provider.URL
	third := restartBroker(t, next, store, gate, defaultRefreshBrokerRebind)
	if status, _ := brokerPost(t, third.URL(), issued.RefreshToken); status != http.StatusOK || second.Load() != 1 {
		t.Fatalf("capability issued after the first restart: status=%d exchanges=%d", status, second.Load())
	}
}

func TestRuntimeBrokerRotatesWhenPortIsTaken_Accounts_AC223601(t *testing.T) {
	first, store, gate, calls := brokerFixture(t)
	oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	before := persistedEndpoint(t, store)
	first.Close()
	squatter, err := net.Listen("tcp4", first.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = squatter.Close() })
	next := restartBroker(t, first, store, gate, refreshBrokerRebind{window: 20 * time.Millisecond, step: 5 * time.Millisecond})
	if next.URL() == first.URL() {
		t.Fatal("broker shared the squatted port")
	}
	after := persistedEndpoint(t, store)
	if after.Port != brokerPort(next) || after.BrokerID != next.instanceID || after.BrokerID == before.BrokerID {
		t.Fatalf("rotation not persisted: before=%+v after=%+v", before, after)
	}
	if status, _ := brokerPost(t, next.URL(), oldCap); status != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("rotated broker accepted previous capability: status=%d exchanges=%d", status, calls.Load())
	}
	freshCap, err := next.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := brokerPost(t, next.URL(), freshCap); status != http.StatusOK || calls.Load() != 1 {
		t.Fatal("rotated broker rejected its own capability")
	}
}

func TestRuntimeBrokerRefusesUnpersistedRotation_Accounts_AC223601(t *testing.T) {
	first, store, _, _ := brokerFixture(t)
	before := persistedEndpoint(t, store)
	first.Close()
	squatter, err := net.Listen("tcp4", first.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = squatter.Close() })
	store.stateMu.Lock()
	store.endpointSaveErr = errors.New("keyring locked")
	store.stateMu.Unlock()
	next, err := newRefreshBroker(store, &brokerLease{owned: true}, zerolog.Nop(), refreshBrokerRebind{window: 20 * time.Millisecond, step: 5 * time.Millisecond})
	if err == nil {
		next.Close()
		t.Fatal("broker served a rotation it could not persist")
	}
	if strings.Contains(err.Error(), before.BrokerID) {
		t.Fatal("error leaked the broker id")
	}
	if after := persistedEndpoint(t, store); after != before {
		t.Fatalf("persisted endpoint changed: before=%+v after=%+v", before, after)
	}
	store.stateMu.Lock()
	tried, ok := decodeRefreshBrokerEndpoint(store.endpointTried)
	store.stateMu.Unlock()
	if !ok || tried.Port == before.Port {
		t.Fatalf("no rotation was attempted: %+v", tried)
	}
	rebound, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(tried.Port)))
	if err != nil {
		t.Fatalf("rotated listener left open on port %d: %v", tried.Port, err)
	}
	_ = rebound.Close()
	if stolen, err := net.Listen("tcp4", first.listener.Addr().String()); err == nil {
		_ = stolen.Close()
		t.Fatal("squatter lost the occupied port")
	}
}

func TestRuntimeBrokerRefusesUnpersistedRotationAfterLoadError_Accounts_AC223601(t *testing.T) {
	first, store, _, _ := brokerFixture(t)
	before := persistedEndpoint(t, store)
	first.Close()
	store.stateMu.Lock()
	store.endpointErr, store.endpointSaveErr = errors.New("keyring locked"), errors.New("keyring locked")
	store.stateMu.Unlock()
	next, err := newRefreshBroker(store, &brokerLease{owned: true}, zerolog.Nop(), refreshBrokerRebind{window: 20 * time.Millisecond, step: 5 * time.Millisecond})
	if err == nil {
		next.Close()
		t.Fatal("broker served a rotation a cleared load error could undo")
	}
	store.stateMu.Lock()
	store.endpointErr = nil
	tried, ok := decodeRefreshBrokerEndpoint(store.endpointTried)
	store.stateMu.Unlock()
	if after := persistedEndpoint(t, store); after != before {
		t.Fatalf("persisted endpoint changed: before=%+v after=%+v", before, after)
	}
	if !ok {
		t.Fatal("no rotation was attempted")
	}
	rebound, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(tried.Port)))
	if err != nil {
		t.Fatalf("rotated listener left open on port %d: %v", tried.Port, err)
	}
	_ = rebound.Close()
}

func TestRuntimeBrokerRotatesOnUnreadableEndpointState_Accounts_AC223601(t *testing.T) {
	for name, corrupt := range map[string]func(*brokerMemory){
		"garbage":    func(s *brokerMemory) { s.endpoint = []byte("not json") },
		"zero port":  func(s *brokerMemory) { s.endpoint = []byte(`{"broker_id":"id","port":0}`) },
		"empty id":   func(s *brokerMemory) { s.endpoint = []byte(`{"broker_id":"","port":4000}`) },
		"load error": func(s *brokerMemory) { s.endpointErr = errors.New("keyring locked") },
		"lost and unsaveable": func(s *brokerMemory) {
			s.endpoint, s.endpointSaveErr = nil, errors.New("keyring locked")
		},
	} {
		t.Run(name, func(t *testing.T) {
			first, store, gate, calls := brokerFixture(t)
			oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
			if err != nil {
				t.Fatal(err)
			}
			store.stateMu.Lock()
			corrupt(store)
			store.stateMu.Unlock()
			next := restartBroker(t, first, store, gate, defaultRefreshBrokerRebind)
			if status, _ := brokerPost(t, next.URL(), oldCap); status != http.StatusUnauthorized || calls.Load() != 0 {
				t.Fatalf("rotated broker accepted previous capability: status=%d exchanges=%d", status, calls.Load())
			}
			freshCap, err := next.Capability(context.Background(), "shared", store.currentBlob())
			if err != nil {
				t.Fatal(err)
			}
			if status, _ := brokerPost(t, next.URL(), freshCap); status != http.StatusOK {
				t.Fatal("rotated broker did not serve")
			}
		})
	}
}

func TestRuntimeBrokerRemovedAccountRefusedAfterRestart_Accounts_AC223601(t *testing.T) {
	first, store, gate, calls := brokerFixture(t)
	oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	store.stateMu.Lock()
	store.state = nil
	store.stateMu.Unlock()
	next := restartBroker(t, first, store, gate, defaultRefreshBrokerRebind)
	if next.URL() != first.URL() {
		t.Fatal("restart did not reclaim the endpoint")
	}
	if status, _ := brokerPost(t, next.URL(), oldCap); status != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatalf("removed account redeemed: status=%d exchanges=%d", status, calls.Load())
	}
}

func TestRuntimeBrokerGateStillDecidesAfterRestart_Accounts_AC223601(t *testing.T) {
	for name, deny := range map[string]func(*brokerLease){
		"denied":  func(g *brokerLease) { g.owned = false },
		"offline": func(g *brokerLease) { g.offline = true },
	} {
		t.Run(name, func(t *testing.T) {
			first, store, gate, calls := brokerFixture(t)
			oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
			if err != nil {
				t.Fatal(err)
			}
			next := restartBroker(t, first, store, gate, defaultRefreshBrokerRebind)
			deny(gate)
			next.peerWait = time.Millisecond
			if status, _ := brokerPost(t, next.URL(), oldCap); status == http.StatusOK || calls.Load() != 0 {
				t.Fatalf("gate bypassed: status=%d exchanges=%d", status, calls.Load())
			}
		})
	}
}
