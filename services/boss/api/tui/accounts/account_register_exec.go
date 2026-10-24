package accounts

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/accountflow"
)

// --- the terminal handoff (BOS-848) ----------------------------------------
//
// WHY THIS EXISTS — the mechanism, confirmed rather than assumed.
//
// `claude setup-token` 2.1.233 opens the operator's CONTROLLING TERMINAL for
// reading and takes keystrokes from it, even though accountflow hands it
// os.DevNull for stdin. Observed directly on macOS 26.4.1 (see
// BOS-848):
//
//	fd 0r  CHR 3,2  /dev/null      <- the stdin NewDevNullStdinExec gave it
//	fd 7r  CHR 2,0  /dev/tty       <- the terminal it opened anyway
//
// Replacing one file descriptor does not detach a child from the terminal:
// accountflow's osExec sets no SysProcAttr, so the child keeps boss's session,
// process group and ctty, and /dev/tty resolves straight back to them. While
// the walkthrough runs there are therefore TWO readers on one terminal — the
// child and Bubble Tea's input reader — racing for every byte the operator
// types. That is the reported "pasting the OAuth code is janky and takes
// several attempts".
//
// The same run REFUTED the other two candidates: the line discipline was
// untouched throughout (so nothing restores termios because nothing breaks it),
// and the child's input-reporting DECSET writes go to its stdout — a pipe boss
// owns — while its /dev/tty handle is read-only, so it cannot strand a mode on
// the terminal either.
//
// WHAT THIS DOES.
//
// The cure for contention is for boss to stop reading the terminal while the
// child runs, which is exactly the bracket tea.Exec provides and which
// attach.go already relies on: Program.exec calls releaseTerminal() before
// Run() and RestoreTerminal() after. The child then has the terminal to itself
// (paste works), and Bubble Tea's reader is rebuilt afterwards rather than left
// to recover on its own.
//
// This is the fallback BOS-267 recorded verbatim for exactly this premise
// failure — "if a target CLI version genuinely requires an interactive TTY
// (e.g. code paste-back into the child), use tea.Exec terminal handoff for just
// the subprocess phase". The premise has now been observed to fail.
//
// CREDENTIAL SAFETY. A naive tea.Exec hands the child boss's real stdout, which
// is where `claude setup-token` prints the raw sk-ant-… value — putting a live
// credential in the operator's scrollback. That is avoided structurally:
// handleExecRequest PRE-SETS cmd.Stdout to a pipe boss owns, and bubbletea's
// osExecCommand.SetStdout assigns only "if c.Stdout == nil" (v2.0.8 exec.go),
// so the handoff cannot take it back. Everything the child prints therefore
// still flows through boss, and reaches the terminal only via maskedRelay.

// execRequest is one subprocess launch the registration flow wants performed on
// the render loop, so it can be wrapped in tea.Exec's release/restore bracket.
// The flow goroutine sends it and blocks on ready, exactly as TUIPrompter's
// ask() blocks on its one-shot reply channel.
type execRequest struct {
	name  string
	args  []string
	env   []string
	ready chan execReady
}

// execReady is the model's answer: the Proc the flow should stream, or the
// error that stopped it being created.
type execReady struct {
	proc accountflow.Proc
	err  error
}

// execRequestMsg carries a launch the model must perform via tea.Exec.
type execRequestMsg struct{ req execRequest }

// ExecDoneMsg is delivered once tea.Exec's callback fires — the child has
// exited AND the terminal has been restored to Bubble Tea.
type ExecDoneMsg struct{ Err error }

// tuiExec is the accountflow.Exec seam for the TUI claude walkthrough. It
// spawns nothing itself: it hands the launch to the render loop and waits.
type tuiExec struct {
	ctx      context.Context
	requests chan execRequest
}

func newTUIExec(ctx context.Context) *tuiExec {
	return &tuiExec{ctx: ctx, requests: make(chan execRequest)}
}

func (e *tuiExec) Start(ctx context.Context, name string, args, extraEnv []string) (accountflow.Proc, error) {
	req := execRequest{name: name, args: args, env: extraEnv, ready: make(chan execReady, 1)}
	select {
	case e.requests <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.ctx.Done():
		return nil, e.ctx.Err()
	}
	select {
	case r := <-req.ready:
		return r.proc, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.ctx.Done():
		return nil, e.ctx.Err()
	}
}

// readExecRequestCmd blocks for the next launch and yields it as an
// execRequestMsg. Mirrors readRequestCmd; retires on ctx cancel.
func readExecRequestCmd(e *tuiExec) tea.Cmd {
	return func() tea.Msg {
		if e == nil {
			return nil
		}
		select {
		case req := <-e.requests:
			return execRequestMsg{req: req}
		case <-e.ctx.Done():
			return nil
		}
	}
}

// handleExecRequest performs the walkthrough launch inside tea.Exec's
// release/restore bracket. This is the whole fix for BOS-848.
//
// Program.exec calls releaseTerminal() before Run() and RestoreTerminal()
// afterwards, so for the duration Bubble Tea's input reader is STOPPED and the
// child is the only reader on the terminal — which is what makes the OAuth code
// paste land in one attempt instead of racing boss for each byte. The restore
// then rebuilds the reader rather than leaving it to recover on its own.
//
// The Proc is handed back to the flow goroutine BEFORE tea.Exec runs, so
// claudeWalkthrough is already ranging over Lines() as the first bytes arrive.
func (m AgentAccountRegisterModel) handleExecRequest(req execRequest) (tea.Model, tea.Cmd) {
	// os.Stdout is the correct relay target precisely because the terminal is
	// about to be released to the child: boss is not rendering over it, so the
	// masked child output is what the operator sees and pastes into.
	proc := accountflow.NewHandoffProc(m.flowCtx, req.name, req.args, req.env, os.Stdout)
	req.ready <- execReady{proc: proc}

	m.state = registerStateHandoff
	return m, tea.Batch(
		tea.Exec(proc, func(err error) tea.Msg {
			// Fires after the child exits AND the terminal is restored.
			proc.Finish(err)
			return ExecDoneMsg{Err: err}
		}),
		// Re-arm so a second launch in the same flow is still bracketed.
		readExecRequestCmd(m.execBridge),
	)
}
