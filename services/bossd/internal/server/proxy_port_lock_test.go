package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func isolateFixedPortLocks(t *testing.T) {
	t.Helper()
	previous := fixedPortLockDir
	fixedPortLockDir = t.TempDir()
	t.Cleanup(func() { fixedPortLockDir = previous })
}

func TestFixedPortLock_WaitsForRelease(t *testing.T) {
	isolateFixedPortLocks(t)
	port := freeLoopbackPort(t)
	unlock, err := acquireFixedPortLock(port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(unlock) }
	defer release()
	acquired := make(chan error, 1)
	go func() {
		release, err := acquireFixedPortLock(port, time.Second)
		if err == nil {
			release()
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second lock did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("second acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired after release")
	}
}

func TestFixedPortLock_DifferentPorts(t *testing.T) {
	isolateFixedPortLocks(t)
	port := freeLoopbackPort(t)
	release, err := acquireFixedPortLock(port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other := freeLoopbackPort(t)
	for other == port {
		other = freeLoopbackPort(t)
	}
	releaseOther, err := acquireFixedPortLock(other, 0)
	if err != nil {
		t.Fatalf("different port contended: %v", err)
	}
	releaseOther()
}

func TestFixedPortLock_Timeout(t *testing.T) {
	isolateFixedPortLocks(t)
	port := freeLoopbackPort(t)
	release, err := acquireFixedPortLock(port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if releaseOther, err := acquireFixedPortLock(port, 20*time.Millisecond); err == nil {
		releaseOther()
		t.Fatal("second acquire succeeded while exclusive lock held")
	}
	// A failed waiter must not affect the owner's lock.
	if releaseOther, err := acquireFixedPortLock(port, 0); err == nil {
		releaseOther()
		t.Fatal("timeout released the owner's lock")
	}
}

func TestProxyListen_FixedPortLockDegrades(t *testing.T) {
	for _, kind := range []string{"missing directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			isolateFixedPortLocks(t)
			port := freeLoopbackPort(t)
			if kind == "missing directory" {
				fixedPortLockDir = filepath.Join(fixedPortLockDir, "missing")
			} else {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
				lockPath := filepath.Join(fixedPortLockDir, fmt.Sprintf("bossd-failover-proxy-%d.lock", port))
				if err := os.Symlink(target, lockPath); err != nil {
					t.Fatal(err)
				}
				if release, err := acquireFixedPortLock(port, 0); err == nil {
					release()
					t.Fatal("lock followed a symlink")
				}
				t.Cleanup(func() {
					content, err := os.ReadFile(target)
					if err != nil || string(content) != "untouched" {
						t.Errorf("symlink target changed: %q, %v", content, err)
					}
				})
			}
			var logs syncBuffer
			ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.New(&logs), Port: port})
			if err != nil {
				t.Fatal(err)
			}
			if err := ps.Listen(); err != nil {
				t.Fatalf("lock error prevented startup: %v", err)
			}
			defer func() { _, _ = ps.Shutdown(context.Background()) }()
			if ps.Port() != port || ps.FixedPortLost() {
				t.Fatalf("lock error lost fixed port: port=%d lost=%v", ps.Port(), ps.FixedPortLost())
			}
			if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), fmt.Sprintf(`"configured_port":%d`, port)) {
				t.Fatalf("missing warning naming configured port: %s", logs.String())
			}
		})
	}
}
