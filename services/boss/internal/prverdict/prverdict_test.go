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
	const waived = "Approved #7: waived open checklist items on head abcdef01 (record: https://x/9). Verify will re-check and merge."
	const ledgerLine = "  The PR checklist has open items: unmet acceptance criteria, open follow-ups or questions, a do-not-merge marker, or an unreadable section"
	const overrode = "Approving overrode open checklist items. A new push is judged afresh."
	signedOff := &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:ui,always-human-path", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want []string
	}{
		{"an approval over a failure names each code and what it overrode", approved("ok", ""), []string{waived, ledgerLine, overrode}},
		{"a sign-off lists the codes, skips words that repeat a category, and warns of nothing", signedOff, []string{
			"Approved #7: waived security, UI change on head abcdef01 (record: https://x/9). Verify will re-check and merge.",
			"  Touches a path that always needs a person's sign-off",
		}},
		{"a UI-only sign-off names the UI change once", &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:ui", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			"Approved #7: waived UI change on head abcdef01 (record: https://x/9). Verify will re-check and merge.",
		}},
		{"a dry run over an unknown code names it once", &pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "made-up", HeadSha: "abcdef0123456789"}, []string{
			"Would approve #7: record an approval waiving an issue this version of boss does not recognise on head abcdef01, then remove the needs-human label (dry run; nothing written).",
			"Approving would override an issue this version of boss does not recognise.",
		}},
		{"a no-receipt waiver reads as its category", &pb.VerifyApproveResult{IsApproved: true, Waives: "no-receipt", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}, []string{
			"Approved #7: waived latest commit not reviewed on head abcdef01 (record: https://x/9). Verify will re-check and merge.",
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
			[]string{waived, ledgerLine, overrode, "The PR's head or its park changed after the approval, so the needs-human label was left on."}},
		{"a closed PR left the label on", approved("skipped", "pr-not-open"),
			[]string{waived, ledgerLine, overrode, "The PR is no longer open, so the needs-human label was left on."}},
		{"an older toolbox's skipped re-read says why in plain words", approved("skipped", "status-unreadable"),
			[]string{waived, ledgerLine, overrode, "The needs-human label was left on because the PR's verify status could not be read."}},
		{"a lifted send-back states what it did and gives no advice",
			&pb.VerifyApproveResult{IsApproved: true, Lifted: "sent-back", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9"},
			[]string{"Approved #7: lifted the older send-back of head abcdef01 (record: https://x/9)."}},
		{"a dry run names what would be written and what it would override",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval waiving open checklist items on head abcdef01, then remove the needs-human label (dry run; nothing written).",
				ledgerLine,
				"Approving would override open checklist items."}},
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
	const unconfirmed = "Couldn't merge PR #7: whether your approval was recorded could not be read back, so nothing was merged"
	for _, tc := range []struct{ name, recordURL, want string }{
		{"no record says to merge again", "", unconfirmed + "; merge again to confirm"},
		{"a posted record says to merge again", url, unconfirmed + "; the record at " + url + " may still stand for head abcdef01, so merge again to confirm"},
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

	t.Run("a refusal after the record was posted names the record and what it still does", func(t *testing.T) {
		for _, tc := range []struct{ reason, want string }{
			{"head-moved", "the record at " + url + " names head abcdef01 only and waives nothing that stands now"},
			{"approval-superseded", "a later operator decision on the head landed while approving"},
			{"approval-unconfirmed", "the record at " + url + " may still stand for head abcdef01, so re-run approve to confirm"},
		} {
			got := ApproveRefusal(7, &pb.VerifyApproveResult{Reason: tc.reason, HeadSha: "abcdef0123456789", ApprovalUrl: url})
			if !strings.HasPrefix(got, "PR #7 not approved: ") || !strings.Contains(got, "["+tc.reason+"]") || !strings.Contains(got, tc.want) {
				t.Fatalf("%s: %q", tc.reason, got)
			}
		}
	})

	t.Run("an unconfirmed approval without a record says to re-run", func(t *testing.T) {
		got := ApproveRefusal(7, &pb.VerifyApproveResult{Reason: "approval-unconfirmed", HeadSha: "abc"})
		if !strings.HasSuffix(got, "whether the approval record was posted and took effect could not be read back [approval-unconfirmed]; re-run approve to confirm") {
			t.Fatalf("%q", got)
		}
	})

	t.Run("a failed record names no record", func(t *testing.T) {
		got := ApproveRefusal(7, &pb.VerifyApproveResult{Reason: "approval-record-failed", HeadSha: "abc"})
		if !strings.Contains(got, "could not be posted on the PR, so nothing is approved") || strings.Contains(got, "the record at") {
			t.Fatalf("%q", got)
		}
	})

	t.Run("an unknown park code names the park and the toolbox's why", func(t *testing.T) {
		got := ApproveRefusal(7, &pb.VerifyApproveResult{Reason: "park-codes-unknown", Park: "needs human: brand-new", Why: "upgrade"})
		if !strings.HasSuffix(got, "the park names a code this toolbox does not know (needs human: brand-new) [park-codes-unknown]: upgrade") {
			t.Fatalf("%q", got)
		}
	})
}

func TestRefusalDetailCoversEveryApproveReason(t *testing.T) {
	// The refusal reasons skills-toolbox/verify-gate.mjs approve returns.
	for _, reason := range []string{
		"pr-unreadable", "pr-not-open", "head-unreadable", "status-unreadable",
		"park-codes-unknown", "comments-unreadable", "no-standing-park",
		"sent-back-approved-head", "approver-not-writer", "approver-unreadable",
		"head-moved", "park-changed", "approval-superseded", "approval-record-failed", "approval-unconfirmed",
	} {
		if got := RefusalDetail(reason, "", ""); strings.HasPrefix(got, "refused [") {
			t.Errorf("reason %q falls back to the generic detail: %q", reason, got)
		}
	}
	if got := RefusalDetail("brand-new", "", ""); got != "refused [brand-new]" {
		t.Fatalf("an unknown reason = %q", got)
	}
}
