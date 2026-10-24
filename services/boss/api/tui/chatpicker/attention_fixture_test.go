package chatpicker

import (
	"context"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type attentionStub struct {
	chatPickerStub
	calls  []string
	head   string
	merges []operatorMergeCall
	// mergeResp / mergeErr answer OperatorMergeSession; a nil mergeResp
	// reports a landed merge with its bookkeeping done.
	mergeResp *pb.OperatorMergeSessionResponse
	mergeErr  error
}

func attentionSession(actions ...pb.AttentionAction) *pb.Session {
	n := int32(42)
	return &pb.Session{Id: "session-1", Title: "Needs review", PrNumber: &n, PrDisplayHeadSha: "rendered-head", Attention: &pb.SessionAttention{Actions: actions, ParkCodes: []string{"ledger-open"}, AlertKey: "resume:auth", TargetChatId: "target-chat"}}
}
func (s *attentionStub) OperatorMergeSession(_ context.Context, id, head string, approve bool, waives []string) (*pb.OperatorMergeSessionResponse, error) {
	s.calls = append(s.calls, "merge:"+id)
	s.head = head
	s.merges = append(s.merges, operatorMergeCall{id: id, head: head, approve: approve, waives: slices.Clone(waives)})
	if s.mergeErr != nil {
		return nil, s.mergeErr
	}
	if s.mergeResp != nil {
		return s.mergeResp, nil
	}
	return &pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "ok"}, nil
}
func (s *attentionStub) DismissSessionAlert(_ context.Context, id, key string) (*pb.Session, error) {
	s.calls = append(s.calls, "dismiss:"+id+":"+key)
	return nil, nil
}
func (s *attentionStub) ResumeSessionRun(_ context.Context, id string) (*pb.Session, error) {
	s.calls = append(s.calls, "resume:"+id)
	return nil, nil
}
func attentionKey(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	if s == "esc" {
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return keyPress([]rune(s)[0])
}

type operatorMergeCall struct {
	id, head string
	approve  bool
	waives   []string
}

var knownParkCodes = []string{
	"always-human-path", "human-review:database", "human-review:security", "human-review:ui",
	"no-receipt", "repair-exhausted", "ledger-open", "extension-failed", "no-session",
	"worktree-diverged", "worktree-unsynced", "human-review:unproven",
}

func parkSession(code string, openItems ...string) *pb.Session {
	n := int32(42)
	a := attention.Compute(attention.Input{ParkCodes: []string{code}, OpenItems: openItems, Evidence: &attention.Evidence{URL: "https://example.com/proof"}})
	return &pb.Session{
		Id: "session-1", Title: "Parked change", PrNumber: &n, PrDisplayHeadSha: renderedHead, Attention: a,
		DisplayStatus:   pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN,
		AttentionStatus: &pb.AttentionStatus{NeedsAttention: true, Reason: pb.AttentionReason_ATTENTION_REASON_AWAITING_HUMAN_INPUT, Summary: "needs human: " + code},
	}
}
