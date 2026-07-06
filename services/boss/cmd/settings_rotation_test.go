package main

import (
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/config"
)

// TestSettingsRotationFlags pins the boss CLI kill-switch surface (BOS-176):
// --rotation / --no-rotation persist config.Rotation.Enabled and are mutually
// exclusive.
func TestSettingsRotationFlags(t *testing.T) {
	setup := func(t *testing.T) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "settings.json")
		t.Setenv("BOSS_SETTINGS_PATH", path)
		if err := config.Save(config.DefaultSettings()); err != nil {
			t.Fatalf("seed settings: %v", err)
		}
	}

	t.Run("--no-rotation persists disabled", func(t *testing.T) {
		setup(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--no-rotation"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		s, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if s.Rotation.RotationEnabled() {
			t.Error("want rotation disabled after --no-rotation")
		}
	})

	t.Run("--rotation persists enabled", func(t *testing.T) {
		setup(t)
		// Start disabled so --rotation is an observable change.
		s, _ := config.Load()
		f := false
		s.Rotation.Enabled = &f
		if err := config.Save(s); err != nil {
			t.Fatalf("save: %v", err)
		}
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--rotation"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		reloaded, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !reloaded.Rotation.RotationEnabled() {
			t.Error("want rotation enabled after --rotation")
		}
	})

	t.Run("both flags error", func(t *testing.T) {
		setup(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--rotation", "--no-rotation"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("want error when both --rotation and --no-rotation are set")
		}
	})
}
