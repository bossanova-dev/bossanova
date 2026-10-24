package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/contract/daemon/socketauth"
)

// TestRunHTTPLoadsTheHTTPTokenNotTheDaemonToken pins the --http wiring to the
// MCP HTTP token file. The non-loopback address makes serve.HTTP refuse after
// the token is loaded, so the server never listens.
func TestRunHTTPLoadsTheHTTPTokenNotTheDaemonToken(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "bossd.sock")

	err := run([]string{"--http", "0.0.0.0:0", "--socket", sock})
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("run() error = %v, want the non-loopback refusal", err)
	}

	info, statErr := os.Stat(socketauth.HTTPTokenPath(sock))
	if statErr != nil {
		t.Fatalf("HTTP token %s not created: %v", socketauth.HTTPTokenPath(sock), statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("HTTP token mode = %o, want 600", mode)
	}
	if _, statErr := os.Stat(socketauth.TokenPath(sock)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon token %s exists (stat err %v); --http must not create or use bossd.token", socketauth.TokenPath(sock), statErr)
	}
}

func TestRunHTTPUsesSettingsProfile(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bossd.sock")
	decoy := filepath.Join(t.TempDir(), "bossd.sock")
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings, err := json.Marshal(map[string]string{"socket_path": socket})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	t.Setenv("BOSS_SOCKET", decoy)
	err = run([]string{"--http", "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("run() error = %v, want non-loopback refusal", err)
	}
	if _, err := os.Stat(socketauth.HTTPTokenPath(socket)); err != nil {
		t.Fatalf("settings profile token: %v", err)
	}
	if _, err := os.Stat(socketauth.HTTPTokenPath(decoy)); !os.IsNotExist(err) {
		t.Fatalf("decoy token stat = %v, want not exist", err)
	}
}
