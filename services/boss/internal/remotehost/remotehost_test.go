package remotehost

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// validToken is a well-formed 64-char lowercase-hex daemon token, the shape
// socketauth.ValidateToken accepts.
const validToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// recordedCmd is one captured invocation: the binary name and the argv the
// package asked for, before the fake substitutes /bin/sh. The destination
// passthrough tests assert against this, so a future "helpful" username parser
// would show up here as a changed argv.
type recordedCmd struct {
	name string
	args []string
	ctx  context.Context //nolint:containedctx // captured purely so tests can assert propagation
}

// fakeSSH returns a CommandFactory that records the requested argv and then
// runs /bin/sh -c script instead of the real ssh binary. This is the
// established fake-command idiom in this repo (see
// plugins/bossd-plugin-claude/runner_test.go).
func fakeSSH(t *testing.T, script string, rec *[]recordedCmd) CommandFactory {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*rec = append(*rec, recordedCmd{name: name, args: args, ctx: ctx})
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

// envReportJSON is a full-shaped `boss env --json` payload: json.MarshalIndent
// formatting, every top-level field the real EnvReport emits, plus deliberately
// unrelated blocks. Parsing it proves the minimal local struct tolerates the
// whole report rather than only the two fields it reads.
const envReportJSON = `{
  "mode": "standalone",
  "profile": "local",
  "session": {
    "session_id": "",
    "agent_session_id": "",
    "repo_id": "",
    "agent": "",
    "worktree": ""
  },
  "binaries": {
    "settings_path": "/home/dave/.bossanova/settings.json",
    "boss": "/usr/local/bin/boss",
    "mcp": "/usr/local/bin/boss-mcp"
  },
  "daemon": {
    "socket": "/home/dave/.bossanova/bossd.sock",
    "reachable": true
  },
  "capabilities": {
    "cli": ["boss ls", "boss session new"],
    "mcp": ["list_sessions", "create_session"]
  }
}
`

func TestDiscoverSocketPassesDestinationVerbatim(t *testing.T) {
	// The destination is an ssh destination, not a structured value: a bare
	// host, a user@host, and a ~/.ssh/config Host alias must all reach ssh
	// byte-identical so every config directive the user relies on still applies.
	destinations := []string{"user@host", "host", "my-config-alias"}
	for _, dest := range destinations {
		t.Run(dest, func(t *testing.T) {
			var rec []recordedCmd
			got, err := DiscoverSocket(context.Background(), Options{
				Destination:    dest,
				CommandFactory: fakeSSH(t, "cat <<'EOF'\n"+envReportJSON+"EOF", &rec),
			})
			if err != nil {
				t.Fatalf("DiscoverSocket: %v", err)
			}
			if got != "/home/dave/.bossanova/bossd.sock" {
				t.Fatalf("socket = %q", got)
			}
			if len(rec) != 1 {
				t.Fatalf("commands run = %d, want 1", len(rec))
			}
			if rec[0].name != "ssh" {
				t.Fatalf("binary = %q, want ssh", rec[0].name)
			}
			if len(rec[0].args) == 0 || rec[0].args[0] != dest {
				t.Fatalf("argv = %q, want destination %q verbatim as argv[0]", rec[0].args, dest)
			}
			wantArgs := []string{dest, "boss", "env", "--json"}
			if strings.Join(rec[0].args, " ") != strings.Join(wantArgs, " ") {
				t.Fatalf("argv = %q, want %q", rec[0].args, wantArgs)
			}
		})
	}
}

func TestDiscoverSocketAndFetchTokenUseIdenticalDestination(t *testing.T) {
	// Discovery, the token fetch, and the tunnel's `ssh -N -L ...` forward must
	// all resolve to the same remote account, so all three must carry the
	// byte-identical destination argument. If they diverged — say a "helpful"
	// username parser rewrote one of them — the fetched token could belong to a
	// different remote user than the socket being forwarded.
	const dest = "deploy@bastion.example.com"

	var discoverRec []recordedCmd
	if _, err := DiscoverSocket(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "cat <<'EOF'\n"+envReportJSON+"EOF", &discoverRec),
	}); err != nil {
		t.Fatalf("DiscoverSocket: %v", err)
	}

	var tokenRec []recordedCmd
	if _, err := FetchToken(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "echo "+validToken, &tokenRec),
	}, "/home/dave/.bossanova/bossd.sock"); err != nil {
		t.Fatalf("FetchToken: %v", err)
	}

	// The third leg: the supervised forward, taken from the argv the tunnel
	// actually asked for rather than from its config.
	forward := &fakeRunner{script: func(int) string { return "sleep 30" }}
	tun, err := NewTunnel(TunnelConfig{
		Options:      Options{Destination: dest, CommandFactory: forward.factory()},
		RemoteSocket: "/home/dave/.bossanova/bossd.sock",
		Logger:       zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewTunnel: %v", err)
	}
	defer func() { _ = tun.Close() }()
	tun.Start(context.Background())
	waitFor(t, "the forward invocation", func() bool { return forward.count() >= 1 })
	_, forwardArgs := forward.argv(0)

	if len(discoverRec) != 1 || len(tokenRec) != 1 {
		t.Fatalf("invocations: discover=%d token=%d", len(discoverRec), len(tokenRec))
	}
	// The destination is argv[0] for a remote command and the final element for
	// the forward, because ssh options must precede it.
	forwardDest := forwardArgs[len(forwardArgs)-1]
	if discoverRec[0].args[0] != tokenRec[0].args[0] {
		t.Fatalf("destinations differ: discover=%q token=%q", discoverRec[0].args[0], tokenRec[0].args[0])
	}
	if forwardDest != discoverRec[0].args[0] {
		t.Fatalf("destinations differ: discover=%q forward=%q", discoverRec[0].args[0], forwardDest)
	}
	if discoverRec[0].args[0] != dest || forwardDest != dest {
		t.Fatalf("destination = discover %q / forward %q, want %q", discoverRec[0].args[0], forwardDest, dest)
	}
}

func TestDiscoverSocketRemoteSocketOverrideSkipsSSH(t *testing.T) {
	var rec []recordedCmd
	got, err := DiscoverSocket(context.Background(), Options{
		Destination:    "user@host",
		RemoteSocket:   "/custom/run/bossd.sock",
		CommandFactory: fakeSSH(t, "exit 1", &rec),
	})
	if err != nil {
		t.Fatalf("DiscoverSocket: %v", err)
	}
	if got != "/custom/run/bossd.sock" {
		t.Fatalf("socket = %q, want the override verbatim", got)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0 (override must short-circuit)", len(rec))
	}
}

func TestDiscoverSocketUnreachableDaemon(t *testing.T) {
	const dest = "user@host"
	var rec []recordedCmd
	_, err := DiscoverSocket(context.Background(), Options{
		Destination: dest,
		CommandFactory: fakeSSH(t, `cat <<'EOF'
{"mode":"standalone","daemon":{"socket":"/home/dave/.bossanova/bossd.sock","reachable":false}}
EOF`, &rec),
	})
	if !errors.Is(err, ErrRemoteDaemonDown) {
		t.Fatalf("err = %v, want ErrRemoteDaemonDown", err)
	}
	if !strings.Contains(err.Error(), dest) {
		t.Fatalf("err %q does not name the destination %q", err, dest)
	}
}

func TestDiscoverSocketMissingRemoteBoss(t *testing.T) {
	// A non-interactive ssh login gets a reduced PATH, so "boss not found" is
	// the common first failure; both the 127 exit status and the shell's
	// message must classify.
	cases := map[string]string{
		"exit127":             "exit 127",
		"stderrNotFound":      `echo "bash: boss: command not found" >&2; exit 1`,
		"stderrColonNotFound": `echo "sh: 1: boss: not found" >&2; exit 1`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			const dest = "user@host"
			var rec []recordedCmd
			_, err := DiscoverSocket(context.Background(), Options{
				Destination:    dest,
				CommandFactory: fakeSSH(t, script, &rec),
			})
			if !errors.Is(err, ErrRemoteBossMissing) {
				t.Fatalf("err = %v, want ErrRemoteBossMissing", err)
			}
			if !strings.Contains(err.Error(), dest) {
				t.Fatalf("err %q does not name the destination", err)
			}
			if !strings.Contains(err.Error(), "--host-socket") {
				t.Fatalf("err %q does not mention the --host-socket escape hatch", err)
			}
		})
	}
}

func TestDiscoverSocketErrorClassesAreDistinct(t *testing.T) {
	const dest = "user@host"
	var missingRec, downRec []recordedCmd
	_, missingErr := DiscoverSocket(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "exit 127", &missingRec),
	})
	_, downErr := DiscoverSocket(context.Background(), Options{
		Destination: dest,
		CommandFactory: fakeSSH(t, `cat <<'EOF'
{"daemon":{"socket":"/s/bossd.sock","reachable":false}}
EOF`, &downRec),
	})

	if missingErr == nil || downErr == nil {
		t.Fatalf("want both errors, got %v / %v", missingErr, downErr)
	}
	if missingErr.Error() == downErr.Error() {
		t.Fatalf("both classes produced the same message %q", missingErr)
	}
	if errors.Is(missingErr, ErrRemoteDaemonDown) {
		t.Fatalf("missing-boss error also matched ErrRemoteDaemonDown: %v", missingErr)
	}
	if errors.Is(downErr, ErrRemoteBossMissing) {
		t.Fatalf("daemon-down error also matched ErrRemoteBossMissing: %v", downErr)
	}
}

func TestDiscoverSocketMalformedJSON(t *testing.T) {
	const dest = "user@host"
	var rec []recordedCmd
	_, err := DiscoverSocket(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, `echo 'not json at all {'`, &rec),
	})
	if err == nil {
		t.Fatal("want an error for malformed JSON")
	}
	if errors.Is(err, ErrRemoteBossMissing) || errors.Is(err, ErrRemoteDaemonDown) {
		t.Fatalf("malformed JSON matched a sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), dest) {
		t.Fatalf("err %q does not name the destination", err)
	}
}

func TestDiscoverSocketIncludesStderrInConnectionFailure(t *testing.T) {
	var rec []recordedCmd
	_, err := DiscoverSocket(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, `echo "ssh: connect to host host port 22: Connection refused" >&2; exit 255`, &rec),
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err %q omits ssh stderr, making the failure undiagnosable", err)
	}
}

func TestDiscoverSocketEmptyDestination(t *testing.T) {
	var rec []recordedCmd
	_, err := DiscoverSocket(context.Background(), Options{
		CommandFactory: fakeSSH(t, "exit 0", &rec),
	})
	if !errors.Is(err, ErrNoDestination) {
		t.Fatalf("err = %v, want ErrNoDestination", err)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0", len(rec))
	}
}

func TestFetchTokenEmptyDestination(t *testing.T) {
	var rec []recordedCmd
	_, err := FetchToken(context.Background(), Options{
		CommandFactory: fakeSSH(t, "echo "+validToken, &rec),
	}, "/s/bossd.sock")
	if !errors.Is(err, ErrNoDestination) {
		t.Fatalf("err = %v, want ErrNoDestination", err)
	}
	if len(rec) != 0 {
		t.Fatalf("ran %d commands, want 0", len(rec))
	}
}

func TestFetchTokenHappyPath(t *testing.T) {
	const dest = "user@host"
	var rec []recordedCmd
	// echo appends the trailing newline a real `cat` of the token file emits.
	got, err := FetchToken(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "echo "+validToken, &rec),
	}, "/home/dave/.bossanova/bossd.sock")
	if err != nil {
		t.Fatalf("FetchToken: %v", err)
	}
	if got != validToken {
		t.Fatalf("token = %q, want the trimmed canonical form", got)
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	want := []string{dest, "cat", "'/home/dave/.bossanova/bossd.token'"}
	if strings.Join(rec[0].args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", rec[0].args, want)
	}
	if rec[0].name != "ssh" {
		t.Fatalf("binary = %q, want ssh", rec[0].name)
	}
}

func TestFetchTokenPathUsesForwardSlashes(t *testing.T) {
	// The remote path is always POSIX even when this code compiles on Windows,
	// so the token path must be built with `path`, never `path/filepath`.
	got := remoteTokenPath("/var/run/bossd/bossd.sock")
	if got != "/var/run/bossd/bossd.token" {
		t.Fatalf("remoteTokenPath = %q", got)
	}
	if strings.Contains(got, `\`) {
		t.Fatalf("remoteTokenPath = %q contains a backslash separator", got)
	}
}

func TestFetchTokenFailureDoesNotLeakToken(t *testing.T) {
	// A fake that emits a real token on stdout AND fails: splicing stdout into
	// the error would put the shared secret in every log that records it.
	var rec []recordedCmd
	_, err := FetchToken(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "echo "+validToken+"; echo 'cat: permission denied' >&2; exit 1", &rec),
	}, "/s/bossd.sock")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), validToken) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err %q omits stderr, making the failure undiagnosable", err)
	}
}

// TestFetchTokenRedactsATokenOnStderr covers the case the stdout/stderr split
// alone cannot: the remote side is not ours to constrain, and a ForceCommand
// wrapper, a tracing shell rc, or anything that merges the two streams can put
// the token file's contents on *stderr* — which FetchToken does splice into its
// error. The guarantee that the token never reaches a log line must not depend
// on the remote shell behaving.
func TestFetchTokenRedactsATokenOnStderr(t *testing.T) {
	var rec []recordedCmd
	_, err := FetchToken(context.Background(), Options{
		Destination: "user@host",
		// A wrapper that echoed the command's output to stderr as well, then failed.
		CommandFactory: fakeSSH(t, "echo 'wrapper: cat "+validToken+"' >&2; exit 1", &rec),
	}, "/s/bossd.sock")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), validToken) {
		t.Fatalf("error leaked a token that arrived on stderr: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("err %q should mark where the token was removed", err)
	}
	// Redaction must not swallow the surrounding diagnostic.
	if !strings.Contains(err.Error(), "wrapper: cat") {
		t.Fatalf("err %q lost the stderr context around the token", err)
	}
}

// TestRedactTokensLeavesOrdinarySSHDiagnosticsAlone: the needle is a 64+ hex
// run, which nothing in ssh's own wording is, so real failures stay readable.
func TestRedactTokensLeavesOrdinarySSHDiagnosticsAlone(t *testing.T) {
	for _, s := range []string{
		"cat: /home/dave/.bossanova/bossd.token: No such file or directory",
		"ssh: Could not resolve hostname bastion: nodename nor servname provided",
		"Permission denied (publickey,keyboard-interactive).",
		"unix_listener: cannot bind to path /tmp/boss-host-123/bossd.sock: Address already in use",
		// A short hex run (a git sha) is not token-shaped and must survive.
		"error at commit 2e0ce333d1b991e415f6eac079ecdafccfb4e957",
	} {
		if got := redactTokens(s); got != s {
			t.Fatalf("redactTokens(%q) = %q, want it unchanged", s, got)
		}
	}
	if got := redactTokens(validToken); got != "[redacted]" {
		t.Fatalf("redactTokens(<token>) = %q, want [redacted]", got)
	}
}

func TestFetchTokenMalformedTokenNotInError(t *testing.T) {
	const junk = "TOTALLY-NOT-A-TOKEN-but-still-secret-shaped"
	var rec []recordedCmd
	_, err := FetchToken(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "echo "+junk, &rec),
	}, "/s/bossd.sock")
	if err == nil {
		t.Fatal("want an error for a malformed token")
	}
	if strings.Contains(err.Error(), junk) {
		t.Fatalf("error leaked the fetched value: %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/dave/.bossanova/bossd.token", "'/home/dave/.bossanova/bossd.token'"},
		{"/home/my user/bossd.token", "'/home/my user/bossd.token'"},
		{"/home/o'brien/bossd.token", `'/home/o'\''brien/bossd.token'`},
		{"", "''"},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTerminfoPresentEntryPresentRemotely(t *testing.T) {
	resetTerminfoCache()
	var rec []recordedCmd
	got := TerminfoPresent(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "exit 0", &rec),
	}, "xterm-ghostty")
	if !got {
		t.Fatal("TerminfoPresent = false, want true when remote infocmp exits 0")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
}

func TestTerminfoPresentEntryAbsentRemotely(t *testing.T) {
	// infocmp's clean "no such entry" signal is exit 1. This is the ONLY status
	// that may downgrade the user's TERM, so it must be pinned.
	resetTerminfoCache()
	var rec []recordedCmd
	got := TerminfoPresent(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, `echo "infocmp: couldn't open terminfo file" >&2; exit 1`, &rec),
	}, "xterm-ghostty")
	if got {
		t.Fatal("TerminfoPresent = true, want false when remote infocmp exits 1 (entry absent)")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
}

// TestTerminfoProbeRcNoiseDoesNotSwallowTheRealAnswer is the collision the
// login-shell routing makes reachable. `exec "$SHELL" -lc` sources /etc/profile,
// ~/.profile, ~/.bash_profile and ~/.zprofile — exactly where guarded
// pyenv/nvm/rbenv init blocks live — so a genuine exit-1 "entry absent" commonly
// arrives with somebody else's "command not found" ahead of it. Matching that
// bare phrase anywhere in stderr (as isCommandNotFound does) would read it as
// "there is no infocmp to ask", fail open, and ship the unresolvable TERM to the
// remote tmux: the exact failure this ticket exists to remove, on the hosts the
// login shell makes MORE likely to hit it.
func TestTerminfoProbeRcNoiseDoesNotSwallowTheRealAnswer(t *testing.T) {
	for name, noise := range map[string]string{
		"bash pyenv":  "bash: line 1: pyenv: command not found",
		"zsh nvm":     "zsh:1: command not found: nvm",
		"dash shim":   "sh: 1: lesspipe: not found",
		"zsh spelled": "command not found: rbenv",
	} {
		t.Run(name, func(t *testing.T) {
			resetTerminfoCache()
			var rec []recordedCmd
			// Both lines go through shellQuote: infocmp's real message carries
			// an apostrophe, and hand-quoting it wrong makes the fake exit 2
			// (a syntax error), which fails open for the wrong reason and would
			// make this test pass against the unfixed classifier.
			script := "echo " + shellQuote(noise) + " >&2; " +
				"echo " + shellQuote("infocmp: couldn't open terminfo file /usr/share/terminfo") +
				" >&2; exit 1"
			got := TerminfoPresent(context.Background(), Options{
				Destination:    "user@host",
				CommandFactory: fakeSSH(t, script, &rec),
			}, "xterm-ghostty")
			if got {
				t.Fatalf("TerminfoPresent = true with rc noise %q alongside infocmp's exit-1 answer; "+
					"want false — the entry really is absent", noise)
			}
		})
	}
}

// TestTerminfoProbeMissingStillFiresWhenInfocmpIsTheMissingOne is the
// falsification half: narrowing the phrase must not stop a real missing infocmp
// from failing open, which is the whole point of the branch.
func TestTerminfoProbeMissingStillFiresWhenInfocmpIsTheMissingOne(t *testing.T) {
	for name, msg := range map[string]string{
		"bash": "bash: line 1: infocmp: command not found",
		"zsh":  "zsh:1: command not found: infocmp",
		"dash": "sh: 1: infocmp: not found",
	} {
		t.Run(name, func(t *testing.T) {
			if !terminfoProbeMissing([]byte(msg)) {
				t.Fatalf("terminfoProbeMissing(%q) = false, want true", msg)
			}
		})
	}
	// And a shell reporting it under a status of its own choosing still fails
	// open end to end, not just in the matcher.
	resetTerminfoCache()
	var rec []recordedCmd
	if !TerminfoPresent(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, `echo "zsh:1: command not found: infocmp" >&2; exit 1`, &rec),
	}, "xterm-ghostty") {
		t.Fatal("TerminfoPresent = false when the shell said infocmp itself is missing; want true (fail open)")
	}
}

func TestTerminfoPresentRemoteInfocmpMissingFailsOpen(t *testing.T) {
	// No infocmp on the remote box is "could not answer", not "absent".
	resetTerminfoCache()
	var rec []recordedCmd
	got := TerminfoPresent(context.Background(), Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, `echo "sh: infocmp: command not found" >&2; exit 127`, &rec),
	}, "xterm-ghostty")
	if !got {
		t.Fatal("TerminfoPresent = false, want true (fail open) when remote infocmp is missing")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
}

func TestTerminfoPresentTransportFailureFailsOpenAndIsCached(t *testing.T) {
	// ssh's own 255. A broken host is exactly the case the cache exists for:
	// re-probing it on every Update tick is the stall we must not reintroduce.
	resetTerminfoCache()
	var rec []recordedCmd
	opts := Options{
		Destination:    "user@unreachable",
		CommandFactory: fakeSSH(t, `echo "ssh: connect to host h port 22: Connection refused" >&2; exit 255`, &rec),
	}
	if !TerminfoPresent(context.Background(), opts, "xterm-ghostty") {
		t.Fatal("TerminfoPresent = false, want true (fail open) on an ssh transport failure")
	}
	if !TerminfoPresent(context.Background(), opts, "xterm-ghostty") {
		t.Fatal("second TerminfoPresent = false, want the cached true")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1: a fail-open result must be cached too", len(rec))
	}
}

func TestTerminfoPresentTimeoutFailsOpenPromptly(t *testing.T) {
	resetTerminfoCache()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var rec []recordedCmd
	start := time.Now()
	got := TerminfoPresent(ctx, Options{
		Destination:    "user@slow-host",
		CommandFactory: fakeSSH(t, "sleep 30", &rec),
	}, "xterm-ghostty")
	elapsed := time.Since(start)

	if !got {
		t.Fatal("TerminfoPresent = false, want true (fail open) when the probe times out")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if elapsed >= terminfoProbeTimeout {
		t.Fatalf("probe took %v, want well under %v: the caller's deadline must bound it "+
			"and the 30s script must never be waited out", elapsed, terminfoProbeTimeout)
	}
}

func TestTerminfoPresentCachesPerDestinationAndTerm(t *testing.T) {
	resetTerminfoCache()
	var rec []recordedCmd
	opts := Options{Destination: "user@host-a", CommandFactory: fakeSSH(t, "exit 0", &rec)}

	if !TerminfoPresent(context.Background(), opts, "xterm-ghostty") {
		t.Fatal("first probe = false, want true")
	}
	if !TerminfoPresent(context.Background(), opts, "xterm-ghostty") {
		t.Fatal("second probe = false, want the cached true")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1: the repeat must be served from the cache", len(rec))
	}

	// A different TERM on the same host is a different question.
	if !TerminfoPresent(context.Background(), opts, "xterm-kitty") {
		t.Fatal("probe for a second term = false, want true")
	}
	if len(rec) != 2 {
		t.Fatalf("commands run = %d, want 2: a different term must re-probe", len(rec))
	}

	// So is the same TERM on a different host — that is the whole bug: a TERM
	// that resolves on one box need not resolve on the next.
	other := Options{Destination: "user@host-b", CommandFactory: fakeSSH(t, "exit 0", &rec)}
	if !TerminfoPresent(context.Background(), other, "xterm-ghostty") {
		t.Fatal("probe for a second destination = false, want true")
	}
	if len(rec) != 3 {
		t.Fatalf("commands run = %d, want 3: a different destination must re-probe", len(rec))
	}
}

func TestTerminfoProbeRidesBatchModeAndControlMaster(t *testing.T) {
	resetTerminfoCache()
	const dest = "deploy@bastion.example.com"
	var rec []recordedCmd
	if !TerminfoPresent(context.Background(), Options{
		Destination:    dest,
		ControlPath:    "/tmp/boss-ctl.sock",
		CommandFactory: fakeSSH(t, "exit 0", &rec),
	}, "xterm-ghostty") {
		t.Fatal("TerminfoPresent = false, want true")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if rec[0].name != "ssh" {
		t.Fatalf("binary = %q, want ssh", rec[0].name)
	}
	want := []string{
		"-o", "BatchMode=yes",
		"-o", "ControlPath=/tmp/boss-ctl.sock",
		dest,
		// Through the remote LOGIN shell, exactly like the attach this probe
		// predicts for — see remoteLoginShell.
		`exec "$SHELL" -lc 'infocmp '\''xterm-ghostty'\'''`,
	}
	if strings.Join(rec[0].args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", rec[0].args, want)
	}
	// ssh stops parsing options at the destination, so every -o must precede it
	// and the destination must reach ssh byte-identical (see the package doc).
	destIdx := slices.Index(rec[0].args, dest)
	if destIdx < 0 {
		t.Fatalf("argv %q does not carry the destination %q verbatim", rec[0].args, dest)
	}
	for i, arg := range rec[0].args {
		if (arg == "BatchMode=yes" || arg == "ControlPath=/tmp/boss-ctl.sock") && i > destIdx {
			t.Fatalf("argv %q puts %q after the destination, where ssh reads it as part of "+
				"the remote command", rec[0].args, arg)
		}
	}
}

func TestTerminfoProbeWithoutControlPathStillBatchMode(t *testing.T) {
	// Before the tunnel's master exists there is nothing to ride, but BatchMode
	// is what keeps an auth prompt off the Update path, so it is unconditional.
	resetTerminfoCache()
	const dest = "deploy@bastion.example.com"
	var rec []recordedCmd
	if !TerminfoPresent(context.Background(), Options{
		Destination:    dest,
		CommandFactory: fakeSSH(t, "exit 0", &rec),
	}, "xterm-256color") {
		t.Fatal("TerminfoPresent = false, want true")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	want := []string{"-o", "BatchMode=yes", dest, `exec "$SHELL" -lc 'infocmp '\''xterm-256color'\'''`}
	if strings.Join(rec[0].args, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %q, want %q", rec[0].args, want)
	}
	for _, arg := range rec[0].args {
		if strings.HasPrefix(arg, "ControlPath=") {
			t.Fatalf("argv %q carries %q with no ControlPath configured", rec[0].args, arg)
		}
	}
}

// TestTerminfoPresentAnswersLocallyWithoutSSH covers the inputs that need no
// round trip, and — crucially — that the two of them answer DIFFERENTLY.
//
// A caller with no destination is not remote, so there is nothing to fail closed
// about: true. But a TERM that cannot name a terminfo entry must answer ABSENT,
// because the caller's next move is to substitute the fallback. Answering true
// for an empty TERM would diverge from termnorm's local probe (which returns
// false for "") and ship a bare `TERM=` to the remote tmux, which dies with the
// exact "missing or unsuitable terminal" this package exists to prevent —
// reachable today, since preflight deliberately does not block on an unset TERM.
func TestTerminfoPresentAnswersLocallyWithoutSSH(t *testing.T) {
	cases := map[string]struct {
		dest, term string
		want       bool
	}{
		"emptyDestination":      {dest: "", term: "xterm-ghostty", want: true},
		"whitespaceDestination": {dest: "   ", term: "xterm-ghostty", want: true},
		"emptyTerm":             {dest: "user@host", term: "", want: false},
		// Not a terminfo name on any host, and the character class is what keeps
		// the two shell parses below composable — see probeableTerm.
		"quoteInTerm":     {dest: "user@host", term: `xterm'; rm -rf ~; '`, want: false},
		"backslashInTerm": {dest: "user@host", term: `xterm\`, want: false},
		"spaceInTerm":     {dest: "user@host", term: "xterm 256color", want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resetTerminfoCache()
			var rec []recordedCmd
			// The fake would report the entry PRESENT if it ever ran, so a
			// `want: false` case cannot be satisfied by accidentally probing.
			got := TerminfoPresent(context.Background(), Options{
				Destination:    tc.dest,
				CommandFactory: fakeSSH(t, "exit 0", &rec),
			}, tc.term)
			if got != tc.want {
				t.Fatalf("TerminfoPresent(%q, %q) = %v, want %v", tc.dest, tc.term, got, tc.want)
			}
			if len(rec) != 0 {
				t.Fatalf("ran %d commands, want 0", len(rec))
			}
		})
	}
}

// TestTerminfoProbeRunsThroughTheRemoteLoginShell is the guard for the fix's
// second half. `ssh host infocmp …` runs in a NON-login shell, so it consults a
// different PATH and a different terminfo search path than the remote tmux the
// attach starts through `exec "$SHELL" -lc` — an infocmp outside sshd's reduced
// PATH answers "not installed" on exactly the hosts most likely to need this
// fix, and a TERMINFO_DIRS exported from ~/.zprofile makes a bare probe exit 1
// and downgrade a TERM tmux would have resolved. Fail-open covers only the first
// of those, so the probe has to ask where tmux will look. The sibling liveness
// probe (views.tmuxProbeCommandLine) already holds this rule; this pins it here.
func TestTerminfoProbeRunsThroughTheRemoteLoginShell(t *testing.T) {
	resetTerminfoCache()
	var rec []recordedCmd
	if !TerminfoPresent(context.Background(), Options{
		Destination:    "deploy@bastion",
		CommandFactory: fakeSSH(t, "exit 0", &rec),
	}, "xterm-ghostty") {
		t.Fatal("TerminfoPresent = false, want true")
	}
	remoteCommand := rec[0].args[len(rec[0].args)-1]
	if !strings.HasPrefix(remoteCommand, `exec "$SHELL" -lc `) {
		t.Fatalf("remote command = %q, want it routed through the remote login shell", remoteCommand)
	}
	// Not ${SHELL:-…}: the POSIX default-value form is a syntax error in fish,
	// which is a real remote login shell — see remoteLoginShell.
	if strings.Contains(remoteCommand, "${SHELL") {
		t.Fatalf("remote command %q uses ${SHELL…}, which fish rejects outright", remoteCommand)
	}

	// The quoting now crosses TWO parses, so asserting its spelling would only
	// restate how the string was built. Run it through real shells against stubs
	// and check what infocmp was actually handed: if either layer is wrong the
	// TERM splits into several arguments, or runs as a command.
	//
	// Every shell on this machine, not just /bin/sh, because the OUTER parse is
	// done by whatever the remote user's login shell is. posixShell's doc calls
	// the fish divergence load-bearing — a backslash inside single quotes
	// re-splits the words there — and probeableTerm is the thing that keeps one
	// out. This loop is what would go red if that character class were relaxed.
	for name, shell := range shellsUnderTest(t) {
		got := remoteInfocmpArgv(t, shell, remoteCommand)
		if !reflect.DeepEqual(got, []string{"xterm-ghostty"}) {
			t.Errorf("under a %s outer shell, remote infocmp was handed %q, want the TERM as exactly one argument",
				name, got)
		}
	}
}

// remoteInfocmpArgv parses a remote command string the way ssh's far side does —
// the given outer shell reads the whole string, then remoteLoginShell's
// `$SHELL -lc` reads the inner one — and returns the arguments the infocmp at
// the end got.
//
// The login shell is a STUB rather than a real `-l`, which would look like the
// more faithful choice and is not: on macOS /etc/profile runs path_helper and
// reorders the system directories ahead of anything a test injected, so the
// assertion would silently exercise the machine's own infocmp. Standing in for
// it keeps the test about the property it asserts while still checking that
// production asked for `-lc`. (Same reasoning, same shape, as
// views.remoteStubDir.)
func remoteInfocmpArgv(t *testing.T, outerShell, remoteCommand string) []string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"infocmp": "#!/bin/sh\nprintf '%s\\n' \"$@\"\n",
		"loginshell": "#!/bin/sh\n" +
			"[ \"$1\" = \"-lc\" ] || { echo \"login shell invoked with $1, want -lc\" >&2; exit 64; }\n" +
			"exec /bin/sh -c \"$2\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	// #nosec G204 -- test-only; remoteCommand is this package's own builder output
	// owner=@recurser review-by=2027-02-07 issue=BOS-714
	cmd := exec.Command(outerShell, "-c", remoteCommand)
	// PATH is narrowed to the stub dir, so the shells are named absolutely.
	cmd.Env = []string{"PATH=" + dir, "SHELL=" + filepath.Join(dir, "loginshell")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote command %q under %s failed: %v", remoteCommand, outerShell, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

func TestDiscoverSocketHonoursContext(t *testing.T) {
	type ctxKey struct{}
	base := context.WithValue(context.Background(), ctxKey{}, "carried")
	ctx, cancel := context.WithCancel(base)
	cancel() // already cancelled: the command must not outlive it

	var rec []recordedCmd
	_, err := DiscoverSocket(ctx, Options{
		Destination:    "user@host",
		CommandFactory: fakeSSH(t, "sleep 30", &rec),
	})
	if err == nil {
		t.Fatal("want an error from the cancelled context")
	}
	if len(rec) != 1 {
		t.Fatalf("commands run = %d, want 1", len(rec))
	}
	if got := rec[0].ctx.Value(ctxKey{}); got != "carried" {
		t.Fatalf("factory received ctx value %v, want the caller's context", got)
	}
}
