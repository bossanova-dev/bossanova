package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/socketauth"
)

func TestRotateToken_RemovesTokenFile(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "bossd.sock")
	if _, err := socketauth.LoadOrCreateToken(socketPath); err != nil {
		t.Fatal(err)
	}
	if err := rotateToken(socketPath); err != nil {
		t.Fatalf("rotateToken: %v", err)
	}
	if _, err := os.Stat(socketauth.TokenPath(socketPath)); !os.IsNotExist(err) {
		t.Fatalf("token file still present: %v", err)
	}
}

func TestRotateToken_MissingFileIsNoError(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "bossd.sock")
	if err := rotateToken(socketPath); err != nil {
		t.Fatalf("rotateToken on missing file should be a no-op, got: %v", err)
	}
}
