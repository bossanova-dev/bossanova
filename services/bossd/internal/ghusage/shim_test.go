package ghusage

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// systemPath lets the wrapper find date and printf behind the fake gh.
const systemPath = "/usr/bin:/bin"

// fakeRealGH writes a "real gh" that echoes its arguments and exits 3, and
// returns the directory holding it.
func fakeRealGH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"real gh: $*\"\nexit 3\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	return dir
}

func installTestShim(t *testing.T) (*Shim, string) {
	t.Helper()
	ledgerDir := filepath.Join(t.TempDir(), DirName)
	shim, err := InstallShim(ledgerDir, fakeRealGH(t)+string(os.PathListSeparator)+systemPath)
	if err != nil {
		t.Fatalf("InstallShim: %v", err)
	}
	if shim == nil {
		t.Fatal("InstallShim returned nil with a gh on PATH")
	}
	return shim, ledgerDir
}

type shimRun struct {
	stdout, stderr string
	exit           int
}

func runShim(t *testing.T, env map[string]string, args ...string) shimRun {
	t.Helper()
	gh := filepath.Join(strings.SplitN(env["PATH"], string(os.PathListSeparator), 2)[0], "gh")
	cmd := exec.Command(gh, args...) // #nosec G204 -- test runs the wrapper it just generated
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	exit := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run wrapper %v: %v", args, err)
	}
	return shimRun{stdout: stdout.String(), stderr: stderr.String(), exit: exit}
}

func TestShimRecordsNetworkedCallsAndPassesThrough_GitHubUsage_AC220401(t *testing.T) {
	shim, ledgerDir := installTestShim(t)
	env := shim.Env("cron:bs-verify")

	calls := [][]string{
		{"api", "repos/x/y"},
		{"pr", "view", "1"},
		{"api", "graphql"},
		{"api", "rate_limit"},
		{"--version"},
	}
	for _, args := range calls {
		got := runShim(t, env, args...)
		want := shimRun{stdout: "real gh: " + strings.Join(args, " ") + "\n", exit: 3}
		if got != want {
			t.Errorf("gh %v = %+v, want %+v", args, got, want)
		}
	}

	usage, err := New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{
		{Label: "cron:bs-verify", Quota: QuotaGraphQL, Calls: 2},
		{Label: "cron:bs-verify", Quota: QuotaCore, Calls: 1},
	}
	if !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("ledger counts = %+v, want %+v", usage.Counts, want)
	}
}

func TestShimRunsRealGHWhenTheLedgerCannotBeWritten_GitHubUsage_AC220401(t *testing.T) {
	shim, _ := installTestShim(t)
	env := shim.Env("session:interactive")
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env[EnvDir] = filepath.Join(blocker, "ledger")

	got := runShim(t, env, "pr", "list")
	want := shimRun{stdout: "real gh: pr list\n", exit: 3}
	if got != want {
		t.Fatalf("gh pr list with an unwritable ledger = %+v, want %+v", got, want)
	}
}

func TestShimEnvPutsTheWrapperFirstOnPath(t *testing.T) {
	shim, ledgerDir := installTestShim(t)
	env := shim.Env("Cron:My Job\"x")
	binDir := filepath.Join(ledgerDir, shimDirName)
	if first := strings.SplitN(env["PATH"], string(os.PathListSeparator), 2)[0]; first != binDir {
		t.Fatalf("PATH starts with %q, want the wrapper dir %q", first, binDir)
	}
	if env[EnvDir] != ledgerDir {
		t.Fatalf("%s = %q, want %q", EnvDir, env[EnvDir], ledgerDir)
	}
	if got, want := env[EnvLabel], "cron:my-job-x"; got != want {
		t.Fatalf("label = %q, want the sanitised %q", got, want)
	}
}

// A login shell inside a session rebuilds PATH with the real gh first. Claude
// Code sources CLAUDE_ENV_FILE before every Bash command, so that script must
// put the wrapper back in front, once, in every shell an agent may use.
func TestClaudeEnvFileRestoresTheWrapperAfterALoginShellReordersPath_GitHubUsage_AC220401(t *testing.T) {
	shim, ledgerDir := installTestShim(t)
	env := shim.Env("session:interactive")
	binDir := filepath.Join(ledgerDir, shimDirName)
	realDir := filepath.Dir(shim.real)
	reordered := realDir + string(os.PathListSeparator) + systemPath + string(os.PathListSeparator) + binDir

	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh"} {
		if _, err := os.Stat(sh); err != nil {
			continue
		}
		cmd := exec.Command(sh, "-c", `. "$CLAUDE_ENV_FILE" && . "$CLAUDE_ENV_FILE" && printf '%s\n%s\n' "$(command -v gh)" "$PATH"`) // #nosec G204 -- fixed test shells
		cmd.Env = []string{"PATH=" + reordered, EnvClaudeEnvFile + "=" + env[EnvClaudeEnvFile]}
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: source %s: %v", sh, EnvClaudeEnvFile, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if want := filepath.Join(binDir, "gh"); len(lines) != 2 || lines[0] != want {
			t.Fatalf("%s: gh resolves to %q, want the wrapper %q", sh, lines, want)
		}
		if want := binDir + string(os.PathListSeparator) + reordered; lines[1] != want {
			t.Fatalf("%s: PATH = %q, want the wrapper prepended once: %q", sh, lines[1], want)
		}
	}
}

func TestNilShimEnvIsEmpty(t *testing.T) {
	var shim *Shim
	if env := shim.Env("cron:x"); env != nil {
		t.Fatalf("nil shim Env = %v, want nil", env)
	}
}

func TestInstallShimWithoutGHRemovesStaleWrapperAndReturnsNil(t *testing.T) {
	_, ledgerDir := installTestShim(t)
	shim, err := InstallShim(ledgerDir, t.TempDir())
	if err != nil {
		t.Fatalf("InstallShim: %v", err)
	}
	if shim != nil {
		t.Fatalf("InstallShim with no gh on PATH = %+v, want nil", shim)
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, shimDirName, "gh")); !os.IsNotExist(err) {
		t.Fatalf("stale wrapper still present: %v", err)
	}
}

func TestInstallShimNeverResolvesItselfAsTheRealGH(t *testing.T) {
	first, ledgerDir := installTestShim(t)
	binDir := filepath.Join(ledgerDir, shimDirName)
	realDir := filepath.Dir(first.real)
	again, err := InstallShim(ledgerDir, binDir+string(os.PathListSeparator)+realDir)
	if err != nil {
		t.Fatalf("InstallShim: %v", err)
	}
	if again.real != first.real {
		t.Fatalf("real gh = %q, want %q (the wrapper must not wrap itself)", again.real, first.real)
	}
	if strings.Contains(again.basePath, binDir) {
		t.Fatalf("base PATH %q still names the wrapper dir", again.basePath)
	}
}

// A daemon started from another daemon's metered session sees that daemon's
// wrapper first on PATH. Taking it as the real gh would make the first
// wrapper exec itself forever, so InstallShim must skip it.
func TestInstallShimSkipsAnotherDaemonsWrapper(t *testing.T) {
	outer, outerLedger := installTestShim(t)
	outerBin := filepath.Join(outerLedger, shimDirName)
	realDir := filepath.Dir(outer.real)

	inner, err := InstallShim(filepath.Join(t.TempDir(), DirName), outerBin+string(os.PathListSeparator)+realDir+string(os.PathListSeparator)+systemPath)
	if err != nil {
		t.Fatalf("InstallShim: %v", err)
	}
	if inner.real != outer.real {
		t.Fatalf("real gh = %q, want %q (another daemon's wrapper is not the real gh)", inner.real, outer.real)
	}
}

// The wrapper falls back to the ledger dir and real gh it was generated with
// when a child process keeps PATH but loses the other variables.
func TestShimFallsBackToItsBuiltInLedgerAndRealGH(t *testing.T) {
	shim, ledgerDir := installTestShim(t)
	env := shim.Env("cron:bs-verify")
	delete(env, EnvDir)
	delete(env, EnvReal)

	got := runShim(t, env, "pr", "list")
	if want := (shimRun{stdout: "real gh: pr list\n", exit: 3}); got != want {
		t.Fatalf("gh pr list = %+v, want %+v", got, want)
	}
	usage, err := New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{{Label: "cron:bs-verify", Quota: QuotaGraphQL, Calls: 1}}
	if !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("ledger counts = %+v, want %+v", usage.Counts, want)
	}
}
