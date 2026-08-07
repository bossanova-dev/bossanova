package views

import (
	"context"
	"strings"
	"testing"
	"time"

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
	resetViewTelemetryGate()
	t.Cleanup(resetViewTelemetryGate)
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
	// The gate is cached with a TTL, so a value read under a previous test's
	// config home would otherwise leak into this one — in both directions.
	resetViewTelemetryGate()
	t.Cleanup(resetViewTelemetryGate)
}

// resetViewTelemetryGate drops the cached gate. Test-only: tests flip the
// settings file directly and must not observe a value cached under a previous
// test's config home.
func resetViewTelemetryGate() {
	viewTelemetryGate.mu.Lock()
	defer viewTelemetryGate.mu.Unlock()
	viewTelemetryGate.checkedAt = time.Time{}
	viewTelemetryGate.enabled = false
}

// TestViewTelemetryGateCachesWithinItsTTL pins that the opt-in gate is not a
// settings-file read per capture. config.Load is os.ReadFile + json.Unmarshal on
// Bubble Tea's update goroutine, and the trash delete-all batch drains one
// session per message, so an uncached gate puts N synchronous disk reads on the
// path the TUI rubric requires to stay non-blocking.
//
// Both halves step the clock explicitly through viewTelemetryEnabledAt rather
// than sleeping or resetting the gate. Resetting would zero checkedAt, and
// `now.Sub(time.Time{})` exceeds ANY finite TTL — so the expiry half would pass
// against a gate that latches forever, which is precisely the failure it claims
// to catch. Stepping the clock makes viewTelemetryGateTTL load-bearing in both
// directions, and removes the wall-clock flake of a >3s stall mid-test.
func TestViewTelemetryGateCachesWithinItsTTL(t *testing.T) {
	enableViewTelemetryForTest(t)
	base := time.Now()

	if !viewTelemetryEnabledAt(base) {
		t.Fatal("gate should be enabled after enableViewTelemetryForTest")
	}
	// Flip the file underneath the cache. Within the TTL the gate must NOT
	// re-read it; an uncached implementation returns false here.
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = false
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	if !viewTelemetryEnabledAt(base.Add(viewTelemetryGateTTL - time.Millisecond)) {
		t.Fatal("gate re-read the settings file within its TTL; every capture would pay a " +
			"synchronous os.ReadFile + json.Unmarshal on the Bubble Tea update goroutine")
	}

	// ...and it must re-read once the TTL elapses, or turning tracing OFF in
	// general settings would not stop events until the operator restarts.
	if viewTelemetryEnabledAt(base.Add(viewTelemetryGateTTL)) {
		t.Fatalf("gate did not re-read the settings file after %s; turning event tracing "+
			"off in general settings would not take effect until restart", viewTelemetryGateTTL)
	}

	// The two assertions above step by the TTL itself, so they pin the caching
	// MECHANISM for any value of it — including one so long the gate never
	// expires in a real session, which is the sync.Once behaviour the design
	// deliberately rejected. Bound the value separately.
	if viewTelemetryGateTTL > 5*time.Second {
		t.Fatalf("viewTelemetryGateTTL = %s. The gate exists to be re-read while the "+
			"operator is still looking at the screen: turning event tracing off in general "+
			"settings must stop events within seconds, not at the next restart.", viewTelemetryGateTTL)
	}
	if viewTelemetryGateTTL <= 0 {
		t.Fatalf("viewTelemetryGateTTL = %s, which disables the cache entirely and puts a "+
			"synchronous settings read back on every capture", viewTelemetryGateTTL)
	}
}
