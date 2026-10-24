package e2e_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// waitForLoggedInHome blocks until the home screen has finished fetching auth
// status and the action bar shows the "[l]ogout" entry. Every login test
// depends on that transition before pressing `l` — otherwise the key races
// the async authStatusMsg and we either get the login wizard or a silent no-op.
func waitForLoggedInHome(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogout"); err != nil {
		t.Fatalf("expected [l]ogout in action bar; screen:\n%s", h.Driver.Screen())
	}
}

// TestTUI_LoginView_DisplaysEmail seeds a logged-in user via the e2e auth
// store override, then asserts the action bar flips to "[l]ogout" and that
// pressing `l` surfaces the email in the logout-confirm modal. This exercises
// the full path Status() → authStatusMsg → HomeModel.loggedInEmail → rendered.
func TestTUI_LoginView_DisplaysEmail(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if h.Driver.ScreenContains("[l]ogin") {
		t.Fatalf("expected [l]ogin to be replaced by [l]ogout; screen:\n%s", h.Driver.Screen())
	}

	// Press `l` to open the logout-confirm modal, which embeds the current
	// email in its prompt.
	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected email in logout confirm prompt; screen:\n%s", h.Driver.Screen())
	}

	// Dismiss the modal so the test's t.Cleanup path doesn't race with a
	// lingering confirm dialog.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if !strings.Contains(h.Driver.Screen(), "Log out test-user@example.com?") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("logout confirm modal did not dismiss on esc; screen:\n%s", h.Driver.Screen())
}

// TestTUI_LoginView_Logout confirms that pressing `l` → `y` while logged in
// clears local auth state (action bar flips back to [l]ogin) and fires a
// NotifyAuthChange("logout") RPC so the daemon can drop any cached bearer
// token.
func TestTUI_LoginView_Logout(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected logout confirm; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('y'); err != nil {
		t.Fatal(err)
	}

	// Action bar flips back to [l]ogin once HomeModel clears loggedIn.
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected [l]ogin after logout; screen:\n%s", h.Driver.Screen())
	}

	// NotifyAuthChange("logout") is dispatched asynchronously via a tea.Cmd,
	// so poll until the mock daemon records it.
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) > 0 {
			if got := calls[len(calls)-1]; got != "logout" {
				t.Fatalf("NotifyAuthChange action = %q, want %q", got, "logout")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("NotifyAuthChange was never called; screen:\n%s", h.Driver.Screen())
}

// TestTUI_LoginView_LogoutCancel asserts that pressing `l` then `n` dismisses
// the confirm modal without calling NotifyAuthChange and without clearing the
// logged-in state — the action bar keeps showing [l]ogout.
func TestTUI_LoginView_LogoutCancel(t *testing.T) {
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithLoggedInUser("test-user@example.com"),
	)

	waitForLoggedInHome(t, h)

	if err := h.Driver.SendKey('l'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Log out test-user@example.com?"); err != nil {
		t.Fatalf("expected logout confirm; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}

	// Modal goes away, action bar still shows [l]ogout (we stayed logged in).
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return !strings.Contains(screen, "Log out test-user@example.com?") &&
			strings.Contains(screen, "[l]ogout")
	}); err != nil {
		t.Fatalf("expected modal dismissed and [l]ogout still shown; screen:\n%s", h.Driver.Screen())
	}

	// Give any stray tea.Cmd time to land, then confirm no RPC fired.
	time.Sleep(300 * time.Millisecond)
	if calls := h.Daemon.NotifyAuthChangeCalls(); len(calls) != 0 {
		t.Fatalf("NotifyAuthChange should not have been called on cancel; got %v", calls)
	}
}

const subscriptionWaitingText = "Loading your account…"

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
	h := tuitest.NewMock(t,
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
	h := tuitest.NewMock(t,
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
	h := tuitest.NewMock(t,
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
	h := tuitest.NewMock(t,
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
	h := tuitest.NewMock(t,
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
	// Cancelling the subscription wait must not sign the user out.
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogout"); err != nil {
		t.Fatalf("expected logged-in action bar after cancel; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_LoginFlow_BillingUnavailableAllowsFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
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
	return tuitest.NewMock(t, options...)
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

	// With no repos, onboarding completion lands on the Home empty state (which
	// guides the user to add a repository and offers [l]ogin while logged out).
	// The user is no longer auto-routed into the Add Repository wizard.
	if err := h.Driver.WaitForText(waitTimeout, "[l]ogin"); err != nil {
		t.Fatalf("expected logged-out home action bar; screen:\n%s", h.Driver.Screen())
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

// TestTUI_OnboardingFlow_FirstRepoLandsOnSessionEmptyState is the BOS-664
// regression: a brand-new user who finishes provider onboarding and then
// registers their very first repository must land on Home's no-active-sessions
// empty state, NOT in the New Session wizard. Users read the wizard's
// "What are you working on?" prompt as a dead end because they never asked to
// start a session.
//
// The whole fresh-user path is driven here — provider onboarding, the zero-repo
// welcome screen, [enter] into the add wizard, and the full add — because the
// unit-level witness (TestRepoAddCompletedFirstRepoGoesHome) only proves the
// routing branch, not that the flow actually reaches it or what it renders.
//
// The assertion is positive-first: the screen must carry Home's empty-session
// guidance and the daemon must hold exactly one repo, so the absence of the New
// Session prompt cannot pass vacuously on some earlier, still-loading screen.
func TestTUI_OnboardingFlow_FirstRepoLandsOnSessionEmptyState(t *testing.T) {
	h := newFreshOnboardingHarness(t, fakeProviderPath(t, "claude", "codex"))
	// Pin validation so the add wizard advances deterministically regardless of
	// what is on the developer's disk.
	h.Daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		OriginUrl:     "https://github.com/acme/widgets.git",
		DefaultBranch: "main",
	})

	completeProviderOnboarding(t, h)

	// Onboarding completion lands a zero-repo user on the welcome empty state,
	// whose primary action is [enter] add repository.
	//
	// Wait on Home's own copy and action bar, NOT on the "Welcome to Bossanova"
	// banner: onboarding runs as its own tea.Program and prints that same banner,
	// so matching it can return while the onboarding program is still tearing
	// down — and a key sent during that handoff is read by nobody.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "To get started, you need to add a repository") &&
			strings.Contains(screen, "[enter] add repository")
	}); err != nil {
		t.Fatalf("expected zero-repo home after onboarding; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("open add-repo wizard from zero-repo home: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Open project"); err != nil {
		t.Fatalf("expected add-repo source phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("pick 'Open project': %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add a local repository"); err != nil {
		t.Fatalf("expected add-repo input phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendString("widgets"); err != nil {
		t.Fatalf("type repository path: %v", err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatalf("submit repository path: %v", err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merge strategy"); err != nil {
		t.Fatalf("expected add-repo details phase; screen:\n%s", h.Driver.Screen())
	}
	completeRepoAddDetails(t, h)

	// The registration really happened: exactly one repo, and no sessions were
	// ever created, so Home must be in its no-sessions (not no-repos) state.
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		return len(h.Daemon.Repos()) == 1
	}); err != nil {
		t.Fatalf("expected exactly 1 registered repo, got %d; screen:\n%s",
			len(h.Daemon.Repos()), h.Driver.Screen())
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "You have no active sessions.") &&
			strings.Contains(screen, "Press 'n' to create a new session.")
	}); err != nil {
		t.Fatalf("expected Home no-active-sessions guidance after first repo add; screen:\n%s",
			h.Driver.Screen())
	}

	screen := h.Driver.Screen()
	// Belt-and-braces only. "What are you working on?" is the New Session prompt's
	// placeholder, rendered only once that field is focused and empty — under the
	// real regression the wizard opens on its mode picker first, so this check does
	// NOT fire. The positive WaitFor above is what actually catches the bug; do not
	// trim it and keep this one.
	if strings.Contains(screen, "What are you working on?") {
		t.Fatalf("first repo registration must not dump the user into the New Session wizard; screen:\n%s", screen)
	}
	// The welcome/zero-repo state is equally wrong here: the repo exists now.
	if strings.Contains(screen, "Press Enter to add your first repository") {
		t.Fatalf("expected the no-sessions empty state, not the zero-repo welcome; screen:\n%s", screen)
	}
}
