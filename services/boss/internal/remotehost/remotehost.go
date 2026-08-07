// Package remotehost drives a bossd running on another machine over SSH: it
// discovers the remote daemon's unix socket, fetches that daemon's shared-secret
// auth token, and (via the tunnel supervisor) forwards the socket to a local one
// so the ordinary boss client can talk to it unmodified.
//
// The central invariant is that the SSH destination is opaque. `--host` takes a
// standard ssh destination — `user@host`, a bare `host`, or a `~/.ssh/config`
// `Host` alias — and this package hands that string to ssh verbatim: it never
// parses it, never splits on "@", never substitutes a local username, and never
// adds `-l`. That is what makes every `~/.ssh/config` directive the user already
// relies on (User, Port, IdentityFile, ProxyJump, HostName) keep working for
// free. A second, partial username resolver here would silently diverge from
// ssh's own resolution for exactly the config-driven setups this feature exists
// to support. Every invocation — discovery, the token fetch, and the tunnel's
// forward — must carry the identical destination argument, or they could resolve
// to different remote accounts and the token would not match the socket.
package remotehost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/socketauth"
)

// CommandFactory creates exec.Cmd instances. Allows injection for testing.
type CommandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Options configures every SSH invocation this package makes.
type Options struct {
	// Destination is the SSH destination exactly as the user typed it. It is
	// handed to ssh unparsed — see the package doc.
	Destination string
	// RemoteSocket, when non-empty, overrides discovery (the --host-socket flag).
	RemoteSocket string
	// ControlPath, when non-empty, is the multiplexing socket mastered by the
	// session's Tunnel (Tunnel.ControlPath). The upload helpers pass it to ssh
	// so a paste rides that already authenticated connection instead of opening
	// its own — the only thing that makes pasting work for a user on password or
	// uncached-passphrase auth, since those helpers must run under BatchMode.
	//
	// It is deliberately consulted by the upload helpers alone. Discovery and
	// the token fetch run before the tunnel exists, so there is no master for
	// them to ride, and they are the two calls that may still prompt.
	ControlPath string
	// CommandFactory is injected in tests; nil means exec.CommandContext.
	CommandFactory CommandFactory
}

// commandFactory returns the injected factory, defaulting to the real one. This
// mirrors the tmux client's WithCommandFactory pattern; Options is a plain
// struct here because callers build it from flags rather than functional opts.
func (o Options) commandFactory() CommandFactory {
	if o.CommandFactory != nil {
		return o.CommandFactory
	}
	return exec.CommandContext
}

// Sentinel errors, wrapped by the returned errors so callers can errors.Is them.
var (
	// ErrRemoteBossMissing means `boss` is not on the remote PATH. A
	// non-interactive ssh login gets a reduced PATH, so this is the most common
	// first failure and its message points at the --host-socket escape hatch.
	ErrRemoteBossMissing = errors.New("remotehost: remote boss binary not found")
	// ErrRemoteDaemonDown means the remote `boss` ran fine but reported no
	// reachable daemon. Deliberately distinct from ErrRemoteBossMissing: the
	// remedy is "start bossd there", not "fix PATH".
	ErrRemoteDaemonDown = errors.New("remotehost: remote bossd is not running")
	// ErrNoDestination means --host was empty; no ssh call is attempted.
	ErrNoDestination = errors.New("remotehost: no ssh destination")
)

// remoteEnvReport is a minimal local mirror of the `boss env --json` schema —
// only the two fields this package reads. The full report lives in package main
// (services/boss/cmd/env.go) and cannot be imported; duplicating a small type
// across a boundary is the repo convention. encoding/json ignores every other
// field in the (MarshalIndent-formatted) report, so the full payload parses.
type remoteEnvReport struct {
	Daemon struct {
		Socket    string `json:"socket"`
		Reachable bool   `json:"reachable"`
	} `json:"daemon"`
}

// DiscoverSocket returns the remote daemon's unix socket path.
//
// When opts.RemoteSocket is set it is returned as-is with no ssh call at all —
// that is the escape hatch for remotes where `boss` is not on the ssh PATH.
// Otherwise it runs `ssh <destination> boss env --json`, which reports the
// daemon block even from a plain non-managed remote shell.
func DiscoverSocket(ctx context.Context, opts Options) (string, error) {
	// Validate the destination even for the override path: the caller needs it
	// for the token fetch and the tunnel regardless, so failing here surfaces
	// the problem at the first call rather than the third.
	if strings.TrimSpace(opts.Destination) == "" {
		return "", ErrNoDestination
	}
	if opts.RemoteSocket != "" {
		return opts.RemoteSocket, nil
	}

	stdout, stderr, err := runSSH(ctx, opts, "boss", "env", "--json")
	if err != nil {
		if isCommandNotFound(err, stderr) {
			return "", fmt.Errorf("%w: `boss` is not on the PATH for %s "+
				"(a non-interactive ssh login gets a reduced PATH); "+
				"install boss there or pass --host-socket <path> to skip discovery: %w",
				ErrRemoteBossMissing, opts.Destination, err)
		}
		return "", fmt.Errorf("remotehost: `ssh %s boss env --json` failed: %w%s",
			opts.Destination, err, stderrDetail(stderr))
	}

	var rep remoteEnvReport
	if jsonErr := json.Unmarshal(stdout, &rep); jsonErr != nil {
		return "", fmt.Errorf("remotehost: could not parse `boss env --json` from %s: %w",
			opts.Destination, jsonErr)
	}
	if !rep.Daemon.Reachable {
		return "", fmt.Errorf("%w: no bossd is reachable on %s; start it there (`bossd` / `boss daemon start`)",
			ErrRemoteDaemonDown, opts.Destination)
	}
	if rep.Daemon.Socket == "" {
		return "", fmt.Errorf("remotehost: %s reported a reachable daemon with no socket path",
			opts.Destination)
	}
	return rep.Daemon.Socket, nil
}

// FetchToken returns the remote daemon's shared-secret auth token, read over the
// SAME ssh destination used for discovery so the token cannot belong to a
// different remote account than the socket.
//
// The token is returned in memory and MUST NOT be written to disk, logged, or
// included in any error message — hence no branch below splices command stdout
// (which is the token) or the parsed value into an error.
func FetchToken(ctx context.Context, opts Options, remoteSocket string) (string, error) {
	if strings.TrimSpace(opts.Destination) == "" {
		return "", ErrNoDestination
	}
	if remoteSocket == "" {
		remoteSocket = opts.RemoteSocket
	}
	if remoteSocket == "" {
		return "", fmt.Errorf("remotehost: no remote socket path for %s", opts.Destination)
	}

	tokenPath := remoteTokenPath(remoteSocket)
	// The remote command string is reassembled and run by the remote *shell*,
	// so the path must be quoted for that shell, not just passed as one argv
	// element locally.
	stdout, stderr, err := runSSH(ctx, opts, "cat", shellQuote(tokenPath))
	if err != nil {
		// Deliberately no stdout here: on a partial-read failure stdout is the
		// secret itself.
		return "", fmt.Errorf("remotehost: could not read the bossd token at %s on %s: %w%s",
			tokenPath, opts.Destination, err, stderrDetail(stderr))
	}
	tok, err := socketauth.ValidateToken(string(stdout))
	if err != nil {
		// %w carries socketauth's sentinel; the offending value is never echoed.
		return "", fmt.Errorf("remotehost: bossd token at %s on %s is unusable: %w",
			tokenPath, opts.Destination, err)
	}
	return tok, nil
}

// remoteTokenPath mirrors socketauth.TokenPath — the token file is co-located
// with the socket — but uses the slash-based `path` package rather than
// path/filepath: the remote path is always POSIX even when this code is compiled
// on Windows, where filepath.Join would emit backslashes.
func remoteTokenPath(remoteSocket string) string {
	return path.Join(path.Dir(remoteSocket), "bossd.token")
}

// terminfoProbeTimeout bounds one remote terminfo probe. It is deliberately
// longer than termnorm's 2s local probeTimeout, which bounds a subprocess on
// this machine: this one bounds a network round trip, and on a cold control
// master that includes a full TCP connect and ssh handshake to a host that may
// be several hops away.
const terminfoProbeTimeout = 5 * time.Second

// terminfoKey is the probe cache key. Neither half identifies a result on its
// own: the same TERM resolves on one host and not the next — which is the whole
// reason this probe exists — and one host answers differently per TERM.
type terminfoKey struct{ destination, term string }

// probeableTerm reports whether term is a name worth asking a remote shell
// about. Terminfo entry names are drawn from this class in practice
// (`xterm-256color`, `screen.xterm-256color`, `xterm+256setaf`), and restricting
// to it before quoting is what makes the two shell parses below compose safely —
// the same character-class-first rule upload.go's posixShell documents, where an
// unrestricted backslash inside single quotes was observed re-splitting into a
// destructive command under a fish outer shell.
//
// An empty or unrepresentable name is not "could not answer": no host has a
// terminfo entry under such a name, so the honest answer is absent, which is
// also what the local infocmp probe returns for one.
func probeableTerm(term string) bool {
	if term == "" {
		return false
	}
	for _, r := range term {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '+':
		default:
			return false
		}
	}
	return true
}

// remoteLoginShell renders command as the single remote command string ssh
// sends, routed through the remote user's LOGIN shell — the same wrapper
// internal/views' remoteTmuxCommand puts around the tmux attach this probe
// predicts for, duplicated rather than imported because views depends on this
// package and not the reverse.
//
// Matching the attach is the point. `ssh host infocmp …` runs in a NON-login
// shell, so it would consult a different PATH and a different terminfo search
// path than the remote tmux does: an infocmp outside sshd's reduced PATH answers
// "not installed" on exactly the hosts most likely to need this fix, and a
// TERMINFO/TERMINFO_DIRS exported from ~/.zprofile is visible to the login-shell
// tmux while invisible to a bare probe — which exits 1 and downgrades a TERM
// that would have resolved. Fail-open does not cover that second direction, so
// asking in the environment tmux itself runs in is what closes it.
//
// Spelled with a bare "$SHELL" for the reason remoteTmuxCommand documents: the
// POSIX default-value form ${SHELL:-/bin/sh} is a syntax error in fish, a real
// remote login shell, and sshd always sets SHELL from the passwd entry anyway.
//
// Two costs come with sourcing the login files, and both are accepted. Their
// stderr joins the probe's, which is why terminfoProbeMissing matches a phrase
// naming infocmp rather than the bare one isCommandNotFound takes. And a login
// file that itself fails (a `set -e` ~/.profile) surfaces as exit 1 and reads as
// "absent", downgrading a TERM that would have resolved — the safe direction,
// since tmux still starts, and the price of asking where tmux will look.
func remoteLoginShell(command string) string {
	return `exec "$SHELL" -lc ` + shellQuote(command)
}

var (
	terminfoMu    sync.Mutex
	terminfoCache = map[terminfoKey]bool{}
)

// resetTerminfoCache empties the probe cache. Tests only: it exists so each one
// starts from an unprobed state. Nothing in production ever clears the cache —
// the answer is a property of the remote box, and re-asking is the stall the
// cache exists to prevent.
func resetTerminfoCache() {
	terminfoMu.Lock()
	defer terminfoMu.Unlock()
	terminfoCache = map[terminfoKey]bool{}
}

// TerminfoPresent reports whether a terminfo entry for term exists on the remote
// host named by opts.Destination, by running `infocmp <term>` there through the
// remote LOGIN shell — the wrapper is load-bearing, not incidental, so it is in
// this sentence rather than only in the paragraph below that explains it. True
// means "use the caller's real TERM"; false means the entry is genuinely absent
// there and the caller should fall back.
//
// The run goes through the upload helpers' ssh options (see uploadSSHOptions),
// so it rides the session's control master when there is one and always runs
// under BatchMode: this is called from the Bubble Tea Update path, where a
// credential prompt would wedge the pane rather than be answerable.
//
// It classifies the outcome in this order, and everything that is not a clean
// answer fails OPEN:
//   - err == nil — the entry is present — true.
//   - errors.Is(err, context.DeadlineExceeded) or context.Canceled — the probe
//     timed out or was cancelled, so it never answered — true.
//   - remote 127, or a shell message naming infocmp specifically: there is no
//     infocmp there to ask — true.
//   - an *exec.ExitError with code 1 — infocmp's clean "no such entry" signal —
//     false. This is the only branch that may downgrade the user's TERM.
//   - anything else — an ssh transport failure (ssh exits 255), a spawn error,
//     any other status — true.
//
// The two timeout-ish branches split by when the deadline lands: a probe killed
// mid-flight surfaces as `signal: killed` (an *exec.ExitError with code -1) and
// falls to the last branch, while the context branch catches a ctx already done
// before ssh is started. Both fail open, which is why the split is harmless.
//
// Failing open is the right default because the two failure modes are not
// symmetric: silently downgrading the user's real terminal, or refusing to
// launch, because the probe could not run is worse than attempting the real
// TERM and letting tmux say so. termnorm.terminfoPresent makes the same choice
// locally when infocmp is not installed.
//
// The probe runs through the remote LOGIN shell (remoteLoginShell), so it reads
// the PATH and terminfo search path the remote tmux will — closely, not
// identically, and the answer is a good prediction rather than a guarantee. The
// entry tmux needs is resolved by the ncurses TMUX is linked against (in the
// client since 3.2, in the server before that), which need not be the one behind
// this infocmp: a keg-only or newer ncurses on the remote can compile in a
// different terminfo directory, and a pre-3.2 server inherited its environment
// from whatever started it rather than from this login shell. Both diverge only
// into fail-open — the real TERM is kept and tmux reports what it reports, just
// as today.
//
// Two further imprecisions are knowingly accepted, both on the safe side. A
// TERMINFO or TERMINFO_DIRS exported only from an INTERACTIVE rc (~/.zshrc,
// ~/.bashrc) is invisible to this probe — and to the tmux the attach starts,
// which is not interactive either. And the attach runs under `ssh -t` while this
// does not (a second pty would fight the live TUI for the terminal), so a login
// file guarded on `[ -t 1 ]` is visible to the attach alone; that one can
// downgrade a TERM tmux would have resolved. The canonical remedy
// termnorm.InstallHint gives writes to ~/.terminfo, which ncurses searches
// unconditionally in every case, and fail-open covers the rest — so no further
// machinery is warranted.
//
// The result is cached per (destination, term) for the process lifetime,
// including the fail-open ones: re-probing a broken host on every Update tick
// is precisely the stall this cache exists to prevent.
//
// Two inputs answer with no ssh call at all. An empty or whitespace-only
// destination returns true — a caller with no destination is not remote, so
// there is nothing to fail closed about. A term that is empty or not a plausible
// terminfo name (probeableTerm) returns FALSE: no host has an entry under such a
// name, so the caller must fall back, exactly as termnorm's local probe does for
// an empty TERM. Answering true there would ship `TERM=` to the remote tmux and
// reproduce the very "missing or unsuitable terminal" this exists to prevent.
func TerminfoPresent(ctx context.Context, opts Options, term string) bool {
	if strings.TrimSpace(opts.Destination) == "" {
		return true
	}
	if !probeableTerm(term) {
		return false
	}
	key := terminfoKey{destination: opts.Destination, term: term}

	// The lock is held across the probe itself rather than only around the map
	// so that "at most one probe per key per process" holds under concurrent
	// callers: a check-then-act window would let two probes race exactly when
	// the host is slow to answer, which is when the second one costs most.
	// Probes are rare (one per key, ever) and each is bounded below.
	terminfoMu.Lock()
	defer terminfoMu.Unlock()
	if present, cached := terminfoCache[key]; cached {
		return present
	}

	probeCtx, cancel := context.WithTimeout(ctx, terminfoProbeTimeout)
	defer cancel()
	// term is quoted twice over: the remote login shell re-parses ssh's
	// concatenated trailing arguments, and remoteLoginShell's `-lc` operand is
	// parsed again by the shell it execs. probeableTerm above is what lets the
	// two layers compose — see its doc and shellQuote's.
	_, stderr, err := runUploadSSH(probeCtx, opts, nil, remoteLoginShell("infocmp "+shellQuote(term)))

	present := terminfoProbeResult(err, stderr)
	terminfoCache[key] = present
	return present
}

// terminfoProbeResult classifies one probe run. The ordering and the
// fail-open rule it implements are documented on TerminfoPresent.
func terminfoProbeResult(err error, stderr []byte) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// ssh could not be started at all.
		return true
	}
	if exitErr.ExitCode() == 127 || terminfoProbeMissing(stderr) {
		return true
	}
	if exitErr.ExitCode() == 1 {
		return false
	}
	return true
}

// terminfoProbeMissing reports whether the remote shell said it could not find
// infocmp, for the shells and ssh wrappers that report a status other than 127.
//
// It deliberately does NOT reuse isCommandNotFound, which matches the bare
// phrase anywhere in stderr. That is safe for DiscoverSocket, whose only use of
// the answer is choosing an error message, and unsafe here: the probe runs
// through the remote LOGIN shell, so /etc/profile, ~/.profile, ~/.bash_profile,
// ~/.zprofile — exactly where guarded pyenv/nvm/rbenv init blocks live — get
// sourced ahead of it, and one printing `bash: line 1: pyenv: command not found`
// alongside infocmp's own exit-1 "absent" would flip a real answer to fail open
// and leave the user's unresolvable TERM in place. views.remoteCommandMissing
// was hardened against this same collision for the tmux liveness probe.
//
// The three shell spellings below are the whole vocabulary; anything else (fish
// says `Unknown command`) is caught by the 127 exit code, which no rc-file noise
// can forge.
func terminfoProbeMissing(stderr []byte) bool {
	msg := string(stderr)
	for _, phrase := range []string{
		"infocmp: command not found", // bash
		"command not found: infocmp", // zsh
		"infocmp: not found",         // dash / ash
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

// runSSH invokes `ssh <destination> <remoteArgs...>` and returns stdout and
// stderr separately. Separation matters for FetchToken, where stdout is a secret
// and only stderr may appear in an error.
//
// It delegates to runSSHStdin (upload.go) so the destination-first argv
// construction — the invariant the package doc is about — has one implementation
// rather than one per caller shape.
func runSSH(ctx context.Context, opts Options, remoteArgs ...string) (stdout, stderr []byte, err error) {
	return runSSHStdin(ctx, opts, nil, remoteArgs...)
}

// isCommandNotFound reports whether a failed ssh run means the remote command
// was not on the remote PATH. ssh propagates the remote shell's 127, but some
// shells and ssh wrappers report a different status, so the shell's own message
// is checked too.
func isCommandNotFound(err error, stderr []byte) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 127 {
		return true
	}
	msg := string(stderr)
	return strings.Contains(msg, "command not found") || strings.Contains(msg, ": not found")
}

// stderrDetail renders trimmed ssh stderr as an error suffix, or "" when there
// was none, so a connection failure is diagnosable without an empty ": " tail.
func stderrDetail(stderr []byte) string {
	trimmed := strings.TrimSpace(string(stderr))
	if trimmed == "" {
		return ""
	}
	return ": " + redactTokens(trimmed)
}

// tokenShaped matches the daemon token's exact on-the-wire form: a run of 64 or
// more hex characters (socketauth mints 32 random bytes as lowercase hex).
var tokenShaped = regexp.MustCompile(`[0-9a-fA-F]{64,}`)

// redactTokens strips anything token-shaped out of remote stderr before it can
// reach an error string.
//
// FetchToken already keeps the token out of errors by splicing only stderr,
// never the stdout the token arrives on. That holds for a plain `cat`, but the
// remote side is not ours to constrain: an authorized_keys ForceCommand
// wrapper, a shell rc that traces or tees, or any wrapper that merges the two
// streams can put the file's contents on stderr too. The guarantee that the
// token never appears in a log line or error message must not rest on the
// remote shell behaving — so redact every stderr this package splices into an
// error: stderrDetail covers all but classifyExit's two forward-failure
// branches, which call this directly.
//
// Nothing legitimate in ssh's own diagnostics is a 64-character hex run, so
// this costs no diagnostic value.
func redactTokens(s string) string {
	return tokenShaped.ReplaceAllString(s, "[redacted]")
}

// shellQuote wraps s in single quotes for POSIX shells, escaping embedded single
// quotes as '\”. Single-quote wrapping is the only form that neutralises every
// metacharacter — the remote command line is re-parsed by the remote shell, so
// an unquoted path with a space would arrive as two arguments.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
