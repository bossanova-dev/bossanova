// Package prverdict renders the boss-verify toolbox's approve and send-back
// outcomes for a person, so `boss pr` and the TUI explain them the same way.
// The TUI presents an approve as the merge it leads to, so it takes the Merge*
// sentences, which share every detail with the Approve* ones.
package prverdict

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/attention"
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

// mergeRefusals overrides the refusal details whose CLI wording names an
// approval, for the TUI, which presents an approve as the merge it leads to.
// Every other reason reads the same in both.
var mergeRefusals = map[string]string{
	"head-moved":              "the PR's head or its park changed while requesting the merge, so what stands now is not cleared to merge; re-check and merge again",
	"approval-record-failed":  "the merge request could not be recorded on the PR, so nothing is cleared to merge and the needs-human label was left alone",
	"approval-superseded":     "a later operator decision on the head (a send-back or another merge request) landed meanwhile and replaces this one",
	"approval-unconfirmed":    "whether the merge request was recorded and took effect could not be read back",
	"sent-back-approved-head": "this head was sent back after verify passed it; run /boss-verify on the PR to re-raise its park, then merge again",
}

// RefusalDetail renders a verify-gate refusal reason, with the park it names
// and the toolbox's why when present.
func RefusalDetail(reason, why, park string) string {
	return refusalDetail(refusals, reason, why, park)
}

// MergeRefusalDetail is RefusalDetail in the TUI's merge wording.
func MergeRefusalDetail(reason, why, park string) string {
	if _, ok := mergeRefusals[reason]; ok {
		return refusalDetail(mergeRefusals, reason, why, park)
	}
	return RefusalDetail(reason, why, park)
}

func refusalDetail(details map[string]string, reason, why, park string) string {
	detail, ok := details[reason]
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
	return fmt.Sprintf("PR #%d not approved: %s%s", pr, RefusalDetail(r.GetReason(), r.GetWhy(), r.GetPark()), postedRecordDetail(r, "re-run approve"))
}

// MergeRefusal is ApproveRefusal for the TUI's [m]erge on a verify-parked PR.
func MergeRefusal(pr int, r *pb.VerifyApproveResult) string {
	return fmt.Sprintf("PR #%d not merged: %s%s", pr, MergeRefusalDetail(r.GetReason(), r.GetWhy(), r.GetPark()), postedRecordDetail(r, "merge again"))
}

// ParkCodeLines renders each park code in plain words, one indented line per
// code in the verify gate's report order, e.g.
// "  ledger-open: review left must-fix findings open".
func ParkCodeLines(codes []string) []string {
	normalized := attention.ParkCodes(codes)
	lines := make([]string, 0, len(normalized))
	for _, code := range normalized {
		lines = append(lines, fmt.Sprintf("  %s: %s", code, attention.ParkCodePlainWords(code)))
	}
	return lines
}

// failureList names a park's failure codes with their plain words, or "" when
// the park only asks for a sign-off.
func failureList(codes []string) string {
	failures := attention.ParkFailureCodes(codes)
	named := make([]string, 0, len(failures))
	for _, code := range failures {
		named = append(named, fmt.Sprintf("%s (%s)", code, attention.ParkCodePlainWords(code)))
	}
	return strings.Join(named, ", ")
}

// sendBackCommand is the CLI verb that rejects the head instead.
func sendBackCommand(pr int) string {
	return fmt.Sprintf("boss pr send-back %d \"<note>\"", pr)
}

// MergeConfirm is the TUI's [m]erge confirmation on a verify-parked PR: the
// question, each park code in plain words, and, when any code reports a
// failure, which failure merging overrides and the send-back alternative.
// overridesFailure reports that last case, in which the TUI makes merging the
// non-default choice.
func MergeConfirm(pr int, codes []string) (lines []string, overridesFailure bool) {
	failures := failureList(codes)
	if failures == "" && len(attention.ParkCodes(codes)) > 0 {
		lines = append(lines, fmt.Sprintf("Merge PR #%d? Verify waives its park on the current head, re-checks it, then merges:", pr))
		return append(lines, ParkCodeLines(codes)...), false
	}
	lines = append(lines, fmt.Sprintf("Merge PR #%d anyway? Verify waives its park on the current head, re-checks it, then merges:", pr))
	lines = append(lines, ParkCodeLines(codes)...)
	if failures == "" {
		failures = "a park that names no reason"
	}
	return append(lines,
		fmt.Sprintf("Merging overrides %s.", failures),
		"To send it back instead: "+sendBackCommand(pr),
	), true
}

// approvedParkLines renders, after an approval that waived waives, each waived
// code in plain words and, when one reports a failure, what the approval
// overrode and how to undo it.
func approvedParkLines(pr int, waives string, planned bool) []string {
	codes := attention.ParkCodes([]string{waives})
	if len(codes) == 0 {
		return nil
	}
	lines := ParkCodeLines(codes)
	failures := failureList(codes)
	switch {
	case failures == "":
		return lines
	case planned:
		return append(lines, fmt.Sprintf("Approving would override %s; %s sends the head back instead.", failures, sendBackCommand(pr)))
	default:
		return append(lines, fmt.Sprintf("Approving overrode %s; if that was not intended, %s revokes the approval and sends the head back.", failures, sendBackCommand(pr)))
	}
}

// SendBackRefusal is the sentence for a send-back that neither posted nor
// planned.
func SendBackRefusal(pr int, r *pb.SendBackSessionPRResponse) string {
	return fmt.Sprintf("PR #%d not sent back: %s", pr, RefusalDetail(r.GetReason(), "", ""))
}

// postedRecordDetail adds what a refused approve's record still does: a posted
// record is inert for a moved head or a superseding operator decision; for an
// unconfirmed approval it may stand (or may not have been posted), so the
// operator retries to confirm.
func postedRecordDetail(r *pb.VerifyApproveResult, retry string) string {
	switch {
	case r.GetReason() == "approval-unconfirmed" && r.GetApprovalUrl() != "":
		return fmt.Sprintf("; the record at %s may still stand for head %s, so %s to confirm", r.GetApprovalUrl(), shortSHA(r.GetHeadSha()), retry)
	case r.GetReason() == "approval-unconfirmed":
		return "; " + retry + " to confirm"
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
		planned := fmt.Sprintf("Would approve #%d: record an approval waiving %s on head %s, then remove the needs-human label (dry run; nothing written).", pr, r.GetWaives(), head)
		return append([]string{planned}, approvedParkLines(pr, r.GetWaives(), true)...)
	case sentBack:
		return []string{fmt.Sprintf("Approved #%d: lifted the send-back of head %s (record: %s). Run /boss-verify %d to merge it.", pr, head, r.GetApprovalUrl(), pr)}
	}
	lines := []string{fmt.Sprintf("Approved #%d: waived %s on head %s (record: %s). Verify will re-check and merge.", pr, r.GetWaives(), head, r.GetApprovalUrl())}
	lines = append(lines, approvedParkLines(pr, r.GetWaives(), false)...)
	if label := labelLine(r, "the approval"); label != "" {
		lines = append(lines, label)
	}
	return lines
}

// MergeLines is ApproveLines for the TUI's [m]erge on a verify-parked PR,
// which never dry-runs.
func MergeLines(pr int, r *pb.VerifyApproveResult) []string {
	head := shortSHA(r.GetHeadSha())
	if r.GetLifted() == "sent-back" {
		return []string{fmt.Sprintf("Send-back lifted for #%d on head %s (record: %s). Run /boss-verify %d to merge it.", pr, head, r.GetApprovalUrl(), pr)}
	}
	lines := []string{fmt.Sprintf("Merge requested for #%d: verify waived %s on head %s and will re-check and merge (record: %s).", pr, r.GetWaives(), head, r.GetApprovalUrl())}
	if label := labelLine(r, "the merge request"); label != "" {
		lines = append(lines, label)
	}
	return lines
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
			return "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done."
		case "head-moved":
			return "The PR's head or its park changed after " + decision + ", so the needs-human label was left on."
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
