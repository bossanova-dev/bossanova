package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/accountflow"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/telemetry"
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
	// registerStateHandoff: the terminal has been handed to the walkthrough
	// child via tea.Exec. Bubble Tea is NOT rendering or reading input for the
	// duration — the child owns the terminal, which is the whole point (BOS-848).
	registerStateHandoff
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

// AgentAccountRegisterModel is the native TUI add-account flow for Claude and Codex
// (BOS-267). It reuses accountflow.RunClaudeAdd / RunCodexAdd unchanged, driving
// them on a background goroutine behind a channel-bridge Prompter (TUIPrompter)
// so the synchronous walkthrough renders as a multi-step Bubble Tea view.
//
// It mirrors the multi-step idioms of account_edit.go / cron_form.go: a state
// enum, a textinput reused across prompts, an action bar, theme styling, a
// returnView, and Cancelled()/Done() signals the App routes on.
//
// CREDENTIAL SAFETY: raw tokens, device codes, and auth.json contents are masked
// at the accountflow layer before reaching Say, and the bridge adds no new sink,
// so nothing secret is ever appended to progress or rendered here.
type AgentAccountRegisterModel struct {
	// client is the daemon view the flow stores/tests through (the app's
	// BossClient satisfies RegisterClient).
	client    RegisterClient
	ctx       context.Context
	telemetry telemetry.Client

	// exec is the subprocess seam. Left nil in production, where startFlow picks
	// per provider: codex keeps the DevNull-stdin variant, claude goes through
	// the tea.Exec terminal handoff (BOS-848). Tests inject a fake, and a
	// non-nil value is always honoured verbatim — same nil-means-default idiom
	// as scratchDir/homeDir below.
	exec accountflow.Exec

	// scratchDir / homeDir override the flows' temp-dir factories in tests; nil
	// uses the accountflow defaults (a 0700 os.MkdirTemp).
	scratchDir func() (string, error)
	homeDir    func() (string, error)

	// remoteHost is the --host ssh destination this session is driving, or "" for
	// the ordinary local daemon. It is snapshotted at construction rather than
	// read mid-flow: hostDestination is a mutable package global, so re-reading it
	// between the provider chooser and the flow goroutine would make the decision
	// sensitive to a concurrent mutation and make unrelated tests that construct
	// this model order-dependent. Same shape as chatpicker.go's newTabSupported,
	// and overridable in tests the same way exec/scratchDir/homeDir are.
	remoteHost string

	provider string
	state    registerState

	// reauthAccountID is non-empty when this model is running in-place
	// reauthentication (BOS-1142) rather than adding an account. It changes
	// three things: the provider chooser is skipped, the codex flow calls
	// RunCodexReauth instead of RunCodexAdd so the credential is replaced on the
	// existing row rather than stored on a second one, and the telemetry action
	// is recorded as a reauthentication so it cannot inflate the add-failure
	// rate.
	reauthAccountID string

	// provider chooser cursor.
	providerCursor int
	// confirmCursor selects Yes (0) or No (1) for the active confirmation.
	confirmCursor int

	// flow plumbing, created when a provider is chosen.
	flowCtx  context.Context
	cancel   context.CancelFunc
	prompter *TUIPrompter
	donec    chan error
	// flowDone closes when the flow goroutine exits (leak guard / tests).
	flowDone <-chan struct{}

	// execBridge routes the walkthrough child's launch onto the render loop so
	// it can be wrapped in tea.Exec's release/restore bracket. Non-nil only
	// while a claude flow that owns its subprocess is running.
	execBridge *tuiExec

	// pending is the in-flight blocking prompt awaiting an answer.
	pending PromptRequest
	input   textinput.Model

	progress  []string
	err       error
	done      bool
	cancelled bool

	returnView ui.View
	width      int
	height     int
}

// NewAgentAccountRegisterModel constructs the add-account flow. c is the app's
// BossClient (satisfies RegisterClient); the flow starts once a
// provider is chosen.
func NewAgentAccountRegisterModel(c RegisterClient, ctx context.Context) AgentAccountRegisterModel {
	ti := textinput.New()
	ti.SetWidth(60)
	destination := ""
	if ui.IsRemoteHost() {
		destination = ui.RemoteHostDestination()
	}
	return AgentAccountRegisterModel{
		client:     c,
		ctx:        ctx,
		state:      registerStateProvider,
		input:      ti,
		remoteHost: destination,
	}
}

// SetTelemetry installs a telemetry client for the completed add-account flow.
func (m *AgentAccountRegisterModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// Cancelled reports whether the operator dismissed the flow (esc/ctrl+c).
func (m AgentAccountRegisterModel) Cancelled() bool { return m.cancelled }

// flowRunning reports whether the background add-account flow has been launched
// and has not yet reported a result — i.e. a FlowDoneMsg is still owed.
//
// The provider chooser precedes the launch, and Done/Error are the two states
// the result already landed in, so everything between them is in flight. App
// uses this to suppress the global bug-report chord for the duration, because
// swapping activeView would send that FlowDoneMsg to the modal instead (see
// handleGlobalKey).
func (m AgentAccountRegisterModel) flowRunning() bool {
	switch m.state {
	case registerStateProgress, registerStateHandoff, registerStateAwaitText, registerStateAwaitSecret, registerStateAwaitConfirm:
		return true
	case registerStateProvider, registerStateDone, registerStateError:
		return false
	default:
		return false
	}
}

// Done reports whether the flow completed successfully (the App then pops back
// to a refreshed accounts list so the new account appears).
func (m AgentAccountRegisterModel) Done() bool { return m.done }

// textEntryActive reports whether a blocking prompt has the text input
// focused, so App can leave ctrl+x alone rather than aliasing it onto Esc
// (BOS-660). The y/n confirmation prompt is deliberately excluded: it consumes
// no free text, so it is a normal back/cancel screen.
func (m AgentAccountRegisterModel) textEntryActive() bool {
	return m.state == registerStateAwaitText || m.state == registerStateAwaitSecret
}

// Init satisfies tea.Model. The flow does not start until a provider is chosen,
// so there is nothing to launch yet.
func (m AgentAccountRegisterModel) Init() tea.Cmd { return nil }

func (m AgentAccountRegisterModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case ProgressMsg:
		m.appendProgress(msg.Line)
		// Re-arm the progress reader for the next Say line.
		return m, readProgressCmd(m.prompter)

	case PromptRequestMsg:
		return m.handlePromptRequest(msg.Req)

	case execRequestMsg:
		return m.handleExecRequest(msg.req)

	case ExecDoneMsg:
		// The child has exited AND tea.Exec has restored the terminal to Bubble
		// Tea, so the reader is live again. The flow goroutine is still running
		// (it now parses the token and prompts for a label); FlowDoneMsg lands
		// later.
		m.state = registerStateProgress
		return m, nil

	case FlowDoneMsg:
		return m.handleFlowDone(msg.Err)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward everything else (paste/IME) to the active input while a text or
	// secret prompt is showing so text entry behaves normally. Same predicate
	// App consults before aliasing ctrl+x, kept as one definition so the two
	// cannot drift apart.
	if m.textEntryActive() {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// handleKey routes key presses by state. Esc/ctrl+c always tears the flow down.
func (m AgentAccountRegisterModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		// Keep direct y/n shortcuts while making the visible Yes/No controls fully
		// keyboard selectable.
		switch msg.String() {
		case "left", "h":
			m.confirmCursor = 0
		case "right", "l":
			m.confirmCursor = 1
		case "tab":
			m.confirmCursor = 1 - m.confirmCursor
		case "enter", "space":
			return m.answerConfirmValue(m.confirmCursor == 0)
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
func (m AgentAccountRegisterModel) handleProviderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		next, cmd := m.startFlow(registerProviders[m.providerCursor])
		return next, cmd
	}
	return m, nil
}

// beginReauth switches the model into in-place reauthentication for accountID
// and launches the codex flow immediately. There is no provider chooser step:
// the account already has a provider, and the flow replaces that account's
// credential rather than registering a new one.
func (m AgentAccountRegisterModel) beginReauth(accountID string) (AgentAccountRegisterModel, tea.Cmd) {
	m.reauthAccountID = accountID
	return m.startFlow(accountReauthProvider)
}

// startFlow wires the bridge prompter and launches RunClaudeAdd / RunCodexAdd on
// a background goroutine, returning the reader Cmds that surface its progress
// and prompt requests.
// It returns the concrete model rather than tea.Model so beginReauth can chain
// onto it without an unchecked type assertion.
func (m AgentAccountRegisterModel) startFlow(provider string) (AgentAccountRegisterModel, tea.Cmd) {
	m.provider = provider
	m.state = registerStateProgress

	// cancel is retained in m.cancel and invoked from teardown() (esc/ctrl+c) and
	// handleFlowDone (flow return), so the context is always released.
	flowCtx, cancel := context.WithCancel(m.ctx)
	m.flowCtx = flowCtx
	m.cancel = cancel
	m.prompter = NewTUIPrompter(flowCtx)
	donec := make(chan error, 1)
	m.donec = donec

	// Capture the values the goroutine needs so it does not read the model copy.
	prov := provider
	prompter := m.prompter
	client := m.client
	scratch := m.scratchDir
	home := m.homeDir
	remote := m.remoteHost
	reauthID := m.reauthAccountID

	// Pick the subprocess seam. An injected exec (tests) always wins verbatim.
	//
	// In production the two providers diverge deliberately. Codex keeps the
	// DevNull-stdin exec: its device flow only prints a URL and polls, and
	// codexCapture depends on LIVE line streaming for disabled-device-auth
	// detection and its timeout kill, so it must not be wrapped in a handoff
	// that blocks the render loop. Claude goes through the tea.Exec bridge,
	// because `claude setup-token` was observed opening the operator's
	// controlling terminal for reading and contending with Bubble Tea's reader
	// (BOS-848 — see account_register_exec.go).
	//
	// The bridge is only wired for the path that actually spawns: under --host
	// the claude flow runs in PasteMode and starts no child at all.
	exec := m.exec
	if exec == nil {
		if pe := proofRegisterExec(); pe != nil {
			// e2e proof builds only; nil in every production binary.
			exec = pe
		} else if prov == "claude" && remote == "" {
			m.execBridge = newTUIExec(flowCtx)
			exec = m.execBridge
		} else {
			exec = accountflow.NewDevNullStdinExec()
		}
	}

	m.flowDone = safego.Go(zerolog.Nop(), func() {
		var err error
		switch prov {
		case "codex":
			// The --host decision is taken HERE, before Exec.Start, rather than at the
			// chooser: the refusal then rides donec into handleFlowDone, which owns
			// the terminal transition and drains buffered Say lines before cancelling
			// (the BOS-267 ordering). A chooser-level short-circuit would run before
			// m.prompter exists and would bypass that drain entirely.
			if remote != "" {
				donec <- codexRemoteRefusal(remote, reauthID)
				return
			}
			opts := accountflow.CodexOptions{
				Exec:     exec,
				Prompter: prompter,
				Client:   client,
				HomeDir:  home,
			}
			if reauthID != "" {
				// Replace the bound row's credential in place. The flow verifies
				// the account exists and is a codex account BEFORE dialling, so a
				// stale row id costs no browser round-trip.
				err = accountflow.RunCodexReauth(flowCtx, opts, reauthID)
			} else {
				err = accountflow.RunCodexAdd(flowCtx, opts)
			}
		default: // claude
			opts := accountflow.ClaudeOptions{
				Exec:       exec,
				Prompter:   prompter,
				Client:     client,
				ScratchDir: scratch,
			}
			if remote != "" {
				// PasteMode only — StdinUnavailable stays unset, so the operator is
				// still asked to label the account exactly as the local paste path
				// does. The flow spawns nothing and stores through the tunnelled
				// client, so the credential lands on the remote daemon.
				opts.PasteMode = true
				prompter.Say("%s", claudeRemotePasteNotice(remote))
			}
			err = accountflow.RunClaudeAdd(flowCtx, opts)
		}
		donec <- err
	})

	return m, tea.Batch(
		readProgressCmd(m.prompter),
		readRequestCmd(m.prompter),
		readExecRequestCmd(m.execBridge),
		readDoneCmd(m.donec),
	)
}

// registrationRefusedError is a policy refusal raised by the TUI itself rather
// than a failure reported by a registration flow. It renders on the error screen
// like any other error, but it is NOT a failed account-add: nothing was
// attempted, so recording it as one would inflate the add-failure rate with
// screens that never touched a credential.
type registrationRefusedError struct{ msg string }

func (e *registrationRefusedError) Error() string { return e.msg }

// isRegistrationRefusal reports whether err is this view's own policy refusal.
func isRegistrationRefusal(err error) bool {
	var refusal *registrationRefusedError
	return errors.As(err, &refusal)
}

// codexRemoteRefusal is what the codex flow returns instead of spawning a codex
// that is not installed here. It names the destination (the shape
// requireLocalDaemonTarget uses, rather than requireLocalRegistration's
// destination-less one) and carries the exact command that does work, because
// esc from this screen pops back to the accounts list and the operator has to
// carry the remedy away with them.
//
// Codex is refused rather than routed to a paste the way claude is because
// RunCodexAdd always spawns the child and then reads back the auth.json that
// child wrote into a LOCAL scratch HOME. A codex paste route is buildable — the
// daemon takes an opaque blob and agentcred.ValidateCodexAuthJSON already
// exists — but it is not built today.
//
// reauthAccountID splits the remedy. `boss account add codex` is the right
// command only when there is no account yet; told to an operator who came here
// to REPAIR a bound account it registers a second one and leaves the broken
// binding exactly as it was. A reauth therefore names its own command, with the
// id, matching the CLI's own reauth refusal in services/boss/cmd/handlers.go
// (BOS-1142).
func codexRemoteRefusal(destination, reauthAccountID string) error {
	remedy := "boss account add codex"
	if reauthAccountID != "" {
		remedy = "boss account reauth " + reauthAccountID
	}
	return &registrationRefusedError{msg: fmt.Sprintf(
		"codex registration runs `codex login` on this machine, not on %s, and this machine need not have the codex CLI at all. "+
			"Run `%s` in a shell on %s instead.",
		destination, remedy, destination)}
}

// claudeRemotePasteNotice is the line shown before the token prompt in a --host
// session. It has to answer two questions, not one: why the walkthrough the
// operator expected is missing, and — for an operator who has no token yet —
// where a token comes from. Explaining only the first leaves them at a prompt
// they cannot answer.
func claudeRemotePasteNotice(destination string) string {
	return fmt.Sprintf(
		"The claude setup-token walkthrough runs the claude CLI on this machine, not on %s, so it is unavailable in a --host session. "+
			"Run `claude setup-token` in a shell on %s (or anywhere the claude CLI is signed in) and paste the token below; "+
			"it will be stored on %s.",
		destination, destination, destination)
}

// readDoneCmd blocks for the flow's result and yields it as a FlowDoneMsg.
func readDoneCmd(donec chan error) tea.Cmd {
	return func() tea.Msg {
		err := <-donec
		return FlowDoneMsg{Err: err}
	}
}

// handlePromptRequest switches to the matching awaiting-state and configures the
// text input only for prompts that accept free-form text.
func (m AgentAccountRegisterModel) handlePromptRequest(req PromptRequest) (tea.Model, tea.Cmd) {
	m.pending = req
	switch req.Kind {
	case PromptKindSecret:
		m.state = registerStateAwaitSecret
		m.input.SetValue("")
		m.input.EchoMode = textinput.EchoPassword
		return m, m.input.Focus()
	case PromptKindConfirm:
		m.state = registerStateAwaitConfirm
		m.confirmCursor = 1
		if req.DefBool {
			m.confirmCursor = 0
		}
		return m, nil
	default:
		m.state = registerStateAwaitText
		m.input.SetValue("")
		m.input.EchoMode = textinput.EchoNormal
		return m, m.input.Focus()
	}
}

// answerText resolves an Ask / AskSecret prompt: an empty entry falls back to
// the prompt default, then the answer is pushed to the flow and the request
// reader re-armed.
func (m AgentAccountRegisterModel) answerText() (tea.Model, tea.Cmd) {
	val := m.input.Value()
	if val == "" {
		val = m.pending.Def
	}
	m.reply(PromptResponse{Text: val})
	return m.resumeProgress()
}

// answerConfirmValue resolves a Confirm prompt from a single y/n keypress,
// or the selected button.
func (m AgentAccountRegisterModel) answerConfirmValue(ok bool) (tea.Model, tea.Cmd) {
	m.reply(PromptResponse{OK: ok})
	return m.resumeProgress()
}

// reply pushes the answer to the pending prompt's one-shot channel (buffered, so
// this never blocks even if the flow goroutine has already unwound on ctx
// cancel).
func (m AgentAccountRegisterModel) reply(resp PromptResponse) {
	if m.pending.Reply != nil {
		m.pending.Reply <- resp
	}
}

// resumeProgress returns to the progress state and re-arms the request reader
// for the next blocking prompt.
func (m AgentAccountRegisterModel) resumeProgress() (tea.Model, tea.Cmd) {
	m.pending = PromptRequest{}
	m.input.Blur()
	m.state = registerStateProgress
	return m, readRequestCmd(m.prompter)
}

// handleFlowDone lands on the done or error screen. Success sets done so the App
// pops back to a refreshed accounts list; an error keeps the view open for the
// operator to read and dismiss with esc.
func (m AgentAccountRegisterModel) handleFlowDone(err error) (tea.Model, tea.Cmd) {
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
	// An operator teardown (esc/ctrl+c) cancels the flow context, so the flow
	// can return an error that is really a cancellation, not a failed add.
	// A cancelled flow emits nothing.
	//
	// A policy refusal is likewise not a failed add: the flow never ran, never
	// dialled, and never touched a credential, so counting it would report a
	// failure rate for an operation that was declined before it started.
	if !m.cancelled && !isRegistrationRefusal(err) {
		// A reauthentication is not an add: recording it as one would report an
		// add-failure rate for an operation that registered nothing.
		action := ui.TUIActionAgentAccountAdded
		if m.reauthAccountID != "" {
			action = ui.TUIActionAgentAccountReauthed
		}
		ui.CaptureTUIAction(m.ctx, m.telemetry, ui.TUIFeatureAgentAccounts, action, ui.TUIActionStatus(err))
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
func (m AgentAccountRegisterModel) teardown() AgentAccountRegisterModel {
	if m.cancel != nil {
		m.cancel()
	}
	m.cancelled = true
	return m
}

// appendProgress adds a Say line to the bounded progress tail.
func (m *AgentAccountRegisterModel) appendProgress(line string) {
	m.progress = append(m.progress, line)
}

// drainProgress non-blockingly moves any already-buffered Say lines into the
// rendered tail. Safe only once the flow goroutine has returned (no concurrent
// sender remains), so the default case reliably terminates the loop.
func (m *AgentAccountRegisterModel) drainProgress() {
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

func (m AgentAccountRegisterModel) View() tea.View {
	if m.state == registerStateProvider {
		return tea.NewView(m.providerView())
	}

	var b strings.Builder

	provider := m.provider
	if provider == "" {
		provider = "account"
	}
	title := "Add " + provider + " account"
	if m.reauthAccountID != "" {
		// Say which account is being overwritten. A device login that does not
		// name its target reads exactly like the add flow, and the operator has
		// no way to notice they are about to replace the wrong credential.
		title = "Reauthenticate " + provider + " account " + m.reauthAccountID
	}
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render(title))
	b.WriteString("\n\n")

	// Progress log tail (concise, bounded).
	b.WriteString(m.progressView())

	// Current prompt widget.
	switch m.state { //nolint:exhaustive // provider/progress render no prompt widget (progress log only)
	case registerStateAwaitText, registerStateAwaitSecret:
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.pending.Text + ":"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(m.input.View()))
		b.WriteString("\n")
	case registerStateAwaitConfirm:
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(m.pending.Text + ":"))
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Padding(0, 4).Render(ui.RenderButtonRow([]ui.Button{
			{Label: "Yes", Primary: true},
			{Label: "No"},
		}, m.confirmCursor)))
		b.WriteString("\n")
	case registerStateHandoff:
		// Mostly unseen: the terminal is released to the child for the duration,
		// so this frame is what remains on screen at the boundaries. It has to
		// say WHY input is going somewhere else, or a paste that lands in the
		// child looks like the TUI ignoring the operator.
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			"The claude CLI has the terminal — follow its prompts below."))
		b.WriteString("\n")
	case registerStateDone:
		// A reauthentication registered nothing — it replaced one account's
		// secret. Saying "registered" here would tell an operator who came to
		// repair a row that a new row now exists, which is the exact confusion
		// RunCodexReauth was built to avoid (BOS-1142).
		doneLine := "Account registered."
		if m.reauthAccountID != "" {
			doneLine = "Credential replaced."
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(ui.StyleStatusSuccess.Render(doneLine)))
		b.WriteString("\n")
	case registerStateError:
		b.WriteString(ui.RenderError(ui.RPCErrorMessage(m.err), m.width))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	switch m.state {
	case registerStateAwaitConfirm:
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[←/→] select", "[enter] confirm"}, []string{"[esc] cancel"}))
	case registerStateAwaitText, registerStateAwaitSecret:
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[enter] submit"}, []string{"[esc] cancel"}))
	case registerStateHandoff:
		// Deliberately offers no [esc]: Bubble Tea is not reading the terminal,
		// so esc reaches the CHILD, not this model. Cancelling mid-handoff is
		// the child's own interrupt.
		b.WriteString(ui.ActionBarWidth(m.width, []string{"claude has the terminal"}, []string{"[ctrl+c] interrupt claude"}))
	case registerStateError:
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[esc] back"}))
	case registerStateDone:
		b.WriteString(ui.ActionBarWidth(m.width, []string{"[esc] back"}))
	default:
		b.WriteString(ui.ActionBarWidth(m.width, []string{"working…"}, []string{"[esc] cancel"}))
	}

	return tea.NewView(b.String())
}

// providerView renders the initial provider chooser.
func (m AgentAccountRegisterModel) providerView() string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Bold(true).Render("Add an account"))
	b.WriteString("\n\n")
	b.WriteString(lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorMuted).Render("Choose a provider:"))
	b.WriteString("\n")
	for i, p := range registerProviders {
		cursor := "  "
		line := p
		if i == m.providerCursor {
			cursor = ui.CursorChevron + " "
			line = ui.StyleSelected.Render(line)
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(cursor + line))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(ui.ActionBarWidth(m.width, []string{"[enter] choose"}, []string{"[esc] back"}))
	return b.String()
}

// renderProgressLine renders one Say line wrapped to the terminal width, the
// same way ui.RenderError already wraps the error screen.
//
// A Say line is not guaranteed to be short: claudeRemotePasteNotice is a single
// ~326-character line whose SECOND half carries the remedy (`claude
// setup-token`, the destination, "paste the token below"). Rendered without a
// Width, bubbletea clips the frame at the terminal edge and the operator sees
// only the first clause — "the walkthrough is unavailable" — at a prompt they
// then cannot answer.
//
// As in ui.RenderError, lipgloss .Width() sets the TOTAL block width with padding
// included, so the terminal width is passed through as-is, and a width at or
// below the horizontal padding leaves no columns to wrap into and is left
// unconstrained (which is also the width==0 "no WindowSizeMsg yet" case).
func renderProgressLine(line string, width int) string {
	s := lipgloss.NewStyle().Padding(0, 2).Foreground(ui.ColorMuted)
	if width > s.GetHorizontalPadding() {
		s = s.Width(width)
	}
	return s.Render(line)
}

// progressView renders the bounded tail of Say lines.
func (m AgentAccountRegisterModel) progressView() string {
	if len(m.progress) == 0 {
		return ""
	}
	tail := m.progress
	if len(tail) > progressTailMax {
		tail = tail[len(tail)-progressTailMax:]
	}
	var b strings.Builder
	for _, line := range tail {
		b.WriteString(renderProgressLine(line, m.width))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// flowVerdictLine returns the flow's most recent Say line, which for a
// completed flow is its closing verdict. Empty when the flow said nothing.
//
// Deliberately the last line rather than a dedicated success field: whatever
// the flow said last IS the outcome it reported, including the "verification
// couldn't run" wording, so this cannot drift into claiming a verified success
// the flow never claimed.
//
// It reads the prompter's own record, not the rendered tail: the reader Cmd can
// still be holding the closing line as an in-flight message when FlowDoneMsg is
// processed, so m.progress is exactly one line short precisely when the verdict
// matters.
func (m AgentAccountRegisterModel) flowVerdictLine() string {
	if m.prompter == nil {
		return ""
	}
	return m.prompter.lastSaid()
}
