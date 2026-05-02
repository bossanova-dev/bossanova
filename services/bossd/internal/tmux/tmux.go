// Package tmux provides a wrapper for tmux CLI operations.
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Default timing knobs for SendPlan. Tests bypass these by passing a
// custom sendPlanOpts to the unexported sendPlan helper, so the production
// 5s deadline never blocks a fast in-test stub.
//
// sendPlanReadyMarker is the prompt indicator Claude Code renders inside
// its input box once the TUI is ready to accept input. We intentionally
// match on the prompt character itself rather than the default footer
// ("? for shortcuts"): users with a custom statusline replace that footer
// entirely, but the input-box prompt is part of Claude's core rendering
// and survives statusline customisation.
const (
	sendPlanDefaultDeadline     = 5 * time.Second
	sendPlanDefaultPollInterval = 100 * time.Millisecond
	sendPlanReadyMarker         = "❯"
)

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Client wraps tmux CLI operations.
type Client struct {
	cmdFunc CommandFactory
}

// Option configures a Client.
type Option func(*Client)

// WithCommandFactory sets a custom CommandFactory for testing.
func WithCommandFactory(f CommandFactory) Option {
	return func(c *Client) {
		c.cmdFunc = f
	}
}

// NewClient creates a new tmux Client.
func NewClient(opts ...Option) *Client {
	c := &Client{
		cmdFunc: exec.CommandContext,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Available checks if tmux is installed and available.
func (c *Client) Available(ctx context.Context) bool {
	cmd := c.cmdFunc(ctx, "tmux", "-V")
	return cmd.Run() == nil
}

// NewSessionOpts configures a new tmux session.
type NewSessionOpts struct {
	Name    string   // Session name (required)
	WorkDir string   // Working directory (required)
	Command []string // Command to run in session (required)
	Width   int      // Initial width (defaults to 200)
	Height  int      // Initial height (defaults to 50)
}

// NewSession creates a new detached tmux session.
// Returns error if session creation fails.
func (c *Client) NewSession(ctx context.Context, opts NewSessionOpts) error {
	if opts.Name == "" {
		return fmt.Errorf("session name is required")
	}
	if opts.WorkDir == "" {
		return fmt.Errorf("work directory is required")
	}
	if len(opts.Command) == 0 {
		return fmt.Errorf("command is required")
	}

	width := opts.Width
	if width == 0 {
		width = 200
	}
	height := opts.Height
	if height == 0 {
		height = 50
	}

	args := make([]string, 0, 10+len(opts.Command))
	args = append(args,
		"new-session",
		"-d",            // Detached
		"-s", opts.Name, // Session name
		"-c", opts.WorkDir, // Working directory
		"-x", strconv.Itoa(width), // Width
		"-y", strconv.Itoa(height), // Height
	)
	args = append(args, opts.Command...)

	cmd := c.cmdFunc(ctx, "tmux", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("failed to create tmux session %q: %w (stderr: %s)", opts.Name, err, msg)
		}
		return fmt.Errorf("failed to create tmux session %q: %w", opts.Name, err)
	}

	// Bind Ctrl+X and Ctrl+] as additional detach keys scoped to this session.
	c.bindDetachKeys(ctx, opts.Name)

	return nil
}

// bindDetachKeys configures Ctrl+X and Ctrl+] as quick detach keys for
// boss-managed sessions. Uses conditional bindings in the root key table
// (via tmux's #{m:} format match) so the keys only detach in sessions whose
// name starts with "boss-"; in all other sessions the keystroke passes
// through normally. This avoids replacing the root table with a custom
// key-table, which would break mouse scrollback and extended-key forwarding.
// Errors are ignored — the standard Ctrl+b d detach still works as a fallback.
func (c *Client) bindDetachKeys(ctx context.Context, sessionName string) {
	// Bind Ctrl+X and Ctrl+] in the root table with a conditional: detach
	// only when the session name matches "boss-*", otherwise send the key
	// through to the pane. The -F flag evaluates the format without spawning
	// a shell, so there is no latency.
	for _, key := range []string{"C-x", "C-]"} {
		cond := `#{m:boss-*,#{session_name}}`
		cmd := c.cmdFunc(ctx, "tmux", "bind-key", "-T", "root", key,
			"if", "-F", cond, "detach-client",
			fmt.Sprintf(`"send-keys %s"`, key))
		_ = cmd.Run()
	}

	// Enable extended-keys in "always" mode so modifier+key combos (e.g.
	// Shift+Enter for multiline input in Claude Code) are forwarded to the
	// application unconditionally. The default "on" mode only forwards
	// extended keys to panes that explicitly request them via the kitty
	// keyboard protocol activation sequence, which Claude Code does not
	// send. "always" bypasses that requirement.
	cmd := c.cmdFunc(ctx, "tmux", "set-option", "-t", sessionName,
		"extended-keys", "always")
	_ = cmd.Run()

	// Append extkeys to terminal-features so tmux negotiates extended key
	// support with the outer terminal (e.g. Ghostty, iTerm2, kitty).
	// Using -a (append) avoids overwriting other terminal-features.
	cmd = c.cmdFunc(ctx, "tmux", "set-option", "-sa",
		"terminal-features", ",xterm*:extkeys")
	_ = cmd.Run()

	// Set extended-keys-format to csi-u so Claude Code receives CSI-u
	// sequences (e.g. \x1b[13;2u for Shift+Enter) instead of xterm format.
	// Claude Code's input handling expects CSI-u encoding for modifier keys.
	cmd = c.cmdFunc(ctx, "tmux", "set-option", "-g",
		"extended-keys-format", "csi-u")
	_ = cmd.Run()

	// Enable mouse mode so trackpad/scroll-wheel scrolling works like a
	// normal terminal — scrolling up enters copy mode automatically and
	// scrolling back down (or pressing q) exits it.
	cmd = c.cmdFunc(ctx, "tmux", "set-option", "-g",
		"mouse", "on")
	_ = cmd.Run()

	// Forward the outer terminal's TERM_PROGRAM into the tmux session so
	// that applications inside tmux can detect the real terminal emulator
	// (e.g. "ghostty", "iTerm.app") instead of seeing "tmux". Skip if the
	// value is already "tmux" since that's what we're trying to override.
	if tp := os.Getenv("TERM_PROGRAM"); tp != "" && tp != "tmux" {
		cmd = c.cmdFunc(ctx, "tmux", "set-environment", "-t", sessionName,
			"TERM_PROGRAM", tp)
		_ = cmd.Run()
	}
}

// HasSession checks if a tmux session exists.
func (c *Client) HasSession(ctx context.Context, name string) bool {
	cmd := c.cmdFunc(ctx, "tmux", "has-session", "-t", name)
	return cmd.Run() == nil
}

// KillSession kills a tmux session.
// Does not return an error if the session doesn't exist (idempotent).
func (c *Client) KillSession(ctx context.Context, name string) error {
	cmd := c.cmdFunc(ctx, "tmux", "kill-session", "-t", name)
	err := cmd.Run()
	if err != nil {
		// Check if session doesn't exist by trying has-session.
		// If has-session fails, the session is already gone (success).
		if !c.HasSession(ctx, name) {
			return nil
		}
		return fmt.Errorf("failed to kill tmux session %q: %w", name, err)
	}
	return nil
}

// SetAttachOptions configures tmux session-level options that govern multi-client
// attach behavior. Called by the web-tmux-attach feature before spawning a
// `tmux attach` PTY so that the local TUI and N browser tabs can attach
// concurrently with predictable layout semantics.
//
//   - aggressive-resize on: tmux re-evaluates window geometry on every
//     client SIGWINCH/attach/detach. Without this, `window-size smallest`
//     happily shrinks the window when a client reports a smaller size but
//     refuses to grow it again when that client catches up — both clients
//     end up stuck at whatever the historical minimum was, which doesn't
//     match what either of them is currently asking for.
//   - window-size smallest: tmux clamps the window to the smallest connected
//     client's geometry. The earlier `largest` value made the bigger client
//     authoritative, which left smaller clients (the boss TUI alongside a
//     full-screen browser) silently truncated at the bottom. `smallest` keeps
//     every client's content fully visible; larger clients pay a stripe of
//     unused space rather than losing rows.
//
// Idempotent — safe to call on every attach. Returns an error if either
// `tmux set-option` invocation fails.
func (c *Client) SetAttachOptions(ctx context.Context, sessionName string) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	options := [][2]string{
		{"aggressive-resize", "on"},
		{"window-size", "smallest"},
	}
	for _, opt := range options {
		cmd := c.cmdFunc(ctx, "tmux", "set-option", "-t", sessionName, opt[0], opt[1])
		out, err := cmd.CombinedOutput()
		if err != nil {
			trimmed := strings.TrimSpace(string(out))
			if trimmed != "" {
				return fmt.Errorf("tmux set-option %s %s on %q: %s: %w",
					opt[0], opt[1], sessionName, trimmed, err)
			}
			return fmt.Errorf("tmux set-option %s %s on %q: %w",
				opt[0], opt[1], sessionName, err)
		}
	}
	return nil
}

// RefreshClient runs `tmux refresh-client -t <session>` to force tmux to
// redraw all currently-attached clients. The web-tmux-attach feature calls
// this after a per-attach ring buffer overflow forces a RESYNC: dropping
// oldest bytes leaves later viewers with a corrupt frame, but a tmux-driven
// repaint resolves it without needing each attach to negotiate its own
// resync. Idempotent and cheap — safe to fire-and-forget on every overflow.
func (c *Client) RefreshClient(ctx context.Context, sessionName string) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	cmd := c.cmdFunc(ctx, "tmux", "refresh-client", "-t", sessionName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("tmux refresh-client -t %q: %s: %w", sessionName, trimmed, err)
		}
		return fmt.Errorf("tmux refresh-client -t %q: %w", sessionName, err)
	}
	return nil
}

// SessionName generates a tmux session name from repository and session IDs.
// Format: boss-{first8repoID}-{first8sessionID}
func SessionName(repoID, sessionID string) string {
	repoShort := repoID
	if len(repoShort) > 8 {
		repoShort = repoShort[:8]
	}
	sessShort := sessionID
	if len(sessShort) > 8 {
		sessShort = sessShort[:8]
	}
	return fmt.Sprintf("boss-%s-%s", repoShort, sessShort)
}

// ChatSessionName generates a tmux session name unique to a specific chat.
// Format: boss-{first8repoID}-{first8claudeID}
// This ensures each chat within a session gets its own tmux session.
func ChatSessionName(repoID, claudeID string) string {
	repoShort := repoID
	if len(repoShort) > 8 {
		repoShort = repoShort[:8]
	}
	chatShort := claudeID
	if len(chatShort) > 8 {
		chatShort = chatShort[:8]
	}
	return fmt.Sprintf("boss-%s-%s", repoShort, chatShort)
}

// CapturePane captures the content of a tmux pane including scrollback history.
// Uses -S -1000 to capture up to 1000 lines of scrollback so that the Claude
// response marker (⏺) is reliably included for accurate question detection.
// Returns the pane content as a string, or an error if the session doesn't exist.
func (c *Client) CapturePane(ctx context.Context, sessionName string) (string, error) {
	cmd := c.cmdFunc(ctx, "tmux", "capture-pane", "-p", "-S", "-1000", "-t", sessionName)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("capture pane %q: %w", sessionName, err)
	}
	return string(out), nil
}

// sendPlanOpts customizes SendPlan timing. Production callers use the
// no-arg SendPlan; tests inject aggressive timeouts so the 5s deadline
// never gates them.
type sendPlanOpts struct {
	deadline     time.Duration
	pollInterval time.Duration
}

// SendPlan delivers a plan to a Claude tmux session as a bracketed paste.
//
// It first polls capture-pane until Claude Code's input-box prompt
// indicator (❯) appears (or the deadline is hit), then loads the plan
// into a tmux paste buffer via stdin and pastes it with bracketed paste
// enabled (-p) so Claude treats the payload as a paste rather than raw
// keystrokes. Finally it sends Enter to submit.
//
// Returns an error if the marker never appears within the deadline or if
// any of the three tmux subcommands (load-buffer / paste-buffer /
// send-keys) returns non-zero. The 5s deadline matches the existing
// prefillClaudeInput marker wait in the boss attach view.
func (c *Client) SendPlan(ctx context.Context, sessionName, plan string) error {
	return c.sendPlan(ctx, sessionName, plan, sendPlanOpts{
		deadline:     sendPlanDefaultDeadline,
		pollInterval: sendPlanDefaultPollInterval,
	})
}

// sendPlan is the test-injectable variant of SendPlan that accepts custom
// timing. Both production code and tests funnel through here.
func (c *Client) sendPlan(ctx context.Context, sessionName, plan string, opts sendPlanOpts) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	if opts.deadline <= 0 {
		opts.deadline = sendPlanDefaultDeadline
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = sendPlanDefaultPollInterval
	}

	// Step 1: poll capture-pane for the ready marker.
	if err := c.waitForReadyMarker(ctx, sessionName, opts); err != nil {
		return err
	}

	// Step 2: load the plan into tmux's paste buffer via stdin.
	loadCmd := c.cmdFunc(ctx, "tmux", "load-buffer", "-")
	loadCmd.Stdin = strings.NewReader(plan)
	var loadStderr bytes.Buffer
	loadCmd.Stderr = &loadStderr
	if err := loadCmd.Run(); err != nil {
		if msg := strings.TrimSpace(loadStderr.String()); msg != "" {
			return fmt.Errorf("tmux load-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux load-buffer for %q: %w", sessionName, err)
	}

	// Step 3: paste the buffer with -p (bracketed paste, so Claude treats
	// it as a paste not raw keystrokes) and -d (delete buffer afterwards).
	pasteCmd := c.cmdFunc(ctx, "tmux", "paste-buffer", "-d", "-p", "-t", sessionName)
	var pasteStderr bytes.Buffer
	pasteCmd.Stderr = &pasteStderr
	if err := pasteCmd.Run(); err != nil {
		if msg := strings.TrimSpace(pasteStderr.String()); msg != "" {
			return fmt.Errorf("tmux paste-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux paste-buffer for %q: %w", sessionName, err)
	}

	// Step 4: send Enter to submit the prompt.
	enterCmd := c.cmdFunc(ctx, "tmux", "send-keys", "-t", sessionName, "Enter")
	var enterStderr bytes.Buffer
	enterCmd.Stderr = &enterStderr
	if err := enterCmd.Run(); err != nil {
		if msg := strings.TrimSpace(enterStderr.String()); msg != "" {
			return fmt.Errorf("tmux send-keys Enter for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux send-keys Enter for %q: %w", sessionName, err)
	}
	return nil
}

// waitForReadyMarker polls CapturePane until the Claude Code ready marker
// is observed or the deadline elapses. The first poll is immediate so
// already-ready sessions return without sleeping.
//
// On timeout, the error embeds the most recent successful pane capture
// (truncated). This matters for the cron path: the caller kills the
// tmux session as failure cleanup, so without this snapshot the operator
// has no way to see what Claude was actually showing — auth prompt,
// update banner, missing binary, or just slow startup.
func (c *Client) waitForReadyMarker(ctx context.Context, sessionName string, opts sendPlanOpts) error {
	deadline := time.Now().Add(opts.deadline)
	var lastPane string
	for {
		out, err := c.CapturePane(ctx, sessionName)
		if err == nil {
			lastPane = out
			if strings.Contains(out, sendPlanReadyMarker) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready marker %q not seen in pane %q within %s; last pane (truncated): %s",
				sendPlanReadyMarker, sessionName, opts.deadline, truncatePaneForError(lastPane))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for ready marker on %q: %w", sessionName, ctx.Err())
		case <-time.After(opts.pollInterval):
		}
	}
}

// truncatePaneForError trims the pane snapshot embedded in a SendPlan
// timeout error. The raw capture can be ~80 cols × 1000 rows; we want
// enough for diagnosis (the input-box area and any error banner) without
// flooding logs or wrapping past usefulness.
func truncatePaneForError(pane string) string {
	const maxBytes = 800
	pane = strings.TrimSpace(pane)
	if pane == "" {
		return "<empty>"
	}
	if len(pane) <= maxBytes {
		return pane
	}
	return pane[len(pane)-maxBytes:] + " (head truncated)"
}
