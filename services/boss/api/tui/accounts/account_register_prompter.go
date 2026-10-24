package accounts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/recurser/boss/api/tui/ui"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/accountflow"
)

// PromptKind identifies which accountflow.Prompter method issued a request so
// the model can render the matching input widget (free text, masked secret, or
// a y/N confirm).
type PromptKind int

const (
	PromptKindAsk PromptKind = iota
	PromptKindSecret
	PromptKindConfirm
)

// PromptRequest is one blocking question raised by the registration flow. The
// flow goroutine sends it over the request channel and then blocks on reply
// (a one-shot per-request channel) until the model pushes an answer. def is the
// default returned on an empty answer for Ask; defBool is the Confirm default.
type PromptRequest struct {
	Kind    PromptKind
	Text    string
	Def     string
	DefBool bool
	Reply   chan PromptResponse
}

// PromptResponse is the model's answer to a PromptRequest. text carries the
// typed value (Ask/AskSecret), ok carries the yes/no (Confirm), and err is set
// only when the flow is being torn down (ctx cancelled) so the flow goroutine
// unwinds instead of leaking.
type PromptResponse struct {
	Text string
	OK   bool
	Err  error
}

// --- bridge messages delivered to accountRegisterModel.Update ---

// ProgressMsg is one concise Say line surfaced by the flow.
type ProgressMsg struct{ Line string }

// PromptRequestMsg carries a blocking prompt the model must render and answer.
type PromptRequestMsg struct{ Req PromptRequest }

// FlowDoneMsg is delivered when RunClaudeAdd / RunCodexAdd returns.
type FlowDoneMsg struct{ Err error }

// TUIPrompter bridges accountflow's synchronous Prompter seam to Bubble Tea's
// non-blocking Update loop. Say lines are pushed onto a buffered progress
// channel (drained by a tea.Cmd reader); Ask/AskSecret/Confirm send a
// PromptRequest over the request channel and BLOCK on its one-shot reply
// channel. Because the flow runs on its own goroutine (never the render loop),
// blocking is safe. ctx cancellation unblocks any pending prompt with an error
// so the flow goroutine exits cleanly on teardown (no leaked goroutine).
//
// CREDENTIAL SAFETY: this bridge adds no new sink for secret material. Tokens,
// device codes, and auth.json contents are masked at the accountflow layer
// (maskLine / agentcred.MaskToken) before they ever reach Say, so the progress
// channel only ever carries already-safe strings.
type TUIPrompter struct {
	ctx      context.Context
	progress chan string
	requests chan PromptRequest

	// mu guards last, which records the most recent Say line so a caller can
	// read the flow's closing verdict without racing the render loop for it.
	//
	// The channel alone cannot serve that: the reader tea.Cmd may already hold
	// the final line as an in-flight message when FlowDoneMsg is processed, and
	// a full buffer or a cancelled ctx drops a line outright. Both make the last
	// thing the flow SAID unrecoverable from the tail it managed to render.
	mu   sync.Mutex
	last string
}

// progressBuffer bounds how many un-drained Say lines the bridge holds. The
// reader tea.Cmd re-arms immediately after each line, so this only absorbs a
// burst; a full buffer briefly blocks the flow goroutine (safe — it is not the
// render loop) until a line is drained or ctx is cancelled.
const progressBuffer = 64

func NewTUIPrompter(ctx context.Context) *TUIPrompter {
	return &TUIPrompter{
		ctx:      ctx,
		progress: make(chan string, progressBuffer),
		requests: make(chan PromptRequest),
	}
}

// forwardedSocketDialParenthesised matches a parenthesised run carrying the
// forwarded-socket dial failure. accountflow embeds an RPC failure as a reason
// inside parentheses twice — "Verification couldn't run right now (%s)" and
// keepOrRemove's "Account verification failed (%s). Keep the account anyway?" —
// so the parentheses are a boundary the substitution can trust: the sentence
// around the reason survives intact.
var forwardedSocketDialParenthesised = regexp.MustCompile(`\([^()]*dial unix[^()]*\)`)

// forwardedSocketDialRun matches the same failure where no parentheses bound
// it. It is anchored at "dial unix" and never extends leftwards, so the
// substitution can only ever consume the failure itself.
//
// Extending leftwards would be destructive on a line this change newly makes
// reachable: accountflow reports a stranded credential as `warning: could not
// remove unverified account <id>: <err>` (keepOrRemove), and that sentence
// carries no parentheses, so a leftward run would replace the whole line with
// "the connection to <destination> dropped" and leave the operator with no
// trace that an unverified account is still sitting on the remote daemon —
// strictly worse than the local path it was replacing.
//
// What precedes "dial unix" is kept as-is, which is the flow's own sentence
// plus whatever gRPC framing the error arrived wrapped in (`rpc error: code =
// Unavailable … Error while dialing`). That tail is noise, but it is noise the
// operator saw before this substitution existed; keeping it is the price of
// never guessing where the sentence ends, and the local socket path — the one
// part that invites acting on the wrong machine — is always gone.
var forwardedSocketDialRun = regexp.MustCompile(`dial unix[^()]*`)

// hostAwareFlowText applies the --host substitution to text the registration
// flow composed for the screen.
//
// Under --host the flow's AgentAccountClient is the tunnelled one, so a dropped
// tunnel fails TestAgentAccount with `dial unix /var/folders/…/bossd.sock: connect:
// no such file or directory` — a LOCAL temp path, from the machine that did not
// stop answering. accountflow then folds that string into a prompt, so the
// operator is asked "Account verification failed (dial unix /var/folders/…)
// Keep the account anyway?" and has nothing to decide on. Every other view
// reaches the same substitution through rpcErrorMessage/rpcStatusDetail; the
// flow's text is composed inside accountflow, which knows nothing about hosts,
// so the bridge is where it has to be applied.
//
// The replacement is taken from rpcStatusDetail rather than written out again,
// so this surface cannot drift from the status lines — including the
// supervisor-stopped and last-tunnel-failure variants. Replacement is literal
// (ReplaceAllLiteralString): the supervisor's classified reason is arbitrary
// text and must not be read as $-expansion.
func hostAwareFlowText(s string) string {
	if !ui.IsRemoteHost() || !strings.Contains(s, "dial unix") {
		return s
	}
	detail := ui.RPCStatusDetail(errors.New("dial unix"))
	s = forwardedSocketDialParenthesised.ReplaceAllLiteralString(s, "("+detail+")")
	if !strings.Contains(s, "dial unix") {
		return s
	}
	return forwardedSocketDialRun.ReplaceAllLiteralString(s, detail)
}

func (p *TUIPrompter) Say(format string, args ...any) {
	line := hostAwareFlowText(fmt.Sprintf(format, args...))
	// Record before the send, so the line survives a dropped delivery.
	p.mu.Lock()
	p.last = line
	p.mu.Unlock()
	select {
	case p.progress <- line:
	case <-p.ctx.Done():
	}
}

// lastSaid returns the most recent Say line, or "" when the flow said nothing.
// Read only after the flow goroutine has returned: Say happens-before the flow
// returns, which happens-before the FlowDoneMsg that consumes this.
func (p *TUIPrompter) lastSaid() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// ask sends req and blocks for the model's answer, honoring ctx cancellation so
// a teardown mid-prompt returns an error rather than deadlocking the flow.
func (p *TUIPrompter) ask(req PromptRequest) (PromptResponse, error) {
	// The prompt text is the one part of a request that reaches the screen, and
	// keepOrRemove builds it from a raw RPC error.
	req.Text = hostAwareFlowText(req.Text)
	req.Reply = make(chan PromptResponse, 1)
	select {
	case p.requests <- req:
	case <-p.ctx.Done():
		return PromptResponse{}, p.ctx.Err()
	}
	select {
	case resp := <-req.Reply:
		if resp.Err != nil {
			return PromptResponse{}, resp.Err
		}
		return resp, nil
	case <-p.ctx.Done():
		return PromptResponse{}, p.ctx.Err()
	}
}

func (p *TUIPrompter) Ask(prompt, def string) (string, error) {
	resp, err := p.ask(PromptRequest{Kind: PromptKindAsk, Text: prompt, Def: def})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func (p *TUIPrompter) AskSecret(prompt string) (string, error) {
	resp, err := p.ask(PromptRequest{Kind: PromptKindSecret, Text: prompt})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func (p *TUIPrompter) Confirm(prompt string, def bool) (bool, error) {
	resp, err := p.ask(PromptRequest{Kind: PromptKindConfirm, Text: prompt, DefBool: def})
	if err != nil {
		return false, err
	}
	return resp.OK, nil
}

// readProgressCmd blocks for the next Say line and yields it as a ProgressMsg;
// the model re-arms it after each line. Returns nil on ctx cancel or channel
// close so the reader retires cleanly.
func readProgressCmd(p *TUIPrompter) tea.Cmd {
	return func() tea.Msg {
		select {
		case line, ok := <-p.progress:
			if !ok {
				return nil
			}
			return ProgressMsg{Line: line}
		case <-p.ctx.Done():
			return nil
		}
	}
}

// readRequestCmd blocks for the next blocking prompt and yields it as a
// PromptRequestMsg; the model re-arms it only after answering the prior request
// (the flow raises at most one prompt at a time).
func readRequestCmd(p *TUIPrompter) tea.Cmd {
	return func() tea.Msg {
		select {
		case req := <-p.requests:
			return PromptRequestMsg{Req: req}
		case <-p.ctx.Done():
			return nil
		}
	}
}

// compile-time proof the bridge satisfies the accountflow seam.
var _ accountflow.Prompter = (*TUIPrompter)(nil)
