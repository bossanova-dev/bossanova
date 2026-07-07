package session

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
)

// TestAutoRotateAllowed pins the per-decision kill-switch gate (BOS-176): with a
// live loader it re-reads settings every call and fails safe (any load error ⇒
// disabled); with no loader it uses the cached injected config.
func TestAutoRotateAllowed(t *testing.T) {
	disabled := false
	enabled := true

	newLC := func() *Lifecycle { return &Lifecycle{logger: zerolog.Nop()} }

	t.Run("loader enabled → allowed", func(t *testing.T) {
		lc := newLC()
		lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
			return config.RotationConfig{Enabled: &enabled}, nil
		})
		if !lc.autoRotateAllowed() {
			t.Error("want allowed when loaded config enables rotation")
		}
	})

	t.Run("loader disabled → blocked", func(t *testing.T) {
		lc := newLC()
		lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
			return config.RotationConfig{Enabled: &disabled}, nil
		})
		if lc.autoRotateAllowed() {
			t.Error("want blocked when loaded config disables rotation")
		}
	})

	t.Run("loader error → fail-safe blocked", func(t *testing.T) {
		lc := newLC()
		lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
			return config.RotationConfig{}, errors.New("boom")
		})
		if lc.autoRotateAllowed() {
			t.Error("want blocked (fail-safe) when settings load fails")
		}
	})

	t.Run("no loader falls back to cached config", func(t *testing.T) {
		lc := newLC()
		lc.SetRotationConfig(config.RotationConfig{Enabled: &disabled})
		if lc.autoRotateAllowed() {
			t.Error("want cached disabled config to block")
		}
		lc.SetRotationConfig(config.RotationConfig{}) // nil Enabled = default ON
		if !lc.autoRotateAllowed() {
			t.Error("want cached default config (nil) to allow")
		}
	})
}

func TestCurrentRotationConfig(t *testing.T) {
	enabled := true
	lc := &Lifecycle{logger: zerolog.Nop()}
	lc.SetRotationConfig(config.RotationConfig{MaxRotationsPerRun: 2})
	lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
		return config.RotationConfig{
			Enabled:            &enabled,
			MaxRotationsPerRun: 7,
		}, nil
	})

	got, ok := lc.currentRotationConfig()
	if !ok {
		t.Fatal("currentRotationConfig ok = false, want true")
	}
	if !got.RotationEnabled() || got.MaxRotations() != 7 {
		t.Fatalf("currentRotationConfig = %+v, want enabled with max=7", got)
	}

	lc.SetRotationConfigLoader(func() (config.RotationConfig, error) {
		return config.RotationConfig{}, errors.New("boom")
	})
	if _, ok := lc.currentRotationConfig(); ok {
		t.Fatal("currentRotationConfig ok = true, want false on load error")
	}
}
