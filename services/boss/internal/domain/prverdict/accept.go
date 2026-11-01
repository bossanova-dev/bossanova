package prverdict

import (
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// AcceptLines renders acceptance and dry-run results for CLI and TUI callers.
func AcceptLines(pr int, r *pb.AcceptSessionPRBlockerResponse) []string {
	if r.GetIsDryRun() {
		lines := make([]string, 1, 1+len(r.GetPlannedWrites()))
		lines[0] = "Would:"
		for _, write := range r.GetPlannedWrites() {
			lines = append(lines, "  "+write)
		}
		return lines
	}
	action := "Accepted"
	if r.GetIsAlreadyAccepted() {
		action = "Already accepted"
	}
	reused := ""
	if r.GetIsExistingTicket() {
		reused = " (existing follow-up reused)"
	}
	return []string{fmt.Sprintf("%s %s on #%d at %s. Follow-up: %s %s%s. Record: %s.", action, r.GetBlockerId(), pr, shortSHA(r.GetHeadSha()), r.GetTicketIdentifier(), r.GetTicketUrl(), reused, r.GetCommentUrl()), "Verify re-judges this commit without it."}
}

var acceptRefusals = map[string]string{
	"pr-unreadable":                "the PR could not be read",
	"not-open":                     "the PR is not open",
	"head-moved":                   "the PR's head changed; refresh it and try again",
	"repair-active":                "repair is running for this session; wait for it to finish",
	"comments-unreadable":          "the PR's comments could not be read",
	"no-blockers":                  "there is no standing blockers comment on this head",
	"blocker-not-listed":           "that blocker is not listed on this head",
	"blocker-not-acceptable":       "that blocker is not a review finding that can be accepted",
	"sent-back":                    "this head was sent back for repair",
	"tracker-create-unavailable":   "the tracker cannot create a follow-up ticket in this setup",
	"tracker-create-failed":        "the follow-up ticket could not be created",
	"tracker-create-indeterminate": "whether the follow-up ticket was created could not be confirmed",
	"comment-failed":               "the acceptance record could not be posted on the PR",
}

// AcceptRefusal explains why accepting one blocker was refused, retaining unknown codes.
func AcceptRefusal(pr int, blocker string, r *pb.AcceptSessionPRBlockerResponse) string {
	reason := acceptRefusals[r.GetReason()]
	if reason == "" {
		reason = r.GetReason()
	}
	detail := ""
	if r.GetDetail() != "" {
		detail = " (" + r.GetDetail() + ")"
	}
	return fmt.Sprintf("PR #%d blocker %s not accepted: %s%s", pr, blocker, reason, detail)
}
