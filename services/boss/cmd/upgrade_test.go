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
	defer func() {
		upgradeCurrentVersion = oldCurrentVersion
		checkUpgrade = oldCheck
		executablePath = oldExecutablePath
		installUpgrade = oldInstallUpgrade
		restartDaemon = oldRestartDaemon
	}()

	dir := t.TempDir()
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
	checkUpgrade = func(context.Context, string) (upgrade.CheckResult, error) {
		return upgrade.CheckResult{
			Available:      true,
			CurrentVersion: "v1.2.3",
			LatestVersion:  "v1.2.4",
			ReleaseURL:     "https://example.test/stable",
		}, nil
	}
	executablePath = func() (string, error) { return linkExe, nil }
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
