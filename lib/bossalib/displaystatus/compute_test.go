package displaystatus

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCompute(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want Output
	}{
		// --- Precedence (ported from sessionStatus.test.ts) ---
		{
			name: "chat QUESTION wins over everything",
			in: Input{
				Session: &pb.Session{
					DisplayStatus:      pb.DisplayStatus_DISPLAY_STATUS_MERGED,
					DisplayIsRepairing: true,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "chat WORKING wins over PR status",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
		{
			name: "workflow RUNNING with leg/max",
			in: Input{
				Session: &pb.Session{
					WorkflowDisplayStatus:  pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
					WorkflowDisplayLeg:     2,
					WorkflowDisplayMaxLegs: 5,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "running 2/5", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "workflow PENDING shows pending with spinner",
			in: Input{
				Session:    &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_PENDING},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "pending", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "workflow PAUSED with leg/max, warning, no spinner",
			in: Input{
				Session: &pb.Session{
					WorkflowDisplayStatus:  pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
					WorkflowDisplayLeg:     1,
					WorkflowDisplayMaxLegs: 4,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "paused 1/4", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "workflow FAILED with leg/max, danger, no spinner",
			in: Input{
				Session: &pb.Session{
					WorkflowDisplayStatus:  pb.WorkflowStatus_WORKFLOW_STATUS_FAILED,
					WorkflowDisplayLeg:     3,
					WorkflowDisplayMaxLegs: 5,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "failed 3/5", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "workflow CANCELLED, muted, no spinner",
			in: Input{
				Session:    &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "cancelled", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "workflow wins over PR status when both set",
			in: Input{
				Session: &pb.Session{
					WorkflowDisplayStatus:  pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
					WorkflowDisplayLeg:     1,
					WorkflowDisplayMaxLegs: 3,
					DisplayStatus:          pb.DisplayStatus_DISPLAY_STATUS_PASSING,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "running 1/3", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "repairing wins over PR status",
			in: Input{
				Session: &pb.Session{
					DisplayIsRepairing: true,
					DisplayStatus:      pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
			},
			want: Output{Label: "repairing", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},

		// --- PR DisplayStatus matrix ---
		{
			name: "PR CHECKING default warning + spinner",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},
		{
			name: "PR CHECKING with failures becomes danger",
			in: Input{
				Session: &pb.Session{
					DisplayStatus:      pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
					DisplayHasFailures: true,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "PR CHECKING with changes-requested becomes danger",
			in: Input{
				Session: &pb.Session{
					DisplayStatus:              pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
					DisplayHasChangesRequested: true,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "PR DRAFT muted",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "draft", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "PR PASSING success",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS},
		},
		{
			name: "PR FAILING danger",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "⨯ failing", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "PR CONFLICT danger",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "conflict", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "PR REJECTED danger",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_REJECTED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "⨯ rejected", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "PR APPROVED success",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_APPROVED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✓ approved", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS},
		},
		{
			name: "PR MERGED muted",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✔ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "PR CLOSED muted",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "closed", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},

		// --- Fallbacks ---
		{
			name: "fallback to idle when chat IDLE and no PR/workflow",
			in: Input{
				Session:    &pb.Session{},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
			},
			want: Output{Label: "idle", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "fallback to stopped when nothing applies",
			in: Input{
				Session:    &pb.Session{},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "fallback to stopped when chat UNSPECIFIED",
			in: Input{
				Session:    &pb.Session{},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_UNSPECIFIED,
			},
			want: Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "nil Session is safe; falls back to chat-status-driven output",
			in: Input{
				Session:    nil,
				ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
			},
			want: Output{Label: "idle", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "nil Session with stopped chat falls back to stopped",
			in: Input{
				Session:    nil,
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "nil Session still respects chat WORKING precedence",
			in: Input{
				Session:    nil,
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.in)
			if got != tt.want {
				t.Errorf("Compute() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
