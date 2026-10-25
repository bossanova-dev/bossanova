package socketbackend

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/projectconfig"
)

func TestSocketBackendProjectIdentity(t *testing.T) {
	const socket = "/tmp/boss-mcp-test.sock"
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"daemon_name":"named-in-settings"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	backend := func(appData string) *Backend {
		return &Backend{
			socketPath:   socket,
			settingsPath: func() (string, error) { return settingsPath, nil },
			appDataDir:   func(config.Settings) (string, error) { return appData, nil },
			hostname:     func() (string, error) { return "host-x", nil },
		}
	}
	record := func(t *testing.T, md daemonstate.Metadata) string {
		t.Helper()
		dir := t.TempDir()
		if err := daemonstate.Write(dir, md); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("a record written for this backend's socket gives its id", func(t *testing.T) {
		b := backend(record(t, daemonstate.Metadata{PID: 1, SocketPath: socket, DaemonID: "d-1", DisplayName: "laptop"}))
		id, warning, err := b.ProjectDaemonIdentity(context.Background())
		if err != nil || id != (projectconfig.DaemonIdentity{ID: "d-1", DisplayName: "laptop"}) || warning != "" {
			t.Fatalf("got %+v %q %v", id, warning, err)
		}
	})

	fallbacks := map[string]func(t *testing.T) string{
		"a record for another socket": func(t *testing.T) string {
			return record(t, daemonstate.Metadata{PID: 1, SocketPath: "/tmp/other.sock", DaemonID: "d-1", DisplayName: "laptop"})
		},
		"no record": func(t *testing.T) string { return t.TempDir() },
	}
	for name, dir := range fallbacks {
		t.Run(name+" is name-only, from the settings, with a warning", func(t *testing.T) {
			id, warning, err := backend(dir(t)).ProjectDaemonIdentity(context.Background())
			if err != nil || id != (projectconfig.DaemonIdentity{DisplayName: "named-in-settings"}) {
				t.Fatalf("got %+v %v", id, err)
			}
			if !strings.Contains(warning, "routing id is unknown") {
				t.Fatalf("warning %q", warning)
			}
		})
	}

	t.Run("checkout branch", func(t *testing.T) {
		repo := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q", "-b", "main")
		git("commit", "-q", "--allow-empty", "-m", "init")
		b := backend(t.TempDir())
		if branch, ok := b.CheckoutBranch(context.Background(), repo); !ok || branch != "main" {
			t.Fatalf("on main: %q %v", branch, ok)
		}
		git("checkout", "-q", "--detach")
		if branch, ok := b.CheckoutBranch(context.Background(), repo); ok || branch != "" {
			t.Fatalf("detached: %q %v", branch, ok)
		}
		if _, ok := b.CheckoutBranch(context.Background(), filepath.Join(repo, "missing")); ok {
			t.Fatal("a missing directory must not read as a branch")
		}
	})
}
