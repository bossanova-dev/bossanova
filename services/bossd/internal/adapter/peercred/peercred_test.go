//go:build darwin || linux

package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveProcessCredentials(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "boss-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	addr := &net.UnixAddr{Name: filepath.Join(dir, "socket"), Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	pid, err := PeerPID(accepted)
	if err != nil {
		t.Fatal(err)
	}
	if pid != os.Getpid() {
		t.Fatalf("peer PID = %d, want %d", pid, os.Getpid())
	}
	parent, err := ParentPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if parent != os.Getppid() {
		t.Fatalf("parent PID = %d, want %d", parent, os.Getppid())
	}
}

func TestRejectNonSocketAndInvalidPID(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	if _, err := PeerPID(client); err == nil {
		t.Fatal("non-unix connection accepted")
	}
	if _, err := ParentPID(0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid PID error = %v", err)
	}
}
