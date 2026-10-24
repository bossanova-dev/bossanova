package accountflow

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const handoffWaitDelay = 5 * time.Second

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

var _ Proc = (*handoffProc)(nil)

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
		display, ok := RelayDisplayLine(text)
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
func newHandoffCommand(ctx context.Context, name string, args, env []string, tee io.Writer) (*exec.Cmd, *handoffProc) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)

	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.WaitDelay = handoffWaitDelay
	// cmd.Stdin intentionally nil — see the doc comment.

	proc := &handoffProc{cmd: cmd, lines: make(chan string, 64), waited: make(chan struct{})}
	go proc.maskedRelay(pr, tee)
	// Closing the writer on exit is what EOFs the relay and closes lines.
	proc.closeOut = func() { _ = pw.Close() }
	return cmd, proc
}

// HandoffProc combines the flow process with the terminal handoff command.
// Run starts the prepared process only after Bubble Tea releases the terminal.
type HandoffProc interface {
	Proc
	Run() error
	SetStdin(io.Reader)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
	Finish(error)
}

// NewHandoffProc prepares a masked terminal process without starting it.
func NewHandoffProc(ctx context.Context, name string, args, env []string, tee io.Writer) HandoffProc {
	_, proc := newHandoffCommand(ctx, name, args, env, tee)
	return proc
}
func (p *handoffProc) Run() error { return p.cmd.Run() }
func (p *handoffProc) SetStdin(r io.Reader) {
	if p.cmd.Stdin == nil {
		p.cmd.Stdin = r
	}
}

// Outputs are fixed to the masked relay when the process is prepared. Never
// replace them with the terminal streams offered by Bubble Tea.
func (p *handoffProc) SetStdout(io.Writer) {}
func (p *handoffProc) SetStderr(io.Writer) {}
func (p *handoffProc) Finish(err error)    { p.finish(err) }
