package views

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeHomeCloudAccessClient struct {
	status      *pb.CloudAccessStatus
	statusErr   error
	checkoutURL string
	checkoutErr error
	portalURL   string
	portalErr   error
	refresh     *pb.CloudAccessStatus
	refreshErr  error
	returnURLs  []string
	cancelURLs  []string
	statusCalls int
	checkouts   int
	portals     int
	refreshes   int
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

func (f *fakeHomeCloudAccessClient) CreateBillingPortalSession(context.Context, string) (string, error) {
	f.portals++
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
	if !strings.Contains(content, "u upgrade") {
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
	if got := rows[0][4]; got != "-" {
		t.Fatalf("session row PR column = %q, want -", got)
	}
	if got := rows[1][4]; got != "-" {
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
	if got := rows[0][4]; got != "-" {
		t.Fatalf("session row PR column = %q, want -", got)
	}
}

func TestHomeModelBuildTableRows_ShowsArchivingStatusForMatchingSession(t *testing.T) {
	h := HomeModel{
		spinner:            newStatusSpinner(),
		archivingSessionID: "sess-1",
		sessions: []*pb.Session{
			{Id: "sess-1", RepoDisplayName: "repo", Title: "first"},
			{Id: "sess-2", RepoDisplayName: "repo", Title: "second"},
		},
	}

	h.buildTableRows()

	rows := h.table.Rows()
	if len(rows) != 2 {
		t.Fatalf("table rows = %d, want 2", len(rows))
	}
	if got := rows[0][5]; !strings.Contains(got, "archiving") {
		t.Fatalf("archiving session STATUS = %q, want archiving", got)
	}
	if got := rows[0][5]; strings.Contains(got, "  archiving") {
		t.Fatalf("archiving session STATUS = %q, want one space before archiving", got)
	}
	if got := rows[1][5]; strings.Contains(got, "archiving") {
		t.Fatalf("non-archiving session STATUS = %q, want normal status", got)
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

func TestSortSessionsByAttention(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "normal-1"},
		{Id: "attn-1", AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS,
		}},
		{Id: "normal-2"},
		{Id: "attn-2", AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
		}},
	}

	sortSessionsByAttention(sessions)

	// First two should be the attention sessions, preserving relative order.
	if sessions[0].Id != "attn-1" {
		t.Errorf("sessions[0].Id = %q, want %q", sessions[0].Id, "attn-1")
	}
	if sessions[1].Id != "attn-2" {
		t.Errorf("sessions[1].Id = %q, want %q", sessions[1].Id, "attn-2")
	}
	// Normal sessions follow, preserving relative order.
	if sessions[2].Id != "normal-1" {
		t.Errorf("sessions[2].Id = %q, want %q", sessions[2].Id, "normal-1")
	}
	if sessions[3].Id != "normal-2" {
		t.Errorf("sessions[3].Id = %q, want %q", sessions[3].Id, "normal-2")
	}
}

func TestSessionNeedsAttention(t *testing.T) {
	tests := []struct {
		name string
		sess *pb.Session
		want bool
	}{
		{"nil status", &pb.Session{}, false},
		{"false", &pb.Session{AttentionStatus: &pb.AttentionStatus{NeedsAttention: false}}, false},
		{"true", &pb.Session{AttentionStatus: &pb.AttentionStatus{NeedsAttention: true}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionNeedsAttention(tt.sess); got != tt.want {
				t.Errorf("sessionNeedsAttention() = %v, want %v", got, tt.want)
			}
		})
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

// TestHomeKeyDispatch_Regression verifies that all home-list keybindings
// dispatch the correct switchViewMsg, and that adding [c]ron did not break
// any existing binding (n/r/s/t/l).
func TestHomeKeyDispatch_Regression(t *testing.T) {
	// Build a HomeModel with one repo (so [n] is enabled) and auth configured
	// (so [l] is enabled). We drive Update() directly without a real daemon.
	authMgr := (*auth.Manager)(nil) // nil authMgr disables l; tested separately

	tests := []struct {
		key      string
		wantView View
	}{
		{"n", ViewNewSession},
		{"r", ViewRepoList},
		{"s", ViewSettings},
		{"t", ViewTrash},
		{"c", ViewCron},
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
	h := HomeModel{
		ctx:       context.Background(),
		authMgr:   &auth.Manager{},
		repoCount: 1,
		loading:   false,
		loggedIn:  false,
		sessions:  []*pb.Session{{Id: "sess-1", Title: "Active work"}},
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

func TestHomeModelUpdate_BlocksOpeningArchivingSession(t *testing.T) {
	h := HomeModel{
		ctx:                context.Background(),
		repoCount:          1,
		loading:            false,
		archivingSessionID: "sess-1",
		sessions:           []*pb.Session{{Id: "sess-1"}},
	}
	h.buildTableRows()

	_, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("key enter returned command for archiving session, want nil")
	}
}

func TestHomeModelUpdate_AllowsOpeningNonArchivingSession(t *testing.T) {
	h := HomeModel{
		ctx:                context.Background(),
		repoCount:          1,
		loading:            false,
		archivingSessionID: "sess-2",
		sessions: []*pb.Session{
			{Id: "sess-1"},
			{Id: "sess-2"},
		},
	}
	h.buildTableRows()

	_, cmd := h.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("key enter: got nil cmd, want switchViewMsg")
	}
	msg := cmd()
	svm, ok := msg.(switchViewMsg)
	if !ok {
		t.Fatalf("key enter: cmd() returned %T, want switchViewMsg", msg)
	}
	if svm.view != ViewChatPicker {
		t.Errorf("key enter: view = %v, want ViewChatPicker", svm.view)
	}
	if svm.sessionID != "sess-1" {
		t.Errorf("key enter: sessionID = %q, want %q", svm.sessionID, "sess-1")
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

	// Check for setup instructions
	if !strings.Contains(content, "Press 'r' to open the repos menu") {
		t.Errorf("expected setup instructions in empty state with no repos, got: %s", content)
	}

	// 'n' (new session) should not be offered when there are no repos
	if strings.Contains(content, "[n]ew session") {
		t.Errorf("should not offer [n]ew session when no repos exist, got: %s", content)
	}
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
}

func TestHomeUpgradeSuccessPromptsRestart(t *testing.T) {
	h := NewHomeModel(nil, context.Background(), nil)
	h.width = 100

	model, _ := h.Update(upgradeRunMsg{})
	h = model.(HomeModel)

	content := h.View().Content
	if !strings.Contains(content, "Upgrade installed") {
		t.Fatalf("expected upgrade-installed prompt, got: %s", content)
	}
	if !strings.Contains(content, "r restart") {
		t.Fatalf("expected restart action in prompt, got: %s", content)
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
