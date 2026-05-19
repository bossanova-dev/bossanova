package views

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/upgrade"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

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
				RepoDisplayName:        "wondercanvas",
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
				RepoDisplayName: "wondercanvas",
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

func TestHomeBuildTableRows_ShowsAgentAfterNameWhenMultipleAgentsPresent(t *testing.T) {
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

	rows := h.table.Rows()
	if got := rows[0][4]; got != "claude" {
		t.Fatalf("session row AGENT column = %q, want claude", got)
	}
	if got := rows[1][4]; got != "codex" {
		t.Fatalf("session row AGENT column = %q, want codex", got)
	}
}

func TestHomeBuildTableRows_ShowsAgentWhenMultipleAgentsAvailable(t *testing.T) {
	h := HomeModel{
		availableAgents: []client.AgentInfo{{Name: "claude"}, {Name: "codex"}},
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

	rows := h.table.Rows()
	if got := rows[0][4]; got != "codex" {
		t.Fatalf("session row AGENT column = %q, want codex", got)
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

// TestHomeKeyDispatch_Regression verifies that all home-list keybindings
// dispatch the correct switchViewMsg, and that adding [c]ron did not break
// any existing binding (n/p/r/s/t/l).
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
