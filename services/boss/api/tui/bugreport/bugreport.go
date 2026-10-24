// Bug-report modal. ctrl+g from anywhere in the TUI opens this view; ctrl+b is
// retained as a deprecated alias for one release. The new chord appears in the
// Home action bar. Session, git, and environment context are collected
// automatically on submit — the user only writes a free-form description.
package bugreport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/recurser/boss/internal/domain/rpc"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/report"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog/log"
)

// bugReportPhase tracks the current state of the bug-report modal.
type bugReportPhase int

const (
	bugReportPhaseEditing bugReportPhase = iota
	bugReportPhaseSubmitting
	bugReportPhaseSuccess
	bugReportPhaseError
)

// bugReportSuccessDuration is how long the success confirmation shows before
// the modal auto-dismisses back to the previous view.
const bugReportSuccessDuration = 3 * time.Second

// bugReportSubmitTimeout caps the end-to-end submit pipeline (context
// collection + RPC) so the modal never wedges in "Submitting…" when bosso
// is unreachable. The submitting phase swallows all key input so there is
// no user escape hatch without this.
const bugReportSubmitTimeout = 30 * time.Second

// SubmitMsg carries the result of the async submit pipeline
// (CollectContext + ReportBug) so the Update loop can transition phases.
type SubmitMsg struct {
	ReportID string
	Err      error
}

// DismissMsg tells the modal to finish after the success toast has
// been shown for bugReportSuccessDuration.
type DismissMsg struct{}

// bugReportFormData holds the comment on the heap so that huh's Value() pointer
// remains valid across Bubbletea value-receiver copies of Model.
type bugReportFormData struct {
	comment string
}

// Model is the modal view backing ctrl+g. It wraps the previous view:
// on cancel or auto-dismiss, the caller restores previousView without going
// through switchViewMsg so existing view state is preserved.
type Model struct {
	client  Client
	ctx     context.Context
	authMgr *auth.Manager

	previousView   ui.View
	session        *pb.Session
	daemonStatuses map[string]string

	phase bugReportPhase
	form  *huh.Form
	// commentField is the Text field, retained so Update can resize it to its
	// content after every keystroke (resizeComment). Like fd it is a pointer, so
	// it survives bubbletea's value-receiver copies of this model.
	commentField *huh.Text
	// formFields is the ordered field slice handed to huh.NewGroup, retained
	// because huh.Form exposes no enumeration; it backs click-to-focus (BOS-512).
	formFields ui.FormFields
	fd         *bugReportFormData
	spinner    spinner.Model

	reportID string
	err      error

	cancel bool
	done   bool
	width  int
}

// NewModel builds a Model bound to the daemon client and auth
// manager. previousView is the View the app was on when ctrl+g was pressed;
// currentSession is the active session (nil when no session context exists);
// daemonStatuses is a snapshot of per-chat or per-session daemon heartbeat
// statuses captured from the active view (nil when the view has none).
func NewModel(
	c Client,
	parentCtx context.Context,
	authMgr *auth.Manager,
	previousView ui.View,
	currentSession *pb.Session,
	daemonStatuses map[string]string,
) Model {
	fd := &bugReportFormData{}
	comment := ui.BossText(fd.comment).
		Title("What went wrong?").
		Placeholder("Describe the issue.").
		Value(&fd.comment)
	form := ui.NewBossForm(comment)

	return Model{
		client:         c,
		ctx:            parentCtx,
		authMgr:        authMgr,
		previousView:   previousView,
		session:        currentSession,
		daemonStatuses: daemonStatuses,
		phase:          bugReportPhaseEditing,
		form:           form,
		commentField:   comment,
		formFields:     ui.NewFormFields(comment),
		fd:             fd,
		spinner:        ui.NewStatusSpinner(),
	}
}

// formPrefix is everything View renders above the huh form — nothing, in this
// modal. It exists so the click hit test and the render read the same figure:
// a title line added here later shifts both together (BOS-512).
func (m Model) formPrefix() string { return "" }

// Cancelled returns true when the user aborted the modal.
func (m Model) Cancelled() bool { return m.cancel }

// Done returns true when the modal has completed (success auto-dismiss, or
// user dismissed the error toast).
func (m Model) Done() bool { return m.done }

// PreviousView returns the View enum the app was on when the modal opened.
// The caller uses it to restore the prior screen without recreating it.
func (m Model) PreviousView() ui.View { return m.previousView }

// textEntryActive reports whether the huh comment form owns keystrokes, so App
// can leave ctrl+x alone rather than aliasing it onto Esc (BOS-660).
//
// This mirrors the guard Update uses before handing a key to the form, so the
// submitting / success / error phases — whose Esc dismisses the modal — stay
// eligible for the alias. `m.form != nil` alone is NOT that test: the form is
// built in the constructor and never cleared, so it is non-nil in every phase.
func (m Model) textEntryActive() bool {
	return m.phase == bugReportPhaseEditing && ui.FormOnScreen(m.form)
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.form.Init(), m.spinner.Tick)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case SubmitMsg:
		if msg.Err != nil {
			m.phase = bugReportPhaseError
			m.err = msg.Err
			return m, nil
		}
		m.phase = bugReportPhaseSuccess
		m.reportID = msg.ReportID
		return m, tea.Tick(bugReportSuccessDuration, func(time.Time) tea.Msg {
			return DismissMsg{}
		})

	case DismissMsg:
		m.done = true
		return m, nil

	case tea.KeyMsg:
		switch m.phase {
		case bugReportPhaseSuccess:
			// Any key (especially esc) dismisses early.
			m.done = true
			return m, nil
		case bugReportPhaseError:
			if msg.String() == "esc" || msg.String() == "enter" {
				m.done = true
				return m, nil
			}
			return m, nil
		case bugReportPhaseSubmitting:
			// Ignore keys while submitting; let the RPC finish.
			return m, nil
		case bugReportPhaseEditing:
			if msg.String() == "esc" {
				m.cancel = true
				return m, nil
			}
		}
	}

	if m.phase != bugReportPhaseEditing || m.form == nil {
		return m, nil
	}

	// Click-to-focus, ahead of huh: huh v2 has no mouse support, so every
	// mouse-shaped message is consumed here rather than forwarded (BOS-512).
	if cmd, handled := m.formFields.HandleMouse(msg, m.form, ui.LinesBefore(m.formPrefix())); handled {
		return m, cmd
	}

	// Grow the comment box for what this message is about to type before huh
	// sees it, so the textarea never scrolls to find its own cursor.
	m.resizeCommentFor(ui.BossTextPendingInsert(msg))
	_, cmd := m.form.Update(msg)
	m.resizeComment()

	if m.form.State == huh.StateAborted {
		m.cancel = true
		return m, nil
	}

	if m.form.State == huh.StateCompleted {
		m.phase = bugReportPhaseSubmitting
		return m, tea.Batch(m.submit(), m.spinner.Tick)
	}

	return m, cmd
}

// resizeComment re-sizes the comment field to its current content, so the box
// tracks what the user has typed instead of sitting at the textarea's fixed
// default height. huh's Text.Lines is a plain textarea.SetHeight, which clamps
// itself, so calling it every update is cheap and safe.
func (m *Model) resizeComment() {
	m.resizeCommentFor("")
}

// resizeCommentFor is resizeComment for the content plus pending — the text a
// message is about to insert. Call it with the pending insert *before* handing
// the message to huh and with "" after; bossTextPendingInsert explains why the
// second call alone leaves the box scrolled off its own first line.
func (m *Model) resizeCommentFor(pending string) {
	if m.commentField == nil || m.fd == nil || m.form == nil {
		return
	}
	lines := ui.BossTextLines(m.fd.comment+pending, ui.BossFormWrapWidth())
	m.commentField.Lines(lines)
	// The form height has to move with the field. This modal never sets one, so
	// huh pinned the group's viewport to the height the form had when it was
	// built; a taller textarea would then scroll inside that viewport instead of
	// growing the modal. The cron form does not need this because it re-applies
	// a terminal-derived height of its own.
	m.form.WithHeight(bugReportFormChrome + lines)
}

// bugReportFormChrome is what the form renders around the comment rows: the
// field title. There is no description, no validation error and no huh help
// footer (newBossForm suppresses it), so the whole form is that one line plus
// however many rows the textarea is showing.
const bugReportFormChrome = 1

// submit spawns a safego-protected goroutine that collects context and
// invokes ReportBug. A panic inside either call is recovered and surfaced as
// an error toast rather than killing the TUI. The outer context is wrapped
// with bugReportSubmitTimeout so an unreachable bosso produces an error
// toast after 30s instead of a permanently-stuck spinner.
func (m Model) submit() tea.Cmd {
	parent := m.ctx
	comment := m.fd.comment
	bossClient := m.client
	authMgr := m.authMgr
	session := m.session
	daemonStatuses := m.daemonStatuses

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, bugReportSubmitTimeout)
		defer cancel()
		result := SubmitMsg{Err: errors.New("bug report submit panicked")}
		<-safego.Go(log.Logger, func() {
			result = runBugReportSubmit(ctx, bossClient, authMgr, session, daemonStatuses, comment)
		})
		return result
	}
}

// runBugReportSubmit is the synchronous submit pipeline, kept separate so the
// panic-recovery wrapper in submit() has a single call to wrap.
func runBugReportSubmit(
	ctx context.Context,
	bossClient Client,
	authMgr *auth.Manager,
	session *pb.Session,
	daemonStatuses map[string]string,
	comment string,
) SubmitMsg {
	reportClient, err := cloudrpc.NewReportClient(ctx, authMgr)
	if err != nil {
		return SubmitMsg{Err: err}
	}

	reportCtx, err := report.CollectContext(ctx, bossClient, session, daemonStatuses)
	if err != nil {
		return SubmitMsg{Err: fmt.Errorf("collect context: %w", err)}
	}

	resp, err := cloudrpc.ReportBug(ctx, reportClient, &pb.ReportBugRequest{
		Comment: comment,
		Context: reportCtx,
	})
	if err != nil {
		return SubmitMsg{Err: err}
	}

	return SubmitMsg{ReportID: resp.GetReportId()}
}

func (m Model) View() tea.View {
	padding := lipgloss.NewStyle().Padding(0, 2)

	switch m.phase { //nolint:exhaustive // editing handled below so the form can render
	case bugReportPhaseSubmitting:
		return tea.NewView(
			padding.Foreground(ui.ColorInfo).Render(m.spinner.View() + " Submitting report…"),
		)

	case bugReportPhaseSuccess:
		ref := m.reportID
		if len(ref) > ui.ShortIDLen {
			ref = ref[:ui.ShortIDLen]
		}
		msg := "Report submitted. Thanks"
		if ref != "" {
			msg = fmt.Sprintf("%s — ref %s.", msg, ref)
		} else {
			msg += "."
		}
		return tea.NewView(
			padding.Foreground(ui.ColorSuccess).Render(msg),
		)

	case bugReportPhaseError:
		return tea.NewView(
			ui.RenderError(fmt.Sprintf("Could not submit report: %v", m.err), m.width) + "\n" +
				ui.ActionBarWidth(m.width, []string{"[esc] dismiss"}),
		)
	}

	if m.form == nil {
		return tea.NewView("")
	}

	content := m.formPrefix() +
		lipgloss.NewStyle().PaddingLeft(2).Render(m.form.View()) + "\n" +
		ui.FormActionBarWidth(m.width, []string{"[enter] submit"}, []string{"[esc] cancel"})
	v := tea.NewView(content)
	// Mouse reporting is scoped to form screens (BOS-512): every other phase and
	// every other view keeps the zero value MouseModeNone, so click-drag text
	// selection is only affected while a form is on screen.
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// Client is the daemon capability consumed by this screen.
type Client interface {
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts rpc.SessionReadOptions) ([]*pb.Session, error)
}

func (m *Model) SetWidth(width int)   { m.width = width }
func (m Model) TextEntryActive() bool { return m.textEntryActive() }

func (m Model) Form() *huh.Form { return m.form }

func (m Model) FormFields() ui.FormFields { return m.formFields }
func (m Model) FormPrefix() string        { return m.formPrefix() }
