// Command proof-tui-agent hosts a T-free fixture stack plus a real boss TUI in
// a PTY and speaks NDJSON over stdin/stdout: one JSON request per input line,
// one JSON response per output line. It is the long-lived Go driver process a
// Node brain (BOS-70) drives to operate the TUI in "agent mode", and can
// optionally capture an asciinema .cast of the session.
//
// The "key" op's key names resolve through tuidriver.KeyBytes, which accepts
// single characters, "ctrl+<a-z>", enter/return, esc/escape, the arrows
// (up/down/right/left), tab and shift+tab, pgup/pgdn, home/end, backspace and
// delete, and f1-f12 (case-insensitive) — enough to drive list- and form-shaped
// views. One caveat: the "key" op writes a list's keys back-to-back with no
// delimiter, so a leading "esc" immediately followed by another key in the SAME
// list decodes as alt+<key> (a bare ESC acts as a meta prefix, per ultraviolet's
// input parser). Send "esc" alone — its own "key" op or the standalone "esc" op
// — to cancel/back out.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuidriver"
	"github.com/recurser/boss/internal/tuitest"
)

// defaultSettle is the production stability auto-settle config. cols/rows are
// filled from the terminal flags before serving.
func defaultSettle(cols, rows int) settleConfig {
	return settleConfig{
		poll:      50 * time.Millisecond,
		stableFor: 350 * time.Millisecond,
		hardCap:   4 * time.Second,
		cols:      cols,
		rows:      rows,
	}
}

// serviceDir resolves the services/boss directory from this file's location.
// This file lives at services/boss/cmd/proof-tui-agent/main.go, so two ".."
// segments (out of proof-tui-agent, out of cmd) reach services/boss.
func serviceDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
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

func main() {
	fixture := flag.String("fixture", "demo", "Fixture profile: demo | login | onboarding")
	width := flag.Int("width", 140, "terminal width in columns")
	height := flag.Int("height", 36, "terminal height in rows")
	worktreeBaseDir := flag.String("worktree-base-dir", "/home/bossanova/worktrees", "deterministic worktree base dir for seeded settings")
	bossBin := flag.String("boss-bin", "", "path to a prebuilt -tags e2e boss binary; if empty, build it")
	castPath := flag.String("cast", "", "if set, write an asciinema .cast of the session to this path")
	flag.Parse()

	if err := run(*fixture, *width, *height, *worktreeBaseDir, *bossBin, *castPath); err != nil {
		log.Fatalf("proof-tui-agent: %v", err)
	}
}

func run(fixture string, width, height int, worktreeBaseDir, bossBin, castPath string) error {
	switch fixture {
	case "demo", "login", "onboarding":
	default:
		return fmt.Errorf("unknown fixture profile %q", fixture)
	}

	// 1. Per-run HOME.
	home, err := os.MkdirTemp("", "proof-tui-agent-home-")
	if err != nil {
		return fmt.Errorf("mkdtemp home: %w", err)
	}

	// 2. Short socket dir (keep socket path < 104 chars for macOS).
	socketDir, err := os.MkdirTemp("", "ptua-")
	if err != nil {
		_ = os.RemoveAll(home)
		return fmt.Errorf("mkdtemp socket: %w", err)
	}
	socket := filepath.Join(socketDir, "daemon.sock")

	// 3. Mock daemon (+ demo seed).
	daemon, stopDaemon, err := tuitest.StartMockDaemon(socket)
	if err != nil {
		_ = os.RemoveAll(socketDir)
		_ = os.RemoveAll(home)
		return fmt.Errorf("start mock daemon: %w", err)
	}
	if fixture == "demo" {
		seedDemoWorld(daemon)
	}

	// Guaranteed teardown (sync.Once), installed as soon as the daemon and temp
	// dirs exist so every early-return path below also unwinds them. drv and
	// castFile are filled in later; the closure reads them at teardown time.
	var (
		drv      *tuidriver.Driver
		castFile *os.File
	)
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			if drv != nil {
				_ = drv.Close()
			}
			_ = stopDaemon()
			if castFile != nil {
				_ = castFile.Close()
			}
			_ = os.RemoveAll(home)
			_ = os.RemoveAll(socketDir)
		})
	}
	defer cleanup()

	// Signal-driven teardown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	// 4. Seed settings.
	switch fixture {
	case "demo", "login":
		if err := tuitest.SeedSettingsAcknowledged(home, worktreeBaseDir); err != nil {
			return fmt.Errorf("seed settings: %w", err)
		}
	case "onboarding":
		if err := tuitest.SeedFirstRunSettings(home, socket); err != nil {
			return fmt.Errorf("seed first-run settings: %w", err)
		}
	}

	// 5. Build env (mirror harness.go:399-417).
	env := tuitest.BaseHarnessEnv(os.Environ())
	env = append(env,
		"BOSS_SKIP_SKILLS=1",
		"BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1",
		"TERM=xterm-256color",
		"HOME="+home,
	)
	if fixture != "onboarding" {
		env = append(env, "BOSS_SOCKET="+socket)
	}
	if runtime.GOOS != "darwin" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	}
	if fixture == "demo" || fixture == "login" {
		env = append(env, "BOSS_AUTH_E2E_EMAIL=proof@example.com")
	}

	// 6. Boss binary: use prebuilt or build with -tags e2e.
	if bossBin == "" {
		built, cleanup, berr := buildBoss()
		if berr != nil {
			return berr
		}
		defer cleanup()
		bossBin = built
	}

	// 7. Optional .cast capture.
	var castWriter *castWriter
	if castPath != "" {
		castFile, err = os.Create(castPath)
		if err != nil {
			return fmt.Errorf("create cast file: %w", err)
		}
		castWriter, err = newCastWriter(castFile, width, height, time.Now)
		if err != nil {
			_ = castFile.Close()
			return fmt.Errorf("new cast writer: %w", err)
		}
	}

	// 8. Driver.
	driverOpts := tuidriver.Options{
		Command: bossBin,
		Env:     env,
		Width:   width,
		Height:  height,
	}
	if castWriter != nil {
		driverOpts.RawOutput = castWriter
	}
	drv, err = tuidriver.New(driverOpts)
	if err != nil {
		return fmt.Errorf("create TUI driver: %w", err)
	}

	// Onboarding never renders the app shell, so only wait for the banner when
	// a real home screen is expected.
	if fixture != "onboarding" {
		if err := drv.WaitForText(10*time.Second, "Bossanova"); err != nil {
			return fmt.Errorf("wait for app shell: %w", err)
		}
	}

	// 10. Serve.
	if err := serve(drv, os.Stdin, os.Stdout, defaultSettle(width, height)); err != nil && !errors.Is(err, errQuit) {
		return err
	}
	return nil
}

// buildBoss compiles boss with the e2e tag into a temp dir and returns the
// binary path plus a cleanup func.
func buildBoss() (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "proof-tui-agent-boss-")
	if err != nil {
		return "", func() {}, fmt.Errorf("mkdtemp boss: %w", err)
	}
	binPath := filepath.Join(dir, "boss")
	cmd := exec.Command("go", "build", "-tags", "e2e", "-o", binPath, "./cmd")
	cmd.Dir = serviceDir()
	out, berr := cmd.CombinedOutput()
	if berr != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("build boss: %w\n%s", berr, out)
	}
	return binPath, func() { _ = os.RemoveAll(dir) }, nil
}
