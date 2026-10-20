package plugin_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// claudeHarness holds the dispensed AgentRunner interface for claude plugin
// integration tests. A shared setup helper constructs it so each test body
// stays focused on the behaviour it is verifying.
type claudeHarness struct {
	AgentRunner pluginpkg.AgentRunner
}

// newClaudeHarness builds the bossd-plugin-claude binary, spawns it via the
// go-plugin broker, and returns a ready-to-use AgentRunner. All resources are
// registered on t.Cleanup so callers need only call newClaudeHarness.
func newClaudeHarness(t *testing.T) *claudeHarness {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping claude plugin integration test in short mode")
	}

	binPath := pluginharness.BuildPlugin(t, "bossd-plugin-claude")

	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeAgentRunner: pluginpkg.NewAgentRunnerGRPCPlugin(hostService),
	}

	client := pluginharness.SpawnPlugin(t, binPath, pluginMap)

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}

	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeAgentRunner)
	if err != nil {
		t.Fatalf("dispense AgentRunner: %v", err)
	}

	generated, ok := raw.(bossanovav1.AgentRunnerServiceClient)
	agent := pluginpkg.NewAgentRunnerClient(generated)
	if !ok {
		t.Fatalf("dispensed type %T does not implement AgentRunner", raw)
	}

	return &claudeHarness{AgentRunner: agent}
}

// TestE2E_Claude_GetInfo verifies the harness stands up end-to-end: the plugin
// builds, spawns, completes the go-plugin handshake, and reports Name="claude"
// via GetInfo. This is the minimal smoke test for the gRPC plumbing between
// bossd's plugin host and the bossd-plugin-claude binary.
func TestE2E_Claude_GetInfo(t *testing.T) {
	h := newClaudeHarness(t)

	info, err := h.AgentRunner.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "claude" {
		t.Errorf("plugin name = %q, want %q", info.GetName(), "claude")
	}
	if info.GetVersion() == "" {
		t.Error("plugin version should not be empty")
	}
}

// TestE2E_Claude_ListIgnoredDirtyFiles verifies that the claude plugin reports
// .claude/settings.local.json as an ignored dirty file. This is a pure plugin
// RPC that does not require the claude CLI to be installed.
func TestE2E_Claude_ListIgnoredDirtyFiles(t *testing.T) {
	h := newClaudeHarness(t)

	resp, err := h.AgentRunner.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}

	found := false
	for _, p := range resp.GetPaths() {
		if p == ".claude/settings.local.json" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Paths missing .claude/settings.local.json: got %v", resp.GetPaths())
	}
}

// TestE2E_Claude_StartAgentRun_RequiresCLI verifies that StartAgentRun returns an error
// (rather than panicking or hanging) when the claude CLI is not installed. When
// the CLI is present in PATH the test is skipped — the StartAgentRun → NDJSON path
// is already exercised by the plugin's own unit tests in
// plugins/bossd-plugin-claude/runner_test.go where the command factory can be
// faked.
func TestE2E_Claude_StartAgentRun_RequiresCLI(t *testing.T) {
	if _, err := exec.LookPath("claude"); err == nil {
		t.Skip("claude CLI is installed; StartAgentRun unit-test coverage sufficient — skipping RPC error path")
	}

	h := newClaudeHarness(t)

	_, err := h.AgentRunner.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		SessionId: "test-session-id",
		WorkDir:   t.TempDir(),
		Plan:      "hello",
	})
	// StartAgentRun should return an error because the claude binary does not exist.
	// We accept any non-nil error — the exact message is an implementation
	// detail. What we're asserting is that the gRPC plumbing round-trips the
	// error back to the host rather than hanging or panicking.
	if err == nil {
		t.Error("StartAgentRun succeeded without claude installed; expected an error")
	}
}

// Exercise the renamed RPC without depending on an installed or authenticated CLI.
func TestE2E_Claude_StartAgentRun(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newClaudeHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := h.AgentRunner.StartAgentRun(ctx, &bossanovav1.StartAgentRunRequest{
		SessionId:                "renamed-rpc",
		WorkDir:                  t.TempDir(),
		Plan:                     "hello",
		LogPath:                  filepath.Join(t.TempDir(), "run.log"),
		ShouldTrustProjectConfig: true,
	})
	if err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	if response.GetSessionId() != "renamed-rpc" {
		t.Fatalf("session id = %q, want renamed-rpc", response.GetSessionId())
	}
}
