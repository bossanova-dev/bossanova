package ghusage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
	return runShimInput(t, env, "", "", args...)
}

func runShimInput(t *testing.T, env map[string]string, stdin, cwd string, args ...string) shimRun {
	t.Helper()
	gh := filepath.Join(strings.SplitN(env["PATH"], string(os.PathListSeparator), 2)[0], "gh")
	cmd := exec.Command(gh, args...) // #nosec G204 -- test runs the wrapper it just generated
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Dir = cwd
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
		{Label: "cron:bs-verify", Quota: QuotaGraphQL, Identity: IdentityUser, Calls: 2},
		{Label: "cron:bs-verify", Quota: QuotaCore, Identity: IdentityUser, Calls: 1},
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
	home := t.TempDir()

	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh"} {
		if _, err := os.Stat(sh); err != nil {
			continue
		}
		cmd := exec.Command(sh, "-c", `. "$CLAUDE_ENV_FILE" && . "$CLAUDE_ENV_FILE" && printf '%s\n%s\n' "$(command -v gh)" "$PATH"`) // #nosec G204 -- fixed test shells
		cmd.Env = []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=" + reordered, EnvClaudeEnvFile + "=" + env[EnvClaudeEnvFile]}
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
	want := []Count{{Label: "cron:bs-verify", Quota: QuotaGraphQL, Identity: IdentityUser, Calls: 1}}
	if !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("ledger counts = %+v, want %+v", usage.Counts, want)
	}
}

// Without a helper, every metered gate and session call uses the user identity.
func TestShimRecordsTheUserIdentity(t *testing.T) {
	shim, ledgerDir := installTestShim(t)
	runShim(t, shim.Env("cron:bs-verify"), "api", "repos/x/y")
	recs := bucketRecords(t, ledgerDir, time.Now())
	if len(recs) == 0 {
		// The call straddled an hour boundary; the record is in the previous bucket.
		recs = bucketRecords(t, ledgerDir, time.Now().Add(-time.Hour))
	}
	if len(recs) != 1 || recs[0].Identity != IdentityUser {
		t.Fatalf("records = %+v, want one with identity %q", recs, IdentityUser)
	}
}

func fakeReadCredentialHelper(t *testing.T, answer string, status int) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "helper's dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls")
	script := fmt.Sprintf("#!/bin/sh\n{ printf 'arg=%%s\\n' \"$@\"; printf 'cwd=%%s\\nhost=%%s\\nrepo=%%s\\nstdin=' \"$PWD\" \"${GH_HOST-}\" \"${GH_REPO-}\"; cat; printf '\\n'; } >>%s\nprintf 'helper stderr' >&2\nprintf '%%s' %s\nexit %d\n", shellQuote(logPath), shellQuote(answer), status)
	helper := filepath.Join(dir, "boss")
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	return helper, logPath
}

func installCredentialShim(t *testing.T, helper string) (*Shim, string) {
	t.Helper()
	realDir := t.TempDir()
	script := `#!/bin/sh
printf 'GH_TOKEN=%s\nGITHUB_TOKEN=%s\nGH_ENTERPRISE_TOKEN=%s\nGITHUB_ENTERPRISE_TOKEN=%s\n' "${GH_TOKEN-}" "${GITHUB_TOKEN-}" "${GH_ENTERPRISE_TOKEN-}" "${GITHUB_ENTERPRISE_TOKEN-}"
printf 'arg=%s\n' "$@"
cat
printf 'gh stderr\n' >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(realDir, "gh"), []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(t.TempDir(), DirName)
	shim, err := InstallShim(ledgerDir, realDir+string(os.PathListSeparator)+systemPath, WithReadCredentialHelper(helper))
	if err != nil || shim == nil {
		t.Fatalf("InstallShim = %v, %v", shim, err)
	}
	return shim, ledgerDir
}

func assertCredentialCall(t *testing.T, shim *Shim, ledgerDir, identity, token, otherTokens, stdin, cwd string, args ...string) {
	t.Helper()
	env := shim.Env("cron:credential-test")
	env["GH_TOKEN"] = "user-env"
	for _, key := range []string{"GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		env[key] = "inherited"
	}
	env["GH_HOST"], env["GH_REPO"] = "github.com", "x/y"
	// Caller-controlled binary variables cannot replace the embedded helper.
	env["helper"], env["BOSS_BIN"], env["BOSS_GH_READ_CREDENTIAL_HELPER"] = "/untrusted/boss", "/untrusted/boss", "/untrusted/boss"
	got := runShimInput(t, env, stdin, cwd, args...)
	want := shimRun{
		stdout: fmt.Sprintf("GH_TOKEN=%s\nGITHUB_TOKEN=%s\nGH_ENTERPRISE_TOKEN=%s\nGITHUB_ENTERPRISE_TOKEN=%s\n", token, otherTokens, otherTokens, otherTokens) + "arg=" + strings.Join(args, "\narg=") + "\n" + stdin,
		stderr: "gh stderr\n", exit: 3,
	}
	if got != want {
		t.Fatalf("gh %v = %+v, want %+v", args, got, want)
	}
	usage, err := New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	quota := QuotaGraphQL
	if args[0] == "api" && args[1] != "graphql" {
		quota = QuotaCore
	}
	wantCounts := []Count{{Label: "cron:credential-test", Quota: quota, Identity: identity, Calls: 1}}
	if !reflect.DeepEqual(usage.Counts, wantCounts) {
		t.Fatalf("counts = %+v, want %+v", usage.Counts, wantCounts)
	}
}

func TestShimRoutesReadCredentials_GitHubUsage_AC233001(t *testing.T) {
	for _, args := range [][]string{{"pr", "view", "12"}, {"api", "repos/x/y"}, {"run", "list"}} {
		t.Run(args[0], func(t *testing.T) {
			helper, _ := fakeReadCredentialHelper(t, "4242 ghs_fakeToken123\n", 0)
			shim, ledgerDir := installCredentialShim(t, helper)
			assertCredentialCall(t, shim, ledgerDir, InstallationIdentity(4242), "ghs_fakeToken123", "", "", "", args...)
			if err := filepath.WalkDir(ledgerDir, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil || entry.IsDir() {
					return walkErr
				}
				data, err := os.ReadFile(path)
				if err == nil && bytes.Contains(data, []byte("ghs_fakeToken123")) {
					t.Errorf("token persisted in %s", path)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShimPreservesUnroutedWrite_GitHubUsage_AC233001(t *testing.T) {
	helper, logPath := fakeReadCredentialHelper(t, "", 0)
	shim, ledgerDir := installCredentialShim(t, helper)
	assertCredentialCall(t, shim, ledgerDir, IdentityUser, "user-env", "inherited", "", "", "pr", "merge", "1")
	log, err := os.ReadFile(logPath)
	if err != nil || !strings.HasPrefix(string(log), "arg=__gh-read-credential\narg=--\narg=pr\narg=merge\narg=1\n") {
		t.Fatalf("helper args = %q, %v", log, err)
	}
}

func TestShimRejectsBadCredentials_GitHubUsage_AC233001(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		status       int
	}{
		{"empty", "", 0}, {"token only", "ghs_x", 0}, {"bad id", "abc ghs_x", 0},
		{"extra line", "42 ghs_x\nextra", 0}, {"punctuation", "42 tok;rm", 0},
		{"failed", "42 ghs_x", 1}, {"empty id", " ghs_x", 0}, {"empty token", "42 ", 0},
		{"extra field", "42 ghs_x extra", 0}, {"missing helper", "42 ghs_x", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			helper, _ := fakeReadCredentialHelper(t, tc.answer, tc.status)
			shim, ledgerDir := installCredentialShim(t, helper)
			if tc.name == "missing helper" {
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
			}
			assertCredentialCall(t, shim, ledgerDir, IdentityUser, "user-env", "inherited", "", "", "pr", "view", "1")
		})
	}
}

func TestShimRejectsNonASCIIInLocale_GitHubUsage_AC233001(t *testing.T) {
	cmd := exec.Command("/usr/bin/locale", "-a")
	cmd.WaitDelay = 5 * time.Second
	locales, err := cmd.Output()
	if err != nil {
		t.Skipf("locale enumeration unavailable: %v", err)
	}
	locale := ""
	for _, candidate := range strings.Fields(string(locales)) {
		if strings.EqualFold(candidate, "en_US.UTF-8") || strings.EqualFold(candidate, "en_US.utf8") {
			locale = candidate
			break
		}
	}
	if locale == "" {
		t.Skip("en_US UTF-8 locale unavailable")
	}
	for _, answer := range []string{"42 ghs_é", "٤٢ ghs_x"} {
		t.Run(answer, func(t *testing.T) {
			helper, _ := fakeReadCredentialHelper(t, answer, 0)
			shim, ledgerDir := installCredentialShim(t, helper)
			env := shim.Env("cron:locale")
			env["LC_ALL"] = locale
			got := runShim(t, env, "pr", "view", "1")
			want := shimRun{stdout: "GH_TOKEN=\nGITHUB_TOKEN=\nGH_ENTERPRISE_TOKEN=\nGITHUB_ENTERPRISE_TOKEN=\narg=pr\narg=view\narg=1\n", stderr: "gh stderr\n", exit: 3}
			if got != want {
				t.Fatalf("gh = %+v, want %+v", got, want)
			}
			usage, err := New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
			wantCounts := []Count{{Label: "cron:locale", Quota: QuotaGraphQL, Identity: IdentityUser, Calls: 1}}
			if err != nil || !reflect.DeepEqual(usage.Counts, wantCounts) {
				t.Fatalf("counts = %+v, %v, want %+v", usage.Counts, err, wantCounts)
			}
		})
	}
}

func TestShimPreservesStdinAndHelperContext_GitHubUsage_AC233001(t *testing.T) {
	for _, answer := range []string{"", "42 ghs_x"} {
		t.Run(answer, func(t *testing.T) {
			helper, logPath := fakeReadCredentialHelper(t, answer, 0)
			shim, ledgerDir := installCredentialShim(t, helper)
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			identity, token, others := IdentityUser, "user-env", "inherited"
			if answer != "" {
				identity, token, others = InstallationIdentity(42), "ghs_x", ""
			}
			assertCredentialCall(t, shim, ledgerDir, identity, token, others, "payload\n", cwd, "api", "graphql", "--input", "-")
			log, err := os.ReadFile(logPath)
			want := "arg=__gh-read-credential\narg=--\narg=api\narg=graphql\narg=--input\narg=-\ncwd=" + cwd + "\nhost=github.com\nrepo=x/y\nstdin=\n"
			if err != nil || string(log) != want {
				t.Fatalf("helper context = %q, %v, want %q", log, err, want)
			}
		})
	}
}

func TestShimSkipsHelperForOtherCalls_GitHubUsage_AC233001(t *testing.T) {
	helper, logPath := fakeReadCredentialHelper(t, "42 ghs_x", 0)
	shim, ledgerDir := installCredentialShim(t, helper)
	for _, args := range [][]string{{"--version"}, {"api", "rate_limit"}, {"api", "/rate_limit"}, {"api"}, {"issue", "list"}, {"repo", "view"}, {"auth", "status"}} {
		got := runShim(t, shim.Env("cron:other-calls"), args...)
		if got.exit != 3 || got.stderr != "gh stderr\n" || !strings.Contains(got.stdout, "GH_TOKEN=\n") {
			t.Fatalf("gh %v = %+v", args, got)
		}
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("helper consulted: %v", err)
	}
	usage, err := New(ledgerDir, zerolog.Nop()).Read(time.Now().Add(time.Second), time.Hour)
	want := []Count{{Label: "cron:other-calls", Quota: QuotaGraphQL, Identity: IdentityUser, Calls: 2}}
	if err != nil || !reflect.DeepEqual(usage.Counts, want) {
		t.Fatalf("counts = %+v, %v, want %+v", usage.Counts, err, want)
	}
}

func TestShimDropsUnusableHelpers_GitHubUsage_AC233001(t *testing.T) {
	for _, kind := range []string{"empty", "relative", "directory", "non-executable", "missing"} {
		t.Run(kind, func(t *testing.T) {
			helper, logPath := fakeReadCredentialHelper(t, "42 ghs_x", 0)
			switch kind {
			case "empty":
				helper = ""
			case "relative":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				helper, err = filepath.Rel(cwd, helper)
				if err != nil {
					t.Fatal(err)
				}
			case "directory":
				helper = filepath.Dir(helper)
			case "non-executable":
				if err := os.Chmod(helper, 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(helper); err != nil {
					t.Fatal(err)
				}
			}
			shim, ledgerDir := installCredentialShim(t, helper)
			assertCredentialCall(t, shim, ledgerDir, IdentityUser, "user-env", "inherited", "", "", "pr", "view", "1")
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				t.Fatalf("helper consulted: %v", err)
			}
		})
	}
}
