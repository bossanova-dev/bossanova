package apiversion

import (
	"testing"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

const armedReason = "awaiting checks_passed_ready on acme/app#7"

// workingOverWaitingSession is the row V20260918 introduced: every working chat
// holds an armed callback, so the session reads "working" where it read
// "waiting" before.
func workingOverWaitingSession() *pb.Session {
	return &pb.Session{
		Id:                   "sess-armed",
		State:                pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayStatus:        pb.DisplayStatus_DISPLAY_STATUS_CHECKING,
		DisplayLabel:         displaystatus.WorkingLabel,
		DisplayIntent:        pb.DisplayIntent_DISPLAY_INTENT_SUCCESS,
		DisplaySpinner:       true,
		IsWorkingOverWaiting: true,
	}
}

// receiptedWorkingSession is the observed bug's row: a receipted, settled green
// PR whose agent is still working with an armed callback. Before V20260918 it
// read "✓ ready".
func receiptedWorkingSession() *pb.Session {
	s := workingOverWaitingSession()
	s.DisplayStatus = pb.DisplayStatus_DISPLAY_STATUS_PASSING
	s.HasBuildReceipt = true
	return s
}

// TestWorkingOverWaiting_ChainMatchesThePreChangeRowAtEveryOlderVersion is the
// exact-inverse property over the whole production chain: at every version
// older than V20260918 a marked row down-converts to what the same session
// served before the change (the waiting row, or Ready over waiting with its
// mark) down-converts to.
func TestWorkingOverWaiting_ChainMatchesThePreChangeRowAtEveryOlderVersion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		row       func() *pb.Session
		preChange func() *pb.Session
	}{
		{"waiting", workingOverWaitingSession, func() *pb.Session {
			s := workingOverWaitingSession()
			s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = displaystatus.WaitingLabel, pb.DisplayIntent_DISPLAY_INTENT_INFO, true
			s.IsWorkingOverWaiting = false
			return s
		}},
		{"ready over waiting", receiptedWorkingSession, func() *pb.Session {
			s := receiptedWorkingSession()
			s.DisplayLabel, s.DisplayIntent, s.DisplaySpinner = displaystatus.ReadyLabel, pb.DisplayIntent_DISPLAY_INTENT_SUCCESS, false
			s.IsWorkingOverWaiting, s.IsReadyOverWaiting = false, true
			return s
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyToSession(t, tc.row(), DefaultRegistry().Current()); !proto.Equal(got, tc.row()) {
				t.Fatalf("Current rewrote the row: %v", got)
			}
			older := 0
			for _, v := range DefaultRegistry().All() {
				if v >= V20260918 {
					continue
				}
				older++
				if got, want := applyToSession(t, tc.row(), v), applyToSession(t, tc.preChange(), v); !proto.Equal(got, want) {
					t.Errorf("at %s: row down-converts to\n%v\nwant the pre-change row's\n%v", v, got, want)
				}
			}
			if older == 0 {
				t.Fatal("no registry version older than V20260918")
			}
		})
	}
	if got := applyToSession(t, receiptedWorkingSession(), V20260917); got.GetDisplayLabel() != displaystatus.ReadyLabel || !got.GetIsReadyOverWaiting() {
		t.Fatalf("one version back = %q ready-over-waiting=%v, want the pre-change Ready with its mark", got.GetDisplayLabel(), got.GetIsReadyOverWaiting())
	}
}

func TestWorkingOverWaiting_LeavesOtherRowsAlone(t *testing.T) {
	unmarked := workingOverWaitingSession()
	unmarked.IsWorkingOverWaiting = false
	for _, s := range []*pb.Session{unmarked, nil} {
		before := proto.Clone(s)
		if got := downconvertWorkingOverWaitingSession(s); got != s || !proto.Equal(s, before) {
			t.Errorf("downconvertWorkingOverWaitingSession(%v) changed the row", s)
		}
	}
	question := workingOverWaitingSession()
	question.DisplayLabel, question.DisplayIntent, question.DisplaySpinner = displaystatus.QuestionLabel, pb.DisplayIntent_DISPLAY_INTENT_WARNING, false
	if got := downconvertWorkingOverWaitingSession(question); got.GetDisplayLabel() != displaystatus.QuestionLabel {
		t.Errorf("a marked row that no longer reads working was rewritten to %q", got.GetDisplayLabel())
	}
}

// TestWorkingOverWaiting_ChatStatusesComposeWithTheWaitingChange pins the
// chat-status leg through the chain: an armed working chat reads WAITING with
// its reason one version back, and WORKING without one before WAITING existed.
// A working chat with no reason, and an idle-derived WAITING, are untouched.
func TestWorkingOverWaiting_ChatStatusesComposeWithTheWaitingChange(t *testing.T) {
	changes := ProductionChanges()
	apply := func(entries []*pb.ChatStatusEntry, v Version) []*pb.ChatStatusEntry {
		resp := &pb.ProxyGetChatStatusesResponse{Statuses: entries}
		changes.Apply(bossanovav1connect.OrchestratorServiceProxyGetChatStatusesProcedure, resp, v)
		return resp.GetStatuses()
	}
	entries := func() []*pb.ChatStatusEntry {
		return []*pb.ChatStatusEntry{
			{AgentSessionId: "armed", Status: pb.ChatStatus_CHAT_STATUS_WORKING, WaitingReason: armedReason, Phase: "building"},
			{AgentSessionId: "plain", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "building"},
			{AgentSessionId: "parked", Status: pb.ChatStatus_CHAT_STATUS_WAITING, WaitingReason: armedReason},
		}
	}
	if got := apply(entries(), DefaultRegistry().Current()); !proto.Equal(got[0], entries()[0]) || !proto.Equal(got[1], entries()[1]) {
		t.Fatalf("Current rewrote the entries: %v", got)
	}
	back := apply(entries(), V20260917)
	if back[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || back[0].GetWaitingReason() != armedReason || back[0].GetPhase() != "" {
		t.Errorf("armed working chat at V20260917 = %v, want WAITING with its reason and no phase", back[0])
	}
	if !proto.Equal(back[1], entries()[1]) || !proto.Equal(back[2], entries()[2]) {
		t.Errorf("unarmed and idle-derived entries changed at V20260917: %v / %v", back[1], back[2])
	}
	baseline := apply(entries(), Baseline)
	if baseline[0].GetStatus() != pb.ChatStatus_CHAT_STATUS_WORKING || baseline[0].GetWaitingReason() != "" {
		t.Errorf("armed working chat at Baseline = %v, want WORKING with no reason", baseline[0])
	}

	streamed := &pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: &pb.ChatStatusDelta{
		AgentSessionId: "armed", Status: pb.ChatStatus_CHAT_STATUS_WORKING, WaitingReason: armedReason,
	}}}
	changes.Apply(bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure, streamed, V20260917)
	if d := streamed.GetStatusDelta(); d.GetStatus() != pb.ChatStatus_CHAT_STATUS_WAITING || d.GetWaitingReason() != armedReason {
		t.Errorf("streamed delta at V20260917 = %v, want WAITING with its reason", d)
	}
}
