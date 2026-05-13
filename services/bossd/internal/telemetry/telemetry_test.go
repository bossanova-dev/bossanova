package telemetry

import (
	"testing"

	"github.com/recurser/bossalib/config"
	libtelemetry "github.com/recurser/bossalib/telemetry"
)

func TestConfigFromSettingsDisabledByDefault(t *testing.T) {
	cfg := ConfigFromSettings(config.DefaultSettings())
	if cfg.Enabled {
		t.Fatal("ConfigFromSettings(config.DefaultSettings()).Enabled = true, want false")
	}
}

func TestConfigFromSettingsUsesBossdApp(t *testing.T) {
	s := config.DefaultSettings()
	s.EventTracingEnabled = true

	cfg := ConfigFromSettings(s)
	if cfg.App != "bossd" {
		t.Fatalf("ConfigFromSettings App = %q, want %q", cfg.App, "bossd")
	}
	if cfg.ProjectToken != libtelemetry.ProductionProjectToken {
		t.Fatalf("ConfigFromSettings ProjectToken = %q, want %q", cfg.ProjectToken, libtelemetry.ProductionProjectToken)
	}
}
