package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Every caller persists discovered paths, so a Homebrew install must hand out the
// opt path rather than the versioned keg path the resolved binary lives in.
func TestDiscoverPluginsReturnsHomebrewOptPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Homebrew opt-symlink layout is macOS/Linux only")
	}
	tmp := t.TempDir()
	keg := filepath.Join(tmp, "Cellar", "bossanova", "1.193.0")
	binDir := filepath.Join(keg, "bin")
	pluginDir := filepath.Join(keg, "libexec", "plugins")
	for _, d := range []string{binDir, pluginDir, filepath.Join(tmp, "opt")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "bossd-plugin-alpha"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keg, filepath.Join(tmp, "opt", "bossanova")); err != nil {
		t.Fatal(err)
	}

	plugins, _ := discoverPluginsFrom(binDir, activePolicy())
	want := filepath.Join(tmp, "opt", "bossanova", "libexec", "plugins", "bossd-plugin-alpha")
	if len(plugins) != 1 || plugins[0].Path != want {
		t.Fatalf("want one plugin at %q, got %+v", want, plugins)
	}
}
