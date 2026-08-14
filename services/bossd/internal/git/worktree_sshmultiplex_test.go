package git

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// withSSHControlDir points the control-socket directory at a temporary path for
// the duration of the test and restores the real resolver afterwards.
//
// gitSSHControlDir is a package-level var rather than a parameter because
// gitCommandEnv is called from runGitWithTimeout, which has no manager and no
// configuration to thread one through. Tests that swap it therefore must not run
// in parallel with each other, and none of them do.
func withSSHControlDir(t *testing.T, dir string) {
	t.Helper()
	prev := gitSSHControlDir
	gitSSHControlDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { gitSSHControlDir = prev })
}

// shortTempDir returns a temporary directory short enough to host a control
// path, and fails loudly if the machine has none.
//
// t.TempDir() is unusable for this: on macOS it lands under
// /var/folders/<...>/T/<TestName>/001, ~85 bytes before the socket name, which
// the length guard correctly refuses. That is the guard working, not a test
// problem — but it means a test that wants the multiplexed branch has to ask for
// a directory as short as the real ~/Library/Application Support/bossanova/ssh.
func shortTempDir(t *testing.T) string {
	t.Helper()
	for _, base := range []string{"/tmp", os.TempDir()} {
		dir, err := os.MkdirTemp(base, "bsd")
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if _, ok := sshControlPath(dir); ok {
			return dir
		}
		_ = os.RemoveAll(dir)
	}
	t.Fatalf("no temporary directory short enough for an ssh ControlPath (limit %d, name budget %d)",
		sshControlPathLimit, sshControlNameBudget)
	return ""
}

// spaceControlDir returns a control directory whose path contains a space, the
// way the real macOS one does: "~/Library/Application Support/bossanova/ssh".
//
// Every other test here uses a space-free path, which is what let a
// GIT_SSH_COMMAND that word-splits on the real macOS directory pass a full green
// suite. The space is the whole point of this helper — do not "tidy" it away.
func spaceControlDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "App Support", "ssh")
	if _, ok := sshControlPath(dir); !ok {
		t.Fatalf("control dir %q does not fit the length guard; the helper needs a shorter base", dir)
	}
	return dir
}

// shellSplit runs command through a POSIX shell exactly as git does — git sets
// use_shell for GIT_SSH_COMMAND, so ssh receives the shell's argv, not the
// string bossd authored — and returns the argv the shell produced.
func shellSplit(t *testing.T, command string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no POSIX shell available: %v", err)
	}
	out, err := exec.Command(sh, "-c", "set -- "+command+`; for a in "$@"; do printf '%s\n' "$a"; done`).Output()
	if err != nil {
		t.Fatalf("shell split of %q: %v", command, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// controlPathArg returns the single argv element carrying ControlPath, and fails
// if the shell produced anything other than exactly one.
func controlPathArg(t *testing.T, argv []string) string {
	t.Helper()
	var found []string
	for _, arg := range argv {
		if strings.HasPrefix(arg, "ControlPath=") {
			found = append(found, arg)
		}
	}
	if len(found) != 1 {
		t.Fatalf("argv %q carries %d ControlPath arguments, want exactly 1", argv, len(found))
	}
	return found[0]
}

// TestGitCommandEnvSSHControlPathSurvivesShellSplitting is the regression test
// for the defect the original change shipped: an unquoted ControlPath under a
// directory containing a space is word split by the shell git runs
// GIT_SSH_COMMAND through, so ssh reads the tail of the path as its destination
// host and every ssh git operation fails with "Could not resolve hostname". It
// broke macOS — bossd's primary platform — while a Linux CI leg stayed green,
// because only macOS puts the app data dir under "Application Support".
func TestGitCommandEnvSSHControlPathSurvivesShellSplitting(t *testing.T) {
	dir := spaceControlDir(t)
	withSSHControlDir(t, dir)
	t.Setenv(gitSSHMultiplexingEnv, "")

	entries := gitSSHCommandEntries(gitCommandEnv(zerolog.Nop()))
	if len(entries) != 1 {
		t.Fatalf("GIT_SSH_COMMAND entries = %v, want exactly the bossd-authored one", entries)
	}
	value := strings.TrimPrefix(entries[0], "GIT_SSH_COMMAND=")

	argv := shellSplit(t, value)
	arg := controlPathArg(t, argv)

	// The whole path, space included, must arrive as ONE argument. ssh strips
	// the inner double quotes itself before binding the socket.
	if want := `ControlPath="` + filepath.Join(dir, sshControlSocketTemplate()) + `"`; arg != want {
		t.Errorf("shell delivered ControlPath argument %q, want %q", arg, want)
	}

	// The tail of a split path becomes ssh's destination, so nothing else in
	// argv may look like a fragment of the control directory.
	for i, a := range argv {
		if strings.HasPrefix(a, "ControlPath=") {
			continue
		}
		if strings.Contains(a, "ssh/"+sshControlSocketTemplate()) {
			t.Errorf("argv[%d] = %q is a fragment of a split control path; argv = %q", i, a, argv)
		}
	}
}

// TestSSHControlPathOptionIsAcceptedByRealSSH closes the second half of the same
// defect: shell quoting ALONE is not enough, because ssh's own -o parser
// re-splits the value it receives and rejects it with "keyword controlpath extra
// arguments at end of line" (exit 255). Only real ssh can prove this, so this
// asks it.
func TestSSHControlPathOptionIsAcceptedByRealSSH(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skipf("ssh not installed, cannot verify the -o parser accepts the quoted value: %v", err)
	}
	dir := spaceControlDir(t)
	controlPath, ok := sshControlPath(dir)
	if !ok {
		t.Fatalf("control path for %q does not fit", dir)
	}
	option, ok := sshControlPathOption(controlPath)
	if !ok {
		t.Fatalf("control path %q is not expressible", controlPath)
	}

	// Take the value the way ssh will: after the shell has finished with it.
	value := controlPathArg(t, shellSplit(t, "ssh "+option))

	out, err := exec.Command(sshBin, "-G", "-o", value, "-p", "22", "git@example.com").Output()
	if err != nil {
		t.Fatalf("ssh -G rejected -o %q: %v", value, err)
	}
	want := filepath.Join(dir, "git@example.com-22")
	var got string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, found := strings.CutPrefix(line, "controlpath "); found {
			got = rest
		}
	}
	if got != want {
		t.Errorf("ssh resolved controlpath = %q, want %q", got, want)
	}
}

// TestSSHControlPathEscapesAPercentInTheDirectory covers the third way this
// value can be destroyed after bossd has handed it over: ssh expands %-tokens
// anywhere in a ControlPath, so an unescaped directory such as /Users/50%off
// exits 255 with "unknown key %o" on every git operation. Real ssh is the only
// thing that can confirm both halves — that %% survives its parser, and that it
// expands back to exactly one percent — so this asks it, and separately pins
// that the template's own tokens were not escaped along with the directory.
func TestSSHControlPathEscapesAPercentInTheDirectory(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "50%off")
	controlPath, ok := sshControlPath(dir)
	if !ok {
		t.Fatalf("control path for %q does not fit", dir)
	}
	if !strings.Contains(controlPath, "50%%off") {
		t.Errorf("control path = %q, want the directory percent doubled", controlPath)
	}
	if !strings.Contains(controlPath, sshControlSocketTemplate()) {
		t.Errorf("control path = %q, want the template tokens left live", controlPath)
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skipf("ssh not installed, cannot verify %%%% expands to a literal percent: %v", err)
	}
	option, ok := sshControlPathOption(controlPath)
	if !ok {
		t.Fatalf("control path %q is not expressible", controlPath)
	}
	value := controlPathArg(t, shellSplit(t, "ssh "+option))
	out, err := exec.Command(sshBin, "-G", "-o", value, "-p", "22", "git@example.com").Output()
	if err != nil {
		t.Fatalf("ssh -G rejected -o %q: %v", value, err)
	}
	want := filepath.Join(dir, "git@example.com-22")
	var got string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, found := strings.CutPrefix(line, "controlpath "); found {
			got = rest
		}
	}
	if got != want {
		t.Errorf("ssh resolved controlpath = %q, want %q", got, want)
	}
}

// TestGitCommandEnvSSHControlPathUnquotableDisablesMultiplexing pins the
// remaining escape hatch: a path bossd cannot express through ssh's parser must
// turn multiplexing off rather than emit a command that fails at connect time.
func TestGitCommandEnvSSHControlPathUnquotableDisablesMultiplexing(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), `a"b`)
	withSSHControlDir(t, dir)
	t.Setenv(gitSSHMultiplexingEnv, "")

	if entries := gitSSHCommandEntries(gitCommandEnv(zerolog.Nop())); len(entries) != 0 {
		t.Errorf("GIT_SSH_COMMAND entries = %v, want none for an unquotable control path", entries)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("control dir %q was created for a path that cannot be used", dir)
	}
}

// TestShellSingleQuoteSurvivesAnEmbeddedQuote covers the home directory the
// quoting has to handle and a naive "'"+s+"'" would break: /Users/o'brien.
func TestShellSingleQuoteSurvivesAnEmbeddedQuote(t *testing.T) {
	const raw = `/Users/o'brien/App Support/ssh`
	argv := shellSplit(t, "echo "+shellSingleQuote(raw))
	if len(argv) != 2 || argv[1] != raw {
		t.Errorf("shell split of quoted %q = %q, want a single argument reproducing it", raw, argv)
	}
}

// resetSSHControlPathWarn rearms the once-guard so a test can observe the
// warning that criterion "logs once" is about. Without this the first test to
// trip the guard would consume it for every later test in the process.
func resetSSHControlPathWarn(t *testing.T) {
	t.Helper()
	gitSSHControlPathWarnOnce = sync.Once{}
	t.Cleanup(func() { gitSSHControlPathWarnOnce = sync.Once{} })
}

// gitSSHCommandEntries returns every GIT_SSH_COMMAND= assignment in env, in
// slice order. Order is the whole point of the operator-precedence contract, so
// the helper preserves it rather than collapsing to a map.
func gitSSHCommandEntries(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			out = append(out, kv)
		}
	}
	return out
}

// TestGitCommandEnvSSHMultiplexingOptions pins the multiplexing options bossd
// authors: the ControlMaster/ControlPersist pair that collapses a session
// start's three handshakes into one, the ConnectTimeout that bounds the cold
// connect (and only the cold connect — a wedged master is bounded by
// GitCommandTimeout, not by this), and a ControlPath under the app-data ssh dir.
func TestGitCommandEnvSSHMultiplexingOptions(t *testing.T) {
	dir := shortTempDir(t)
	withSSHControlDir(t, dir)
	t.Setenv(gitSSHMultiplexingEnv, "")

	env := gitCommandEnv(zerolog.Nop())

	entries := gitSSHCommandEntries(env)
	if len(entries) != 1 {
		t.Fatalf("GIT_SSH_COMMAND entries = %v, want exactly the bossd-authored one", entries)
	}
	value := strings.TrimPrefix(entries[0], "GIT_SSH_COMMAND=")
	for _, want := range []string{
		"ssh ",
		"-o ControlMaster=auto",
		"-o ControlPersist=60s",
		"-o ConnectTimeout=10",
		// Quoted for ssh's -o parser AND for the shell git runs this through;
		// see sshControlPathOption.
		`-o 'ControlPath="` + dir + string(os.PathSeparator),
	} {
		if !strings.Contains(value, want) {
			t.Errorf("GIT_SSH_COMMAND = %q, want it to contain %q", value, want)
		}
	}

	// The dir is created on demand, and privately: the socket is a live channel
	// into an authenticated connection, so a group- or world-traversable parent
	// would widen who can ride it.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat control dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("control dir mode = %#o, want 0700", perm)
	}
}

// TestGitCommandEnvSSHMultiplexingCreatesControlDir proves the on-demand
// creation rather than assuming t.TempDir's own existence proved it.
func TestGitCommandEnvSSHMultiplexingCreatesControlDir(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "ssh")
	withSSHControlDir(t, dir)
	t.Setenv(gitSSHMultiplexingEnv, "")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("control dir already exists before the call: %v", err)
	}
	if got := len(gitSSHCommandEntries(gitCommandEnv(zerolog.Nop()))); got != 1 {
		t.Fatalf("GIT_SSH_COMMAND entries = %d, want 1", got)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("control dir was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("control dir mode = %#o, want 0700", perm)
	}
}

// TestGitCommandEnvSSHMultiplexingOptOut pins the escape hatch. An operator who
// hits a wedged master needs out NOW, without editing settings JSON and
// restarting into a possibly-broken config — so "0" must leave the environment
// byte-for-byte as bossd inherited it.
func TestGitCommandEnvSSHMultiplexingOptOut(t *testing.T) {
	// A short dir on purpose: with a long one the length guard would suppress
	// GIT_SSH_COMMAND anyway and this test would pass even if the opt-out were
	// broken.
	withSSHControlDir(t, shortTempDir(t))
	t.Setenv(gitSSHMultiplexingEnv, "0")

	env := gitCommandEnv(zerolog.Nop())

	if entries := gitSSHCommandEntries(env); len(entries) != 0 {
		t.Fatalf("GIT_SSH_COMMAND entries = %v, want none under the opt-out", entries)
	}
	if want := os.Environ(); !equalStrings(env, want) {
		t.Errorf("env differs from the inherited environment under the opt-out")
	}
}

// TestGitCommandEnvSSHMultiplexingOperatorOverrideWins is the precedence
// contract. Exporting GIT_SSH_COMMAND is a supported thing for an operator to
// do — a jump host, a pinned identity file — and bossd's own value must not
// quietly replace it.
//
// os/exec deduplicates cmd.Env with the LAST assignment winning, so bossd's
// entry is placed BEFORE the inherited environment: the operator's copy, which
// arrives with os.Environ(), is therefore the effective one. The operator's
// value is never filtered out.
func TestGitCommandEnvSSHMultiplexingOperatorOverrideWins(t *testing.T) {
	withSSHControlDir(t, shortTempDir(t))
	t.Setenv(gitSSHMultiplexingEnv, "")
	const operator = "ssh -i /custom/key -o IdentitiesOnly=yes"
	t.Setenv("GIT_SSH_COMMAND", operator)

	env := gitCommandEnv(zerolog.Nop())

	entries := gitSSHCommandEntries(env)
	if len(entries) != 2 {
		t.Fatalf("GIT_SSH_COMMAND entries = %v, want both bossd's and the operator's", entries)
	}
	if !strings.Contains(entries[0], "ControlMaster=auto") {
		t.Errorf("first entry = %q, want the bossd-authored one first", entries[0])
	}
	if got, want := entries[len(entries)-1], "GIT_SSH_COMMAND="+operator; got != want {
		t.Errorf("last entry = %q, want the operator's %q — last duplicate wins in exec", got, want)
	}
}

// TestGitCommandEnvSSHControlPathTooLongFallsBack pins the guard that keeps an
// optimization from becoming an outage: an over-long ControlPath makes ssh
// refuse the socket, so bossd measures the assembled path first and falls
// through to the un-multiplexed environment when it will not fit.
func TestGitCommandEnvSSHControlPathTooLongFallsBack(t *testing.T) {
	resetSSHControlPathWarn(t)
	deep := filepath.Join(t.TempDir(), strings.Repeat("d", sshControlPathLimit))
	withSSHControlDir(t, deep)
	t.Setenv(gitSSHMultiplexingEnv, "")

	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	env := gitCommandEnv(logger)
	if entries := gitSSHCommandEntries(env); len(entries) != 0 {
		t.Fatalf("GIT_SSH_COMMAND entries = %v, want none when the control path cannot fit", entries)
	}
	if _, err := os.Stat(deep); !os.IsNotExist(err) {
		t.Errorf("control dir was created despite the length guard: %v", err)
	}

	// Logged, and logged ONCE: every git invocation builds this environment, so
	// a per-call warning would bury the daemon log in a fact that never changes.
	if n := countWarnLines(t, buf.String()); n != 1 {
		t.Fatalf("warn lines after one call = %d, want 1 (log: %q)", n, buf.String())
	}
	gitCommandEnv(logger)
	gitCommandEnv(logger)
	if n := countWarnLines(t, buf.String()); n != 1 {
		t.Errorf("warn lines after three calls = %d, want 1 (log: %q)", n, buf.String())
	}
}

// TestSSHControlPathLengthGuardIsPure asserts the guard without a filesystem:
// sshControlPath only measures, so a path that would be fatal on the platform is
// rejected whether or not any of it exists on disk.
func TestSSHControlPathLengthGuardIsPure(t *testing.T) {
	fits, ok := sshControlPath("/short/ssh")
	if !ok {
		t.Fatalf("sshControlPath(/short/ssh) reported no fit, want a usable path")
	}
	if !strings.HasPrefix(fits, "/short/ssh"+string(os.PathSeparator)) {
		t.Errorf("path = %q, want it under the given dir", fits)
	}

	tooLong := "/" + strings.Repeat("x", sshControlPathLimit)
	if got, ok := sshControlPath(tooLong); ok {
		t.Errorf("sshControlPath(<%d bytes>) = %q, ok — want the length guard to refuse it", len(tooLong), got)
	}

	// The boundary is the reserved expansion budget, not the template's own
	// length: ssh rewrites the tokens at connect time, so the fixed prefix plus
	// the budget is what has to fit.
	exact := "/" + strings.Repeat("x", sshControlPathLimit-sshControlNameBudget-2)
	if _, ok := sshControlPath(exact); !ok {
		t.Errorf("sshControlPath with a dir of %d bytes was refused, want it to fit", len(exact))
	}
	if _, ok := sshControlPath(exact + "x"); ok {
		t.Errorf("sshControlPath with a dir of %d bytes fit, want it refused one byte over", len(exact)+1)
	}
}

// TestSSHControlSocketNameIsDeterministicPerRemote pins the keying contract.
// A ControlPath shared across remote identities would multiplex one host's git
// commands over another host's authenticated master, so the name must vary with
// every component of <user>@<host>:<port> and never with anything else.
func TestSSHControlSocketNameIsDeterministicPerRemote(t *testing.T) {
	base := sshControlSocketName("git", "github.com", "22")
	if again := sshControlSocketName("git", "github.com", "22"); again != base {
		t.Errorf("name = %q then %q, want it stable for one identity", base, again)
	}
	for _, other := range []struct{ user, host, port string }{
		{"git", "gitlab.com", "22"},
		{"git", "github.com", "2222"},
		{"deploy", "github.com", "22"},
	} {
		if got := sshControlSocketName(other.user, other.host, other.port); got == base {
			t.Errorf("name for %s@%s:%s = %q, want it to differ from %q",
				other.user, other.host, other.port, got, base)
		}
	}

	// The production template is the same function fed OpenSSH's own tokens, so
	// ssh performs the substitution and the socket stays keyed per identity
	// without bossd ever inspecting a remote URL.
	tmpl := sshControlSocketTemplate()
	for _, token := range []string{"%r", "%n", "%p"} {
		if !strings.Contains(tmpl, token) {
			t.Errorf("template = %q, want it to carry the %s token", tmpl, token)
		}
	}
	// %h is the hostname AFTER ssh_config resolution, so two aliases that differ
	// only by IdentityFile expand to one name and share a master — which is how
	// a push authorized as one account rides another's connection. %n is the
	// host as written, so the aliases stay distinct.
	if strings.Contains(tmpl, "%h") {
		t.Errorf("template = %q, want the original-host token %%n rather than the resolved %%h", tmpl)
	}
	if strings.ContainsAny(tmpl, "/") {
		t.Errorf("template = %q, want a single path component", tmpl)
	}
}

// TestSweepStaleSSHControlSocketsRemovesOnlyStale pins the startup sweep.
//
// An hour against a 60s ControlPersist is a margin, not a proof — a socket's
// mtime is set when it is bound and is not refreshed by the connections riding
// it, so a continuously reused master can own a socket far older than this (see
// sshControlSocketStaleAfter). The margin is what keeps a wrong guess rare; the
// cost when it is wrong is one forgone reuse, since unlinking the name leaves
// the in-flight command alone and ControlMaster=auto just reconnects.
func TestSweepStaleSSHControlSocketsRemovesOnlyStale(t *testing.T) {
	dir := t.TempDir()
	withSSHControlDir(t, dir)

	now := time.Now()
	stale := filepath.Join(dir, "stale-socket")
	fresh := filepath.Join(dir, "fresh-socket")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	old := now.Add(-2 * sshControlSocketStaleAfter)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// Through the constructor, because the wiring is the thing under test: a
	// sweep nobody calls at startup is a sweep that never runs.
	NewManager(zerolog.Nop())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale socket survived the sweep: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh socket was swept: %v", err)
	}
}

// TestSweepStaleSSHControlSocketsIsBestEffort covers the paths a daemon start
// must survive: no directory yet (the common first-run case) and an unreadable
// one. Neither may panic, and neither may stop NewManager from returning a
// usable manager.
func TestSweepStaleSSHControlSocketsIsBestEffort(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	withSSHControlDir(t, missing)
	if mgr := NewManager(zerolog.Nop()); mgr == nil {
		t.Fatal("NewManager returned nil when the control dir does not exist")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("sweep created the control dir; it should only ever remove")
	}

	// A directory entry is not a socket and is left alone rather than
	// half-removed by an os.Remove that cannot succeed on a non-empty dir.
	dir := t.TempDir()
	withSSHControlDir(t, dir)
	nested := filepath.Join(dir, "subdir")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := time.Now().Add(-2 * sshControlSocketStaleAfter)
	if err := os.Chtimes(nested, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	NewManager(zerolog.Nop())
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("sweep removed a directory entry: %v", err)
	}
}

// TestGitCommandEnvSSHMultiplexingReachesGitInvocations is what criterion "both
// entry points share one helper" is really about, asserted where it matters: in
// the environment the git process actually receives.
//
// The package builds git commands in exactly two places — runGitWithTimeout and
// IsAncestor, which cannot use it because it reads git's exit code — and a stub
// git on PATH records what each was handed. Reading the source for a
// `cmd.Env = ` line would prove the same thing far more weakly, and would not
// survive os/exec's environment deduplication being wrong.
func TestGitCommandEnvSSHMultiplexingReachesGitInvocations(t *testing.T) {
	dir := shortTempDir(t)
	withSSHControlDir(t, dir)
	t.Setenv(gitSSHMultiplexingEnv, "")

	stubGit(t)
	mgr := NewManager(zerolog.Nop())
	ctx := context.Background()

	t.Run("runGitWithTimeout", func(t *testing.T) {
		out := stubGitEnvFile(t)
		if _, err := runGitWithTimeout(ctx, time.Minute, t.TempDir(), "status"); err != nil {
			t.Fatalf("runGitWithTimeout: %v", err)
		}
		assertMultiplexed(t, readStubGitEnv(t, out), dir)
	})

	t.Run("IsAncestor", func(t *testing.T) {
		out := stubGitEnvFile(t)
		if _, err := mgr.IsAncestor(ctx, t.TempDir(), "a", "b"); err != nil {
			t.Fatalf("IsAncestor: %v", err)
		}
		assertMultiplexed(t, readStubGitEnv(t, out), dir)
	})
}

// stubGit puts a git on PATH that records its environment and exits 0. os/exec
// resolves the program name against the process PATH at Command time, before
// cmd.Env is consulted, so t.Setenv is enough to divert both call sites.
func stubGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n/usr/bin/env > \"$" + stubGitEnvVar + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write stub git: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stubGitEnvVar names the file the stub git dumps its environment into. It
// travels through the same os.Environ() the helper under test copies, which is
// incidentally a second proof that the inherited environment survives.
const stubGitEnvVar = "BOSSD_TEST_GIT_ENV_OUT"

func stubGitEnvFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	t.Setenv(stubGitEnvVar, path)
	return path
}

func readStubGitEnv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub git recorded no environment: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func assertMultiplexed(t *testing.T, env []string, controlDir string) {
	t.Helper()
	entries := gitSSHCommandEntries(env)
	if len(entries) != 1 {
		t.Fatalf("git received GIT_SSH_COMMAND entries %v, want exactly one", entries)
	}
	for _, want := range []string{"ControlMaster=auto", "ControlPersist=60s", "ConnectTimeout=10", `ControlPath="` + controlDir} {
		if !strings.Contains(entries[0], want) {
			t.Errorf("git received %q, want it to contain %q", entries[0], want)
		}
	}
}

// equalStrings compares two environments element-wise, order included.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countWarnLines counts zerolog JSON records at level warn.
func countWarnLines(t *testing.T, out string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if rec.Level == "warn" {
			n++
		}
	}
	return n
}
