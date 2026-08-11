package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/chatdelivery"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type chatTargetClient struct {
	session *pb.Session
	err     error
}

func (c chatTargetClient) GetSession(context.Context, string, client.SessionReadOptions) (*pb.Session, error) {
	return c.session, c.err
}

func TestResolveChatTarget(t *testing.T) {
	t.Run("session id resolves to primary chat", func(t *testing.T) {
		agentSessionID := "agent-123"
		got, err := resolveChatTarget(context.Background(), chatTargetClient{
			session: &pb.Session{Id: "sess-123", AgentSessionId: &agentSessionID},
		}, "sess-123")
		if err != nil {
			t.Fatalf("resolveChatTarget: %v", err)
		}
		if got.SessionID != "sess-123" || got.AgentSessionID != agentSessionID {
			t.Fatalf("target = %+v, want session/chat ids", got)
		}
	})

	t.Run("not found means target is already a chat id", func(t *testing.T) {
		got, err := resolveChatTarget(context.Background(), chatTargetClient{
			err: connect.NewError(connect.CodeNotFound, errors.New("session not found")),
		}, "agent-123")
		if err != nil {
			t.Fatalf("resolveChatTarget: %v", err)
		}
		if got.SessionID != "" || got.AgentSessionID != "agent-123" {
			t.Fatalf("target = %+v, want chat id passthrough", got)
		}
	})

	t.Run("session without primary chat is an error", func(t *testing.T) {
		_, err := resolveChatTarget(context.Background(), chatTargetClient{
			session: &pb.Session{Id: "sess-empty"},
		}, "sess-empty")
		if err == nil || !strings.Contains(err.Error(), "no primary chat id") {
			t.Fatalf("error = %v, want no primary chat id", err)
		}
	})

	t.Run("transport error is not mistaken for chat id", func(t *testing.T) {
		_, err := resolveChatTarget(context.Background(), chatTargetClient{
			err: errors.New("dial unix: connection refused"),
		}, "agent-123")
		if err == nil || !strings.Contains(err.Error(), "resolve chat target") {
			t.Fatalf("error = %v, want resolve chat target", err)
		}
	})
}

// chatSendRecorder answers resolveChatTarget's GetSession with NotFound (so the
// argument is treated as a chat id) and captures the SendChatMessage request.
// The request is the only place the wake/submit flags are observable — the
// response says nothing about them, and neither does stdout.
type chatSendRecorder struct {
	req *pb.SendChatMessageRequest
}

func (c *chatSendRecorder) GetSession(context.Context, string, client.SessionReadOptions) (*pb.Session, error) {
	return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
}

func (c *chatSendRecorder) SendChatMessage(_ context.Context, req *pb.SendChatMessageRequest) (*pb.SendChatMessageResponse, error) {
	c.req = req
	return &pb.SendChatMessageResponse{Delivered: true, TmuxSessionName: "boss-chat-agent-123"}, nil
}

// runChatSendArgs drives the real `chat send` subcommand's flag parsing — not a
// hand-built flag set — so the registered default is what the assertion sees.
func runChatSendArgs(t *testing.T, c *chatSendRecorder, args ...string) {
	t.Helper()
	root := chatCmd()
	var send *cobra.Command
	for _, sub := range root.Commands() {
		if sub.Name() == "send" {
			send = sub
			break
		}
	}
	if send == nil {
		t.Fatal("chat send subcommand not registered")
	}
	send.SetOut(&bytes.Buffer{})
	if err := send.Flags().Parse(args); err != nil {
		t.Fatalf("parse flags %v: %v", args, err)
	}
	if err := chatSend(context.Background(), c, send, "agent-123", "hello"); err != nil {
		t.Fatalf("chatSend: %v", err)
	}
	if c.req == nil {
		t.Fatal("no SendChatMessage request reached the daemon")
	}
}

func TestChatSendWakeIfAsleepFlag(t *testing.T) {
	t.Run("omitted flag still wakes a sleeping chat", func(t *testing.T) {
		c := &chatSendRecorder{}
		runChatSendArgs(t, c)
		if !c.req.GetWakeIfAsleep() {
			t.Fatal("wake_if_asleep = false with the flag omitted; existing callers rely on the wake")
		}
	})

	t.Run("explicit false leaves a stopped chat stopped", func(t *testing.T) {
		c := &chatSendRecorder{}
		runChatSendArgs(t, c, "--wake-if-asleep=false")
		if c.req.GetWakeIfAsleep() {
			t.Fatal("wake_if_asleep = true, want false: --wake-if-asleep=false was ignored")
		}
	})

	t.Run("explicit true is passed through", func(t *testing.T) {
		c := &chatSendRecorder{}
		runChatSendArgs(t, c, "--wake-if-asleep=true", "--submit")
		if !c.req.GetWakeIfAsleep() {
			t.Fatal("wake_if_asleep = false, want true")
		}
		if !c.req.GetSubmit() {
			t.Fatal("submit = false, want true: --submit regressed alongside the new flag")
		}
	})
}

type chatWaitClient struct {
	statusSessionID string
	statuses        []*pb.ChatStatusEntry
	statusErr       error
	transcriptReqs  []*pb.GetChatTranscriptRequest
	transcript      *pb.GetChatTranscriptResponse
	transcriptErr   error
}

func (c *chatWaitClient) GetChatStatuses(_ context.Context, sessionID string) ([]*pb.ChatStatusEntry, error) {
	c.statusSessionID = sessionID
	return c.statuses, c.statusErr
}

func (c *chatWaitClient) GetChatTranscript(_ context.Context, req *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error) {
	c.transcriptReqs = append(c.transcriptReqs, req)
	if c.transcript != nil {
		return c.transcript, c.transcriptErr
	}
	return &pb.GetChatTranscriptResponse{}, c.transcriptErr
}

func TestChatWaitTickUsesScopedStatusAndBaseline(t *testing.T) {
	t.Run("polls session scoped status", func(t *testing.T) {
		c := &chatWaitClient{
			statuses: []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_WORKING}},
		}
		done, _, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", false)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if done {
			t.Fatal("done = true, want false for working status")
		}
		if c.statusSessionID != "sess-123" {
			t.Fatalf("status session id = %q, want sess-123", c.statusSessionID)
		}
		if len(c.transcriptReqs) != 0 {
			t.Fatalf("transcript requests = %d, want 0 while working", len(c.transcriptReqs))
		}
	})

	t.Run("does not return stale baseline while transcript is unchanged", func(t *testing.T) {
		c := &chatWaitClient{
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		// Every tick (not just the first) must keep waiting while the transcript
		// still shows the pre-wait answer AND the grace window has not expired: a
		// still-running follow-up leaves the final text equal to the baseline,
		// and returning it would hand back the stale previous result.
		for tick := range 3 {
			done, _, err := chatWaitTick(context.Background(), c, chatTarget{AgentSessionID: "agent-123"}, "old", false)
			if err != nil {
				t.Fatalf("chatWaitTick tick %d: %v", tick, err)
			}
			if done {
				t.Fatalf("done = true on tick %d, want false for unchanged baseline", tick)
			}
		}
	})

	t.Run("returns baseline result once grace expires", func(t *testing.T) {
		// A chat that was already finished when wait began keeps its final text
		// equal to the baseline forever. Once the grace window expires it must
		// return that result instead of sleeping until --timeout.
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		done, result, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", true)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if !done || result != "old" {
			t.Fatalf("done/result = %v/%q, want true/old once grace expired", done, result)
		}
	})

	t.Run("returns finished result when timeout is shorter than grace", func(t *testing.T) {
		// `boss chat wait --timeout 5s` on an already-finished chat whose final
		// text equals the baseline must surface that result, not report a timeout
		// just because the 12s baseline grace window never elapsed.
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "old"},
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := chatWaitTimeout(cmd, c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", "agent-123", 5*time.Second); err != nil {
			t.Fatalf("chatWaitTimeout: %v", err)
		}
		if strings.TrimSpace(out.String()) != "old" {
			t.Fatalf("output = %q, want old", out.String())
		}
	})

	t.Run("still reports timeout while a follow-up is working", func(t *testing.T) {
		c := &chatWaitClient{
			statuses: []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_WORKING}},
		}
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		err := chatWaitTimeout(cmd, c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", "agent-123", 5*time.Second)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want timed out", err)
		}
		if out.Len() != 0 {
			t.Fatalf("output = %q, want empty while working", out.String())
		}
	})

	t.Run("changed final result completes", func(t *testing.T) {
		c := &chatWaitClient{
			statuses:   []*pb.ChatStatusEntry{{AgentSessionId: "agent-123", Status: pb.ChatStatus_CHAT_STATUS_IDLE}},
			transcript: &pb.GetChatTranscriptResponse{Exists: true, FinalAssistantText: "new"},
		}
		done, result, err := chatWaitTick(context.Background(), c, chatTarget{SessionID: "sess-123", AgentSessionID: "agent-123"}, "old", false)
		if err != nil {
			t.Fatalf("chatWaitTick: %v", err)
		}
		if !done || result != "new" {
			t.Fatalf("done/result = %v/%q, want true/new", done, result)
		}
		if len(c.transcriptReqs) != 1 || c.transcriptReqs[0].GetSessionId() != "sess-123" || c.transcriptReqs[0].GetAgentSessionId() != "agent-123" {
			t.Fatalf("transcript request not scoped: %+v", c.transcriptReqs)
		}
	})
}

// TestReportChatSendOutcome pins the three-way rendering of a send outcome
// (BOS-598). The UNCONFIRMED case is the load-bearing one: it must NOT read as
// either "delivered" or a plain failure, because the operator's next decision is
// whether resending is safe — and it is not.
func TestReportChatSendOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resp     *pb.SendChatMessageResponse
		contains []string
		absent   []string
		// once names substrings that must appear exactly once. The resend
		// guidance is the case that matters: bossd puts it in notice_text, so a
		// surface that also appends it locally would print it twice. The tmux
		// session name is the other: it must be named, and named once.
		once []string
		// ownLine names substrings that must be a whole output line of their own.
		// The resend guidance is the sentence the operator has to act on, so it
		// cannot be buried mid-line behind a verifier error.
		ownLine []string
	}{
		{
			// The local shape: notice_text as bossd builds it, mirroring the
			// verifier's own wording. That wording states the OBSERVATION only —
			// the verdict is this line's headline, and a notice that restated it
			// would make the operator read the same fact twice before reaching the
			// part that says which pane and what was seen.
			name: "unconfirmed names the state, the pane and the guidance once each",
			resp: &pb.SendChatMessageResponse{
				TmuxSessionName: "boss-chat-1",
				Delivered:       false,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED,
				NoticeText: `no live composer was drawn on tmux session "boss-chat-1" within the verify budget, ` +
					"so the payload's state is unknown; " +
					"the message may already have been submitted; check the pane before resending",
			},
			contains: []string{
				"delivery unconfirmed",
				"boss-chat-1",
				"no live composer was drawn",
			},
			absent: []string{"not delivered", "could not be verified"},
			once: []string{
				"the message may already have been submitted; check the pane before resending",
				"boss-chat-1",
				"unconfirmed",
			},
			ownLine: []string{"the message may already have been submitted; check the pane before resending"},
		},
		{
			// The shape a proxied client produces: RemoteClient rebuilds the
			// response field by field and does not carry delivery_state, so this
			// branch never sees UNCONFIRMED. The guidance bossd folded into
			// notice_text is what identifies it, and tmux_session_name — which
			// every converter does forward — is what names the pane the operator
			// is being told to go look at.
			name: "unconfirmed over the proxy is still headlined and names the pane",
			resp: &pb.SendChatMessageResponse{
				TmuxSessionName: "boss-chat-1",
				Delivered:       false,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED,
				NoticeText: "verify command submission: capture-pane failed; " +
					"the message may already have been submitted; check the pane before resending",
			},
			contains: []string{
				"delivery unconfirmed",
				"boss-chat-1",
				"capture-pane failed",
			},
			absent: []string{"not delivered"},
			once: []string{
				"the message may already have been submitted; check the pane before resending",
				"boss-chat-1",
				"unconfirmed",
			},
			ownLine: []string{"the message may already have been submitted; check the pane before resending"},
		},
		{
			// BOS-599. A queued send is a success with a caveat, and the caveat
			// is the OPPOSITE of the unconfirmed one: the agent already holds
			// the message, so a resend runs it twice. Rendering it under the
			// unconfirmed headline — or with the resend guidance anywhere in
			// the output — is precisely how an operator ends up double-sending.
			name: "queued names the state and never suggests a resend",
			resp: &pb.SendChatMessageResponse{
				TmuxSessionName: "boss-chat-1",
				Delivered:       true,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED,
				NoticeText: "submit verification timed out; " +
					"the agent already holds the message and will run it when the current turn ends; do not resend",
			},
			contains: []string{"delivery queued", "boss-chat-1", "submit verification timed out"},
			absent: []string{
				"delivery unconfirmed",
				"not delivered",
				"check the pane before resending",
			},
			once: []string{
				"the agent already holds the message and will run it when the current turn ends; do not resend",
				"boss-chat-1",
				"queued",
			},
			ownLine: []string{
				"the agent already holds the message and will run it when the current turn ends; do not resend",
			},
		},
		{
			// The proxied shape of the case above: delivery_state is dropped in
			// transit, so the queued guidance inside notice_text is the only
			// thing standing between the operator and a generic notice that
			// says nothing about whether to resend.
			name: "queued over the proxy is still headlined and names the pane",
			resp: &pb.SendChatMessageResponse{
				TmuxSessionName: "boss-chat-1",
				Delivered:       true,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED,
				NoticeText: "submit verification timed out; " +
					"the agent already holds the message and will run it when the current turn ends; do not resend",
			},
			contains: []string{"delivery queued", "boss-chat-1"},
			absent:   []string{"delivery unconfirmed", "not delivered", "check the pane before resending"},
			once: []string{
				"the agent already holds the message and will run it when the current turn ends; do not resend",
			},
			ownLine: []string{
				"the agent already holds the message and will run it when the current turn ends; do not resend",
			},
		},
		{
			name: "submitted still reports delivered",
			resp: &pb.SendChatMessageResponse{
				TmuxSessionName: "boss-chat-1",
				Delivered:       true,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED,
			},
			contains: []string{"delivered (tmux: boss-chat-1)"},
			absent:   []string{"unconfirmed"},
		},
		{
			// The "/boss switch" interception leaves delivery_state UNSPECIFIED, so
			// its notice must still print verbatim and unchanged.
			name: "interception notice prints verbatim",
			resp: &pb.SendChatMessageResponse{
				Delivered:  false,
				NoticeText: "switched to work-account",
			},
			contains: []string{"switched to work-account"},
			absent:   []string{"unconfirmed", "not delivered"},
		},
		{
			name:     "plain undelivered is unchanged",
			resp:     &pb.SendChatMessageResponse{Delivered: false},
			contains: []string{"not delivered"},
			absent:   []string{"unconfirmed"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			reportChatSendOutcome(&buf, tc.resp)
			got := buf.String()
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("output %q missing %q", got, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Fatalf("output %q unexpectedly contains %q", got, unwanted)
				}
			}
			for _, want := range tc.once {
				if n := strings.Count(got, want); n != 1 {
					t.Fatalf("output %q contains %q %d times, want exactly 1", got, want, n)
				}
			}
			for _, want := range tc.ownLine {
				found := false
				for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
					if strings.TrimSpace(line) == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("output %q does not carry %q on a line of its own", got, want)
				}
			}
		})
	}
}

// TestReportChatSendOutcomeRecognisesTheSharedGuidance pins the coupling the
// table above cannot: its notices spell the guidance out, so both sides of a
// rewording would have to be edited together for it to fail, and the daemon is
// free to reword its copy alone.
//
// The recognition is not cosmetic. A response that reached the CLI through the
// proxy has no delivery_state (the proxy response carries no mirror of it), so
// this sentence inside notice_text is the ONLY thing that identifies the send as
// unconfirmed — and everything the operator needs hangs off recognising it: the
// headline, the pane to go and look at, and the guidance on its own line rather
// than trailing a verifier error. Building the notice the way the daemon does,
// from the SHARED constant, is what turns a divergence into a red test here
// instead of a silently degraded cloud client.
func TestReportChatSendOutcomeRecognisesTheSharedGuidance(t *testing.T) {
	t.Parallel()

	// Exactly how bossd composes an unconfirmed notice: a verifier error, then
	// the shared guidance.
	notice := fmt.Sprintf("%v; %s", errors.New("verify command submission: capture-pane failed"), chatdelivery.ResendGuidance)

	var buf bytes.Buffer
	reportChatSendOutcome(&buf, &pb.SendChatMessageResponse{
		TmuxSessionName: "boss-chat-1",
		Delivered:       false,
		// The proxied shape: recognition can only come from the notice.
		DeliveryState: pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED,
		NoticeText:    notice,
	})
	got := buf.String()

	for _, want := range []string{"delivery unconfirmed", "boss-chat-1", "capture-pane failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q missing %q (the shared guidance was not recognised)", got, want)
		}
	}
	if strings.Contains(got, "not delivered") {
		t.Fatalf("output %q fell through to the generic branch, which never names the pane", got)
	}
	if n := strings.Count(got, chatdelivery.ResendGuidance); n != 1 {
		t.Fatalf("output %q states the guidance %d times, want exactly 1", got, n)
	}
	onOwnLine := false
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.TrimSpace(line) == chatdelivery.ResendGuidance {
			onOwnLine = true
			break
		}
	}
	if !onOwnLine {
		t.Fatalf("output %q does not carry the guidance on a line of its own", got)
	}
}

// TestReportChatSendOutcomeRecognisesTheQueuedGuidance is the BOS-599 twin of
// the test above, and it exists for the same reason with a worse consequence.
// The table cases spell the queued sentence out, so a daemon-side rewording
// leaves them green; only a notice built from the SHARED constant fails here
// when the two copies drift.
//
// The consequence is worse because the proxied queued response and the proxied
// unconfirmed response are indistinguishable once delivery_state is gone —
// both arrive as a notice and a pane name. If the CLI stops recognising the
// queued sentence the send does not merely lose its headline: it falls through
// to a branch that says nothing about resending, and the operator's natural
// next move is to send the message a second time to a chat that is already
// holding it.
func TestReportChatSendOutcomeRecognisesTheQueuedGuidance(t *testing.T) {
	t.Parallel()

	// Exactly how bossd composes a queued notice.
	notice := fmt.Sprintf("%v; %s", errors.New("submit verification timed out"), chatdelivery.QueuedGuidance)

	var buf bytes.Buffer
	reportChatSendOutcome(&buf, &pb.SendChatMessageResponse{
		TmuxSessionName: "boss-chat-1",
		Delivered:       true,
		// The proxied shape: recognition can only come from the notice.
		DeliveryState: pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED,
		NoticeText:    notice,
	})
	got := buf.String()

	for _, want := range []string{"delivery queued", "boss-chat-1", "submit verification timed out"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output %q missing %q (the shared guidance was not recognised)", got, want)
		}
	}
	if strings.Contains(got, chatdelivery.ResendGuidance) {
		t.Fatalf("output %q tells the operator to resend a message the agent already holds", got)
	}
	if n := strings.Count(got, chatdelivery.QueuedGuidance); n != 1 {
		t.Fatalf("output %q states the guidance %d times, want exactly 1", got, n)
	}
}
