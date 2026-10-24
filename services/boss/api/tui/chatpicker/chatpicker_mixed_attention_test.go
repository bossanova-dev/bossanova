package chatpicker

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestChatPickerRoutinePRWaitingHints(t *testing.T) {
	for _, trigger := range []string{"checks_passed", "checks_passed_ready", "merged"} {
		reason := displaystatus.CallbackWaitingReason(trigger, "acme", "widget", 123)
		sess := &pb.Session{Id: "s1", Title: "parked session", DisplayLabel: displaystatus.WaitingLabel, DisplaySpinner: true, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_INFO}
		m := Model{session: sess, chats: []*pb.AgentChat{{AgentSessionId: "a", Title: "verify"}}, daemonStatuses: map[string]string{"a": ui.StatusWaiting}, daemonWaitingReasons: map[string]string{"a": reason}}
		if got, want := firstArmedLine(m), "verify · "+reason; got != want {
			t.Fatalf("chat picker routine wait = %q, want %q", got, want)
		}
	}
}
func TestChatPickerReadyWaitingHint(t *testing.T) {
	sess := &pb.Session{DisplayLabel: displaystatus.ReadyLabel, DisplayIntent: pb.DisplayIntent_DISPLAY_INTENT_SUCCESS}
	m := Model{session: sess, chats: []*pb.AgentChat{{AgentSessionId: "a"}}, daemonStatuses: map[string]string{"a": ui.StatusWaiting}, daemonWaitingReasons: map[string]string{"a": testWaitingReason}}
	if got := firstArmedLine(m); got != "New chat · waiting: "+testWaitingReason {
		t.Fatalf("Ready chat picker hint = %q", got)
	}
}

func TestChatPickerAttentionHasNoLegacyWarning(t *testing.T) {
	s := &pb.Session{Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: strings.Repeat("word ", 100)}, SetupError: "stale warning"}
	if block := (Model{session: s}).legacyWarningBlock(); block != "" {
		t.Fatalf("details view rendered legacy hints beside attention: %q", ansi.Strip(block))
	}
}

func TestChatPickerMergeSentenceInTimeline(t *testing.T) {
	merge := &pb.Session{Id: "merge", Title: "Merge feature", Attention: &pb.SessionAttention{Kind: pb.AttentionKind_ATTENTION_KIND_MERGE, Sentence: "Ready to merge"}}
	details := Model{session: merge, timeline: []*pb.TimelineStep{{Title: "CI passed", State: pb.TimelineStepState_TIMELINE_STEP_STATE_DONE}}}
	if block := ansi.Strip(details.timelineBlock()); !strings.Contains(block, "Ready to merge") {
		t.Fatalf("details view lost the merge sentence: %q", block)
	}
}
func TestChatPickerFooterActionsNeverShareAKey(t *testing.T) {
	mergeable := true
	inputs := map[string]*attention.Input{
		"fix":         {Fix: &attention.FixFact{AlertKey: "alert", Sentence: "broken"}},
		"question":    {QuestionText: "which one?"},
		"review":      {ParkCodes: []string{"human-review:ui"}, Evidence: &attention.Evidence{URL: "https://example.com/proof.mp4"}},
		"blocked":     {ParkCodes: []string{"ledger-open"}},
		"merge":       {HasPR: true, PRState: attention.PRStateOpen, ChecksTotal: 1, ChecksDone: 1, Mergeable: &mergeable},
		"in progress": {ChatStatus: pb.ChatStatus_CHAT_STATUS_WORKING},
		"legacy":      nil,
	}
	offered := map[pb.AttentionAction]bool{}
	keyOf := regexp.MustCompile(`\[([^\]]+)\]`)
	for kind, in := range inputs {
		var att *pb.SessionAttention
		if in != nil {
			in.HasTrackerLink = true
			att = attention.Compute(*in)
		}
		for _, a := range att.GetActions() {
			offered[a] = true
		}
		trackerURL := "https://linear.app/team/issue/BOS-1"
		m := seedChatPicker(&chatPickerStub{}, ui.StatusWorking)
		m.newTabSupported = true
		m.session = &pb.Session{Id: "session-1", PrNumber: proto.Int32(42), TrackerUrl: &trackerURL, Attention: att}
		left, middle, back := m.chatListActionGroups()
		claimed := map[string]string{}
		for _, label := range slices.Concat(left, middle, back) {
			key := keyOf.FindStringSubmatch(label)[1]
			if prev, dup := claimed[key]; dup {
				t.Errorf("%s/details: %q and %q both claim [%s]", kind, prev, label, key)
			}
			claimed[key] = label
		}
	}
	for _, b := range ui.AttentionActionBindings {
		if !offered[b.Action] {
			t.Errorf("no state offers %v, so the matrix no longer covers every footer action", b.Action)
		}
	}
}
