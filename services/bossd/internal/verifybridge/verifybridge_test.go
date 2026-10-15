package verifybridge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fixture is one bridge run against a stub `node`: the stub records its argv
// and working directory, then prints stdout, writes stderr and exits with code.
type fixture struct {
	bridge   *Bridge
	worktree string
	argvFile string
	cwdFile  string
	envFile  string
}

func newFixture(t *testing.T, stdout, stderr string, code int) *fixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub node is POSIX-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "skills", ScriptRelPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("// stub verify-gate.mjs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		worktree: filepath.Join(dir, "worktree"),
		argvFile: filepath.Join(dir, "argv"),
		cwdFile:  filepath.Join(dir, "cwd"),
		envFile:  filepath.Join(dir, "env"),
	}
	if err := os.Mkdir(f.worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(dir, "stdout")
	errFile := filepath.Join(dir, "stderr")
	if err := os.WriteFile(outFile, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errFile, []byte(stderr), 0o644); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(dir, "node")
	body := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + f.argvFile + "'\n" +
		"pwd -P > '" + f.cwdFile + "'\n" +
		"printf '%s' \"${BRIDGE_PROBE-unset}\" > '" + f.envFile + "'\n" +
		"cat '" + outFile + "'\n" +
		"cat '" + errFile + "' >&2\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(node, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	f.bridge = &Bridge{Node: node, Script: script, Timeout: 10 * time.Second}
	return f
}

func (f *fixture) argv(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.argvFile)
	if err != nil {
		t.Fatalf("stub node never ran: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (f *fixture) cwd(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.cwdFile)
	if err != nil {
		t.Fatalf("stub node never ran: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestBridgeEnvIsTheChildsWholeEnvironment(t *testing.T) {
	const out = `{"posted":true,"commentUrl":"https://x/1"}`
	t.Setenv("BRIDGE_PROBE", "inherited")

	t.Run("nil inherits this process's environment", func(t *testing.T) {
		f := newFixture(t, out, "", 0)
		if _, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Dir: f.worktree}, "x"); err != nil {
			t.Fatalf("SendBack: %v", err)
		}
		if got := readFile(t, f.envFile); got != "inherited" {
			t.Fatalf("BRIDGE_PROBE = %q, want the inherited value", got)
		}
	})

	t.Run("a set Env replaces it, the later of two entries winning", func(t *testing.T) {
		f := newFixture(t, out, "", 0)
		f.bridge.Env = []string{"PATH=" + os.Getenv("PATH"), "BRIDGE_PROBE=first", "BRIDGE_PROBE=repo"}
		if _, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Dir: f.worktree}, "x"); err != nil {
			t.Fatalf("SendBack: %v", err)
		}
		if got := readFile(t, f.envFile); got != "repo" {
			t.Fatalf("BRIDGE_PROBE = %q, want the last Env entry", got)
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("stub node never ran: %v", err)
	}
	return string(b)
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestApproveSuccessRunsTheVerbInTheWorktree(t *testing.T) {
	f := newFixture(t, `{"approved":true,"waives":"ledger-open","headSha":"`+strings.Repeat("a", 40)+`","trackerWrites":"ok","approvalUrl":"https://github.com/o/r/pull/7#issuecomment-9","writes":[]}`+"\n", "", 0)
	got, err := f.bridge.Approve(context.Background(), Target{PR: 7, Repo: "o/r", Dir: f.worktree})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !got.Approved || got.Waives != "ledger-open" || got.TrackerWrites != "ok" || got.HeadSHA != strings.Repeat("a", 40) || got.ApprovalURL != "https://github.com/o/r/pull/7#issuecomment-9" {
		t.Fatalf("result = %+v", got)
	}
	want := []string{f.bridge.Script, "approve", "--pr=7", "--repo=o/r", "--json"}
	if g := f.argv(t); strings.Join(g, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", g, want)
	}
	if g, w := f.cwd(t), resolved(t, f.worktree); g != w {
		t.Fatalf("cwd = %q, want the worktree %q", g, w)
	}
}

func TestApproveReportsAFailedLabelWriteAndALiftedSendBack(t *testing.T) {
	for name, stdout := range map[string]string{
		"failed label write": `{"approved":true,"waives":"ledger-open","headSha":"y","approvalUrl":"https://x","trackerWrites":"failed"}`,
		"lifted send-back":   `{"approved":true,"waives":"","lifted":"sent-back","headSha":"y","approvalUrl":"https://x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, stdout, "", 0)
			got, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree})
			if err != nil || !got.Approved || got.ApprovalURL != "https://x" {
				t.Fatalf("Approve = %+v, %v", got, err)
			}
		})
	}
}

func TestApproveRefusalIsAResultNotAnError(t *testing.T) {
	f := newFixture(t, `{"approved":false,"reason":"no-standing-park","headSha":"abc","writes":[]}`, "", 0)
	got, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree, DryRun: true})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Approved || got.Reason != "no-standing-park" {
		t.Fatalf("result = %+v", got)
	}
	want := []string{f.bridge.Script, "approve", "--pr=7", "--dry-run", "--json"}
	if g := f.argv(t); strings.Join(g, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", g, want)
	}
}

func TestSendBackSuccessPassesTheNote(t *testing.T) {
	f := newFixture(t, `{"posted":true,"commentUrl":"https://github.com/o/r/pull/7#issuecomment-1","headSha":"abc","writes":[]}`, "", 0)
	note := "line one\nline two"
	got, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Repo: "o/r", Dir: f.worktree}, note)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if !got.Posted || got.CommentURL != "https://github.com/o/r/pull/7#issuecomment-1" {
		t.Fatalf("result = %+v", got)
	}
	// argv is recorded one per line, so the two-line note reads back as two entries.
	want := []string{f.bridge.Script, "send-back", "--pr=7", "--repo=o/r", "--note=line one", "line two", "--json"}
	if g := f.argv(t); strings.Join(g, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", g, want)
	}
}

func TestBridgeFailureModesAreDistinctErrors(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		stderr string
		code   int
		want   error
	}{
		{"non-zero exit", "", "verify-gate: approve needs --pr <number>", 2, ErrNodeExit},
		{"non-zero exit with JSON on stdout", `{"approved":true,"waives":"x","headSha":"y","trackerWrites":"ok","approvalUrl":"https://x"}`, "boom", 1, ErrNodeExit},
		{"empty stdout", "", "", 0, ErrEmptyOutput},
		{"whitespace-only stdout", "  \n\n", "", 0, ErrEmptyOutput},
		{"bad JSON", "not json", "", 0, ErrBadJSON},
		{"trailing output", `{"approved":false,"reason":"x"} extra`, "", 0, ErrBadJSON},
		{"missing approved field", `{"reason":"x"}`, "", 0, ErrShape},
		{"approved without the head-bound approval record", `{"approved":true,"waives":"x","headSha":"y","trackerWrites":"ok"}`, "", 0, ErrShape},
		{"approved with neither waived codes nor a lifted send-back", `{"approved":true,"waives":"","headSha":"y","approvalUrl":"https://x"}`, "", 0, ErrShape},
		{"approved lifting something other than a send-back", `{"approved":true,"lifted":"other","headSha":"y","approvalUrl":"https://x"}`, "", 0, ErrShape},
		{"refusal without a reason", `{"approved":false}`, "", 0, ErrShape},
		{"JSON array", `[]`, "", 0, ErrBadJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.stdout, tc.stderr, tc.code)
			got, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got.Approved {
				t.Fatalf("a failed bridge reported approved: %+v", got)
			}
			if tc.stderr != "" && !strings.Contains(err.Error(), tc.stderr) {
				t.Fatalf("err = %v, want stderr %q surfaced", err, tc.stderr)
			}
		})
	}
}

func TestSendBackShapeIsChecked(t *testing.T) {
	for name, stdout := range map[string]string{
		"missing posted":          `{"commentUrl":"https://x"}`,
		"posted without a url":    `{"posted":true}`,
		"refusal without reason":  `{"posted":false}`,
		"empty stdout is failure": "",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, stdout, "", 0)
			got, err := f.bridge.SendBack(context.Background(), Target{PR: 7, Dir: f.worktree}, "n")
			if err == nil || got.Posted {
				t.Fatalf("SendBack = %+v, %v; want an error and no post", got, err)
			}
		})
	}
}

func TestTimeoutIsItsOwnError(t *testing.T) {
	f := newFixture(t, "", "", 0)
	if err := os.WriteFile(f.bridge.Node, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.bridge.Timeout = 200 * time.Millisecond
	_, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree})
	if !errors.Is(err, ErrNodeTimeout) {
		t.Fatalf("err = %v, want ErrNodeTimeout", err)
	}
}

func TestTargetIsValidatedBeforeAnyRun(t *testing.T) {
	f := newFixture(t, `{"approved":false,"reason":"x"}`, "", 0)
	for name, tc := range map[string]struct {
		target Target
		note   string
	}{
		"no PR":          {Target{Dir: f.worktree}, "n"},
		"no worktree":    {Target{PR: 7}, "n"},
		"missing dir":    {Target{PR: 7, Dir: filepath.Join(f.worktree, "gone")}, "n"},
		"empty note":     {Target{PR: 7, Dir: f.worktree}, "  "},
		"malformed repo": {Target{PR: 7, Dir: f.worktree, Repo: "o/r --json"}, "n"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.bridge.SendBack(context.Background(), tc.target, tc.note); err == nil {
				t.Fatal("want a validation error")
			}
			if _, err := os.Stat(f.argvFile); err == nil {
				t.Fatal("node ran for an invalid target")
			}
		})
	}
}

func TestNodeMissing(t *testing.T) {
	f := newFixture(t, "", "", 0)
	f.bridge.Node = ""
	t.Setenv("PATH", t.TempDir())
	_, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree})
	if !errors.Is(err, ErrNodeMissing) {
		t.Fatalf("err = %v, want ErrNodeMissing", err)
	}
}

func TestLocateSearchesTheSkillHomesInOrder(t *testing.T) {
	home := t.TempDir()
	custom := t.TempDir()
	place := func(root string) string {
		p := filepath.Join(root, ScriptRelPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("//"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	if _, err := Locate(getenv, home); !errors.Is(err, ErrToolboxMissing) {
		t.Fatalf("empty homes: err = %v, want ErrToolboxMissing", err)
	}
	codex := place(filepath.Join(home, ".codex", "skills"))
	if got, err := Locate(getenv, home); err != nil || got != codex {
		t.Fatalf("Locate = %q, %v; want the codex tree %q", got, err, codex)
	}
	claude := place(filepath.Join(home, ".claude", "skills"))
	if got, _ := Locate(getenv, home); got != claude {
		t.Fatalf("Locate = %q, want the claude tree first %q", got, claude)
	}
	env["BOSS_SKILLS_HOME"] = custom
	if got, _ := Locate(getenv, home); got != claude {
		t.Fatalf("an empty BOSS_SKILLS_HOME tree must fall through, got %q", got)
	}
	explicit := place(custom)
	if got, _ := Locate(getenv, home); got != explicit {
		t.Fatalf("Locate = %q, want BOSS_SKILLS_HOME first %q", got, explicit)
	}
}

// The script runs with cmd.Dir set to the PR worktree, so a relative skills
// home must come back absolute or node resolves it against the wrong directory.
func TestLocateReturnsAnAbsolutePathForARelativeSkillsHome(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "skills", ScriptRelPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("//"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	getenv := func(k string) string {
		if k == "BOSS_SKILLS_HOME" {
			return "./skills"
		}
		return ""
	}
	got, err := Locate(getenv, "")
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Locate = %q, want an absolute path", got)
	}
	want, _ := filepath.EvalSymlinks(p)
	if gotReal, _ := filepath.EvalSymlinks(got); gotReal != want {
		t.Fatalf("Locate = %q, want %q", gotReal, want)
	}
}

func TestMissingToolboxIsAnError(t *testing.T) {
	f := newFixture(t, `{"approved":true}`, "", 0)
	f.bridge.Script = ""
	t.Setenv("BOSS_SKILLS_HOME", "")
	t.Setenv("BOSS_VERIFY_TOOLBOX", "")
	t.Setenv("HOME", t.TempDir())
	_, err := f.bridge.Approve(context.Background(), Target{PR: 7, Dir: f.worktree})
	if !errors.Is(err, ErrToolboxMissing) {
		t.Fatalf("err = %v, want ErrToolboxMissing", err)
	}
	if _, statErr := os.Stat(f.argvFile); statErr == nil {
		t.Fatal("node ran without a toolbox")
	}
}

// TestRealNodeRunsAStubVerifyGate drives a real node over a stub verify-gate.mjs,
// proving the invocation shape end to end where node is installed.
func TestRealNodeRunsAStubVerifyGate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "verify-gate.mjs")
	stub := `const [verb, ...rest] = process.argv.slice(2)
const note = rest.find((a) => a.startsWith('--note='))?.slice('--note='.length)
process.stdout.write(JSON.stringify({ posted: true, commentUrl: 'https://x/' + verb + '#' + note, headSha: process.cwd() }) + '\n')
`
	if err := os.WriteFile(script, []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{Node: node, Script: script, Timeout: 30 * time.Second}
	got, err := b.SendBack(context.Background(), Target{PR: 7, Dir: dir}, "--fix it")
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.CommentURL != "https://x/send-back#--fix it" {
		t.Fatalf("a flag-like note must reach the verb verbatim, got %q", got.CommentURL)
	}
	if got.HeadSHA != resolved(t, dir) {
		t.Fatalf("result = %+v", got)
	}
}
