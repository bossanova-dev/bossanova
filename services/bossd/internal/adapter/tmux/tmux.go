// Package tmux provides a wrapper for tmux CLI operations.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/contract/daemon/termnorm"
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
// deadlines never block a fast in-test stub.
//
// There are two readiness deadlines, not one, because the two delivery paths
// answer to different ceilings (BOS-893). Session start and resume have to
// cover tmux spawn, a full interactive login-shell init, exec of the agent,
// node boot, and TUI first paint — measured shell init alone ranged 0.75s to
// 12s on an affected host — and nothing downstream bounds them. An established
// chat's send runs inside the SendChatMessage RPC, which bosso relays under a
// fixed command deadline, so its budget stays short; letting it inherit the
// generous start budget would produce an ambiguous delivery the caller must not
// retry, because a retry double-types into the composer.
//
// Both are operator-tunable per host via settings.json (`tmux_delivery`), and
// reach this package as WithSessionStartReadyDeadline / WithSendReadyDeadline
// rather than by importing lib/bossalib/kit/config: this package stays a thin tmux
// CLI wrapper rather than gaining a config dependency to carry two integers.
// The config accessors carry the same two defaults, and a drift guard in
// tmux_delivery_deadline_test.go asserts the pairs stay equal.
//
// The session-start deadline is not the outermost limit on every path, so it
// cannot be raised in isolation (BOS-896). On the repair-driven session-start
// path this readiness wait runs *inside* a plugin→host unary RPC, StartChatRun,
// which carries its own ceiling in lib/bossalib/contract/plugin/plugin/hostclient. That ceiling
// is derived from the host's resolved readiness deadline on the repair path:
// readiness + max(readiness, the submit-verifier and fresh-session-ID tails).
// At the 45s default it is 90s; at 300s it is 600s. This deadline plus the
// in-RPC tails that follow it (the submit verifier here, and
// freshProviderSessionIDResolveDeadline over in internal/session) must stay
// under that ceiling with headroom, or the RPC is cut off first and the number
// below silently stops taking effect on that path. Be precise about what that
// costs. It is the CONFIGURED wait rather than the diagnostic: this is the
// multi-attempt session-start path, so waitForReadyMarkerWithAttempts clamps
// each attempt to what the caller's context can still fund and the failure
// still arrives as the ready-marker error carrying the pane capture that
// distinguishes a slow boot from an auth prompt from an update interstitial.
// The attempt simply runs for less than the number below promises, and only the
// clamp clause in the timeout message says so. A bare context error is the
// FROZEN single-attempt path's failure mode (see ready_marker.go) — that
// delivery is handed its context unclamped, so ctx.Done() reaches the caller
// with nothing attached. TestSessionStartReadinessFitsStartChatRunBudget in
// tmux_budget_test.go enforces the configured-value relationship, and drift
// guards in tmux and session pin the two shared in-RPC tail terms.
//
// sendPlanReadyMarker is the prompt indicator Claude Code renders inside
// its input box once the TUI is ready to accept input. We intentionally
// match on the prompt character itself rather than the default footer
// ("? for shortcuts"): users with a custom statusline replace that footer
// entirely, but the input-box prompt is part of Claude's core rendering
// and survives statusline customisation.
const (
	// DefaultSessionStartReadyDeadline bounds the readiness wait on the
	// session-start and resume path when the operator has configured nothing.
	// It is sized against the measured 12s shell-init ceiling with roughly 33s
	// — about 3x — left for exec, node boot, and TUI first paint.
	DefaultSessionStartReadyDeadline = 45 * time.Second
	// DefaultSendReadyDeadline bounds the readiness wait when delivering into
	// an ALREADY-RUNNING agent's composer. It keeps the historical 5s budget on
	// purpose: that agent is booted, so its composer is either there now or
	// wedged, and this delivery has a relay deadline above it.
	DefaultSendReadyDeadline    = 5 * time.Second
	tmuxCommandWaitDelay        = 2 * time.Second
	sendPlanDefaultPollInterval = 100 * time.Millisecond
	sendPlanReadyMarker         = "❯"
	codexReadyMarker            = "›"

	// sendPlanDefaultReadyAttempts is the floor a delivery falls back to when
	// its options name no attempt count — one attempt, i.e. exactly today's
	// behaviour. It is deliberately the CONSERVATIVE value rather than the
	// session-start one: an option struct built by hand (every pre-BOS-895 test
	// does this) must not silently acquire a retry it never asked for, which
	// would double the wall clock of every timeout assertion in the package.
	sendPlanDefaultReadyAttempts = 1

	// sessionStartReadyAttempts is how many times the session-start and resume
	// path re-runs the readiness wait before giving up. The wait is confined to
	// the phase BEFORE any keystroke reaches the pane, so re-running it cannot
	// re-deliver a payload — that lexical confinement, not a judgement about
	// idempotence, is what makes the retry safe (BOS-895). Two attempts covers
	// the observed failure: an agent TUI that is still repainting when the
	// first budget expires.
	sessionStartReadyAttempts = 2

	// sendReadyAttempts holds the ALREADY-RUNNING send path at one attempt.
	// That delivery runs inside an RPC bosso relays under a 30s command
	// deadline, so a second 5s wait buys little and risks overrunning the
	// relay; and a live composer that is not ready within 5s is a different
	// fault from a TUI that has not finished booting.
	sendReadyAttempts = 1
)

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// DaemonIDEnvKey is the session-environment variable every pane this client
// creates carries, naming the bossd instance that created it. The orphaned-tmux
// reaper (BOS-846) reads it back with `show-environment` so it only ever
// considers killing panes it can attribute to itself: a pane stamped with
// another daemon's id belongs to that daemon, and one carrying no stamp at all
// predates this mechanism or was created by something else entirely.
const DaemonIDEnvKey = "BOSS_DAEMON_ID"

// Client wraps tmux CLI operations.
type Client struct {
	cmdFunc  CommandFactory
	daemonID string

	// sessionStartReadyDeadline and sendReadyDeadline are the operator-tuned
	// composer-readiness budgets for the two delivery paths (BOS-893). Zero
	// means "unconfigured": each builder substitutes its OWN package default,
	// so a client with only one of them set can never let the other path
	// inherit the wrong number.
	sessionStartReadyDeadline time.Duration
	sendReadyDeadline         time.Duration

	// pasteSettleWait overrides the package pasteSettleWait bound on the
	// read-only wait between a Claude paste and its Enter. Zero means
	// "unconfigured", so the package const applies.
	pasteSettleWait time.Duration

	// sessionStartReadyAttempts is the injectable half of the session-start
	// readiness COST, the other half being the budget above. Zero means
	// "unconfigured", so the package const applies.
	//
	// It exists for out-of-package callers that pin timing — the test harness
	// in particular. Before it, the budget was the only factor such a caller
	// could reach, so pinning the cost meant halving the budget to compensate
	// for an attempt count it could not see, and the pin silently stopped
	// meaning what it said the moment either factor moved. Exposing both lets a
	// caller state the product it wants.
	sessionStartReadyAttempts int
}

// Option configures a Client.
type Option func(*Client)

// WithCommandFactory sets a custom CommandFactory for testing.
func WithCommandFactory(f CommandFactory) Option {
	return func(c *Client) {
		c.cmdFunc = f
	}
}

// WithDaemonID makes every session this client creates carry DaemonIDEnvKey.
//
// The stamp lives on the client rather than at each spawn site deliberately:
// bossd constructs exactly one production client, so one option covers every
// present and future call site, whereas a per-call-site stamp would silently
// leave any new spawn path producing panes the reaper cannot attribute. An
// empty id stamps nothing — "unstamped" and "owned by the daemon whose id is
// the empty string" must not be the same thing.
func WithDaemonID(id string) Option {
	return func(c *Client) {
		c.daemonID = id
	}
}

// WithSessionStartReadyDeadline sets how long a session-start or resume
// delivery waits for the agent's composer prompt before giving up. It exists so
// a host whose interactive login shell is slow can buy the readiness wait more
// room than DefaultSessionStartReadyDeadline without a rebuild.
//
// A non-positive duration is IGNORED rather than stored: zero would mean "no
// wait" once it reached sendPlan, which is the failure the floor there exists
// to prevent, and no configuration should be able to express it.
func WithSessionStartReadyDeadline(d time.Duration) Option {
	return func(c *Client) {
		if d <= 0 {
			return
		}
		c.sessionStartReadyDeadline = d
	}
}

// WithSessionStartReadyAttempts sets how many times the session-start and
// resume path runs the composer-readiness wait before failing. It is the
// companion knob to WithSessionStartReadyDeadline: together they are the whole
// of what a doomed session start costs, and a caller pinning that cost needs
// both or it is pinning a product it can only half see.
//
// This is deliberately NOT surfaced as an operator setting. The attempt count
// is a safety property — the retry is sound only because the wait is lexically
// confined to the phase before the first keystroke (BOS-895) — and an operator
// raising it past what that confinement covers would not be told. The seam is
// for callers inside this codebase that construct a Client directly.
//
// A non-positive count is IGNORED rather than stored, matching the deadline
// options: zero would mean "never wait at all" once resolveReadyAttemptsFloor
// saw it, and no caller should be able to express that by accident.
func WithSessionStartReadyAttempts(n int) Option {
	return func(c *Client) {
		if n <= 0 {
			return
		}
		c.sessionStartReadyAttempts = n
	}
}

// WithSendReadyDeadline sets how long a delivery into an ALREADY-RUNNING
// agent's composer waits for the ready marker. Keep it short: this delivery
// happens inside the SendChatMessage RPC, which bosso relays under its own
// command deadline, and a readiness wait that outlives the relay leaves the
// caller holding an ambiguous delivery it must not retry. The clamp that
// enforces that ceiling lives with the setting, in config.TmuxDeliveryConfig.
//
// A non-positive duration is ignored, for the same reason as above.
func WithSendReadyDeadline(d time.Duration) Option {
	return func(c *Client) {
		if d <= 0 {
			return
		}
		c.sendReadyDeadline = d
	}
}

// WithPasteSettleWait bounds the read-only wait between a Claude paste and its
// Enter (see pasteSettleWait). The wait never decides an outcome — Enter and
// the verifier run whatever it sees — so a caller whose fake pane never
// repaints can shorten it without changing what is delivered.
//
// A non-positive duration is ignored, for the same reason as above.
func WithPasteSettleWait(d time.Duration) Option {
	return func(c *Client) {
		if d <= 0 {
			return
		}
		c.pasteSettleWait = d
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
	// The values reach tmux on stdin, never on the process argv, because they
	// include credentials and argv is world-readable through ps (BOS-1511).
	Env map[string]string
	// RemainOnExit arms `remain-on-exit on` on the session after creation so a
	// pane whose process exits stays present (marked pane_dead) with its final
	// screen intact rather than collapsing the session. bossd uses this for chat
	// panes so a fast-exiting agent's output can be captured at death before the
	// pane is reaped (BOS-477).
	RemainOnExit bool
}

// ErrCommandTooLong reports a new-session command too large for tmux to
// accept. When the command travelled as argv, tmux packed it into one
// client-to-server message and rejected it with "command too long" past
// roughly 16 KB (measured on tmux 3.7c and 3.8), so NewSession refuses before
// invoking tmux and names the size.
var ErrCommandTooLong = errors.New("tmux command too long")

// maxNewSessionCommandBytes is NewSession's refusal threshold, measured as the
// sum of len(arg)+1 over every logical new-session argument (env entries
// included). It sat about 1 KB below the measured argv-delivery limit to absorb
// message framing. Since BOS-1511 the command reaches tmux on stdin through
// `source-file -`, where that limit does not apply (a 40,000-byte env value
// launched fine), so this is now a conservative bound; relaxing it is a
// separate change.
const maxNewSessionCommandBytes = 15 * 1024

func commandBytes(args []string) int {
	n := 0
	for _, a := range args {
		n += len(a) + 1
	}
	return n
}

// newSessionScriptArgs is the whole process argv, after "tmux", that
// NewSession executes (BOS-1511). The logical `new-session …` command, env
// values included, travels on stdin as one tmux command-language line instead:
// process argv is readable by every local user through ps, and the first
// new-session on a host forks the tmux server, which keeps that argv for its
// whole life. `start-server` comes first because `source-file -` fails with
// "error connecting to …" when no server is running yet. Verified on tmux 3.7c
// and 3.8.
var newSessionScriptArgs = []string{"start-server", ";", "source-file", "-"}

// IsNewSessionScript reports whether args — a tmux argv without the leading
// "tmux" — is the invocation NewSession runs. Test fakes that recognise
// new-session by its argv use it, then recover the logical new-session argv
// from stdin with ScriptSink / DecodeScriptLine.
func IsNewSessionScript(args []string) bool {
	return slices.Equal(args, newSessionScriptArgs)
}

// encodeScriptLine renders args as one tmux command-language line: every
// argument single-quoted, an embedded single quote written as close-quote,
// backslash-quote, reopen-quote, arguments separated by one space, and a
// trailing newline. Inside single quotes tmux expands nothing — not $VAR, ~, #{format},
// ;, %if, or a backslash — and a quoted newline stays part of the argument, so
// every value reaches tmux byte-exact (verified on tmux 3.7c and 3.8).
func encodeScriptLine(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('\'')
		b.WriteString(strings.ReplaceAll(a, "'", `'\''`))
		b.WriteByte('\'')
	}
	b.WriteByte('\n')
	return b.String()
}

// DecodeScriptLine is the exact inverse of encodeScriptLine, for test fakes
// that need the logical new-session argv NewSession delivered on stdin. It
// accepts only the subset encodeScriptLine emits — single-quoted segments, \'
// outside quotes, one space between arguments, and exactly one trailing
// newline — and returns an error for anything else. It is NOT a general tmux
// command parser. Errors name byte offsets only, never the line's content,
// because the line carries session-environment values.
func DecodeScriptLine(line string) ([]string, error) {
	body, ok := strings.CutSuffix(line, "\n")
	if !ok {
		return nil, errors.New("tmux script line: missing trailing newline")
	}
	var (
		args  []string
		cur   strings.Builder
		inArg bool
	)
	for i := 0; i < len(body); {
		switch body[i] {
		case '\'':
			end := strings.IndexByte(body[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("tmux script line: unterminated quote at byte %d", i)
			}
			cur.WriteString(body[i+1 : i+1+end])
			i += end + 2
			inArg = true
		case '\\':
			if i+1 >= len(body) || body[i+1] != '\'' {
				return nil, fmt.Errorf("tmux script line: unsupported escape at byte %d", i)
			}
			cur.WriteByte('\'')
			i += 2
			inArg = true
		case ' ':
			if !inArg {
				return nil, fmt.Errorf("tmux script line: unexpected separator at byte %d", i)
			}
			args = append(args, cur.String())
			cur.Reset()
			inArg = false
			i++
		default:
			return nil, fmt.Errorf("tmux script line: unquoted byte at %d", i)
		}
	}
	if !inArg {
		return nil, errors.New("tmux script line: empty line or trailing separator")
	}
	return append(args, cur.String()), nil
}

// ScriptSink is an io.Writer for test fakes standing in for tmux. A fake
// answers the IsNewSessionScript invocation with a `cat` whose Stdout is a
// ScriptSink; once the whole stdin line has arrived the sink decodes it and
// hands the logical new-session argv (starting "new-session") to its callback,
// exactly once. exec.Cmd.Run waits for that output copy, so the callback has
// fired by the time NewSession returns. Production code never uses it.
type ScriptSink struct {
	mu       sync.Mutex
	buf      strings.Builder
	done     bool
	onScript func(args []string)
}

// NewScriptSink returns a ScriptSink that reports the decoded argv to onScript.
func NewScriptSink(onScript func(args []string)) *ScriptSink {
	return &ScriptSink{onScript: onScript}
}

// Write buffers p and, once the buffer holds one complete script line, decodes
// it and fires the callback. The only newline outside quotes is the terminator,
// so a buffer ending in a newline that still fails to decode is a write split
// at a quoted newline, and the sink keeps waiting.
func (s *ScriptSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf.Write(p)
	if s.done || !strings.HasSuffix(s.buf.String(), "\n") {
		s.mu.Unlock()
		return len(p), nil
	}
	args, err := DecodeScriptLine(s.buf.String())
	if err != nil {
		s.mu.Unlock()
		return len(p), nil
	}
	s.done = true
	s.mu.Unlock()
	s.onScript(args)
	return len(p), nil
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
	//
	// The client's daemon-id stamp is merged in before sorting rather than
	// appended, so it takes its place in the deterministic ordering, and it
	// overwrites any caller-supplied value: a caller must not be able to make a
	// pane look like another daemon's and thereby exempt it from reaping. The
	// merge happens into a copy — the caller's map is shared state.
	env := opts.Env
	if c.daemonID != "" {
		env = make(map[string]string, len(opts.Env)+1)
		for k, v := range opts.Env {
			env[k] = v
		}
		env[DaemonIDEnvKey] = c.daemonID
	}
	sessionEnv := make([]string, 0, len(env)+1)
	for _, k := range sortedKeys(env) {
		sessionEnv = append(sessionEnv, k+"="+env[k])
	}
	for _, kv := range termnorm.Normalize(sessionEnv) {
		args = append(args, "-e", kv)
	}
	args = append(args, opts.Command...)
	if size := commandBytes(args); size > maxNewSessionCommandBytes {
		return fmt.Errorf("create tmux session %q: %w: %d bytes exceeds the %d-byte limit (argv plus %d env entries)",
			opts.Name, ErrCommandTooLong, size, maxNewSessionCommandBytes, len(env))
	}

	// Deliver the unchanged logical command on stdin rather than as argv
	// (BOS-1511): see newSessionScriptArgs. A tmux-side error in the script
	// exits non-zero with the reason on stderr, prefixed "-:1:".
	cmd := c.cmdFunc(ctx, "tmux", slices.Clone(newSessionScriptArgs)...)
	cmd.Stdin = strings.NewReader(encodeScriptLine(args))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
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
	cmd.WaitDelay = tmuxCommandWaitDelay
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

// LiveSession is one session tmux currently reports as running: its name, the
// instant tmux created it, and how many clients are attached. Created comes
// from tmux's own `session_created` clock rather than any database column, so
// it is available for a pane that has no row at all and survives a daemon
// restart (BOS-846 D4).
type LiveSession struct {
	Name            string
	Created         time.Time
	AttachedClients int
}

// listSessionsFormat is the `-F` format ListSessions parses. Fields are
// tab-separated because a tmux session name may contain spaces but never a tab.
// The test fakes in internal/status and internal/testharness emit this exact
// shape, so changing it means changing them too.
const listSessionsFormat = "#{session_name}\t#{session_created}\t#{session_attached}"

// ListSessions returns every tmux session the server currently reports.
//
// It is the only place in bossd that asks tmux what is actually running rather
// than starting from a database row, so its error contract is deliberately
// asymmetric (BOS-846 D6). "no server running" is an *affirmative* absent
// signal — an idle host genuinely has zero sessions — and returns an empty
// slice with a nil error. Every other failure returns an error, because a
// destructive caller must never read an unreadable tmux as an empty one.
//
// Malformed lines are skipped rather than guessed at: a line with missing
// fields, an empty name, or a non-numeric creation/attached stamp contributes
// nothing and the remaining well-formed lines are still returned.
func (c *Client) ListSessions(ctx context.Context) ([]LiveSession, error) {
	cmd := c.cmdFunc(ctx, "tmux", "list-sessions", "-F", listSessionsFormat)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "no server running") {
			return nil, nil
		}
		if msg == "" {
			return nil, fmt.Errorf("tmux list-sessions: %w", err)
		}
		return nil, fmt.Errorf("tmux list-sessions: %w (stderr: %s)", err, msg)
	}

	var out []LiveSession
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" {
			continue
		}
		name := fields[0]
		secs, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			continue
		}
		attached, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		out = append(out, LiveSession{Name: name, Created: time.Unix(secs, 0), AttachedClients: attached})
	}
	return out, nil
}

// ShowEnvStatus is the outcome of a session-environment read (BOS-1281).
//
// The three cases exist because `tmux show-environment` exits non-zero BOTH for
// a variable that is simply not set AND for a read that failed outright — no
// tmux server, a session that has gone, an exec error, a timeout. Exit status
// therefore cannot separate them, and a caller that collapses the two is
// reading a transient failure as a settled negative answer. That is only safe
// where both answers route to the same harmless outcome; it is not safe where
// the read gates a destructive decision.
type ShowEnvStatus int

const (
	// ShowEnvSet means tmux answered and the variable carries a value. The
	// value may legitimately be empty, which is what tmux prints as "KEY=".
	ShowEnvSet ShowEnvStatus = iota
	// ShowEnvUnset means tmux answered, and the answer is that the variable
	// holds no value in this session's environment — either "unknown variable"
	// on stderr, or a "-KEY" removal marker on stdout.
	ShowEnvUnset
	// ShowEnvError means the read itself failed, so NOTHING is known about the
	// variable. A caller gating a destructive action must fail closed here
	// rather than treat it as ShowEnvUnset.
	ShowEnvError
)

// String renders the status for log lines and test failures.
func (s ShowEnvStatus) String() string {
	switch s {
	case ShowEnvSet:
		return "set"
	case ShowEnvUnset:
		return "unset"
	case ShowEnvError:
		return "error"
	default:
		return "unknown"
	}
}

// tmuxUnknownVariableStderr is what tmux prints when the requested variable is
// not set in the session environment (cmd-show-environment.c emits
// "unknown variable: %s"). It is the ONLY signal that separates unset from
// failed, because tmux exits non-zero for both — which is exactly why the
// classification below reads stderr rather than the exit status.
const tmuxUnknownVariableStderr = "unknown variable"

// ShowEnv reads a single environment variable baked into a tmux session's
// session-environment via `tmux show-environment -t <name> <key>` (BOS-409). On
// success tmux prints `KEY=value` on stdout; ShowEnv returns (value, ShowEnvSet),
// and an explicitly-empty value ("KEY=") returns ("", ShowEnvSet).
//
// An absent variable and a removal marker line ("-KEY", no value) return
// ("", ShowEnvUnset). Anything else — a dead session, no tmux server, an exec
// or timeout failure — returns ("", ShowEnvError) so a caller whose decision is
// destructive can fail closed instead of mistaking a failed read for a settled
// "not set" (BOS-1281).
func (c *Client) ShowEnv(ctx context.Context, name, key string) (string, ShowEnvStatus) {
	cmd := c.cmdFunc(ctx, "tmux", "show-environment", "-t", name, key)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(strings.ToLower(stderr.String()), tmuxUnknownVariableStderr) {
			return "", ShowEnvUnset
		}
		// Includes the empty-stderr case: a non-zero exit that said nothing is
		// not evidence the variable is unset, so it is an error, not an answer.
		return "", ShowEnvError
	}
	line := strings.TrimRight(string(out), "\r\n")
	// The first line is the only one that matters. Guard against multi-line
	// output by taking the leading line.
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	prefix := key + "="
	if !strings.HasPrefix(line, prefix) {
		// "-KEY" removal marker, or anything unexpected. tmux ANSWERED here, so
		// this is a settled "no value", not a failed read.
		return "", ShowEnvUnset
	}
	return strings.TrimPrefix(line, prefix), ShowEnvSet
}

// ServerCommandLineCarriesEnv reports whether the running tmux server's own
// command line still carries `-e NAME=value` session-environment flags — the
// mark of a server forked by a pre-BOS-1511 bossd's argv-delivered
// new-session. That server keeps its argv, credentials included, readable by
// every local user through ps until it exits, and no bossd change can scrub
// it: the caller warns the operator to run `tmux kill-server` once idle.
//
// No server running is (false, nil). The command line itself is never
// returned, logged, or put in an error: it is the secret.
func (c *Client) ServerCommandLineCarriesEnv(ctx context.Context) (bool, error) {
	cmd := c.cmdFunc(ctx, "tmux", "display-message", "-p", "#{pid}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return false, nil
		}
		return false, fmt.Errorf("tmux display-message server pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("tmux display-message server pid: unparseable output (%d bytes)", len(out))
	}
	ps := c.cmdFunc(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid))
	ps.WaitDelay = tmuxCommandWaitDelay
	line, err := ps.Output()
	if err != nil {
		return false, fmt.Errorf("ps tmux server pid %d: %w", pid, err)
	}
	return commandLineCarriesEnvFlag(string(line)), nil
}

// commandLineCarriesEnvFlag reports whether a ps command line holds a `-e`
// token followed by a `NAME=` token, the shape argv-delivered session env took.
func commandLineCarriesEnvFlag(line string) bool {
	fields := strings.Fields(line)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-e" && isEnvAssignment(fields[i+1]) {
			return true
		}
	}
	return false
}

// isEnvAssignment reports whether s starts with a shell-style variable name
// followed by '='.
func isEnvAssignment(s string) bool {
	name, _, ok := strings.Cut(s, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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

// RefreshClient forces tmux to redraw every client currently attached to
// sessionName. The web-tmux-attach feature calls this after a per-attach ring
// buffer overflow forces a RESYNC: dropping oldest bytes leaves later viewers
// with a corrupt frame, but a tmux-driven repaint resolves it without needing
// each attach to negotiate its own resync. Idempotent and cheap — safe to
// fire-and-forget on every overflow.
//
// This resolves the session's clients first and refreshes each by name, because
// `refresh-client -t` takes a CLIENT, not a session. Passing a session name to
// it — as this wrapper used to — always fails with "can't find client", so the
// RESYNC repaint never actually happened. There is no session-targeting spelling
// of refresh-client to fall back on; the client lookup is the whole fix.
//
// A session with no attached clients is a no-op success: there is nothing to
// repaint, which is the normal state for a headless/cron session and must not be
// reported as a failure. Every resolved client is refreshed even if an earlier
// one fails, so a client that detached between the list and the refresh (a
// benign race) cannot skip the clients behind it; the failures are joined into
// the returned error.
func (c *Client) RefreshClient(ctx context.Context, sessionName string) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	clients, err := c.listClientNames(ctx, sessionName)
	if err != nil {
		return err
	}
	var errs []error
	for _, client := range clients {
		cmd := c.cmdFunc(ctx, "tmux", "refresh-client", "-t", client)
		out, runErr := cmd.CombinedOutput()
		if runErr == nil {
			continue
		}
		if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
			errs = append(errs, fmt.Errorf("tmux refresh-client -t %q: %s: %w", client, trimmed, runErr))
			continue
		}
		errs = append(errs, fmt.Errorf("tmux refresh-client -t %q: %w", client, runErr))
	}
	return errors.Join(errs...)
}

// listClientNames returns the tmux client names attached to sessionName, via
// `tmux list-clients -t <session> -F '#{client_name}'`. Unlike refresh-client,
// list-clients DOES accept a session as its target.
//
// A session tmux cannot find yields no clients and no error: the session dying
// between a caller's decision to repaint and this lookup is a race the repaint
// path must absorb, not an error to propagate up a fire-and-forget call. Any
// other tmux failure is surfaced.
func (c *Client) listClientNames(ctx context.Context, sessionName string) ([]string, error) {
	cmd := c.cmdFunc(ctx, "tmux", "list-clients", "-t", sessionName, "-F", "#{client_name}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "can't find session") || strings.Contains(msg, "no server running") {
			return nil, nil
		}
		if msg == "" {
			return nil, fmt.Errorf("tmux list-clients -t %q: %w", sessionName, err)
		}
		return nil, fmt.Errorf("tmux list-clients -t %q: %w (stderr: %s)", sessionName, err, msg)
	}
	var names []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
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
	cmd.WaitDelay = tmuxCommandWaitDelay
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
// '\” idiom. Mirrors the helper in lib/bossalib/contract/plugin/agentruntime so the
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
	return domain.ChatSessionName(repoID, agentSessionID)
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

// captureVisibleStyled captures the visible pane with its SGR escape sequences
// kept (-e), for the one question plain text cannot answer: whether composer
// text is a dim placeholder or real input (refuseOccupiedComposer).
func (c *Client) captureVisibleStyled(ctx context.Context, sessionName string) (string, error) {
	out, err := c.cmdFunc(ctx, "tmux", "capture-pane", "-p", "-e", "-t", sessionName).Output()
	if err != nil {
		return "", fmt.Errorf("capture styled pane %q: %w", sessionName, err)
	}
	return string(out), nil
}

// sendPlanOpts customizes SendPlan timing. Production callers never build one
// by hand: they go through startDeliveryOpts or sendDeliveryOpts, which stamp
// the readiness budget belonging to their delivery path. Tests inject
// aggressive timeouts so neither production budget gates them, and an injected
// positive deadline always wins over the client's configured one.
type sendPlanOpts struct {
	deadline         time.Duration
	pollInterval     time.Duration
	readyMarker      string
	submitVerifyWait time.Duration
	submitVerifyTick time.Duration

	// readyAttempts is how many times the READINESS WAIT — and nothing else —
	// may run before the delivery fails. It bounds only waitForReadyMarker,
	// which finishes before the first keystroke is sent, so raising it can
	// never cause a double delivery. Non-positive means
	// sendPlanDefaultReadyAttempts (one attempt).
	readyAttempts int

	// retried marks the options of an attempt made by the readiness RETRY loop,
	// as opposed to a delivery that gets exactly one wait. Only the retried path
	// enriches a context-cut failure with capture accounting and a pane
	// snapshot; a single-attempt wait keeps the message it returned before
	// BOS-895, which the established-send path is required to preserve.
	retried bool

	// clampedFrom is the per-attempt budget this attempt WOULD have had if the
	// caller's context had been long enough to fund it, set only when
	// clampAttemptDeadline actually shortened deadline. Zero means the attempt
	// got what it asked for. It exists so a timeout can say which of the two
	// numbers the operator is looking at.
	clampedFrom time.Duration

	// attemptStart is the one clock reading a retried attempt both clamps from
	// and counts its deadline from (clampAttemptDeadline). Zero means the wait
	// reads its own clock, which is the single-attempt path: it never clamps.
	attemptStart time.Time

	// readinessCapture replaces the readiness wait's capture-pane. Production
	// leaves it nil, which means CapturePane. A test of the wait's deadlines
	// sets it so the timing under test is the wait's own, not how long a loaded
	// host takes to fork a fake capture command.
	readinessCapture func(ctx context.Context, sessionName string) (string, error)

	// prefillOnly delivers the payload into the composer but sends no Enter and
	// runs no submission verification: the human (or composer owner) submits.
	// Used by the Prefill* wrappers; the Send* wrappers leave it false so they
	// submit and verify as before.
	prefillOnly bool

	// beforeSubmit runs after the payload is in the composer and immediately
	// before the first Enter is sent. It lets callers capture a baseline for
	// post-submit observation without moving that observation concern into tmux.
	beforeSubmit func()

	// modalDetector is the "is this a menu?" half of the readiness gate for
	// THIS delivery. It lives here, in the per-call options, rather than on
	// Client because the grammar is per agent — codex's glyphs are not Claude's
	// — while the daemon holds one long-lived Client shared by every chat. The
	// caller is the only layer that knows which agent owns this pane, so it is
	// the only layer that can supply the right grammar (BOS-600). Nil disables
	// the check; see ModalDetector in tmux_modal.go.
	modalDetector ModalDetector

	// pasteFreeText routes every free-text payload — single-line too — through
	// load-buffer/paste-buffer instead of literal keystrokes. Set for Claude by
	// deliveryOpts (BOS-1506): Claude reads any fast keystroke burst as a paste,
	// so typing a long line collapses it into "[Pasted text #N]<tail>" or drops
	// its head outright (testdata/panes/capture_paste.sh), and how fast is "fast"
	// depends on host load, so no length threshold is reliable. A single-line
	// slash command still types literally, which is what executes it.
	pasteFreeText bool

	// pasteSettleWait bounds the read-only wait between a pasteFreeText paste
	// and its Enter (awaitComposerSettled). Zero skips the wait.
	pasteSettleWait time.Duration

	// typedDrawWaits bounds each draw window. Growing partial input carries
	// into the next window; stalled or dropped input is cleared and retyped.
	// An empty schedule skips the wait.
	typedDrawWaits []time.Duration

	// submitted is the caller's independent proof that the agent took the
	// payload — for Claude, the UserPromptSubmit receipt. The verifier consults
	// it before every pane read and before the clear-and-redeliver retry, so a
	// receipt is authoritative and a pane the verifier cannot read is never
	// retried into a second copy. Nil leaves the pane verifier as the only
	// signal.
	submitted func() bool

	// receiptRequired says submitted records every turn the agent takes, so
	// the pane's "payload left the composer" reading cannot confirm a send
	// without it (SendHooks.ReceiptRequired).
	receiptRequired bool

	// receiptGrace lets a start receipt or late repaint settle before retry keys.
	// Established sends leave it zero to preserve the relay reservation.
	receiptGrace time.Duration
}

// pasteSettleWait is the production bound on awaitComposerSettled. A paste
// lands within a repaint or two; the wait only has to outlast a loaded host's
// lag, and it runs at most twice per send (first delivery and the one
// re-delivery), so it stays well inside the established-send relay
// reservation — see TestEstablishedSendTailFitsRelayReservation.
const pasteSettleWait = time.Second

// startReceiptGrace covers the measured ~3s typed draw lag plus the hook round
// trip. A hook that never fires delays the existing retry by this bound; it
// never suppresses that retry.
const startReceiptGrace = 4 * time.Second

// A Claude pane that has just drawn its composer can still be loading, and
// keys sent then are read late: the typed line and its Enter arrive in one read,
// which Claude treats as a paste, so the Enter becomes a newline and the next
// Enter is eaten too. Some typed lines are dropped outright. Measured on Claude
// Code 2.1.302 under load, a typed line took up to ~3s to draw. Session start
// therefore waits for the line to draw before Enter. Escalating windows give
// a slow composer time to read its keys without restarting a growing draw.
// An established chat is already reading its input, so it gets the paste
// settle bound and one attempt, which keeps its tail inside the relay
// reservation (TestEstablishedSendTailFitsRelayReservation).
func startTypedDrawWaits() []time.Duration {
	return []time.Duration{5 * time.Second, 10 * time.Second, 25 * time.Second}
}

func typedDrawBudget(opts sendPlanOpts) time.Duration {
	var total time.Duration
	for _, wait := range opts.typedDrawWaits {
		total += wait
	}
	return total
}

// startDeliveryOpts builds the production timing for a SESSION-START (or
// resume) delivery: this client's configured session-start readiness budget, or
// DefaultSessionStartReadyDeadline when nothing is configured, plus the shared
// poll/marker/submit settings below. The four public Send*/Prefill* wrappers
// share it, so a timing change to that path is one edit rather than four
// drifting literals.
//
// It takes a ModalDetector for the same reason sendDeliveryOpts does: since
// BOS-894 this path has …WithModal siblings that can supply one, and a pane the
// agent has only just drawn is the likeliest place to find a boot interstitial
// rather than a composer. The plain …WithReadyMarker wrappers pass nil, which
// disables the check exactly as before.
func (c *Client) startDeliveryOpts(readyMarker string, submit bool, detector ModalDetector) sendPlanOpts {
	attempts := c.sessionStartReadyAttempts
	if attempts <= 0 {
		attempts = sessionStartReadyAttempts
	}
	opts := c.deliveryOpts(c.sessionStartReadyDeadline, DefaultSessionStartReadyDeadline, attempts, readyMarker, submit, detector)
	if len(opts.typedDrawWaits) > 0 {
		opts.typedDrawWaits = startTypedDrawWaits()
	}
	return opts
}

// SessionStartReadyDeadline reports the composer-readiness budget this client
// applies to a SESSION-START (or resume) delivery: the operator-configured
// value when one was supplied through WithSessionStartReadyDeadline, otherwise
// DefaultSessionStartReadyDeadline.
//
// It reads the SAME field startDeliveryOpts reads and substitutes the same
// default, deliberately, so an out-of-package caller asking what a delivery
// will wait cannot get a different answer from the one the delivery uses.
//
// It is exported for exactly one caller: Server.executeAccountSwitch
// (services/bossd/internal/server) sizes the switch's respawn budget with
// config.SwitchRespawnBudgetFor over this value (BOS-948). The budget has to
// scale with the readiness wait it funds, and the daemon holds one long-lived
// client that already carries the resolved setting — so reading it here is both
// cheaper and more honest than re-reading settings.json per switch, which would
// quietly break the "takes effect on the next daemon restart" contract the
// setting documents.
func (c *Client) SessionStartReadyDeadline() time.Duration {
	if c == nil {
		return 0
	}
	if c.sessionStartReadyDeadline > 0 {
		return c.sessionStartReadyDeadline
	}
	return DefaultSessionStartReadyDeadline
}

// sendDeliveryOpts builds the production timing for a delivery into an
// ESTABLISHED chat's live composer: this client's configured send readiness
// budget, or DefaultSendReadyDeadline when nothing is configured.
//
// It substitutes its OWN default rather than sharing one with the start path.
// That is the whole point of the split: this delivery runs inside an RPC bosso
// relays under a fixed command deadline, so inheriting the generous
// session-start budget would let the daemon keep waiting past the point the
// relay has already returned CodeDeadlineExceeded — an ambiguous delivery the
// caller must not retry, since a retry double-types into the composer.
func (c *Client) sendDeliveryOpts(readyMarker string, submit bool, detector ModalDetector) sendPlanOpts {
	return c.deliveryOpts(c.sendReadyDeadline, DefaultSendReadyDeadline, sendReadyAttempts, readyMarker, submit, detector)
}

// deliveryOpts is the shared builder behind the two above: everything a
// delivery needs except the readiness budget, which its caller supplies as a
// (configured, fallback) pair so only that one value can differ between paths.
// It sets the 100ms poll, the agent's ready marker, and either the
// submit-verifier budget or the prefill (no-Enter) routing.
func (c *Client) deliveryOpts(configured, fallback time.Duration, attempts int, readyMarker string, submit bool, detector ModalDetector) sendPlanOpts {
	deadline := configured
	if deadline <= 0 {
		deadline = fallback
	}
	opts := sendPlanOpts{
		deadline:      deadline,
		pollInterval:  sendPlanDefaultPollInterval,
		readyMarker:   readyMarker,
		readyAttempts: attempts,
		modalDetector: detector,
		pasteFreeText: isClaudeComposer(readyMarker),
	}
	if submit {
		opts.submitVerifyWait = 2 * time.Second
		opts.submitVerifyTick = 100 * time.Millisecond
		if opts.pasteFreeText {
			opts.pasteSettleWait = pasteSettleWait
			if c.pasteSettleWait > 0 {
				opts.pasteSettleWait = c.pasteSettleWait
			}
			opts.typedDrawWaits = []time.Duration{opts.pasteSettleWait}
		}
	} else {
		opts.prefillOnly = true
	}
	return opts
}

// isClaudeComposer reports whether readyMarker names Claude's composer. The
// marker is the only per-agent fact this package receives; an empty marker is
// the legacy Claude default, exactly as sendPlan resolves it.
func isClaudeComposer(readyMarker string) bool {
	return readyMarker == "" || readyMarker == sendPlanReadyMarker
}

// resolveDeadlineFloor is the last line of defence for a hand-built
// sendPlanOpts (every test constructs one directly). A non-positive deadline
// must never reach the readiness wait, where it would mean "no wait at all";
// it falls back to the SESSION-START default, the conservative of the two,
// because a caller who supplied no budget has told us nothing about which path
// they are on.
func resolveDeadlineFloor(d time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return DefaultSessionStartReadyDeadline
}

// resolveReadyAttemptsFloor is the same last line of defence for the readiness
// attempt count. It mirrors resolveDeadlineFloor deliberately, but falls back
// the other way: an unspecified deadline takes the GENEROUS default because a
// too-short wait is the dangerous direction, while an unspecified attempt count
// takes the CONSERVATIVE one because an unasked-for retry is.
func resolveReadyAttemptsFloor(n int) int {
	if n > 0 {
		return n
	}
	return sendPlanDefaultReadyAttempts
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
// send-keys) returns non-zero. This is a session-start delivery, so the wait is
// bounded by the client's session-start budget (DefaultSessionStartReadyDeadline
// unless the operator raised it), not by the much shorter established-send one.
func (c *Client) SendPlan(ctx context.Context, sessionName, plan string) error {
	return c.SendPlanWithReadyMarker(ctx, sessionName, plan, sendPlanReadyMarker)
}

// WaitForBracketedPaste waits for the pane application to request bracketed
// paste before a multi-line payload is delivered. This uses the established
// chat-send budget: after the process has begun rendering, an enable sequence
// that does not arrive promptly is a broken readiness state, not a second
// session boot.
func (c *Client) WaitForBracketedPaste(ctx context.Context, logPath string) error {
	deadline := c.sendReadyDeadline
	if deadline <= 0 {
		deadline = DefaultSendReadyDeadline
	}
	return waitForBracketedPaste(ctx, logPath, deadline, sendPlanDefaultPollInterval)
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
	return c.SendPlanWithModal(ctx, sessionName, plan, readyMarker, nil)
}

// SendLineWithReadyMarker sends a short literal line and submits it with Enter.
// Use this for command invocations where bracketed paste can leave some TUIs
// with the command text loaded but not executed.
func (c *Client) SendLineWithReadyMarker(ctx context.Context, sessionName, line, readyMarker string) error {
	return c.SendLineWithModal(ctx, sessionName, line, readyMarker, nil)
}

// PrefillPlanWithReadyMarker delivers a plan into the composer WITHOUT
// submitting it: it waits for the ready marker and pastes/types the payload,
// but sends no Enter and runs no verification. Use it when the composer owner
// (a human, or a later explicit-submit step) will submit — the counterpart to
// SendPlanWithReadyMarker's auto-submit-and-verify behaviour.
func (c *Client) PrefillPlanWithReadyMarker(ctx context.Context, sessionName, plan, readyMarker string) error {
	return c.PrefillPlanWithModal(ctx, sessionName, plan, readyMarker, nil)
}

// PrefillLineWithReadyMarker delivers a short literal line into the composer
// WITHOUT submitting it (no Enter, no verification), mirroring
// SendLineWithReadyMarker for the prefill case.
func (c *Client) PrefillLineWithReadyMarker(ctx context.Context, sessionName, line, readyMarker string) error {
	return c.PrefillLineWithModal(ctx, sessionName, line, readyMarker, nil)
}

// The four …WithModal siblings are the wrappers above with the readiness gate's
// modal check bound for one delivery, refusing with ErrBlockedByModal
// (OutcomeBlockedByModal) when the pane turns out to be showing a menu rather
// than a composer. A nil detector makes each behave exactly like its
// …WithReadyMarker sibling, which is now literally how those are implemented:
// the pair share one body so the gate cannot be wired into one arm and missed
// in another.
//
// Four wrappers rather than one parameterised entry point because the four
// existing spellings already encode two orthogonal choices — plan vs line
// (bracketed paste vs literal keystrokes) and submit vs prefill (Enter and
// verification, or neither) — and collapsing them here would force every
// existing caller to restate a decision it has already made. SendMessage takes
// the opposite shape (one entry, routing on submit intent) because its callers
// carry a user's submit flag rather than a fixed delivery kind.
//
// These exist for the session-start path (BOS-894), which delivers the first
// message into a pane the agent has only just drawn — the one moment an agent
// is most likely to be showing a boot interstitial instead of a composer, and
// the one path that reached sendPlan with the detector hard-coded nil.

// SendPlanWithModal is SendPlanWithReadyMarker with the modal check bound.
func (c *Client) SendPlanWithModal(ctx context.Context, sessionName, plan, readyMarker string, detector ModalDetector) error {
	return c.SendPlanWithHooks(ctx, sessionName, plan, readyMarker, detector, SendHooks{})
}

// SendLineWithModal is SendLineWithReadyMarker with the modal check bound.
func (c *Client) SendLineWithModal(ctx context.Context, sessionName, line, readyMarker string, detector ModalDetector) error {
	return c.SendLineWithHooks(ctx, sessionName, line, readyMarker, detector, SendHooks{})
}

// SendPlanWithHooks binds observers to a session-start plan delivery.
func (c *Client) SendPlanWithHooks(ctx context.Context, sessionName, plan, readyMarker string, detector ModalDetector, hooks SendHooks) error {
	return c.sendPlan(ctx, sessionName, plan, c.startDeliveryOptsWithHooks(readyMarker, detector, hooks))
}

// SendLineWithHooks binds observers to a session-start literal command.
func (c *Client) SendLineWithHooks(ctx context.Context, sessionName, line, readyMarker string, detector ModalDetector, hooks SendHooks) error {
	return c.sendLine(ctx, sessionName, line, c.startDeliveryOptsWithHooks(readyMarker, detector, hooks))
}

func (c *Client) startDeliveryOptsWithHooks(readyMarker string, detector ModalDetector, hooks SendHooks) sendPlanOpts {
	opts := c.startDeliveryOpts(readyMarker, true, detector)
	opts.beforeSubmit = hooks.BeforeSubmit
	opts.submitted = hooks.Submitted
	opts.receiptRequired = hooks.ReceiptRequired && hooks.Submitted != nil
	if hooks.Submitted != nil {
		opts.receiptGrace = startReceiptGrace
	}
	return opts
}

// PrefillPlanWithModal is PrefillPlanWithReadyMarker with the modal check
// bound. The check applies to the prefill path exactly as it does to the submit
// path: a prefill sends no Enter, but it still TYPES into whatever has the
// keyboard, and on a menu those keystrokes are selection shortcuts.
func (c *Client) PrefillPlanWithModal(ctx context.Context, sessionName, plan, readyMarker string, detector ModalDetector) error {
	return c.sendPlan(ctx, sessionName, plan, c.startDeliveryOpts(readyMarker, false, detector))
}

// PrefillLineWithModal is PrefillLineWithReadyMarker with the modal check
// bound; see PrefillPlanWithModal for why prefill is gated too.
func (c *Client) PrefillLineWithModal(ctx context.Context, sessionName, line, readyMarker string, detector ModalDetector) error {
	return c.sendLine(ctx, sessionName, line, c.startDeliveryOpts(readyMarker, false, detector))
}

// WillSubmit reports whether SendMessage would take the submit path (deliver +
// Enter + verify) for this request, rather than routing it to a prefill. It is
// exported so callers that must describe the delivery they just asked for — the
// SendChatMessage RPC populating delivery_state — read the routing decision from
// the one place that makes it, instead of re-deriving it and drifting from it.
func WillSubmit(submit bool, text string) bool { return domain.WillSubmit(submit, text) }

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
//     verifies each shape against the matching signal one layer down. Claude
//     is the exception: every free-text payload is pasted and Enter waits for
//     the composer to settle, and only a single-line slash command is typed
//     (BOS-1506).
//   - !submit (prefill, the default): paste/type into the composer, NO Enter,
//     for the composer owner (a human, or a later explicit-submit step) to
//     submit.
//
// Both arms differ only in the options sendDeliveryOpts builds; neither routes
// through the Send/Prefill wrappers, so a reader chasing either path lands on
// sendPlan with the modal detector already threaded in. Both are established
// sends into a running agent, so both take the short send budget rather than the
// session-start one.
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
	return c.sendPlan(ctx, sessionName, text, c.sendDeliveryOpts(readyMarker, WillSubmit(submit, text), detector))
}

// SendMessageWithModal is SendMessage with the readiness gate's modal check
// bound to detector for this one delivery, refusing with ErrBlockedByModal
// (OutcomeBlockedByModal) when the pane is showing a menu rather than a
// composer. A nil detector behaves exactly like SendMessage.
func (c *Client) SendMessageWithModal(ctx context.Context, sessionName, text string, submit bool, readyMarker string, detector ModalDetector) error {
	return c.sendMessage(ctx, sessionName, text, submit, readyMarker, detector)
}

// SendHooks are the caller-side observers of one delivery. Neither may
// mutate tmux state.
type SendHooks = domain.SendHooks

// SendMessageWithHooks is SendMessageWithModal with the caller's SendHooks
// bound to this one delivery.
func (c *Client) SendMessageWithHooks(ctx context.Context, sessionName, text string, submit bool, readyMarker string, detector ModalDetector, hooks SendHooks) error {
	opts := c.sendDeliveryOpts(readyMarker, WillSubmit(submit, text), detector)
	opts.beforeSubmit = hooks.BeforeSubmit
	opts.submitted = hooks.Submitted
	opts.receiptRequired = hooks.ReceiptRequired && hooks.Submitted != nil
	return c.sendPlan(ctx, sessionName, text, opts)
}

// sendPlan is the test-injectable variant of SendPlan that accepts custom
// timing. Both production code and tests funnel through here.
func (c *Client) sendPlan(ctx context.Context, sessionName, plan string, opts sendPlanOpts) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	opts.deadline = resolveDeadlineFloor(opts.deadline)
	if opts.pollInterval <= 0 {
		opts.pollInterval = sendPlanDefaultPollInterval
	}
	if opts.readyMarker == "" {
		opts.readyMarker = sendPlanReadyMarker
	}
	opts.readyAttempts = resolveReadyAttemptsFloor(opts.readyAttempts)

	// Step 1: poll capture-pane for the ready marker. This is the ONLY step the
	// retry in waitForReadyMarkerWithAttempts covers; every line below it has
	// already touched the pane.
	if err := c.waitForReadyMarkerWithAttempts(ctx, sessionName, opts); err != nil {
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
	//
	// Claude (opts.pasteFreeText) is the exception to the literal single-line
	// path: its TUI reads a fast keystroke burst as a paste, so a typed long line
	// is collapsed or loses its head (BOS-1506). Every Claude free-text payload
	// is pasted instead — the trimmed text for a single line, so the paste adds
	// no stray newline — and Enter waits for the composer to settle. A
	// single-line slash command keeps the literal path, which is what makes
	// Claude execute it.
	trimmedPlan := strings.TrimSpace(plan)
	singleLine := trimmedPlan != "" && !strings.ContainsAny(trimmedPlan, "\r\n")
	pasted := plan
	if singleLine {
		pasted = trimmedPlan
	}
	typed := singleLine && (!opts.pasteFreeText || strings.HasPrefix(trimmedPlan, "/"))
	if !opts.pasteFreeText {
		pasted = plan
	}
	drawn := composerDrawing{
		text:      plan,
		collapsed: !typed && isClaudeComposer(opts.readyMarker) && claudeCollapsesPaste(pasted),
	}
	settle := opts.pasteFreeText && !opts.prefillOnly && opts.pasteSettleWait > 0
	deliver := func(ctx context.Context) error {
		if typed {
			return c.typeLineAwaitDrawn(ctx, sessionName, trimmedPlan, opts)
		}
		var before string
		if settle {
			before = c.composerRowsSnapshot(ctx, sessionName)
		}
		drawn.placeholder = ""
		if err := c.pasteBufferNoEnter(ctx, sessionName, pasted); err != nil {
			return err
		}
		if settle {
			settled := c.awaitComposerSettled(ctx, sessionName, before, opts.pasteSettleWait, opts.submitVerifyTick)
			if drawn.collapsed {
				drawn.placeholder = placeholderIn(settled, drawn.text)
			}
		}
		return nil
	}
	if err := c.refuseOccupiedComposer(ctx, sessionName, opts); err != nil {
		return err
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
	if opts.beforeSubmit != nil {
		opts.beforeSubmit()
	}
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
		if err := c.verifyWithEnterRetry(ctx, sessionName, drawn, opts, deliver); err != nil {
			return c.withdrawUnsubmitted(ctx, sessionName, drawn, err)
		}
	}
	return nil
}

// withdrawBudget bounds withdrawUnsubmitted, which runs after the send's own
// budget may already be spent.
const withdrawBudget = 5 * time.Second

// withdrawUnsubmitted removes the payload a not-submitted send leaves in the
// composer, so the send leaves the pane as it found it. Without it the payload
// strands every later send: the next one, including the caller's retry of this
// payload, finds the box occupied and is refused. A retry is often not the same
// text, since the callback worker marks every retry "REPEAT DELIVERY — attempt
// N", so the box cannot be matched against the retry instead.
//
// It presses C-u only while Claude's ruled input box holds exactly what this
// send drew (composerDrawing.fills), exactly the part of it the Enter retry's
// presses left (composerDrawing.leftByRetry), or, after a press of its own,
// exactly what that press leaves (pressLeft). It reads the box afresh before
// every press. Typing appends, so such a box held nothing when this send typed
// into it, and nobody has typed into it since the read. Anything else is left
// as it is, and the error says so (withdrawal). A rule-less composer (Codex) is
// never withdrawn from: its box ends at a blank row, which is also how a
// draft's second paragraph starts, so what it holds cannot be read. Codex
// confirms a submit from its rollout anyway.
func (c *Client) withdrawUnsubmitted(ctx context.Context, sessionName string, drawn composerDrawing, sendErr error) error {
	if OutcomeOf(sendErr) != OutcomeNotSubmitted {
		return sendErr
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), withdrawBudget)
	defer cancel()
	result := func(end error) error {
		return classifySubmit(OutcomeNotSubmitted, fmt.Errorf("%w; %w", sendErr, end))
	}
	styled, err := c.captureVisibleStyled(ctx, sessionName)
	if err != nil {
		return result(fmt.Errorf("%w: the composer could not be read: %v", withdrawFailed, err))
	}
	box, ruled := ruledComposerText(styled, drawn.text)
	switch {
	case !ruled:
		return result(withdrawUnruled)
	case composerTextIsEmpty(box):
		return result(withdrawAlreadyEmpty)
	case !drawn.fills(box) && !drawn.leftByRetry(box, sendErr):
		return result(withdrawHeldOther)
	}
	return result(c.pressOutPayload(ctx, sessionName, drawn, box))
}

// withdrawal is how withdrawUnsubmitted ended. The not-submitted error wraps
// it, so the caller's message says what became of the payload.
type withdrawal string

func (w withdrawal) Error() string { return string(w) }

const (
	withdrawn            withdrawal = "the payload was withdrawn from the composer"
	withdrawUnruled      withdrawal = "the payload was left in the composer, because bossd withdraws only from Claude's bordered input box and found none"
	withdrawAlreadyEmpty withdrawal = "the composer was already empty"
	withdrawHeldOther    withdrawal = "the composer holds text other than the payload, so it was left as it is"
	withdrawUnchanged    withdrawal = "clearing the composer changed nothing, so the payload may still be in it"
	withdrawForeign      withdrawal = "the composer changed in a way a clear cannot explain, so it was left as it is"
	withdrawVanished     withdrawal = "the input box disappeared while the payload was withdrawn"
	withdrawPressCap     withdrawal = "the composer still holds part of the payload after every clear it should take"
	withdrawFailed       withdrawal = "withdrawing the payload from the composer failed"
)

// pressOutPayload presses C-u until the composer is empty, and says how it
// ended. box is what the composer held on the read just before the first
// press. Each later press acts only on the read just before it, and only when
// that read shows exactly what the press before leaves of the one before it
// (pressLeft).
func (c *Client) pressOutPayload(ctx context.Context, sessionName string, drawn composerDrawing, box string) error {
	presses := drawn.pressesToEmpty(box)
	for press := 1; ; press++ {
		if err := c.clearComposer(ctx, sessionName); err != nil {
			return fmt.Errorf("%w: %v", withdrawFailed, err)
		}
		next, ruled, err := c.awaitRuledComposerChange(ctx, sessionName, drawn.text, box)
		switch {
		case err != nil:
			return fmt.Errorf("%w: the composer could not be read after a clear: %v", withdrawFailed, err)
		case !ruled:
			return withdrawVanished
		case composerTextIsEmpty(next):
			return withdrawn
		case composerShape(next) == composerShape(box):
			return withdrawUnchanged
		case !pressLeft(box, next):
			return withdrawForeign
		case press >= presses:
			return withdrawPressCap
		}
		box = next
	}
}

// awaitRuledComposerChange polls the ruled composer until its rows differ from
// before, and returns the last reading, which is unchanged when the window ran
// out. A frame drawn without the box is a repaint and is skipped.
func (c *Client) awaitRuledComposerChange(ctx context.Context, sessionName, payload, before string) (string, bool, error) {
	deadline := time.Now().Add(clearComposerCheckWindow)
	text, ruled := before, false
	for {
		styled, err := c.captureVisibleStyled(ctx, sessionName)
		if err != nil {
			return "", false, err
		}
		if now, ok := ruledComposerText(styled, payload); ok {
			text, ruled = now, true
			if composerShape(now) != composerShape(before) {
				return text, ruled, nil
			}
		}
		if !time.Now().Before(deadline) {
			return text, ruled, nil
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(clearComposerCheckTick):
		}
	}
}

// sendLine is the test-injectable variant of SendLineWithReadyMarker.
func (c *Client) sendLine(ctx context.Context, sessionName, line string, opts sendPlanOpts) error {
	if sessionName == "" {
		return fmt.Errorf("session name is required")
	}
	opts.deadline = resolveDeadlineFloor(opts.deadline)
	if opts.pollInterval <= 0 {
		opts.pollInterval = sendPlanDefaultPollInterval
	}
	if opts.readyMarker == "" {
		opts.readyMarker = sendPlanReadyMarker
	}
	opts.readyAttempts = resolveReadyAttemptsFloor(opts.readyAttempts)

	if err := c.waitForReadyMarkerWithAttempts(ctx, sessionName, opts); err != nil {
		return err
	}
	// Claude draws an argument hint after a slash command that ends in a space,
	// so a line typed with surrounding whitespace may never draw exactly as
	// typed. The line typed and checked for is trimmed, as sendPlan's is.
	line = strings.TrimSpace(line)
	if err := c.refuseOccupiedComposer(ctx, sessionName, opts); err != nil {
		return err
	}
	// A closure so the verifier's retry can re-run the identical delivery after
	// clearing the composer (see verifyWithEnterRetry).
	deliver := func(ctx context.Context) error {
		return c.typeLineAwaitDrawn(ctx, sessionName, line, opts)
	}
	if err := deliver(ctx); err != nil {
		return err
	}

	// Prefill delivery stops before Enter: the composer owner submits.
	if opts.prefillOnly {
		return nil
	}

	if opts.beforeSubmit != nil {
		opts.beforeSubmit()
	}
	if err := c.sendEnter(ctx, sessionName); err != nil {
		return err
	}
	if opts.submitVerifyWait > 0 {
		drawn := composerDrawing{text: line}
		if err := c.verifyWithEnterRetry(ctx, sessionName, drawn, opts, deliver); err != nil {
			return c.withdrawUnsubmitted(ctx, sessionName, drawn, err)
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
	textCmd.WaitDelay = tmuxCommandWaitDelay
	if err := textCmd.Run(); err != nil {
		if msg := strings.TrimSpace(textStderr.String()); msg != "" {
			return fmt.Errorf("tmux send-keys literal line for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux send-keys literal line for %q: %w", sessionName, err)
	}
	return nil
}

// typeLineAwaitDrawn waits for a typed line before sending Enter. A growing
// prefix keeps its place across draw windows; only dropped or stalled input
// is retyped. Failure clears the line so a later retry cannot duplicate it.
func (c *Client) typeLineAwaitDrawn(ctx context.Context, sessionName, line string, opts sendPlanOpts) error {
	if len(opts.typedDrawWaits) == 0 {
		return c.typeLiteralLineNoEnter(ctx, sessionName, line)
	}
	needsTyping, partialSeen, prefixLen := true, false, 0
	for attempt, wait := range opts.typedDrawWaits {
		if needsTyping {
			if err := c.typeLiteralLineNoEnter(ctx, sessionName, line); err != nil {
				return err
			}
		}
		draw, err := c.awaitComposerDraws(ctx, sessionName, line, wait, opts.submitVerifyTick, prefixLen)
		partialSeen = partialSeen || draw.partialSeen
		if err == nil && draw.drawn {
			return nil
		}
		var verdict error
		switch {
		case err != nil:
			verdict = fmt.Errorf("read composer after typing into tmux session %q, so no Enter was sent: %w", sessionName, err)
		case attempt == len(opts.typedDrawWaits)-1:
			verdict = fmt.Errorf("command was not submitted to tmux session %q: the composer did not draw the typed line within %s over %d attempts, so no Enter was sent; partial draw seen: %t; it last showed %s", sessionName, typedDrawBudget(opts), len(opts.typedDrawWaits), partialSeen, composerRowsForError(draw.rows))
		}
		if verdict == nil && draw.growing && draw.prefixLen > 0 {
			needsTyping, prefixLen = false, draw.prefixLen
			continue
		}
		clearCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), typedLineClearTimeout)
		clearErr := c.clearTypedLine(clearCtx, sessionName, line)
		cancel()
		if verdict != nil || clearErr != nil {
			return classifySubmit(OutcomeNotSubmitted, errors.Join(verdict, clearErr))
		}
		needsTyping, prefixLen = true, 0
	}
	return nil
}

// typedLineClearTimeout bounds each clear typeLineAwaitDrawn sends. The clear
// runs even once the caller's context is done, because the typed keys are in
// the pane either way.
const typedLineClearTimeout = 2 * time.Second

// shellModePrefix is the leading character that switches Claude's composer to
// shell mode. Claude then draws it as the box's glyph in place of the prompt
// marker (testdata/panes/claude_typed_shell_mode_real.txt).
const shellModePrefix = "!"

// clearTypedLine empties the composer of a typed line that did not draw. Shell
// mode survives C-u, and a "!" typed into it draws a second "!", so a shell-mode
// line also gets a BSpace, which leaves shell mode once the composer is empty.
// Measured on Claude Code 2.1.302.
func (c *Client) clearTypedLine(ctx context.Context, sessionName, line string) error {
	if strings.HasPrefix(line, shellModePrefix) {
		return c.sendNamedKeys(ctx, sessionName, "C-u", "BSpace")
	}
	return c.clearComposer(ctx, sessionName)
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
	loadCmd.WaitDelay = tmuxCommandWaitDelay
	if err := loadCmd.Run(); err != nil {
		if msg := strings.TrimSpace(loadStderr.String()); msg != "" {
			return fmt.Errorf("tmux load-buffer for %q: %w (stderr: %s)", sessionName, err, msg)
		}
		return fmt.Errorf("tmux load-buffer for %q: %w", sessionName, err)
	}

	pasteCmd := c.cmdFunc(ctx, "tmux", "paste-buffer", "-d", "-p", "-t", sessionName)
	var pasteStderr bytes.Buffer
	pasteCmd.Stderr = &pasteStderr
	pasteCmd.WaitDelay = tmuxCommandWaitDelay
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
	return c.sendNamedKeys(ctx, sessionName, "Enter")
}

// sendNamedKeys presses the named tmux keys, in order, in one send-keys call.
// stderr from tmux is wrapped into the returned error.
func (c *Client) sendNamedKeys(ctx context.Context, sessionName string, keys ...string) error {
	cmd := c.cmdFunc(ctx, "tmux", append([]string{"send-keys", "-t", sessionName}, keys...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxCommandWaitDelay
	if err := cmd.Run(); err != nil {
		named := strings.Join(keys, " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("tmux send-keys %s for %q: %w (stderr: %s)", named, sessionName, err, msg)
		}
		return fmt.Errorf("tmux send-keys %s for %q: %w", named, sessionName, err)
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
	return c.sendNamedKeys(ctx, sessionName, "C-u")
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

// NewSessionWithCmd starts a chat pane that remains visible when its agent exits.
func (c *Client) NewSessionWithCmd(ctx context.Context, name, workDir string, command []string, env map[string]string) error {
	return c.NewSession(ctx, NewSessionOpts{Name: name, WorkDir: workDir, Command: command, Env: env, RemainOnExit: true})
}
