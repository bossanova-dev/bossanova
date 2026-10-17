package views

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/prverdict"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/proto"
)

type attentionActionBinding struct {
	action pb.AttentionAction
	key    string
	label  string
	// details marks an action the session details view also offers. That view
	// keeps its own t ([t]erminal) and [l]inear tracker button, so the actions
	// bound there stay Home-only.
	details bool
	// homeHidden keeps the key working on Home without advertising it in the
	// footer, like Home's [r]ename.
	homeHidden bool
	// detailsHidden keeps the key working on the session details view without
	// advertising it in that view's action bar.
	detailsHidden bool
}

// footer is the action-bar hint; the action bar itself compacts "[m] merge"
// to "[m]erge".
func (b attentionActionBinding) footer() string {
	return "[" + b.key + "] " + b.label
}

var attentionActionBindings = []attentionActionBinding{
	{action: pb.AttentionAction_ATTENTION_ACTION_MERGE, key: "m", label: "merge", details: true},
	// [x] and [r] act whenever the attention offers them, though neither
	// footer names them: the operator asked for a shorter bar. This is the
	// deliberate exception to "a hidden shortcut must do nothing", which
	// guards keys whose action is unavailable, not keys left unadvertised.
	{action: pb.AttentionAction_ATTENTION_ACTION_DISMISS, key: "x", label: "dismiss", details: true, homeHidden: true, detailsHidden: true},
	{action: pb.AttentionAction_ATTENTION_ACTION_RESUME, key: "r", label: "resume", details: true, homeHidden: true, detailsHidden: true},
	{action: pb.AttentionAction_ATTENTION_ACTION_GITHUB, key: "g", label: "github", details: true, homeHidden: true},
	{action: pb.AttentionAction_ATTENTION_ACTION_VIDEO, key: "v", label: "video", details: true},
	{action: pb.AttentionAction_ATTENTION_ACTION_TRACKER, key: "t", label: "tracker", homeHidden: true},
}

func attentionBinding(s *pb.Session, key string, details bool) (attentionActionBinding, bool) {
	for _, b := range attentionActionBindings {
		if b.key == key && (!details || b.details) && slices.Contains(s.GetAttention().GetActions(), b.action) {
			return b, true
		}
	}
	return attentionActionBinding{}, false
}

func attentionFooter(s *pb.Session, details bool) []string {
	var labels []string
	for _, a := range s.GetAttention().GetActions() {
		for _, b := range attentionActionBindings {
			if b.action == a && (details && b.details && !b.detailsHidden || !details && !b.homeHidden) {
				labels = append(labels, b.footer())
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
	// unsettled marks a merge whose call ended without bossd's answer, so
	// whether it landed is not known yet.
	unsettled bool
}

type attentionActionState struct {
	action    pb.AttentionAction
	confirm   confirmPrompt
	busy      bool
	sessionID string
	// progress is what the footer says, beside the spinner, while the
	// confirmed action runs; empty reads "Working…".
	progress string
	// waiting is the status line of a merge whose call ended without bossd's
	// answer. A session poll that shows how the merge ended settles it.
	waiting string
}

// awaitingMerge is a merge whose call ended without bossd's answer.
func awaitingMerge(sessionID, waiting string) attentionActionState {
	return attentionActionState{action: pb.AttentionAction_ATTENTION_ACTION_MERGE, sessionID: sessionID, waiting: waiting}
}

func (a attentionActionState) active() bool { return a.busy || a.confirm.active }

// mergeInFlight reports a confirmed [m]erge whose call is still running.
func (a attentionActionState) mergeInFlight() bool {
	return a.busy && a.action == pb.AttentionAction_ATTENTION_ACTION_MERGE
}

// mergeUnresolved reports a confirmed [m]erge whose outcome is not known: its
// call is still running, or it ended unanswered and no poll has settled it.
// Home and the details view offer no attention action on such a session.
func (a attentionActionState) mergeUnresolved() bool {
	return a.mergeInFlight() || a.action == pb.AttentionAction_ATTENTION_ACTION_MERGE && a.waiting != ""
}

// settleUnansweredMerge reads how a poll of the session ends a merge whose call
// went unanswered: its PR merged or closed, or bossd is no longer merging it.
// settled is false while bossd still reports the merge running.
func settleUnansweredMerge(sess *pb.Session) (status string, isErr, settled bool) {
	pr := sess.GetPrNumber()
	switch {
	case sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_MERGED:
		return fmt.Sprintf("Merged PR #%d", pr), false, true
	case sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return fmt.Sprintf("PR #%d was closed without merging", pr), true, true
	case !sess.GetDisplayMerging():
		return fmt.Sprintf("PR #%d did not merge", pr), true, true
	}
	return "", false, false
}

func (a attentionActionState) update(msg tea.Msg) (attentionActionState, tea.Cmd) {
	if a.busy {
		return a, nil
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
	b, ok := attentionBinding(s, key, details)
	if !ok {
		return a, nil, false
	}
	captured, _ := proto.Clone(s).(*pb.Session)
	a = attentionActionState{sessionID: captured.GetId(), action: b.action}
	command := attentionActionCmd(ctx, c, captured, b.action)
	switch b.action {
	case pb.AttentionAction_ATTENTION_ACTION_MERGE:
		a.confirm = newConfirmPrompt(fmt.Sprintf("Merge PR #%d?", captured.GetPrNumber()), command)
		a.progress = fmt.Sprintf("Merging PR #%d…", captured.GetPrNumber())
	case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
		a.confirm = newConfirmPrompt("Dismiss this alert? It returns only if a different failure happens.", command)
	case pb.AttentionAction_ATTENTION_ACTION_RESUME:
		// Both screens also bind a hidden [r]ename, so a resume must not
		// start on a key pressed out of habit.
		a.confirm = newConfirmPrompt("Resume this run?", command)
	default:
		return a, command, true
	}
	return a, nil, true
}

// footer renders the confirmation, or, while the confirmed action runs, its
// progress beside spin (the caller's spinner frame). The confirmation opens
// with its own blank line because confirmPrompt.footer carries none and the
// callers write this straight after the table's last row; the progress and
// Working lines get theirs from their top padding. Both tableHeights reserve
// that extra row, so the table stays on screen.
func (a attentionActionState) footer(width int, spin string) string {
	if a.confirm.active {
		return "\n" + a.confirm.footer(width)
	}
	if a.progress != "" {
		return lipgloss.NewStyle().Padding(actionBarPadY, 2).Foreground(colorWarning).Render(spin + a.progress)
	}
	return styleActionBar.Render("Working…")
}

func (a attentionActionState) lineCount(width int) int {
	return lipgloss.Height(a.footer(width, "")) - 2*actionBarPadY
}

func attentionActionCmd(ctx context.Context, c client.BossClient, s *pb.Session, action pb.AttentionAction) tea.Cmd {
	return func() tea.Msg {
		result := attentionActionResultMsg{sessionID: s.GetId(), action: action}
		switch action {
		case pb.AttentionAction_ATTENTION_ACTION_MERGE:
			result.text, result.err = operatorMerge(ctx, c, s)
			if errors.Is(result.err, errMergeUnsettled) {
				result.text = fmt.Sprintf("Waiting to hear whether PR #%d merged", s.GetPrNumber())
				result.err, result.unsettled = nil, true
			}
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
		case pb.AttentionAction_ATTENTION_ACTION_UNSPECIFIED,
			// Retired: an older daemon may still list them, but no key binds them.
			pb.AttentionAction_ATTENTION_ACTION_APPROVE, pb.AttentionAction_ATTENTION_ACTION_SEND_BACK,
			// Home's enter opens the details view with the target chat selected
			// instead, on every row.
			pb.AttentionAction_ATTENTION_ACTION_OPEN_CHAT, pb.AttentionAction_ATTENTION_ACTION_ANSWER:
			return nil
		}
		return result
	}
}

// operatorMerge runs [m]erge pinned to the head the row showed. A review row
// records the approval first, waiving exactly the park codes it showed.
func operatorMerge(ctx context.Context, c client.BossClient, s *pb.Session) (string, error) {
	head, pr := s.GetPrDisplayHeadSha(), int(s.GetPrNumber())
	if head == "" {
		return "", mergeFailure(fmt.Sprintf("Couldn't merge PR #%d: its latest commit is not known yet", pr))
	}
	a := s.GetAttention()
	review := a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW
	r, err := c.OperatorMergeSession(ctx, s.GetId(), head, review, a.GetParkCodes())
	switch {
	case mergeOutcomeUnknown(err):
		return "", errMergeUnsettled
	case err != nil:
		return "", plainMergeError(pr, err)
	case r.GetApproval().GetReason() == "head-moved":
		return "", headMovedError(pr)
	case !r.GetIsMerged():
		return "", mergeFailure(prverdict.MergeRefusal(pr, r.GetApproval()))
	}
	return prverdict.OperatorMerged(pr, r), nil
}

// errMergeUnsettled is a merge whose call ended before bossd answered. bossd
// runs the merge on regardless, so a session poll settles the outcome.
var errMergeUnsettled = errors.New("merge outcome not known yet")

// mergeOutcomeUnknown reports a merge call that ended without bossd's answer:
// this client stopped waiting or lost the daemon. An answer bossd sent, even
// one with the same code, is an outcome.
func mergeOutcomeUnknown(err error) bool {
	code := connect.CodeOf(err)
	return (code == connect.CodeDeadlineExceeded || code == connect.CodeUnavailable) && !connect.IsWireError(err)
}

// mergeFailure is the result line a merge that did not land leaves on screen.
// It reads as a sentence, so it is a type rather than an error string.
type mergeFailure string

func (f mergeFailure) Error() string { return string(f) }

// headMovedError is the refusal for a PR whose head moved since the row showed
// it: nothing was approved or merged.
func headMovedError(pr int) error {
	return mergeFailure(fmt.Sprintf("PR #%d changed since you opened it, so nothing was merged", pr))
}

// mergeBlockedPrefix opens the daemon's refusal of a PR its merge gate will not
// merge ("merge blocked: gate=<slug>; <detail>...").
const mergeBlockedPrefix = "merge blocked: gate="

// plainMergeError words a failed merge for the operator as one result line: a
// moved head, the gate's own detail without its gate slug or head, or the
// daemon's message.
func plainMergeError(pr int, err error) error {
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	if connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(msg, vcs.ErrHeadMismatch.Error()) {
		return headMovedError(pr)
	}
	if _, rest, ok := strings.Cut(msg, mergeBlockedPrefix); ok {
		_, detail, _ := strings.Cut(rest, "; ")
		detail, _, _ = strings.Cut(detail, "; gated at head")
		detail, _, _ = strings.Cut(detail, "; nothing was approved or merged")
		detail, _, _ = strings.Cut(detail, "; the approval of ")
		refusal := fmt.Sprintf("Couldn't merge PR #%d", pr)
		if detail = strings.TrimSpace(detail); detail != "" {
			refusal += ": " + detail
		}
		if strings.Contains(msg, "stays recorded") {
			refusal += "; your approval stays recorded, so verify may merge it once it passes"
		}
		return mergeFailure(refusal)
	}
	detail := rpcStatusDetail(err)
	if detail == err.Error() {
		detail = msg
	}
	return mergeFailure(fmt.Sprintf("Couldn't merge PR #%d: %s", pr, detail))
}
