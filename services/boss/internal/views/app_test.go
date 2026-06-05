package views

import (
	"context"
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

func TestAppWithSettingsPassesSettingsToHome(t *testing.T) {
	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true

	a := NewApp(nil, nil)
	a.WithSettings(settings)

	if !a.home.settings.BossCloudGuestOfferHidden {
		t.Fatal("home.settings.BossCloudGuestOfferHidden = false, want true")
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
	if !strings.Contains(got.View().Content, "Loading your account. Continue in your browser...") {
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
