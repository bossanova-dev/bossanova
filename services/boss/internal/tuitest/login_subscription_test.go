package tuitest_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

const subscriptionWaitingText = "Loading your account..."

func waitForSubscriptionWaitingView(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitForText(waitTimeout, subscriptionWaitingText); err != nil {
		t.Fatalf("expected subscription waiting view; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_ActiveSubscriptionReachesHome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessActive),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Your authentication code:"); err != nil {
		t.Fatalf("expected login device-code screen; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected home after scripted login; screen:\n%s", h.Driver.Screen())
	}
	if !strings.Contains(h.Driver.Screen(), "[l]ogout") {
		t.Fatalf("expected logged-in action bar; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_NeedsSubscriptionShowsWaitingView(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessNeedsSubscription),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	waitForSubscriptionWaitingView(t, h)
	// The action bar renders a frame after the loading view, so poll for each
	// action rather than checking the screen the instant the loading text appears.
	for _, want := range []string{"[enter] re-open subscription page", "[esc] cancel"} {
		if err := h.Driver.WaitForText(waitTimeout, want); err != nil {
			t.Fatalf("expected %q in subscription waiting view; screen:\n%s", want, h.Driver.Screen())
		}
	}
}

func TestTUI_LoginFlow_SubscriptionEnterReopensExistingCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessNeedsSubscription),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	waitForSubscriptionWaitingView(t, h)
	if err := h.Driver.WaitForText(waitTimeout, "[enter] re-open subscription page"); err != nil {
		t.Fatalf("expected subscription reopen action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	waitForSubscriptionWaitingView(t, h)
	if h.Driver.ScreenContains("[o]pen subscription page") {
		t.Fatalf("expected existing checkout reopen, not fresh checkout prompt; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_SubscriptionActivationReturnsHome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(
			tuitest.E2ECloudAccessNeedsSubscription,
			tuitest.E2ECloudAccessNeedsSubscription,
			tuitest.E2ECloudAccessPendingEntitlement,
			tuitest.E2ECloudAccessActive,
		),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
		tuitest.WithE2ECloudRefreshInterval("100ms"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	waitForSubscriptionWaitingView(t, h)
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected home after subscription activation; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogout"); err != nil {
		t.Fatalf("expected logged-in action bar; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_SubscriptionCancelIsRecoverable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessNeedsSubscription),
		tuitest.WithE2ECloudCheckoutURL("https://billing.example.test/checkout"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	waitForSubscriptionWaitingView(t, h)
	if err := h.Driver.WaitForText(waitTimeout, "[esc] cancel"); err != nil {
		t.Fatalf("expected subscription cancel action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected home after cancel; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[n]ew"); err != nil {
		t.Fatalf("expected home action bar after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_BillingUnavailableAllowsFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithE2ELogin("test-user@example.com"),
		tuitest.WithE2ECloudAccessSequence(tuitest.E2ECloudAccessBillingUnavailable),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out action bar; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	// The subscription flow surfaces the billing error and waits for the user to
	// acknowledge it before returning home.
	if err := h.Driver.WaitForText(waitTimeout, "Cloud billing unavailable. Local sessions are still available."); err != nil {
		t.Fatalf("expected billing unavailable message; screen:\n%s", h.Driver.Screen())
	}
	if !h.Driver.ScreenContains("[enter] continue") {
		t.Fatalf("expected continue action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected home after billing unavailable fallback; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogout"); err != nil {
		t.Fatalf("expected logged-in action bar; screen:\n%s", h.Driver.Screen())
	}
}
