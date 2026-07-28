package views

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
	"github.com/recurser/boss/internal/agent"
	"github.com/recurser/boss/internal/client"
	bosspty "github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/termnorm"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// minLaunchingDisplay is the floor on how long the "Launching... Press
// Ctrl+X to detach" message stays on screen before we hand off to tmux.
// Tuned so users actually see the detach shortcut even on a warm daemon.
const minLaunchingDisplay = 1500 * time.Millisecond

// errChatSessionGone is shown when a resume-attach targets a chat whose
// tmux session no longer exists (e.g. the session was interrupted during
// setup, or torn down by another bossd). Surfacing it through m.err routes
// the user to the dismissable "[esc] back" view instead of handing the
// terminal to a `tmux attach` that would print a raw "can't find session"
// error and trap them. See BOS-108.
var errChatSessionGone = errors.New(
	"this chat's agent session is no longer available (it may have been interrupted during setup) — press esc to go back and start a new chat")

// startExecMsg fires once the minLaunchingDisplay budget has elapsed.
// It triggers the deferred tea.Exec that attaches to tmux.
type startExecMsg struct{}

// attachTmuxEnv returns the environment for the `tmux attach` child with a
// resolvable TERM (falling back to xterm-256color when the user's TERM has no
// terminfo entry on this host) plus the effective TERM string for diagnostics.
// effectiveTERM is read back from the normalized env itself (rather than
// re-deriving it from os.Getenv) so the two never disagree, even when base
// isn't the current process environment (e.g. in tests).
func attachTmuxEnv(base []string) (env []string, effectiveTERM string) {
	env = termnorm.Normalize(base)
	for _, e := range env {
		if strings.HasPrefix(e, "TERM=") {
			return env, strings.TrimPrefix(e, "TERM=")
		}
	}
	return env, ""
}

// pendingExec carries the prepared exec across the launching delay.
// Populated in the attachReadyMsg handler, consumed in startExecMsg.
type pendingExec struct {
	ptycmd      *bosspty.PTYCommand
	tmuxName    string
	prefillPlan string // empty unless first attach with a plan to paste
}

// agentFinishedMsg signals that the interactive Claude process has exited or
// the user has detached from it.
type agentFinishedMsg struct {
	err      error
	detached bool
}

// chatTitleUpdatedMsg signals a best-effort title update completed (ignored).
type chatTitleUpdatedMsg struct{}

// launchDescribedMsg carries the daemon's description of a chat's launch
// command, fetched after the agent pane dies so the error screen can show a
// reproduction command. info is nil when the lookup failed (best-effort).
type launchDescribedMsg struct {
	info *pb.DescribeChatLaunchResponse
}

// attachReadyMsg carries the session plus its chat list, fetched via RPC
// before launching Claude. The chat list is needed so first-attach prefill
// can be gated on len(chats) == 0 (matching the original semantics from the
// daemon-side prefill that was removed in PR #179).
type attachReadyMsg struct {
	session *pb.Session
	chats   []*pb.ClaudeChat
}

// attachErrMsg signals a fetch or launch error.
type attachErrMsg struct {
	err error
}

// AttachModel launches an interactive Claude Code TUI in the session's worktree.
type AttachModel struct {
	client         client.BossClient
	telemetry      telemetry.Client
	ctx            context.Context
	manager        *bosspty.Manager
	sessionID      string
	resumeID       string // Claude Code session UUID to resume (empty = new chat)
	agentSessionID string // The Claude Code session UUID actually launched

	session   *pb.Session
	launching bool  // true while fetching session
	returned  bool  // true after claude exits
	agentErr  error // error from claude process (if any)
	// launchInfo is the daemon's description of the launch command, fetched on
	// error so View can show a copy-pasteable reproduction command. nil until
	// the lookup returns (or stays nil if it failed — best-effort).
	launchInfo *pb.DescribeChatLaunchResponse
	detach     bool
	err        error
	width      int
	height     int

	// overrideAgent populates RecordChat's agent_name override at chat
	// creation time. Empty inherits the parent session's AgentName. Set
	// by the chat picker before pushing onto the attach view.
	overrideAgent string

	// launchStartedAt is captured in NewAttachModel so the minimum-display
	// budget covers the entire launch path, not just the post-RPC tail.
	launchStartedAt time.Time

	// pendingExec is set in the attachReadyMsg handler and consumed by
	// the startExecMsg handler once the launching-display delay elapses.
	pendingExec *pendingExec

	effectiveTERM string // TERM actually handed to `tmux attach`, for diagnostics

	// tmuxName is the tmux session name this attach targets, stashed as soon
	// as it's known so the error screen can offer a reproduction command even
	// though pendingExec (which also carries it) is cleared before the pane
	// exits.
	tmuxName string
	// tmuxTail is tmux's own startup output (e.g. a missing-terminfo error),
	// captured via Manager.RecentOutput on a failed attach — before the
	// process can be evicted — so the error screen can show the real reason
	// instead of a bare "exit status 1".
	tmuxTail string
}

// SetTelemetry installs a telemetry client for successful attach actions.
func (m *AttachModel) SetTelemetry(client telemetry.Client) {
	m.telemetry = client
}

// SetOverrideAgent supplies a per-chat agent override that's forwarded as
// RecordChat's agent_name. Empty inherits the parent session's AgentName.
func (m *AttachModel) SetOverrideAgent(name string) { m.overrideAgent = name }

// NewAttachModel creates an AttachModel for the given session.
// If resumeID is non-empty, Claude Code will be launched with --resume.
func NewAttachModel(c client.BossClient, parentCtx context.Context, manager *bosspty.Manager, sessionID, resumeID string) AttachModel {
	return AttachModel{
		client:          c,
		ctx:             parentCtx,
		manager:         manager,
		sessionID:       sessionID,
		resumeID:        resumeID,
		launching:       true,
		launchStartedAt: time.Now(),
	}
}

func (m AttachModel) Init() tea.Cmd {
	return m.fetchAttachState()
}

func (m AttachModel) fetchAttachState() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, client.SessionReadOptions{})
		if err != nil {
			return attachErrMsg{err: err}
		}
		// Best-effort chat list — used only to gate first-attach prefill.
		// A failure here should not block attach; treat as "unknown count"
		// (i.e. assume non-empty so we don't accidentally prefill on a
		// resumed session).
		chats, _ := m.client.ListChats(m.ctx, m.sessionID)
		return attachReadyMsg{session: sess, chats: chats}
	}
}

func (m AttachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case attachReadyMsg:
		m.session = msg.session

		// Decide whether this is a fresh first attach to a tracker-backed
		// session that should have its plan pasted into Claude's input box.
		// Mirrors the gating that used to live in shouldConsiderPrefill on
		// the daemon side (removed in PR #179 along with the per-chat tmux
		// indirection that hosted it).
		prefillPlan := ""
		if m.resumeID == "" &&
			msg.session.GetTrackerId() != "" &&
			msg.session.GetPlan() != "" &&
			len(msg.chats) == 0 {
			prefillPlan = msg.session.GetPlan()
		}

		// Resolve the Claude session UUID and ask the daemon to record the
		// chat AND ensure a tmux session is hosting it. Doing the create on
		// the daemon side means the tmux server outlives bossd (it
		// double-forks), so the user can kill bossd, restart it, and
		// reattach to the same chat without losing state.
		resume := m.resumeID != ""
		if resume {
			m.agentSessionID = m.resumeID
		} else {
			m.agentSessionID = uuid.New().String()
		}
		chat, err := m.client.RecordChat(m.ctx, m.sessionID, m.agentSessionID, "New chat", m.overrideAgent, resume)
		if err != nil {
			m.err = fmt.Errorf("record chat: %w", err)
			return m, nil
		}
		tmuxName := chat.GetTmuxSessionName()
		if tmuxName == "" {
			m.err = fmt.Errorf("daemon did not return a tmux session name; check that tmux is installed")
			return m, nil
		}
		// Stash on the model (not just pendingExec) so it survives to the
		// agentFinishedMsg handler, which fires after pendingExec has already
		// been cleared.
		m.tmuxName = tmuxName
		// Resume-attach guard: the chat record can outlive its tmux session
		// (interrupted setup, daemon restart, external teardown). If we'd be
		// resuming a session that no longer exists, don't hand the terminal
		// to `tmux attach` — it would dump a raw "can't find session" error
		// and leave the user stranded. Surface a dismissable error instead.
		// New chats (resumeID == "") get a freshly created session from the
		// daemon, so they are guaranteed alive and skip this check.
		if resume && !tmuxSessionAlive(tmuxName) {
			m.err = errChatSessionGone
			m.launching = false
			return m, nil
		}
		if !resume {
			captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatCreated, map[string]any{
				"source": "tui",
			})
		}
		captureViewTelemetry(m.ctx, m.telemetry, telemetry.EventChatAttached, map[string]any{
			"source": "tui",
			"action": "attach",
			"resume": resume,
		})

		// Build the tmux-attach exec command and bosspty wrapper. We do
		// this synchronously so that, by the time the launching-display
		// delay elapses, the manager already knows about the session and
		// the user's Ctrl+X intercept has somewhere to land.
		// #nosec G204 -- tmux attach -t <name>; const argv, app-generated session id; no shell
		// owner=@recurser review-by=2027-01-18 issue=BOS-28
		agentCmd := exec.Command("tmux", "attach", "-t", tmuxName)
		agentCmd.Dir = msg.session.GetWorktreePath()
		env, effTERM := attachTmuxEnv(os.Environ())
		agentCmd.Env = env
		m.effectiveTERM = effTERM
		m.manager.RegisterSession(m.agentSessionID, m.sessionID)
		ptycmd := bosspty.NewPTYCommand(m.manager, m.agentSessionID, agentCmd)

		// Stash everything needed for the deferred exec and stay in the
		// launching state so View() keeps rendering the Ctrl+X hint.
		m.pendingExec = &pendingExec{
			ptycmd:      ptycmd,
			tmuxName:    tmuxName,
			prefillPlan: prefillPlan,
		}

		// Schedule the actual tea.Exec for whenever the rest of the
		// minimum-display budget runs out. If RPCs already took longer
		// than the budget, fire startExecMsg on the next Update cycle.
		remaining := minLaunchingDisplay - time.Since(m.launchStartedAt)
		if remaining <= 0 {
			return m, func() tea.Msg { return startExecMsg{} }
		}
		return m, tea.Tick(remaining, func(time.Time) tea.Msg {
			return startExecMsg{}
		})

	case startExecMsg:
		// Nothing to do if the user already bailed via esc (m.detach)
		// or if some duplicate startExecMsg snuck through after we
		// consumed the stash. Clear pendingExec in both cases so no
		// stale state hangs around.
		if m.detach || m.pendingExec == nil {
			m.pendingExec = nil
			return m, nil
		}
		pe := m.pendingExec
		m.pendingExec = nil
		m.launching = false

		// Kick the prefill goroutine off here (not in attachReadyMsg)
		// so its 3 s processWait window starts when the manager is
		// about to start the *Process, not 1.5 s earlier.
		if pe.prefillPlan != "" {
			go prefillClaudeInput(m.ctx, m.manager, m.agentSessionID, pe.prefillPlan)
		}

		return m, tea.Exec(pe.ptycmd, func(err error) tea.Msg {
			// Clear the primary screen buffer so Claude content doesn't
			// linger in scrollback when boss exits alt screen.
			fmt.Print("\033[2J\033[H\033[3J")
			// Treat as detach if EITHER signal fires:
			//   - bosspty intercepted Ctrl+X / Ctrl+] (user-intent signal)
			//   - the tmux session is still alive (the chat survives even
			//     if bosspty didn't see the keystroke, e.g. user typed the
			//     literal `tmux detach` prefix or detached via the menu).
			detached := pe.ptycmd.Detached || tmuxSessionAlive(pe.tmuxName)
			return agentFinishedMsg{err: err, detached: detached}
		})

	case agentFinishedMsg:
		if msg.detached {
			// User pressed Ctrl+X or Ctrl+] — process is still running in
			// the background; drop back to the caller's view.
			m.detach = true
			return m, nil
		}
		m.returned = true
		m.agentErr = msg.err
		// Clean exits auto-bounce back to the chat picker so the user
		// doesn't have to press esc to dismiss an empty "session ended"
		// screen. Error exits hold the screen so the failure is readable
		// — otherwise an immediately-failing tmux attach (e.g. session
		// torn down by another bossd) looks like the chat just refuses
		// to open, with no clue why.
		if msg.err == nil {
			m.detach = true
		}
		if msg.err != nil {
			// Capture tmux's own startup output (e.g. a missing-terminfo error)
			// before the process is evicted, so the error screen can show the
			// real reason instead of a bare "exit status 1".
			m.tmuxTail = string(m.manager.RecentOutput(m.agentSessionID, 2048))
			// The pane died on launch — ask the daemon what command it ran so
			// the error screen can show a copy-pasteable reproduction.
			// Do this before title cleanup because unused chats have no JSONL
			// title and updateChatTitle deletes that orphan chat row.
			return m, tea.Batch(
				m.reportStopped(),
				tea.Sequence(m.describeLaunch(), m.updateChatTitle()),
			)
		}
		// Best-effort: update chat title from JSONL and report stopped status.
		return m, tea.Batch(m.updateChatTitle(), m.reportStopped())

	case chatTitleUpdatedMsg:
		// Ignored — fire-and-forget.
		return m, nil

	case launchDescribedMsg:
		// Enriches the already-rendered error screen with the reproduction
		// command once the daemon responds. nil info leaves the bare error.
		m.launchInfo = msg.info
		return m, nil

	case attachErrMsg:
		m.err = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			m.detach = true
			return m, nil
		}
	}

	return m, nil
}

// updateChatTitle reads the Claude JSONL file and updates the chat title via RPC.
// If the session was never used (no real user message), it deletes the orphan record.
func (m AttachModel) updateChatTitle() tea.Cmd {
	if m.agentSessionID == "" || m.session == nil {
		return nil
	}
	agentSessionID := m.agentSessionID
	worktreePath := m.session.GetWorktreePath()
	return func() tea.Msg {
		title := agent.ChatTitle(worktreePath, agentSessionID)
		if title != "" {
			_ = m.client.UpdateChatTitle(m.ctx, agentSessionID, title)
		} else if agent.TranscriptAbsentOrEmpty(worktreePath, agentSessionID) {
			// Reap a genuinely-unused orphan ONLY when its transcript is
			// confirmed absent or zero-length. A non-empty transcript whose
			// title won't parse — or any read failure — must never delete the
			// row: a mis-encoded project key once made rich chats look
			// title-less here and this branch destroyed them. See
			// agent.TranscriptAbsentOrEmpty.
			_ = m.client.DeleteChat(m.ctx, agentSessionID)
		}
		return chatTitleUpdatedMsg{}
	}
}

// reportStopped sends a fire-and-forget stopped status to the daemon so other
// clients see the change immediately.
func (m AttachModel) reportStopped() tea.Cmd {
	if m.agentSessionID == "" {
		return nil
	}
	agentSessionID := m.agentSessionID
	return func() tea.Msg {
		_ = m.client.ReportChatStatus(m.ctx, []*pb.ChatStatusReport{{
			AgentSessionId: agentSessionID,
			Status:         pb.ChatStatus_CHAT_STATUS_STOPPED,
			LastOutputAt:   timestamppb.Now(),
		}})
		return nil
	}
}

// describeLaunch asks the daemon for the exact command it used to launch this
// chat's agent so the error view can show a copy-pasteable reproduction. The
// command runs on the daemon host, which may differ from where boss runs, so
// the daemon also reports its hostname. Best-effort: any failure (including the
// remote-orchestrator client, which is local-only) yields a nil info and the
// view falls back to the bare exit error.
func (m AttachModel) describeLaunch() tea.Cmd {
	if m.agentSessionID == "" {
		return nil
	}
	agentSessionID := m.agentSessionID
	return func() tea.Msg {
		info, err := m.client.DescribeChatLaunch(m.ctx, agentSessionID)
		if err != nil {
			return launchDescribedMsg{info: nil}
		}
		return launchDescribedMsg{info: info}
	}
}

// tmuxSessionAlive reports whether a tmux session by the given name is still
// running. Used by the attach flow to distinguish a Ctrl+X / Ctrl+] detach
// (session still alive on the tmux server, claude still running inside) from
// a real claude exit (session torn down by tmux when its only window's
// command exited).
func tmuxSessionAlive(name string) bool {
	if name == "" {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", name).Run() == nil
}

// prefillClaudeInput writes the session plan into Claude's TUI input box via
// a bracketed-paste sequence so the user lands on a ready-to-refine prompt
// instead of an empty input. Best-effort and non-fatal — a missing prefill
// is a UX degradation, not a reason to abort the attach.
//
// Flow:
//  1. Wait for the bosspty.Manager to register the *Process for agentSessionID
//     (tea.Exec calls Manager.GetOrStart on its own goroutine, so there is a
//     small race window between this goroutine starting and the entry being
//     in the map).
//  2. Wait for Claude Code's input-box prompt indicator (❯) to appear
//     in the PTY output. If the marker never shows up within the
//     deadline, paste anyway — same fail-open policy as the daemon-side
//     prefill that this replaces. We match on the prompt character
//     rather than the default footer so users with custom statuslines
//     (which replace the footer entirely) still get the prefill.
//  3. Write \x1b[200~ + plan + \x1b[201~ to the PTY. Bracketed paste tells
//     Claude "treat this as paste, not keystrokes," so it lands in the input
//     box without auto-submitting.
func prefillClaudeInput(ctx context.Context, manager *bosspty.Manager, agentSessionID, plan string) {
	const (
		readyMarker      = "❯"
		recentBufferSize = 4096
		processWait      = 3 * time.Second
		readyWait        = 2 * time.Second
		pollInterval     = 100 * time.Millisecond
	)

	// Step 1: wait for the process to appear in the manager.
	var proc *bosspty.Process
	procDeadline := time.Now().Add(processWait)
	for {
		if p, ok := manager.Get(agentSessionID); ok {
			proc = p
			break
		}
		if time.Now().After(procDeadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 2: wait for Claude's input box to be ready, then paste.
	marker := []byte(readyMarker)
	readyDeadline := time.Now().Add(readyWait)
	for time.Now().Before(readyDeadline) {
		if bytes.Contains(proc.RecentOutput(recentBufferSize), marker) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Step 3: paste. Errors are silently dropped — the process may have
	// exited or detached by now; either way the user can recover by typing
	// the prompt manually.
	payload := append([]byte("\x1b[200~"), plan...)
	payload = append(payload, "\x1b[201~"...)
	_ = proc.WriteInput(payload)
}

// Detached returns true if the user should return to the home screen.
func (m AttachModel) Detached() bool { return m.detach }

// SessionID returns the session ID for navigation after detach.
func (m AttachModel) SessionID() string { return m.sessionID }

// AgentSessionID returns the agent session UUID for navigation after detach.
func (m AttachModel) AgentSessionID() string { return m.agentSessionID }

func agentDisplayName(name string) string {
	switch strings.ToLower(name) {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "":
		return "agent"
	default:
		return name
	}
}

func (m AttachModel) displayAgentName() string {
	if m.overrideAgent != "" {
		return m.overrideAgent
	}
	if m.session != nil {
		return m.session.GetAgentName()
	}
	return ""
}

// renderLaunchDiagnostic formats a copy-pasteable reproduction command from the
// daemon's launch description, shown under the bare exit error so the user can
// run the agent themselves and see the underlying failure. The cause is NOT
// always a PATH/login-shell problem — the agent binary may resolve fine and then
// exit non-zero on its own (e.g. `--session-id <id>` colliding with an existing
// transcript: "Session ID already in use"), and bossd often can't capture that
// stderr before the pane dies. So the prose stays neutral and points the user at
// the reproduction command rather than asserting a single cause. Returns "" when
// no description is available so the caller renders just the bare error.
func renderLaunchDiagnostic(info *pb.DescribeChatLaunchResponse, agentLabel string) string {
	if info == nil || len(info.GetArgv()) == 0 {
		return ""
	}
	prose := lipgloss.NewStyle().Padding(0, 2)
	code := lipgloss.NewStyle().Padding(0, 4)

	where := "the daemon host"
	if host := info.GetHost(); host != "" {
		where = "host '" + host + "'"
	}

	cmd := shellJoin(info.GetArgv())
	if wt := info.GetWorktreePath(); wt != "" {
		cmd = "cd " + shellQuote(wt) + " && \\\n" + cmd
	}

	var b strings.Builder
	// When bossd captured the agent's own final output at pane death (BOS-477),
	// lead with it verbatim (sanitized like the tmux tail so control bytes can't
	// scramble the boss frame) — it is the real error. Otherwise fall back to the
	// neutral prose that points the user at the reproduction command.
	if captured := strings.TrimSpace(sanitizeTmuxTail(info.GetCapturedOutput())); captured != "" {
		b.WriteString(prose.Render(agentLabel + " reported:"))
		b.WriteString("\n\n")
		b.WriteString(code.Render(captured))
		b.WriteString("\n\n")
	} else {
		b.WriteString(prose.Render(fmt.Sprintf(
			"The %s CLI exited before its pane came up. This is often a PATH or\n"+
				"login-shell problem that only reproduces where the agent runs, but it\n"+
				"can also be the agent itself refusing to start. Run the command to see\n"+
				"the underlying error.", agentLabel)))
		b.WriteString("\n\n")
	}
	b.WriteString(prose.Render("bossd ran this on " + where + ":"))
	b.WriteString("\n\n")
	b.WriteString(code.Render(cmd))
	b.WriteString("\n\n")
	b.WriteString(prose.Render("Run it there to see the underlying error."))
	return b.String()
}

// renderTmuxAttachDiagnostic formats a copy-pasteable local `tmux attach`
// reproduction plus the captured tmux startup output, shown under the bare exit
// error so a user can see (and reproduce) the underlying tmux failure — most
// commonly a missing terminfo entry for their TERM. Returns "" when there is no
// session name to reproduce with.
func renderTmuxAttachDiagnostic(tmuxName, effectiveTERM, capturedTail string) string {
	if tmuxName == "" {
		return ""
	}
	prose := lipgloss.NewStyle().Padding(0, 2)
	code := lipgloss.NewStyle().Padding(0, 4)

	repro := "tmux attach -t " + tmuxName
	if effectiveTERM != "" {
		repro = "TERM=" + effectiveTERM + " " + repro
	}

	var b strings.Builder
	b.WriteString(prose.Render("boss attached with:"))
	b.WriteString("\n\n")
	b.WriteString(code.Render(repro))
	b.WriteString("\n\n")
	// capturedTail is raw PTY output: sanitize it before rendering so a tmux
	// failure whose tail carries ANSI/cursor sequences or stray control bytes
	// can't scramble the boss error frame the view is meant to keep control of.
	if tail := strings.TrimSpace(sanitizeTmuxTail(capturedTail)); tail != "" {
		b.WriteString(prose.Render("tmux reported:"))
		b.WriteString("\n\n")
		b.WriteString(code.Render(tail))
		b.WriteString("\n\n")
	}
	b.WriteString(prose.Render(
		"If this is a missing-terminfo error, install the entry on this host\n" +
			"(" + termnorm.InstallHint + ") or run: export TERM=" + termnorm.FallbackTERM))
	return b.String()
}

// sanitizeTmuxTail strips ANSI/CSI escape sequences and any remaining C0
// control bytes (keeping newlines and tabs) from captured PTY output, so a
// tmux startup-failure tail is safe to render inside the boss error frame. The
// common missing-terminfo error is plain stderr text and passes through
// unchanged.
func sanitizeTmuxTail(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// shellJoin renders argv as a single copy-pasteable shell command, quoting any
// argument that contains whitespace or shell-significant characters.
func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps s in single quotes when it contains characters the shell
// would otherwise interpret, escaping embedded single quotes the POSIX way
// ('\”). A safe-looking token is returned unquoted for readability.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if safeShellToken(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// safeShellToken reports whether s consists solely of characters that need no
// shell quoting.
func safeShellToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./:=@", r):
		default:
			return false
		}
	}
	return true
}

func (m AttachModel) View() tea.View {
	if m.err != nil {
		return tea.NewView(
			renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
				styleActionBar.Render("[esc] back"),
		)
	}

	if m.launching {
		var b strings.Builder
		title := m.sessionID
		if m.session != nil {
			title = m.session.Title
		}
		b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(
			fmt.Sprintf("Launching %s for %s...  Press Ctrl+X to detach", agentDisplayName(m.displayAgentName()), title)))
		return tea.NewView(b.String())
	}

	if m.returned {
		var b strings.Builder
		agentLabel := agentDisplayName(m.displayAgentName())
		if m.agentErr != nil {
			b.WriteString(renderError(fmt.Sprintf("%s exited with error: %v", agentLabel, m.agentErr), m.width))
			if diag := renderLaunchDiagnostic(m.launchInfo, agentLabel); diag != "" {
				b.WriteString("\n")
				b.WriteString(diag)
			}
			if diag := renderTmuxAttachDiagnostic(m.tmuxName, m.effectiveTERM, m.tmuxTail); diag != "" {
				b.WriteString("\n")
				b.WriteString(diag)
			}
		} else {
			b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(fmt.Sprintf("%s session ended.", agentLabel)))
		}
		b.WriteString("\n")
		b.WriteString(actionBar([]string{"[esc] back"}))
		return tea.NewView(b.String())
	}

	// Never render a completely empty frame: a blank view lets stray
	// terminal output (e.g. a child process's leftover stderr) masquerade as
	// the boss screen with no affordance. A minimal line guarantees the user
	// always sees boss is in control.
	return tea.NewView(lipgloss.NewStyle().Padding(0, 2).Render("Attaching…"))
}
