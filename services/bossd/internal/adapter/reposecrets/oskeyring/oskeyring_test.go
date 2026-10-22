package oskeyring

import (
	"context"
	"errors"
	"testing"

	"github.com/99designs/keyring"

	"github.com/recurser/bossd/internal/adapter/reposecrets"
)

// openFileRing opens a real 99designs file-backend keyring under a test temp
// dir, the backend headless Linux uses by default (and macOS under
// BOSS_KEYRING_BACKEND=file), wrapped in the daemon's adapter.
func openFileRing(t *testing.T) reposecrets.Ring {
	t.Helper()
	ring, err := keyring.Open(keyring.Config{
		ServiceName:      "bossanova-test",
		AllowedBackends:  []keyring.BackendType{keyring.FileBackend},
		FileDir:          t.TempDir(),
		FilePasswordFunc: keyring.FixedStringPrompt("test-passphrase"),
	})
	if err != nil {
		t.Fatalf("open file keyring: %v", err)
	}
	return adapter{ring: ring}
}

// TestAdapter_FileBackendAbsentItemIsNotFound: on the file backend, reading or
// removing an item that is not there must read as reposecrets.ErrNotFound, so
// clearing keys or deleting a repo whose item is already gone succeeds instead
// of failing on the backend's raw "no such file or directory".
func TestAdapter_FileBackendAbsentItemIsNotFound(t *testing.T) {
	ring := openFileRing(t)
	key := reposecrets.ItemKey("repo-1")

	if _, err := ring.Get(key); !errors.Is(err, reposecrets.ErrNotFound) {
		t.Fatalf("Get of an absent item: err = %v, want ErrNotFound", err)
	}
	if err := ring.Remove(key); !errors.Is(err, reposecrets.ErrNotFound) {
		t.Fatalf("Remove of an absent item: err = %v, want ErrNotFound", err)
	}

	if err := ring.Set(key, []byte(`{"linear_api_key":"lin-marker-file"}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := ring.Remove(key); err != nil {
		t.Fatalf("Remove of a present item: %v", err)
	}
	if _, err := ring.Get(key); !errors.Is(err, reposecrets.ErrNotFound) {
		t.Fatalf("Get after Remove: err = %v, want ErrNotFound", err)
	}

	// Through the Store, removing an absent item is not an error.
	store := reposecrets.New(func() (reposecrets.Ring, error) { return ring, nil })
	if err := store.Delete(context.Background(), "repo-1"); err != nil {
		t.Fatalf("Store.Delete of an absent item on the file backend: %v", err)
	}
}
