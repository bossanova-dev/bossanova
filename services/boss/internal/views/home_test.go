package views

import (
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
			name: "review requested renders yellow",
			session: &pb.Session{
				AttentionStatus: &pb.AttentionStatus{
					NeedsAttention: true,
					Reason:         pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED,
				},
			},
			want: styleStatusWarning.Render("!"),
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
			Reason:         pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED,
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
