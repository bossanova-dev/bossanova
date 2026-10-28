package realstack

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBaseEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/real", "BOSS_SOCKET=/production", "BOSSD_ORCHESTRATOR_URL=https://production", "ANTHROPIC_NEW_SECRET=x", "CLAUDE_TOKEN=x", "PROOF_ANTHROPIC_API_KEY=x", "OPENAI_API_KEY=x", "GH_TOKEN=x", "XDG_CONFIG_HOME=/real", "KEEP=yes"}
	original := append([]string(nil), base...)
	got := BaseEnv(base)
	if want := []string{"PATH=/bin", "KEEP=yes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("env=%v, want %v", got, want)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatal("input mutated")
	}
}
func TestWrittenSettingsOnlyStub(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "settings.json")
	if err := writeSettings(path, settings(home, "/tmp/isolated.sock", "/tmp/stub", 12345)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Plugins []struct {
			Name, Path string
			Enabled    bool
		}
		SocketPath string `json:"socket_path"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Plugins) != 1 || got.Plugins[0].Name != "stub" || got.Plugins[0].Path != "/tmp/stub" || !got.Plugins[0].Enabled || got.SocketPath != "/tmp/isolated.sock" {
		t.Fatalf("settings=%s", data)
	}
}
func TestSessionsToSweep(t *testing.T) {
	current := []tmuxSession{{"boss-old", "/tmp/run/a"}, {"boss-own", "/tmp/run/a"}, {"boss-other", "/tmp/run-other/a"}, {"boss-prod", "/home/user"}, {"unrelated", "/tmp/run/a"}, {"boss-root", "/tmp/run"}}
	got := sessionsToSweep(current, map[string]bool{"boss-old": true}, []string{"/tmp/run"})
	if want := []string{"boss-own", "boss-root"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sweep=%v want %v", got, want)
	}
}

func TestHoldHelpers(t *testing.T) {
	s := &Stack{home: t.TempDir()}
	if err := s.ReleaseHold("merge-42"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.home, "holds", "merge-42.release")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.home, "holds", "merge-42.entered"), []byte(`{"pid":42,"argv":[],"enteredAt":"2026-10-26T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitHoldEntered(t.Context(), "merge-42"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := s.WaitHoldEntered(ctx, "missing"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing hold error=%v", err)
	}
	for _, name := range []string{"", "../escape", "UPPER", "-leading", strings.Repeat("a", 65)} {
		if err := s.ReleaseHold(name); err == nil {
			t.Errorf("ReleaseHold(%q) accepted", name)
		}
		if err := s.WaitHoldEntered(t.Context(), name); err == nil {
			t.Errorf("WaitHoldEntered(%q) accepted", name)
		}
	}
}

func TestHoldHelperIOErrors(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "holds"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Stack{home: home}
	if err := s.ReleaseHold("merge-42"); err == nil {
		t.Fatal("release ignored unusable holds directory")
	}
	if err := s.WaitHoldEntered(t.Context(), "merge-42"); err == nil {
		t.Fatal("wait ignored unusable holds directory")
	}
}
