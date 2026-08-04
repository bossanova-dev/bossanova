package auth

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/99designs/keyring"
)

func TestKeychainStoreMigratesWhenFileBackendLegacyKeyIsAbsent(t *testing.T) {
	ring := &fileLikeTokenKeyring{items: map[string]keyring.Item{}}
	store := &KeychainStore{ring: ring}
	tokens := &Tokens{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}

	if err := store.Save(tokens); err != nil {
		t.Fatalf("Save() error = %v, want nil when the legacy file is absent", err)
	}
	if _, ok := ring.items[versionedTokenKey]; !ok {
		t.Fatal("Save() did not write the authoritative record")
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete() error = %v, want nil after migration", err)
	}
	if _, ok := ring.items[versionedTokenKey]; ok {
		t.Fatal("Delete() left the authoritative record behind")
	}
}

func TestKeychainStoreLoadsLegacyTokensWhenFileBackendVersionedKeyIsAbsent(t *testing.T) {
	want := Tokens{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal legacy tokens: %v", err)
	}
	ring := &fileLikeTokenKeyring{items: map[string]keyring.Item{
		tokenKey: {Key: tokenKey, Data: data},
	}}

	got, err := (&KeychainStore{ring: ring}).Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want legacy fallback", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("Load() = %+v, want %+v", *got, want)
	}
}

func TestKeychainStoreTreatsMissingFileRecordsAsSignedOut(t *testing.T) {
	_, err := (&KeychainStore{ring: &fileLikeTokenKeyring{items: map[string]keyring.Item{}}}).Load()
	if err == nil {
		t.Fatal("Load() error = nil, want missing-record error")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load() error = %v, want fs.ErrNotExist", err)
	}
	if errors.Is(err, ErrCredentialsUnreadable) {
		t.Fatalf("Load() error = %v, missing records must not be unreadable credentials", err)
	}
}

// fileLikeTokenKeyring mirrors the file backend's missing-item behavior:
// Remove reports fs.ErrNotExist rather than keyring.ErrKeyNotFound.
type fileLikeTokenKeyring struct {
	items map[string]keyring.Item
}

func (r *fileLikeTokenKeyring) Get(key string) (keyring.Item, error) {
	item, ok := r.items[key]
	if !ok {
		return keyring.Item{}, fs.ErrNotExist
	}
	return item, nil
}

func (r *fileLikeTokenKeyring) GetMetadata(key string) (keyring.Metadata, error) {
	item, err := r.Get(key)
	if err != nil {
		return keyring.Metadata{}, err
	}
	return keyring.Metadata{Item: &item}, nil
}

func (r *fileLikeTokenKeyring) Set(item keyring.Item) error {
	r.items[item.Key] = item
	return nil
}

func (r *fileLikeTokenKeyring) Remove(key string) error {
	if _, ok := r.items[key]; !ok {
		return fs.ErrNotExist
	}
	delete(r.items, key)
	return nil
}

func (r *fileLikeTokenKeyring) Keys() ([]string, error) {
	keys := make([]string, 0, len(r.items))
	for key := range r.items {
		keys = append(keys, key)
	}
	return keys, nil
}
