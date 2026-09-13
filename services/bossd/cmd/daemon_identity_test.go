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

// TestResolveDaemonIdentityBlankNameKeepsMachineDefault pins the no-override
// branch through the public entry point. It compares against the shared
// derivation rather than against the raw hostname because a host that exposes an
// operator-facing computer name answers with THAT — which is the whole point of
// the derivation. The ordering guard below is what keeps this from being a
// tautology: it proves the value bosso is told is not the value identity
// resolution was handed.
func TestResolveDaemonIdentityBlankNameKeepsMachineDefault(t *testing.T) {
	cfg := &upstream.Config{Hostname: "studio-imac"}

	if err := resolveDaemonIdentity(cfg, config.Settings{}, noEnv, t.TempDir()); err != nil {
		t.Fatalf("resolveDaemonIdentity: %v", err)
	}

	if want := config.DefaultDisplayHostname("studio-imac"); cfg.Hostname != want {
		t.Fatalf("Hostname = %q, want the derived machine default %q", cfg.Hostname, want)
	}
	if strings.TrimSpace(cfg.Hostname) == "" {
		t.Fatalf("Hostname = %q, want a non-blank name — bosso rejects a blank hostname at registration", cfg.Hostname)
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

// TestResolveDaemonIdentityDerivesDisplayHostname is the R1 ordering guard for
// the display derivation. The machine hostname carries a ".lan" suffix that the
// derivation removes, so the value bosso is told and the value identity
// resolution is handed are provably different strings — apply the derivation
// before resolveID and this test goes red.
func TestResolveDaemonIdentityDerivesDisplayHostname(t *testing.T) {
	const machineHostname = "mac.lan"
	derived := config.DefaultDisplayHostname(machineHostname)
	if derived == machineHostname {
		// Only reachable on a host whose ComputerName is literally "mac.lan".
		// Skipping beats passing vacuously: with the two strings equal, the
		// ordering assertion below could not distinguish the mutation it exists
		// to catch.
		t.Skipf("this host derives %q from %q, so the ordering guard would be vacuous", derived, machineHostname)
	}

	cfg := &upstream.Config{Hostname: machineHostname}

	var calls int
	var sawHostname string
	resolver := func(_ func(string) string, _, hostname string) (string, error) {
		calls++
		sawHostname = hostname
		return "generated-id", nil
	}

	if err := resolveDaemonIdentityWith(cfg, config.Settings{}, noEnv, t.TempDir(), resolver); err != nil {
		t.Fatalf("resolveDaemonIdentityWith: %v", err)
	}

	if calls != 1 {
		t.Fatalf("id resolver called %d times, want exactly 1", calls)
	}
	// Identity: the RAW machine hostname, never the derived display name.
	if sawHostname != machineHostname {
		t.Fatalf("id resolver saw hostname %q, want the raw machine hostname %q", sawHostname, machineHostname)
	}
	// Presentation: the derived default, resolved through the SHARED helper the
	// boss TUI previews with — not a local re-implementation (R4).
	if cfg.Hostname != derived {
		t.Fatalf("Hostname = %q, want the derived display default %q", cfg.Hostname, derived)
	}
	if cfg.Hostname == machineHostname {
		t.Fatalf("Hostname = %q, want the derivation to have been applied", cfg.Hostname)
	}
	if cfg.DaemonID != "generated-id" {
		t.Fatalf("DaemonID = %q, want the resolved id", cfg.DaemonID)
	}
	if cfg.DaemonID == cfg.Hostname {
		t.Fatalf("DaemonID = %q, want it distinct from the display name", cfg.DaemonID)
	}
}

// TestResolveDaemonIdentityOverrideBeatsDerivedDefault pins R3 against R1
// together: the daemon_name override still wins over the derived default, and
// identity resolution still sees the raw machine hostname behind both.
func TestResolveDaemonIdentityOverrideBeatsDerivedDefault(t *testing.T) {
	const machineHostname = "mac.lan"
	cfg := &upstream.Config{Hostname: machineHostname}

	var sawHostname string
	resolver := func(_ func(string) string, _, hostname string) (string, error) {
		sawHostname = hostname
		return "generated-id", nil
	}

	if err := resolveDaemonIdentityWith(cfg, config.Settings{DaemonName: "studio-mini"}, noEnv, t.TempDir(), resolver); err != nil {
		t.Fatalf("resolveDaemonIdentityWith: %v", err)
	}

	if cfg.Hostname != "studio-mini" {
		t.Fatalf("Hostname = %q, want the daemon_name override", cfg.Hostname)
	}
	if sawHostname != machineHostname {
		t.Fatalf("id resolver saw hostname %q, want the raw machine hostname %q", sawHostname, machineHostname)
	}
}

// TestResolveDaemonIdentityEmptyMachineHostname pins the os.Hostname()-failed
// edge: identity resolution is handed the empty string it actually read, and the
// display name is whatever the shared derivation makes of it — on a host with an
// operator-facing computer name that is a real name where there was none, and
// everywhere else it stays empty for the caller's "hostname unavailable" branch.
func TestResolveDaemonIdentityEmptyMachineHostname(t *testing.T) {
	cfg := &upstream.Config{Hostname: ""}

	var calls int
	var sawHostname string
	resolver := func(_ func(string) string, _, hostname string) (string, error) {
		calls++
		sawHostname = hostname
		return "generated-id", nil
	}

	if err := resolveDaemonIdentityWith(cfg, config.Settings{}, noEnv, t.TempDir(), resolver); err != nil {
		t.Fatalf("resolveDaemonIdentityWith: %v", err)
	}

	if calls != 1 {
		t.Fatalf("id resolver called %d times, want exactly 1", calls)
	}
	if sawHostname != "" {
		t.Fatalf("id resolver saw hostname %q, want the empty machine hostname", sawHostname)
	}
	if want := config.DefaultDisplayHostname(""); cfg.Hostname != want {
		t.Fatalf("Hostname = %q, want the derived default %q", cfg.Hostname, want)
	}
}
