package views

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

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	// The picker is rebuilt from four places; a site that skipped the agent
	// wiring would reintroduce the stuck default on that path alone.
	if got.chatPicker.onAgentSelected == nil {
		t.Fatal("chat picker agent-selection handler was not wired after attach detach")
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
	a := App{ctx: context.Background(), width: 80, height: 24}

	m := a.newChatPickerModel("session-1", "")

	if m.onAgentSelected == nil {
		t.Error("onAgentSelected = nil; confirmed picks would not persist")
	}
	if m.preferredAgent != "codex" {
		t.Errorf("preferredAgent = %q, want %q from settings", m.preferredAgent, "codex")
	}
	if m.width != 80 || m.height != 24 {
		t.Errorf("size = %dx%d, want 80x24", m.width, m.height)
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
	a.activeView = ViewGeneralSettings
	a.generalSettings = NewGeneralSettingsModel(nil, a.ctx)

	for i, row := range a.generalSettings.rows {
		if row.Kind == settingsRowKindNotifications {
			a.generalSettings.cursor = i
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

func TestAppSettingsSuccessfulOtherSaveAfterNotificationFailureKeepsSavedNotificationValue(t *testing.T) {
	withTempConfigHome(t)

	a := NewApp(nil, nil)
	a.activeView = ViewGeneralSettings
	a.generalSettings = NewGeneralSettingsModel(nil, a.ctx)
	for i, row := range a.generalSettings.rows {
		if row.Kind == settingsRowKindNotifications {
			a.generalSettings.cursor = i
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
	if a.generalSettings.err == nil {
		t.Fatal("failed notification save did not retain an error")
	}

	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	for i, row := range a.generalSettings.rows {
		if row.Kind == settingsRowKindErrorTracking {
			a.generalSettings.cursor = i
			break
		}
	}
	model, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := model.(App)
	if got.generalSettings.err != nil {
		t.Fatalf("successful other save retained stale error: %v", got.generalSettings.err)
	}
	if !config.NotificationsEnabled(got.userSettings) {
		t.Fatal("successful other save persisted the rolled-back App.userSettings notification setting")
	}
	if !config.NotificationsEnabled(got.home.settings) {
		t.Fatal("successful other save persisted the rolled-back HomeModel notification setting")
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

func TestAppPreservesLogoutStateAcrossHomeRebuild(t *testing.T) {
	a := NewApp(nil, nil)
	a.home.loggedIn = true
	a.home.loggedInEmail = "dev@example.com"
	a.home.logoutPending = true
	a.home.authStatusGeneration = 1

	a.switchToHome()

	if !a.home.logoutPending {
		t.Fatal("rebuilding Home dropped a pending logout")
	}
	if got := a.home.authStatusGeneration; got != 1 {
		t.Fatalf("auth status generation = %d, want 1", got)
	}
	if !a.home.loggedIn || a.home.loggedInEmail != "dev@example.com" {
		t.Fatalf("rebuilding Home dropped the signed-in logout snapshot: loggedIn=%t email=%q", a.home.loggedIn, a.home.loggedInEmail)
	}

	updated, _ := a.home.Update(authStatusMsg{generation: 0, loggedIn: true, email: "stale@example.com"})
	a.home = updated.(HomeModel)
	if !a.home.loggedIn || a.home.loggedInEmail != "dev@example.com" {
		t.Fatalf("stale auth status changed the preserved signed-in state: loggedIn=%t email=%q", a.home.loggedIn, a.home.loggedInEmail)
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
	a.home.sessions = []*pb.Session{}
	a.home.repoCount = 1
	a.home.loggedIn = true
	a.home.loggedInEmail = "dev@example.com"

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

	updated, _ = a.Update(switchViewMsg{view: ViewLogin})
	a = updated.(App)
	updated, loginCmd := a.Update(loginCompleteMsg{
		email:        "new@example.com",
		verification: auth.LoginVerification{Outcome: auth.LoginVerified, Email: "new@example.com"},
	})
	a = updated.(App)
	loginMsg := loginCmd()
	batch, ok := loginMsg.(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("login success command = %T, want non-empty tea.BatchMsg", loginMsg)
	}
	updated, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	a = updated.(App)
	if a.activeView != ViewHome {
		t.Fatalf("activeView = %v, want %v after dismissing login", a.activeView, ViewHome)
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

// --- BOS-506: the rotation toast must be budgeted out of the frame ---

// sizedToastApp builds an App on view v, sized to width x height through the
// real WindowSizeMsg path, populated with n sessions/chats whose titles carry
// the "zq" marker so rendered data rows can be counted.
func sizedToastApp(t *testing.T, v View, n, width, height int) App {
	t.Helper()
	a := NewApp(nil, nil)
	a.activeView = v
	sessions := make([]*pb.Session, 0, n)
	chats := make([]*pb.ClaudeChat, 0, n)
	for i := range n {
		id := fmt.Sprintf("zq%02d", i)
		sessions = append(sessions, &pb.Session{Id: id, Title: id})
		chats = append(chats, &pb.ClaudeChat{SessionId: "zqsess", AgentSessionId: id, Title: id})
	}
	a.home.sessions = sessions
	a.home.loading = false
	a.chatPicker.chats = chats
	a.chatPicker.loading = false
	a.chatPicker.session = &pb.Session{Id: "zqsess", Title: "zqsess"}
	model, _ := a.Update(tea.WindowSizeMsg{Width: width, Height: height})
	sized := model.(App)
	// Views build their table rows (and cache the table height) in Update, so
	// prime both the way a data poll would before rendering.
	sized.home.buildTableRows()
	sized.chatPicker.buildTableRows()
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
		view View
	}{
		{"home", ViewHome},
		{"chatpicker", ViewChatPicker},
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
	a := sizedToastApp(t, ViewHome, 30, width, height)

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

	model, _ := a.Update(toastExpireMsg{id: a.toast.id})
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
	sized.attach.height = 40

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

// Shrinking a table must not hide the row it has selected. bubbles' SetHeight
// recomputes the rendered row window from the cursor but leaves viewport.YOffset
// alone, so without the re-anchor in setReservedTableHeight a cursor resting in
// the last rows of the unscrolled first page falls outside the shortened window:
// the selected title and the ❯ caret both disappear for the toast's six seconds
// while Enter still acts on the invisible selection (BOS-506).
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
				a.activeView = ViewHome
				a.home.sessions = sessions
				a.home.loading = false
				model, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: height})
				sized := model.(App)
				sized.home.buildTableRows()
				// Park the cursor exactly as a key handler would.
				sized.home.table.SetCursor(cur)
				updateCursorColumn(&sized.home.table)

				want := sessions[cur].Title
				if !strings.Contains(sized.View().Content, want) {
					continue // already off-screen without a toast; nothing to preserve
				}
				checked++

				sized.toast, _ = sized.toast.Show("session: rotated to backup account")
				got := sized.View().Content
				// Locate the selected row's rendered line and check the caret on
				// it. This is a visibility check with a precise failure message,
				// NOT a cursor-preservation check: updateCursorColumn writes the
				// chevron into the row data at cur, so caret and title are two
				// cells of one row and the second check cannot fail on its own.
				// That the re-anchor leaves the cursor alone is pinned by
				// TestSetReservedTableHeightPreservesTheCursor.
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
				if !strings.Contains(selected, cursorChevron) {
					t.Errorf("cursor %d: the %q caret is not on the selected row's line %q", cur, cursorChevron, selected)
				}
			}
			if checked < 10 {
				t.Fatalf("only %d cursor positions reached the assertions; the fixture no longer exercises the shrink and this guard has gone vacuous", checked)
			}
		})
	}
}

// setReservedTableHeight re-anchors the viewport, and that is ALL it may do to
// the selection: the row bubbles highlights must stay the row updateCursorColumn
// put the ❯ caret on, or the two disagree on screen. This is the only test that
// catches a re-anchor which moves the cursor — the integration test above cannot,
// because the caret travels in the row data rather than with the cursor.
//
// The empty-table case is separate: bubbles' clamp is min(max(v, low), high)
// with no low > high case,
// so MoveDown on a zero-row table yields clamp(0, 0, -1) = -1, and a later
// SetRows only ever clamps a cursor down, so that -1 would stick (BOS-506).
func TestSetReservedTableHeightPreservesTheCursor(t *testing.T) {
	cols := []table.Column{cursorColumn, {Title: "NAME", Width: 20}}
	rows := make([]table.Row, 60)
	for i := range rows {
		rows[i] = table.Row{"", fmt.Sprintf("row-%02d", i)}
	}

	tbl := newBossTable(cols, rows, 16)
	tbl.SetCursor(13)
	setReservedTableHeight(&tbl, 14)
	if got := tbl.Cursor(); got != 13 {
		t.Errorf("cursor moved to %d while reserving the toast's height, want 13", got)
	}

	empty := newBossTable(cols, nil, 16)
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

	switched, _ := sized.Update(switchViewMsg{view: ViewNewSession})
	got := switched.(App)
	if got.newSession.height != termRows {
		t.Errorf("newSession.height = %d after switching to New Session, want %d", got.newSession.height, termRows)
	}
	if got.newSession.width != 120 {
		t.Errorf("newSession.width = %d after switching to New Session, want 120", got.newSession.width)
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
	if sized.newSession.height != termRows {
		t.Fatalf("newSession.height = %d after WindowSizeMsg, want %d; the App-level seed is missing, so its three tables silently take clampedTableHeight's uncapped branch", sized.newSession.height, termRows)
	}

	sessions := make([]*pb.Session, rows)
	chats := make([]*pb.ClaudeChat, rows)
	repos := make([]*pb.Repo, rows)
	jobs := make([]*pb.CronJob, rows)
	accounts := make([]*pb.Account, rows)
	idxs := make([]int, rows)
	for i := range rows {
		id := fmt.Sprintf("zq%02d", i)
		sessions[i] = &pb.Session{Id: id, Title: id}
		chats[i] = &pb.ClaudeChat{AgentSessionId: id, Title: id}
		repos[i] = &pb.Repo{Id: id, DisplayName: id}
		jobs[i] = &pb.CronJob{Id: id, Name: id}
		accounts[i] = &pb.Account{Id: id, Label: id}
		idxs[i] = i
	}
	sized.home.sessions = sessions
	sized.chatPicker.chats = chats
	sized.newSession.repos = repos
	sized.newSession.prsFiltered = idxs
	sized.newSession.issuesFiltered = idxs
	sized.repoList.repos = repos
	sized.trash.filteredSessions = idxs
	sized.cronList.jobs = jobs
	sized.accountsList.accounts = accounts

	// Stand in for the table.SetHeight each view performs in Update: cache every
	// table height against the un-reserved terminal height first, so the deltas
	// below are attributable to the reservation alone.
	sized.applyReservedTableHeights()
	tables := func(a *App) map[string]int {
		return map[string]int{
			"home":                 a.home.table.Height(),
			"chatPicker":           a.chatPicker.table.Height(),
			"newSession.repoTable": a.newSession.repoTable.Height(),
			"newSession.prTable":   a.newSession.prTable.Height(),
			"newSession.issueTab":  a.newSession.issueTable.Height(),
			"repoList":             a.repoList.table.Height(),
			"trash":                a.trash.table.Height(),
			"cronList":             a.cronList.table.Height(),
			"accountsList":         a.accountsList.table.Height(),
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
	a.home.height = 2
	a.chatPicker.height = 1
	a.reserveHeight(5)

	if a.home.height != 1 {
		t.Errorf("home.height = %d after over-reserving, want 1", a.home.height)
	}
	if a.chatPicker.height != 1 {
		t.Errorf("chatPicker.height = %d after over-reserving, want 1", a.chatPicker.height)
	}
}
