package main

import (
	"context"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestLiveDescribeMCPSurface is BOS-867's live-validation harness for codex.
// Every other DescribeMCPSurface test in this package drives a fake registry,
// and a fake agrees with whatever the fake was told — so none of them can
// establish the thing the ticket is about: that a REAL `codex app-server`,
// launched in the chat's own work dir under the chat's own environment, reports
// the servers that session actually resolved.
//
// The three scenes are deliberately run TOGETHER, because the ticket's own
// acceptance criterion is a COMPARISON, not a single observation:
//
//   - Scene 1: a credentialed connector declared only in a repo-level
//     .codex/config.toml ⇒ is_declared=true, a non-zero tool_count, a
//     reconstructed mcp__<key>__ prefix, and source=REPO_FILE naming the file.
//   - Scene 2: the SAME declaration with its bearer-token env var unset ⇒
//     is_declared=true with tool_count=0. This is the declared-but-empty state.
//   - Scene 3: a work dir that declares nothing ⇒ the server is ABSENT from
//     servers entirely.
//
// Scene 2 next to Scene 3 is the artifact: "declared but broken" and "never
// declared" are indistinguishable from a tool list alone, and separating them
// is the signal BOS-804 consumes.
//
// Opt-in (CODEX_LIVE=1) and self-skipping without the binary or credential, so
// CI — which has neither — stays green. A skipped test is not evidence; run it
// locally and paste the transcript:
//
//	CODEX_LIVE=1 rtk proxy go test ./plugins/bossd-plugin-codex -run TestLiveDescribeMCPSurface -v
func TestLiveDescribeMCPSurface(t *testing.T) {
	requireLiveCodex(t)

	probe := func(t *testing.T, workDir string, env map[string]string) *bossanovav1.DescribeMCPSurfaceResponse {
		t.Helper()
		resp, err := liveServer(t).DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
			WorkDir:  workDir,
			ProbeEnv: env,
		})
		if err != nil {
			t.Fatalf("DescribeMCPSurface returned a gRPC error, which it never should: %v", err)
		}
		if resp.GetProbeError() != "" {
			t.Fatalf("live probe failed: %s", resp.GetProbeError())
		}
		return resp
	}

	find := func(resp *bossanovav1.DescribeMCPSurfaceResponse, name string) *bossanovav1.MCPServerReport {
		for _, report := range resp.GetServers() {
			if report.GetName() == name {
				return report
			}
		}
		return nil
	}

	// The key the live fixture declares. codex builds its callable tool names
	// from this key, so the reconstructed prefix must echo it verbatim.
	const key = "bossanova_linear"

	t.Run("credentialed connector reports tools and a repo-file source", func(t *testing.T) {
		apiKey := requireLiveLinearKey(t)
		workDir := liveRepoWithCodexConfig(t)
		home := liveCodexHome(t, workDir)

		resp := probe(t, workDir, map[string]string{"CODEX_HOME": home, "LINEAR_API_KEY": apiKey})
		report := find(resp, key)
		if report == nil {
			t.Fatalf("%q absent from the live surface: %+v", key, resp.GetServers())
		}
		if !report.GetIsDeclared() {
			t.Fatal("a server codex listed must report is_declared=true")
		}
		if report.GetToolCount() == 0 {
			t.Fatalf("a credentialed connector must expose tools; got %+v", report)
		}
		if want := "mcp__" + key + "__"; report.GetToolNamePrefix() != want {
			t.Fatalf("tool_name_prefix = %q, want %q rebuilt from the config key", report.GetToolNamePrefix(), want)
		}
		if report.GetSource() != bossanovav1.MCPServerSource_MCP_SERVER_SOURCE_REPO_FILE {
			t.Fatalf("source = %v, want REPO_FILE — it is declared only in the repo file", report.GetSource())
		}
		t.Logf("live credentialed: name=%s declared=%v tools=%d prefix=%s auth=%s source=%v detail=%s",
			report.GetName(), report.GetIsDeclared(), report.GetToolCount(),
			report.GetToolNamePrefix(), report.GetAuthStatus(), report.GetSource(), report.GetSourceDetail())
	})

	t.Run("credential-less connector is declared with zero tools", func(t *testing.T) {
		workDir := liveRepoWithCodexConfig(t)
		home := liveCodexHome(t, workDir)

		// Present-but-empty is exactly what dotenv.OverlayWithRepo guarantees
		// when the key is configured nowhere — the measured shape of a chat
		// launched without the credential.
		resp := probe(t, workDir, map[string]string{"CODEX_HOME": home, "LINEAR_API_KEY": ""})
		report := find(resp, key)
		if report == nil {
			t.Fatalf("a declared-but-uncredentialed server must still be REPORTED, not dropped: %+v", resp.GetServers())
		}
		if !report.GetIsDeclared() || report.GetToolCount() != 0 {
			t.Fatalf("declared/tools = %v/%d, want true/0 — the declared-but-empty state",
				report.GetIsDeclared(), report.GetToolCount())
		}
		t.Logf("live credential-less: name=%s declared=%v tools=%d prefix=%s auth=%s source=%v detail=%s",
			report.GetName(), report.GetIsDeclared(), report.GetToolCount(),
			report.GetToolNamePrefix(), report.GetAuthStatus(), report.GetSource(), report.GetSourceDetail())
	})

	t.Run("undeclared connector is absent entirely", func(t *testing.T) {
		// No .codex/config.toml and a fresh CODEX_HOME, so nothing ambient can
		// leak in and make the absence vacuous. liveTempDir resolves symlinks so
		// the project stays TRUSTED — an untrusted project also reports zero
		// servers, which would make this scene pass for the wrong reason.
		workDir := liveTempDir(t)
		home := liveCodexHome(t, workDir)

		resp := probe(t, workDir, map[string]string{"CODEX_HOME": home})
		if report := find(resp, key); report != nil {
			t.Fatalf("an undeclared server must be ABSENT, not present-and-empty: %+v", report)
		}
		t.Logf("live undeclared: servers=%d (the connector is absent, not zero-tooled)", len(resp.GetServers()))
	})
}
