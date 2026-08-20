package upstream

import (
	"encoding/json"
	"io/fs"
	"testing"
	"time"

	"github.com/99designs/keyring"
)

func TestSaveKeychainTokensAllowsAbsentLegacyFileRecord(t *testing.T) {
	ring := &fileLikeKeyring{items: map[string]keyring.Item{}}
	originalOpen := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = originalOpen })

	if err := saveKeychainTokens(&keychainTokens{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("saveKeychainTokens() error = %v, want nil when the legacy file is absent", err)
	}
	if _, ok := ring.items[versionedTokenKey]; !ok {
		t.Fatal("saveKeychainTokens() did not write the authoritative record")
	}
}

func TestLoadKeychainTokensFallsBackWhenVersionedFileRecordIsAbsent(t *testing.T) {
	want := keychainTokens{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal legacy tokens: %v", err)
	}
	ring := &fileLikeKeyring{items: map[string]keyring.Item{
		legacyTokenKey: {Key: legacyTokenKey, Data: data},
	}}
	originalOpen := openKeyring
	openKeyring = func() (keyring.Keyring, error) { return ring, nil }
	t.Cleanup(func() { openKeyring = originalOpen })

	got, err := loadKeychainTokens()
	if err != nil {
		t.Fatalf("loadKeychainTokens() error = %v, want legacy fallback", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("loadKeychainTokens() = %+v, want %+v", *got, want)
	}
}

// fileLikeKeyring mirrors the file backend's os.Remove behavior for a missing
// legacy record, while Get continues to expose the keyring interface shape.
type fileLikeKeyring struct {
	items map[string]keyring.Item
}

func (r *fileLikeKeyring) Get(key string) (keyring.Item, error) {
	item, ok := r.items[key]
	if !ok {
		return keyring.Item{}, fs.ErrNotExist
	}
	return item, nil
}

func (r *fileLikeKeyring) GetMetadata(key string) (keyring.Metadata, error) {
	item, err := r.Get(key)
	if err != nil {
		return keyring.Metadata{}, err
	}
	return keyring.Metadata{Item: &item}, nil
}

func (r *fileLikeKeyring) Set(item keyring.Item) error {
	r.items[item.Key] = item
	return nil
}

func (r *fileLikeKeyring) Remove(key string) error {
	if _, ok := r.items[key]; !ok {
		return fs.ErrNotExist
	}
	delete(r.items, key)
	return nil
}

func (r *fileLikeKeyring) Keys() ([]string, error) {
	keys := make([]string, 0, len(r.items))
	for key := range r.items {
		keys = append(keys, key)
	}
	return keys, nil
}

// --- CredentialVerdict (BOS-945) ---

// TestCredentialVerdict covers every combination the post-login gate has to
// tell apart. The two failure causes are deliberately distinguishable: a
// flagged record needs a fresh login, whereas an empty record with no marker
// usually means the CLI wrote to a keyring backend this daemon does not read.
func TestCredentialVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		accessToken   string
		reloginReason string
		wantUsable    bool
		wantReason    string
	}{
		{
			name:        "token and no marker is usable",
			accessToken: "access-token-fixture",
			wantUsable:  true,
		},
		{
			name:          "marker alone disables the record",
			reloginReason: reloginReasonRefreshTokenRejected,
			wantReason:    reloginReasonRefreshTokenRejected,
		},
		{
			name:          "a marker disables the record even with a token still cached",
			accessToken:   "stale-access-token-fixture",
			reloginReason: reloginReasonRefreshOutcomeUnknown,
			wantReason:    reloginReasonRefreshOutcomeUnknown,
		},
		{
			name: "no token and no marker is unusable with no reason to report",
		},
		{
			name:          "an unrecognised marker still disables the record",
			accessToken:   "access-token-fixture",
			reloginReason: "some_future_reason",
			wantReason:    "some_future_reason",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &KeychainTokenProvider{accessToken: tc.accessToken, reloginReason: tc.reloginReason}

			usable, reason := p.CredentialVerdict()
			if usable != tc.wantUsable {
				t.Errorf("usable = %t, want %t", usable, tc.wantUsable)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if reason == tc.accessToken && tc.accessToken != "" {
				t.Fatal("CredentialVerdict returned token material as the reason")
			}
		})
	}
}

// TestCredentialVerdictIsAtomicUnderRefresh is the reason this accessor exists
// rather than composing ReloginReason() and Token() at the call site: a
// concurrent refresh commits both fields together, so reading them together
// must never observe a token from before the flag or a flag from after the
// token. Run under -race, this also proves the read takes the lock.
func TestCredentialVerdictIsAtomicUnderRefresh(t *testing.T) {
	t.Parallel()

	p := &KeychainTokenProvider{accessToken: "initial-access-fixture"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			p.mu.Lock()
			// Mirror applyTokensLocked: both fields move as one write.
			p.accessToken, p.reloginReason = "rotated-access-fixture", ""
			p.accessToken, p.reloginReason = "", reloginReasonRefreshTokenRejected
			p.mu.Unlock()
		}
	}()

	for i := 0; i < 500; i++ {
		usable, reason := p.CredentialVerdict()
		// The two states written above are the only coherent ones. "unusable
		// with neither field set" would mean the read had torn across the
		// write. The "usable with a reason" arm below is structurally
		// impossible — usable is DEFINED as reloginReason == "" && token != ""
		// — so it guards the definition, not the tearing; under -race the real
		// proof in this test is that the read takes mu.RLock at all.
		if usable && reason != "" {
			t.Fatalf("torn read: usable with reason %q", reason)
		}
	}
	<-done
}
