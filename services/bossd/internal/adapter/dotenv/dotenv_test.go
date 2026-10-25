package dotenv

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/proofenv"
)

// writeEnvFile creates dir/.env with the given content and returns dir.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestLoadMissingFileReturnsNil(t *testing.T) {
	if got := Load(t.TempDir()); got != nil {
		t.Fatalf("Load on dir without .env = %v, want nil", got)
	}
}

func TestLoadEmptyAndCommentOnlyFileReturnsNil(t *testing.T) {
	dir := writeEnvFile(t, "# only a comment\n\n   \n# another\n")
	if got := Load(dir); got != nil {
		t.Fatalf("Load on comment-only .env = %v, want nil", got)
	}
}

func TestLoadParsesRealWorldShapes(t *testing.T) {
	dir := writeEnvFile(t, `# Linear
LINEAR_API_KEY=lin_api_abc123

export EXPORTED=yes
DOUBLE_QUOTED="hello world"
SINGLE_QUOTED='single value'
SPACED_KEY = padded
EMPTY=
HASH_IN_VALUE=se#cret
CRLF_VALUE=trimmed`+"\r\n")
	want := map[string]string{
		"LINEAR_API_KEY": "lin_api_abc123",
		"EXPORTED":       "yes",
		"DOUBLE_QUOTED":  "hello world",
		"SINGLE_QUOTED":  "single value",
		"SPACED_KEY":     "padded",
		"EMPTY":          "",
		"HASH_IN_VALUE":  "se#cret",
		"CRLF_VALUE":     "trimmed",
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	dir := writeEnvFile(t, `no_equals_sign_here
=value_without_key
1BAD=starts with digit
BAD KEY=space in key
bad-key=hyphen
GOOD=kept
"QUOTED_KEY"=skipped
`)
	want := map[string]string{"GOOD": "kept"}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadKeepsUnbalancedQuotes(t *testing.T) {
	dir := writeEnvFile(t, `LEADING="unclosed
MIXED="double'
SHORT="
`)
	want := map[string]string{
		"LEADING": `"unclosed`,
		"MIXED":   `"double'`,
		"SHORT":   `"`,
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestOverlayBaseWinsOnConflict(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\nLINEAR_API_KEY=lin_api_abc123\n")
	base := map[string]string{"BOSS_SETTINGS_PATH": "/managed/settings.json"}
	got := Overlay(base, dir)
	want := map[string]string{
		"BOSS_SETTINGS_PATH": "/managed/settings.json",
		"LINEAR_API_KEY":     "lin_api_abc123",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay = %v, want %v", got, want)
	}
	// The input map must not be mutated.
	if len(base) != 1 {
		t.Fatalf("Overlay mutated base: %v", base)
	}
}

func TestOverlayWithoutDotenvReturnsBaseUnchanged(t *testing.T) {
	base := map[string]string{"BOSS_AGENT": "claude"}
	if got := Overlay(base, t.TempDir()); !maps.Equal(got, base) {
		t.Fatalf("Overlay without .env = %v, want %v", got, base)
	}
}

func TestOverlayNilBaseReturnsDotenvOnly(t *testing.T) {
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_api_abc123\n")
	got := Overlay(nil, dir)
	want := map[string]string{"LINEAR_API_KEY": "lin_api_abc123"}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay(nil) = %v, want %v", got, want)
	}
}

func TestOverlayNilBaseNoDotenvReturnsNil(t *testing.T) {
	if got := Overlay(nil, t.TempDir()); got != nil {
		t.Fatalf("Overlay(nil, empty dir) = %v, want nil", got)
	}
}

// repoWith returns a *models.Repo carrying only the three secret fields
// OverlayWithRepo reads.
func repoWith(linear, sentryKey, sentryOrg string) *models.Repo {
	return &models.Repo{LinearAPIKey: linear, SentryAPIKey: sentryKey, SentryOrg: sentryOrg}
}

func TestOverlayWithRepoPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		envFile  string // "" → no .env written
		base     map[string]string
		repo     *models.Repo      // nil → repo lookup failed
		wantKV   map[string]string // keys asserted present with exact value
		wantGone []string          // keys asserted absent
	}{
		{
			name:   "repo config fills when no .env",
			repo:   repoWith("lin_repo", "", ""),
			wantKV: map[string]string{"LINEAR_API_KEY": "lin_repo"},
		},
		{
			name:    "worktree .env wins over repo config",
			envFile: "LINEAR_API_KEY=lin_env\n",
			repo:    repoWith("lin_repo", "", ""),
			wantKV:  map[string]string{"LINEAR_API_KEY": "lin_env"},
		},
		{
			name:    "worktree .env delivered when repo has no key",
			envFile: "LINEAR_API_KEY=lin_env\n",
			repo:    repoWith("", "", ""),
			wantKV:  map[string]string{"LINEAR_API_KEY": "lin_env"},
		},
		{
			name:   "fail-clean: key present and empty when configured nowhere",
			repo:   repoWith("", "", ""),
			wantKV: map[string]string{"LINEAR_API_KEY": ""},
		},
		{
			name:   "fail-clean guarantee still fires when repo is nil",
			repo:   nil,
			wantKV: map[string]string{"LINEAR_API_KEY": ""},
		},
		{
			name:    "managed base value survives .env and repo shadow attempts",
			envFile: "BOSS_SESSION_ID=from_env\nLINEAR_API_KEY=lin_env\n",
			base:    map[string]string{"BOSS_SESSION_ID": "managed"},
			repo:    repoWith("lin_repo", "", ""),
			wantKV:  map[string]string{"BOSS_SESSION_ID": "managed", "LINEAR_API_KEY": "lin_env"},
		},
		{
			name:   "empty repo sentry key is pinned empty, non-empty org added",
			repo:   repoWith("lin_repo", "", "acme"),
			wantKV: map[string]string{"LINEAR_API_KEY": "lin_repo", "SENTRY_ORG": "acme", "SENTRY_API_KEY": ""},
		},
		{
			name:    "worktree .env wins over repo sentry values too",
			envFile: "SENTRY_ORG=env_org\n",
			repo:    repoWith("lin_repo", "repo_sentry_key", "repo_org"),
			wantKV: map[string]string{
				"SENTRY_ORG":     "env_org",
				"SENTRY_API_KEY": "repo_sentry_key",
				"LINEAR_API_KEY": "lin_repo",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.envFile != "" {
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(tt.envFile), 0o600); err != nil {
					t.Fatalf("write .env: %v", err)
				}
			}
			got := OverlayWithRepo(tt.base, dir, tt.repo)
			for k, want := range tt.wantKV {
				v, ok := got[k]
				if !ok {
					t.Errorf("key %q absent, want %q", k, want)
					continue
				}
				if v != want {
					t.Errorf("key %q = %q, want %q", k, v, want)
				}
			}
			for _, k := range tt.wantGone {
				if v, ok := got[k]; ok {
					t.Errorf("key %q present (=%q), want absent", k, v)
				}
			}
		})
	}
}

// TestOverlayWithRepoDoesNotMutateBase guards the maps.Clone: Overlay returns
// base by reference when the worktree has no .env, so OverlayWithRepo must not
// write repo/guarantee keys back into the caller's base map.
func TestOverlayWithRepoDoesNotMutateBase(t *testing.T) {
	base := map[string]string{"BOSS_AGENT": "claude"}
	got := OverlayWithRepo(base, t.TempDir(), repoWith("lin_repo", "", ""))
	if _, ok := base["LINEAR_API_KEY"]; ok {
		t.Fatalf("OverlayWithRepo mutated base: %v", base)
	}
	if got["LINEAR_API_KEY"] != "lin_repo" {
		t.Fatalf("LINEAR_API_KEY = %q, want lin_repo", got["LINEAR_API_KEY"])
	}
	if got["BOSS_AGENT"] != "claude" {
		t.Fatalf("BOSS_AGENT = %q, want claude", got["BOSS_AGENT"])
	}
}

// redirectKeys are the CLI-redirecting keys a worktree .env must never forward
// into a session (BOS-1512). Spelled out here rather than read from the
// package so the test pins the contract independently of the implementation.
var redirectKeys = []string{
	"BOSS_SETTINGS_PATH",
	"BOSS_KEYRING_BACKEND",
	"BOSS_WEB_PORT",
	"BOSS_CLOUD_URL",
	"BOSSD_ORCHESTRATOR_URL",
}

func TestOverlayDropsRedirectKeysFromDotenv(t *testing.T) {
	for _, key := range redirectKeys {
		t.Run(key, func(t *testing.T) {
			dir := writeEnvFile(t, key+"=/from/dotenv\nORDINARY_KEY=kept\n")
			for name, got := range map[string]map[string]string{
				"Overlay":         Overlay(map[string]string{"BOSS_AGENT": "claude"}, dir),
				"OverlayWithRepo": OverlayWithRepo(map[string]string{"BOSS_AGENT": "claude"}, dir, repoWith("lin_repo", "", "")),
			} {
				if v, ok := got[key]; ok {
					t.Errorf("%s forwarded %s from .env (=%q), want absent", name, key, v)
				}
				if got["ORDINARY_KEY"] != "kept" {
					t.Errorf("%s ORDINARY_KEY = %q, want kept", name, got["ORDINARY_KEY"])
				}
				if got["BOSS_AGENT"] != "claude" {
					t.Errorf("%s BOSS_AGENT = %q, want claude", name, got["BOSS_AGENT"])
				}
			}
		})
	}
}

func TestOverlayDropsRedirectKeysWhenTheyAreTheOnlyKeys(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\nBOSS_WEB_PORT=1234\n")
	base := map[string]string{"BOSS_AGENT": "claude"}
	if got := Overlay(base, dir); !maps.Equal(got, base) {
		t.Fatalf("Overlay = %v, want %v", got, base)
	}
	if got := Overlay(nil, dir); len(got) != 0 {
		t.Fatalf("Overlay(nil) = %v, want empty", got)
	}
}

func TestOverlayKeepsRedirectKeysSuppliedByBase(t *testing.T) {
	for _, key := range redirectKeys {
		t.Run(key, func(t *testing.T) {
			dir := writeEnvFile(t, key+"=/from/dotenv\n")
			base := map[string]string{key: "managed"}
			for name, got := range map[string]map[string]string{
				"Overlay":         Overlay(base, dir),
				"OverlayWithRepo": OverlayWithRepo(base, dir, nil),
			} {
				if got[key] != "managed" {
					t.Errorf("%s %s = %q, want managed", name, key, got[key])
				}
			}
		})
	}
}

// TestLoadStillParsesRedirectKeys pins that the filter lives in Overlay: Load
// stays a pure parser of the file.
func TestLoadStillParsesRedirectKeys(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\n")
	if got := Load(dir)["BOSS_SETTINGS_PATH"]; got != "/from/dotenv" {
		t.Fatalf("Load BOSS_SETTINGS_PATH = %q, want /from/dotenv", got)
	}
}

// runGitIn runs git in dir with a throwaway identity and fails the test on
// error.
func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-C", dir,
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
	}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitRepoWithEnv initialises a git repository in a fresh temp dir, writes
// .env with content, and — when track is true — commits it, so the file is in
// the index the way a branch-supplied .env would be. When track is false the
// file is gitignored, the shape the setup step leaves behind.
func gitRepoWithEnv(t *testing.T, content string, track bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		runGitIn(t, dir, args...)
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if track {
		runGit("add", ".env")
		runGit("commit", "-q", "-m", "commit .env")
	} else {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".env\n"), 0o600); err != nil {
			t.Fatalf("write .gitignore: %v", err)
		}
		runGit("add", ".gitignore")
		runGit("commit", "-q", "-m", "ignore .env")
	}
	return dir
}

// TestLoadIgnoresTrackedDotenv pins that a .env the checked-out branch
// supplies (tracked in the index) contributes nothing: the branch author is
// not trusted to steer the agent environment.
func TestLoadIgnoresTrackedDotenv(t *testing.T) {
	dir := gitRepoWithEnv(t, "HOME=.h\nLINEAR_API_KEY=lin_branch\n", true)
	if got := Load(dir); got != nil {
		t.Fatalf("Load on tracked .env = %v, want nil", got)
	}
	got := OverlayWithRepo(map[string]string{"BOSS_AGENT": "claude"}, dir, repoWith("lin_repo", "", ""))
	if _, ok := got["HOME"]; ok {
		t.Fatalf("tracked .env HOME reached the overlay: %v", got)
	}
	if got["LINEAR_API_KEY"] != "lin_repo" {
		t.Fatalf("LINEAR_API_KEY = %q, want the repo value lin_repo", got["LINEAR_API_KEY"])
	}
}

// TestLoadIgnoresTrackedDotenvOfOtherCase pins that the tracked check matches
// the name case-insensitively: on a case-insensitive filesystem a committed
// .ENV is the file Load would open as .env.
func TestLoadIgnoresTrackedDotenvOfOtherCase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGitIn(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, ".ENV"), []byte("LINEAR_API_KEY=lin_branch\n"), 0o600); err != nil {
		t.Fatalf("write .ENV: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Skip("temp filesystem is case-sensitive; .ENV is not .env here")
	}
	runGitIn(t, dir, "add", ".ENV")
	runGitIn(t, dir, "commit", "-q", "-m", "commit .ENV")
	if got := Load(dir); got != nil {
		t.Fatalf("Load with tracked .ENV = %v, want nil", got)
	}
}

// TestTrackedInIndexMatchesOtherCase exercises the tracked check itself, so
// the case-insensitive match is pinned on every filesystem: the index holds
// .ENV (core.ignorecase off, as on Linux) and the check must still report a
// tracked .env. TestLoadIgnoresTrackedDotenvOfOtherCase covers the end-to-end
// read, but only where the filesystem folds case.
func TestTrackedInIndexMatchesOtherCase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGitIn(t, dir, "init", "-q")
	runGitIn(t, dir, "config", "core.ignorecase", "false")
	if err := os.WriteFile(filepath.Join(dir, ".ENV"), []byte("LINEAR_API_KEY=lin_branch\n"), 0o600); err != nil {
		t.Fatalf("write .ENV: %v", err)
	}
	runGitIn(t, dir, "add", ".ENV")
	tracked, err := trackedInIndex(dir)
	if err != nil {
		t.Fatalf("trackedInIndex: %v", err)
	}
	if !tracked {
		t.Fatal("trackedInIndex = false for an index holding .ENV, want true")
	}
}

// TestLoadTrackedCheckIgnoresInheritedGitEnv pins that the daemon's own GIT_*
// variables cannot steer the tracked check at another repository or index,
// which would read the branch's committed .env as untracked.
func TestLoadTrackedCheckIgnoresInheritedGitEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(t *testing.T) (string, string)
	}{
		{"GIT_INDEX_FILE", func(t *testing.T) (string, string) {
			return "GIT_INDEX_FILE", filepath.Join(t.TempDir(), "empty-index")
		}},
		{"GIT_DIR", func(t *testing.T) (string, string) {
			other := t.TempDir()
			runGitIn(t, other, "init", "-q")
			return "GIT_DIR", filepath.Join(other, ".git")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepoWithEnv(t, "LINEAR_API_KEY=lin_branch\n", true)
			t.Setenv(tc.env(t))
			if got := Load(dir); got != nil {
				t.Fatalf("Load on tracked .env with %s set = %v, want nil", tc.name, got)
			}
		})
	}
}

// TestLoadFailsClosedOnBrokenWorktreeGitfile pins that only git's genuine
// "no repository here" answer reads as untracked. A linked worktree whose
// .git file points at a missing gitdir (pruned or moved) makes git fail with
// "not a git repository: <gitdir>"; that is a repository git could not open,
// so the .env — possibly branch-committed — must be refused, not loaded.
func TestLoadFailsClosedOnBrokenWorktreeGitfile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_branch\n")
	gitfile := "gitdir: " + filepath.Join(t.TempDir(), "missing", "worktrees", "wt") + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(gitfile), 0o600); err != nil {
		t.Fatalf("write .git: %v", err)
	}
	if got := Load(dir); got != nil {
		t.Fatalf("Load in a worktree with a broken gitfile = %v, want nil", got)
	}
}

// TestLoadKeepsDotenvOutsideAnyRepository pins the other side: a directory
// git finds no repository for has no index, so its .env loads.
func TestLoadKeepsDotenvOutsideAnyRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_env\n")
	if got := Load(dir)["LINEAR_API_KEY"]; got != "lin_env" {
		t.Fatalf("Load LINEAR_API_KEY outside a repository = %q, want lin_env", got)
	}
}

// TestLoadKeepsUntrackedDotenv pins that a gitignored .env written by the
// setup step inside a real git worktree still loads.
func TestLoadKeepsUntrackedDotenv(t *testing.T) {
	dir := gitRepoWithEnv(t, "LINEAR_API_KEY=lin_setup\n", false)
	if got := Load(dir)["LINEAR_API_KEY"]; got != "lin_setup" {
		t.Fatalf("Load LINEAR_API_KEY = %q, want lin_setup", got)
	}
}

// TestLoadIgnoresSymlinkDotenv pins that a symlinked .env is refused: the
// link target can live anywhere, including inside the branch's own tree.
func TestLoadIgnoresSymlinkDotenv(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.env")
	if err := os.WriteFile(target, []byte("LINEAR_API_KEY=lin_link\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".env")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got := Load(dir); got != nil {
		t.Fatalf("Load on symlinked .env = %v, want nil", got)
	}
}

// TestLoadFailsClosedWhenGitCannotRun pins the fail-closed branch: when the
// tracked check cannot be performed at all, the file is skipped.
func TestLoadFailsClosedWhenGitCannotRun(t *testing.T) {
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_env\n")
	prev := gitBinary
	gitBinary = filepath.Join(t.TempDir(), "no-such-git")
	t.Cleanup(func() { gitBinary = prev })
	if got := Load(dir); got != nil {
		t.Fatalf("Load with unrunnable git = %v, want nil", got)
	}
}

// deniedDotenvKeys are the keys no .env may supply, spelled out here rather
// than read from the package so the test pins the contract independently of
// the implementation. Prefix entries are represented by a concrete key.
var deniedDotenvKeys = []string{
	"PATH",
	"ZDOTDIR",
	"BASH_ENV",
	"ENV",
	"PROMPT_COMMAND",
	"NODE_OPTIONS",
	"PYTHONSTARTUP",
	"CLAUDE_CONFIG_DIR",
	"ANTHROPIC_BASE_URL",
	"OPENAI_BASE_URL",
	"DYLD_INSERT_LIBRARIES",
	"DYLD_LIBRARY_PATH",
	"LD_PRELOAD",
	"GIT_SSH_COMMAND",
	"GIT_CONFIG_GLOBAL",
}

func TestOverlayDropsDeniedKeys(t *testing.T) {
	for _, key := range deniedDotenvKeys {
		t.Run(key, func(t *testing.T) {
			dir := writeEnvFile(t, key+"=/from/dotenv\nORDINARY_KEY=kept\n")
			for name, got := range map[string]map[string]string{
				"Overlay":         Overlay(map[string]string{"BOSS_AGENT": "claude"}, dir),
				"OverlayWithRepo": OverlayWithRepo(map[string]string{"BOSS_AGENT": "claude"}, dir, repoWith("lin_repo", "", "")),
			} {
				if v, ok := got[key]; ok {
					t.Errorf("%s forwarded %s from .env (=%q), want absent", name, key, v)
				}
				if got["ORDINARY_KEY"] != "kept" {
					t.Errorf("%s ORDINARY_KEY = %q, want kept", name, got["ORDINARY_KEY"])
				}
			}
		})
	}
}

// TestOverlayKeepsDeniedKeysSuppliedByBase pins that the denylist filters only
// the .env side: a managed base value still passes through.
func TestOverlayKeepsDeniedKeysSuppliedByBase(t *testing.T) {
	dir := writeEnvFile(t, "PATH=/from/dotenv\n")
	got := Overlay(map[string]string{"PATH": "/managed/bin"}, dir)
	if got["PATH"] != "/managed/bin" {
		t.Fatalf("PATH = %q, want /managed/bin", got["PATH"])
	}
}

// TestOverlayKeepsRelativeHomeFromUntrackedDotenv pins the BOS-1749 use case:
// a setup-written (untracked) .env may still point HOME / CODEX_HOME inside
// the worktree.
func TestOverlayKeepsRelativeHomeFromUntrackedDotenv(t *testing.T) {
	dir := gitRepoWithEnv(t, "HOME=.h\nCODEX_HOME=.codex\n", false)
	got := Overlay(nil, dir)
	if got["HOME"] != ".h" || got["CODEX_HOME"] != ".codex" {
		t.Fatalf("Overlay = %v, want HOME=.h and CODEX_HOME=.codex", got)
	}
}

// TestOverlayWithRepoDropsConfigHomesFromUntrustedDotenv pins BOS-1562 for
// the agent config-home selectors: an untrusted repo's (untracked) worktree
// .env may not set HOME or CODEX_HOME, because either one chooses which agent
// config — and with it which project trust — the launch reads, sidestepping
// the trust gate. A value base supplies (the managed or account env) is kept,
// and a trusted repo keeps the BOS-1749 relative homes.
func TestOverlayWithRepoDropsConfigHomesFromUntrustedDotenv(t *testing.T) {
	for name, repo := range map[string]*models.Repo{
		"untrusted repo": {IsTrusted: false},
		"nil repo":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := gitRepoWithEnv(t, "HOME=.h\nCODEX_HOME=.codex\nORDINARY_KEY=kept\n", false)
			got := OverlayWithRepo(nil, dir, repo)
			for _, key := range []string{"HOME", "CODEX_HOME"} {
				if v, ok := got[key]; ok {
					t.Errorf("%s = %q from an untrusted .env, want absent", key, v)
				}
			}
			if got["ORDINARY_KEY"] != "kept" {
				t.Errorf("ORDINARY_KEY = %q, want the .env value kept", got["ORDINARY_KEY"])
			}

			base := map[string]string{"HOME": "/account/home", "CODEX_HOME": "/account/codex"}
			got = OverlayWithRepo(base, dir, repo)
			if got["HOME"] != "/account/home" || got["CODEX_HOME"] != "/account/codex" {
				t.Errorf("homes = HOME=%q CODEX_HOME=%q, want the base values", got["HOME"], got["CODEX_HOME"])
			}
		})
	}
	t.Run("trusted repo", func(t *testing.T) {
		dir := gitRepoWithEnv(t, "HOME=.h\nCODEX_HOME=.codex\n", false)
		got := OverlayWithRepo(nil, dir, &models.Repo{IsTrusted: true})
		if got["HOME"] != ".h" || got["CODEX_HOME"] != ".codex" {
			t.Fatalf("homes = HOME=%q CODEX_HOME=%q, want the .env's .h and .codex", got["HOME"], got["CODEX_HOME"])
		}
	})
}

// TestOverlayWithRepoPinsEmptySentryKeys pins that an unconfigured repo (and a
// failed repo lookup) still emits SENTRY_API_KEY and SENTRY_ORG, empty, so an
// ambient daemon or plugin value cannot show through to the agent.
func TestOverlayWithRepoPinsEmptySentryKeys(t *testing.T) {
	for name, repo := range map[string]*models.Repo{
		"unconfigured repo": repoWith("", "", ""),
		"nil repo":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := OverlayWithRepo(nil, t.TempDir(), repo)
			for _, key := range []string{"SENTRY_API_KEY", "SENTRY_ORG", "LINEAR_API_KEY"} {
				v, ok := got[key]
				if !ok {
					t.Errorf("key %q absent, want present and empty", key)
					continue
				}
				if v != "" {
					t.Errorf("key %q = %q, want empty", key, v)
				}
			}
		})
	}
}

// TestOverlayWithRepoPinsProofKeysForUntrustedRepo pins BOS-1562: a repo that
// may not receive the proof overlay gets every proof key pinned to "", while a
// trusted repo's proof keys are left to base.
func TestOverlayWithRepoPinsProofKeysForUntrustedRepo(t *testing.T) {
	for name, repo := range map[string]*models.Repo{
		"untrusted repo": {IsTrusted: false},
		"nil repo":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := OverlayWithRepo(nil, t.TempDir(), repo)
			for _, key := range proofenv.Keys() {
				if v, ok := got[key]; !ok || v != "" {
					t.Errorf("%s = %q (present %v), want present and empty", key, v, ok)
				}
			}
		})
	}
	t.Run("trusted repo", func(t *testing.T) {
		base := map[string]string{proofenv.EnvAnthropicAPIKey: "proof-key"}
		got := OverlayWithRepo(base, t.TempDir(), &models.Repo{IsTrusted: true})
		if v := got[proofenv.EnvAnthropicAPIKey]; v != "proof-key" {
			t.Errorf("%s = %q, want the base value", proofenv.EnvAnthropicAPIKey, v)
		}
		if v, ok := got[proofenv.EnvCloudflareAPIToken]; !ok || v != "" {
			t.Errorf("%s = %q, want pinned empty", proofenv.EnvCloudflareAPIToken, v)
		}
	})
}

// TestOverlayWithRepoHidesAmbientProofKeyFromUntrustedChild proves the pin
// reaches a real child process: an ambient PROOF_ANTHROPIC_API_KEY in the
// parent environment does not survive the overlay being appended after
// os.Environ(), the way the headless runner builds a spawn's environment.
func TestOverlayWithRepoHidesAmbientProofKeyFromUntrustedChild(t *testing.T) {
	t.Setenv(proofenv.EnvAnthropicAPIKey, "ambient-daemon-secret")
	envBin, err := exec.LookPath("env")
	if err != nil {
		t.Skipf("env binary unavailable: %v", err)
	}
	env := os.Environ()
	for k, v := range OverlayWithRepo(nil, t.TempDir(), &models.Repo{IsTrusted: false}) {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command(envBin)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run env: %v", err)
	}
	if strings.Contains(string(out), "ambient-daemon-secret") {
		t.Fatalf("untrusted child saw the ambient %s", proofenv.EnvAnthropicAPIKey)
	}
	if !slices.Contains(strings.Split(string(out), "\n"), proofenv.EnvAnthropicAPIKey+"=") {
		t.Fatalf("untrusted child env lacks an empty %s", proofenv.EnvAnthropicAPIKey)
	}
}

func TestOverlayWithRepoPinsUploadSources_Sessions_AC98(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, source := range []string{"resolver", "dotenv", "ambient", "all"} {
			t.Run(fmt.Sprintf("trusted=%v/source=%s", trusted, source), func(t *testing.T) {
				dir := t.TempDir()
				base := map[string]string{}
				var dotenv strings.Builder
				for _, key := range proofenv.UploadKeys() {
					if source == "ambient" || source == "all" {
						t.Setenv(key, "ambient-upload-value")
					}
					if source == "resolver" || source == "all" {
						base[key] = "resolver-upload-value"
					}
					if source == "dotenv" || source == "all" {
						fmt.Fprintf(&dotenv, "%s=dotenv-upload-value\n", key)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(dotenv.String()), 0600); err != nil {
					t.Fatal(err)
				}
				got := OverlayWithRepo(base, dir, &models.Repo{IsTrusted: trusted})
				for _, key := range proofenv.UploadKeys() {
					if value, ok := got[key]; !ok || value != "" {
						t.Errorf("%s not pinned", key)
					}
				}
				t.Logf("upload environment pins empty: %s", strings.Join(proofenv.UploadKeys(), ", "))
			})
		}
	}
}
