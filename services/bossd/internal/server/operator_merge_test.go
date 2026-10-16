package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/vcs"

	"github.com/recurser/bossd/internal/verifybridge"
)

// operatorMergeHarness is MergeSession's live-gate harness plus a fake toolbox
// sharing one call log with the provider's MergePR, so a test can read the
// order in which approve, merge and mark-merged ran.
type operatorMergeHarness struct {
	srv   *Server
	prov  *mergeGateProvider
	verbs *verifyVerbsFake
	calls []string
}

func newOperatorMergeHarness(t *testing.T, liveHead string) *operatorMergeHarness {
	t.Helper()
	h := &operatorMergeHarness{}
	st := openCleanPRStatus()
	st.HeadSHA = liveHead
	h.prov = &mergeGateProvider{prStatus: st, checks: livePassingChecks(), mergeCommitSHA: "abc123"}
	h.prov.onMerge = func() {
		h.calls = append(h.calls, "merge")
		h.prov.prStatus.State = vcs.PRStateMerged
	}
	h.srv = mergeGateServer(t, h.prov, vcs.DisplayStatusPassing, withMergeWorktrees(&mergePolicyWorktrees{isAncestor: true}))
	sess := blockedFixLoopSession()
	sess.State = machine.ReadyForReview
	sess.BlockedReason = nil
	sess.WorktreePath = t.TempDir()
	h.srv.sessions = &lifecycleSessionStoreFake{session: sess}
	h.verbs = &verifyVerbsFake{
		calls:      &h.calls,
		approve:    verifybridge.ApproveResult{Approved: true, Waives: "human-review:ui", HeadSHA: headPin, ApprovalURL: "https://x/approval", TrackerWrites: "ok"},
		markMerged: verifybridge.MarkMergedResult{Merged: true, MergeSHA: "abc123", TrackerWrites: "ok"},
	}
	h.srv.newVerifyVerbs = func(env []string) verifyVerbs {
		h.verbs.env = env
		h.verbs.builtCount++
		return h.verbs
	}
	return h
}

func (h *operatorMergeHarness) merge(approve bool, waives ...string) (*pb.OperatorMergeSessionResponse, error) {
	resp, err := h.srv.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{
		Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: approve, Waives: waives,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestOperatorMergeSession_ReviewRecordsTheApprovalThenMergesAtThePinnedHead(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)

	resp, err := h.merge(true, "human-review:ui")
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if want := []string{"approve", "merge", "mark-merged"}; !slices.Equal(h.calls, want) {
		t.Fatalf("calls = %q, want %q: the approval is recorded before the merge and the bookkeeping after it", h.calls, want)
	}
	if got := h.verbs.forMerge[0]; got == nil || got.Head != headPin || !slices.Equal(got.Waives, []string{"human-review:ui"}) {
		t.Fatalf("approve ran with %+v, want --for-merge pinned to %s waiving human-review:ui", got, headPin)
	}
	if len(h.prov.mergeOpts) != 1 || h.prov.mergeOpts[0].ExpectedHeadSHA != headPin {
		t.Fatalf("MergePR opts = %+v, want one merge pinned to %s", h.prov.mergeOpts, headPin)
	}
	if !resp.GetIsMerged() || resp.GetApproval().GetApprovalUrl() != "https://x/approval" || resp.GetTrackerWrites() != "ok" {
		t.Fatalf("response = %+v", resp)
	}
	if h.verbs.markCtxErr != nil {
		t.Fatalf("mark-merged ran on a dead context: %v", h.verbs.markCtxErr)
	}
}

func TestOperatorMergeSession_MovedHeadIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	moved := strings.Repeat("ffee", 10)
	h := newOperatorMergeHarness(t, moved)

	_, err := h.merge(true, "human-review:ui")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	for _, want := range []string{"HEAD_MISMATCH", headPin, moved, "nothing was approved or merged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if len(h.calls) != 0 || h.prov.mergeCalled {
		t.Fatalf("calls = %q (merge called %v), want nothing approved or merged on a moved head", h.calls, h.prov.mergeCalled)
	}
}

func TestOperatorMergeSession_AGateBlockRecordsNoApproval(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.prov.checks = []vcs.CheckResult{{Status: vcs.CheckStatusCompleted, Conclusion: checkConclusionPtr(vcs.CheckConclusionFailure)}}

	_, err := h.merge(true, "human-review:ui")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "merge blocked: gate=") {
		t.Fatalf("err = %v, want a FailedPrecondition merge block", err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("calls = %q, want no approval recorded for a PR that cannot merge", h.calls)
	}
}

func TestOperatorMergeSession_ARefusedApprovalMergesNothing(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.verbs.approve = verifybridge.ApproveResult{Reason: "park-changed", Park: "needs human: human-review:security", HeadSHA: headPin}

	resp, err := h.merge(true, "human-review:ui")
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if resp.GetIsMerged() || resp.GetApproval().GetReason() != "park-changed" {
		t.Fatalf("response = %+v, want a refusal naming park-changed", resp)
	}
	if want := []string{"approve"}; !slices.Equal(h.calls, want) || h.prov.mergeCalled {
		t.Fatalf("calls = %q, want only the refused approve", h.calls)
	}
}

func TestOperatorMergeSession_AFailedMergeSaysTheApprovalStands(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.prov.onMerge = func() { h.calls = append(h.calls, "merge") }
	h.prov.mergeErr = errors.New("required status check boss/verify is pending")

	_, err := h.merge(true, "human-review:ui")
	if err == nil || !strings.Contains(err.Error(), "required status check") || !strings.Contains(err.Error(), "the approval of "+headPin+" (https://x/approval) stays recorded") {
		t.Fatalf("err = %v, want the merge failure and the standing approval", err)
	}
	if slices.Contains(h.calls, "mark-merged") {
		t.Fatalf("calls = %q, want no bookkeeping for a merge that did not land", h.calls)
	}
}

func TestOperatorMergeSession_AMergeRowMergesWithoutApprovingAndRunsTheBookkeeping(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if want := []string{"merge", "mark-merged"}; !slices.Equal(h.calls, want) {
		t.Fatalf("calls = %q, want %q", h.calls, want)
	}
	if !resp.GetIsMerged() || resp.GetApproval() != nil || resp.GetTrackerWrites() != "ok" {
		t.Fatalf("response = %+v", resp)
	}
	if len(h.prov.mergeOpts) != 1 || h.prov.mergeOpts[0].ExpectedHeadSHA != headPin {
		t.Fatalf("MergePR opts = %+v, want one merge pinned to %s", h.prov.mergeOpts, headPin)
	}
}

func TestOperatorMergeSession_FailedBookkeepingNeverFailsALandedMerge(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.verbs.markErr = errors.New("verify-gate exited non-zero")

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !resp.GetIsMerged() || resp.GetTrackerWrites() != "failed" || !strings.Contains(resp.GetTrackerWhy(), "exited non-zero") {
		t.Fatalf("response = %+v, want a landed merge reporting the failed bookkeeping", resp)
	}
}

func TestOperatorMergeSession_RequiresAPinnedHead(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	for _, pin := range []string{"", "abc"} {
		_, err := h.srv.OperatorMergeSession(context.Background(), connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "s1", ExpectedHeadSha: pin, ShouldApprove: true}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("pin %q: err = %v, want InvalidArgument", pin, err)
		}
	}
	if len(h.calls) != 0 || h.verbs.builtCount != 0 {
		t.Fatalf("calls = %q, want nothing run without a valid pin", h.calls)
	}
}

// refusedBeforeAnything asserts a FailedPrecondition carrying every want, with
// nothing approved, merged or bookkept.
func (h *operatorMergeHarness) refusedBeforeAnything(t *testing.T, err error, wants ...string) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if len(h.calls) != 0 || h.prov.mergeCalled {
		t.Fatalf("calls = %q (merge called %v), want nothing approved or merged", h.calls, h.prov.mergeCalled)
	}
}

func TestOperatorMergeSession_ABlockedParkIsRefusedOnEveryPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		approve bool
		waives  []string
	}{
		{"a plain merge", false, nil},
		{"an approval waiving the blocked codes", true, []string{"repair-exhausted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			h.srv.displayTracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: "repair-exhausted", HeadSHA: headPin})

			_, err := h.merge(tc.approve, tc.waives...)
			h.refusedBeforeAnything(t, err, "PR #42 is blocked: ", "use boss pr approve 42 to override")
		})
	}
}

func TestOperatorMergeSession_AFailureIsRefused(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.srv.sessions.(*lifecycleSessionStoreFake).session.State = machine.Blocked

	_, err := h.merge(false)
	h.refusedBeforeAnything(t, err, "PR #42 is blocked: Run stopped", "use boss merge s1 to override")
}

func TestOperatorMergeSession_ADraftOrPendingHeadRecordsNoApproval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*operatorMergeHarness)
		gate  string
	}{
		{"draft", func(h *operatorMergeHarness) { h.prov.prStatus.Draft = true }, "gate=draft"},
		{"checks running", func(h *operatorMergeHarness) {
			h.prov.checks = []vcs.CheckResult{{Name: "go-test", Status: vcs.CheckStatusInProgress}}
		}, "gate=pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			tc.setup(h)

			_, err := h.merge(true, "human-review:ui")
			h.refusedBeforeAnything(t, err, "merge blocked: "+tc.gate, "nothing was approved or merged")
		})
	}
}

func TestOperatorMergeSession_APushAfterTheApprovalMergesNothing(t *testing.T) {
	moved := strings.Repeat("ffee", 10)
	h := newOperatorMergeHarness(t, headPin)
	h.verbs.onApprove = func() { h.prov.prStatus.HeadSHA = moved }

	_, err := h.merge(true, "human-review:ui")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err=%v)", connect.CodeOf(err), err)
	}
	for _, want := range []string{"HEAD_MISMATCH", moved, "the approval of " + headPin} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
	if want := []string{"approve"}; !slices.Equal(h.calls, want) || h.prov.mergeCalled {
		t.Fatalf("calls = %q (merge called %v), want the approval only and no merge of the pushed head", h.calls, h.prov.mergeCalled)
	}
}

func TestOperatorMergeSession_ApproveRunsUnderItsOwnBudget(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.srv.operatorMergeBudget = operatorMergeBudget{total: time.Minute, approve: 5 * time.Second}

	before := time.Now()
	if _, err := h.merge(true, "human-review:ui"); err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if d := h.verbs.approveCtxDeadline; d.IsZero() || d.After(before.Add(6*time.Second)) {
		t.Fatalf("approve deadline = %v, want one within the 5s approve budget of %v", d, before)
	}
}

func TestOperatorMergeSession_SlowBookkeepingStillReportsTheMerge(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.srv.operatorMergeBudget = operatorMergeBudget{total: 300 * time.Millisecond, approve: 100 * time.Millisecond}
	h.verbs.markBlocks = true

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !resp.GetIsMerged() || resp.GetTrackerWrites() != "failed" || !strings.Contains(resp.GetTrackerWhy(), "timed out") {
		t.Fatalf("response = %+v, want a landed merge whose hung bookkeeping was cut off at the budget", resp)
	}
}

func TestOperatorMergeSession_NoTimeLeftSkipsTheBookkeeping(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	h.srv.operatorMergeBudget = operatorMergeBudget{total: time.Nanosecond, approve: time.Nanosecond}

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !resp.GetIsMerged() || resp.GetTrackerWrites() != "failed" || slices.Contains(h.calls, "mark-merged") {
		t.Fatalf("response = %+v, calls = %q, want a landed merge and no bookkeeping started after the budget ran out", resp, h.calls)
	}
}
