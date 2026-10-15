package views

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attentionBoard preserves the daemon's manual order within In progress.
func attentionBoard(sessions []*pb.Session) []*pb.Session {
	out := make([]*pb.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_DONE {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b *pb.Session) int {
		aa, ba := a.GetAttention(), b.GetAttention()
		an, bn := attention.NeedsYou(aa.GetKind()), attention.NeedsYou(ba.GetKind())
		if an != bn {
			if an {
				return -1
			}
			return 1
		}
		if !an {
			return 0
		}
		af, bf := aa.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX, ba.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX
		if af != bf {
			if af {
				return -1
			}
			return 1
		}
		return aa.GetSince().AsTime().Compare(ba.GetSince().AsTime())
	})
	return out
}

// Legacy-only boards retain their layout against an older daemon.
func (h HomeModel) hasAttentionBoard() bool {
	for _, s := range h.sessions {
		if s.GetAttention() != nil {
			return true
		}
	}
	return false
}

func (h HomeModel) sectionStarts(i int, grouped bool) bool {
	return grouped && (i == 0 || attention.NeedsYou(h.sessions[i-1].GetAttention().GetKind()) != attention.NeedsYou(h.sessions[i].GetAttention().GetKind()))
}

func (h HomeModel) sectionTitle(i int) string {
	needs := attention.NeedsYou(h.sessions[i].GetAttention().GetKind())
	n := 0
	for _, s := range h.sessions {
		if attention.NeedsYou(s.GetAttention().GetKind()) == needs {
			n++
		}
	}
	title := "IN PROGRESS"
	if needs {
		title = "NEEDS YOU"
	}
	return fmt.Sprintf("%s · %d", title, n)
}

func renderAttentionStatus(a *pb.SessionAttention) string {
	switch a.GetKind() {
	case pb.AttentionKind_ATTENTION_KIND_FIX:
		return styleStatusDanger.Render("fix")
	case pb.AttentionKind_ATTENTION_KIND_QUESTION:
		return styleStatusWarning.Render("? question")
	case pb.AttentionKind_ATTENTION_KIND_REVIEW:
		return styleStatusWarning.Render("review")
	case pb.AttentionKind_ATTENTION_KIND_MERGE:
		return styleStatusWarning.Render("merge")
	default:
		return styleSubtle.Render(ansi.Truncate(a.GetProgressLabel(), homeStatusWidth, "…"))
	}
}

func attentionSentenceLines(s *pb.Session, width int) []string {
	if !attention.NeedsYou(s.GetAttention().GetKind()) || strings.TrimSpace(s.GetAttention().GetSentence()) == "" {
		return nil
	}
	text := strings.Join(strings.Fields(s.GetAttention().GetSentence()), " ")
	lines := strings.Split(ansi.Hardwrap(ansi.Wrap(text, max(1, width), ""), max(1, width), false), "\n")
	if len(lines) > 4 {
		lines = lines[:4]
		lines[3] = ansi.Truncate(lines[3], max(1, width)-1, "") + "…"
	}
	return lines
}

func (h HomeModel) attentionSentenceWidth() int {
	for _, c := range h.table.Columns() {
		if c.Title == "NAME" {
			return c.Width
		}
	}
	return 60
}

// attentionActionLabels follows the daemon's available actions and their order.
func attentionActionLabels(a *pb.SessionAttention) []string {
	labels := make([]string, 0, len(a.GetActions()))
	for _, action := range a.GetActions() {
		key, label := attentionActionHint(action)
		if key != "" {
			labels = append(labels, fmt.Sprintf("[%s] %s", key, label))
		}
	}
	return labels
}

func attentionActionHint(action pb.AttentionAction) (key, label string) {
	switch action {
	case pb.AttentionAction_ATTENTION_ACTION_RESUME:
		return "r", "resume"
	case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
		return "x", "dismiss"
	case pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT:
		return "enter", "open the failed chat"
	case pb.AttentionAction_ATTENTION_ACTION_ANSWER:
		return "enter", "answer in the chat"
	case pb.AttentionAction_ATTENTION_ACTION_APPROVE:
		return "y", "approve"
	case pb.AttentionAction_ATTENTION_ACTION_SEND_BACK:
		return "b", "send back"
	case pb.AttentionAction_ATTENTION_ACTION_GITHUB:
		return "g", "github"
	case pb.AttentionAction_ATTENTION_ACTION_VIDEO:
		return "v", "video"
	case pb.AttentionAction_ATTENTION_ACTION_MERGE:
		return "m", "merge"
	case pb.AttentionAction_ATTENTION_ACTION_TRACKER:
		return "l", "tracker"
	default:
		return "", ""
	}
}
