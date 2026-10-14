package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// mustSetTrust is setCodexProjectTrustLevel for a config the test expects to
// be editable.
func mustSetTrust(t *testing.T, config, path, level string) string {
	t.Helper()
	got, err := setCodexProjectTrustLevel(config, path, level)
	if err != nil {
		t.Fatalf("setCodexProjectTrustLevel: %v", err)
	}
	return got
}

// resolvedTempDir is t.TempDir with symlinks resolved, so the worktree has a
// single spelling (macOS hands out /var/... for /private/var/...).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// projectHeader is the [projects."<path>"] header the writer emits.
func projectHeader(path string) string {
	return `[projects."` + tomlBasicStringEscape(path) + `"]`
}

func TestSetCodexProjectTrustUpdatesExistingProjectSection(t *testing.T) {
	const config = `[projects."/tmp/repo"]
  trust_level = "untrusted"
notes = "kept"

[projects."/tmp/other"]
trust_level = "trusted"
`

	got := mustSetTrust(t, config, "/tmp/repo", codexTrustedProjectLevel)

	if strings.Count(got, `[projects."/tmp/repo"]`) != 1 {
		t.Fatalf("project section count = %d, want 1:\n%s", strings.Count(got, `[projects."/tmp/repo"]`), got)
	}
	if !strings.Contains(got, "  trust_level = \"trusted\"\nnotes = \"kept\"") {
		t.Fatalf("existing trust line was not updated in place:\n%s", got)
	}
	if strings.Contains(got, "untrusted") {
		t.Fatalf("stale trust level still present:\n%s", got)
	}
	if !strings.Contains(got, `[projects."/tmp/other"]`+"\ntrust_level = \"trusted\"") {
		t.Fatalf("neighbor project section was not preserved:\n%s", got)
	}
}

func TestSetCodexProjectTrustEscapesProjectPathAsTOMLBasicString(t *testing.T) {
	worktree := "C:\\Users\\Dave\\repo \"two\"\nline\t" + string(rune(0x01))

	got := mustSetTrust(t, "", worktree, codexTrustedProjectLevel)

	wantHeader := `[projects."C:\\Users\\Dave\\repo \"two\"\nline\t\u0001"]`
	if !strings.Contains(got, wantHeader+"\ntrust_level = \"trusted\"") {
		t.Fatalf("escaped project header missing:\n%s", got)
	}
	// The header the writer emits must read back as the same path, or a
	// second edit would append a duplicate table.
	if again := mustSetTrust(t, got, worktree, codexUntrustedProjectLevel); strings.Count(again, "trust_level") != 1 {
		t.Fatalf("re-edit did not find the escaped header it wrote:\n%s", again)
	}
}

// TestUntrustCodexWorktree pins the revoke half of BOS-1562: the worktree's
// trust_level becomes an explicit "untrusted" — rewritten in place when one
// exists, added when none does — while other keys and other projects stay
// exactly as they were.
func TestUntrustCodexWorktree(t *testing.T) {
	worktree := resolvedTempDir(t)
	header := projectHeader(worktree)
	untrustedTable := header + "\ntrust_level = \"untrusted\"\n"
	for _, tc := range []struct {
		name, config, want string
	}{
		{
			name:   "trusted entry made untrusted",
			config: header + "\ntrust_level = \"trusted\"\nnotes = \"kept\"\n\n[projects.\"/tmp/other\"]\ntrust_level = \"trusted\"\n",
			want:   header + "\ntrust_level = \"untrusted\"\nnotes = \"kept\"\n\n[projects.\"/tmp/other\"]\ntrust_level = \"trusted\"\n",
		},
		{
			name:   "spacing, literal quotes and a comment",
			config: "model = \"x\"\n" + header + "\n  trust_level='trusted' # boss\n",
			want:   "model = \"x\"\n" + header + "\n  trust_level=\"untrusted\" # boss\n",
		},
		{
			name:   "untrusted entry untouched",
			config: untrustedTable,
			want:   untrustedTable,
		},
		{
			name:   "section without trust_level gains one",
			config: header + "\nnotes = 1\n",
			want:   header + "\ntrust_level = \"untrusted\"\nnotes = 1\n",
		},
		{
			name:   "other projects kept, worktree added untrusted",
			config: "[projects.\"/tmp/other\"]\ntrust_level = \"trusted\"\n",
			want:   "[projects.\"/tmp/other\"]\ntrust_level = \"trusted\"\n\n" + untrustedTable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			configPath := filepath.Join(codexHome, "config.toml")
			if err := os.WriteFile(configPath, []byte(tc.config), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := untrustCodexWorktree(worktree, nil); err != nil {
				t.Fatalf("untrustCodexWorktree: %v", err)
			}
			got, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("config =\n%s\nwant\n%s", got, tc.want)
			}
			info, err := os.Stat(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Fatalf("config mode changed to %v", info.Mode().Perm())
			}
		})
	}
}

// TestUntrustCodexWorktreeOverridesMainCheckoutTrust pins the reason a revoke
// writes "untrusted" instead of deleting: Codex falls back from a linked
// worktree with no entry of its own to the trust entry of the repository's
// main checkout. A user who trusts their own checkout of a repo bossd marks
// untrusted must still get an explicit untrusted entry for the worktree, and
// the main checkout's own trust is theirs to keep.
func TestUntrustCodexWorktreeOverridesMainCheckoutTrust(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	worktree := resolvedTempDir(t)
	mainCheckout := projectHeader("/Users/me/code/repo") + "\ntrust_level = \"trusted\"\n"
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(mainCheckout), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := untrustCodexWorktree(worktree, nil); err != nil {
		t.Fatalf("untrustCodexWorktree: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), projectHeader(worktree)+"\ntrust_level = \"untrusted\"") {
		t.Fatalf("worktree has no explicit untrusted entry to override the main checkout's trust:\n%s", got)
	}
	if !strings.HasPrefix(string(got), mainCheckout) {
		t.Fatalf("main checkout's own trust entry changed:\n%s", got)
	}
}

// TestCodexTrustCoversEverySpellingOfTheWorktree pins that a decision lands
// under every path Codex may look the project up by: the path bossd passed and
// its symlink-resolved form, which Codex consults first. A revoke keyed only
// on the unresolved spelling leaves a trusted entry under the resolved one in
// force.
func TestCodexTrustCoversEverySpellingOfTheWorktree(t *testing.T) {
	real := resolvedTempDir(t)
	link := filepath.Join(resolvedTempDir(t), "wt-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	configPath := filepath.Join(codexHome, "config.toml")
	trusted := projectHeader(real) + "\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(configPath, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := untrustCodexWorktree(link, nil); err != nil {
		t.Fatalf("untrustCodexWorktree: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{real, link} {
		if !strings.Contains(string(got), projectHeader(path)+"\ntrust_level = \"untrusted\"") {
			t.Fatalf("revoke via %s left %s without an untrusted entry:\n%s", link, path, got)
		}
	}
	if strings.Contains(string(got), `"trusted"`) {
		t.Fatalf("a trusted entry survived the revoke:\n%s", got)
	}

	if err := trustCodexWorktree(link, nil); err != nil {
		t.Fatalf("trustCodexWorktree: %v", err)
	}
	if got, _ := os.ReadFile(configPath); strings.Contains(string(got), "untrusted") || strings.Count(string(got), `trust_level = "trusted"`) != 2 {
		t.Fatalf("grant via %s did not trust both spellings:\n%s", link, got)
	}
}

// TestSetCodexProjectTrustRecognisesEquivalentSpellings pins that the edit
// finds the worktree's table and trust_level key in every spelling TOML
// treats as the same key — literal and escaped keys, whitespace around the
// dots, a quoted key name, a dotted key — and rewrites that one value rather
// than appending a duplicate table (a TOML error that breaks the whole Codex
// config) or leaving a trusted entry in place.
func TestSetCodexProjectTrustRecognisesEquivalentSpellings(t *testing.T) {
	const path = "/tmp/a b/wt"
	for _, tc := range []struct{ name, config, want string }{
		{
			name:   "literal-quoted table key",
			config: "[projects.'/tmp/a b/wt']\ntrust_level = 'trusted'\n",
			want:   "[projects.'/tmp/a b/wt']\ntrust_level = \"untrusted\"\n",
		},
		{
			name:   "whitespace around the dots",
			config: "[ projects . \"/tmp/a b/wt\" ]\ntrust_level = \"trusted\"\n",
			want:   "[ projects . \"/tmp/a b/wt\" ]\ntrust_level = \"untrusted\"\n",
		},
		{
			name:   "escaped characters in a basic key",
			config: "[\"projects\".\"/tmp/a\\u0020b\\u002Fwt\"]\ntrust_level = \"trusted\"\n",
			want:   "[\"projects\".\"/tmp/a\\u0020b\\u002Fwt\"]\ntrust_level = \"untrusted\"\n",
		},
		{
			name:   "quoted trust_level key",
			config: "[projects.\"/tmp/a b/wt\"]\n\"trust_level\" = \"trusted\"\n",
			want:   "[projects.\"/tmp/a b/wt\"]\n\"trust_level\" = \"untrusted\"\n",
		},
		{
			name:   "literal trust_level key",
			config: "[projects.\"/tmp/a b/wt\"]\n'trust_level' = \"trusted\"\n",
			want:   "[projects.\"/tmp/a b/wt\"]\n'trust_level' = \"untrusted\"\n",
		},
		{
			name:   "dotted key at the root",
			config: "model = \"m\"\nprojects.\"/tmp/a b/wt\".trust_level = \"trusted\" # hand\n\n[mcp_servers.x]\ncommand = \"y\"\n",
			want:   "model = \"m\"\nprojects.\"/tmp/a b/wt\".trust_level = \"untrusted\" # hand\n\n[mcp_servers.x]\ncommand = \"y\"\n",
		},
		{
			name:   "dotted key under [projects]",
			config: "[projects]\n\"/tmp/a b/wt\" . trust_level = \"trusted\"\n",
			want:   "[projects]\n\"/tmp/a b/wt\" . trust_level = \"untrusted\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustSetTrust(t, tc.config, path, codexUntrustedProjectLevel)
			if got != tc.want {
				t.Fatalf("config =\n%s\nwant\n%s", got, tc.want)
			}
			if strings.Count(got, "trust_level") != 1 || strings.Contains(got, `"trusted"`) || strings.Contains(got, `'trusted'`) {
				t.Fatalf("edit did not rewrite the one existing entry in place:\n%s", got)
			}
		})
	}
}

// TestSetCodexProjectTrustSkipsLookalikesInValues pins that brackets and
// newlines inside strings and arrays are not read as structure: a multi-line
// string that contains the project's header text must not be edited, and a
// multi-line array must not hide the real header that follows it.
func TestSetCodexProjectTrustSkipsLookalikesInValues(t *testing.T) {
	const path = "/tmp/wt"
	config := "notes = \"\"\"\n[projects.\"/tmp/wt\"]\ntrust_level = \"trusted\"\n\"\"\"\n" +
		"args = [\n  \"]\", # a ] comment\n  'x',\n]\n" +
		"[projects.\"/tmp/wt\"]\ntrust_level = \"trusted\"\n"
	got := mustSetTrust(t, config, path, codexUntrustedProjectLevel)
	want := config[:strings.LastIndex(config, `"trusted"`)] + `"untrusted"` + "\n"
	if got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
}

// TestSetCodexProjectTrustFailsClosedOnUneditableShapes pins the refusal: a
// config whose project table cannot be edited without guessing is an error —
// never a duplicate table appended after it, never a trusted entry left in
// place — and the caller turns the error into a refused launch.
func TestSetCodexProjectTrustFailsClosedOnUneditableShapes(t *testing.T) {
	const path = "/tmp/wt"
	for _, tc := range []struct{ name, config string }{
		{"projects as an inline table", "projects = { \"/tmp/wt\" = { trust_level = \"trusted\" } }\n"},
		{"project as an inline table", "[projects]\n\"/tmp/wt\" = { trust_level = \"trusted\" }\n"},
		{"projects as an array of tables", "[[projects]]\nname = \"x\"\n"},
		{"project table defined by dotted keys", "projects.\"/tmp/wt\".notes = 1\n"},
		{"trust_level written as a table", "[projects.\"/tmp/wt\".trust_level]\nx = 1\n"},
		{"unterminated multi-line string", "notes = \"\"\"\nnever closed\n"},
		{"unterminated header", "[projects.\"/tmp/wt\"\ntrust_level = \"trusted\"\n"},
		{"junk after a value", "model = \"x\" y\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, level := range []string{codexUntrustedProjectLevel, codexTrustedProjectLevel} {
				if got, err := setCodexProjectTrustLevel(tc.config, path, level); err == nil {
					t.Fatalf("level %s: edit succeeded on an uneditable config:\n%s", level, got)
				}
			}
		})
	}
}

// TestUntrustCodexWorktreeFailsClosedAndLeavesConfig pins the end-to-end
// refusal: an uneditable config makes the revoke — and so the launch — fail,
// and the file is left byte-for-byte as it was.
func TestUntrustCodexWorktreeFailsClosedAndLeavesConfig(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	worktree := resolvedTempDir(t)
	config := `projects = { "` + tomlBasicStringEscape(worktree) + `" = { trust_level = "trusted" } }` + "\n"
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := untrustCodexWorktree(worktree, nil); err == nil {
		t.Fatal("untrustCodexWorktree succeeded on an uneditable config")
	}
	if got, _ := os.ReadFile(configPath); string(got) != config {
		t.Fatalf("config changed by a refused revoke:\n%s", got)
	}
	srv := &Server{}
	if _, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "agent-1",
		WorktreePath: worktree,
		ProjectTrust: bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
	}); err == nil {
		t.Fatal("BuildInteractiveCommand launched with a revoke it could not apply")
	}
}

// TestUntrustCodexWorktreeAbsentConfigIsNoop: with no Codex config there is
// nothing to revoke, and nothing is created.
func TestUntrustCodexWorktreeAbsentConfigIsNoop(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexHome)
	if err := untrustCodexWorktree(t.TempDir(), nil); err != nil {
		t.Fatalf("untrustCodexWorktree: %v", err)
	}
	if _, err := os.Stat(codexHome); !os.IsNotExist(err) {
		t.Fatalf("codex home created for a no-op revoke (stat err %v)", err)
	}
}

// TestCodexProjectTrustReadsCommentedTableHeaders pins that a TOML table
// header with a trailing comment is still a header, on both the revoke and the
// grant path: missing the target's header would keep (or duplicate) its
// trust, and missing the NEXT project's header would let the edit run on into
// that project's section.
func TestCodexProjectTrustReadsCommentedTableHeaders(t *testing.T) {
	const target = "/tmp/a#b"
	header := `[projects."/tmp/a#b"]`
	const other = `[projects."/tmp/other"] # added by hand`
	for _, tc := range []struct {
		name, config, want, level string
	}{
		{
			name:   "revoke: target header carries a comment",
			config: header + " # boss\ntrust_level = \"trusted\"\n",
			want:   header + " # boss\ntrust_level = \"untrusted\"\n",
			level:  codexUntrustedProjectLevel,
		},
		{
			name:   "revoke: next header carries a comment",
			config: header + "\nnotes = 1\n" + other + "\ntrust_level = \"trusted\"\n",
			want:   header + "\ntrust_level = \"untrusted\"\nnotes = 1\n" + other + "\ntrust_level = \"trusted\"\n",
			level:  codexUntrustedProjectLevel,
		},
		{
			name:   "trust: target header carries a comment",
			config: header + " # boss\ntrust_level = \"untrusted\"\n",
			want:   header + " # boss\ntrust_level = \"trusted\"\n",
			level:  codexTrustedProjectLevel,
		},
		{
			name:   "trust: next header carries a comment",
			config: header + "\nnotes = 1\n" + other + "\ntrust_level = \"untrusted\"\n",
			want:   header + "\ntrust_level = \"trusted\"\nnotes = 1\n" + other + "\ntrust_level = \"untrusted\"\n",
			level:  codexTrustedProjectLevel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mustSetTrust(t, tc.config, target, tc.level); got != tc.want {
				t.Fatalf("config =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestUntrustCodexWorktreeRevokesInAccountHome pins that a revoke reaches the
// Codex home the session actually runs with: an account-bound launch passes
// its account CODEX_HOME in config_home_env, and Codex reads trust from that
// config, so clearing only the plugin's own home would leave the account's
// trust entry in force. The plugin's own home is cleared too.
func TestUntrustCodexWorktreeRevokesInAccountHome(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	accountHome := t.TempDir()
	worktree := t.TempDir()
	trusted := `[projects."` + tomlBasicStringEscape(worktree) + `"]` + "\ntrust_level = \"trusted\"\n"
	for _, home := range []string{baseHome, accountHome} {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	srv := &Server{}
	if _, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "agent-1",
		WorktreePath:  worktree,
		ConfigHomeEnv: map[string]string{"CODEX_HOME": accountHome},
		ProjectTrust:  bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
	}); err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	for name, home := range map[string]string{"account": accountHome, "base": baseHome} {
		got, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), `trust_level = "trusted"`) {
			t.Fatalf("%s Codex home still trusts the worktree after a revoke:\n%s", name, got)
		}
	}
}

// TestUntrustCodexWorktreeAbsentAccountHomeIsNoop: an account home with no
// Codex config has nothing to revoke, and the revoke creates nothing there.
func TestUntrustCodexWorktreeAbsentAccountHomeIsNoop(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	accountHome := filepath.Join(t.TempDir(), "account-codex")
	if err := untrustCodexWorktree(t.TempDir(), map[string]string{"CODEX_HOME": accountHome}); err != nil {
		t.Fatalf("untrustCodexWorktree: %v", err)
	}
	if _, err := os.Stat(accountHome); !os.IsNotExist(err) {
		t.Fatalf("account codex home created for a no-op revoke (stat err %v)", err)
	}
}

// TestStartRunHonoursProjectTrustDecision pins the headless half of BOS-1562:
// `codex exec` has no trust prompt but still loads a worktree's
// .codex/config.toml when its Codex home trusts the worktree, so a headless
// run bossd marks untrusted (REVOKE) must clear a stale trusted entry from both
// the plugin's own home and the run's account home BEFORE codex starts. The
// fake codex exits 3 when it can still see the entry, so a revoke that ran
// after the spawn reds here too. GRANT leaves the trusted entry trusted (GRANT
// over an "untrusted" entry is TestHeadlessGrantRestoresTrustAfterRevoke's)
// and UNSPECIFIED changes nothing.
func TestStartRunHonoursProjectTrustDecision(t *testing.T) {
	for _, tc := range []struct {
		decision    bossanovav1.ProjectTrustDecision
		wantTrusted bool
	}{
		{bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED, true},
		{bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT, true},
		{bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE, false},
	} {
		t.Run(tc.decision.String(), func(t *testing.T) {
			isolateCaptureDir(t)
			baseHome := t.TempDir()
			t.Setenv("CODEX_HOME", baseHome)
			accountHome := t.TempDir()
			workDir := t.TempDir()
			trusted := `[projects."` + tomlBasicStringEscape(workDir) + `"]` + "\ntrust_level = \"trusted\"\n"
			for _, home := range []string{baseHome, accountHome} {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			r := NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t,
				`if grep -q 'trust_level = "trusted"' "$CODEX_HOME/config.toml"; then echo "stale codex trust visible at start"; exit 3; fi`)))
			srv := &Server{logger: zerolog.Nop(), runner: r}
			start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
				WorkDir:      workDir,
				SessionId:    "sid-trust",
				LogPath:      filepath.Join(t.TempDir(), "agent.log"),
				ExtraEnv:     map[string]string{"CODEX_HOME": accountHome},
				ProjectTrust: tc.decision,
			})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			exit := waitExit(t, srv, start.GetSessionId())

			for name, home := range map[string]string{"account": accountHome, "base": baseHome} {
				got, err := os.ReadFile(filepath.Join(home, "config.toml"))
				if err != nil {
					t.Fatal(err)
				}
				if has := strings.Contains(string(got), `trust_level = "trusted"`); has != tc.wantTrusted {
					t.Fatalf("%s Codex home trust entry present = %v, want %v:\n%s", name, has, tc.wantTrusted, got)
				}
			}
			if !tc.wantTrusted && exit.GetExitError() != "" {
				t.Fatalf("codex started while the revoked trust was still visible: %q", exit.GetExitError())
			}
		})
	}
}

// headlessTrustRPC drives one headless RPC that applies a project-trust
// decision to workDir, so a test can run the same scenario through each.
type headlessTrustRPC struct {
	name string
	call func(t *testing.T, srv *Server, workDir string, decision bossanovav1.ProjectTrustDecision, profile bossanovav1.HeadlessCapabilityProfile) error
}

var headlessTrustRPCs = []headlessTrustRPC{
	{name: "StartRun", call: func(t *testing.T, srv *Server, workDir string, decision bossanovav1.ProjectTrustDecision, profile bossanovav1.HeadlessCapabilityProfile) error {
		t.Helper()
		start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
			WorkDir:                   workDir,
			SessionId:                 "sid-" + decision.String(),
			LogPath:                   filepath.Join(t.TempDir(), "agent.log"),
			HeadlessCapabilityProfile: profile,
			ProjectTrust:              decision,
		})
		if err == nil {
			waitExit(t, srv, start.GetSessionId())
		}
		return err
	}},
	{name: "PreflightHeadlessRun", call: func(t *testing.T, srv *Server, workDir string, decision bossanovav1.ProjectTrustDecision, profile bossanovav1.HeadlessCapabilityProfile) error {
		t.Helper()
		_, err := srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
			WorkDir:                   workDir,
			HeadlessCapabilityProfile: profile,
			ProjectTrust:              decision,
		})
		return err
	}},
}

// TestHeadlessGrantRestoresTrustAfterRevoke pins that GRANT means the same on
// every path: once a REVOKE has written an explicit "untrusted" entry, a later
// headless GRANT (the repo was re-trusted) writes "trusted" back. A no-op GRANT
// would leave every later headless run of the re-trusted repo untrusted.
func TestHeadlessGrantRestoresTrustAfterRevoke(t *testing.T) {
	for _, rpc := range headlessTrustRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			isolateCaptureDir(t)
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := resolvedTempDir(t)
			configPath := filepath.Join(codexHome, "config.toml")
			if err := os.WriteFile(configPath, []byte(projectHeader(workDir)+"\ntrust_level = \"trusted\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop(), WithCommandFactory(fakeCodexShell(t, "exit 0")))}

			for _, step := range []struct {
				decision bossanovav1.ProjectTrustDecision
				want     string
			}{
				{bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE, codexUntrustedProjectLevel},
				{bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT, codexTrustedProjectLevel},
			} {
				if err := rpc.call(t, srv, workDir, step.decision, bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED); err != nil {
					t.Fatalf("%s %s: %v", rpc.name, step.decision, err)
				}
				got, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if want := projectHeader(workDir) + "\ntrust_level = \"" + step.want + "\"\n"; string(got) != want {
					t.Fatalf("after %s config =\n%s\nwant\n%s", step.decision, got, want)
				}
			}
		})
	}
}

// TestHeadlessRevokeFailureRefusesLaunch pins the fail-closed half of a
// headless REVOKE: when the revoke cannot be applied (an inline projects
// table cannot be edited safely), StartRun and PreflightHeadlessRun return an
// error and start nothing — no capability probe, no codex process — rather
// than run an untrusted repo while its stale trust is still in force. The
// config is left as it was.
func TestHeadlessRevokeFailureRefusesLaunch(t *testing.T) {
	for _, rpc := range headlessTrustRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			isolateCaptureDir(t)
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := resolvedTempDir(t)
			config := `projects = { "` + tomlBasicStringEscape(workDir) + `" = { trust_level = "trusted" } }` + "\n"
			configPath := filepath.Join(codexHome, "config.toml")
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			var spawned, probed atomic.Bool
			srv := &Server{logger: zerolog.Nop(), runner: NewRunner(zerolog.Nop(), WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				spawned.Store(true)
				return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
			}))}
			srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
				probed.Store(true)
				return runtimeOperationSurface{}, nil
			})

			err := rpc.call(t, srv, workDir, bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
				bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1)
			if err == nil {
				t.Fatalf("%s succeeded with a revoke it could not apply", rpc.name)
			}
			if got := status.Code(err); got != codes.Internal || !strings.Contains(err.Error(), "untrust codex worktree") {
				t.Fatalf("%s error = %v (code %s), want Internal naming the revoke", rpc.name, err, got)
			}
			if probed.Load() {
				t.Fatalf("%s ran the capability probe after its revoke failed", rpc.name)
			}
			if spawned.Load() {
				t.Fatalf("%s started codex after its revoke failed", rpc.name)
			}
			if got, _ := os.ReadFile(configPath); string(got) != config {
				t.Fatalf("config changed by a refused revoke:\n%s", got)
			}
		})
	}
}

// TestUntrustCodexWorktreeResolvesRelativeHomeAgainstWorktree pins that a
// revoke edits the Codex home the child will actually read. A relative
// CODEX_HOME (or HOME) reaching the plugin un-resolved — the resume, orphan
// resume and rotation env builders in bossd do not pre-resolve it — is
// resolved by the codex child against its cwd, the worktree; resolving it
// against the plugin's own cwd would leave the worktree's trust in force.
func TestUntrustCodexWorktreeResolvesRelativeHomeAgainstWorktree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		homePath string // relative to the worktree
	}{
		{name: "relative CODEX_HOME", env: map[string]string{"CODEX_HOME": "acct-codex"}, homePath: "acct-codex"},
		{name: "relative HOME", env: map[string]string{"CODEX_HOME": "", "HOME": "acct-home"}, homePath: filepath.Join("acct-home", ".codex")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			worktree := t.TempDir()
			// Run from an unrelated cwd so a resolution against the plugin's
			// cwd cannot accidentally land in the worktree.
			t.Chdir(t.TempDir())
			home := filepath.Join(worktree, tc.homePath)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			trusted := `[projects."` + tomlBasicStringEscape(worktree) + `"]` + "\ntrust_level = \"trusted\"\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := untrustCodexWorktree(worktree, tc.env); err != nil {
				t.Fatalf("untrustCodexWorktree: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), `trust_level = "trusted"`) {
				t.Fatalf("worktree-relative Codex home still trusts the worktree after a revoke:\n%s", got)
			}
		})
	}
}

// TestGrantAfterRevokeRestoresTrustInAccountHome pins that GRANT writes the
// same homes REVOKE clears. An account-bound chat runs with its own
// CODEX_HOME (config_home_env) and reads trust only from there; a grant that
// wrote only the plugin's home left such a chat untrusted after a
// revoke-then-grant (BOS-1562).
func TestGrantAfterRevokeRestoresTrustInAccountHome(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	accountHome := t.TempDir()
	worktree := t.TempDir()
	srv := &Server{}
	build := func(decision bossanovav1.ProjectTrustDecision) {
		t.Helper()
		if _, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			SessionId:     "agent-1",
			WorktreePath:  worktree,
			ConfigHomeEnv: map[string]string{"CODEX_HOME": accountHome},
			ProjectTrust:  decision,
		}); err != nil {
			t.Fatalf("BuildInteractiveCommand(%v): %v", decision, err)
		}
	}
	build(bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT)
	build(bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE)
	build(bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT)

	header := `[projects."` + tomlBasicStringEscape(worktree) + `"]` + "\ntrust_level = \"trusted\""
	for name, home := range map[string]string{"account": accountHome, "base": baseHome} {
		got, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil {
			t.Fatalf("%s Codex home: %v", name, err)
		}
		if !strings.Contains(string(got), header) {
			t.Fatalf("%s Codex home does not trust the worktree after revoke-then-grant:\n%s", name, got)
		}
	}
}

// TestSetCodexProjectTrustEditsARealisticCodexConfig runs the edit over a
// user config.toml Codex itself loads (checked with `codex mcp list`):
// comments, dates, multi-line strings and arrays, inline tables, profiles and
// project tables in more than one spelling. Every existing entry is rewritten
// in place, a header-lookalike inside a string is left alone, and nothing
// else in the file moves.
func TestSetCodexProjectTrustEditsARealisticCodexConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex_user_config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(raw)

	got := mustSetTrust(t, config, "/Users/me/code/other", codexTrustedProjectLevel)
	if want := strings.Replace(config, `"trust_level" = 'untrusted'`, `"trust_level" = "trusted"`, 1); got != want {
		t.Fatalf("in-place rewrite moved other bytes:\n%s", got)
	}
	got = mustSetTrust(t, config, "/Users/me/code/repo", codexUntrustedProjectLevel)
	if strings.Count(got, `[projects."/Users/me/code/repo"]`) != 1 || !strings.Contains(got, `[projects."/Users/me/code/repo"]`+"\ntrust_level = \"untrusted\"") {
		t.Fatalf("existing project not rewritten in place:\n%s", got)
	}
	got = mustSetTrust(t, config, "/fake", codexUntrustedProjectLevel)
	if want := config + "\n" + projectHeader("/fake") + "\ntrust_level = \"untrusted\"\n"; got != want {
		t.Fatalf("a header inside a multi-line string was read as a table:\n%s", got)
	}
}

// TestLiveRevokeUntrustsLinkedWorktree checks the trust decision against a
// real codex: a linked git worktree with no trust entry of its own inherits
// the trust of the repository's main checkout, so its .codex/config.toml (here
// declaring one MCP server) loads; a revoke — passed the worktree's
// unresolved path, as bossd may — must make codex stop loading it, and a
// grant must bring it back. `codex mcp list` run in the worktree is the
// signal: it lists the project's server only when codex trusts the project.
//
// Opt-in (CODEX_LIVE=1), like the other live tests in this package:
//
//	CODEX_LIVE=1 go test ./plugins/bossd-plugin-codex -run TestLiveRevokeUntrustsLinkedWorktree -v
func TestLiveRevokeUntrustsLinkedWorktree(t *testing.T) {
	requireLiveCodex(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir() // unresolved on macOS (/var/... -> /private/var/...)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	mainCheckout := filepath.Join(root, "main")
	worktree := filepath.Join(root, "wt")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(mainCheckout, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := "[mcp_servers.bos_live_probe]\ncommand = \"/usr/bin/true\"\n"
	if err := os.WriteFile(filepath.Join(mainCheckout, ".codex", "config.toml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	git(mainCheckout, "init", "-q")
	git(mainCheckout, "add", ".")
	git(mainCheckout, "commit", "-q", "-m", "init")
	git(mainCheckout, "worktree", "add", "-q", worktree)

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	// The user trusts their own checkout, keyed the way codex writes it.
	mainTrust := mustSetTrust(t, "", filepath.Join(resolvedRoot, "main"), codexTrustedProjectLevel)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(mainTrust), 0o600); err != nil {
		t.Fatal(err)
	}
	loadsProject := func() bool {
		t.Helper()
		cmd := exec.Command("codex", "mcp", "list")
		cmd.Dir = worktree
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("codex mcp list: %v\n%s", err, out)
		}
		return strings.Contains(string(out), "bos_live_probe")
	}

	if !loadsProject() {
		t.Fatal("precondition: codex does not load the worktree's project config through the main checkout's trust")
	}
	if err := untrustCodexWorktree(worktree, nil); err != nil {
		t.Fatalf("untrustCodexWorktree: %v", err)
	}
	if loadsProject() {
		config, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
		t.Fatalf("codex still loads the worktree's project config after a revoke:\n%s", config)
	}
	if err := trustCodexWorktree(worktree, nil); err != nil {
		t.Fatalf("trustCodexWorktree: %v", err)
	}
	if !loadsProject() {
		config, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
		t.Fatalf("codex does not load the worktree's project config after a grant:\n%s", config)
	}
}
