package upgrade

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/config"
)

func TestRunUpgradeSkipsInvalidCurrentVersion(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.upgradeCurrentVersion = func() string { return "dev" }
	checkCalled := false
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		checkCalled = true
		return CheckResult{}, nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{CheckOnly: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if checkCalled {
		t.Fatal("g.Run() called checker for invalid current version")
	}
	if strings.Contains(out.String(), "boss is up to date") {
		t.Fatalf("g.Run() output = %q, contains misleading up-to-date message", out.String())
	}
}

func TestRunUpgradeYesInstallsWithResolvedExecutableDir(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	appDataDir := filepath.Join(dir, "appdata")
	settings := config.DefaultSettings()
	settings.AppDataDir = appDataDir
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	realDir := filepath.Join(dir, "real")
	wrapperDir := filepath.Join(dir, "wrapper")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(wrapperDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(realDir, "boss")
	if err := os.WriteFile(realExe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkExe := filepath.Join(wrapperDir, "boss")
	if err := os.Symlink(realExe, linkExe); err != nil {
		t.Fatal(err)
	}
	resolvedRealDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}

	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
			ReleaseURL:     "https://example.test/stable",
		}, nil
	}
	g.executablePath = func() (string, error) { return linkExe, nil }
	g.loadSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	g.saveSettings = func(config.Settings) error { return nil }
	g.discoverPlugins = config.DiscoverPlugins
	var gotPlan InstallPlan
	g.installUpgrade = func(_ context.Context, plan InstallPlan) error {
		gotPlan = plan
		return nil
	}
	restartCalled := false
	g.restartDaemonAfterUpgrade = func() error {
		restartCalled = true
		return nil
	}
	t.Setenv("HOME", dir)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err = g.Run(cmd, Options{Yes: true})
	if err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "installing v1.2.4 assets into "+resolvedRealDir) {
		t.Fatalf("g.Run() output = %q, want resolved executable dir %q", out.String(), resolvedRealDir)
	}
	if strings.Contains(out.String(), wrapperDir) {
		t.Fatalf("g.Run() output = %q, used wrapper dir %q", out.String(), wrapperDir)
	}
	if gotPlan.BinDir != resolvedRealDir {
		t.Fatalf("InstallPlan.BinDir = %q, want %q", gotPlan.BinDir, resolvedRealDir)
	}
	wantPluginDir, err := defaultPluginDir(runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	if gotPlan.PluginDir != wantPluginDir {
		t.Fatalf("InstallPlan.PluginDir = %q, want %q", gotPlan.PluginDir, wantPluginDir)
	}
	if strings.Contains(gotPlan.PluginDir, string(filepath.Separator)+"bin") {
		t.Fatalf("InstallPlan.PluginDir = %q, want user plugin dir when cwd dev plugins exist", gotPlan.PluginDir)
	}
	if !strings.Contains(out.String(), "upgrade installed v1.2.4") {
		t.Fatalf("g.Run() output = %q, want success message", out.String())
	}
	if !restartCalled {
		t.Fatal("g.restartDaemonAfterUpgrade was not called")
	}
	if !strings.Contains(out.String(), "daemon restarted") {
		t.Fatalf("g.Run() output = %q, want daemon restarted message", out.String())
	}
}

func TestRunUpgradeExplicitVersionInstallsWithoutCheckingLatest(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	g.upgradeCurrentVersion = func() string { return "dev" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for explicit version")
		return CheckResult{}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	t.Setenv("HOME", dir)

	var gotPlan InstallPlan
	g.installUpgrade = func(_ context.Context, plan InstallPlan) error {
		gotPlan = plan
		return nil
	}
	g.restartDaemonAfterUpgrade = func() error { return nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{Yes: true, Version: "1.2.4"}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if gotPlan.Version != "v1.2.4" {
		t.Fatalf("InstallPlan.Version = %q, want v1.2.4", gotPlan.Version)
	}
	if gotPlan.ReleaseURL != "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.4" {
		t.Fatalf("InstallPlan.ReleaseURL = %q, want trusted v1.2.4 URL", gotPlan.ReleaseURL)
	}
	if !strings.Contains(out.String(), "upgrade installed v1.2.4") {
		t.Fatalf("g.Run() output = %q, want success message", out.String())
	}
}

func TestRunUpgradeHomebrewRoutesToBrewUpgrade(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testHomebrewExecutable(t, dir)
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.installUpgrade = func(context.Context, InstallPlan) error {
		t.Fatal("g.installUpgrade called for Homebrew install")
		return nil
	}
	brewCalled := false
	pluginDir := filepath.Join(dir, "opt", "homebrew", "opt", "bossanova", "libexec", "plugins")
	g.brewUpgradeBossanova = func(context.Context, string) (string, error) {
		brewCalled = true
		return pluginDir, nil
	}
	restartCalled := false
	g.restartHomebrewDaemon = func(_ context.Context, path string) error {
		if want := filepath.Join(dir, "opt", "homebrew", "opt", "bossanova", "bin", "boss"); path != want {
			t.Fatalf("post-upgrade executable = %q, want %q", path, want)
		}
		restartCalled = true
		return nil
	}
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := g.Run(cmd, Options{Yes: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if !brewCalled {
		t.Fatal("g.brewUpgradeBossanova was not called")
	}
	if !restartCalled {
		t.Fatal("g.restartDaemonAfterUpgrade was not called")
	}
	if strings.Contains(out.String(), "assets into") || strings.Contains(out.String(), "Cellar") {
		t.Fatalf("g.Run() output = %q, want no direct Cellar asset install", out.String())
	}
	if !strings.Contains(out.String(), "Homebrew upgrade completed") || !strings.Contains(out.String(), "daemon restarted") {
		t.Fatalf("g.Run() output = %q, want Homebrew success and restart", out.String())
	}
}

func TestRunUpgradeHomebrewPersistsPostUpgradePluginPaths(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	oldPluginDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.3", "libexec", "plugins")
	newPluginDir := filepath.Join(dir, "opt", "homebrew", "opt", "bossanova", "libexec", "plugins")
	g.executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	g.installUpgrade = func(context.Context, InstallPlan) error {
		t.Fatal("g.installUpgrade called for Homebrew install")
		return nil
	}
	g.brewUpgradeBossanova = func(context.Context, string) (string, error) {
		return newPluginDir, nil
	}
	g.restartHomebrewDaemon = func(context.Context, string) error { return nil }
	g.loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(oldPluginDir, "bossd-plugin-claude"), Enabled: false},
				{Name: "linear", Path: filepath.Join(oldPluginDir, "bossd-plugin-linear"), Enabled: true},
			},
		}, nil
	}
	var saved config.Settings
	saveCount := 0
	g.saveSettings = func(s config.Settings) error {
		saveCount++
		saved = s
		return nil
	}
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := g.Run(cmd, Options{Yes: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if saveCount != 1 {
		t.Fatalf("g.saveSettings called %d times, want 1", saveCount)
	}
	assertPluginSaved(t, saved, "claude", filepath.Join(newPluginDir, "bossd-plugin-claude"), false)
	assertPluginSaved(t, saved, "codex", filepath.Join(newPluginDir, "bossd-plugin-codex"), true)
	assertPluginSaved(t, saved, "linear", filepath.Join(newPluginDir, "bossd-plugin-linear"), true)
}

func TestRunUpgradeHomebrewNoRestartSkipsDaemonRestart(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	g.executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	g.brewUpgradeBossanova = func(context.Context, string) (string, error) { return "", nil }
	g.restartDaemonAfterUpgrade = func() error {
		t.Fatal("g.restartDaemonAfterUpgrade called with --no-restart")
		return nil
	}
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := g.Run(cmd, Options{Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "daemon restart skipped") {
		t.Fatalf("g.Run() output = %q, want no-restart message", out.String())
	}
}

func TestRunUpgradeHomebrewBrewFailureIsActionable(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	g.executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	g.brewUpgradeBossanova = func(context.Context, string) (string, error) {
		return "", errors.New("brew upgrade bossanova-dev/tap/bossanova failed: exit status 1\nRun manually: brew upgrade bossanova-dev/tap/bossanova")
	}
	g.restartDaemonAfterUpgrade = func() error {
		t.Fatal("g.restartDaemonAfterUpgrade called after brew failure")
		return nil
	}
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }

	err := g.Run(&cobra.Command{}, Options{Yes: true})
	if err == nil {
		t.Fatal("g.Run() error = nil, want brew failure")
	}
	if !strings.Contains(err.Error(), "brew upgrade bossanova-dev/tap/bossanova failed") ||
		!strings.Contains(err.Error(), "Run manually: brew upgrade bossanova-dev/tap/bossanova") {
		t.Fatalf("g.Run() error = %v, want failed command and manual command", err)
	}
}

func TestBrewUpgradeBossanovaUsesDetectedHomebrewBrewOutsidePath(t *testing.T) {
	g := NewRunner(func() error { return nil })
	dir := t.TempDir()
	exe := testHomebrewExecutable(t, dir)
	binDir := filepath.Dir(exe)
	brewDir := filepath.Join(dir, "opt", "homebrew", "bin")
	if err := os.MkdirAll(brewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brewLog := filepath.Join(dir, "brew.log")
	prefix := filepath.Join(dir, "opt", "homebrew", "opt", "bossanova")
	brew := filepath.Join(brewDir, "brew")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BREW_LOG"
case "$1" in
upgrade)
	exit 0
	;;
--prefix)
	printf '%s\n' "$BREW_PREFIX"
	exit 0
	;;
*)
	exit 2
	;;
esac
`
	if err := os.WriteFile(brew, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BREW_LOG", brewLog)
	t.Setenv("BREW_PREFIX", prefix)
	t.Setenv("PATH", filepath.Join(dir, "empty-path"))

	pluginDir, err := g.brewUpgradeBossanova(context.Background(), binDir)
	if err != nil {
		t.Fatalf("g.brewUpgradeBossanova() error = %v", err)
	}
	if pluginDir != filepath.Join(prefix, "libexec", "plugins") {
		t.Fatalf("g.brewUpgradeBossanova() pluginDir = %q, want prefix libexec plugins", pluginDir)
	}
	log, err := os.ReadFile(brewLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "upgrade bossanova-dev/tap/bossanova") ||
		!strings.Contains(string(log), "--prefix bossanova-dev/tap/bossanova") {
		t.Fatalf("brew log = %q, want upgrade and --prefix commands", string(log))
	}
}

func TestRunUpgradeHomebrewExplicitVersionErrors(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	g.upgradeCurrentVersion = func() string { return "dev" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for explicit version")
		return CheckResult{}, nil
	}
	g.executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	g.installUpgrade = func(context.Context, InstallPlan) error {
		t.Fatal("g.installUpgrade called for Homebrew --version")
		return nil
	}
	g.brewUpgradeBossanova = func(context.Context, string) (string, error) {
		t.Fatal("g.brewUpgradeBossanova called for Homebrew --version")
		return "", nil
	}

	err := g.Run(&cobra.Command{}, Options{Yes: true, Version: "v1.2.4"})
	if err == nil {
		t.Fatal("g.Run() error = nil, want Homebrew --version error")
	}
	if !strings.Contains(err.Error(), "exact --version installs are not supported") ||
		!strings.Contains(err.Error(), "brew upgrade bossanova-dev/tap/bossanova") {
		t.Fatalf("g.Run() error = %v, want Homebrew version guidance", err)
	}
}

func TestRunUpgradeInvalidExplicitVersionErrors(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for invalid explicit version")
		return CheckResult{}, nil
	}
	g.installUpgrade = func(context.Context, InstallPlan) error {
		t.Fatal("g.installUpgrade called for invalid explicit version")
		return nil
	}

	err := g.Run(&cobra.Command{}, Options{Yes: true, Version: "dev"})
	if err == nil {
		t.Fatal("g.Run() error = nil, want invalid version error")
	}
	if !strings.Contains(err.Error(), "invalid upgrade version") {
		t.Fatalf("g.Run() error = %v, want invalid version error", err)
	}
}

func TestRunUpgradeCheckVersionVerifiesReleaseExists(t *testing.T) {
	g := NewRunner(func() error { return nil })

	called := false
	gotVersion := ""
	g.verifyUpgradeVersion = func(_ context.Context, version string) error {
		called = true
		gotVersion = version
		return nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{CheckOnly: true, Version: "1.2.4"}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if !called {
		t.Fatal("g.verifyUpgradeVersion was not called for --check --version")
	}
	if gotVersion != "v1.2.4" {
		t.Fatalf("g.verifyUpgradeVersion got %q, want v1.2.4", gotVersion)
	}
	if !strings.Contains(out.String(), "release v1.2.4 exists") {
		t.Fatalf("g.Run() output = %q, want existence confirmation", out.String())
	}
}

func TestRunUpgradeCheckVersionPropagatesNotFound(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.verifyUpgradeVersion = func(context.Context, string) error {
		return errors.New("release v9.9.9 not found")
	}

	err := g.Run(&cobra.Command{}, Options{CheckOnly: true, Version: "9.9.9"})
	if err == nil {
		t.Fatal("g.Run() error = nil, want not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("g.Run() error = %v, want not-found error", err)
	}
}

func TestRunUpgradePrereleaseExplicitVersionErrors(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for prerelease explicit version")
		return CheckResult{}, nil
	}
	g.installUpgrade = func(context.Context, InstallPlan) error {
		t.Fatal("g.installUpgrade called for prerelease explicit version")
		return nil
	}

	err := g.Run(&cobra.Command{}, Options{Yes: true, Version: "v1.2.3-beta.1"})
	if err == nil {
		t.Fatal("g.Run() error = nil, want prerelease version error")
	}
	if !strings.Contains(err.Error(), "prerelease upgrade version") {
		t.Fatalf("g.Run() error = %v, want prerelease version error", err)
	}
}

func TestRunUpgradeNoRestartChangesOutput(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
		}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.installUpgrade = func(context.Context, InstallPlan) error { return nil }
	g.restartDaemonAfterUpgrade = func() error {
		t.Fatal("g.restartDaemonAfterUpgrade called with --no-restart")
		return nil
	}
	t.Setenv("HOME", dir)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if !strings.Contains(out.String(), "daemon restart skipped") {
		t.Fatalf("g.Run() output = %q, want no-restart message", out.String())
	}
}

func TestRunUpgradeReportsRestartError(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	g.upgradeActionCachePath = func() string { return "" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
		}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.installUpgrade = func(context.Context, InstallPlan) error { return nil }
	g.restartDaemonAfterUpgrade = func() error { return errors.New("restart failed") }
	t.Setenv("HOME", dir)

	err := g.Run(&cobra.Command{}, Options{Yes: true})
	if err == nil {
		t.Fatal("g.Run() error = nil, want restart error")
	}
	if !strings.Contains(err.Error(), "restart daemon") || !strings.Contains(err.Error(), "restart failed") {
		t.Fatalf("g.Run() error = %v, want restart daemon error", err)
	}
}

func TestCurrentExecutableDirReturnsExecutablePathError(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.executablePath = func() (string, error) { return "", errors.New("boom") }
	if _, err := g.currentExecutableDir(); err == nil {
		t.Fatal("g.currentExecutableDir() error = nil, want error")
	}
}

func TestRunUpgradeInstallsPluginsIntoConfiguredPluginDir(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	configuredPluginDir := filepath.Join(dir, "libexec", "plugins")
	discoveredPluginDir := filepath.Join(dir, "user", "plugins")
	if err := os.MkdirAll(configuredPluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(discoveredPluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	g.upgradeCurrentVersion = func() string { return "dev" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for explicit version")
		return CheckResult{}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{{
				Name:    "codex",
				Path:    filepath.Join(configuredPluginDir, "bossd-plugin-codex"),
				Enabled: true,
			}},
		}, nil
	}
	g.discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "codex",
			Path:    filepath.Join(discoveredPluginDir, "bossd-plugin-codex"),
			Enabled: true,
		}}
	}
	var gotPlan InstallPlan
	g.installUpgrade = func(_ context.Context, plan InstallPlan) error {
		gotPlan = plan
		return nil
	}
	g.restartDaemonAfterUpgrade = func() error { return nil }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := g.Run(cmd, Options{Version: "v1.2.4", Yes: true}); err != nil {
		t.Fatalf("g.Run(): %v", err)
	}
	if gotPlan.PluginDir != configuredPluginDir {
		t.Fatalf("InstallPlan.PluginDir = %q, want configured dir %q", gotPlan.PluginDir, configuredPluginDir)
	}
}

func TestRunUpgradeInstallsPluginsIntoDiscoveredPluginDir(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	discoveredPluginDir := filepath.Join(dir, "libexec", "plugins")
	if err := os.MkdirAll(discoveredPluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	g.upgradeCurrentVersion = func() string { return "dev" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for explicit version")
		return CheckResult{}, nil
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.loadSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	g.discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "codex",
			Path:    filepath.Join(discoveredPluginDir, "bossd-plugin-codex"),
			Enabled: true,
		}}
	}
	var gotPlan InstallPlan
	g.installUpgrade = func(_ context.Context, plan InstallPlan) error {
		gotPlan = plan
		return nil
	}
	g.restartDaemonAfterUpgrade = func() error { return nil }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := g.Run(cmd, Options{Version: "v1.2.4", Yes: true}); err != nil {
		t.Fatalf("g.Run(): %v", err)
	}
	if gotPlan.PluginDir != discoveredPluginDir {
		t.Fatalf("InstallPlan.PluginDir = %q, want discovered dir %q", gotPlan.PluginDir, discoveredPluginDir)
	}
}

func TestUpgradePluginDirRepairsMixedHomebrewCellarPluginDirs(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	binDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.4", "bin")
	currentPluginDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.4", "libexec", "plugins")
	g.loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.2", "libexec", "plugins", "bossd-plugin-claude"), Enabled: true},
				{Name: "codex", Path: filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.3", "libexec", "plugins", "bossd-plugin-codex"), Enabled: true},
			},
		}, nil
	}
	g.discoverPlugins = func() []config.PluginConfig { return nil }

	got, err := g.upgradePluginDir(runtime.GOOS, binDir)
	if err != nil {
		t.Fatalf("g.upgradePluginDir() error = %v", err)
	}
	if got != currentPluginDir {
		t.Fatalf("g.upgradePluginDir() = %q, want %q", got, currentPluginDir)
	}
}

func TestUpgradePluginDirRejectsMixedCustomPluginDirs(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	g.loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(dir, "custom-a", "bossd-plugin-claude"), Enabled: true},
				{Name: "codex", Path: filepath.Join(dir, "custom-b", "bossd-plugin-codex"), Enabled: true},
			},
		}, nil
	}
	g.discoverPlugins = func() []config.PluginConfig { return nil }

	_, err := g.upgradePluginDir(runtime.GOOS, filepath.Join(dir, "bin"))
	if err == nil {
		t.Fatal("g.upgradePluginDir() error = nil, want mixed custom plugin dir error")
	}
	if !strings.Contains(err.Error(), "span multiple directories") {
		t.Fatalf("g.upgradePluginDir() error = %v, want mixed directory error", err)
	}
}

func TestRunUpgradeRewritesInstalledPluginPathsPreservingEnabledState(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	exe := filepath.Join(binDir, "boss")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPluginDir := filepath.Join(dir, "old-plugins")
	currentPluginDir := filepath.Join(dir, "new-plugins")
	settings := config.Settings{
		Plugins: []config.PluginConfig{
			{Name: "claude", Path: filepath.Join(oldPluginDir, "bossd-plugin-claude"), Enabled: false, Config: map[string]string{"k": "v"}},
			{Name: "codex", Path: filepath.Join(oldPluginDir, "bossd-plugin-codex"), Enabled: false},
			{Name: "linear", Path: filepath.Join(currentPluginDir, "bossd-plugin-linear"), Enabled: true},
		},
	}
	g.loadSettings = func() (config.Settings, error) { return settings, nil }
	g.discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "claude",
			Path:    filepath.Join(currentPluginDir, "bossd-plugin-claude"),
			Enabled: true,
		}}
	}
	g.executablePath = func() (string, error) { return exe, nil }
	g.upgradeCurrentVersion = func() string { return "dev" }
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		t.Fatal("g.checkUpgrade called for explicit version")
		return CheckResult{}, nil
	}
	g.installUpgrade = func(context.Context, InstallPlan) error { return nil }
	g.restartDaemonAfterUpgrade = func() error {
		t.Fatal("g.restartDaemonAfterUpgrade called with --no-restart")
		return nil
	}
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }
	saveCount := 0
	var saved config.Settings
	g.saveSettings = func(s config.Settings) error {
		saveCount++
		saved = s
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := g.Run(cmd, Options{Version: "v1.2.4", Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("g.Run(): %v", err)
	}
	if saveCount != 1 {
		t.Fatalf("g.saveSettings called %d times, want 1", saveCount)
	}
	assertPluginSaved(t, saved, "claude", filepath.Join(currentPluginDir, "bossd-plugin-claude"), false)
	assertPluginSaved(t, saved, "codex", filepath.Join(currentPluginDir, "bossd-plugin-codex"), false)
	assertPluginSaved(t, saved, "dependabot", filepath.Join(currentPluginDir, "bossd-plugin-dependabot"), true)
	assertPluginSaved(t, saved, "linear", filepath.Join(currentPluginDir, "bossd-plugin-linear"), true)
	assertPluginSaved(t, saved, "repair", filepath.Join(currentPluginDir, "bossd-plugin-repair"), true)
	if saved.Plugins[0].Config["k"] != "v" {
		t.Fatalf("claude config = %v, want preserved config", saved.Plugins[0].Config)
	}
}

func TestWithUpgradeLockRejectsConcurrentAttempt(t *testing.T) {
	g := NewRunner(func() error { return nil })

	dir := t.TempDir()
	g.upgradeLockPath = func() (string, error) { return filepath.Join(dir, "lock"), nil }
	innerCalled := false
	err := g.withUpgradeLock(func() error {
		innerErr := g.withUpgradeLock(func() error {
			innerCalled = true
			return nil
		})
		if innerErr == nil {
			t.Fatal("inner g.withUpgradeLock() error = nil, want concurrent lock rejection")
		}
		if !strings.Contains(innerErr.Error(), "already in progress") {
			t.Fatalf("inner g.withUpgradeLock() error = %v, want already in progress", innerErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("g.withUpgradeLock() error = %v", err)
	}
	if innerCalled {
		t.Fatal("inner lock body ran")
	}
}

func assertPluginSaved(t *testing.T, settings config.Settings, name, path string, enabled bool) {
	t.Helper()
	for _, plugin := range settings.Plugins {
		if plugin.Name != name {
			continue
		}
		if plugin.Path != path || plugin.Enabled != enabled {
			t.Fatalf("plugin %s = %+v, want path %q enabled %v", name, plugin, path, enabled)
		}
		return
	}
	t.Fatalf("plugin %s not saved in %+v", name, settings.Plugins)
}

func testExecutable(t *testing.T, dir string) string {
	t.Helper()

	exeDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "boss")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func testHomebrewExecutable(t *testing.T, dir string) string {
	t.Helper()

	exeDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.3", "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "boss")
	if err := os.WriteFile(exe, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func stubUpgradeCache(t *testing.T, g *Runner, check func(context.Context, string) (CheckResult, error), entry CacheEntry) string {
	t.Helper()

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	if err := WriteCache(cachePath, entry); err != nil {
		t.Fatal(err)
	}
	g.upgradeCurrentVersion = func() string { return entry.CurrentVersion }
	g.upgradeActionCachePath = func() string { return cachePath }
	g.checkUpgrade = check
	return cachePath
}

func TestRunUpgradeChecksFreshDespiteFreshUpToDateCache(t *testing.T) {
	g := NewRunner(func() error { return nil })
	checkCalls := 0
	cachePath := stubUpgradeCache(t, g, func(context.Context, string) (CheckResult, error) {
		checkCalls++
		return CheckResult{
			CurrentVersion: "v1.146.2",
			LatestVersion:  "v1.147.0",
			ReleaseURL:     "https://example.test/v1.147.0",
			Available:      true,
		}, nil
	}, CacheEntry{
		CheckedAt:      time.Now().Add(-time.Hour),
		CurrentVersion: "v1.146.2",
		LatestVersion:  "v1.146.2",
		ReleaseURL:     "https://example.test/v1.146.2",
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	beforeCheck := time.Now()
	if err := g.Run(cmd, Options{CheckOnly: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if checkCalls != 1 {
		t.Fatalf("g.checkUpgrade calls = %d, want 1: an explicit upgrade must query GitHub even with a fresh cache", checkCalls)
	}
	if got := out.String(); !strings.Contains(got, "upgrade available: v1.146.2 -> v1.147.0") || strings.Contains(got, "up to date") {
		t.Fatalf("g.Run() output = %q, want the fresh v1.147.0 result", got)
	}
	entry, ok, err := ReadCache(cachePath)
	if err != nil || !ok {
		t.Fatalf("ReadCache() = (_, %v, %v), want the refreshed entry", ok, err)
	}
	if entry.LatestVersion != "v1.147.0" || entry.CheckedAt.Before(beforeCheck) {
		t.Fatalf("cache after explicit check = %+v, want LatestVersion v1.147.0 checked at or after %v", entry, beforeCheck)
	}
}

func TestRunUpgradeSnoozeNeverSuppressesExplicitCheck(t *testing.T) {
	g := NewRunner(func() error { return nil })
	snoozedUntil := time.Now().Add(DefaultSnoozeDuration).Truncate(time.Second)
	cachePath := stubUpgradeCache(t, g, func(context.Context, string) (CheckResult, error) {
		return CheckResult{
			CurrentVersion: "v1.146.2",
			LatestVersion:  "v1.147.0",
			ReleaseURL:     "https://example.test/v1.147.0",
			Available:      true,
		}, nil
	}, CacheEntry{
		CheckedAt:      time.Now().Add(-time.Hour),
		CurrentVersion: "v1.146.2",
		LatestVersion:  "v1.147.0",
		ReleaseURL:     "https://example.test/v1.147.0",
		SnoozedVersion: "v1.147.0",
		SnoozedUntil:   snoozedUntil,
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{CheckOnly: true}); err != nil {
		t.Fatalf("g.Run() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "upgrade available: v1.146.2 -> v1.147.0") || strings.Contains(got, "up to date") {
		t.Fatalf("g.Run() output = %q, want the snoozed v1.147.0 still reported as available", got)
	}
	entry, ok, err := ReadCache(cachePath)
	if err != nil || !ok {
		t.Fatalf("ReadCache() = (_, %v, %v), want the refreshed entry", ok, err)
	}
	if entry.SnoozedVersion != "v1.147.0" || !entry.SnoozedUntil.Equal(snoozedUntil) {
		t.Fatalf("cache after explicit check = %+v, want the v1.147.0 snooze until %v kept", entry, snoozedUntil)
	}
}

func TestRunUpgradeRateLimitFallsBackToFreshCache(t *testing.T) {
	g := NewRunner(func() error { return nil })
	stubUpgradeCache(t, g, func(context.Context, string) (CheckResult, error) {
		return CheckResult{}, &RateLimitError{Resets: time.Now().Add(30 * time.Minute)}
	}, CacheEntry{
		CheckedAt:      time.Now().Add(-time.Hour),
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/v1.2.4",
	})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{CheckOnly: true}); err != nil {
		t.Fatalf("g.Run() error = %v, want nil", err)
	}
	for _, want := range []string{"rate limit", "using the result cached at", "upgrade available: v1.2.3 -> v1.2.4"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("g.Run() output = %q, want containing %q", out.String(), want)
		}
	}
}

func TestRunUpgradeRateLimitPrintsFriendlyMessage(t *testing.T) {
	g := NewRunner(func() error { return nil })

	g.upgradeCurrentVersion = func() string { return "v1.2.3" }
	g.upgradeActionCachePath = func() string { return "" } // force the network path
	g.checkUpgrade = func(context.Context, string) (CheckResult, error) {
		return CheckResult{}, &RateLimitError{Resets: time.Now().Add(30 * time.Minute)}
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := g.Run(cmd, Options{CheckOnly: true}); err != nil {
		t.Fatalf("g.Run() error = %v, want nil (friendly message, not fatal)", err)
	}
	for _, want := range []string{"rate limit", "resets at", "gh auth login"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("g.Run() output = %q, want containing %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "HTTP 403") {
		t.Fatalf("g.Run() output = %q, should not contain raw HTTP 403", out.String())
	}
}

// TestMain preserves the settings isolation used by these tests before the move.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-upgrade-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
