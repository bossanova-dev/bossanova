package main

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

	"github.com/recurser/boss/internal/upgrade"
	"github.com/recurser/bossalib/config"
)

func TestRunUpgradeSkipsInvalidCurrentVersion(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
	}()

	upgradeCurrentVersion = func() string { return "dev" }
	checkCalled := false
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		checkCalled = true
		return upgrade.CheckResult{}, nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{CheckOnly: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if checkCalled {
		t.Fatal("runUpgrade() called checker for invalid current version")
	}
	if strings.Contains(out.String(), "boss is up to date") {
		t.Fatalf("runUpgrade() output = %q, contains misleading up-to-date message", out.String())
	}
}

func TestRunUpgradeYesInstallsWithResolvedExecutableDir(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	oldDiscoverPlugins := discoverPlugins
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
		discoverPlugins = oldDiscoverPlugins
	}()

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

	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
			ReleaseURL:     "https://example.test/stable",
		}, nil
	}
	executablePath = func() (string, error) { return linkExe, nil }
	loadSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	saveSettings = func(config.Settings) error { return nil }
	discoverPlugins = config.DiscoverPlugins
	var gotPlan upgrade.InstallPlan
	installUpgrade = func(_ context.Context, plan upgrade.InstallPlan) error {
		gotPlan = plan
		return nil
	}
	restartCalled := false
	restartDaemon = func() error {
		restartCalled = true
		return nil
	}
	t.Setenv("HOME", dir)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err = runUpgrade(cmd, upgradeOptions{Yes: true})
	if err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !strings.Contains(out.String(), "installing v1.2.4 assets into "+resolvedRealDir) {
		t.Fatalf("runUpgrade() output = %q, want resolved executable dir %q", out.String(), resolvedRealDir)
	}
	if strings.Contains(out.String(), wrapperDir) {
		t.Fatalf("runUpgrade() output = %q, used wrapper dir %q", out.String(), wrapperDir)
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
		t.Fatalf("runUpgrade() output = %q, want success message", out.String())
	}
	if !restartCalled {
		t.Fatal("restartDaemon was not called")
	}
	if !strings.Contains(out.String(), "daemon restarted") {
		t.Fatalf("runUpgrade() output = %q, want daemon restarted message", out.String())
	}
}

func TestRunUpgradeExplicitVersionInstallsWithoutCheckingLatest(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
	}()

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	upgradeCurrentVersion = func() string { return "dev" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for explicit version")
		return upgrade.CheckResult{}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	t.Setenv("HOME", dir)

	var gotPlan upgrade.InstallPlan
	installUpgrade = func(_ context.Context, plan upgrade.InstallPlan) error {
		gotPlan = plan
		return nil
	}
	restartDaemon = func() error { return nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{Yes: true, Version: "1.2.4"}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if gotPlan.Version != "v1.2.4" {
		t.Fatalf("InstallPlan.Version = %q, want v1.2.4", gotPlan.Version)
	}
	if gotPlan.ReleaseURL != "https://github.com/bossanova-dev/bossanova/releases/download/v1.2.4" {
		t.Fatalf("InstallPlan.ReleaseURL = %q, want trusted v1.2.4 URL", gotPlan.ReleaseURL)
	}
	if !strings.Contains(out.String(), "upgrade installed v1.2.4") {
		t.Fatalf("runUpgrade() output = %q, want success message", out.String())
	}
}

func TestRunUpgradeHomebrewRoutesToBrewUpgrade(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldBrewUpgradeBossanova := brewUpgradeBossanova
	oldRestartDaemon := restartDaemon
	oldUpgradeLockPath := upgradeLockPath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		brewUpgradeBossanova = oldBrewUpgradeBossanova
		restartDaemon = oldRestartDaemon
		upgradeLockPath = oldUpgradeLockPath
	}()

	dir := t.TempDir()
	exe := testHomebrewExecutable(t, dir)
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	installUpgrade = func(context.Context, upgrade.InstallPlan) error {
		t.Fatal("installUpgrade called for Homebrew install")
		return nil
	}
	brewCalled := false
	brewUpgradeBossanova = func(context.Context, string) (string, error) {
		brewCalled = true
		return "", nil
	}
	restartCalled := false
	restartDaemon = func() error {
		restartCalled = true
		return nil
	}
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runUpgrade(cmd, upgradeOptions{Yes: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !brewCalled {
		t.Fatal("brewUpgradeBossanova was not called")
	}
	if !restartCalled {
		t.Fatal("restartDaemon was not called")
	}
	if strings.Contains(out.String(), "assets into") || strings.Contains(out.String(), "Cellar") {
		t.Fatalf("runUpgrade() output = %q, want no direct Cellar asset install", out.String())
	}
	if !strings.Contains(out.String(), "Homebrew upgrade completed") || !strings.Contains(out.String(), "daemon restarted") {
		t.Fatalf("runUpgrade() output = %q, want Homebrew success and restart", out.String())
	}
}

func TestRunUpgradeHomebrewPersistsPostUpgradePluginPaths(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldBrewUpgradeBossanova := brewUpgradeBossanova
	oldRestartDaemon := restartDaemon
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	oldUpgradeLockPath := upgradeLockPath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		brewUpgradeBossanova = oldBrewUpgradeBossanova
		restartDaemon = oldRestartDaemon
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
		upgradeLockPath = oldUpgradeLockPath
	}()

	dir := t.TempDir()
	oldPluginDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.3", "libexec", "plugins")
	newPluginDir := filepath.Join(dir, "opt", "homebrew", "opt", "bossanova", "libexec", "plugins")
	executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	installUpgrade = func(context.Context, upgrade.InstallPlan) error {
		t.Fatal("installUpgrade called for Homebrew install")
		return nil
	}
	brewUpgradeBossanova = func(context.Context, string) (string, error) {
		return newPluginDir, nil
	}
	restartDaemon = func() error { return nil }
	loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(oldPluginDir, "bossd-plugin-claude"), Enabled: false},
				{Name: "linear", Path: filepath.Join(oldPluginDir, "bossd-plugin-linear"), Enabled: true},
			},
		}, nil
	}
	var saved config.Settings
	saveCount := 0
	saveSettings = func(s config.Settings) error {
		saveCount++
		saved = s
		return nil
	}
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runUpgrade(cmd, upgradeOptions{Yes: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if saveCount != 1 {
		t.Fatalf("saveSettings called %d times, want 1", saveCount)
	}
	assertPluginSaved(t, saved, "claude", filepath.Join(newPluginDir, "bossd-plugin-claude"), false)
	assertPluginSaved(t, saved, "codex", filepath.Join(newPluginDir, "bossd-plugin-codex"), true)
	assertPluginSaved(t, saved, "linear", filepath.Join(newPluginDir, "bossd-plugin-linear"), true)
}

func TestRunUpgradeHomebrewNoRestartSkipsDaemonRestart(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldBrewUpgradeBossanova := brewUpgradeBossanova
	oldRestartDaemon := restartDaemon
	oldUpgradeLockPath := upgradeLockPath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		brewUpgradeBossanova = oldBrewUpgradeBossanova
		restartDaemon = oldRestartDaemon
		upgradeLockPath = oldUpgradeLockPath
	}()

	dir := t.TempDir()
	executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	brewUpgradeBossanova = func(context.Context, string) (string, error) { return "", nil }
	restartDaemon = func() error {
		t.Fatal("restartDaemon called with --no-restart")
		return nil
	}
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runUpgrade(cmd, upgradeOptions{Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !strings.Contains(out.String(), "daemon restart skipped") {
		t.Fatalf("runUpgrade() output = %q, want no-restart message", out.String())
	}
}

func TestRunUpgradeHomebrewBrewFailureIsActionable(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldBrewUpgradeBossanova := brewUpgradeBossanova
	oldRestartDaemon := restartDaemon
	oldUpgradeLockPath := upgradeLockPath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		brewUpgradeBossanova = oldBrewUpgradeBossanova
		restartDaemon = oldRestartDaemon
		upgradeLockPath = oldUpgradeLockPath
	}()

	dir := t.TempDir()
	executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{Available: true, CurrentVersion: "v1.2.3", LatestVersion: "v1.2.4"}, nil
	}
	brewUpgradeBossanova = func(context.Context, string) (string, error) {
		return "", errors.New("brew upgrade bossanova-dev/tap/bossanova failed: exit status 1\nRun manually: brew upgrade bossanova-dev/tap/bossanova")
	}
	restartDaemon = func() error {
		t.Fatal("restartDaemon called after brew failure")
		return nil
	}
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }

	err := runUpgrade(&cobra.Command{}, upgradeOptions{Yes: true})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want brew failure")
	}
	if !strings.Contains(err.Error(), "brew upgrade bossanova-dev/tap/bossanova failed") ||
		!strings.Contains(err.Error(), "Run manually: brew upgrade bossanova-dev/tap/bossanova") {
		t.Fatalf("runUpgrade() error = %v, want failed command and manual command", err)
	}
}

func TestBrewUpgradeBossanovaUsesDetectedHomebrewBrewOutsidePath(t *testing.T) {
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

	pluginDir, err := brewUpgradeBossanova(context.Background(), binDir)
	if err != nil {
		t.Fatalf("brewUpgradeBossanova() error = %v", err)
	}
	if pluginDir != filepath.Join(prefix, "libexec", "plugins") {
		t.Fatalf("brewUpgradeBossanova() pluginDir = %q, want prefix libexec plugins", pluginDir)
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
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldBrewUpgradeBossanova := brewUpgradeBossanova
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		brewUpgradeBossanova = oldBrewUpgradeBossanova
	}()

	dir := t.TempDir()
	upgradeCurrentVersion = func() string { return "dev" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for explicit version")
		return upgrade.CheckResult{}, nil
	}
	executablePath = func() (string, error) { return testHomebrewExecutable(t, dir), nil }
	installUpgrade = func(context.Context, upgrade.InstallPlan) error {
		t.Fatal("installUpgrade called for Homebrew --version")
		return nil
	}
	brewUpgradeBossanova = func(context.Context, string) (string, error) {
		t.Fatal("brewUpgradeBossanova called for Homebrew --version")
		return "", nil
	}

	err := runUpgrade(&cobra.Command{}, upgradeOptions{Yes: true, Version: "v1.2.4"})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want Homebrew --version error")
	}
	if !strings.Contains(err.Error(), "exact --version installs are not supported") ||
		!strings.Contains(err.Error(), "brew upgrade bossanova-dev/tap/bossanova") {
		t.Fatalf("runUpgrade() error = %v, want Homebrew version guidance", err)
	}
}

func TestRunUpgradeInvalidExplicitVersionErrors(t *testing.T) {
	oldCheck := checkUpgrade
	oldInstallUpgrade := installUpgrade
	defer func() {
		checkUpgrade = oldCheck
		installUpgrade = oldInstallUpgrade
	}()

	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for invalid explicit version")
		return upgrade.CheckResult{}, nil
	}
	installUpgrade = func(context.Context, upgrade.InstallPlan) error {
		t.Fatal("installUpgrade called for invalid explicit version")
		return nil
	}

	err := runUpgrade(&cobra.Command{}, upgradeOptions{Yes: true, Version: "dev"})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want invalid version error")
	}
	if !strings.Contains(err.Error(), "invalid upgrade version") {
		t.Fatalf("runUpgrade() error = %v, want invalid version error", err)
	}
}

func TestRunUpgradeCheckVersionVerifiesReleaseExists(t *testing.T) {
	oldVerify := verifyUpgradeVersion
	defer func() { verifyUpgradeVersion = oldVerify }()

	called := false
	gotVersion := ""
	verifyUpgradeVersion = func(_ context.Context, version string) error {
		called = true
		gotVersion = version
		return nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{CheckOnly: true, Version: "1.2.4"}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !called {
		t.Fatal("verifyUpgradeVersion was not called for --check --version")
	}
	if gotVersion != "v1.2.4" {
		t.Fatalf("verifyUpgradeVersion got %q, want v1.2.4", gotVersion)
	}
	if !strings.Contains(out.String(), "release v1.2.4 exists") {
		t.Fatalf("runUpgrade() output = %q, want existence confirmation", out.String())
	}
}

func TestRunUpgradeCheckVersionPropagatesNotFound(t *testing.T) {
	oldVerify := verifyUpgradeVersion
	defer func() { verifyUpgradeVersion = oldVerify }()

	verifyUpgradeVersion = func(context.Context, string) error {
		return errors.New("release v9.9.9 not found")
	}

	err := runUpgrade(&cobra.Command{}, upgradeOptions{CheckOnly: true, Version: "9.9.9"})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want not-found error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("runUpgrade() error = %v, want not-found error", err)
	}
}

func TestRunUpgradePrereleaseExplicitVersionErrors(t *testing.T) {
	oldCheck := checkUpgrade
	oldInstallUpgrade := installUpgrade
	defer func() {
		checkUpgrade = oldCheck
		installUpgrade = oldInstallUpgrade
	}()

	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for prerelease explicit version")
		return upgrade.CheckResult{}, nil
	}
	installUpgrade = func(context.Context, upgrade.InstallPlan) error {
		t.Fatal("installUpgrade called for prerelease explicit version")
		return nil
	}

	err := runUpgrade(&cobra.Command{}, upgradeOptions{Yes: true, Version: "v1.2.3-beta.1"})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want prerelease version error")
	}
	if !strings.Contains(err.Error(), "prerelease upgrade version") {
		t.Fatalf("runUpgrade() error = %v, want prerelease version error", err)
	}
}

func TestRunUpgradeNoRestartChangesOutput(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
	}()

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
		}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	installUpgrade = func(context.Context, upgrade.InstallPlan) error { return nil }
	restartDaemon = func() error {
		t.Fatal("restartDaemon called with --no-restart")
		return nil
	}
	t.Setenv("HOME", dir)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !strings.Contains(out.String(), "daemon restart skipped") {
		t.Fatalf("runUpgrade() output = %q, want no-restart message", out.String())
	}
}

func TestRunUpgradeReportsRestartError(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
	}()

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	upgradeCurrentVersion = func() string { return "v1.2.3" }
	// Disable the shared upgrade cache so these tests exercise the live check
	// path deterministically, independent of any on-disk banner cache.
	oldActionCachePath := upgradeActionCachePath
	upgradeActionCachePath = func() string { return "" }
	t.Cleanup(func() { upgradeActionCachePath = oldActionCachePath })
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
		}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	installUpgrade = func(context.Context, upgrade.InstallPlan) error { return nil }
	restartDaemon = func() error { return errors.New("restart failed") }
	t.Setenv("HOME", dir)

	err := runUpgrade(&cobra.Command{}, upgradeOptions{Yes: true})
	if err == nil {
		t.Fatal("runUpgrade() error = nil, want restart error")
	}
	if !strings.Contains(err.Error(), "restart daemon") || !strings.Contains(err.Error(), "restart failed") {
		t.Fatalf("runUpgrade() error = %v, want restart daemon error", err)
	}
}

func TestCurrentExecutableDirReturnsExecutablePathError(t *testing.T) {
	oldExecutablePath := executablePath
	defer func() {
		executablePath = oldExecutablePath
	}()

	executablePath = func() (string, error) { return "", errors.New("boom") }
	if _, err := currentExecutableDir(); err == nil {
		t.Fatal("currentExecutableDir() error = nil, want error")
	}
}

func TestRunUpgradeInstallsPluginsIntoConfiguredPluginDir(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	oldLoadSettings := loadSettings
	oldDiscoverPlugins := discoverPlugins
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
		loadSettings = oldLoadSettings
		discoverPlugins = oldDiscoverPlugins
	}()

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

	upgradeCurrentVersion = func() string { return "dev" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for explicit version")
		return upgrade.CheckResult{}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{{
				Name:    "codex",
				Path:    filepath.Join(configuredPluginDir, "bossd-plugin-codex"),
				Enabled: true,
			}},
		}, nil
	}
	discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "codex",
			Path:    filepath.Join(discoveredPluginDir, "bossd-plugin-codex"),
			Enabled: true,
		}}
	}
	var gotPlan upgrade.InstallPlan
	installUpgrade = func(_ context.Context, plan upgrade.InstallPlan) error {
		gotPlan = plan
		return nil
	}
	restartDaemon = func() error { return nil }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runUpgrade(cmd, upgradeOptions{Version: "v1.2.4", Yes: true}); err != nil {
		t.Fatalf("runUpgrade(): %v", err)
	}
	if gotPlan.PluginDir != configuredPluginDir {
		t.Fatalf("InstallPlan.PluginDir = %q, want configured dir %q", gotPlan.PluginDir, configuredPluginDir)
	}
}

func TestRunUpgradeInstallsPluginsIntoDiscoveredPluginDir(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	oldLoadSettings := loadSettings
	oldDiscoverPlugins := discoverPlugins
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
		loadSettings = oldLoadSettings
		discoverPlugins = oldDiscoverPlugins
	}()

	dir := t.TempDir()
	exe := testExecutable(t, dir)
	discoveredPluginDir := filepath.Join(dir, "libexec", "plugins")
	if err := os.MkdirAll(discoveredPluginDir, 0o755); err != nil {
		t.Fatal(err)
	}

	upgradeCurrentVersion = func() string { return "dev" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for explicit version")
		return upgrade.CheckResult{}, nil
	}
	executablePath = func() (string, error) { return exe, nil }
	loadSettings = func() (config.Settings, error) { return config.Settings{}, nil }
	discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "codex",
			Path:    filepath.Join(discoveredPluginDir, "bossd-plugin-codex"),
			Enabled: true,
		}}
	}
	var gotPlan upgrade.InstallPlan
	installUpgrade = func(_ context.Context, plan upgrade.InstallPlan) error {
		gotPlan = plan
		return nil
	}
	restartDaemon = func() error { return nil }

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runUpgrade(cmd, upgradeOptions{Version: "v1.2.4", Yes: true}); err != nil {
		t.Fatalf("runUpgrade(): %v", err)
	}
	if gotPlan.PluginDir != discoveredPluginDir {
		t.Fatalf("InstallPlan.PluginDir = %q, want discovered dir %q", gotPlan.PluginDir, discoveredPluginDir)
	}
}

func TestUpgradePluginDirRepairsMixedHomebrewCellarPluginDirs(t *testing.T) {
	oldLoadSettings := loadSettings
	oldDiscoverPlugins := discoverPlugins
	defer func() {
		loadSettings = oldLoadSettings
		discoverPlugins = oldDiscoverPlugins
	}()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.4", "bin")
	currentPluginDir := filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.4", "libexec", "plugins")
	loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.2", "libexec", "plugins", "bossd-plugin-claude"), Enabled: true},
				{Name: "codex", Path: filepath.Join(dir, "opt", "homebrew", "Cellar", "bossanova", "v1.2.3", "libexec", "plugins", "bossd-plugin-codex"), Enabled: true},
			},
		}, nil
	}
	discoverPlugins = func() []config.PluginConfig { return nil }

	got, err := upgradePluginDir(runtime.GOOS, binDir)
	if err != nil {
		t.Fatalf("upgradePluginDir() error = %v", err)
	}
	if got != currentPluginDir {
		t.Fatalf("upgradePluginDir() = %q, want %q", got, currentPluginDir)
	}
}

func TestUpgradePluginDirRejectsMixedCustomPluginDirs(t *testing.T) {
	oldLoadSettings := loadSettings
	oldDiscoverPlugins := discoverPlugins
	defer func() {
		loadSettings = oldLoadSettings
		discoverPlugins = oldDiscoverPlugins
	}()

	dir := t.TempDir()
	loadSettings = func() (config.Settings, error) {
		return config.Settings{
			Plugins: []config.PluginConfig{
				{Name: "claude", Path: filepath.Join(dir, "custom-a", "bossd-plugin-claude"), Enabled: true},
				{Name: "codex", Path: filepath.Join(dir, "custom-b", "bossd-plugin-codex"), Enabled: true},
			},
		}, nil
	}
	discoverPlugins = func() []config.PluginConfig { return nil }

	_, err := upgradePluginDir(runtime.GOOS, filepath.Join(dir, "bin"))
	if err == nil {
		t.Fatal("upgradePluginDir() error = nil, want mixed custom plugin dir error")
	}
	if !strings.Contains(err.Error(), "span multiple directories") {
		t.Fatalf("upgradePluginDir() error = %v, want mixed directory error", err)
	}
}

func TestRunUpgradeRewritesInstalledPluginPathsPreservingEnabledState(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldExecutablePath := executablePath
	oldInstallUpgrade := installUpgrade
	oldRestartDaemon := restartDaemon
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	oldDiscoverPlugins := discoverPlugins
	oldUpgradeLockPath := upgradeLockPath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
		discoverPlugins = oldDiscoverPlugins
		upgradeLockPath = oldUpgradeLockPath
	}()

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
	loadSettings = func() (config.Settings, error) { return settings, nil }
	discoverPlugins = func() []config.PluginConfig {
		return []config.PluginConfig{{
			Name:    "claude",
			Path:    filepath.Join(currentPluginDir, "bossd-plugin-claude"),
			Enabled: true,
		}}
	}
	executablePath = func() (string, error) { return exe, nil }
	upgradeCurrentVersion = func() string { return "dev" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("checkUpgrade called for explicit version")
		return upgrade.CheckResult{}, nil
	}
	installUpgrade = func(context.Context, upgrade.InstallPlan) error { return nil }
	restartDaemon = func() error {
		t.Fatal("restartDaemon called with --no-restart")
		return nil
	}
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }
	saveCount := 0
	var saved config.Settings
	saveSettings = func(s config.Settings) error {
		saveCount++
		saved = s
		return nil
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	if err := runUpgrade(cmd, upgradeOptions{Version: "v1.2.4", Yes: true, NoRestart: true}); err != nil {
		t.Fatalf("runUpgrade(): %v", err)
	}
	if saveCount != 1 {
		t.Fatalf("saveSettings called %d times, want 1", saveCount)
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
	oldUpgradeLockPath := upgradeLockPath
	defer func() { upgradeLockPath = oldUpgradeLockPath }()

	dir := t.TempDir()
	upgradeLockPath = func() (string, error) { return filepath.Join(dir, "upgrade.lock"), nil }
	innerCalled := false
	err := withUpgradeLock(func() error {
		innerErr := withUpgradeLock(func() error {
			innerCalled = true
			return nil
		})
		if innerErr == nil {
			t.Fatal("inner withUpgradeLock() error = nil, want concurrent lock rejection")
		}
		if !strings.Contains(innerErr.Error(), "already in progress") {
			t.Fatalf("inner withUpgradeLock() error = %v, want already in progress", innerErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withUpgradeLock() error = %v", err)
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

func TestRunUpgradeReusesFreshCacheWithoutChecking(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldActionCache := upgradeActionCachePath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		upgradeActionCachePath = oldActionCache
	}()

	upgradeCurrentVersion = func() string { return "v1.2.3" }
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		t.Fatal("runUpgrade() hit the network despite a fresh cache entry")
		return upgrade.CheckResult{}, nil
	}

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	if err := upgrade.WriteCache(cachePath, upgrade.CacheEntry{
		CheckedAt:      time.Now(),
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/v1.2.4",
	}); err != nil {
		t.Fatal(err)
	}
	upgradeActionCachePath = func() string { return cachePath }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{CheckOnly: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !strings.Contains(out.String(), "upgrade available: v1.2.3 -> v1.2.4") {
		t.Fatalf("runUpgrade() output = %q, want cached upgrade-available line", out.String())
	}
}

func TestRunUpgradeStaleCacheFallsThroughToCheck(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldActionCache := upgradeActionCachePath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		upgradeActionCachePath = oldActionCache
	}()

	upgradeCurrentVersion = func() string { return "v1.2.3" }
	checkCalled := false
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		checkCalled = true
		return upgrade.CheckResult{CurrentVersion: "v1.2.3", LatestVersion: "v1.2.5", Available: true}, nil
	}

	cachePath := filepath.Join(t.TempDir(), "upgrade-cache.json")
	if err := upgrade.WriteCache(cachePath, upgrade.CacheEntry{
		CheckedAt:      time.Now().Add(-48 * time.Hour), // stale, beyond CacheTTL
		CurrentVersion: "v1.2.3",
		LatestVersion:  "v1.2.4",
		ReleaseURL:     "https://example.test/v1.2.4",
	}); err != nil {
		t.Fatal(err)
	}
	upgradeActionCachePath = func() string { return cachePath }

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{CheckOnly: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v", err)
	}
	if !checkCalled {
		t.Fatal("runUpgrade() did not fall through to checkUpgrade on a stale cache")
	}
	if !strings.Contains(out.String(), "v1.2.5") {
		t.Fatalf("runUpgrade() output = %q, want fresh check result v1.2.5", out.String())
	}
}

func TestRunUpgradeRateLimitPrintsFriendlyMessage(t *testing.T) {
	oldCurrentVersion := upgradeCurrentVersion
	oldCheck := checkUpgrade
	oldActionCache := upgradeActionCachePath
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		upgradeActionCachePath = oldActionCache
	}()

	upgradeCurrentVersion = func() string { return "v1.2.3" }
	upgradeActionCachePath = func() string { return "" } // force the network path
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{}, &upgrade.RateLimitError{Resets: time.Now().Add(30 * time.Minute)}
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := runUpgrade(cmd, upgradeOptions{CheckOnly: true}); err != nil {
		t.Fatalf("runUpgrade() error = %v, want nil (friendly message, not fatal)", err)
	}
	for _, want := range []string{"rate limit", "resets at", "gh auth login"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("runUpgrade() output = %q, want containing %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "HTTP 403") {
		t.Fatalf("runUpgrade() output = %q, should not contain raw HTTP 403", out.String())
	}
}
