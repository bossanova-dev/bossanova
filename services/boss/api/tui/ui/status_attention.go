package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/recurser/boss/internal/domain/prverdict"
	"github.com/recurser/boss/internal/domain/rpcerr"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/proto"
)

type AttentionActionBinding struct {
	Action pb.AttentionAction
	Key    string
	Label  string
	// Details marks an action the session Details view also offers. That view
	// keeps its own t ([t]erminal) and [l]inear tracker button, so the actions
	// bound there stay Home-only.
	Details bool
	// HomeHidden keeps the key working on Home without advertising it in the
	// footer, like Home's [r]ename.
	HomeHidden bool
	// DetailsHidden keeps the key working on the session details view without
	// advertising it in that view's action bar.
	DetailsHidden bool
}

// Footer is the action-bar hint; the action bar itself compacts "[m] merge"
// to "[m]erge".
func (b AttentionActionBinding) Footer() string {
	return "[" + b.Key + "] " + b.Label
}

var AttentionActionBindings = []AttentionActionBinding{
	{Action: pb.AttentionAction_ATTENTION_ACTION_MERGE, Key: "m", Label: "merge", Details: true},
	// [x] and [r] act whenever the attention offers them, though neither
	// footer names them: the operator asked for a shorter bar. This is the
	// deliberate exception to "a hidden shortcut must do nothing", which
	// guards keys whose action is unavailable, not keys left unadvertised.
	{Action: pb.AttentionAction_ATTENTION_ACTION_DISMISS, Key: "x", Label: "dismiss", Details: true, HomeHidden: true, DetailsHidden: true},
	{Action: pb.AttentionAction_ATTENTION_ACTION_RESUME, Key: "r", Label: "resume", Details: true, HomeHidden: true, DetailsHidden: true},
	{Action: pb.AttentionAction_ATTENTION_ACTION_GITHUB, Key: "g", Label: "github", Details: true, HomeHidden: true},
	{Action: pb.AttentionAction_ATTENTION_ACTION_VIDEO, Key: "v", Label: "video", Details: true},
	{Action: pb.AttentionAction_ATTENTION_ACTION_TRACKER, Key: "t", Label: "tracker", HomeHidden: true},
}

func AttentionBinding(s *pb.Session, key string, details bool) (AttentionActionBinding, bool) {
	for _, b := range AttentionActionBindings {
		if b.Key == key && (!details || b.Details) && slices.Contains(s.GetAttention().GetActions(), b.Action) {
			return b, true
		}
	}
	return AttentionActionBinding{}, false
}

func AttentionFooter(s *pb.Session, details bool) []string {
	var labels []string
	for _, a := range s.GetAttention().GetActions() {
		for _, b := range AttentionActionBindings {
			if b.Action == a && (details && b.Details && !b.DetailsHidden || !details && !b.HomeHidden) {
				labels = append(labels, b.Footer())
				break
			}
		}
	}
	return labels
}

type AttentionActionResultMsg struct {
	Action    pb.AttentionAction
	SessionID string
	Text      string
	Err       error
	// Unsettled marks a merge whose call ended without bossd's answer, so
	// whether it landed is not known yet.
	Unsettled bool
}

type AttentionActionState struct {
	Action    pb.AttentionAction
	Confirm   ConfirmPrompt
	Busy      bool
	SessionID string
	// Progress is what the footer says, beside the spinner, while the
	// confirmed action runs; empty reads "Working…".
	Progress string
	// Waiting is the status line of a merge whose call ended without bossd's
	// answer. A session poll that shows how the merge ended settles it.
	Waiting string
}

// AwaitingMerge is a merge whose call ended without bossd's answer.
func AwaitingMerge(sessionID, waiting string) AttentionActionState {
	return AttentionActionState{Action: pb.AttentionAction_ATTENTION_ACTION_MERGE, SessionID: sessionID, Waiting: waiting}
}

func (a AttentionActionState) Active() bool { return a.Busy || a.Confirm.Active }

// MergeInFlight reports a confirmed [m]erge whose call is still running.
func (a AttentionActionState) MergeInFlight() bool {
	return a.Busy && a.Action == pb.AttentionAction_ATTENTION_ACTION_MERGE
}

// MergeUnresolved reports a confirmed [m]erge whose outcome is not known: its
// call is still running, or it ended unanswered and no poll has settled it.
// Home and the details view offer no attention action on such a session.
func (a AttentionActionState) MergeUnresolved() bool {
	return a.MergeInFlight() || a.Action == pb.AttentionAction_ATTENTION_ACTION_MERGE && a.Waiting != ""
}

// OngoingStatus is the status line of an unresolved merge: the waiting line
// once its call ended unanswered, else the progress line while it runs.
func (a AttentionActionState) OngoingStatus() string {
	switch {
	case !a.MergeUnresolved():
		return ""
	case a.Waiting != "":
		return a.Waiting
	default:
		return a.Progress
	}
}

// SettleUnansweredMerge reads how a poll of the session ends a merge whose call
// went unanswered: its PR merged or closed, or bossd is no longer merging it.
// settled is false while bossd still reports the merge running.
func SettleUnansweredMerge(sess *pb.Session) (status string, isErr, settled bool) {
	pr := sess.GetPrNumber()
	switch {
	case sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_MERGED:
		return fmt.Sprintf("Merged PR #%d", pr), false, true
	case sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_CLOSED:
		return fmt.Sprintf("PR #%d was closed without merging", pr), true, true
	case !sess.GetDisplayMerging():
		return fmt.Sprintf("No answer came back about merging PR #%d, and it has not merged", pr), true, true
	}
	return "", false, false
}

func (a AttentionActionState) Update(msg tea.Msg) (AttentionActionState, tea.Cmd) {
	if a.Busy {
		return a, nil
	}
	if k, ok := msg.(tea.KeyMsg); ok && a.Confirm.Active {
		var cmd tea.Cmd
		a.Confirm, cmd = a.Confirm.Update(k)
		if cmd != nil {
			a.Busy = true
		}
		return a, cmd
	}
	return a, nil
}

func (a AttentionActionState) Start(ctx context.Context, c AttentionClient, s *pb.Session, key string, details bool, openURL func(string) error) (AttentionActionState, tea.Cmd, bool) {
	b, ok := AttentionBinding(s, key, details)
	if !ok {
		return a, nil, false
	}
	captured, _ := proto.Clone(s).(*pb.Session)
	a = AttentionActionState{SessionID: captured.GetId(), Action: b.Action}
	command := AttentionActionCmd(ctx, c, captured, b.Action, openURL)
	switch b.Action {
	case pb.AttentionAction_ATTENTION_ACTION_MERGE:
		a.Confirm = NewConfirmPrompt(fmt.Sprintf("Merge PR #%d?", captured.GetPrNumber()), command)
		a.Progress = fmt.Sprintf("Merging PR #%d…", captured.GetPrNumber())
	case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
		a.Confirm = NewConfirmPrompt("Dismiss this alert? It returns only if a different failure happens.", command)
	case pb.AttentionAction_ATTENTION_ACTION_RESUME:
		// Both screens also bind a hidden [r]ename, so a resume must not
		// start on a key pressed out of habit.
		a.Confirm = NewConfirmPrompt("Resume this run?", command)
	default:
		return a, command, true
	}
	return a, nil, true
}

// Footer renders the confirmation, or, while the confirmed action runs, its
// progress beside spin (the caller's spinner frame). The confirmation opens
// with its own blank line because confirmPrompt.Footer carries none and the
// callers write this straight after the table's last row; the progress and
// Working lines get theirs from their top padding. Both tableHeights reserve
// that extra row, so the table stays on screen.
func (a AttentionActionState) Footer(width int, spin string) string {
	if a.Confirm.Active {
		return "\n" + a.Confirm.Footer(width)
	}
	if a.Progress != "" {
		return lipgloss.NewStyle().Padding(ActionBarPadY, 2).Foreground(ColorWarning).Render(spin + a.Progress)
	}
	return StyleActionBar.Render("Working…")
}

func (a AttentionActionState) LineCount(width int) int {
	return lipgloss.Height(a.Footer(width, "")) - 2*ActionBarPadY
}

func AttentionActionCmd(ctx context.Context, c AttentionClient, s *pb.Session, action pb.AttentionAction, openURL func(string) error) tea.Cmd {
	return func() tea.Msg {
		result := AttentionActionResultMsg{SessionID: s.GetId(), Action: action}
		switch action {
		case pb.AttentionAction_ATTENTION_ACTION_MERGE:
			result.Text, result.Err = OperatorMerge(ctx, c, s)
			if errors.Is(result.Err, ErrMergeUnsettled) {
				result.Text = fmt.Sprintf("Waiting to hear whether PR #%d merged", s.GetPrNumber())
				result.Err, result.Unsettled = nil, true
			}
		case pb.AttentionAction_ATTENTION_ACTION_DISMISS:
			_, result.Err = c.DismissSessionAlert(ctx, s.GetId(), s.GetAttention().GetAlertKey())
			result.Text = "Alert dismissed"
		case pb.AttentionAction_ATTENTION_ACTION_RESUME:
			_, result.Err = c.ResumeSessionRun(ctx, s.GetId())
			result.Text = "Run resumed"
		case pb.AttentionAction_ATTENTION_ACTION_GITHUB:
			result.Err = openURL(s.GetPrUrl())
			result.Text = "Opened GitHub"
		case pb.AttentionAction_ATTENTION_ACTION_VIDEO:
			result.Err = openURL(s.GetAttention().GetEvidenceUrl())
			result.Text = "Opened video"
		case pb.AttentionAction_ATTENTION_ACTION_TRACKER:
			result.Err = openURL(s.GetTrackerUrl())
			result.Text = "Opened issue"
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

// OperatorMerge runs [m]erge pinned to the head the row showed. A review row
// records the approval first, waiving exactly the park codes it showed.
func OperatorMerge(ctx context.Context, c AttentionClient, s *pb.Session) (string, error) {
	head, pr := s.GetPrDisplayHeadSha(), int(s.GetPrNumber())
	if head == "" {
		return "", MergeFailure(fmt.Sprintf("Couldn't merge PR #%d: its latest commit is not known yet", pr))
	}
	a := s.GetAttention()
	review := a.GetKind() == pb.AttentionKind_ATTENTION_KIND_REVIEW
	r, err := c.OperatorMergeSession(ctx, s.GetId(), head, review, a.GetParkCodes())
	switch {
	case MergeOutcomeUnknown(err):
		return "", ErrMergeUnsettled
	case err != nil:
		return "", PlainMergeError(pr, err)
	case r.GetApproval().GetReason() == "head-moved":
		return "", HeadMovedError(pr)
	case !r.GetIsMerged():
		return "", MergeFailure(prverdict.MergeRefusal(pr, r.GetApproval()))
	}
	return prverdict.OperatorMerged(pr, r), nil
}

// ErrMergeUnsettled is a merge whose call ended before bossd answered. bossd
// runs the merge on regardless, so a session poll settles the outcome.
var ErrMergeUnsettled = errors.New("merge outcome not known yet")

// MergeOutcomeUnknown reports a merge call that ended without bossd's answer:
// this client stopped waiting or lost the daemon. An answer bossd sent, even
// one with the same code, is an outcome.
func MergeOutcomeUnknown(err error) bool {
	code := rpcerr.KindOf(err)
	return (code == rpcerr.DeadlineExceeded || code == rpcerr.Unavailable) && !rpcerr.IsWireError(err)
}

// MergeFailure is the result line a merge that did not land leaves on screen.
// It reads as a sentence, so it is a type rather than an error string.
type MergeFailure string

func (f MergeFailure) Error() string { return string(f) }

// HeadMovedError is the refusal for a PR whose head moved since the row showed
// it: nothing was approved or merged.
func HeadMovedError(pr int) error {
	return MergeFailure(fmt.Sprintf("PR #%d changed since you opened it, so nothing was merged", pr))
}

// MergeBlockedPrefix opens the daemon's refusal of a PR its merge gate will not
// merge ("merge blocked: gate=<slug>; <detail>...").
const MergeBlockedPrefix = "merge blocked: gate="

// PlainMergeError words a failed merge for the operator as one result line: a
// moved head, the gate's own detail without its gate slug or head, or the
// daemon's message.
func PlainMergeError(pr int, err error) error {
	msg := rpcerr.Message(err)
	if rpcerr.KindOf(err) == rpcerr.FailedPrecondition && strings.Contains(msg, vcs.ErrHeadMismatch.Error()) {
		return HeadMovedError(pr)
	}
	if _, rest, ok := strings.Cut(msg, MergeBlockedPrefix); ok {
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
		return MergeFailure(refusal)
	}
	detail := RPCStatusDetail(err)
	if detail == err.Error() {
		detail = msg
	}
	return MergeFailure(fmt.Sprintf("Couldn't merge PR #%d: %s", pr, detail))
}

// AttentionClient supplies the operations shared by the board and session details.
type AttentionClient interface {
	DismissSessionAlert(context.Context, string, string) (*pb.Session, error)
	ResumeSessionRun(context.Context, string) (*pb.Session, error)
	OperatorMergeSession(context.Context, string, string, bool, []string) (*pb.OperatorMergeSessionResponse, error)
}
