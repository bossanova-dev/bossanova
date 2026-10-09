package displaystatus

import (
	"errors"
	"fmt"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
)

func TestReadyPrecedenceMatrix(t *testing.T) {
	chats := []pb.ChatStatus{pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_WAITING, pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_STOPPED, pb.ChatStatus_CHAT_STATUS_QUESTION, pb.ChatStatus_CHAT_STATUS_LIMITED, pb.ChatStatus_CHAT_STATUS_UNSPECIFIED}
	prs := []pb.DisplayStatus{pb.DisplayStatus_DISPLAY_STATUS_PASSING, pb.DisplayStatus_DISPLAY_STATUS_APPROVED, pb.DisplayStatus_DISPLAY_STATUS_REVIEW, pb.DisplayStatus_DISPLAY_STATUS_CHECKING, pb.DisplayStatus_DISPLAY_STATUS_VERIFYING, pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN, pb.DisplayStatus_DISPLAY_STATUS_FAILING, pb.DisplayStatus_DISPLAY_STATUS_CONFLICT, pb.DisplayStatus_DISPLAY_STATUS_REJECTED, pb.DisplayStatus_DISPLAY_STATUS_DRAFT, pb.DisplayStatus_DISPLAY_STATUS_MERGED, pb.DisplayStatus_DISPLAY_STATUS_CLOSED}
	for _, chat := range chats {
		for _, pr := range prs {
			for _, idle := range []bool{false, true} {
				for _, receipt := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/idle=%t/receipt=%t", chat, pr, idle, receipt), func(t *testing.T) {
						sess := &pb.Session{DisplayStatus: pr}
						in := Input{Session: sess, ChatStatus: chat, AllWaitingChatsIdle: idle}
						before := Compute(in)
						demoted := WasWaitingDemoted(in, before)
						sess.HasBuildReceipt = receipt
						out := Compute(in)
						green := pr == pb.DisplayStatus_DISPLAY_STATUS_PASSING || pr == pb.DisplayStatus_DISPLAY_STATUS_APPROVED || pr == pb.DisplayStatus_DISPLAY_STATUS_REVIEW
						settled := chat == pb.ChatStatus_CHAT_STATUS_WAITING || chat == pb.ChatStatus_CHAT_STATUS_IDLE || chat == pb.ChatStatus_CHAT_STATUS_STOPPED || chat == pb.ChatStatus_CHAT_STATUS_UNSPECIFIED
						if receipt && green && settled {
							if out != (Output{Label: ReadyLabel, Intent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}) {
								t.Fatalf("ready output = %+v", out)
							}
						} else if out != before {
							t.Fatalf("changed non-ready cell: %+v -> %+v", before, out)
						}
						if WasWaitingDemoted(in, out) != demoted {
							t.Fatal("waiting demotion changed")
						}
						over := IsReadyLabel(out.Label) && before.Label == WaitingLabel
						if WasReadyOverWaiting(in, out) != over {
							t.Fatal("ready-over-waiting mark")
						}
						sess.DisplayLabel = out.Label
						sess.DisplayIntent = out.Intent
						sess.DisplaySpinner = out.Spinner
						sess.IsReadyOverWaiting = over
						if got := PreReadyOutput(sess); got != before {
							t.Fatalf("inverse = %+v want %+v", got, before)
						}
					})
				}
			}
		}
	}
}

func TestReadyTransientAndFailureExclusions(t *testing.T) {
	reason := "blocked"
	draftFailure := sessionreason.DraftPRCreationFailure(errors.New("create draft PR: authentication required"))
	rows := map[string]func(*pb.Session){
		"setup":            func(s *pb.Session) { s.DisplaySettingUp = true },
		"merge":            func(s *pb.Session) { s.DisplayMerging = true },
		"archive":          func(s *pb.Session) { s.ArchivePending = true },
		"repair":           func(s *pb.Session) { s.DisplayIsRepairing = true },
		"workflow":         func(s *pb.Session) { s.WorkflowDisplayStatus = pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING },
		"blocked reason":   func(s *pb.Session) { s.BlockedReason = &reason },
		"draft PR failure": func(s *pb.Session) { s.BlockedReason = &draftFailure },
		"orphaned":         func(s *pb.Session) { s.State = pb.SessionState_SESSION_STATE_ORPHANED },
		"blocked":          func(s *pb.Session) { s.State = pb.SessionState_SESSION_STATE_BLOCKED },
	}
	for name, mutate := range rows {
		t.Run(name, func(t *testing.T) {
			s := &pb.Session{DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING}
			mutate(s)
			in := Input{Session: s, ChatStatus: pb.ChatStatus_CHAT_STATUS_IDLE}
			before := Compute(in)
			s.HasBuildReceipt = true
			if got := Compute(in); got != before {
				t.Fatalf("changed %+v -> %+v", before, got)
			}
		})
	}
}

func TestPreReadyOutputTotal(t *testing.T) {
	for _, s := range []*pb.Session{nil, {}, {DisplayLabel: "working", HasBuildReceipt: true}, {DisplayLabel: ReadyLabel, DisplayStatus: pb.DisplayStatus(999)}} {
		want := Output{Label: s.GetDisplayLabel(), Intent: s.GetDisplayIntent(), Spinner: s.GetDisplaySpinner()}
		if got := PreReadyOutput(s); got != want {
			t.Fatalf("got %+v want %+v", got, want)
		}
	}
}
