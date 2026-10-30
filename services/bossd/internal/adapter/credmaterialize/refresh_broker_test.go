package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type brokerMemory struct {
	fakeStore
	stateMu sync.Mutex
	state   []byte
}

func (s *brokerMemory) LoadRefreshState(context.Context, string) ([]byte, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return append([]byte(nil), s.state...), nil
}
func (s *brokerMemory) SaveRefreshState(_ context.Context, _ string, b []byte) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.state = append([]byte(nil), b...)
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
	r, err := http.Post(url, "application/json", bytes.NewReader(body))
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

func TestRuntimeBrokerRejectsCapabilityFromPreviousDaemonInstance(t *testing.T) {
	first, store, gate, calls := brokerFixture(t)
	oldCap, err := first.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	next, err := NewRefreshBroker(store, gate, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	next.providerURL = first.providerURL
	if status, _ := brokerPost(t, next.URL(), oldCap); status != http.StatusUnauthorized || calls.Load() != 0 {
		t.Fatal("old broker capability remained redeemable after restart")
	}
	freshCap, err := next.Capability(context.Background(), "shared", store.currentBlob())
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := brokerPost(t, next.URL(), freshCap); status != 200 || calls.Load() != 1 {
		t.Fatal("new broker capability did not work")
	}
}
