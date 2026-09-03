package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/fixtures"
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

// A caller that loses one of CreateCheckoutSession's compare-and-swap claims to a
// concurrent caller is refused with copy that shares no phrase with the two
// refusals below (server const cloudCheckoutContendedMessage). That is
// load-bearing, not cosmetic: this account sits in checkout_started with a live
// session the winner just created, and both prose-matched branches start polling
// the cloud access status — the poll that promotes a checkout_started account
// into the stuck entitlement_pending state BOS-1076 exists to keep it out of. The
// loser must fall through to the plain error path, from which a retry resumes the
// winner's session.
func TestHomeCloudCheckoutContentionDoesNotPoll(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	abandoned := &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CheckoutStarted:   true,
		CanCreateCheckout: true,
	}
	h.cloudStatus = abandoned

	updated, _ := h.Update(homeCloudCheckoutMsg{
		err: connect.NewError(connect.CodeFailedPrecondition, errors.New("bossanova cloud checkout is already being created; retry to resume it")),
	})
	h = updated.(HomeModel)

	if h.cloudCheckoutPolling {
		t.Fatal("a contended checkout claim must not start polling: the poll is what re-poisons this account")
	}
	if h.cloudStatus.GetState() == pb.CloudAccessState_CLOUD_ACCESS_STATE_PENDING_ENTITLEMENT_REFRESH {
		t.Fatal("a contended checkout claim must not restate the account as pending entitlement")
	}
	if h.cloudStatus != abandoned {
		t.Fatalf("contended checkout claim replaced the status: %v", h.cloudStatus)
	}
}

// An abandoned checkout now reads as needs-subscription, so this user can reach
// the checkout action again — and the server answers a paying account with
// "already active" rather than a session URL. That refusal must re-read status,
// not tell a paying customer that cloud checkout is unavailable.
func TestHomeCloudCheckoutAlreadyActiveKeepsPolling(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{
		State:           pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CheckoutStarted: true,
	}

	updated, cmd := h.Update(homeCloudCheckoutMsg{
		err: connect.NewError(connect.CodeFailedPrecondition, errors.New("bossanova cloud is already active")),
	})
	h = updated.(HomeModel)

	content := h.View().Content
	if strings.Contains(content, "Cloud checkout unavailable") {
		t.Fatalf("an already-active refusal should not render checkout unavailable: %q", content)
	}
	// A paying customer must not be told their setup has not completed. Home
	// renders no Message field, so the reassurance has to reach a line it does
	// render, and the pending gate copy must not stand alongside it.
	if !strings.Contains(content, statusCloudAlreadyActive) {
		t.Fatalf("an already-active refusal should reassure the caller: %q", content)
	}
	if strings.Contains(content, "setup has not completed yet") {
		t.Fatalf("an already-active refusal should not render the pending gate line: %q", content)
	}
	if !h.cloudCheckoutPolling {
		t.Fatal("an already-active refusal should keep polling")
	}
	if cmd == nil {
		t.Fatal("an already-active refusal should re-read the cloud access status")
	}
}

// The provisioning failure shares the refusal code but is not a state the
// account can poll its way out of, so it must still reach the error path.
func TestHomeCloudCheckoutStripeCustomerRequiredStaysAnError(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudSubscription(&fakeHomeCloudAccessClient{}, "bossanova://billing/return", "bossanova://billing/cancel")
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.cloudStatus = &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION}

	updated, _ := h.Update(homeCloudCheckoutMsg{
		err: connect.NewError(connect.CodeFailedPrecondition, errors.New("stripe customer is required")),
	})
	h = updated.(HomeModel)

	if h.cloudCheckoutPolling {
		t.Fatal("a provisioning failure should not start polling")
	}
	if h.cloudCheckoutStatus != "Cloud checkout unavailable. Local sessions are still available." {
		t.Fatalf("cloudCheckoutStatus = %q, want the checkout-unavailable line", h.cloudCheckoutStatus)
	}
}

// Home is the surface the reported account was staring at, so the abandoned /
// activating split has to be visible here and not only in the subscription
// flow. The pair is the only thing that carries it: NEEDS_SUBSCRIPTION alone
// cannot tell an unfinished checkout from one that was never started.
func TestHomeCloudGateAbandonedCheckoutOffersResume(t *testing.T) {
	for _, tc := range []struct {
		name             string
		upgradeAvailable bool
		want             string
		notWant          string
	}{
		{name: "resume", want: statusCloudResumeCheckout, notWant: statusCloudNeedsSubscription},
		{name: "resume with upgrade", upgradeAvailable: true, want: statusCloudResumeCheckoutUpgrade, notWant: statusCloudNeedsSubscriptionUpgrade},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
			h.loading = false
			h.loggedIn = true
			h.repoCount = 1
			h.width = 200
			h.upgradeAvailable = tc.upgradeAvailable
			h.cloudStatus = &pb.CloudAccessStatus{
				State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
				CheckoutStarted:   true,
				CanCreateCheckout: true,
			}

			line := reflowStatusBlock(h.cloudGateLine())
			if line != tc.want {
				t.Fatalf("cloudGateLine() = %q, want %q", line, tc.want)
			}
			if line == tc.notWant {
				t.Fatalf("abandoned checkout rendered the never-started copy: %q", line)
			}
		})
	}
}

// The other half of the pair: checkout_started with can_create_checkout false
// means the user came back from Stripe and the entitlement is landing. That is
// a genuine wait, and it must keep the subscribe copy rather than inviting a
// resume the server would refuse.
func TestHomeCloudGateLandingEntitlementKeepsSubscribeCopy(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.width = 200
	h.cloudStatus = &pb.CloudAccessStatus{
		State:             pb.CloudAccessState_CLOUD_ACCESS_STATE_NEEDS_SUBSCRIPTION,
		CheckoutStarted:   true,
		CanCreateCheckout: false,
	}

	if line := reflowStatusBlock(h.cloudGateLine()); line != statusCloudNeedsSubscription {
		t.Fatalf("cloudGateLine() = %q, want %q", line, statusCloudNeedsSubscription)
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

	// "r" is deliberately absent from this list: BOS-837 brought it back to Home
	// as the hidden rename shortcut, with the same meaning it has on the chat
	// picker it was moved to. Its coverage is TestHomeConsumesTheHiddenRenameKey-
	// BeforeTheTable and the TestHomeRename* tests below.
	t.Run("moved keys are inert", func(t *testing.T) {
		for _, key := range []string{"t", "c", "a"} {
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

func TestHomeActionBarsKeepBugReportShortcutHidden(t *testing.T) {
	tests := []struct {
		name string
		home HomeModel
	}{
		{
			name: "no repositories",
			home: HomeModel{ctx: context.Background(), loading: false, repoCount: 0},
		},
		{
			name: "no sessions",
			home: HomeModel{ctx: context.Background(), loading: false, repoCount: 1},
		},
		{
			name: "session table",
			home: HomeModel{
				ctx:       context.Background(),
				loading:   false,
				repoCount: 1,
				sessions:  []*pb.Session{{Id: "session-1", Title: "Active work"}},
			},
		},
		{
			name: "daemon error",
			home: HomeModel{
				ctx:     context.Background(),
				loading: false,
				err:     errors.New("daemon unavailable"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.home
			h.buildTableRows()
			if got := h.View().Content; strings.Contains(got, "[ctrl+g] report a bug") {
				t.Fatalf("Home action bar exposes hidden Ctrl+G report action: %s", got)
			}
		})
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
	// The banner is a status line, so it wraps at statusWrapWidth (BOS-530) and
	// the two hints can land on different rows. Reflow before matching: this
	// asserts the hints are present, not that the line happens to fit on one
	// row.
	if !strings.Contains(reflowStatusBlock(content), "[u]pgrade [d]ismiss") {
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
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	runBossDaemonRestart = func() error { return nil }
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
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	runBossDaemonRestart = func() error { return nil }
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
			return true // old bossd still accepting after the restart command returns
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

func TestRestartDaemonCmdDoesNotWaitForSocketHandoffWithoutOldPID(t *testing.T) {
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
	}()

	runBossDaemonRestart = func() error { return nil }
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true}, nil
	}
	defaultSocketPath = func() (string, error) { return "/tmp/bossd.sock", nil }
	attempts := 0
	daemonSocketReachable = func(path string) bool {
		attempts++
		return true
	}

	msg, ok := restartDaemonCmd()().(daemonRestartMsg)
	if !ok {
		t.Fatalf("restartDaemonCmd returned %T, want daemonRestartMsg", msg)
	}
	if msg.err != nil {
		t.Fatalf("restartDaemonCmd error = %v, want nil", msg.err)
	}
	if attempts != 2 {
		t.Fatalf("socket probe attempts = %d, want 2", attempts)
	}
}

func TestRestartDaemonCmdUsesCLIPathForStandaloneDaemon(t *testing.T) {
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

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
}

func TestRunBossDaemonRestartUsesBossFromPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "restart.log")
	bossPath := filepath.Join(dir, "boss")
	if err := os.WriteFile(bossPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$BOSS_RESTART_LOG\"\n"), 0o755); err != nil {
		t.Fatalf("write boss executable: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("BOSS_RESTART_LOG", logPath)

	if err := runBossDaemonRestart(); err != nil {
		t.Fatalf("runBossDaemonRestart() error = %v", err)
	}
	output, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read restart log: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != "daemon restart" {
		t.Fatalf("boss args = %q, want %q", got, "daemon restart")
	}
}

func TestBossDaemonRestartExecutableFallsBackToRunningBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	want, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	got, err := bossDaemonRestartExecutable()
	if err != nil {
		t.Fatalf("bossDaemonRestartExecutable: %v", err)
	}
	if got != want {
		t.Fatalf("restart executable = %q, want running executable %q", got, want)
	}
}

func TestRestartDaemonCmdUsesCLIPathForInstalledDaemon(t *testing.T) {
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDaemonGetStatus := daemonGetStatus
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		daemonGetStatus = oldDaemonGetStatus
	}()

	cliRestartCalled := false
	runBossDaemonRestart = func() error {
		cliRestartCalled = true
		return nil
	}
	daemonGetStatus = func() (*daemon.Status, error) {
		return &daemon.Status{Installed: true, Running: true, PID: 1234}, nil
	}

	readiness, err := restartDaemonForStatus(true)
	if err != nil {
		t.Fatalf("restartDaemonForStatus error = %v, want nil", err)
	}
	if !cliRestartCalled {
		t.Fatal("runBossDaemonRestart was not called for installed daemon")
	}
	if !readiness.waitForSocketGone || readiness.oldPID != 1234 {
		t.Fatalf("restart readiness = %+v, want socket handoff from PID 1234", readiness)
	}
}

func TestRestartDaemonCmdTimesOutWithStatusHint(t *testing.T) {
	oldRunBossDaemonRestart := runBossDaemonRestart
	oldDefaultSocketPath := defaultSocketPath
	oldSocketReachable := daemonSocketReachable
	oldDaemonGetStatus := daemonGetStatus
	oldPollInterval := restartPollInterval
	oldWaitTimeout := restartWaitTimeout
	defer func() {
		runBossDaemonRestart = oldRunBossDaemonRestart
		defaultSocketPath = oldDefaultSocketPath
		daemonSocketReachable = oldSocketReachable
		daemonGetStatus = oldDaemonGetStatus
		restartPollInterval = oldPollInterval
		restartWaitTimeout = oldWaitTimeout
	}()

	runBossDaemonRestart = func() error { return nil }
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

// --- BOS-474: HTTP endpoint auxiliary rows on the home session list --------

func endpointSession(id string, ports ...uint32) *pb.Session {
	sess := &pb.Session{Id: id, Title: "Session " + id}
	for _, p := range ports {
		sess.HttpEndpoints = append(sess.HttpEndpoints, &pb.HttpEndpoint{
			Port: p,
			Url:  fmt.Sprintf("http://localhost:%d", p),
		})
	}
	return sess
}

// osc8Pattern matches an OSC 8 introducer or terminator (ESC ] 8 ; ; <url> ESC \).
var osc8Pattern = regexp.MustCompile("\x1b\\]8;;[^\x1b]*\x1b\\\\")

// visibleRowText strips SGR and OSC 8 bytes from a table row's NAME cell so a
// test can assert on what the operator actually sees.
func visibleRowText(cell string) string {
	return stripANSI(osc8Pattern.ReplaceAllString(cell, ""))
}

// TestHomeEndpointRowAccounting pins the centralized auxiliary-row accounting:
// row construction, height, cursor→session mapping, and session→cursor mapping
// must all agree that an endpoint row precedes any warning rows.
func TestHomeEndpointRowAccounting(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	withBoth := endpointSession("both", 3000, 5173)
	withBoth.SetupError = "setup script blew up"
	h.sessions = []*pb.Session{
		endpointSession("eps", 3000, 5173), // primary + endpoint row
		withBoth,                           // primary + endpoint row + warning row
		{Id: "plain", Title: "Plain"},      // primary only
	}

	if got, want := h.primarySessionRows(), []int{0, 2, 5}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("primarySessionRows() = %v, want %v", got, want)
	}
	if got := h.tableDataRowCount(); got != 6 {
		t.Fatalf("tableDataRowCount() = %d, want 6", got)
	}

	for _, tt := range []struct {
		cursor int
		id     string
		ok     bool
	}{
		{cursor: -1},
		{cursor: 0, id: "eps", ok: true},
		{cursor: 1, id: "eps", ok: true}, // endpoint row maps back to its session
		{cursor: 2, id: "both", ok: true},
		{cursor: 3, id: "both", ok: true}, // endpoint row
		{cursor: 4, id: "both", ok: true}, // warning row
		{cursor: 5, id: "plain", ok: true},
		{cursor: 6},
	} {
		idx, ok := h.sessionIndexForTableCursor(tt.cursor)
		if ok != tt.ok {
			t.Errorf("sessionIndexForTableCursor(%d) ok = %t, want %t", tt.cursor, ok, tt.ok)
			continue
		}
		if ok && h.sessions[idx].GetId() != tt.id {
			t.Errorf("sessionIndexForTableCursor(%d) = %q, want %q", tt.cursor, h.sessions[idx].GetId(), tt.id)
		}
	}

	for _, tt := range []struct{ index, want int }{
		{0, 0}, {1, 2}, {2, 5}, {3, -1},
	} {
		if got := h.tableCursorForSessionIndex(tt.index); got != tt.want {
			t.Errorf("tableCursorForSessionIndex(%d) = %d, want %d", tt.index, got, tt.want)
		}
	}
}

// TestHomeEndpointRowRendersBeforeWarnings verifies the rendered row order and
// that only the NAME column carries the labels.
func TestHomeEndpointRowRendersBeforeWarnings(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	sess := endpointSession("both", 3000, 5173)
	sess.SetupError = "setup script blew up"
	h.sessions = []*pb.Session{sess}
	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (primary + endpoint + warning)", len(rows))
	}
	endpointRow := rows[1]
	if got := visibleRowText(endpointRow[3]); got != ":3000 :5173" {
		t.Errorf("endpoint row NAME = %q, want %q", got, ":3000 :5173")
	}
	for i, cell := range endpointRow {
		if i == 3 {
			continue
		}
		if cell != "" {
			t.Errorf("endpoint row cell %d = %q, want empty", i, cell)
		}
	}
	if !strings.Contains(endpointRow[3], "\x1b]8;;http://localhost:3000\x1b\\") {
		t.Errorf("endpoint row lost the :3000 OSC 8 hyperlink: %q", endpointRow[3])
	}
	if !strings.Contains(endpointRow[3], "\x1b]8;;http://localhost:5173\x1b\\") {
		t.Errorf("endpoint row lost the :5173 OSC 8 hyperlink: %q", endpointRow[3])
	}
	if got := visibleRowText(rows[2][3]); !strings.Contains(got, "setup script blew up") {
		t.Errorf("warning row NAME = %q, want the setup-error hint below the endpoint row", got)
	}
}

// TestHomeEndpointLinksSurviveTableRender is the clickability gate. The row
// cells above are only the INPUT to bubbles' cell rendering — ansi.Truncate,
// lipgloss Inline/MaxWidth, and the selected-row style all sit between them and
// the screen, and each is documented as capable of mangling an OSC 8 envelope.
// Assert on the rendered View so a lipgloss/bubbles bump that eats the envelope
// fails here rather than silently shipping unclickable ports.
func TestHomeEndpointLinksSurviveTableRender(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.width = 120
	h.height = 24
	h.sessions = []*pb.Session{
		endpointSession("eps", 3000, 5173),
		{Id: "plain", Title: "Plain session"},
	}
	h.buildTableRows()

	content := h.View().Content
	for _, want := range []string{
		"\x1b]8;;http://localhost:3000\x1b\\",
		"\x1b]8;;http://localhost:5173\x1b\\",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("rendered home view lost the hyperlink %q:\n%q", want, content)
		}
	}
	if !strings.Contains(stripANSI(osc8Pattern.ReplaceAllString(content, "")), ":3000 :5173") {
		t.Errorf("rendered home view lost the visible endpoint labels:\n%q", content)
	}
}

// TestHomeNoEndpointsRowsUnchanged pins that an endpoint-free board grows no
// auxiliary rows and emits no endpoint hyperlink. It is NOT the escape-byte
// identity gate — that is TestLinkRenderers_ExactEscapes in status_test.go.
func TestHomeNoEndpointsRowsUnchanged(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{
		{Id: "plain", Title: "Plain session"},
		{Id: "warned", Title: "Warned session", SetupError: "boom"},
		// Endpoints that cannot be rendered (zero port) must not add a row.
		{Id: "portless", Title: "Portless", HttpEndpoints: []*pb.HttpEndpoint{{Url: "http://localhost"}}},
	}
	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (3 primary + 1 warning)", len(rows))
	}
	for _, row := range rows {
		if strings.Contains(row[3], "\x1b]8;;http") {
			t.Errorf("unexpected endpoint hyperlink in a no-endpoint board: %q", row[3])
		}
	}
	if got := h.tableDataRowCount(); got != 4 {
		t.Errorf("tableDataRowCount() = %d, want 4", got)
	}
}

// TestHomeEndpointLabelsCountTowardNameWidth verifies the NAME column widens for
// a long endpoint list even when every session title is short.
func TestHomeEndpointLabelsCountTowardNameWidth(t *testing.T) {
	narrow := NewHomeModel(nil, context.Background(), nil)
	narrow.sessions = []*pb.Session{{Id: "a", Title: "ab"}}
	narrow.buildTableRows()
	narrowWidth := narrow.table.Columns()[3].Width

	wide := NewHomeModel(nil, context.Background(), nil)
	wide.sessions = []*pb.Session{endpointSession("a", 3000, 5173, 8080, 9229)}
	wide.sessions[0].Title = "ab"
	wide.buildTableRows()
	wideWidth := wide.table.Columns()[3].Width

	if wideWidth <= narrowWidth {
		t.Fatalf("NAME width did not account for endpoint labels: %d <= %d", wideWidth, narrowWidth)
	}
	// Derive the floor from the same helper the production width path reads
	// (home_table.go) instead of transcribing the joined labels: a literal
	// silently keeps passing for any separator WIDER than the current one, so it
	// would stop bounding the property this test names. The separator's own bytes
	// are pinned by TestSessionEndpointLabels. Keep this fixture's port list
	// short: want is uncapped, while the production NAME width is capped at 60
	// columns, so a label run past that cap would make the floor unreachable.
	if want := lipgloss.Width(sessionEndpointLabels(wide.sessions[0])); wideWidth < want {
		t.Fatalf("NAME width = %d, want at least %d to fit the endpoint labels", wideWidth, want)
	}
}

// TestHomeTableHeightCountsEndpointRows mirrors the repair-warning height test.
func TestHomeTableHeightCountsEndpointRows(t *testing.T) {
	h := HomeModel{sessions: []*pb.Session{endpointSession("a", 3000)}}
	if got := h.tableHeight(); got != 3 {
		t.Fatalf("tableHeight() = %d, want 3: header + session row + endpoint row", got)
	}
}

// TestHomeNavigationSkipsEndpointRows walks the cursor down and back up through
// a board whose middle session carries an endpoint row, asserting the cursor
// only ever settles on primary rows.
func TestHomeNavigationSkipsEndpointRows(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.sessions = []*pb.Session{
		{Id: "first", Title: "First"},
		endpointSession("middle", 3000),
		{Id: "last", Title: "Last"},
	}
	h.buildTableRows()

	// Down through every auxiliary row: cursor must land on 0 → 2 → 3.
	wantDown := []string{"middle", "last"}
	for _, want := range wantDown {
		prev := h.table.Cursor()
		h.table.MoveDown(1)
		h.normalizeTableCursor(prev)
		sess := h.selectedSession()
		if sess == nil || sess.GetId() != want {
			t.Fatalf("moving down from %d selected %v, want %q", prev, sess.GetId(), want)
		}
	}
	wantUp := []string{"middle", "first"}
	for _, want := range wantUp {
		prev := h.table.Cursor()
		h.table.MoveUp(1)
		h.normalizeTableCursor(prev)
		sess := h.selectedSession()
		if sess == nil || sess.GetId() != want {
			t.Fatalf("moving up from %d selected %v, want %q", prev, sess.GetId(), want)
		}
	}
}

// TestHomeSelectionSurvivesEndpointChurn drives the real poll path and verifies
// that endpoints appearing and disappearing on OTHER sessions keep the SAME
// session selected by ID, even though the row indices shift underneath it.
func TestHomeSelectionSurvivesEndpointChurn(t *testing.T) {
	plain := func() []*pb.Session {
		return []*pb.Session{
			{Id: "first", Title: "First"},
			{Id: "second", Title: "Second"},
			{Id: "third", Title: "Third"},
		}
	}
	churned := func() []*pb.Session {
		first := endpointSession("first", 3000)
		first.Title = "First"
		second := endpointSession("second", 5173, 8080)
		second.Title = "Second"
		return []*pb.Session{first, second, {Id: "third", Title: "Third"}}
	}

	h := NewHomeModel(nil, context.Background(), nil)
	poll := func(sessions []*pb.Session) {
		t.Helper()
		updated, _ := h.Update(sessionListMsg{sessions: sessions})
		h = updated.(HomeModel)
	}

	poll(plain())
	cursor, ok := h.tableCursorForSessionID("third")
	if !ok {
		t.Fatal("third session not found")
	}
	h.table.SetCursor(cursor)
	if got := h.selectedSession().GetId(); got != "third" {
		t.Fatalf("pre-churn selection = %q, want third", got)
	}

	poll(churned())
	if got := h.selectedSession().GetId(); got != "third" {
		t.Fatalf("selection after endpoints appeared = %q, want third", got)
	}
	// first: primary + 1 endpoint row; second: primary + 1 endpoint row (both
	// its ports share a single row) → third's primary row is 4.
	if got := h.table.Cursor(); got != 4 {
		t.Fatalf("cursor after endpoints appeared = %d, want 4", got)
	}

	poll(plain())
	if got := h.selectedSession().GetId(); got != "third" {
		t.Fatalf("selection after endpoints vanished = %q, want third", got)
	}
	if got := h.table.Cursor(); got != 2 {
		t.Fatalf("cursor after endpoints vanished = %d, want 2", got)
	}
}

// TestHomeEndpointRowNarrowTerminal verifies a narrow terminal neither drops
// the endpoint row from the accounting nor panics on the truncated NAME column.
func TestHomeEndpointRowNarrowTerminal(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 40
	h.height = 10
	h.sessions = []*pb.Session{endpointSession("a", 3000, 5173, 8080, 9229, 4000)}
	h.buildTableRows()

	if got := h.tableDataRowCount(); got != 2 {
		t.Fatalf("tableDataRowCount() = %d, want 2", got)
	}
	if len(h.table.Rows()) != 2 {
		t.Fatalf("rows = %d, want 2", len(h.table.Rows()))
	}
	if h.table.Cursor() != 0 {
		t.Fatalf("cursor = %d, want 0 (the primary row)", h.table.Cursor())
	}
	// The NAME column caps at 60 columns, so five endpoint labels get cut by the
	// bubbles table's ansi.Truncate. Truncation must not strand a hyperlink
	// introducer without its terminator: each OSC 8 link contributes exactly two
	// "\x1b]8;;" markers, so an odd count means the rest of the screen would be
	// swallowed into one link.
	content := h.View().Content
	if n := strings.Count(content, "\x1b]8;;"); n%2 != 0 {
		t.Errorf("rendered narrow home view has %d OSC 8 markers (odd) — an envelope was left open:\n%q", n, content)
	}
}

// longCloudAccessErrorDetail is the real failure from the BOS-507 report: 173
// columns on its own, 243 once cloudAccessUnavailableLine has composed it. Long
// enough that a 120-column terminal must wrap it across several lines.
//
// Aliased from the fixtures package rather than re-declared so these wrap tests
// and the cloud-error proof scenario pin the SAME string: the proof screenshot
// and this guard would otherwise drift apart the moment either copy is edited.
const longCloudAccessErrorDetail = fixtures.LongCloudAccessError

// homeStatusWrapColumns returns a column set whose columnsWidth is exactly
// want, so the width matrix can pin table-derived widths without depending on
// the real home column layout.
func homeStatusWrapColumns(want int) []table.Column {
	// columnsWidth adds tableColumnGap per column, so a single column of
	// (want - tableColumnGap) yields exactly want.
	return []table.Column{{Title: "NAME", Width: want - tableColumnGap}}
}

func TestHomeStatusWrapWidth(t *testing.T) {
	someSessions := []*pb.Session{{Id: "s1", Title: "work"}}

	tests := []struct {
		name     string
		width    int
		sessions []*pb.Session
		columns  []table.Column
		want     int
	}{
		{
			name:     "terminal width unknown leaves the line unconstrained",
			width:    0,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     0,
		},
		{
			name:     "negative terminal width leaves the line unconstrained",
			width:    -1,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     0,
		},
		{
			name:     "wide terminal wide table tracks the table",
			width:    200,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     116,
		},
		{
			name:     "wide terminal narrow table floors at minStatusWrapWidth",
			width:    200,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(38),
			want:     minStatusWrapWidth,
		},
		{
			name:     "narrow terminal clamps to the terminal width",
			width:    50,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     50,
		},
		{
			name:     "terminal narrower than the floor clamps below it",
			width:    40,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     40,
		},
		{
			// Below twice the padding lipgloss has no columns to wrap into and
			// silently renders the line unwrapped, so report 0 rather than a
			// width that only looks like it constrains.
			name:     "terminal too narrow to wrap into leaves the line unconstrained",
			width:    statusLinePadding * 2,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     0,
		},
		{
			name:     "narrowest terminal that can actually wrap clamps to itself",
			width:    statusLinePadding*2 + 1,
			sessions: someSessions,
			columns:  homeStatusWrapColumns(116),
			want:     statusLinePadding*2 + 1,
		},
		{
			name:     "empty state ignores stale columns",
			width:    200,
			sessions: nil,
			columns:  homeStatusWrapColumns(116),
			want:     minStatusWrapWidth,
		},
		{
			name:     "empty state with no columns uses the floor",
			width:    200,
			sessions: nil,
			columns:  nil,
			want:     minStatusWrapWidth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.width = tt.width
			h.sessions = tt.sessions
			h.table.SetColumns(tt.columns)
			if got := h.statusWrapWidth(); got != tt.want {
				t.Fatalf("statusWrapWidth() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestHomeFixedStatusCopyStaysReadableAtFloor bounds the deliberate trade-off
// baked into minStatusWrapWidth. The floor is narrower than most of Home's
// fixed status copy, so in the empty state (the one place the floor is the
// whole rule) that copy renders on two rows rather than one. That cost is
// accepted — a floor wide enough for the copy would overhang a typical board —
// but it must stay bounded: copy edited past two rows turns a status line into
// a ribbon, and this test fails before that ships.
func TestHomeFixedStatusCopyStaysReadableAtFloor(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 200 // wide terminal, no sessions: the floor is the whole rule
	if got := h.statusWrapWidth(); got != minStatusWrapWidth {
		t.Fatalf("statusWrapWidth() = %d, want the floor %d", got, minStatusWrapWidth)
	}

	// Every fixed (non-error-text) string Home renders through statusLine,
	// referenced from the production consts so the guard cannot pass against a
	// stale copy of text the screen no longer shows.
	fixed := []string{
		cloudBillingUnavailableLine,
		statusCloudChecking,
		statusCloudNeedsSubscriptionUpgrade,
		statusCloudNeedsSubscription,
		statusCloudResumeCheckoutUpgrade,
		statusCloudResumeCheckout,
		statusCloudAlreadyActive,
		statusCloudPendingUpgrade,
		statusCloudPending,
		statusUpgrading,
		statusRestartingDaemon,
		statusUpgradeRestartPrompt,
		statusUpgradeDone,
		fmt.Sprintf(statusUpgradeAvailableFormat, "v1.2.3", "v1.2.4"),
	}
	for _, text := range fixed {
		rows := strings.Split(h.statusLine(colorWarning, text), "\n")
		if len(rows) > 2 {
			t.Errorf("fixed status copy renders %d rows at the %d-column floor, want <= 2 (%d columns): %q",
				len(rows), minStatusWrapWidth, lipgloss.Width(text), text)
		}
		// No copy may be dropped by the wrap.
		if got := reflowStatusBlock(h.statusLine(colorWarning, text)); got != text {
			t.Errorf("wrapped copy does not reflow to the original:\n got: %q\nwant: %q", got, text)
		}
	}
}

// TestHomeStatusWrapWidthTracksRealTable pins the branch the ticket exists for
// against a real table built by buildTableRows, not the synthetic columns the
// width matrix uses: with sessions on screen the status width must equal the
// rendered table's columnsWidth, and must be the derived value rather than the
// floor. Without this, every render test could pass while the table-derived
// branch was never exercised.
func TestHomeStatusWrapWidthTracksRealTable(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 200
	h.sessions = []*pb.Session{
		{Id: "sess-1", RepoDisplayName: "mobile-app", Title: "Add rate limiting to the public API", PrNumber: i32ForTest(597)},
		{Id: "sess-2", RepoDisplayName: "my-app", Title: "Fix login bug"},
	}
	h.buildTableRows()

	derived := columnsWidth(h.table.Columns())
	if derived <= minStatusWrapWidth {
		t.Fatalf("real table columnsWidth = %d, want > the %d floor so this test covers the derived branch", derived, minStatusWrapWidth)
	}
	if got := h.statusWrapWidth(); got != derived {
		t.Fatalf("statusWrapWidth() = %d, want the table's columnsWidth %d", got, derived)
	}

	// The whole point: the rendered status block must sit inside the rendered
	// table, not overhang it. View() draws the table inside Padding(0, 1), so
	// the table's outer width is columnsWidth + 2.
	tableWidth := lipgloss.Width(lipgloss.NewStyle().Padding(0, 1).Render(h.table.View()))
	block := h.statusLine(colorWarning, "Cloud access status unavailable: "+longCloudAccessErrorDetail)
	for i, line := range strings.Split(block, "\n") {
		if got := lipgloss.Width(line); got > tableWidth {
			t.Errorf("status line %d measures %d columns, want <= the rendered table width %d: %q", i, got, tableWidth, line)
		}
	}
}

func i32ForTest(v int32) *int32 { return &v }

// homeWithLongCloudError builds a Home model on a wide terminal with sessions
// present and a long cloud-access failure pending.
func homeWithLongCloudError(t *testing.T) HomeModel {
	t.Helper()
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	// The title is long enough that the real columnsWidth clears
	// minStatusWrapWidth, so these render tests exercise the table-derived
	// branch rather than silently falling back to the floor (the floor branch
	// has its own test, TestHomeEmptyStateCloudErrorWrapsAtFloor).
	h.sessions = []*pb.Session{{Id: "sess-1", RepoDisplayName: "my-app", Title: "Add rate limiting to the public API"}}
	h.buildTableRows()
	h.width = 120
	h.height = 30

	model, _ := h.Update(cloudAccessMsg{err: errors.New(longCloudAccessErrorDetail)})
	return model.(HomeModel)
}

// reflowStatusBlock undoes a status block's wrapping: it strips ANSI colour and
// collapses the newlines and right-padding that Width() introduces back into
// single spaces, so a caller can assert on copy that a wrap may have split
// across rows.
func reflowStatusBlock(block string) string {
	return strings.Join(strings.Fields(stripANSI(block)), " ")
}

// assertWrapped checks a rendered status block wrapped onto more than one line
// and that every line fits the wrap width. Measured with lipgloss.Width, not
// len(): the lines carry ANSI colour, so len() would silently pass.
func assertWrapped(t *testing.T, label, block string, want int) {
	t.Helper()
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		t.Fatalf("%s rendered %d line(s), want it wrapped across several:\n%q", label, len(lines), block)
	}
	for i, line := range lines {
		if got := lipgloss.Width(line); got > want {
			t.Errorf("%s line %d measures %d columns, want <= %d: %q", label, i, got, want, line)
		}
	}
}

func TestHomeCloudGateLineWrapsAtContentWidth(t *testing.T) {
	h := homeWithLongCloudError(t)

	want := h.statusWrapWidth()
	if want <= 0 || want > h.width {
		t.Fatalf("statusWrapWidth() = %d, want a positive width no wider than the terminal (%d)", want, h.width)
	}

	line := h.cloudGateLine()
	assertWrapped(t, "cloudGateLine()", line, want)

	// Wrapping inserts newlines mid-sentence and right-pads every line, so
	// reflow the block back to a single space-separated string before checking
	// that no copy was dropped.
	plain := reflowStatusBlock(line)
	if !strings.Contains(plain, "Cloud access status unavailable") {
		t.Errorf("wrapped gate line lost its heading:\n%s", plain)
	}
	// The tail is what ran off the terminal edge before this fix.
	if !strings.Contains(plain, "Local sessions are still available.") {
		t.Errorf("wrapped gate line lost its trailing copy:\n%s", plain)
	}
}

func TestHomeCloudCheckoutStatusLineWrapsAtContentWidth(t *testing.T) {
	h := homeWithLongCloudError(t)
	h.cloudCheckoutStatus = longCloudAccessErrorDetail

	assertWrapped(t, "cloudCheckoutStatusLine()", h.cloudCheckoutStatusLine(), h.statusWrapWidth())
}

func TestHomeStatusLineWrapsAtContentWidth(t *testing.T) {
	h := homeWithLongCloudError(t)
	h.cloudErr = nil
	h.status = longCloudAccessErrorDetail

	want := h.statusWrapWidth()
	content := h.View().Content
	lines := strings.Split(content, "\n")

	// Measure EVERY row of the status block, not just the rows that happen to
	// carry a recognisable token: an overflow confined to a middle wrapped row
	// (".../authenticate\": dial tcp: lookup") would otherwise pass. Take the
	// block's row count from the helper itself, then measure that whole span
	// where it lands in the view.
	blockRows := len(strings.Split(h.statusLine(colorSuccess, h.status), "\n"))
	if blockRows < 2 {
		t.Fatalf("status block rendered on a single row, want it wrapped")
	}
	first := -1
	for i, line := range lines {
		if strings.Contains(stripANSI(line), "refresh token") {
			first = i
			break
		}
	}
	if first < 0 || first+blockRows > len(lines) {
		t.Fatalf("home view did not render the wrapped status block (first=%d rows=%d):\n%s", first, blockRows, stripANSI(content))
	}
	for i := first; i < first+blockRows; i++ {
		line := lines[i]
		if got := lipgloss.Width(line); got > want {
			t.Errorf("status line %d measures %d columns, want <= %d: %q", i, got, want, line)
		}
	}
}

func TestHomeUpgradeStatusViewWrapsAtContentWidth(t *testing.T) {
	h := homeWithLongCloudError(t)
	h.upgradeAvailable = true
	h.upgradeCurrent = "v1.0.0"
	h.upgradeLatest = "v2.0.0"

	want := h.statusWrapWidth()
	for i, line := range strings.Split(h.upgradeStatusView(), "\n") {
		if got := lipgloss.Width(line); got != want {
			t.Errorf("upgrade status line %d measures %d columns, want exactly %d (Width right-pads): %q", i, got, want, line)
		}
	}
}

// TestHomeEmptyStateCloudErrorWrapsAtFloor pins the empty-state branch: with no
// sessions the table is not drawn, so the wrap width falls back to the floor
// rather than a stale column width.
func TestHomeEmptyStateCloudErrorWrapsAtFloor(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.SetCloudAccessClient(&fakeHomeCloudAccessClient{})
	h.loading = false
	h.loggedIn = true
	h.repoCount = 0
	h.width = 120
	h.height = 30

	model, _ := h.Update(cloudAccessMsg{err: errors.New(longCloudAccessErrorDetail)})
	h = model.(HomeModel)

	if got := h.statusWrapWidth(); got != minStatusWrapWidth {
		t.Fatalf("statusWrapWidth() = %d, want the floor %d", got, minStatusWrapWidth)
	}
	assertWrapped(t, "empty-state cloudGateLine()", h.cloudGateLine(), minStatusWrapWidth)
}

// TestRenderErrorFillsTheGivenWidth pins the lipgloss contract renderError
// depends on: .Width(n) sets the TOTAL block width with styleError's padding
// included, and right-pads every line to n. So a caller passing the terminal
// width must get a block exactly that wide — the assertion is equality, not
// <=, because a block that came out narrower is the bug this guards (BOS-507:
// subtracting the padding a second time rendered every error 4 columns short).
//
// Widths are all comfortably above styleError's 4 columns of horizontal
// padding: lipgloss no-ops .Width(n) for n at or below the padding, so an
// equality assertion down there would fail for unrelated reasons.
// TestStyleErrorWidthNoOpsAtOrBelowItsPadding covers that band instead.
func TestRenderErrorFillsTheGivenWidth(t *testing.T) {
	// The real BOS-507 failure — long enough to wrap at every width below, and
	// it carries a token wider than the narrowest content area so the hard-break
	// path is covered too.
	const msg = longCloudAccessErrorDetail

	for _, width := range []int{40, 60, 100} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			lines := strings.Split(renderError(msg, width), "\n")
			// Guard the guard: a single unwrapped line would satisfy the width
			// assertion below without exercising the wrap at all.
			if len(lines) < 2 {
				t.Fatalf("renderError(msg, %d) rendered %d line(s), want it wrapped across several "+
					"(if fixtures.LongCloudAccessError was shortened, widen this subtest rather than "+
					"reading this as a renderError bug)", width, len(lines))
			}
			for i, line := range lines {
				// lipgloss.Width, not len(): the lines carry ANSI colour.
				if got := lipgloss.Width(line); got != width {
					t.Errorf("renderError(msg, %d) line %d measures %d columns, want exactly %d (Width right-pads): %q",
						width, i, got, width, line)
				}
			}
			// No copy may be dropped by the wrap.
			if got := squashErrorWhitespace(strings.Join(lines, "\n")); got != squashErrorWhitespace(msg) {
				t.Errorf("wrapped error dropped copy:\n got: %q\nwant: %q", got, squashErrorWhitespace(msg))
			}
		})
	}
}

func TestHomeNarrowChromeUsesContentWidth(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 72
	h.sessions = []*pb.Session{{Id: "sess-1", Title: "A session title long enough to build the fitted table"}}
	h.buildTableRows()

	h.confirm = newConfirmPrompt("Log out of the account with this.deliberately.long.email.address@example.com?", nil)
	footer := h.renderSessionTable()
	for _, line := range strings.Split(footer, "\n") {
		if got := lipgloss.Width(line); got > h.width {
			t.Errorf("logout confirmation line is %d columns, want <= %d: %q", got, h.width, line)
		}
	}
	if !strings.Contains(footer, "[y/enter] confirm") || !strings.Contains(footer, "[n/esc] cancel") {
		t.Errorf("logout confirmation does not use the shared footer: %q", footer)
	}

	h.confirm = confirmPrompt{}
	h.upgradeError = longCloudAccessErrorDetail
	assertWrapped(t, "upgrade error", h.upgradeStatusView(), h.statusWrapWidth())
	h.err = errors.New(longCloudAccessErrorDetail)
	assertWrapped(t, "daemon error", h.renderDaemonError(), h.statusWrapWidth())
}

// TestRenderErrorUnknownWidthIsUnconstrained pins the width == 0 fallback: with
// no tea.WindowSizeMsg yet there is nothing to wrap into, so the message stays
// on one line at its natural width plus styleError's padding.
func TestRenderErrorUnknownWidthIsUnconstrained(t *testing.T) {
	const msg = longCloudAccessErrorDetail

	rendered := renderError(msg, 0)
	if lines := strings.Split(rendered, "\n"); len(lines) != 1 {
		t.Fatalf("renderError(msg, 0) rendered %d lines, want the message unconstrained on one: %q", len(lines), rendered)
	}
	// Ask styleError for its own padding rather than restating theme.go's
	// Padding(0, 2) as a local constant: a literal here would go quietly stale
	// the day the theme changes, and fail naming the wrong culprit. Only the
	// width == 0 case needs the term at all — when a width is given, lipgloss
	// folds the padding into that total.
	if want := lipgloss.Width(msg) + styleError.GetHorizontalPadding(); lipgloss.Width(rendered) != want {
		t.Errorf("renderError(msg, 0) measures %d columns, want the message plus padding (%d)", lipgloss.Width(rendered), want)
	}
	if got := stripANSI(rendered); !strings.Contains(got, msg) {
		t.Errorf("renderError(msg, 0) dropped copy:\n got: %q\nwant it to contain: %q", got, msg)
	}
}

// TestStyleErrorWidthNoOpsAtOrBelowItsPadding pins the LIPGLOSS behaviour that
// renderError's and statusWrapWidth's floors both rest on: lipgloss wraps at the
// width minus the horizontal padding, so a width at or below styleError's 4
// columns of padding leaves nothing to wrap into and .Width() does nothing at
// all. Both helpers therefore skip the call and fall back to unconstrained,
// which is honest only while this holds.
//
// Assert it against the STYLE, not through renderError: renderError's guard
// short-circuits before .Width() is ever reached, so going through the helper
// would compare its fallback against itself and could never fail. Driving
// styleError directly is what gives this teeth — if a lipgloss upgrade starts
// honouring these widths, this fails and both floors need revisiting.
func TestStyleErrorWidthNoOpsAtOrBelowItsPadding(t *testing.T) {
	const msg = longCloudAccessErrorDetail
	bare := styleError.Render(msg)

	// 1..4 is the whole band at or below styleError's horizontal padding; 5 is
	// the first width lipgloss actually honours, and anchors the boundary.
	for width := 1; width <= styleError.GetHorizontalPadding(); width++ {
		t.Run(fmt.Sprintf("width_%d_noop", width), func(t *testing.T) {
			if got := styleError.Width(width).Render(msg); got != bare {
				t.Errorf("styleError.Width(%d).Render(msg) differs from an unconstrained Render, so lipgloss now honours a width at or below the padding; renderError's and statusWrapWidth's floors both assume it does not:\n got: %q\nwant: %q",
					width, got, bare)
			}
		})
	}

	t.Run("width_just_above_padding_wraps", func(t *testing.T) {
		width := styleError.GetHorizontalPadding() + 1
		if got := styleError.Width(width).Render(msg); got == bare {
			t.Errorf("styleError.Width(%d).Render(msg) was still a no-op; the floor is meant to sit at the padding, so a width above it must constrain", width)
		}
	})
}

// squashErrorWhitespace strips ANSI then drops every space and newline, so a
// wrap can be checked for lost copy. Unlike reflowStatusBlock it does not
// preserve word boundaries: lipgloss hard-breaks a token wider than the content
// area rather than overflowing, and that break lands mid-word, which a
// space-joining reflow would report as a difference even though no characters
// were lost. The strip is folded in — as its siblings flattenPrompt and
// reflowStatusBlock do — because escape sequences carry no whitespace and so
// survive strings.Fields intact, making a forgotten strip a confusing diff
// rather than a clean failure.
func squashErrorWhitespace(s string) string { return strings.Join(strings.Fields(stripANSI(s)), "") }

// --- BOS-572: the Home session table fits the terminal width ---------------

// responsiveHomeSessions is the fixture the responsive-column tests share: a
// long-titled session that pushes NAME to its 60-column cap, a session with
// HTTP endpoints (an endpoint sub-row), and a session with a setup failure (a
// warning sub-row). Between them the declared column set is ~102 columns wide,
// comfortably inside a 140-column board and comfortably outside a 72-column
// one, so both tiers are exercised by real data rather than a synthetic table.
func responsiveHomeSessions() []*pb.Session {
	eps := &pb.Session{
		Id:              "eps",
		RepoDisplayName: "my-app",
		Title:           "Serve the docs site",
		HttpEndpoints: []*pb.HttpEndpoint{
			{Port: 3000, Url: "http://localhost:3000"},
			{Port: 5173, Url: "http://localhost:5173"},
		},
	}
	return []*pb.Session{
		{
			Id:              "long",
			RepoDisplayName: "bossanova",
			// Longer than the NAME column's 60-column cap on purpose.
			Title:      "Add responsive table primitives and apply them to the Home session list",
			PrNumber:   i32ForTest(1234),
			BranchName: "boss-build-bos-572",
		},
		eps,
		{
			Id:              "warn",
			RepoDisplayName: "web",
			Title:           "Fix the login redirect",
			SetupError:      "setup script failed to install dependencies",
		},
	}
}

// homeResponsiveModel builds a Home model at the given terminal width with the
// shared fixture and a freshly fitted table.
func homeResponsiveModel(width int) HomeModel {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.width = width
	h.height = 40
	h.sessions = responsiveHomeSessions()
	h.buildTableRows()
	return h
}

func homeColumnTitles(cols []table.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Title
	}
	return out
}

// TestHomeColumnsUnchangedAtFullWidth is the no-regression guard for BOS-572.
// The full tier must draw exactly what the pre-responsive board drew: same
// titles, same widths, same cell count per row. h.width == 0 is the unfitted
// board (no tea.WindowSizeMsg has arrived), so DeepEqual against it pins the
// fit as a provable no-op at 140 columns rather than a coincidence.
func TestHomeColumnsUnchangedAtFullWidth(t *testing.T) {
	full := homeResponsiveModel(140)
	unfitted := homeResponsiveModel(0)

	wantTitles := []string{" ", " ", "REPO", "NAME", "PR", "STATUS"}
	if got := homeColumnTitles(full.table.Columns()); !reflect.DeepEqual(got, wantTitles) {
		t.Fatalf("column titles at 140 columns = %v, want %v", got, wantTitles)
	}
	if got, want := full.table.Columns(), unfitted.table.Columns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns at 140 columns = %+v, want the unfitted set %+v", got, want)
	}

	gotRows, wantRows := full.table.Rows(), unfitted.table.Rows()
	if len(gotRows) != len(wantRows) {
		t.Fatalf("rows at 140 columns = %d, want %d", len(gotRows), len(wantRows))
	}
	for i := range gotRows {
		if len(gotRows[i]) != len(wantTitles) {
			t.Errorf("row %d has %d cells at 140 columns, want %d", i, len(gotRows[i]), len(wantTitles))
		}
	}
}

// homeResponsiveWidths are the terminal widths the fit is exercised at: two
// narrow-tier widths, the narrow ceiling, the classic 80-column default, a
// compact width, and the full-tier proof width.
var homeResponsiveWidths = []int{40, 60, 72, 80, 100, 140}

// TestRenderSessionTablePadsByHomeTableBlockPadding closes the loop between the
// fit budget and the view that spends it.
//
// tableAvailWidth subtracts homeTableBlockPadding from both sides of the
// terminal, which is only correct while renderSessionTable actually wraps the
// table in that much padding. Every other assertion in this file re-applies the
// padding itself, so if renderSessionTable's Padding(0, ...) were widened to 2
// the fit would over-budget by 2, every narrow board would overhang the
// terminal by 2 — and the whole suite would stay green. This test is the one
// that reads the real view output, so that change fails here.
func TestRenderSessionTablePadsByHomeTableBlockPadding(t *testing.T) {
	for _, width := range homeResponsiveWidths {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			h := homeResponsiveModel(width)
			bare := h.table.View()
			want := lipgloss.Width(lipgloss.NewStyle().Padding(0, homeTableBlockPadding).Render(bare))

			// renderSessionTable writes the padded table block first, then the
			// status/action lines, so the table's own height bounds the block.
			lines := strings.Split(h.renderSessionTable(), "\n")
			blockHeight := lipgloss.Height(bare)
			if len(lines) < blockHeight {
				t.Fatalf("renderSessionTable returned %d lines, want at least the table's %d", len(lines), blockHeight)
			}
			got := 0
			for _, line := range lines[:blockHeight] {
				got = max(got, lipgloss.Width(line))
			}
			if got != want {
				// `Padding(0, ...)` is Go call notation naming the argument list this assertion is
				// about, so it stays spelled the way the source reads: ellipsis: literal-dots ok
				t.Errorf("renderSessionTable drew the table block %d columns wide at a %d-column terminal, want %d — the view's Padding(0, ...) no longer matches homeTableBlockPadding (%d), so tableAvailWidth is budgeting against the wrong number",
					got, width, want, homeTableBlockPadding)
			}
		})
	}
}

func TestHomeColumnsFitTerminalWidth(t *testing.T) {
	for _, width := range homeResponsiveWidths {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			h := homeResponsiveModel(width)
			cols := h.table.Columns()
			// Budget against tableAvailWidth, not the raw terminal width: the
			// table is drawn inside renderSessionTable's Padding(0, 1), so a
			// set that merely fits `width` still overhangs by both pads.
			// Asserting the looser bound would let homeTableBlockPadding go to
			// zero with every test still green and the board still overhanging.
			if got, avail := columnsWidth(cols), h.tableAvailWidth(); got > avail {
				t.Errorf("columnsWidth = %d at a %d-column terminal, want <= tableAvailWidth %d (titles %v)",
					got, width, avail, homeColumnTitles(cols))
			}
			// And the end-to-end claim the ticket is actually about: what is
			// RENDERED, padding included, fits the terminal.
			rendered := lipgloss.Width(lipgloss.NewStyle().Padding(0, homeTableBlockPadding).Render(h.table.View()))
			if rendered > width {
				t.Errorf("the rendered session table is %d columns wide at a %d-column terminal, want <= %d",
					rendered, width, width)
			}
			for i, c := range cols {
				// lipgloss reads Width(0) as unconstrained, so a fitted column
				// must never come back at or below zero.
				if c.Width <= 0 {
					t.Errorf("column %d (%q) has width %d at a %d-column terminal, want > 0", i, c.Title, c.Width, width)
				}
			}
		})
	}
}

// TestHomeNarrowTerminalDropsLowPriorityColumns pins that the fit actually
// bites on a narrow board: the expendable columns go and NAME stays.
func TestHomeNarrowTerminalDropsLowPriorityColumns(t *testing.T) {
	h := homeResponsiveModel(72)
	titles := homeColumnTitles(h.table.Columns())

	has := func(title string) bool {
		for _, got := range titles {
			if got == title {
				return true
			}
		}
		return false
	}
	if has("REPO") && has("PR") {
		t.Errorf("neither REPO nor PR was dropped at a 72-column terminal: %v", titles)
	}
	if !has("NAME") {
		t.Fatalf("NAME was dropped at a 72-column terminal: %v", titles)
	}
}

// TestHomeSubRowsFollowFittedNameColumn is the silent-bug gate. The endpoint
// and warning sub-rows carry their text in the NAME cell, and the NAME cell's
// index moves as soon as REPO is dropped — so a hard-coded index 3 puts the
// text in the wrong column (or off the end of the row) on a narrow board.
// Both sub-rows must be projected through the same fitted mapping the header
// is, at every width.
func TestHomeSubRowsFollowFittedNameColumn(t *testing.T) {
	for _, width := range []int{140, 72} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			h := homeResponsiveModel(width)
			cols := h.table.Columns()
			nameIdx := -1
			for i, c := range cols {
				if c.Title == "NAME" {
					nameIdx = i
				}
			}
			if nameIdx < 0 {
				t.Fatalf("no NAME column at a %d-column terminal: %v", width, homeColumnTitles(cols))
			}

			rows := h.table.Rows()
			// Fixture layout: long (primary), eps (primary + endpoint row),
			// warn (primary + warning row).
			if len(rows) != 5 {
				t.Fatalf("rows = %d, want 5", len(rows))
			}
			for _, tc := range []struct {
				label string
				row   int
				want  string
			}{
				{label: "endpoint sub-row", row: 2, want: ":3000"},
				{label: "warning sub-row", row: 4, want: "setup script failed"},
			} {
				row := rows[tc.row]
				if len(row) != len(cols) {
					t.Fatalf("%s has %d cells, want one per fitted column (%d)", tc.label, len(row), len(cols))
				}
				if got := visibleRowText(row[nameIdx]); !strings.Contains(got, tc.want) {
					t.Errorf("%s NAME cell (index %d) = %q, want it to contain %q", tc.label, nameIdx, got, tc.want)
				}
				for i, cell := range row {
					if i == nameIdx {
						continue
					}
					if cell != "" {
						t.Errorf("%s cell %d (%q) = %q, want empty", tc.label, i, cols[i].Title, cell)
					}
				}
			}
		})
	}
}

// TestHomeStatusWrapWidthNeverExceedsFittedTable checks BOS-507's alignment
// invariant still holds once the columns are width-fitted: the status block
// tracks the table it sits under, and never runs past the terminal.
//
// The minStatusWrapWidth term in the bound is deliberate, not slack. BOS-507's
// floor intentionally OVERRIDES the table-derived width on a very narrow board
// (a 250-column error wrapped at ~38 columns is an unreadable ribbon), and
// TestHomeStatusWrapWidth's "wide terminal narrow table floors at
// minStatusWrapWidth" case pins that override as intended behaviour. So the
// status width is bounded by the wider of the rendered table and that floor,
// and separately by the terminal.
func TestHomeStatusWrapWidthNeverExceedsFittedTable(t *testing.T) {
	for _, width := range homeResponsiveWidths {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			h := homeResponsiveModel(width)
			got := h.statusWrapWidth()
			if got > width {
				t.Errorf("statusWrapWidth() = %d at a %d-column terminal, want <= %d", got, width, width)
			}
			// renderSessionTable draws the table inside Padding(0,
			// homeTableBlockPadding) — use the constant, not a literal, so a
			// retune moves this measurement with the view.
			renderedTableWidth := lipgloss.Width(lipgloss.NewStyle().Padding(0, homeTableBlockPadding).Render(h.table.View()))
			if bound := max(renderedTableWidth, minStatusWrapWidth); got > bound {
				t.Errorf("statusWrapWidth() = %d at a %d-column terminal, want <= max(rendered table %d, floor %d) = %d",
					got, width, renderedTableWidth, minStatusWrapWidth, bound)
			}
		})
	}
}

// TestHomeEmptyStateRendersAtEveryWidth pins that the empty state — where
// buildTableRows returns before it ever fits a column — still renders and
// still wraps at the documented floor/terminal clamp.
func TestHomeEmptyStateRendersAtEveryWidth(t *testing.T) {
	for _, width := range []int{40, 72, 140} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			h := NewHomeModel(nil, context.Background(), nil)
			h.loading = false
			h.loggedIn = true
			h.repoCount = 1
			h.width = width
			h.height = 40
			h.sessions = nil
			h.buildTableRows()

			if content := h.View().Content; strings.TrimSpace(content) == "" {
				t.Fatalf("empty state rendered nothing at a %d-column terminal", width)
			}
			want := min(minStatusWrapWidth, width)
			if got := h.statusWrapWidth(); got != want {
				t.Errorf("statusWrapWidth() = %d at a %d-column terminal, want %d (the floor, clamped to the terminal)", got, width, want)
			}
		})
	}
}

// TestHomeTableRebuildsAcrossWidths is the crash gate for the responsive fit.
//
// buildTableRows hands bubbles a NEW column set and a NEW row set, and every
// bubbles setter re-renders the viewport immediately — so between the two
// calls the table holds the new columns against the PREVIOUS rows. Before
// BOS-572 the column count was a constant 6 and that window was harmless;
// now it shrinks, and table.renderRow indexes m.cols[i] for every cell in
// m.rows[r], so a stale 6-cell row against a 3-column set panics with an
// index-out-of-range. A narrowing resize (the feature's own headline path) and
// an ordinary poll that brings in a longer session title on a sub-full-tier
// terminal both reach it.
//
// Rebuilding in both directions, and at a constant width with growing data,
// is the only thing that exercises that window — a single build never can.
func TestHomeTableRebuildsAcrossWidths(t *testing.T) {
	t.Run("narrowing then widening", func(t *testing.T) {
		h := homeResponsiveModel(140)
		for _, width := range []int{72, 40, 140, 100, 60, 140} {
			h.width = width
			h.buildTableRows()
			cols := h.table.Columns()
			for _, row := range h.table.Rows() {
				if len(row) != len(cols) {
					t.Fatalf("at width %d a row has %d cells, want one per fitted column (%d)", width, len(row), len(cols))
				}
			}
			// Render too: the panic surfaces inside the viewport update, so a
			// View() here proves the model is drawable and not merely built.
			if h.table.View() == "" {
				t.Fatalf("table rendered nothing at width %d", width)
			}
		}
	})

	t.Run("resize message", func(t *testing.T) {
		h := homeResponsiveModel(140)
		for _, width := range []int{72, 140, 40} {
			updated, _ := h.handleWindowSize(tea.WindowSizeMsg{Width: width, Height: 40})
			next, ok := updated.(HomeModel)
			if !ok {
				t.Fatalf("handleWindowSize returned %T, want HomeModel", updated)
			}
			h = next
			if got := columnsWidth(h.table.Columns()); got > h.tableAvailWidth() {
				t.Errorf("after resizing to %d columns the table is %d wide, want <= %d",
					width, got, h.tableAvailWidth())
			}
		}
	})

	t.Run("constant width, growing data", func(t *testing.T) {
		// A 100-column terminal fits the short-titled board, then a poll brings
		// in a session whose title pushes the declared set over budget and
		// forces a drop — no resize involved.
		h := homeResponsiveModel(100)
		h.sessions = append(h.sessions, &pb.Session{
			Id:              "grew",
			RepoDisplayName: "bossanova",
			Title:           strings.Repeat("a very long session title ", 4),
		})
		h.buildTableRows()
		if h.table.View() == "" {
			t.Fatal("table rendered nothing after the session list grew")
		}
	})
}

// TestHomeNameMinWidthTiers pins the per-tier NAME floors and their tie to the
// width-tier constants.
//
// Only the NARROW floor is reachable through a Home board today, and
// TestHomeNameFloorBindsOnANarrowBoard is the test that reaches it. The squeeze
// pass only runs once every droppable column is gone, which NAME's 60-column
// cap confines to terminals below ~68 columns, so homeNameMinWidthCompact and
// homeNameMinWidthFull are inert — see homeNameMinWidth's comment for the
// arithmetic and for why they are kept anyway. This test is what stops the two
// inert constants from being behaviourally untested configuration, and what
// pins the tier boundaries themselves.
func TestHomeNameMinWidthTiers(t *testing.T) {
	tests := []struct {
		name  string
		width int
		want  int
	}{
		{name: "unknown width assumes the full tier", width: 0, want: 32},
		{name: "negative width assumes the full tier", width: -1, want: 32},
		{name: "narrow tier", width: 40, want: 16},
		{name: "narrow ceiling", width: narrowWidthMax, want: 16},
		{name: "compact tier starts one past the narrow ceiling", width: narrowWidthMax + 1, want: 24},
		{name: "compact ceiling", width: compactWidthMax, want: 24},
		{name: "full tier starts one past the compact ceiling", width: compactWidthMax + 1, want: 32},
		{name: "proof width is full tier", width: 140, want: 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := homeNameMinWidth(tt.width); got != tt.want {
				t.Fatalf("homeNameMinWidth(%d) = %d, want %d", tt.width, got, tt.want)
			}
		})
	}

	// The floors must widen monotonically with the terminal, or a wider board
	// would be allowed to truncate titles harder than a narrower one.
	prev := 0
	for _, width := range []int{40, narrowWidthMax, narrowWidthMax + 1, compactWidthMax, compactWidthMax + 1, 200} {
		got := homeNameMinWidth(width)
		if got < prev {
			t.Fatalf("homeNameMinWidth(%d) = %d, want >= the narrower tier's %d", width, got, prev)
		}
		prev = got
	}
}

// TestHomeNameFloorBindsOnANarrowBoard drives the Home column set to the width
// where homeNameMinWidth actually clamps the squeeze, so the floor is proven to
// be wired into the fit rather than merely declared.
func TestHomeNameFloorBindsOnANarrowBoard(t *testing.T) {
	const width = 22 // narrow tier: NAME floors at 16
	h := homeResponsiveModel(width)

	cols := h.table.Columns()
	titles := homeColumnTitles(cols)
	if len(cols) != 3 {
		t.Fatalf("columns at a %d-column terminal = %v, want only the two indicators and NAME", width, titles)
	}
	name := cols[2]
	if name.Title != "NAME" {
		t.Fatalf("last surviving column = %q, want NAME", name.Title)
	}
	// Assert the LITERAL, not homeNameMinWidth(width): deriving the expectation
	// from the production function would pass for any retuned value the squeeze
	// can reach, pinning only "a floor is wired in" rather than which one.
	if name.Width != homeNameMinWidthNarrow {
		t.Fatalf("NAME width at a %d-column terminal = %d, want the narrow tier floor %d", width, name.Width, homeNameMinWidthNarrow)
	}
	if got := homeNameMinWidth(width); got != homeNameMinWidthNarrow {
		t.Fatalf("homeNameMinWidth(%d) = %d, so this test is no longer exercising the narrow tier it names", width, got)
	}
	// The floor is what stops the squeeze, so the set is knowingly over budget
	// here — that is rule 6 (overflow beats a zero/negative width), not a bug.
	if got := columnsWidth(cols); got <= h.tableAvailWidth() {
		t.Fatalf("columnsWidth = %d at avail %d: expected the floor to hold the set over budget", got, h.tableAvailWidth())
	}
	for _, c := range cols {
		if c.Width <= 0 {
			t.Fatalf("column %q has width %d, want > 0 even when the floor binds", c.Title, c.Width)
		}
	}
}

// TestHomeRebuildKeepsTheSelectionVisible pins that a rebuild does not scroll
// the selected row out of the viewport.
//
// buildTableRows runs on every keypress and on every spinner tick — roughly ten
// times a second — so anything it does to the table's scroll offset happens
// continuously. bubbles keeps that offset inside its viewport and exposes no
// accessor for it: SetCursor, SetWidth and SetHeight all leave it alone, only
// MoveUp/MoveDown maintain it, and viewport.SetContent CLAMPS it (to zero for
// empty content). So a rebuild that empties the rows silently sends a scrolled
// board back to the top while the cursor stays where it was — the ❯ caret and
// the selected row leave the screen, and Enter goes on acting on a selection
// the operator can no longer see. app_view.go's setReservedTableHeight
// documents the same hazard on the SetHeight path.
//
// A short board can never show this, so the fixture here is deliberately taller
// than the viewport.
func TestHomeRebuildKeepsTheSelectionVisible(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.loading = false
	h.loggedIn = true
	h.repoCount = 1
	h.width = 140
	h.height = 20
	for i := range 30 {
		h.sessions = append(h.sessions, &pb.Session{
			Id:              fmt.Sprintf("s%02d", i),
			RepoDisplayName: "bossanova",
			Title:           fmt.Sprintf("session number %02d", i),
		})
	}
	h.buildTableRows()

	if len(h.table.Rows()) <= h.table.Height() {
		t.Fatalf("fixture is %d rows against a %d-row viewport: it cannot scroll, so this test would be vacuous",
			len(h.table.Rows()), h.table.Height())
	}

	// Scroll to the bottom through the table's own movement API, which is the
	// only thing that maintains the viewport offset.
	h.table.GotoBottom()
	selected := "session number 29"
	if !strings.Contains(visibleRowText(strings.Join(h.table.SelectedRow(), " ")), selected) {
		t.Fatalf("precondition failed: GotoBottom did not select the last session, got %q",
			visibleRowText(strings.Join(h.table.SelectedRow(), " ")))
	}
	if !strings.Contains(visibleRowText(h.table.View()), selected) {
		t.Fatalf("precondition failed: the selected row %q is not visible before the rebuild", selected)
	}

	// A spinner tick, a keypress, a poll — every one of them lands here.
	h.buildTableRows()

	if got := visibleRowText(h.table.View()); !strings.Contains(got, selected) {
		t.Errorf("after a rebuild the selected row %q is no longer visible in the viewport:\n%s", selected, got)
	}
}

// logoutFailingTokenStore makes Manager.Logout fail at the keychain delete.
type logoutFailingTokenStore struct{}

func (logoutFailingTokenStore) Save(*auth.Tokens) error { return nil }

func (logoutFailingTokenStore) Load() (*auth.Tokens, error) {
	return &auth.Tokens{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (logoutFailingTokenStore) Delete() error { return errors.New("keychain refused the delete") }

// TestHomeLogoutFailureIsSurfaced pins the fix for a silently discarded error:
// the confirmation used to run `_ = authMgr.Logout(ctx)`, so a logout that
// failed — the credential lock is cross-process and bossd can hold it while it
// refreshes — left the user staring at an unchanged, still-signed-in board with
// no explanation at all.
func TestHomeLogoutFailureIsSurfaced(t *testing.T) {
	h := HomeModel{
		ctx:           context.Background(),
		client:        &stubClient{},
		authMgr:       auth.NewManager(logoutFailingTokenStore{}, auth.Config{}),
		sessions:      []*pb.Session{},
		repoCount:     1,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		width:         100,
	}

	updated, _ := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h = updated.(HomeModel)
	if !h.confirm.active || h.confirm.action == nil {
		t.Fatal("expected an active logout confirmation with an action")
	}

	updated, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	h = updated.(HomeModel)
	if h.confirm.active {
		t.Fatal("confirmation stayed open after accepting logout")
	}
	msg := cmd()
	logout, ok := msg.(logoutMsg)
	if !ok {
		t.Fatalf("logout action returned %T, want a logoutMsg carrying the result", msg)
	}
	if logout.err == nil {
		t.Fatal("logout error was discarded; the TUI cannot report a failed logout")
	}
	if got := h.client.(*stubClient).authChanges; len(got) != 0 {
		t.Fatalf("logout notification = %v, want none when credential deletion fails", got)
	}

	updated, _ = h.Update(logout)
	h = updated.(HomeModel)
	if !h.loggedIn || h.loggedInEmail != "dev@example.com" {
		t.Fatalf("failed logout changed auth presentation: loggedIn=%t email=%q", h.loggedIn, h.loggedInEmail)
	}
	if content := h.View().Content; !strings.Contains(content, "Logout:") {
		t.Fatalf("failed logout is not rendered anywhere, got: %s", content)
	}
	if content := h.View().Content; !strings.Contains(content, "[l]ogout") {
		t.Fatalf("failed logout no longer offers logout, got: %s", content)
	}
}

// TestHomeLogoutFailureRecalculatesTableHeight keeps the full session board
// above both lines of the failed-logout status. The table caches its height, so
// merely counting the new footer rows is insufficient unless the result
// handler applies that count when the asynchronous logout fails.
func TestHomeLogoutFailureRecalculatesTableHeight(t *testing.T) {
	h := HomeModel{
		ctx:           context.Background(),
		authMgr:       &auth.Manager{},
		repoCount:     1,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		width:         100,
		height:        24,
		sessions:      make([]*pb.Session, 100),
	}
	for i := range h.sessions {
		h.sessions[i] = &pb.Session{Id: fmt.Sprintf("session-%d", i)}
	}
	h.buildTableRows()
	before := h.table.Height()

	updated, _ := h.Update(logoutMsg{err: errors.New("keychain refused the delete")})
	h = updated.(HomeModel)

	if h.table.Height() >= before {
		t.Fatalf("logout failure left table height at %d, want reservation below %d", h.table.Height(), before)
	}
	// bubbles' Height reports its viewport, excluding Home's one-row header.
	if got, want := h.table.Height(), h.tableHeight()-1; got != want {
		t.Fatalf("cached table height = %d, want %d after logout failure", got, want)
	}
}

// TestHomeLogoutSuccessClearsPriorError checks the other edge: a retry that
// succeeds must not leave the previous failure on screen.
func TestHomeLogoutSuccessClearsPriorError(t *testing.T) {
	h := HomeModel{
		ctx:           context.Background(),
		sessions:      []*pb.Session{},
		repoCount:     1,
		width:         100,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		needsRelogin:  true,
		reloginReason: auth.ReloginReasonRefreshOutcomeUnknown,
	}

	updated, _ := h.Update(logoutMsg{err: errors.New("keychain refused the delete")})
	h = updated.(HomeModel)
	if content := h.View().Content; !strings.Contains(content, "keychain refused the delete") {
		t.Fatalf("logout error not rendered, got: %s", content)
	}

	updated, _ = h.Update(logoutMsg{})
	h = updated.(HomeModel)
	if h.loggedIn || h.loggedInEmail != "" || h.needsRelogin || h.reloginReason != "" {
		t.Fatalf("successful logout did not clear auth presentation: %+v", h)
	}
	if content := h.View().Content; strings.Contains(content, "keychain refused the delete") {
		t.Fatalf("a successful logout left the previous error on screen, got: %s", content)
	}
}

func TestHomeLogoutDoesNotStartAnotherDeleteWhilePending(t *testing.T) {
	store := &countingLogoutTokenStore{}
	h := HomeModel{
		ctx:           context.Background(),
		client:        &stubClient{},
		authMgr:       auth.NewManager(store, auth.Config{}),
		sessions:      []*pb.Session{},
		repoCount:     1,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		width:         100,
	}

	updated, _ := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h = updated.(HomeModel)
	updated, first := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	h = updated.(HomeModel)
	if first == nil {
		t.Fatal("first logout confirmation returned no command")
	}

	updated, second := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h = updated.(HomeModel)
	if second != nil || h.confirm.active {
		t.Fatal("a pending logout must not open another confirmation")
	}

	updated, _ = h.Update(first())
	h = updated.(HomeModel)
	if got := store.deletes; got != 1 {
		t.Fatalf("Delete calls = %d, want 1", got)
	}
	if h.loggedIn {
		t.Fatal("successful logout must clear signed-in state")
	}
}

func TestHomeLogoutReturnsBeforeStalledNotification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	notifyStarted := make(chan struct{})
	h := HomeModel{
		ctx: ctx,
		client: &stubClient{notifyAuthChange: func(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
			if action != "logout" {
				t.Errorf("NotifyAuthChange action = %q, want logout", action)
			}
			close(notifyStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}},
		authMgr:       auth.NewManager(&countingLogoutTokenStore{}, auth.Config{}),
		sessions:      []*pb.Session{},
		repoCount:     1,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		width:         100,
	}

	updated, _ := h.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	h = updated.(HomeModel)
	updated, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	h = updated.(HomeModel)
	if cmd == nil {
		t.Fatal("logout confirmation returned no command")
	}

	commandResult := make(chan tea.Msg, 1)
	go func() { commandResult <- cmd() }()

	select {
	case msg := <-commandResult:
		logout, ok := msg.(logoutMsg)
		if !ok {
			t.Fatalf("logout command returned %T, want a logoutMsg", msg)
		}
		if logout.err != nil {
			t.Fatalf("logout error = %v, want nil", logout.err)
		}
		_, notifyCmd := h.Update(logout)
		if notifyCmd == nil {
			t.Fatal("successful logout did not schedule NotifyAuthChange")
		}
		go notifyCmd()
		select {
		case <-notifyStarted:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("NotifyAuthChange was not dispatched after logout")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("logout command waited for NotifyAuthChange")
	}
}

func TestAuthChangeQueueReleasesFollowerAfterTimedOutNotification(t *testing.T) {
	logoutStarted := make(chan struct{})
	logoutCanceled := make(chan error, 1)
	loginStarted := make(chan struct{})
	q := newAuthChangeQueue()
	q.notificationTimeout = 10 * time.Millisecond
	c := &stubClient{notifyAuthChange: func(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
		switch action {
		case "logout":
			close(logoutStarted)
			<-ctx.Done()
			logoutCanceled <- ctx.Err()
		case "login":
			close(loginStarted)
		}
		return nil, nil
	}}

	logout := q.notify(context.Background(), c, "logout")
	login := q.notify(context.Background(), c, "login")
	go logout()

	select {
	case <-logoutStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("logout notification did not start")
	}
	go login()

	select {
	case err := <-logoutCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("logout notification error = %v, want deadline exceeded", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stalled logout notification was not canceled")
	}
	select {
	case <-loginStarted:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("login notification remained blocked after logout timed out")
	}
}

func TestHomeLogoutIgnoresAnOlderAuthStatusResult(t *testing.T) {
	h := HomeModel{
		sessions:      []*pb.Session{},
		repoCount:     1,
		loggedIn:      true,
		loggedInEmail: "dev@example.com",
		width:         100,
	}

	updated, _ := h.Update(logoutMsg{})
	h = updated.(HomeModel)
	updated, _ = h.Update(authStatusMsg{loggedIn: true, email: "dev@example.com"})
	h = updated.(HomeModel)

	if h.loggedIn || h.loggedInEmail != "" {
		t.Fatalf("stale auth status restored signed-in state: loggedIn=%v email=%q", h.loggedIn, h.loggedInEmail)
	}

	updated, _ = h.Update(authStatusMsg{generation: h.authStatusGeneration, loggedIn: true, email: "new@example.com"})
	h = updated.(HomeModel)
	if !h.loggedIn || h.loggedInEmail != "new@example.com" {
		t.Fatalf("current auth status was not applied: loggedIn=%v email=%q", h.loggedIn, h.loggedInEmail)
	}
}

type countingLogoutTokenStore struct {
	deletes int
}

func (s *countingLogoutTokenStore) Save(*auth.Tokens) error { return nil }

func (s *countingLogoutTokenStore) Load() (*auth.Tokens, error) {
	return &auth.Tokens{AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (s *countingLogoutTokenStore) Delete() error {
	s.deletes++
	return nil
}

// --- BOS-837: the hidden [r] rename shortcut --------------------------------

// renameStubClient records every write the rename path issues. It layers on
// stubSessionSettingsClient, whose other methods panic, so an unexpected RPC
// from Home fails loudly rather than being quietly absorbed.
type renameStubClient struct {
	*stubSessionSettingsClient
	reqs []*pb.UpdateSessionRequest
}

var _ client.BossClient = (*renameStubClient)(nil)

func (c *renameStubClient) UpdateSession(ctx context.Context, req *pb.UpdateSessionRequest) (*pb.Session, error) {
	c.reqs = append(c.reqs, req)
	return c.stubSessionSettingsClient.UpdateSession(ctx, req)
}

// renamingHome is the three-session board with the rename editor already open
// on the first session, plus the client that counts its writes.
func renamingHome(t *testing.T, updated *pb.Session) (HomeModel, *renameStubClient) {
	t.Helper()
	stub := &renameStubClient{stubSessionSettingsClient: &stubSessionSettingsClient{updated: updated}}
	h := renameKeyHome(t)
	h.client = stub
	h = homeFromKey(t, mustModel(h.handleKey(keyPress('r'))))
	if !h.rename.Active() {
		t.Fatal("fixture failed to open the rename editor")
	}
	return h, stub
}

func TestHomeRenameCommitWritesExactlyOneUpdate(t *testing.T) {
	h, stub := renamingHome(t, &pb.Session{Id: "sess-1", Title: "Renamed board"})
	// Surrounding whitespace is what the operator would leave behind after
	// editing; the bytes tested for emptiness must be the bytes committed.
	h.rename.input.SetValue("  Renamed board  ")

	model, cmd := h.handleKey(specialKeyPress(tea.KeyEnter))
	h = homeFromKey(t, model)

	if cmd == nil {
		t.Fatal("enter scheduled no write")
	}
	msg := cmd()
	if _, ok := msg.(sessionRenamedMsg); !ok {
		t.Fatalf("the write produced %T, want sessionRenamedMsg", msg)
	}
	if len(stub.reqs) != 1 {
		t.Fatalf("UpdateSession called %d times, want exactly 1", len(stub.reqs))
	}
	if got := stub.reqs[0].GetId(); got != "sess-1" {
		t.Fatalf("wrote to session %q, want the selected sess-1", got)
	}
	if got := stub.reqs[0].GetTitle(); got != "Renamed board" {
		t.Fatalf("wrote title %q, want the trimmed title", got)
	}
	if h.rename.Active() {
		t.Fatal("the editor stayed open after a committed rename")
	}
}

func TestHomeRenameRefusesAWhitespaceOnlyTitle(t *testing.T) {
	h, stub := renamingHome(t, nil)
	h.rename.input.SetValue("   \t ")

	model, cmd := h.handleKey(specialKeyPress(tea.KeyEnter))
	h = homeFromKey(t, model)

	if cmd != nil {
		t.Fatal("a whitespace-only title scheduled a command")
	}
	if len(stub.reqs) != 0 {
		t.Fatalf("UpdateSession called %d times for a whitespace-only title, want 0", len(stub.reqs))
	}
	// The editor stays open with the typed text intact: a blank title is a
	// correctable mistake, and closing would discard the edit.
	if !h.rename.Active() {
		t.Fatal("the editor closed on a refused title")
	}
	if content := h.View().Content; !strings.Contains(content, "Title cannot be empty") {
		t.Fatalf("the refusal is not on screen:\n%s", content)
	}
}

// TestHomeRenameSuccessUpdatesTheRowWithoutAPoll drives the response message
// directly: the board has to show the new title as soon as the write returns,
// not two seconds later when the next poll happens to land.
func TestHomeRenameSuccessUpdatesTheRowWithoutAPoll(t *testing.T) {
	h := renameKeyHome(t)

	model, _ := h.Update(sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "Renamed board"}})
	h = homeFromKey(t, model)

	content := h.View().Content
	if !strings.Contains(content, "Renamed board") {
		t.Fatalf("the renamed title is not on the board:\n%s", content)
	}
	if strings.Contains(content, "Add dark mode") {
		t.Fatalf("the old title survived the rename:\n%s", content)
	}
	if !strings.Contains(content, "Renamed session to Renamed board") {
		t.Fatalf("the rename is not confirmed on the status line:\n%s", content)
	}
	if !strings.Contains(content, h.statusLine(colorSuccess, h.status)) {
		t.Fatalf("the confirmation is not rendered in the success colour:\n%q", content)
	}
	// Only the renamed row moves; the rest of the board is untouched.
	if !strings.Contains(content, "Fix login bug") {
		t.Fatalf("a sibling row was lost by the rename:\n%s", content)
	}
}

// TestHomeRenameLeavesThePublishedSessionPointerAlone is the data-race guard.
// applySessionList hands the very pointers it stores in h.sessions to
// notifyForSessions (home_sessions.go:417-418), whose tea.Cmd runs on its own
// goroutine and reads GetTitle() off them — so patching a title THROUGH the
// stored pointer would write a field another goroutine is reading. The fix
// clones and swaps the slice entry instead, which is what this asserts: the
// pointer the notify command captured must come back byte-identical.
//
// -race cannot see this on its own; no test pairs a question-state
// notification with a rename, so the two goroutines never overlap under the
// detector. The pointer identity below is the observable proxy for the
// invariant.
//
// Falsification: restore `h.sessions[i].Title = renamed.GetTitle()` in
// handleSessionRenamed and this fails on the first assertion. Performed once
// and the clone restored.
func TestHomeRenameLeavesThePublishedSessionPointerAlone(t *testing.T) {
	h := renameKeyHome(t)
	published := h.sessions[0]

	model, _ := h.Update(sessionRenamedMsg{session: &pb.Session{Id: "sess-1", Title: "Renamed board"}})
	h = homeFromKey(t, model)

	if got := published.GetTitle(); got != "Add dark mode" {
		t.Fatalf("the rename wrote through the published pointer: title = %q, want the original %q", got, "Add dark mode")
	}
	if h.sessions[0] == published {
		t.Fatal("the renamed row still holds the published pointer; the patch must land on a clone")
	}
	if got := h.sessions[0].GetTitle(); got != "Renamed board" {
		t.Fatalf("the clone did not receive the new title: %q", got)
	}
	// The clone must be a full copy, not a stub carrying only the title.
	if got := h.sessions[0].GetRepoDisplayName(); got != "bossanova" {
		t.Fatalf("the clone dropped poll-derived fields: repo = %q, want %q", got, "bossanova")
	}
}

// TestHomeRenameAcknowledgesASessionThatLeftTheBoard covers the response for a
// session no longer in h.sessions. The 2s poll runs throughout the rename, so
// the row can be archived — or filtered out — between the keystroke and the
// daemon's reply. The write still landed, so reporting only on a matched row
// would make a successful rename look like it was swallowed.
func TestHomeRenameAcknowledgesASessionThatLeftTheBoard(t *testing.T) {
	h := renameKeyHome(t)

	model, cmd := h.Update(sessionRenamedMsg{session: &pb.Session{Id: "sess-gone", Title: "Renamed elsewhere"}})
	h = homeFromKey(t, model)

	if cmd != nil {
		t.Fatal("adopting a rename response scheduled a command")
	}
	content := h.View().Content
	if !strings.Contains(content, "Renamed session to Renamed elsewhere") {
		t.Fatalf("a rename whose session left the board went unacknowledged:\n%s", content)
	}
	if h.statusErr {
		t.Fatal("a successful rename was recorded as a failure")
	}
	// The absent session must not be adopted onto the board either: the reply
	// is an acknowledgement, not a row source.
	if len(h.sessions) != 3 {
		t.Fatalf("the board holds %d sessions, want the original 3", len(h.sessions))
	}
	for _, want := range []string{"Add dark mode", "Fix login bug", "Add rate limiting"} {
		if !strings.Contains(content, want) {
			t.Fatalf("row %q was disturbed by a rename it had nothing to do with:\n%s", want, content)
		}
	}
}

func TestHomeRenameFailureKeepsTheOriginalTitle(t *testing.T) {
	h := renameKeyHome(t)

	model, _ := h.Update(sessionRenamedMsg{err: errors.New("daemon unavailable")})
	h = homeFromKey(t, model)

	content := h.View().Content
	// Nothing was written optimistically, so nothing has to be rolled back —
	// but the row must not have drifted either.
	if !strings.Contains(content, "Add dark mode") {
		t.Fatalf("a failed rename disturbed the original title:\n%s", content)
	}
	if !strings.Contains(content, "Rename failed") {
		t.Fatalf("the failure is not surfaced:\n%s", content)
	}
	// The substring above is colour-blind: it matches whether the line is drawn
	// in the failure colour or the success one, and Home's only status renderer
	// used to hard-code the success colour — a green "Rename failed" tells the
	// operator the opposite of what happened. Guard the two renderings apart
	// first, so a stripped colour profile fails this test rather than passing
	// the two assertions below vacuously.
	if h.statusLine(colorDanger, h.status) == h.statusLine(colorSuccess, h.status) {
		t.Fatal("danger and success render identically here; the colour assertions below would prove nothing")
	}
	if !strings.Contains(content, h.statusLine(colorDanger, h.status)) {
		t.Fatalf("the failure is not rendered in the danger colour:\n%q", content)
	}
	if strings.Contains(content, h.statusLine(colorSuccess, h.status)) {
		t.Fatalf("the failure is rendered in the success colour:\n%q", content)
	}
}

// TestHomeRenameStartClearsThePreviousOutcome: no poll clears the status line,
// so without this the last rename's result sits on the board forever — and a
// stale "Rename failed" hanging above a freshly opened editor reads as a
// complaint about the edit in progress.
func TestHomeRenameStartClearsThePreviousOutcome(t *testing.T) {
	h := homeFromKey(t, mustModel(renameKeyHome(t).Update(sessionRenamedMsg{err: errors.New("daemon unavailable")})))
	if h.status == "" {
		t.Fatal("fixture produced no status for the reopened editor to clear")
	}

	h = homeFromKey(t, mustModel(h.handleKey(keyPress('r'))))

	if !h.rename.Active() {
		t.Fatal("r did not reopen the rename editor")
	}
	if h.status != "" || h.statusErr {
		t.Fatalf("reopening the editor kept the previous outcome %q (statusErr=%v)", h.status, h.statusErr)
	}
	if content := h.View().Content; strings.Contains(content, "Rename failed") {
		t.Fatalf("the stale failure is still on screen:\n%s", content)
	}
}

func TestHomeRenameEscapeRestoresTheOriginalTitle(t *testing.T) {
	h, stub := renamingHome(t, nil)
	h.rename.input.SetValue("half-typed replacement")

	model, cmd := h.handleKey(specialKeyPress(tea.KeyEsc))
	h = homeFromKey(t, model)

	if cmd != nil {
		t.Fatal("esc scheduled a command")
	}
	if h.rename.Active() {
		t.Fatal("esc left the editor open")
	}
	if len(stub.reqs) != 0 {
		t.Fatalf("esc issued %d writes, want 0", len(stub.reqs))
	}
	content := h.View().Content
	if !strings.Contains(content, "Add dark mode") {
		t.Fatalf("esc did not leave the original title on the row:\n%s", content)
	}
	if strings.Contains(content, "half-typed replacement") {
		t.Fatalf("the discarded edit is still on screen:\n%s", content)
	}
}

// TestHomePasteReachesTheRenameInput covers bracketed paste, which is not a
// KeyMsg and so has to be forwarded by its own arm in Update or a pasted title
// is silently dropped. Off the rename path it must be equally silently ignored.
func TestHomePasteReachesTheRenameInput(t *testing.T) {
	t.Run("while renaming", func(t *testing.T) {
		h, _ := renamingHome(t, nil)
		h.rename.input.SetValue("")

		model, _ := h.Update(pasteText("Pasted title"))
		h = homeFromKey(t, model)

		if got := h.rename.Value(); got != "Pasted title" {
			t.Fatalf("rename value = %q, want the pasted text", got)
		}
	})

	t.Run("with no rename active", func(t *testing.T) {
		h := renameKeyHome(t)
		before := h.View().Content

		model, cmd := h.Update(pasteText("Pasted title"))
		h = homeFromKey(t, model)

		if cmd != nil {
			t.Fatal("a stray paste scheduled a command")
		}
		if h.rename.Active() {
			t.Fatal("a stray paste opened the rename editor")
		}
		if got := h.View().Content; got != before {
			t.Fatalf("a stray paste changed the board:\n%s", got)
		}
	})
}

// TestHomeRenamePromptFitsTheTerminalHeight is the reservation contract: the
// editor is one line taller than the action bar it replaces, so tableHeight has
// to give a row back or the board grows every time the operator presses r and
// the bottom of the list scrolls off.
//
// The bound that bites is RELATIVE, not the absolute `<= height`. Home's
// overhead formula over-reserves by a constant handful of lines (the footers'
// bottom padding, among others), so at any terminal size the rendered board
// sits several lines inside the terminal — measured 19 at height 24 — and an
// absolute assertion has enough slack to swallow a one-row error silently. The
// renaming board is compared against the same board's action-bar height
// instead, which has no slack in it. The absolute bound is asserted too, since
// a board that outgrows the terminal is the failure this is ultimately about.
//
// Falsification: delete the `case h.rename.Active():` arm from tableHeight
// (home_table.go) so the editor is reserved the action bar's single line, and
// "while renaming" fails with a board one line taller than the baseline. This
// was performed once and the arm restored.
func TestHomeRenamePromptFitsTheTerminalHeight(t *testing.T) {
	const (
		width  = 120
		height = 24
	)

	build := func(t *testing.T) HomeModel {
		t.Helper()
		h := NewHomeModel(nil, context.Background(), nil)
		h.loading = false
		h.repoCount = 1
		// Far more sessions than fit, so the terminal height is what bounds the
		// table and the reservation arithmetic is load-bearing.
		for i := range 100 {
			h.sessions = append(h.sessions, &pb.Session{
				Id:              fmt.Sprintf("sess-%d", i),
				Title:           fmt.Sprintf("Session %d", i),
				RepoDisplayName: "bossanova",
			})
		}
		return homeFromKey(t, mustModel(h.Update(tea.WindowSizeMsg{Width: width, Height: height})))
	}

	baseline := lipgloss.Height(build(t).View().Content)

	t.Run("baseline", func(t *testing.T) {
		if baseline > height {
			t.Fatalf("the board renders %d lines at height %d:\n%s", baseline, height, build(t).View().Content)
		}
	})

	t.Run("while renaming", func(t *testing.T) {
		h := homeFromKey(t, mustModel(build(t).handleKey(keyPress('r'))))
		if !h.rename.Active() {
			t.Fatal("r did not open the rename editor")
		}

		content := h.View().Content
		got := lipgloss.Height(content)
		if got > baseline {
			t.Fatalf("opening the editor grew the board from %d lines to %d; tableHeight did not give the row back:\n%s", baseline, got, content)
		}
		if got > height {
			t.Fatalf("the renaming board renders %d lines at height %d:\n%s", got, height, content)
		}
	})

	// The other half of the reservation, and the half the subtests above cannot
	// see: they run on a board whose status line is EMPTY, so the layout that
	// exists after a rename lands is the untested one. The status is rendered
	// between the table and the footer and wraps at statusWrapWidth, and a
	// session title is operator-supplied text of no bounded length — so the
	// confirmation can cost several rows, none of which tableHeight reserved
	// before BOS-837's follow-up.
	//
	// Falsification: drop `h.statusHeight()` from tableHeight's overhead
	// (home_table.go) and this fails with a board taller than the baseline by
	// the number of rows the status wrapped to. Performed once, then restored.
	t.Run("after a rename lands", func(t *testing.T) {
		long := strings.Repeat("a very long session title ", 8)
		h := homeFromKey(t, mustModel(build(t).Update(sessionRenamedMsg{
			session: &pb.Session{Id: "sess-0", Title: long},
		})))

		if h.status == "" {
			t.Fatal("fixture produced no status line, so the reservation under test is not exercised")
		}
		// Positive control: a one-line status would make this pass against a
		// hard-coded reservation of 1 and prove nothing about the measurement.
		if lines := h.statusHeight(); lines < 2 {
			t.Fatalf("fixture status wrapped to %d line(s); the test needs a multi-line status to bite", lines)
		}

		content := h.View().Content
		got := lipgloss.Height(content)
		if got > baseline {
			t.Fatalf("the confirmed rename grew the board from %d lines to %d; tableHeight did not reserve the status:\n%s", baseline, got, content)
		}
		if got > height {
			t.Fatalf("the board renders %d lines at height %d after a rename:\n%s", got, height, content)
		}
	})
}

// TestHomeNeverAdvertisesTheRenameKey is the hidden half of BOS-837: the
// shortcut is deliberately undocumented on screen, so none of Home's three
// action bars may mention it. Asserted on the rendered view rather than on the
// action slices, because a bar assembled somewhere else would slip past a
// slice-level check.
func TestHomeNeverAdvertisesTheRenameKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) HomeModel
	}{
		{"no repositories", func(t *testing.T) HomeModel {
			h := NewHomeModel(nil, context.Background(), nil)
			h.loading = false
			h.width = 120
			h.height = 30
			return h
		}},
		{"no sessions", func(t *testing.T) HomeModel {
			h := NewHomeModel(nil, context.Background(), nil)
			h.loading = false
			h.repoCount = 1
			h.width = 120
			h.height = 30
			return h
		}},
		{"session list", renameKeyHome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.build(t).View().Content
			if strings.Contains(strings.ToLower(content), "rename") {
				t.Fatalf("the action bar advertises the hidden shortcut:\n%s", content)
			}
		})
	}
}

func TestHomeRenameFooterReplacesTheActionBar(t *testing.T) {
	h, _ := renamingHome(t, nil)

	content := h.View().Content
	for _, want := range []string{"[enter] rename", "[esc] cancel"} {
		if !strings.Contains(content, want) {
			t.Fatalf("the editor does not offer %q:\n%s", want, content)
		}
	}
	// The action bar's keys are not live while the editor has the keyboard, so
	// leaving it on screen would advertise keys that now type letters.
	if strings.Contains(content, "[n]ew session") {
		t.Fatalf("the action bar is still on screen under the editor:\n%s", content)
	}
}
