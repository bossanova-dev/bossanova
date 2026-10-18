package prverdict

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestApproveLines(t *testing.T) {
	approved := func(writes, why string) *pb.VerifyApproveResult {
		return &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: writes, Why: why}
	}
	const waived = "Approved #7 on head abcdef01 (record: https://x/9). It waives:"
	const ledgerLine = "  The PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section"
	const overrode = "This overrode a blocking issue. A new push is judged afresh."
	signedOff := &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:ui,always-human-path", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want []string
	}{
		{"an approval over a failure names each code and what it overrode", approved("ok", ""), []string{waived, ledgerLine, overrode}},
		{"a sign-off lists each code once in plain words and warns of nothing", signedOff, []string{
			waived,
			"  Touches a path that always needs a person's sign-off",
			"  A UI change",
		}},
		{"a dry run over an unknown code names it once", &pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "made-up", HeadSha: "abcdef0123456789"}, []string{
			"Would approve #7: record an approval on head abcdef01, then remove the needs-human label (dry run; nothing written). It would waive:",
			"  An issue this version of boss does not recognise",
			"Approving would override a blocking issue, not only a sign-off.",
		}},
		{"an approval over an unknown code names it once", &pb.VerifyApproveResult{IsApproved: true, Waives: "made-up-code", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			waived,
			"  An issue this version of boss does not recognise",
			overrode,
		}},
		{"an approval over open checklist items and a UI change names each once", &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open,human-review:ui", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			waived,
			ledgerLine,
			"  A UI change",
			overrode,
		}},
		{"an approval that waives nothing says so", &pb.VerifyApproveResult{IsApproved: true, HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			"Approved #7 on head abcdef01 (record: https://x/9). It waives nothing.",
		}},
		{"a no-receipt waiver reads as its plain words", &pb.VerifyApproveResult{IsApproved: true, Waives: "no-receipt", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			waived,
			"  The latest commit has not had an automated build and review, or changed after its review",
		}},
		{"a toolbox that reported no tracker writes is still clean", approved("", ""), []string{waived, ledgerLine, overrode}},
		{"a failed label removal says why in plain words", approved("failed", "rate-limited"),
			[]string{waived, ledgerLine, overrode, "The needs-human label could not be removed because the tracker was rate limited."}},
		{"missing credentials read in plain words", approved("unavailable", "TRACKER_CREDENTIALS_MISSING"),
			[]string{waived, ledgerLine, overrode, "The needs-human label could not be removed because the tracker's credentials are missing."}},
		{"a reason with no plain words is left unsaid", approved("failed", "brand-new-reason"),
			[]string{waived, ledgerLine, overrode, "The needs-human label could not be removed."}},
		{"a moved head left the label on", approved("skipped", "head-moved"),
			[]string{waived, ledgerLine, overrode, "The PR changed after the approval, so the needs-human label was left on."}},
		{"a closed PR left the label on", approved("skipped", "pr-not-open"),
			[]string{waived, ledgerLine, overrode, "The PR is no longer open, so the needs-human label was left on."}},
		{"an older toolbox's skipped re-read says why in plain words", approved("skipped", "status-unreadable"),
			[]string{waived, ledgerLine, overrode, "The needs-human label was left on because the PR's verify status could not be read."}},
		{"a lifted send-back states what it did and gives no advice",
			&pb.VerifyApproveResult{IsApproved: true, Lifted: "sent-back", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9"},
			[]string{"Approved #7: lifted the older send-back of head abcdef01 (record: https://x/9)."}},
		{"a dry run names what would be written and what it would override",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval on head abcdef01, then remove the needs-human label (dry run; nothing written). It would waive:",
				ledgerLine,
				"Approving would override a blocking issue, not only a sign-off."}},
		{"a dry-run lift names the send-back",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Lifted: "sent-back", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval lifting the older send-back of head abcdef01 (dry run; nothing written)."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApproveLines(7, tc.r); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("lines = %q\nwant    %q", got, tc.want)
			}
		})
	}
}

func TestOperatorMerged(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    *pb.OperatorMergeSessionResponse
		want string
	}{
		{"bookkeeping done", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "ok"}, "Merged PR #7"},
		{"a PR with no ticket needs no bookkeeping", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "none"}, "Merged PR #7"},
		{"failed bookkeeping says why in plain words", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "unavailable", TrackerWhy: "TRACKER_CREDENTIALS_MISSING"},
			"Merged PR #7; the tracker was not updated because the tracker's credentials are missing"},
		{"bookkeeping that never ran says so", &pb.OperatorMergeSessionResponse{IsMerged: true}, "Merged PR #7; the tracker was not updated"},
		{"bookkeeping that ran out of time says so", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "failed", TrackerWhy: "no time was left to update the tracker"},
			"Merged PR #7; the tracker was not updated because no time was left to update the tracker"},
		{"a queued retry says the ticket update will be retried", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "retrying", TrackerWhy: "pr-unreadable", TrackerTicket: "MAD-1764"},
			"Merged PR #7; the ticket update will be retried"},
		{"a write the tracker refuses for good names the ticket and promises nothing", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "permanent", TrackerWhy: "state-not-found: Done", TrackerTicket: "MAD-1764"},
			"Merged PR #7, but its ticket MAD-1764 could not be moved"},
		{"a refused write with no ticket on record still says so", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "permanent", TrackerWhy: "state-not-found: Done"},
			"Merged PR #7, but its ticket could not be moved"},
		{"a reason with no plain words is left unsaid", &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "failed", TrackerWhy: "exit status 1"}, "Merged PR #7; the tracker was not updated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OperatorMerged(7, tc.r); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestMergeRefusalUsesMergeWording checks that every refusal the TUI's merge
// words differently from the CLI says nothing merged, and never shows a park.
func TestMergeRefusalUsesMergeWording(t *testing.T) {
	for _, reason := range []string{"head-moved", "park-changed", "approval-record-failed", "approval-superseded", "approval-unconfirmed"} {
		r := &pb.VerifyApproveResult{Reason: reason, HeadSha: "abcdef0123456789", Park: "needs human: human-review:security"}
		got := MergeRefusal(7, r)
		if !strings.HasPrefix(got, "Couldn't merge PR #7: ") || strings.Contains(got, "human-review:") {
			t.Errorf("%s: TUI refusal = %q", reason, got)
		}
		if strings.Contains(got, reason) {
			t.Errorf("%s: TUI refusal %q shows the raw reason code", reason, got)
		}
	}
	if got, want := MergeRefusalDetail("pr-not-open"), "the PR is not open"; got != want {
		t.Errorf("a reason with no merge wording = %q, want the CLI's words %q without the code", got, want)
	}
	if got, want := MergeRefusalDetail("brand-new"), "verify refused the approval, so nothing was merged"; got != want {
		t.Errorf("an unknown reason = %q, want %q", got, want)
	}
}

func TestMergeRefusal(t *testing.T) {
	const url = "https://github.com/acme/app/pull/7#issuecomment-9"
	const unconfirmed = "Couldn't merge PR #7: whether your approval was recorded could not be confirmed, so nothing was merged"
	for _, tc := range []struct{ name, recordURL, want string }{
		{"no record names no record", "", unconfirmed},
		{"a posted record says it may stand", url, unconfirmed + "; the approval at " + url + " may still stand for head abcdef01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &pb.VerifyApproveResult{Reason: "approval-unconfirmed", HeadSha: "abcdef0123456789", ApprovalUrl: tc.recordURL}
			if got := MergeRefusal(7, r); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestApproveRefusal(t *testing.T) {
	const url = "https://github.com/acme/app/pull/7#issuecomment-9"
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want string
	}{
		{"a moved head after the approval was posted names what it still covers",
			&pb.VerifyApproveResult{Reason: "head-moved", HeadSha: "abcdef0123456789", ApprovalUrl: url, Why: "head-moved"},
			"PR #7 not approved: the PR changed while it was being approved, so nothing that stands now is approved; the approval at " + url + " covers head abcdef01 only and waives nothing that stands now"},
		{"a replaced approval says so",
			&pb.VerifyApproveResult{Reason: "approval-superseded", HeadSha: "abcdef0123456789", ApprovalUrl: url},
			"PR #7 not approved: a later decision on this commit replaced this approval; the approval at " + url + " covers head abcdef01 only and waives nothing that stands now"},
		{"an unconfirmed approval with a record says it may stand",
			&pb.VerifyApproveResult{Reason: "approval-unconfirmed", HeadSha: "abcdef0123456789", ApprovalUrl: url},
			"PR #7 not approved: whether the approval was posted and took effect could not be confirmed; the approval at " + url + " may still stand for head abcdef01"},
		{"an unconfirmed approval without a record stops at the reason",
			&pb.VerifyApproveResult{Reason: "approval-unconfirmed", HeadSha: "abc"},
			"PR #7 not approved: whether the approval was posted and took effect could not be confirmed"},
		{"a failed record names no record",
			&pb.VerifyApproveResult{Reason: "approval-record-failed", HeadSha: "abc"},
			"PR #7 not approved: the approval could not be posted on the PR, so nothing is approved"},
		{"an unknown park code shows neither the park nor the toolbox's why",
			&pb.VerifyApproveResult{Reason: "park-codes-unknown", Park: "needs human: brand-new", Why: "upgrade"},
			"PR #7 not approved: the PR is waiting on an issue this version of boss does not recognise"},
		{"a reason with no words is left unsaid",
			&pb.VerifyApproveResult{Reason: "brand-new", Why: "exit status 1"},
			"PR #7 not approved"},
		{"an older send-back on a passed head is stated without advice",
			&pb.VerifyApproveResult{Reason: "sent-back-approved-head"},
			"PR #7 not approved: this commit passed under an approval, but an older send-back still holds it, so verify will not merge it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApproveRefusal(7, tc.r); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRefusalDetailCoversEveryApproveReason(t *testing.T) {
	// The refusal reasons skills-toolbox/verify-gate.mjs approve returns.
	for _, reason := range []string{
		"pr-unreadable", "pr-not-open", "head-unreadable", "status-unreadable",
		"park-codes-unknown", "comments-unreadable", "no-standing-park",
		"sent-back-approved-head", "approver-not-writer", "approver-unreadable",
		"head-moved", "park-changed", "approval-superseded", "approval-record-failed", "approval-unconfirmed",
	} {
		if RefusalDetail(reason) == "" {
			t.Errorf("reason %q has no plain words", reason)
		}
	}
	if got := RefusalDetail("brand-new"); got != "" {
		t.Fatalf("an unknown reason = %q, want it left unsaid", got)
	}
}
