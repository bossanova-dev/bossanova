package upgrade

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WriteCache creates the cache directory with least-privilege 0700 perms
// (gosec G301). Assert the created dir is owner-only and the entry still
// round-trips through ReadCache.
func TestWriteCacheDirIsOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "banner-cache")
	path := filepath.Join(dir, "banner.json")
	want := CacheEntry{
		CheckedAt:      time.Now().Truncate(time.Second),
		CurrentVersion: "1.2.3",
		LatestVersion:  "1.2.4",
		ReleaseURL:     "https://example.test/releases/1.2.4",
	}
	if err := WriteCache(path, want); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("cache dir perm = %o, want 0700", got)
	}

	got, ok, err := ReadCache(path)
	if err != nil || !ok {
		t.Fatalf("ReadCache: ok=%v err=%v", ok, err)
	}
	if got.CurrentVersion != want.CurrentVersion || got.LatestVersion != want.LatestVersion {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
}
