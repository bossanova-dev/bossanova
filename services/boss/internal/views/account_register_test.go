package views

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/accountflow"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- fakes (mirroring accountflow/fakes_test.go, which is test-only and cannot
// be imported across packages) ------------------------------------------------

type regProc struct {
	lines    chan string
	waitHook func() error

	gate chan struct{} // non-nil = Wait blocks until Kill (models a hung flow)

	mu     sync.Mutex
	killed bool
}

func newRegScriptedProc(lines []string, hook func() error) *regProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	close(ch)
	return &regProc{lines: ch, waitHook: hook}
}

func newRegBlockingProc(lines []string) *regProc {
	ch := make(chan string, len(lines)+1)
	for _, l := range lines {
		ch <- l
	}
	return &regProc{lines: ch, gate: make(chan struct{})}
}

func (p *regProc) Lines() <-chan string { return p.lines }

func (p *regProc) Wait() error {
	if p.gate != nil {
		<-p.gate
	}
	if p.waitHook != nil {
		if err := p.waitHook(); err != nil {
			return err
		}
	}
	return nil
}

func (p *regProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killed = true
	if p.gate != nil {
		select {
		case <-p.gate:
		default:
			close(p.gate)
		}
	}
	return nil
}

func (p *regProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

type regExec struct {
	proc     accountflow.Proc
	startErr error

	mu   sync.Mutex
	name string
}

func (e *regExec) Start(_ context.Context, name string, _, _ []string) (accountflow.Proc, error) {
	e.mu.Lock()
	e.name = name
	e.mu.Unlock()
	if e.startErr != nil {
		return nil, e.startErr
	}
	return e.proc, nil
}

// launchedName returns the binary Start was invoked with (guarded because Start
// runs on the flow goroutine).
func (e *regExec) launchedName() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.name
}

type regAcctClient struct {
	listResult []*pb.Account
	listErr    error
	addResult  *pb.Account
	addErr     error
	testResult *pb.TestAccountResponse
	testErr    error
	removeErr  error

	mu         sync.Mutex
	addReqs    []*pb.AddAccountRequest
	removedIDs []string
}

func (c *regAcctClient) ListAccounts(_ context.Context, _ string, _ bool) ([]*pb.Account, error) {
	return c.listResult, c.listErr
}

func (c *regAcctClient) AddAccount(_ context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
	c.mu.Lock()
	c.addReqs = append(c.addReqs, req)
	c.mu.Unlock()
	if c.addErr != nil {
		return nil, c.addErr
	}
	if c.addResult != nil {
		return c.addResult, nil
	}
	return &pb.Account{Id: "acc-new", Provider: req.GetProvider(), Label: req.GetLabel()}, nil
}

func (c *regAcctClient) TestAccount(_ context.Context, _ string) (*pb.TestAccountResponse, error) {
	if c.testErr != nil {
		return nil, c.testErr
	}
	if c.testResult != nil {
		return c.testResult, nil
	}
	return &pb.TestAccountResponse{}, nil
}

func (c *regAcctClient) RemoveAccount(_ context.Context, id string) error {
	c.mu.Lock()
	c.removedIDs = append(c.removedIDs, id)
	c.mu.Unlock()
	return c.removeErr
}

func (c *regAcctClient) addCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.addReqs)
}

func (c *regAcctClient) removedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.removedIDs)
}

var _ accountflow.Exec = (*regExec)(nil)
var _ accountflow.Proc = (*regProc)(nil)
var _ accountflow.AccountClient = (*regAcctClient)(nil)

// --- pump harness -------------------------------------------------------------

type regAnswers struct {
	confirms []bool
	texts    []string
	ci, ti   int
}

func (a *regAnswers) nextConfirm() bool {
	if a.ci >= len(a.confirms) {
		return false
	}
	v := a.confirms[a.ci]
	a.ci++
	return v
}

func (a *regAnswers) nextText() string {
	if a.ti >= len(a.texts) {
		return ""
	}
	v := a.texts[a.ti]
	a.ti++
	return v
}

// drivePump mirrors what the runtime's reader Cmds do: it drains the bridge
// channels, feeds each event through Update, answers each blocking prompt from
// the scripted answers, and returns once the flow finishes. transcript records
// every Say line and prompt text so tests can assert on the full user-visible
// record (and that no secret ever appears in it).
func drivePump(t *testing.T, m AccountRegisterModel, ans *regAnswers) (AccountRegisterModel, []string) {
	t.Helper()
	var transcript []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("flow did not complete; transcript so far:\n%s", strings.Join(transcript, "\n"))
		case line := <-m.prompter.progress:
			transcript = append(transcript, line)
			upd, _ := m.Update(progressMsg{line: line})
			m = upd.(AccountRegisterModel)
		case req := <-m.prompter.requests:
			transcript = append(transcript, req.text)
			upd, _ := m.Update(promptRequestMsg{req: req})
			m = upd.(AccountRegisterModel)
			if req.kind == promptKindConfirm {
				v := "n"
				if ans.nextConfirm() {
					v = "y"
				}
				m.input.SetValue(v)
			} else {
				m.input.SetValue(ans.nextText())
			}
			upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = upd.(AccountRegisterModel)
		case err := <-m.donec:
			// Drain any buffered progress emitted before the flow returned so the
			// final milestone (e.g. "registered and verified") is captured.
			for {
				select {
				case line := <-m.prompter.progress:
					transcript = append(transcript, line)
					upd, _ := m.Update(progressMsg{line: line})
					m = upd.(AccountRegisterModel)
				default:
					upd, _ := m.Update(flowDoneMsg{err: err})
					return upd.(AccountRegisterModel), transcript
				}
			}
		}
	}
}

// selectProvider drives the provider chooser to the named provider and launches
// the flow, returning the running model.
func selectProvider(t *testing.T, m AccountRegisterModel, provider string) AccountRegisterModel {
	t.Helper()
	for i, p := range registerProviders {
		if p == provider {
			for j := 0; j < i; j++ {
				upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				m = upd.(AccountRegisterModel)
			}
			upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return upd.(AccountRegisterModel)
		}
	}
	t.Fatalf("unknown provider %q", provider)
	return m
}

func newRegisterModel(t *testing.T, client accountflow.AccountClient, ex accountflow.Exec) AccountRegisterModel {
	t.Helper()
	m := NewAccountRegisterModel(client, context.Background())
	m.exec = ex
	// Deterministic temp scratch/home so subprocess-driven flows don't touch the
	// real filesystem defaults.
	m.scratchDir = func() (string, error) { return t.TempDir(), nil }
	return m
}

// --- tests --------------------------------------------------------------------

const claudeFakeToken = "sk-ant-api03-CLAUDEFAKETOKEN0123456789abcd"

func TestAccountRegisterClaudeFlow(t *testing.T) {
	client := &regAcctClient{} // empty testResult → verified
	ex := &regExec{proc: newRegScriptedProc(
		[]string{"Opening browser to sign in…", "setup token: " + claudeFakeToken},
		nil,
	)}
	m := newRegisterModel(t, client, ex)
	m = selectProvider(t, m, "claude")

	// Confirm(walkthrough)=yes, Ask(label)=default.
	m, transcript := drivePump(t, m, &regAnswers{confirms: []bool{true}, texts: []string{""}})

	if !m.Done() {
		t.Fatalf("claude flow should have completed; state=%d err=%v", m.state, m.err)
	}
	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "Label for this account") {
		t.Fatalf("literal label prompt missing:\n%s", joined)
	}
	if !strings.Contains(joined, "registered and verified") {
		t.Fatalf("missing 'registered and verified' milestone:\n%s", joined)
	}
	// CREDENTIAL SAFETY: the raw token must never appear; only the masked form.
	if strings.Contains(joined, claudeFakeToken) {
		t.Fatalf("raw setup token leaked into transcript:\n%s", joined)
	}
	if !strings.Contains(joined, "sk-ant-…") {
		t.Fatalf("masked token form missing from transcript:\n%s", joined)
	}
	if client.addCount() != 1 {
		t.Fatalf("AddAccount calls = %d, want 1", client.addCount())
	}
	if ex.launchedName() != "claude" {
		t.Fatalf("exec launched %q, want claude", ex.launchedName())
	}
}

func codexAuthJSON() []byte {
	return []byte(`{"tokens":{"access_token":"acc","refresh_token":"ref","id_token":"idt"}}`)
}

func TestAccountRegisterCodexFlow(t *testing.T) {
	dir := t.TempDir()
	client := &regAcctClient{}
	hook := func() error { return os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(), 0o600) }
	ex := &regExec{proc: newRegScriptedProc(
		[]string{"Open https://auth.openai.com/codex/device and enter code ABCD-EFGH1"},
		hook,
	)}
	m := newRegisterModel(t, client, ex)
	m.homeDir = func() (string, error) { return dir, nil }
	m = selectProvider(t, m, "codex")

	// Codex has no walkthrough confirm; the only prompt is the label (default).
	m, transcript := drivePump(t, m, &regAnswers{texts: []string{""}})

	if !m.Done() {
		t.Fatalf("codex flow should have completed; state=%d err=%v", m.state, m.err)
	}
	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "https://auth.openai.com/codex/device") {
		t.Fatalf("device-auth URL not surfaced:\n%s", joined)
	}
	if !strings.Contains(joined, "ABCD-EFGH1") {
		t.Fatalf("device-auth code not surfaced:\n%s", joined)
	}
	if !strings.Contains(joined, "registered and verified") {
		t.Fatalf("missing verified milestone:\n%s", joined)
	}
	if client.addCount() != 1 {
		t.Fatalf("AddAccount calls = %d, want 1", client.addCount())
	}
}

func TestAccountRegisterCodexDeviceAuthDisabled(t *testing.T) {
	dir := t.TempDir()
	client := &regAcctClient{}
	ex := &regExec{proc: newRegScriptedProc(
		[]string{"error: device code login is not enabled for this account"},
		nil,
	)}
	m := newRegisterModel(t, client, ex)
	m.homeDir = func() (string, error) { return dir, nil }
	m = selectProvider(t, m, "codex")

	// No prompts: the flow returns the disabled error before promptIdentity.
	m, transcript := drivePump(t, m, &regAnswers{})

	if m.state != registerStateError || m.err == nil {
		t.Fatalf("expected error state; state=%d err=%v", m.state, m.err)
	}
	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "Security") {
		t.Fatalf("remediation missing ChatGPT Security hint:\n%s", joined)
	}
	if !strings.Contains(strings.ToLower(joined), "device code authorization") {
		t.Fatalf("remediation missing device-code-authorization hint:\n%s", joined)
	}
	// The account must NOT be stored on a disabled-device-auth failure.
	if client.addCount() != 0 {
		t.Fatalf("AddAccount must not be called on disabled device auth; got %d", client.addCount())
	}
	// The failure screen must not falsely claim verification.
	if strings.Contains(joined, "registered and verified") {
		t.Fatalf("disabled-device-auth flow must not show a verified milestone:\n%s", joined)
	}
}

// TestAccountRegisterFlowDoneDrainsTrailingProgress is the regression guard for
// the completion-boundary race: handleFlowDone must drain buffered Say lines
// BEFORE cancelling the flow ctx. drivePump masks the bug because it drains on
// <-donec; this test instead delivers flowDoneMsg with the remediation lines
// still buffered (as the runtime would, where readProgressCmd races ctx.Done()),
// and asserts they still reach the rendered error screen.
func TestAccountRegisterFlowDoneDrainsTrailingProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newTUIPrompter(ctx)

	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.prompter = p
	m.cancel = cancel
	m.provider = "codex"
	m.state = registerStateProgress

	// Buffer the trailing remediation lines exactly as the flow goroutine emits
	// them immediately before returning, then hand the model flowDoneMsg with them
	// still undrained.
	p.progress <- "Enable it, then re-run this command:"
	p.progress <- "  2. Turn on \"Enable device code authorization for Codex\"."

	upd, _ := m.Update(flowDoneMsg{err: errors.New("codex device-code login disabled")})
	m = upd.(AccountRegisterModel)

	if m.state != registerStateError {
		t.Fatalf("expected error state; got state=%d", m.state)
	}
	content := m.View().Content
	if !strings.Contains(content, "device code authorization for Codex") {
		t.Fatalf("trailing remediation dropped from error screen:\n%s", content)
	}
}

// TestAccountRegisterConfirmSingleKey covers the single-keypress y/n confirm
// affordance: y/Y answer yes and n/N answer no without typing + enter.
func TestAccountRegisterConfirmSingleKey(t *testing.T) {
	cases := []struct {
		code rune
		text string
		want bool
	}{
		{'y', "y", true},
		{'Y', "Y", true},
		{'n', "n", false},
		{'N', "N", false},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			reply := make(chan promptResponse, 1)
			m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
			m.prompter = newTUIPrompter(context.Background())
			m.state = registerStateAwaitConfirm
			m.pending = promptRequest{kind: promptKindConfirm, text: "Keep the account anyway?", reply: reply}

			upd, _ := m.Update(tea.KeyPressMsg{Code: tc.code, Text: tc.text})
			m = upd.(AccountRegisterModel)

			select {
			case resp := <-reply:
				if resp.ok != tc.want {
					t.Fatalf("key %q -> ok=%v, want %v", tc.text, resp.ok, tc.want)
				}
			default:
				t.Fatalf("key %q did not answer the confirm", tc.text)
			}
			if m.state != registerStateProgress {
				t.Fatalf("after single-key confirm, state=%d want progress", m.state)
			}
		})
	}
}

func TestAccountRegisterFailedLiveSmoke(t *testing.T) {
	cases := []struct {
		name   string
		client *regAcctClient
	}{
		{
			name: "non-empty last_test_error",
			client: &regAcctClient{
				addResult: &pb.Account{Id: "acc-1", Provider: "claude", Label: "claude-1"},
				testResult: &pb.TestAccountResponse{
					Account: &pb.Account{Id: "acc-1", LastTestError: "auth failed: invalid credential"},
				},
			},
		},
		{
			name: "transport testErr",
			client: &regAcctClient{
				addResult: &pb.Account{Id: "acc-1", Provider: "claude", Label: "claude-1"},
				testErr:   errors.New("daemon unreachable"),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &regExec{proc: newRegScriptedProc(
				[]string{"setup token: " + claudeFakeToken}, nil,
			)}
			m := newRegisterModel(t, tc.client, ex)
			m = selectProvider(t, m, "claude")

			// walkthrough=yes, label=default, keep-on-failure=no (decline → remove).
			m, transcript := drivePump(t, m, &regAnswers{confirms: []bool{true, false}, texts: []string{""}})

			joined := strings.Join(transcript, "\n")
			if strings.Contains(joined, "registered and verified") {
				t.Fatalf("failed smoke must never render a verified milestone:\n%s", joined)
			}
			if !strings.Contains(strings.ToLower(joined), "verification failed") {
				t.Fatalf("failed smoke should surface the keep-or-remove prompt:\n%s", joined)
			}
			if m.state != registerStateError {
				t.Fatalf("declined keep must end in error state; state=%d", m.state)
			}
			if tc.client.removedCount() != 1 {
				t.Fatalf("declining keep must remove the account; removes=%d", tc.client.removedCount())
			}
		})
	}
}

func TestAccountRegisterCancelMidFlow(t *testing.T) {
	dir := t.TempDir()
	client := &regAcctClient{}
	proc := newRegBlockingProc([]string{"Open https://auth.openai.com/codex/device and enter code ABCD-EFGH1"})
	ex := &regExec{proc: proc}
	m := newRegisterModel(t, client, ex)
	m.homeDir = func() (string, error) { return dir, nil }
	m = selectProvider(t, m, "codex")

	// Drain the surfaced device line (best-effort) so the flow is mid-poll.
	select {
	case <-m.prompter.progress:
	case <-time.After(2 * time.Second):
		t.Fatal("device prompt was never surfaced")
	}

	// Esc tears the flow down.
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = upd.(AccountRegisterModel)
	if !m.Cancelled() {
		t.Fatal("esc must cancel the flow")
	}

	// The flow goroutine must exit (no leak) and the subprocess must be killed.
	select {
	case <-m.flowDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flow goroutine leaked after cancel")
	}
	if !proc.wasKilled() {
		t.Fatal("subprocess not killed on cancel")
	}
	if client.addCount() != 0 {
		t.Fatalf("cancelled flow must not store an account; got %d", client.addCount())
	}
}

func TestAccountRegisterProviderChooserEscCancels(t *testing.T) {
	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	if m.Cancelled() {
		t.Fatal("precondition: not cancelled")
	}
	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !upd.(AccountRegisterModel).Cancelled() {
		t.Fatal("esc on the provider chooser should cancel")
	}
}

func TestAccountRegisterProviderChooserView(t *testing.T) {
	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	got := m.View().Content
	for _, want := range []string{"Add an account", "claude", "codex"} {
		if !strings.Contains(got, want) {
			t.Fatalf("provider chooser missing %q:\n%s", want, got)
		}
	}
}
