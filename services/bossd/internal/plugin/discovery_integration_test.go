package plugin_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/status"
)

// allPluginNames lists every plugin the daemon ships today. The discovery
// tests build each into a shared directory and assert the host loader picks
// them all up. If a new plugin is added, extend this slice (plus the
// expected-interface map below) so discovery coverage doesn't silently drift.
var allPluginNames = []string{
	"bossd-plugin-dependabot",
	"bossd-plugin-linear",
	"bossd-plugin-repair",
}

// pluginInterfaces encodes which service each plugin serves. TaskSource and
// WorkflowService are the only two interfaces the host probes after Dispense;
// discovery asserts that GetInfo succeeds on exactly the interface we expect
// for each plugin so a regression where a plugin loses its registration
// surfaces as a missing service rather than a silent no-op.
var pluginInterfaces = map[string]string{
	"bossd-plugin-repair":     "workflow",
	"bossd-plugin-dependabot": "task",
	"bossd-plugin-linear":     "task",
}

// buildAllPluginsInto compiles every plugin in allPluginNames into dir. The
// SkipsBadBinary test reuses this helper so both discovery tests share the
// ~5s cost of four `go build` invocations per run (see handoff's "Things to
// Watch For" — four builds in one test is the single biggest cost here).
func buildAllPluginsInto(t *testing.T, dir string) {
	t.Helper()
	for _, name := range allPluginNames {
		pluginharness.BuildPluginInto(t, dir, name)
	}
}

// discoveryHost bundles a *plugin.Host with all the stores, trackers, and
// claude runner that its HostServiceServer needs. Validate() runs inside
// Start() when at least one plugin is enabled and refuses to proceed unless
// every dep is wired, so tests that exercise the real loader must wire them
// all — even the ones discovery itself never touches.
type discoveryHost struct {
	host     *pluginpkg.Host
	db       *sql.DB
	eventBus *eventbus.Bus
}

// newDiscoveryHost assembles a fully-wired Host. The fake Claude script is
// used as the CommandFactory target so the runner has a valid executable,
// but discovery tests never drive an attempt — GetInfo doesn't touch claude.
func newDiscoveryHost(t *testing.T) *discoveryHost {
	t.Helper()

	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := migrate.Run(sqlDB, os.DirFS(pluginharness.MigrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewClaudeChatStore(sqlDB)

	provider := &testVCSProvider{}
	bus := eventbus.New(zerolog.Nop())
	host := pluginpkg.New(bus, provider, zerolog.Nop())
	host.SetSessionDeps(repos, sessions, chats, status.NewDisplayTracker(), status.NewTracker())
	host.SetClaudeRunner(claude.NewRunner(zerolog.Nop()))

	// Cleanup runs LIFO: register bus.Close first so host.Stop (registered
	// second) runs before the bus is torn down — host depends on bus.
	t.Cleanup(func() { bus.Close() })
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Logf("host.Stop: %v", err)
		}
	})

	return &discoveryHost{host: host, db: sqlDB, eventBus: bus}
}

// configsFromDir constructs PluginConfigs for every entry in dir whose name
// matches the daemon's plugin prefix. This mirrors the shape of what
// config.DiscoverPlugins() hands to Host.Start in production — the scan
// itself is covered by config_test; here we exercise the full loader stack
// once we have the configs.
func configsFromDir(t *testing.T, dir string) []config.PluginConfig {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	const prefix = "bossd-plugin-"
	var cfgs []config.PluginConfig
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) <= len(prefix) || name[:len(prefix)] != prefix {
			continue
		}
		cfgs = append(cfgs, config.PluginConfig{
			Name:    name[len(prefix):],
			Path:    filepath.Join(dir, name),
			Enabled: true,
		})
	}
	return cfgs
}

// TestE2E_PluginDiscovery_LoadsAllPlugins builds every plugin binary into a
// shared directory, points a real *plugin.Host at the resulting configs, and
// asserts that (a) each plugin appears in Plugins() as Running, and (b) the
// GetInfo RPC returns the expected plugin name via whichever interface the
// plugin serves. This is the end-to-end happy path of the daemon's loader.
func TestE2E_PluginDiscovery_LoadsAllPlugins(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping plugin discovery integration test in short mode")
	}

	pluginDir := t.TempDir()
	buildAllPluginsInto(t, pluginDir)

	cfgs := configsFromDir(t, pluginDir)
	if len(cfgs) != len(allPluginNames) {
		t.Fatalf("expected %d configs discovered from %s, got %d", len(allPluginNames), pluginDir, len(cfgs))
	}

	d := newDiscoveryHost(t)
	if err := d.host.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	statuses := d.host.Plugins()
	if len(statuses) != len(allPluginNames) {
		t.Fatalf("expected %d plugins tracked, got %d: %+v", len(allPluginNames), len(statuses), statuses)
	}

	gotNames := make([]string, len(statuses))
	for i, s := range statuses {
		if !s.Running {
			t.Errorf("plugin %q tracked but not Running", s.Name)
		}
		gotNames[i] = s.Name
	}
	sort.Strings(gotNames)
	wantNames := make([]string, len(allPluginNames))
	for i, n := range allPluginNames {
		wantNames[i] = n[len("bossd-plugin-"):]
	}
	sort.Strings(wantNames)
	for i, want := range wantNames {
		if gotNames[i] != want {
			t.Errorf("plugin[%d] name = %q, want %q (full list: got=%v want=%v)", i, gotNames[i], want, gotNames, wantNames)
		}
	}

	// GetInfo through each cached interface. Host caches an interface only
	// after its own GetInfo probe succeeds during Start, so the counts here
	// are the real attestation that discovery dispensed every plugin — a
	// regression where, say, repair drops Workflow registration would
	// surface as GetWorkflowServices returning fewer interfaces.
	var wantTask, wantWorkflow int
	for _, iface := range pluginInterfaces {
		switch iface {
		case "task":
			wantTask++
		case "workflow":
			wantWorkflow++
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	taskSources := d.host.GetTaskSources()
	if len(taskSources) != wantTask {
		t.Fatalf("GetTaskSources len = %d, want %d", len(taskSources), wantTask)
	}
	for i, ts := range taskSources {
		info, err := ts.GetInfo(ctx)
		if err != nil {
			t.Errorf("taskSources[%d].GetInfo: %v", i, err)
			continue
		}
		if info.GetName() == "" {
			t.Errorf("taskSources[%d].GetInfo returned empty Name", i)
		}
	}

	workflowServices := d.host.GetWorkflowServices()
	if len(workflowServices) != wantWorkflow {
		t.Fatalf("GetWorkflowServices len = %d, want %d", len(workflowServices), wantWorkflow)
	}
	for i, ws := range workflowServices {
		info, err := ws.GetInfo(ctx)
		if err != nil {
			t.Errorf("workflowServices[%d].GetInfo: %v", i, err)
			continue
		}
		if info.GetName() == "" {
			t.Errorf("workflowServices[%d].GetInfo returned empty Name", i)
		}
	}
}

// buildCrashingPluginBinary compiles a tiny Go program that exits(1)
// immediately into outDir under a name that passes the daemon's plugin-prefix
// filter. The go-plugin handshake fails because the subprocess exits before
// writing the handshake line, exercising the exact failure mode a stale or
// broken production build would hit.
func buildCrashingPluginBinary(t *testing.T, outDir, name string) string {
	t.Helper()

	srcDir := t.TempDir()
	src := `package main

import "os"

func main() { os.Exit(1) }
`
	srcPath := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(srcPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write crashing main.go: %v", err)
	}

	binPath := filepath.Join(outDir, name)
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build crashing plugin: %v", err)
	}
	return binPath
}

// TestE2E_PluginDiscovery_SkipsBadBinary plants a handshake-crashing binary
// alongside the four real plugins. The host must log the failure and
// continue loading the other plugins — the production contract pinned in
// host.go's log-and-continue path. If this ever regresses to fail-fast, the
// remaining three valid plugins silently won't load and the daemon is DOS'd
// by whichever plugin the loader hit first.
func TestE2E_PluginDiscovery_SkipsBadBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping plugin discovery integration test in short mode")
	}

	pluginDir := t.TempDir()
	buildAllPluginsInto(t, pluginDir)
	buildCrashingPluginBinary(t, pluginDir, "bossd-plugin-broken")

	cfgs := configsFromDir(t, pluginDir)
	if len(cfgs) != len(allPluginNames)+1 {
		t.Fatalf("expected %d configs (%d plugins + 1 broken), got %d", len(allPluginNames)+1, len(allPluginNames), len(cfgs))
	}

	d := newDiscoveryHost(t)
	if err := d.host.Start(t.Context(), cfgs); err != nil {
		t.Fatalf("host.Start should not abort on bad plugin: %v", err)
	}

	// Every valid plugin must have loaded; the bad one must be absent.
	statuses := d.host.Plugins()
	running := map[string]bool{}
	for _, s := range statuses {
		if s.Running {
			running[s.Name] = true
		}
	}

	for _, name := range allPluginNames {
		short := name[len("bossd-plugin-"):]
		if !running[short] {
			t.Errorf("valid plugin %q not running after bad binary was present; statuses=%+v", short, statuses)
		}
	}
	if running["broken"] {
		t.Error("expected broken plugin NOT to be Running, but it was")
	}

	// Spot-check GetInfo still works for valid plugins after the skip.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for i, ts := range d.host.GetTaskSources() {
		if _, err := ts.GetInfo(ctx); err != nil {
			t.Errorf("GetInfo on taskSources[%d] after skip: %v", i, err)
		}
	}
	for i, ws := range d.host.GetWorkflowServices() {
		if _, err := ws.GetInfo(ctx); err != nil {
			t.Errorf("GetInfo on workflowServices[%d] after skip: %v", i, err)
		}
	}
}
