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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/recurser/boss/internal/fixtures"
	"github.com/recurser/boss/internal/tuidriver"
	"github.com/recurser/boss/internal/tuitest"
)

// seedEnvVar is the env var a scenario driver (the Node side) uses to forward a
// RAW, unvalidated env map into the bridge as a JSON object. The bridge is the
// SINGLE validator (tuitest.FilterProofEnv): Node never filters, so there is no
// Go/TS whitelist to drift (the PathToProjectKey anti-pattern). The bridge reads
// this var, validates every key against tuitest.ProofEnvWhitelist, aborts boot
// listing any rejected key by name, and appends only the allowed subset to the
// boss subprocess env. Chosen over a -seed-env flag because Node already builds
// the child env object, so a single JSON var is the least plumbing; it survives
// BaseHarnessEnv (which strips the whitelisted families themselves, not this
// carrier var).
const seedEnvVar = "BOSS_PROOF_TUI_SEED_ENV"

// maxSeedBytes caps the -seed overlay file so a runaway/hostile file cannot
// exhaust memory at boot. 1 MiB is far above any legitimate display overlay.
const maxSeedBytes = 1 << 20

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

// sortedKeys returns a map's keys in sorted order for deterministic iteration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// serviceDir resolves the services/boss directory from this file's location.
// This file lives at services/boss/cmd/proof-tui-agent/main.go, so two ".."
// segments (out of proof-tui-agent, out of cmd) reach services/boss.
func serviceDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// seedWorld populates the mock daemon with a preset's deterministic dataset.
// An empty World (login/onboarding/empty presets) is a no-op.
func seedWorld(d *tuitest.MockDaemon, w fixtures.World) {
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
	for _, a := range w.Accounts {
		// Display-safe metadata only; the Account proto has no credential field,
		// so a nil credential is correct (BOS-265 Settings → Accounts list).
		d.SeedAccount(a, nil)
	}
}

func main() {
	fixture := flag.String("fixture", "demo", "Fixture preset: "+strings.Join(fixtures.PresetNames(), " | "))
	width := flag.Int("width", 140, "terminal width in columns")
	height := flag.Int("height", 36, "terminal height in rows")
	worktreeBaseDir := flag.String("worktree-base-dir", "/home/bossanova/worktrees", "deterministic worktree base dir for seeded settings")
	bossBin := flag.String("boss-bin", "", "path to a prebuilt -tags e2e boss binary; if empty, build it")
	castPath := flag.String("cast", "", "if set, write an asciinema .cast of the session to this path")
	seedPath := flag.String("seed", "", "if set, a JSON overlay file of extra display entities to seed on top of the preset world")
	flag.Parse()

	// Scenario env is forwarded RAW via seedEnvVar (see its doc). Read it here so
	// run() stays testable with an explicit string.
	if err := run(*fixture, *width, *height, *worktreeBaseDir, *bossBin, *castPath, *seedPath, os.Getenv(seedEnvVar)); err != nil {
		log.Fatalf("proof-tui-agent: %v", err)
	}
}

func run(fixture string, width, height int, worktreeBaseDir, bossBin, castPath, seedPath, seedEnvJSON string) error {
	// The preset registry replaces the old fixture switches: it owns the seeded
	// World, the settings-seed kind, and any default env. Unknown names error
	// with the full valid list.
	preset, err := fixtures.LookupPreset(fixture)
	if err != nil {
		return err
	}

	// Validate the boot-time inputs (overlay file + scenario env) BEFORE any
	// expensive work (mock daemon, boss build) so a bad -seed field or a
	// non-whitelisted env key aborts fast with a self-explanatory error. Both are
	// agent-authored, so each failure must name the offender.
	var overlay fixtures.Overlay
	if seedPath != "" {
		overlay, err = readSeedOverlay(seedPath)
		if err != nil {
			return err
		}
	}
	allowedEnv, err := validatedSeedEnv(seedEnvJSON)
	if err != nil {
		return err
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
	seedWorld(daemon, preset.World())
	// Slow archives just enough that the optimistic in-flight "archiving" row
	// state is observable/recordable (BOS-251); an instant mock archive settles
	// before any capture could catch it.
	daemon.SetArchiveDelay(4 * time.Second)

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

	// Apply the -seed overlay on top of the preset world. *tuitest.MockDaemon
	// satisfies the fixtures.overlayTarget interface structurally, so it passes
	// directly — no import cycle. Validation already ran in readSeedOverlay's
	// parse; Build() re-validates required/enum fields as it maps to pb. A failure
	// here returns through the deferred cleanup above — no manual teardown ladder.
	if err := fixtures.ApplyOverlay(daemon, overlay); err != nil {
		return fmt.Errorf("apply -seed overlay: %w", err)
	}

	// Signal-driven teardown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	// 4. Seed settings by preset kind. The two tuitest helpers take different
	// args (acknowledged wants worktreeBaseDir; first-run wants the socket), so
	// the bridge — not the fixtures package — maps kind -> helper.
	switch preset.SeedKind {
	case fixtures.SeedAcknowledged:
		if err := tuitest.SeedSettingsAcknowledged(home, worktreeBaseDir); err != nil {
			return fmt.Errorf("seed settings: %w", err)
		}
	case fixtures.SeedFirstRun:
		if err := tuitest.SeedFirstRunSettings(home, socket); err != nil {
			return fmt.Errorf("seed first-run settings: %w", err)
		}
	}

	// firstRun presets (onboarding) drive the pre-auth path: boss skips the
	// daemon-startup preflight only when BOSS_SOCKET is unset, never renders the
	// app shell, and shows no signed-in email. Acknowledged presets (demo/login/
	// empty/busy) are the signed-in home-board path. Keying these off SeedKind —
	// rather than a hardcoded name list — keeps demo/login byte-identical to the
	// pre-registry bridge while giving the new empty/busy presets the sensible
	// signed-in env (socket + auth email).
	firstRun := preset.SeedKind == fixtures.SeedFirstRun

	// 5. Build env (mirror harness.go:399-417).
	env := tuitest.BaseHarnessEnv(os.Environ())
	env = append(env,
		"BOSS_SKIP_SKILLS=1",
		"BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1",
		"TERM=xterm-256color",
		"HOME="+home,
	)
	if !firstRun {
		env = append(env, "BOSS_SOCKET="+socket)
	}
	if runtime.GOOS != "darwin" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	}
	if !firstRun {
		env = append(env, "BOSS_AUTH_E2E_EMAIL=proof@example.com")
	}

	// Merge the preset's default env (K=V) on top. Sorted for a deterministic
	// order. For demo this adds BOSS_CLOUD_ACCESS_E2E_SEQUENCE=active. Note this
	// DOES reach the boss subprocess where the ambient copy would not: BaseHarnessEnv
	// (above) strips the BOSS_CLOUD_ACCESS_E2E_* family from os.Environ(), so the
	// Node-set bridge value never survived to boss pre-registry — demo then fell
	// back to the real remote cloud client. Pinning it via the preset makes demo
	// use the deterministic e2e cloud-access fake instead (intended, BOS-217).
	for _, k := range sortedKeys(preset.DefaultEnv) {
		env = append(env, k+"="+preset.DefaultEnv[k])
	}

	// Layer the validated scenario-requested env ON TOP of the preset defaults
	// (sorted for a deterministic order). Only whitelisted keys reach here —
	// validatedSeedEnv already aborted boot if any were rejected.
	for _, k := range sortedKeys(allowedEnv) {
		env = append(env, k+"="+allowedEnv[k])
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

	// First-run (onboarding) never renders the app shell, so only wait for the
	// banner when a real home screen is expected.
	if !firstRun {
		if err := drv.WaitForText(10*time.Second, "Bossanova"); err != nil {
			return fmt.Errorf("wait for app shell: %w", err)
		}
	}

	// 10. Serve. The mock daemon is the daemonMutator the "daemon" op drives
	// (*tuitest.MockDaemon satisfies the interface directly).
	if err := serve(drv, daemon, os.Stdin, os.Stdout, defaultSettle(width, height)); err != nil && !errors.Is(err, errQuit) {
		return err
	}
	return nil
}

// readSeedOverlay reads and parses the -seed overlay file, capping the read at
// maxSeedBytes. Parsing uses fixtures.ParseOverlay (DisallowUnknownFields), so a
// typo'd field aborts boot naming the field.
func readSeedOverlay(path string) (fixtures.Overlay, error) {
	f, err := os.Open(path)
	if err != nil {
		return fixtures.Overlay{}, fmt.Errorf("open -seed file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// Read one byte past the cap so an oversized file is detected, not truncated.
	data, err := io.ReadAll(io.LimitReader(f, maxSeedBytes+1))
	if err != nil {
		return fixtures.Overlay{}, fmt.Errorf("read -seed file: %w", err)
	}
	if len(data) > maxSeedBytes {
		return fixtures.Overlay{}, fmt.Errorf("-seed file %q exceeds %d bytes", path, maxSeedBytes)
	}
	overlay, err := fixtures.ParseOverlay(data)
	if err != nil {
		return fixtures.Overlay{}, fmt.Errorf("-seed %q: %w", path, err)
	}
	return overlay, nil
}

// validatedSeedEnv parses the RAW scenario env JSON (from seedEnvVar) and returns
// only the whitelisted subset. A non-whitelisted key aborts boot with an error
// listing every rejected key by name — a required-proof deliverable. An empty
// input is a no-op (nil map).
func validatedSeedEnv(seedEnvJSON string) (map[string]string, error) {
	if strings.TrimSpace(seedEnvJSON) == "" {
		return nil, nil
	}
	var requested map[string]string
	if err := json.Unmarshal([]byte(seedEnvJSON), &requested); err != nil {
		return nil, fmt.Errorf("parse %s: %w", seedEnvVar, err)
	}
	allowed, rejected := tuitest.FilterProofEnv(requested)
	if len(rejected) > 0 {
		return nil, fmt.Errorf("%s: rejected non-whitelisted env key(s): %s (allowed prefixes: %s)",
			seedEnvVar, strings.Join(rejected, ", "), strings.Join(tuitest.ProofEnvWhitelist, ", "))
	}
	return allowed, nil
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
