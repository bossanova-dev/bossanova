package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
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
		"collapsed into a paste":  {"❯ [Pasted text #1]", true},
		"collapsed with a tail":   {"❯ [Pasted text #5][Pasted text #6]lorem ipsum padding 90", true},
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
	waits := make([]time.Duration, attempts)
	for i := range waits {
		waits[i] = wait
	}
	return sendPlanOpts{
		deadline:         time.Second,
		pollInterval:     5 * time.Millisecond,
		readyMarker:      sendPlanReadyMarker,
		submitVerifyWait: 200 * time.Millisecond,
		submitVerifyTick: 5 * time.Millisecond,
		typedDrawWaits:   waits,
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

	opts := typedDrawOpts(30*time.Millisecond, 2)
	opts.typedDrawWaits = []time.Duration{20 * time.Millisecond, 40 * time.Millisecond}
	err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, opts)
	if want := "within 60ms over 2 attempts, so no Enter was sent; partial draw seen: false"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %q", err, want)
	}
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

// The fake pane advances on the clock rather than the capture count, so a
// polling-rate change cannot hide a draw that spans multiple attempt windows.
func TestTypedDrawKeepsProgressAndRetypesAStalledPrefix(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled=%t", stalled), func(t *testing.T) {
			undrawn, drawn, submitted := typedSlashPanes(t)
			prefix := func(text string) string {
				return strings.Replace(undrawn, undrawnPlaceholderRow, "❯ "+text, 1)
			}
			rules := []capturePaneRule{{0, undrawn}, {20 * time.Millisecond, prefix("/boss")}}
			if !stalled {
				rules = append(rules, capturePaneRule{120 * time.Millisecond, prefix("/boss-plan --exclude")})
				rules = append(rules, capturePaneRule{250 * time.Millisecond, drawn})
			}
			fake := &sendPlanRecordingFactory{capturePaneRules: rules}
			typeCount := 0
			c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				if slices.Contains(args, typedSlashPayload) {
					typeCount++
					if stalled && typeCount == 2 {
						// A stalled composer cannot finish before the retry, even if
						// scheduling skips over its entire timeout window.
						fake.capturePaneRules = nil
						fake.capturePaneOutputs = []string{drawn}
					}
				}
				if slices.Contains(args, "Enter") {
					fake.capturePaneRules = nil
					fake.capturePaneOutputs = []string{submitted}
				}
				return fake.factory(ctx, name, args...)
			}))
			if err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(100*time.Millisecond, 3)); err != nil {
				t.Fatalf("sendLine: %v", err)
			}
			typed, cleared, entered := 0, 0, 0
			for _, call := range fake.calls {
				if call.subcommand != "send-keys" {
					continue
				}
				switch call.args[len(call.args)-1] {
				case typedSlashPayload:
					typed++
				case "C-u":
					cleared++
				case "Enter":
					entered++
				}
			}
			wantRetypes := 0
			if stalled {
				wantRetypes = 1
			}
			if typed != 1+wantRetypes || cleared != wantRetypes || entered != 1 {
				t.Fatalf("typed=%d cleared=%d Enter=%d; want %d, %d, 1", typed, cleared, entered, 1+wantRetypes, wantRetypes)
			}
		})
	}
}

func TestTypedDrawReportsPartialWrappedAndShellInput(t *testing.T) {
	undrawn, _, _ := typedSlashPanes(t)
	for name, tc := range map[string]struct{ payload, pane string }{
		"wrapped":    {typedSlashPayload, strings.Replace(undrawn, undrawnPlaceholderRow, "❯ /boss-plan\n  --exclude", 1)},
		"shell mode": {"!echo hello", strings.Replace(readPaneFixture(t, "claude_typed_shell_mode_real.txt"), "! echo hi", "! echo h", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			// The readiness capture is empty; subsequent captures keep a strict
			// prefix. It gets one more window, then fails without Enter.
			fake := &sendPlanRecordingFactory{capturePaneOutputs: []string{undrawn, tc.pane}}
			c := NewClient(WithCommandFactory(fake.factory))
			err := c.sendLine(context.Background(), "boss-test-typed", tc.payload, typedDrawOpts(30*time.Millisecond, 2))
			if OutcomeOf(err) != OutcomeNotSubmitted || !strings.Contains(err.Error(), "partial draw seen: true") {
				t.Fatalf("error = %v; want not-submitted with partial draw", err)
			}
			if got := literalSendKeysCount(fake); got != 1 {
				t.Errorf("typed %d copies, want one carried into the second window", got)
			}
			if callIndex(fake.calls, "send-keys", "Enter") >= 0 {
				t.Fatal("an incomplete line must not be submitted")
			}
		})
	}
}

func TestProductionTypedDrawBudgets(t *testing.T) {
	c := NewClient()
	start := c.startDeliveryOpts(sendPlanReadyMarker, true, nil)
	if len(start.typedDrawWaits) != 3 || typedDrawBudget(start) < 30*time.Second {
		t.Errorf("Claude session start schedule = %v, want three attempts totalling at least 30s", start.typedDrawWaits)
	}
	send := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)
	if !slices.Equal(send.typedDrawWaits, []time.Duration{send.pasteSettleWait}) {
		t.Errorf("Claude established send schedule = %v, want one paste settle %v", send.typedDrawWaits, send.pasteSettleWait)
	}
	for name, opts := range map[string]sendPlanOpts{
		"codex start":    c.startDeliveryOpts(codexReadyMarker, true, nil),
		"Claude prefill": c.startDeliveryOpts(sendPlanReadyMarker, false, nil),
	} {
		if len(opts.typedDrawWaits) != 0 {
			t.Errorf("%s: typedDrawWaits = %v, want empty", name, opts.typedDrawWaits)
		}
	}
}

// slowComposerEnv carries the startup delay to the fake agent the real-tmux
// test below launches by re-running this test binary in the pane.
const slowComposerEnv = "BOSS_TMUX_SLOW_COMPOSER_BUSY"

// slowComposerSwallowEnv carries how many lone Enters the fake agent drops,
// as a Claude composer mid-render can.
const slowComposerSwallowEnv = "BOSS_TMUX_SLOW_COMPOSER_SWALLOW"

// slowComposerColumnsEnv carries the pane width, which the fake agent wraps
// its composer text to.
const slowComposerColumnsEnv = "BOSS_TMUX_SLOW_COMPOSER_COLUMNS"

// slowComposerClearLagEnv carries how long the fake agent takes to act on a
// C-u, so the frame read just after the press is stale.
const slowComposerClearLagEnv = "BOSS_TMUX_SLOW_COMPOSER_CLEAR_LAG"

// slowComposer configures the fake agent a real-tmux test runs in its pane.
type slowComposer struct {
	busy     time.Duration
	swallow  int
	clearLag time.Duration
}

// slowComposerColumns is the width of every pane the fake agent runs in.
const slowComposerColumns = 120

// TestSlowComposerProcess is not a test of its own: it is the agent a pane runs
// for TestTypedSlashCommandSubmitsOnASlowStartingComposer.
func TestSlowComposerProcess(t *testing.T) {
	busy, err := time.ParseDuration(os.Getenv(slowComposerEnv))
	if err != nil {
		t.Skip("helper process for TestTypedSlashCommandSubmitsOnASlowStartingComposer")
	}
	swallow, _ := strconv.Atoi(os.Getenv(slowComposerSwallowEnv))
	clearLag, _ := time.ParseDuration(os.Getenv(slowComposerClearLagEnv))
	columns, _ := strconv.Atoi(os.Getenv(slowComposerColumnsEnv))
	runSlowComposer(os.Stdin, os.Stdout, slowComposer{busy: busy, swallow: swallow, clearLag: clearLag}, columns)
}

// claudeComposerWrapWidth is how many columns of a pane columns wide Claude
// Code 2.1.302 wraps its input text at. The prompt input is given the pane's
// columns less 3, and its text measure is one narrower than that.
func claudeComposerWrapWidth(columns int) int {
	return columns - 4
}

// wrapClaudeComposerLine splits one line of composer text into the rows Claude
// Code 2.1.302 draws it as: wrap-ansi with hard set and trim unset. A row
// breaks before the word that does not fit and keeps the space before it, a
// word wider than a row is cut, and widths are display cells, so a wide
// character takes two. The rows joined are the line.
func wrapClaudeComposerLine(line string, width int) []string {
	rows := []string{""}
	for i, word := range strings.Split(line, " ") {
		rowWidth := cellsWide(rows[len(rows)-1])
		if i > 0 {
			if rowWidth >= width {
				rows = append(rows, "")
				rowWidth = 0
			}
			rows[len(rows)-1] += " "
			rowWidth++
		}
		wordWidth := cellsWide(word)
		if wordWidth > width {
			breaksOnThisRow := 1 + (wordWidth-(width-rowWidth)-1)/width
			breaksOnNextRow := (wordWidth - 1) / width
			if breaksOnNextRow < breaksOnThisRow {
				rows = append(rows, "")
			}
			rows = cutClaudeComposerWord(rows, word, width)
			continue
		}
		if rowWidth+wordWidth > width && rowWidth > 0 && wordWidth > 0 {
			rows = append(rows, "")
		}
		rows[len(rows)-1] += word
	}
	return rows
}

// cutClaudeComposerWord is wrap-ansi's wrapWord: it appends word to the last
// row a character at a time, starting a new row whenever the next character
// would pass width.
func cutClaudeComposerWord(rows []string, word string, width int) []string {
	visible := cellsWide(rows[len(rows)-1])
	chars := []rune(word)
	for i, r := range chars {
		w := cellWidth(r)
		if visible+w <= width {
			rows[len(rows)-1] += string(r)
		} else {
			rows = append(rows, string(r))
			visible = 0
		}
		visible += w
		if visible == width && i < len(chars)-1 {
			rows = append(rows, "")
			visible = 0
		}
	}
	if last := len(rows) - 1; visible == 0 && rows[last] != "" && last > 0 {
		rows[last-1] += rows[last]
		rows = rows[:last]
	}
	return rows
}

// cellsWide is how many terminal cells s takes.
func cellsWide(s string) int {
	n := 0
	for _, r := range s {
		n += cellWidth(r)
	}
	return n
}

// cellWidth is how many terminal cells r takes: none for a combining mark,
// two for an East Asian wide or fullwidth character or an emoji, else one.
func cellWidth(r rune) int {
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me):
		return 0
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0x303e, r >= 0x3041 && r <= 0x33ff,
		r >= 0x3400 && r <= 0x4dbf, r >= 0x4e00 && r <= 0x9fff, r >= 0xa000 && r <= 0xa4cf,
		r >= 0xac00 && r <= 0xd7a3, r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f,
		r >= 0xff00 && r <= 0xff60, r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1f64f,
		r >= 0x1f900 && r <= 0x1f9ff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// deleteToLineStart is Claude Code 2.1.302's ctrl+u with the cursor at the end
// of the text, as its Cursor.deleteToLineStart does it: a newline just before
// the cursor is killed on its own; otherwise the text is killed back to the
// start of the cursor's wrapped row.
func deleteToLineStart(text string, width int) string {
	if strings.HasSuffix(text, "\n") {
		return strings.TrimSuffix(text, "\n")
	}
	head, last := "", text
	if i := strings.LastIndex(text, "\n"); i >= 0 {
		head, last = text[:i+1], text[i+1:]
	}
	rows := wrapClaudeComposerLine(last, width)
	return head + strings.Join(rows[:len(rows)-1], "")
}

// runSlowComposer models the Claude startup behaviour measured on Claude Code
// 2.1.302: the composer is drawn before input is read, and a read that carries
// typed text together with a CR is a paste, so that CR is a newline. Only a CR
// read on its own submits. A bare slash command opens the slash-command menu
// below the box, its selected entry marked "❯" as in
// testdata/panes/claude_typed_slash_menu_real.txt. It styles the composer the
// way Claude Code 2.1.302 renders it: the prompt glyph is coloured, the cursor
// is an inverse cell, which an empty composer puts on the first letter of its
// dim placeholder, and a slash command followed by a space shows a dim
// argument hint. Input is read as UTF-8 characters, and a read holding more
// than two CRs or 800 UTF-16 units is drawn as one "[Pasted text #N +M lines]"
// placeholder, as Claude draws a long paste. Text is wrapped as Claude wraps
// it (wrapClaudeComposerLine), with the cursor cell wrapped as part of the
// text, so a last row that fills the width puts the cursor on an empty row of
// its own. C-u is deleteToLineStart. The first swallow lone CRs are dropped.
func runSlowComposer(in io.Reader, out io.Writer, cfg slowComposer, columns int) {
	busy, swallow := cfg.busy, cfg.swallow
	width := claudeComposerWrapWidth(columns)
	if columns <= 0 {
		width = 1 << 20
	}
	var composer string
	var ran []string
	pastes := 0
	draw := func() {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		for _, r := range ran {
			fmt.Fprintf(&b, "⏺ ran: %s\r\n", r)
		}
		b.WriteString("────────\r\n")
		var lines []string
		for _, l := range strings.Split(composer, "\n") {
			lines = append(lines, wrapClaudeComposerLine(l, width)...)
		}
		if composer != "" && cellsWide(lines[len(lines)-1]) >= width {
			lines = append(lines, "")
		}
		last := len(lines) - 1
		lines[last] += "\x1b[7m \x1b[27m"
		if composer == "" {
			lines = []string{"\x1b[7mT\x1b[27m\x1b[2m" + `ry "fix typecheck errors"` + "\x1b[22m"}
		}
		if strings.HasPrefix(composer, "/") && strings.HasSuffix(composer, " ") && !strings.ContainsAny(strings.TrimSpace(composer), " \n") {
			lines[last] += "\x1b[2m<ticket-id>\x1b[22m"
		}
		b.WriteString("\x1b[38;2;153;153;153m❯\x1b[39m " + lines[0] + "\r\n")
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
	var read []byte
	for {
		n, err := in.Read(buf)
		if err != nil {
			return
		}
		read = append(read, buf[:n]...)
		end := len(read)
		for i := len(read) - 1; i >= 0 && i > len(read)-utf8.UTFMax; i-- {
			if utf8.RuneStart(read[i]) {
				if !utf8.FullRune(read[i:]) {
					end = i
				}
				break
			}
		}
		chunk := string(read[:end])
		read = append([]byte(nil), read[end:]...)
		pasted := strings.ContainsFunc(chunk, func(r rune) bool { return r >= ' ' })
		if newlines := strings.Count(chunk, "\r"); pasted && (len(utf16.Encode([]rune(chunk))) > 800 || newlines > 2) {
			pastes++
			if newlines == 0 {
				composer += fmt.Sprintf("[Pasted text #%d]", pastes)
			} else {
				composer += fmt.Sprintf("[Pasted text #%d +%d lines]", pastes, newlines)
			}
			draw()
			continue
		}
		for _, ch := range chunk {
			switch {
			case ch == '\r' && pasted:
				composer += "\n"
			case ch == '\r' && swallow > 0:
				swallow--
			case ch == '\r':
				ran = append(ran, composer)
				composer = ""
			case ch == 0x15:
				time.Sleep(cfg.clearLag)
				composer = deleteToLineStart(composer, width)
			case ch >= ' ':
				composer += string(ch)
			}
		}
		draw()
	}
}

// slowComposerSlots bounds how many parallel subtests drive a slow composer at
// once. Each runs a fake agent process and polls its pane, and unbounded they
// starve this package's tests whose verify windows are milliseconds long.
var slowComposerSlots = make(chan struct{}, 4)

// parallelSlowComposer marks t parallel and waits for a slow-composer slot,
// which it frees when t ends.
func parallelSlowComposer(t *testing.T) {
	t.Helper()
	t.Parallel()
	slowComposerSlots <- struct{}{}
	t.Cleanup(func() { <-slowComposerSlots })
}

// startSlowComposer launches runSlowComposer in a real tmux pane that reads no
// input for busy after drawing its composer.
func startSlowComposer(ctx context.Context, t *testing.T, busy time.Duration) (*Client, string) {
	t.Helper()
	return startSlowComposerSwallowing(ctx, t, busy, 0)
}

// startSlowComposerSwallowing is startSlowComposer with a composer that drops
// its first swallow lone Enters.
func startSlowComposerSwallowing(ctx context.Context, t *testing.T, busy time.Duration, swallow int) (*Client, string) {
	t.Helper()
	return startSlowComposerWith(ctx, t, slowComposer{busy: busy, swallow: swallow})
}

// startSlowComposerWith launches runSlowComposer, configured by cfg, in a real
// tmux pane slowComposerColumns wide, and waits for it to draw its composer.
// Keys sent before then reach a terminal still in cooked mode, which echoes
// them and turns a paste's CRs into the LFs the composer drops, so a
// three-line paste draws as one row.
func startSlowComposerWith(ctx context.Context, t *testing.T, cfg slowComposer) (*Client, string) {
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
		Env: map[string]string{
			slowComposerEnv:         cfg.busy.String(),
			slowComposerSwallowEnv:  strconv.Itoa(cfg.swallow),
			slowComposerClearLagEnv: cfg.clearLag.String(),
			slowComposerColumnsEnv:  strconv.Itoa(slowComposerColumns),
		},
		Width:  slowComposerColumns,
		Height: 30,
	}); err != nil {
		t.Fatalf("new session: %v", err)
	}
	for {
		if out, err := c.CapturePane(ctx, name); err == nil && composerRows(out) != "" {
			return c, name
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the slow composer never drew its input box: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
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
	for _, delay := range []time.Duration{1500 * time.Millisecond, 6 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, name := startSlowComposer(ctx, t, delay)

			if err := c.sendLine(ctx, name, typedSlashPayload, c.startDeliveryOpts(sendPlanReadyMarker, true, nil)); err != nil {
				t.Fatalf("sendLine: %v", err)
			}
			if ran := ranLines(ctx, t, c, name); !slices.Equal(ran, []string{typedSlashPayload}) {
				t.Fatalf("sendLine reported success; the agent ran %q, want exactly the payload once", ran)
			}
		})
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

// An operator typing in an attached Claude pane must not lose the draft when a
// delivery lands. The send is refused before a key is typed, so the draft stays
// in the box, nothing runs, and the caller can retry once the box is clear.
// An operator's collapsed paste is a draft like any other.
func TestDeliveryLeavesAnOperatorsDraftAlone(t *testing.T) {
	sends := map[string]func(context.Context, *Client, string) error{
		"typed slash command": func(ctx context.Context, c *Client, pane string) error {
			return c.sendLine(ctx, pane, typedSlashPayload, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
		},
		"pasted message": func(ctx context.Context, c *Client, pane string) error {
			return c.sendPlan(ctx, pane, "summarise the repo", c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
		},
	}
	drafts := map[string]string{"draft": "half-written draft", "collapsed paste": "[Pasted text #1 +3 lines]"}
	for draftName, draft := range drafts {
		for name, send := range sends {
			t.Run(draftName+" "+name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
				defer cancel()
				c, pane := startSlowComposer(ctx, t, 0)
				if err := c.typeLiteralLineNoEnter(ctx, pane, draft); err != nil {
					t.Fatalf("type the operator's draft: %v", err)
				}
				if draw, err := c.awaitComposerDraws(ctx, pane, draft, 5*time.Second, 50*time.Millisecond, 0); err != nil || !draw.drawn {
					t.Fatalf("the composer never drew the draft (err %v); it showed %q", err, draw.rows)
				}

				err := send(ctx, c, pane)
				if OutcomeOf(err) != OutcomeComposerOccupied || !errors.Is(err, ErrComposerOccupied) {
					t.Fatalf("err = %v (outcome %v); want %v", err, OutcomeOf(err), OutcomeComposerOccupied)
				}
				screen, err := c.CapturePane(ctx, pane)
				if err != nil {
					t.Fatalf("capture: %v", err)
				}
				if rows := composerRows(screen); !composerShowsPayload(rows, draft) {
					t.Fatalf("the composer shows %q after the refused send, want the draft %q untouched", rows, draft)
				}
				if ran := ranLines(ctx, t, c, pane); len(ran) != 0 {
					t.Fatalf("the agent ran %q, want nothing", ran)
				}
			})
		}
	}
}

// A send that ends not submitted withdraws its payload, so the next send finds
// an empty composer. The callback worker's retry is not the same text, since it
// adds a "REPEAT DELIVERY" line, so without the withdrawal every retry finds the
// first attempt in the box and is refused as occupied.
func TestNotSubmittedSendWithdrawsItsPayloadForTheRetry(t *testing.T) {
	type send func(context.Context, *Client, string, string) error
	sendPlan := func(ctx context.Context, c *Client, pane, payload string) error {
		return c.sendPlan(ctx, pane, payload, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
	}
	sendLine := func(ctx context.Context, c *Client, pane, payload string) error {
		return c.sendLine(ctx, pane, payload, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
	}
	for name, tc := range map[string]struct {
		send         send
		first, retry string
	}{
		"pasted message":      {sendPlan, "summarise the repo", "REPEAT DELIVERY, attempt 2: summarise the repo"},
		"two-line message":    {sendPlan, withdrawalPayloads["two lines"], "REPEAT DELIVERY, attempt 2"},
		"wrapped message":     {sendPlan, withdrawalPayloads["wrapped line"], "REPEAT DELIVERY, attempt 2"},
		"three-line message":  {sendPlan, withdrawalPayloads["three lines"], "REPEAT DELIVERY, attempt 2"},
		"typed slash command": {sendLine, bareSlashCommand, bareSlashCommand},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			// The first send's Enter and its retry's Enter are both dropped.
			c, pane := startSlowComposerSwallowing(ctx, t, 0, 2)

			sendErr := tc.send(ctx, c, pane, tc.first)
			if OutcomeOf(sendErr) != OutcomeNotSubmitted {
				t.Fatalf("first send: outcome = %v (%v), want %v while the composer drops Enter", OutcomeOf(sendErr), sendErr, OutcomeNotSubmitted)
			}
			styled, err := c.captureVisibleStyled(ctx, pane)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if composerHoldsDraft(styled) {
				t.Fatalf("the not-submitted send (%v) left %q in the composer, want it empty", sendErr, composerRows(plainText(styled)))
			}

			if err := tc.send(ctx, c, pane, tc.retry); err != nil {
				t.Fatalf("retry: %v (outcome %v); ran %q", err, OutcomeOf(err), ranLines(ctx, t, c, pane))
			}
			if ran := ranLines(ctx, t, c, pane); !slices.Equal(ran, []string{tc.retry}) {
				t.Fatalf("the agent ran %q, want exactly the retry once", ran)
			}
		})
	}
}

// The withdrawal clears only a composer holding exactly the payload. Text
// beside it stays, since an operator may have typed it while the send waited.
func TestWithdrawLeavesAComposerHoldingMoreThanThePayload(t *testing.T) {
	const payload = "summarise the repo"
	for name, box := range map[string]string{
		"text before the payload": "half-written " + payload,
		"text after the payload":  payload + " and the tests",
		"text after part of it":   "summarise the and the tests",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 0)
			typeComposerText(ctx, t, c, pane, box)

			err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
			if OutcomeOf(err) != OutcomeNotSubmitted || !errors.Is(err, errSubmissionPending) {
				t.Fatalf("err = %v (outcome %v); want the not-submitted verdict kept", err, OutcomeOf(err))
			}
			screen, err := c.CapturePane(ctx, pane)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if rows := composerRows(screen); !composerHoldsExactly(rows, box) {
				t.Fatalf("the composer shows %q after the withdrawal, want %q untouched", rows, box)
			}
		})
	}
}

// typeComposerText types box into the slow composer's empty box and waits
// until the box draws it.
func typeComposerText(ctx context.Context, t *testing.T, c *Client, pane, box string) {
	t.Helper()
	if err := c.typeLiteralLineNoEnter(ctx, pane, box); err != nil {
		t.Fatalf("type the composer text: %v", err)
	}
	if draw, err := c.awaitComposerDraws(ctx, pane, box, 5*time.Second, 50*time.Millisecond, 0); err != nil || !draw.drawn {
		t.Fatalf("the composer never drew %q (err %v); it showed %q", box, err, draw.rows)
	}
}

// An operator who clears bossd's text and starts a message of their own that
// begins as bossd's does leaves a box holding a prefix of the payload. A
// not-submitted send that did not end with the Enter retry's presses leaving
// part of the payload takes only the whole payload, so it presses nothing.
func TestWithdrawalLeavesAnOperatorsPrefixOfThePayloadAlone(t *testing.T) {
	for name, tc := range map[string]struct{ box, payload string }{
		"words":               {"fix the", "fix the build please"},
		"one letter":          {"f", "fix the build please"},
		"part of a command":   {"/boss", bareSlashCommand},
		"slash of a command":  {"/", bareSlashCommand},
		"first of two lines":  {"line one of the message", withdrawalPayloads["two lines"]},
		"first wrapped words": {"withdraw the wrapped", withdrawalPayloads["wrapped line"]},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 0)
			typeComposerText(ctx, t, c, pane, tc.box)
			presses := 0
			onTmux(c, func(args []string, _ CommandFactory) {
				if slices.Contains(args, "send-keys") {
					presses++
				}
			})

			err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: tc.payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
			screen, capErr := c.CapturePane(ctx, pane)
			if capErr != nil {
				t.Fatalf("capture: %v", capErr)
			}
			if rows := composerRows(screen); !errors.Is(err, withdrawHeldOther) || presses != 0 || !composerHoldsExactly(rows, tc.box) {
				t.Fatalf("the withdrawal ended %v after %d keys, leaving %q; want the operator's %q untouched", err, presses, rows, tc.box)
			}
		})
	}
}

// When the Enter retry's presses leave part of the payload behind, the
// withdrawal clears that part, and only while the box holds exactly it.
func TestWithdrawalTakesOnlyThePartTheRetryLeft(t *testing.T) {
	const payload = "fix the build please"
	for name, tc := range map[string]struct {
		left, box string
		withdraws bool
	}{
		"the part the retry left":            {"fix the", "fix the", true},
		"less than the retry left":           {"fix the build", "fix the", false},
		"more than the retry left":           {"fix", "fix the", false},
		"the retry left text past the start": {"the build", "the build", false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 0)
			typeComposerText(ctx, t, c, pane, tc.box)
			left := classifySubmit(OutcomeNotSubmitted, &partialPayload{held: tc.left, err: errSubmissionPending})

			err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, left)
			styled, capErr := c.captureVisibleStyled(ctx, pane)
			if capErr != nil {
				t.Fatalf("capture: %v", capErr)
			}
			rows := composerRows(plainText(styled))
			switch {
			case tc.withdraws && (!errors.Is(err, withdrawn) || composerHoldsDraft(styled)):
				t.Fatalf("the withdrawal ended %v, leaving %q; want the retry's %q withdrawn", err, rows, tc.left)
			case !tc.withdraws && (!errors.Is(err, withdrawHeldOther) || !composerHoldsExactly(rows, tc.box)):
				t.Fatalf("the withdrawal ended %v, leaving %q; want %q untouched", err, rows, tc.box)
			}
		})
	}
}

// partOf takes what C-u presses leave, which is a prefix of the payload. Text
// from further along the payload is not that, so a press never takes it.
func TestComposerDrawingPartOfIsAPrefixOfThePayload(t *testing.T) {
	drawn := composerDrawing{text: withdrawalPayloads["two lines"]}
	for box, want := range map[string]bool{
		"line one of the message":        true,
		"line one of the message\nline":  true,
		"line one":                       true,
		"of the message":                 false,
		"message\nline two":              false,
		"line two of the message":        false,
		"":                               false,
		"line one of the message, and I": false,
	} {
		if got := drawn.partOf(box); got != want {
			t.Errorf("partOf(%q) = %v, want %v", box, got, want)
		}
	}
	left := classifySubmit(OutcomeNotSubmitted, &partialPayload{held: "of the message", err: errSubmissionPending})
	if drawn.leftByRetry("of the message", left) {
		t.Error("leftByRetry took text from the middle of the payload for what the retry left")
	}
}

// The Enter retry presses C-u only into a box that shows this send's drawing
// whole. A Claude paste placeholder counts only with this send's paste id, and
// a Codex one only with the payload's character count, since an operator's own
// paste draws the same kind of placeholder.
func TestComposerDrawingRenderedByIsTheWholeDrawing(t *testing.T) {
	long := pasteFixtureLongLine()
	for _, tc := range []struct {
		name      string
		drawn     composerDrawing
		footprint string
		want      bool
	}{
		{"the whole text, wrapped", composerDrawing{text: "fix the build please"}, "fix the build\nplease", true},
		{"the start of the text", composerDrawing{text: "fix the build please"}, "fix the", false},
		{"other text", composerDrawing{text: "fix the build please"}, "please hold off, I am reviewing", false},
		{"the text and more", composerDrawing{text: "fix the build please"}, "fix the build please, and I", false},
		{"this send's placeholder", composerDrawing{text: long, collapsed: true, placeholder: "[Pasted text #3]"}, "[Pasted text #3]", true},
		{"another paste's placeholder", composerDrawing{text: long, collapsed: true, placeholder: "[Pasted text #3]"}, "[Pasted text #4]", false},
		{"a placeholder this send never read", composerDrawing{text: long, collapsed: true}, "[Pasted text #3]", false},
		{"Codex's placeholder counting the payload", composerDrawing{text: long}, "[Pasted Content 2181 chars]", true},
		{"Codex's placeholder counting another paste", composerDrawing{text: long}, "[Pasted Content 2180 chars]", false},
	} {
		if got := tc.drawn.renderedBy(tc.footprint); got != tc.want {
			t.Errorf("%s: renderedBy(%q) = %v, want %v", tc.name, tc.footprint, got, tc.want)
		}
	}
}

// After a C-u empties the box, Claude and Codex draw a placeholder, which
// reads as text in a plain capture. Only a styled capture tells it from an
// operator's text, and a capture with no style tells nothing.
func TestReadComposerTellsAPlaceholderFromTypedText(t *testing.T) {
	for _, tc := range []struct {
		name, capture string
		want          composerReading
	}{
		{"Claude's placeholder", readPaneFixture(t, "claude_composer_placeholder_styled_rendered.txt"), composerPlaceholder},
		{"Codex's placeholder", readPaneFixture(t, "codex_placeholder_styled_real.txt"), composerPlaceholder},
		{"a Claude draft", readPaneFixture(t, "claude_composer_draft_styled_rendered.txt"), composerTyped},
		{"Claude's placeholder under NO_COLOR", readPaneFixture(t, "claude_composer_no_color_placeholder_styled_rendered.txt"), composerUnstyled},
		{"a plain capture of text", "────\n❯ please hold off, I am reviewing\n────\n", composerUnstyled},
		{"a bare glyph", "────\n❯ \n────\n", composerBlank},
		{"no composer", "⏺ working\n", composerUndrawn},
	} {
		if got := readComposer(tc.capture); got != tc.want {
			t.Errorf("%s: readComposer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A C-u kills a newline before the cursor, or the cursor's wrapped row, back
// over an empty last row. The withdrawal takes for what a press left only what
// one of those leaves, so an operator who clears the box after a press and
// types the payload's first words is not taken for it.
func TestPressLeftIsWhatOneCtrlULeaves(t *testing.T) {
	for _, tc := range []struct {
		prev, next string
		want       bool
	}{
		{"line one of the message\nline two of the message", "line one of the message", true},
		{"line one of the message\n", "line one of the message", true},
		{"line one\n\nline three", "line one\n", true},
		{"withdraw the wrapped \npayload", "withdraw the wrapped ", true},
		{"xxxxxxxxxx\n", "", true},
		{"aaaa\nxxxxxxxxxx\n", "aaaa", true},
		{"fix the build please", "fix the", false},
		{"line one of the message\nline two of the message", "line one of the", false},
		{"line one of the message\nline two of the message", "line one of the message\nline two", false},
		{"line one of the message\nline two of the message", "line one of the message\nline two of the message", false},
		{"line one of the message\nline two of the message", "line one of the message and mine", false},
		{"line one of the message", "line one of the message\nmine", false},
	} {
		if got := pressLeft(tc.prev, tc.next); got != tc.want {
			t.Errorf("pressLeft(%q, %q) = %v, want %v", tc.prev, tc.next, got, tc.want)
		}
	}
}

// The Enter retry presses again only on a read that is the read before less
// what one C-u removes. Claude's removes the cursor's wrapped row, so an
// operator's trim of two rows does not pass there; Codex's removes the whole
// logical line, which may be several wrapped rows. A box an operator emptied
// and typed the payload's first words into passes for neither.
func TestClearLeftIsTheReadBeforeLessWhatOneCtrlURemoves(t *testing.T) {
	const held = "abcdefghi abcdefghi abc\ndefghi abcdefghi\nabcdefghi abcdef"
	for _, tc := range []struct {
		after         string
		claude, codex bool
	}{
		{"abcdefghi abcdefghi abc\ndefghi abcdefghi", true, true},
		{"abcdefghi abcdefghi abc", false, true},
		{"abcdefghi abcdefghi abc ", false, true},
		{"abcdefghi abc", false, false},
		{"abcdefghi abcdefghi abc\ndefghi", false, false},
		{held, false, false},
		{held + "\nmine", false, false},
		{"", false, false},
	} {
		for _, rule := range []struct {
			marker string
			want   bool
		}{{sendPlanReadyMarker, tc.claude}, {codexReadyMarker, tc.codex}} {
			if got := clearLeft(held, tc.after, ctrlUKillOf(rule.marker)); got != rule.want {
				t.Errorf("clearLeft(%q, %q) under %q's C-u = %v, want %v", held, tc.after, rule.marker, got, rule.want)
			}
		}
	}
}

// operatorSweepsEnv opts in to the exhaustive operator sweeps, which type
// before every capture bossd takes and so run one real-tmux send per capture.
// The per-PR suite keeps one named case per way they have found a send to
// erase or submit onto an operator's text; the nightly native job runs the
// sweeps.
const operatorSweepsEnv = "BOSS_TMUX_OPERATOR_SWEEPS"

// requireOperatorSweeps skips t unless operatorSweepsEnv is "1".
func requireOperatorSweeps(t *testing.T) {
	t.Helper()
	if os.Getenv(operatorSweepsEnv) != "1" {
		t.Skipf("exhaustive operator sweep; set %s=1 to run it", operatorSweepsEnv)
	}
}

// operatorMoment is when an operator acts during a send: just before the
// at-th capture bossd takes after it has sent key the nth time. An at of zero
// never acts, which counts the captures a send takes.
type operatorMoment struct {
	key     string
	nth, at int
}

// operatorSend is how one send against a slow composer ended. acted is
// whether the operator's moment came, and changed whether the operator then
// changed the box.
type operatorSend struct {
	captures int
	acted    bool
	changed  bool
	rows     string
	ran      []string
	err      error
}

// sendWithOperator runs send against a fresh slow composer that drops its
// first two Enters, and runs act once, at when.
func sendWithOperator(t *testing.T, send func(context.Context, *Client, string) error, when operatorMoment, act func(tmux CommandFactory, pane string) bool) operatorSend {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposerSwallowing(ctx, t, 0, 2)
	var got operatorSend
	keys, sent := 0, false
	onTmux(c, func(args []string, tmux CommandFactory) {
		if slices.Contains(args, "send-keys") && slices.Contains(args, when.key) {
			keys++
		}
		if !sent && keys >= when.nth && slices.Contains(args, "capture-pane") {
			if got.captures++; got.captures == when.at {
				got.acted = true
				got.changed = act(tmux, pane)
			}
		}
	})
	got.err = send(ctx, c, pane)
	sent = true
	screen, err := c.CapturePane(ctx, pane)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	got.rows, got.ran = composerRows(screen), ranLines(ctx, t, c, pane)
	return got
}

// sweepOperator first counts the captures a send with no operator takes
// from when, and hands that send to cleanOK. Then, in parallel subtests, it
// acts before each of those captures in turn and hands check what the send
// left. A send that ends before reaching its capture is tried twice more,
// then skipped.
func sweepOperator(t *testing.T, send func(context.Context, *Client, string) error, when operatorMoment, cleanOK func(*testing.T, operatorSend), act func(*testing.T, CommandFactory, string) bool, check func(*testing.T, operatorSend)) {
	t.Helper()
	clean := sendWithOperator(t, send, when, nil)
	cleanOK(t, clean)
	for at := 1; at <= clean.captures; at++ {
		t.Run(fmt.Sprintf("typed before capture %d of %d", at, clean.captures), func(t *testing.T) {
			parallelSlowComposer(t)
			moment := operatorMoment{when.key, when.nth, at}
			do := func(tmux CommandFactory, pane string) bool { return act(t, tmux, pane) }
			got := sendWithOperator(t, send, moment, do)
			for try := 2; !got.acted && try <= 3; try++ {
				t.Logf("send %d took only %d captures, ending %v (outcome %v) with %q", try-1, got.captures, got.err, OutcomeOf(got.err), got.rows)
				got = sendWithOperator(t, send, moment, do)
			}
			if !got.acted {
				t.Skipf("no send reached capture %d", at)
			}
			check(t, got)
		})
	}
}

// pasteMessage sends text as a Claude chat message.
func pasteMessage(text string) func(context.Context, *Client, string) error {
	return func(ctx context.Context, c *Client, pane string) error {
		return c.sendPlan(ctx, pane, text, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
	}
}

// keepsOperatorsPrefix checks that a send left the operator's text in the box
// and ran nothing.
func keepsOperatorsPrefix(operator string) func(*testing.T, operatorSend) {
	return func(t *testing.T, got operatorSend) {
		t.Helper()
		if !strings.Contains(withoutSpace(got.rows), withoutSpace(operator)) || len(got.ran) != 0 {
			t.Fatalf("the send ended %v (outcome %v), leaving %q and running %q; the operator's %q is gone", got.err, OutcomeOf(got.err), got.rows, got.ran, operator)
		}
	}
}

// keepsOperatorsMessage checks that a send left exactly the operator's text in
// the box and ran nothing.
func keepsOperatorsMessage(operator string) func(*testing.T, operatorSend) {
	return func(t *testing.T, got operatorSend) {
		t.Helper()
		if !composerHoldsExactly(got.rows, operator) || len(got.ran) != 0 {
			t.Fatalf("the send ended %v (outcome %v), leaving %q and running %q; want the operator's %q kept", got.err, OutcomeOf(got.err), got.rows, got.ran, operator)
		}
	}
}

// After the Enter retry, bossd reads the box until its wait runs out, and an
// operator who clears bossd's text and types a message of their own that
// begins as bossd's does may do so then. The send ends without erasing it.
func TestNotSubmittedSendKeepsAnOperatorsPrefixTypedRightAfterTheRetryEnter(t *testing.T) {
	parallelSlowComposer(t)
	got := sendWithOperator(t, pasteMessage("fix the build please"), operatorMoment{"Enter", 2, 1}, func(tmux CommandFactory, pane string) bool {
		operatorTypes(t, tmux, pane, true, "fix the")
		return true
	})
	if !got.acted {
		t.Fatalf("the send took no capture after its retry Enter, ending %v (outcome %v)", got.err, OutcomeOf(got.err))
	}
	keepsOperatorsPrefix("fix the")(t, got)
}

// An operator who empties the box while the Enter retry presses C-u, and
// types a message of their own, keeps it: the retry types nothing after it and
// presses no Enter, since the two would be submitted as one message.
func TestNotSubmittedSendKeepsForeignTextTypedInAPressWindow(t *testing.T) {
	parallelSlowComposer(t)
	const foreign = "please hold off, I am reviewing"
	got := sendWithOperator(t, pasteMessage(wrapShapes["780 characters"]), operatorMoment{"C-u", 1, 1}, func(tmux CommandFactory, pane string) bool {
		operatorEmptiesAndTypes(t, tmux, pane, foreign)
		return true
	})
	if !got.acted {
		t.Fatalf("the send pressed no C-u, ending %v (outcome %v)", got.err, OutcomeOf(got.err))
	}
	keepsOperatorsMessage(foreign)(t, got)
}

// The sweep behind TestNotSubmittedSendKeepsAnOperatorsPrefixTypedRightAfterTheRetryEnter:
// the operator's prefix lands before each capture bossd takes after its retry
// Enter.
func TestOperatorSweep_PrefixTypedAfterTheRetryEnter(t *testing.T) {
	requireOperatorSweeps(t)
	for name, tc := range map[string]struct {
		send     func(context.Context, *Client, string) error
		operator string
	}{
		"pasted message": {pasteMessage("fix the build please"), "fix the"},
		"typed slash command": {func(ctx context.Context, c *Client, pane string) error {
			return c.sendLine(ctx, pane, bareSlashCommand, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
		}, "/boss"},
	} {
		t.Run(name, func(t *testing.T) {
			cleanOK := func(t *testing.T, clean operatorSend) {
				if OutcomeOf(clean.err) != OutcomeNotSubmitted || clean.captures == 0 {
					t.Fatalf("a send with no operator ended %v (outcome %v) after %d captures past the retry Enter, leaving %q", clean.err, OutcomeOf(clean.err), clean.captures, clean.rows)
				}
			}
			types := func(t *testing.T, tmux CommandFactory, pane string) bool {
				operatorTypes(t, tmux, pane, true, tc.operator)
				return true
			}
			sweepOperator(t, tc.send, operatorMoment{"Enter", 2, 0}, cleanOK, types, keepsOperatorsPrefix(tc.operator))
		})
	}
}

// A payload of four or more wrapped rows takes every press the Enter retry
// has, so the retry can end with part of it left. An operator who acts before
// any of bossd's reads from its first Enter on, in the first verify window, at
// the baseline read, between the retry's presses or during the withdrawal,
// keeps what they leave: a message that begins as bossd's does, one that does
// not, or bossd's text with two rows trimmed off. Nothing is erased, and
// nothing is typed after it or submitted.
func TestOperatorSweep_OperatorActsOverALongPayload(t *testing.T) {
	requireOperatorSweeps(t)
	const foreign = "please hold off, I am reviewing"
	types := func(text string) func(*testing.T, CommandFactory, string) bool {
		return func(t *testing.T, tmux CommandFactory, pane string) bool {
			operatorEmptiesAndTypes(t, tmux, pane, text)
			return true
		}
	}
	for _, tc := range []struct {
		name, shape string
		act         func(*testing.T, CommandFactory, string) bool
		check       func(*testing.T, operatorSend)
	}{
		{"780 characters, a prefix typed", "780 characters", types("abcdefghi abc"), keepsOperatorsMessage("abcdefghi abc")},
		{"wide characters wrapped, a prefix typed", "wide characters wrapped", types("修复构建"), keepsOperatorsMessage("修复构建")},
		{"780 characters, other text typed", "780 characters", types(foreign), keepsOperatorsMessage(foreign)},
		{"780 characters, two rows trimmed", "780 characters", func(t *testing.T, tmux CommandFactory, pane string) bool { return operatorTrims(t, tmux, pane, 2) }, keepsATrimOf(wrapShapes["780 characters"])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sweepOperator(t, pasteMessage(wrapShapes[tc.shape]), operatorMoment{"Enter", 1, 0}, leavesPartOfThePayload, tc.act, tc.check)
		})
	}
}

// leavesPartOfThePayload checks that a send with no operator ended with the
// retry's presses leaving part of the payload, which is the ending a sweep
// over a long payload reaches.
func leavesPartOfThePayload(t *testing.T, clean operatorSend) {
	t.Helper()
	var partial *partialPayload
	if OutcomeOf(clean.err) != OutcomeNotSubmitted || clean.captures == 0 || !errors.As(clean.err, &partial) {
		t.Fatalf("a send with no operator ended %v (outcome %v) after %d captures past the first Enter, leaving %q; want the retry's presses to leave part of the payload", clean.err, OutcomeOf(clean.err), clean.captures, clean.rows)
	}
}

// keepsATrimOf checks that a send left a non-empty leading part of payload in
// the box and ran nothing. An operator who found nothing left to trim has
// nothing to keep, so only the second half applies.
func keepsATrimOf(payload string) func(*testing.T, operatorSend) {
	return func(t *testing.T, got operatorSend) {
		t.Helper()
		if !got.changed {
			if len(got.ran) != 0 {
				t.Fatalf("the send ended %v (outcome %v), running %q", got.err, OutcomeOf(got.err), got.ran)
			}
			return
		}
		first, rest, _ := strings.Cut(got.rows, "\n")
		content, _ := composerContent(strings.TrimSpace(first))
		box := withoutSpace(content + " " + rest)
		if box == "" || !strings.HasPrefix(withoutSpace(payload), box) || len(got.ran) != 0 {
			t.Fatalf("the send ended %v (outcome %v), leaving %q and running %q; want the operator's trim of the payload kept", got.err, OutcomeOf(got.err), got.rows, got.ran)
		}
	}
}

// operatorEmptiesAndTypes empties the slow composer's box as an operator
// holding C-u would, one press per wrapped row and newline, then types text.
func operatorEmptiesAndTypes(t *testing.T, tmux CommandFactory, pane, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	presses := []string{"send-keys", "-t", pane}
	for range 12 {
		presses = append(presses, "C-u")
	}
	if out, err := tmux(ctx, "tmux", presses...).CombinedOutput(); err != nil {
		t.Errorf("operator C-u: %v: %s", err, out)
		return
	}
	operatorTypes(t, tmux, pane, false, text)
}

// operatorTrims presses C-u rows times in the slow composer, as an operator
// cutting the last rows of the box would, and waits for the box to lose them.
// It presses nothing, and reports false, when the box holds no more rows than
// that, since the cut would empty it.
func operatorTrims(t *testing.T, tmux CommandFactory, pane string, rows int) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count := func() int {
		out, err := tmux(ctx, "tmux", "capture-pane", "-p", "-t", pane).Output()
		if err != nil {
			return -1
		}
		return composerRowCount(composerRows(string(out)))
	}
	before := count()
	if before <= rows {
		return false
	}
	presses := []string{"send-keys", "-t", pane}
	for range rows {
		presses = append(presses, "C-u")
	}
	if out, err := tmux(ctx, "tmux", presses...).CombinedOutput(); err != nil {
		t.Errorf("operator C-u: %v: %s", err, out)
		return false
	}
	for ctx.Err() == nil {
		if n := count(); n >= 0 && n <= before-rows {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the composer never lost the %d rows the operator cut", rows)
	return false
}

// onTmux wraps c's tmux command factory so hook sees each tmux command's
// arguments as the command is made, which is just before it runs. hook acts on
// the pane through the factory it is handed, which is not wrapped.
func onTmux(c *Client, hook func(args []string, tmux CommandFactory)) {
	inner := c.cmdFunc
	c.cmdFunc = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name == "tmux" {
			hook(args, inner)
		}
		return inner(ctx, name, args...)
	}
}

// operatorTypes types text into pane as an operator at the keyboard would,
// after C-u when clearFirst is set, and waits until the box ends with it.
// Waiting for the box merely to contain text returns at once when the box
// already holds it, as bossd's payload holds its own first word, and bossd
// then reads the box before the operator's keys have landed.
func operatorTypes(t *testing.T, tmux CommandFactory, pane string, clearFirst bool, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if clearFirst {
		if out, err := tmux(ctx, "tmux", "send-keys", "-t", pane, "C-u").CombinedOutput(); err != nil {
			t.Errorf("operator C-u: %v: %s", err, out)
			return
		}
	}
	if out, err := tmux(ctx, "tmux", "send-keys", "-t", pane, "-l", "--", text).CombinedOutput(); err != nil {
		t.Errorf("operator typing: %v: %s", err, out)
		return
	}
	var rows string
	var readErr error
	for ctx.Err() == nil {
		out, err := tmux(ctx, "tmux", "capture-pane", "-p", "-t", pane).Output()
		if readErr = err; err == nil {
			if rows = composerRows(string(out)); strings.HasSuffix(withoutSpace(rows), withoutSpace(text)) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the composer never drew the operator's %q; it showed %q (last capture error: %v)", text, rows, readErr)
}

// An operator who replaces bossd's text with a paste of their own while the
// send waits on its retry Enter keeps the paste. The withdrawal takes a
// placeholder only for a paste Claude collapses, and only the one its own
// paste drew, paste id included, so an operator's placeholder is never read as
// a short payload, a typed command, or a paste of any length.
func TestWithdrawalLeavesAnOperatorsPasteAlone(t *testing.T) {
	sendPlan := func(payload string) func(context.Context, *Client, string) error {
		return func(ctx context.Context, c *Client, pane string) error {
			return c.sendPlan(ctx, pane, payload, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
		}
	}
	for name, tc := range map[string]struct {
		send      func(context.Context, *Client, string) error
		operators string
	}{
		"short pasted message":               {sendPlan("summarise the repo"), "[Pasted text #2]"},
		"long pasted message":                {sendPlan("line one\nline two\nline three\nline four"), "[Pasted text #5 +5 lines]"},
		"long pasted message, as many lines": {sendPlan("line one\nline two\nline three\nline four"), "[Pasted text #5 +3 lines]"},
		"long single line":                   {sendPlan(strings.Repeat("a long single line ", 50)), "[Pasted text #7]"},
		"typed slash command": {func(ctx context.Context, c *Client, pane string) error {
			return c.sendLine(ctx, pane, bareSlashCommand, c.sendDeliveryOpts(sendPlanReadyMarker, true, nil))
		}, "[Pasted text #2]"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposerSwallowing(ctx, t, 0, 2)
			enters := 0
			onTmux(c, func(args []string, tmux CommandFactory) {
				if enters == 2 {
					enters++
					operatorTypes(t, tmux, pane, true, tc.operators)
				}
				if slices.Contains(args, "send-keys") && slices.Contains(args, "Enter") {
					enters++
				}
			})

			err := tc.send(ctx, c, pane)
			if OutcomeOf(err) != OutcomeNotSubmitted {
				t.Fatalf("outcome = %v (%v), want %v while the composer drops Enter", OutcomeOf(err), err, OutcomeNotSubmitted)
			}
			screen, err := c.CapturePane(ctx, pane)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if rows := composerRows(screen); !composerHoldsExactly(rows, tc.operators) {
				t.Fatalf("the composer shows %q after the send, want the operator's %q untouched", rows, tc.operators)
			}
			if ran := ranLines(ctx, t, c, pane); len(ran) != 0 {
				t.Fatalf("the agent ran %q, want nothing", ran)
			}
		})
	}
}

// A paste Claude collapses is withdrawn by its placeholder, so the retry finds
// an empty composer.
func TestNotSubmittedCollapsedPasteIsWithdrawn(t *testing.T) {
	for name, payload := range map[string]string{
		"four lines":       "line one\nline two\nline three\nline four",
		"long single line": strings.Repeat("a long single line ", 50),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposerSwallowing(ctx, t, 0, 2)
			opts := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)

			err := c.sendPlan(ctx, pane, payload, opts)
			if OutcomeOf(err) != OutcomeNotSubmitted || !errors.Is(err, withdrawn) {
				t.Fatalf("first send: %v (outcome %v), want %v with the payload withdrawn while the composer drops Enter", err, OutcomeOf(err), OutcomeNotSubmitted)
			}
			styled, err := c.captureVisibleStyled(ctx, pane)
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			if composerHoldsDraft(styled) {
				t.Fatalf("the not-submitted send left %q in the composer, want it empty", composerRows(plainText(styled)))
			}
			if err := c.sendPlan(ctx, pane, "REPEAT DELIVERY, attempt 2:\nline one\nline two\nline three", opts); err != nil {
				t.Fatalf("retry: %v (outcome %v); ran %q", err, OutcomeOf(err), ranLines(ctx, t, c, pane))
			}
			if ran := ranLines(ctx, t, c, pane); len(ran) != 1 {
				t.Fatalf("the agent ran %q, want the retry once", ran)
			}
		})
	}
}

// withdrawalPayloads are payloads Claude draws as text that C-u empties in
// different numbers of presses: one per wrapped row and one per newline.
var withdrawalPayloads = map[string]string{
	"two lines":    "line one of the message\nline two of the message",
	"wrapped line": strings.Repeat("withdraw the wrapped payload ", 7)[:189],
	"three lines":  "line one\nline two\nline three",
	"short line":   "summarise the repo",
}

// wrapShapes are payloads whose rows depend on where Claude wraps, at the slow
// composer's width of slowComposerColumns less 4, in display cells.
var wrapShapes = map[string]string{
	"wide characters wrapped": strings.Repeat("修复构建错误", 30),
	"780 characters":          strings.Repeat("abcdefghi ", 78),
	"two wrapped lines":       strings.Repeat("first line words ", 9) + "\n" + strings.Repeat("second line words ", 9),
	"exactly the width":       strings.Repeat("x", claudeComposerWrapWidth(slowComposerColumns)),
	"one past the width":      strings.Repeat("y", claudeComposerWrapWidth(slowComposerColumns)+1),
	"words to the width":      strings.Repeat("abc ", claudeComposerWrapWidth(slowComposerColumns)/4) + " tail",
	"blank middle line":       "line one\n\nline three",
	"trailing newline":        "line one\nline two\n",
	"wrapped then a line":     strings.Repeat("wrap me please ", 12) + "\nshort",
	"a line then the width":   "line one\n" + strings.Repeat("z", claudeComposerWrapWidth(slowComposerColumns)),
}

// A not-submitted send of any shape Claude draws as text ends with the
// composer empty, whether the Enter retry's presses emptied it or left part
// of it for the withdrawal, and the retry that follows runs once.
func TestNotSubmittedSendOfEveryShapeEndsWithAnEmptyComposer(t *testing.T) {
	shapes := maps.Clone(wrapShapes)
	maps.Copy(shapes, withdrawalPayloads)
	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			parallelSlowComposer(t)
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposerSwallowing(ctx, t, 0, 2)
			opts := c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)

			err := c.sendPlan(ctx, pane, payload, opts)
			styled, capErr := c.captureVisibleStyled(ctx, pane)
			if capErr != nil {
				t.Fatalf("capture: %v", capErr)
			}
			t.Logf("%s: %v", name, err)
			if OutcomeOf(err) != OutcomeNotSubmitted || !errors.Is(err, withdrawn) || composerHoldsDraft(styled) {
				t.Fatalf("the send ended %v (outcome %v), leaving %q; want it not submitted and withdrawn", err, OutcomeOf(err), composerRows(plainText(styled)))
			}
			const retry = "REPEAT DELIVERY, attempt 2"
			if err := c.sendPlan(ctx, pane, retry, opts); err != nil {
				t.Fatalf("retry: %v (outcome %v); ran %q", err, OutcomeOf(err), ranLines(ctx, t, c, pane))
			}
			if ran := ranLines(ctx, t, c, pane); !slices.Equal(ran, []string{retry}) {
				t.Fatalf("the agent ran %q, want exactly the retry once", ran)
			}
		})
	}
}

// fillComposer pastes payload into the slow composer's empty box and waits
// until the box holds exactly it.
func fillComposer(ctx context.Context, t *testing.T, c *Client, pane, payload string) {
	t.Helper()
	if err := c.pasteBufferNoEnter(ctx, pane, payload); err != nil {
		t.Fatalf("paste the payload: %v", err)
	}
	var styled string
	for range 100 {
		var err error
		if styled, err = c.captureVisibleStyled(ctx, pane); err == nil && composerHoldsOnlyPayload(styled, composerDrawing{text: payload}) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the composer never held exactly %q; it showed %q", payload, composerRows(plainText(styled)))
}

// Claude Code 2.1.302's ctrl+u kills a newline before the cursor on its own,
// or else back to the start of the cursor's wrapped row, so a payload of
// several rows takes several presses. The withdrawal presses until the box is
// empty, and nothing runs.
func TestWithdrawalEmptiesAPayloadOfSeveralRows(t *testing.T) {
	shapes := maps.Clone(wrapShapes)
	maps.Copy(shapes, withdrawalPayloads)
	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			parallelSlowComposer(t)
			ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
			defer cancel()
			c, pane := startSlowComposer(ctx, t, 0)
			fillComposer(ctx, t, c, pane, payload)
			presses := 0
			onTmux(c, func(args []string, _ CommandFactory) {
				if slices.Contains(args, "send-keys") && slices.Contains(args, "C-u") {
					presses++
				}
			})

			err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
			styled, capErr := c.captureVisibleStyled(ctx, pane)
			if capErr != nil {
				t.Fatalf("capture: %v", capErr)
			}
			t.Logf("%s: %d presses; %v", name, presses, err)
			if !errors.Is(err, withdrawn) || composerHoldsDraft(styled) {
				t.Fatalf("after %d presses the withdrawal ended %v, leaving %q in the composer; want it withdrawn", presses, err, composerRows(plainText(styled)))
			}
			if ran := ranLines(ctx, t, c, pane); len(ran) != 0 {
				t.Fatalf("the agent ran %q, want nothing", ran)
			}
		})
	}
}

// The fake agent's C-u is Claude Code 2.1.302's Cursor.deleteToLineStart with
// the cursor at the end of the text.
func TestSlowComposerClearsAsClaudeDoes(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		steps []string
	}{
		{"A\nB", 80, []string{"A\n", "A", ""}},
		{"aaaa bbbb cccc", 10, []string{"aaaa bbbb ", ""}},
		{"abcdefghijklmno", 10, []string{"abcdefghij", ""}},
	} {
		text := tc.text
		for _, want := range tc.steps {
			if text = deleteToLineStart(text, tc.width); text != want {
				t.Fatalf("C-u on %q at width %d left %q, want %q", tc.text, tc.width, text, want)
			}
		}
	}
}

// The fake agent wraps as Claude Code 2.1.302 does, with wrap-ansi's hard
// wrap and no trim. Each want is what wrap-ansi 9.0.2 returns.
func TestSlowComposerWrapsAsClaudeDoes(t *testing.T) {
	if got := claudeComposerWrapWidth(120); got != 116 {
		t.Errorf("a 120-column pane wraps at %d, want 116", got)
	}
	for _, tc := range []struct {
		line  string
		width int
		want  []string
	}{
		{"aaaa bbbb cccc", 10, []string{"aaaa bbbb ", "cccc"}},
		{"aaaaaaaaaa bb", 10, []string{"aaaaaaaaaa", " bb"}},
		{"xxxxxxxxxx", 10, []string{"xxxxxxxxxx"}},
		{"abcdefghijklmno", 10, []string{"abcdefghij", "klmno"}},
		{"ab cdefghijklmnopqrstu v", 10, []string{"ab ", "cdefghijkl", "mnopqrstu ", "v"}},
		{"修复构建错误", 10, []string{"修复构建错", "误"}},
		{"ab 修复构建错误", 10, []string{"ab 修复构", "建错误"}},
		{"修复 构建", 4, []string{"修复", " ", "构建"}},
	} {
		if got := wrapClaudeComposerLine(tc.line, tc.width); !slices.Equal(got, tc.want) {
			t.Errorf("%q at width %d wraps to %q, want %q", tc.line, tc.width, got, tc.want)
		}
	}
}

// Text an operator types while the withdrawal runs survives whenever it lands
// before one of the withdrawal's reads: each read either sees it and stops, or
// comes after a C-u that has already removed the payload. That holds for words
// added to the box and for a box the operator clears and starts again with the
// payload's own first word, which no press leaves. The sweep places the typing
// before the first read, then before the first read after each press a clean
// withdrawal makes, in turn, for payloads that take one press and several. It
// counts presses rather than reads: a read that comes before the composer has
// redrawn a press is followed by another, so how many reads a withdrawal takes
// depends on how fast the composer draws.
func TestWithdrawalRereadsTheComposerBeforeEveryPress(t *testing.T) {
	for name, payload := range withdrawalPayloads {
		for action, op := range map[string]struct {
			clearFirst bool
			words      string
		}{
			"words added":                {false, " and my own words"},
			"cleared, then its own word": {true, strings.Fields(payload)[0]},
		} {
			t.Run(name+", "+action, func(t *testing.T) {
				withdraw := func(t *testing.T, typeAfter int) (presses int, typed bool, rows string, err error) {
					t.Helper()
					ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
					defer cancel()
					c, pane := startSlowComposer(ctx, t, 0)
					fillComposer(ctx, t, c, pane, payload)
					done := false
					onTmux(c, func(args []string, tmux CommandFactory) {
						switch {
						case done:
						case slices.Contains(args, "send-keys") && slices.Contains(args, "C-u"):
							presses++
						case slices.Contains(args, "capture-pane") && presses == typeAfter && !typed:
							typed = true
							operatorTypes(t, tmux, pane, op.clearFirst, op.words)
						}
					})
					err = c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
					done = true
					if OutcomeOf(err) != OutcomeNotSubmitted {
						t.Fatalf("err = %v (outcome %v); want the not-submitted verdict kept", err, OutcomeOf(err))
					}
					screen, capErr := c.CapturePane(ctx, pane)
					if capErr != nil {
						t.Fatalf("capture: %v", capErr)
					}
					return presses, typed, composerRows(screen), err
				}

				clean, _, rows, err := withdraw(t, -1)
				if !errors.Is(err, withdrawn) {
					t.Fatalf("a clean withdrawal ended %v, leaving %q", err, rows)
				}
				for after := 0; after <= clean; after++ {
					t.Run(fmt.Sprintf("typed after press %d of %d", after, clean), func(t *testing.T) {
						parallelSlowComposer(t)
						presses, typed, rows, err := withdraw(t, after)
						if !typed {
							t.Fatalf("the withdrawal ended %v after %d presses, before the operator's moment after press %d", err, presses, after)
						}
						if !strings.HasSuffix(withoutSpace(rows), withoutSpace(op.words)) {
							t.Fatalf("the composer shows %q after the withdrawal; the operator's %q is gone", rows, op.words)
						}
					})
				}
			})
		}
	}
}

// An operator who types the payload's next words after a press leaves the box
// holding text that is still a prefix of the payload, but bigger than before
// the press. A press can only shrink the box, so the withdrawal stops there
// rather than erase what the operator typed.
func TestWithdrawalStopsWhenTheBoxGrows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposer(ctx, t, 0)
	const payload = "line one of the message\nline two of the message"
	const words = "line two"
	fillComposer(ctx, t, c, pane, payload)
	presses, typed := 0, false
	onTmux(c, func(args []string, tmux CommandFactory) {
		switch {
		case slices.Contains(args, "send-keys") && slices.Contains(args, "C-u"):
			presses++
		case slices.Contains(args, "capture-pane") && presses == 2 && !typed:
			typed = true
			operatorTypes(t, tmux, pane, false, words)
		}
	})

	err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
	screen, capErr := c.CapturePane(ctx, pane)
	if capErr != nil {
		t.Fatalf("capture: %v", capErr)
	}
	if rows := composerRows(screen); !errors.Is(err, withdrawForeign) || !strings.Contains(withoutSpace(rows), withoutSpace(words)) {
		t.Fatalf("the withdrawal ended %v after %d presses, leaving %q; want it stopped with the operator's %q kept", err, presses, rows, words)
	}
}

// A frame read just after a C-u can still show the box as it was before the
// press. The withdrawal waits for the press to show rather than taking that
// stale frame for a press that changed nothing.
func TestWithdrawalWaitsOutAStaleFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposerWith(ctx, t, slowComposer{clearLag: 150 * time.Millisecond})
	const payload = "summarise the repo"
	fillComposer(ctx, t, c, pane, payload)

	err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
	styled, capErr := c.captureVisibleStyled(ctx, pane)
	if capErr != nil {
		t.Fatalf("capture: %v", capErr)
	}
	if !errors.Is(err, withdrawn) || composerHoldsDraft(styled) {
		t.Fatalf("the withdrawal ended %v, leaving %q in the composer; want it withdrawn", err, composerRows(plainText(styled)))
	}
}

// A Codex composer has no rules, and a blank row ends it as it ends a draft's
// paragraph, so a box holding the payload, a blank row and an operator's
// paragraph is not read as the payload. The withdrawal presses nothing.
func TestWithdrawalPressesNothingInARulelessComposer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	skipIfNoTmux(t)
	const payload = "summarise the repo"
	frame := "\x1b[1m›\x1b[0m " + payload + "\r\n\r\n  my second paragraph the operator typed\r\n\r\n  \x1b[38;2;246;226;183mGPT-6.1-Sol default\x1b[39m\r\n"
	c, _ := newTestTmuxClient(t)
	pane := uniqueSessionName("boss-codex-frame")
	if err := c.NewSession(ctx, NewSessionOpts{
		Name:    pane,
		WorkDir: t.TempDir(),
		Command: []string{"sh", "-c", `printf '%b' "$0"; exec sleep 600`, strings.ReplaceAll(frame, "\x1b", `\033`)},
		Width:   120,
		Height:  30,
	}); err != nil {
		t.Fatalf("new session: %v", err)
	}
	var drawn string
	for range 100 {
		styled, err := c.captureVisibleStyled(ctx, pane)
		if err == nil && strings.Contains(styled, "GPT-6.1-Sol default") {
			drawn = styled
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if drawn == "" {
		t.Fatal("the pane never drew the Codex frame")
	}
	var keys [][]string
	onTmux(c, func(args []string, _ CommandFactory) {
		if slices.Contains(args, "send-keys") {
			keys = append(keys, args)
		}
	})

	err := c.withdrawUnsubmitted(ctx, pane, composerDrawing{text: payload}, classifySubmit(OutcomeNotSubmitted, errSubmissionPending))
	if OutcomeOf(err) != OutcomeNotSubmitted || !errors.Is(err, withdrawUnruled) {
		t.Fatalf("err = %v (outcome %v); want the not-submitted verdict kept, saying the box has no rules to read", err, OutcomeOf(err))
	}
	if len(keys) != 0 {
		t.Fatalf("the withdrawal pressed %q in a rule-less composer holding an operator's paragraph", keys)
	}
}

// Claude draws an argument hint after a slash command that ends in a space,
// so the line is typed trimmed or the box never holds exactly what was typed.
func TestTypedLineWithATrailingSpaceRunsTheCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), integrationTestTimeout)
	defer cancel()
	c, pane := startSlowComposer(ctx, t, 0)

	if err := c.sendLine(ctx, pane, bareSlashCommand+" ", c.sendDeliveryOpts(sendPlanReadyMarker, true, nil)); err != nil {
		t.Fatalf("sendLine: %v", err)
	}
	if ran := ranLines(ctx, t, c, pane); !slices.Equal(ran, []string{bareSlashCommand}) {
		t.Fatalf("the agent ran %q, want exactly %q once", ran, bareSlashCommand)
	}
}

// A ruled box whose first row carries no prompt marker is not the composer
// row: Claude's shell mode draws "!" there. The composer row is then the
// bottom-most marker row, and this real pane has none.
func TestComposerMarkerIdxSkipsARuledBoxWithoutAPromptMarker(t *testing.T) {
	shellMode := strings.Split(readPaneFixture(t, "claude_typed_shell_mode_real.txt"), "\n")
	if top, _, ok := inputBoxRules(shellMode); !ok || strings.TrimSpace(shellMode[top+1]) != "! echo hi" {
		t.Fatal("fixture lost its shell-mode box; recapture with capture_typed_shell_mode.sh")
	}
	if got := composerMarkerIdx(shellMode); got != -1 {
		t.Errorf("composerMarkerIdx on the shell-mode pane = %d (%q), want -1", got, shellMode[got])
	}

	echoed := []string{"❯ earlier prompt", "⏺ reply", "────────", "  some other content", "────────", "  footer"}
	if got := composerMarkerIdx(echoed); got != 0 {
		t.Errorf("composerMarkerIdx = %d, want 0, the bottom-most marker row", got)
	}
}
