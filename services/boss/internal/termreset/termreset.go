// Package termreset owns the xterm mouse-tracking DECRST disable sequence and
// the writers that emit it.
//
// A foreign full-screen child proxied through boss's PTY (tmux with
// `set -g mouse on`, or Claude Code's own mouse mode) enables mouse reporting
// on the user's real terminal but never emits the matching reset when boss
// abandons the proxy. The terminal is then stranded in mouse-reporting mode and
// every idle pointer movement is echoed as garbage text like
// "35;39;28M0;17;5m". BOS-499 fixed the clean-teardown case; BOS-650 reuses the
// same byte sequence from the CLI layer (startup self-heal, SIGHUP rescue, and
// `boss fix-terminal`) without dragging in the PTY process manager.
package termreset

import (
	"io"
	"os"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

const (
	resetModeMouseUTF8  = "\x1b[?1005l" // no ansi constant: legacy UTF-8 mouse encoding
	resetModeMouseUrxvt = "\x1b[?1015l" // no ansi constant: urxvt mouse encoding
)

// MouseReset is the DECRST sequence that disables every xterm mouse-reporting
// mode boss might have to clean up after.
//
// The core set (?1000/?1002/?1003/?1006) covers what tmux `mouse on` enables;
// the defensive extras (?1005 legacy UTF-8, ?1015 urxvt, ?9 X10) cover exotic
// terminal/tmux encodings. DECRST on a mode that was never set is a harmless
// no-op, so the blanket reset is safe — it is what tmux, vim, less and Bubble
// Tea all do.
const MouseReset = "" +
	// Core set first.
	ansi.ResetModeMouseNormal + // ?1000l
	ansi.ResetModeMouseButtonEvent + // ?1002l
	ansi.ResetModeMouseAnyEvent + // ?1003l
	ansi.ResetModeMouseExtSgr + // ?1006l
	// Defensive extras.
	resetModeMouseUTF8 + // ?1005l
	resetModeMouseUrxvt + // ?1015l
	ansi.ResetModeMouseX10 // ?9l

// MouseEnable is the DECSET sequence that puts a terminal back into the
// mouse-reporting mode a proxied tmux expects, undoing MouseReset.
//
// It exists because the teardown reset and the PTY manager's process reuse pull
// in opposite directions. A detach writes MouseReset to the real terminal (the
// TUI it returns to does not want mouse reporting on), but Process.Detach only
// drops the output writer — the `tmux attach` CLIENT STAYS ALIVE. On re-attach
// the manager hands back that same client, which has no reason to re-run its
// terminal init and whose cached mode still says reporting is on, so nothing
// re-sends the DECSET. The terminal is then left with reporting off while tmux
// believes it is on, and NO wheel event is generated for the whole life of that
// attach: scrolling is dead until the client is killed and respawned. Confirmed
// by instrumenting the raw stdin read — zero mouse reports across a frozen
// attach while keystrokes flowed normally.
//
// Deliberately NARROWER than MouseReset, which is not its exact inverse.
// MouseReset is a blanket clear covering exotic encodings (?1005, ?1015, ?9) and
// ?1003 any-event motion, because clearing a mode that was never set is free.
// Setting one is not: re-enabling everything would hand the terminal a motion
// reporting mode the child never asked for, so every idle pointer movement would
// cross the PTY. This restores only what tmux `mouse on` negotiates in practice —
// ?1000 normal, ?1002 button-event, ?1006 SGR encoding — which is exactly what a
// healthy pane is observed running.
const MouseEnable = ansi.SetModeMouseNormal + // ?1000h
	ansi.SetModeMouseButtonEvent + // ?1002h
	ansi.SetModeMouseExtSgr // ?1006h

// WriteMouseEnable writes MouseEnable to w. Best-effort: write errors are
// ignored, matching the other terminal mode writes.
func WriteMouseEnable(w io.Writer) {
	writeSequence(w, MouseEnable)
}

// resetKittyKeyboard disables the Kitty keyboard protocol. It is a raw literal
// because ansi.KittyKeyboard is a function, not a constant, and this set has to
// stay const-foldable; TestAbnormalExitResetMatchesBubbleTeaTeardown pins it to
// ansi.KittyKeyboard(0, 1), exactly what Bubble Tea's renderer close() emits.
const resetKittyKeyboard = "\x1b[=0;1u"

// AbnormalExitReset is every *input-reporting* mode a boss process can strand
// on the outer terminal when it dies without running its teardown: the
// mouse-reporting set plus the modes Bubble Tea's renderer turns on and only
// undoes in its clean close(), which a SIGHUP skips like every other deferred
// cleanup. It is what the CLI layer writes — startup self-heal, SIGHUP rescue,
// and `boss fix-terminal`. Each addition mirrors an input-mode write in that
// close():
//
//   - ?1004l  focus reporting. Boss's own views enable it (BOS-459, see
//     internal/views/app.go ReportFocus). Stranded, the terminal sends ESC[I on
//     focus-in and ESC[O on focus-out, which a shell prompt renders as a literal
//     "I"/"O" typed into the command line — the likeliest form in the reported
//     "walked away from the session" scenario.
//   - ?2004l  bracketed paste. Stranded, every paste is wrapped in a literal
//     "200~"…"201~".
//   - ESC[>4m and ESC[=0;1u  modifyOtherKeys and the Kitty keyboard protocol,
//     which the renderer enables unconditionally on start. Stranded, keypresses
//     arrive as CSI-u escape codes in kitty/ghostty/foot/WezTerm.
//
// Deliberately NOT folded into MouseReset. Bubble Tea owns these modes across a
// tea.Exec window — its renderer close() disables them before the PTY proxy
// runs and start() re-asserts them when tea.Exec calls RestoreTerminal — so the
// PTY teardown's job is only the modes the *foreign pane* enabled, which is the
// mouse set. Writing the wider set before the TUI starts is likewise safe:
// renderer start() re-asserts each of these unconditionally.
//
// close() also exits the alternate screen and shows the text cursor. Those are
// deliberately NOT here: they are screen state rather than input reporting, they
// inject no characters, and switching a user's screen buffer out from under them
// from an escape hatch that may be run at any moment is a bigger behavioural
// claim than clearing an input mode. Recorded in the BOS-650 plan's Risks.
const AbnormalExitReset = MouseReset +
	ansi.ResetModeFocusEvent + // ?1004l
	ansi.ResetModeBracketedPaste + // ?2004l
	ansi.ResetModifyOtherKeys + // ESC[>4m
	resetKittyKeyboard // ESC[=0;1u

// WriteMouseReset writes MouseReset to w. Best-effort: write errors are
// ignored, matching the other terminal teardown writes.
//
// This is the narrow, mouse-only set the PTY teardown needs. Callers cleaning
// up after an abnormal exit want WriteAbnormalExitReset instead.
func WriteMouseReset(w io.Writer) {
	writeSequence(w, MouseReset)
}

// WriteAbnormalExitReset writes AbnormalExitReset to w. Best-effort, as above.
func WriteAbnormalExitReset(w io.Writer) {
	writeSequence(w, AbnormalExitReset)
}

// WriteMouseResetIfTerminal writes MouseReset to f only when f is a real
// terminal, returning whether it wrote. The gate is what keeps a redirected or
// piped TUI launch (`boss > boss.log`, `boss | tee`) from being corrupted with
// control bytes. Note the deliberate asymmetry: `boss fix-terminal` uses the
// *ungated* writer, because `ssh <host> boss fix-terminal` hands the remote
// process a pipe and gating there would make the documented remedy a no-op.
func WriteMouseResetIfTerminal(f *os.File) bool {
	return writeSequenceIfTerminal(f, MouseReset)
}

// WriteAbnormalExitResetIfTerminal writes AbnormalExitReset to f only when f is
// a real terminal, returning whether it wrote. Same TTY gate as
// WriteMouseResetIfTerminal.
func WriteAbnormalExitResetIfTerminal(f *os.File) bool {
	return writeSequenceIfTerminal(f, AbnormalExitReset)
}

func writeSequence(w io.Writer, seq string) {
	_, _ = io.WriteString(w, seq)
}

func writeSequenceIfTerminal(f *os.File, seq string) bool {
	if f == nil {
		return false
	}
	if !term.IsTerminal(int(f.Fd())) {
		return false
	}
	writeSequence(f, seq)
	return true
}
