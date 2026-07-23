package views

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// resumeTickCmd exists to restart the self-perpetuating tick chain in views
// that drive periodic status refreshes via tickMsg. The bug-report modal
// swallows tickMsg while it's open, so the chain needs restarting when the
// modal dismisses back to a tick-driven view — otherwise daemon statuses
// silently stop refreshing until the user navigates away and back.
func TestResumeTickCmd(t *testing.T) {
	tickDriven := []View{ViewHome, ViewChatPicker}
	for _, v := range tickDriven {
		if resumeTickCmd(v) == nil {
			t.Errorf("resumeTickCmd(%v) returned nil; expected a tick command", v)
		}
	}

	notTickDriven := []View{
		ViewNewSession,
		ViewAttach,
		ViewRepoAdd,
		ViewRepoList,
		ViewRepoSettings,
		ViewTrash,
		ViewSettings,
		ViewSessionSettings,
		ViewLogin,
		ViewBugReport,
	}
	for _, v := range notTickDriven {
		if resumeTickCmd(v) != nil {
			t.Errorf("resumeTickCmd(%v) returned non-nil; expected nil", v)
		}
	}
}

func TestAppAttachDetachWiresChatPickerTelemetry(t *testing.T) {
	rec := &fakeTelemetry{}
	a := App{
		ctx:        context.Background(),
		activeView: ViewAttach,
		telemetry:  rec,
		width:      80,
		height:     24,
	}
	a.attach = NewAttachModel(nil, a.ctx, nil, "session-1", "")
	a.attach.agentSessionID = "agent-1"
	a.attach.detach = true

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ViewChatPicker {
		t.Fatalf("activeView = %v, want %v", got.activeView, ViewChatPicker)
	}
	if got.chatPicker.telemetry != rec {
		t.Fatal("chat picker telemetry was not preserved after attach detach")
	}
}

func TestAppSwitchToHomePreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.home.cloudAccess = nil

	cmd := a.switchToHome()
	if cmd == nil {
		t.Fatal("switchToHome returned nil cmd")
	}
	if a.home.cloudAccess != cloud {
		t.Fatal("cloud access client was not preserved after switching home")
	}
}

func TestAppSwitchViewHomePreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.home.cloudAccess = nil

	model, _ := a.Update(switchViewMsg{view: ViewHome})
	got := model.(App)

	if got.home.cloudAccess != cloud {
		t.Fatal("cloud access client was not preserved after direct home navigation")
	}
}

func TestAppSettingsBillingUsesAccountReturnURL(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{portalURL: "https://billing.example.test/portal"}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.WithCheckoutURLs(
		"https://app.example.test/subscribe/success?source=cli",
		"https://app.example.test/subscribe/canceled?source=cli",
	)

	openedURL := ""
	originalOpen := openBillingPortalURL
	openBillingPortalURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	defer func() { openBillingPortalURL = originalOpen }()

	model, _ := a.Update(switchViewMsg{view: ViewSettings})
	got := model.(App)
	model, cmd := got.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	got = model.(App)
	if cmd == nil {
		t.Fatal("billing key returned nil cmd")
	}
	got.Update(cmd())

	if len(cloud.portalReturnURLs) != 1 {
		t.Fatalf("portal return URLs = %v, want one call", cloud.portalReturnURLs)
	}
	if got, want := cloud.portalReturnURLs[0], "https://app.example.test/settings/account?source=cli"; got != want {
		t.Fatalf("portal return URL = %q, want %q", got, want)
	}
	if openedURL != cloud.portalURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, cloud.portalURL)
	}
}

func TestAppWithSettingsPassesSettingsToHome(t *testing.T) {
	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true

	a := NewApp(nil, nil)
	a.WithSettings(settings)

	if !a.home.settings.BossCloudGuestOfferHidden {
		t.Fatal("home.settings.BossCloudGuestOfferHidden = false, want true")
	}
}

func TestAppSettingsNotificationToggleAppliesToHomeImmediately(t *testing.T) {
	withTempConfigHome(t)

	a := NewApp(nil, nil)
	a.activeView = ViewSettings
	a.settings = NewSettingsModel(nil, a.ctx)

	for i, row := range a.settings.rows {
		if row.Kind == settingsRowKindNotifications {
			a.settings.cursor = i
			break
		}
	}
	if !config.NotificationsEnabled(a.home.settings) {
		t.Fatal("precondition: home notifications should default to enabled")
	}

	model, _ := a.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	got := model.(App)
	if config.NotificationsEnabled(got.userSettings) {
		t.Fatal("settings toggle did not update App.userSettings")
	}
	if config.NotificationsEnabled(got.home.settings) {
		t.Fatal("settings toggle did not update HomeModel immediately")
	}
}

func TestAppSettingsSuccessfulOtherSaveAfterNotificationFailureAppliesToHome(t *testing.T) {
	withTempConfigHome(t)

	a := NewApp(nil, nil)
	a.activeView = ViewSettings
	a.settings = NewSettingsModel(nil, a.ctx)
	for i, row := range a.settings.rows {
		if row.Kind == settingsRowKindNotifications {
			a.settings.cursor = i
			break
		}
	}

	badPath := filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", badPath, err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
	model, _ := a.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	a = model.(App)
	if a.settings.err == nil {
		t.Fatal("failed notification save did not retain an error")
	}

	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	for i, row := range a.settings.rows {
		if row.Kind == settingsRowKindErrorTracking {
			a.settings.cursor = i
			break
		}
	}
	model, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(App)
	if got.settings.err != nil {
		t.Fatalf("successful other save retained stale error: %v", got.settings.err)
	}
	if config.NotificationsEnabled(got.userSettings) {
		t.Fatal("successful other save did not update App.userSettings notification setting")
	}
	if config.NotificationsEnabled(got.home.settings) {
		t.Fatal("successful other save did not update HomeModel notification setting")
	}
	if !got.home.settings.ErrorTrackingEnabled {
		t.Fatal("successful other save did not update HomeModel changed setting")
	}
}

func TestAppDiscardsSessionPollFromReplacedHome(t *testing.T) {
	a := NewApp(nil, nil)
	oldGeneration := a.home.generation
	oldPollID := a.home.nextSessionPollID

	a.switchToHome()
	if a.home.generation == oldGeneration {
		t.Fatal("replacement HomeModel reused its generation")
	}

	model, _ := a.Update(sessionListMsg{
		homeGeneration: oldGeneration,
		pollID:         oldPollID,
		sessions:       []*pb.Session{{Id: "old-session", HasActiveChat: true}},
	})
	got := model.(App)
	if !got.home.loading {
		t.Fatal("stale poll cleared replacement HomeModel loading state")
	}
	if len(got.home.sessions) != 0 {
		t.Fatalf("stale poll populated replacement HomeModel sessions: %+v", got.home.sessions)
	}
	if got.rotationSeen != nil {
		t.Fatalf("stale poll mutated App rotation state: %+v", got.rotationSeen)
	}
}

// TestNewHomeModelPreservesSessionStart guards the cloud-offer session-fatigue
// timer: recreating Home (on every navigation back to the list) must reuse the
// App's session epoch, not reset it to "now". Otherwise the 60s guest offer
// reappears with a fresh clock on each round-trip to Home.
func TestNewHomeModelPreservesSessionStart(t *testing.T) {
	epoch := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	a := NewApp(nil, nil)
	a.startedAt = epoch

	home := a.newHomeModel()
	if !home.startedAt.Equal(epoch) {
		t.Fatalf("newHomeModel startedAt = %v, want %v (session epoch must survive Home recreation)", home.startedAt, epoch)
	}
}

// TestAppHomeKeepsCloudOfferHiddenAcrossNavigationAfterSessionLimit exercises
// the full path: once the session-fatigue window has elapsed, navigating back
// to Home must not re-show the guest cloud offer.
func TestAppHomeKeepsCloudOfferHiddenAcrossNavigationAfterSessionLimit(t *testing.T) {
	epoch := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = epoch

	a := NewApp(nil, &auth.Manager{})
	a.WithSettings(settings)
	a.startedAt = epoch

	a.switchToHome()
	a.home.repoCount = 1
	a.home.loading = false
	a.home.loggedIn = false
	a.home.sessions = []*pb.Session{{Id: "sess-1", Title: "Active work"}}
	a.home.buildTableRows()
	a.home.now = func() time.Time { return epoch.Add(cloudGuestOfferSessionLimit + time.Second) }

	content := a.home.View().Content
	if strings.Contains(content, "Bossanova Cloud") {
		t.Fatalf("cloud offer reappeared after navigation past the session limit: %q", content)
	}
}

// TestAppLatchValueDeliveredPersistsAcrossHomeRecreation guards the headline
// acceptance criterion that BossCloudValueDeliveredAt, once set, is persisted
// and never moves. The home model latches into its own settings copy, so the
// App must propagate that back to userSettings — otherwise every return-to-home
// recreates the home from the stale startup snapshot, re-stamping the latch
// (moving the timestamp) and reverting the promo once has_active_chat drops.
func TestAppLatchValueDeliveredPersistsAcrossHomeRecreation(t *testing.T) {
	epoch := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	// Stub the persister so the latch never touches real config on disk.
	prevSave := saveSettings
	saveSettings = func(config.Settings) error { return nil }
	defer func() { saveSettings = prevSave }()

	a := NewApp(nil, &auth.Manager{})
	a.WithSettings(config.DefaultSettings())
	a.startedAt = epoch
	a.activeView = ViewHome
	a.home.now = func() time.Time { return epoch }
	a.home.repoCount = 1
	a.home.loading = false

	// A session with an active chat delivers value (repo + session + chat).
	model, _ := a.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", HasActiveChat: true}}})
	a = model.(App)

	if a.userSettings.BossCloudValueDeliveredAt.IsZero() {
		t.Fatal("latch must propagate to App.userSettings so home recreations keep it")
	}
	latched := a.userSettings.BossCloudValueDeliveredAt

	// A return-to-home recreates the home model; it must re-seed the latch.
	home := a.newHomeModel()
	if !home.settings.BossCloudValueDeliveredAt.Equal(latched) {
		t.Fatalf("recreated home lost the latch: got %v want %v", home.settings.BossCloudValueDeliveredAt, latched)
	}

	// A later poll with no active chat must NOT move or clear the timestamp.
	home.now = func() time.Time { return epoch.Add(time.Hour) }
	home.repoCount = 1
	m2, _ := home.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", HasActiveChat: false}}})
	home = m2.(HomeModel)
	if !home.settings.BossCloudValueDeliveredAt.Equal(latched) {
		t.Fatalf("latch moved or reverted after recreation: got %v want %v", home.settings.BossCloudValueDeliveredAt, latched)
	}
}

func TestAppChatPickerReturnPreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.activeView = ViewChatPicker
	a.chatPicker = ChatPickerModel{cancel: true, sessionID: "session-1"}

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.home.cloudAccess != cloud {
		t.Fatal("cloud access client was not preserved after chat picker return")
	}
}

func TestAppHomeSubscribeKeyShowsSubscriptionWaitingView(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{
		status:      &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION},
		checkoutURL: "https://billing.example.test/checkout",
	}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.activeView = ViewHome
	a.home.loading = false
	a.home.loggedIn = true
	a.home.repoCount = 1
	a.home.cloudStatus = cloud.status

	openedURL := ""
	originalOpen := openSubscriptionCheckoutURL
	openSubscriptionCheckoutURL = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}
	defer func() { openSubscriptionCheckoutURL = originalOpen }()

	model, cmd := a.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	got := model.(App)
	if cmd == nil {
		t.Fatal("subscribe key returned nil cmd")
	}

	msg := cmd()
	model, cmd = got.Update(msg)
	got = model.(App)
	if got.activeView != ViewLogin {
		t.Fatalf("activeView = %v, want %v", got.activeView, ViewLogin)
	}
	if cmd == nil {
		t.Fatal("subscription flow start returned nil cmd")
	}
	msg = cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("subscription flow start returned %T, want tea.BatchMsg", msg)
	}
	if len(batch) != 2 {
		t.Fatalf("subscription flow start batch len = %d, want subscription check + spinner tick", len(batch))
	}

	msg = batch[0]()
	model, cmd = got.Update(msg)
	got = model.(App)
	if cmd == nil {
		t.Fatal("subscription access update returned nil checkout cmd")
	}

	msg = cmd()
	model, cmd = got.Update(msg)
	got = model.(App)
	if got.login.subscription.phase != subscriptionPhaseWaiting {
		t.Fatalf("subscription phase = %v, want waiting", got.login.subscription.phase)
	}
	if !strings.Contains(got.View().Content, "Loading your account...") {
		t.Fatalf("login view missing waiting copy: %q", got.View().Content)
	}
	if cmd == nil {
		t.Fatal("subscription checkout update returned nil wait batch")
	}
	msg = cmd()
	waitBatch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("subscription checkout update returned %T, want tea.BatchMsg", msg)
	}
	if len(waitBatch) < 1 {
		t.Fatal("subscription checkout wait batch is empty")
	}
	msg = waitBatch[0]()
	model, _ = got.Update(msg)
	got = model.(App)
	if openedURL != cloud.checkoutURL {
		t.Fatalf("opened URL = %q, want %q", openedURL, cloud.checkoutURL)
	}
}

// A successful add of a second repo returns to the repo list with the new repo
// highlighted (its options were configured inline on the add wizard), rather
// than diverting to a settings screen.
func TestRepoAddCompletedSecondRepoGoesToList(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewRepoAdd
	a.repoAddCompleting = true
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(repoAddCompletedMsg{repos: repos, highlightID: "r2"})
	got := model.(App)

	if got.repoAddCompleting {
		t.Fatal("repoAddCompleting should be reset to false")
	}
	if got.activeView != ViewRepoList {
		t.Fatalf("activeView = %v, want ViewRepoList", got.activeView)
	}
	if got.repoList.highlightRepoID != "r2" {
		t.Fatalf("highlightRepoID = %q, want %q", got.repoList.highlightRepoID, "r2")
	}
}

// The first-ever repo (len(repos) <= 1, not opened from Settings) returns to
// Home rather than diverting to a settings screen.
func TestRepoAddCompletedFirstRepoGoesHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}}

	model, _ := a.Update(repoAddCompletedMsg{repos: repos, highlightID: "r1"})
	got := model.(App)

	if got.activeView != ViewHome {
		t.Fatalf("first repo add should return Home, got %v", got.activeView)
	}
}

// A failed add preserves the prior fallback (repo list), without diverting to
// a settings screen for a repo that may not exist.
func TestRepoAddCompletedErrorFallsBackToList(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(repoAddCompletedMsg{repos: repos, err: errors.New("boom"), highlightID: ""})
	got := model.(App)

	if got.activeView != ViewRepoList {
		t.Fatalf("on error want ViewRepoList, got %v", got.activeView)
	}
}

// Defensive: success but no usable repo id falls back to the prior behavior
// rather than opening settings for a missing repo.
func TestRepoAddCompletedEmptyIDFallsBack(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(repoAddCompletedMsg{repos: repos, highlightID: ""})
	got := model.(App)

	if got.activeView != ViewRepoList {
		t.Fatalf("empty id should fall back to list, got %v", got.activeView)
	}
}
