package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// wantDenyOverride is the per-process Codex config override a DENY launch in
// workDir must carry: every spelling of workDir Codex may look trust up under
// (the path as given and its symlink-resolved form) explicitly untrusted, as
// one inline `projects` table — Codex's `-c` splits a dotted key on every
// '.', so a path cannot be named in the key itself.
func wantDenyOverride(t *testing.T, workDir string) string {
	t.Helper()
	spellings := []string{workDir}
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil && resolved != workDir {
		spellings = append(spellings, resolved)
	}
	entries := make([]string, 0, len(spellings))
	for _, path := range spellings {
		entries = append(entries, `"`+tomlBasicStringEscape(path)+`"={trust_level="untrusted"}`)
	}
	return "projects={" + strings.Join(entries, ",") + "}"
}

// hasConfigOverride reports whether argv passes override as a `-c` value.
func hasConfigOverride(argv []string, override string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" && argv[i+1] == override {
			return true
		}
	}
	return false
}

// hasTrustOverride reports whether argv carries any per-process project
// override at all.
func hasTrustOverride(argv []string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" && strings.HasPrefix(argv[i+1], "projects=") {
			return true
		}
	}
	return false
}

// staleTrustHomes writes a "trusted" entry for workDir — a grant left from
// while the repo was trusted, or the user's own — into both the plugin's
// Codex home and a launch's account home, and returns a check that both files
// are still byte-identical.
func staleTrustHomes(t *testing.T, workDir string) (accountHome string, unchanged func(t *testing.T)) {
	t.Helper()
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	accountHome = t.TempDir()
	config := mustSetTrust(t, "# the user's config\n", workDir, codexTrustedProjectLevel)
	for _, home := range []string{baseHome, accountHome} {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return accountHome, func(t *testing.T) {
		t.Helper()
		for name, home := range map[string]string{"base": baseHome, "account": accountHome} {
			got, err := os.ReadFile(filepath.Join(home, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != config {
				t.Errorf("%s Codex config changed by a DENY launch:\n%s", name, got)
			}
		}
	}
}

// argvRecorder captures the argv of every codex process a test server starts.
type argvRecorder struct {
	mu    sync.Mutex
	argvs [][]string
}

func (r *argvRecorder) record(name string, args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argvs = append(r.argvs, append([]string{name}, args...))
}

func (r *argvRecorder) all(t *testing.T) [][]string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.argvs) == 0 {
		t.Fatal("no codex process was started")
	}
	return slices.Clone(r.argvs)
}

// recordingAppServer is a codex app-server registry whose process is a fake
// that answers the handshake with no MCP servers, recording its argv.
func recordingAppServer(rec *argvRecorder) codexAppServerOperationRegistry {
	return codexAppServerOperationRegistry{
		binary: "codex",
		commandFactory: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			rec.record(name, args)
			return exec.CommandContext(ctx, "/bin/sh", "-c", `
				read -r initialize
				printf '%s\n' '{"id":1,"result":{}}'
				read -r initialized
				read -r list
				printf '%s\n' '{"id":2,"result":{"data":[]}}'
			`)
		},
	}
}

// denyPath drives one RPC that starts codex in workDir with a decision and
// returns the argv of every codex process it started.
type denyPath struct {
	name string
	run  func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string
}

var denyPaths = []denyPath{
	{name: "BuildInteractiveCommand", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		srv := newTestServer(t)
		resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			SessionId:     "agent-1",
			WorktreePath:  workDir,
			InitialPrompt: "hello",
			ConfigHomeEnv: map[string]string{"CODEX_HOME": accountHome},
			ProjectTrust:  decision,
		})
		if err != nil {
			t.Fatalf("BuildInteractiveCommand: %v", err)
		}
		argv := resp.GetArgv()
		// The override is a codex option, so it must precede the `--` that
		// turns everything after it into the prompt.
		if sep := slices.Index(argv, "--"); sep >= 0 && hasTrustOverride(argv[sep:]) {
			t.Fatalf("trust override placed after `--`, where codex reads it as prompt text: %q", argv)
		}
		return [][]string{argv}
	}},
	{name: "BuildInteractiveCommand resume", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		srv := newTestServer(t)
		resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			SessionId:     "agent-1",
			Resume:        true,
			WorktreePath:  workDir,
			ConfigHomeEnv: map[string]string{"CODEX_HOME": accountHome},
			ProjectTrust:  decision,
		})
		if err != nil {
			t.Fatalf("BuildInteractiveCommand: %v", err)
		}
		return [][]string{resp.GetArgv()}
	}},
	{name: "StartRun", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		isolateCaptureDir(t)
		rec := &argvRecorder{}
		r := NewRunner(zerolog.Nop(), WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			rec.record(name, args)
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
		}))
		srv := &Server{logger: zerolog.Nop(), runner: r}
		start, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
			WorkDir:      workDir,
			SessionId:    "sid-deny",
			LogPath:      filepath.Join(t.TempDir(), "agent.log"),
			ExtraEnv:     map[string]string{"CODEX_HOME": accountHome},
			ProjectTrust: decision,
		})
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		waitExit(t, srv, start.GetSessionId())
		return rec.all(t)
	}},
	{name: "StartRun resume with preflight", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		isolateCaptureDir(t)
		rec := &argvRecorder{}
		r := NewRunner(zerolog.Nop(), WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
			rec.record(name, args)
			return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
		}))
		srv := &Server{logger: zerolog.Nop(), runner: r, operationRegistry: recordingAppServer(rec)}
		resume := "019e068c-9ade-7cf0-be8c-3b21977576d0"
		// The fake app-server reports no Linear server, so the profile's
		// preflight refuses the run; what matters is the probe's argv.
		_, _ = srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
			WorkDir:                   workDir,
			ResumeId:                  &resume,
			SessionId:                 "sid-deny",
			LogPath:                   filepath.Join(t.TempDir(), "agent.log"),
			ExtraEnv:                  map[string]string{"CODEX_HOME": accountHome},
			HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
			ProjectTrust:              decision,
		})
		return rec.all(t)
	}},
	{name: "PreflightHeadlessRun", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		rec := &argvRecorder{}
		srv := newTestServer(t)
		srv.operationRegistry = recordingAppServer(rec)
		req := fullProfileRequest(accountHome)
		req.WorkDir = workDir
		req.ProjectTrust = decision
		// As above: the refusal is expected; the probe's argv is the subject.
		_, _ = srv.PreflightHeadlessRun(context.Background(), req)
		return rec.all(t)
	}},
	{name: "DescribeMCPSurface", run: func(t *testing.T, workDir, accountHome string, decision bossanovav1.ProjectTrustDecision) [][]string {
		t.Helper()
		rec := &argvRecorder{}
		srv := newTestServer(t)
		srv.operationRegistry = recordingAppServer(rec)
		resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir:      workDir,
			ProbeEnv:     map[string]string{"CODEX_HOME": accountHome},
			ProjectTrust: decision,
		})
		if err != nil || resp.GetProbeError() != "" {
			t.Fatalf("DescribeMCPSurface: err %v, probe_error %q", err, resp.GetProbeError())
		}
		return rec.all(t)
	}},
}

// TestDenyRunsEveryCodexPathUntrustedWithoutPersisting pins the DENY decision
// (BOS-1562): bossd sends it for an untrusted repo's launch in the repo's own
// checkout, whose persisted Codex trust belongs to the user (and decides the
// trust of their linked worktrees). Every path that starts codex for the
// request must run it untrusted — overriding a stale "trusted" entry, bossd's
// own from while the repo was trusted or one written before BOS-1562 — through
// a per-process `-c` override, and must leave every Codex config it could have
// edited byte-identical.
func TestDenyRunsEveryCodexPathUntrustedWithoutPersisting(t *testing.T) {
	for _, path := range denyPaths {
		t.Run(path.name, func(t *testing.T) {
			// Unresolved on macOS (/var -> /private/var), so both spellings
			// must be overridden.
			workDir := t.TempDir()
			accountHome, unchanged := staleTrustHomes(t, workDir)
			want := wantDenyOverride(t, workDir)

			for _, argv := range path.run(t, workDir, accountHome, bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY) {
				if !hasConfigOverride(argv, want) {
					t.Errorf("codex started without the untrusted override %q: %q", want, argv)
				}
			}
			unchanged(t)
		})
	}
}

// TestRevokeAlsoRunsEveryCodexPathUntrustedPerProcess pins that REVOKE does
// not rely on its persisted edits alone (BOS-1562). Codex starts through the
// user's interactive login shell and tmux server, so a CODEX_HOME exported
// there can point it at a home REVOKE never edited, where the main checkout's
// trust would carry over to the worktree. Every path that starts codex for a
// REVOKE must therefore carry the same per-process untrusted override DENY
// uses, on top of the persisted "untrusted" entry.
func TestRevokeAlsoRunsEveryCodexPathUntrustedPerProcess(t *testing.T) {
	for _, path := range denyPaths {
		t.Run(path.name, func(t *testing.T) {
			workDir := t.TempDir()
			accountHome, _ := staleTrustHomes(t, workDir)
			want := wantDenyOverride(t, workDir)

			for _, argv := range path.run(t, workDir, accountHome, bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE) {
				if !hasConfigOverride(argv, want) {
					t.Errorf("REVOKE started codex without the untrusted override %q: %q", want, argv)
				}
			}
			config, err := os.ReadFile(filepath.Join(accountHome, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), `trust_level = "untrusted"`) {
				t.Errorf("REVOKE did not also persist an untrusted entry in the launch's home:\n%s", config)
			}
		})
	}
}

// TestOnlyDenyAndRevokeAddATrustOverride pins the other direction: GRANT
// persists its decision and UNSPECIFIED makes none, so neither may add a
// per-process override — one on a GRANT would run a trusted repo untrusted.
func TestOnlyDenyAndRevokeAddATrustOverride(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
	} {
		for _, path := range denyPaths {
			t.Run(decision.String()+"/"+path.name, func(t *testing.T) {
				workDir := t.TempDir()
				accountHome, _ := staleTrustHomes(t, workDir)
				for _, argv := range path.run(t, workDir, accountHome, decision) {
					if hasTrustOverride(argv) {
						t.Errorf("%s added a per-process trust override: %q", decision, argv)
					}
				}
			})
		}
	}
}

// TestUnknownTrustDecisionRefusesLaunch pins that a decision value this
// plugin does not recognise fails closed: treating it as UNSPECIFIED would
// leave a stale "trusted" entry in force for a launch bossd may have meant to
// run untrusted. The RPC errors, and no Codex config is edited.
func TestUnknownTrustDecisionRefusesLaunch(t *testing.T) {
	workDir := t.TempDir()
	accountHome, unchanged := staleTrustHomes(t, workDir)
	unknown := bossanovav1.ProjectTrustDecision(99)

	if override, err := applyCodexProjectTrust(unknown, workDir, map[string]string{"CODEX_HOME": accountHome}); err == nil {
		t.Fatalf("applyCodexProjectTrust accepted unknown decision %d (override %q)", unknown, override)
	}
	_, err := newTestServer(t).BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:     "agent-1",
		WorktreePath:  workDir,
		ConfigHomeEnv: map[string]string{"CODEX_HOME": accountHome},
		ProjectTrust:  unknown,
	})
	if err == nil {
		t.Fatal("BuildInteractiveCommand built a launch for an unknown trust decision")
	}
	unchanged(t)
}

// TestLiveDenyOverridesStaleCheckoutTrust proves DENY against a real codex
// (opt-in, CODEX_LIVE=1): the user's checkout carries a repo-level MCP server
// and a "trusted" Codex entry, and the user has a linked worktree of it that
// inherits that trust. A DENY probe of the checkout must not load the repo's
// server, the CLI path with the plugin's override must not either, and
// afterwards the Codex config is byte-identical and the user's linked
// worktree still loads it — nothing was persisted.
//
//	CODEX_LIVE=1 rtk proxy go test ./plugins/bossd-plugin-codex -run TestLiveDenyOverridesStaleCheckoutTrust -v
func TestLiveDenyOverridesStaleCheckoutTrust(t *testing.T) {
	requireLiveCodex(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir() // unresolved on macOS (/var/... -> /private/var/...)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "main")
	linked := filepath.Join(root, "wt")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(checkout, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	const server = "bos_live_probe"
	project := "[mcp_servers." + server + "]\ncommand = \"/usr/bin/true\"\n"
	if err := os.WriteFile(filepath.Join(checkout, ".codex", "config.toml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	git(checkout, "init", "-q")
	git(checkout, "add", ".")
	git(checkout, "commit", "-q", "-m", "init")
	git(checkout, "worktree", "add", "-q", linked)

	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	// The stale grant, keyed the way codex writes it.
	config := mustSetTrust(t, "", filepath.Join(resolvedRoot, "main"), codexTrustedProjectLevel)
	configPath := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cliLoads := func(dir string, args ...string) bool {
		t.Helper()
		cmd := exec.Command("codex", append(args, "mcp", "list")...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("codex mcp list: %v\n%s", err, out)
		}
		return strings.Contains(string(out), server)
	}
	probeLoads := func(decision bossanovav1.ProjectTrustDecision) bool {
		t.Helper()
		resp, err := liveServer(t).DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir:      checkout,
			ProbeEnv:     map[string]string{"CODEX_HOME": codexHome},
			ProjectTrust: decision,
		})
		if err != nil || resp.GetProbeError() != "" {
			t.Fatalf("DescribeMCPSurface: err %v, probe_error %q", err, resp.GetProbeError())
		}
		for _, report := range resp.GetServers() {
			if report.GetName() == server {
				return true
			}
		}
		return false
	}

	if !cliLoads(checkout) || !cliLoads(linked) || !probeLoads(bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED) {
		t.Fatal("precondition: the stale trusted entry does not make codex load the checkout's project config")
	}
	if probeLoads(bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY) {
		t.Fatal("a DENY app-server probe of the checkout loaded the untrusted repo's MCP server")
	}
	argv := denyPaths[0].run(t, checkout, codexHome, bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY)[0]
	var override string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" && strings.HasPrefix(argv[i+1], "projects=") {
			override = argv[i+1]
		}
	}
	if override == "" {
		t.Fatalf("DENY interactive argv carries no trust override: %q", argv)
	}
	if cliLoads(checkout, "-c", override) {
		t.Fatalf("codex with the DENY override %q still loads the checkout's MCP server", override)
	}
	if got, err := os.ReadFile(configPath); err != nil || string(got) != config {
		t.Fatalf("DENY changed the Codex config (err %v):\n%s", err, got)
	}
	if !cliLoads(linked) {
		t.Fatal("the user's linked worktree lost its inherited trust after DENY launches")
	}
}
