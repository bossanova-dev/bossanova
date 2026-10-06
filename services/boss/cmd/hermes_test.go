package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/daemon"
	"github.com/recurser/boss/internal/hermes"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// hermesRunCall records one invocation of the injected hermes runner.
type hermesRunCall struct {
	bin  string
	env  []string
	args []string
}

// hermesTestEnv snapshots every hermes.go seam, points the host at a throwaway
// HOME, and installs defaults under which nothing real is touched: a fixed MCP
// path, a reachable daemon with a login registration, and no hermes on PATH.
// Tests using it must not call t.Parallel(): the seams are package state.
type hermesTestEnv struct {
	root  string
	calls *[]hermesRunCall
}

func setupHermesTest(t *testing.T) hermesTestEnv {
	t.Helper()
	oldLookPath := hermesLookPath
	oldRun := hermesRun
	oldResolve := hermesResolveMcpPath
	oldStatus := hermesDaemonStatus
	oldReach := hermesSocketReachable
	oldTools := hermesToolDefinitions
	t.Cleanup(func() {
		hermesLookPath = oldLookPath
		hermesRun = oldRun
		hermesResolveMcpPath = oldResolve
		hermesDaemonStatus = oldStatus
		hermesSocketReachable = oldReach
		hermesToolDefinitions = oldTools
	})

	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HERMES_HOME", "")
	t.Setenv("BOSS_SOCKET", "")
	t.Setenv(trustCheckoutSkillSourcesEnv, "")

	calls := &[]hermesRunCall{}
	hermesLookPath = func(string) (string, error) { return "", errors.New("hermes not on PATH") }
	hermesRun = func(_ context.Context, bin string, env []string, args ...string) ([]byte, error) {
		*calls = append(*calls, hermesRunCall{bin: bin, env: env, args: args})
		return nil, nil
	}
	hermesResolveMcpPath = func() (string, error) { return "/opt/boss/bin/boss-mcp", nil }
	hermesDaemonStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: true, Running: true}, nil }
	hermesSocketReachable = func(string) bool { return true }
	return hermesTestEnv{root: root, calls: calls}
}

// writeSettings writes a settings file at path carrying socketPath.
func writeSettings(t *testing.T, path string, s map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runHermes(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	c := hermesCmd()
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	err = c.Execute()
	return out.String(), errOut.String(), err
}

func readInstallConfig(t *testing.T, pluginDir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(pluginDir, hermes.InstallFile))
	if err != nil {
		t.Fatalf("read %s: %v", hermes.InstallFile, err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse %s: %v", hermes.InstallFile, err)
	}
	return got
}

func TestResolveHermesTargets(t *testing.T) {
	env := setupHermesTest(t)
	home := filepath.Join(env.root, "home")
	defaultDir, err := config.DefaultAppDataDir()
	if err != nil {
		t.Fatal(err)
	}

	ambient := filepath.Join(env.root, "ambient", "settings.json")
	writeSettings(t, ambient, map[string]any{"socket_path": "/run/ambient/bossd.sock"})
	named := filepath.Join(env.root, "named", "settings.json")
	writeSettings(t, named, map[string]any{"socket_path": "/run/named/bossd.sock"})
	dataDirOnly := filepath.Join(env.root, "datadir", "settings.json")
	appData := filepath.Join(env.root, "appdata-not-created")
	writeSettings(t, dataDirOnly, map[string]any{"app_data_dir": appData})
	defaultFile := filepath.Join(defaultDir, "settings.json")

	cases := []struct {
		name    string
		flags   hermesFlags
		env     map[string]string
		want    hermesTargets
		wantErr string
	}{
		{
			name: "all defaults",
			env:  map[string]string{"BOSS_SETTINGS_PATH": ""},
			want: hermesTargets{
				SettingsPath:   defaultFile,
				DefaultProfile: true,
				Socket:         filepath.Join(defaultDir, "bossd.sock"),
				HermesHome:     filepath.Join(home, ".hermes"),
			},
		},
		{
			name: "BOSS_SETTINGS_PATH selects the profile and its socket_path",
			env:  map[string]string{"BOSS_SETTINGS_PATH": ambient},
			want: hermesTargets{SettingsPath: ambient, Socket: "/run/ambient/bossd.sock", HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "--settings beats BOSS_SETTINGS_PATH and loads socket_path from the named file",
			flags: hermesFlags{settings: named},
			env:   map[string]string{"BOSS_SETTINGS_PATH": ambient},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/named/bossd.sock", HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "--settings naming the OS default is the default profile",
			flags: hermesFlags{settings: defaultFile},
			env:   map[string]string{"BOSS_SETTINGS_PATH": ambient},
			want: hermesTargets{
				SettingsPath:   defaultFile,
				DefaultProfile: true,
				Socket:         filepath.Join(defaultDir, "bossd.sock"),
				HermesHome:     filepath.Join(home, ".hermes"),
			},
		},
		{
			name:  "app_data_dir-only profile puts the socket there without creating it",
			flags: hermesFlags{settings: dataDirOnly},
			want:  hermesTargets{SettingsPath: dataDirOnly, Socket: filepath.Join(appData, "bossd.sock"), HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "BOSS_SOCKET beats the settings file",
			flags: hermesFlags{settings: named},
			env:   map[string]string{"BOSS_SOCKET": "/run/env/bossd.sock"},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/env/bossd.sock", HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "--socket beats BOSS_SOCKET",
			flags: hermesFlags{settings: named, socket: "/run/flag/bossd.sock"},
			env:   map[string]string{"BOSS_SOCKET": "/run/env/bossd.sock"},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/flag/bossd.sock", HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "absolute HERMES_HOME is used",
			flags: hermesFlags{settings: named},
			env:   map[string]string{"HERMES_HOME": "/srv/hermes-env"},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/named/bossd.sock", HermesHome: "/srv/hermes-env"},
		},
		{
			name:  "relative HERMES_HOME is ignored",
			flags: hermesFlags{settings: named},
			env:   map[string]string{"HERMES_HOME": "relative/hermes"},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/named/bossd.sock", HermesHome: filepath.Join(home, ".hermes")},
		},
		{
			name:  "--hermes-home beats HERMES_HOME",
			flags: hermesFlags{settings: named, hermesHome: "/srv/hermes-flag"},
			env:   map[string]string{"HERMES_HOME": "/srv/hermes-env"},
			want:  hermesTargets{SettingsPath: named, Socket: "/run/named/bossd.sock", HermesHome: "/srv/hermes-flag"},
		},
		{
			name:    "relative --settings is refused",
			flags:   hermesFlags{settings: "relative/settings.json"},
			wantErr: "--settings must be absolute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := resolveHermesTargets(tc.flags)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHermesTargets: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
	if _, err := os.Stat(appData); !os.IsNotExist(err) {
		t.Errorf("resolution created app_data_dir %s (stat err %v); it must write nothing", appData, err)
	}
}

func TestHermesInstall_NonDefaultProfileWritesPluginAndSkills(t *testing.T) {
	env := setupHermesTest(t)
	settings := filepath.Join(env.root, "profile", "settings.json")
	writeSettings(t, settings, map[string]any{"socket_path": "/run/profile/bossd.sock"})
	hermesHome := filepath.Join(env.root, "hermes")

	stdout, _, err := runHermes(t, "install", "--settings", settings, "--hermes-home", hermesHome)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, stdout)
	}

	pluginDir := filepath.Join(hermesHome, "plugins", hermes.PluginName)
	got := readInstallConfig(t, pluginDir)
	want := map[string]any{
		"mcp_bin":       "/opt/boss/bin/boss-mcp",
		"socket_path":   "/run/profile/bossd.sock",
		"settings_path": settings,
	}
	if len(got) != len(want) {
		t.Errorf("boss_install.json = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("boss_install.json[%q] = %v, want %v", k, got[k], v)
		}
	}
	for _, f := range []string{hermes.ManifestFile, hermes.ToolsFile, "__init__.py"} {
		if _, err := os.Stat(filepath.Join(pluginDir, f)); err != nil {
			t.Errorf("plugin file %s missing: %v", f, err)
		}
	}
	if !libskillinstall.IsInstalled(filepath.Join(hermesHome, "skills")) {
		t.Errorf("Hermes skills were not installed under %s", filepath.Join(hermesHome, "skills"))
	}
	entries, err := os.ReadDir(filepath.Join(hermesHome, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != hermes.PluginName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("plugins/ holds %v, want only %s (no staging leftovers)", names, hermes.PluginName)
	}
	// No hermes on PATH: exit 0, plugin in place, enable command printed.
	if !strings.Contains(stdout, "hermes plugins enable bossanova") {
		t.Errorf("stdout does not print the enable command:\n%s", stdout)
	}
	if len(*env.calls) != 0 {
		t.Errorf("runner called %d times with hermes absent", len(*env.calls))
	}
}

func TestHermesInstall_DefaultProfileOmitsSettingsPath(t *testing.T) {
	env := setupHermesTest(t)
	t.Setenv("BOSS_SETTINGS_PATH", "")
	hermesHome := filepath.Join(env.root, "hermes")

	if stdout, _, err := runHermes(t, "install", "--hermes-home", hermesHome); err != nil {
		t.Fatalf("install: %v\n%s", err, stdout)
	}
	got := readInstallConfig(t, filepath.Join(hermesHome, "plugins", hermes.PluginName))
	if _, ok := got["settings_path"]; ok {
		t.Errorf("default profile wrote settings_path: %v", got)
	}
}

func TestHermesInstall_EnablesThroughRunnerWithHermesHome(t *testing.T) {
	env := setupHermesTest(t)
	hermesLookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
	hermesHome := filepath.Join(env.root, "hermes")

	stdout, _, err := runHermes(t, "install", "--hermes-home", hermesHome)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, stdout)
	}
	if len(*env.calls) != 1 {
		t.Fatalf("runner called %d times, want 1: %+v", len(*env.calls), *env.calls)
	}
	call := (*env.calls)[0]
	if call.bin != "/fake/bin/hermes" {
		t.Errorf("runner bin = %q", call.bin)
	}
	if !slices.Equal(call.args, []string{"plugins", "enable", "bossanova"}) {
		t.Errorf("runner args = %q, want plugins enable bossanova", call.args)
	}
	if !slices.Contains(call.env, "HERMES_HOME="+hermesHome) {
		t.Errorf("runner env = %q, want HERMES_HOME=%s", call.env, hermesHome)
	}
}

func TestHermesInstall_NoEnableSkipsRunner(t *testing.T) {
	env := setupHermesTest(t)
	hermesLookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
	hermesHome := filepath.Join(env.root, "hermes")

	stdout, _, err := runHermes(t, "install", "--hermes-home", hermesHome, "--no-enable")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, stdout)
	}
	if len(*env.calls) != 0 {
		t.Errorf("--no-enable still ran hermes: %+v", *env.calls)
	}
	if !strings.Contains(stdout, "hermes plugins enable bossanova") {
		t.Errorf("--no-enable did not print the enable command:\n%s", stdout)
	}
}

func TestHermesInstall_EnableFailureIsAnError(t *testing.T) {
	env := setupHermesTest(t)
	hermesLookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
	hermesRun = func(context.Context, string, []string, ...string) ([]byte, error) {
		return []byte("no such plugin"), errors.New("exit status 1")
	}
	hermesHome := filepath.Join(env.root, "hermes")

	_, _, err := runHermes(t, "install", "--hermes-home", hermesHome)
	if err == nil || !strings.Contains(err.Error(), "no such plugin") {
		t.Fatalf("err = %v, want the hermes failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(hermesHome, "plugins", hermes.PluginName, hermes.InstallFile)); statErr != nil {
		t.Errorf("plugin not left in place after the enable failure: %v", statErr)
	}
}

func TestHermesInstall_RefusesUnmanagedPluginDirUnlessForced(t *testing.T) {
	env := setupHermesTest(t)
	hermesHome := filepath.Join(env.root, "hermes")
	pluginDir := filepath.Join(hermesHome, "plugins", hermes.PluginName)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	handMade := filepath.Join(pluginDir, "__init__.py")
	if err := os.WriteFile(handMade, []byte("# hand-made\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := runHermes(t, "install", "--hermes-home", hermesHome)
	if !errors.Is(err, errHermesPluginUnmanaged) {
		t.Fatalf("err = %v, want errHermesPluginUnmanaged", err)
	}
	if data, _ := os.ReadFile(handMade); string(data) != "# hand-made\n" {
		t.Errorf("hand-made plugin was modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, hermes.InstallFile)); !os.IsNotExist(err) {
		t.Errorf("boss_install.json written into the refused dir (stat err %v)", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(hermesHome, "skills")) {
		t.Error("skills installed even though the plugin step refused")
	}

	if stdout, _, err := runHermes(t, "install", "--hermes-home", hermesHome, "--force"); err != nil {
		t.Fatalf("install --force: %v\n%s", err, stdout)
	}
	if data, _ := os.ReadFile(handMade); string(data) == "# hand-made\n" {
		t.Error("--force did not replace the hand-made plugin")
	}
	readInstallConfig(t, pluginDir)
}

func TestHermesInstall_ReplacesItsOwnInstall(t *testing.T) {
	env := setupHermesTest(t)
	hermesHome := filepath.Join(env.root, "hermes")
	if _, _, err := runHermes(t, "install", "--hermes-home", hermesHome); err != nil {
		t.Fatal(err)
	}
	hermesResolveMcpPath = func() (string, error) { return "/moved/boss-mcp", nil }
	if stdout, _, err := runHermes(t, "install", "--hermes-home", hermesHome); err != nil {
		t.Fatalf("re-install: %v\n%s", err, stdout)
	}
	got := readInstallConfig(t, filepath.Join(hermesHome, "plugins", hermes.PluginName))
	if got["mcp_bin"] != "/moved/boss-mcp" {
		t.Errorf("mcp_bin = %v after re-install, want /moved/boss-mcp", got["mcp_bin"])
	}
}

func TestHermesInstall_DryRunWritesAndRunsNothing(t *testing.T) {
	env := setupHermesTest(t)
	hermesLookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
	settings := filepath.Join(env.root, "profile", "settings.json")
	writeSettings(t, settings, map[string]any{"socket_path": "/run/profile/bossd.sock"})
	hermesHome := filepath.Join(env.root, "hermes")

	stdout, _, err := runHermes(t, "install", "--settings", settings, "--hermes-home", hermesHome, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, err := os.Stat(hermesHome); !os.IsNotExist(err) {
		t.Errorf("--dry-run created %s (stat err %v)", hermesHome, err)
	}
	if len(*env.calls) != 0 {
		t.Errorf("--dry-run ran hermes: %+v", *env.calls)
	}
	for _, want := range []string{settings, "/run/profile/bossd.sock", hermesHome, "/opt/boss/bin/boss-mcp", hermes.InstallFile, hermes.ManifestFile} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestHermesInstall_WarnsWhenDaemonUnreachableOrUnregistered(t *testing.T) {
	env := setupHermesTest(t)
	hermesSocketReachable = func(string) bool { return false }
	hermesDaemonStatus = func() (*daemon.Status, error) { return &daemon.Status{Installed: false}, nil }
	settings := filepath.Join(env.root, "profile", "settings.json")
	writeSettings(t, settings, map[string]any{"socket_path": "/run/profile/bossd.sock"})

	_, stderr, err := runHermes(t, "install", "--settings", settings, "--hermes-home", filepath.Join(env.root, "hermes"))
	if err != nil {
		t.Fatalf("install must warn, not fail: %v", err)
	}
	for _, want := range []string{"not reachable", "login registration", "BOSS_SETTINGS_PATH=" + settings + " boss daemon install"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func hermesStatusJSON(t *testing.T, args ...string) hermesStatusReport {
	t.Helper()
	stdout, _, err := runHermes(t, append([]string{"status", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var r hermesStatusReport
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("parse status JSON: %v\n%s", err, stdout)
	}
	return r
}

// Under XDG_CONFIG_HOME the OS-default settings path is not the default
// profile: the Hermes-spawned MCP binary may not inherit XDG, so the path has
// to be pinned in boss_install.json.
func TestResolveHermesTargets_XDGDefaultIsPinned(t *testing.T) {
	env := setupHermesTest(t)
	t.Setenv("BOSS_SETTINGS_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(env.root, "xdg"))
	want, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveHermesTargets(hermesFlags{})
	if err != nil {
		t.Fatalf("resolveHermesTargets: %v", err)
	}
	if got.SettingsPath != want || got.DefaultProfile {
		t.Errorf("got settings=%q default=%v, want settings=%q default=false", got.SettingsPath, got.DefaultProfile, want)
	}
	if got.installSettingsPath() != want {
		t.Errorf("installSettingsPath = %q, want the pinned %q", got.installSettingsPath(), want)
	}
}

func TestHermesStatus_StaleAfterMcpBinaryMoves(t *testing.T) {
	env := setupHermesTest(t)
	hermesHome := filepath.Join(env.root, "hermes")

	if r := hermesStatusJSON(t, "--hermes-home", hermesHome); r.Plugin != hermesPluginMissing || r.Skills != hermesPluginMissing {
		t.Fatalf("before install: plugin=%q skills=%q, want missing/missing", r.Plugin, r.Skills)
	}
	if _, _, err := runHermes(t, "install", "--hermes-home", hermesHome); err != nil {
		t.Fatal(err)
	}
	// Hermes importing the plugin leaves bytecode behind; that is not drift.
	pycache := filepath.Join(hermesHome, "plugins", hermes.PluginName, "__pycache__")
	if err := os.MkdirAll(pycache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pycache, "config.cpython-312.pyc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := hermesStatusJSON(t, "--hermes-home", hermesHome)
	if r.Plugin != hermesPluginCurrent || r.Skills != hermesPluginCurrent {
		t.Fatalf("after install: plugin=%q skills=%q, want current/current", r.Plugin, r.Skills)
	}

	hermesResolveMcpPath = func() (string, error) { return "/moved/boss-mcp", nil }
	r = hermesStatusJSON(t, "--hermes-home", hermesHome)
	if r.Plugin != hermesPluginStale {
		t.Errorf("after the MCP binary moved: plugin=%q, want stale", r.Plugin)
	}
	if r.McpBin != "/moved/boss-mcp" {
		t.Errorf("mcp_bin = %q", r.McpBin)
	}

	stdout, _, err := runHermes(t, "status", "--hermes-home", hermesHome)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "boss hermes install") {
		t.Errorf("stale status does not say to re-run install:\n%s", stdout)
	}
	// The advice must target the inspected Hermes home, not the ambient default.
	if want := "--hermes-home " + shellQuote(hermesHome); !strings.Contains(stdout, want) {
		t.Errorf("reinstall advice drops the selected Hermes home (want %q):\n%s", want, stdout)
	}
}

func TestHermesStatus_UnmanagedPluginDir(t *testing.T) {
	env := setupHermesTest(t)
	hermesHome := filepath.Join(env.root, "hermes")
	if err := os.MkdirAll(filepath.Join(hermesHome, "plugins", hermes.PluginName), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := hermesStatusJSON(t, "--hermes-home", hermesHome); r.Plugin != hermesPluginUnmanaged {
		t.Errorf("plugin = %q, want unmanaged", r.Plugin)
	}
}

func TestHermesStatus_EnabledStateFromHermesList(t *testing.T) {
	cases := []struct {
		name   string
		output string
		err    error
		want   string
	}{
		{name: "enabled", output: "NAME       STATUS\nbossanova  enabled\nother  disabled\n", want: "enabled"},
		{name: "disabled", output: "bossanova  disabled\n", want: "disabled"},
		{name: "absent from the list", output: "other  enabled\n", want: "not-listed"},
		{name: "list fails", err: errors.New("exit status 2"), want: hermesStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupHermesTest(t)
			hermesHome := filepath.Join(env.root, "hermes")
			hermesLookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
			var gotEnv []string
			var gotArgs []string
			hermesRun = func(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
				gotEnv, gotArgs = env, args
				return []byte(tc.output), tc.err
			}
			r := hermesStatusJSON(t, "--hermes-home", hermesHome)
			if r.Enabled != tc.want {
				t.Errorf("enabled = %q, want %q", r.Enabled, tc.want)
			}
			if !slices.Equal(gotArgs, []string{"plugins", "list"}) || !slices.Contains(gotEnv, "HERMES_HOME="+hermesHome) {
				t.Errorf("runner got args %q env %q", gotArgs, gotEnv)
			}
		})
	}
}

func TestHermesStatus_EnabledUnknownWithoutHermes(t *testing.T) {
	env := setupHermesTest(t)
	if r := hermesStatusJSON(t, "--hermes-home", filepath.Join(env.root, "hermes")); r.Enabled != hermesStateUnknown {
		t.Errorf("enabled = %q without hermes, want unknown", r.Enabled)
	}
	if len(*env.calls) != 0 {
		t.Errorf("runner called without hermes on PATH: %+v", *env.calls)
	}
}
