package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/upstream"
)

// The daemon's display name is presentation metadata. These tests pin the one
// property that makes it safe: renaming a daemon changes what bosso shows and
// nothing about how the daemon is addressed.

func noEnv(string) string { return "" }

func TestResolveDaemonIdentityBlankNameKeepsMachineHostname(t *testing.T) {
	cfg := &upstream.Config{Hostname: "studio-imac"}

	if err := resolveDaemonIdentity(cfg, config.Settings{}, noEnv, t.TempDir()); err != nil {
		t.Fatalf("resolveDaemonIdentity: %v", err)
	}

	if cfg.Hostname != "studio-imac" {
		t.Fatalf("Hostname = %q, want the machine hostname", cfg.Hostname)
	}
}

// TestResolveDaemonIdentityHandsMachineHostnameToIDResolver is the direct pin on
// the ordering: it records the hostname the id resolver was actually handed.
// Mutate resolveDaemonIdentityWith to apply the display override first and pass
// cfg.Hostname to resolveID, and this test — plus the empty-data-dir fallback
// below, and nothing else — goes red.
func TestResolveDaemonIdentityHandsMachineHostnameToIDResolver(t *testing.T) {
	cfg := &upstream.Config{Hostname: "studio-imac"}

	var calls int
	var sawHostname string
	resolver := func(_ func(string) string, _, hostname string) (string, error) {
		calls++
		sawHostname = hostname
		return "generated-id", nil
	}

	if err := resolveDaemonIdentityWith(cfg, config.Settings{DaemonName: "studio-mini"}, noEnv, t.TempDir(), resolver); err != nil {
		t.Fatalf("resolveDaemonIdentityWith: %v", err)
	}

	// Without this the hostname assertion would pass vacuously on a resolver
	// that was never called.
	if calls != 1 {
		t.Fatalf("id resolver called %d times, want exactly 1", calls)
	}
	if sawHostname != "studio-imac" {
		t.Fatalf("id resolver saw hostname %q, want the machine hostname", sawHostname)
	}
	if cfg.DaemonID != "generated-id" {
		t.Fatalf("DaemonID = %q, want the resolved id", cfg.DaemonID)
	}
	if cfg.Hostname != "studio-mini" {
		t.Fatalf("Hostname = %q, want the display override", cfg.Hostname)
	}
}

// TestResolveDaemonIdentityRenameDoesNotRotatePersistedID pins the user-facing
// consequence: renaming a daemon that already has a persisted id keeps that id
// and does not write a second one. It is deliberately insensitive to the
// assignment ordering — the resolver-seam test above owns that.
func TestResolveDaemonIdentityRenameDoesNotRotatePersistedID(t *testing.T) {
	dataDir := t.TempDir()

	plain := &upstream.Config{Hostname: "studio-imac"}
	if err := resolveDaemonIdentity(plain, config.Settings{}, noEnv, dataDir); err != nil {
		t.Fatalf("resolveDaemonIdentity (unnamed): %v", err)
	}

	named := &upstream.Config{Hostname: "studio-imac"}
	if err := resolveDaemonIdentity(named, config.Settings{DaemonName: "  studio-mini  "}, noEnv, dataDir); err != nil {
		t.Fatalf("resolveDaemonIdentity (named): %v", err)
	}

	if named.Hostname != "studio-mini" {
		t.Fatalf("Hostname = %q, want the trimmed display override", named.Hostname)
	}
	if named.DaemonID != plain.DaemonID {
		t.Fatalf("DaemonID = %q, want it unchanged by the rename (%q)", named.DaemonID, plain.DaemonID)
	}

	// The persisted UUID selection is likewise untouched: one id file, one id.
	raw, err := os.ReadFile(filepath.Join(dataDir, "daemon-id"))
	if err != nil {
		t.Fatalf("read persisted daemon id: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != plain.DaemonID {
		t.Fatalf("persisted daemon id = %q, want %q", got, plain.DaemonID)
	}
}

func TestResolveDaemonIdentityHostnameFallbackIgnoresDisplayName(t *testing.T) {
	// An empty data dir forces ResolveDaemonID's last-resort hostname
	// fallback — the one path where a hostname literally becomes the id.
	cfg := &upstream.Config{Hostname: "studio-imac"}

	err := resolveDaemonIdentity(cfg, config.Settings{DaemonName: "studio-mini"}, noEnv, "")
	if err == nil {
		t.Fatal("resolveDaemonIdentity error = nil, want the empty-data-dir fallback error")
	}
	if cfg.DaemonID != "studio-imac" {
		t.Fatalf("DaemonID = %q, want the machine hostname fallback", cfg.DaemonID)
	}
	if cfg.Hostname != "studio-mini" {
		t.Fatalf("Hostname = %q, want the display override", cfg.Hostname)
	}
}

func TestResolveDaemonIdentityEnvOverrideWinsOverDisplayName(t *testing.T) {
	cfg := &upstream.Config{Hostname: "studio-imac"}
	getenv := func(key string) string {
		if key == "BOSSD_DAEMON_ID" {
			return "pinned-daemon-id"
		}
		return ""
	}

	if err := resolveDaemonIdentity(cfg, config.Settings{DaemonName: "studio-mini"}, getenv, t.TempDir()); err != nil {
		t.Fatalf("resolveDaemonIdentity: %v", err)
	}

	if cfg.DaemonID != "pinned-daemon-id" {
		t.Fatalf("DaemonID = %q, want the BOSSD_DAEMON_ID override", cfg.DaemonID)
	}
	if cfg.Hostname != "studio-mini" {
		t.Fatalf("Hostname = %q, want the display override", cfg.Hostname)
	}
}
