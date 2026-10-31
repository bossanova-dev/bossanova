package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"

	"github.com/recurser/bossd/internal/adapter/verifybridge"
	"github.com/recurser/bossd/internal/status"
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
	wireVerifyTestEnvironment(h.srv)
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

func TestOperatorMergeSession_AdmissionHoldOverride_ChatPicker_AC214401(t *testing.T) {
	for _, reason := range []string{"ticket-needs-person", "ticket-not-selected", "ticket-needs-person,ticket-not-selected"} {
		t.Run(reason, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			h.srv.displayTracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: reason, HeadSHA: headPin})
			_, err := h.merge(false)
			h.refusedBeforeAnything(t, err, "PR #42 is blocked: ", "use boss merge s1 to override")
			if strings.Contains(err.Error(), "boss pr approve") {
				t.Fatalf("unwaivable hold suggests approval: %v", err)
			}
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
	h.srv.operatorMergeBudget = operatorMergeBudget{total: 100 * time.Millisecond, approve: 100 * time.Millisecond}
	merge := h.prov.onMerge
	h.prov.onMerge = func() {
		time.Sleep(200 * time.Millisecond)
		merge()
	}

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !resp.GetIsMerged() || resp.GetTrackerWrites() != "failed" || slices.Contains(h.calls, "mark-merged") {
		t.Fatalf("response = %+v, calls = %q, want a landed merge and no bookkeeping started after the budget ran out", resp, h.calls)
	}
}

func TestOperatorMergeSession_TheOperatorLeavingDoesNotAbandonTheMerge(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	h.verbs.onApprove = leave

	resp, err := h.srv.OperatorMergeSession(ctx, connect.NewRequest(&pb.OperatorMergeSessionRequest{
		Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: true, Waives: []string{"human-review:ui"},
	}))
	if err != nil {
		t.Fatalf("OperatorMergeSession after the caller left: %v", err)
	}
	if want := []string{"approve", "merge", "mark-merged"}; !slices.Equal(h.calls, want) {
		t.Fatalf("calls = %q, want %q: a caller that leaves mid-merge must not stop the merge or its bookkeeping", h.calls, want)
	}
	if !resp.Msg.GetIsMerged() || h.verbs.markCtxErr != nil {
		t.Fatalf("response = %+v, mark-merged ctx err = %v, want a landed merge with live bookkeeping", resp.Msg, h.verbs.markCtxErr)
	}
}

func TestOperatorMergeSession_BookkeepingRunsOnTheMergesBudgetNotTheCallers(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	merge := h.prov.onMerge
	h.prov.onMerge = func() {
		<-ctx.Done()
		merge()
	}

	start := time.Now()
	resp, err := h.srv.OperatorMergeSession(ctx, connect.NewRequest(&pb.OperatorMergeSessionRequest{Id: "s1", ExpectedHeadSha: headPin}))
	if err != nil {
		t.Fatalf("OperatorMergeSession past the caller's deadline: %v", err)
	}
	if want := []string{"merge", "mark-merged"}; !slices.Equal(h.calls, want) || !resp.Msg.GetIsMerged() {
		t.Fatalf("calls = %q, response = %+v, want %q: a merge that lands after the caller gave up still runs its bookkeeping", h.calls, resp.Msg, want)
	}
	if d := h.verbs.markCtxDeadline; h.verbs.markCtxErr != nil || d.Before(start.Add(config.OperatorMergeCeiling-time.Minute)) {
		t.Fatalf("mark-merged ran with deadline %v (err %v), want the merge's own %v budget from %v", d, h.verbs.markCtxErr, config.OperatorMergeCeiling, start)
	}
}

func TestOperatorMergeSession_RecordsTheOutcomeOfAMergeTheOperatorLeft(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		refuse        bool
		approval      verifybridge.ApproveResult
		want          map[string]any
	}{
		{name: "merged", outcome: "merged"},
		{name: "refused", outcome: "refused", refuse: true},
		{
			name: "not approved", outcome: "not approved",
			approval: verifybridge.ApproveResult{Reason: "pr-unreadable", Detail: "gh: HTTP 401: Bad credentials"},
			want:     map[string]any{"reason": "pr-unreadable", "detail": "gh: HTTP 401: Bad credentials"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			var logs bytes.Buffer
			h.srv.logger = zerolog.New(&logs)
			ctx, leave := context.WithCancel(context.Background())
			defer leave()
			h.verbs.onApprove = leave
			if tc.refuse {
				h.verbs.err = errors.New("toolbox exploded")
			}
			if tc.approval.Reason != "" {
				h.verbs.approve = tc.approval
			}

			_, _ = h.srv.OperatorMergeSession(ctx, connect.NewRequest(&pb.OperatorMergeSessionRequest{
				Id: "s1", ExpectedHeadSha: headPin, ShouldApprove: true,
			}))

			var entry map[string]any
			for line := range strings.Lines(logs.String()) {
				var e map[string]any
				if json.Unmarshal([]byte(line), &e) == nil && e["message"] == "operator merge finished" {
					entry = e
				}
			}
			want := map[string]any{"level": "info", "session": "s1", "pr": float64(h.verbs.targets[0].PR), "outcome": tc.outcome, "operator_left": true}
			maps.Copy(want, tc.want)
			for k, v := range want {
				if entry[k] != v {
					t.Fatalf("outcome log %v: %s = %v, want %v (logs:\n%s)", entry, k, entry[k], v, logs.String())
				}
			}
		})
	}
}

// mergingRecorder is a Recomputer that records the merging flag each time the
// tracker publishes, which is what every client reads.
type mergingRecorder struct {
	tracker   *status.DisplayTracker
	published []bool
}

func (r *mergingRecorder) Recompute(_ context.Context, id string) error {
	e := r.tracker.Get(id)
	r.published = append(r.published, e != nil && e.Merging)
	return nil
}

// requireHeldThenReleased fails unless merging, once published, stays
// published until a single clear at the end.
func (r *mergingRecorder) requireHeldThenReleased(t *testing.T) {
	t.Helper()
	first := slices.Index(r.published, true)
	if first < 0 {
		t.Fatalf("published = %v, want merging served", r.published)
	}
	last := len(r.published) - 1
	if r.published[last] || slices.Contains(r.published[first:last], false) {
		t.Fatalf("published = %v, want merging held from the first publish until the merge ends, then cleared once", r.published)
	}
}

// pollingRefresher stands in for MergeSession's post-merge re-poll: it lands
// the merged PR status in the tracker and notes whether merging still held.
type pollingRefresher struct {
	tracker      *status.DisplayTracker
	calls        int
	mergingAfter bool
}

func (r *pollingRefresher) RefreshPRWithoutWebhookCredit(context.Context, string, int) error {
	r.calls++
	r.tracker.Set("s1", vcs.DisplayInfo{Status: vcs.DisplayStatusMerged})
	e := r.tracker.Get("s1")
	r.mergingAfter = e != nil && e.Merging
	return nil
}

func TestOperatorMergeSession_HoldsMergingFromTheGateThroughTheBookkeeping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*operatorMergeHarness)
		landed bool
	}{
		{"landed", func(*operatorMergeHarness) {}, true},
		{"approval refused", func(h *operatorMergeHarness) {
			h.verbs.approve = verifybridge.ApproveResult{Reason: "park-changed", HeadSHA: headPin}
		}, false},
		{"gate blocked", func(h *operatorMergeHarness) { h.prov.prStatus.Draft = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperatorMergeHarness(t, headPin)
			tc.setup(h)
			tracker := h.srv.displayTracker
			rec := &mergingRecorder{tracker: tracker}
			tracker.SetRecomputer(rec)
			refresher := &pollingRefresher{tracker: tracker}
			h.srv.prRefresher = refresher
			merging := func() bool {
				e := tracker.Get("s1")
				return e != nil && e.Merging
			}
			var unserved []string
			note := func(step string) {
				if !merging() {
					unserved = append(unserved, step)
				}
			}
			h.verbs.onApprove = func() { note("approve") }
			h.verbs.onMarkMerged = func() { note("mark-merged") }
			onMerge := h.prov.onMerge
			h.prov.onMerge = func() { note("merge"); onMerge() }

			_, _ = h.merge(true, "human-review:ui")

			if len(unserved) > 0 {
				t.Errorf("merging was not served during %q", unserved)
			}
			if tc.landed && (refresher.calls != 1 || !refresher.mergingAfter) {
				t.Errorf("post-merge re-poll: calls = %d, merging held after it = %v; want one re-poll under the hold", refresher.calls, refresher.mergingAfter)
			}
			rec.requireHeldThenReleased(t)
			if merging() {
				t.Error("merging still set after OperatorMergeSession returned")
			}
			if e := tracker.Get("s1"); tc.landed && (e == nil || e.Status != vcs.DisplayStatusMerged) {
				t.Errorf("entry after the merge = %+v, want the re-polled merged status kept", e)
			}
		})
	}
}

// archivingRefresher stands in for MergeSession's post-merge re-poll on a repo
// that archives sessions after merge: the re-poll lands the session on Merged,
// which requests the archive. It requests it synchronously, the worst case of
// the production goroutine racing the tracker bookkeeping.
type archivingRefresher struct {
	srv *Server
	id  string
}

func (r *archivingRefresher) RefreshPRWithoutWebhookCredit(ctx context.Context, _ string, _ int) error {
	return r.srv.RequestArchiveAutomatic(ctx, r.id)
}

// An operator who archives the session while its approval runs (the page keeps
// MarkArchived enabled during a merge) must not pull the worktree out from under
// the approval, which runs in it.
func TestOperatorMergeSession_AnArchiveDuringTheApprovalWaitsForTheMerge(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	worktree := h.srv.sessions.(*lifecycleSessionStoreFake).session.WorktreePath
	archived := make(chan struct{})
	h.srv.archiveDeferrer = newArchiveDeferrer(nil, nil, nil, nil, func(context.Context, string) error {
		defer close(archived)
		return os.RemoveAll(worktree)
	}, zerolog.Nop())
	h.srv.archiveDeferrer.interval = 10 * time.Millisecond
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	loopDone := h.srv.RunArchiveDeferrer(ctx, nil)
	defer func() { stop(); <-loopDone }()
	var approvedInWorktree bool
	h.verbs.onApprove = func() {
		if err := h.srv.RequestArchiveAutomatic(context.Background(), "s1"); err != nil {
			t.Errorf("archive request: %v", err)
		}
		_, err := os.Stat(worktree)
		approvedInWorktree = err == nil
	}

	if _, err := h.merge(true, "human-review:ui"); err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if !approvedInWorktree {
		t.Fatal("the archive removed the worktree while the approval was running in it")
	}
	select {
	case <-archived:
	case <-time.After(5 * time.Second):
		t.Fatal("the archive requested during the approval never ran after the merge")
	}
}

// TestOperatorMergeSession_TheArchiveWaitsForTheTrackerBookkeeping reproduces
// the delta report: on a repo that archives after merge, the post-merge re-poll
// archived the session and removed its worktree before mark-merged ran in it,
// so every gh read the toolbox made failed (ENOENT on the vanished cwd) and the
// merge reported "the PR could not be read" with the ticket left unmoved.
func TestOperatorMergeSession_TheArchiveWaitsForTheTrackerBookkeeping(t *testing.T) {
	h := newOperatorMergeHarness(t, headPin)
	worktree := h.srv.sessions.(*lifecycleSessionStoreFake).session.WorktreePath
	archived := make(chan struct{})
	h.srv.archiveDeferrer = newArchiveDeferrer(nil, nil, nil, nil, func(context.Context, string) error {
		defer close(archived)
		return os.RemoveAll(worktree)
	}, zerolog.Nop())
	h.srv.archiveDeferrer.interval = 10 * time.Millisecond
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	loopDone := h.srv.RunArchiveDeferrer(ctx, nil)
	defer func() { stop(); <-loopDone }()
	h.srv.prRefresher = &archivingRefresher{srv: h.srv, id: "s1"}
	h.verbs.onMarkMerged = func() {
		if _, err := os.Stat(worktree); err != nil {
			h.verbs.markMerged = verifybridge.MarkMergedResult{Reason: "pr-unreadable"}
		}
	}

	resp, err := h.merge(false)
	if err != nil {
		t.Fatalf("OperatorMergeSession: %v", err)
	}
	if resp.GetTrackerWrites() != "ok" {
		t.Fatalf("tracker writes = %q (why %q), want ok: the archive removed the worktree before the bookkeeping ran in it", resp.GetTrackerWrites(), resp.GetTrackerWhy())
	}
	select {
	case <-archived:
	case <-time.After(5 * time.Second):
		t.Fatal("the archive the merge requested never ran after the bookkeeping finished")
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after the archive: %v", err)
	}
}
