package views

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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
// docs/plans/BOS-848-diagnosis.md):
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
// The flow goroutine sends it and blocks on ready, exactly as tuiPrompter's
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

// execDoneMsg is delivered once tea.Exec's callback fires — the child has
// exited AND the terminal has been restored to Bubble Tea.
type execDoneMsg struct{ err error }

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

// handoffProc adapts a tea.Exec-run child to accountflow's Proc contract, so
// claudeWalkthrough streams and waits exactly as it does for osProc.
type handoffProc struct {
	cmd   *exec.Cmd
	lines chan string

	// closeOut closes the write half of the output pipe once the child has
	// exited. That EOFs the relay, which closes lines, which is what lets
	// claudeWalkthrough's `for line := range proc.Lines()` terminate.
	closeOut func()

	waited  chan struct{}
	once    sync.Once
	waitErr error
}

func (p *handoffProc) Lines() <-chan string { return p.lines }

func (p *handoffProc) Wait() error {
	<-p.waited
	return p.waitErr
}

func (p *handoffProc) Kill() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// finish records the child's result and releases Wait. Idempotent: tea.Exec's
// callback fires once, but a teardown may race it.
func (p *handoffProc) finish(err error) {
	p.once.Do(func() {
		p.waitErr = err
		// Close the pipe FIRST: the relay drains to EOF and closes lines, so
		// claudeWalkthrough's range loop ends before it calls Wait.
		if p.closeOut != nil {
			p.closeOut()
		}
		close(p.waited)
	})
}

var _ accountflow.Proc = (*handoffProc)(nil)

// relayIdleFlush bounds how long a partial line (a prompt with no trailing
// newline, which is how a CLI asks for a pasted code) is held before being
// shown. Without it the operator would be staring at a blank terminal waiting
// to paste into a prompt that never appears.
const relayIdleFlush = 120 * time.Millisecond

// relayDedupeWindow bounds how many recent display lines are remembered when
// suppressing repaint noise. Large enough to span one full Ink frame, small
// enough that a line repeated much later still gets shown.
const relayDedupeWindow = 32

// maskedRelay pumps the child's output to two places at once: RAW into the
// lines channel, because agentcred.ParseClaudeSetupTokenOutput has to see the
// real token to extract it; and MASKED to the operator's terminal, which is the
// only surface where an unmasked line would persist in scrollback.
//
// Partial lines are flushed after a short idle so a prompt that ends without a
// newline still appears — EXCEPT when the pending text contains the token
// marker, which is held back until its newline arrives and it can be masked as
// a whole line. That asymmetry is deliberate: a delayed prompt is a UX wrinkle,
// a half-flushed credential is a leak.
func (p *handoffProc) maskedRelay(r io.Reader, tee io.Writer) {
	defer close(p.lines)

	type chunk struct {
		b   []byte
		err error
	}
	reads := make(chan chunk, 8)
	go func() {
		defer close(reads)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				reads <- chunk{b: cp}
			}
			if err != nil {
				reads <- chunk{err: err}
				return
			}
		}
	}()

	var pending string
	// shown de-duplicates over a WINDOW of recent display lines, not just the
	// previous one. `claude setup-token` repaints its whole frame on every
	// spinner tick and its frame is multi-line, so the output alternates
	// banner/spinner/banner/spinner — a single-slot check never matches and the
	// banner is relayed once per tick, burying the sign-in URL and the code
	// prompt. Observed in the first live run (BOS-848).
	shown := make(map[string]struct{}, relayDedupeWindow)
	order := make([]string, 0, relayDedupeWindow)

	show := func(text string, newline bool) {
		if tee == nil {
			return
		}
		display, ok := accountflow.RelayDisplayLine(text)
		if !ok {
			return
		}
		if _, dup := shown[display]; dup {
			return
		}
		shown[display] = struct{}{}
		order = append(order, display)
		// Bounded so a long walkthrough cannot grow this without limit, and so a
		// line repeated much later can still be shown again.
		if len(order) > relayDedupeWindow {
			delete(shown, order[0])
			order = order[1:]
		}
		if newline {
			display += "\n"
		}
		_, _ = io.WriteString(tee, display)
	}

	emitLine := func(line string) {
		show(line, true)
		// RAW on the channel: ParseClaudeSetupTokenOutput needs the real token.
		p.lines <- line
	}
	flushPartial := func() {
		// Never let an in-flight token fragment reach the terminal: it can only
		// be masked safely once the whole line is known.
		if pending == "" || tee == nil || strings.Contains(pending, "sk-ant-") {
			return
		}
		// No trailing newline — a partial line is a prompt awaiting input on the
		// same row (`Paste code here if prompted >`).
		show(pending, false)
		pending = ""
	}

	idle := time.NewTimer(relayIdleFlush)
	defer idle.Stop()
	for {
		select {
		case c, ok := <-reads:
			if !ok {
				if pending != "" {
					emitLine(pending)
				}
				return
			}
			if c.err != nil {
				if pending != "" {
					emitLine(pending)
					pending = ""
				}
				continue
			}
			pending += string(c.b)
			for {
				i := strings.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				emitLine(strings.TrimRight(pending[:i], "\r"))
				pending = pending[i+1:]
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(relayIdleFlush)
		case <-idle.C:
			flushPartial()
			idle.Reset(relayIdleFlush)
		}
	}
}

// newHandoffCommand builds the child for a tea.Exec handoff.
//
// The two stdio decisions here are the whole design, and both rely on
// bubbletea's nil-guards (v2.0.8 exec.go):
//
//   - Stdout/Stderr are PRE-SET to boss's pipe, so SetStdout/SetStderr leave
//     them alone and no child byte can reach the terminal unmasked.
//   - Stdin is deliberately left nil, so SetStdin gives the child the program's
//     real input. That is the point: with Bubble Tea's reader released, the
//     child is the only reader on the terminal and the paste lands cleanly.
func newHandoffCommand(ctx context.Context, req execRequest, tee io.Writer) (*exec.Cmd, *handoffProc) {
	cmd := exec.CommandContext(ctx, req.name, req.args...)
	cmd.Env = append(os.Environ(), req.env...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	// cmd.Stdin intentionally nil — see the doc comment.

	proc := &handoffProc{cmd: cmd, lines: make(chan string, 64), waited: make(chan struct{})}
	go proc.maskedRelay(pr, tee)
	// Closing the writer on exit is what EOFs the relay and closes lines.
	proc.closeOut = func() { _ = pw.Close() }
	return cmd, proc
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
func (m AccountRegisterModel) handleExecRequest(req execRequest) (tea.Model, tea.Cmd) {
	// os.Stdout is the correct relay target precisely because the terminal is
	// about to be released to the child: boss is not rendering over it, so the
	// masked child output is what the operator sees and pastes into.
	cmd, proc := newHandoffCommand(m.flowCtx, req, os.Stdout)
	req.ready <- execReady{proc: proc}

	m.state = registerStateHandoff
	return m, tea.Batch(
		tea.ExecProcess(cmd, func(err error) tea.Msg {
			// Fires after the child exits AND the terminal is restored.
			proc.finish(err)
			return execDoneMsg{err: err}
		}),
		// Re-arm so a second launch in the same flow is still bracketed.
		readExecRequestCmd(m.execBridge),
	)
}
