package credmaterialize

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

// ErrRefreshOwned is transient: the owner must publish before a peer can refresh.
var ErrRefreshOwned = errors.New("refresh is owned by another daemon")

const brokerCapabilityPrefix = "boss-refresh-v1."
const codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// RefreshBrokerStore keeps the attempt journal in a separate local keyring item.
// A missing journal returns nil, nil; an unreadable journal must return an error.
type RefreshBrokerStore interface {
	CredentialStore
	LoadRefreshState(context.Context, string) ([]byte, error)
	SaveRefreshState(context.Context, string, []byte) error
}

// RefreshBrokerGate binds an exchange to the exact distributed credential and
// publishes its known result without replacing it with a winning peer's blob.
type RefreshBrokerGate interface {
	Receive(context.Context, string) error
	Acquire(context.Context, string, []byte, string) (bool, error)
	PublishRefresh(context.Context, string, []byte, []byte) error
}

type refreshJournal struct {
	Seed          []byte   `json:"seed"`
	AttemptDigest string   `json:"attempt_digest,omitempty"`
	Consumed      []string `json:"consumed,omitempty"`
	Result        []byte   `json:"result,omitempty"`
}

type refreshCapability struct {
	AccountID string `json:"account_id"`
	Digest    string `json:"digest"`
	BrokerID  string `json:"broker_id"`
}

// RefreshBroker is a daemon-owned loopback OAuth exchange endpoint. Managed
// subprocesses receive only its capabilities, never a provider refresh secret.
// Close cancels in-flight exchanges and joins the serving goroutine.
type RefreshBroker struct {
	store       RefreshBrokerStore
	gate        RefreshBrokerGate
	listener    net.Listener
	server      *http.Server
	client      *http.Client
	providerURL string
	instanceID  string
	peerWait    time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	done        <-chan struct{}
	closeOnce   sync.Once
	requestMu   sync.Mutex
	closed      bool
	requests    sync.WaitGroup
	locks       keyedMutex
}

// NewRefreshBroker starts the local endpoint. The caller must Close it at daemon shutdown.
func NewRefreshBroker(store RefreshBrokerStore, gate RefreshBrokerGate, logger zerolog.Logger) (*RefreshBroker, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("credential refresh listener unavailable")
	}
	// #nosec G118 -- Close owns cancellation beyond the constructor and joins active requests.
	// owner=@recurser review-by=2027-01-25 issue=BOS-1952
	ctx, cancel := context.WithCancel(context.Background())
	b := &RefreshBroker{store: store, gate: gate, listener: listener, ctx: ctx, cancel: cancel, providerURL: "https://auth.openai.com/oauth/token", instanceID: rand.Text(), peerWait: 30 * time.Second, client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	b.server = &http.Server{Handler: http.HandlerFunc(b.serve), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	b.done = safego.Go(logger, func() { _ = b.server.Serve(listener) })
	return b, nil
}

// URL is the sole refresh destination injected into the managed Codex environment.
func (b *RefreshBroker) URL() string { return "http://" + b.listener.Addr().String() + "/refresh" }

// Close shuts down the endpoint; retained capabilities then fail closed.
func (b *RefreshBroker) Close() {
	b.closeOnce.Do(func() {
		b.requestMu.Lock()
		b.closed = true
		b.requestMu.Unlock()
		b.cancel()
		_ = b.server.Close()
		<-b.done
		b.requests.Wait()
	})
}

// WithRefreshBroker requires brokered refresh for this materializer, including
// when initialization failed and broker is nil (materialization fails closed).
func WithRefreshBroker(b *RefreshBroker) Option {
	return func(m *Materializer) { m.broker = b; m.brokerRequired = true }
}

func (b *RefreshBroker) journal(ctx context.Context, id string) (refreshJournal, error) {
	raw, err := b.store.LoadRefreshState(ctx, id)
	if err != nil {
		return refreshJournal{}, err
	}
	var j refreshJournal
	if len(raw) > 0 {
		if json.Unmarshal(raw, &j) != nil || len(j.Seed) != 32 {
			return j, errors.New("credential refresh journal unreadable")
		}
		return j, nil
	}
	j.Seed = make([]byte, 32)
	if _, err := rand.Read(j.Seed); err != nil {
		return j, err
	}
	return j, b.saveJournal(ctx, id, j)
}
func (b *RefreshBroker) saveJournal(ctx context.Context, id string, j refreshJournal) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return errors.New("credential refresh journal encoding failed")
	}
	return b.store.SaveRefreshState(ctx, id, raw)
}

func credentialDigest(blob []byte) (string, error) {
	top, err := parseObject(codexAuthForWrite(blob))
	if err != nil {
		return "", errors.New("credential refresh token unavailable")
	}
	tokens, err := parseTokenObject(top["tokens"])
	if err != nil {
		return "", errors.New("credential refresh token unavailable")
	}
	known := decodeKnown(tokens)
	if known.RefreshToken == "" || strings.HasPrefix(known.RefreshToken, brokerCapabilityPrefix) {
		return "", errors.New("credential refresh token unavailable")
	}
	raw, _ := json.Marshal([]string{known.AccessToken, known.RefreshToken, known.IDToken})
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// refreshSecretDigest is the consumption identity: access/id-token changes or
// ciphertext re-encryption cannot make an ambiguous refresh safe to retry.
func refreshSecretDigest(blob []byte) (string, error) {
	top, err := parseObject(codexAuthForWrite(blob))
	if err != nil {
		return "", errors.New("credential refresh token unavailable")
	}
	tokens, err := parseTokenObject(top["tokens"])
	if err != nil {
		return "", err
	}
	secret := decodeKnown(tokens).RefreshToken
	if secret == "" {
		return "", errors.New("credential refresh token unavailable")
	}
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}
func (b *RefreshBroker) signedCapability(seed []byte, id, digest string) string {
	raw, _ := json.Marshal(refreshCapability{AccountID: id, Digest: digest, BrokerID: b.instanceID})
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, seed)
	_, _ = mac.Write([]byte(encoded))
	return brokerCapabilityPrefix + encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Capability persists the account's signing seed before exposing a capability.
// Its digest survives key re-encryption and changes only with provider tokens.
func (b *RefreshBroker) Capability(ctx context.Context, id string, blob []byte) (string, error) {
	if b.ctx.Err() != nil {
		return "", errors.New("credential refresh broker stopped")
	}
	unlock := b.locks.Lock(id)
	defer unlock()
	j, err := b.journal(ctx, id)
	if err != nil {
		return "", err
	}
	digest, err := credentialDigest(blob)
	if err != nil {
		return "", err
	}
	return b.signedCapability(j.Seed, id, digest), nil
}
func (b *RefreshBroker) authenticate(ctx context.Context, capability string) (refreshCapability, refreshJournal, error) {
	var c refreshCapability
	parts := strings.Split(strings.TrimPrefix(capability, brokerCapabilityPrefix), ".")
	if !strings.HasPrefix(capability, brokerCapabilityPrefix) || len(parts) != 2 {
		return c, refreshJournal{}, errors.New("invalid capability")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &c) != nil || validateAccountID(c.AccountID) != nil || len(c.Digest) != 43 {
		return c, refreshJournal{}, errors.New("invalid capability")
	}
	// Authentication must never create state for an attacker-chosen account.
	raw, err = b.store.LoadRefreshState(ctx, c.AccountID)
	var j refreshJournal
	if err != nil || json.Unmarshal(raw, &j) != nil || len(j.Seed) != 32 {
		return c, j, errors.New("invalid capability")
	}
	if c.BrokerID != b.instanceID {
		return c, j, errors.New("invalid capability")
	}
	expected := b.signedCapability(j.Seed, c.AccountID, c.Digest)
	if !hmac.Equal([]byte(expected), []byte(capability)) {
		return c, j, errors.New("invalid capability")
	}
	return c, j, nil
}
func (b *RefreshBroker) serve(w http.ResponseWriter, r *http.Request) {
	b.requestMu.Lock()
	if b.closed {
		b.requestMu.Unlock()
		http.Error(w, "credential refresh broker stopped", http.StatusServiceUnavailable)
		return
	}
	b.requests.Add(1)
	b.requestMu.Unlock()
	defer b.requests.Done()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost || r.URL.Path != "/refresh" || r.URL.RawQuery != "" {
		http.Error(w, "invalid refresh request", http.StatusBadRequest)
		return
	}
	var q struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || q.ClientID != codexOAuthClientID || q.GrantType != "refresh_token" {
		http.Error(w, "invalid refresh request", http.StatusBadRequest)
		return
	}
	// The daemon owns completing a known result even if Codex closes its request.
	ctx, cancel := context.WithTimeout(b.ctx, 60*time.Second)
	defer cancel()
	c, _, err := b.authenticate(ctx, q.RefreshToken)
	if err != nil {
		http.Error(w, "invalid refresh capability", http.StatusUnauthorized)
		return
	}
	unlock := b.locks.Lock(c.AccountID)
	defer unlock()
	c, j, err := b.authenticate(ctx, q.RefreshToken)
	if err != nil {
		http.Error(w, "invalid refresh capability", http.StatusUnauthorized)
		return
	}
	blob, err := b.refresh(ctx, c, j)
	if err != nil {
		http.Error(w, "credential refresh unavailable; retry after synchronization or reauthenticate", http.StatusServiceUnavailable)
		return
	}
	digest, err := credentialDigest(blob)
	if err != nil {
		http.Error(w, "credential refresh unavailable", http.StatusServiceUnavailable)
		return
	}
	top, _ := parseObject(codexAuthForWrite(blob))
	tokens, _ := parseTokenObject(top["tokens"])
	known := decodeKnown(tokens)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenFields{AccessToken: known.AccessToken, IDToken: known.IDToken, RefreshToken: b.signedCapability(j.Seed, c.AccountID, digest)})
}
func (b *RefreshBroker) refresh(ctx context.Context, c refreshCapability, j refreshJournal) ([]byte, error) {
	current, err := b.store.LoadCredential(ctx, c.AccountID)
	if err != nil {
		return nil, err
	}
	secretDigest, err := refreshSecretDigest(current)
	if err != nil {
		return nil, err
	}
	if j.AttemptDigest != "" {
		// Reauthentication is the only recovery for an unknown provider outcome.
		// A key roll leaves this digest unchanged and cannot authorize another POST.
		resultDigest, _ := refreshSecretDigest(j.Result)
		if secretDigest != j.AttemptDigest && secretDigest != resultDigest {
			j.AttemptDigest = ""
			j.Result = nil
			if err = b.saveJournal(ctx, c.AccountID, j); err != nil {
				return nil, err
			}
		} else if len(j.Result) == 0 {
			return nil, errors.New("credential refresh outcome unknown; reauthenticate")
		} else {
			return b.finish(ctx, c.AccountID, j)
		}
	}
	if err = b.gate.Receive(ctx, c.AccountID); err != nil {
		return nil, err
	}
	current, err = b.store.LoadCredential(ctx, c.AccountID)
	if err != nil {
		return nil, err
	}
	digest, err := credentialDigest(current)
	if err != nil {
		return nil, err
	}
	if digest != c.Digest {
		return current, nil
	}
	fingerprint, err := refreshSecretDigest(current)
	if err != nil {
		return nil, err
	}
	fingerprintBytes, err := base64.RawURLEncoding.DecodeString(fingerprint)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, j.Seed)
	_, _ = mac.Write([]byte("credential-refresh-attempt-v1\x00" + c.AccountID + "\x00" + fingerprint))
	attemptID := hex.EncodeToString(mac.Sum(nil))
	replacement, err := b.acquireOrReceive(ctx, c, fingerprintBytes, attemptID)
	if err != nil {
		return nil, err
	}
	if replacement != nil {
		return replacement, nil
	}
	// The gate binds acquisition to the local distributed version. Reload after
	// acquisition so an in-process Receive cannot leave us spending an old token.
	current, err = b.store.LoadCredential(ctx, c.AccountID)
	if err != nil {
		return nil, err
	}
	after, err := credentialDigest(current)
	if err != nil {
		return nil, err
	}
	if after != digest {
		return nil, errors.New("credential changed while acquiring refresh ownership")
	}
	j.AttemptDigest, err = refreshSecretDigest(current)
	if err != nil {
		return nil, err
	}
	j.Result = nil
	if slices.Contains(j.Consumed, j.AttemptDigest) {
		return nil, errors.New("credential refresh secret already consumed; reauthenticate")
	}
	j.Consumed = append(j.Consumed, j.AttemptDigest)
	if err = b.saveJournal(ctx, c.AccountID, j); err != nil {
		return nil, err
	}
	result, err := b.exchange(ctx, current)
	if err != nil {
		return nil, err
	}
	j.Result = result
	if err = b.saveJournal(ctx, c.AccountID, j); err != nil {
		return nil, err
	}
	return b.finish(ctx, c.AccountID, j)
}
func (b *RefreshBroker) finish(ctx context.Context, id string, j refreshJournal) ([]byte, error) {
	save := func() error {
		current, err := b.store.LoadCredential(ctx, id)
		if err != nil {
			return err
		}
		currentSecret, err := refreshSecretDigest(current)
		if err != nil {
			return err
		}
		resultSecret, _ := refreshSecretDigest(j.Result)
		if currentSecret != j.AttemptDigest && currentSecret != resultSecret {
			return errors.New("credential replaced while refresh was pending")
		}
		return b.store.SaveCredential(ctx, id, j.Result)
	}
	var err error
	if locker, ok := b.store.(CredentialStoreLocker); ok {
		err = locker.WithCredentialLock(id, save)
	} else {
		err = save()
	}
	if err != nil {
		return nil, err
	}
	consumed, err := base64.RawURLEncoding.DecodeString(j.AttemptDigest)
	if err != nil {
		return nil, err
	}
	if err := b.gate.PublishRefresh(ctx, id, j.Result, consumed); err != nil {
		return nil, err
	}
	result := j.Result
	if digest, _ := refreshSecretDigest(result); digest == j.AttemptDigest {
		j.Consumed = slices.DeleteFunc(j.Consumed, func(x string) bool { return x == j.AttemptDigest })
	}
	j.AttemptDigest = ""
	j.Result = nil
	if err := b.saveJournal(ctx, id, j); err != nil {
		return nil, err
	}
	return result, nil
}
func (b *RefreshBroker) exchange(ctx context.Context, blob []byte) ([]byte, error) {
	top, _ := parseObject(codexAuthForWrite(blob))
	tokens, _ := parseTokenObject(top["tokens"])
	known := decodeKnown(tokens)
	request, _ := json.Marshal(map[string]string{"client_id": codexOAuthClientID, "grant_type": "refresh_token", "refresh_token": known.RefreshToken})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, b.providerURL, strings.NewReader(string(request)))
	if err != nil {
		return nil, errors.New("credential exchange unavailable")
	}
	r.Header.Set("Content-Type", "application/json")
	out, err := b.client.Do(r)
	if err != nil {
		return nil, errors.New("credential exchange outcome unknown")
	}
	defer func() { _ = out.Body.Close() }()
	if out.StatusCode != http.StatusOK {
		return nil, errors.New("credential exchange unsuccessful; reauthenticate")
	}
	raw, err := io.ReadAll(io.LimitReader(out.Body, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, errors.New("credential exchange result unreadable")
	}
	var refreshed tokenFields
	if json.Unmarshal(raw, &refreshed) != nil || refreshed.AccessToken == "" {
		return nil, errors.New("credential exchange result invalid")
	}
	next, _ := json.Marshal(map[string]any{"tokens": refreshed, "last_refresh": time.Now().UTC().Format(time.RFC3339Nano)})
	merged, err := mergePreservingIDToken(blob, next)
	if err != nil {
		return nil, fmt.Errorf("credential exchange merge failed")
	}
	return merged, nil
}

func brokerAuth(blob []byte, capability string) ([]byte, error) {
	top, err := parseObject(codexAuthForWrite(blob))
	if err != nil {
		return nil, errors.New("credential auth invalid")
	}
	tokens, err := parseTokenObject(top["tokens"])
	if err != nil {
		return nil, err
	}
	setKnown(tokens, "refresh_token", capability)
	top["tokens"], _ = json.Marshal(tokens)
	for _, key := range []string{"refresh", "refresh_token"} {
		delete(top, key)
	}
	if nested, err := parseTokenObject(top["openai"]); err == nil && nested != nil {
		delete(nested, "refresh")
		delete(nested, "refresh_token")
		top["openai"], _ = json.Marshal(nested)
	}
	return json.Marshal(top)
}

func (b *RefreshBroker) acquireOrReceive(ctx context.Context, c refreshCapability, fingerprint []byte, attemptID string) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, b.peerWait)
	defer cancel()
	for {
		owned, err := b.gate.Acquire(waitCtx, c.AccountID, fingerprint, attemptID)
		if err != nil {
			return nil, err
		}
		if owned {
			return nil, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return nil, ErrRefreshOwned
		case <-timer.C:
		}
		if err := b.gate.Receive(waitCtx, c.AccountID); err != nil {
			return nil, err
		}
		current, err := b.store.LoadCredential(waitCtx, c.AccountID)
		if err != nil {
			return nil, err
		}
		digest, err := credentialDigest(current)
		if err != nil {
			return nil, err
		}
		if digest != c.Digest {
			return current, nil
		}
	}
}
