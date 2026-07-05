package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func chatCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Interact with session chats headlessly",
	}

	// send subcommand
	send := &cobra.Command{
		Use:   "send <session-id|chat-id> <message>",
		Short: "Send a message to a chat",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChatSend(cmd, args[0], args[1])
		},
	}
	// Default true: `boss chat send` is a human/script sending a message and
	// expecting the agent to act on it, so a single-line message is submitted
	// (Enter + verified). --submit=false prefills the composer without submitting.
	send.Flags().Bool("submit", true, "Submit the message (press Enter and verify); false prefills the composer without submitting")

	// show subcommand
	show := &cobra.Command{
		Use:   "show <session-id|chat-id>",
		Short: "Print a chat transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChatShow(cmd, args[0])
		},
	}
	show.Flags().Int32("limit", 0, "Maximum number of messages to show (0 = all)")
	show.Flags().Bool("result-only", false, "Print only the final assistant result text")

	// wait subcommand
	wait := &cobra.Command{
		Use:   "wait <session-id|chat-id>",
		Short: "Wait for a chat to become idle, then print the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChatWait(cmd, args[0])
		},
	}
	wait.Flags().Duration("timeout", 30*time.Minute, "Maximum time to wait (e.g. 5m, 1h)")

	cmd.AddCommand(send, show, wait)
	return cmd
}

func runChatSend(cmd *cobra.Command, chatID, message string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	ctx := context.Background()
	target, err := resolveChatTarget(ctx, c, chatID)
	if err != nil {
		return err
	}

	submit, _ := cmd.Flags().GetBool("submit")
	resp, err := c.SendChatMessage(ctx, &pb.SendChatMessageRequest{
		AgentSessionId: target.AgentSessionID,
		Message:        message,
		WakeIfAsleep:   true,
		Submit:         submit,
	})
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	if resp.Delivered {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "delivered (tmux: %s)\n", resp.TmuxSessionName)
	} else {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "not delivered")
	}
	return nil
}

type chatTarget struct {
	SessionID      string
	AgentSessionID string
}

func resolveChatTarget(ctx context.Context, c interface {
	GetSession(context.Context, string) (*pb.Session, error)
}, target string) (chatTarget, error) {
	sess, err := c.GetSession(ctx, target)
	if err == nil && sess != nil {
		if agentSessionID := sess.GetAgentSessionId(); agentSessionID != "" {
			return chatTarget{SessionID: sess.GetId(), AgentSessionID: agentSessionID}, nil
		}
		return chatTarget{}, fmt.Errorf("session %s has no primary chat id", target)
	}
	if err == nil {
		return chatTarget{AgentSessionID: target}, nil
	}
	if connect.CodeOf(err) == connect.CodeNotFound {
		return chatTarget{AgentSessionID: target}, nil
	}
	return chatTarget{}, fmt.Errorf("resolve chat target: %w", err)
}

func runChatShow(cmd *cobra.Command, chatID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	limit, _ := cmd.Flags().GetInt32("limit")
	resultOnly, _ := cmd.Flags().GetBool("result-only")

	ctx := context.Background()
	target, err := resolveChatTarget(ctx, c, chatID)
	if err != nil {
		return err
	}

	resp, err := c.GetChatTranscript(ctx, &pb.GetChatTranscriptRequest{
		SessionId:      target.SessionID,
		AgentSessionId: target.AgentSessionID,
		MaxMessages:    limit,
	})
	if err != nil {
		return fmt.Errorf("get transcript: %w", err)
	}
	if !resp.Exists {
		return fmt.Errorf("chat %s not found", chatID)
	}

	if resultOnly {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.FinalAssistantText)
		return nil
	}

	for _, msg := range resp.Messages {
		role := strings.ToUpper(msg.Role)
		ts := msg.Timestamp
		if ts == "" {
			ts = "-"
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "[%s %s]\n%s\n\n", role, ts, msg.Text)
	}
	if resp.FinalAssistantText != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "--- final result ---\n%s\n", resp.FinalAssistantText)
	}
	return nil
}

func runChatWait(cmd *cobra.Command, chatID string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	timeout, _ := cmd.Flags().GetDuration("timeout")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	target, err := resolveChatTarget(ctx, c, chatID)
	if err != nil {
		return err
	}
	baseline := ""
	if resp, err := c.GetChatTranscript(ctx, &pb.GetChatTranscriptRequest{
		SessionId:      target.SessionID,
		AgentSessionId: target.AgentSessionID,
	}); err == nil && resp.GetExists() {
		baseline = resp.GetFinalAssistantText()
	}

	// A chat that was already finished when wait began keeps the same final
	// assistant text on every poll, which is indistinguishable from a freshly
	// sent follow-up whose new answer has not landed yet. Suppress the baseline
	// result only during an initial grace window long enough for a real
	// follow-up to flip the chat to a working state; after the window an
	// unchanged, non-working chat was already done, so return its result rather
	// than sleeping until --timeout.
	const (
		pollInterval  = 3 * time.Second
		baselineGrace = 12 * time.Second
	)
	waitStart := time.Now()
	for {
		baselineGraceExpired := time.Since(waitStart) >= baselineGrace
		done, result, err := chatWaitTick(ctx, c, target, baseline, baselineGraceExpired)
		if err != nil {
			if ctx.Err() != nil {
				return chatWaitTimeout(cmd, c, target, baseline, chatID, timeout)
			}
			return fmt.Errorf("wait: %w", err)
		}
		if done {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), result)
			return nil
		}

		select {
		case <-ctx.Done():
			return chatWaitTimeout(cmd, c, target, baseline, chatID, timeout)
		case <-time.After(pollInterval):
		}
	}
}

// chatWaitTimeout runs one final check after the wait context expired. A chat
// that was already finished when wait began keeps its final text equal to the
// baseline, which the grace window deliberately suppresses; a --timeout shorter
// than that window (e.g. `boss chat wait --timeout 5s`) would otherwise report a
// timeout even though the result is already available. Treat the grace window as
// expired and consult the chat once more with a fresh short context: if it is no
// longer working, print its result instead of reporting a timeout. A still-
// working follow-up reports the timeout as before.
func chatWaitTimeout(cmd *cobra.Command, c interface {
	GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error)
	GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
}, target chatTarget, baseline, chatID string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if done, result, err := chatWaitTick(ctx, c, target, baseline, true); err == nil && done {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), result)
		return nil
	}
	return fmt.Errorf("timed out waiting for chat %s after %s", chatID, timeout)
}

// chatWaitTick checks whether the chat is done. When the target was resolved
// from a session id it polls that session's scoped chat statuses before reading
// the transcript. The baseline is the chat's final assistant text captured
// before waiting began; while the transcript still matches it and the grace
// window has not expired, a follow-up answer may not have landed yet, so we
// keep waiting rather than return the stale previous result. Once the grace
// window has expired (baselineGraceExpired) an unchanged transcript means the
// chat was already finished when waiting began, so we return that result.
// Returns (done, finalText, err).
func chatWaitTick(ctx context.Context, c interface {
	GetChatStatuses(context.Context, string) ([]*pb.ChatStatusEntry, error)
	GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
}, target chatTarget, baselineFinal string, baselineGraceExpired bool) (bool, string, error) {
	if target.SessionID != "" {
		statuses, err := c.GetChatStatuses(ctx, target.SessionID)
		if err != nil && connect.CodeOf(err) != connect.CodeUnimplemented {
			return false, "", fmt.Errorf("get chat statuses: %w", err)
		}
		for _, s := range statuses {
			if s.AgentSessionId != target.AgentSessionID {
				continue
			}
			switch s.Status {
			case pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_QUESTION, pb.ChatStatus_CHAT_STATUS_STOPPED:
				return chatWaitTranscriptDone(ctx, c, target, baselineFinal, baselineGraceExpired)
			case pb.ChatStatus_CHAT_STATUS_WORKING:
				return false, "", nil
			default: // CHAT_STATUS_UNSPECIFIED or future values
				return false, "", nil
			}
		}
	}

	return chatWaitTranscriptDone(ctx, c, target, baselineFinal, baselineGraceExpired)
}

func chatWaitTranscriptDone(ctx context.Context, c interface {
	GetChatTranscript(context.Context, *pb.GetChatTranscriptRequest) (*pb.GetChatTranscriptResponse, error)
}, target chatTarget, baselineFinal string, baselineGraceExpired bool) (bool, string, error) {
	resp, err := c.GetChatTranscript(ctx, &pb.GetChatTranscriptRequest{
		SessionId:      target.SessionID,
		AgentSessionId: target.AgentSessionID,
	})
	if err != nil {
		return false, "", fmt.Errorf("get transcript: %w", err)
	}
	if resp.Exists && resp.FinalAssistantText != "" {
		// A follow-up that is still running leaves the final text equal to the
		// baseline, so returning it here would hand back the stale previous
		// result. Keep waiting through the grace window, but once it expires an
		// unchanged final means the chat was already finished when wait began,
		// so return that result instead of hanging until --timeout.
		if resp.FinalAssistantText == baselineFinal {
			if baselineGraceExpired {
				return true, resp.FinalAssistantText, nil
			}
			return false, "", nil
		}
		return true, resp.FinalAssistantText, nil
	}
	return false, "", nil
}
