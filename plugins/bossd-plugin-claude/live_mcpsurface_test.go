package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestLiveDescribeMCPSurface is BOS-867's live-validation harness for claude.
// Every other DescribeMCPSurface test here feeds the decoder a recorded
// fixture; this one spawns a REAL claude and reads the init event it actually
// emits, which is the only way to catch the payload shape drifting under a new
// Claude Code release.
//
// It runs in the repo checkout itself (CLAUDE_LIVE_WORK_DIR overrides), because
// that worktree declares MCP servers in its own .mcp.json — a bare temp dir
// declares none and would make every assertion vacuous.
//
// Opt-in (CLAUDE_LIVE=1) and self-skipping without the binary, so CI stays
// green. A skipped test is not evidence; run it locally and paste the
// transcript:
//
//	CLAUDE_LIVE=1 rtk proxy go test ./plugins/bossd-plugin-claude -run TestLiveDescribeMCPSurface -v
func TestLiveDescribeMCPSurface(t *testing.T) {
	if os.Getenv("CLAUDE_LIVE") != "1" {
		t.Skip("CLAUDE_LIVE!=1 — skipping live claude MCP-surface validation (opt-in)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH — skipping live validation")
	}
	workDir := strings.TrimSpace(os.Getenv("CLAUDE_LIVE_WORK_DIR"))
	if workDir == "" {
		workDir = liveRepoRoot(t)
	}

	srv := newServer(nil, zerolog.Nop())
	// Force the REAL prober. newServer already leaves initProber nil (which
	// means live), but stating it here means a future default change cannot
	// silently turn this harness back into a fixture-driven test.
	srv.initProber = liveClaudeInitProber{loginShell: srv.runner.loginShell}

	resp, err := srv.DescribeMCPSurface(context.Background(), &bossanovav1.DescribeMCPSurfaceRequest{
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("DescribeMCPSurface returned a gRPC error, which it never should: %v", err)
	}
	if resp.GetProbeError() != "" {
		t.Fatalf("live probe failed: %s", resp.GetProbeError())
	}
	if len(resp.GetServers()) == 0 {
		t.Fatalf("the checkout declares MCP servers but the live probe reported none — either the payload shape drifted or the probe read the wrong work dir (%s)", workDir)
	}

	withTools := 0
	for _, server := range resp.GetServers() {
		if want := "mcp__" + server.GetName() + "__"; server.GetToolNamePrefix() != want {
			t.Errorf("tool_name_prefix = %q, want %q", server.GetToolNamePrefix(), want)
		}
		if !server.GetIsDeclared() {
			t.Errorf("server %q was listed by claude but reports is_declared=false", server.GetName())
		}
		for _, tool := range server.GetToolNames() {
			if !strings.HasPrefix(tool, server.GetToolNamePrefix()) {
				t.Errorf("server %q reported tool %q outside its own prefix", server.GetName(), tool)
			}
		}
		if server.GetToolCount() > 0 {
			withTools++
		}
		t.Logf("live claude: name=%s declared=%v tools=%d prefix=%s auth=%s source=%v detail=%s",
			server.GetName(), server.GetIsDeclared(), server.GetToolCount(),
			server.GetToolNamePrefix(), server.GetAuthStatus(), server.GetSource(), server.GetSourceDetail())
	}
	if withTools == 0 {
		// Every server still connecting reports "pending" with zero tools, so a
		// run where none had connected yet proves nothing about the tool
		// filtering. Fail loudly rather than pass on an empty observation.
		t.Fatal("no live server reported a non-zero tool count — re-run; every server was still pending at init")
	}
	t.Logf("live claude source label: %s", resp.GetSourceLabel())
}

// liveRepoRoot walks up from the package dir to the checkout that declares
// .mcp.json. The ROOT is the right work dir, not the package dir: claude
// resolves a project's .mcp.json relative to its cwd, and the daemon always
// passes the worktree root — a probe run from a subdirectory reports the same
// servers (they come from the user scope) but attributes every one of them
// UNKNOWN, which is a materially weaker answer and not the production shape.
func liveRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve cwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, ".mcp.json")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no .mcp.json found above the package dir — set CLAUDE_LIVE_WORK_DIR to a checkout that declares MCP servers")
		}
		dir = parent
	}
}
