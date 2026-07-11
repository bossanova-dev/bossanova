package views

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/accountflow"
)

// promptKind identifies which accountflow.Prompter method issued a request so
// the model can render the matching input widget (free text, masked secret, or
// a y/N confirm).
type promptKind int

const (
	promptKindAsk promptKind = iota
	promptKindSecret
	promptKindConfirm
)

// promptRequest is one blocking question raised by the registration flow. The
// flow goroutine sends it over the request channel and then blocks on reply
// (a one-shot per-request channel) until the model pushes an answer. def is the
// default returned on an empty answer for Ask; defBool is the Confirm default.
type promptRequest struct {
	kind    promptKind
	text    string
	def     string
	defBool bool
	reply   chan promptResponse
}

// promptResponse is the model's answer to a promptRequest. text carries the
// typed value (Ask/AskSecret), ok carries the yes/no (Confirm), and err is set
// only when the flow is being torn down (ctx cancelled) so the flow goroutine
// unwinds instead of leaking.
type promptResponse struct {
	text string
	ok   bool
	err  error
}

// --- bridge messages delivered to accountRegisterModel.Update ---

// progressMsg is one concise Say line surfaced by the flow.
type progressMsg struct{ line string }

// promptRequestMsg carries a blocking prompt the model must render and answer.
type promptRequestMsg struct{ req promptRequest }

// flowDoneMsg is delivered when RunClaudeAdd / RunCodexAdd returns.
type flowDoneMsg struct{ err error }

// tuiPrompter bridges accountflow's synchronous Prompter seam to Bubble Tea's
// non-blocking Update loop. Say lines are pushed onto a buffered progress
// channel (drained by a tea.Cmd reader); Ask/AskSecret/Confirm send a
// promptRequest over the request channel and BLOCK on its one-shot reply
// channel. Because the flow runs on its own goroutine (never the render loop),
// blocking is safe. ctx cancellation unblocks any pending prompt with an error
// so the flow goroutine exits cleanly on teardown (no leaked goroutine).
//
// CREDENTIAL SAFETY: this bridge adds no new sink for secret material. Tokens,
// device codes, and auth.json contents are masked at the accountflow layer
// (maskLine / agentcred.MaskToken) before they ever reach Say, so the progress
// channel only ever carries already-safe strings.
type tuiPrompter struct {
	ctx      context.Context
	progress chan string
	requests chan promptRequest
}

// progressBuffer bounds how many un-drained Say lines the bridge holds. The
// reader tea.Cmd re-arms immediately after each line, so this only absorbs a
// burst; a full buffer briefly blocks the flow goroutine (safe — it is not the
// render loop) until a line is drained or ctx is cancelled.
const progressBuffer = 64

func newTUIPrompter(ctx context.Context) *tuiPrompter {
	return &tuiPrompter{
		ctx:      ctx,
		progress: make(chan string, progressBuffer),
		requests: make(chan promptRequest),
	}
}

func (p *tuiPrompter) Say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	select {
	case p.progress <- line:
	case <-p.ctx.Done():
	}
}

// ask sends req and blocks for the model's answer, honoring ctx cancellation so
// a teardown mid-prompt returns an error rather than deadlocking the flow.
func (p *tuiPrompter) ask(req promptRequest) (promptResponse, error) {
	req.reply = make(chan promptResponse, 1)
	select {
	case p.requests <- req:
	case <-p.ctx.Done():
		return promptResponse{}, p.ctx.Err()
	}
	select {
	case resp := <-req.reply:
		if resp.err != nil {
			return promptResponse{}, resp.err
		}
		return resp, nil
	case <-p.ctx.Done():
		return promptResponse{}, p.ctx.Err()
	}
}

func (p *tuiPrompter) Ask(prompt, def string) (string, error) {
	resp, err := p.ask(promptRequest{kind: promptKindAsk, text: prompt, def: def})
	if err != nil {
		return "", err
	}
	return resp.text, nil
}

func (p *tuiPrompter) AskSecret(prompt string) (string, error) {
	resp, err := p.ask(promptRequest{kind: promptKindSecret, text: prompt})
	if err != nil {
		return "", err
	}
	return resp.text, nil
}

func (p *tuiPrompter) Confirm(prompt string, def bool) (bool, error) {
	resp, err := p.ask(promptRequest{kind: promptKindConfirm, text: prompt, defBool: def})
	if err != nil {
		return false, err
	}
	return resp.ok, nil
}

// readProgressCmd blocks for the next Say line and yields it as a progressMsg;
// the model re-arms it after each line. Returns nil on ctx cancel or channel
// close so the reader retires cleanly.
func readProgressCmd(p *tuiPrompter) tea.Cmd {
	return func() tea.Msg {
		select {
		case line, ok := <-p.progress:
			if !ok {
				return nil
			}
			return progressMsg{line: line}
		case <-p.ctx.Done():
			return nil
		}
	}
}

// readRequestCmd blocks for the next blocking prompt and yields it as a
// promptRequestMsg; the model re-arms it only after answering the prior request
// (the flow raises at most one prompt at a time).
func readRequestCmd(p *tuiPrompter) tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-p.requests:
			return promptRequestMsg{req: req}
		case <-p.ctx.Done():
			return nil
		}
	}
}

// compile-time proof the bridge satisfies the accountflow seam.
var _ accountflow.Prompter = (*tuiPrompter)(nil)
