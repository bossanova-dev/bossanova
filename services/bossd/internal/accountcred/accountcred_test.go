package accountcred

import (
	"errors"
	"strings"
	"testing"

	"github.com/99designs/keyring"
)

// fakeKeyring is an in-memory credKeyring for tests: it never touches a real
// keyring. When failAll is set every operation returns a generic (non
// ErrKeyNotFound) error, modeling a keyring op failure.
type fakeKeyring struct {
	data    map[string][]byte
	failAll bool
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{data: map[string][]byte{}}
}

func (f *fakeKeyring) Get(key string) (keyring.Item, error) {
	if f.failAll {
		return keyring.Item{}, errors.New("keyring op failed")
	}
	v, ok := f.data[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return keyring.Item{Key: key, Data: v}, nil
}

func (f *fakeKeyring) Set(item keyring.Item) error {
	if f.failAll {
		return errors.New("keyring op failed")
	}
	f.data[item.Key] = item.Data
	return nil
}

func (f *fakeKeyring) Remove(key string) error {
	if f.failAll {
		return errors.New("keyring op failed")
	}
	if _, ok := f.data[key]; !ok {
		return keyring.ErrKeyNotFound
	}
	delete(f.data, key)
	return nil
}

// storeOver builds a Store over the given fake keyring.
func storeOver(f *fakeKeyring) *Store {
	return &Store{open: func() (credKeyring, error) { return f, nil }}
}

// storeOpenErr builds a Store whose keyring open always fails.
func storeOpenErr(err error) *Store {
	return &Store{open: func() (credKeyring, error) { return nil, err }}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := storeOver(newFakeKeyring())
	blob := []byte("sk-ant-SECRET-should-round-trip")

	if err := s.Save("acct-1", blob); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load("acct-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("Load = %q, want %q", got, blob)
	}
}

func TestLoadMissing(t *testing.T) {
	s := storeOver(newFakeKeyring())
	_, err := s.Load("no-such-account")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("Load(missing) err = %v, want ErrCredentialNotFound", err)
	}
}

func TestDeleteThenLoad(t *testing.T) {
	s := storeOver(newFakeKeyring())
	if err := s.Save("acct-1", []byte("blob")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("acct-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load("acct-1"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("Load after Delete err = %v, want ErrCredentialNotFound", err)
	}
}

func TestDeleteMissing(t *testing.T) {
	s := storeOver(newFakeKeyring())
	err := s.Delete("no-such-account")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("Delete(missing) err = %v, want ErrCredentialNotFound", err)
	}
}

func TestSaveOverwrites(t *testing.T) {
	s := storeOver(newFakeKeyring())
	if err := s.Save("acct-1", []byte("first")); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	if err := s.Save("acct-1", []byte("second")); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	got, err := s.Load("acct-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("Load = %q, want %q", got, "second")
	}
}

func TestKeyringOpenFailure(t *testing.T) {
	s := storeOpenErr(errors.New("keyring locked"))

	if err := s.Save("acct-1", []byte("blob")); err == nil {
		t.Error("Save with open failure: expected error, got nil")
	}
	if _, err := s.Load("acct-1"); err == nil {
		t.Error("Load with open failure: expected error, got nil")
	}
	if err := s.Delete("acct-1"); err == nil {
		t.Error("Delete with open failure: expected error, got nil")
	}
}

func TestKeyringOpError(t *testing.T) {
	f := newFakeKeyring()
	f.failAll = true
	s := storeOver(f)

	if err := s.Save("acct-1", []byte("blob")); err == nil {
		t.Error("Save with op error: expected error, got nil")
	}
	if _, err := s.Load("acct-1"); err == nil {
		t.Error("Load with op error: expected error, got nil")
	}
	if err := s.Delete("acct-1"); err == nil {
		t.Error("Delete with op error: expected error, got nil")
	}
}

// TestSaveErrorNeverLeaksBlob asserts a returned error string never contains the
// credential blob bytes — the core confidentiality gate for this store.
func TestSaveErrorNeverLeaksBlob(t *testing.T) {
	const secret = "sk-ant-TOPSECRET-should-never-be-in-an-error"
	f := newFakeKeyring()
	f.failAll = true
	s := storeOver(f)

	err := s.Save("acct-1", []byte(secret))
	if err == nil {
		t.Fatal("Save with op error: expected error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("blob leaked into error string: %q", err.Error())
	}
}
