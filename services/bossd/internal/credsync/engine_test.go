package credsync

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"

	"github.com/recurser/bossd/internal/devicekey"
)

type syncCloud struct {
	devices                         []Device
	generation                      int64
	seals                           map[string][]byte
	blobs                           map[string]Blob
	rows                            []AgentAccountMetadata
	caller                          string
	rolls                           int
	conflict                        bool
	rollErrorBefore, rollErrorAfter bool
}

func (c *syncCloud) ListDevices(context.Context) ([]Device, error) { return c.devices, nil }
func (c *syncCloud) GetKeySeal(context.Context) (int64, []byte, error) {
	return c.generation, c.seals[c.caller], nil
}
func (c *syncCloud) PutKeySeals(_ context.Context, expected, generation int64, seals []KeySeal) error {
	if c.generation != expected {
		return ErrConflict
	}
	c.generation = generation
	for _, s := range seals {
		if _, exists := c.seals[s.DaemonID]; !exists {
			c.seals[s.DaemonID] = s.SealedCredentialKey
		}
	}
	return nil
}
func (c *syncCloud) GetBlob(_ context.Context, id string) (Blob, error) {
	b, ok := c.blobs[id]
	if !ok {
		return Blob{}, ErrNotFound
	}
	return b, nil
}
func (c *syncCloud) PutBlob(_ context.Context, expected int64, b Blob) error {
	if c.conflict || c.blobs[b.SyncedAgentAccountID].CredentialVersion != expected {
		return ErrConflict
	}
	c.blobs[b.SyncedAgentAccountID] = b
	return nil
}
func (c *syncCloud) ListAgentAccounts(context.Context) ([]AgentAccountMetadata, error) {
	return c.rows, nil
}
func (c *syncCloud) RollKey(_ context.Context, expected, generation int64, seals []KeySeal, blobs []Blob) error {
	if c.rollErrorBefore {
		c.rollErrorBefore = false
		return errors.New("transport interrupted before commit")
	}
	if c.generation != expected {
		return ErrConflict
	}
	for _, b := range blobs {
		if b.CredentialVersion != c.blobs[b.SyncedAgentAccountID].CredentialVersion+1 {
			return ErrConflict
		}
	}
	c.generation = generation
	c.seals = map[string][]byte{}
	for _, s := range seals {
		c.seals[s.DaemonID] = s.SealedCredentialKey
	}
	for _, b := range blobs {
		c.blobs[b.SyncedAgentAccountID] = b
	}
	c.rolls++
	if c.rollErrorAfter {
		c.rollErrorAfter = false
		return errors.New("response lost after commit")
	}
	return nil
}

type syncLocal struct {
	rows      map[string]LocalCredential
	secrets   map[string][]byte
	installed AgentAccountMetadata
}

func (l *syncLocal) Lookup(_ context.Context, id string) (LocalCredential, error) {
	r, ok := l.rows[id]
	if !ok {
		return r, ErrNotFound
	}
	return r, nil
}
func (l *syncLocal) Read(_ context.Context, id string) ([]byte, error) {
	b, ok := l.secrets[id]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}
func (l *syncLocal) Install(_ context.Context, m AgentAccountMetadata, b []byte, v int64) error {
	row, ok := l.rows[m.SyncedAgentAccountID]
	if ok && !row.IsAuthInvalid && row.SyncedCredentialVersion == 0 {
		return nil
	}
	id := "received-" + m.SyncedAgentAccountID
	if ok {
		id = row.LocalID
	}
	l.rows[m.SyncedAgentAccountID] = LocalCredential{LocalID: id, SyncedCredentialVersion: v}
	l.secrets[id] = bytes.Clone(b)
	l.installed = m
	return nil
}
func (l *syncLocal) SetVersion(_ context.Context, id string, v int64) error {
	for syncID, row := range l.rows {
		if row.LocalID == id {
			row.SyncedCredentialVersion = v
			l.rows[syncID] = row
			return nil
		}
	}
	return ErrNotFound
}

type syncPins map[string]ed25519.PublicKey

func (p syncPins) PinnedIdentity(id string) (ed25519.PublicKey, error) {
	if k, ok := p[id]; ok {
		return k, nil
	}
	return nil, ErrNotFound
}

type syncFixture struct {
	cloud    *syncCloud
	a, b     *Engine
	la, lb   *syncLocal
	privateB *ecdh.PrivateKey
	signer   ed25519.PrivateKey
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	pub, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approval := func(id string, key *ecdh.PrivateKey) []byte {
		sig, e := devicekey.SignApproval(signer, devicekey.Approval{ApproverDaemonID: "a", DaemonID: id, DevicePub: key.PublicKey().Bytes()})
		if e != nil {
			t.Fatal(e)
		}
		return sig
	}
	c := &syncCloud{devices: []Device{{DaemonID: "a", Status: "enrolled", ApprovedByDaemonID: "a", DevicePublicKey: a.PublicKey().Bytes(), IdentityPublicKey: pub, ApprovalSignature: approval("a", a)}, {DaemonID: "b", Status: "enrolled", ApprovedByDaemonID: "a", DevicePublicKey: b.PublicKey().Bytes(), IdentityPublicKey: pub, ApprovalSignature: approval("b", b)}}, seals: map[string][]byte{}, blobs: map[string]Blob{}, rows: []AgentAccountMetadata{{SyncedAgentAccountID: "account", Provider: "claude", Label: "work", DesiredStatus: "enabled", Priority: 3, IsInScope: true}}}
	local := func() *syncLocal { return &syncLocal{rows: map[string]LocalCredential{}, secrets: map[string][]byte{}} }
	la, lb := local(), local()
	f := &syncFixture{cloud: c, la: la, lb: lb, privateB: b, signer: signer}
	f.a = NewEngine(Config{UserID: "u", DaemonID: "a", BootstrapDaemonID: "a", DeviceKey: a, IdentityKey: signer}, c, la, &syncTrustCache{syncPins: syncPins{"a": pub, "b": pub}, records: map[string]Device{}, seals: map[int64][]byte{}})
	f.b = NewEngine(Config{UserID: "u", DaemonID: "b", BootstrapDaemonID: "a", DeviceKey: b, IdentityKey: signer}, c, lb, &syncTrustCache{syncPins: syncPins{"a": pub, "b": pub}, records: map[string]Device{}, seals: map[int64][]byte{}})
	la.rows["account"] = LocalCredential{LocalID: "original"}
	la.secrets["original"] = []byte("oauth-secret")
	return f
}
func TestDistributionAndReceiveProtection_Accounts_AC23(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	if f.cloud.generation != 1 {
		t.Fatal("bootstrap did not create generation one")
	}
	f.cloud.caller = "b"
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if string(f.lb.secrets["received-account"]) != "oauth-secret" || f.lb.installed.DesiredStatus != "enabled" || f.lb.installed.Priority != 3 {
		t.Fatal("credential and desired registry state not installed")
	}
	f.lb.rows["account"] = LocalCredential{LocalID: "independent"}
	f.lb.secrets["independent"] = []byte("deliberate-local")
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if string(f.lb.secrets["independent"]) != "deliberate-local" {
		t.Fatal("independent copy overwritten")
	}
	row := f.lb.rows["account"]
	row.IsAuthInvalid = true
	f.lb.rows["account"] = row
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if string(f.lb.secrets["independent"]) != "oauth-secret" {
		t.Fatal("invalid copy not replaced")
	}
	row = f.lb.rows["account"]
	row.SyncedCredentialVersion = 2
	f.lb.rows["account"] = row
	if err := f.b.Receive(ctx, "account"); !errors.Is(err, ErrRollback) {
		t.Fatalf("rollback: %v", err)
	}
}
func TestForgedApprovalGetsNoSeal(t *testing.T) {
	f := newSyncFixture(t)
	f.cloud.devices[1].ApprovalSignature = make([]byte, 64)
	f.cloud.caller = "a"
	if err := f.a.EnsureCredentialKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cloud.seals["b"]) != 0 {
		t.Fatal("server-enrolled forged device got seal")
	}
	f.cloud.caller = "b"
	if err := f.b.EnsureCredentialKey(context.Background()); err == nil {
		t.Fatal("forged device became usable")
	}
}
func TestRevocationReencryptsAtomically_Accounts_AC24(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	oldVersion := f.cloud.blobs["account"].CredentialVersion
	old := bytes.Clone(f.cloud.blobs["account"].Ciphertext)
	f.cloud.devices[1].Status = "revoked"
	if err := f.a.Roll(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if f.cloud.rolls != 1 || f.cloud.generation != 2 || len(f.cloud.seals["b"]) != 0 || bytes.Equal(old, f.cloud.blobs["account"].Ciphertext) {
		t.Fatal("roll did not atomically replace seals and blobs")
	}
	if _, err := OpenCredentialKey(f.privateB, f.cloud.seals["a"], "u", 2); err == nil {
		t.Fatal("revoked key opened survivor seal")
	}
	var envelope signedKeySeal
	if err := json.Unmarshal(f.cloud.seals["a"], &envelope); err != nil {
		t.Fatal(err)
	}
	key, err := OpenCredentialKey(f.a.config.DeviceKey, envelope.Box, "u", 2)
	if err != nil {
		t.Fatal(err)
	}
	blob := f.cloud.blobs["account"]
	if blob.CredentialVersion != oldVersion+1 {
		t.Fatal("rotation did not advance credential version")
	}
	plain, err := DecryptCredential(key, blob.Ciphertext, f.a.aad(blob))
	if err != nil || string(plain) != "oauth-secret" {
		t.Fatalf("survivor lost credential: %v", err)
	}
}
func TestBossoDataAloneCannotDecrypt(t *testing.T) {
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	if err := f.a.Publish(context.Background(), "account", "original"); err != nil {
		t.Fatal(err)
	}
	attacker, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, seal := range f.cloud.seals {
		var envelope signedKeySeal
		if err := json.Unmarshal(seal, &envelope); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenCredentialKey(attacker, envelope.Box, "u", 1); err == nil {
			t.Fatal("stored server seal opened without enrolled private key")
		}
	}
	blob := f.cloud.blobs["account"]
	if _, err := DecryptCredential(make([]byte, 32), blob.Ciphertext, f.a.aad(blob)); err == nil {
		t.Fatal("server blob opened without key")
	}
}
func TestReconnectSeesRevocation(t *testing.T) {
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	if err := f.a.Publish(context.Background(), "account", "original"); err != nil {
		t.Fatal(err)
	}
	f.cloud.devices[1].Status = "revoked"
	if err := f.a.EnsureCredentialKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cloud.generation != 2 {
		t.Fatal("reconnect did not roll revoked device")
	}
	if err := f.a.EnsureCredentialKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.cloud.generation != 2 {
		t.Fatal("already observed revocation rolled again")
	}
}

func TestStalePublisherReceivesWinningVersion(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	f.cloud.caller = "b"
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	f.cloud.caller = "a"
	f.la.secrets["original"] = []byte("owner-refresh")
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	f.cloud.caller = "b"
	f.lb.secrets["received-account"] = []byte("stale-refresh")
	if err := f.b.Publish(ctx, "account", "received-account"); err != nil {
		t.Fatal(err)
	}
	if f.cloud.blobs["account"].CredentialVersion != 2 {
		t.Fatal("stale writer overwrote winning credential")
	}
	if string(f.lb.secrets["received-account"]) != "owner-refresh" {
		t.Fatal("stale writer did not receive winning credential")
	}
}

type syncTrustCache struct {
	syncPins
	records map[string]Device
	seals   map[int64][]byte
	highest int64
	pending *KeyRotation
}

func (c *syncTrustCache) ApprovedDevice(id string) (Device, error) {
	d, ok := c.records[id]
	if !ok {
		return d, ErrNotFound
	}
	return d, nil
}
func (c *syncTrustCache) SaveApprovedDevice(d Device) error { c.records[d.DaemonID] = d; return nil }

func TestRevokedBootstrapKeepsKnownSurvivorsAndRejectsFreshSignatures(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	cache := &syncTrustCache{syncPins: syncPins{"a": f.cloud.devices[0].IdentityPublicKey, "b": f.cloud.devices[1].IdentityPublicKey}, records: map[string]Device{}, seals: map[int64][]byte{}}
	f.b.trust = cache
	f.cloud.caller = "b"
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	// Restart loses memory, retaining only locally persisted authenticated evidence.
	f.b = NewEngine(f.b.config, f.cloud, f.lb, cache)
	f.cloud.devices[0].Status = "revoked"
	c, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := devicekey.SignApproval(f.signer, devicekey.Approval{ApproverDaemonID: "a", DaemonID: "c", DevicePub: c.PublicKey().Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	f.cloud.devices = append(f.cloud.devices, Device{DaemonID: "c", Status: "enrolled", ApprovedByDaemonID: "a", DevicePublicKey: c.PublicKey().Bytes(), IdentityPublicKey: f.cloud.devices[0].IdentityPublicKey, ApprovalSignature: forged})
	if err := f.b.EnsureCredentialKey(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cloud.generation != 2 || len(f.cloud.seals["a"]) != 0 || len(f.cloud.seals["c"]) != 0 || len(f.cloud.seals["b"]) == 0 {
		t.Fatal("revoked bootstrap granted new forged recipient or lost known survivor")
	}
	if err := f.b.Receive(ctx, "account"); err != nil {
		t.Fatal(err)
	}
}

func TestNewApprovedDeviceGetsCurrentGenerationSeal(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	approved := f.cloud.devices[1]
	f.cloud.devices = f.cloud.devices[:1]
	f.cloud.caller = "a"
	if err := f.a.EnsureCredentialKey(ctx); err != nil {
		t.Fatal(err)
	}
	f.cloud.devices = append(f.cloud.devices, approved)
	if err := f.a.EnsureCredentialKey(ctx); err != nil {
		t.Fatal(err)
	}
	if f.cloud.generation != 1 {
		t.Fatal("approving device unexpectedly changed generation")
	}
	f.cloud.caller = "b"
	if err := f.b.EnsureCredentialKey(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServerCannotSwapRegistryAccountBlob(t *testing.T) {
	f := newSyncFixture(t)
	ctx := context.Background()
	f.cloud.caller = "a"
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	// The server returns a valid account ciphertext under another requested id.
	f.cloud.blobs["other"] = f.cloud.blobs["account"]
	f.cloud.rows = append(f.cloud.rows, AgentAccountMetadata{SyncedAgentAccountID: "other", IsInScope: true})
	f.cloud.caller = "b"
	if err := f.b.Receive(ctx, "other"); err == nil {
		t.Fatal("cross-account ciphertext installed under wrong registry row")
	}
	if _, ok := f.lb.rows["other"]; ok {
		t.Fatal("swapped credential was installed")
	}
}

func (c *syncTrustCache) AcceptedKeySeal(g int64) ([]byte, error) {
	s, ok := c.seals[g]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(s), nil
}
func (c *syncTrustCache) HighestKeyGeneration() (int64, error) { return c.highest, nil }
func (c *syncTrustCache) SaveAcceptedKeySeal(g int64, s []byte) error {
	if g < c.highest {
		return ErrRollback
	}
	if c.seals == nil {
		c.seals = map[int64][]byte{}
	}
	c.seals[g] = bytes.Clone(s)
	c.highest = g
	return nil
}

func (c *syncTrustCache) PendingRotation() (*KeyRotation, error) {
	if c.pending == nil {
		return nil, nil
	}
	data, _ := json.Marshal(c.pending)
	var p KeyRotation
	_ = json.Unmarshal(data, &p)
	return &p, nil
}
func (c *syncTrustCache) SavePendingRotation(p KeyRotation) error {
	if err := c.SaveAcceptedKeySeal(p.KeyGeneration, p.OwnSeal); err != nil {
		return err
	}
	data, _ := json.Marshal(p)
	var copy KeyRotation
	_ = json.Unmarshal(data, &copy)
	c.pending = &copy
	return nil
}
func (c *syncTrustCache) ClearPendingRotation() error { c.pending = nil; return nil }

func TestRefreshPublicationRetainsKnownResultAfterConflictAndRotation(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	f.la.secrets["original"] = []byte(`{"tokens":{"refresh_token":"consumed"}}`)
	consumed := sha256.Sum256([]byte("consumed"))
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	fresh := []byte(`{"tokens":{"refresh_token":"fresh-rotating-credential"}}`)
	f.la.secrets["original"] = fresh
	f.cloud.conflict = true
	if err := f.a.PublishRefresh(ctx, "account", "original", fresh, consumed[:]); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict=%v", err)
	}
	if !bytes.Equal(f.la.secrets["original"], fresh) {
		t.Fatal("conflict replaced known provider result with spent remote credential")
	}
	f.cloud.conflict = false
	// A key rotation may advance the remote version while the result is pending.
	if err := f.a.roll(ctx, "b", f.cloud.devices[:1]); err != nil {
		t.Fatal(err)
	}
	if err := f.a.PublishRefresh(ctx, "account", "original", fresh, consumed[:]); err != nil {
		t.Fatal(err)
	}
	f.cloud.caller = "b"
	blob := f.cloud.blobs["account"]
	plain, err := DecryptCredential(f.a.key, blob.Ciphertext, f.a.aad(blob))
	if err != nil || !bytes.Equal(plain, fresh) {
		t.Fatal("retry did not publish known result at the current key generation")
	}
}

func TestRefreshPublicationPreservesPeerReauthentication(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	f.la.secrets["original"] = []byte(`{"tokens":{"refresh_token":"consumed"}}`)
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	peer := []byte(`{"tokens":{"refresh_token":"peer-reauth"}}`)
	current := f.cloud.blobs["account"]
	current.CredentialVersion++
	var err error
	current.Ciphertext, err = EncryptCredential(f.a.key, peer, f.a.aad(current))
	if err != nil {
		t.Fatal(err)
	}
	f.cloud.blobs["account"] = current
	fresh := []byte(`{"tokens":{"refresh_token":"known-result"}}`)
	consumed := sha256.Sum256([]byte("consumed"))
	if err := f.a.PublishRefresh(ctx, "account", "original", fresh, consumed[:]); !errors.Is(err, ErrCredentialReplaced) {
		t.Fatalf("replacement error=%v", err)
	}
	if !bytes.Equal(f.cloud.blobs["account"].Ciphertext, current.Ciphertext) {
		t.Fatal("peer ciphertext changed")
	}
}

func TestResolveRefreshVersionPreservesIndependentCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, local string
		wantVersion int64
	}{
		{"matching original", `{"tokens":{"refresh_token":"distributed"}}`, 1},
		{"independent authentication", `{"tokens":{"refresh_token":"independent"}}`, 0},
		{"legacy top level", `{"refresh":"distributed"}`, 1},
		{"legacy openai", `{"openai":{"refresh_token":"distributed"}}`, 1},
		{"openai promotion", `{"openai":{"refresh_token":"distributed"},"refresh":"independent"}`, 1},
		{"partial tokens use top fallback", `{"tokens":{"access_token":"access"},"openai":{"refresh_token":"independent"},"refresh":"distributed"}`, 1},
		{"nested takes precedence", `{"tokens":{"refresh_token":"independent"},"refresh":"distributed"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newSyncFixture(t)
			f.cloud.caller = "a"
			f.la.secrets["original"] = []byte(`{"tokens":{"refresh_token":"distributed"}}`)
			if err := f.a.Publish(ctx, "account", "original"); err != nil {
				t.Fatal(err)
			}
			row := f.la.rows["account"]
			row.SyncedCredentialVersion = 0
			f.la.rows["account"] = row
			f.la.secrets["original"] = []byte(tc.local)
			guard, err := f.a.ResolveRefreshVersion(ctx, "account", "original")
			if err != nil || guard != 1 {
				t.Fatalf("guard=%d err=%v", guard, err)
			}
			if f.la.rows["account"].SyncedCredentialVersion != tc.wantVersion {
				t.Fatalf("version=%d", f.la.rows["account"].SyncedCredentialVersion)
			}
			if string(f.la.secrets["original"]) != tc.local {
				t.Fatal("independent credential modified")
			}
		})
	}
}

func TestResolveRefreshVersionRejectsUnauthenticatedRemote(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	f.la.secrets["original"] = []byte(`{"tokens":{"refresh_token":"distributed"}}`)
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	row := f.la.rows["account"]
	row.SyncedCredentialVersion = 0
	f.la.rows["account"] = row
	blob := f.cloud.blobs["account"]
	blob.Ciphertext[0] ^= 1
	f.cloud.blobs["account"] = blob
	if _, err := f.a.ResolveRefreshVersion(ctx, "account", "original"); err == nil {
		t.Fatal("unauthenticated credential adopted")
	}
	if f.la.rows["account"].SyncedCredentialVersion != 0 {
		t.Fatal("invalid ciphertext claimed a version")
	}
}

func TestPublishRefreshRecoversCommittedResultWithoutReceiving(t *testing.T) {
	ctx := context.Background()
	f := newSyncFixture(t)
	f.cloud.caller = "a"
	fresh := []byte(`{"tokens":{"refresh_token":"new-result"}}`)
	f.la.secrets["original"] = fresh
	if err := f.a.Publish(ctx, "account", "original"); err != nil {
		t.Fatal(err)
	}
	consumed := sha256.Sum256([]byte("consumed"))
	if err := f.a.PublishRefresh(ctx, "account", "original", fresh, consumed[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.la.secrets["original"], fresh) {
		t.Fatal("known result changed")
	}
}
