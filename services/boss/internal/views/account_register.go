package views

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// registerState is the accountRegisterModel step machine.
type registerState int

const (
	// registerStateProvider is the initial provider chooser (claude | codex).
	// A single [a]dd launch covers both providers via this first step (BOS-267
	// decision: one entry point rather than two list actions).
	registerStateProvider registerState = iota
	// registerStateProgress: the flow is running and only surfacing Say lines.
	registerStateProgress
	// registerStateAwaitText / Secret / Confirm: a blocking prompt is showing
	// its input widget and waiting for the operator's answer.
	registerStateAwaitText
	registerStateAwaitSecret
	registerStateAwaitConfirm
	// registerStateDone: the flow succeeded; the app pops back to a refreshed
	// accounts list.
	registerStateDone
	// registerStateError: the flow returned an error; the operator reads it and
	// presses esc to leave.
	registerStateError
)

// registerProviders is the ordered provider chooser.
var registerProviders = []string{"claude", "codex"}

// progressTailMax bounds how many trailing Say lines the progress log renders so
// a long walkthrough stays concise (acceptance: bounded log tail).
const progressTailMax = 8

// AccountRegisterModel is the native TUI add-account flow for Claude and Codex
// (BOS-267). It reuses accountflow.RunClaudeAdd / RunCodexAdd unchanged, driving
// them on a background goroutine behind a channel-bridge Prompter (tuiPrompter)
// so the synchronous walkthrough renders as a multi-step Bubble Tea view.
//
// It mirrors the multi-step idioms of account_edit.go / cron_form.go: a state
// enum, a textinput reused across prompts, an action bar, theme styling, a
// returnView, and Cancelled()/Done() signals the App routes on.
//
// CREDENTIAL SAFETY: raw tokens, device codes, and auth.json contents are masked
// at the accountflow layer before reaching Say, and the bridge adds no new sink,
// so nothing secret is ever appended to progress or rendered here.
type AccountRegisterModel struct {
	// client is the daemon view the flow stores/tests through (the app's
	// BossClient satisfies accountflow.AccountClient).
	client accountflow.AccountClient
	ctx    context.Context

	// exec is the subprocess seam. Defaults to the DevNull-stdin variant (Bubble
	// Tea owns os.Stdin); tests inject a fake.
	exec accountflow.Exec

	// scratchDir / homeDir override the flows' temp-dir factories in tests; nil
	// uses the accountflow defaults (a 0700 os.MkdirTemp).
	scratchDir func() (string, error)
	homeDir    func() (string, error)

	provider string
	state    registerState

	// provider chooser cursor.
	providerCursor int

	// flow plumbing, created when a provider is chosen.
	flowCtx  context.Context
	cancel   context.CancelFunc
	prompter *tuiPrompter
	donec    chan error
	// flowDone closes when the flow goroutine exits (leak guard / tests).
	flowDone <-chan struct{}

	// pending is the in-flight blocking prompt awaiting an answer.
	pending promptRequest
	input   textinput.Model

	progress  []string
	err       error
	done      bool
	cancelled bool

	returnView View
	width      int
	height     int
}

// NewAccountRegisterModel constructs the add-account flow. c is the app's
// BossClient (satisfies accountflow.AccountClient); the flow starts once a
// provider is chosen.
func NewAccountRegisterModel(c accountflow.AccountClient, ctx context.Context) AccountRegisterModel {
	ti := textinput.New()
	ti.SetWidth(60)
	return AccountRegisterModel{
		client: c,
		ctx:    ctx,
		exec:   accountflow.NewDevNullStdinExec(),
		state:  registerStateProvider,
		input:  ti,
	}
}

// Cancelled reports whether the operator dismissed the flow (esc/ctrl+c).
func (m AccountRegisterModel) Cancelled() bool { return m.cancelled }

// Done reports whether the flow completed successfully (the App then pops back
// to a refreshed accounts list so the new account appears).
func (m AccountRegisterModel) Done() bool { return m.done }

// Init satisfies tea.Model. The flow does not start until a provider is chosen,
// so there is nothing to launch yet.
func (m AccountRegisterModel) Init() tea.Cmd { return nil }

func (m AccountRegisterModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case progressMsg:
		m.appendProgress(msg.line)
		// Re-arm the progress reader for the next Say line.
		return m, readProgressCmd(m.prompter)

	case promptRequestMsg:
		return m.handlePromptRequest(msg.req)

	case flowDoneMsg:
		return m.handleFlowDone(msg.err)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward everything else (paste/IME) to the active input while a prompt is
	// showing so text entry behaves normally.
	if m.state == registerStateAwaitText || m.state == registerStateAwaitSecret || m.state == registerStateAwaitConfirm {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleKey routes key presses by state. Esc/ctrl+c always tears the flow down.
func (m AccountRegisterModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		return m.teardown(), nil
	}

	switch m.state { //nolint:exhaustive // progress/done/error consume no keys but esc/ctrl+c (handled above)
	case registerStateProvider:
		return m.handleProviderKey(msg)
	case registerStateAwaitText, registerStateAwaitSecret:
		if msg.String() == "enter" {
			return m.answerText()
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	case registerStateAwaitConfirm:
		// A y/N confirm accepts a single keypress (y/Y → yes, n/N → no) matching
		// the shared confirm idiom, while [enter] still resolves to the default.
		switch msg.String() {
		case "enter":
			return m.answerConfirm()
		case "y", "Y":
			return m.answerConfirmValue(true)
		case "n", "N":
			return m.answerConfirmValue(false)
		}
		return m, nil
	}
	return m, nil
}

// handleProviderKey moves the chooser cursor and launches the flow on enter.
func (m AccountRegisterModel) handleProviderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.providerCursor > 0 {
			m.providerCursor--
		}
	case "down", "j":
		if m.providerCursor < len(registerProviders)-1 {
			m.providerCursor++
		}
	case "enter", "space":
		return m.startFlow(registerProviders[m.providerCursor])
	}
	return m, nil
}

// startFlow wires the bridge prompter and launches RunClaudeAdd / RunCodexAdd on
// a background goroutine, returning the reader Cmds that surface its progress
// and prompt requests.
func (m AccountRegisterModel) startFlow(provider string) (tea.Model, tea.Cmd) {
	m.provider = provider
	m.state = registerStateProgress

	// cancel is retained in m.cancel and invoked from teardown() (esc/ctrl+c) and
	// handleFlowDone (flow return), so the context is always released.
	flowCtx, cancel := context.WithCancel(m.ctx) //nolint:gosec // G118: cancel stored in m.cancel, called in teardown()/handleFlowDone
	m.flowCtx = flowCtx
	m.cancel = cancel
	m.prompter = newTUIPrompter(flowCtx)
	donec := make(chan error, 1)
	m.donec = donec

	// Capture the values the goroutine needs so it does not read the model copy.
	prov := provider
	prompter := m.prompter
	exec := m.exec
	client := m.client
	scratch := m.scratchDir
	home := m.homeDir

	m.flowDone = safego.Go(zerolog.Nop(), func() {
		var err error
		switch prov {
		case "codex":
			err = accountflow.RunCodexAdd(flowCtx, accountflow.CodexOptions{
				Exec:     exec,
				Prompter: prompter,
				Client:   client,
				HomeDir:  home,
			})
		default: // claude
			err = accountflow.RunClaudeAdd(flowCtx, accountflow.ClaudeOptions{
				Exec:       exec,
				Prompter:   prompter,
				Client:     client,
				ScratchDir: scratch,
			})
		}
		donec <- err
	})

	return m, tea.Batch(
		readProgressCmd(m.prompter),
		readRequestCmd(m.prompter),
		readDoneCmd(m.donec),
	)
}

// readDoneCmd blocks for the flow's result and yields it as a flowDoneMsg.
func readDoneCmd(donec chan error) tea.Cmd {
	return func() tea.Msg {
		err := <-donec
		return flowDoneMsg{err: err}
	}
}

// handlePromptRequest switches to the matching awaiting-state and configures the
// input widget for the incoming blocking prompt.
func (m AccountRegisterModel) handlePromptRequest(req promptRequest) (tea.Model, tea.Cmd) {
	m.pending = req
	m.input.SetValue("")
	m.input.EchoMode = textinput.EchoNormal
	switch req.kind {
	case promptKindSecret:
		m.state = registerStateAwaitSecret
		m.input.EchoMode = textinput.EchoPassword
	case promptKindConfirm:
		m.state = registerStateAwaitConfirm
	default:
		m.state = registerStateAwaitText
	}
	return m, m.input.Focus()
}

// answerText resolves an Ask / AskSecret prompt: an empty entry falls back to
// the prompt default, then the answer is pushed to the flow and the request
// reader re-armed.
func (m AccountRegisterModel) answerText() (tea.Model, tea.Cmd) {
	val := m.input.Value()
	if val == "" {
		val = m.pending.def
	}
	m.reply(promptResponse{text: val})
	return m.resumeProgress()
}

// answerConfirm resolves a Confirm prompt: empty falls back to the default,
// otherwise y/yes → true and anything else → false.
func (m AccountRegisterModel) answerConfirm() (tea.Model, tea.Cmd) {
	raw := strings.ToLower(strings.TrimSpace(m.input.Value()))
	ok := m.pending.defBool
	switch raw {
	case "":
		// keep default
	case "y", "yes":
		ok = true
	default:
		ok = false
	}
	m.reply(promptResponse{ok: ok})
	return m.resumeProgress()
}

// answerConfirmValue resolves a Confirm prompt from a single y/n keypress,
// bypassing the text field.
func (m AccountRegisterModel) answerConfirmValue(ok bool) (tea.Model, tea.Cmd) {
	m.reply(promptResponse{ok: ok})
	return m.resumeProgress()
}

// reply pushes the answer to the pending prompt's one-shot channel (buffered, so
// this never blocks even if the flow goroutine has already unwound on ctx
// cancel).
func (m AccountRegisterModel) reply(resp promptResponse) {
	if m.pending.reply != nil {
		m.pending.reply <- resp
	}
}

// resumeProgress returns to the progress state and re-arms the request reader
// for the next blocking prompt.
func (m AccountRegisterModel) resumeProgress() (tea.Model, tea.Cmd) {
	m.pending = promptRequest{}
	m.input.Blur()
	m.state = registerStateProgress
	return m, readRequestCmd(m.prompter)
}

// handleFlowDone lands on the done or error screen. Success sets done so the App
// pops back to a refreshed accounts list; an error keeps the view open for the
// operator to read and dismiss with esc.
func (m AccountRegisterModel) handleFlowDone(err error) (tea.Model, tea.Cmd) {
	// Drain any trailing Say lines into the rendered tail BEFORE cancelling. The
	// flow goroutine only sends to donec after RunClaudeAdd/RunCodexAdd returns,
	// which happens after its final Say, so those lines — the device-auth-disabled
	// remediation, or the "registered and verified" milestone — are already
	// buffered and no new line can arrive. Cancelling first would let
	// readProgressCmd's select lose the race to ctx.Done() and drop them, blanking
	// the remediation on the error screen (the acceptance-criteria path).
	m.drainProgress()

	// The flow goroutine has returned, so release the flow context (unblocks the
	// bridge readers, which retire on ctx.Done()).
	if m.cancel != nil {
		m.cancel()
	}
	if err != nil {
		m.err = err
		m.state = registerStateError
		return m, nil
	}
	m.done = true
	m.state = registerStateDone
	return m, nil
}

// teardown cancels the flow context (unblocking any pending prompt and killing
// the subprocess) and marks the view cancelled so the App pops back.
func (m AccountRegisterModel) teardown() AccountRegisterModel {
	if m.cancel != nil {
		m.cancel()
	}
	m.cancelled = true
	return m
}

// appendProgress adds a Say line to the bounded progress tail.
func (m *AccountRegisterModel) appendProgress(line string) {
	m.progress = append(m.progress, line)
}

// drainProgress non-blockingly moves any already-buffered Say lines into the
// rendered tail. Safe only once the flow goroutine has returned (no concurrent
// sender remains), so the default case reliably terminates the loop.
func (m *AccountRegisterModel) drainProgress() {
	if m.prompter == nil {
		return
	}
	for {
		select {
		case line := <-m.prompter.progress:
			m.appendProgress(line)
		default:
			return
		}
	}
}

// --- View ---

func (m AccountRegisterModel) View() tea.View {
	if m.state == registerStateProvider {
		return tea.NewView(m.providerView())
	}

	var b strings.Builder

	provider := m.provider
	if provider == "" {
		provider = "account"
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render("Add " + provider + " account"))
	b.WriteString("\n\n")

	// Progress log tail (concise, bounded).
	b.WriteString(m.progressView())

	// Current prompt widget.
	switch m.state { //nolint:exhaustive // provider/progress render no prompt widget (progress log only)
	case registerStateAwaitText, registerStateAwaitSecret:
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.pending.text + ":"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.input.View()))
		b.WriteString("\n")
	case registerStateAwaitConfirm:
		hint := "y/N"
		if m.pending.defBool {
			hint = "Y/n"
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("%s [%s]:", m.pending.text, hint)))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.input.View()))
		b.WriteString("\n")
	case registerStateDone:
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(styleStatusSuccess.Render("Account registered.")))
		b.WriteString("\n")
	case registerStateError:
		b.WriteString(renderError(fmt.Sprintf("Error: %v", m.err), m.width))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch m.state {
	case registerStateAwaitConfirm:
		b.WriteString(actionBar([]string{"[y/enter] confirm"}, []string{"[n/esc] cancel"}))
	case registerStateAwaitText, registerStateAwaitSecret:
		b.WriteString(actionBar([]string{"[enter] submit"}, []string{"[esc] cancel"}))
	case registerStateError:
		b.WriteString(actionBar([]string{"[esc] back"}))
	case registerStateDone:
		b.WriteString(actionBar([]string{"[esc] back"}))
	default:
		b.WriteString(actionBar([]string{"working…"}, []string{"[esc] cancel"}))
	}

	return tea.NewView(b.String())
}

// providerView renders the initial provider chooser.
func (m AccountRegisterModel) providerView() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render("Add an account"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(colorMuted).Render("Choose a provider:"))
	b.WriteString("\n")
	for i, p := range registerProviders {
		cursor := "  "
		line := p
		if i == m.providerCursor {
			cursor = cursorChevron + " "
			line = styleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(cursor + line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(actionBar([]string{"[enter] choose"}, []string{"[esc] back"}))
	return b.String()
}

// progressView renders the bounded tail of Say lines.
func (m AccountRegisterModel) progressView() string {
	if len(m.progress) == 0 {
		return ""
	}
	tail := m.progress
	if len(tail) > progressTailMax {
		tail = tail[len(tail)-progressTailMax:]
	}
	var b strings.Builder
	muted := lipgloss.NewStyle().Foreground(colorMuted)
	for _, line := range tail {
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(muted.Render(line)))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}
