package plugin

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/plugin/eventbus"
)

func testHost() *Host {
	bus := eventbus.New(zerolog.Nop())
	return New(bus, zerolog.Nop())
}

func TestStartEmptyPlugins(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start with nil plugins: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartEmptySlice(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), []config.PluginConfig{}); err != nil {
		t.Fatalf("Start with empty slice: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins, got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartDisabledPluginSkipped(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{
			Name:    "disabled-plugin",
			Path:    "/nonexistent/binary",
			Enabled: false,
		},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start with disabled plugin: %v", err)
	}

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 running plugins (disabled was skipped), got %d", len(statuses))
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartMultipleDisabledPlugins(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "plugin-a", Path: "/nonexistent/a", Enabled: false},
		{Name: "plugin-b", Path: "/nonexistent/b", Enabled: false},
		{Name: "plugin-c", Path: "/nonexistent/c", Enabled: false},
	}

	if err := h.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if n := len(h.Plugins()); n != 0 {
		t.Errorf("expected 0 plugins, got %d", n)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStopIdempotent(t *testing.T) {
	h := testHost()

	if err := h.Start(t.Context(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	// Second stop should not panic or error.
	if err := h.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStopWithoutStart(t *testing.T) {
	h := testHost()

	// Stop without Start should not panic or error.
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop without Start: %v", err)
	}
}

func TestPluginsReturnsEmptyBeforeStart(t *testing.T) {
	h := testHost()

	statuses := h.Plugins()
	if len(statuses) != 0 {
		t.Errorf("expected 0 plugins before Start, got %d", len(statuses))
	}
}

func TestStartEnabledPluginInvalidPath(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{
			Name:    "bad-plugin",
			Path:    "/nonexistent/plugin/binary",
			Enabled: true,
		},
	}

	err := h.Start(t.Context(), cfgs)
	if err == nil {
		t.Fatal("expected error starting plugin with nonexistent binary")
	}

	// Should still be able to stop cleanly after a failed start.
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

func TestStartMixedEnabledDisabled(t *testing.T) {
	h := testHost()

	cfgs := []config.PluginConfig{
		{Name: "disabled-first", Path: "/nonexistent/a", Enabled: false},
		{Name: "bad-enabled", Path: "/nonexistent/b", Enabled: true},
		{Name: "disabled-last", Path: "/nonexistent/c", Enabled: false},
	}

	// The enabled plugin has an invalid path, so Start should fail.
	err := h.Start(context.Background(), cfgs)
	if err == nil {
		t.Fatal("expected error for enabled plugin with invalid path")
	}

	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
