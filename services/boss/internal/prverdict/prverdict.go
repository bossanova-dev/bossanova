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

// refusals maps each reason verify-gate.mjs refuses with to its detail. A
// reason missing here renders as "refused".
var refusals = map[string]string{
	"no-standing-park":        "no `needs human:` park stands on the PR's current head",
	"head-moved":              "the PR's head or its park changed while approving, so what stands now is not approved; re-check and approve again",
	"park-changed":            "the PR's park changed since it was read, so what stands now is not approved; re-check and approve again",
	"approval-record-failed":  "the approval record could not be posted on the PR, so nothing is approved and the needs-human label was left alone",
	"approval-superseded":     "a later operator decision on the head landed while approving and replaces this one",
	"approval-unconfirmed":    "whether the approval record was posted and took effect could not be read back",
	"approver-not-writer":     "the account boss runs gh as cannot write to the repository, so its record on the PR would not count",
	"approver-unreadable":     "the repository permission of the account boss runs gh as could not be read",
	"sent-back-approved-head": "the head verify passed under an approval carries an older send-back; run /boss-verify on the PR to re-raise its park, then approve that",
	"comments-unreadable":     "the PR's comments could not be read, so an earlier operator decision cannot be ruled out",
	"park-codes-unknown":      "the park names a code this toolbox does not know",
	"pr-not-open":             "the PR is not open",
	"pr-unreadable":           "the PR could not be read",
	"status-unreadable":       "the PR's verify status could not be read",
	"head-unreadable":         "the PR's head commit could not be read",
	"pr-comment-failed":       "the PR comment could not be posted",
}

// mergeRefusals overrides the refusal details whose CLI wording names an
// approval, for the TUI's [m]erge, which records the approval and merges.
// Every other reason reads the same in both. Nothing was merged in any case.
var mergeRefusals = map[string]string{
	"head-moved":             "the PR changed since you opened it; refresh and try again",
	"park-changed":           "what verify asks you to sign off changed since you looked; review it again, then merge",
	"approval-record-failed": "your approval could not be recorded on the PR, so nothing was merged",
	"approval-superseded":    "a later operator decision on the head landed meanwhile and replaces this one",
	"approval-unconfirmed":   "whether your approval was recorded could not be read back, so nothing was merged",
}

// RefusalDetail renders a verify-gate refusal reason for the CLI, with the
// park it names, the reason code, and the toolbox's why when present.
func RefusalDetail(reason, why, park string) string {
	detail := refusalText(refusals, reason, park, "refused") + " [" + reason + "]"
	if why != "" {
		detail += ": " + why
	}
	return detail
}

// MergeRefusalDetail renders a refusal for the TUI's [m]erge in plain words
// only: no reason code, park code or toolbox why, which are machine text.
func MergeRefusalDetail(reason string) string {
	if detail, ok := mergeRefusals[reason]; ok {
		return detail
	}
	return refusalText(refusals, reason, "", "verify refused the approval, so nothing was merged")
}

func refusalText(details map[string]string, reason, park, unknown string) string {
	detail, ok := details[reason]
	if !ok {
		return unknown
	}
	if reason == "park-codes-unknown" && park != "" {
		detail += " (" + park + ")"
	}
	return detail
}

// ApproveRefusal is the sentence for an approve that neither approved nor
// planned: the reason, then what a record it posted still does.
func ApproveRefusal(pr int, r *pb.VerifyApproveResult) string {
	return fmt.Sprintf("PR #%d not approved: %s%s", pr, RefusalDetail(r.GetReason(), r.GetWhy(), r.GetPark()), postedRecordDetail(r, "re-run approve"))
}

// MergeRefusal is ApproveRefusal for the TUI's [m]erge on a review row, whose
// approval was refused, so nothing merged.
func MergeRefusal(pr int, r *pb.VerifyApproveResult) string {
	return fmt.Sprintf("Couldn't merge PR #%d: %s%s", pr, MergeRefusalDetail(r.GetReason()), postedRecordDetail(r, "merge again"))
}

// ParkCodeLines renders each park code in plain words, one indented line per
// code in the verify gate's report order, e.g.
// "  human-review:ui: a UI change; watch the evidence before it merges".
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

// approvedParkLines renders, after an approval that waived waives, each waived
// code in plain words and, when one reports a failure, what the approval
// overrode.
func approvedParkLines(waives string, planned bool) []string {
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
		return append(lines, fmt.Sprintf("Approving would override %s.", failures))
	default:
		return append(lines, fmt.Sprintf("Approving overrode %s; to undo it, push a new head, which verify judges afresh.", failures))
	}
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
		return []string{fmt.Sprintf("Would approve #%d: record an approval lifting the older send-back of head %s (dry run; nothing written).", pr, head)}
	case r.GetIsPlanned():
		planned := fmt.Sprintf("Would approve #%d: record an approval waiving %s on head %s, then remove the needs-human label (dry run; nothing written).", pr, r.GetWaives(), head)
		return append([]string{planned}, approvedParkLines(r.GetWaives(), true)...)
	case sentBack:
		return []string{fmt.Sprintf("Approved #%d: lifted the older send-back of head %s (record: %s). Run /boss-verify %d to merge it.", pr, head, r.GetApprovalUrl(), pr)}
	}
	lines := []string{fmt.Sprintf("Approved #%d: waived %s on head %s (record: %s). Verify will re-check and merge.", pr, r.GetWaives(), head, r.GetApprovalUrl())}
	lines = append(lines, approvedParkLines(r.GetWaives(), false)...)
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
	case "":
		return merged + "; the tracker was not updated, so move the ticket to done by hand"
	}
	why := r.GetTrackerWrites()
	if r.GetTrackerWhy() != "" {
		why += ": " + r.GetTrackerWhy()
	}
	return fmt.Sprintf("%s; the tracker was not updated (%s), so move the ticket to done by hand", merged, why)
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
