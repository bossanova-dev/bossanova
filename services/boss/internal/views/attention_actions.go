package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/prverdict"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

type attentionActionBinding struct {
	action pb.AttentionAction
	key    string
	label  string
}

var attentionActionBindings = []attentionActionBinding{
	{pb.AttentionAction_ATTENTION_ACTION_APPROVE, "y", "approve"},
	{pb.AttentionAction_ATTENTION_ACTION_SEND_BACK, "b", "send back"},
	{pb.AttentionAction_ATTENTION_ACTION_MERGE, "m", "merge"},
	{pb.AttentionAction_ATTENTION_ACTION_DISMISS, "x", "dismiss"},
	{pb.AttentionAction_ATTENTION_ACTION_RESUME, "r", "resume"},
	{pb.AttentionAction_ATTENTION_ACTION_GITHUB, "g", "github"},
	{pb.AttentionAction_ATTENTION_ACTION_VIDEO, "v", "video"},
	{pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, "enter", "open the failed chat"},
	{pb.AttentionAction_ATTENTION_ACTION_ANSWER, "enter", "answer in the chat"},
	{pb.AttentionAction_ATTENTION_ACTION_TRACKER, "l", "tracker"},
}

func attentionBinding(s *pb.Session, key string) (attentionActionBinding, bool) {
	for _, b := range attentionActionBindings {
		if b.key == key && slices.Contains(s.GetAttention().GetActions(), b.action) {
			return b, true
		}
	}
	return attentionActionBinding{}, false
}

func attentionFooter(s *pb.Session, details bool) []string {
	var labels []string
	for _, a := range s.GetAttention().GetActions() {
		for _, b := range attentionActionBindings {
			if b.action == a && (!details || b.key != "enter") {
				labels = append(labels, "["+b.key+"] "+b.label)
				break
			}
		}
	}
	return labels
}

type attentionActionResultMsg struct {
	action    pb.AttentionAction
	sessionID string
	text      string
	err       error
}

type attentionActionState struct {
	action    pb.AttentionAction
	confirm   confirmPrompt
	note      renamePrompt
	sendNote  func(string) tea.Cmd
	busy      bool
	sessionID string
}

func (a attentionActionState) active() bool { return a.busy || a.confirm.active || a.note.Active() }

func (a attentionActionState) update(msg tea.Msg) (attentionActionState, tea.Cmd) {
	if a.busy {
		return a, nil
	}
	if a.note.Active() {
		if k, ok := msg.(tea.KeyMsg); ok {
			switch k.String() {
			case "esc":
				return attentionActionState{}, nil
			case "enter":
				note := a.note.Value()
				if note == "" {
					a.note.err = "Note cannot be empty"
					return a, nil
				}
				cmd := a.sendNote(note)
				a.note = renamePrompt{}
				a.sendNote = nil
				a.busy = true
				return a, cmd
			}
		}
		var cmd tea.Cmd
		a.note, cmd = a.note.Update(msg)
		return a, cmd
	}
	if k, ok := msg.(tea.KeyMsg); ok && a.confirm.active {
		var cmd tea.Cmd
		a.confirm, cmd = a.confirm.update(k)
		if cmd != nil {
			a.busy = true
		}
		return a, cmd
	}
	return a, nil
}

func (a attentionActionState) start(ctx context.Context, c client.BossClient, s *pb.Session, key string, details bool) (attentionActionState, tea.Cmd, bool) {
	if details && key == "enter" {
		return a, nil, false
	}
	b, ok := attentionBinding(s, key)
	if !ok {
		return a, nil, false
	}
	captured, _ := proto.Clone(s).(*pb.Session)
	a = attentionActionState{sessionID: captured.GetId(), action: b.action}
	command := func(note string) tea.Cmd { return attentionActionCmd(ctx, c, captured, b.action, note) }
	switch b.action {
	case pb.AttentionAction_ATTENTION_ACTION_SEND_BACK:
		var cmd tea.Cmd
		a.note, cmd = newRenamePrompt(captured.GetId(), "")
		a.note.input.Prompt = "Send back: "
		a.note.input.Placeholder = "note for repair"
		a.sendNote = command
		return a, cmd, true
	case pb.AttentionAction_ATTENTION_ACTION_APPROVE:
		a.confirm = newConfirmPrompt(fmt.Sprintf("Approve #%d? Waives: %s.", captured.GetPrNumber(), strings.Join(captured.GetAttention().GetParkCodes(), ", ")), command(""))
	case pb.AttentionAction_ATTENTION_ACTION_MERGE:
		a.confirm = newConfirmPrompt(fmt.Sprintf("Merge #%d?", captured.GetPrNumber()), command(""))
	case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
		a.confirm = newConfirmPrompt("Dismiss this alert? It returns only if a different failure happens.", command(""))
	default:
		a.busy = b.action == pb.AttentionAction_ATTENTION_ACTION_RESUME
		return a, command(""), true
	}
	return a, nil, true
}

func (a attentionActionState) footer(width int) string {
	if a.note.Active() {
		return a.note.footerFor(width, "send")
	}
	if a.confirm.active {
		return a.confirm.footer(width)
	}
	return styleActionBar.Render("Working…")
}

func (a attentionActionState) lineCount(width int) int {
	return lipgloss.Height(a.footer(width)) - 2*actionBarPadY
}

func attentionActionCmd(ctx context.Context, c client.BossClient, s *pb.Session, action pb.AttentionAction, note string) tea.Cmd {
	return func() tea.Msg {
		result := attentionActionResultMsg{sessionID: s.GetId(), action: action}
		switch action {
		case pb.AttentionAction_ATTENTION_ACTION_APPROVE:
			var r *pb.VerifyApproveResult
			r, result.err = c.ApproveSessionPR(ctx, s.GetId(), false)
			if result.err == nil {
				if r.GetIsApproved() {
					result.text = strings.Join(prverdict.ApproveLines(int(s.GetPrNumber()), r), " ")
				} else {
					result.err = errors.New(prverdict.ApproveRefusal(int(s.GetPrNumber()), r))
				}
			}
		case pb.AttentionAction_ATTENTION_ACTION_SEND_BACK:
			var r *pb.SendBackSessionPRResponse
			r, result.err = c.SendBackSessionPR(ctx, s.GetId(), note, false)
			if result.err == nil {
				if r.GetIsPosted() {
					result.text, result.err = startAttentionRepair(ctx, c, s, note, r.GetCommentUrl())
				} else {
					result.err = errors.New(prverdict.SendBackRefusal(int(s.GetPrNumber()), r))
				}
			}
		case pb.AttentionAction_ATTENTION_ACTION_MERGE:
			if s.GetPrDisplayHeadSha() == "" {
				result.err = errors.New("PR head is unavailable; refresh before merging")
				break
			}
			_, _, result.err = c.MergeSession(ctx, s.GetId(), s.GetPrDisplayHeadSha())
			result.text = fmt.Sprintf("Merged #%d", s.GetPrNumber())
		case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
			_, result.err = c.DismissSessionAlert(ctx, s.GetId(), s.GetAttention().GetAlertKey())
			result.text = "Alert dismissed"
		case pb.AttentionAction_ATTENTION_ACTION_RESUME:
			_, result.err = c.ResumeSessionRun(ctx, s.GetId())
			result.text = "Run resumed"
		case pb.AttentionAction_ATTENTION_ACTION_GITHUB:
			result.err = openURLFunc(s.GetPrUrl())
			result.text = "Opened GitHub"
		case pb.AttentionAction_ATTENTION_ACTION_VIDEO:
			result.err = openURLFunc(s.GetAttention().GetEvidenceUrl())
			result.text = "Opened video"
		case pb.AttentionAction_ATTENTION_ACTION_TRACKER:
			result.err = openURLFunc(s.GetTrackerUrl())
			result.text = "Opened issue"
		case pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, pb.AttentionAction_ATTENTION_ACTION_ANSWER:
			return switchViewMsg{view: ViewAttach, sessionID: s.GetId(), resumeID: s.GetAttention().GetTargetChatId()}
		case pb.AttentionAction_ATTENTION_ACTION_UNSPECIFIED:
			return nil
		}
		return result
	}
}

func startAttentionRepair(ctx context.Context, c client.BossClient, s *pb.Session, note, commentURL string) (string, error) {
	posted := fmt.Sprintf("the note is posted at %s", commentURL)
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("%s; create repair chat ID: %w", posted, err)
	}
	chat, err := c.RecordChat(ctx, s.GetId(), id.String(), "send back", "", pb.ChatRole_CHAT_ROLE_OPERATOR, false)
	if err != nil {
		return "", fmt.Errorf("%s; start repair chat: %w", posted, err)
	}
	if chat.GetTmuxSessionName() == "" {
		return "", fmt.Errorf("%s; repair chat %s has no live agent", posted, chat.GetAgentSessionId())
	}
	message := fmt.Sprintf("/boss-repair %d\n\nThe operator sent PR #%d back. Address this feedback before anything else:\n\n%s", s.GetPrNumber(), s.GetPrNumber(), note)
	delivery, err := c.SendChatMessage(ctx, &pb.SendChatMessageRequest{AgentSessionId: chat.GetAgentSessionId(), Message: message, WakeIfAsleep: true, Submit: true})
	if err != nil {
		return "", fmt.Errorf("%s; deliver repair to chat %s: %w", posted, chat.GetAgentSessionId(), err)
	}
	if !delivery.GetDelivered() {
		return fmt.Sprintf("Sent back #%d — repair delivery unconfirmed in chat %s; inspect it before retrying. %s", s.GetPrNumber(), chat.GetAgentSessionId(), delivery.GetNoticeText()), nil
	}
	return fmt.Sprintf("Sent back #%d — repair requested in chat %s. %s", s.GetPrNumber(), chat.GetAgentSessionId(), delivery.GetNoticeText()), nil
}
