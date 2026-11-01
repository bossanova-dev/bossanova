package prverdict

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAcceptRefusal(t *testing.T) {
	for _, reason := range []string{"pr-unreadable", "not-open", "head-moved", "repair-active", "comments-unreadable", "no-blockers", "blocker-not-listed", "blocker-not-acceptable", "sent-back", "tracker-create-unavailable", "tracker-create-failed", "tracker-create-indeterminate", "comment-failed"} {
		t.Run(reason, func(t *testing.T) {
			got := AcceptRefusal(7, "review:012345abcdef", &pb.AcceptSessionPRBlockerResponse{Reason: reason, Detail: "more detail"})
			if strings.Contains(got, reason) || !strings.Contains(got, "more detail") || !strings.Contains(got, "#7") || !strings.Contains(got, "review:012345abcdef") {
				t.Fatal(got)
			}
		})
	}
	if got := AcceptRefusal(7, "blocker", &pb.AcceptSessionPRBlockerResponse{Reason: "future-reason"}); !strings.Contains(got, "future-reason") {
		t.Fatal(got)
	}
}
func TestAcceptLines(t *testing.T) {
	r := &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, BlockerId: "review:012345abcdef", HeadSha: strings.Repeat("a", 40), TicketIdentifier: "TASK-1", TicketUrl: "https://tickets/1", CommentUrl: "https://comments/1"}
	for _, mode := range []string{"success", "already", "reused", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			r.IsAlreadyAccepted = mode == "already"
			r.IsExistingTicket = mode == "reused"
			r.IsDryRun = mode == "dry-run"
			r.PlannedWrites = []string{"create ticket", "post acceptance"}
			lines := AcceptLines(7, r)
			got := strings.Join(lines, "\n")
			switch mode {
			case "already":
				if !strings.HasPrefix(got, "Already accepted") {
					t.Fatal(got)
				}
			case "reused":
				if !strings.Contains(got, "existing follow-up reused") {
					t.Fatal(got)
				}
			case "dry-run":
				if len(lines) != 3 || lines[0] != "Would:" || !strings.Contains(got, "post acceptance") {
					t.Fatal(got)
				}
			default:
				for _, part := range []string{"review:012345abcdef", "#7", "aaaaaaaa", "TASK-1", "https://tickets/1", "https://comments/1", "re-judges"} {
					if !strings.Contains(got, part) {
						t.Fatalf("missing %q: %s", part, got)
					}
				}
			}
		})
	}
}
