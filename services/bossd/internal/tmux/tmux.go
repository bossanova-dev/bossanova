// Package tmux provides a wrapper for tmux CLI operations.
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sortedKeys returns the keys of m in sorted order, so callers that emit
// per-key flags (e.g. tmux `-e KEY=VALUE`) produce deterministic argv.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

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
	// Env sets session-environment variables (tmux `new-session -e KEY=VALUE`)
	// so the launched command — and any later window opened in the session —
	// inherits them. Used to mark cron-spawned sessions with BOSS_CRON=true.
	Env map[string]string
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

	args := make([]string, 0, 10+2*len(opts.Env)+len(opts.Command))
	args = append(args,
		"new-session",
		"-d",            // Detached
		"-s", opts.Name, // Session name
		"-c", opts.WorkDir, // Working directory
		"-x", strconv.Itoa(width), // Width
		"-y", strconv.Itoa(height), // Height
	)
	// Set session-environment variables before the command. Keys are sorted
	// so the argv is deterministic (stable logs and tests).
	for _, k := range sortedKeys(opts.Env) {
		args = append(args, "-e", k+"="+opts.Env[k])
	}
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
	exists, _ := c.HasSessionStatus(ctx, name)
	return exists
}

// HasSessionStatus checks if a tmux session exists and reports tmux command
// failures separately from a definite "session missing" result.
func (c *Client) HasSessionStatus(ctx context.Context, name string) (bool, error) {
	cmd := c.cmdFunc(ctx, "tmux", "has-session", "-t", name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "can't find session") || strings.Contains(msg, "no server running") {
			return false, nil
		}
		if msg == "" {
			return false, fmt.Errorf("tmux has-session %q: %w", name, err)
		}
		return false, fmt.Errorf("tmux has-session %q: %w (stderr: %s)", name, err, msg)
	}
	return true, nil
}

// KillSession kills a tmux session.
// Does not return an error if the session doesn't exist (idempotent).
func (c *Client) KillSession(ctx context.Context, name string) error {
	cmd := c.cmdFunc(ctx, "tmux", "kill-session", "-t", name)
	err := cmd.Run()
	if err != nil {
		// Check if session doesn't exist by trying has-session.
		// A definite missing session is already gone (success); command or
		// tmux availability errors must surface so callers do not assume kill
		// succeeded without liveness evidence.
		exists, statusErr := c.HasSessionStatus(ctx, name)
		if statusErr != nil {
			return fmt.Errorf("failed to kill tmux session %q: verify tmux session liveness: %w", name, statusErr)
		}
		if !exists {
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

// PipePane arms `tmux pipe-pane` on the session's primary pane so that
// everything written to the pane is mirrored to logPath via
// `cat >> <quoted-path>`. Used as a non-invasive replacement for the
// previous in-process pipe wrapping (`bash -c "... | tee log"`): tee
// inside a pipe made the agent's stdout a non-TTY, which modern claude
// auto-degrades to headless-print mode and refuses to run. pipe-pane
// taps the pane's output stream from outside the process, so the
// running agent keeps a real PTY on stdout while bossd still gets a
// persistent log on disk.
//
//   - The `-o` flag toggles the pipe: calling pipe-pane twice on an
//     already-piped session would stop the recording, so callers must
//     only invoke this once per session lifetime. Use the matching
//     no-op when re-spawning (NewSession produces a fresh pane, so no
//     toggling concern there).
//   - logPath is shell-quoted (single-quoted, with embedded single
//     quotes escaped) before being interpolated into the pipe command;
//     no untrusted operator input ever reaches this argument today, but
//     bossd's agentLogsDir + agent_session_id+".log" is still a path
//     the daemon owns and quoting it costs nothing.
//   - Append (`cat >>`) rather than truncate (`cat >`) so a re-spawn
//     after a tmux crash doesn't blow away the prior pane history that
//     a previous pipe-pane already captured.
//   - Idempotent at the "did we attach a pipe" level only: if a stale
//     `pipe-pane -o` is already in effect on this session for whatever
//     reason, a second call here will TOGGLE IT OFF. Production callers
//     only invoke this once, right after NewSession, so the freshness
//     of the pane is guaranteed.
//
// Returns an error if `tmux pipe-pane` itself exits non-zero. Best-
// effort use by the caller is fine — losing on-disk capture must not
// abort the chat launch — but the error is surfaced so the caller can
// log it.
func (c *Client) PipePane(ctx context.Context, sessionName, logPath string) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	if logPath == "" {
		return fmt.Errorf("log path is required")
	}
	pipeCmd := "cat >> " + shellSingleQuote(logPath)
	cmd := c.cmdFunc(ctx, "tmux", "pipe-pane", "-o", "-t", sessionName, pipeCmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("tmux pipe-pane -t %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux pipe-pane -t %q: %w", sessionName, err)
	}
	return nil
}

// shellSingleQuote wraps s for inclusion in a single-quoted shell
// string, escaping any embedded single quotes via the canonical
// '\” idiom. Mirrors the helper in lib/bossalib/agentruntime so the
// tmux package stays free of an extra dependency.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
// Format: boss-{first8repoID}-{first8agentSessionID}
// This ensures each chat within a session gets its own tmux session.
func ChatSessionName(repoID, agentSessionID string) string {
	repoShort := repoID
	if len(repoShort) > 8 {
		repoShort = repoShort[:8]
	}
	chatShort := agentSessionID
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
	deadline         time.Duration
	pollInterval     time.Duration
	readyMarker      string
	submitVerifyWait time.Duration
	submitVerifyTick time.Duration
}

// SendPlan delivers a plan to a tmux-hosted agent session as a bracketed paste.
//
// It first polls capture-pane until the agent's input-box prompt
// indicator appears (or the deadline is hit), then loads the plan
// into a tmux paste buffer via stdin and pastes it with bracketed paste
// enabled (-p) so the agent treats the payload as a paste rather than
// raw keystrokes. Finally it sends Enter to submit.
//
// Returns an error if the marker never appears within the deadline or if
// any of the three tmux subcommands (load-buffer / paste-buffer /
// send-keys) returns non-zero. The 5s deadline matches the existing
// prefillClaudeInput marker wait in the boss attach view.
func (c *Client) SendPlan(ctx context.Context, sessionName, plan string) error {
	return c.SendPlanWithReadyMarker(ctx, sessionName, plan, sendPlanReadyMarker)
}

// SendPlanWithReadyMarker is SendPlan with an agent-specific readiness marker.
// Empty readyMarker preserves the legacy Claude marker for old plugins.
//
// Like sendLine, it verifies the payload actually left the prompt after Enter.
// A cron session is headless, so a paste that loads but doesn't execute (the
// failure mode SendLine warns about) would otherwise be reported as a clean
// fire while nothing ran; the verification turns that silent no-op into a
// surfaced error. The check is line-oriented, so it applies to a single-line,
// non-empty payload (e.g. a free-text cron prompt); multi-line plans and the
// empty string are skipped explicitly (see sendPlan), since neither sits as a
// single matchable prompt row.
func (c *Client) SendPlanWithReadyMarker(ctx context.Context, sessionName, plan, readyMarker string) error {
	return c.sendPlan(ctx, sessionName, plan, sendPlanOpts{
		deadline:         sendPlanDefaultDeadline,
		pollInterval:     sendPlanDefaultPollInterval,
		readyMarker:      readyMarker,
		submitVerifyWait: 2 * time.Second,
		submitVerifyTick: 100 * time.Millisecond,
	})
}

// SendLineWithReadyMarker sends a short literal line and submits it with Enter.
// Use this for command invocations where bracketed paste can leave some TUIs
// with the command text loaded but not executed.
func (c *Client) SendLineWithReadyMarker(ctx context.Context, sessionName, line, readyMarker string) error {
	return c.sendLine(ctx, sessionName, line, sendPlanOpts{
		deadline:         sendPlanDefaultDeadline,
		pollInterval:     sendPlanDefaultPollInterval,
		readyMarker:      readyMarker,
		submitVerifyWait: 2 * time.Second,
		submitVerifyTick: 100 * time.Millisecond,
	})
}

// SendMessage delivers text to an existing tmux session via bracketed paste
// (load-buffer → paste-buffer -p → send-keys Enter). It does not poll for
// a ready marker or verify submission; callers that need those guarantees
// should use SendPlan or SendPlanWithReadyMarker instead.
func (c *Client) SendMessage(ctx context.Context, sessionName, text string) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}

	loadCmd := c.cmdFunc(ctx, "tmux", "load-buffer", "-")
	loadCmd.Stdin = strings.NewReader(text)
	var loadStderr bytes.Buffer
	loadCmd.Stderr = &loadStderr
	if err := loadCmd.Run(); err != nil {
		if msg := strings.TrimSpace(loadStderr.String()); msg != "" {
			return fmt.Errorf("tmux load-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux load-buffer for %q: %w", sessionName, err)
	}

	pasteCmd := c.cmdFunc(ctx, "tmux", "paste-buffer", "-d", "-p", "-t", sessionName)
	var pasteStderr bytes.Buffer
	pasteCmd.Stderr = &pasteStderr
	if err := pasteCmd.Run(); err != nil {
		if msg := strings.TrimSpace(pasteStderr.String()); msg != "" {
			return fmt.Errorf("tmux paste-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux paste-buffer for %q: %w", sessionName, err)
	}

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
	if opts.readyMarker == "" {
		opts.readyMarker = sendPlanReadyMarker
	}

	// Step 1: poll capture-pane for the ready marker.
	if err := c.waitForReadyMarker(ctx, sessionName, opts); err != nil {
		return err
	}

	// Steps 2-4: deliver the payload. A non-empty single-line payload (e.g. a
	// free-text cron prompt) is typed via literal keystrokes + Enter, which
	// Claude Code's TUI reliably executes; bracketed paste can leave such a
	// payload loaded but not submitted (the failure mode SendLine warns about).
	// Multi-line plans and the empty string keep the bracketed-paste path so
	// intermediate newlines aren't treated as premature submits. The dispatch
	// keys off the trimmed payload — and types the trimmed text — so a single
	// logical line carrying surrounding whitespace (e.g. a trailing newline)
	// still takes the reliable literal path rather than slipping into paste,
	// matching the trimmed payload the step-5 verifier checks for.
	trimmedPlan := strings.TrimSpace(plan)
	if trimmedPlan != "" && !strings.ContainsAny(trimmedPlan, "\r\n") {
		if err := c.sendLiteralLineAndEnter(ctx, sessionName, trimmedPlan); err != nil {
			return err
		}
	} else if err := c.SendMessage(ctx, sessionName, plan); err != nil {
		return err
	}

	// Step 5: verify the payload actually left the prompt, so a delivery that
	// loaded but did not execute surfaces as an error instead of a silent
	// no-op. The check is line-oriented (lineStillAtPrompt matches the payload
	// against a single prompt row), so it only applies to a single-line,
	// non-empty payload. Multi-line plans never sit as one matchable row, and
	// the empty string matches any row — both are skipped explicitly so the
	// verification neither runs a needless capture-pane nor reports a spurious
	// error for content it cannot meaningfully check.
	if opts.submitVerifyWait > 0 && trimmedPlan != "" && !strings.ContainsAny(trimmedPlan, "\r\n") {
		if err := c.waitForLineSubmission(ctx, sessionName, trimmedPlan, opts.submitVerifyWait, opts.submitVerifyTick); err != nil {
			return err
		}
	}
	return nil
}

// sendLine is the test-injectable variant of SendLineWithReadyMarker.
func (c *Client) sendLine(ctx context.Context, sessionName, line string, opts sendPlanOpts) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	if opts.deadline <= 0 {
		opts.deadline = sendPlanDefaultDeadline
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = sendPlanDefaultPollInterval
	}
	if opts.readyMarker == "" {
		opts.readyMarker = sendPlanReadyMarker
	}

	if err := c.waitForReadyMarker(ctx, sessionName, opts); err != nil {
		return err
	}

	if err := c.sendLiteralLineAndEnter(ctx, sessionName, line); err != nil {
		return err
	}
	if opts.submitVerifyWait > 0 {
		if err := c.waitForLineSubmission(ctx, sessionName, line, opts.submitVerifyWait, opts.submitVerifyTick); err != nil {
			return err
		}
	}
	return nil
}

// sendLiteralLineAndEnter types line as literal keystrokes (send-keys -l) and
// then submits it with a separate send-keys Enter. Used for "must execute"
// content — single-line cron prompts and command invocations — where bracketed
// paste can leave some TUIs (Claude Code) with the text loaded but not run.
// stderr from either tmux invocation is wrapped into the returned error.
func (c *Client) sendLiteralLineAndEnter(ctx context.Context, sessionName, line string) error {
	// The "--" terminator stops tmux from parsing a payload that begins with
	// "-" (e.g. a free-text cron prompt starting with a hyphen) as a send-keys
	// flag, which would otherwise fail with "unknown flag" and strand the run.
	// escapeSendKeysLiteral protects the one metacharacter "--" does not: a
	// trailing ";", which tmux's command lexer would otherwise drop as a
	// command terminator (so "explain ls;" would be typed as "explain ls").
	textCmd := c.cmdFunc(ctx, "tmux", "send-keys", "-t", sessionName, "-l", "--", escapeSendKeysLiteral(line))
	var textStderr bytes.Buffer
	textCmd.Stderr = &textStderr
	if err := textCmd.Run(); err != nil {
		if msg := strings.TrimSpace(textStderr.String()); msg != "" {
			return fmt.Errorf("tmux send-keys literal line for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux send-keys literal line for %q: %w", sessionName, err)
	}

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

// escapeSendKeysLiteral protects a payload from tmux's command lexer on the
// literal "send-keys -l --" path, where the text becomes the final argument of
// a tmux command. tmux treats a single trailing ";" on that argument as a
// command terminator and drops it, so a prompt like "explain ls;" would be
// typed (and then submitted) as "explain ls". Escaping the trailing ";" as
// "\;" makes tmux deliver it literally. Only the trailing ";" is special on
// this argv path — tmux preserves mid-string semicolons, backslashes, and
// other shell metacharacters verbatim — and it consumes at most one terminator,
// so escaping the final ";" is sufficient (e.g. "a;b;" -> "a;b\;" -> "a;b;").
func escapeSendKeysLiteral(line string) string {
	if strings.HasSuffix(line, ";") {
		return line[:len(line)-1] + `\;`
	}
	return line
}

func (c *Client) waitForLineSubmission(ctx context.Context, sessionName, line string, waitFor, tickEvery time.Duration) error {
	if tickEvery <= 0 {
		tickEvery = 100 * time.Millisecond
	}

	deadline := time.NewTimer(waitFor)
	defer deadline.Stop()

	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()

	for {
		pane, err := c.CapturePane(ctx, sessionName)
		if err != nil {
			return fmt.Errorf("verify command submission: %w", err)
		}
		if !lineStillAtPrompt(pane, line) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("command was not submitted; %q is still present at the tmux prompt", line)
		case <-ticker.C:
		}
	}
}

func lineStillAtPrompt(pane, line string) bool {
	lines := strings.Split(pane, "\n")

	// Locate the bottom-most prompt-marker row: the live input box. Everything
	// below it is footer — a separator rule, the "model | cwd" line, and any
	// custom statusline rows, which are arbitrary user text (e.g. "PR #133",
	// "◉ xhigh · /effort") that no fixed predicate can enumerate — so the scan
	// skips all of it while finding the marker. The empty prompt ("❯" with no
	// text) is a marker too, so a cleared/submitted input reports false below.
	markerIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if hasPromptMarker(strings.TrimSpace(lines[i])) {
			markerIdx = i
			break
		}
	}
	if markerIdx == -1 {
		return false
	}

	// If agent activity appears below that marker, the marker is a submitted
	// prompt echoed into the transcript ("❯ do the thing" above the agent's
	// response or working spinner) with no fresh input box drawn yet, so the
	// payload has already left the prompt. Footer and statusline rows are not
	// agent activity, so a still-pending payload beneath a custom statusline is
	// never mistaken for a submitted one.
	for _, l := range lines[markerIdx+1:] {
		if isAgentActivity(strings.TrimSpace(l)) {
			return false
		}
	}
	return strings.Contains(strings.TrimSpace(lines[markerIdx]), line)
}

// hasPromptMarker reports whether a trimmed pane row begins with one of the
// input-box prompt indicators.
func hasPromptMarker(text string) bool {
	for _, marker := range []string{"❯", "›", ">"} {
		if strings.HasPrefix(text, marker) {
			return true
		}
	}
	return false
}

// agentActivityMarkers are the leading glyphs that mark a pane row as agent
// activity below the input box — an assistant/tool response, tool output, or a
// thinking/working spinner — rather than input-box footer or statusline chrome.
// The verifier runs against both Claude Code and Codex panes, so it covers
// both grammars: the Claude working/output markers statusdetect already trusts
// (lib/bossalib/statusdetect/question.go optionStopMarkers: ⎿ ⏺ · ✻) plus the
// "✽" spinner frame, and Codex's working bullet (plugins/bossd-plugin-codex/
// question.go codexWorking: "• Working (…)"). This lets the predicate recognise
// the activity row each agent renders immediately after accepting a line and
// before any response body appears.
var agentActivityMarkers = []string{
	"⏺", // Claude response / tool-use bullet (U+23FA)
	"⎿", // Claude tool-result branch (U+23BF)
	"·", // Claude working spinner (U+00B7)
	"✻", // Claude thinking spinner (U+273B)
	"✽", // Claude thinking spinner (U+273D)
	"•", // Codex working bullet (U+2022)
}

// isAgentActivity reports whether a trimmed pane row begins with an agent
// activity marker. Matching only the leading glyph keeps custom statusline rows
// safe (e.g. "◉ xhigh · /effort" has a mid-row "·" but does not start with
// one). It is deliberately conservative on unknown rows: misclassifying footer
// chrome as activity would let lineStillAtPrompt report a still-pending payload
// as submitted — the silent cron no-op this guards against — so only these
// distinctive glyphs qualify and arbitrary footer/statusline text never does.
func isAgentActivity(text string) bool {
	for _, marker := range agentActivityMarkers {
		if strings.HasPrefix(text, marker) {
			return true
		}
	}
	return false
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
			if strings.Contains(out, opts.readyMarker) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ready marker %q not seen in pane %q within %s; last pane (truncated): %s",
				opts.readyMarker, sessionName, opts.deadline, truncatePaneForError(lastPane))
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
