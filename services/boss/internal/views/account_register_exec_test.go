package views

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/agentcred"
)

// syncBuf is a mutex-guarded relay target, since maskedRelay writes from its
// own goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHandoffCommandStdioContract pins the two decisions that make the BOS-848
// fix work at all. Both depend on bubbletea's nil-guards in
// osExecCommand.SetStdin/SetStdout (v2.0.8 exec.go), so a well-meaning tidy-up
// that "initialises" either field would silently undo the fix:
//
//   - Stdout/Stderr PRE-SET  => tea.Exec leaves them alone => every child byte
//     stays inside boss and can only reach the terminal through maskedRelay.
//     If this regresses, the raw sk-ant-… lands in the operator's scrollback.
//   - Stdin left nil         => tea.Exec hands the child the real terminal.
//     That is the point of the handoff: with Bubble Tea's reader released the
//     child is the ONLY reader, so the OAuth paste stops racing boss.
func TestHandoffCommandStdioContract(t *testing.T) {
	req := execRequest{name: "claude", args: []string{"setup-token"}, env: []string{"CLAUDE_CONFIG_DIR=/tmp/x"}}
	cmd, proc := newHandoffCommand(context.Background(), req, &syncBuf{})
	t.Cleanup(func() { proc.finish(nil) })

	if cmd.Stdin != nil {
		t.Error("cmd.Stdin must stay nil so tea.Exec hands the child the real terminal; " +
			"setting it here re-creates the reader contention BOS-848 fixed")
	}
	if cmd.Stdout == nil || cmd.Stderr == nil {
		t.Fatal("cmd.Stdout/Stderr must be PRE-SET so tea.Exec cannot point them at the " +
			"terminal; unset means the raw setup token reaches the operator's scrollback")
	}
	if cmd.Stdout != cmd.Stderr {
		t.Error("stdout and stderr should share the relay so interleaved output keeps its order")
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != "setup-token" {
		t.Errorf("args not forwarded: %v", cmd.Args)
	}
	if !containsEnv(cmd.Env, "CLAUDE_CONFIG_DIR=/tmp/x") {
		t.Error("extraEnv not appended to the child environment")
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestMaskedRelaySplitsRawFromDisplayed is the credential-safety guard on the
// one path that writes child output straight to the operator's terminal.
//
// The two sinks must disagree on purpose: agentcred.ParseClaudeSetupTokenOutput
// needs the RAW token off Lines() to extract it, while the terminal must only
// ever see the masked form.
func TestMaskedRelaySplitsRawFromDisplayed(t *testing.T) {
	tok := "sk-ant-oat01-" + strings.Repeat("a", 40)
	pr, pw := newPipePair()
	proc := &handoffProc{lines: make(chan string, 16), waited: make(chan struct{})}
	tee := &syncBuf{}
	go proc.maskedRelay(pr, tee)

	_, _ = pw.Write([]byte("Welcome to Claude Code\n" + tok + "\ndone\n"))
	_ = pw.Close()

	var got []string
	for line := range proc.Lines() {
		got = append(got, line)
	}
	joined := strings.Join(got, "\n")

	// Raw on the channel: the parser depends on it.
	if !strings.Contains(joined, tok) {
		t.Fatalf("Lines() must carry the RAW token for the parser; got:\n%s", joined)
	}
	// Masked on the terminal: the operator must never see it.
	shown := tee.String()
	if strings.Contains(shown, tok) {
		t.Fatalf("RAW TOKEN RELAYED TO THE TERMINAL:\n%s", shown)
	}
	if !strings.Contains(shown, agentcred.MaskToken(tok)) {
		t.Errorf("masked form missing from the relay; got:\n%s", shown)
	}
	if !strings.Contains(shown, "Welcome to Claude Code") {
		t.Errorf("ordinary child output must still reach the operator; got:\n%s", shown)
	}
}

// TestMaskedRelayWithholdsPartialTokenLine covers the nastiest leak shape: the
// idle flush exists so a prompt with no trailing newline still appears, and a
// token arriving mid-chunk would otherwise be flushed before it can be masked
// as a whole line. Such a fragment is held until its newline instead.
func TestMaskedRelayWithholdsPartialTokenLine(t *testing.T) {
	tok := "sk-ant-oat01-" + strings.Repeat("b", 40)
	pr, pw := newPipePair()
	proc := &handoffProc{lines: make(chan string, 16), waited: make(chan struct{})}
	tee := &syncBuf{}
	go proc.maskedRelay(pr, tee)

	// A partial line carrying the token marker, with NO newline.
	_, _ = pw.Write([]byte(tok[:30]))
	time.Sleep(4 * relayIdleFlush) // give the idle flush every chance to fire
	if s := tee.String(); strings.Contains(s, "sk-ant-") {
		t.Fatalf("partial token fragment was flushed to the terminal: %q", s)
	}

	// A partial line with no token DOES flush, so prompts still appear.
	_, _ = pw.Write([]byte("\nPaste your code here: "))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(tee.String(), "Paste your code here:") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(tee.String(), "Paste your code here:") {
		t.Fatalf("a newline-less prompt never reached the operator: %q", tee.String())
	}
	_ = pw.Close()
}

// TestHandoffProcWaitReleasesOnFinish pins the Proc contract claudeWalkthrough
// relies on: Lines() must close (so its range loop ends) and Wait() must then
// return the child's error.
func TestHandoffProcWaitReleasesOnFinish(t *testing.T) {
	pr, pw := newPipePair()
	proc := &handoffProc{lines: make(chan string, 4), waited: make(chan struct{})}
	proc.closeOut = func() { _ = pw.Close() }
	go proc.maskedRelay(pr, nil)

	boom := context.DeadlineExceeded
	proc.finish(boom)

	drained := make(chan struct{})
	go func() {
		for range proc.Lines() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Lines() never closed after finish(); claudeWalkthrough would hang forever")
	}
	if err := proc.Wait(); err != boom {
		t.Fatalf("Wait() = %v, want the child's error", err)
	}
	// finish is idempotent — a teardown may race tea.Exec's callback.
	proc.finish(nil)
	if err := proc.Wait(); err != boom {
		t.Fatalf("second finish() overwrote the result: %v", err)
	}
}

// TestStartFlowWiresHandoffOnlyWhereItSpawns proves the bridge is attached to
// the path that actually launches a child, and to no other. Codex must keep the
// DevNull exec (codexCapture depends on live line streaming for its
// device-auth-disabled detection and timeout kill), and the --host claude path
// runs in PasteMode and spawns nothing at all.
func TestStartFlowWiresHandoffOnlyWhereItSpawns(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		remote   string
		want     bool
	}{
		{"local claude spawns -> bridged", "claude", "", true},
		{"codex must not be bridged", "codex", "", false},
		{"remote claude pastes, spawns nothing", "claude", "ssh-dest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAccountRegisterModel(&regAcctClient{}, context.Background())
			m.remoteHost = tc.remote
			upd, _ := m.startFlow(tc.provider)
			got := upd.(AccountRegisterModel)
			// startFlow spawns the real flow goroutine, and that goroutine reads
			// package-level --host state on its way to the first prompt
			// (isRemoteHost, via the prompter's hostAwareFlowText). Cancelling it
			// is not enough: an abandoned goroutine is unordered against the next
			// test's SetHostDestination, which -race reports as a data race even
			// once the goroutine has finished. Join it, as the flow tests in
			// account_register_test.go do, so the read is ordered before the write.
			t.Cleanup(func() {
				got.teardown()
				select {
				case <-got.flowDone:
				case <-time.After(3 * time.Second):
					t.Error("flow goroutine leaked after teardown")
				}
			})

			if (got.execBridge != nil) != tc.want {
				t.Fatalf("execBridge non-nil = %v, want %v", got.execBridge != nil, tc.want)
			}
		})
	}
}

// newPipePair returns an in-memory pipe standing in for the child's stdout.
func newPipePair() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// TestMaskedRelaySuppressesRepaintNoise covers the display defect seen in the
// first live run: `claude setup-token` is an Ink app that repaints its whole
// frame on every spinner tick, so a verbatim relay replayed the banner once per
// frame and buried the sign-in URL. Normalisation strips the cursor-control
// sequences (meaningless once each line gets its own newline) and consecutive
// duplicates are dropped.
func TestMaskedRelaySuppressesRepaintNoise(t *testing.T) {
	pr, pw := newPipePair()
	proc := &handoffProc{lines: make(chan string, 64), waited: make(chan struct{})}
	tee := &syncBuf{}
	go proc.maskedRelay(pr, tee)

	// Five repaints of the same banner with different spinner glyphs and ANSI,
	// then the line that actually matters.
	for _, glyph := range []string{"·", "✢", "✳", "✶", "✻"} {
		_, _ = pw.Write([]byte("\x1b[2K\x1b[1GWelcome to Claude Code v2.1.233\n"))
		_, _ = pw.Write([]byte("\x1b[36m " + glyph + " Opening browser to sign in…\x1b[0m\n"))
	}
	_, _ = pw.Write([]byte("https://claude.com/cai/oauth/authorize?code=true\n"))
	_ = pw.Close()
	for range proc.Lines() {
	}

	shown := tee.String()
	if n := strings.Count(shown, "Welcome to Claude Code"); n != 1 {
		t.Errorf("banner relayed %d times, want 1 — repaint noise is back:\n%s", n, shown)
	}
	if strings.Contains(shown, "\x1b[") {
		t.Errorf("ANSI escapes reached the terminal verbatim:\n%q", shown)
	}
	if !strings.Contains(shown, "https://claude.com/cai/oauth/authorize") {
		t.Errorf("the sign-in URL must survive normalisation:\n%s", shown)
	}
}
