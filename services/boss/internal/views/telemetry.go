package views

import (
	"context"
	"os"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

func captureViewTelemetry(ctx context.Context, client telemetry.Client, event telemetry.Event, props map[string]any) {
	if client == nil {
		return
	}
	if !viewTelemetryEnabled() {
		return
	}
	if props == nil {
		props = map[string]any{}
	}
	client.Capture(ctx, event, viewDistinctID(), props)
}

func viewTelemetryEnabled() bool {
	settings, err := config.Load()
	return err == nil && settings.EventTracingEnabled
}

func viewDistinctID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return telemetry.LocalDistinctID("")
	}
	return telemetry.LocalDistinctID(home)
}
