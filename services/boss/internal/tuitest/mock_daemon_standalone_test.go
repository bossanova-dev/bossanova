package tuitest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

func TestStartMockDaemonStandalone(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	d, stop, err := tuitest.StartMockDaemon(sock)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = stop() }()
	if d.SocketPath() != sock {
		t.Fatalf("socket: got %q want %q", d.SocketPath(), sock)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
}
