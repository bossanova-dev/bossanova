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
	"charm.land/lipgloss/v2"
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
				key := tea.KeyPressMsg{Code: 'n', Text: "n"}
				if ans.nextConfirm() {
					key = tea.KeyPressMsg{Code: 'y', Text: "y"}
				}
				upd, _ = m.Update(key)
			} else {
				m.input.SetValue(ans.nextText())
				upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			}
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

func TestAccountRegisterConfirmArrowAndEnter(t *testing.T) {
	reply := make(chan promptResponse, 1)
	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.prompter = newTUIPrompter(context.Background())
	m.state = registerStateAwaitConfirm
	m.pending = promptRequest{
		kind:    promptKindConfirm,
		text:    "Run the walkthrough now?",
		defBool: true,
		reply:   reply,
	}

	upd, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = upd.(AccountRegisterModel)
	upd, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = upd.(AccountRegisterModel)

	select {
	case resp := <-reply:
		if resp.ok {
			t.Fatal("right then enter answered yes, want No")
		}
	default:
		t.Fatal("right then enter did not answer the confirm")
	}
	if m.state != registerStateProgress {
		t.Fatalf("after button confirm, state=%d want progress", m.state)
	}
}

func TestAccountRegisterConfirmDefaultFocus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		defBool bool
		want    int
	}{
		{name: "yes", defBool: true, want: 0},
		{name: "no", defBool: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
			model, _ := m.handlePromptRequest(promptRequest{kind: promptKindConfirm, defBool: tc.defBool})
			m = model.(AccountRegisterModel)
			if m.confirmCursor != tc.want {
				t.Fatalf("default %v focused button %d, want %d", tc.defBool, m.confirmCursor, tc.want)
			}
		})
	}
}

func TestAccountRegisterConfirmViewRendersButtonsWithoutTextInput(t *testing.T) {
	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.provider = "claude"
	m.state = registerStateAwaitConfirm
	m.pending = promptRequest{kind: promptKindConfirm, text: "Run the walkthrough now?", defBool: true}

	view := m.View().Content
	for _, label := range []string{"Yes", "No", "Run the walkthrough now?"} {
		if !strings.Contains(view, label) {
			t.Fatalf("confirm view missing %q:\n%s", label, view)
		}
	}
	if strings.Contains(view, ">") {
		t.Fatalf("confirm view still renders text input cursor:\n%s", view)
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

// --- --host registration (BOS-847) --------------------------------------------
//
// `boss --host <dest>` forwards only the daemon socket: every subprocess and
// every scratch directory still belongs to THIS machine. The registration flows
// resolve `claude` / `codex` against this process's PATH, so under --host they
// were running (or failing to run) the wrong machine's agent CLI while the
// resulting credential went to the remote daemon.
//
// These tests must not run in parallel: hostDestination is a package global.

// TestAccountRegisterCodexUnderHostRefusesBeforeSpawning pins that the codex
// flow never reaches Exec.Start under --host. The assertion is on the recorded
// binary NAME rather than on the options passed, because "no local spawn" is
// the actual claim — an assertion about arguments would still pass if the
// process were launched.
func TestAccountRegisterCodexUnderHostRefusesBeforeSpawning(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")

	client := &regAcctClient{}
	ex := &regExec{proc: newRegScriptedProc([]string{"should never be read"}, nil)}
	m := newRegisterModel(t, client, ex)
	m.homeDir = func() (string, error) { return t.TempDir(), nil }
	m = selectProvider(t, m, "codex")

	m, _ = drivePump(t, m, &regAnswers{})

	if got := ex.launchedName(); got != "" {
		t.Fatalf("codex was spawned locally under --host (launched %q); the refusal must "+
			"come before Exec.Start", got)
	}
	if m.state != registerStateError || m.err == nil {
		t.Fatalf("expected the refusal to land on the error screen; state=%d err=%v", m.state, m.err)
	}
	if !strings.Contains(m.err.Error(), "deploy@build-box.invalid") {
		t.Fatalf("refusal %q must name the destination it is talking about", m.err.Error())
	}
	// Esc pops back to the accounts list, so the remedy has to travel with the
	// message rather than be something the operator is expected to work out.
	if !strings.Contains(m.err.Error(), "boss account add codex") {
		t.Fatalf("refusal %q must carry the command that does work", m.err.Error())
	}
	if client.addCount() != 0 {
		t.Fatalf("a refused registration must store nothing; adds=%d", client.addCount())
	}

	// The flow goroutine returns on the refusal path too.
	select {
	case <-m.flowDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flow goroutine leaked after a policy refusal")
	}
}

// TestAccountRegisterCodexRefusalRendersOnTheErrorScreen is the rendered half:
// m.err is only useful if it reaches the screen through rpcErrorMessage, which
// is host-aware and could in principle swallow it.
//
// The refusal is raised INSIDE the flow goroutine, before any Exec.Start, so it
// arrives as a flowDoneMsg with the flow's trailing Say lines still sitting in
// the prompter's buffered progress channel. Those lines are therefore staged
// here rather than left empty: handleFlowDone drains before it cancels
// (BOS-267), and an empty channel would exercise drainProgress over zero lines
// and prove nothing about the buffered case the acceptance criteria name.
func TestAccountRegisterCodexRefusalRendersOnTheErrorScreen(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")

	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.prompter = newTUIPrompter(context.Background())
	m.provider = "codex"
	m.state = registerStateProgress

	// Buffered before the flow-done message, exactly as a flow that Say'd and
	// then returned leaves them.
	buffered := []string{
		"Checking the codex CLI on this machine…",
		"Registration target is deploy@build-box.invalid.",
	}
	for _, line := range buffered {
		m.prompter.progress <- line
	}

	upd, _ := m.Update(flowDoneMsg{err: codexRemoteRefusal("deploy@build-box.invalid")})
	m = upd.(AccountRegisterModel)

	content := stripANSI(m.View().Content)
	for _, want := range []string{"deploy@build-box.invalid", "boss account add codex"} {
		if !strings.Contains(content, want) {
			t.Fatalf("error screen missing %q:\n%s", want, content)
		}
	}
	// The buffered lines survived the drain-before-cancel ordering rather than
	// being dropped when the flow context was released.
	for _, want := range buffered {
		if !strings.Contains(flattenPrompt(content), flattenPrompt(want)) {
			t.Fatalf("progress line %q buffered before flowDoneMsg was dropped:\n%s", want, content)
		}
	}
}

// TestAccountRegisterHostRefusalIsNotAFailedAdd guards the telemetry meaning: a
// policy refusal never dialled, never spawned, and never touched a credential,
// so counting it as a failed account-add would report a failure rate for an
// operation that was declined before it started.
func TestAccountRegisterHostRefusalIsNotAFailedAdd(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")
	enableViewTelemetryForTest(t)

	rec := &fakeTelemetry{}
	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.SetTelemetry(rec)
	m.prompter = newTUIPrompter(context.Background())
	m.provider = "codex"
	m.state = registerStateProgress

	upd, _ := m.Update(flowDoneMsg{err: codexRemoteRefusal("deploy@build-box.invalid")})
	if upd.(AccountRegisterModel).state != registerStateError {
		t.Fatal("precondition: the refusal should land on the error screen")
	}
	if len(rec.events) != 0 {
		t.Fatalf("a refusal emitted %v; nothing was attempted, so nothing may be recorded", rec.events)
	}

	// ...and an ordinary failure still is, or the guard would have silenced the
	// signal entirely.
	m2 := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m2.SetTelemetry(rec)
	m2.prompter = newTUIPrompter(context.Background())
	m2.provider = "codex"
	m2.state = registerStateProgress
	_, _ = m2.Update(flowDoneMsg{err: errors.New("codex device-code login disabled")})
	if len(rec.events) != 1 {
		t.Fatalf("events = %v, want a real failure to still be recorded", rec.events)
	}
}

// TestAccountRegisterClaudeUnderHostPastesInstead is the routed half of the
// decision: claude has a token shape that needs no local subprocess, so the
// --host flow drops to it rather than refusing.
func TestAccountRegisterClaudeUnderHostPastesInstead(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")

	client := &regAcctClient{}
	ex := &regExec{proc: newRegScriptedProc([]string{"setup token: " + claudeFakeToken}, nil)}
	m := newRegisterModel(t, client, ex)
	m = selectProvider(t, m, "claude")

	// Paste the token, then accept the default label.
	m, transcript := drivePump(t, m, &regAnswers{texts: []string{claudeFakeToken, ""}})

	if got := ex.launchedName(); got != "" {
		t.Fatalf("claude was spawned locally under --host (launched %q)", got)
	}
	if !m.Done() {
		t.Fatalf("host paste flow should complete; state=%d err=%v", m.state, m.err)
	}

	joined := strings.Join(transcript, "\n")
	if !strings.Contains(joined, "Paste your Claude setup token") {
		t.Fatalf("paste prompt missing:\n%s", joined)
	}
	// StdinUnavailable stays unset, so the label question is still asked — the
	// bug this ticket fixes was PasteMode silently suppressing it.
	if !strings.Contains(joined, "Label for this account") {
		t.Fatalf("label prompt missing; PasteMode must not suppress it:\n%s", joined)
	}
	// The explanatory line has to answer both questions: why the walkthrough the
	// operator expected is gone, and where a token comes from.
	if !strings.Contains(joined, "deploy@build-box.invalid") {
		t.Fatalf("notice must name the destination:\n%s", joined)
	}
	if !strings.Contains(joined, "claude setup-token") {
		t.Fatalf("notice must say how to obtain a token:\n%s", joined)
	}
	// No walkthrough confirm is offered, because answering yes could not work.
	if strings.Contains(joined, "Run the claude setup-token walkthrough now?") {
		t.Fatalf("host flow must not offer a walkthrough it cannot run:\n%s", joined)
	}

	if client.addCount() != 1 {
		t.Fatalf("AddAccount calls = %d, want 1", client.addCount())
	}
	if got := string(client.addReqs[0].GetCredential()); got != claudeFakeToken {
		t.Fatalf("stored credential = %q, want the pasted token", got)
	}
	if strings.Contains(joined, claudeFakeToken) {
		t.Fatalf("raw token leaked into the transcript:\n%s", joined)
	}
}

// TestAccountRegisterClaudeUnderHostRepromptsOnce pins that routing to the paste
// branch did not change the paste branch: one re-prompt, then failure.
func TestAccountRegisterClaudeUnderHostRepromptsOnce(t *testing.T) {
	withHostDestination(t, "deploy@build-box.invalid")

	client := &regAcctClient{}
	ex := &regExec{}
	m := newRegisterModel(t, client, ex)
	m = selectProvider(t, m, "claude")

	m, transcript := drivePump(t, m, &regAnswers{texts: []string{"nope", "still-bad"}})

	if m.state != registerStateError {
		t.Fatalf("two invalid tokens must fail; state=%d", m.state)
	}
	if client.addCount() != 0 {
		t.Fatalf("an invalid token must not be stored; adds=%d", client.addCount())
	}
	joined := strings.Join(transcript, "\n")
	if strings.Count(joined, "Paste your Claude setup token") != 2 {
		t.Fatalf("want exactly one re-prompt:\n%s", joined)
	}
	if got := ex.launchedName(); got != "" {
		t.Fatalf("claude was spawned locally under --host (launched %q)", got)
	}
}

// TestAccountRegisterLocalFlowsAreUnchangedByTheHostSwitch is the negative
// control for the snapshot in the constructor: with no --host destination the
// walkthrough confirm is still offered and the CLI is still spawned.
func TestAccountRegisterLocalFlowsAreUnchangedByTheHostSwitch(t *testing.T) {
	withHostDestination(t, "")

	client := &regAcctClient{}
	ex := &regExec{proc: newRegScriptedProc([]string{"setup token: " + claudeFakeToken}, nil)}
	m := newRegisterModel(t, client, ex)
	if m.remoteHost != "" {
		t.Fatalf("remoteHost = %q, want empty for a local session", m.remoteHost)
	}
	m = selectProvider(t, m, "claude")

	m, transcript := drivePump(t, m, &regAnswers{confirms: []bool{true}, texts: []string{""}})

	if !m.Done() {
		t.Fatalf("local claude flow should complete; state=%d err=%v", m.state, m.err)
	}
	if ex.launchedName() != "claude" {
		t.Fatalf("local flow must still spawn claude; launched %q", ex.launchedName())
	}
	if !strings.Contains(strings.Join(transcript, "\n"), "Run the claude setup-token walkthrough now?") {
		t.Fatalf("local flow must still offer the walkthrough:\n%s", strings.Join(transcript, "\n"))
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

// TestAccountRegisterHostNoticeWrapsIntoTheFrame is the rendered half of plan
// acceptance criterion 5. claudeRemotePasteNotice is one ~326-character Say
// line whose SECOND clause carries the remedy; progressView used to render it
// with padding but no Width, so bubbletea clipped the frame at the terminal
// edge and an operator at a realistic 80 columns saw only "the walkthrough is
// unavailable" — never `claude setup-token`, never "paste the token below".
//
// The assertion is deliberately on the RENDERED frame rather than on the model
// state or the transcript: state carried the whole sentence throughout, so only
// the frame can tell a clipped notice from a wrapped one. Whitespace is
// flattened because a wrapped sentence is broken across lines by design.
func TestAccountRegisterHostNoticeWrapsIntoTheFrame(t *testing.T) {
	const width = 80
	const dest = "deploy@build-box.invalid"

	m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
	m.prompter = newTUIPrompter(context.Background())
	m.provider = "claude"
	m.remoteHost = dest
	m.width = width
	m.state = registerStateAwaitText
	m.pending = promptRequest{kind: promptKindAsk, text: "Paste your Claude setup token"}
	m.appendProgress(claudeRemotePasteNotice(dest))

	frame := stripANSI(m.View().Content)
	flat := flattenPrompt(frame)

	// Both halves of the notice: why the walkthrough is gone, AND how to obtain
	// a token. The remedy substrings are the ones clipping used to eat.
	for _, want := range []string{
		"so it is unavailable in a --host session",
		"Run `claude setup-token` in a shell on " + dest,
		"paste the token below",
		"it will be stored on " + dest,
	} {
		if !strings.Contains(flat, flattenPrompt(want)) {
			t.Fatalf("rendered frame at %d columns is missing %q:\n%s", width, want, frame)
		}
	}

	// ...and it is genuinely wrapped, not merely present in an over-wide frame
	// that the terminal would clip anyway.
	for _, line := range strings.Split(frame, "\n") {
		if got := lipgloss.Width(line); got > width {
			t.Fatalf("rendered line is %d columns wide, over the %d-column terminal: %q", got, width, line)
		}
	}
}
