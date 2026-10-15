// Package prverdict renders the boss-verify toolbox's approve and send-back
// outcomes for a person, so `boss pr` and the TUI say the same thing.
package prverdict

import (
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// refusals maps each reason verify-gate.mjs refuses with to its detail. A
// reason missing here renders as "refused".
var refusals = map[string]string{
	"no-standing-park":        "no `needs human:` park stands on the PR's current head",
	"head-moved":              "the PR's head or its park changed while approving, so what stands now is not approved; re-check and approve again",
	"approval-record-failed":  "the approval record could not be posted on the PR, so nothing is approved and the needs-human label was left alone",
	"approval-superseded":     "a later operator decision on the head (a send-back or another approval) landed while approving and replaces this one",
	"approval-unconfirmed":    "whether the approval record was posted and took effect could not be read back",
	"approver-not-writer":     "the account boss runs gh as cannot write to the repository, so its record on the PR would not count",
	"approver-unreadable":     "the repository permission of the account boss runs gh as could not be read",
	"sent-back-approved-head": "the head verify passed under an approval was sent back; run /boss-verify on the PR to re-raise its park, then approve that",
	"comments-unreadable":     "the PR's comments could not be read, so an earlier send-back cannot be ruled out",
	"park-codes-unknown":      "the park names a code this toolbox does not know",
	"pr-not-open":             "the PR is not open",
	"pr-unreadable":           "the PR could not be read",
	"status-unreadable":       "the PR's verify status could not be read",
	"head-unreadable":         "the PR's head commit could not be read",
	"pr-comment-failed":       "the PR comment could not be posted",
}

// RefusalDetail renders a verify-gate refusal reason, with the park it names
// and the toolbox's why when present.
func RefusalDetail(reason, why, park string) string {
	detail, ok := refusals[reason]
	if !ok {
		detail = "refused"
	}
	if reason == "park-codes-unknown" && park != "" {
		detail += " (" + park + ")"
	}
	detail += " [" + reason + "]"
	if why != "" {
		detail += ": " + why
	}
	return detail
}

// ApproveRefusal is the sentence for an approve that neither approved nor
// planned: the reason, then what a record it posted still does.
func ApproveRefusal(pr int, r *pb.VerifyApproveResult) string {
	return fmt.Sprintf("PR #%d not approved: %s%s", pr, RefusalDetail(r.GetReason(), r.GetWhy(), r.GetPark()), postedRecordDetail(r))
}

// SendBackRefusal is the sentence for a send-back that neither posted nor
// planned.
func SendBackRefusal(pr int, r *pb.SendBackSessionPRResponse) string {
	return fmt.Sprintf("PR #%d not sent back: %s", pr, RefusalDetail(r.GetReason(), "", ""))
}

// postedRecordDetail adds what a refused approve's record still does: a posted
// record is inert for a moved head or a superseding operator decision; for an
// unconfirmed approval it may stand (or may not have been posted), so the
// operator re-runs approve to confirm.
func postedRecordDetail(r *pb.VerifyApproveResult) string {
	switch {
	case r.GetReason() == "approval-unconfirmed" && r.GetApprovalUrl() != "":
		return fmt.Sprintf("; the record at %s may still stand for head %s, so re-run approve to confirm", r.GetApprovalUrl(), shortSHA(r.GetHeadSha()))
	case r.GetReason() == "approval-unconfirmed":
		return "; re-run approve to confirm"
	case r.GetApprovalUrl() != "":
		return fmt.Sprintf("; the record at %s names head %s only and waives nothing that stands now", r.GetApprovalUrl(), shortSHA(r.GetHeadSha()))
	}
	return ""
}

// ApproveLines renders an approve that approved or planned: the outcome, then,
// for a real approval, any needs-human label removal that did not happen.
func ApproveLines(pr int, r *pb.VerifyApproveResult) []string {
	head := shortSHA(r.GetHeadSha())
	sentBack := r.GetLifted() == "sent-back"
	switch {
	case r.GetIsPlanned() && sentBack:
		return []string{fmt.Sprintf("Would approve #%d: record an approval lifting the send-back of head %s (dry run; nothing written).", pr, head)}
	case r.GetIsPlanned():
		return []string{fmt.Sprintf("Would approve #%d: record an approval waiving %s on head %s, then remove the needs-human label (dry run; nothing written).", pr, r.GetWaives(), head)}
	case sentBack:
		return []string{fmt.Sprintf("Approved #%d: lifted the send-back of head %s (record: %s). Run /boss-verify %d to merge it.", pr, head, r.GetApprovalUrl(), pr)}
	}
	lines := []string{fmt.Sprintf("Approved #%d: waived %s on head %s (record: %s). Verify will re-check and merge.", pr, r.GetWaives(), head, r.GetApprovalUrl())}
	if label := labelLine(r); label != "" {
		lines = append(lines, label)
	}
	return lines
}

// labelLine explains a needs-human label the approval did not remove.
func labelLine(r *pb.VerifyApproveResult) string {
	switch r.GetTrackerWrites() {
	case "ok", "":
		return ""
	case "skipped":
		// verify-gate left the label on deliberately: what was approved no longer stands.
		switch r.GetWhy() {
		case "pr-not-open":
			return "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done."
		case "head-moved":
			return "The PR's head or its park changed after the approval, so the needs-human label was left on."
		}
		// An older toolbox reported a failed re-read as skipped too.
		return fmt.Sprintf("The needs-human label was left on (%s); remove it on the ticket by hand if this head still stands.", r.GetWhy())
	}
	why := r.GetTrackerWrites()
	if r.GetWhy() != "" {
		why += ": " + r.GetWhy()
	}
	// The head may have moved and parked meanwhile, so the removal is conditional.
	return fmt.Sprintf("The needs-human label could not be removed (%s); remove it on the ticket by hand if this head still stands.", why)
}

// shortSHA abbreviates a commit SHA for display.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
