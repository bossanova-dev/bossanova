// Command proof-fixture-daemon starts a mock bossd on a Unix socket and seeds
// it with demo data so the VHS proof-capture scripts have a stable world to
// drive boss against. The launcher (Task 3) boots this before exec'ing boss.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuitest"
)

// configDirForHome mirrors the unexported helper in tuitest/harness.go.
func configDirForHome(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "bossanova")
	}
	return filepath.Join(home, ".config", "bossanova")
}

// seedDemoWorld populates the mock daemon with the deterministic demo dataset.
func seedDemoWorld(d *tuitest.MockDaemon) {
	w := fixtures.DemoWorld()
	for _, r := range w.Repos {
		d.AddRepo(r)
	}
	for _, s := range w.Sessions {
		d.AddSession(s)
	}
	for _, c := range w.Chats {
		d.AddChat(c)
	}
	for _, j := range w.CronJobs {
		d.AddCronJob(j)
	}
}

// seedHome writes the settings profile that matches tuitest proof fixtures.
func seedHome(home, socket, fixture string) error {
	configDir := configDirForHome(home)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	var settings map[string]any
	switch fixture {
	case "demo", "login":
		settings = map[string]any{
			"providers_acknowledged": true,
			"worktree_base_dir":      "/home/bossanova/worktrees",
		}
	case "onboarding":
		settings = map[string]any{
			"providers_acknowledged": false,
			"socket_path":            socket,
		}
	default:
		return fmt.Errorf("unknown fixture profile %q", fixture)
	}
	contents, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "settings.json"), contents, 0o644)
}

func main() {
	socket := flag.String("socket", "", "Unix socket path for the mock daemon (required)")
	fixture := flag.String("fixture", "demo", "Fixture profile: demo | login | onboarding")
	seedHomeDir := flag.String("seed-home", "", "HOME directory to seed with settings.json (required)")
	flag.Parse()

	if *socket == "" {
		log.Fatal("--socket is required")
	}
	if *seedHomeDir == "" {
		log.Fatal("--seed-home is required")
	}

	d, stop, err := tuitest.StartMockDaemon(*socket)
	if err != nil {
		log.Fatalf("start mock daemon: %v", err)
	}

	if *fixture == "demo" {
		seedDemoWorld(d)
	}

	if err := seedHome(*seedHomeDir, *socket, *fixture); err != nil {
		log.Fatalf("seed home: %v", err)
	}

	// Signal readiness to the launcher.
	_, _ = os.Stdout.WriteString("PROOF_FIXTURE_DAEMON_READY\n")

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch

	if err := stop(); err != nil {
		log.Printf("stop: %v", err)
	}
}
