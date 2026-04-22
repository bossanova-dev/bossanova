package keyringutil

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestNew_CreatesRandomPassphrase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	prompt := New(false)
	pass, err := prompt("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if pass == LegacyPassphrase {
		t.Fatalf("got legacy passphrase; expected a random one")
	}
	if len(pass) < 32 {
		t.Fatalf("passphrase too short: %d bytes", len(pass))
	}

	path := filepath.Join(home, ".config", "bossanova", "keyring.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("passphrase file perms: got %o, want 0600", perm)
	}
}

func TestNew_ReusesExistingPassphrase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	prompt := New(false)
	first, err := prompt("")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := prompt("")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Fatalf("passphrase changed between calls: %q vs %q", first, second)
	}
}

func TestNew_RejectsEmptyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "bossanova", "keyring.key")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	prompt := New(false)
	if _, err := prompt(""); err == nil {
		t.Fatalf("expected error for empty passphrase file")
	}
}

func TestNew_UnwritablePath_Errors(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := os.UserHomeDir(); err == nil {
		t.Setenv("HOME", "/nonexistent-root-"+t.Name())
	}

	prompt := New(false)
	_, err := prompt("")
	if err == nil {
		t.Fatalf("expected error when no writable location is available")
	}
	if !strings.Contains(err.Error(), "allow-insecure-keyring") {
		t.Fatalf("error message should point at opt-in flag, got: %v", err)
	}
}

func TestNew_AllowInsecureFallback(t *testing.T) {
	t.Setenv("HOME", "/nonexistent-root-"+t.Name())

	prompt := New(true)
	pass, err := prompt("")
	if err != nil {
		t.Fatalf("allowInsecure should not error: %v", err)
	}
	if pass != LegacyPassphrase {
		t.Fatalf("allowInsecure should return legacy passphrase, got %q", pass)
	}
}

func TestPassphrasePath_UsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := passphrasePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "bossanova", "keyring.key")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPassphrasePath_IgnoresXDGRuntimeDir(t *testing.T) {
	// The encrypted keyring data is persistent, so the passphrase must be too.
	// $XDG_RUNTIME_DIR is tmpfs on Linux and would brick credentials on reboot.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := passphrasePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "bossanova", "keyring.key")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLoadOrCreatePassphrase_ConcurrentAgree(t *testing.T) {
	// Many goroutines racing to initialize the same passphrase file must all
	// end up with the same value — i.e. the O_EXCL loser reads the winner's
	// bytes instead of silently overwriting them.
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring.key")

	const n = 16
	results := make(chan string, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			pass, err := loadOrCreatePassphrase(path)
			if err != nil {
				errs <- err
				return
			}
			results <- pass
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("loadOrCreatePassphrase: %v", err)
	}
	var first string
	for pass := range results {
		if first == "" {
			first = pass
			continue
		}
		if pass != first {
			t.Fatalf("concurrent callers disagreed: %q vs %q", first, pass)
		}
	}
}
