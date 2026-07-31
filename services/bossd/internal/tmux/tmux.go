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

	"github.com/recurser/bossalib/termnorm"
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
	// RemainOnExit arms `remain-on-exit on` on the session after creation so a
	// pane whose process exits stays present (marked pane_dead) with its final
	// screen intact rather than collapsing the session. bossd uses this for chat
	// panes so a fast-exiting agent's output can be captured at death before the
	// pane is reaped (BOS-477).
	RemainOnExit bool
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
	// so the argv is deterministic (stable logs and tests). TERM is normalized
	// last: bossd under launchd may have no TERM, and a stray host-specific
	// TERM (e.g. xterm-ghostty) whose terminfo entry is missing on this box
	// would make tmux exit with "missing or unsuitable terminal". termnorm
	// keeps a resolvable TERM and otherwise falls back to xterm-256color. It
	// returns a copy, so the caller's Env map is never mutated.
	sessionEnv := make([]string, 0, len(opts.Env)+1)
	for _, k := range sortedKeys(opts.Env) {
		sessionEnv = append(sessionEnv, k+"="+opts.Env[k])
	}
	for _, kv := range termnorm.Normalize(sessionEnv) {
		args = append(args, "-e", kv)
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

	// Arm remain-on-exit so a pane whose process exits stays present (pane_dead)
	// with its final screen intact, letting bossd capture a fast-exiting agent's
	// output at death before reaping it (BOS-477). Best-effort like the other
	// session options in bindDetachKeys — losing it must not abort a chat launch.
	if opts.RemainOnExit {
		cmd := c.cmdFunc(ctx, "tmux", "set-option", "-t", opts.Name, "remain-on-exit", "on")
		_ = cmd.Run()
	}

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

// ShowEnv reads a single environment variable baked into a tmux session's
// session-environment via `tmux show-environment -t <name> <key>` (BOS-409). On
// success tmux prints `KEY=value` on stdout; ShowEnv returns (value, true). It
// is best-effort: an absent variable (tmux exits non-zero with "unknown
// variable"), a removal marker line ("-KEY", no value), a dead session, or any
// tmux failure all return ("", false) so a single bad row never blocks a sweep.
// An explicitly-empty value ("KEY=") returns ("", true).
func (c *Client) ShowEnv(ctx context.Context, name, key string) (string, bool) {
	cmd := c.cmdFunc(ctx, "tmux", "show-environment", "-t", name, key)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	line := strings.TrimRight(string(out), "\r\n")
	// The first line is the only one that matters. Guard against multi-line
	// output by taking the leading line.
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	prefix := key + "="
	if !strings.HasPrefix(line, prefix) {
		// "-KEY" removal marker, or anything unexpected — treat as absent.
		return "", false
	}
	return strings.TrimPrefix(line, prefix), true
}

// PanePID returns the PID of the first pane in the named tmux session (the
// login shell tmux launched the session's command under). Used by the codex
// provider-session resolver to walk the pane's process tree to the codex
// process and bind the chat to the rollout that process holds open. Returns an
// error when the session is missing, tmux fails, or no pane pid is reported.
func (c *Client) PanePID(ctx context.Context, sessionName string) (int, error) {
	cmd := c.cmdFunc(ctx, "tmux", "list-panes", "-t", sessionName, "-F", "#{pane_pid}")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("list panes %q: %w", sessionName, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			return 0, fmt.Errorf("parse pane pid %q for session %q: %w", line, sessionName, err)
		}
		return pid, nil
	}
	return 0, fmt.Errorf("no pane pid for session %q", sessionName)
}

// PaneDead reports whether the named session's active pane has exited its
// process but is being held present by `remain-on-exit on`. It runs
// `tmux display-message -p -t <name> '#{pane_dead}'` and returns true iff the
// trimmed stdout is "1". A tmux command failure returns (false, err) so callers
// can distinguish a definitely-alive pane from an inability to tell — the
// BOS-477 capture-then-reap path only acts on a definite dead result.
func (c *Client) PaneDead(ctx context.Context, sessionName string) (bool, error) {
	cmd := c.cmdFunc(ctx, "tmux", "display-message", "-p", "-t", sessionName, "#{pane_dead}")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("display-message pane_dead %q: %w", sessionName, err)
	}
	return strings.TrimSpace(string(out)) == "1", nil
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

	// prefillOnly delivers the payload into the composer but sends no Enter and
	// runs no submission verification: the human (or composer owner) submits.
	// Used by the Prefill* wrappers; the Send* wrappers leave it false so they
	// submit and verify as before.
	prefillOnly bool

	// modalDetector is the "is this a menu?" half of the readiness gate for
	// THIS delivery. It lives here, in the per-call options, rather than on
	// Client because the grammar is per agent — codex's glyphs are not Claude's
	// — while the daemon holds one long-lived Client shared by every chat. The
	// caller is the only layer that knows which agent owns this pane, so it is
	// the only layer that can supply the right grammar (BOS-600). Nil disables
	// the check; see ModalDetector in tmux_modal.go.
	modalDetector ModalDetector
}

// deliveryOpts builds the standard production timing for one delivery: the 5s
// readiness deadline, the 100ms poll, the agent's ready marker, and either the
// submit-verifier budget or the prefill (no-Enter) routing. The four public
// Send*/Prefill* wrappers and SendMessage all share it so a timing change is
// one edit rather than five drifting literals.
func deliveryOpts(readyMarker string, submit bool, detector ModalDetector) sendPlanOpts {
	opts := sendPlanOpts{
		deadline:      sendPlanDefaultDeadline,
		pollInterval:  sendPlanDefaultPollInterval,
		readyMarker:   readyMarker,
		modalDetector: detector,
	}
	if submit {
		opts.submitVerifyWait = 2 * time.Second
		opts.submitVerifyTick = 100 * time.Millisecond
	} else {
		opts.prefillOnly = true
	}
	return opts
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
// surfaced error. Every non-empty payload shape is verified: a single-line
// payload against its literal prompt row, a multi-line plan against the
// shape-aware multiLineSubmitted signal (see sendPlan). The empty string is the
// only shape skipped, since it matches any row. When the first Enter is
// swallowed the verifier sends one more Enter and re-checks before erroring.
func (c *Client) SendPlanWithReadyMarker(ctx context.Context, sessionName, plan, readyMarker string) error {
	return c.sendPlan(ctx, sessionName, plan, deliveryOpts(readyMarker, true, nil))
}

// SendLineWithReadyMarker sends a short literal line and submits it with Enter.
// Use this for command invocations where bracketed paste can leave some TUIs
// with the command text loaded but not executed.
func (c *Client) SendLineWithReadyMarker(ctx context.Context, sessionName, line, readyMarker string) error {
	return c.sendLine(ctx, sessionName, line, deliveryOpts(readyMarker, true, nil))
}

// PrefillPlanWithReadyMarker delivers a plan into the composer WITHOUT
// submitting it: it waits for the ready marker and pastes/types the payload,
// but sends no Enter and runs no verification. Use it when the composer owner
// (a human, or a later explicit-submit step) will submit — the counterpart to
// SendPlanWithReadyMarker's auto-submit-and-verify behaviour.
func (c *Client) PrefillPlanWithReadyMarker(ctx context.Context, sessionName, plan, readyMarker string) error {
	return c.sendPlan(ctx, sessionName, plan, deliveryOpts(readyMarker, false, nil))
}

// PrefillLineWithReadyMarker delivers a short literal line into the composer
// WITHOUT submitting it (no Enter, no verification), mirroring
// SendLineWithReadyMarker for the prefill case.
func (c *Client) PrefillLineWithReadyMarker(ctx context.Context, sessionName, line, readyMarker string) error {
	return c.sendLine(ctx, sessionName, line, deliveryOpts(readyMarker, false, nil))
}

// WillSubmit reports whether SendMessage would take the submit path (deliver +
// Enter + verify) for this request, rather than routing it to a prefill. It is
// exported so callers that must describe the delivery they just asked for — the
// SendChatMessage RPC populating delivery_state — read the routing decision from
// the one place that makes it, instead of re-deriving it and drifting from it.
func WillSubmit(submit bool, text string) bool {
	return submit && strings.TrimSpace(text) != ""
}

// SendMessage delivers text into an existing chat's live agent composer,
// routing purely on submit intent (BOS-242 Gap 1, BOS-488). readyMarker is
// the agent's input-box prompt glyph (e.g. claude "❯", codex "›"); an empty
// marker defaults to the legacy Claude marker.
//
// Routing (honoring the send_chat_message submit contract exactly):
//
//   - submit && non-empty: deliver the text, press Enter, and run the BOS-228
//     submit-verifier so a swallowed Enter is retried once and a false
//     "submitted" cannot happen — a delivery that never executes surfaces as a
//     loud error, not a silent no-op. This holds for BOTH single- and
//     multi-line payloads: sendPlan picks the reliable literal-keystroke
//     delivery for a single line and bracketed paste for a multi-line plan, and
//     verifies each shape against the matching signal one layer down.
//   - !submit (prefill, the default): paste/type into the composer, NO Enter,
//     for the composer owner (a human, or a later explicit-submit step) to
//     submit.
//
// Both arms differ only in the options deliveryOpts builds; neither routes
// through the Send/Prefill wrappers, so a reader chasing either path lands on
// sendPlan with the modal detector already threaded in.
//
// An empty/whitespace-only payload has nothing to submit, so it is always
// treated as a prefill (no Enter, no verification) regardless of submit.
func (c *Client) SendMessage(ctx context.Context, sessionName, text string, submit bool, readyMarker string) error {
	return c.sendMessage(ctx, sessionName, text, submit, readyMarker, nil)
}

// sendMessage is SendMessage with the readiness gate's modal detector threaded
// through. Both public spellings funnel through here so the routing decision —
// and the fact that a modal check applies to the prefill path exactly as it
// does to the submit path — is made in one place.
func (c *Client) sendMessage(ctx context.Context, sessionName, text string, submit bool, readyMarker string, detector ModalDetector) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	// Submit: deliver + Enter + verify (fails toward "still pending"). sendPlan
	// picks literal-type vs bracketed paste by payload shape and verifies each
	// shape one layer down; a payload the agent queues behind a running turn is
	// recognised there from the pane itself (BOS-599), so no caller has to supply
	// an agent working-state probe for it. Prefill (submit=false, or nothing
	// meaningful to submit): deliver into the composer with no Enter and no
	// verification.
	return c.sendPlan(ctx, sessionName, text, deliveryOpts(readyMarker, WillSubmit(submit, text), detector))
}

// SendMessageWithModal is SendMessage with the readiness gate's modal check
// bound to detector for this one delivery, refusing with ErrBlockedByModal
// (OutcomeBlockedByModal) when the pane is showing a menu rather than a
// composer. A nil detector behaves exactly like SendMessage.
func (c *Client) SendMessageWithModal(ctx context.Context, sessionName, text string, submit bool, readyMarker string, detector ModalDetector) error {
	return c.sendMessage(ctx, sessionName, text, submit, readyMarker, detector)
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

	// Step 2: deliver the payload into the composer WITHOUT submitting. A
	// non-empty single-line payload (e.g. a free-text cron prompt) is typed via
	// literal keystrokes, which Claude Code's TUI reliably executes; bracketed
	// paste can leave such a payload loaded but not submitted (the failure mode
	// SendLine warns about). Multi-line plans and the empty string keep the
	// bracketed-paste path so intermediate newlines aren't treated as premature
	// submits. The dispatch keys off the trimmed payload — and types the trimmed
	// text — so a single logical line carrying surrounding whitespace (e.g. a
	// trailing newline) still takes the reliable literal path rather than
	// slipping into paste, matching the trimmed payload the verifier checks for.
	//
	// The dispatch is a closure so the verifier's retry can re-run the exact same
	// delivery after clearing the composer (see verifyWithEnterRetry): a retry
	// must reproduce this shape choice, not approximate it.
	trimmedPlan := strings.TrimSpace(plan)
	deliver := func(ctx context.Context) error {
		if trimmedPlan != "" && !strings.ContainsAny(trimmedPlan, "\r\n") {
			return c.typeLiteralLineNoEnter(ctx, sessionName, trimmedPlan)
		}
		return c.pasteBufferNoEnter(ctx, sessionName, plan)
	}
	if err := deliver(ctx); err != nil {
		return err
	}

	// Prefill delivery stops here: the composer owner submits, so we send no
	// Enter and run no verification.
	if opts.prefillOnly {
		return nil
	}

	// Step 3: submit with Enter.
	if err := c.sendEnter(ctx, sessionName); err != nil {
		return err
	}

	// Step 4: verify the payload actually left the prompt, so a delivery that
	// loaded but did not execute surfaces as an error instead of a silent no-op.
	// Every non-empty payload shape is verified — single-line against its literal
	// prompt row, multi-line against the shape-aware multiLineSubmitted signal.
	// Only the empty string is skipped (it matches any row). A first Enter the
	// TUI swallows is retried once inside verifyWithEnterRetry before erroring.
	if opts.submitVerifyWait > 0 && trimmedPlan != "" {
		if err := c.verifyWithEnterRetry(ctx, sessionName, plan, opts, deliver); err != nil {
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

	// A closure so the verifier's retry can re-run the identical delivery after
	// clearing the composer (see verifyWithEnterRetry).
	deliver := func(ctx context.Context) error {
		return c.typeLiteralLineNoEnter(ctx, sessionName, line)
	}
	if err := deliver(ctx); err != nil {
		return err
	}

	// Prefill delivery stops before Enter: the composer owner submits.
	if opts.prefillOnly {
		return nil
	}

	if err := c.sendEnter(ctx, sessionName); err != nil {
		return err
	}
	if opts.submitVerifyWait > 0 {
		if err := c.verifyWithEnterRetry(ctx, sessionName, line, opts, deliver); err != nil {
			return err
		}
	}
	return nil
}

// typeLiteralLineNoEnter types line as literal keystrokes (send-keys -l) WITHOUT
// submitting. Delivery and submission are split — this types, sendEnter submits
// — so prefill delivery can type without Enter and the submit path can retry the
// Enter independently. Used for "must execute" content (single-line cron prompts
// and command invocations) where bracketed paste can leave some TUIs (Claude
// Code) with the text loaded but not run. stderr from tmux is wrapped into the
// returned error.
func (c *Client) typeLiteralLineNoEnter(ctx context.Context, sessionName, line string) error {
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
	return nil
}

// pasteBufferNoEnter loads text into a tmux paste buffer and pastes it into the
// composer with bracketed paste enabled (-p) WITHOUT submitting. It is the
// load-buffer → paste-buffer half of SendMessage, split out so prefill delivery
// and the multi-line submit path can paste without an Enter. stderr from either
// tmux invocation is wrapped into the returned error.
func (c *Client) pasteBufferNoEnter(ctx context.Context, sessionName, text string) error {
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
	return nil
}

// sendEnter submits the current composer contents with a single send-keys
// Enter. It is the submission half of the literal/paste delivery helpers, split
// out so prefill delivery can skip it and the submit path can retry it
// independently. stderr from tmux is wrapped into the returned error.
func (c *Client) sendEnter(ctx context.Context, sessionName string) error {
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

// clearComposer sends a single "C-u" — kill-to-start-of-line — to the live
// input box. It is the first half of the idempotent submit retry: clear, then
// re-deliver, so a retry types the payload exactly once instead of appending a
// second copy to a composer that may still hold the first (BOS-598).
//
// Two limits are deliberate, and neither is assumed away. First, C-u is a
// best-effort clear, not a guaranteed empty: it kills to the start of the
// LINE, and no fixture in this repo demonstrates that either the Claude or the
// Codex composer collapses a multi-line payload to nothing on one press — so
// the caller confirms the postcondition with awaitComposerCleared instead of
// trusting this call's success. Second, C-u in a working full-screen pane is an
// unwanted keystroke, so callers MUST only invoke it with positive evidence
// that a composer is drawn and holds the payload (the verifier's
// confirmed-pending result, which waitForSubmission returns only when the final
// poll actually saw a prompt marker). stderr from tmux is wrapped into the
// returned error.
func (c *Client) clearComposer(ctx context.Context, sessionName string) error {
	clearCmd := c.cmdFunc(ctx, "tmux", "send-keys", "-t", sessionName, "C-u")
	var clearStderr bytes.Buffer
	clearCmd.Stderr = &clearStderr
	if err := clearCmd.Run(); err != nil {
		if msg := strings.TrimSpace(clearStderr.String()); msg != "" {
			return fmt.Errorf("tmux send-keys C-u for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux send-keys C-u for %q: %w", sessionName, err)
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

// waitForReadyMarker polls CapturePane until a live composer is observed or the
// deadline elapses. The first poll is immediate so already-ready sessions return
// without sleeping.
//
// Readiness is two conditions, checked against the same capture in this order,
// so the gate adds no tmux calls:
//
//  1. A row whose leading glyph is the ready marker exists — a drawn input box,
//     not the glyph occurring somewhere in the capture.
//  2. That row is not part of a modal. A modal is refused — not waited out —
//     with ErrBlockedByModal / OutcomeBlockedByModal, because the alternative is
//     an Enter into a menu whose side effect is unbounded. One such Enter
//     selected "Update now" on a codex update interstitial, and the reinstall
//     killed the pane and destroyed the chat (BOS-600).
//
// The order is load-bearing, not cosmetic. Condition 2 is answered by the
// agent's plugin over gRPC, so asking it first would cost one round-trip per
// poll tick — up to ~50 per send — and would need a memoizer to claw that back.
// Asking it only about a capture that already satisfies condition 1 makes the
// probe at-most-once per wait BY CONSTRUCTION: either no marker row is drawn
// (keep polling, no RPC at all) or one is, and the probe's answer ends the loop
// in both directions. This costs nothing in coverage — every modal that draws a
// marker-shaped row, which includes both captured fixtures, is still probed on
// the tick it appears — and the deadline branch below probes once more so a
// modal that draws no marker row at all is still NAMED rather than reported as
// a bare timeout.
//
// Refusing is deliberately conservative: it fails loud rather than guessing, and
// it never dismisses the modal (pressing Escape into an unknown TUI state is the
// same gamble). A composer whose text happens to match an agent's menu grammar
// would be refused too — a new, visible failure mode this trade accepts.
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
			if composerRowIndex(out, opts.readyMarker) != -1 {
				if paneShowsModal(ctx, opts.modalDetector, out) {
					return blockedByModalErr(sessionName, out)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			// The loop above never probed this pane: nothing composer-shaped was
			// ever drawn on it. Ask once here so a modal that renders no marker
			// row still refuses under its own name instead of masquerading as a
			// slow-starting agent.
			if lastPane != "" && paneShowsModal(ctx, opts.modalDetector, lastPane) {
				return blockedByModalErr(sessionName, lastPane)
			}
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
