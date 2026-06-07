package tuitest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

func fakeProviderPath(t *testing.T, commands ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, command := range commands {
		path := filepath.Join(dir, command)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake provider %q: %v", command, err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func emptyProviderPath(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for _, command := range []string{"tmux", "bash", "tee"} {
		path := filepath.Join(dir, command)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake command %q: %v", command, err)
		}
	}
	return dir
}

// newFreshOnboardingHarness launches boss into the first-run onboarding gate.
// WithFirstRunOnboarding seeds settings that leave providers unacknowledged and
// points socket_path at the mock daemon while leaving BOSS_SOCKET unset, so boss
// runs the onboarding preflight yet still dials the mock daemon directly — no
// socket proxy required. providerPath becomes the boss subprocess PATH (WithEnv
// is appended last, so it wins).
func newFreshOnboardingHarness(t *testing.T, providerPath string, opts ...tuitest.Option) *tuitest.Harness {
	t.Helper()

	options := []tuitest.Option{
		tuitest.WithFirstRunOnboarding(),
		tuitest.WithEnv("PATH=" + providerPath),
	}
	options = append(options, opts...)
	return tuitest.New(t, options...)
}

func completeProviderOnboarding(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "Choose providers to enable"); err != nil {
		t.Fatalf("expected provider onboarding welcome; screen:\n%s", h.Driver.Screen())
	}
	// Providers default to enabled; confirm both are checked rather than relying
	// on a toggle that would never fire.
	screen := h.Driver.Screen()
	for _, want := range []string{"[x] Claude", "[x] Codex"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("expected provider %q enabled by default; screen:\n%s", want, screen)
		}
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("confirm default providers: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Dangerous mode"); err != nil {
		t.Fatalf("expected dangerous-mode prompt; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("confirm dangerous-mode prompt: %v", err)
	}
}

func loginFromHome(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if !strings.Contains(h.Driver.Screen(), "[l]ogin") {
		if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
			t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
		}
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatalf("press login: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Your authentication code:"); err != nil {
		t.Fatalf("expected login device-code screen; screen:\n%s", h.Driver.Screen())
	}
}

func assertHomeAfterLogin(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, "To get started, you need to add a repository"); err != nil {
		t.Fatalf("expected home after scripted login; screen:\n%s", h.Driver.Screen())
	}
	if !strings.Contains(h.Driver.Screen(), "[l]ogout") {
		t.Fatalf("expected logged-in action bar; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_OnboardingFlow_ActiveSubscriptionReachesHome(t *testing.T) {
	h := newFreshOnboardingHarness(t,
		fakeProviderPath(t, "claude", "codex"),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessActive),
	)

	completeProviderOnboarding(t, h)
	loginFromHome(t, h)
	assertHomeAfterLogin(t, h)
}

func TestTUI_OnboardingFlow_NoProvidersShowsInstallInstructions(t *testing.T) {
	h := newFreshOnboardingHarness(t, emptyProviderPath(t))

	if err := h.Driver.WaitForText(waitTimeout, "Install an agent provider"); err != nil {
		t.Fatalf("expected install-required screen; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	// Assert the actual install instructions render — these only appear on the
	// install-required screen, unlike the static provider names which would show
	// even if detection were broken.
	for _, want := range []string{
		"install: npm install -g @anthropic-ai/claude-code",
		"https://docs.claude.com/en/docs/claude-code/overview",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("expected install instruction %q; screen:\n%s", want, screen)
		}
	}
	if h.Driver.ScreenContains("[l]ogin") {
		t.Fatalf("install-required onboarding should not expose login action; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_OnboardingFlow_DangerousModeToggleChecksBox(t *testing.T) {
	h := newFreshOnboardingHarness(t,
		fakeProviderPath(t, "claude", "codex"),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessActive),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Choose providers to enable"); err != nil {
		t.Fatalf("expected provider onboarding; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("confirm providers: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Dangerous mode"); err != nil {
		t.Fatalf("expected dangerous-mode prompt; screen:\n%s", h.Driver.Screen())
	}
	if !h.Driver.ScreenContains("[ ] Use 'dangerous mode' by default") {
		t.Fatalf("dangerous mode should default off; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatalf("toggle dangerous mode: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[x] Use 'dangerous mode' by default"); err != nil {
		t.Fatalf("expected dangerous mode checked after toggle; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("confirm dangerous mode: %v", err)
	}
	loginFromHome(t, h)
	assertHomeAfterLogin(t, h)
}

func TestTUI_OnboardingFlow_NeedsSubscriptionShowsWaitingView(t *testing.T) {
	h := newFreshOnboardingHarness(t,
		fakeProviderPath(t, "claude", "codex"),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessNeedsSubscription),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
	)

	completeProviderOnboarding(t, h)
	loginFromHome(t, h)
	waitForSubscriptionWaitingView(t, h)

	screen := h.Driver.Screen()
	for _, want := range []string{
		subscriptionWaitingText,
		"[enter] re-open subscription page",
		"[esc] cancel",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("expected subscription waiting view to include %q; screen:\n%s", want, screen)
		}
	}
}

func TestTUI_OnboardingFlow_SubscriptionActivationReturnsHome(t *testing.T) {
	h := newFreshOnboardingHarness(t,
		fakeProviderPath(t, "claude", "codex"),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(
			tuitest.E2ECloudAccessNeedsSubscription,
			tuitest.E2ECloudAccessPendingEntitlement,
			tuitest.E2ECloudAccessActive,
		),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
		tuitest.WithE2ECloudRefreshInterval("100ms"),
	)

	completeProviderOnboarding(t, h)
	loginFromHome(t, h)
	waitForSubscriptionWaitingView(t, h)
	assertHomeAfterLogin(t, h)
}

func TestTUI_OnboardingFlow_SubscriptionCancelIsRecoverable(t *testing.T) {
	h := newFreshOnboardingHarness(t,
		fakeProviderPath(t, "claude", "codex"),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessNeedsSubscription),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
	)

	completeProviderOnboarding(t, h)
	loginFromHome(t, h)
	waitForSubscriptionWaitingView(t, h)

	if !h.Driver.ScreenContains("[esc] cancel") {
		t.Fatalf("expected subscription cancel action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatalf("cancel subscription waiting view: %v", err)
	}
	assertHomeAfterLogin(t, h)
}
