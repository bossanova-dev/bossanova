package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
)

func TestHealPluginPaths_RepairsMissingPathFromDiscovery(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "bossd-plugin-claude")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configured := []config.PluginConfig{
		{Name: "claude", Path: "/opt/homebrew/Cellar/bossanova/1.63.0/libexec/plugins/bossd-plugin-claude",
			Enabled: true, Config: map[string]string{"dangerously_skip_permissions": "true"}},
	}
	discovered := []config.PluginConfig{{Name: "claude", Path: good, Enabled: true}}

	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 1 || names[0] != "claude" {
		t.Fatalf("want [claude] healed, got %v", names)
	}
	if healed[0].Path != good {
		t.Fatalf("path not repaired: %q", healed[0].Path)
	}
	if healed[0].Config["dangerously_skip_permissions"] != "true" {
		t.Fatalf("per-plugin config must be preserved through heal")
	}
	if !healed[0].Enabled {
		t.Fatalf("Enabled must be preserved through heal")
	}
}

func TestHealPluginPaths_LeavesValidPathsUntouched(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "bossd-plugin-claude")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configured := []config.PluginConfig{{Name: "claude", Path: good, Enabled: true}}
	discovered := []config.PluginConfig{{Name: "claude", Path: "/somewhere/else/bossd-plugin-claude"}}
	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 0 || healed[0].Path != good {
		t.Fatalf("valid path must not be healed; names=%v path=%q", names, healed[0].Path)
	}
}

func TestHealPluginPaths_NoDiscoveryMatchLeavesStalePath(t *testing.T) {
	stale := "/opt/homebrew/Cellar/bossanova/1.63.0/libexec/plugins/bossd-plugin-claude"
	configured := []config.PluginConfig{{Name: "claude", Path: stale, Enabled: true}}
	discovered := []config.PluginConfig{{Name: "codex", Path: "/anything/bossd-plugin-codex"}}
	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 0 || healed[0].Path != stale {
		t.Fatalf("no same-name discovery ⇒ leave stale path for VerifyConfiguredPlugins to reject; names=%v path=%q", names, healed[0].Path)
	}
}

// mkCellarPlugin creates <root>/Cellar/<formula>/<version>/libexec/plugins/<bin>
// as an executable file and returns its path.
func mkCellarPlugin(t *testing.T, root, formula, version, bin string) string {
	t.Helper()
	pluginDir := filepath.Join(root, "Cellar", formula, version, "libexec", "plugins")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(pluginDir, bin)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHealPluginPaths_RewritesToStableOptSymlinkPath exercises the marquee
// upgrade-proofing branch: a discovered Homebrew Cellar binary reachable via an
// opt/<formula> symlink is persisted in its version-independent opt form.
func TestHealPluginPaths_RewritesToStableOptSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew opt-symlink layout is macOS/Linux only")
	}
	dir := t.TempDir()
	cellarBin := mkCellarPlugin(t, dir, "bossanova", "1.63.0", "bossd-plugin-claude")

	// opt/<formula> -> Cellar/<formula>/<version>, the symlink Homebrew re-points on each upgrade.
	optDir := filepath.Join(dir, "opt")
	if err := os.MkdirAll(optDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "Cellar", "bossanova", "1.63.0"), filepath.Join(optDir, "bossanova")); err != nil {
		t.Fatal(err)
	}
	wantOpt := filepath.Join(optDir, "bossanova", "libexec", "plugins", "bossd-plugin-claude")

	configured := []config.PluginConfig{{Name: "claude", Path: filepath.Join(dir, "gone", "bossd-plugin-claude"), Enabled: true}}
	discovered := []config.PluginConfig{{Name: "claude", Path: cellarBin, Enabled: true}}

	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 1 || names[0] != "claude" {
		t.Fatalf("want [claude] healed, got %v", names)
	}
	if healed[0].Path != wantOpt {
		t.Fatalf("want stable opt path %q, got %q", wantOpt, healed[0].Path)
	}
	if strings.Contains(healed[0].Path, "/Cellar/") {
		t.Fatalf("healed path must not contain /Cellar/: %q", healed[0].Path)
	}
}

// TestHealPluginPaths_CellarWithoutOptSymlinkKeepsDiscoveredPath covers the
// negative rewrite path: no opt symlink ⇒ sameFile fails ⇒ keep the Cellar path.
func TestHealPluginPaths_CellarWithoutOptSymlinkKeepsDiscoveredPath(t *testing.T) {
	dir := t.TempDir()
	cellarBin := mkCellarPlugin(t, dir, "bossanova", "1.63.0", "bossd-plugin-claude")

	configured := []config.PluginConfig{{Name: "claude", Path: filepath.Join(dir, "gone", "bossd-plugin-claude"), Enabled: true}}
	discovered := []config.PluginConfig{{Name: "claude", Path: cellarBin, Enabled: true}}

	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 1 {
		t.Fatalf("want claude healed, got %v", names)
	}
	if healed[0].Path != cellarBin {
		t.Fatalf("no opt symlink ⇒ keep discovered Cellar path; got %q", healed[0].Path)
	}
}

// TestHealPluginPaths_SkipsNonExecutableDiscovery covers the guard that a healed
// entry is never marked repaired to a discovered path that exists but cannot run.
func TestHealPluginPaths_SkipsNonExecutableDiscovery(t *testing.T) {
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "bossd-plugin-claude")
	if err := os.WriteFile(nonExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const stale = "/opt/homebrew/Cellar/bossanova/1.63.0/libexec/plugins/bossd-plugin-claude"
	configured := []config.PluginConfig{{Name: "claude", Path: stale, Enabled: true}}
	discovered := []config.PluginConfig{{Name: "claude", Path: nonExec, Enabled: true}}

	healed, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 0 || healed[0].Path != stale {
		t.Fatalf("non-executable discovery must not heal; names=%v path=%q", names, healed[0].Path)
	}
}

func TestHealPluginPaths_IncompleteCellarPathIsLeftUnchanged(t *testing.T) {
	dir := t.TempDir()
	cellar := filepath.Join(dir, "Cellar")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	// This is a valid executable discovery path but not a complete
	// Cellar/<formula>/<version>/... layout. It must not be rewritten or panic.
	incomplete := filepath.Join(cellar, "bossanova")
	if err := os.WriteFile(incomplete, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	configured := []config.PluginConfig{{Name: "claude", Path: filepath.Join(dir, "gone", "bossd-plugin-claude"), Enabled: true}}
	healed, names := config.HealPluginPaths(configured, []config.PluginConfig{{Name: "claude", Path: incomplete, Enabled: true}})
	if len(names) != 1 || healed[0].Path != incomplete {
		t.Fatalf("incomplete Cellar path must be retained; names=%v path=%q", names, healed[0].Path)
	}
}

// TestHealPluginPaths_DoesNotMutateInput locks the copy-on-write contract: the
// caller's configured slice (Path and Config) must survive a heal untouched.
func TestHealPluginPaths_DoesNotMutateInput(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "bossd-plugin-claude")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const stale = "/opt/homebrew/Cellar/bossanova/1.63.0/libexec/plugins/bossd-plugin-claude"
	configured := []config.PluginConfig{{Name: "claude", Path: stale, Enabled: true, Config: map[string]string{"k": "v"}}}
	discovered := []config.PluginConfig{{Name: "claude", Path: good, Enabled: true}}

	_, names := config.HealPluginPaths(configured, discovered)
	if len(names) != 1 {
		t.Fatalf("expected heal, got %v", names)
	}
	if configured[0].Path != stale {
		t.Fatalf("input slice mutated: configured[0].Path = %q", configured[0].Path)
	}
	if configured[0].Config["k"] != "v" {
		t.Fatalf("input Config mutated: %v", configured[0].Config)
	}
}
