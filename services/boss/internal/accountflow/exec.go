// Package accountflow drives the interactive `boss account add` registration
// flows for coding-agent providers (claude, codex). It isolates subprocess
// execution, terminal prompting, and the daemon AccountService client behind
// small seams so the state machines are table-testable with fakes.
package accountflow

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
)

// scannerBufferMax bounds a single output line at 1MB (device prompts and
// setup tokens are short; this only guards against pathological CLI output).
const scannerBufferMax = 1024 * 1024

// Proc is a running child process whose merged stdout+stderr is delivered
// line-by-line.
type Proc interface {
	// Lines yields stdout+stderr lines; the channel is closed once both
	// streams reach EOF and the process has exited.
	Lines() <-chan string
	// Wait blocks for process exit and returns its error (nil on exit 0).
	// It is safe to call more than once.
	Wait() error
	// Kill terminates the process.
	Kill() error
}

// Exec launches child processes for a registration flow.
type Exec interface {
	// Start launches name with args; extraEnv entries (KEY=VAL) are appended
	// to os.Environ(). The child's stdin is connected to the user's terminal
	// so it can run its own interactive prompts.
	Start(ctx context.Context, name string, args, extraEnv []string) (Proc, error)
}

// NewOSExec returns an Exec backed by os/exec.
func NewOSExec() Exec { return osExec{} }

type osExec struct{}

func (osExec) Start(ctx context.Context, name string, args, extraEnv []string) (Proc, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = os.Stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &osProc{cmd: cmd, lines: make(chan string, 64), waited: make(chan struct{})}
	log := zerolog.Nop()
	d1 := safego.Go(log, func() { p.pump(stdout) })
	d2 := safego.Go(log, func() { p.pump(stderr) })
	safego.Go(log, func() {
		<-d1
		<-d2
		// Both pipes drained: safe to Wait (which closes the pipes) and then
		// close the merged channel so readers observe completion.
		p.waitErr = cmd.Wait()
		close(p.waited)
		close(p.lines)
	})
	return p, nil
}

type osProc struct {
	cmd     *exec.Cmd
	lines   chan string
	waited  chan struct{}
	waitErr error
}

func (p *osProc) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), scannerBufferMax)
	for sc.Scan() {
		p.lines <- sc.Text()
	}
}

func (p *osProc) Lines() <-chan string { return p.lines }

func (p *osProc) Wait() error {
	<-p.waited
	return p.waitErr
}

func (p *osProc) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

// compile-time guards.
var (
	_ Exec = osExec{}
	_ Proc = (*osProc)(nil)
)
