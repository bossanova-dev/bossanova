package views

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type notifyRequest struct {
	Title string
	Body  string
	Sound bool
}

// notifyEnv captures the terminal-identifying environment variables used to
// decide whether the controlling terminal can raise a desktop notification
// directly from an escape sequence (OSC 9 / OSC 777). A terminal-emitted
// notification is attributed to the terminal itself, so clicking it focuses
// the terminal — unlike osascript, whose banner opens Script Editor. The
// markers we key on (GHOSTTY_RESOURCES_DIR, ITERM_SESSION_ID, WEZTERM_PANE)
// survive a tmux/ssh wrap, so we can still identify the outer terminal when
// multiplexed and wrap the sequence in tmux's passthrough envelope.
type notifyEnv struct {
	term            string
	termProgram     string
	tmux            string
	ghosttyResource string
	itermSession    string
	weztermPane     string
}

func currentNotifyEnv() notifyEnv {
	return notifyEnv{
		term:            os.Getenv("TERM"),
		termProgram:     os.Getenv("TERM_PROGRAM"),
		tmux:            os.Getenv("TMUX"),
		ghosttyResource: os.Getenv("GHOSTTY_RESOURCES_DIR"),
		itermSession:    os.Getenv("ITERM_SESSION_ID"),
		weztermPane:     os.Getenv("WEZTERM_PANE"),
	}
}

// inTmux reports whether we are running under a tmux server, which intercepts
// OSC notification sequences and only forwards them when they are wrapped in a
// DCS passthrough envelope (and the user has `allow-passthrough` enabled).
func (e notifyEnv) inTmux() bool {
	return e.tmux != "" || e.termProgram == "tmux"
}

// outerTerminal identifies the terminal emulator that will ultimately render a
// desktop notification, looking through a tmux wrap via markers that survive
// it. Returns "" when the terminal is unknown or known not to support OSC
// notifications (e.g. Apple Terminal), in which case callers fall back to the
// OS-level notifier.
func (e notifyEnv) outerTerminal() string {
	switch {
	case e.ghosttyResource != "" || e.termProgram == "ghostty" || e.term == "xterm-ghostty":
		return "ghostty"
	case e.weztermPane != "" || e.termProgram == "WezTerm":
		return "wezterm"
	case e.itermSession != "" || e.termProgram == "iTerm.app":
		return "iterm"
	default:
		return ""
	}
}

// oscNotificationSequence builds the escape sequence that makes the controlling
// terminal raise a desktop notification, plus whether the current terminal
// supports it. Ghostty and WezTerm take rxvt-style OSC 777 (title + body);
// iTerm2 takes iTerm/ConEmu-style OSC 9 (body only, so the title is folded in).
// When multiplexed under tmux the sequence is wrapped in a passthrough envelope.
func oscNotificationSequence(e notifyEnv, req notifyRequest) (string, bool) {
	title := sanitizeOSCField(req.Title)
	body := sanitizeOSCField(req.Body)
	if title == "" && body == "" {
		return "", false
	}

	var seq string
	switch e.outerTerminal() {
	case "ghostty", "wezterm":
		// OSC 777 ; notify ; <title> ; <body> BEL
		seq = fmt.Sprintf("\x1b]777;notify;%s;%s\a", title, body)
	case "iterm":
		// OSC 9 carries only a body; fold the title in. Per the OSC 9 spec the
		// message must not begin with "<number>;" (ConEmu collision) — our
		// titles never do.
		msg := body
		if title != "" {
			msg = title + ": " + body
		}
		seq = fmt.Sprintf("\x1b]9;%s\a", msg)
	default:
		return "", false
	}

	if e.inTmux() {
		seq = wrapTmuxPassthrough(seq)
	}
	return seq, true
}

// wrapTmuxPassthrough wraps a raw escape sequence so a tmux server forwards it
// verbatim to the outer terminal. tmux requires every embedded ESC to be
// doubled and the whole payload delivered inside a DCS `tmux;` … ST envelope.
// Requires `set -g allow-passthrough on` in the user's tmux (default off on
// older tmux); without it the sequence is silently dropped.
func wrapTmuxPassthrough(seq string) string {
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// sanitizeOSCField strips the bytes that would break OSC parsing — the `;`
// field separator, ESC/BEL terminators, and other C0/DEL control characters —
// replacing each with a space so a session title can never truncate or escape
// the notification payload.
func sanitizeOSCField(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == ';' || r == '\x1b' || r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// notifyCmd builds a supported OS notification command, or nil when no
// supported notification utility is available.
//
//nolint:unparam // callers keep the error result so notification builders can report construction failures without an API change.
func notifyCmd(goos string, lookPath func(string) (string, error), req notifyRequest) (*exec.Cmd, error) {
	switch goos {
	case "darwin":
		// osascript ships with every macOS install (/usr/bin/osascript), so OS
		// notifications require no external dependency. We deliberately do NOT
		// use terminal-notifier: it needs a separate Homebrew/gem install, and a
		// stale shim on PATH that fails at runtime (e.g. an rbenv shim for a Ruby
		// version that no longer has the gem) would satisfy lookPath yet exit
		// non-zero, silently shadowing this always-present path. The modern
		// UNUserNotification API is not an option either — it requires a signed
		// .app bundle with a bundle identifier, which a bare CLI binary is not —
		// so osascript is the native notification path for an unsigned tool.
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, escapeForAppleScript(req.Body), escapeForAppleScript(req.Title))
		if req.Sound {
			script += ` sound name "default"`
		}
		// #nosec G204 -- osascript receives a fixed command and escaped AppleScript string; no shell is invoked
		// owner=@recurser review-by=2027-01-22 issue=BOS-459
		return exec.Command("osascript", "-e", script), nil

	case "linux":
		notifySend, err := lookPath("notify-send")
		if err != nil {
			return nil, nil
		}
		// #nosec G204 -- notify-send executable is resolved from PATH; arguments are passed without a shell
		// owner=@recurser review-by=2027-01-22 issue=BOS-459
		return exec.Command(notifySend, req.Title, req.Body), nil

	default:
		return nil, nil
	}
}

// notifyEnvSource is overridable in tests. It supplies the terminal-identifying
// environment used to decide whether to emit an OSC notification.
var notifyEnvSource = currentNotifyEnv

// terminalNotificationSequence returns the OSC notification bytes for the
// current terminal, or ("", false) when the terminal can't render one.
func terminalNotificationSequence(req notifyRequest) (string, bool) {
	return oscNotificationSequence(notifyEnvSource(), req)
}

// writeTerminalSequence writes an out-of-band escape sequence to the controlling
// terminal. It targets /dev/tty (not stdout) so it is independent of the Bubble
// Tea renderer's buffer; the sequence is written in a single call and does not
// touch the screen grid, so it interleaves safely between rendered frames. A
// var so tests can intercept the write instead of touching a real tty.
var writeTerminalSequence = func(seq string) error {
	// #nosec G304 -- fixed path, no user input; the controlling terminal device
	// owner=@recurser review-by=2027-01-22 issue=BOS-459
	f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(seq)
	return err
}

var runNotify = func(req notifyRequest) error {
	// Prefer a terminal-emitted notification (OSC 9/777) when the terminal
	// supports it: the banner is attributed to the terminal, so a click focuses
	// it instead of opening Script Editor. Fall back to the OS-level notifier
	// when the terminal can't render one, or when the tty write fails (e.g. no
	// controlling terminal because output is redirected).
	if seq, ok := terminalNotificationSequence(req); ok {
		if err := writeTerminalSequence(seq); err == nil {
			return nil
		}
	}
	cmd, err := notifyCmd(runtime.GOOS, exec.LookPath, req)
	if err != nil || cmd == nil {
		return err
	}
	return cmd.Run()
}

// notifyForSessions runs notification delivery outside the Bubble Tea update
// loop. OS delivery is best-effort: a missing binary or command error never
// affects session polling.
func notifyForSessions(sessions []*pb.Session) tea.Cmd {
	return func() tea.Msg {
		for _, sess := range sessions {
			if sess == nil {
				continue
			}
			_ = runNotify(notifyRequest{
				Title: "Bossanova",
				Body:  fmt.Sprintf("%s needs your input", notificationSessionName(sess)),
				Sound: true,
			})
		}
		return nil
	}
}

func notificationSessionName(sess *pb.Session) string {
	if title := strings.TrimSpace(sess.GetTitle()); title != "" {
		return title
	}
	if branch := strings.TrimSpace(sess.GetBranchName()); branch != "" {
		return branch
	}
	return sess.GetId()
}
