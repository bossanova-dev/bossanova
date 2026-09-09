package plugin_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	"github.com/recurser/bossalib/config"
	sharedplugin "github.com/recurser/bossalib/plugin"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// These are the only end-to-end evidence that a real orphaned plugin actually
// dies. They spawn real subprocesses, so they are gated on -short like every
// other real-subprocess test in this package — which means `make test` and
// `make test-smoke` SKIP them and a green run from either says nothing about
// this behaviour. Run the package's non-short suite to exercise them.
const (
	// orphanExitBudget bounds how long an orphaned plugin may take to notice.
	// The watchdog polls every DefaultParentPollInterval, so this is many
	// intervals of slack — and still four orders of magnitude under the ~1d21h
	// the leaked processes survived in the sighting that motivated this.
	orphanExitBudget = 30 * time.Second

	// orphanSurvivalWindow is how long the negative controls wait before
	// concluding a plugin is still running. It must be comfortably longer than
	// the watchdog's poll interval, or "did not die yet" would be
	// indistinguishable from "has not polled yet".
	orphanSurvivalWindow = 10 * time.Second
)

func requireOrphanTestEnv(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping orphan integration test in short mode")
	}
	return pluginharness.PluginBinary(t, "bossd-plugin-stub-runner")
}

// TestOrphanPluginExitsWhenItsRealParentDies is R1's end-to-end proof. A real
// plugin subprocess whose actual parent process is SIGKILLed must exit on its
// own, with no host action, well inside a bounded window.
func TestOrphanPluginExitsWhenItsRealParentDies(t *testing.T) {
	binPath := requireOrphanTestEnv(t)

	spawner := startOrphanSpawner(t, binPath, true)
	pluginPID := spawner.pluginPID

	// Kill the process that really is the plugin's parent. Abandoning the
	// host-side go-plugin client instead would leave the plugin parented to
	// this test process, os.Getppid would never change, and the watchdog would
	// correctly never fire.
	spawner.kill(t)

	if !waitProcessGone(pluginPID, orphanExitBudget) {
		t.Fatalf("orphaned plugin pid %d still alive after %v; the parent-death watchdog did not fire",
			pluginPID, orphanExitBudget)
	}
}

// TestOrphanPluginSurvivesWhileItsParentLives is the negative control without
// which a watchdog that kills unconditionally would pass the test above. It is
// only worth anything because startOrphanSpawner first proves the plugin
// reported arming: "did not die" is otherwise satisfied just as well by a
// plugin whose watchdog never armed at all.
func TestOrphanPluginSurvivesWhileItsParentLives(t *testing.T) {
	binPath := requireOrphanTestEnv(t)

	spawner := startOrphanSpawner(t, binPath, true)
	pluginPID := spawner.pluginPID

	// The spawner stays alive for the whole window: the plugin's parent never
	// changes, so an armed watchdog must never fire.
	time.Sleep(orphanSurvivalWindow)

	if !processAlive(pluginPID) {
		t.Fatalf("plugin pid %d exited within %v while its parent was still alive; "+
			"the watchdog is firing unconditionally", pluginPID, orphanSurvivalWindow)
	}
}

// TestOrphanPluginWithoutStampedIdentitySurvivesParentDeath is the R7
// fail-open control, and the one that catches a guard wired backwards: with no
// parent identity supplied the plugin must keep running even after its real
// parent is killed. That is today's leak, deliberately preserved as the
// compatibility case for a plugin launched by anything other than bossd.
func TestOrphanPluginWithoutStampedIdentitySurvivesParentDeath(t *testing.T) {
	binPath := requireOrphanTestEnv(t)

	spawner := startOrphanSpawner(t, binPath, false)
	pluginPID := spawner.pluginPID

	spawner.kill(t)
	time.Sleep(orphanSurvivalWindow)

	if !processAlive(pluginPID) {
		t.Fatalf("plugin pid %d with no stamped parent identity exited after its parent died; "+
			"the watchdog armed on an absent identity instead of failing open", pluginPID)
	}
	killPID(pluginPID)
}

// TestOrphanGracefulKillStillReapsWithWatchdogArmed is R3: arming the watchdog
// must not disturb the graceful path. The plugin here is a direct child of the
// test process and is stamped with the test process's own PID, so the watchdog
// arms — and then go-plugin's Kill, the same call Host.Stop makes under
// killWithTimeout, must still reap it inside the existing budget.
//
// "So the watchdog arms" is checked here, not assumed. client.Kill reaps by
// SIGKILL whether or not anything armed, so without reading the plugin's own
// armed marker off its stderr this test passes identically against a build
// where the stamp is shadowed and the watchdog silently refuses to arm — which
// is the state that reinstates the leak this whole change exists to close.
func TestOrphanGracefulKillStillReapsWithWatchdogArmed(t *testing.T) {
	binPath := requireOrphanTestEnv(t)

	// #nosec G204 -- test harness runs a plugin binary this test resolved.
	cmd := exec.Command(binPath)
	cmd.Env = pluginpkg.PluginSubprocessEnv(config.PluginConfig{Name: "stub-runner"}, os.Getpid())

	watcher := &armedWatcher{}
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: pluginpkg.NewHandshake("orphan-graceful-cookie"),
		Plugins: goplugin.PluginSet{
			sharedplugin.PluginTypeAgentRunner: pluginpkg.NewAgentRunnerGRPCPlugin(hostService),
		},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		Stderr:           watcher,
	})
	t.Cleanup(client.Kill)

	if _, err := client.Client(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	rc := client.ReattachConfig()
	if rc == nil || rc.Pid <= 0 {
		t.Fatal("no plugin pid after handshake")
	}
	pluginPID := rc.Pid
	t.Cleanup(func() { killPID(pluginPID) })

	if !watcher.waitArmed(orphanArmWindow) {
		t.Fatalf("plugin pid %d never reported %q; it is sitting in a fail-open branch, "+
			"so a graceful Kill here proves nothing about the armed path",
			pluginPID, sharedplugin.ParentWatchArmedMsg)
	}

	start := time.Now()
	client.Kill()

	if !waitProcessGone(pluginPID, 5*time.Second) {
		t.Fatalf("graceful Kill did not reap plugin pid %d within 5s (took >%v)",
			pluginPID, time.Since(start))
	}
}
