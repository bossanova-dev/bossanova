package views

import (
	"context"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
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
			want: lipgloss.NewStyle().Foreground(lipgloss.Color("#FF8C00")).Render("!"),
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
		ms   = "\x1b[38;2;98;98;98;9m"   // muted + strike open
		msc  = "\x1b[39;29m"             // muted + strike close
		msu  = "\x1b[38;2;98;98;98;9;4m" // muted + strike + underline open
		msuc = "\x1b[39;29;24m"          // muted + strike + underline close
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
