package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/home"

	"github.com/recurser/boss/api/tui/attach"

	"github.com/recurser/boss/api/tui/chatpicker"

	"github.com/recurser/boss/api/tui/newsession"

	"github.com/recurser/boss/api/tui/settings"

	repospkg "github.com/recurser/boss/api/tui/repos"

	"charm.land/bubbles/v2/table"
	accountscreen "github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/login"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// resumeTickCmd exists to restart the self-perpetuating tick chain in views
// that drive periodic status refreshes via tickMsg. The bug-report modal
// swallows tickMsg while it's open, so the chain needs restarting when the
// modal dismisses back to a tick-driven view — otherwise daemon statuses
// silently stop refreshing until the user navigates away and back.
func TestResumeTickCmd(t *testing.T) {
	tickDriven := []ui.View{ui.ViewHome, ui.ViewChatPicker}
	for _, v := range tickDriven {
		if resumeTickCmd(v) == nil {
			t.Errorf("resumeTickCmd(%v) returned nil; expected a tick command", v)
		}
	}

	notTickDriven := []ui.View{
		ui.ViewNewSession,
		ui.ViewAttach,
		ui.ViewRepoAdd,
		ui.ViewRepoList,
		ui.ViewRepoSettings,
		ui.ViewTrash,
		ui.ViewSettings,
		ui.ViewSessionSettings,
		ui.ViewLogin,
		ui.ViewBugReport,
	}
	for _, v := range notTickDriven {
		if resumeTickCmd(v) != nil {
			t.Errorf("resumeTickCmd(%v) returned non-nil; expected nil", v)
		}
	}
}

func TestAppAttachDetachWiresChatPickerTelemetry(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	a := App{
		client:     &pickerAgentsFixture{},
		ctx:        context.Background(),
		activeView: ui.ViewAttach,
		telemetry:  rec,
		width:      80,
		height:     24,
	}
	a.attach = NewAttachModel(nil, a.ctx, nil, "session-1", "")
	detached, _ := a.attach.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a.attach = detached.(attach.Model)

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.activeView != ui.ViewChatPicker {
		t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewChatPicker)
	}
	probe := chatpicker.NewModel(nil, a.ctx, "session-1", "", chatpicker.ExternalActions{SupportsNewTab: true, OpenNewTab: func(string) error { return nil }})
	probe, _ = updateModel(probe, chatpicker.SessionMsg{Session: &pb.Session{Id: "session-1", WorktreePath: "/tmp/session"}})
	_, tabCmd := updateModel(probe, keyPress('t'))
	got.chatPicker, _ = updateModel(got.chatPicker, tabCmd())
	if len(rec.events) != 1 {
		t.Fatalf("chat picker open events = %d, want 1 after detach", len(rec.events))
	}
	got.chatPicker = loadPickerAgents(got.chatPicker)
	got.chatPicker, _ = updateModel(got.chatPicker, chatpicker.ChatsListedMsg{})
	got.chatPicker, _ = updateModel(got.chatPicker, keyPress('n'))
	got.chatPicker, _ = updateModel(got.chatPicker, tea.KeyPressMsg{Code: tea.KeyDown})
	_, pickCmd := updateModel(got.chatPicker, tea.KeyPressMsg{Code: tea.KeyEnter})
	if pickCmd == nil {
		t.Fatal("agent confirmation did not navigate")
	}
	if _, err := config.Load(); err != nil {
		t.Fatalf("agent selection did not persist settings: %v", err)
	}
	stored, _ := config.Load()
	if stored.DefaultAgent != "codex" {
		t.Fatalf("saved default agent = %q, want codex", stored.DefaultAgent)
	}

}

// TestAppChatPickerModelWiresAgentDefaults pins that the shared constructor
// installs both halves of the agent-picker contract: a persistence handler, and
// a preferred agent read from settings. The configured value is deliberately
// not "claude", so the assertion cannot pass on the DefaultSettings seed.
func TestAppChatPickerModelWiresAgentDefaults(t *testing.T) {
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.DefaultAgent = "codex"
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	a := App{client: &pickerAgentsFixture{}, ctx: context.Background(), width: 80, height: 24}

	m := a.newChatPickerModel("session-1", "")

	m = loadPickerAgents(m)
	m, _ = updateModel(m, chatpicker.ChatsListedMsg{})
	m, _ = updateModel(m, keyPress('n'))
	var cmd tea.Cmd
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("default agent confirmation did not navigate")
	}
	if got := cmd().(ui.SwitchViewMsg).AgentName; got != "codex" {
		t.Fatalf("default agent = %q, want codex", got)
	}
	// Selecting a different agent must update persisted settings too.
	m, _ = updateModel(m, keyPress('n'))
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyUp})
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	stored, err := config.Load()
	if err != nil || stored.DefaultAgent != "claude" {
		t.Fatalf("saved settings = %v, %v, want claude", stored, err)
	}
	if m.Width() != 80 || m.Height() != 24 {
		t.Errorf("size = %dx%d, want 80x24", m.Width(), m.Height())
	}

}

func TestAppSwitchToHomePreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.home.CloudAccess = nil

	cmd := a.switchToHome()
	if cmd == nil {
		t.Fatal("switchToHome returned nil cmd")
	}
	if a.home.CloudAccess != cloud {
		t.Fatal("cloud access client was not preserved after switching home")
	}
}

func TestAppSwitchViewHomePreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.home.CloudAccess = nil

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewHome})
	got := model.(App)

	if got.home.CloudAccess != cloud {
		t.Fatal("cloud access client was not preserved after direct home navigation")
	}
}

func TestAppSettingsBillingUsesAccountReturnURL(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{portalErr: errors.New("billing portal unavailable")}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.WithCheckoutURLs(
		"https://app.example.test/subscribe/success?source=cli",
		"https://app.example.test/subscribe/canceled?source=cli",
	)

	model, _ := a.Update(ui.SwitchViewMsg{View: ui.ViewSettings})
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

}

func TestAppWithSettingsPassesSettingsToHome(t *testing.T) {
	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true

	a := NewApp(nil, nil)
	a.WithSettings(settings)

	if !a.home.Settings.BossCloudGuestOfferHidden {
		t.Fatal("home.Settings.BossCloudGuestOfferHidden = false, want true")
	}
}

func TestAppSettingsNotificationToggleAppliesToHomeImmediately(t *testing.T) {
	withTempConfigHome(t)

	a := NewApp(nil, nil)
	a.activeView = ui.ViewGeneralSettings
	a.generalSettings = settings.NewGeneralSettingsModel(nil, a.ctx, nil)

	selectGeneralSettingsRow(t, &a.generalSettings, "Enable desktop notifications for questions")
	if !config.NotificationsEnabled(a.home.Settings) {
		t.Fatal("precondition: home notifications should default to enabled")
	}

	model, _ := a.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	got := model.(App)
	if config.NotificationsEnabled(got.userSettings) {
		t.Fatal("settings toggle did not update App.userSettings")
	}
	if config.NotificationsEnabled(got.home.Settings) {
		t.Fatal("settings toggle did not update HomeModel immediately")
	}
}

func TestAppSettingsSuccessfulOtherSaveAfterNotificationFailureKeepsSavedNotificationValue(t *testing.T) {
	withTempConfigHome(t)

	a := NewApp(nil, nil)
	a.activeView = ui.ViewGeneralSettings
	a.generalSettings = settings.NewGeneralSettingsModel(nil, a.ctx, nil)
	selectGeneralSettingsRow(t, &a.generalSettings, "Enable desktop notifications for questions")

	badPath := filepath.Join(t.TempDir(), "settings-dir")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", badPath, err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", badPath)
	model, _ := a.Update(tea.KeyPressMsg{Code: ' ', Text: " "})
	a = model.(App)
	if a.generalSettings.Err() == nil {
		t.Fatal("failed notification save did not retain an error")
	}

	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	selectGeneralSettingsRow(t, &a.generalSettings, "Enable error tracking")
	model, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(App)
	if got.generalSettings.Err() != nil {
		t.Fatalf("successful other save retained stale error: %v", got.generalSettings.Err())
	}
	if !config.NotificationsEnabled(got.userSettings) {
		t.Fatal("successful other save persisted the rolled-back App.userSettings notification setting")
	}
	if !config.NotificationsEnabled(got.home.Settings) {
		t.Fatal("successful other save persisted the rolled-back HomeModel notification setting")
	}
	if !got.home.Settings.ErrorTrackingEnabled {
		t.Fatal("successful other save did not update HomeModel changed setting")
	}
}

func TestAppDiscardsSessionPollFromReplacedHome(t *testing.T) {
	a := NewApp(nil, nil)
	oldGeneration := a.home.Generation
	oldPollID := a.home.NextSessionPollID

	a.switchToHome()
	if a.home.Generation == oldGeneration {
		t.Fatal("replacement HomeModel reused its generation")
	}

	model, _ := a.Update(sessionListMsg{
		HomeGeneration: oldGeneration,
		PollID:         oldPollID,
		Sessions:       []*pb.Session{{Id: "old-session", HasActiveChat: true}},
	})
	got := model.(App)
	if !got.home.Loading {
		t.Fatal("stale poll cleared replacement HomeModel loading state")
	}
	if len(got.home.Sessions) != 0 {
		t.Fatalf("stale poll populated replacement HomeModel sessions: %+v", got.home.Sessions)
	}
	if got.rotationSeen != nil {
		t.Fatalf("stale poll mutated App rotation state: %+v", got.rotationSeen)
	}
}

func TestAppPreservesLogoutStateAcrossHomeRebuild(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.LoggedIn = true
	a.home.LoggedInEmail = "dev@example.com"
	a.home.LogoutPending = true
	a.home.AuthStatusGeneration = 1

	a.switchToHome()

	if !a.home.LogoutPending {
		t.Fatal("rebuilding Home dropped a pending logout")
	}
	if got := a.home.AuthStatusGeneration; got != 1 {
		t.Fatalf("auth status generation = %d, want 1", got)
	}
	if !a.home.LoggedIn || a.home.LoggedInEmail != "dev@example.com" {
		t.Fatalf("rebuilding Home dropped the signed-in logout snapshot: loggedIn=%t email=%q", a.home.LoggedIn, a.home.LoggedInEmail)
	}

	updated, _ := a.home.Update(authStatusMsg{Generation: 0, LoggedIn: true, Email: "stale@example.com"})
	a.home = updated.(HomeModel)
	if !a.home.LoggedIn || a.home.LoggedInEmail != "dev@example.com" {
		t.Fatalf("stale auth status changed the preserved signed-in state: loggedIn=%t email=%q", a.home.LoggedIn, a.home.LoggedInEmail)
	}
}

func TestAppDeliversQueuedLoginNotificationAfterLoginIsDismissed(t *testing.T) {
	logoutStarted := make(chan struct{})
	releaseLogout := make(chan struct{})
	received := make(chan string, 2)
	c := &stubClient{notifyAuthChange: func(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if action == "logout" {
			close(logoutStarted)
			<-releaseLogout
		}
		received <- action
		return nil, nil
	}}
	a := NewApp(c, auth.NewManager(&countingLogoutTokenStore{}, auth.Config{}))
	a.home.Sessions = []*pb.Session{}
	a.home.RepoCount = 1
	a.home.LoggedIn = true
	a.home.LoggedInEmail = "dev@example.com"

	updated, _ := a.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	a = updated.(App)
	updated, logoutCmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = updated.(App)
	if logoutCmd == nil {
		t.Fatal("logout confirmation returned no command")
	}
	logoutResult := logoutCmd()

	updated, logoutNotifyCmd := a.Update(logoutResult)
	a = updated.(App)
	if logoutNotifyCmd == nil {
		t.Fatal("successful logout returned no notification command")
	}
	go logoutNotifyCmd()
	select {
	case <-logoutStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("logout notification did not start")
	}

	updated, _ = a.Update(ui.SwitchViewMsg{View: ui.ViewLogin})
	a = updated.(App)
	updated, loginCmd := a.Update(login.CompleteMsg{
		Email:        "new@example.com",
		Verification: auth.LoginVerification{Outcome: auth.LoginVerified, Email: "new@example.com"},
	})
	a = updated.(App)
	loginMsg := loginCmd()
	batch, ok := loginMsg.(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("login success command = %T, want non-empty tea.BatchMsg", loginMsg)
	}
	updated, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	a = updated.(App)
	if a.activeView != ui.ViewHome {
		t.Fatalf("activeView = %v, want %v after dismissing login", a.activeView, ui.ViewHome)
	}
	go batch[0]()

	select {
	case action := <-received:
		t.Fatalf("%s notification reached the daemon before the earlier stalled logout", action)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseLogout)
	for _, want := range []string{"logout", "login"} {
		select {
		case got := <-received:
			if got != want {
				t.Fatalf("notification = %q, want %q", got, want)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("did not receive %s notification", want)
		}
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
	if !home.StartedAt.Equal(epoch) {
		t.Fatalf("newHomeModel startedAt = %v, want %v (session epoch must survive Home recreation)", home.StartedAt, epoch)
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
	a.home.RepoCount = 1
	a.home.Loading = false
	a.home.LoggedIn = false
	a.home.Sessions = []*pb.Session{{Id: "sess-1", Title: "Active work"}}
	a.home.BuildTableRows()
	a.home.Now = func() time.Time { return epoch.Add(ui.CloudGuestOfferSessionLimit + time.Second) }

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
	prevSave := home.SaveSettings
	home.SaveSettings = func(config.Settings) error { return nil }
	defer func() { home.SaveSettings = prevSave }()

	a := NewApp(nil, &auth.Manager{})
	a.WithSettings(config.DefaultSettings())
	a.startedAt = epoch
	a.activeView = ui.ViewHome
	a.home.Now = func() time.Time { return epoch }
	a.home.RepoCount = 1
	a.home.Loading = false

	// A session with an active chat delivers value (repo + session + chat).
	model, _ := a.Update(sessionListMsg{Sessions: []*pb.Session{{Id: "s1", HasActiveChat: true}}})
	a = model.(App)

	if a.userSettings.BossCloudValueDeliveredAt.IsZero() {
		t.Fatal("latch must propagate to App.userSettings so home recreations keep it")
	}
	latched := a.userSettings.BossCloudValueDeliveredAt

	// A return-to-home recreates the home model; it must re-seed the latch.
	home := a.newHomeModel()
	if !home.Settings.BossCloudValueDeliveredAt.Equal(latched) {
		t.Fatalf("recreated home lost the latch: got %v want %v", home.Settings.BossCloudValueDeliveredAt, latched)
	}

	// A later poll with no active chat must NOT move or clear the timestamp.
	home.Now = func() time.Time { return epoch.Add(time.Hour) }
	home.RepoCount = 1
	m2, _ := home.Update(sessionListMsg{Sessions: []*pb.Session{{Id: "s1", HasActiveChat: false}}})
	home = m2.(HomeModel)
	if !home.Settings.BossCloudValueDeliveredAt.Equal(latched) {
		t.Fatalf("latch moved or reverted after recreation: got %v want %v", home.Settings.BossCloudValueDeliveredAt, latched)
	}
}

func TestAppChatPickerReturnPreservesCloudAccessClient(t *testing.T) {
	cloud := &fakeHomeCloudAccessClient{}
	a := NewApp(nil, nil)
	a.WithCloudAccessClient(cloud)
	a.activeView = ui.ViewChatPicker
	a.chatPicker = pickerSessionFixture("session-1", &pb.Session{Id: "session-1"})
	a.chatPicker, _ = updateModel(a.chatPicker, tea.KeyPressMsg{Code: tea.KeyEsc})

	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	got := model.(App)

	if got.home.CloudAccess != cloud {
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
	a.activeView = ui.ViewHome
	a.home.Loading = false
	a.home.LoggedIn = true
	a.home.RepoCount = 1
	a.home.CloudStatus = cloud.status

	openedURL := ""

	model, cmd := a.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	got := model.(App)
	if cmd == nil {
		t.Fatal("subscribe key returned nil cmd")
	}

	msg := cmd()
	model, cmd = got.Update(msg)
	got = model.(App)
	if got.activeView != ui.ViewLogin {
		t.Fatalf("activeView = %v, want %v", got.activeView, ui.ViewLogin)
	}
	got.login.SetSubscriptionBrowserOpener(func(rawURL string) error {
		openedURL = rawURL
		return nil
	})
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
	if !got.login.SubscriptionWaiting() {
		t.Fatal("subscription phase is not waiting")
	}
	if !strings.Contains(got.View().Content, "Loading your account…") {
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
	a.activeView = ui.ViewRepoAdd
	a.repoAddCompleting = true
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(ui.RepoAddCompletedMsg{Repos: repos, HighlightID: "r2"})
	got := model.(App)

	if got.repoAddCompleting {
		t.Fatal("repoAddCompleting should be reset to false")
	}
	if got.activeView != ui.ViewRepoList {
		t.Fatalf("activeView = %v, want ViewRepoList", got.activeView)
	}
	if got.repoList.HighlightRepoID() != "r2" {
		t.Fatalf("highlightRepoID = %q, want %q", got.repoList.HighlightRepoID(), "r2")
	}
}

// The first-ever repo (len(repos) <= 1, not opened from Settings) returns to
// Home rather than diverting to a settings screen.
func TestRepoAddCompletedFirstRepoGoesHome(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}}

	model, _ := a.Update(ui.RepoAddCompletedMsg{Repos: repos, HighlightID: "r1"})
	got := model.(App)

	if got.activeView != ui.ViewHome {
		t.Fatalf("first repo add should return Home, got %v", got.activeView)
	}
}

// A failed add preserves the prior fallback (repo list), without diverting to
// a settings screen for a repo that may not exist.
func TestRepoAddCompletedErrorFallsBackToList(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(ui.RepoAddCompletedMsg{Repos: repos, Err: errors.New("boom"), HighlightID: ""})
	got := model.(App)

	if got.activeView != ui.ViewRepoList {
		t.Fatalf("on error want ViewRepoList, got %v", got.activeView)
	}
}

// Defensive: success but no usable repo id falls back to the prior behavior
// rather than opening settings for a missing repo.
func TestRepoAddCompletedEmptyIDFallsBack(t *testing.T) {
	a := NewApp(nil, nil)
	a.activeView = ui.ViewRepoAdd
	repos := []*pb.Repo{{Id: "r1"}, {Id: "r2"}}

	model, _ := a.Update(ui.RepoAddCompletedMsg{Repos: repos, HighlightID: ""})
	got := model.(App)

	if got.activeView != ui.ViewRepoList {
		t.Fatalf("empty id should fall back to list, got %v", got.activeView)
	}
}

// --- BOS-506: the rotation toast must be budgeted out of the frame ---

// sizedToastApp builds an App on view v, sized to width x height through the
// real WindowSizeMsg path, populated with n sessions/chats whose titles carry
// the "zq" marker so rendered data rows can be counted.
func sizedToastApp(t *testing.T, v ui.View, n, width, height int) App {
	t.Helper()
	a := NewApp(nil, nil)
	a.activeView = v
	sessions := make([]*pb.Session, 0, n)
	chats := make([]*pb.AgentChat, 0, n)
	for i := range n {
		id := fmt.Sprintf("zq%02d", i)
		sessions = append(sessions, &pb.Session{Id: id, Title: id})
		chats = append(chats, &pb.AgentChat{SessionId: "zqsess", AgentSessionId: id, Title: id})
	}
	a.home.Sessions = sessions
	a.home.Loading = false
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.ChatsListedMsg{Chats: chats})
	a.chatPicker, _ = updateModel(a.chatPicker, chatpicker.SessionMsg{Session: &pb.Session{Id: "zqsess", Title: "zqsess"}})
	model, _ := a.Update(tea.WindowSizeMsg{Width: width, Height: height})
	sized := model.(App)
	// Views build their table rows (and cache the table height) in Update, so
	// prime both the way a data poll would before rendering.
	sized.home.BuildTableRows()
	sized.chatPicker.SyncTableHeight()
	return sized
}

// countMarkerRows reports how many rendered lines carry a data-row marker.
func countMarkerRows(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "zq") {
			n++
		}
	}
	return n
}

// A visible toast must not push the action bar off the bottom of the frame:
// the rendered frame stays within the terminal height either way (BOS-506).
func TestAppViewFitsTerminalHeightWithToast(t *testing.T) {
	const (
		width  = 80
		height = 24
	)
	for _, tc := range []struct {
		name string
		view ui.View
	}{
		{"home", ui.ViewHome},
		{"chatpicker", ui.ViewChatPicker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := sizedToastApp(t, tc.view, 30, width, height)

			plain := lipgloss.Height(a.View().Content)
			if plain > height {
				t.Fatalf("frame without toast = %d lines, want <= %d", plain, height)
			}

			a.toast, _ = a.toast.Show("session: rotated to backup account")
			withToast := lipgloss.Height(a.View().Content)
			if withToast > height {
				t.Errorf("frame with toast = %d lines, want <= %d (toast overflows the frame and clips the action bar)", withToast, height)
			}
		})
	}
}

// The active view's table gives up exactly the toast's lines while it is
// visible and regains them once the toast expires through the real path.
func TestAppViewToastShrinksTableAndRestoresOnExpiry(t *testing.T) {
	const (
		width  = 80
		height = 24
	)
	a := sizedToastApp(t, ui.ViewHome, 30, width, height)

	before := countMarkerRows(a.View().Content)
	if before < 5 {
		t.Fatalf("expected the home table to be height-capped with rows to spare, got %d rendered rows", before)
	}

	a.toast, _ = a.toast.Show("session: rotated to backup account")
	reserved := a.toast.Height(width)
	if reserved <= 0 {
		t.Fatalf("visible toast reported height %d, want > 0", reserved)
	}

	during := countMarkerRows(a.View().Content)
	if want := before - reserved; during != want {
		t.Errorf("rendered rows with toast = %d, want %d (before %d - toast %d)", during, want, before, reserved)
	}

	model, _ := a.Update(ui.ToastExpireMsg{ID: a.toast.ID})
	expired := model.(App)
	if after := countMarkerRows(expired.View().Content); after != before {
		t.Errorf("rendered rows after toast expiry = %d, want %d", after, before)
	}
}

// heightOwningSubModels reflects over App and returns the name of every
// sub-model field that owns an unexported `height int`, along with its current
// value. This is the durable form of "keep reserveHeight in step with new
// views": a new height-owning view shows up here automatically.
//
// Pointer sub-models are followed too. A view added as *FooModel would
// otherwise be invisible here, and the guard would pass vacuously while that
// view silently kept the pre-BOS-506 overflow — the exact regression this test
// exists to catch.
func heightOwningSubModels(t *testing.T, a *App) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	av := reflect.ValueOf(a).Elem()
	at := av.Type()
	for i := range at.NumField() {
		fv := av.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				continue
			}
			fv = fv.Elem()
		}
		if fv.Kind() != reflect.Struct {
			continue
		}
		hv := fv.FieldByName("height")
		if !hv.IsValid() || hv.Kind() != reflect.Int {
			continue
		}
		out[at.Field(i).Name] = hv.Int()
	}
	return out
}

// Every height-owning sub-model is shrunk by reserveHeight — discovered by
// reflection so a newly added view fails here instead of silently reverting to
// the pre-BOS-506 overflow.
func TestReserveHeightCoversEveryHeightOwningSubModel(t *testing.T) {
	a := NewApp(nil, nil)
	model, _ := a.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	sized := model.(App)
	// WindowSizeMsg does not reach every sub-model (attach sizes itself from its
	// own Update), so seed the rest explicitly; a field left at 0 below means the
	// helper's list and this test are both missing a newly added view.
	sized.attach.SetHeight(40)

	found := heightOwningSubModels(t, &sized)
	if len(found) == 0 {
		t.Fatal("reflection found no height-owning sub-models on App")
	}
	for name, h := range found {
		if h != 40 {
			t.Fatalf("sub-model %s owns a height but the test did not seed it (got %d); add it to reserveHeight, to applyReservedTableHeights if it caches a table height in Update, and to this test", name, h)
		}
	}

	sized.reserveHeight(3)
	for name, h := range heightOwningSubModels(t, &sized) {
		if h != 37 {
			t.Errorf("after reserveHeight(3), %s.height = %d, want 37", name, h)
		}
	}
}

// Shrinking a table for a toast must keep the selected title and caret visible.
func TestAppViewToastKeepsTheSelectedRowOnScreen(t *testing.T) {
	const rows = 60

	sessions := make([]*pb.Session, rows)
	for i := range rows {
		id := fmt.Sprintf("zq%02d", i)
		sessions[i] = &pb.Session{Id: id, Title: id}
	}

	for _, height := range []int{24, 30, 40} {
		t.Run(fmt.Sprintf("height%d", height), func(t *testing.T) {
			checked := 0
			for cur := range 45 {
				a := NewApp(nil, nil)
				a.activeView = ui.ViewHome
				a.home.Sessions = sessions
				a.home.Loading = false
				model, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: height})
				sized := model.(App)
				sized.home.BuildTableRows()
				// Park the cursor exactly as a key handler would.
				sized.home.Table.SetCursor(cur)

				want := sessions[cur].Title
				if !strings.Contains(sized.View().Content, want) {
					continue // already off-screen without a toast; nothing to preserve
				}
				checked++

				sized.toast, _ = sized.toast.Show("session: rotated to backup account")
				got := sized.View().Content
				// The selected title and the table-owned caret must both stay visible.
				selected := ""
				for _, line := range strings.Split(got, "\n") {
					if strings.Contains(line, want) {
						selected = line
						break
					}
				}
				if selected == "" {
					t.Errorf("cursor %d: selected row %q left the frame when the toast appeared", cur, want)
					continue
				}
				if !strings.Contains(selected, ui.CursorChevron) {
					t.Errorf("cursor %d: the %q caret is not on the selected row's line %q", cur, ui.CursorChevron, selected)
				}
			}
			if checked < 10 {
				t.Fatalf("only %d cursor positions reached the assertions; the fixture no longer exercises the shrink and this guard has gone vacuous", checked)
			}
		})
	}
}

// Reserving height changes the viewport without changing the selection,
// including on an empty table that later receives rows.
func TestSetReservedTableHeightPreservesTheCursor(t *testing.T) {
	cols := []table.Column{ui.CursorColumn, {Title: "NAME", Width: 20}}
	rows := make([]table.Row, 60)
	for i := range rows {
		rows[i] = table.Row{"", fmt.Sprintf("row-%02d", i)}
	}

	tbl := ui.NewBossTable(cols, rows, 16)
	tbl.SetCursor(13)
	setReservedTableHeight(&tbl, 14)
	if got := tbl.Cursor(); got != 13 {
		t.Errorf("cursor moved to %d while reserving the toast's height, want 13", got)
	}

	empty := ui.NewBossTable(cols, nil, 16)
	setReservedTableHeight(&empty, 14)
	if got := empty.Cursor(); got != 0 {
		t.Errorf("empty table cursor = %d after reserving, want 0 (a negative cursor survives SetRows)", got)
	}
}

// The switch-to-NewSession path builds a FRESH NewSessionModel, so it must seed
// the height itself — the WindowSizeMsg seed is discarded with the old model.
// This is the seed that matters in production: without it newSession's three
// tables take clampedTableHeight's uncapped branch and the toast reservation is
// dead on that view (BOS-506).
func TestSwitchToNewSessionSeedsHeight(t *testing.T) {
	const termRows = 40

	a := NewApp(nil, nil)
	model, _ := a.Update(tea.WindowSizeMsg{Width: 120, Height: termRows})
	sized := model.(App)

	switched, _ := sized.Update(ui.SwitchViewMsg{View: ui.ViewNewSession})
	got := switched.(App)
	if got.newSession.Height() != termRows {
		t.Errorf("newSession.Height() = %d after switching to New Session, want %d", got.newSession.Height(), termRows)
	}
	if got.newSession.Width() != 120 {
		t.Errorf("newSession.Width() = %d after switching to New Session, want 120", got.newSession.Width())
	}
}

// reserveHeight on its own is inert: list views cache their table height by
// calling table.SetHeight in Update, never in View, so shrinking the sub-model
// height alone never reaches the frame being rendered.
// applyReservedTableHeights is what closes that gap — and it must close it for
// EVERY height-derived table, not just Home's. A view wired into reserveHeight
// but missed there silently keeps the pre-BOS-506 clipping, which no other test
// would catch, so pin all nine tables here (BOS-506).
func TestApplyReservedTableHeightsCoversEveryHeightDerivedTable(t *testing.T) {
	const (
		reserve  = 3
		termRows = 40
		// More rows than the terminal can show, so every table lands on
		// clampedTableHeight's capped branch and therefore tracks the reserved
		// height. An uncapped table would be unaffected and prove nothing.
		rows = 60
	)

	a := NewApp(nil, nil)
	model, _ := a.Update(tea.WindowSizeMsg{Width: 120, Height: termRows})
	sized := model.(App)
	if sized.newSession.Height() != termRows {
		t.Fatalf("newSession.Height() = %d after WindowSizeMsg, want %d; the App-level seed is missing, so its three tables silently take clampedTableHeight's uncapped branch", sized.newSession.Height(), termRows)
	}

	sessions := make([]*pb.Session, rows)
	chats := make([]*pb.AgentChat, rows)
	repos := make([]*pb.Repo, rows)
	jobs := make([]*pb.CronJob, rows)
	accounts := make([]*pb.AgentAccount, rows)
	for i := range rows {
		id := fmt.Sprintf("zq%02d", i)
		sessions[i] = &pb.Session{Id: id, Title: id}
		chats[i] = &pb.AgentChat{AgentSessionId: id, Title: id}
		repos[i] = &pb.Repo{Id: id, DisplayName: id}
		jobs[i] = &pb.CronJob{Id: id, Name: id}
		accounts[i] = &pb.AgentAccount{Id: id, Label: id}
	}
	sized.home.Sessions = sessions
	sized.chatPicker, _ = updateModel(sized.chatPicker, chatpicker.ChatsListedMsg{Chats: chats})
	sized.newSession, _ = updateModel(sized.newSession, newsession.ReposMsg{Repos: repos})
	prs := make([]*pb.PRSummary, rows)
	issues := make([]*pb.TrackerIssue, rows)
	for i := range rows {
		prs[i] = &pb.PRSummary{Number: int32(i + 1)}
		issues[i] = &pb.TrackerIssue{ExternalId: fmt.Sprint(i)}
	}
	sized.newSession, _ = updateModel(sized.newSession, newsession.PrsMsg{PRs: prs})
	sized.newSession, _ = updateModel(sized.newSession, newsession.IssuesMsg{Issues: issues})
	v, _ := sized.repoList.Update(repospkg.RepoListLoadedMsg{Repos: repos})
	sized.repoList = v.(repospkg.RepoListModel)
	sized.trash, _ = updateModel(sized.trash, trashListMsg{Sessions: sessions})
	sized.cronList, _ = updateModel(sized.cronList, cronJobsLoadedMsg{Jobs: jobs})
	sized.accountsList, _ = updateModel(sized.accountsList, accountscreen.AgentAccountsLoadedMsg{Accounts: accounts})

	// Stand in for the table.SetHeight each view performs in Update: cache every
	// table height against the un-reserved terminal height first, so the deltas
	// below are attributable to the reservation alone.
	sized.applyReservedTableHeights()
	tables := func(a *App) map[string]int {
		return map[string]int{
			"home":                 a.home.Table.Height(),
			"chatPicker":           a.chatPicker.Table().Height(),
			"newSession.repoTable": a.newSession.RepoTable().Height(),
			"newSession.prTable":   a.newSession.PRTable().Height(),
			"newSession.issueTab":  a.newSession.IssueTable().Height(),
			"repoList":             a.repoList.Table().Height(),
			"trash":                a.trash.Table().Height(),
			"cronList":             a.cronList.Table().Height(),
			"accountsList":         a.accountsList.Table().Height(),
		}
	}
	before := tables(&sized)
	for name, h := range before {
		if h <= reserve {
			t.Fatalf("%s table height = %d after priming; either it is missing from applyReservedTableHeights or the fixture no longer leaves it height-capped with rows to spare", name, h)
		}
	}

	sized.reserveHeight(reserve)
	for name, h := range tables(&sized) {
		if want := before[name] - reserve; h != want {
			t.Errorf("after reserveHeight(%d), %s table height = %d, want %d (missing from applyReservedTableHeights?)", reserve, name, h, want)
		}
	}
}

// A sub-model that has not seen a WindowSizeMsg yet keeps height 0, which
// clampedTableHeight reads as "terminal height unknown" (uncapped). Reserving
// must not force it onto a bogus small value.
func TestReserveHeightLeavesUnsizedSubModelsAlone(t *testing.T) {
	a := NewApp(nil, nil)
	a.reserveHeight(2)
	for name, h := range heightOwningSubModels(t, &a) {
		if h != 0 {
			t.Errorf("unsized sub-model %s.height = %d after reserveHeight, want 0", name, h)
		}
	}
}

// On a very short terminal the reservation clamps at 1, never 0: a 0 would flip
// clampedTableHeight onto its uncapped path and grow the table instead.
func TestReserveHeightClampsAtOne(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Height = 2
	a.chatPicker.SetHeight(1)
	a.reserveHeight(5)

	if a.home.Height != 1 {
		t.Errorf("home.Height = %d after over-reserving, want 1", a.home.Height)
	}
	if a.chatPicker.Height() != 1 {
		t.Errorf("chatPicker.height = %d after over-reserving, want 1", a.chatPicker.Height())
	}
}

func selectGeneralSettingsRow(t *testing.T, model *settings.GeneralSettingsModel, label string) {
	t.Helper()
	for range 100 {
		for _, line := range strings.Split(model.View().Content, "\n") {
			if strings.Contains(line, "┃") && strings.Contains(line, label) {
				return
			}
		}
		updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		*model = updated.(settings.GeneralSettingsModel)
	}
	t.Fatalf("settings row %q not found in %s", label, model.View().Content)
}

func TestAppReentersSessionSetup(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.Sessions = []*pb.Session{{Id: "session-setup", Title: "Preparing workspace"}}
	a.width, a.height = 120, 35
	updated, cmd := a.Update(ui.SwitchViewMsg{View: ui.ViewNewSession, SessionID: "session-setup"})
	got := updated.(App)
	if cmd == nil || got.activeView != ui.ViewNewSession {
		t.Fatal("setup not entered")
	}
	for _, text := range []string{"Preparing workspace", "Session session-setup", "Running setup script…"} {
		if !strings.Contains(got.newSession.View().Content, text) {
			t.Fatalf("missing %q: %s", text, got.newSession.View().Content)
		}
	}
	if got.newSession.Width() != 120 || got.newSession.Height() != 35 {
		t.Fatal("setup size not seeded")
	}
}
