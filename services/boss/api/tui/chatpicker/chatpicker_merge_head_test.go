package chatpicker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/adapter/rpcerror"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Proves chat-picker.AC20: the view's [m]erge is Home's operator merge, pinned
// to the head the view showed, approving what a review row showed, and merging
// an unverified head on the first confirmation.
func TestMergeCmdIsTheOperatorMergeAtTheDisplayedHead_ChatPicker_AC20(t *testing.T) {
	review := &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_REVIEW, ParkCodes: []string{"human-review:ui", "no-receipt"}}
	for _, tc := range []struct {
		name      string
		attention *pb.SessionAttention
		head      string
		refuse    bool
		approve   bool
		waives    []string
	}{
		{name: "a passing row merges without an approval", head: renderedHead},
		{name: "a review row approves what it showed, then merges", attention: review, head: renderedHead, approve: true, waives: []string{"human-review:ui", "no-receipt"}},
		{name: "a PR with no known head is refused before any request", refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &attentionStub{}
			model := Model{client: client, ctx: context.Background(), sessionID: "session-1", session: &pb.Session{Id: "session-1", PrNumber: proto.Int32(23), PrDisplayHeadSha: tc.head, Attention: tc.attention}}
			cmd := model.mergeCmd()
			// The command carries the head the view showed when the operator
			// confirmed, even if a refresh lands before it runs.
			model.session.PrDisplayHeadSha = strings.Repeat("b", 40)
			msg := cmd().(MergeResultMsg)
			if msg.SessionID != "session-1" {
				t.Fatalf("msg = %+v, want it tagged for session-1", msg)
			}
			if tc.refuse {
				if msg.Err == nil || !strings.Contains(msg.Err.Error(), "latest commit is not known yet") || len(client.merges) != 0 {
					t.Fatalf("msg=%+v merges=%+v, want a refusal and no request", msg, client.merges)
				}
				return
			}
			if msg.Err != nil || msg.Unsettled || msg.Text != "Merged PR #23" {
				t.Fatalf("msg = %+v, want the merged result line", msg)
			}
			want := operatorMergeCall{id: "session-1", head: tc.head, approve: tc.approve, waives: tc.waives}
			if len(client.merges) != 1 || client.merges[0].id != want.id || client.merges[0].head != want.head || client.merges[0].approve != want.approve || strings.Join(client.merges[0].waives, ",") != strings.Join(want.waives, ",") {
				t.Fatalf("merges = %+v, want one %+v", client.merges, want)
			}
		})
	}
}

// A merge the daemon refuses leaves the refusal on screen; a merge whose call
// ended without bossd's answer waits for a session poll to settle it, the way
// Home's [m]erge does.
func TestMergeCmdWordsRefusalsAndUnansweredCallsLikeHome(t *testing.T) {
	refused := rpcerror.WrapError(connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("merge blocked: gate=failing; a check failed; gated at head %s", renderedHead)))
	noAnswer := fmt.Errorf("daemon did not answer in time: %w", rpcerror.WrapError(connect.NewError(connect.CodeDeadlineExceeded, context.DeadlineExceeded)))
	for _, tc := range []struct {
		name          string
		mergeErr      error
		mergeResp     *pb.OperatorMergeSessionResponse
		wantErr       string
		wantUnsettled bool
		wantText      string
	}{
		{name: "gate refusal", mergeErr: refused, wantErr: "Couldn't merge PR #23: a check failed"},
		{name: "approval refusal", mergeResp: &pb.OperatorMergeSessionResponse{Approval: &pb.VerifyApproveResult{Reason: "pr-not-open"}}, wantErr: "Couldn't merge PR #23"},
		{name: "no answer", mergeErr: noAnswer, wantUnsettled: true, wantText: "Waiting to hear whether PR #23 merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &attentionStub{mergeErr: tc.mergeErr, mergeResp: tc.mergeResp}
			m := newTestModel(client, context.Background(), "session-1", "")
			m.session = &pb.Session{Id: "session-1", PrNumber: proto.Int32(23), PrDisplayHeadSha: renderedHead}
			m.merging = true

			msg := m.mergeCmd()().(MergeResultMsg)
			updated, _ := m.Update(msg)
			m = updated.(Model)

			if m.merging || m.merged {
				t.Fatalf("merging=%v merged=%v after %+v, want neither", m.merging, m.merged, msg)
			}
			if tc.wantErr != "" {
				var failure ui.MergeFailure
				if !errors.As(msg.Err, &failure) || !strings.HasPrefix(msg.Err.Error(), tc.wantErr) || m.statusMsg != msg.Err.Error() {
					t.Fatalf("msg.Err = %v, statusMsg = %q; want a %q refusal on screen", msg.Err, m.statusMsg, tc.wantErr)
				}
				return
			}
			if !msg.Unsettled || msg.Text != tc.wantText || msg.Err != nil {
				t.Fatalf("msg = %+v, want an unsettled merge saying %q", msg, tc.wantText)
			}
			if a, ok := m.MergeInFlight(); !ok || a.Waiting != tc.wantText || m.canMerge() {
				t.Fatalf("after an unanswered merge: in flight %v (%+v), canMerge %v; want the view waiting and [m]erge withheld", ok, a, m.canMerge())
			}
		})
	}
}
