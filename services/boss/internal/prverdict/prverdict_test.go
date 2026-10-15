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
	const waived = "Approved #7: waived ledger-open on head abcdef01 (record: https://x/9). Verify will re-check and merge."
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want []string
	}{
		{"a clean approval is one line", approved("ok", ""), []string{waived}},
		{"a toolbox that reported no tracker writes is still clean", approved("", ""), []string{waived}},
		{"a failed label removal names the failure", approved("failed", "rate-limited"),
			[]string{waived, "The needs-human label could not be removed (failed: rate-limited); remove it on the ticket by hand if this head still stands."}},
		{"missing credentials name the code", approved("unavailable", "TRACKER_CREDENTIALS_MISSING"),
			[]string{waived, "The needs-human label could not be removed (unavailable: TRACKER_CREDENTIALS_MISSING); remove it on the ticket by hand if this head still stands."}},
		{"a moved head left the label on", approved("skipped", "head-moved"),
			[]string{waived, "The PR's head or its park changed after the approval, so the needs-human label was left on."}},
		{"a closed PR left the label on", approved("skipped", "pr-not-open"),
			[]string{waived, "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done."}},
		{"an older toolbox's skipped re-read names why", approved("skipped", "status-unreadable"),
			[]string{waived, "The needs-human label was left on (status-unreadable); remove it on the ticket by hand if this head still stands."}},
		{"a lifted send-back names how to merge",
			&pb.VerifyApproveResult{IsApproved: true, Lifted: "sent-back", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9"},
			[]string{"Approved #7: lifted the send-back of head abcdef01 (record: https://x/9). Run /boss-verify 7 to merge it."}},
		{"a dry run names what would be written",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval waiving ledger-open on head abcdef01, then remove the needs-human label (dry run; nothing written)."}},
		{"a dry-run lift names the send-back",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Lifted: "sent-back", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval lifting the send-back of head abcdef01 (dry run; nothing written)."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApproveLines(7, tc.r); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("lines = %q\nwant    %q", got, tc.want)
			}
		})
	}
}

func TestApproveRefusal(t *testing.T) {
	const url = "https://github.com/acme/app/pull/7#issuecomment-9"

	t.Run("a refusal after the record was posted names the record and what it still does", func(t *testing.T) {
		for _, tc := range []struct{ reason, want string }{
			{"head-moved", "the record at " + url + " names head abcdef01 only and waives nothing that stands now"},
			{"approval-superseded", "a later operator decision on the head (a send-back or another approval)"},
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
		"head-moved", "approval-superseded", "approval-record-failed", "approval-unconfirmed",
	} {
		if got := RefusalDetail(reason, "", ""); strings.HasPrefix(got, "refused [") {
			t.Errorf("reason %q falls back to the generic detail: %q", reason, got)
		}
	}
	if got := RefusalDetail("brand-new", "", ""); got != "refused [brand-new]" {
		t.Fatalf("an unknown reason = %q", got)
	}
}

func TestSendBackRefusal(t *testing.T) {
	if got := SendBackRefusal(7, &pb.SendBackSessionPRResponse{Reason: "pr-not-open"}); got != "PR #7 not sent back: the PR is not open [pr-not-open]" {
		t.Fatalf("%q", got)
	}
}
