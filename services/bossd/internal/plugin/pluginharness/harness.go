// Package pluginharness centralises the build-and-spawn logic for plugin
// integration tests. The host spawns each plugin binary as a go-plugin
// subprocess using a handshake + gRPC broker; every test that needs a live
// plugin duplicates the same "go build then goplugin.NewClient" dance. This
// package owns that dance so individual tests stay focused on the behaviour
// they're verifying.
package pluginharness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"

	pluginpkg "github.com/recurser/bossd/internal/plugin"
)

// workspaceRoot locates the repository root by resolving this file's path via
// runtime.Caller. This file lives at services/bossd/internal/plugin/pluginharness/harness.go,
// so the workspace root is five directories up. Using runtime.Caller keeps the
// lookup independent of the caller's working directory — a cwd-relative path
// would silently misresolve when tests run from a different depth.
func workspaceRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "..", "..")
}

// MigrationsDir returns the absolute path to services/bossd/migrations, anchored
// to this file's location via runtime.Caller so tests are independent of cwd.
// Tests in package plugin_test should call this instead of keeping their own
// copies; the plugin package itself can't import pluginharness (import cycle),
// so host_service_test.go has its own local copy.
func MigrationsDir() string {
	return filepath.Join(workspaceRoot(), "services", "bossd", "migrations")
}

// BuildPlugin compiles the plugin named pluginName (the directory under
// plugins/, e.g. "bossd-plugin-autopilot") into a temp directory and returns
// the path to the resulting binary. If the plugin source directory is not
// present (public repo checkout without plugins/), the test is skipped via
// t.Skip — this mirrors the convention already used in integration_test.go
// so checkouts without the private plugin source still pass CI.
//
// The binary is built from the workspace root so go.work can resolve the
// shared bossalib module.
func BuildPlugin(t *testing.T, pluginName string) string {
	t.Helper()
	return buildPlugin(t, t.TempDir(), pluginName, nil)
}

// BuildPluginWithTags is BuildPlugin plus `go build -tags <tags>`. Used by
// tests that need to enable build-tag-fenced hooks in the plugin binary —
// for example the linear plugin exposes LINEAR_API_ENDPOINT only under the
// `e2e` tag, so the production binary never reads that env var.
func BuildPluginWithTags(t *testing.T, pluginName string, tags ...string) string {
	t.Helper()
	return buildPlugin(t, t.TempDir(), pluginName, tags)
}

// BuildPluginInto compiles pluginName into outDir (which must already exist)
// and returns the full path to the resulting binary. The discovery tests use
// this variant so all four plugin binaries land in a single directory that
// the daemon's loader can scan — replicating the production Homebrew /
// dev-mode layout where every bossd-plugin-* sits next to the others.
func BuildPluginInto(t *testing.T, outDir, pluginName string) string {
	t.Helper()
	return buildPlugin(t, outDir, pluginName, nil)
}

func buildPlugin(t *testing.T, outDir, pluginName string, tags []string) string {
	t.Helper()

	wsRoot := workspaceRoot()
	pluginSrc := filepath.Join(wsRoot, "plugins", pluginName)
	if _, err := os.Stat(pluginSrc); os.IsNotExist(err) {
		t.Skipf("skipping: plugins/%s not present (public repo)", pluginName)
	}

	binPath := filepath.Join(outDir, pluginName)
	args := []string{"build"}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	args = append(args, "-o", binPath, "./plugins/"+pluginName)
	cmd := exec.Command("go", args...)
	cmd.Dir = wsRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build plugin %q: %v", pluginName, err)
	}
	return binPath
}

// logger returns the hclog logger that SpawnPlugin uses by default. It is
// quiet (Error level) unless BOSSANOVA_PLUGIN_TEST_VERBOSE is set, in which
// case the plugin's stdout/stderr and handshake debug output are surfaced.
func logger() hclog.Logger {
	level := hclog.Error
	if os.Getenv("BOSSANOVA_PLUGIN_TEST_VERBOSE") != "" {
		level = hclog.Debug
	}
	return hclog.New(&hclog.LoggerOptions{
		Name:   "pluginharness",
		Level:  level,
		Output: os.Stderr,
	})
}

// SpawnPlugin starts the plugin binary at binaryPath with the supplied
// pluginMap and registers a t.Cleanup that kills the subprocess when the
// test ends. The returned client is already dialled — callers should call
// client.Client().Dispense(...) to obtain typed plugin references.
//
// pluginMap is the PluginSet the host expects; callers typically construct
// it from pluginpkg.NewPluginMap or a trimmed subset that isolates one
// plugin type (matching what the plugin binary under test actually serves).
func SpawnPlugin(t *testing.T, binaryPath string, pluginMap goplugin.PluginSet) *goplugin.Client {
	t.Helper()

	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: pluginpkg.NewHandshake("test-cookie"),
		Plugins:         pluginMap,
		Cmd:             exec.Command(binaryPath),
		AllowedProtocols: []goplugin.Protocol{
			goplugin.ProtocolGRPC,
		},
		Logger: logger(),
	})
	t.Cleanup(client.Kill)
	return client
}
