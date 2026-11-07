package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
)

func TestRun_StartupPhaseLog(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")
	t.Setenv("XDG_STATE_HOME", baseDir)

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     filepath.Join(baseDir, "bossd.db"),
			socketPath: filepath.Join(baseDir, "bossd.sock"),
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state")
	}
	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
	data, err := os.ReadFile(filepath.Join(baseDir, "bossanova", "logs", "bossd.log"))
	if err != nil {
		t.Fatal(err)
	}
	events := startupEvents(t, bytes.NewBuffer(data))
	summaries := 0
	var completed []string
	for _, event := range events {
		switch string(event["message"]) {
		case `"startup phase done"`:
			var phase string
			if err := json.Unmarshal(event["phase"], &phase); err != nil {
				t.Fatal(err)
			}
			completed = append(completed, phase)
		case `"daemon ready to serve"`:
			summaries++
			if string(event["level"]) != `"info"` || event["total"] == nil || event["slowest_phase"] == nil || event["slowest_elapsed"] == nil {
				t.Fatalf("incomplete summary: %s", data)
			}
			var phases map[string]json.RawMessage
			if err := json.Unmarshal(event["phases"], &phases); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"stores", "session_infrastructure", "serving"} {
				if phases[name] == nil {
					t.Errorf("missing phase %s", name)
				}
			}
		}
	}
	if summaries != 1 {
		t.Errorf("got %d ready summaries, want 1", summaries)
	}
	want := []string{"stores", "session_infrastructure", "plugins", "accounts", "lifecycle", "rotation", "shutdown_tracking", "repair_plugin", "task_workers", "stream_observers", "chat_uploads", "upstream", "daemon", "delivery_workers", "hooks", "proxy", "bootstrap", "background_workers", "serving"}
	if !slices.Equal(completed, want) {
		t.Errorf("phase order = %v, want %v", completed, want)
	}

}
