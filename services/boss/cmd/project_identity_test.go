package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/daemonstate"
)

func TestLocalDaemonIdentity(t *testing.T) {
	const socket = "/tmp/boss-test.sock"
	settingsDir := t.TempDir()
	settingsPath := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"daemon_name":"named-in-settings"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := func(appData string) projectExportEnv {
		return projectExportEnv{
			profile: func() (daemonProfile, error) {
				return daemonProfile{AppDataDir: appData, SocketPath: socket, SettingsPath: settingsPath}, nil
			},
			hostname: func() (string, error) { return "host-x", nil },
			homeDir:  func() (string, error) { return t.TempDir(), nil },
			cloud:    func(*cobra.Command) (projectCloudClient, error) { return nil, errors.New("unused") },
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

	t.Run("record with id and matching socket", func(t *testing.T) {
		dir := record(t, daemonstate.Metadata{PID: 1, SocketPath: socket, DaemonID: "d-1", DisplayName: "laptop"})
		id, warning := localApplyIdentity(env(dir))
		if id.ID != "d-1" || id.DisplayName != "laptop" || warning != "" {
			t.Fatalf("got %+v warning %q", id, warning)
		}
	})
	fallbacks := map[string]func(t *testing.T) string{
		"record names another socket": func(t *testing.T) string {
			return record(t, daemonstate.Metadata{PID: 1, SocketPath: "/tmp/other.sock", DaemonID: "d-1", DisplayName: "laptop"})
		},
		"missing record": func(t *testing.T) string { return t.TempDir() },
		"empty daemon id": func(t *testing.T) string {
			return record(t, daemonstate.Metadata{PID: 1, SocketPath: socket, DisplayName: "laptop"})
		},
	}
	for name, dir := range fallbacks {
		t.Run(name, func(t *testing.T) {
			id, warning := localApplyIdentity(env(dir(t)))
			if id.ID != "" || id.DisplayName != "named-in-settings" {
				t.Fatalf("want name-only identity from settings, got %+v", id)
			}
			if !strings.Contains(warning, "routing id is unknown") || !strings.Contains(warning, `"named-in-settings"`) {
				t.Fatalf("warning %q", warning)
			}
		})
	}
}
