package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
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

	CaptureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0", len(rec.events))
	}
}

func TestCaptureViewTelemetryCapturesWhenEnabled(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	CaptureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
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

// TestCaptureViewTelemetryEmitsOnTheFunnelDistinctID is the sharpest anchor of
// the cutover. This test previously asserted that viewDistinctID() had a
// "local-" prefix and contained NO colon — exactly what a funnel id
// (user:<sub> / email:<address>) is. A logged-in TUI event must now land on the
// funnel person the web and bosso events target, so both halves of that old
// assertion are inverted here for the logged-in case, and preserved in
// TestViewDistinctIDFallsBackToLocalWithoutAnEmail for the anonymous one.
//
// It asserts on the id a *captured event* carries, not just on the resolver's
// return value: the defect was persons events landed on, so the event is the
// artifact under test.
func TestCaptureViewTelemetryEmitsOnTheFunnelDistinctID(t *testing.T) {
	enableViewTelemetryForTest(t)
	withViewTelemetryEmail(t, "  Person@Example.COM ")
	rec := &fakeTelemetry{}

	CaptureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.distinctIDs) != 1 {
		t.Fatalf("distinctIDs = %d, want 1", len(rec.distinctIDs))
	}
	got := rec.distinctIDs[0]
	want := telemetry.FunnelDistinctID("", "person@example.com")
	if got != want {
		t.Fatalf("captured distinctID = %q, want %q", got, want)
	}
	if got == telemetry.UserDistinctID("person@example.com") {
		t.Fatalf("captured distinctID = %q, still the retired user-<hash> namespace", got)
	}
	if got == viewLocalDistinctIDForTest(t) {
		t.Fatalf("captured distinctID = %q, still the pre-login local-<hash> namespace", got)
	}
	if !strings.Contains(got, ":") {
		t.Fatalf("captured distinctID = %q, want the colon-separated funnel form", got)
	}
}

// TestViewEventsShareTheCLIFunnelDistinctID pins the TUI half of the
// cross-surface parity claim. Both surfaces resolve through
// telemetry.LocalFunnelDistinctID, and services/boss/cmd/telemetry_test.go pins
// the CLI half against the same expression, so the two ids are equal by
// construction rather than by two call sites happening to agree. Package
// boundaries make a single test that calls both impossible.
func TestViewEventsShareTheCLIFunnelDistinctID(t *testing.T) {
	enableViewTelemetryForTest(t)
	withViewTelemetryEmail(t, "person@example.com")
	rec := &fakeTelemetry{}

	CaptureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	// The literal, not telemetry.LocalFunnelDistinctID(home, ...): computing the
	// expectation with the same production expression the code under test
	// evaluates makes this assertion unfalsifiable — a regression back to the
	// local- namespace would move both sides together. The CLI half in
	// services/boss/cmd/telemetry_test.go pins this same literal, and a shared
	// literal is the only thing that can pin parity across a package boundary.
	const want = "email:person@example.com"
	if len(rec.distinctIDs) != 1 || rec.distinctIDs[0] != want {
		t.Fatalf("captured distinctIDs = %#v, want [%q]", rec.distinctIDs, want)
	}
}

// TestViewDistinctIDFallsBackToLocalWithoutAnEmail keeps the anonymous machine
// on a stable per-home identity — the one the login-time alias bridges forward.
// "anonymous" would collapse every unauthenticated machine into one person.
func TestViewDistinctIDFallsBackToLocalWithoutAnEmail(t *testing.T) {
	withTempConfigHome(t)
	withViewTelemetryEmail(t, "")

	got := viewDistinctID()
	want := viewLocalDistinctIDForTest(t)
	if got != want {
		t.Fatalf("viewDistinctID() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "local-") {
		t.Fatalf("viewDistinctID() = %q, want local- prefix", got)
	}
	if got == "anonymous" {
		t.Fatalf("viewDistinctID() = %q, which merges every anonymous machine into one person", got)
	}
}

// TestViewTelemetryEmailIsCachedWithinItsTTL pins that resolving the identity is
// not a keychain read per capture. Every capture runs on Bubble Tea's update
// goroutine, which the TUI rubric requires to stay non-blocking.
//
// Both halves step the clock through viewTelemetrySignedInEmailAt rather than
// sleeping. Resetting the cache instead would zero checkedAt, and
// now.Sub(time.Time{}) exceeds ANY finite TTL — so the expiry half would pass
// against a cache that latches forever, precisely the failure it claims to catch.
func TestViewTelemetryEmailIsCachedWithinItsTTL(t *testing.T) {
	withTempConfigHome(t)
	calls := 0
	original := viewTelemetryEmailLookup
	viewTelemetryEmailLookup = func() string {
		calls++
		return "person@example.com"
	}
	resetViewTelemetryIdentity()
	t.Cleanup(func() {
		viewTelemetryEmailLookup = original
		resetViewTelemetryIdentity()
	})

	start := time.Now()
	for range 3 {
		viewTelemetrySignedInEmailAt(start)
	}
	if calls != 1 {
		t.Fatalf("lookup calls within the TTL = %d, want 1", calls)
	}

	viewTelemetrySignedInEmailAt(start.Add(viewTelemetryGateTTL))
	if calls != 2 {
		t.Fatalf("lookup calls after the TTL = %d, want 2", calls)
	}
}

// withViewTelemetryEmail replaces the keychain lookup and drops the cached
// identity in both directions, so a value cached under another test's
// environment cannot leak in or out.
func withViewTelemetryEmail(t *testing.T, email string) {
	t.Helper()
	original := viewTelemetryEmailLookup
	viewTelemetryEmailLookup = func() string { return email }
	resetViewTelemetryIdentity()
	t.Cleanup(func() {
		viewTelemetryEmailLookup = original
		resetViewTelemetryIdentity()
	})
}

// resetViewTelemetryIdentity drops the cached signed-in email. Test-only.
func resetViewTelemetryIdentity() {
	viewTelemetryIdentity.mu.Lock()
	defer viewTelemetryIdentity.mu.Unlock()
	viewTelemetryIdentity.checkedAt = time.Time{}
	viewTelemetryIdentity.email = ""
}

func viewLocalDistinctIDForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return telemetry.LocalDistinctID(home)
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

// withTempConfigHome redirects config.Path() at a per-test temp directory.
//
// It points BOSS_SETTINGS_PATH directly at a fresh temp settings.json, which is
// what config.Path() actually consults first — redirecting HOME alone is not
// enough, because BOSS_SETTINGS_PATH (set in the developer's shell via direnv)
// takes precedence and would otherwise route config.Save() straight at the real
// settings.json. HOME is redirected too so any HOME-derived paths (e.g.
// DefaultSettings' worktree_base_dir) also land in the temp tree.
//
// Mirrors the isolation that cmd/testmain_test.go gives the cmd package.
func withTempConfigHome(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	if runtime.GOOS != "darwin" {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	}
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tempHome, "settings.json"))
	// viewDistinctID resolves the signed-in email through the keychain, and on
	// darwin the system Keychain is per-user rather than per-HOME, so the lines
	// above do not redirect it. Pin the file backend the way
	// enableCommandTelemetryForTest does, or these tests read the developer's
	// real login keychain and can raise an authorization prompt that hangs a
	// non-interactive run.
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
}
