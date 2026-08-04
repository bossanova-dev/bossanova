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
