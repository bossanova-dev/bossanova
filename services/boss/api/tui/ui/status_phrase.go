package ui

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/lipgloss/v2"
	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// RenderStatusPhrase styles a's served status phrase, the one STATUS text of
// a session row. A NEEDS YOU phrase reads in warning, a fix (repair failed,
// fix needed, resolve conflict) in danger, and a review or approve merge in
// success; no spinner leads it: the move is the operator's. An IN PROGRESS
// phrase is styled, and led by its spinner, as ProgressStatusStyle styles the
// state it names. ok is false when the daemon serves no phrase (an older
// daemon, or a finished session), so the caller keeps its own cell.
func RenderStatusPhrase(a *pb.SessionAttention, sp spinner.Model) (cell string, ok bool) {
	phrase := a.GetStatusPhrase()
	if phrase == "" {
		return "", false
	}
	if !attention.NeedsYou(a.GetKind()) {
		style, spin := ProgressStatusStyle(phraseProgressKey(a))
		if spin {
			phrase = sp.View() + phrase
		}
		return style.Render(phrase), true
	}
	switch {
	case a.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX:
		return StyleStatusDanger.Render(phrase), true
	case a.GetKind() == pb.AttentionKind_ATTENTION_KIND_MERGE, a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW, phrase == attention.ApproveMergePhrase:
		return StyleStatusSuccess.Render(phrase), true
	default:
		return StyleStatusWarning.Render(phrase), true
	}
}

// phraseProgressKey is the text an IN PROGRESS phrase is styled by. A repair
// styles by its phrase, which names the work under way whatever chat state
// the progress label reports; every other phrase is its progress label, or a
// cut of it, so it styles by the label.
func phraseProgressKey(a *pb.SessionAttention) string {
	if phrase := a.GetStatusPhrase(); strings.HasPrefix(phrase, attention.RepairingLabel) {
		return phrase
	}
	return a.GetProgressLabel()
}

// ProgressStatusStyle styles an in-progress label the way the display status
// styles the same state: stopped muted, idle in warning, running checks as a
// warning spinner, a session still starting or a park on an external event as
// an info spinner, and live work (a repair or rebase included) as a success
// spinner.
func ProgressStatusStyle(label string) (style lipgloss.Style, spin bool) {
	switch {
	case label == StatusStopped, label == attention.VerifyUnknownLabel:
		return StyleStatusMuted, false
	case label == StatusIdle, attention.IsVerifyHoldLabel(label):
		return StyleStatusWarning, false
	case strings.HasPrefix(label, "checking "):
		return StyleStatusWarning, true
	case label == attention.InitializingLabel, attention.IsWaitLabel(label):
		return StyleStatusInfo, true
	default:
		return StyleStatusSuccess, true
	}
}

// SessionStatusDetail is the served status detail on one line. Home draws no
// line for it but its filter reads it, so an area the phrase counts as +N
// still finds the row. It is "" for a row with nothing to add and for a
// daemon that serves no detail.
func SessionStatusDetail(sess *pb.Session) string {
	if sess.GetAttention() == nil {
		return ""
	}
	return strings.Join(strings.Fields(sess.GetAttention().GetStatusDetail()), " ")
}

// SessionStatusText is the status a session header names in its suffix: the
// served phrase while the session has one, else the display status. An
// ending label (merging, archiving, merged, closed) still wins, as on the
// Home row.
func SessionStatusText(sess *pb.Session, sp spinner.Model) string {
	if a := sess.GetAttention(); a != nil && !displaystatus.IsStateEndingLabel(sess.GetDisplayLabel()) {
		if cell, ok := RenderStatusPhrase(a, sp); ok {
			return cell
		}
	}
	return RenderDisplayStatus(sess, sp)
}
