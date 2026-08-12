package displaystatus

import (
	"errors"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
)

func TestQuestionLabel(t *testing.T) {
	if QuestionLabel != "? question" {
		t.Fatalf("QuestionLabel = %q, want %q", QuestionLabel, "? question")
	}
	if !IsQuestionLabel(QuestionLabel) {
		t.Fatalf("IsQuestionLabel(%q) = false, want true", QuestionLabel)
	}
	if IsQuestionLabel("? PR failed") {
		t.Fatal("IsQuestionLabel(? PR failed) = true, want false")
	}

	got := Compute(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION})
	if got.Label != QuestionLabel {
		t.Fatalf("Compute(QUESTION).Label = %q, want %q", got.Label, QuestionLabel)
	}
}

func TestCompute(t *testing.T) {
	// blockedPRFailure is a draft-PR-creation-failure BlockedReason, built the
	// same way as TestComputeDraftPRFailureShowsWarningWithoutSpinner. Paired
	// with State=BLOCKED it exercises the errored-recolor overlay turning the
	// normally-WARNING "? PR failed" label DANGER.
	blockedPRFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))

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
			name: "chat LIMITED wins over PR status, warning, no spinner (no reset → fallback label)",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			want: Output{Label: "usage-limited", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "chat LIMITED with reset time composes resets ~HH:MM",
			in: Input{
				Session:     &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
				ChatStatus:  pb.ChatStatus_CHAT_STATUS_LIMITED,
				ChatResetAt: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC),
			},
			want: Output{Label: "usage-limited (resets ~15:00)", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "chat LIMITED with zero reset falls back to bare usage-limited",
			in: Input{
				Session:     &pb.Session{DisplaySettingUp: true},
				ChatStatus:  pb.ChatStatus_CHAT_STATUS_LIMITED,
				ChatResetAt: time.Time{},
			},
			// SettingUp normally shows "initializing"; LIMITED ranks above it,
			// mirroring how QUESTION ranks above SettingUp.
			want: Output{Label: "usage-limited", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "QUESTION outranks LIMITED even with a reset time set",
			in: Input{
				Session:     &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
				ChatStatus:  pb.ChatStatus_CHAT_STATUS_QUESTION,
				ChatResetAt: time.Date(2026, 1, 2, 15, 0, 0, 0, time.UTC),
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "LIMITED outranks a WORKING-eligible session (draft PR) as usage-limited",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			want: Output{Label: "usage-limited", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "chat WORKING over PR conflict uses danger intent",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "chat WORKING over PR rejected uses danger intent",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_REJECTED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "chat WORKING over checking with requested changes uses danger intent",
			in: Input{
				Session: &pb.Session{
					DisplayStatus:              pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
					DisplayHasChangesRequested: true,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
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
			name: "PR REVIEW success",
			in: Input{
				Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_REVIEW},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✓ review", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS},
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
			want: Output{Label: "⨯ conflict", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
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
			want: Output{Label: "✓ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
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

		// --- DisplaySettingUp (initializing) ---
		{
			name: "setting up shows initializing with spinner and info intent",
			in: Input{
				Session: &pb.Session{DisplaySettingUp: true},
			},
			want: Output{Label: "initializing", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "chat QUESTION wins over initializing",
			in: Input{
				Session:    &pb.Session{DisplaySettingUp: true},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "initializing wins over a stale PR display status",
			in: Input{
				Session: &pb.Session{
					DisplaySettingUp: true,
					DisplayStatus:    pb.DisplayStatus_DISPLAY_STATUS_DRAFT,
				},
			},
			want: Output{Label: "initializing", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},

		// --- DisplayMerging (merging) ---
		{
			name: "merging shows merging with spinner and info intent",
			in: Input{
				Session: &pb.Session{DisplayMerging: true},
			},
			want: Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "merging wins over a passing PR display status",
			in: Input{
				Session: &pb.Session{
					DisplayMerging: true,
					DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_PASSING,
				},
			},
			want: Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "merging wins over an approved PR display status",
			in: Input{
				Session: &pb.Session{
					DisplayMerging: true,
					DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_APPROVED,
				},
			},
			want: Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "chat QUESTION wins over merging",
			in: Input{
				Session:    &pb.Session{DisplayMerging: true},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},

		// --- ArchivePending (archiving) ---
		{
			name: "archiving shows archiving with spinner and warning intent",
			in: Input{
				Session: &pb.Session{ArchivePending: true},
			},
			want: Output{Label: "archiving", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},
		{
			name: "archiving wins over a merged PR display status",
			in: Input{
				Session: &pb.Session{
					ArchivePending: true,
					DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_MERGED,
				},
			},
			want: Output{Label: "archiving", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},
		{
			name: "merging wins over archiving",
			in: Input{
				Session: &pb.Session{
					DisplayMerging: true,
					ArchivePending: true,
				},
			},
			want: Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "chat QUESTION wins over archiving",
			in: Input{
				Session:    &pb.Session{ArchivePending: true, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			// The archiving branch sits above WORKING, so an in-flight archive
			// wins over a live working chat (matches merging's placement).
			name: "archiving wins over a WORKING chat",
			in: Input{
				Session:    &pb.Session{ArchivePending: true},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "archiving", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},
		{
			// LIMITED ranks just below QUESTION and above archiving, so a
			// usage-limited chat still wins over an in-flight archive.
			name: "usage-limited chat wins over archiving",
			in: Input{
				Session:    &pb.Session{ArchivePending: true, DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED,
			},
			want: Output{Label: "usage-limited", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "merged without archive_pending stays merged",
			in: Input{
				Session: &pb.Session{
					ArchivePending: false,
					DisplayStatus:  pb.DisplayStatus_DISPLAY_STATUS_MERGED,
				},
			},
			want: Output{Label: "✓ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},

		// --- Errored recolor overlay (BOS-430) ---
		//
		// An orphaned/blocked session keeps its REAL underlying status label and
		// spinner (so a live working chat or a pending question is never hidden
		// behind a static "orphaned"), but its intent is recolored to DANGER so the
		// error stays visible. Honest-green is preserved: a dead run's bootstrap-only
		// passing/draft PR is shown in red, never green. The one exception is a
		// terminal muted PR label ("✓ merged" / "closed"), left MUTED.
		{
			name: "orphaned + draft PR recolors draft to danger, keeping real label",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_ORPHANED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT,
				},
			},
			// draft is normally MUTED → recolored DANGER.
			want: Output{Label: "draft", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "orphaned + passing PR recolors ✓ passing to danger (honest green: red check, never green)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_ORPHANED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
				},
			},
			want: Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "orphaned + stale WORKING chat recolors working to danger, keeping spinner",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "orphaned + QUESTION chat recolors ? question to danger (pending question not hidden)",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "orphaned + IDLE chat recolors idle to danger",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
			},
			want: Output{Label: "idle", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "orphaned + CHECKING PR recolors checking to danger, keeping spinner",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_ORPHANED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			// checking is normally WARNING+spinner → recolored DANGER.
			want: Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "orphaned + nothing recolors stopped to danger",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			// Guard: a merged PR is a legitimate terminal end state, so an orphaned
			// session whose real status is merged stays MUTED, not alarmed red.
			name: "orphaned + MERGED PR is NOT recolored (terminal muted end state)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_ORPHANED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✓ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "orphaned + CLOSED PR is NOT recolored (terminal muted end state)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_ORPHANED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "closed", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			// BLOCKED is symmetric with ORPHANED: a blocked session with a live
			// working chat has the identical hidden-state bug, so it is recolored too.
			name: "blocked + WORKING chat recolors working to danger, keeping spinner",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_BLOCKED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			// A blocked session's "? PR failed" is recolored WARNING→DANGER. This is
			// intended: a blocked session is an error state, so its real status is
			// alarmed red even though the standalone draft-PR-failure label is WARNING
			// (see TestComputeDraftPRFailureShowsWarningWithoutSpinner, State=IMPLEMENTING_PLAN).
			name: "blocked + draft-PR-failure recolors ? PR failed to danger",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					BlockedReason: &blockedPRFailure,
				},
			},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "blocked + QUESTION chat recolors ? question to danger (pending question not hidden)",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_BLOCKED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION,
			},
			want: Output{Label: "? question", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "blocked + IDLE chat recolors idle to danger",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_BLOCKED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
			},
			want: Output{Label: "idle", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "blocked + CHECKING PR recolors checking to danger, keeping spinner",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "checking", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true},
		},
		{
			name: "blocked + passing PR recolors ✓ passing to danger (honest green: red check, never green)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
				},
			},
			want: Output{Label: "✓ passing", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "blocked + draft PR recolors draft to danger, keeping real label",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT,
				},
			},
			want: Output{Label: "draft", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "blocked + nothing recolors stopped to danger",
			in: Input{
				Session:    &pb.Session{State: pb.SessionState_SESSION_STATE_BLOCKED},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "stopped", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			// Guard, symmetric with the orphaned cases: the muted-terminal exemption
			// (isMutedTerminalPR) must hold on the BLOCKED path too. A regression that
			// recolored a blocked + merged/closed session red would otherwise slip
			// through, since errored() treats BLOCKED and ORPHANED identically.
			name: "blocked + MERGED PR is NOT recolored (terminal muted end state)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "✓ merged", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
		},
		{
			name: "blocked + CLOSED PR is NOT recolored (terminal muted end state)",
			in: Input{
				Session: &pb.Session{
					State:         pb.SessionState_SESSION_STATE_BLOCKED,
					DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED,
				},
				ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED,
			},
			want: Output{Label: "closed", Intent: pb.DisplayIntent_DISPLAY_INTENT_MUTED},
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

func TestComputeDraftPRFailureShowsWarningWithoutSpinner(t *testing.T) {
	reason := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))
	got := Compute(Input{
		Session: &pb.Session{
			Id:            "sess-1",
			Title:         "Open missing PR",
			State:         pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			BranchName:    "open-missing-pr",
			BlockedReason: &reason,
		},
	})

	if got.Label != "? PR failed" {
		t.Fatalf("Label = %q, want %q", got.Label, "? PR failed")
	}
	if got.Intent != pb.DisplayIntent_DISPLAY_INTENT_WARNING {
		t.Fatalf("Intent = %v, want WARNING", got.Intent)
	}
	if got.Spinner {
		t.Fatal("Spinner = true, want false")
	}
}

// TestPreErroredOutputInvertsRecolor locks the apiversion down-convert
// (ErroredStatusChange, V20260718): for every base cascade branch, feeding a
// served (post-BOS-430) errored Session back through PreErroredOutput must
// reproduce the exact pre-BOS-430 Output — for ORPHANED the fixed
// "orphaned"/DANGER short-circuit, for BLOCKED the un-recolored base cascade.
// Iterating every branch means the reuse of prOutput/workflowOutput plus the
// fixed-intent switch in preErroredBlockedIntent cannot drift from the cascade
// without failing here.
func TestPreErroredOutputInvertsRecolor(t *testing.T) {
	reset := time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC)
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))

	// Each case is the non-state part of an Input exercising one base branch.
	// The loop runs it under both errored states.
	cases := []struct {
		name string
		in   Input
	}{
		{"question", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION}},
		{"limited-no-reset", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED}},
		{"limited-reset", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED, ChatResetAt: reset}},
		{"pr-failed", Input{Session: &pb.Session{BlockedReason: &prFailure}}},
		{"initializing", Input{Session: &pb.Session{DisplaySettingUp: true}}},
		{"merging", Input{Session: &pb.Session{DisplayMerging: true}}},
		{"archiving", Input{Session: &pb.Session{ArchivePending: true}}},
		{"working-ok", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING}},
		{"working-needsfix", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING}},
		{"workflow-running", Input{Session: &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING, WorkflowDisplayLeg: 2, WorkflowDisplayMaxLegs: 4}}},
		{"workflow-pending", Input{Session: &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_PENDING}}},
		{"workflow-paused", Input{Session: &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED, WorkflowDisplayLeg: 1, WorkflowDisplayMaxLegs: 3}}},
		{"workflow-failed", Input{Session: &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_FAILED, WorkflowDisplayLeg: 2, WorkflowDisplayMaxLegs: 3}}},
		{"workflow-cancelled", Input{Session: &pb.Session{WorkflowDisplayStatus: pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED}}},
		{"repairing", Input{Session: &pb.Session{DisplayIsRepairing: true}}},
		{"pr-merged", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_MERGED}}},
		{"pr-closed", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CLOSED}}},
		{"pr-approved", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_APPROVED}}},
		{"pr-passing", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}}},
		{"pr-review", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_REVIEW}}},
		{"pr-failing", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING}}},
		{"pr-conflict", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT}}},
		{"pr-rejected", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_REJECTED}}},
		{"pr-draft", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_DRAFT}}},
		{"pr-checking-clean", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING}}},
		{"pr-checking-failures", Input{Session: &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CHECKING, DisplayHasFailures: true}}},
		{"idle", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}},
		{"stopped", Input{Session: &pb.Session{}, ChatStatus: pb.ChatStatus_CHAT_STATUS_STOPPED}},
	}

	states := []struct {
		name  string
		state pb.SessionState
	}{
		{"orphaned", pb.SessionState_SESSION_STATE_ORPHANED},
		{"blocked", pb.SessionState_SESSION_STATE_BLOCKED},
	}

	for _, st := range states {
		for _, tc := range cases {
			t.Run(st.name+"/"+tc.name, func(t *testing.T) {
				in := tc.in
				in.Session.State = st.state

				// Pre-BOS-430 behavior: ORPHANED short-circuited to a fixed
				// tuple; BLOCKED had no overlay, so it equals the base cascade.
				wantOld := baseStatus(in)
				if st.state == pb.SessionState_SESSION_STATE_ORPHANED {
					wantOld = Output{Label: "orphaned", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER}
				}

				// Simulate the served Session: bossd writes Compute's output onto
				// the Session's Display* fields, exactly what bosso then serves.
				served := Compute(in)
				in.Session.DisplayLabel = served.Label
				in.Session.DisplayIntent = served.Intent
				in.Session.DisplaySpinner = served.Spinner

				got := PreErroredOutput(in.Session)
				if got != wantOld {
					t.Errorf("PreErroredOutput() = %+v, want %+v (served %+v)", got, wantOld, served)
				}
			})
		}
	}
}

// TestPreErroredOutputEmptyLabelUnchanged verifies an errored session whose
// display was never computed (empty label) is returned unchanged — there is
// nothing to invert.
func TestPreErroredOutputEmptyLabelUnchanged(t *testing.T) {
	sess := &pb.Session{State: pb.SessionState_SESSION_STATE_ORPHANED}
	got := PreErroredOutput(sess)
	if got != (Output{}) {
		t.Errorf("PreErroredOutput() = %+v, want zero Output", got)
	}
}

// ComputeBase omits the errored-recolor overlay: a BLOCKED session that Compute
// recolors DANGER keeps its base intent under ComputeBase, while for a
// non-errored session the two agree.
func TestComputeBaseOmitsErroredOverlay(t *testing.T) {
	blocked := Input{
		Session: &pb.Session{
			State:         pb.SessionState_SESSION_STATE_BLOCKED,
			DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
		},
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	}
	if got := Compute(blocked); got.Intent != pb.DisplayIntent_DISPLAY_INTENT_DANGER {
		t.Fatalf("Compute() intent = %v, want DANGER (overlay applied)", got.Intent)
	}
	base := ComputeBase(blocked)
	if base.Label != "✓ passing" {
		t.Errorf("ComputeBase() label = %q, want ✓ passing", base.Label)
	}
	if base.Intent != pb.DisplayIntent_DISPLAY_INTENT_SUCCESS {
		t.Errorf("ComputeBase() intent = %v, want SUCCESS (no recolor)", base.Intent)
	}

	// For a non-errored session ComputeBase and Compute must agree.
	normal := Input{
		Session:    &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE,
	}
	if ComputeBase(normal) != Compute(normal) {
		t.Errorf("ComputeBase(%+v) = %+v, want equal to Compute() %+v", normal, ComputeBase(normal), Compute(normal))
	}
}

// --- BOS-668: waiting on an external event ---

func TestCallbackWaitingReason_CanonicalWording(t *testing.T) {
	got := CallbackWaitingReason("checks_passed_ready", "acme", "widget", 123)
	const want = "awaiting checks_passed_ready on acme/widget#123"
	if got != want {
		t.Fatalf("CallbackWaitingReason = %q, want %q", got, want)
	}
}

func TestCallbackWaitingReason_IncompleteInputYieldsNoReason(t *testing.T) {
	cases := []struct {
		name                    string
		trigger, owner, repoNam string
		pr                      int
	}{
		{name: "no trigger", owner: "acme", repoNam: "widget", pr: 1},
		{name: "no owner", trigger: "merged", repoNam: "widget", pr: 1},
		{name: "no name", trigger: "merged", owner: "acme", pr: 1},
		{name: "no pr", trigger: "merged", owner: "acme", repoNam: "widget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CallbackWaitingReason(tc.trigger, tc.owner, tc.repoNam, tc.pr); got != "" {
				t.Fatalf("CallbackWaitingReason = %q, want empty", got)
			}
		})
	}
}

func TestBaseStatus_WaitingChatStatus(t *testing.T) {
	got := Compute(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING})
	want := Output{Label: WaitingLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true}
	if got != want {
		t.Fatalf("Compute(WAITING) = %+v, want %+v", got, want)
	}
	if WaitingLabel != "waiting" {
		t.Fatalf("WaitingLabel = %q, want waiting", WaitingLabel)
	}
}

func TestBaseStatus_WaitingLosesToQuestionAndLimited(t *testing.T) {
	// The chat-status cascade already resolves a single winning status before
	// Compute is called; these assert the label ordering inside baseStatus so a
	// future reorder cannot silently promote waiting above a human-action state.
	if got := Compute(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION}); got.Label != QuestionLabel {
		t.Fatalf("QUESTION = %q, want %q", got.Label, QuestionLabel)
	}
	if got := Compute(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED}); got.Label != "usage-limited" {
		t.Fatalf("LIMITED = %q, want usage-limited", got.Label)
	}
}

func TestBaseStatus_WaitingWinsOverPRDerivedLabels(t *testing.T) {
	// Waiting sits exactly where working sat: above the workflow/PR-derived
	// labels, so a parked chat does not fall back to a stale "✓ passing".
	sess := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}
	if got := Compute(Input{Session: sess, ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING}); got.Label != WaitingLabel {
		t.Fatalf("waiting over passing PR = %q, want %q", got.Label, WaitingLabel)
	}
	// ...but the transient in-flight overrides still win, exactly as for working.
	setup := &pb.Session{DisplaySettingUp: true}
	if got := Compute(Input{Session: setup, ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING}); got.Label != "initializing" {
		t.Fatalf("initializing over waiting = %q, want initializing", got.Label)
	}
}

func TestPreErroredBlockedIntent_WaitingIsInfo(t *testing.T) {
	sess := &pb.Session{
		State:         pb.SessionState_SESSION_STATE_BLOCKED,
		DisplayLabel:  WaitingLabel,
		DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_DANGER,
	}
	got := PreErroredOutput(sess)
	if got.Intent != pb.DisplayIntent_DISPLAY_INTENT_INFO {
		t.Fatalf("PreErroredOutput(waiting).Intent = %v, want INFO", got.Intent)
	}
}

// TestBaseStatus_DraftPRFailureLosesToLiveActivity pins BOS-855's precedence
// rule at the cascade level: a draft-PR-creation failure is a PAST outcome, so
// it must not claim the row's primary label while a chat is live. The
// "? PR failed" branch therefore sits immediately BELOW the WORKING branch and
// above the workflow branch — every live-activity label wins, every non-live
// state still falls through to "? PR failed".
func TestBaseStatus_DraftPRFailureLosesToLiveActivity(t *testing.T) {
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))
	reset := time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		in   Input
		want Output
	}{
		{
			name: "working chat wins the label",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
		{
			name: "waiting chat wins the label",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING},
			want: Output{Label: WaitingLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "initializing wins the label",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure, DisplaySettingUp: true}},
			want: Output{Label: "initializing", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "merging wins the label",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure, DisplayMerging: true}},
			want: Output{Label: "merging", Intent: pb.DisplayIntent_DISPLAY_INTENT_INFO, Spinner: true},
		},
		{
			name: "archiving wins the label",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure, ArchivePending: true}},
			want: Output{Label: "archiving", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING, Spinner: true},
		},
		{
			// The branches ABOVE the moved one must be undisturbed.
			name: "question still wins",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION},
			want: Output{Label: QuestionLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "usage-limited still wins",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED, ChatResetAt: reset},
			want: Output{Label: "usage-limited (resets ~09:30)", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			// Nothing live: the past outcome is still the most informative label.
			name: "idle chat still reads ? PR failed",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "no chat status still reads ? PR failed",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure}},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			// A workflow is not live activity by the row's own reckoning here: the
			// moved branch sits ABOVE the workflow branch, so "? PR failed" wins.
			name: "active workflow still reads ? PR failed",
			in: Input{Session: &pb.Session{
				BlockedReason:          &prFailure,
				WorkflowDisplayStatus:  pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
				WorkflowDisplayLeg:     2,
				WorkflowDisplayMaxLegs: 5,
			}},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "repairing still reads ? PR failed",
			in:   Input{Session: &pb.Session{BlockedReason: &prFailure, DisplayIsRepairing: true}},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compute(tt.in); got != tt.want {
				t.Errorf("Compute() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestCompute_DraftPRFailureWorkingOnErroredSessionIsDanger pins the interaction
// with BOS-430: a BLOCKED session whose chat is working now reads "working", and
// the errored overlay still recolors it DANGER so the row stays alarming.
func TestCompute_DraftPRFailureWorkingOnErroredSessionIsDanger(t *testing.T) {
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))
	got := Compute(Input{
		Session: &pb.Session{
			State:         pb.SessionState_SESSION_STATE_BLOCKED,
			BlockedReason: &prFailure,
		},
		ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING,
	})
	want := Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER, Spinner: true}
	if got != want {
		t.Fatalf("Compute() = %+v, want %+v", got, want)
	}
}

// TestIsLiveActivityLabel_Classification pins the BOS-855 predicate against the
// exact label list both clients mirror. Every live label is present tense;
// everything else — including the empty string and an unrecognised label — is
// not, so the recessive treatment can never be applied to a row whose label this
// package does not understand.
func TestIsLiveActivityLabel_Classification(t *testing.T) {
	live := []string{
		QuestionLabel,
		"usage-limited",
		"usage-limited (resets ~09:30)",
		WaitingLabel,
		"working",
		"initializing",
		"merging",
		"archiving",
		"repairing",
		"pending",
		"running 1/5",
		"running 12/12",
	}
	notLive := []string{
		"? PR failed",
		"paused 2/5",
		"failed 3/5",
		"cancelled",
		labelMerged,
		labelClosed,
		"✓ approved",
		"✓ passing",
		"✓ review",
		"⨯ failing",
		"⨯ conflict",
		"⨯ rejected",
		"draft",
		"checking",
		"idle",
		"stopped",
		"",
		"orphaned",
		"something nobody has written yet",
		// Near misses: the two prefix rules must not fire on a bare word or on
		// an unrelated label that merely starts with the same letters.
		"running",
		"runningfast",
		"usage",
	}
	for _, label := range live {
		if !IsLiveActivityLabel(label) {
			t.Errorf("IsLiveActivityLabel(%q) = false, want true", label)
		}
	}
	for _, label := range notLive {
		if IsLiveActivityLabel(label) {
			t.Errorf("IsLiveActivityLabel(%q) = true, want false", label)
		}
	}
}

// TestIsLiveActivityLabel_CoversEveryCascadeLabel is the anti-rot guard: it
// enumerates every label baseStatus, prOutput and workflowOutput can emit by
// DRIVING them, and fails when one is classified in neither the live nor the
// non-live set. Adding a cascade branch without deciding its liveness reds here
// rather than silently defaulting to "not live" in production.
func TestIsLiveActivityLabel_CoversEveryCascadeLabel(t *testing.T) {
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("gh pr create failed"))
	reset := time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC)

	// The declared classification. Every label the producers emit must appear
	// here exactly once.
	classified := map[string]bool{
		QuestionLabel:                   true,
		"usage-limited":                 true,
		"usage-limited (resets ~09:30)": true,
		WaitingLabel:                    true,
		"working":                       true,
		"initializing":                  true,
		"merging":                       true,
		"archiving":                     true,
		"repairing":                     true,
		"pending":                       true,
		"running 2/5":                   true,
		"? PR failed":                   false,
		"paused 2/5":                    false,
		"failed 2/5":                    false,
		"cancelled":                     false,
		labelMerged:                     false,
		labelClosed:                     false,
		"✓ approved":                    false,
		"✓ passing":                     false,
		"✓ review":                      false,
		"⨯ failing":                     false,
		"⨯ conflict":                    false,
		"⨯ rejected":                    false,
		"draft":                         false,
		"checking":                      false,
		"idle":                          false,
		"stopped":                       false,
	}

	// Drive every producer branch. workflow/PR cases are enumerated over their
	// enums so a NEW enum value that produces a label lands here automatically.
	emitted := make(map[string]struct{})
	add := func(out Output) { emitted[out.Label] = struct{}{} }

	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION}))
	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED}))
	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED, ChatResetAt: reset}))
	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING}))
	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING}))
	add(baseStatus(Input{ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}))
	add(baseStatus(Input{}))
	add(baseStatus(Input{Session: &pb.Session{DisplaySettingUp: true}}))
	add(baseStatus(Input{Session: &pb.Session{DisplayMerging: true}}))
	add(baseStatus(Input{Session: &pb.Session{ArchivePending: true}}))
	add(baseStatus(Input{Session: &pb.Session{DisplayIsRepairing: true}}))
	add(baseStatus(Input{Session: &pb.Session{BlockedReason: &prFailure}}))
	for _, ws := range []pb.WorkflowStatus{
		pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		pb.WorkflowStatus_WORKFLOW_STATUS_PENDING,
		pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
		pb.WorkflowStatus_WORKFLOW_STATUS_FAILED,
		pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED,
	} {
		out, ok := workflowOutput(&pb.Session{WorkflowDisplayStatus: ws, WorkflowDisplayLeg: 2, WorkflowDisplayMaxLegs: 5})
		if !ok {
			t.Fatalf("workflowOutput(%v) reported no output", ws)
		}
		add(out)
	}
	for name, value := range pb.DisplayStatus_value {
		out, ok := prOutput(&pb.Session{DisplayStatus: pb.DisplayStatus(value)})
		if !ok {
			continue // UNSPECIFIED and any future non-label value
		}
		if out.Label == "" {
			t.Errorf("prOutput(%s) produced an empty label", name)
		}
		add(out)
	}

	for label := range emitted {
		want, ok := classified[label]
		if !ok {
			t.Errorf("cascade emits label %q which IsLiveActivityLabel's test table does not classify — decide whether it is live activity and add it here and to the predicate", label)
			continue
		}
		if got := IsLiveActivityLabel(label); got != want {
			t.Errorf("IsLiveActivityLabel(%q) = %v, want %v", label, got, want)
		}
	}
	for label := range classified {
		if _, ok := emitted[label]; !ok {
			t.Errorf("test table classifies %q but no cascade producer emits it — the table has drifted from the cascade", label)
		}
	}
}

// TestPreDraftPRFailureOutput_RestoresPreMoveComposite locks the BOS-855
// down-convert inverse: an older client must still see "? PR failed" for every
// state the moved branch used to outrank, and nothing else may be rewritten.
func TestPreDraftPRFailureOutput_RestoresPreMoveComposite(t *testing.T) {
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: gh pr create: authentication required"))
	otherReason := "blocked — needs human intervention"

	tests := []struct {
		name string
		sess *pb.Session
		want Output
	}{
		{
			name: "working restores ? PR failed",
			sess: &pb.Session{
				BlockedReason:  &prFailure,
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "waiting restores ? PR failed",
			sess: &pb.Session{
				BlockedReason:  &prFailure,
				DisplayLabel:   WaitingLabel,
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_INFO,
				DisplaySpinner: true,
			},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "blocked working restores ? PR failed recolored DANGER",
			sess: &pb.Session{
				State:          pb.SessionState_SESSION_STATE_BLOCKED,
				BlockedReason:  &prFailure,
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_DANGER,
				DisplaySpinner: true,
			},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_DANGER},
		},
		{
			name: "question is left alone",
			sess: &pb.Session{
				BlockedReason: &prFailure,
				DisplayLabel:  QuestionLabel,
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			want: Output{Label: QuestionLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			name: "usage-limited is left alone",
			sess: &pb.Session{
				BlockedReason: &prFailure,
				DisplayLabel:  "usage-limited (resets ~09:30)",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			want: Output{Label: "usage-limited (resets ~09:30)", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
		{
			// Keyed on the blocked reason, NOT on label == "working": a working
			// session with any other blocked reason is untouched.
			name: "other blocked reason is left alone",
			sess: &pb.Session{
				BlockedReason:  &otherReason,
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
		{
			name: "no blocked reason is left alone",
			sess: &pb.Session{
				DisplayLabel:   "working",
				DisplayIntent:  pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
				DisplaySpinner: true,
			},
			want: Output{Label: "working", Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, Spinner: true},
		},
		{
			name: "empty label is returned unchanged, not fabricated",
			sess: &pb.Session{BlockedReason: &prFailure},
			want: Output{},
		},
		{
			name: "nil session is returned unchanged",
			sess: nil,
			want: Output{},
		},
		{
			// Idle already reads "? PR failed" post-move — the inverse is a no-op.
			name: "already ? PR failed is idempotent",
			sess: &pb.Session{
				BlockedReason: &prFailure,
				DisplayLabel:  "? PR failed",
				DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_WARNING,
			},
			want: Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PreDraftPRFailureOutput(tt.sess); got != tt.want {
				t.Errorf("PreDraftPRFailureOutput() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestPreDraftPRFailureOutput_MatchesOldCascade proves the hand-written inverse
// against the behavior it claims to reproduce, rather than against a restated
// expectation: it reimplements the PRE-BOS-855 branch order inline and asserts
// the inverse agrees with it for every live state the move affected.
func TestPreDraftPRFailureOutput_MatchesOldCascade(t *testing.T) {
	prFailure := sessionreason.DraftPRCreationFailure(errors.New("gh pr create failed"))

	// oldCompute is the pre-BOS-855 cascade for a draft-PR-failure session: the
	// branch sat directly below QUESTION/LIMITED, so it beat everything else.
	oldCompute := func(in Input) Output {
		if in.ChatStatus == pb.ChatStatus_CHAT_STATUS_QUESTION ||
			in.ChatStatus == pb.ChatStatus_CHAT_STATUS_LIMITED {
			return Compute(in)
		}
		out := Output{Label: "? PR failed", Intent: pb.DisplayIntent_DISPLAY_INTENT_WARNING}
		if errored(in) {
			out.Intent = pb.DisplayIntent_DISPLAY_INTENT_DANGER
		}
		return out
	}

	inputs := []Input{
		{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
		{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WAITING},
		{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE},
		{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_QUESTION},
		{Session: &pb.Session{BlockedReason: &prFailure}, ChatStatus: pb.ChatStatus_CHAT_STATUS_LIMITED},
		{Session: &pb.Session{BlockedReason: &prFailure, DisplaySettingUp: true}},
		{Session: &pb.Session{BlockedReason: &prFailure, DisplayMerging: true}},
		{Session: &pb.Session{BlockedReason: &prFailure, ArchivePending: true}},
		{Session: &pb.Session{BlockedReason: &prFailure, State: pb.SessionState_SESSION_STATE_BLOCKED}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
		{Session: &pb.Session{BlockedReason: &prFailure, State: pb.SessionState_SESSION_STATE_ORPHANED}, ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
	}
	for i, in := range inputs {
		served := Compute(in)
		sess := in.Session
		sess.DisplayLabel, sess.DisplayIntent, sess.DisplaySpinner = served.Label, served.Intent, served.Spinner
		got := PreDraftPRFailureOutput(sess)
		if want := oldCompute(in); got != want {
			t.Errorf("input %d: PreDraftPRFailureOutput() = %+v, want (old cascade) %+v", i, got, want)
		}
	}
}
