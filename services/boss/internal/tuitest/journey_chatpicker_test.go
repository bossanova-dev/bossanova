package tuitest_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// A build that handed its PR off to verify, watched from the session's chat
// list while the daemon reports the chats parking, then the session needing a
// review that is merged from the same screen.
func TestJourney_ChatPicker_AC1_AC2_AC3_AC4_AC7_StandbyHandoffToMerge(t *testing.T) {
	const (
		head       = "0123456789abcdef0123456789abcdef01234567"
		verifyID   = "claude-1531-z-verify"
		buildID    = "claude-1531-a-build"
		parkCode   = "human-review:ui"
		buildTitle = fixtures.BuildHandoffBuildChatTitle
	)
	w := fixtures.BuildHandoffStandbyWorld()
	sess := proto.Clone(w.Sessions[0]).(*pb.Session)
	sess.PrDisplayHeadSha = head
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sess),
		tuitest.WithChats(w.Chats...),
		tuitest.WithGithubCallbacks(w.GithubCallbacks...),
	)
	h.Daemon.SetOperatorMergeResult(&pb.OperatorMergeSessionResponse{IsMerged: true})

	if !t.Run("AC1 enter on the home row lists the session's chats with a STATUS column", func(t *testing.T) {
		waitText(t, h, sess.GetTitle())
		sendEnter(t, h)
		waitText(t, h, "STATUS")
		waitText(t, h, buildTitle)
		waitText(t, h, "verify")
	}) {
		return
	}

	if !t.Run("AC3 a standby build chat reads standby beside the blocking verify chat", func(t *testing.T) {
		for _, e := range w.ChatStatuses {
			h.Daemon.AddChatStatus(e)
		}
		for _, e := range w.SessionStatuses {
			h.Daemon.AddSessionStatus(e)
		}
		waitRow(t, h, buildTitle, "standby")
		waitRow(t, h, "verify", "awaiting green")
		if row := screenRow(h.Driver.Screen(), buildTitle); strings.Contains(row, "awaiting") {
			t.Fatalf("standby chat reads as a wait: %q", row)
		}
		fetched := func() bool {
			var chats []string
			for _, c := range h.Daemon.ListGithubCallbackCalls() {
				chats = append(chats, c.GetTargetChatId())
			}
			return slices.Contains(chats, verifyID) && slices.Contains(chats, buildID)
		}
		if err := h.Driver.WaitFor(waitTimeout, func(string) bool { return fetched() }); err != nil {
			t.Fatalf("picker never listed the waiting chats' callbacks: %v", h.Daemon.ListGithubCallbackCalls())
		}
	}) {
		return
	}

	if !t.Run("AC4 the home board never shows a standby waiting reason", func(t *testing.T) {
		h.Daemon.AddSessionStatus(&pb.SessionStatusEntry{
			SessionId:     sess.GetId(),
			Status:        pb.ChatStatus_CHAT_STATUS_WAITING,
			WaitingReason: fixtures.BuildHandoffStandbyReason,
		})
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "[n]ew session")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if screen := h.Driver.Screen(); strings.Contains(screen, "checks_failed") {
				t.Fatalf("home shows the standby wait:\n%s", screen)
			}
			time.Sleep(100 * time.Millisecond)
		}
		sendEnter(t, h)
		waitRow(t, h, buildTitle, "standby")
	}) {
		return
	}

	if !t.Run("AC2 opening the waiting chat names it and its awaiting reason", func(t *testing.T) {
		selectRow(t, h, "verify")
		h.Daemon.SetRPCStallFor("RecordChat", time.Minute)
		sendEnter(t, h)
		waitText(t, h, "Launching")
		waitText(t, h, "verify · "+fixtures.BuildHandoffVerifyReason)
		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}
		waitText(t, h, "STATUS")
		h.Daemon.SetRPCStallFor("RecordChat", 0)
		calls := waitCalls(t, h, h.Daemon.RecordChatCalls, 1)
		if c := calls[0]; c.GetSessionId() != sess.GetId() || c.GetAgentSessionId() != verifyID || !c.GetResume() {
			t.Fatalf("RecordChat request = %v, want a resume of %s", c, verifyID)
		}
	}) {
		return
	}

	if !t.Run("AC7 a review park reads parked for you and the standby build reads done", func(t *testing.T) {
		h.Daemon.SetSessionAttention(sess.GetId(), &pb.SessionAttention{
			Kind:         pb.AttentionKind_ATTENTION_KIND_REVIEW,
			Sentence:     "UI change: does it look right in the video?",
			Actions:      []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE},
			ParkCodes:    []string{parkCode},
			TargetChatId: verifyID,
		})
		waitRow(t, h, "verify", "parked for you")
		waitRow(t, h, buildTitle, "done · standby: wakes if CI turns red")
	}) {
		return
	}

	t.Run("sessions.AC30 merge from the chat list confirms once and merges the head it showed", func(t *testing.T) {
		sendKeys(t, h, 'm')
		waitText(t, h, "Merge PR #2989?")
		if calls := h.Daemon.OperatorMergeCalls(); len(calls) != 0 {
			t.Fatalf("OperatorMergeSession before confirmation: %v", calls)
		}
		sendEnter(t, h)
		calls := waitCalls(t, h, h.Daemon.OperatorMergeCalls, 1)
		c := calls[0]
		if c.GetId() != sess.GetId() || c.GetExpectedHeadSha() != head || !c.GetShouldApprove() || !slices.Equal(c.GetWaives(), []string{parkCode}) {
			t.Fatalf("OperatorMergeSession request = %v", c)
		}
		waitText(t, h, "Merged PR #2989")
	})
}

// screenRow returns the first screen line containing marker, or "".
func screenRow(screen, marker string) string {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, marker) {
			return line
		}
	}
	return ""
}

// rowShows reports whether one screen line contains both marker and want.
func rowShows(screen, marker, want string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, marker) && strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// waitRow waits for a line naming marker to also show want.
func waitRow(t *testing.T, h *tuitest.Harness, marker, want string) {
	t.Helper()
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return rowShows(screen, marker, want)
	}); err != nil {
		t.Fatalf("row %q never showed %q: %v\n%s", marker, want, err, h.Driver.Screen())
	}
}

// selectRow moves the cursor down until the selected line names marker.
func selectRow(t *testing.T, h *tuitest.Harness, marker string) {
	t.Helper()
	selected := func(screen string) bool { return rowShows(screen, "❯", marker) }
	for _, key := range []byte("jjjkkkkkk") {
		if selected(h.Driver.Screen()) {
			return
		}
		sendKeys(t, h, key)
		_ = h.Driver.WaitFor(time.Second, selected)
	}
	if !selected(h.Driver.Screen()) {
		t.Fatalf("could not select %q:\n%s", marker, h.Driver.Screen())
	}
}
