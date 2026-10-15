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
	const ledgerLine = "  ledger-open: review left must-fix findings open"
	const overrode = `Approving overrode ledger-open (review left must-fix findings open); if that was not intended, boss pr send-back 7 "<note>" revokes the approval and sends the head back.`
	signedOff := &pb.VerifyApproveResult{IsApproved: true, Waives: "human-review:ui,always-human-path", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: "ok"}
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want []string
	}{
		{"an approval over a failure names each code and what it overrode", approved("ok", ""), []string{waived, ledgerLine, overrode}},
		{"a sign-off lists the codes and warns of nothing", signedOff, []string{
			"Approved #7: waived human-review:ui,always-human-path on head abcdef01 (record: https://x/9). Verify will re-check and merge.",
			"  always-human-path: touches a path that always needs a person's sign-off",
			"  human-review:ui: a UI change; watch the evidence before it merges",
		}},
		{"a toolbox that reported no tracker writes is still clean", approved("", ""), []string{waived, ledgerLine, overrode}},
		{"a failed label removal names the failure", approved("failed", "rate-limited"),
			[]string{waived, ledgerLine, overrode, "The needs-human label could not be removed (failed: rate-limited); remove it on the ticket by hand if this head still stands."}},
		{"missing credentials name the code", approved("unavailable", "TRACKER_CREDENTIALS_MISSING"),
			[]string{waived, ledgerLine, overrode, "The needs-human label could not be removed (unavailable: TRACKER_CREDENTIALS_MISSING); remove it on the ticket by hand if this head still stands."}},
		{"a moved head left the label on", approved("skipped", "head-moved"),
			[]string{waived, ledgerLine, overrode, "The PR's head or its park changed after the approval, so the needs-human label was left on."}},
		{"a closed PR left the label on", approved("skipped", "pr-not-open"),
			[]string{waived, ledgerLine, overrode, "The PR is no longer open, so the needs-human label was left on; remove it on the ticket by hand if the work is done."}},
		{"an older toolbox's skipped re-read names why", approved("skipped", "status-unreadable"),
			[]string{waived, ledgerLine, overrode, "The needs-human label was left on (status-unreadable); remove it on the ticket by hand if this head still stands."}},
		{"a lifted send-back names how to merge",
			&pb.VerifyApproveResult{IsApproved: true, Lifted: "sent-back", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9"},
			[]string{"Approved #7: lifted the send-back of head abcdef01 (record: https://x/9). Run /boss-verify 7 to merge it."}},
		{"a dry run names what would be written and what it would override",
			&pb.VerifyApproveResult{IsPlanned: true, IsDryRun: true, Waives: "ledger-open", HeadSha: "abcdef0123456789"},
			[]string{"Would approve #7: record an approval waiving ledger-open on head abcdef01, then remove the needs-human label (dry run; nothing written).",
				ledgerLine,
				`Approving would override ledger-open (review left must-fix findings open); boss pr send-back 7 "<note>" sends the head back instead.`}},
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

func TestMergeLines(t *testing.T) {
	approved := func(writes, why string) *pb.VerifyApproveResult {
		return &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9", TrackerWrites: writes, Why: why}
	}
	const waived = "Merge requested for #7: verify waived ledger-open on head abcdef01 and will re-check and merge (record: https://x/9)."
	for _, tc := range []struct {
		name string
		r    *pb.VerifyApproveResult
		want []string
	}{
		{"a clean merge request is one line", approved("ok", ""), []string{waived}},
		{"a failed label removal names the failure", approved("failed", "rate-limited"),
			[]string{waived, "The needs-human label could not be removed (failed: rate-limited); remove it on the ticket by hand if this head still stands."}},
		{"a moved head names the merge request", approved("skipped", "head-moved"),
			[]string{waived, "The PR's head or its park changed after the merge request, so the needs-human label was left on."}},
		{"a lifted send-back names how to merge",
			&pb.VerifyApproveResult{IsApproved: true, Lifted: "sent-back", HeadSha: "abcdef0123456789", ApprovalUrl: "https://x/9"},
			[]string{"Send-back lifted for #7 on head abcdef01 (record: https://x/9). Run /boss-verify 7 to merge it."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergeLines(7, tc.r); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("lines = %q\nwant    %q", got, tc.want)
			}
		})
	}
}

func TestMergeConfirm(t *testing.T) {
	t.Run("a sign-off lists each code and merges by default", func(t *testing.T) {
		lines, overrides := MergeConfirm(42, []string{"human-review:ui"})
		want := []string{
			"Merge PR #42? Verify waives its park on the current head, re-checks it, then merges:",
			"  human-review:ui: a UI change; watch the evidence before it merges",
		}
		if overrides || strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("lines = %q overrides=%v\nwant    %q overrides=false", lines, overrides, want)
		}
	})
	t.Run("a failure names what merging overrides and the send-back", func(t *testing.T) {
		lines, overrides := MergeConfirm(42, []string{"human-review:ui,ledger-open"})
		want := []string{
			"Merge PR #42 anyway? Verify waives its park on the current head, re-checks it, then merges:",
			"  ledger-open: review left must-fix findings open",
			"  human-review:ui: a UI change; watch the evidence before it merges",
			"Merging overrides ledger-open (review left must-fix findings open).",
			`To send it back instead: boss pr send-back 42 "<note>"`,
		}
		if !overrides || strings.Join(lines, "\n") != strings.Join(want, "\n") {
			t.Fatalf("lines = %q overrides=%v\nwant    %q overrides=true", lines, overrides, want)
		}
	})
	t.Run("a park naming no codes is treated as a failure", func(t *testing.T) {
		lines, overrides := MergeConfirm(42, nil)
		if !overrides || !strings.Contains(strings.Join(lines, "\n"), "Merging overrides a park that names no reason.") {
			t.Fatalf("lines = %q overrides=%v", lines, overrides)
		}
	})
}

// TestMergeRefusalUsesMergeWording keeps the TUI free of "approve" for every
// refusal whose CLI detail names an approval, while the CLI wording stands.
func TestMergeRefusalUsesMergeWording(t *testing.T) {
	for _, reason := range []string{"head-moved", "approval-record-failed", "approval-superseded", "approval-unconfirmed", "sent-back-approved-head"} {
		r := &pb.VerifyApproveResult{Reason: reason, HeadSha: "abcdef0123456789"}
		if got := strings.ToLower(MergeRefusal(7, r)); strings.Contains(got, "approv") && !strings.Contains(got, "["+reason+"]") {
			t.Errorf("%s: TUI refusal still says approve: %q", reason, got)
		} else if detail := strings.ToLower(MergeRefusalDetail(reason, "", "")); strings.Contains(strings.TrimSuffix(detail, " ["+reason+"]"), "approv") {
			t.Errorf("%s: TUI detail still says approve: %q", reason, detail)
		}
		if RefusalDetail(reason, "", "") == MergeRefusalDetail(reason, "", "") {
			t.Errorf("%s: the CLI and TUI details are identical; one of them lost its wording", reason)
		}
	}
	if MergeRefusalDetail("pr-not-open", "", "") != RefusalDetail("pr-not-open", "", "") {
		t.Error("a reason with no merge wording must read as the CLI does")
	}
}

func TestMergeRefusal(t *testing.T) {
	const url = "https://github.com/acme/app/pull/7#issuecomment-9"
	const unconfirmed = "PR #7 not merged: whether the merge request was recorded and took effect could not be read back [approval-unconfirmed]"
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
