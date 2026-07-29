package plugin_test

import (
	"context"
	"os/exec"
	"slices"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/agent"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// opencodeHarness holds the dispensed AgentRunner interface for opencode plugin
// integration tests. A shared setup helper constructs it so each test body
// stays focused on the behaviour it is verifying.
type opencodeHarness struct {
	AgentRunner pluginpkg.AgentRunner
}

// newOpencodeHarness builds the bossd-plugin-opencode binary, spawns it via the
// go-plugin broker, and returns a ready-to-use AgentRunner. All resources are
// registered on t.Cleanup so callers need only call newOpencodeHarness. Mirrors
// newClaudeHarness: it skips in -short mode and BuildPlugin t.Skips when the
// plugin source is absent (public-mirror convention), keeping CI green with or
// without the plugin dir.
func newOpencodeHarness(t *testing.T) *opencodeHarness {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping opencode plugin integration test in short mode")
	}

	binPath := pluginharness.BuildPlugin(t, "bossd-plugin-opencode")

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

	agentRunner, ok := raw.(pluginpkg.AgentRunner)
	if !ok {
		t.Fatalf("dispensed type %T does not implement AgentRunner", raw)
	}

	return &opencodeHarness{AgentRunner: agentRunner}
}

// TestE2E_Opencode_GetInfo verifies the harness stands up end-to-end: the
// plugin builds, spawns, completes the go-plugin handshake, and reports
// Name="opencode" via GetInfo. This is the minimal smoke test for the gRPC
// plumbing between bossd's plugin host and the bossd-plugin-opencode binary.
func TestE2E_Opencode_GetInfo(t *testing.T) {
	h := newOpencodeHarness(t)

	info, err := h.AgentRunner.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.GetName() != "opencode" {
		t.Errorf("plugin name = %q, want %q", info.GetName(), "opencode")
	}
	if info.GetVersion() == "" {
		t.Error("plugin version should not be empty")
	}
}

// TestE2E_Opencode_ListIgnoredDirtyFiles verifies that the opencode plugin's
// ignored-dirty-files RPC survives the real gRPC round-trip and reports the one
// worktree file bossd itself writes for opencode: the BOS-486 question-signal
// hook the plugin injects at StartRun
// (plugins/bossd-plugin-opencode/questionhook.go). This is a pure plugin RPC
// that does not require the opencode CLI to be installed.
//
// Everything else opencode touches lives outside the worktree (session state is
// in the opencode data dir), so this single entry is the whole set — mirroring
// claude's .claude/settings.local.json. Finalize filters `git status` through
// it, catching the case a repo already tracks files under .opencode/plugins/;
// the bossd-maintained .git/info/exclude pattern is the primary filter for the
// far commoner collapsed-`?? .opencode/` case. See
// plugins/bossd-plugin-opencode/dirty_files.go for the split.
func TestE2E_Opencode_ListIgnoredDirtyFiles(t *testing.T) {
	h := newOpencodeHarness(t)

	resp, err := h.AgentRunner.ListIgnoredDirtyFiles(context.Background(), &bossanovav1.ListIgnoredDirtyFilesRequest{
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	want := []string{".opencode/plugins/bossd-question.js"}
	if !slices.Equal(resp.GetPaths(), want) {
		t.Errorf("Paths = %v, want %v", resp.GetPaths(), want)
	}
}

// recordingRunner is a minimal agent.AgentRunner that records the sessionID
// seen by its Start call so the dispatcher-routing test can assert which
// registered runner was routed to. Other AgentRunner methods are no-op stubs —
// the dispatcher only needs to forward Start here. (agent's own
// labeledAgentRunner is unexported, so plugin_test defines its own stub.)
type recordingRunner struct {
	startSeen string
}

func (r *recordingRunner) Start(_ context.Context, _, _ string, _ *string, sessionID, _ string, _ map[string]string) (string, error) {
	r.startSeen = sessionID
	return sessionID, nil
}

func (r *recordingRunner) Stop(_ string) error      { return nil }
func (r *recordingRunner) IsRunning(_ string) bool  { return false }
func (r *recordingRunner) ExitError(_ string) error { return nil }
func (r *recordingRunner) Subscribe(_ context.Context, _ string) (<-chan agent.OutputLine, error) {
	return nil, nil
}
func (r *recordingRunner) History(_ string) []agent.OutputLine { return nil }

// TestE2E_Opencode_DispatcherRoutesByName proves an opencode-named session
// reaches the opencode runner. Two complementary layers:
//
//   - Unit-level routing (always runs, no binary): construct an agent.Dispatcher
//     with a name-keyed registry {"opencode", "claude"} and assert both
//     StartByAgent("opencode", …) and a lookup-driven Start route to the
//     opencode stub and never to the co-registered claude stub. This nails the
//     "routed by GetInfo name" contract deterministically without the real
//     binary.
//   - Live GetInfo name binding (skips cleanly): via the harness, assert the
//     spawned real plugin's GetInfo().Name is exactly the registry key the
//     dispatcher routes on ("opencode") — closing the loop that the name the
//     dispatcher routes on is the name the plugin actually reports.
func TestE2E_Opencode_DispatcherRoutesByName(t *testing.T) {
	t.Run("unit_routing", func(t *testing.T) {
		ctx := context.Background()

		// StartByAgent("opencode", …) routes by explicit name.
		ocStub := &recordingRunner{}
		claudeStub := &recordingRunner{}
		registry := map[string]agent.AgentRunner{
			"opencode": ocStub,
			"claude":   claudeStub,
		}
		byName := agent.NewDispatcher(registry, func(string) (string, error) {
			t.Fatalf("lookup must not be called for StartByAgent")
			return "", nil
		}, "claude", zerolog.Nop())

		if _, err := byName.StartByAgent(ctx, "opencode", "/w", "p", nil, "sid-oc", "", nil); err != nil {
			t.Fatalf("StartByAgent: %v", err)
		}
		if ocStub.startSeen != "sid-oc" {
			t.Errorf("opencode runner did not see Start; got %q", ocStub.startSeen)
		}
		if claudeStub.startSeen != "" {
			t.Errorf("claude runner unexpectedly saw Start: %q", claudeStub.startSeen)
		}

		// A lookup returning "opencode" routes Start to the opencode runner too.
		ocLookupStub := &recordingRunner{}
		claudeLookupStub := &recordingRunner{}
		lookupRegistry := map[string]agent.AgentRunner{
			"opencode": ocLookupStub,
			"claude":   claudeLookupStub,
		}
		byLookup := agent.NewDispatcher(lookupRegistry, func(string) (string, error) {
			return "opencode", nil
		}, "claude", zerolog.Nop())

		if _, err := byLookup.Start(ctx, "/w", "p", nil, "sid-lookup", "", nil); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if ocLookupStub.startSeen != "sid-lookup" {
			t.Errorf("opencode runner did not see lookup-driven Start; got %q", ocLookupStub.startSeen)
		}
		if claudeLookupStub.startSeen != "" {
			t.Errorf("claude runner unexpectedly saw lookup-driven Start: %q", claudeLookupStub.startSeen)
		}
	})

	t.Run("live_getinfo_name_binding", func(t *testing.T) {
		h := newOpencodeHarness(t)

		info, err := h.AgentRunner.GetInfo(context.Background())
		if err != nil {
			t.Fatalf("GetInfo: %v", err)
		}
		// The name the live plugin reports must equal the registry key the
		// dispatcher routes on.
		if info.GetName() != "opencode" {
			t.Errorf("live plugin GetInfo name = %q, want routing key %q", info.GetName(), "opencode")
		}
	})
}

// TestE2E_Opencode_StartRun_RequiresCLI verifies that StartRun returns an error
// (rather than panicking or hanging) when the opencode CLI is not installed.
// When the CLI is present in PATH the test is skipped — live end-to-end runs
// against the real binary are part 5's scope, not this slice's. What we assert
// here is that the gRPC plumbing round-trips the error back to the host.
//
// In this slice StartRun is still codes.Unimplemented (the run/launch path lands
// in a later slice — see plugins/bossd-plugin-opencode/server.go), so today this
// assertion proves the gRPC error round-trip rather than a genuine CLI-absence
// failure. It is forward-compatible: once StartRun is wired to the real runner,
// the same assertion becomes a true "requires CLI" check on the no-binary path.
func TestE2E_Opencode_StartRun_RequiresCLI(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err == nil {
		t.Skip("opencode CLI is installed; live-binary runs are part 5 — skipping RPC error path")
	}

	h := newOpencodeHarness(t)

	_, err := h.AgentRunner.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		SessionId: "test-session-id",
		WorkDir:   t.TempDir(),
		Plan:      "hello",
	})
	if err == nil {
		t.Error("StartRun succeeded without opencode installed; expected an error")
	}
}
