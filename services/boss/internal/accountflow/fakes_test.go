package accountflow

import (
	"context"
	"fmt"
	"strings"
	"sync"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- fakeProc -------------------------------------------------------------

// fakeProc is a scripted Proc. Lines() replays a fixed set of lines; when
// closeLines is true the channel is closed after the scripted lines (the
// normal case), otherwise it stays open to model a process that keeps
// running (timeout cases). Wait optionally blocks on waitGate (so a test can
// assert output was surfaced BEFORE Wait returns), runs waitHook (e.g. to
// write an auth.json into a scratch dir), then returns waitErr.
type fakeProc struct {
	lines    chan string
	waitGate chan struct{}
	waitHook func() error
	waitErr  error

	mu     sync.Mutex
	killed bool
}

// newScriptedProc returns a proc that emits lines then closes its channel.
func newScriptedProc(lines []string, waitErr error) *fakeProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return &fakeProc{lines: ch, waitErr: waitErr}
}

// newBlockingProc emits lines but never closes the channel and gives Wait a
// gate that must be closed for it to return — used to model a hung/abandoned
// flow that only ends on Kill/deadline.
func newBlockingProc(lines []string) *fakeProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	return &fakeProc{lines: ch, waitGate: make(chan struct{})}
}

func (p *fakeProc) Lines() <-chan string { return p.lines }

func (p *fakeProc) Wait() error {
	if p.waitGate != nil {
		<-p.waitGate
	}
	if p.waitHook != nil {
		if err := p.waitHook(); err != nil {
			return err
		}
	}
	return p.waitErr
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// --- fakeExec -------------------------------------------------------------

// fakeExec hands back a scripted proc (or an error to simulate a missing
// binary) and records how it was invoked.
type fakeExec struct {
	proc     Proc
	startErr error

	started  bool
	name     string
	args     []string
	extraEnv []string
}

func (e *fakeExec) Start(_ context.Context, name string, args, extraEnv []string) (Proc, error) {
	e.started = true
	e.name = name
	e.args = append([]string(nil), args...)
	e.extraEnv = append([]string(nil), extraEnv...)
	if e.startErr != nil {
		return nil, e.startErr
	}
	return e.proc, nil
}

// --- fakePrompter ---------------------------------------------------------

// fakePrompter replays queued answers and records everything it was asked to
// print. answers feeds both Ask and AskSecret in order; confirms feeds
// Confirm in order. When a queue is exhausted the caller-supplied default is
// returned.
type fakePrompter struct {
	answers  []string
	confirms []bool

	said  []string
	asked []string

	ai int
	ci int
}

func (p *fakePrompter) Say(format string, args ...any) {
	p.said = append(p.said, fmt.Sprintf(format, args...))
}

func (p *fakePrompter) Ask(prompt, def string) (string, error) {
	p.asked = append(p.asked, prompt)
	if p.ai >= len(p.answers) {
		return def, nil
	}
	a := p.answers[p.ai]
	p.ai++
	if a == "" {
		return def, nil
	}
	return a, nil
}

func (p *fakePrompter) AskSecret(prompt string) (string, error) {
	p.asked = append(p.asked, prompt)
	if p.ai >= len(p.answers) {
		return "", nil
	}
	a := p.answers[p.ai]
	p.ai++
	return a, nil
}

func (p *fakePrompter) Confirm(prompt string, def bool) (bool, error) {
	p.asked = append(p.asked, prompt)
	if p.ci >= len(p.confirms) {
		return def, nil
	}
	c := p.confirms[p.ci]
	p.ci++
	return c, nil
}

// transcript is the full record of everything printed and prompted; tests use
// it to assert that credential material never leaks.
func (p *fakePrompter) transcript() string {
	return strings.Join(append(append([]string{}, p.said...), p.asked...), "\n")
}

// --- fakeAccountClient ----------------------------------------------------

// fakeAccountClient scripts the daemon RPCs and records the mutations the
// flow performed.
type fakeAccountClient struct {
	listResult []*pb.Account
	listErr    error

	addResult *pb.Account
	addErr    error

	testResult *pb.TestAccountResponse
	testErr    error

	removeErr error

	listProviders []string
	addReqs       []*pb.AddAccountRequest
	removedIDs    []string
}

func (c *fakeAccountClient) ListAccounts(_ context.Context, provider string, _ bool) ([]*pb.Account, error) {
	c.listProviders = append(c.listProviders, provider)
	return c.listResult, c.listErr
}

func (c *fakeAccountClient) AddAccount(_ context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
	c.addReqs = append(c.addReqs, req)
	if c.addErr != nil {
		return nil, c.addErr
	}
	if c.addResult != nil {
		return c.addResult, nil
	}
	return &pb.Account{Id: "acc-new", Provider: req.GetProvider(), Label: req.GetLabel()}, nil
}

func (c *fakeAccountClient) TestAccount(_ context.Context, id string) (*pb.TestAccountResponse, error) {
	_ = id
	if c.testErr != nil {
		return nil, c.testErr
	}
	if c.testResult != nil {
		return c.testResult, nil
	}
	return &pb.TestAccountResponse{}, nil
}

func (c *fakeAccountClient) RemoveAccount(_ context.Context, id string) error {
	c.removedIDs = append(c.removedIDs, id)
	return c.removeErr
}

// compile-time guards that the fakes satisfy the seams.
var (
	_ Proc          = (*fakeProc)(nil)
	_ Exec          = (*fakeExec)(nil)
	_ Prompter      = (*fakePrompter)(nil)
	_ AccountClient = (*fakeAccountClient)(nil)
)
