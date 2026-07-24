package views

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeHomeCloudAccessClient struct {
	status           *pb.CloudAccessStatus
	statusErr        error
	checkoutURL      string
	checkoutErr      error
	portalURL        string
	portalErr        error
	refresh          *pb.CloudAccessStatus
	refreshErr       error
	returnURLs       []string
	cancelURLs       []string
	portalReturnURLs []string
	statusCalls      int
	checkouts        int
	portals          int
	refreshes        int
}

func boolPtr(value bool) *bool { return &value }

func TestHomeQuestionNotifications(t *testing.T) {
	question := func(id string) *pb.Session {
		return &pb.Session{Id: id, Title: "BOS-459 alert", DisplayLabel: displaystatus.QuestionLabel}
	}
	notQuestion := func(id string) *pb.Session {
		return &pb.Session{Id: id, Title: "BOS-459 alert", DisplayLabel: "working"}
	}

	tests := []struct {
		name     string
		settings config.Settings
		polls    []sessionListMsg
		want     int
		wantCmds int
	}{
		{
			name: "rising edge notifies once",
			polls: []sessionListMsg{
				{sessions: []*pb.Session{notQuestion("s1")}},
				{sessions: []*pb.Session{question("s1")}},
			},
			want: 1, wantCmds: 1,
		},
		{
			name: "persistent question does not re-notify",
			polls: []sessionListMsg{
				{sessions: []*pb.Session{question("s1")}},
				{sessions: []*pb.Session{question("s1")}},
			},
			want: 1, wantCmds: 1,
		},
		{
			name:     "disabled notifications suppresses question",
			settings: config.Settings{NotificationsEnabled: boolPtr(false)},
			polls:    []sessionListMsg{{sessions: []*pb.Session{question("s1")}}},
			want:     0, wantCmds: 0,
		},
		{
			name: "leaving and re-entering question notifies again",
			polls: []sessionListMsg{
				{sessions: []*pb.Session{question("s1")}},
				{sessions: []*pb.Session{notQuestion("s1")}},
				{sessions: []*pb.Session{question("s1")}},
			},
			want: 2, wantCmds: 2,
		},
		{
			name: "first successful poll notifies waiting question",
			polls: []sessionListMsg{
				{sessions: []*pb.Session{question("s1")}},
			},
			want: 1, wantCmds: 1,
		},
		{
			name: "transient poll error retains prior question state",
			polls: []sessionListMsg{
				{sessions: []*pb.Session{question("s1")}},
				{err: errors.New("temporary daemon error")},
				{sessions: []*pb.Session{question("s1")}},
			},
			want: 1, wantCmds: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalRunNotify := runNotify
			defer func() { runNotify = originalRunNotify }()

			var requests []notifyRequest
			runNotify = func(req notifyRequest) error {
				requests = append(requests, req)
				return nil
			}

			h := NewHomeModel(nil, context.Background(), nil)
			h.SetSettings(tt.settings)
			cmds := 0
			for _, poll := range tt.polls {
				model, cmd := h.Update(poll)
				h = model.(HomeModel)
				if cmd != nil {
					cmds++
					cmd()
				}
			}

			if got := len(requests); got != tt.want {
				t.Errorf("notification count = %d, want %d", got, tt.want)
			}
			if cmds != tt.wantCmds {
				t.Errorf("notification command count = %d, want %d", cmds, tt.wantCmds)
			}
		})
	}
}

func TestHomeQuestionNotificationsDiscardOutOfOrderPolls(t *testing.T) {
	question := func() *pb.Session {
		return &pb.Session{Id: "s1", Title: "BOS-459 alert", DisplayLabel: displaystatus.QuestionLabel}
	}
	working := func() *pb.Session {
		return &pb.Session{Id: "s1", Title: "BOS-459 alert", DisplayLabel: "working"}
	}

	originalRunNotify := runNotify
	defer func() { runNotify = originalRunNotify }()
	requests := 0
	runNotify = func(notifyRequest) error {
		requests++
		return nil
	}

	h := NewHomeModel(nil, context.Background(), nil)
	for _, poll := range []sessionListMsg{
		{pollID: 1, sessions: []*pb.Session{working()}},
		{pollID: 2, sessions: []*pb.Session{question()}},
		// The initial fetch can finish after a newer timer poll. Its stale
		// snapshot must not reset the question edge state.
		{pollID: 1, sessions: []*pb.Session{working()}},
		{pollID: 3, sessions: []*pb.Session{question()}},
	} {
		model, cmd := h.Update(poll)
		h = model.(HomeModel)
		if cmd != nil {
			cmd()
		}
	}

	if requests != 1 {
		t.Fatalf("notification count = %d, want 1 after stale poll is discarded", requests)
	}
}

func TestHomeDiscardsSessionPollFromAnotherGeneration(t *testing.T) {
	oldHome := NewHomeModel(nil, context.Background(), nil)
	h := NewHomeModel(nil, context.Background(), nil)
	if oldHome.generation == h.generation {
		t.Fatal("separate HomeModels reused their generation")
	}

	model, cmd := h.Update(sessionListMsg{
		homeGeneration: oldHome.generation,
		pollID:         1,
		sessions:       []*pb.Session{{Id: "old-session"}},
	})
	got := model.(HomeModel)
	if cmd != nil {
		t.Fatal("discarded session poll returned a command")
	}
	if !got.loading {
		t.Fatal("discarded session poll cleared loading state")
	}
	if len(got.sessions) != 0 {
		t.Fatalf("discarded session poll populated sessions: %+v", got.sessions)
	}
}

func (f *fakeHomeCloudAccessClient) GetCloudAccessStatus(context.Context) (*pb.CloudAccessStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}

func (f *fakeHomeCloudAccessClient) CreateCheckoutSession(_ context.Context, returnURL, cancelURL string) (string, error) {
	f.checkouts++
	f.returnURLs = append(f.returnURLs, returnURL)
	f.cancelURLs = append(f.cancelURLs, cancelURL)
	if f.checkoutErr != nil {
		return "", f.checkoutErr
	}
	return f.checkoutURL, nil
}

func (f *fakeHomeCloudAccessClient) CreateBillingPortalSession(_ context.Context, returnURL string) (string, error) {
	f.portals++
	f.portalReturnURLs = append(f.portalReturnURLs, returnURL)
	if f.portalErr != nil {
		return "", f.portalErr
	}
	return f.portalURL, nil
}

func (f *fakeHomeCloudAccessClient) RefreshCloudEntitlements(context.Context) (*pb.CloudAccessStatus, error) {
	f.refreshes++
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if f.refresh != nil {
		return f.refresh, nil
	}
	return f.status, nil
}

func TestRenderAttentionIndicator(t *testing.T) {
	tests := []struct {
		name    string
		session *pb.Session
		want    string
	}{
		{
			name:    "nil attention status",
			session: &pb.Session{},
			want:    "",
		},
		{
			name: "no attention needed",
			session: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{NeedsAttention: false},
			},
			want: "",
		},
		{
			name: "blocked max attempts renders red",
			session: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
				},
			},
			want: styleStatusDanger.Render("!"),
		},
		{
			name: "merge conflict renders orange",
			session: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
				},
			},
			want: "",
		},
		{
			name: "review requested does not render indicator",
			session: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: false,
					Reason:         pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED,
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderAttentionIndicator(tt.session)
			if got != tt.want {
				t.Errorf("renderAttentionIndicator() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHomeCloudGateRendersNeedsSubscription(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	model, _ := h.Update(cloudAccessMsg{status: &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
	}})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Bossanova Cloud requires a subscription") {
		t.Fatalf("expected subscription gate, got: %s", content)
	}
	if !strings.Contains(content, "Local sessions are still available") {
		t.Fatalf("expected local-usable copy, got: %s", content)
	}
	if !strings.Contains(content, "Type u to s[u]bscribe.") {
		t.Fatalf("expected subscribe key hint, got: %s", content)
	}
	if strings.Contains(content, "[b]illing") {
		t.Fatalf("home should not render a permanent billing action, got: %s", content)
	}
}

func TestHomeCloudGateActiveIsSilent(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	model, _ := h.Update(cloudAccessMsg{status: &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
	}})
	h = model.(HomeModel)

	content := h.View().Content
	if strings.Contains(content, "Finish Bossanova Cloud setup") {
		t.Fatalf("active access should not render setup gate, got: %s", content)
	}
	if strings.Contains(content, "[b]illing") {
		t.Fatalf("active access should not render checkout action, got: %s", content)
	}
}

func TestHomeCloudGatePendingRefreshCommand(t *testing.T) {
	fake := &fakeHomeCloudAccessClient{
		refresh: &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE},
	}
	msg := refreshCloudAccessStatus(fake, context.Background())()
	got, ok := msg.(cloudAccessMsg)
	if !ok {
		t.Fatalf("refreshCloudAccessStatus returned %T, want cloudAccessMsg", msg)
	}
	if fake.refreshes != 1 {
		t.Fatalf("refresh calls = %d, want 1", fake.refreshes)
	}
	if got.status.GetState() != pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE {
		t.Fatalf("refresh status = %v, want active", got.status.GetState())
	}
}

func TestHomeCloudGateAccessErrorIsNonBlocking(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	model, _ := h.Update(cloudAccessMsg{err: errors.New("connection refused")})
	h = model.(HomeModel)

	line := h.cloudGateLine()
	if !strings.Contains(line, "Cloud access status unavailable") {
		t.Fatalf("expected cloud access status error, got: %s", line)
	}
	if !strings.Contains(line, "connection refused") {
		t.Fatalf("expected error detail, got: %s", line)
	}
	if !strings.Contains(line, "Local sessions are still available") {
		t.Fatalf("expected local-usable copy, got: %s", line)
	}
	if strings.Contains(line, "Cloud billing unavailable") {
		t.Fatalf("generic error used billing copy: %s", line)
	}
	content := h.View().Content
	if !strings.Contains(content, "[n]ew session") {
		t.Fatalf("expected local session actions to remain available, got: %s", content)
	}
}

func TestHome_ActiveCloudAccess_NoUnavailableBanner(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	model, _ := h.Update(cloudAccessMsg{status: &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE,
	}})
	h = model.(HomeModel)

	line := h.cloudGateLine()
	if strings.Contains(line, "Cloud access status unavailable") {
		t.Fatalf("active cloud access should not render the unavailable banner; got:\n%s", line)
	}
}

func TestHomeCloudGateBillingUnavailableStatusUsesBillingCopy(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	model, _ := h.Update(cloudAccessMsg{status: &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_BILLING_UNAVAILABLE,
	}})
	h = model.(HomeModel)

	line := h.cloudGateLine()
	if !strings.Contains(line, "Cloud billing unavailable") {
		t.Fatalf("expected billing unavailable copy, got: %s", line)
	}
	if !strings.Contains(line, "Local sessions are still available") {
		t.Fatalf("expected local-usable copy, got: %s", line)
	}
	if strings.Contains(line, "Cloud access status unavailable") {
		t.Fatalf("billing status used generic access copy: %s", line)
	}
}

func TestHomeCloudGateConnectErrorIncludesCodeAndDetail(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	err := connect.NewError(connect.CodeUnauthenticated, errors.New("refresh token rejected"))
	model, _ := h.Update(cloudAccessMsg{err: err})
	h = model.(HomeModel)

	line := h.cloudGateLine()
	if !strings.Contains(line, "Cloud access status unavailable") {
		t.Fatalf("expected cloud access status error, got: %s", line)
	}
	if !strings.Contains(line, "unauthenticated") {
		t.Fatalf("expected connect code, got: %s", line)
	}
	if count := strings.Count(line, "unauthenticated"); count != 1 {
		t.Fatalf("expected connect code once, got %d occurrences in: %s", count, line)
	}
	if !strings.Contains(line, "unauthenticated: refresh token rejected") {
		t.Fatalf("expected single code prefix with detail, got: %s", line)
	}
	if !strings.Contains(line, "refresh token rejected") {
		t.Fatalf("expected safe detail, got: %s", line)
	}
}

func TestHomeCloudGateAccessErrorRedactsSecrets(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1

	err := errors.New("request failed: access_token=access-token-123 refresh_token=refresh-token-456 Authorization: Bearer secret-token-123")
	model, _ := h.Update(cloudAccessMsg{err: err})
	h = model.(HomeModel)

	line := h.cloudGateLine()
	if !strings.Contains(line, "[redacted]") {
		t.Fatalf("expected redacted marker, got: %s", line)
	}
	for _, secret := range []string{"access-token-123", "refresh-token-456", "secret-token-123"} {
		if strings.Contains(line, secret) {
			t.Fatalf("rendered secret %q in line: %s", secret, line)
		}
	}
}

func TestHomeCloudGateDoesNotRenderPermanentBillingAction(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
	}

	content := h.View().Content
	if strings.Contains(content, "[b]illing") {
		t.Fatalf("home rendered permanent billing action: %q", content)
	}
	if !strings.Contains(content, "Bossanova Cloud requires a subscription") {
		t.Fatalf("home missing quiet cloud reminder: %q", content)
	}
}

func TestHomeCloudGateKeepsLocalSessionsUsable(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.sessions = []*pb.Session{{Id: "sess-1", Title: "local-session"}}
	h.buildTableRows()
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
	}

	content := h.View().Content
	if !strings.Contains(content, "[enter] select") {
		t.Fatalf("home did not keep local session action usable: %q", content)
	}
	if strings.Contains(content, "press l") {
		t.Fatalf("home rendered misleading logout key as billing hint: %q", content)
	}
}

func TestHomeCloudGateSubscribeKeyStartsSubscriptionFlow(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
	}

	_, cmd := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd == nil {
		t.Fatal("key u: got nil cmd, want subscription flow command")
	}
	msg := cmd()
	if _, ok := msg.(startSubscriptionFlowMsg); !ok {
		t.Fatalf("subscription command returned %T, want startSubscriptionFlowMsg", msg)
	}
}

func TestHomeUpgradeKeyWinsWhenUpgradeAvailable(t *testing.T) {
	oldRunUpgradeCmd := runUpgradeCmd
	defer func() { runUpgradeCmd = oldRunUpgradeCmd }()

	upgradeRan := false
	runUpgradeCmd = func(string) tea.Cmd {
		return func() tea.Msg {
			upgradeRan = true
			return upgradeRunMsg{}
		}
	}

	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.upgradeAvailable = true
	h.cloudStatus = &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CanCreateCheckout: true,
	}

	_, cmd := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd == nil {
		t.Fatal("key u: got nil cmd, want upgrade command")
	}
	msg := cmd()
	if !upgradeRan {
		t.Fatal("key u did not run upgrade")
	}
	if _, ok := msg.(upgradeRunMsg); !ok {
		t.Fatalf("upgrade command returned %T, want upgradeRunMsg", msg)
	}
}

func TestHomeUpgradeFailureShowsCapturedOutput(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.repoCount = 1
	h.upgradeAvailable = true

	model, _ := h.Update(upgradeRunMsg{
		err:    errors.New("exit status 1"),
		output: "download failed\npermission denied\n",
	})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "exit status 1") ||
		!strings.Contains(content, "download failed") ||
		!strings.Contains(content, "permission denied") {
		t.Fatalf("upgrade failure output not persisted in home view: %q", content)
	}
}

func TestHomeCloudGateExplainsUpgradeBeforeSubscribe(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.upgradeAvailable = true
	h.cloudStatus = &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CanCreateCheckout: true,
	}

	content := h.View().Content
	if !strings.Contains(content, "Upgrade boss first, then subscribe.") {
		t.Fatalf("missing upgrade-before-subscribe copy: %q", content)
	}
	if strings.Contains(content, "Type u to s[u]bscribe") {
		t.Fatalf("rendered conflicting u subscribe copy during upgrade: %q", content)
	}
}

func TestHomeCloudGateUpgradeKeyWinsOverSubscribeKey(t *testing.T) {
	oldRunUpgradeCmd := runUpgradeCmd
	defer func() { runUpgradeCmd = oldRunUpgradeCmd }()

	upgradeRan := false
	runUpgradeCmd = func(string) tea.Cmd {
		return func() tea.Msg {
			upgradeRan = true
			return upgradeRunMsg{}
		}
	}

	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
	}
	h.upgradeAvailable = true

	content := h.View().Content
	if !strings.Contains(content, "[u]pgrade") {
		t.Fatalf("home did not render upgrade shortcut: %q", content)
	}
	if strings.Contains(content, "Type u to s[u]bscribe.") {
		t.Fatalf("home rendered conflicting subscription shortcut: %q", content)
	}

	_, cmd := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd == nil {
		t.Fatal("key u: got nil cmd, want upgrade command")
	}
	msg := cmd()
	if !upgradeRan {
		t.Fatal("key u did not run upgrade")
	}
	if _, ok := msg.(upgradeRunMsg); !ok {
		t.Fatalf("upgrade command returned %T, want upgradeRunMsg", msg)
	}
}

func TestHomeCloudGatePendingSetupCanStartSubscriptionFlowAgain(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
	}

	content := h.View().Content
	if !strings.Contains(content, "Bossanova Cloud setup has not completed yet. Type u to s[u]bscribe.") {
		t.Fatalf("pending setup copy missing: %q", content)
	}

	_, cmd := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd == nil {
		t.Fatal("key u: got nil cmd, want subscription flow command")
	}
	msg := cmd()
	if _, ok := msg.(startSubscriptionFlowMsg); !ok {
		t.Fatalf("subscription command returned %T, want startSubscriptionFlowMsg", msg)
	}
}

func TestHomeCloudCheckoutActivationPendingKeepsPolling(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
	}

	updated, cmd := h.Update(homeCloudCheckoutMsg{
		err: connect.NewError(connect.CodeFailedPrecondition, errors.New("bossanova cloud subscription is being activated")),
	})
	h = updated.(HomeModel)

	content := h.View().Content
	if strings.Contains(content, "Cloud checkout unavailable") {
		t.Fatalf("pending activation should not render checkout unavailable: %q", content)
	}
	if !strings.Contains(content, "Bossanova Cloud setup has not completed yet. Type u to s[u]bscribe.") {
		t.Fatalf("pending activation copy missing: %q", content)
	}
	if !h.cloudCheckoutPolling {
		t.Fatal("pending activation checkout failure should keep polling")
	}
	if cmd == nil {
		t.Fatal("pending activation checkout failure should refresh entitlements")
	}
}

func TestHomeCloudCheckoutStatusClearsWhenLoggedOut(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudCheckoutStatus = "Opened Bossanova Cloud subscription checkout."
	h.cloudCheckoutPolling = true
	h.cloudStatus = &pb.CloudAccessStatus{
		State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH,
	}

	updated, _ := h.Update(authStatusMsg{loggedIn: false})
	h = updated.(HomeModel)

	content := h.View().Content
	if strings.Contains(content, "Opened Bossanova Cloud subscription checkout.") {
		t.Fatalf("logged-out home rendered stale checkout status: %q", content)
	}
	if h.cloudCheckoutStatus != "" {
		t.Fatalf("cloudCheckoutStatus = %q, want empty", h.cloudCheckoutStatus)
	}
	if h.cloudCheckoutPolling {
		t.Fatal("logged-out home should stop checkout polling")
	}
}

func TestStateLabel_MergedUsesLightCheck(t *testing.T) {
	got := StateLabel(pb.SessionState_SESSION_STATE_MERGED)
	if got != "✓ merged" {
		t.Fatalf("StateLabel(MERGED) = %q, want %q", got, "✓ merged")
	}
}

// TestStateLabel_Orphaned pins the distinct non-green label so a restart-killed
// headless run never renders as "green" or leaks the raw enum as "unknown".
func TestStateLabel_Orphaned(t *testing.T) {
	got := StateLabel(pb.SessionState_SESSION_STATE_ORPHANED)
	if got != "orphaned" {
		t.Fatalf("StateLabel(ORPHANED) = %q, want %q", got, "orphaned")
	}
}

func TestHomeBuildTableRows_RendersRepairWarningUnderName(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:                     "sess-1",
				RepoDisplayName:        "agenticboost",
				Title:                  "[WON-462] Restore SSE no-config guard",
				DisplayLabel:           "? question",
				DisplayIntent:          pb.DisplayIntent_DISPLAY_INTENT_WARNING,
				LastRepairRunnerError:  "claude not on PATH",
				LastRepairAttemptCount: 2,
			},
		},
	}

	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2: session row plus repair warning row", len(rows))
	}
	if got := rows[0][5]; strings.Contains(got, "repair") {
		t.Fatalf("STATUS column contains repair warning %q; warning belongs under NAME", got)
	}
	if got := rows[1][3]; !strings.Contains(got, "repair failed (2") {
		t.Fatalf("warning row NAME column = %q, want repair warning", got)
	}
	if got := rows[1][5]; got != "" {
		t.Fatalf("warning row STATUS column = %q, want empty", got)
	}
}

func TestHomeBuildTableRows_RendersAttentionWarningUnderName(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:              "sess-1",
				RepoDisplayName: "agenticboost",
				Title:           "[WON-832] Improve cache eviction behaviour",
				DisplayLabel:    "working",
				DisplayIntent:   pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
					Summary:        "auto-resolve conflicts disabled, needs human",
				},
			},
		},
	}

	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2: session row plus attention warning row", len(rows))
	}
	if got := rows[0][1]; got != "" {
		t.Fatalf("session attention column = %q, want empty when warning row is rendered", got)
	}
	if got := rows[0][5]; strings.Contains(got, "auto-resolve") {
		t.Fatalf("STATUS column contains attention warning %q; warning belongs under NAME", got)
	}
	if got := rows[1][3]; !strings.Contains(got, "auto-resolve conflicts disabled") {
		t.Fatalf("warning row NAME column = %q, want attention warning", got)
	}
	if got := rows[1][5]; got != "" {
		t.Fatalf("warning row STATUS column = %q, want empty", got)
	}
}

func TestHomeBuildTableRows_PreservesSessionOrderWhenSessionNeedsAttention(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:              "normal-newer",
				RepoDisplayName: "bossanova",
				Title:           "Normal newer",
			},
			{
				Id:              "attention-older",
				RepoDisplayName: "bossanova",
				Title:           "Attention older",
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
					Summary:        "needs human review",
				},
			},
			{
				Id:              "normal-oldest",
				RepoDisplayName: "bossanova",
				Title:           "Normal oldest",
			},
		},
	}

	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 4 {
		t.Fatalf("table rows = %d, want 4: three sessions plus attention warning row", len(rows))
	}
	// The cursor row (row 0) now carries selection-blue SGR; strip ANSI so this
	// order assertion compares visible text regardless of selection styling.
	if got := stripANSI(rows[0][3]); got != "Normal newer" {
		t.Fatalf("first session row NAME = %q, want Normal newer", got)
	}
	if got := stripANSI(rows[1][3]); got != "Attention older" {
		t.Fatalf("second session row NAME = %q, want Attention older", got)
	}
	if got := stripANSI(rows[3][3]); got != "Normal oldest" {
		t.Fatalf("third session row NAME = %q, want Normal oldest", got)
	}
}

func TestHomeBuildTableRows_HidesAgentColumnWhenMultipleAgentsPresent(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:              "sess-1",
				RepoDisplayName: "bossanova",
				Title:           "Claude session",
				AgentName:       "claude",
			},
			{
				Id:              "sess-2",
				RepoDisplayName: "bossanova",
				Title:           "Codex session",
				AgentName:       "codex",
			},
		},
	}

	h.buildTableRows()

	cols := h.table.Columns()
	for _, col := range cols {
		if col.Title == "AGENT" {
			t.Fatalf("session table should not render AGENT column: %#v", cols)
		}
	}
	rows := h.table.Rows()
	// Row 0 is the cursor row and now carries selection-blue SGR around the "-"
	// PR placeholder; strip ANSI to assert the placeholder regardless of styling.
	if got := stripANSI(rows[0][4]); got != "-" {
		t.Fatalf("session row PR column = %q, want -", got)
	}
	if got := stripANSI(rows[1][4]); got != "-" {
		t.Fatalf("session row PR column = %q, want -", got)
	}
}

func TestHomeBuildTableRows_HidesAgentColumnWhenMultipleAgentsAvailable(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:              "sess-1",
				RepoDisplayName: "bossanova",
				Title:           "Codex session",
				AgentName:       "codex",
			},
		},
	}

	h.buildTableRows()

	cols := h.table.Columns()
	for _, col := range cols {
		if col.Title == "AGENT" {
			t.Fatalf("session table should not render AGENT column: %#v", cols)
		}
	}
	rows := h.table.Rows()
	// Row 0 is the cursor row and now carries selection-blue SGR around the "-"
	// PR placeholder; strip ANSI to assert the placeholder regardless of styling.
	if got := stripANSI(rows[0][4]); got != "-" {
		t.Fatalf("session row PR column = %q, want -", got)
	}
}

func TestHomeTableHeightCountsRepairWarningRows(t *testing.T) {
	h := HomeModel{
		sessions: []*pb.Session{
			{
				Id:                     "sess-1",
				LastRepairRunnerError:  "claude not on PATH",
				LastRepairAttemptCount: 2,
			},
		},
	}

	if got := h.tableHeight(); got != 3 {
		t.Fatalf("tableHeight() = %d, want 3: header plus session row plus repair warning row", got)
	}
}

func TestRenderTrackerLink(t *testing.T) {
	url := "https://linear.app/team/issue/FRE-1176"
	tests := []struct {
		name  string
		sess  *pb.Session
		title string
		want  string
	}{
		{
			name:  "nil session",
			sess:  nil,
			title: "[FRE-1176] Some title",
			want:  "[FRE-1176] Some title",
		},
		{
			name:  "no tracker ID",
			sess:  &pb.Session{},
			title: "[FRE-1176] Some title",
			want:  "[FRE-1176] Some title",
		},
		{
			name:  "tracker ID not in title",
			sess:  &pb.Session{TrackerId: strPtr("FRE-999"), TrackerUrl: &url},
			title: "[FRE-1176] Some title",
			want:  "[FRE-1176] Some title",
		},
		{
			name:  "tracker ID with URL",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176"), TrackerUrl: &url},
			title: "[FRE-1176] Some title",
			want:  "\x1b]8;;" + url + "\x1b\\\x1b[4m[FRE-1176]\x1b[24m\x1b]8;;\x1b\\ Some title",
		},
		{
			name:  "tracker ID without URL",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176")},
			title: "[FRE-1176] Some title",
			want:  "\x1b[4m[FRE-1176]\x1b[24m Some title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderTrackerLink(tt.sess, tt.title)
			if got != tt.want {
				t.Errorf("renderTrackerLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderMutedTrackerLink(t *testing.T) {
	url := "https://linear.app/team/issue/FRE-1176"
	// Shorthands for the raw-ANSI envelopes used in the expected strings.
	const (
		ms   = "\x1b[38;2;98;98;98;9m"                 // muted + strike open
		msc  = "\x1b[39;29m"                           // muted + strike close
		msu  = "\x1b[38;2;98;98;98;58;2;98;98;98;9;4m" // muted + strike + underline (with matching underline color) open
		msuc = "\x1b[39;59;29;24m"                     // muted + strike + underline close
	)
	target := "[FRE-1176]"
	styledTarget := msu + target + msuc
	linkedTarget := "\x1b]8;;" + url + "\x1b\\" + styledTarget + "\x1b]8;;\x1b\\"

	tests := []struct {
		name  string
		sess  *pb.Session
		title string
		want  string
	}{
		{
			name:  "nil session wraps whole title",
			sess:  nil,
			title: "[FRE-1176] Some title",
			want:  ms + "[FRE-1176] Some title" + msc,
		},
		{
			name:  "no tracker ID wraps whole title",
			sess:  &pb.Session{},
			title: "[FRE-1176] Some title",
			want:  ms + "[FRE-1176] Some title" + msc,
		},
		{
			name:  "tracker ID not in title wraps whole title",
			sess:  &pb.Session{TrackerId: strPtr("FRE-999"), TrackerUrl: &url},
			title: "[FRE-1176] Some title",
			want:  ms + "[FRE-1176] Some title" + msc,
		},
		{
			name:  "tracker ID with URL",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176"), TrackerUrl: &url},
			title: "[FRE-1176] Some title",
			want:  linkedTarget + ms + " Some title" + msc,
		},
		{
			name:  "tracker ID without URL",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176")},
			title: "[FRE-1176] Some title",
			want:  styledTarget + ms + " Some title" + msc,
		},
		{
			name:  "tracker ID at end of title",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176"), TrackerUrl: &url},
			title: "Some title [FRE-1176]",
			want:  ms + "Some title " + msc + linkedTarget,
		},
		{
			name:  "title is only the tracker ID",
			sess:  &pb.Session{TrackerId: strPtr("FRE-1176"), TrackerUrl: &url},
			title: "[FRE-1176]",
			want:  linkedTarget,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderMutedTrackerLink(tt.sess, tt.title)
			if got != tt.want {
				t.Errorf("renderMutedTrackerLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHomeDoesNotShowRetryPRActionForDraftPRFailure(t *testing.T) {
	reason := "draft PR creation failed: gh auth failed"
	h := HomeModel{
		ctx:     context.Background(),
		loading: false,
		sessions: []*pb.Session{{
			Id:             "sess-1",
			Title:          "Add dark mode",
			BranchName:     "task-branch",
			RepoOriginUrl:  "https://github.com/example/repo.git",
			BlockedReason:  &reason,
			DisplayLabel:   "? PR failed",
			DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			DisplaySpinner: false,
		}},
	}
	h.buildTableRows()

	content := h.View().Content
	if strings.Contains(content, "[p] retry PR") {
		t.Fatalf("view contains retry PR action: %s", content)
	}
	if !strings.Contains(content, "? PR failed") {
		t.Fatalf("view missing draft PR warning %q: %s", "? PR failed", content)
	}
}

// TestHomeKeyDispatch_Regression verifies that home-list keybindings dispatch
// the correct switchViewMsg.
func TestHomeKeyDispatch_Regression(t *testing.T) {
	// Build a HomeModel with one repo (so [n] is enabled) and auth configured
	// (so [l] is enabled). We drive Update() directly without a real daemon.
	authMgr := (*auth.Manager)(nil) // nil authMgr disables l; tested separately

	tests := []struct {
		key      string
		wantView View
	}{
		{"n", ViewNewSession},
		{"s", ViewSettings},
	}

	for _, tt := range tests {
		t.Run("key="+tt.key, func(t *testing.T) {
			h := HomeModel{
				ctx:       context.Background(),
				authMgr:   authMgr,
				repoCount: 1, // enable [n]
				loading:   false,
			}
			model, cmd := h.Update(tea.KeyPressMsg{Code: rune(tt.key[0]), Text: tt.key})
			_ = model
			if cmd == nil {
				t.Fatalf("key %q: got nil cmd, want a switchViewMsg command", tt.key)
			}
			msg := cmd()
			svm, ok := msg.(switchViewMsg)
			if !ok {
				t.Fatalf("key %q: cmd() returned %T, want switchViewMsg", tt.key, msg)
			}
			if svm.view != tt.wantView {
				t.Errorf("key %q: view = %v, want %v", tt.key, svm.view, tt.wantView)
			}
		})
	}

	// [enter] on an existing session always opens the chat picker. This is a
	// regression guard against re-introducing auto-attach: previously, if a
	// session had exactly one running chat, Enter would skip the picker and
	// jump directly into ViewAttach. The picker self-highlights the running
	// chat (chatpicker.go:316-332), so resume is still cheap from the picker.
	t.Run("key=enter dispatches ViewChatPicker (no auto-attach)", func(t *testing.T) {
		h := HomeModel{
			ctx:       context.Background(),
			repoCount: 1,
			loading:   false,
			sessions:  []*pb.Session{{Id: "sess-1"}},
		}
		h.buildTableRows()
		_, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("key enter: got nil cmd, want a switchViewMsg command")
		}
		msg := cmd()
		svm, ok := msg.(switchViewMsg)
		if !ok {
			t.Fatalf("key enter: cmd() returned %T, want switchViewMsg (do NOT route via auto-attach)", msg)
		}
		if svm.view != ViewChatPicker {
			t.Errorf("key enter: view = %v, want ViewChatPicker", svm.view)
		}
		if svm.sessionID != "sess-1" {
			t.Errorf("key enter: sessionID = %q, want %q", svm.sessionID, "sess-1")
		}
		if svm.resumeID != "" {
			t.Errorf("key enter: resumeID = %q, want empty (no auto-attach)", svm.resumeID)
		}
	})

	t.Run("key=h no longer opens history", func(t *testing.T) {
		h := HomeModel{
			ctx:       context.Background(),
			repoCount: 1,
			loading:   false,
			sessions:  []*pb.Session{{Id: "sess-1"}},
		}
		h.buildTableRows()
		_, cmd := h.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
		if cmd != nil {
			t.Fatal("key h returned command, want nil")
		}
	})

	t.Run("moved keys are inert", func(t *testing.T) {
		for _, key := range []string{"r", "t", "c", "a"} {
			t.Run("key="+key, func(t *testing.T) {
				h := HomeModel{
					ctx:       context.Background(),
					repoCount: 1,
					loading:   false,
					sessions:  []*pb.Session{{Id: "sess-1"}},
				}
				h.buildTableRows()

				_, cmd := h.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
				if cmd != nil {
					t.Fatalf("key %q returned command, want nil", key)
				}
			})
		}
	})

	// [l] with auth configured and not logged-in dispatches ViewLogin.
	t.Run("key=l dispatches ViewLogin when not logged in", func(t *testing.T) {
		// We need a non-nil authMgr to enable [l]; use a real zero-value Manager.
		mgr := &auth.Manager{}
		h := HomeModel{
			ctx:       context.Background(),
			authMgr:   mgr,
			repoCount: 1,
			loading:   false,
			loggedIn:  false,
		}
		_, cmd := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
		if cmd == nil {
			t.Fatal("key l: got nil cmd, want a switchViewMsg command")
		}
		msg := cmd()
		svm, ok := msg.(switchViewMsg)
		if !ok {
			t.Fatalf("key l: cmd() returned %T, want switchViewMsg", msg)
		}
		if svm.view != ViewLogin {
			t.Errorf("key l: view = %v, want %v", svm.view, ViewLogin)
		}
	})
}

func TestHomeViewShowsCloudDiscoveryWithActiveSessions(t *testing.T) {
	now := time.Now()
	settings := config.DefaultSettings()
	settings.BossCloudValueDeliveredAt = now // within the 72 h offer window
	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		loggedIn:  false,
		sessions:  []*pb.Session{{Id: "sess-1", Title: "Active work"}},
		settings:  settings,
	}
	h.buildTableRows()

	content := h.View().Content
	for _, want := range []string{"Bossanova Cloud", "[l]ogin to try Bossanova Cloud for free"} {
		if !strings.Contains(content, want) {
			t.Fatalf("home view missing %q in active-session cloud prompt: %q", want, content)
		}
	}
	menuIdx := strings.Index(content, "[q]uit")
	promptIdx := strings.Index(content, "[l]ogin to try Bossanova Cloud for free")
	if menuIdx == -1 || promptIdx == -1 || promptIdx < menuIdx {
		t.Fatalf("home view should render cloud prompt under bottom menu; got: %q", content)
	}
	separator := content[menuIdx:promptIdx]
	if !strings.Contains(separator, "\n") || strings.Contains(separator, "\n\n") {
		t.Fatalf("home view should leave one newline between bottom menu and cloud prompt; got: %q", content)
	}
}

func TestHomeViewShowsCloudDiscoveryWithoutExtraBlankLinesWhenNoSessions(t *testing.T) {
	now := time.Now()
	settings := config.DefaultSettings()
	settings.BossCloudValueDeliveredAt = now // within the 72 h offer window
	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		loggedIn:  false,
		sessions:  []*pb.Session{},
		settings:  settings,
	}

	content := h.View().Content
	menuIdx := strings.Index(content, "[q]uit")
	promptIdx := strings.Index(content, "[l]ogin to try Bossanova Cloud for free")
	if menuIdx == -1 || promptIdx == -1 || promptIdx < menuIdx {
		t.Fatalf("home view should render cloud prompt under bottom menu; got: %q", content)
	}
	separator := content[menuIdx:promptIdx]
	if !strings.Contains(separator, "\n") || strings.Contains(separator, "\n\n") {
		t.Fatalf("home view should leave one newline between bottom menu and cloud prompt; got: %q", content)
	}
}

func TestHomeViewHidesCloudDiscoveryWhenOfferHiddenInSettings(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = now
	settings.BossCloudGuestOfferHidden = true

	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		loggedIn:  false,
		sessions:  []*pb.Session{{Id: "sess-1", Title: "Active work"}},
		settings:  settings,
		startedAt: now,
		now:       func() time.Time { return now },
	}
	h.buildTableRows()

	content := h.View().Content
	if strings.Contains(content, "Bossanova Cloud") {
		t.Fatalf("home view showed cloud discovery despite settings hide flag: %q", content)
	}
}

func TestHomeViewHidesCloudDiscoveryAfterSessionLimit(t *testing.T) {
	startedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = startedAt

	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		loggedIn:  false,
		sessions:  []*pb.Session{{Id: "sess-1", Title: "Active work"}},
		settings:  settings,
		startedAt: startedAt,
		now:       func() time.Time { return startedAt.Add(cloudGuestOfferSessionLimit + time.Second) },
	}
	h.buildTableRows()

	content := h.View().Content
	if strings.Contains(content, "Bossanova Cloud") {
		t.Fatalf("home view showed cloud discovery after session limit: %q", content)
	}
}

func strPtr(s string) *string { return &s }

func TestViewEmptyStateNoRepos(t *testing.T) {
	// Create a HomeModel with no sessions and no repos
	h := HomeModel{
		ctx:       context.Background(),
		loading:   false,
		sessions:  []*pb.Session{},
		repoCount: 0,
	}

	// Render the view
	view := h.View()
	content := view.Content

	// Check for welcome message
	if !strings.Contains(content, "Welcome to Bossanova") {
		t.Errorf("expected welcome message in empty state with no repos, got: %s", content)
	}

	// Check for setup instructions guiding the user to add their first repo.
	if !strings.Contains(content, "Press Enter to add your first repository") {
		t.Errorf("expected add-repository prompt in empty state with no repos, got: %s", content)
	}

	if strings.Contains(content, "[n]ew session") {
		t.Errorf("should not offer [n]ew session when no repos exist, got: %s", content)
	}
	// The zero-repo empty state's primary action is adding a repository, not
	// opening Settings.
	if !strings.Contains(content, "[enter] add repository") {
		t.Errorf("expected [enter] add repository in empty state action bar, got: %s", content)
	}
	if strings.Contains(content, "[s]ettings") {
		t.Errorf("should not advertise [s]ettings in the zero-repo empty state, got: %s", content)
	}
	if !strings.Contains(content, "[q]uit") {
		t.Errorf("expected [q]uit in empty state action bar, got: %s", content)
	}
}

func TestHomeLogoutConfirmationVisibleInEmptyNoRepoState(t *testing.T) {
	h := HomeModel{
		ctx:           context.Background(),
		loading:       false,
		sessions:      []*pb.Session{},
		repoCount:     0,
		authMgr:       &auth.Manager{},
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
	}

	updated, _ := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h = updated.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Log out dev@example.com?") {
		t.Fatalf("expected visible logout confirmation in empty no-repo state, got: %s", content)
	}
	if !strings.Contains(content, "[y/enter] confirm") {
		t.Fatalf("expected logout confirmation actions in empty no-repo state, got: %s", content)
	}
	// While confirming, the [l]ogout action must be suppressed so the control
	// and its confirmation prompt aren't shown at once (mirrors the list view).
	if strings.Contains(content, "[l]ogout") {
		t.Fatalf("expected [l]ogout action to be hidden while confirming logout, got: %s", content)
	}
	// The logout confirmation must be visually separated from the action bar
	// above it (the separation comes from styleActionBar's bottom padding plus
	// the helper's leading newline). Assert at least one blank line so the two
	// blocks aren't glued together, without over-coupling to the exact padding.
	got := blankLinesBetween(content, "[q]uit", "Log out dev@example.com?")
	if got == markersNotFound {
		t.Fatalf("could not locate action bar and logout confirmation markers in: %s", content)
	}
	if got < 1 {
		t.Fatalf("blank lines between action bar and logout confirmation = %d, want >= 1; got: %s", got, content)
	}

	updated, _ = h.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	h = updated.(HomeModel)

	if h.confirm.active {
		t.Fatal("expected n to cancel logout confirmation")
	}
}

// markersNotFound is returned by blankLinesBetween when either marker is
// missing (or out of order), distinguishing a lookup failure from a real
// count of zero blank lines.
const markersNotFound = -1

func blankLinesBetween(content, from, to string) int {
	plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(content, "")
	lines := strings.Split(plain, "\n")
	fromIdx, toIdx := -1, -1
findTarget:
	for i, line := range lines {
		switch {
		case fromIdx == -1 && strings.Contains(line, from):
			fromIdx = i
		case fromIdx != -1 && strings.Contains(line, to):
			toIdx = i
			break findTarget
		}
	}
	if fromIdx == -1 || toIdx == -1 || toIdx <= fromIdx {
		return markersNotFound
	}
	count := 0
	for _, line := range lines[fromIdx+1 : toIdx] {
		if strings.TrimSpace(line) == "" {
			count++
		}
	}
	return count
}

func lineBeforeMarkerIsBlank(content, marker string) bool {
	plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(content, "")
	lines := strings.Split(plain, "\n")
	for i, line := range lines {
		if strings.Contains(line, marker) {
			return i > 0 && strings.TrimSpace(lines[i-1]) == ""
		}
	}
	return false
}

func TestHomeViewDoesNotOfferHistoryAction(t *testing.T) {
	h := HomeModel{
		ctx:       context.Background(),
		repoCount: 1,
		loading:   false,
		sessions:  []*pb.Session{{Id: "sess-1"}},
	}
	h.buildTableRows()

	content := h.View().Content
	if strings.Contains(content, "[h]istory") {
		t.Fatalf("home action bar offered [h]istory, want removed; got: %s", content)
	}
}

func TestApplyMergedOptimisticOverride(t *testing.T) {
	passing := pb.DisplayStatus_DISPLAY_STATUS_PASSING
	merged := pb.DisplayStatus_DISPLAY_STATUS_MERGED
	closed := pb.DisplayStatus_DISPLAY_STATUS_CLOSED

	tests := []struct {
		name          string
		trackedID     string
		serverStatus  pb.DisplayStatus
		wantStatus    pb.DisplayStatus
		wantTrackedID string
	}{
		{
			name:          "no tracked id is a no-op",
			trackedID:     "",
			serverStatus:  passing,
			wantStatus:    passing,
			wantTrackedID: "",
		},
		{
			name:          "overrides passing while webhook is in flight",
			trackedID:     "s1",
			serverStatus:  passing,
			wantStatus:    merged,
			wantTrackedID: "s1",
		},
		{
			name:          "clears override once server reports merged",
			trackedID:     "s1",
			serverStatus:  merged,
			wantStatus:    merged,
			wantTrackedID: "",
		},
		{
			name:          "clears override once server reports closed",
			trackedID:     "s1",
			serverStatus:  closed,
			wantStatus:    closed,
			wantTrackedID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &pb.Session{Id: "s1", DisplayStatus: tt.serverStatus}
			h := HomeModel{
				sessions:           []*pb.Session{sess},
				mergedOptimisticID: tt.trackedID,
			}
			h.applyMergedOptimisticOverride()
			if got := sess.DisplayStatus; got != tt.wantStatus {
				t.Errorf("session DisplayStatus = %v, want %v", got, tt.wantStatus)
			}
			if h.mergedOptimisticID != tt.wantTrackedID {
				t.Errorf("mergedOptimisticID = %q, want %q", h.mergedOptimisticID, tt.wantTrackedID)
			}
		})
	}
}

func TestArchivingOverrideClearedWhenSessionGone(t *testing.T) {
	// A successful archive stays overridden until its own row disappears.
	h := NewHomeModel(nil, context.Background(), nil)
	h.markArchiving("s1")
	h.markArchiving("s2")
	h.resolveArchive("s1", nil)

	model, _ := h.Update(sessionListMsg{
		sessions: []*pb.Session{{Id: "s2"}},
	})
	got := model.(HomeModel)

	if got.isArchiving("s1") || got.archiveInFlight("s1") {
		t.Fatal("expected s1 archive state cleared when s1 is absent")
	}
	if !got.isArchiving("s2") || !got.archiveInFlight("s2") {
		t.Fatal("expected s2 archive state retained when s2 remains")
	}
}

func TestRenderSessionStatusConcurrentArchivingOverrides(t *testing.T) {
	h := HomeModel{
		archivingOverrideIDs: map[string]struct{}{"s1": {}, "s2": {}},
		spinner:              newStatusSpinner(),
	}

	for _, id := range []string{"s1", "s2"} {
		got := h.renderSessionStatus(&pb.Session{Id: id, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING})
		if !strings.Contains(got, "archiving") {
			t.Fatalf("renderSessionStatus for archiving session %s = %q, want to contain %q", id, got, "archiving")
		}
	}

	// Other sessions should pass through to renderDisplayStatus.
	other := &pb.Session{
		Id:            "s3",
		DisplayLabel:  "✓ passing",
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
	}
	want := renderDisplayStatus(other, h.spinner)
	if got2 := h.renderSessionStatus(other); got2 != want {
		t.Fatalf("renderSessionStatus for non-archiving session = %q, want passthrough %q", got2, want)
	}
}

func TestArchiveStateTransitionsPreserveInFlightSubset(t *testing.T) {
	tests := []struct {
		name             string
		transitions      []func(*HomeModel)
		wantArchiving    []string
		wantNotArchiving []string
		wantInFlight     []string
	}{
		{
			name: "multiple archives start independently",
			transitions: []func(*HomeModel){
				func(h *HomeModel) { h.markArchiving("s1") },
				func(h *HomeModel) { h.markArchiving("s2") },
			},
			wantArchiving: []string{"s1", "s2"},
			wantInFlight:  []string{"s1", "s2"},
		},
		{
			name: "successful archive retains only rendering override",
			transitions: []func(*HomeModel){
				func(h *HomeModel) { h.markArchiving("s1") },
				func(h *HomeModel) { h.markArchiving("s2") },
				func(h *HomeModel) { h.resolveArchive("s1", nil) },
			},
			wantArchiving: []string{"s1", "s2"},
			wantInFlight:  []string{"s2"},
		},
		{
			name: "failed archive drops only its own state",
			transitions: []func(*HomeModel){
				func(h *HomeModel) { h.markArchiving("s1") },
				func(h *HomeModel) { h.markArchiving("s2") },
				func(h *HomeModel) { h.resolveArchive("s1", errors.New("boom")) },
			},
			wantArchiving:    []string{"s2"},
			wantNotArchiving: []string{"s1"},
			wantInFlight:     []string{"s2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			for _, transition := range tt.transitions {
				transition(&h)
				for id := range h.archiveInFlightIDs {
					if !h.isArchiving(id) {
						t.Fatalf("archiveInFlightIDs contains %q outside archivingOverrideIDs", id)
					}
				}
			}
			for _, id := range tt.wantArchiving {
				if !h.isArchiving(id) {
					t.Fatalf("isArchiving(%q) = false, want true", id)
				}
			}
			for _, id := range tt.wantNotArchiving {
				if h.isArchiving(id) {
					t.Fatalf("isArchiving(%q) = true, want false", id)
				}
			}
			for _, id := range tt.wantInFlight {
				if !h.archiveInFlight(id) {
					t.Fatalf("archiveInFlight(%q) = false, want true", id)
				}
			}
		})
	}
}

func TestViewEmptyStateWithRepos(t *testing.T) {
	// Create a HomeModel with no sessions but repos exist
	h := HomeModel{
		ctx:       context.Background(),
		loading:   false,
		sessions:  []*pb.Session{},
		repoCount: 2,
	}

	// Render the view
	view := h.View()
	content := view.Content

	// Check for simplified guidance
	if !strings.Contains(content, "no active sessions") {
		t.Errorf("expected 'no active sessions' message when repos exist, got: %s", content)
	}

	// Check for new-session prompt
	if !strings.Contains(content, "Press 'n' to create a new session") {
		t.Errorf("expected new-session prompt when repos exist, got: %s", content)
	}

	// Should NOT show welcome message when repos exist
	if strings.Contains(content, "Welcome to Bossanova") {
		t.Errorf("should not show welcome message when repos exist, got: %s", content)
	}
}

func TestHomeUpgradeBannerRenders(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	h.loading = false
	h.repoCount = 1

	model, _ := h.Update(upgradeCheckMsg{
		current:   "v1.2.3",
		latest:    "v1.2.4",
		available: true,
	})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Upgrade available") {
		t.Fatalf("expected upgrade banner, got: %s", content)
	}
	if !strings.Contains(content, "v1.2.4") {
		t.Fatalf("expected latest version in upgrade banner, got: %s", content)
	}
	if !strings.Contains(content, "[u]pgrade [d]ismiss") {
		t.Fatalf("expected upgrade actions in banner, got: %s", content)
	}
	if strings.Contains(content, "u upgrade  U dismiss") {
		t.Fatalf("rendered old upgrade actions, got: %s", content)
	}
	if got := blankLinesBetween(content, "[q]uit", "Upgrade available"); got == markersNotFound {
		t.Fatalf("upgrade banner did not render after bottom action bar, got: %s", content)
	}
}

func TestHomeUpgradeKeyShowsBusySpinnerAndHidesActions(t *testing.T) {
	oldRunUpgradeCmd := runUpgradeCmd
	defer func() { runUpgradeCmd = oldRunUpgradeCmd }()

	runUpgradeCmd = func(string) tea.Cmd {
		return func() tea.Msg { return upgradeRunMsg{} }
	}

	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	h.loading = false
	h.repoCount = 1
	h.upgradeAvailable = true
	h.upgradeCurrent = "v1.2.3"
	h.upgradeLatest = "v1.2.4"

	model, cmd := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if cmd == nil {
		t.Fatal("upgrade key returned nil command")
	}
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Upgrading") {
		t.Fatalf("expected upgrading spinner state, got: %s", content)
	}
	if strings.Contains(content, "  Upgrading") {
		t.Fatalf("expected exactly one space between the spinner and the upgrading label, got a double space: %s", content)
	}
	if !lineBeforeMarkerIsBlank(content, "Upgrading") {
		t.Fatalf("expected blank line immediately above upgrading spinner, got: %s", content)
	}
	for _, hidden := range []string{"[u]pgrade [d]ismiss", "[n]ew session", "[q]uit"} {
		if strings.Contains(content, hidden) {
			t.Fatalf("expected %q hidden while upgrading, got: %s", hidden, content)
		}
	}

	model, _ = h.Update(upgradeRunMsg{})
	h = model.(HomeModel)
	if h.upgrading {
		t.Fatal("upgrading flag not cleared after upgrade result")
	}
}

func TestHomeQuitAllowedWhileBusy(t *testing.T) {
	tests := []struct {
		name       string
		upgrading  bool
		restarting bool
	}{
		{name: "upgrading", upgrading: true},
		{name: "restarting", restarting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width = 100
			h.loading = false
			h.repoCount = 1
			h.upgrading = tt.upgrading
			h.restarting = tt.restarting

			_, cmd := h.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
			if cmd == nil {
				t.Fatal("q returned nil command while busy")
			}
			msg := cmd()
			if _, ok := msg.(tea.QuitMsg); !ok {
				t.Fatalf("q returned %T, want tea.QuitMsg", msg)
			}
		})
	}
}

func TestHomeUpgradeAndCloudPromptsRenderAfterActionBar(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.width = 100
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.upgradeAvailable = true
	h.upgradeCurrent = "v1.2.3"
	h.upgradeLatest = "v1.2.4"
	h.cloudStatus = &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CanCreateCheckout: true,
	}

	content := h.View().Content
	if got := blankLinesBetween(content, "[q]uit", "Upgrade available"); got == markersNotFound {
		t.Fatalf("upgrade prompt did not render after bottom action bar, got: %s", content)
	}
	if got := blankLinesBetween(content, "Upgrade available", "Upgrade boss first"); got == markersNotFound {
		t.Fatalf("cloud prompt did not render after upgrade prompt, got: %s", content)
	}
}

func TestHomeUpgradeSuccessPromptsRestart(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	h.loading = false
	h.repoCount = 1

	model, _ := h.Update(upgradeRunMsg{})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Upgrade installed") {
		t.Fatalf("expected upgrade-installed prompt, got: %s", content)
	}
	if !strings.Contains(content, "[r]estart [esc] later") {
		t.Fatalf("expected restart action in prompt, got: %s", content)
	}
	if strings.Contains(content, "r restart  n later") {
		t.Fatalf("rendered old restart actions, got: %s", content)
	}
	if got := blankLinesBetween(content, "[q]uit", "Upgrade installed"); got == markersNotFound {
		t.Fatalf("restart prompt did not render after bottom action bar, got: %s", content)
	}
	// The reviewer specifically asked for a relaunch hint; ensure the
	// running TUI does not silently keep using the old binary.
	if !strings.Contains(content, "Quit boss") {
		t.Fatalf("expected 'Quit boss' relaunch hint, got: %s", content)
	}
}

func TestHomeUpgradeAfterRestartTellsUserToRelaunch(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	model, _ := h.Update(upgradeRunMsg{})
	h = model.(HomeModel)
	model, _ = h.Update(daemonRestartMsg{})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "re-launch") {
		t.Fatalf("expected re-launch hint after restart, got: %s", content)
	}
}

func TestHomeRestartKeyShowsBusySpinnerAndHidesActions(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	h.loading = false
	h.repoCount = 1
	h.restartPrompt = true

	model, cmd := h.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	if cmd == nil {
		t.Fatal("restart key returned nil command")
	}
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Restarting daemon") {
		t.Fatalf("expected restarting spinner state, got: %s", content)
	}
	if strings.Contains(content, "  Restarting daemon") {
		t.Fatalf("expected exactly one space between the spinner and the restarting label, got a double space: %s", content)
	}
	if !lineBeforeMarkerIsBlank(content, "Restarting daemon") {
		t.Fatalf("expected blank line immediately above restarting spinner, got: %s", content)
	}
	for _, hidden := range []string{"[r]estart [esc] later", "[n]ew session", "[q]uit"} {
		if strings.Contains(content, hidden) {
			t.Fatalf("expected %q hidden while restarting, got: %s", hidden, content)
		}
	}

	model, _ = h.Update(daemonRestartMsg{})
	h = model.(HomeModel)
	if h.restarting {
		t.Fatal("restarting flag not cleared after restart result")
	}
}

func TestHomeRestartIgnoresPollFailuresUntilSuccess(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	h.loading = false
	h.repoCount = 1
	h.sessions = []*pb.Session{{Id: "s1", Title: "work"}}
	h.pollFailures = pollFailureThreshold - 1
	h.restarting = true
	h.buildTableRows()

	for i := 0; i < pollFailureThreshold; i++ {
		model, _ := h.Update(sessionListMsg{err: errors.New("dial unix: connection refused")})
		h = model.(HomeModel)
	}
	if h.pollFailures != pollFailureThreshold-1 {
		t.Fatalf("pollFailures = %d, want unchanged while restarting", h.pollFailures)
	}
	if h.err != nil {
		t.Fatalf("err = %v, want nil while restarting", h.err)
	}
	if strings.Contains(h.View().Content, "Cannot connect to daemon") {
		t.Fatal("daemon error screen shown while restarting")
	}

	model, _ := h.Update(daemonRestartMsg{})
	h = model.(HomeModel)

	if h.restarting {
		t.Fatal("restarting flag not cleared after restart result")
	}
	if h.pollFailures != 0 {
		t.Fatalf("pollFailures = %d, want 0 after successful restart", h.pollFailures)
	}
	if h.err != nil {
		t.Fatalf("err not cleared after successful restart: %v", h.err)
	}
	if h.daemonRemediation != "" {
		t.Fatalf("daemonRemediation = %q, want empty after successful restart", h.daemonRemediation)
	}
	if strings.Contains(h.View().Content, "Cannot connect to daemon") {
		t.Fatal("daemon error screen still shown after successful restart")
	}
}

func TestRestartDaemonCmdWaitsForSocketReachable(t *testing.T) {
	oldRestartDaemon := restartDaemon
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		restartDaemon = oldRestartDaemon
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	restartDaemon = func() error { return nil }
	runBossDaemonRestart = func() error {
		t.Fatal("runBossDaemonRestart called for installed daemon")
		return nil
	}
	daemonGetStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: true}, nil }
	defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	restartPollInterval = time.Nanosecond
	restartWaitTimeout = time.Second
	attempts := 0
	daemonSocketReachable = func(path string) bool {
		if path != "/tmp/bossd.sock" {
			t.Fatalf("socket path = %q, want /tmp/bossd.sock", path)
		}
		attempts++
		return attempts >= 3
	}

	msg, ok := restartDaemonCmd()().(daemonRestartMsg)
	if !ok {
		t.Fatalf("restartDaemonCmd returned %T, want daemonRestartMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("restartDaemonCmd error = %v, want nil", msg.err)
	}
	if attempts != 3 {
		t.Fatalf("socket probe attempts = %d, want 3", attempts)
	}
}

func TestRestartDaemonCmdWaitsForOldSocketToStopBeforeReachable(t *testing.T) {
	oldRestartDaemon := restartDaemon
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		restartDaemon = oldRestartDaemon
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	restartDaemon = func() error { return nil }
	runBossDaemonRestart = func() error {
		t.Fatal("runBossDaemonRestart called for installed daemon")
		return nil
	}
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 1234}, nil
	}
	defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	restartPollInterval = time.Nanosecond
	restartWaitTimeout = time.Second
	attempts := 0
	daemonSocketReachable = func(path string) bool {
		if path != "/tmp/bossd.sock" {
			t.Fatalf("socket path = %q, want /tmp/bossd.sock", path)
		}
		attempts++
		switch attempts {
		case 1:
			return true // pre-restart socket was reachable
		case 2, 3:
			return true // old bossd still accepting after restartDaemon returns
		case 4:
			return false
		default:
			return true
		}
	}

	msg, ok := restartDaemonCmd()().(daemonRestartMsg)
	if !ok {
		t.Fatalf("restartDaemonCmd returned %T, want daemonRestartMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("restartDaemonCmd error = %v, want nil", msg.err)
	}
	if attempts != 5 {
		t.Fatalf("socket probe attempts = %d, want 5", attempts)
	}
}

func TestRestartDaemonCmdUsesCLIPathForStandaloneDaemon(t *testing.T) {
	oldRestartDaemon := restartDaemon
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		restartDaemon = oldRestartDaemon
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	platformRestartCalled := false
	restartDaemon = func() error {
		platformRestartCalled = true
		return nil
	}
	cliRestartCalled := false
	runBossDaemonRestart = func() error {
		cliRestartCalled = true
		return nil
	}
	daemonGetStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: false}, nil }
	defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	daemonSocketReachable = func(path string) bool {
		if path != "/tmp/bossd.sock" {
			t.Fatalf("socket path = %q, want /tmp/bossd.sock", path)
		}
		return cliRestartCalled
	}
	restartPollInterval = time.Nanosecond
	restartWaitTimeout = time.Second

	msg := restartDaemonCmd()().(daemonRestartMsg)
	if msg.err != nil {
		t.Fatalf("restartDaemonCmd error = %v, want nil", msg.err)
	}
	if !cliRestartCalled {
		t.Fatal("runBossDaemonRestart was not called for standalone daemon")
	}
	if platformRestartCalled {
		t.Fatal("restartDaemon called for standalone daemon")
	}
}

func TestRestartDaemonCmdTimesOutWithStatusHint(t *testing.T) {
	oldRestartDaemon := restartDaemon
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		restartDaemon = oldRestartDaemon
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	restartDaemon = func() error { return nil }
	runBossDaemonRestart = func() error {
		t.Fatal("runBossDaemonRestart called for installed daemon")
		return nil
	}
	daemonGetStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: true}, nil }
	defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	daemonSocketReachable = func(string) bool { return false }
	restartPollInterval = time.Nanosecond
	restartWaitTimeout = time.Nanosecond

	msg := restartDaemonCmd()().(daemonRestartMsg)
	if msg.err == nil {
		t.Fatal("restartDaemonCmd error = nil, want timeout")
	}
	if !strings.Contains(msg.err.Error(), "boss daemon status") {
		t.Fatalf("timeout error missing status hint: %v", msg.err)
	}
}

func TestHomeUpgradeFailureRendersError(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	model, _ := h.Update(upgradeRunMsg{err: errors.New("checksum mismatch")})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "checksum mismatch") {
		t.Fatalf("expected upgrade error, got: %s", content)
	}
}

func TestHomeUpgradeCheckFailureIsSilent(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	model, _ := h.Update(upgradeCheckMsg{err: errors.New("offline")})
	h = model.(HomeModel)

	content := h.View().Content
	if strings.Contains(content, "Upgrade:") {
		t.Fatalf("passive upgrade check error rendered upgrade error banner: %s", content)
	}
	if strings.Contains(content, "offline") {
		t.Fatalf("passive upgrade check error rendered raw error: %s", content)
	}
}

func TestHomeUpgradeCheckRateLimitShowsFriendlyBanner(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	model, _ := h.Update(upgradeCheckMsg{err: &upgrade.RateLimitError{Resets: time.Now().Add(30 * time.Minute)}})
	h = model.(HomeModel)

	content := h.View().Content
	for _, want := range []string{"rate limit", "resets at", "gh auth login"} {
		if !strings.Contains(content, want) {
			t.Fatalf("rate-limit upgrade banner missing %q, got: %s", want, content)
		}
	}
	if strings.Contains(content, "HTTP 403") {
		t.Fatalf("rate-limit banner leaked raw HTTP 403: %s", content)
	}
}

func TestHomePollFailureDebounce(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	// Seed a successful poll with one session (the last-good list).
	model, _ := h.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", Title: "work"}}})
	h = model.(HomeModel)

	// A single failed poll must NOT show the error screen (debounced).
	model, _ = h.Update(sessionListMsg{err: errors.New("dial unix: connection refused")})
	h = model.(HomeModel)
	if strings.Contains(h.View().Content, "Cannot connect to daemon") {
		t.Fatal("error screen shown after a single poll failure; want debounced")
	}

	// Reaching the consecutive-failure threshold surfaces the error screen.
	for h.pollFailures < pollFailureThreshold {
		model, _ = h.Update(sessionListMsg{err: errors.New("dial unix: connection refused")})
		h = model.(HomeModel)
	}
	if !strings.Contains(h.View().Content, "Cannot connect to daemon") {
		t.Fatalf("error screen not shown after %d consecutive failures", pollFailureThreshold)
	}

	// A successful poll clears both the error and the failure streak.
	model, _ = h.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", Title: "work"}}})
	h = model.(HomeModel)
	if h.err != nil {
		t.Fatalf("err not cleared after successful poll: %v", h.err)
	}
	if h.pollFailures != 0 {
		t.Fatalf("pollFailures = %d, want 0 after success", h.pollFailures)
	}
	if strings.Contains(h.View().Content, "Cannot connect to daemon") {
		t.Fatal("error screen still shown after successful poll")
	}
}

func TestHomeDaemonDownRemediationUsesStaticDaemonCommands(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	for h.pollFailures < pollFailureThreshold {
		model, _ := h.Update(sessionListMsg{err: errors.New("dial unix: connection refused")})
		h = model.(HomeModel)
	}

	content := h.View().Content
	for _, want := range []string{"boss daemon restart", "boss daemon status", "boss daemon install", "bossd"} {
		if !strings.Contains(content, want) {
			t.Fatalf("daemon-down remediation missing %q: %s", want, content)
		}
	}
}

// TestHomeDaemonDownRemediationHasBlankLineBeforeTry pins the BOS-375 layout: the
// daemon-unreachable screen must render exactly one blank line between the
// "Cannot connect to daemon (<err>)" line and the "Try:" hint. It fails if the
// separator regresses from "\n\n" to a single "\n".
func TestHomeDaemonDownRemediationHasBlankLineBeforeTry(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	for h.pollFailures < pollFailureThreshold {
		model, _ := h.Update(sessionListMsg{err: errors.New("dial unix: connection refused")})
		h = model.(HomeModel)
	}

	content := stripANSI(h.View().Content)
	if !strings.Contains(content, "Cannot connect to daemon") {
		t.Fatalf("daemon-down screen not rendered: %s", content)
	}

	lines := strings.Split(content, "\n")
	tryIdx := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "Try:" {
			tryIdx = i
			break
		}
	}
	if tryIdx < 0 {
		t.Fatalf("no %q line found in daemon-down screen: %s", "Try:", content)
	}
	// The line immediately above "Try:" must be blank (the inserted blank line).
	if tryIdx == 0 || strings.TrimSpace(lines[tryIdx-1]) != "" {
		got := ""
		if tryIdx > 0 {
			got = lines[tryIdx-1]
		}
		t.Fatalf("expected a blank line immediately before %q, got %q: %s", "Try:", got, content)
	}
	// And the daemon error line must sit directly above that blank line, so the
	// separation is exactly one blank line (guards the "\n\n" join, not more/less).
	if tryIdx < 2 || !strings.Contains(lines[tryIdx-2], "Cannot connect to daemon") {
		t.Fatalf("expected the daemon error line above the blank line before %q: %s", "Try:", content)
	}
}

func TestHomeUpgradeCheckSkipsInvalidBuildVersions(t *testing.T) {
	for _, version := range []string{"dev", "not-semver"} {
		t.Run(version, func(t *testing.T) {
			called := false
			cmd := checkUpgradeCmdForVersion(context.Background(), version, func(context.Context, string) (upgrade.CheckResult, error) {
				called = true
				return upgrade.CheckResult{Available: true}, nil
			})

			msg, ok := cmd().(upgradeCheckMsg)
			if !ok {
				t.Fatalf("cmd() returned %T, want upgradeCheckMsg", msg)
			}
			if called {
				t.Fatalf("checker called for invalid build version %q", version)
			}
			if msg.err != nil {
				t.Fatalf("invalid build version returned error: %v", msg.err)
			}
			if msg.available {
				t.Fatalf("invalid build version returned available upgrade")
			}
		})
	}
}

func TestHomeUpgradeDismissPersistsSnooze(t *testing.T) {
	oldCachePath := upgradeCachePath
	oldNow := upgradeNow
	defer func() {
		upgradeCachePath = oldCachePath
		upgradeNow = oldNow
	}()

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	upgradeCachePath = func() string { return cachePath }
	pinned := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	upgradeNow = func() time.Time { return pinned }

	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100
	model, _ := h.Update(upgradeCheckMsg{
		current:   "v1.2.3",
		latest:    "v1.2.4",
		url:       "https://example.test/release",
		available: true,
	})
	h = model.(HomeModel)
	model, _ = h.Update(tea.KeyPressMsg{Code: 'U', Text: "U"})
	h = model.(HomeModel)
	if !h.upgradeAvailable {
		t.Fatal("upgradeAvailable = false after uppercase U, want true")
	}

	model, _ = h.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	h = model.(HomeModel)

	if h.upgradeAvailable {
		t.Fatal("upgradeAvailable = true after dismiss, want false")
	}

	entry, ok, err := upgrade.ReadCache(cachePath)
	if err != nil || !ok {
		t.Fatalf("ReadCache() = (_, %v, %v), want (entry, true, nil)", ok, err)
	}
	if entry.SnoozedVersion != "v1.2.4" {
		t.Fatalf("SnoozedVersion = %q, want v1.2.4", entry.SnoozedVersion)
	}
	if !entry.SnoozedUntil.After(pinned) {
		t.Fatalf("SnoozedUntil = %v, want after now (%v)", entry.SnoozedUntil, pinned)
	}
}

func TestHomeUpgradeCheckPreservesSnoozeAcrossRefresh(t *testing.T) {
	oldCachePath := upgradeCachePath
	oldNow := upgradeNow
	defer func() {
		upgradeCachePath = oldCachePath
		upgradeNow = oldNow
	}()

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	upgradeCachePath = func() string { return cachePath }
	pinned := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	upgradeNow = func() time.Time { return pinned }

	// Write a prior cache entry that is past its TTL but has an active
	// snooze for v1.2.4 that runs for another six days.
	if err := upgrade.WriteCache(cachePath, upgrade.CacheEntry{
		CheckedAt:      pinned.Add(-48 * time.Hour),
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/release",
		SnoozedVersion: "v1.2.4",
		SnoozedUntil:   pinned.Add(6 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("WriteCache() error = %v", err)
	}

	msg := checkUpgradeCmdForVersion(context.Background(), "v1.2.3", func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
			ReleaseURL:     "https://example.test/release",
			Available:      true,
		}, nil
	})().(upgradeCheckMsg)

	if msg.available {
		t.Fatal("fresh check reported available=true despite active snooze; snooze was dropped on cache refresh")
	}

	entry, ok, err := upgrade.ReadCache(cachePath)
	if err != nil || !ok {
		t.Fatalf("ReadCache() after refresh = (_, %v, %v), want preserved entry", ok, err)
	}
	if entry.SnoozedVersion != "v1.2.4" {
		t.Fatalf("SnoozedVersion after refresh = %q, want v1.2.4", entry.SnoozedVersion)
	}
}

func TestHomeUpgradeCheckUsesFreshCache(t *testing.T) {
	oldCachePath := upgradeCachePath
	oldNow := upgradeNow
	defer func() {
		upgradeCachePath = oldCachePath
		upgradeNow = oldNow
	}()

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	upgradeCachePath = func() string { return cachePath }
	upgradeNow = func() time.Time { return time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC) }

	calls := 0
	check := func(context.Context, string) (upgrade.CheckResult, error) {
		calls++
		return upgrade.CheckResult{
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
			ReleaseURL:     "https://example.test/stable",
			Available:      true,
		}, nil
	}

	first := checkUpgradeCmdForVersion(context.Background(), "v1.2.3", check)().(upgradeCheckMsg)
	second := checkUpgradeCmdForVersion(context.Background(), "v1.2.3", check)().(upgradeCheckMsg)

	if calls != 1 {
		t.Fatalf("checker calls = %d, want 1", calls)
	}
	if !first.available || !second.available || second.latest != "v1.2.4" {
		t.Fatalf("cached upgrade messages = first %+v second %+v, want available v1.2.4", first, second)
	}
}

func TestHomeNeighborSessionID(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "s1", Title: "first"},
		{Id: "s2", Title: "second"},
		{Id: "s3", Title: "third"},
	}

	cases := []struct {
		name      string
		sessions  []*pb.Session
		removedID string
		want      string
	}{
		{"middle highlights next", sessions, "s2", "s3"},
		{"first highlights next", sessions, "s1", "s2"},
		{"last highlights previous", sessions, "s3", "s2"},
		{"only session has no neighbor", sessions[:1], "s1", ""},
		{"unknown id has no neighbor", sessions, "missing", ""},
		{"empty list has no neighbor", nil, "s1", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.sessions = tc.sessions
			if got := h.neighborSessionID(tc.removedID); got != tc.want {
				t.Fatalf("neighborSessionID(%q) = %q, want %q", tc.removedID, got, tc.want)
			}
		})
	}
}

// TestHomeArchiveKeepsCursorPosition verifies the end-to-end behaviour: after a
// session is archived the cursor should land on the session that takes its
// place rather than jumping back to the top of the list.
func TestValueDelivered(t *testing.T) {
	tests := []struct {
		name       string
		repoCount  int
		sessCount  int
		hasChat    bool
		wantResult bool
	}{
		{"nothing", 0, 0, false, false},
		{"repo only", 1, 0, false, false},
		{"repo and session no chat", 1, 1, false, false},
		{"repo and session and chat", 1, 1, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valueDelivered(tt.repoCount, tt.sessCount, tt.hasChat)
			if got != tt.wantResult {
				t.Errorf("valueDelivered(%d,%d,%v) = %v, want %v",
					tt.repoCount, tt.sessCount, tt.hasChat, got, tt.wantResult)
			}
		})
	}
}

func TestSessionsHaveChat(t *testing.T) {
	if sessionsHaveChat(nil) {
		t.Error("nil sessions: want false, got true")
	}
	if sessionsHaveChat([]*pb.Session{{HasActiveChat: false}}) {
		t.Error("all inactive: want false, got true")
	}
	if !sessionsHaveChat([]*pb.Session{{HasActiveChat: false}, {HasActiveChat: true}}) {
		t.Error("one active: want true, got false")
	}
}

func TestLatchValueDeliveredSetsOnce(t *testing.T) {
	now1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now2 := now1.Add(time.Hour)

	oldSave := saveSettings
	saveSettings = func(config.Settings) error { return nil }
	defer func() { saveSettings = oldSave }()

	h := HomeModel{
		ctx:       context.Background(),
		repoCount: 1,
		sessions:  []*pb.Session{{HasActiveChat: true}},
		settings:  config.DefaultSettings(),
		now:       func() time.Time { return now1 },
	}

	h.latchValueDeliveredIfNeeded()
	if h.settings.BossCloudValueDeliveredAt.IsZero() {
		t.Fatal("BossCloudValueDeliveredAt not set after latch")
	}
	if !h.settings.BossCloudValueDeliveredAt.Equal(now1.UTC()) {
		t.Fatalf("BossCloudValueDeliveredAt = %v, want %v", h.settings.BossCloudValueDeliveredAt, now1.UTC())
	}

	first := h.settings.BossCloudValueDeliveredAt
	h.now = func() time.Time { return now2 }
	h.latchValueDeliveredIfNeeded()
	if !h.settings.BossCloudValueDeliveredAt.Equal(first) {
		t.Fatalf("BossCloudValueDeliveredAt moved from %v to %v, want set-once", first, h.settings.BossCloudValueDeliveredAt)
	}
}

// runCmd executes a tea.Cmd and returns the resulting message, or nil if cmd is nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

// isRepoAddSwitch returns true if msg is a switchViewMsg routing to ViewRepoAdd
// with firstRepo=true.
func isRepoAddSwitch(msg tea.Msg) bool {
	svm, ok := msg.(switchViewMsg)
	return ok && svm.view == ViewRepoAdd && svm.firstRepo
}

// repoCountMsg must never auto-route the user anywhere. The zero-repo user is
// guided to add-repo via an explicit [enter] press from the home empty state,
// not by a silent redirect that covers the welcome screen with the form.
func TestRepoCountZeroDoesNotAutoRoute(t *testing.T) {
	oldSave := saveSettings
	saveSettings = func(config.Settings) error { return nil }
	defer func() { saveSettings = oldSave }()

	h := NewHomeModel(nil, context.Background(), nil)
	for _, count := range []int{0, 1, 2} {
		_, cmd := h.Update(repoCountMsg{count: count})
		if cmd != nil {
			t.Fatalf("repoCountMsg{%d}: got non-nil cmd %T, want no auto-route", count, runCmd(cmd))
		}
	}
}

// Pressing Enter on the zero-repo home empty state opens the add-repo wizard
// with firstRepo=true (so cancel returns to the home empty state).
func TestEnterOnZeroRepoEmptyStateOpensAddRepo(t *testing.T) {
	h := HomeModel{
		ctx:       context.Background(),
		loading:   false,
		sessions:  []*pb.Session{},
		repoCount: 0,
	}
	_, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on zero-repo empty state: got nil cmd, want a switch to ViewRepoAdd")
	}
	if !isRepoAddSwitch(runCmd(cmd)) {
		t.Fatalf("enter on zero-repo empty state: routing msg = %T, want switchViewMsg{view:ViewRepoAdd, firstRepo:true}", runCmd(cmd))
	}
}

func TestHomeArchiveKeepsCursorPosition(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "s1", Title: "first"},
		{Id: "s2", Title: "second"},
		{Id: "s3", Title: "third"},
	}

	t.Run("middle session keeps position", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.width = 100
		h.height = 40
		model, _ := h.Update(sessionListMsg{sessions: sessions})
		h = model.(HomeModel)

		// User is sitting on the middle session ("s2") and archives it. The app
		// highlights the neighbour that fills the gap.
		h.highlightSessionID = h.neighborSessionID("s2")

		// The reloaded list no longer contains the archived session.
		remaining := []*pb.Session{sessions[0], sessions[2]}
		model, _ = h.Update(sessionListMsg{sessions: remaining})
		h = model.(HomeModel)

		if got := h.selectedSessionID(); got != "s3" {
			t.Fatalf("after archiving s2, selected = %q, want s3", got)
		}
	})

	t.Run("last session falls back to previous", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.width = 100
		h.height = 40
		model, _ := h.Update(sessionListMsg{sessions: sessions})
		h = model.(HomeModel)

		h.highlightSessionID = h.neighborSessionID("s3")

		remaining := []*pb.Session{sessions[0], sessions[1]}
		model, _ = h.Update(sessionListMsg{sessions: remaining})
		h = model.(HomeModel)

		if got := h.selectedSessionID(); got != "s2" {
			t.Fatalf("after archiving last session s3, selected = %q, want s2", got)
		}
	})
}

// TestHomeSelectionStickyWhenSiblingRowRemoved covers the BOS-367 scenario: the
// one-shot highlightSessionID hand-off has already been consumed (the user
// returned to the list and arrowed onto a different session), then a later poll
// removes a *sibling* row above the cursor. The cursor must follow the selected
// session rather than holding its stale row number.
func TestHomeSelectionStickyWhenSiblingRowRemoved(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "A", Title: "alpha"},
		{Id: "B", Title: "bravo"},
		{Id: "C", Title: "charlie"},
		{Id: "D", Title: "delta"},
	}

	t.Run("sibling above cursor removed keeps selection", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.width = 100
		h.height = 40
		model, _ := h.Update(sessionListMsg{sessions: sessions})
		h = model.(HomeModel)

		// Move the cursor onto C (no highlight set — it was already consumed).
		h.table.SetCursor(h.tableCursorForSessionIndex(2))
		if got := h.selectedSessionID(); got != "C" {
			t.Fatalf("setup: selected = %q, want C", got)
		}

		// A later poll removes the archiving sibling B (above the cursor).
		remaining := []*pb.Session{sessions[0], sessions[2], sessions[3]}
		model, _ = h.Update(sessionListMsg{sessions: remaining})
		h = model.(HomeModel)

		if got := h.selectedSessionID(); got != "C" {
			t.Fatalf("after removing sibling B, selected = %q, want C", got)
		}
	})

	t.Run("selected session removed falls back sanely", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.width = 100
		h.height = 40
		model, _ := h.Update(sessionListMsg{sessions: sessions})
		h = model.(HomeModel)

		// Select C, then a poll removes C itself with no highlight set.
		h.table.SetCursor(h.tableCursorForSessionIndex(2))
		if got := h.selectedSessionID(); got != "C" {
			t.Fatalf("setup: selected = %q, want C", got)
		}

		remaining := []*pb.Session{sessions[0], sessions[1], sessions[3]}
		model, _ = h.Update(sessionListMsg{sessions: remaining})
		h = model.(HomeModel)

		// Fallback (normalizeTableCursor): cursor must stay in range on a
		// surviving session and must not panic.
		got := h.selectedSessionID()
		if got == "" {
			t.Fatalf("after removing selected C, selection is empty; want a surviving session")
		}
		switch got {
		case "A", "B", "D":
		default:
			t.Fatalf("after removing selected C, selected = %q, want one of A/B/D", got)
		}
	})
}

// blueSelectedSGR is the bold + selection-blue (#4CA7F8) open SGR that
// pre-styled selected-row cells carry (see renderSelectedText in status.go).
const blueSelectedSGR = "\x1b[1;38;2;76;167;248m"

func TestBuildTableRows_SelectedAttentionRowIsBlue(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{
		{
			Id:              "s-attn",
			RepoDisplayName: "repo-attn",
			Title:           "Attention session",
			AttentionStatus: &pb.AttentionStatus{NeedsAttention: true},
		},
		{
			Id:              "s-plain",
			RepoDisplayName: "repo-plain",
			Title:           "Plain session",
		},
	}
	h.buildTableRows()
	// Put the cursor on the attention session (row 0) and restyle.
	h.table.SetCursor(h.tableCursorForSessionIndex(0))
	h.buildTableRows()

	rows := h.table.Rows()
	// Locate the attention session row by its repo display name (col 2 carries
	// the selection-blue open code; visible text is still present).
	var selAttn, selRepo, selName string
	for _, r := range rows {
		if strings.Contains(r[2], "repo-attn") {
			selAttn, selRepo, selName = r[1], r[2], r[3]
		}
	}
	if selRepo == "" {
		t.Fatalf("attention session row not found in %#v", rows)
	}
	if !strings.Contains(selRepo, blueSelectedSGR) {
		t.Errorf("selected attention repo cell not blue: %q", selRepo)
	}
	if !strings.Contains(selName, blueSelectedSGR) {
		t.Errorf("selected attention name cell not blue: %q", selName)
	}
	// The attention "!" indicator (col 1) keeps its own semantic warning/danger
	// color even when its row is selected — it must not be re-styled blue.
	if !strings.Contains(selAttn, "!") {
		t.Errorf("selected attention row should keep its ! indicator, got %q", selAttn)
	}
	if strings.Contains(selAttn, blueSelectedSGR) {
		t.Errorf("attention ! indicator should not be re-styled selection blue: %q", selAttn)
	}

	// The non-selected plain row must NOT be pre-styled blue.
	for _, r := range rows {
		if strings.Contains(r[2], "repo-plain") && strings.Contains(r[2], blueSelectedSGR) {
			t.Errorf("non-selected plain repo cell should not be pre-styled blue: %q", r[2])
		}
	}
}

func TestNavigation_RestylesSelectedRow(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{
		{Id: "s0", RepoDisplayName: "repo0", Title: "Zero"},
		{
			Id:              "s1",
			RepoDisplayName: "repo1",
			Title:           "One",
			AttentionStatus: &pb.AttentionStatus{NeedsAttention: true},
		},
	}
	h.buildTableRows()
	h.table.SetCursor(h.tableCursorForSessionIndex(0))
	h.buildTableRows()

	// Press down-arrow to move selection from session 0 to session 1.
	updated, _ := h.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	hm := updated.(HomeModel)

	var repo0, repo1 string
	for _, r := range hm.table.Rows() {
		if strings.Contains(r[2], "repo0") {
			repo0 = r[2]
		}
		if strings.Contains(r[2], "repo1") {
			repo1 = r[2]
		}
	}
	if !strings.Contains(repo1, blueSelectedSGR) {
		t.Errorf("after moving down, selected attention row repo not blue: %q", repo1)
	}
	if strings.Contains(repo0, blueSelectedSGR) {
		t.Errorf("after moving down, previously selected row repo should not be blue: %q", repo0)
	}
}

func TestHomeWarningRowsMapToSessionsAndNormalizeCursor(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{
		{
			Id:                     "warn",
			LastRepairAttemptCount: 2,
			LastRepairExitError:    "exit status 1",
			AttentionStatus: &pb.AttentionStatus{
				NeedsAttention: true,
				Summary:        "repair needs attention",
			},
		},
		{Id: "plain"},
	}

	rows := h.primarySessionRows()
	if len(rows) != 2 || rows[0] != 0 || rows[1] != 3 {
		t.Fatalf("primarySessionRows() = %v, want [0 3]", rows)
	}
	if got := h.tableDataRowCount(); got != 4 {
		t.Fatalf("tableDataRowCount() = %d, want 4", got)
	}

	for _, tt := range []struct {
		cursor int
		id     string
		ok     bool
	}{
		{cursor: -1},
		{cursor: 0, id: "warn", ok: true},
		{cursor: 1, id: "warn", ok: true},
		{cursor: 2, id: "warn", ok: true},
		{cursor: 3, id: "plain", ok: true},
		{cursor: 4},
	} {
		index, ok := h.sessionIndexForTableCursor(tt.cursor)
		if ok != tt.ok {
			t.Errorf("sessionIndexForTableCursor(%d) ok = %t, want %t", tt.cursor, ok, tt.ok)
			continue
		}
		if ok && h.sessions[index].GetId() != tt.id {
			t.Errorf("sessionIndexForTableCursor(%d) session = %q, want %q", tt.cursor, h.sessions[index].GetId(), tt.id)
		}
	}

	for _, tt := range []struct {
		index int
		want  int
	}{
		{index: 0, want: 0},
		{index: 1, want: 3},
		{index: 2, want: -1},
	} {
		if got := h.tableCursorForSessionIndex(tt.index); got != tt.want {
			t.Errorf("tableCursorForSessionIndex(%d) = %d, want %d", tt.index, got, tt.want)
		}
	}

	h.buildTableRows()
	for _, tt := range []struct {
		cursor   int
		previous int
		want     int
	}{
		{cursor: 1, previous: 0, want: 3},
		{cursor: 2, previous: 3, want: 0},
		{cursor: 2, previous: 2, want: 0},
	} {
		h.table.SetCursor(tt.cursor)
		h.normalizeTableCursor(tt.previous)
		if got := h.table.Cursor(); got != tt.want {
			t.Errorf("normalizeTableCursor(%d) from %d = %d, want %d", tt.previous, tt.cursor, got, tt.want)
		}
	}
}

func TestHomeCloudStatusTransitionsStartAndContinuePolling(t *testing.T) {
	t.Run("authenticated status check starts when no status is cached", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})

		updated, cmd := h.Update(authStatusMsg{loggedIn: true, email: "dave@example.com"})
		h = updated.(HomeModel)

		if !h.loggedIn || h.loggedInEmail != "dave@example.com" {
			t.Fatalf("auth state = (%t, %q), want (true, dave@example.com)", h.loggedIn, h.loggedInEmail)
		}
		if !h.cloudChecking {
			t.Fatal("authenticated status update did not start cloud status check")
		}
		if cmd == nil {
			t.Fatal("authenticated status update returned nil cloud status command")
		}
	})

	t.Run("checkout error with billing URL preserves recovery path", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})

		updated, cmd := h.Update(homeCloudCheckoutMsg{
			err: errors.New("checkout request failed"),
			url: "https://billing.example.test/session",
		})
		h = updated.(HomeModel)

		if got, want := h.cloudCheckoutStatus, "Open this billing URL: https://billing.example.test/session"; got != want {
			t.Fatalf("cloudCheckoutStatus = %q, want %q", got, want)
		}
		if !h.cloudCheckoutPolling || !h.cloudChecking {
			t.Fatalf("checkout retry flags = (polling=%t, checking=%t), want both true", h.cloudCheckoutPolling, h.cloudChecking)
		}
		if cmd == nil {
			t.Fatal("checkout error with billing URL returned nil refresh command")
		}
	})

	t.Run("tick refreshes pending authenticated cloud status", func(t *testing.T) {
		h := NewHomeModel(nil, context.Background(), nil)
		h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
		h.loggedIn = true
		h.cloudStatus = &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH}

		updated, cmd := h.Update(tickMsg{})
		h = updated.(HomeModel)

		if !h.cloudChecking {
			t.Fatal("tick did not start pending cloud status refresh")
		}
		if cmd == nil {
			t.Fatal("tick returned nil batch command")
		}
	})
}
