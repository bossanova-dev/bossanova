// Command proof-tui-agent hosts a T-free fixture stack plus a real boss TUI in
// a PTY and speaks NDJSON over stdin/stdout: one JSON request per input line,
// one JSON response per output line. It is the long-lived Go driver process a
// Node brain (BOS-70) drives to operate the TUI in "agent mode", and can
// optionally capture an asciinema .cast of the session. With -socket and -home
// it runs in attach mode instead: no fixture stack, the TUI is driven against
// an existing daemon socket with the caller's HOME (see runAttach).
//
// The "key" op's key names resolve through tuidriver.KeyBytes: single
// characters, "ctrl+<a-z>", and the named keys and aliases enumerated in that
// package's namedKeys map (arrows, the "alt+up"/"alt+down" reorder chords, tab,
// paging, home/end, backspace, delete, and f1-f12) — see tuidriver.KeyBytes for
// the authoritative set, enough to drive list- and form-shaped views. One
// caveat: the "key" op writes a list's keys back-to-back with no delimiter, so a
// leading "esc" immediately followed by another key in the SAME list decodes as
// alt+<key> (a bare ESC acts as a meta prefix, per ultraviolet's input parser).
// Send "esc" alone — its own "key" op or the standalone "esc" op — to
// cancel/back out. That caveat is why "alt+up"/"alt+down" are the parameterised
// CSI sequences rather than the ESC-prefix meta form: the chord has to be
// distinguishable from an "esc" the scenario chained before an arrow.
package main

import (
	"bufio"
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
	"slices"
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

const (
	proofSettingsEventTracingEnv = "BOSS_PROOF_SETTINGS_EVENT_TRACING"
	proofSettingsPostHogHostEnv  = "BOSS_PROOF_SETTINGS_POSTHOG_HOST"
)

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
	for _, e := range w.ChatStatuses {
		d.AddChatStatus(e)
	}
	for _, e := range w.SessionStatuses {
		d.AddSessionStatus(e)
	}
	for _, j := range w.CronJobs {
		d.AddCronJob(j)
	}
	if len(w.CreateSessionScript) > 0 {
		d.SetCreateSessionScript(w.CreateSessionScript, w.CreateSessionFrameDelay)
	}
	if len(w.ResurrectSetupLines) > 0 || w.ResurrectSetupError != "" {
		d.SetResurrectScript(w.ResurrectSetupLines, w.ResurrectFrameDelay, w.ResurrectSetupError)
	}
	// ListAgents answers empty unless a world seeds it, and boss's agent
	// preflight intersects that inventory with settings.Plugins — so a preset
	// that needs a startup agent check to actually run has to fill both sides.
	if len(w.Agents) > 0 {
		d.SetAgents(w.Agents)
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
	socket := flag.String("socket", "", "attach mode: drive boss against this EXISTING daemon unix socket instead of a bridge-owned mock daemon (requires -home)")
	home := flag.String("home", "", "attach mode: HOME for the boss child; used as-is, never seeded or removed (required with -socket)")
	settings := flag.String("settings", "", "attach mode: optional settings file handed to boss as BOSS_SETTINGS_PATH")
	flag.Parse()

	af := attachFlags{
		socket:     *socket,
		home:       *home,
		settings:   *settings,
		fixtureSet: flagWasSet("fixture"),
		seedSet:    *seedPath != "",
	}
	if err := validateAttachFlags(af); err != nil {
		log.Fatalf("proof-tui-agent: usage: %v", err)
	}
	if af.socket != "" {
		if err := runAttach(af, *width, *height, *bossBin, *castPath, os.Getenv(seedEnvVar)); err != nil {
			log.Fatalf("proof-tui-agent: %v", err)
		}
		return
	}

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
		overrides := mergeSettingsOverrides(preset.SettingsOverrides, proofSettingsSeedOverrides(allowedEnv))
		if err := tuitest.SeedSettingsAcknowledgedWithOverrides(home, worktreeBaseDir, overrides); err != nil {
			return fmt.Errorf("seed settings: %w", err)
		}
	case fixtures.SeedFirstRun:
		// SeedFirstRunSettings writes a fixed unacknowledged settings.json and
		// takes no overrides, and the first-frame wait below is skipped entirely
		// for a first-run preset — so both fields a preset can use to shape the
		// boot are inert on this path. Refuse loudly rather than seed a preset
		// that silently is not the preset it declared: a mis-seeded proof run
		// captures the wrong screen and looks exactly as green as a correct one,
		// which is the failure class this whole ticket exists to remove.
		if err := rejectUnhonouredFirstRunFields(preset); err != nil {
			return err
		}
		if err := tuitest.SeedFirstRunSettings(home, socket); err != nil {
			return fmt.Errorf("seed first-run settings: %w", err)
		}
	}

	// Stage the preset's extra HOME files (a slow .bashrc, a stub binary) after
	// the settings seed and before boss starts, so boss reads them through its
	// ordinary host-state code paths rather than through a test hook.
	if err := writeHomeFiles(home, preset.HomeFiles); err != nil {
		return err
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
	castFile, castWriter, err = openCast(castPath, width, height)
	if err != nil {
		return err
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
		// A preset whose whole point is a slow startup (slow-agent-probe blocks on
		// a login-shell probe until it times out) paints nothing until that wait
		// is over, so it raises the bound rather than racing the default.
		bootWait := preset.BootWait
		if bootWait <= 0 {
			bootWait = fixtures.DefaultBootWait
		}
		if err := drv.WaitForText(bootWait, "Bossanova"); err != nil {
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

// openCast creates the optional .cast capture. An empty castPath is no capture:
// nil file, nil writer, nil error.
func openCast(castPath string, width, height int) (*os.File, *castWriter, error) {
	if castPath == "" {
		return nil, nil, nil
	}
	// #nosec G304 -- operator-supplied -cast output path for a local proof/test harness; not attacker-controlled.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	f, err := os.Create(castPath)
	if err != nil {
		return nil, nil, fmt.Errorf("create cast file: %w", err)
	}
	cw, err := newCastWriter(f, width, height, time.Now)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("new cast writer: %w", err)
	}
	return f, cw, nil
}

// attachFlags are the attach-mode inputs plus whether the fixture-mode seeding
// flags were used, which attach mode refuses rather than silently ignores.
type attachFlags struct {
	socket     string
	home       string
	settings   string
	fixtureSet bool
	seedSet    bool
}

// flagWasSet reports whether name was passed explicitly on the command line —
// -fixture has a non-empty default, so its value alone cannot tell.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// validateAttachFlags enforces the attach-mode usage contract. -socket needs
// -home: the bridge cannot invent a HOME for a daemon it does not own. -home
// and -settings without -socket would be silently ignored by fixture mode, so
// they are refused. -fixture and -seed shape the bridge-owned mock daemon,
// which attach mode never starts, so combining them with -socket is refused
// instead of quietly producing a run that is not the fixture it named.
func validateAttachFlags(f attachFlags) error {
	if f.socket == "" {
		if f.home != "" || f.settings != "" {
			return errors.New("-home and -settings are attach-mode flags and require -socket")
		}
		return nil
	}
	if f.home == "" {
		return errors.New("-socket requires -home (the HOME directory the boss child runs with)")
	}
	if f.fixtureSet {
		return errors.New("-socket and -fixture are mutually exclusive: attach mode drives an existing daemon and seeds no fixture")
	}
	if f.seedSet {
		return errors.New("-socket and -seed are mutually exclusive: attach mode has no bridge-owned mock daemon to seed")
	}
	return nil
}

// attachEnv builds the boss child env for attach mode: the same harness base
// and signed-in e2e env as an acknowledged fixture preset, pointed at the
// caller's socket and HOME, plus BOSS_SETTINGS_PATH when -settings was given
// and the validated scenario env on top.
func attachEnv(environ []string, f attachFlags, allowedEnv map[string]string) []string {
	env := tuitest.BaseHarnessEnv(environ)
	env = append(env,
		"BOSS_SKIP_SKILLS=1",
		"BOSS_SKIP_PROVIDER_STARTUP_DAEMON_RESTART=1",
		"TERM=xterm-256color",
		"HOME="+f.home,
		"BOSS_SOCKET="+f.socket,
	)
	if runtime.GOOS != "darwin" {
		env = append(env, "XDG_CONFIG_HOME="+filepath.Join(f.home, ".config"))
	}
	env = append(env, "BOSS_AUTH_E2E_EMAIL=proof@example.com")
	if f.settings != "" {
		env = append(env, "BOSS_SETTINGS_PATH="+f.settings)
	}
	for _, k := range sortedKeys(allowedEnv) {
		env = append(env, k+"="+allowedEnv[k])
	}
	return env
}

// runAttach drives the e2e-tagged boss TUI against an EXISTING daemon socket.
// It starts no mock daemon, seeds no settings or world, and never removes the
// caller's HOME or socket; cast capture, settle and the NDJSON ops are the
// fixture-mode ones, except that the "daemon" op is refused (see serveAttached).
func runAttach(f attachFlags, width, height int, bossBin, castPath, seedEnvJSON string) error {
	allowedEnv, err := validatedSeedEnv(seedEnvJSON)
	if err != nil {
		return err
	}
	if fi, serr := os.Stat(f.home); serr != nil || !fi.IsDir() {
		return fmt.Errorf("-home %q must be an existing directory", f.home)
	}
	if _, serr := os.Stat(f.socket); serr != nil {
		return fmt.Errorf("-socket %q: %w", f.socket, serr)
	}
	env := attachEnv(os.Environ(), f, allowedEnv)

	if bossBin == "" {
		built, cleanupBoss, berr := buildBoss()
		if berr != nil {
			return berr
		}
		defer cleanupBoss()
		bossBin = built
	}

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
			if castFile != nil {
				_ = castFile.Close()
			}
		})
	}
	defer cleanup()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cleanup()
		os.Exit(0)
	}()

	castFile, cw, err := openCast(castPath, width, height)
	if err != nil {
		return err
	}
	driverOpts := tuidriver.Options{Command: bossBin, Env: env, Width: width, Height: height}
	if cw != nil {
		driverOpts.RawOutput = cw
	}
	drv, err = tuidriver.New(driverOpts)
	if err != nil {
		return fmt.Errorf("create TUI driver: %w", err)
	}
	if err := drv.WaitForText(fixtures.DefaultBootWait, "Bossanova"); err != nil {
		return fmt.Errorf("wait for app shell: %w", err)
	}

	if err := serveAttached(drv, os.Stdin, os.Stdout, defaultSettle(width, height)); err != nil && !errors.Is(err, errQuit) {
		return err
	}
	return nil
}

// attachDaemonOpError answers a "daemon" op in attach mode: the mutators and
// pushers drive a bridge-owned mock, and an attached daemon is not one.
const attachDaemonOpError = "daemon op unavailable in attach mode (-socket): the bridge does not own the daemon it is attached to"

// serveAttached is serve for attach mode: identical NDJSON handling, except a
// "daemon" op is answered with attachDaemonOpError (never a nil-daemon panic or
// a silent no-op) and capabilities stops advertising the daemon surface.
func serveAttached(t tui, in io.Reader, out io.Writer, st settleConfig) error {
	bw := bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if resp, handled := attachModeResponse(line); handled {
			if err := writeJSON(bw, resp); err != nil {
				return err
			}
			continue
		}
		// No daemon reaches handleLine: attachModeResponse claims every op
		// that would touch it.
		quit, err := handleLine(t, nil, bw, line, st)
		if err != nil {
			return err
		}
		if quit {
			return errQuit
		}
	}
	if err := scanner.Err(); err != nil {
		_ = writeJSON(bw, errorResponse(0, err.Error()))
	}
	return nil
}

// attachModeResponse answers the ops whose fixture-mode handling assumes a
// bridge-owned daemon. Malformed lines are left to handleLine's error path.
func attachModeResponse(line []byte) (map[string]any, bool) {
	var req struct {
		ID int64  `json:"id"`
		Op string `json:"op"`
	}
	if json.Unmarshal(line, &req) != nil {
		return nil, false
	}
	switch req.Op {
	case "daemon":
		return errorResponse(req.ID, attachDaemonOpError), true
	case "capabilities":
		resp := capabilitiesResponse(req.ID)
		ops, _ := resp["ops"].([]string)
		resp["ops"] = slices.DeleteFunc(ops, func(op string) bool { return op == "daemon" })
		resp["daemonActions"] = []string{}
		resp["mode"] = "attach"
		return resp, true
	}
	return nil, false
}

// readSeedOverlay reads and parses the -seed overlay file, capping the read at
// maxSeedBytes. Parsing uses fixtures.ParseOverlay (DisallowUnknownFields), so a
// typo'd field aborts boot naming the field.
func readSeedOverlay(path string) (fixtures.Overlay, error) {
	// #nosec G304 -- operator-supplied -seed overlay path for a local proof/test harness; read is capped at maxSeedBytes.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
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

// rejectUnhonouredFirstRunFields fails a first-run preset that declares preset
// fields this seed path cannot honour, naming the field and the preset so the
// author is told what to do rather than left reading a screen that quietly does
// not match the preset.
//
// Nothing declares either field on a first-run preset today. When something
// needs to, the fix is to teach the bridge to honour it here — not to delete
// this guard.
func rejectUnhonouredFirstRunFields(preset fixtures.Preset) error {
	if len(preset.SettingsOverrides) > 0 {
		return fmt.Errorf(
			"preset declares SettingsOverrides %v, but its SeedFirstRun path seeds a fixed "+
				"settings.json that ignores them; teach the bridge to honour them or use a "+
				"SeedAcknowledged preset",
			sortedAnyKeys(preset.SettingsOverrides))
	}
	if preset.BootWait > 0 {
		return fmt.Errorf(
			"preset declares BootWait %s, but a SeedFirstRun preset never waits for the app "+
				"shell (first-run renders no banner), so the value is dropped",
			preset.BootWait)
	}
	return nil
}

// sortedAnyKeys is sortedKeys for a map[string]any — the settings-override
// shape — so an error message names the offending keys in a stable order.
func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// mergeSettingsOverrides layers the scenario's env-driven settings overrides on
// top of the preset's. Both are merged into the same settings.json, and the
// env-driven layer wins on a collision: a preset default is the baseline a
// scenario is allowed to move, not a floor it cannot.
func mergeSettingsOverrides(presetOverrides map[string]any, envOverrides map[string]any) map[string]any {
	merged := make(map[string]any, len(presetOverrides)+len(envOverrides))
	for k, v := range presetOverrides {
		merged[k] = v
	}
	for k, v := range envOverrides {
		merged[k] = v
	}
	return merged
}

// writeHomeFiles materializes a preset's HomeFiles inside the per-run HOME.
// Every key must be a plain relative path: the preset registry is ordinary repo
// data, but this writes to disk, so an absolute or parent-escaping key is
// rejected rather than trusted — otherwise a typo'd preset could overwrite the
// developer's real dotfiles instead of the throwaway home.
func writeHomeFiles(home string, files map[string]string) error {
	// Sorted so a failure names the same offending key on every run.
	for _, rel := range sortedKeys(files) {
		content := files[rel]
		if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			return fmt.Errorf("preset HomeFiles key %q must be a plain relative path inside HOME", rel)
		}
		dest := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return fmt.Errorf("create HOME dir for %q: %w", rel, err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write HOME file %q: %w", rel, err)
		}
	}
	return nil
}

func proofSettingsSeedOverrides(allowedEnv map[string]string) map[string]any {
	overrides := map[string]any{}
	if allowedEnv[proofSettingsEventTracingEnv] == "1" {
		overrides["event_tracing_enabled"] = true
	}
	if host := strings.TrimSpace(allowedEnv[proofSettingsPostHogHostEnv]); host != "" {
		overrides["posthog_host"] = host
	}
	return overrides
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
		return nil, fmt.Errorf("%s: rejected non-whitelisted env key(s): %s (allowed prefixes: %s; allowed exact keys: %s)",
			seedEnvVar, strings.Join(rejected, ", "), strings.Join(tuitest.ProofEnvWhitelist, ", "), strings.Join(tuitest.ProofEnvAllowedKeys, ", "))
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
	// #nosec G204 -- test-only e2e build; constant argv except binPath (a mktemp path); no shell.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	cmd := exec.Command("go", "build", "-tags", "e2e", "-o", binPath, "./cmd")
	cmd.Dir = serviceDir()
	out, berr := cmd.CombinedOutput()
	if berr != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("build boss: %w\n%s", berr, out)
	}
	return binPath, func() { _ = os.RemoveAll(dir) }, nil
}
