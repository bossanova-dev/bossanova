package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/socketauth"
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
