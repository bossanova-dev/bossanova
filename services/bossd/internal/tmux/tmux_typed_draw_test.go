package tmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const typedSlashPayload = "/boss-plan --exclude-creator alice --exclude-assignee bob"

const undrawnPlaceholderRow = "❯\u00a0Try \"fix typecheck errors\""

// typedSlashPanes returns the real Claude 2.1.302 pane captured right after an
// Enter that Claude had not read yet, plus the same pane with the composer
// drawing the payload and with the composer emptied by a submit.
func typedSlashPanes(t *testing.T) (undrawn, drawn, submitted string) {
	t.Helper()
	undrawn = readPaneFixture(t, "claude_typed_slash_undrawn_real.txt")
	if !strings.Contains(undrawn, undrawnPlaceholderRow) {
		t.Fatalf("fixture lost its placeholder row %q; recapture with capture_typed_slash.sh", undrawnPlaceholderRow)
	}
	drawn = strings.Replace(undrawn, undrawnPlaceholderRow, "❯ "+typedSlashPayload, 1)
	submitted = strings.Replace(undrawn, undrawnPlaceholderRow, "❯ ", 1)
	return undrawn, drawn, submitted
}

func TestComposerShowsPayloadOnRealClaudePanes(t *testing.T) {
	undrawn, drawn, _ := typedSlashPanes(t)
	newline := readPaneFixture(t, "claude_typed_slash_enter_newline_real.txt")
	for name, tc := range map[string]struct {
		pane string
		want bool
	}{
		"placeholder before Claude reads the keys": {undrawn, false},
		"typed line drawn":                         {drawn, true},
		"Enter taken as a newline":                 {newline, true},
	} {
		if got := composerShowsPayload(composerRows(tc.pane), typedSlashPayload); got != tc.want {
			t.Errorf("%s: composerShowsPayload = %v, want %v", name, got, tc.want)
		}
	}
	if !lineStillAtPrompt(newline, typedSlashPayload) {
		t.Error("a composer still holding the payload above the newline an Enter left must read as not submitted")
	}
}

func TestComposerShowsPayloadOnlyWhenTheBoxHoldsExactlyThePayload(t *testing.T) {
	for name, tc := range map[string]struct {
		rows string
		want bool
	}{
		"the payload":             {"❯ " + typedSlashPayload, true},
		"wrapped across rows":     {"❯ /boss-plan --exclude-creator\n  alice --exclude-assignee bob", true},
		"a draft before it":       {"❯ half-written draft " + typedSlashPayload, false},
		"a second copy behind it": {"❯ " + typedSlashPayload + typedSlashPayload, false},
		"the placeholder":         {undrawnPlaceholderRow, false},
	} {
		if got := composerShowsPayload(tc.rows, typedSlashPayload); got != tc.want {
			t.Errorf("%s: composerShowsPayload(%q) = %v, want %v", name, tc.rows, got, tc.want)
		}
	}
}

const bareSlashCommand = "/boss-repair"

// typedSlashMenuPanes returns the real Claude 2.1.302 pane drawing a typed bare
// slash command with the slash-command menu open below the input box, and the
// same pane after the command was submitted, which empties the box and closes
// the menu.
func typedSlashMenuPanes(t *testing.T) (drawn, submitted string) {
	t.Helper()
	drawn = readPaneFixture(t, "claude_typed_slash_menu_real.txt")
	const menuRow = "  ❯ " + bareSlashCommand + " "
	if !strings.Contains(drawn, "\n❯ "+bareSlashCommand+"\n") || !strings.Contains(drawn, "\n"+menuRow) {
		t.Fatalf("fixture lost its composer row or its menu row; recapture with capture_typed_slash_menu.sh")
	}
	lines := strings.Split(drawn, "\n")
	_, bottom, _ := inputBoxRules(lines)
	submitted = strings.Replace(strings.Join(lines[:bottom+1], "\n"), "❯ "+bareSlashCommand, "❯ ", 1)
	return drawn, submitted
}

func TestBareSlashCommandReadsAsDrawnWithTheMenuOpen(t *testing.T) {
	drawn, submitted := typedSlashMenuPanes(t)
	if !composerShowsPayload(typedComposerRows(drawn, bareSlashCommand), bareSlashCommand) {
		t.Errorf("the input box holds exactly %q, but the draw check read %q", bareSlashCommand, typedComposerRows(drawn, bareSlashCommand))
	}
	if !lineStillAtPrompt(drawn, bareSlashCommand) {
		t.Error("an unsubmitted bare slash command must read as still at the prompt while its menu is open")
	}
	if lineStillAtPrompt(submitted, bareSlashCommand) {
		t.Error("the submitted pane must read as submitted")
	}
}

func TestBareSlashCommandIsEnteredOnceTheComposerDrawsItWithTheMenuOpen(t *testing.T) {
	drawn, submitted := typedSlashMenuPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{drawn, drawn, drawn, submitted}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendLine(context.Background(), "boss-test-typed", bareSlashCommand, typedDrawOpts(time.Second, 3)); err != nil {
		t.Fatalf("sendLine: %v", err)
	}
	if callIndex(fake.calls, "send-keys", "Enter") < 0 {
		t.Fatalf("no Enter was sent for a drawn command: %+v", fake.calls)
	}
}

func TestShellModeLineReadsAsDrawnOnARealClaudePane(t *testing.T) {
	const line = "!echo hi"
	const drawnRow = "! echo hi"
	pane := readPaneFixture(t, "claude_typed_shell_mode_real.txt")
	if !strings.Contains(pane, drawnRow) {
		t.Fatalf("fixture lost its shell-mode row %q; recapture with capture_typed_shell_mode.sh", drawnRow)
	}
	for name, tc := range map[string]struct {
		pane, line string
		want       bool
	}{
		"the line drawn in shell mode":            {pane, line, true},
		"a different shell line":                  {pane, "!echo bye", false},
		"not read yet":                            {strings.Replace(pane, drawnRow, "❯ Try \"x\"", 1), line, false},
		"retyped into shell mode after only C-u":  {strings.Replace(pane, drawnRow, "! !echo hi", 1), line, false},
		"empty shell mode with the hint below it": {strings.Replace(pane, drawnRow, "! ", 1), line, false},
	} {
		if got := composerShowsPayload(typedComposerRows(tc.pane, tc.line), tc.line); got != tc.want {
			t.Errorf("%s: drawn = %v, want %v", name, got, tc.want)
		}
	}
}

func TestShellModeLineThatNeverDrawsLeavesShellModeWhenCleared(t *testing.T) {
	undrawn, _, _ := typedSlashPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendLine(context.Background(), "boss-test-typed", "!echo hi", typedDrawOpts(30*time.Millisecond, 2))
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
	clears := 0
	for _, call := range fake.calls {
		if call.subcommand == "send-keys" && slices.Equal(call.args[len(call.args)-2:], []string{"C-u", "BSpace"}) {
			clears++
		}
	}
	if clears != 2 {
		t.Fatalf("sent C-u BSpace %d times, want once between the two tries and once before giving up: %+v", clears, fake.calls)
	}
}

func TestTypedLineWhoseComposerCannotBeReadIsClearedBeforeGivingUp(t *testing.T) {
	undrawn, _, _ := typedSlashPanes(t)
	failFrom := 1
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn}, failCapturePaneFrom: &failFrom}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(time.Second, 3))
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
	typed := callIndex(fake.calls, "send-keys", typedSlashPayload)
	cleared := callIndex(fake.calls, "send-keys", "C-u")
	if typed < 0 || cleared < typed {
		t.Fatalf("the typed line (call %d) must be followed by a C-u (call %d): %+v", typed, cleared, fake.calls)
	}
	if callIndex(fake.calls, "send-keys", "Enter") >= 0 {
		t.Fatal("an Enter was sent for a line the composer was never read for")
	}
}

func typedDrawOpts(wait time.Duration, attempts int) sendPlanOpts {
	return sendPlanOpts{
		deadline:          time.Second,
		pollInterval:      5 * time.Millisecond,
		readyMarker:       sendPlanReadyMarker,
		submitVerifyWait:  200 * time.Millisecond,
		submitVerifyTick:  5 * time.Millisecond,
		typedDrawWait:     wait,
		typedDrawAttempts: attempts,
	}
}

// capturesBefore counts plain capture-pane calls recorded before index end.
func capturesBefore(calls []sendPlanCall, end int) int {
	n := 0
	for _, call := range calls[:end] {
		if call.subcommand == "capture-pane" && !slices.Contains(call.args, "-e") {
			n++
		}
	}
	return n
}

func TestTypedLineWaitsForTheComposerToDrawItBeforeEnter(t *testing.T) {
	undrawn, drawn, submitted := typedSlashPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn, undrawn, drawn, drawn, submitted}}
	c := NewClient(WithCommandFactory(fake.factory))

	if err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(time.Second, 1)); err != nil {
		t.Fatalf("sendLine: %v", err)
	}
	enter := callIndex(fake.calls, "send-keys", "Enter")
	if enter < 0 {
		t.Fatalf("no Enter was sent: %+v", fake.calls)
	}
	if got := capturesBefore(fake.calls, enter); got < 4 {
		t.Fatalf("Enter followed %d captures; it must wait for the composer to draw the line twice (4 captures)", got)
	}
}

func TestTypedLineThatNeverDrawsIsRetypedThenStopsBeforeEnter(t *testing.T) {
	undrawn, _, _ := typedSlashPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(30*time.Millisecond, 2))
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("outcome = %v (%v), want %v", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
	if callIndex(fake.calls, "send-keys", "Enter") >= 0 {
		t.Fatal("an Enter was sent for a line the composer never drew")
	}
	var typed, cleared int
	for _, call := range fake.calls {
		switch {
		case call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == typedSlashPayload:
			typed++
		case call.subcommand == "send-keys" && len(call.args) > 0 && call.args[len(call.args)-1] == "C-u":
			cleared++
		}
	}
	if typed != 2 || cleared != 2 {
		t.Fatalf("typed %d times and cleared %d times, want 2 and 2 (a clear between the tries and one before giving up)", typed, cleared)
	}
}

func TestTypedLineThatNeverDrawsNamesWhatTheComposerShowed(t *testing.T) {
	undrawn, _, _ := typedSlashPanes(t)
	fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn}}
	c := NewClient(WithCommandFactory(fake.factory))

	err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(30*time.Millisecond, 1))
	if want := `it last showed "❯ Try \"fix typecheck errors\""`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to end with %s", err, want)
	}
}

func TestProductionTypedDrawBudgets(t *testing.T) {
	c := NewClient()
	start := c.startDeliveryOpts(sendPlanReadyMarker, true, nil)
	if start.typedDrawWait != startTypedDrawWait || start.typedDrawAttempts != startTypedDrawAttempts {
		t.Errorf("Claude session start: typedDrawWait=%v attempts=%d, want %v and %d", start.typedDrawWait, start.typedDrawAttempts, startTypedDrawWait, startTypedDrawAttempts)
	}
	send := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)
	if send.typedDrawWait != send.pasteSettleWait || send.typedDrawAttempts != 1 {
		t.Errorf("Claude established send: typedDrawWait=%v attempts=%d, want the paste settle %v and 1", send.typedDrawWait, send.typedDrawAttempts, send.pasteSettleWait)
	}
	for name, opts := range map[string]sendPlanOpts{
		"codex start":    c.startDeliveryOpts(codexReadyMarker, true, nil),
		"Claude prefill": c.startDeliveryOpts(sendPlanReadyMarker, false, nil),
	} {
		if opts.typedDrawWait != 0 {
			t.Errorf("%s: typedDrawWait = %v, want 0", name, opts.typedDrawWait)
		}
	}
}

// slowComposerEnv carries the startup delay to the fake agent the real-tmux
// test below launches by re-running this test binary in the pane.
const slowComposerEnv = "BOSS_TMUX_SLOW_COMPOSER_BUSY"

// TestSlowComposerProcess is not a test of its own: it is the agent a pane runs
// for TestTypedSlashCommandSubmitsOnASlowStartingComposer.
func TestSlowComposerProcess(t *testing.T) {
	busy, err := time.ParseDuration(os.Getenv(slowComposerEnv))
	if err != nil {
		t.Skip("helper process for TestTypedSlashCommandSubmitsOnASlowStartingComposer")
	}
	runSlowComposer(os.Stdin, os.Stdout, busy)
}

// runSlowComposer models the Claude startup behaviour measured on Claude Code
// 2.1.302: the composer is drawn before input is read, and a read that carries
// typed text together with a CR is a paste, so that CR is a newline. Only a CR
// read on its own submits. A bare slash command opens the slash-command menu
// below the box, its selected entry marked "❯" as in
// testdata/panes/claude_typed_slash_menu_real.txt.
func runSlowComposer(in io.Reader, out io.Writer, busy time.Duration) {
	var composer string
	var ran []string
	draw := func() {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		for _, r := range ran {
			fmt.Fprintf(&b, "⏺ ran: %s\r\n", r)
		}
		b.WriteString("────────\r\n")
		lines := strings.Split(composer, "\n")
		if composer == "" {
			lines = []string{`Try "fix typecheck errors"`}
		}
		b.WriteString("❯ " + lines[0] + "\r\n")
		for _, l := range lines[1:] {
			b.WriteString("  " + l + "\r\n")
		}
		b.WriteString("────────\r\n")
		if strings.HasPrefix(composer, "/") && !strings.ContainsAny(composer, " \n") {
			fmt.Fprintf(&b, "  ❯ %-30s Automated PR repair (user)\r\n", composer)
			fmt.Fprintf(&b, "    %-30s Find-then-verify code review (user)\r\n", "/boss-review")
		}
		_, _ = io.WriteString(out, b.String())
	}
	draw()
	time.Sleep(busy)
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return
		}
		chunk := buf[:n]
		pasted := bytes.ContainsFunc(chunk, func(r rune) bool { return r >= ' ' })
		for _, ch := range chunk {
			switch {
			case ch == '\r' && pasted:
				composer += "\n"
			case ch == '\r':
				ran = append(ran, composer)
				composer = ""
			case ch == 0x15:
				composer = ""
			case ch >= ' ':
				composer += string(ch)
			}
		}
		draw()
	}
}

// startSlowComposer launches runSlowComposer in a real tmux pane that reads no
// input for busy after drawing its composer.
func startSlowComposer(ctx context.Context, t *testing.T, busy time.Duration) (*Client, string) {
	t.Helper()
	skipIfNoTmux(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	c, _ := newTestTmuxClient(t)
	name := uniqueSessionName("boss-typed-draw")
	if err := c.NewSession(ctx, NewSessionOpts{
		Name:    name,
		WorkDir: t.TempDir(),
		Command: []string{"sh", "-c", `stty raw -echo && exec "$0" -test.run='^TestSlowComposerProcess$'`, exe},
		Env:     map[string]string{slowComposerEnv: busy.String()},
		Width:   120,
		Height:  30,
	}); err != nil {
		t.Fatalf("new session: %v", err)
	}
	return c, name
}

// ranLines returns the lines the slow composer reports it ran.
func ranLines(ctx context.Context, t *testing.T, c *Client, name string) []string {
	t.Helper()
	pane, err := c.CapturePane(ctx, name)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	var ran []string
	for _, l := range strings.Split(pane, "\n") {
		if r, ok := strings.CutPrefix(strings.TrimRight(l, " "), "⏺ ran: "); ok {
			ran = append(ran, r)
		}
	}
	return ran
}

func TestTypedSlashCommandSubmitsOnASlowStartingComposer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, name := startSlowComposer(ctx, t, 1500*time.Millisecond)

	if err := c.sendLine(ctx, name, typedSlashPayload, c.startDeliveryOpts(sendPlanReadyMarker, true, nil)); err != nil {
		t.Fatalf("sendLine: %v", err)
	}
	if ran := ranLines(ctx, t, c, name); !slices.Equal(ran, []string{typedSlashPayload}) {
		t.Fatalf("sendLine reported success; the agent ran %q, want exactly the payload once", ran)
	}
}

func TestBareSlashCommandSubmitsWhileClaudeShowsTheSlashMenu(t *testing.T) {
	for name, delivery := range map[string]func(*Client) sendPlanOpts{
		"session start":    func(c *Client) sendPlanOpts { return c.startDeliveryOpts(sendPlanReadyMarker, true, nil) },
		"established chat": func(c *Client) sendPlanOpts { return c.sendDeliveryOpts(sendPlanReadyMarker, true, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 0)

			if err := c.sendLine(ctx, pane, bareSlashCommand, delivery(c)); err != nil {
				t.Fatalf("sendLine: %v", err)
			}
			if ran := ranLines(ctx, t, c, pane); !slices.Equal(ran, []string{bareSlashCommand}) {
				t.Fatalf("sendLine reported success; the agent ran %q, want exactly the command once", ran)
			}
		})
	}
}

// An established send gives a slow composer one short try. The caller retries a
// not-submitted send, so the typed line the composer reads late must not be
// left behind for the retry to submit a second copy with.
func TestRetriedTypedLineSubmitsOneCopyAfterTheComposerReadsLate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, name := startSlowComposer(ctx, t, 2500*time.Millisecond)
	opts := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)

	err := c.sendLine(ctx, name, typedSlashPayload, opts)
	if OutcomeOf(err) != OutcomeNotSubmitted {
		t.Fatalf("first send: outcome = %v (%v), want %v while the composer is not reading", OutcomeOf(err), err, OutcomeNotSubmitted)
	}
	for try := 2; ; try++ {
		time.Sleep(300 * time.Millisecond)
		if err = c.sendLine(ctx, name, typedSlashPayload, opts); err == nil {
			break
		}
		if try == 6 {
			t.Fatalf("no retry submitted the line; last error: %v; ran %q", err, ranLines(ctx, t, c, name))
		}
	}
	if ran := ranLines(ctx, t, c, name); !slices.Equal(ran, []string{typedSlashPayload}) {
		t.Fatalf("the retry reported success; the agent ran %q, want exactly the payload once", ran)
	}
}
