package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

func TestSeedDemoWorld(t *testing.T) {
	// Use /tmp (not t.TempDir) to stay under the 104-char macOS unix-socket
	// limit, but qualify with the PID so concurrent/repeated runs don't collide.
	sock := filepath.Join("/tmp", fmt.Sprintf("bos47-fxd-%d.sock", os.Getpid()))
	d, stop, err := tuitest.StartMockDaemon(sock)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = stop() }()
	seedDemoWorld(d) // helper under test
	if got := len(d.Repos()); got == 0 {
		t.Fatalf("expected demo repos seeded, got %d", got)
	}
}

func TestSeedHomeProfiles(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    map[string]any
	}{
		{
			name:    "demo skips onboarding",
			fixture: "demo",
			want: map[string]any{
				"providers_acknowledged": true,
				"worktree_base_dir":      "/home/bossanova/worktrees",
			},
		},
		{
			name:    "login stays logged out but skips onboarding",
			fixture: "login",
			want: map[string]any{
				"providers_acknowledged": true,
				"worktree_base_dir":      "/home/bossanova/worktrees",
			},
		},
		{
			name:    "onboarding enters first run gate",
			fixture: "onboarding",
			want: map[string]any{
				"providers_acknowledged": false,
				"socket_path":            "/tmp/boss-proof.sock",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			if err := seedHome(home, "/tmp/boss-proof.sock", tt.fixture); err != nil {
				t.Fatalf("seed home: %v", err)
			}
			got := readSeededSettings(t, home)
			for key, want := range tt.want {
				if got[key] != want {
					t.Fatalf("settings[%s] = %#v, want %#v; settings=%#v", key, got[key], want, got)
				}
			}
		})
	}
}

func TestSeedHomeRejectsUnknownFixture(t *testing.T) {
	err := seedHome(t.TempDir(), "/tmp/boss-proof.sock", "bogus")
	if err == nil {
		t.Fatal("expected unknown fixture error")
	}
}

func readSeededSettings(t *testing.T, home string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(configDirForHome(home), "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return settings
}
