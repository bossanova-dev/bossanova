// Package pluginharness centralises the build-and-spawn logic for plugin
// integration tests. The host spawns each plugin binary as a go-plugin
// subprocess using a handshake + gRPC broker; every test that needs a live
// plugin duplicates the same "go build then goplugin.NewClient" dance. This
// package owns that dance so individual tests stay focused on the behaviour
// they're verifying.
package pluginharness

import (
	"io"
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

// PluginBinary returns an executable path for the plugin named pluginName (the
// directory under plugins/, e.g. "bossd-plugin-repair"). This is the single
// resolution contract every helper funnels through:
//
//   - Under `bazel test`, the plugin go_binary is prebuilt and staged into the
//     test's runfiles; its runfiles-relative path is handed in via a per-plugin
//     env var (see stagedBinary / pluginBinEnvKey). No `go` toolchain runs.
//   - Under plain `go test`, that env var is unset, so the binary is compiled
//     from source into a temp dir (the historical path).
//
// If the plugin source directory is not present (public repo checkout without
// plugins/) and no staged binary was provided, the test is skipped via t.Skip —
// mirroring the convention in integration_test.go so public checkouts pass CI.
func PluginBinary(t *testing.T, pluginName string) string {
	t.Helper()
	return resolvePlugin(t, pluginName, nil)
}

// BuildPlugin is the historical name for the no-tags resolution; it now routes
// through the single PluginBinary contract.
func BuildPlugin(t *testing.T, pluginName string) string {
	t.Helper()
	return PluginBinary(t, pluginName)
}

// BuildPluginWithTags resolves a build-tag-fenced variant of the plugin. Used
// by tests that need tag-gated hooks — for example the linear plugin exposes
// LINEAR_API_ENDPOINT only under the `e2e` tag, so the production binary never
// reads that env var. Under Bazel the tagged variant is a separate go_binary
// (gotags=[...]) staged under its own env key; under `go test` it is built with
// `go build -tags <tags>`.
func BuildPluginWithTags(t *testing.T, pluginName string, tags ...string) string {
	t.Helper()
	return resolvePlugin(t, pluginName, tags)
}

// BuildPluginInto resolves pluginName and copies the resulting binary into
// outDir (which must already exist), returning the full destination path. The
// discovery / host-restart / repair tests use this variant so every plugin
// binary lands in a single directory the daemon's loader can scan — replicating
// the production Homebrew / dev-mode layout where every bossd-plugin-* sits next
// to the others. Copying works identically under both runners: under `go test`
// the source is a freshly built temp binary, under Bazel it is the prebuilt
// runfiles binary.
func BuildPluginInto(t *testing.T, outDir, pluginName string) string {
	t.Helper()
	src := resolvePlugin(t, pluginName, nil)
	dst := filepath.Join(outDir, pluginName)
	copyExecutable(t, src, dst)
	return dst
}

// resolvePlugin is the one place that decides how a plugin binary is obtained:
// a Bazel-staged prebuilt binary if its env var is set, otherwise a go-build
// from source. There is no other "if bazel" branch in this package.
func resolvePlugin(t *testing.T, pluginName string, tags []string) string {
	t.Helper()
	if staged := stagedBinary(t, pluginName, tags); staged != "" {
		return staged
	}
	return buildPlugin(t, t.TempDir(), pluginName, tags)
}

// stagedBinary returns the absolute path to a Bazel-prebuilt plugin binary, or
// "" when running under plain `go test` (env var unset). The env var holds a
// runfiles-root-relative path; the go_test sets rundir="." so the test's cwd is
// the runfiles root, making filepath.Abs resolve it to the staged executable.
func stagedBinary(t *testing.T, pluginName string, tags []string) string {
	t.Helper()
	rel := os.Getenv(pluginBinEnvKey(pluginName, tags))
	if rel == "" {
		return ""
	}
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve staged plugin %q path %q: %v", pluginName, rel, err)
	}
	return abs
}

// pluginBinEnvKey derives the env var name the go_test uses to hand a prebuilt
// binary to the harness: "BOSS_PLUGIN_BIN_" + upper(name[+tags]) with every
// non-[A-Z0-9_] char replaced by "_". Examples:
//
//	("bossd-plugin-claude", nil)          -> BOSS_PLUGIN_BIN_BOSSD_PLUGIN_CLAUDE
//	("bossd-plugin-linear", []string{"e2e"}) -> BOSS_PLUGIN_BIN_BOSSD_PLUGIN_LINEAR_E2E
func pluginBinEnvKey(pluginName string, tags []string) string {
	parts := append([]string{pluginName}, tags...)
	raw := strings.ToUpper(strings.Join(parts, "_"))
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, raw)
	return "BOSS_PLUGIN_BIN_" + sanitized
}

// copyExecutable copies src to dst with mode 0o755. Used by BuildPluginInto so
// staged/built binaries can be gathered into one scanned directory.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(filepath.Clean(src)) // #nosec G304 -- test harness copies a caller-supplied, freshly built plugin binary path (filepath.Clean sanitized); owner=@recurser; review-by=2026-10-18; issue=BOS-423
	if err != nil {
		t.Fatalf("open plugin binary %q: %v", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) // #nosec G302 G304 -- copied plugin binary must be 0o755 to be executable; dst is a test-harness-owned staging path (filepath.Clean sanitized); owner=@recurser; review-by=2026-10-18; issue=BOS-423
	if err != nil {
		t.Fatalf("create plugin binary %q: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy plugin binary %q -> %q: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close plugin binary %q: %v", dst, err)
	}
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
