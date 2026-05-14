package views

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Alias(context.Context, string, string)            {}
func (f *fakeTelemetry) Close()                                           {}

func assertNoSensitiveTelemetryProps(t *testing.T, props map[string]any) {
	t.Helper()
	for _, key := range []string{"args", "prompt", "transcript", "repo_path", "branch", "path", "file_path", "comment", "email"} {
		if _, ok := props[key]; ok {
			t.Fatalf("sensitive prop %q present in %v", key, props)
		}
	}
}

func TestCaptureViewTelemetrySuppressesDisabledSettings(t *testing.T) {
	withTempConfigHome(t)
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0", len(rec.events))
	}
}

func TestCaptureViewTelemetryCapturesWhenEnabled(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.events))
	}
	if rec.events[0] != telemetry.EventChatAttached {
		t.Fatalf("event = %q, want %q", rec.events[0], telemetry.EventChatAttached)
	}
	if got := rec.props[0]["source"]; got != "tui" {
		t.Fatalf("source = %v, want tui", got)
	}
	assertNoSensitiveTelemetryProps(t, rec.props[0])
}

func TestViewDistinctIDUsesHyphenatedSharedHelper(t *testing.T) {
	got := viewDistinctID()
	if !strings.HasPrefix(got, "local-") {
		t.Fatalf("viewDistinctID() = %q, want local- prefix", got)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("viewDistinctID() = %q, want no colon", got)
	}
}

func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}
