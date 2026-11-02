package ui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/recurser/boss/internal/domain/display"
	"github.com/recurser/boss/internal/domain/prverdict"
	"github.com/recurser/boss/internal/domain/rpcerr"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// BlockerAcceptClient is what the accept-blocker flow needs from the daemon.
type BlockerAcceptClient interface {
	ListSessionPRBlockers(ctx context.Context, id string) (*pb.ListSessionPRBlockersResponse, error)
	AcceptSessionPRBlocker(ctx context.Context, req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error)
}

// AcceptBlockerTarget is the PR the flow accepts a blocker on. It holds plain
// values so a host whose rows are not sessions can open the flow too.
type AcceptBlockerTarget struct {
	SessionID string
	PR        int
}

// AcceptBlockersLoadedMsg answers the flow's blocker list. Token ties it to the
// Start that asked, so a late reply never reopens a cancelled picker.
type AcceptBlockersLoadedMsg struct {
	SessionID string
	Token     uint64
	Resp      *pb.ListSessionPRBlockersResponse
	Err       error
}

// AcceptBlockerResultMsg is how the flow ended: Text for the status line, or
// Err when nothing was accepted.
type AcceptBlockerResultMsg struct {
	SessionID string
	Text      string
	Err       error
}

type acceptBlockerStep int

const (
	acceptStepIdle acceptBlockerStep = iota
	acceptStepLoading
	acceptStepPicking
	acceptStepReason
	acceptStepConfirming
	acceptStepBusy
)

// acceptReasonLimit matches the daemon's cap on operator text.
const acceptReasonLimit = 2000

var acceptBlockerTokens atomic.Uint64

// AcceptBlockerFlow picks a review-finding verify blocker, takes a reason and a
// confirmation, then accepts it pinned to the head the picker showed. It is a
// value type that only returns commands and messages; the host routes them.
type AcceptBlockerFlow struct {
	step      acceptBlockerStep
	token     uint64
	target    AcceptBlockerTarget
	send      func(*pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error)
	head      string
	blockers  []*pb.VerifyBlocker
	table     Table
	selected  string
	reason    textinput.Model
	reasonErr string
	confirm   ConfirmPrompt
	height    int
}

// Start opens the flow on target and asks the daemon for its blockers.
func (f AcceptBlockerFlow) Start(ctx context.Context, c BlockerAcceptClient, target AcceptBlockerTarget) (AcceptBlockerFlow, tea.Cmd) {
	token := acceptBlockerTokens.Add(1)
	f = AcceptBlockerFlow{
		step: acceptStepLoading, token: token, target: target, height: f.height,
		send: func(req *pb.AcceptSessionPRBlockerRequest) (*pb.AcceptSessionPRBlockerResponse, error) {
			return c.AcceptSessionPRBlocker(ctx, req)
		},
	}
	return f, func() tea.Msg {
		resp, err := c.ListSessionPRBlockers(ctx, target.SessionID)
		return AcceptBlockersLoadedMsg{SessionID: target.SessionID, Token: token, Resp: resp, Err: err}
	}
}

func (f AcceptBlockerFlow) Active() bool          { return f.step != acceptStepIdle }
func (f AcceptBlockerFlow) Busy() bool            { return f.step == acceptStepBusy }
func (f AcceptBlockerFlow) TextEntryActive() bool { return f.step == acceptStepReason }

// Update takes keys, pastes, window sizes, the flow's own loaded message and
// its result message.
func (f AcceptBlockerFlow) Update(msg tea.Msg) (AcceptBlockerFlow, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		f.height = msg.Height
		f.fitTableHeight()
		return f, nil
	case AcceptBlockersLoadedMsg:
		return f.loaded(msg)
	case AcceptBlockerResultMsg:
		if f.step == acceptStepBusy && msg.SessionID == f.target.SessionID {
			return f.closed(), nil
		}
		return f, nil
	case tea.PasteMsg:
		if f.step != acceptStepReason {
			return f, nil
		}
		var cmd tea.Cmd
		f.reason, cmd = f.reason.Update(msg)
		f.reasonErr = ""
		return f, cmd
	case tea.KeyMsg:
		return f.key(msg)
	}
	return f, nil
}

// closed returns the flow to idle, keeping only the terminal height.
func (f AcceptBlockerFlow) closed() AcceptBlockerFlow {
	return AcceptBlockerFlow{height: f.height}
}

func (f AcceptBlockerFlow) loaded(msg AcceptBlockersLoadedMsg) (AcceptBlockerFlow, tea.Cmd) {
	if f.step != acceptStepLoading || msg.Token != f.token || msg.SessionID != f.target.SessionID {
		return f, nil
	}
	resp, pr := msg.Resp, f.target.PR
	reason := resp.GetReason()
	switch {
	case msg.Err != nil:
		return f.closed(), f.result("", AcceptFailure(RPCStatusDetail(msg.Err)))
	case reason == "" && len(resp.GetBlockers()) == 0:
		reason = "no-blockers"
	case reason == "" && resp.GetHeadSha() == "":
		reason = "head-unknown"
	}
	if reason != "" {
		text, failed := BlockerListWords(pr, reason)
		if failed {
			return f.closed(), f.result("", AcceptFailure(text))
		}
		return f.closed(), f.result(text, nil)
	}
	f.step, f.head, f.blockers = acceptStepPicking, resp.GetHeadSha(), resp.GetBlockers()
	f.table = blockerTable(f.blockers)
	f.fitTableHeight()
	return f, nil
}

// acceptBlockerOverhead approximates the rows around the picker's table (the
// host's banner, the heading and the footer) so paging keys move by a page.
// View sizes the drawn table exactly.
const acceptBlockerOverhead = BannerOverhead + 8

func (f *AcceptBlockerFlow) fitTableHeight() {
	if len(f.blockers) > 0 {
		f.table.SetHeight(ClampedTableHeight(len(f.blockers), f.height, acceptBlockerOverhead))
	}
}

// BlockerListWords is the line a blocker list that offers nothing to accept
// leaves on the status line. failed marks the reasons that are failures rather
// than an honest "nothing here"; an unknown reason is printed as it is.
func BlockerListWords(pr int, reason string) (text string, failed bool) {
	switch reason {
	case "no-blockers":
		return fmt.Sprintf("PR #%d has no standing verify blockers.", pr), false
	case "no-acceptable-blockers":
		return fmt.Sprintf("PR #%d's blockers are checks, conflicts or notes, which cannot be accepted.", pr), false
	case "comments-unreadable":
		return fmt.Sprintf("Couldn't read PR #%d's verify comments.", pr), true
	case "head-unknown":
		return fmt.Sprintf("PR #%d's latest commit is not known yet.", pr), true
	}
	return reason, true
}

func (f AcceptBlockerFlow) result(text string, err error) tea.Cmd {
	msg := AcceptBlockerResultMsg{SessionID: f.target.SessionID, Text: text, Err: err}
	return func() tea.Msg { return msg }
}

func (f AcceptBlockerFlow) key(msg tea.KeyMsg) (AcceptBlockerFlow, tea.Cmd) {
	switch f.step {
	case acceptStepIdle, acceptStepBusy:
		return f, nil
	case acceptStepLoading:
		if msg.String() == "esc" {
			return f.closed(), nil
		}
		return f, nil
	case acceptStepPicking:
		return f.pickKey(msg)
	case acceptStepReason:
		return f.reasonKey(msg)
	case acceptStepConfirming:
		var cmd tea.Cmd
		f.confirm, cmd = f.confirm.Update(msg)
		switch {
		case cmd != nil:
			f.step = acceptStepBusy
		case !f.confirm.Active:
			return f.closed(), nil
		}
		return f, cmd
	}
	return f, nil
}

func (f AcceptBlockerFlow) pickKey(msg tea.KeyMsg) (AcceptBlockerFlow, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return f.closed(), nil
	case "enter":
		i := f.table.Cursor()
		if i < 0 || i >= len(f.blockers) {
			return f, nil
		}
		f.selected, f.step, f.reasonErr = f.blockers[i].GetId(), acceptStepReason, ""
		f.reason = newAcceptReasonInput()
		cmd := f.reason.Focus()
		return f, cmd
	}
	var cmd tea.Cmd
	f.table, cmd = f.table.Update(msg)
	return f, cmd
}

func newAcceptReasonInput() textinput.Model {
	input := textinput.New()
	input.Placeholder = "why this finding can wait for a follow-up"
	input.Prompt = "Reason: "
	input.SetWidth(renameInputWidth)
	input.CharLimit = acceptReasonLimit
	return input
}

func (f AcceptBlockerFlow) reasonKey(msg tea.KeyMsg) (AcceptBlockerFlow, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return f.closed(), nil
	case "enter":
		reason := strings.TrimSpace(f.reason.Value())
		if reason == "" {
			f.reasonErr = "Reason cannot be empty"
			return f, nil
		}
		f.reason.Blur()
		f.step = acceptStepConfirming
		prompt := fmt.Sprintf("Accept %s on PR #%d at %s? Verify files a follow-up ticket and re-judges this commit without it.", f.selected, f.target.PR, shortHead(f.head))
		f.confirm = NewConfirmPrompt(prompt, f.acceptCmd(reason))
		return f, nil
	}
	var cmd tea.Cmd
	f.reason, cmd = f.reason.Update(NormalizePrintableKey(msg))
	f.reasonErr = ""
	return f, cmd
}

// acceptCmd sends the accept pinned to the head the picker showed.
func (f AcceptBlockerFlow) acceptCmd(reason string) func() tea.Msg {
	send, target, blocker := f.send, f.target, f.selected
	req := &pb.AcceptSessionPRBlockerRequest{Id: target.SessionID, BlockerId: blocker, Reason: reason, ExpectedHeadSha: f.head, IsDryRun: false}
	return func() tea.Msg {
		resp, err := send(req)
		text, err := AcceptOutcome(target.PR, blocker, resp, err)
		return AcceptBlockerResultMsg{SessionID: target.SessionID, Text: text, Err: err}
	}
}

// AcceptOutcome words an accept call's answer: the follow-up line when the
// blocker was accepted (now or before), else a sentence error saying why not.
func AcceptOutcome(pr int, blocker string, resp *pb.AcceptSessionPRBlockerResponse, err error) (string, error) {
	switch {
	case MergeOutcomeUnknown(err):
		return "", AcceptFailure(fmt.Sprintf("No answer came back about accepting %s on PR #%d; it may already be recorded, and accepting it again is safe", blocker, pr))
	case rpcerr.KindOf(err) == rpcerr.Unimplemented && !strings.Contains(err.Error(), "only available on a local daemon"):
		return "", AcceptFailure(RPCStatusDetail(err) + "; the daemon is older than boss; upgrade the daemon")
	case err != nil:
		return "", AcceptFailure(RPCStatusDetail(err))
	case !resp.GetIsAccepted() && !resp.GetIsAlreadyAccepted():
		return "", AcceptFailure(prverdict.AcceptRefusal(pr, blocker, resp))
	}
	return prverdict.AcceptLines(pr, resp)[0], nil
}

func shortHead(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func blockerLocation(b *pb.VerifyBlocker) string {
	switch {
	case b.GetFilePath() == "":
		return "-"
	case b.GetLineNumber() > 0:
		return fmt.Sprintf("%s:%d", b.GetFilePath(), b.GetLineNumber())
	}
	return b.GetFilePath()
}

// blockerColumns sizes the picker's columns. Only LOCATION and SUMMARY may
// squeeze, and SUMMARY, the widest, gives way first.
func blockerColumns(blockers []*pb.VerifyBlocker) []ResponsiveColumn {
	severities, locations, summaries, ids := make([]string, len(blockers)), make([]string, len(blockers)), make([]string, len(blockers)), make([]string, len(blockers))
	for i, b := range blockers {
		severities[i], locations[i], summaries[i], ids[i] = b.GetSeverity(), blockerLocation(b), b.GetSummary(), b.GetId()
	}
	severity := display.MaxColWidth("SEVERITY", severities, 10) + TableColumnSep
	id := display.MaxColWidth("ID", ids, 24) + TableColumnSep
	return []ResponsiveColumn{
		{Col: CursorColumn, MinWidth: 1},
		{Col: table.Column{Title: "SEVERITY", Width: severity}, MinWidth: severity},
		{Col: table.Column{Title: "LOCATION", Width: display.MaxColWidth("LOCATION", locations, 40) + TableColumnSep}, MinWidth: 12},
		{Col: table.Column{Title: "SUMMARY", Width: display.MaxColWidth("SUMMARY", summaries, 72) + TableColumnSep}, MinWidth: 12},
		{Col: table.Column{Title: "ID", Width: id}, MinWidth: id},
	}
}

func blockerTable(blockers []*pb.VerifyBlocker) Table {
	rows := make([]table.Row, len(blockers))
	for i, b := range blockers {
		rows[i] = table.Row{"", b.GetSeverity(), blockerLocation(b), b.GetSummary(), b.GetId()}
	}
	return NewBossTable(FitColumns(blockerColumns(blockers), 0), rows, len(rows)+1)
}

// View renders the whole flow body at width × height; spin is the host's
// spinner frame.
func (f AcceptBlockerFlow) View(width, height int, spin string) string {
	pad := lipgloss.NewStyle().Padding(0, 2)
	if f.step == acceptStepLoading {
		return pad.Foreground(ColorWarning).Render(fmt.Sprintf("%sLoading PR #%d's verify blockers…", spin, f.target.PR))
	}
	if f.step == acceptStepIdle {
		return ""
	}
	heading := pad.Foreground(ColorMuted).Render(fmt.Sprintf("Accept a verify blocker · PR #%d at %s", f.target.PR, shortHead(f.head)))
	footer := f.footer(width, spin)
	t := f.table
	cols := FitColumns(blockerColumns(f.blockers), FitAvailWidth(width, 1))
	t.SetColumns(cols)
	t.SetWidth(ColumnsWidth(cols))
	t.SetHeight(ClampedTableHeight(len(f.blockers), height, lipgloss.Height(heading)+lipgloss.Height(footer)+2))
	return heading + "\n\n" + lipgloss.NewStyle().Padding(0, 1).Render(t.View()) + "\n" + footer
}

func (f AcceptBlockerFlow) footer(width int, spin string) string {
	switch f.step {
	case acceptStepReason:
		inputStyle := lipgloss.NewStyle().Padding(ActionBarPadY, 2, 0, 2)
		hintStyle := StyleActionBar.Padding(0, 2, ActionBarPadY, 2)
		if width > 0 {
			inputStyle, hintStyle = inputStyle.MaxWidth(width), hintStyle.MaxWidth(width)
		}
		hints := "[enter] next  [esc] cancel"
		if f.reasonErr != "" {
			hints += "  " + lipgloss.NewStyle().Foreground(ColorDanger).Render(f.reasonErr)
		}
		return inputStyle.Render(f.reason.View()) + "\n" + hintStyle.Render(hints)
	case acceptStepConfirming:
		return "\n" + f.confirm.Footer(width)
	case acceptStepBusy:
		return lipgloss.NewStyle().Padding(ActionBarPadY, 2).Foreground(ColorWarning).Render(fmt.Sprintf("%sAccepting %s on PR #%d…", spin, f.selected, f.target.PR))
	case acceptStepIdle, acceptStepLoading, acceptStepPicking:
	}
	return ActionBarWidth(width, []string{"[enter] select"}, []string{"[esc] cancel"})
}

// CanAcceptBlocker reports whether a session may offer acc[e]pt blocker: an
// open PR with a known head, no repair running, and a blocked verify park
// (FIX with park codes) or a failing status. The daemon's list then says
// whether anything is acceptable.
func CanAcceptBlocker(s *pb.Session) bool {
	if s.GetPrNumber() == 0 || s.GetPrDisplayHeadSha() == "" || s.GetRepairActive() {
		return false
	}
	status := s.GetDisplayStatus()
	if status == pb.DisplayStatus_DISPLAY_STATUS_MERGED || status == pb.DisplayStatus_DISPLAY_STATUS_CLOSED {
		return false
	}
	a := s.GetAttention()
	return status == pb.DisplayStatus_DISPLAY_STATUS_FAILING || a.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && len(a.GetParkCodes()) > 0
}

// AcceptFailure is the status line an accept that did not land leaves on
// screen. It reads as a sentence, so it is a type rather than an error string.
type AcceptFailure string

func (f AcceptFailure) Error() string { return string(f) }
