package plugin_test

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// TestPluginBinaryResolves is the harness self-test for the single plugin-path
// contract. PluginBinary must return an executable path under both runners:
// under `go test` it go-builds the plugin from source; under `bazel test` it
// resolves the prebuilt go_binary staged into runfiles via the per-plugin
// BOSS_PLUGIN_BIN_* env var (see BUILD.bazel data + env + rundir="."). Either
// way the returned path must exist and be executable.
func TestPluginBinaryResolves(t *testing.T) {
	path := pluginharness.PluginBinary(t, "bossd-plugin-stub-runner")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat resolved plugin binary %q: %v", path, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("resolved plugin binary %q is not executable (mode %v)", path, info.Mode())
	}
}
