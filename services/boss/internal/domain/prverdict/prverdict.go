// Package prverdict renders the boss-verify toolbox's approve outcomes for a
// person, so `boss pr approve` and the TUI's [m]erge explain them the same way.
// The TUI records an approval as the first step of a merge, so it takes the
// Merge* sentences, which share every detail with the Approve* ones.
package prverdict

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// refusals maps each reason verify-gate.mjs refuses with to its detail in
// plain words. A reason missing here is left unsaid rather than shown as a
// code.
var refusals = map[string]string{
	"no-standing-park":        "nothing on the PR's latest commit is waiting for a person",
	"head-moved":              "the PR changed while it was being approved, so nothing that stands now is approved",
	"park-changed":            "what needs a person changed since it was read, so nothing that stands now is approved",
	"approval-record-failed":  "the approval could not be posted on the PR, so nothing is approved",
	"approval-superseded":     "a later decision on this commit replaced this approval",
	"approval-unconfirmed":    "whether the approval was posted and took effect could not be confirmed",
	"approver-not-writer":     "the GitHub account boss uses cannot write to the repository, so its approval would not count",
	"approver-unreadable":     "whether the GitHub account boss uses can write to the repository could not be read",
	"sent-back-approved-head": "this commit passed under an approval, but an older send-back still holds it, so verify will not merge it",
	"comments-unreadable":     "the PR's comments could not be read, so an earlier decision cannot be ruled out",
	"park-codes-unknown":      "the PR is waiting on an issue this version of boss does not recognise",
	"pr-not-open":             "the PR is not open",
	"pr-unreadable":           "the PR could not be read",
	"status-unreadable":       "the PR's verify status could not be read",
	"head-unreadable":         "the PR's latest commit could not be read",
	"pr-comment-failed":       "the PR comment could not be posted",
}

// mergeRefusals overrides the refusal details whose CLI wording names an
// approval, for the TUI's [m]erge, which records the approval and merges.
// Every other reason reads the same in both. Nothing was merged in any case.
var mergeRefusals = map[string]string{
	"head-moved":             "the PR changed since you opened it, so nothing was merged",
	"park-changed":           "what needs your sign-off changed since you opened it, so nothing was merged",
	"approval-record-failed": "your approval could not be recorded on the PR, so nothing was merged",
	"approval-superseded":    "a later decision on this commit replaced your approval, so nothing was merged",
	"approval-unconfirmed":   "whether your approval was recorded could not be confirmed, so nothing was merged",
}

// RefusalDetail renders a verify-gate refusal reason in plain words, or ""
// for a reason it has no words for.
func RefusalDetail(reason string) string {
	return refusals[reason]
}

// MergeRefusalDetail renders a refusal for the TUI's [m]erge in plain words.
func MergeRefusalDetail(reason string) string {
	if detail, ok := mergeRefusals[reason]; ok {
		return detail
	}
	if detail := RefusalDetail(reason); detail != "" {
		return detail
	}
	return "verify refused the approval, so nothing was merged"
}

// ApproveRefusal is the sentence for an approve that neither approved nor
// planned: the reason, then what an approval it posted still does.
func ApproveRefusal(pr int, r *pb.VerifyApproveResult) string {
	refusal := fmt.Sprintf("PR #%d not approved", pr)
	if detail := RefusalDetail(r.GetReason()); detail != "" {
		refusal += ": " + detail
	}
	return refusal + readFailure(r) + postedRecordDetail(r)
}

// MergeRefusal is ApproveRefusal for the TUI's [m]erge on a review row, whose
// approval was refused, so nothing merged.
func MergeRefusal(pr int, r *pb.VerifyApproveResult) string {
	return fmt.Sprintf("Couldn't merge PR #%d: %s%s%s", pr, MergeRefusalDetail(r.GetReason()), readFailure(r), postedRecordDetail(r))
}

// readFailure adds what the failed read behind a refusal said, such as gh's
// HTTP status, so "could not be read" names its cause.
func readFailure(r *pb.VerifyApproveResult) string {
	if r.GetDetail() == "" {
		return ""
	}
	return " (" + r.GetDetail() + ")"
}

// waivedLines states each waived park code's issue in plain words, one
// indented line per code in the verify gate's report order.
func waivedLines(codes []string) []string {
	lines := make([]string, 0, len(codes))
	for _, code := range codes {
		plain := attention.ParkCodePlainWords(code)
		lines = append(lines, "  "+strings.ToUpper(plain[:1])+plain[1:])
	}
	return lines
}

// waivedPark renders what an approval waives: the summary's ending, which
// says nothing is waived or introduces the codes, then each waived code once
// in plain words and, when one of them blocks rather than asks for a
// sign-off, that the approval overrides it.
func waivedPark(waives string, planned bool) (string, []string) {
	codes := attention.ParkCodes([]string{waives})
	if len(codes) == 0 {
		if planned {
			return " It would waive nothing.", nil
		}
		return " It waives nothing.", nil
	}
	lines := waivedLines(codes)
	blocking := len(attention.ParkFailureCodes(codes)) > 0
	switch {
	case planned && blocking:
		return " It would waive:", append(lines, "Approving would override a blocking issue, not only a sign-off.")
	case planned:
		return " It would waive:", lines
	case blocking:
		return " It waives:", append(lines, "This overrode a blocking issue. A new push is judged afresh.")
	default:
		return " It waives:", lines
	}
}

// postedRecordDetail adds what a refused approve's posted approval still
// does: one that took effect covers a head that moved or a decision that
// replaced it, so it waives nothing that stands now; an unconfirmed one may
// stand.
func postedRecordDetail(r *pb.VerifyApproveResult) string {
	switch {
	case r.GetApprovalUrl() == "":
		return ""
	case r.GetReason() == "approval-unconfirmed":
		return fmt.Sprintf("; the approval at %s may still stand for head %s", r.GetApprovalUrl(), shortSHA(r.GetHeadSha()))
	}
	return fmt.Sprintf("; the approval at %s covers head %s only and waives nothing that stands now", r.GetApprovalUrl(), shortSHA(r.GetHeadSha()))
}

// ApproveLines renders an approve that approved or planned: the outcome, then,
// for a real approval, any needs-human label removal that did not happen.
func ApproveLines(pr int, r *pb.VerifyApproveResult) []string {
	head := shortSHA(r.GetHeadSha())
	sentBack := r.GetLifted() == "sent-back"
	switch {
	case r.GetIsPlanned() && sentBack:
		return []string{fmt.Sprintf("Would approve #%d: record an approval lifting the older send-back of head %s (dry run; nothing written).", pr, head)}
	case r.GetIsPlanned():
		ending, waived := waivedPark(r.GetWaives(), true)
		planned := fmt.Sprintf("Would approve #%d: record an approval on head %s, then remove the needs-human label (dry run; nothing written).%s", pr, head, ending)
		return append([]string{planned}, waived...)
	case sentBack:
		return []string{fmt.Sprintf("Approved #%d: lifted the older send-back of head %s (record: %s).", pr, head, r.GetApprovalUrl())}
	}
	ending, waived := waivedPark(r.GetWaives(), false)
	lines := append([]string{fmt.Sprintf("Approved #%d on head %s (record: %s).%s", pr, head, r.GetApprovalUrl(), ending)}, waived...)
	if label := labelLine(r, "the approval"); label != "" {
		lines = append(lines, label)
	}
	return lines
}

// OperatorMerged is the TUI's result line for a landed [m]erge: "Merged PR
// #N", plus the tracker bookkeeping when it did not happen.
func OperatorMerged(pr int, r *pb.OperatorMergeSessionResponse) string {
	merged := fmt.Sprintf("Merged PR #%d", pr)
	switch r.GetTrackerWrites() {
	case "ok", "none":
		return merged
	case "retrying":
		return merged + "; the ticket update will be retried"
	case "permanent":
		if t := r.GetTrackerTicket(); t != "" {
			return merged + ", but its ticket " + t + " could not be moved"
		}
		return merged + ", but its ticket could not be moved"
	}
	return merged + "; the tracker was not updated" + because(r.GetTrackerWhy())
}

// whyWords says in plain words why a tracker write or a label removal did not
// happen, for the reasons the verify toolbox reports. A reason missing here
// is left unsaid rather than shown as a code.
var whyWords = map[string]string{
	"TRACKER_CREDENTIALS_MISSING":            "the tracker's credentials are missing",
	"missing-credential":                     "the tracker's credentials are missing",
	"unauthorized":                           "the tracker refused the credentials",
	"forbidden":                              "the tracker refused the credentials",
	"rate-limited":                           "the tracker was rate limited",
	"no-ticket-line":                         "the PR names no ticket",
	"no-capability":                          "this setup cannot write to the tracker",
	"no-adapter":                             "no tracker is set up",
	"no time was left to update the tracker": "no time was left to update the tracker",
	"status-unreadable":                      refusals["status-unreadable"],
	"pr-unreadable":                          refusals["pr-unreadable"],
	"head-unreadable":                        refusals["head-unreadable"],
}

// because is " because <why>" for a reason whyWords knows, else "".
func because(why string) string {
	if words, ok := whyWords[why]; ok {
		return " because " + words
	}
	return ""
}

// labelLine explains a needs-human label the approval did not remove; decision
// names the approval as the caller presents it.
func labelLine(r *pb.VerifyApproveResult, decision string) string {
	switch r.GetTrackerWrites() {
	case "ok", "":
		return ""
	case "skipped":
		// verify-gate left the label on deliberately: what was approved no longer stands.
		switch r.GetWhy() {
		case "pr-not-open":
			return "The PR is no longer open, so the needs-human label was left on."
		case "head-moved":
			return "The PR changed after " + decision + ", so the needs-human label was left on."
		}
		// An older toolbox reported a failed re-read as skipped too.
		return "The needs-human label was left on" + because(r.GetWhy()) + "."
	}
	return "The needs-human label could not be removed" + because(r.GetWhy()) + "."
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
