package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestLivePreflightTrackerPlanAttachment is the BOS-865 live-validation harness.
// Every other test in this package drives a fake runtimeOperationRegistry, so
// none of them can prove the thing the ticket is actually about: that a real
// `codex app-server`, launched in the gated run's working directory, loads the
// repo-level `.codex/config.toml` and reports the connector's real tools. A fake
// agrees with whatever the fake was told.
//
// It drives the REAL codexAppServerOperationRegistry through
// Server.PreflightHeadlessRun across three scenes:
//
//   - Scene 1 (AC1 + AC4): a credentialed connector declared ONLY in a
//     repo-level .codex/config.toml, with WorkDir pointing at it ⇒ the preflight
//     passes and reports the connector's real operations. This is the scene that
//     fails without the work-dir fix, because codex resolves the repo file
//     relative to its cwd.
//   - Scene 2 (AC3 + AC4): identical, but LINEAR_API_KEY is empty ⇒
//     FailedPrecondition carrying connector_declared_but_exposes_no_operations
//     rather than the seven-name missing list. This automates the hand-run A/B
//     the ticket recorded.
//   - Scene 3 (AC2): a work dir with NO .codex/config.toml and a fresh empty
//     CODEX_HOME ⇒ the genuinely-undeclared repo still fails closed with
//     connector_plugin_disabled_or_missing.
//
// Opt-in (CODEX_LIVE=1) and self-skipping when the binary or the credential is
// absent, so CI — which has neither — stays green. Run locally against a real
// codex:
//
//	CODEX_LIVE=1 go test ./plugins/bossd-plugin-codex -run TestLivePreflight -v
//
// Per this repo's "commands whose result lies" rules, run it through the
// shell's raw passthrough (`rtk proxy go test …`) so the `=== RUN` / `--- PASS`
// lines prove the scenes executed rather than skipped.
func TestLivePreflightTrackerPlanAttachment(t *testing.T) {
	requireLiveCodex(t)

	t.Run("credentialed repo-level connector passes", func(t *testing.T) {
		key := requireLiveLinearKey(t)
		workDir := liveRepoWithCodexConfig(t)
		home := liveCodexHome(t, workDir)

		resp, err := liveServer(t).PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
			WorkDir:                   workDir,
			ExtraEnv:                  map[string]string{"CODEX_HOME": home, "LINEAR_API_KEY": key},
			HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		})
		if err != nil {
			t.Fatalf("preflight must pass for a credentialed repo-level connector, got: %v", err)
		}
		// Provided carries the connector's REAL operation surface, which is the
		// half a fake registry can never establish.
		if len(resp.GetProvided()) == 0 {
			t.Fatalf("Provided is empty; the connector reported no operations. resp=%+v", resp)
		}
		for _, required := range []string{"get_issue", "save_issue", "prepare_attachment_upload", "create_attachment_from_upload", "get_attachment"} {
			if !containsFold(resp.GetProvided(), required) {
				t.Errorf("Provided %v missing the real connector tool %q", resp.GetProvided(), required)
			}
		}
		t.Logf("live connector operations: %v", resp.GetProvided())
	})

	t.Run("empty credential reads as a credential signature", func(t *testing.T) {
		workDir := liveRepoWithCodexConfig(t)
		home := liveCodexHome(t, workDir)

		_, err := liveServer(t).PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
			WorkDir: workDir,
			// Present-but-empty is exactly what dotenv.OverlayWithRepo
			// guarantees when the key is configured nowhere.
			ExtraEnv:                  map[string]string{"CODEX_HOME": home, "LINEAR_API_KEY": ""},
			HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
		}
		message := status.Convert(err).Message()
		if !strings.Contains(message, "connector_declared_but_exposes_no_operations") {
			t.Fatalf("an empty bearer credential must not read as a declaration problem. message: %s", message)
		}
		t.Logf("live empty-credential message: %s", message)
	})

	t.Run("undeclared connector still fails closed", func(t *testing.T) {
		// No .codex/config.toml at all, and a fresh CODEX_HOME so no ambient
		// user-level declaration can leak in and make this pass vacuously.
		//
		// liveTempDir, not t.TempDir: an untrusted project reports zero servers,
		// which is indistinguishable from the genuinely-undeclared repo this
		// scene is asserting. Resolving the path keeps the project trusted, so
		// the marker is earned by the empty config rather than by a trust miss.
		workDir := liveTempDir(t)
		home := liveCodexHome(t, workDir)

		_, err := liveServer(t).PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
			WorkDir:                   workDir,
			ExtraEnv:                  map[string]string{"CODEX_HOME": home},
			HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
		}
		message := status.Convert(err).Message()
		if !strings.Contains(message, "connector_plugin_disabled_or_missing") {
			t.Fatalf("a genuinely undeclared repo must keep its existing marker. message: %s", message)
		}
		t.Logf("live undeclared message: %s", message)
	})
}

func liveServer(t *testing.T) *Server {
	t.Helper()
	srv := newTestServer(t)
	// Force the REAL app-server registry: newServer already installs it, but
	// stating it here means a future default change cannot silently turn this
	// harness back into a fake-driven test.
	srv.operationRegistry = codexAppServerOperationRegistry{binary: "codex", loginShell: srv.runner.loginShell}
	return srv
}

// liveRepoWithCodexConfig builds a work dir whose Linear MCP server is declared
// ONLY at repo scope, mirroring this repo's own .codex/config.toml. Declaring it
// nowhere else is the point: if the preflight does not run in this directory, it
// cannot see the server at all.
//
// Escape hatch: if codex declines the synthetic temp project despite the trust
// entry liveCodexHome writes, set CODEX_LIVE_WORK_DIR to a real trusted checkout
// that ships a repo-level .codex/config.toml (this repo is one) and it is used
// verbatim instead.
func liveRepoWithCodexConfig(t *testing.T) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv("CODEX_LIVE_WORK_DIR")); override != "" {
		if _, err := os.Stat(filepath.Join(override, ".codex", "config.toml")); err != nil {
			t.Skipf("CODEX_LIVE_WORK_DIR=%q has no .codex/config.toml: %v", override, err)
		}
		// Resolved for the same reason liveTempDir resolves: the trust entry
		// must match the cwd codex canonicalises to.
		resolved, err := filepath.EvalSymlinks(override)
		if err != nil {
			t.Skipf("CODEX_LIVE_WORK_DIR=%q could not be resolved: %v", override, err)
		}
		return resolved
	}
	workDir := liveTempDir(t)
	configDir := filepath.Join(workDir, ".codex")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("create repo .codex dir: %v", err)
	}
	config := `[mcp_servers.bossanova_linear]
url = "https://mcp.linear.app/mcp"
bearer_token_env_var = "LINEAR_API_KEY"
required = false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write repo .codex/config.toml: %v", err)
	}
	return workDir
}

// liveTempDir is t.TempDir with symlinks resolved.
//
// On macOS t.TempDir hands back a /var/folders/... path, but /var is a symlink
// to /private/var, and codex canonicalises its cwd before matching the
// `[projects."<path>"]` trust entry liveCodexHome writes. The unresolved form
// therefore never matches: codex treats the project as untrusted, declines to
// load its repo-level .codex/config.toml, and reports zero MCP servers — which
// this package's diagnostic correctly, but confusingly, reports as
// connector_plugin_disabled_or_missing. That failure is an artifact of the
// harness, not of the code under test, so resolve the path once here and use
// the same resolved value for both the work dir and the trust entry.
func liveTempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir symlinks: %v", err)
	}
	return resolved
}

// liveCodexHome builds a throwaway CODEX_HOME that trusts workDir, so codex does
// not decline to load an untrusted project. It deliberately declares no MCP
// servers of its own — every server the preflight sees must have come from the
// repo-level file.
//
// workDir must already be symlink-resolved (liveTempDir, or a real checkout via
// CODEX_LIVE_WORK_DIR) or the trust entry will not match codex's own view of
// its cwd.
func liveCodexHome(t *testing.T, workDir string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(setCodexProjectTrust("", workDir)), 0o600); err != nil {
		t.Fatalf("write CODEX_HOME config.toml: %v", err)
	}
	return home
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

func requireLiveCodex(t *testing.T) {
	t.Helper()
	if os.Getenv("CODEX_LIVE") != "1" {
		t.Skip("CODEX_LIVE!=1 — skipping live codex preflight validation (opt-in)")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not on PATH — skipping live validation")
	}
}

// requireLiveLinearKey skips only the scene that genuinely needs a working
// credential, so the empty-credential and undeclared scenes still run on a host
// without one.
//
// If codex declines the temp project despite the trust entry written by
// liveCodexHome, point CODEX_LIVE_WORK_DIR at a real trusted checkout that ships
// a repo-level .codex/config.toml (this repo is one) and re-run.
func requireLiveLinearKey(t *testing.T) string {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("LINEAR_API_KEY"))
	if key == "" {
		t.Skip("LINEAR_API_KEY unset — skipping the credentialed live scene")
	}
	return key
}
